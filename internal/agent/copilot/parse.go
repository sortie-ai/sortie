package copilot

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
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

// rawUsage holds usage counters from the result event.
type rawUsage struct {
	PremiumRequests int64          `json:"premiumRequests"`
	TotalAPIDurMS   int64          `json:"totalApiDurationMs"`
	SessionDurMS    int64          `json:"sessionDurationMs"`
	CodeChanges     *rawCodeChange `json:"codeChanges,omitempty"`
}

// rawCodeChange holds code change stats from the result event.
type rawCodeChange struct {
	LinesAdded    int      `json:"linesAdded"`
	LinesRemoved  int      `json:"linesRemoved"`
	FilesModified []string `json:"filesModified"`
}

// assistantMessageData holds fields parsed from assistant.message data.
type assistantMessageData struct {
	MessageID    string           `json:"messageId"`
	Content      string           `json:"content"`
	ToolRequests []rawToolRequest `json:"toolRequests"`
	OutputTokens int64            `json:"outputTokens"`
}

// rawToolRequest holds a tool request inside an assistant.message.
type rawToolRequest struct {
	ToolCallID       string          `json:"toolCallId"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	IntentionSummary string          `json:"intentionSummary,omitempty"`
}

// toolExecutionData holds fields parsed from tool.execution_start
// and tool.execution_complete events.
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

// sessionInfoData holds fields from session.info or session.warning events.
type sessionInfoData struct {
	InfoType string `json:"infoType,omitempty"`
	Message  string `json:"message,omitempty"`
}

// sessionWarningData holds fields from session.warning events.
type sessionWarningData struct {
	WarningType string `json:"warningType,omitempty"`
	Message     string `json:"message,omitempty"`
}

// sessionTaskCompleteData holds fields from session.task_complete events.
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

// parseAssistantMessageData extracts assistant message fields from a
// rawEvent Data payload.
func parseAssistantMessageData(data json.RawMessage) (assistantMessageData, error) {
	var d assistantMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return assistantMessageData{}, fmt.Errorf("parse assistant message data: %w", err)
	}
	return d, nil
}

// parseToolExecutionData extracts tool execution fields from a
// rawEvent Data payload.
func parseToolExecutionData(data json.RawMessage) (toolExecutionData, error) {
	var d toolExecutionData
	if err := json.Unmarshal(data, &d); err != nil {
		return toolExecutionData{}, fmt.Errorf("parse tool execution data: %w", err)
	}
	return d, nil
}

// parseSessionInfoData extracts session info fields from a rawEvent
// Data payload.
func parseSessionInfoData(data json.RawMessage) (sessionInfoData, error) {
	var d sessionInfoData
	if err := json.Unmarshal(data, &d); err != nil {
		return sessionInfoData{}, fmt.Errorf("parse session info data: %w", err)
	}
	return d, nil
}

// parseSessionWarningData extracts session warning fields from a
// rawEvent Data payload.
func parseSessionWarningData(data json.RawMessage) (sessionWarningData, error) {
	var d sessionWarningData
	if err := json.Unmarshal(data, &d); err != nil {
		return sessionWarningData{}, fmt.Errorf("parse session warning data: %w", err)
	}
	return d, nil
}

// parseSessionTaskCompleteData extracts task completion fields from a
// rawEvent Data payload.
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
		return truncate(strings.TrimSpace(data.Content), 200)
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

// normalizeUsage converts raw usage counters and cumulative output
// token data into a [domain.TokenUsage]. Input tokens are not
// available from Copilot CLI JSONL output, so only output tokens are
// populated. raw is accepted for forward compatibility with future
// Copilot CLI versions that may include per-token counts in the result
// event.
func normalizeUsage(_ *rawUsage, cumulativeOutput int64) domain.TokenUsage {
	return domain.TokenUsage{
		InputTokens:     0,
		OutputTokens:    cumulativeOutput,
		TotalTokens:     cumulativeOutput,
		CacheReadTokens: 0,
	}
}

// truncate returns s truncated to maxLen runes with a "…" suffix if
// truncation occurred.
func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen]) + "…"
}
