package pi

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// The wire schema of `pi --mode json`, as pi writes it. One JSON object per
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

// toolMappings gives the kind and main-argument member of each built-in pi
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"bash":  {Kind: domain.ToolKindShell, Key: "command"},
	"read":  {Kind: domain.ToolKindRead, Key: "path"},
	"edit":  {Kind: domain.ToolKindEdit, Key: "path"},
	"write": {Kind: domain.ToolKindEdit, Key: "path"},
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
	Entry                 json.RawMessage `json:"entry,omitempty"`

	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	DurationMS int64           `json:"durationMs,omitempty"`

	WillRetry    bool   `json:"willRetry,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Aborted      bool   `json:"aborted,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// rawMessage is an AgentMessage as the pi CLI serializes it. Only
// the assistant role carries usage, a model name, and a stop reason.
type rawMessage struct {
	Role          string       `json:"role"`
	Content       []rawContent `json:"content"`
	Usage         *rawUsage    `json:"usage,omitempty"`
	Model         string       `json:"model,omitempty"`
	ResponseModel string       `json:"responseModel,omitempty"`
	Provider      string       `json:"provider,omitempty"`
	StopReason    string       `json:"stopReason,omitempty"`
	ErrorMessage  string       `json:"errorMessage,omitempty"`
	ResponseID    string       `json:"responseId,omitempty"`
	Timestamp     int64        `json:"timestamp,omitempty"`
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
	if msg.Role != "assistant" {
		// Only an assistant response carries what the adapter reads. The
		// content of any other role may be a string.
		return msg, nil
	}
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return rawMessage{}, fmt.Errorf("event %q message: %w", e.Type, err)
	}
	return msg, nil
}

// cacheWarmResponse decodes the entry an entry_appended event carries when
// it is the usage record of a cache-warming request. The record has the
// counts and the serving model of an assistant response, and its entry id
// is the request's identity. Any other entry yields false.
func (e *rawRunEvent) cacheWarmResponse() (rawMessage, bool, error) {
	var entry struct {
		Type     string          `json:"type"`
		ID       string          `json:"id"`
		Kind     string          `json:"kind"`
		Provider string          `json:"provider"`
		Model    string          `json:"model"`
		Usage    json.RawMessage `json:"usage"`
	}
	if err := decodeStrict(e.Entry, &entry); err != nil {
		return rawMessage{}, false, fmt.Errorf("entry_appended entry: %w", err)
	}
	if entry.Type != "usage" || entry.Kind != "cache_warm" {
		return rawMessage{}, false, nil
	}
	msg := rawMessage{Role: "assistant", Provider: entry.Provider, Model: entry.Model, ResponseID: entry.ID}
	if err := decodeStrict(entry.Usage, &msg.Usage); err != nil {
		return rawMessage{}, false, fmt.Errorf("entry_appended cache_warm usage: %w", err)
	}
	return msg, true, nil
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

// modelName returns the provider-qualified model that served the response.
// pi reports responseModel only when the provider answered with a model other
// than the one requested, as an alias or a router does.
func (m rawMessage) modelName() string {
	model := m.Model
	if m.ResponseModel != "" {
		model = m.ResponseModel
	}
	if m.Provider == "" || model == "" {
		return model
	}
	return m.Provider + "/" + model
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

// hasOutput reports whether the response carries a content block other than
// an empty text block.
func (m rawMessage) hasOutput() bool {
	for _, c := range m.Content {
		if c.Type != "text" || c.Text != "" {
			return true
		}
	}
	return false
}

// usageReading is the response's request report. The same response arrives
// in message_end and again in turn_end, so its identity is the response id
// the provider assigned, or the response's own timestamp when it assigned
// none. A response with no usage, or with every count zero as a failed
// request reports, has no report.
func (m rawMessage) usageReading() (usage.SpendReading, bool) {
	if m.Usage == nil || *m.Usage == (rawUsage{}) {
		return usage.SpendReading{}, false
	}
	identity := m.ResponseID
	if identity == "" && m.Timestamp != 0 {
		identity = strconv.FormatInt(m.Timestamp, 10)
	}
	return usage.SpendReading{
		Input:      &m.Usage.Input,
		Output:     &m.Usage.Output,
		CacheRead:  &m.Usage.CacheRead,
		CacheWrite: &m.Usage.CacheWrite,
		Model:      m.modelName(),
		Identity:   identity,
	}, true
}

// response is the largest counts read for one response identity and the
// model that served it.
type response struct {
	model string
	usage rawUsage
}

// holdResponse keeps the largest counts read for identity this turn.
func (s *sessionState) holdResponse(msg rawMessage, identity string) {
	if s.responses == nil {
		s.responses = make(map[string]response)
	}
	held := s.responses[identity]
	held.model = msg.modelName()
	held.usage = rawUsage{
		Input:      max(held.usage.Input, msg.Usage.Input),
		Output:     max(held.usage.Output, msg.Usage.Output),
		CacheRead:  max(held.usage.CacheRead, msg.Usage.CacheRead),
		CacheWrite: max(held.usage.CacheWrite, msg.Usage.CacheWrite),
	}
	s.responses[identity] = held
}

// turnReading is the turn's report: the held responses summed per serving
// model. It is nil when no response carried an identity. A response with no
// identity is left out of the sum.
func (s *sessionState) turnReading() *usage.SpendReading {
	if len(s.responses) == 0 {
		return nil
	}
	perModel := make(map[string]rawUsage)
	for _, held := range s.responses {
		sum := perModel[held.model]
		sum.Input += held.usage.Input
		sum.Output += held.usage.Output
		sum.CacheRead += held.usage.CacheRead
		sum.CacheWrite += held.usage.CacheWrite
		perModel[held.model] = sum
	}
	entries := make([]usage.SpendReading, 0, len(perModel))
	for _, model := range slices.Sorted(maps.Keys(perModel)) {
		sum := perModel[model]
		entries = append(entries, usage.SpendReading{
			Input:      &sum.Input,
			Output:     &sum.Output,
			CacheRead:  &sum.CacheRead,
			CacheWrite: &sum.CacheWrite,
			Model:      model,
		})
	}
	return &usage.SpendReading{ByModel: entries}
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
