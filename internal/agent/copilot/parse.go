package copilot

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
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
// event.
type assistantMessageData struct {
	MessageID    string           `json:"messageId"`
	Content      string           `json:"content"`
	ToolRequests []rawToolRequest `json:"toolRequests"`
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
	MCPServerName string              `json:"mcpServerName,omitempty"`
	MCPToolName   string              `json:"mcpToolName,omitempty"`
	Arguments     json.RawMessage     `json:"arguments,omitempty"`
	Model         string              `json:"model,omitempty"`
	InteractionID string              `json:"interactionId,omitempty"`
	Success       bool                `json:"success"`
	Result        json.RawMessage     `json:"result,omitempty"`
	Error         *toolExecutionError `json:"error,omitempty"`
	ToolTelemetry json.RawMessage     `json:"toolTelemetry,omitempty"`
}

// toolMappings gives the kind and main-argument member of each Copilot CLI
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"bash":   {Kind: domain.ToolKindShell, Key: "command"},
	"view":   {Kind: domain.ToolKindRead, Key: "path"},
	"create": {Kind: domain.ToolKindEdit, Key: "path"},
	"edit":   {Kind: domain.ToolKindEdit, Key: "path"},
	"skill":  {Kind: domain.ToolKindSkill, Key: "skill"},
}

// toolCall maps a tool.execution_start payload to a tool call. An MCP call
// is named from the server and tool members because toolName joins them
// lossily.
func (d toolExecutionData) toolCall() agentcore.ToolCall {
	if d.MCPServerName != "" && d.MCPToolName != "" {
		return agentcore.ToolCall{Name: agentcore.QualifiedToolName(d.MCPServerName, d.MCPToolName)}
	}
	return agentcore.MapToolCall(toolMappings, d.ToolName, d.Arguments)
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
	return redact.Truncate(summary, 500)
}

// summarizeAssistantMessage returns the message content truncated to 200
// runes, or "" when there is none. Tool requests are reported as tool_result
// events, so listing them here would only repeat them in the run timeline.
func summarizeAssistantMessage(data assistantMessageData) string {
	return redact.Truncate(strings.TrimSpace(data.Content), 200)
}
