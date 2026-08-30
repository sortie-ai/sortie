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

// assistantMessageData is the data payload of an assistant.message
// event. OutputTokens is a pointer so a message whose outputTokens
// field is absent from the wire payload is distinguishable from one
// reporting a measured zero.
type assistantMessageData struct {
	MessageID    string           `json:"messageId"`
	APICallID    string           `json:"apiCallId"`
	Content      string           `json:"content"`
	ToolRequests []rawToolRequest `json:"toolRequests"`
	OutputTokens *int64           `json:"outputTokens"`
}

// modelMessageData is the data payload of a model.message event, the
// post-relocation carrier of the per-message output-token count.
type modelMessageData struct {
	Message modelMessage `json:"message"`
}

// modelMessage is the message payload of a model.message event. Role
// selects the assistant-authored records, the only ones that carry a
// count. OutputTokens is a pointer for the same reason it is one on
// assistantMessageData: an absent field is distinguishable from a
// measured zero.
type modelMessage struct {
	Role         string `json:"role"`
	APICallID    string `json:"apiCallId"`
	OutputTokens *int64 `json:"outputTokens"`
}

type rawToolRequest struct {
	ToolCallID       string          `json:"toolCallId"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	IntentionSummary string          `json:"intentionSummary,omitempty"`
}

type toolExecutionData struct {
	ToolCallID    string              `json:"toolCallId"`
	ToolName      string              `json:"toolName"`
	Arguments     json.RawMessage     `json:"arguments,omitempty"`
	Model         string              `json:"model,omitempty"`
	InteractionID string              `json:"interactionId,omitempty"`
	Success       bool                `json:"success"`
	Result        json.RawMessage     `json:"result,omitempty"`
	Error         *toolExecutionError `json:"error,omitempty"`
	ToolTelemetry json.RawMessage     `json:"toolTelemetry,omitempty"`
}

// toolExecutionError is the error payload a tool.execution_complete event
// carries when Success is false. Code "denied" is the CLI's own
// non-interactive permission policy refusing the tool call and continuing
// the session; every other value is an unrelated execution failure.
type toolExecutionError struct {
	Message string `json:"message,omitempty"`
	Code    string `json:"code,omitempty"`
}

type sessionInfoData struct {
	InfoType string `json:"infoType,omitempty"`
	Message  string `json:"message,omitempty"`
}

type sessionWarningData struct {
	WarningType string `json:"warningType,omitempty"`
	Message     string `json:"message,omitempty"`
}

// sessionTaskCompleteData is the data payload of a session.task_complete
// event. Success is a pointer so a payload whose success field is absent
// from the wire payload is distinguishable from one reporting an explicit
// false, the same idiom rawEvent.ExitCode already uses.
type sessionTaskCompleteData struct {
	Summary string `json:"summary,omitempty"`
	Success *bool  `json:"success,omitempty"`
}

// userMessageData is the data payload of a user.message event.
type userMessageData struct {
	IsAutopilotContinuation bool `json:"isAutopilotContinuation,omitempty"`
}

// shutdownEvent is one line of the copilot session-state events journal
// (<session-state root>/<session id>/events.jsonl) whose Type is
// "session.shutdown".
type shutdownEvent struct {
	Type string       `json:"type"`
	Data shutdownData `json:"data"`
}

// shutdownData holds the two alternative token-count shapes a
// session.shutdown record carries. ModelMetrics is preferred when
// present and non-empty; TokenDetails is the fallback.
type shutdownData struct {
	TokenDetails map[string]shutdownTokenCount `json:"tokenDetails,omitempty"`
	ModelMetrics map[string]shutdownModel      `json:"modelMetrics,omitempty"`
}

// shutdownTokenCount is one entry of a session.shutdown record's
// tokenDetails map.
type shutdownTokenCount struct {
	TokenCount int64 `json:"tokenCount"`
}

// shutdownModel is one entry of a session.shutdown record's
// modelMetrics map.
type shutdownModel struct {
	Usage shutdownModelUsage `json:"usage"`
}

// shutdownModelUsage holds one model's token counts from a
// session.shutdown record's modelMetrics entry.
type shutdownModelUsage struct {
	InputTokens     int64 `json:"inputTokens"`
	OutputTokens    int64 `json:"outputTokens"`
	CacheReadTokens int64 `json:"cacheReadTokens"`
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

func parseModelMessageData(data json.RawMessage) (modelMessageData, error) {
	var d modelMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return modelMessageData{}, fmt.Errorf("parse model message data: %w", err)
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

func parseUserMessageData(data json.RawMessage) (userMessageData, error) {
	var d userMessageData
	if err := json.Unmarshal(data, &d); err != nil {
		return userMessageData{}, fmt.Errorf("parse user message data: %w", err)
	}
	return d, nil
}

// completionFailureMessage builds the failure message for a
// session.task_complete report that explicitly declares success: false.
// It returns a fixed message when summary is empty, since an empty
// summary is not itself evidence of anything.
func completionFailureMessage(summary string) string {
	if summary == "" {
		return "agent reported the task complete without success"
	}
	return typeutil.TruncateRunes(summary, 500)
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
