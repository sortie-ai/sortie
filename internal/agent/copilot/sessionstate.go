package copilot

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	// maxSessionStateFileBytes is the maximum size of a session-state
	// events.jsonl file readSessionUsage will read. Larger files are
	// rejected without being opened.
	maxSessionStateFileBytes = 64 * 1024 * 1024

	// maxSessionStateLineBytes is the maximum size of a single line
	// readSessionUsage will scan, matching the stdout scanner cap the
	// fork-per-turn skeleton already uses.
	maxSessionStateLineBytes = 10 * 1024 * 1024
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
	if v := env("COPILOT_HOME"); v != "" {
		return filepath.Join(v, "session-state"), nil
	}
	dir, err := home()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(dir, ".copilot", "session-state"), nil
}

// readSessionUsage scans the session-state events journal at path and
// returns the totals of the last line whose decoded type is
// "session.shutdown" as current, and the totals of the line before that
// as previous (the zero value when at most one such record exists).
// found is false when the file holds no session.shutdown record.
//
// The read is abandoned and err is non-nil when the file exceeds 64 MB
// or a single line exceeds 10 MB. A line that fails to decode is
// skipped without failing the read. readSessionUsage never logs the
// file's contents.
func readSessionUsage(path string) (current, previous domain.TokenUsage, found bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return domain.TokenUsage{}, domain.TokenUsage{}, false, statErr
	}
	if info.Size() > maxSessionStateFileBytes {
		return domain.TokenUsage{}, domain.TokenUsage{}, false,
			fmt.Errorf("session-state events file exceeds %d bytes: %w", maxSessionStateFileBytes, errSessionStateCapExceeded)
	}

	f, openErr := os.Open(path) //nolint:gosec // path is derived from a validated session id, not user input
	if openErr != nil {
		return domain.TokenUsage{}, domain.TokenUsage{}, false, openErr
	}
	defer f.Close() //nolint:errcheck // read-only handle

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSessionStateLineBytes)

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
			return domain.TokenUsage{}, domain.TokenUsage{}, false,
				fmt.Errorf("session-state events file line exceeds %d bytes: %w", maxSessionStateLineBytes, errSessionStateCapExceeded)
		}
		return domain.TokenUsage{}, domain.TokenUsage{}, false, fmt.Errorf("scan session-state events file: %w", scanErr)
	}

	if len(records) == 0 {
		return domain.TokenUsage{}, domain.TokenUsage{}, false, nil
	}
	current = shutdownTotals(records[len(records)-1])
	if len(records) == 2 {
		previous = shutdownTotals(records[0])
	}
	return current, previous, true, nil
}

// shutdownTotals extracts token totals from one session.shutdown
// record, preferring data.modelMetrics summed across model entries and
// falling back to data.tokenDetails when modelMetrics is absent or
// empty.
func shutdownTotals(evt shutdownEvent) domain.TokenUsage {
	if len(evt.Data.ModelMetrics) > 0 {
		var usage domain.TokenUsage
		for _, model := range evt.Data.ModelMetrics {
			usage.InputTokens += model.Usage.InputTokens
			usage.OutputTokens += model.Usage.OutputTokens
			usage.CacheReadTokens += model.Usage.CacheReadTokens
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		return usage
	}

	details := evt.Data.TokenDetails
	input := details["input"].TokenCount + details["cache_read"].TokenCount + details["cache_write"].TokenCount
	output := details["output"].TokenCount
	return domain.TokenUsage{
		InputTokens:     input,
		OutputTokens:    output,
		TotalTokens:     input + output,
		CacheReadTokens: details["cache_read"].TokenCount,
	}
}

// subtractUsage returns a minus b componentwise, floored at zero, with
// TotalTokens recomputed as InputTokens plus OutputTokens.
func subtractUsage(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:     max(a.InputTokens-b.InputTokens, 0),
		OutputTokens:    max(a.OutputTokens-b.OutputTokens, 0),
		CacheReadTokens: max(a.CacheReadTokens-b.CacheReadTokens, 0),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
}
