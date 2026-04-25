package copilot

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// rawEvent is the intermediate representation of a Copilot CLI JSONL
// line. Fields are populated from JSON and then mapped to domain
// event types by the adapter.
type rawEvent struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Timestamp string          `json:"timestamp,omitempty"`
	ParentID  string          `json:"parentId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Ephemeral bool            `json:"ephemeral,omitempty"`

	// Top-level fields on result events only (no data wrapper).
	SessionID string    `json:"sessionId,omitempty"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	Usage     *rawUsage `json:"usage,omitempty"`
}

type rawUsage struct {
	PremiumRequests int64          `json:"premiumRequests"`
	TotalAPIDurMS   int64          `json:"totalApiDurationMs"`
	SessionDurMS    int64          `json:"sessionDurationMs"`
	CodeChanges     *rawCodeChange `json:"codeChanges,omitempty"`
}

type rawCodeChange struct {
	LinesAdded    int      `json:"linesAdded"`
	LinesRemoved  int      `json:"linesRemoved"`
	FilesModified []string `json:"filesModified"`
}

type assistantMessageData struct {
	MessageID    string           `json:"messageId"`
	Content      string           `json:"content"`
	ToolRequests []rawToolRequest `json:"toolRequests"`
	OutputTokens int64            `json:"outputTokens"`
}

type rawToolRequest struct {
	ToolCallID       string          `json:"toolCallId"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	IntentionSummary string          `json:"intentionSummary,omitempty"`
}

type toolExecutionData struct {
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Model         string          `json:"model,omitempty"`
	InteractionID string          `json:"interactionId,omitempty"`
	Success       bool            `json:"success"`
	Result        json.RawMessage `json:"result,omitempty"`
	ToolTelemetry json.RawMessage `json:"toolTelemetry,omitempty"`
}

type sessionInfoData struct {
	InfoType string `json:"infoType,omitempty"`
	Message  string `json:"message,omitempty"`
}

type sessionWarningData struct {
	WarningType string `json:"warningType,omitempty"`
	Message     string `json:"message,omitempty"`
}

type sessionTaskCompleteData struct {
	Summary string `json:"summary,omitempty"`
	Success bool   `json:"success"`
}

// parseEvent parses a single JSONL line from Copilot CLI stdout into
// a [rawEvent]. Returns an error if JSON parsing fails.
func parseEvent(line []byte) (rawEvent, error) {
	var ev rawEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return rawEvent{}, fmt.Errorf("parse event: %w", err)
	}
	return ev, nil
}

func parseAssistantMessageData(data json.RawMessage) (assistantMessageData, error) {
	var d assistantMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return assistantMessageData{}, fmt.Errorf("parse assistant message data: %w", err)
	}
	return d, nil
}

func parseToolExecutionData(data json.RawMessage) (toolExecutionData, error) {
	var d toolExecutionData
	if err := json.Unmarshal(data, &d); err != nil {
		return toolExecutionData{}, fmt.Errorf("parse tool execution data: %w", err)
	}
	return d, nil
}

func parseSessionInfoData(data json.RawMessage) (sessionInfoData, error) {
	var d sessionInfoData
	if err := json.Unmarshal(data, &d); err != nil {
		return sessionInfoData{}, fmt.Errorf("parse session info data: %w", err)
	}
	return d, nil
}

func parseSessionWarningData(data json.RawMessage) (sessionWarningData, error) {
	var d sessionWarningData
	if err := json.Unmarshal(data, &d); err != nil {
		return sessionWarningData{}, fmt.Errorf("parse session warning data: %w", err)
	}
	return d, nil
}

func parseSessionTaskCompleteData(data json.RawMessage) (sessionTaskCompleteData, error) {
	var d sessionTaskCompleteData
	if err := json.Unmarshal(data, &d); err != nil {
		return sessionTaskCompleteData{}, fmt.Errorf("parse session task complete data: %w", err)
	}
	return d, nil
}

// summarizeAssistantMessage produces a human-readable summary from an
// [assistantMessageData]. If content is non-empty it is truncated to
// 200 runes; otherwise tool requests are listed by name.
func summarizeAssistantMessage(data assistantMessageData) string {
	if data.Content != "" {
		return typeutil.TruncateRunes(strings.TrimSpace(data.Content), 200)
	}
	if len(data.ToolRequests) > 0 {
		names := make([]string, len(data.ToolRequests))
		for i, tr := range data.ToolRequests {
			names[i] = tr.Name
		}
		return fmt.Sprintf("requesting %d tool(s): %s", len(data.ToolRequests), strings.Join(names, ", "))
	}
	return "assistant message"
}
