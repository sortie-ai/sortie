package claude

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// rawEvent is the intermediate representation of a Claude Code JSONL
// line. Fields are populated from JSON and then mapped to domain
// event types by the adapter.
type rawEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`

	// System/init fields.
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`

	// System/task_started fields.
	TaskID string `json:"task_id,omitempty"`

	// System/permission_denied fields.
	ToolName string `json:"tool_name,omitempty"`

	// Result fields.
	Result      string `json:"result,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	DurationMS  int64  `json:"duration_ms,omitempty"`
	DurationAPI int64  `json:"duration_api_ms,omitempty"`
	NumTurns    int    `json:"num_turns,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`

	// APIErrorStatus is the HTTP status of the API failure behind an
	// error result, zero when the run met none.
	APIErrorStatus int `json:"api_error_status,omitempty"`

	// RateLimitInfo is present on rate_limit_event lines.
	RateLimitInfo *rawRateLimitInfo `json:"rate_limit_info,omitempty"`

	// ModelUsageSinceLastResult is the per-model spend of the requests the
	// process made since its previous result event.
	ModelUsageSinceLastResult json.RawMessage `json:"model_usage_since_last_result,omitempty"`

	// Assistant message wrapper.
	Message json.RawMessage `json:"message,omitempty"`

	// API retry fields.
	Attempt      int    `json:"attempt,omitempty"`
	MaxRetries   int    `json:"max_retries,omitempty"`
	RetryDelayMS int    `json:"retry_delay_ms,omitempty"`
	ErrorStatus  int    `json:"error_status,omitempty"`
	ErrorField   string `json:"error,omitempty"`
}

// rawContentBlock represents a single block inside an assistant
// message content array.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// toolMappings gives the kind and main-argument member of each Claude Code
// tool the adapter reports an argument for.
var toolMappings = map[string]agentcore.ToolMapping{
	"Bash":         {Kind: domain.ToolKindShell, Key: "command"},
	"Read":         {Kind: domain.ToolKindRead, Key: "file_path"},
	"Write":        {Kind: domain.ToolKindEdit, Key: "file_path"},
	"Edit":         {Kind: domain.ToolKindEdit, Key: "file_path"},
	"NotebookEdit": {Kind: domain.ToolKindEdit, Key: "notebook_path"},
	"Skill":        {Kind: domain.ToolKindSkill, Key: "skill"},
}

const mcpToolPrefix = "mcp__"

// toolCallOf maps a tool_use block's name and input to a tool call. An MCP
// tool is named {server}.{tool} and carries no kind or argument.
func toolCallOf(name string, input json.RawMessage) agentcore.ToolCall {
	if rest, ok := strings.CutPrefix(name, mcpToolPrefix); ok {
		if server, tool, ok := strings.Cut(rest, "__"); ok {
			return agentcore.ToolCall{Name: agentcore.QualifiedToolName(server, tool)}
		}
	}
	return agentcore.MapToolCall(toolMappings, name, input)
}

// rawAssistantMessage is the nested message object inside an
// assistant-type event.
type rawAssistantMessage struct {
	Content []rawContentBlock `json:"content,omitempty"`
}

// rawAssistantMessageMeta holds the message id, model, and per-request
// usage from an assistant event's message object. Claude Code repeats
// one message id across every streamed event of the same model request.
type rawAssistantMessageMeta struct {
	ID    string          `json:"id,omitempty"`
	Model string          `json:"model,omitempty"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

// usageMembers names the members of a usage object that carry each count;
// the stream, the journal, and a result's per-model figure spell them
// differently.
type usageMembers struct {
	input, output, cacheRead, cacheWrite string
}

var (
	requestMembers = usageMembers{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"}
	resultMembers  = usageMembers{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"}
)

// spendReadingOf maps a usage object to a reading. A count that is absent,
// null, or not an integer stays nil, and an object that does not decode
// yields a reading with no counts, so a malformed usage never fails the line
// that carries it.
func spendReadingOf(raw json.RawMessage, members usageMembers) usage.SpendReading {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return usage.SpendReading{}
	}
	return usage.SpendReading{
		Input:      countOf(fields[members.input]),
		Output:     countOf(fields[members.output]),
		CacheRead:  countOf(fields[members.cacheRead]),
		CacheWrite: countOf(fields[members.cacheWrite]),
	}
}

func countOf(raw json.RawMessage) *int64 {
	var count int64
	if absent(raw) || json.Unmarshal(raw, &count) != nil {
		return nil
	}
	return &count
}

// costUSDOf returns the costUSD of a per-model result entry: the cost of the
// requests the result covers, so a turn's amount and never the process's
// running total. It is nil for an absent, null, non-numeric, or negative
// member.
func costUSDOf(raw json.RawMessage) *float64 {
	var entry struct {
		CostUSD json.RawMessage `json:"costUSD"`
	}
	var cost float64
	if json.Unmarshal(raw, &entry) != nil || absent(entry.CostUSD) ||
		json.Unmarshal(entry.CostUSD, &cost) != nil || cost < 0 {
		return nil
	}
	return &cost
}

// absent reports whether a raw member is missing or null.
func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// requestReading maps the usage of one assistant message. It reports false
// for a message that names no request or no real model, or carries no usage
// object.
func requestReading(meta rawAssistantMessageMeta) (usage.SpendReading, bool) {
	if meta.ID == "" || absent(meta.Usage) || meta.Model == "" || meta.Model == syntheticModel {
		return usage.SpendReading{}, false
	}
	reading := spendReadingOf(meta.Usage, requestMembers)
	reading.Model = meta.Model
	reading.Identity = meta.ID
	return reading, true
}

// liveReading maps the per-model members of every result event of a turn
// to one reading. It is nil when the turn has no member or any member is
// not a JSON object, so the records settle the turn. A member with no keys
// adds nothing; when no member has a key the reading is a measured zero.
func liveReading(members []json.RawMessage) *usage.SpendReading {
	if len(members) == 0 {
		return nil
	}
	var entries []usage.SpendReading
	for _, member := range members {
		var perModel map[string]json.RawMessage
		if absent(member) || json.Unmarshal(member, &perModel) != nil {
			return nil
		}
		for _, model := range slices.Sorted(maps.Keys(perModel)) {
			entry := spendReadingOf(perModel[model], resultMembers)
			entry.Model = model
			entry.Amounts = []usage.AmountReading{{Amount: costUSDOf(perModel[model])}}
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		zero := int64(0)
		return &usage.SpendReading{Input: &zero, Output: &zero, CacheRead: &zero, CacheWrite: &zero}
	}
	return &usage.SpendReading{ByModel: entries}
}

// parseEvent parses a single JSONL line from Claude Code stdout into
// a [rawEvent]. Returns an error if JSON parsing fails.
func parseEvent(line []byte) (rawEvent, error) {
	var ev rawEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return rawEvent{}, fmt.Errorf("parse event: %w", err)
	}
	return ev, nil
}

// contentBlocks extracts the content array from an assistant message
// event. Returns nil if the message field is absent or unparseable.
func (e rawEvent) contentBlocks() []rawContentBlock {
	if len(e.Message) == 0 {
		return nil
	}
	var msg rawAssistantMessage
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return nil
	}
	return msg.Content
}

// summarizeAssistant returns the text blocks of an assistant message event
// joined by spaces, or "" when there are none. Tool calls are reported
// separately as tool_result events, so naming them here would only repeat
// them as noise in the run timeline.
func summarizeAssistant(event rawEvent) string {
	var parts []string
	for _, b := range event.contentBlocks() {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, redact.Truncate(b.Text, 200))
		}
	}
	return strings.Join(parts, " ")
}

// formatAPIRetry formats an api_retry system event into a
// human-readable notification string.
func formatAPIRetry(event rawEvent) string {
	return fmt.Sprintf("API retry attempt %d/%d (delay %dms, status %d: %s)",
		event.Attempt, event.MaxRetries, event.RetryDelayMS,
		event.ErrorStatus, event.ErrorField)
}
