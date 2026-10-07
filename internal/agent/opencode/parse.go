package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

type parsedLine struct {
	Event     *rawRunEvent
	PlainText string
}

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

type rawToolState struct {
	Status   string      `json:"status"`
	Input    any         `json:"input,omitempty"`
	Output   any         `json:"output,omitempty"`
	Metadata any         `json:"metadata,omitempty"`
	Error    string      `json:"error,omitempty"`
	Title    string      `json:"title,omitempty"`
	Time     rawPartTime `json:"time,omitempty"`
}

type rawStepFinishPart struct {
	ID        string         `json:"id"`
	MessageID string         `json:"messageID"`
	SessionID string         `json:"sessionID"`
	Type      string         `json:"type"`
	Reason    string         `json:"reason"`
	Tokens    *rawStepTokens `json:"tokens,omitempty"`
}

type rawStepTokens struct {
	Total     int64         `json:"total,omitempty"`
	Input     int64         `json:"input,omitempty"`
	Output    int64         `json:"output,omitempty"`
	Reasoning int64         `json:"reasoning,omitempty"`
	Cache     rawCacheUsage `json:"cache,omitempty"`
}

type rawCacheUsage struct {
	Read  int64 `json:"read,omitempty"`
	Write int64 `json:"write,omitempty"`
}

type exportUsage struct {
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Model            string

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

// freeTierRefusalClause returns a clause naming the free-tier-required
// tools state's policy denies, or "" when runErr does not match the
// free-tier refusal envelope, or the policy denies neither tool. It reads no member of runErr beyond what identifies the
// refusal shape, starts no subprocess, and emits no event.
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
// is skipped. The reported model comes from the last kept message.
// Returns the zero exportUsage unless the document decodes and its
// info.id equals sessionID.
func parseSessionExport(data []byte, sessionID string, sinceUnixMS int64) exportUsage {
	var payload struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return exportUsage{}
	}
	if payload.Info.ID != sessionID {
		return exportUsage{}
	}

	var sum exportUsage
	kept := false
	for _, message := range payload.Messages {
		if stringFromAny(message["type"]) != "assistant" {
			continue
		}
		if stringFromAny(message["finish"]) == "" {
			continue
		}
		if sinceUnixMS != 0 {
			created, ok := messageCreatedMS(message)
			if !ok || created < sinceUnixMS {
				continue
			}
		}
		tokens := mapFromAny(message["tokens"])
		if tokens == nil {
			continue
		}

		inputTokens, _ := int64FromAny(tokens["input"])
		outputTokens, _ := int64FromAny(tokens["output"])
		var reasoningTokens int64
		if reasoning, ok := int64FromAny(tokens["reasoning"]); ok {
			reasoningTokens = reasoning
		}
		var cacheReadTokens, cacheWriteTokens int64
		if cache := mapFromAny(tokens["cache"]); cache != nil {
			if read, ok := int64FromAny(cache["read"]); ok {
				cacheReadTokens = read
			}
			if write, ok := int64FromAny(cache["write"]); ok {
				cacheWriteTokens = write
			}
		}

		sum.InputTokens += inputTokens + cacheReadTokens + cacheWriteTokens
		sum.OutputTokens += outputTokens + reasoningTokens
		sum.CacheReadTokens += cacheReadTokens
		sum.CacheWriteTokens += cacheWriteTokens
		kept = true

		model := mapFromAny(message["model"])
		providerID := stringFromAny(model["providerID"])
		modelID := stringFromAny(model["id"])
		if providerID != "" && modelID != "" {
			sum.Model = providerID + "/" + modelID
		}
	}
	if !kept {
		return exportUsage{}
	}

	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
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

	cmd, err := auxiliaryCommand(queryCtx, state, exportArgs(sessionID))
	if err != nil {
		state.logger().Warn("failed to build opencode export environment", slog.Any("error", err))
		return exportUsage{}
	}

	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{
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

	usage := parseSessionExport(stdout.Bytes(), sessionID, sinceUnixMS)
	if !usage.Recovered {
		state.logger().Warn("no assistant token usage found in opencode export")
	}
	return usage
}

// messageCreatedMS extracts time.created (Unix milliseconds) from an
// assistant message. ok is false when the field is absent
// or not parseable as a number.
func messageCreatedMS(info map[string]any) (createdMS int64, ok bool) {
	t := mapFromAny(info["time"])
	if t == nil {
		return 0, false
	}
	return int64FromAny(t["created"])
}

func mapFromAny(value any) map[string]any {
	if value == nil {
		return nil
	}
	typed, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return typed
}

func stringFromAny(value any) string {
	str, ok := value.(string)
	if !ok {
		return ""
	}
	return str
}

func int64FromAny(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		value, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return value, true
	default:
		return 0, false
	}
}
