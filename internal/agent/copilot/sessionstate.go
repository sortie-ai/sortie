package copilot

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/domain"
)

// errSessionStateCapExceeded wraps the error readSessionUsage returns
// when the events file exceeds the byte cap or a single line exceeds
// the line cap, so callers can distinguish a cap violation from an
// ordinary I/O failure.
var errSessionStateCapExceeded = errors.New("session-state read exceeds cap")

// sessionStateRoot resolves the root directory under which the copilot
// runtime writes one subdirectory per session id, each holding an
// events.jsonl journal. It returns
// filepath.Join(env("COPILOT_HOME"), "session-state") when COPILOT_HOME
// is set and non-empty, and filepath.Join(home directory, ".copilot",
// "session-state") otherwise. env and home let callers substitute
// os.Getenv and os.UserHomeDir in production and fixed values in tests.
func sessionStateRoot(env func(string) string, home func() (string, error)) (string, error) {
	if v := env(sessionstate.HomeVar); v != "" {
		return filepath.Join(v, "session-state"), nil
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(dir, sessionstate.DefaultHome, "session-state"), nil
}

// readSessionUsage scans the session-state events journal at path and
// returns the totals of the last line whose decoded type is
// "session.shutdown" as current, and its per-model split as byModel, nil
// when its modelMetrics is empty. model names the model
// [shutdownModelName] selects between that record and the one before it,
// empty whenever found is false or err is non-nil. found is false when the
// file holds no session.shutdown record.
//
// The read is abandoned and err is non-nil when the file exceeds 64 MB
// or a single line exceeds 10 MB. A line that fails to decode is
// skipped without failing the read. readSessionUsage never logs the
// file's contents.
func readSessionUsage(path string) (current domain.TokenUsage, byModel map[string]domain.TokenUsage, model string, found bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return domain.TokenUsage{}, nil, "", false, statErr
	}
	if info.Size() > sessionstate.MaxFileBytes {
		return domain.TokenUsage{}, nil, "", false,
			fmt.Errorf("session-state events file exceeds %d bytes: %w", sessionstate.MaxFileBytes, errSessionStateCapExceeded)
	}

	f, openErr := os.Open(path) //nolint:gosec // path is derived from a validated session id, not user input
	if openErr != nil {
		return domain.TokenUsage{}, nil, "", false, openErr
	}
	defer f.Close() //nolint:errcheck // read-only handle

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), sessionstate.MaxLineBytes)

	var records []shutdownEvent
	for scanner.Scan() {
		var evt shutdownEvent
		if err := json.Unmarshal(scanner.Bytes(), &evt); err != nil {
			continue
		}
		if evt.Type != "session.shutdown" {
			continue
		}
		records = append(records, evt)
		if len(records) > 2 {
			records = records[1:]
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return domain.TokenUsage{}, nil, "", false,
				fmt.Errorf("session-state events file line exceeds %d bytes: %w", sessionstate.MaxLineBytes, errSessionStateCapExceeded)
		}
		return domain.TokenUsage{}, nil, "", false, fmt.Errorf("scan session-state events file: %w", scanErr)
	}

	if len(records) == 0 {
		return domain.TokenUsage{}, nil, "", false, nil
	}
	last := records[len(records)-1]
	var prevRecord shutdownEvent
	if len(records) == 2 {
		prevRecord = records[0]
	}
	return shutdownTotals(last), shutdownByModel(last), shutdownModelName(last, prevRecord), true, nil
}

// shutdownModelName returns the trimmed modelMetrics key of current
// whose token growth against the same key in previous is the largest
// positive value, breaking a tie by the trimmed name that sorts first
// lexicographically. A key blank after trimming is skipped. Returns ""
// when current.Data.ModelMetrics is empty or no key grew.
func shutdownModelName(current, previous shutdownEvent) string {
	best, bestGrowth := "", int64(0)
	for key, entry := range current.Data.ModelMetrics {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		prior := previous.Data.ModelMetrics[key]
		growth := (entry.Usage.InputTokens + entry.Usage.OutputTokens) -
			(prior.Usage.InputTokens + prior.Usage.OutputTokens)
		if growth <= 0 {
			continue
		}
		if growth > bestGrowth || (growth == bestGrowth && name < best) {
			best, bestGrowth = name, growth
		}
	}
	return best
}

// modelMetricsUsage maps one modelMetrics entry to a [domain.TokenUsage],
// counting its input tokens as reported.
func modelMetricsUsage(model shutdownModel) domain.TokenUsage {
	usage := domain.TokenUsage{
		InputTokens:      model.Usage.InputTokens,
		OutputTokens:     model.Usage.OutputTokens,
		CacheReadTokens:  model.Usage.CacheReadTokens,
		CacheWriteTokens: model.Usage.CacheWriteTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return usage
}

// shutdownByModel returns the modelMetrics entries of evt by model name,
// mapped as [shutdownTotals] maps them, or nil when there are none.
func shutdownByModel(evt shutdownEvent) map[string]domain.TokenUsage {
	if len(evt.Data.ModelMetrics) == 0 {
		return nil
	}
	byModel := make(map[string]domain.TokenUsage, len(evt.Data.ModelMetrics))
	for name, model := range evt.Data.ModelMetrics {
		byModel[name] = modelMetricsUsage(model)
	}
	return byModel
}

// shutdownTotals extracts token totals from one session.shutdown
// record, preferring data.modelMetrics summed across model entries and
// falling back to data.tokenDetails when modelMetrics is absent or
// empty.
func shutdownTotals(evt shutdownEvent) domain.TokenUsage {
	if len(evt.Data.ModelMetrics) > 0 {
		var usage domain.TokenUsage
		for _, model := range evt.Data.ModelMetrics {
			usage = agentcore.AddUsage(usage, modelMetricsUsage(model))
		}
		return usage
	}

	details := evt.Data.TokenDetails
	input := details["input"].TokenCount + details["cache_read"].TokenCount + details["cache_write"].TokenCount
	output := details["output"].TokenCount
	return domain.TokenUsage{
		InputTokens:      input,
		OutputTokens:     output,
		TotalTokens:      input + output,
		CacheReadTokens:  details["cache_read"].TokenCount,
		CacheWriteTokens: details["cache_write"].TokenCount,
	}
}
