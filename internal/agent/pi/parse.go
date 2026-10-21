package pi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// The wire schema of `pi --mode json`, as pi 1.3.0 writes it. One JSON object per
// stdout line. The session header comes first, then one AgentSessionEvent
// per line as the session runs:
//
//	{"type":"session","version":3,"id":"<uuid>","timestamp":"...","cwd":"<dir>"}
//	{"type":"agent_start"}
//	{"type":"turn_start"}
//	{"type":"message_start","message":{...}}
//	{"type":"message_update","usage":{...},"assistantMessageEvent":{...}}
//	{"type":"message_end","message":{...}}
//	{"type":"tool_execution_start","toolCallId":"...","toolName":"...","args":{...}}
//	{"type":"tool_execution_update","toolCallId":"...","partialResult":{...}}
//	{"type":"tool_execution_end","toolCallId":"...","toolName":"...","result":{...},"isError":false}
//	{"type":"turn_end","message":{...},"toolResults":[...]}
//	{"type":"agent_end","messages":[...],"willRetry":false}
//
// Two shapes decide how this package reads a line. message_update is
// delta-only: the CLI strips both its cumulative `message` and the
// assistantMessageEvent's `partial`, so only the event's own fields carry
// information. And pi --mode json applies no exit-code check to a failed
// response, so a response that stopped with "error" or "aborted" still
// exits 0; only the message's stopReason distinguishes it.

// knownEventTypes is every event type the pi CLI writes to the run
// stream, framing and content alike. A type outside this set is a stream
// the adapter does not understand, and is reported as a malformed line
// rather than skipped: a future event the adapter silently drops could
// carry the only text, the only usage, or the only failure the turn
// produced.
var knownEventTypes = map[string]struct{}{
	"session":                           {},
	"agent_start":                       {},
	"agent_end":                         {},
	"agent_settled":                     {},
	"turn_start":                        {},
	"turn_end":                          {},
	"message_start":                     {},
	"message_update":                    {},
	"message_end":                       {},
	"tool_execution_start":              {},
	"tool_execution_update":             {},
	"tool_execution_end":                {},
	"bash_execution_update":             {},
	"queue_update":                      {},
	"entry_appended":                    {},
	"session_info_changed":              {},
	"thinking_level_changed":            {},
	"compaction_start":                  {},
	"compaction_end":                    {},
	"auto_retry_start":                  {},
	"auto_retry_end":                    {},
	"summarization_retry_scheduled":     {},
	"summarization_retry_attempt_start": {},
	"summarization_retry_finished":      {},
}

// knownDeltaTypes is every assistantMessageEvent variant the pi CLI
// writes inside a message_update. It is bounded for the same reason
// knownEventTypes is.
var knownDeltaTypes = map[string]struct{}{
	"start":          {},
	"text_start":     {},
	"text_delta":     {},
	"text_end":       {},
	"thinking_start": {},
	"thinking_delta": {},
	"thinking_end":   {},
	"toolcall_start": {},
	"toolcall_delta": {},
	"toolcall_end":   {},
	"done":           {},
	"error":          {},
}

// parsedLine is one stdout line's outcome: exactly one of the two fields
// is set. PlainText carries a line that is not a JSON event object, so
// the caller can report it verbatim.
type parsedLine struct {
	Event     *rawRunEvent
	PlainText string
}

type rawRunEvent struct {
	Type string `json:"type"`

	ID      string `json:"id,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Version int    `json:"version,omitempty"`

	Message     json.RawMessage `json:"message,omitempty"`
	Messages    json.RawMessage `json:"messages,omitempty"`
	ToolResults json.RawMessage `json:"toolResults,omitempty"`

	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent,omitempty"`

	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	DurationMS int64           `json:"durationMs,omitempty"`

	WillRetry bool   `json:"willRetry,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// rawMessage is an AgentMessage as the pi CLI serializes it. Only
// the assistant role carries usage, a model name, and a stop reason.
type rawMessage struct {
	Role         string       `json:"role"`
	Content      []rawContent `json:"content"`
	Usage        *rawUsage    `json:"usage,omitempty"`
	Model        string       `json:"model,omitempty"`
	Provider     string       `json:"provider,omitempty"`
	StopReason   string       `json:"stopReason,omitempty"`
	ErrorMessage string       `json:"errorMessage,omitempty"`
}

type rawContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// rawUsage is pi-ai's per-response Usage. Output already includes
// Reasoning, so the two are never added together.
type rawUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

// rawAssistantDelta is the assistantMessageEvent of a message_update.
type rawAssistantDelta struct {
	Type         string `json:"type"`
	ContentIndex int    `json:"contentIndex"`
	Delta        string `json:"delta,omitempty"`
	Content      string `json:"content,omitempty"`
	ToolName     string `json:"toolName,omitempty"`
	Reason       string `json:"reason,omitempty"`

	ToolCall *struct {
		Name string `json:"name"`
	} `json:"toolCall,omitempty"`
}

// parseRunEvent decodes one run-stream line. A line that is not a JSON
// object carrying a type is a fault, not an event.
func parseRunEvent(line []byte) (rawRunEvent, error) {
	var event rawRunEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return rawRunEvent{}, fmt.Errorf("parse run event: %w", err)
	}
	if event.Type == "" {
		return rawRunEvent{}, fmt.Errorf("parse run event: no type field")
	}
	return event, nil
}

// known reports whether the CLI's schema defines this event type.
func (e *rawRunEvent) known() bool {
	_, ok := knownEventTypes[e.Type]
	return ok
}

// sessionHeader returns the id and cwd a session header carries. The id
// is the session's identity for every later resume; the cwd is the
// directory pi works in, which for a resumed session is the header's
// own rather than the one the process was launched in.
func (e *rawRunEvent) sessionHeader() (id, cwd string, err error) {
	if e.Type != "session" {
		return "", "", fmt.Errorf("event %q is not a session header", e.Type)
	}
	if e.ID == "" {
		return "", "", fmt.Errorf("session header carries no id")
	}
	if e.CWD == "" {
		return "", "", fmt.Errorf("session header %q carries no cwd", e.ID)
	}
	return e.ID, e.CWD, nil
}

// completedMessage decodes the message a message_end or turn_end event
// carries. Both name the same assistant response for one pi turn, so
// the caller picks which of the two it treats as authoritative.
func (e *rawRunEvent) completedMessage() (rawMessage, error) {
	if e.Type != "message_end" && e.Type != "turn_end" {
		return rawMessage{}, fmt.Errorf("event %q carries no completed message", e.Type)
	}
	var msg rawMessage
	if err := decodeStrict(e.Message, &struct {
		Role *string `json:"role"`
	}{&msg.Role}); err != nil {
		return rawMessage{}, fmt.Errorf("event %q message: %w", e.Type, err)
	}
	if msg.Role == "" {
		return rawMessage{}, fmt.Errorf("event %q message carries no role", e.Type)
	}
	if msg.Role == "system" {
		// The system message's content is a string and carries nothing
		// the adapter reads.
		return msg, nil
	}
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return rawMessage{}, fmt.Errorf("event %q message: %w", e.Type, err)
	}
	return msg, nil
}

// assistantDelta decodes the assistantMessageEvent of a message_update.
// Every delta the pi CLI writes is one of knownDeltaTypes; anything
// else is a variant the adapter does not understand.
func (e *rawRunEvent) assistantDelta() (rawAssistantDelta, error) {
	if e.Type != "message_update" {
		return rawAssistantDelta{}, fmt.Errorf("event %q carries no assistant message event", e.Type)
	}
	var delta rawAssistantDelta
	if err := decodeStrict(e.AssistantMessageEvent, &delta); err != nil {
		return rawAssistantDelta{}, fmt.Errorf("message_update assistantMessageEvent: %w", err)
	}
	if delta.Type == "" {
		return rawAssistantDelta{}, fmt.Errorf("message_update assistantMessageEvent carries no type")
	}
	if _, ok := knownDeltaTypes[delta.Type]; !ok {
		return rawAssistantDelta{}, fmt.Errorf("message_update assistantMessageEvent type %q is not part of the pi schema", delta.Type)
	}
	return delta, nil
}

// failure returns the message describing an assistant response that
// stopped in failure, or "" when the response completed normally.
func (m rawMessage) failure() string {
	if m.StopReason != "error" && m.StopReason != "aborted" {
		return ""
	}
	if m.ErrorMessage != "" {
		return m.ErrorMessage
	}
	return "pi reported stop reason " + m.StopReason
}

// text concatenates the response's text blocks. A thinking block is not
// one: it is internal deliberation, and pi already counts its reasoning
// tokens inside the response's output usage.
func (m rawMessage) text() string {
	var sb strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// tokenUsage normalizes one response's usage. Reasoning is a subset of
// output, so it is never added a second time, and CacheRead is reported
// both as part of the input total and as its own subset counter.
func (u rawUsage) tokenUsage() domain.TokenUsage {
	usage := domain.TokenUsage{
		InputTokens:     u.Input + u.CacheRead + u.CacheWrite,
		OutputTokens:    u.Output,
		CacheReadTokens: u.CacheRead,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return usage
}

// addUsage returns the componentwise sum of two usage figures, with
// TotalTokens recomputed.
func addUsage(a, b domain.TokenUsage) domain.TokenUsage {
	sum := domain.TokenUsage{
		InputTokens:     a.InputTokens + b.InputTokens,
		OutputTokens:    a.OutputTokens + b.OutputTokens,
		CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens,
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	return sum
}

// decodeStrict decodes raw into target, rejecting a payload the event's
// type says must be present. The event families that carry a nested
// object use it so a payload that fails to decode becomes a fault the
// caller reports, rather than a zero value the caller reads as a
// successful empty record.
func decodeStrict(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing payload")
	}
	return json.Unmarshal(raw, target)
}

// resultText concatenates the text blocks of the result a
// tool_execution_end carries, which for a failed tool is its error
// message. It returns "" when the result carries no text.
func (e *rawRunEvent) resultText() string {
	var result struct {
		Content []rawContent `json:"content"`
	}
	if err := json.Unmarshal(e.Result, &result); err != nil {
		return ""
	}
	return rawMessage{Content: result.Content}.text()
}
