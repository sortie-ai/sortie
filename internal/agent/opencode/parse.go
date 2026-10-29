package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/opencode/sessionexport"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

type rawRunEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part,omitempty"`
	Error     *rawRunError    `json:"error,omitempty"`
}

type rawRunError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
	Status  any    `json:"status,omitempty"` // a JSON number when present

	// RetryAfterMs is the delay in milliseconds after the failure at which
	// the runtime expects a retry to succeed.
	RetryAfterMs *int64 `json:"retryAfterMs"`
}

const (
	// freeTierAuthType is the error type a provider authorization
	// refusal, free-tier refusals included, carries.
	freeTierAuthType = "provider.auth"
	// freeTierMessageMarker is the substring that distinguishes a
	// free-tier refusal from any other provider's authorization refusal
	// under the same error type.
	freeTierMessageMarker = "free tier can only be used from within OpenCode"
)

// freeTierRequiredTools lists, in the order [freeTierRefusalClause]
// reports them, the tools the hosted free tier requires in a session's
// tool set.
var freeTierRequiredTools = [...]string{"bash", "read"}

type rawPartTime struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

type rawStepStartPart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageID"`
	SessionID string `json:"sessionID"`
	Snapshot  string `json:"snapshot,omitempty"`
	Type      string `json:"type"`
}

type rawTextPart struct {
	ID        string      `json:"id"`
	MessageID string      `json:"messageID"`
	SessionID string      `json:"sessionID"`
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Time      rawPartTime `json:"time,omitempty"`
}

type rawReasoningPart struct {
	ID        string      `json:"id"`
	MessageID string      `json:"messageID"`
	SessionID string      `json:"sessionID"`
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Time      rawPartTime `json:"time,omitempty"`
}

type rawToolPart struct {
	ID        string       `json:"id"`
	MessageID string       `json:"messageID"`
	SessionID string       `json:"sessionID"`
	Type      string       `json:"type"`
	Tool      string       `json:"tool"`
	CallID    string       `json:"callID"`
	State     rawToolState `json:"state"`
}

// toolMappings gives the kind and main-argument member of each OpenCode 2.x
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"shell": {Kind: domain.ToolKindShell, Key: "command"},
	"read":  {Kind: domain.ToolKindRead, Key: "path"},
	"write": {Kind: domain.ToolKindEdit, Key: "path"},
	"edit":  {Kind: domain.ToolKindEdit, Key: "path"},
	"skill": {Kind: domain.ToolKindSkill, Key: "id"},
}

type rawToolState struct {
	Status   string          `json:"status"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   any             `json:"output,omitempty"`
	Metadata any             `json:"metadata,omitempty"`
	Error    string          `json:"error,omitempty"`
	Title    string          `json:"title,omitempty"`
	Time     rawPartTime     `json:"time,omitempty"`
}

type rawStepFinishPart struct {
	ID        string         `json:"id"`
	MessageID string         `json:"messageID"`
	SessionID string         `json:"sessionID"`
	Type      string         `json:"type"`
	Reason    string         `json:"reason"`
	Tokens    *rawStepTokens `json:"tokens,omitempty"`
}

// rawStepTokens keeps each count raw so that an absent, null, or non-integer
// member stays distinguishable from a zero.
type rawStepTokens struct {
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Reasoning json.RawMessage `json:"reasoning,omitempty"`
	Cache     rawCacheUsage   `json:"cache,omitempty"`
}

type rawCacheUsage struct {
	Read  json.RawMessage `json:"read,omitempty"`
	Write json.RawMessage `json:"write,omitempty"`
}

type exportUsage struct {
	Usage domain.TokenUsage
	Model string

	// ByModel splits the totals by each kept message's `providerID/modelID`;
	// a message missing either part adds to no entry.
	ByModel map[string]domain.TokenUsage

	// Recovered reports whether the export produced a figure at all,
	// which is not the same question as whether that figure is non-zero.
	// The runtime's assistant message carries `tokens` as a required
	// object rather than one present only when something was spent, so a
	// turn that genuinely cost zero exports the same shape as any other
	// and must not read back as "nothing was recovered".
	Recovered bool
}

func parseRunEvent(line []byte) (rawRunEvent, error) {
	var event rawRunEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return rawRunEvent{}, fmt.Errorf("parse run event: %w", err)
	}
	return event, nil
}

func parseStepStartPart(raw json.RawMessage) (rawStepStartPart, error) {
	var part rawStepStartPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawStepStartPart{}, err
	}
	return part, nil
}

func parseTextPart(raw json.RawMessage) (rawTextPart, error) {
	var part rawTextPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawTextPart{}, err
	}
	return part, nil
}

func parseReasoningPart(raw json.RawMessage) (rawReasoningPart, error) {
	var part rawReasoningPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawReasoningPart{}, err
	}
	return part, nil
}

func parseToolPart(raw json.RawMessage) (rawToolPart, error) {
	var part rawToolPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawToolPart{}, err
	}
	return part, nil
}

func parseStepFinishPart(raw json.RawMessage) (rawStepFinishPart, error) {
	var part rawStepFinishPart
	if err := json.Unmarshal(raw, &part); err != nil {
		return rawStepFinishPart{}, err
	}
	return part, nil
}

// stepSpendReading builds the reading of one step report. The runtime sends
// reasoning and cache counts only above zero, so those read zero when absent;
// input and output are always sent, so a missing one is unknown.
func stepSpendReading(t *rawStepTokens) usage.SpendReading {
	return usage.SpendReading{
		Input:      sessionexport.Count(t.Input),
		Output:     sessionexport.Count(t.Output),
		Reasoning:  countOmittedAtZero(t.Reasoning),
		CacheRead:  countOmittedAtZero(t.Cache.Read),
		CacheWrite: countOmittedAtZero(t.Cache.Write),
	}
}

func countOmittedAtZero(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		var zero int64
		return &zero
	}
	return sessionexport.Count(raw)
}

// freeTierRefusalClause returns a clause naming the free-tier-required
// tools state's policy denies, or "" when runErr does not match the
// free-tier refusal envelope, or the policy denies neither tool.
func freeTierRefusalClause(runErr *rawRunError, pt passthroughConfig) string {
	if runErr == nil || !isFreeTierRefusal(runErr) {
		return ""
	}

	policy, ok := buildPermissionPolicy(pt)
	if !ok {
		return ""
	}

	var denied []string
	for _, tool := range freeTierRequiredTools {
		if policy[tool] == permissionDeny {
			denied = append(denied, tool)
		}
	}

	switch len(denied) {
	case 0:
		return ""
	case 1:
		return "; the opencode.allowed_tools and opencode.denied_tools settings deny " + denied[0] +
			", a tool the runtime's free tier requires"
	default:
		return "; the opencode.allowed_tools and opencode.denied_tools settings deny " +
			strings.Join(denied, " and ") + ", tools the runtime's free tier requires"
	}
}

// isFreeTierRefusal reports whether runErr matches the free-tier
// refusal envelope.
func isFreeTierRefusal(runErr *rawRunError) bool {
	if runErr.Type != freeTierAuthType {
		return false
	}
	status, ok := runErr.Status.(float64)
	if !ok || status != 403 {
		return false
	}
	return strings.Contains(runErr.Message, freeTierMessageMarker)
}

// parseSessionExport extracts run-cumulative token usage from the
// `session export --standalone --sanitize` document, summing over every
// finished assistant message. When sinceUnixMS is non-zero, only messages
// created at or after it are counted. A message without a tokens object
// is skipped. The reported model comes from the last kept message that names
// one. Returns the zero exportUsage unless the document decodes, its info.id
// equals sessionID, and every kept message carries all its counts.
func parseSessionExport(data []byte, sessionID string, sinceUnixMS int64) exportUsage {
	records, err := sessionexport.Parse(data, sessionID)
	if err != nil {
		return exportUsage{}
	}

	var sum exportUsage
	kept := false
	for _, record := range records {
		if sinceUnixMS != 0 && record.CreatedMS < sinceUnixMS {
			continue
		}
		messageUsage, ok := usage.Normalize(record.Spend, usageSources.Records.Spend)
		if !ok {
			return exportUsage{}
		}
		sum.Usage = agentcore.AddUsage(sum.Usage, messageUsage)
		kept = true

		if record.Spend.Model != "" {
			sum.Model = record.Spend.Model
			if sum.ByModel == nil {
				sum.ByModel = map[string]domain.TokenUsage{}
			}
			sum.ByModel[sum.Model] = agentcore.AddUsage(sum.ByModel[sum.Model], messageUsage)
		}
	}
	if !kept {
		return exportUsage{}
	}

	sum.Recovered = true
	return sum
}

func queryExportUsage(ctx context.Context, state *sessionState, sinceUnixMS int64) exportUsage {
	sessionID := state.currentSessionID()
	if sessionID == "" {
		return exportUsage{}
	}

	queryCtx, cancel := context.WithTimeout(ctx, agentcore.AuxiliaryTimeout(state.agentConfig))
	defer cancel()

	cmd, err := auxiliaryCommand(queryCtx, state, sessionexport.Args(sessionID))
	if err != nil {
		state.logger().Warn("failed to build opencode export environment", slog.Any("error", err))
		return exportUsage{}
	}

	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(queryCtx, cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
		Stdout: &stdout,
		Logger: state.logger(),
	})
	if startErr != nil || result.WaitErr != nil {
		err := startErr
		if err == nil {
			err = result.WaitErr
		}
		state.logger().Warn("failed to export opencode usage", slog.Any("error", err))
		return exportUsage{}
	}

	exported := parseSessionExport(stdout.Bytes(), sessionID, sinceUnixMS)
	if !exported.Recovered {
		state.logger().Warn("no assistant token usage found in opencode export")
	}
	return exported
}
