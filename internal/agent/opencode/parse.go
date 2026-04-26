package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

const maxLineBytes = 10 * 1024 * 1024

type parsedLine struct {
	Event     *rawRunEvent
	PlainText string
	Err       error
}

type rawRunEvent struct {
	Type      string          `json:"type"`
	Timestamp int64           `json:"timestamp"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part,omitempty"`
	Error     *rawRunError    `json:"error,omitempty"`
}

type rawRunError struct {
	Name string         `json:"name,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

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
	Cost      float64        `json:"cost,omitempty"`
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
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CacheReadTokens int64
	Model           string
	Cost            float64
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

func queryExportUsage(ctx context.Context, state *sessionState) exportUsage {
	sessionID := state.currentSessionID()
	if sessionID == "" {
		return exportUsage{}
	}

	env, err := buildRunEnv(os.Environ(), state.passthrough)
	if err != nil {
		state.logger().Warn("failed to build opencode export environment", slog.Any("error", err))
		return exportUsage{}
	}

	managedEnv, err := buildManagedEnv(state.passthrough)
	if err != nil {
		state.logger().Warn("failed to build opencode export command", slog.Any("error", err))
		return exportUsage{}
	}

	queryCtx, cancel := context.WithTimeout(ctx, exportTimeout(state))
	defer cancel()

	exportArgs := []string{"export", "--sanitize", sessionID}
	var cmd *exec.Cmd
	if state.target.RemoteCommand != "" {
		remoteCommand := buildSSHRemoteCommand(state.target.RemoteCommand, managedEnv)
		sshArgs := sshutil.BuildSSHArgs(
			state.target.SSHHost,
			state.target.WorkspacePath,
			remoteCommand,
			exportArgs,
			sshutil.SSHOptions{StrictHostKeyChecking: state.target.SSHStrictHostKeyChecking},
		)
		cmd = exec.CommandContext(queryCtx, state.target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		allArgs := append(slices.Clone(state.target.Args), exportArgs...)
		cmd = exec.CommandContext(queryCtx, state.target.Command, allArgs...) //nolint:gosec // args are constructed programmatically
	}
	cmd.Dir = state.target.WorkspacePath
	cmd.Env = env

	stdout, err := cmd.Output()
	if err != nil {
		state.logger().Warn("failed to export opencode usage", slog.Any("error", err))
		return exportUsage{}
	}

	usage := parseExportOutput(stdout, sessionID)
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		state.logger().Warn("no assistant token usage found in opencode export")
	}
	return usage
}

// parseExportOutput extracts token usage from the JSON returned by
// opencode export, scanning messages in reverse to find the most recent
// assistant message for sessionID. Returns zero exportUsage on any parse
// failure or when no matching message is found.
func parseExportOutput(data []byte, sessionID string) exportUsage {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return exportUsage{}
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return exportUsage{}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		info := mapFromAny(message["info"])
		if info == nil {
			continue
		}
		if stringFromAny(info["role"]) != "assistant" {
			continue
		}
		if stringFromAny(info["sessionID"]) != sessionID {
			continue
		}
		tokens := mapFromAny(info["tokens"])
		if tokens == nil {
			continue
		}
		inputTokens, ok := int64FromAny(tokens["input"])
		if !ok {
			continue
		}
		outputTokens, ok := int64FromAny(tokens["output"])
		if !ok {
			continue
		}
		totalTokens := inputTokens + outputTokens
		if total, ok := int64FromAny(tokens["total"]); ok {
			totalTokens = total
		}
		var cacheReadTokens int64
		if cache := mapFromAny(tokens["cache"]); cache != nil {
			if read, ok := int64FromAny(cache["read"]); ok {
				cacheReadTokens = read
			}
		}
		var model string
		providerID := stringFromAny(info["providerID"])
		modelID := stringFromAny(info["modelID"])
		if providerID != "" && modelID != "" {
			model = providerID + "/" + modelID
		}
		cost, _ := float64FromAny(info["cost"])
		return exportUsage{
			InputTokens:     inputTokens,
			OutputTokens:    outputTokens,
			TotalTokens:     totalTokens,
			CacheReadTokens: cacheReadTokens,
			Model:           model,
			Cost:            cost,
		}
	}
	return exportUsage{}
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

func float64FromAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		value, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return value, true
	default:
		return 0, false
	}
}
