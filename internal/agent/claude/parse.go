package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/typeutil"
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

	// System/permission_denied fields.
	ToolName string `json:"tool_name,omitempty"`

	// Result fields.
	Result      string  `json:"result,omitempty"`
	IsError     bool    `json:"is_error,omitempty"`
	TotalCost   float64 `json:"total_cost_usd,omitempty"`
	DurationMS  int64   `json:"duration_ms,omitempty"`
	DurationAPI int64   `json:"duration_api_ms,omitempty"`
	NumTurns    int     `json:"num_turns,omitempty"`
	StopReason  string  `json:"stop_reason,omitempty"`

	// Usage (present in result events).
	Usage *rawUsage `json:"usage,omitempty"`

	// ModelUsage is the per-model usage breakdown on a result event,
	// present when the terminal turn touched at least one model.
	ModelUsage map[string]rawModelUsage `json:"modelUsage,omitempty"`

	// Assistant message wrapper.
	Message json.RawMessage `json:"message,omitempty"`

	// API retry fields.
	Attempt      int    `json:"attempt,omitempty"`
	MaxRetries   int    `json:"max_retries,omitempty"`
	RetryDelayMS int    `json:"retry_delay_ms,omitempty"`
	ErrorStatus  int    `json:"error_status,omitempty"`
	ErrorField   string `json:"error,omitempty"`
}

// rawUsage holds token counts from a Claude Code result event.
type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// rawModelUsage holds one model's token counts from a result event's
// modelUsage map.
type rawModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
}

// rawContentBlock represents a single block inside an assistant
// message content array.
type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Name      string          `json:"name,omitempty"`
	ID        string          `json:"id,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// rawAssistantMessage is the nested message object inside an
// assistant-type event.
type rawAssistantMessage struct {
	Content []rawContentBlock `json:"content,omitempty"`
}

// rawAssistantMessageMeta holds the message id, model, and per-request
// usage from an assistant event's message object. Claude Code repeats
// one message id across every streamed event of the same model
// request, so ID is the deduplication key.
type rawAssistantMessageMeta struct {
	ID    string    `json:"id,omitempty"`
	Model string    `json:"model,omitempty"`
	Usage *rawUsage `json:"usage,omitempty"`
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

// usageFromAssistant converts a raw usage payload from a Claude Code
// assistant event into a [domain.TokenUsage]. InputTokens includes
// prompt-cache reads and prompt-cache writes; TotalTokens is computed
// as InputTokens plus OutputTokens.
func usageFromAssistant(raw *rawUsage) domain.TokenUsage {
	if raw == nil {
		return domain.TokenUsage{}
	}
	input := raw.InputTokens + raw.CacheReadInputTokens + raw.CacheCreationInputTokens
	return domain.TokenUsage{
		InputTokens:     input,
		OutputTokens:    raw.OutputTokens,
		TotalTokens:     input + raw.OutputTokens,
		CacheReadTokens: raw.CacheReadInputTokens,
	}
}

// usageFromResult returns the authoritative per-turn usage and the
// model that produced it from a result event. It prefers the
// modelUsage map, which is present whenever the turn touched at least
// one model and includes sub-agent activity that the top-level usage
// object excludes; it falls back to the top-level usage object when
// modelUsage is absent or empty. The reported model is the modelUsage
// key with the greatest OutputTokens, ties broken by the
// lexicographically smallest key. InputTokens includes prompt-cache
// reads and prompt-cache writes in both cases; TotalTokens is computed
// as InputTokens plus OutputTokens.
func usageFromResult(event rawEvent) (usage domain.TokenUsage, model string) {
	if len(event.ModelUsage) > 0 {
		var bestOutput int64 = -1
		for name, mu := range event.ModelUsage {
			input := mu.InputTokens + mu.CacheReadInputTokens + mu.CacheCreationInputTokens
			usage.InputTokens += input
			usage.OutputTokens += mu.OutputTokens
			usage.CacheReadTokens += mu.CacheReadInputTokens
			if mu.OutputTokens > bestOutput || (mu.OutputTokens == bestOutput && name < model) {
				bestOutput = mu.OutputTokens
				model = name
			}
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		return usage, model
	}
	return usageFromAssistant(event.Usage), ""
}

// componentwiseMaxUsage returns the componentwise maximum of a and b,
// with TotalTokens recomputed as InputTokens plus OutputTokens.
func componentwiseMaxUsage(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:     max(a.InputTokens, b.InputTokens),
		OutputTokens:    max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens: max(a.CacheReadTokens, b.CacheReadTokens),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
}

// sumTurnMessages sums every stored per-message-id usage into one
// turn-provisional [domain.TokenUsage], with TotalTokens recomputed as
// InputTokens plus OutputTokens.
func sumTurnMessages(turnMessages map[string]domain.TokenUsage) domain.TokenUsage {
	var sum domain.TokenUsage
	for _, usage := range turnMessages {
		sum.InputTokens += usage.InputTokens
		sum.OutputTokens += usage.OutputTokens
		sum.CacheReadTokens += usage.CacheReadTokens
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	return sum
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

// summarizeAssistant extracts a human-readable summary from an
// assistant message event's content blocks.
func summarizeAssistant(event rawEvent) string {
	blocks := event.contentBlocks()
	if len(blocks) == 0 {
		return "assistant message"
	}

	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, typeutil.TruncateRunes(b.Text, 200))
			}
		case "tool_use":
			parts = append(parts, fmt.Sprintf("[tool: %s]", b.Name))
		case "tool_result":
			status := "ok"
			if b.IsError {
				status = "error"
			}
			parts = append(parts, fmt.Sprintf("[tool_result: %s]", status))
		}
	}

	if len(parts) == 0 {
		return "assistant message"
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

func (e rawEvent) summary() string {
	if e.Subtype != "" {
		return fmt.Sprintf("%s/%s", e.Type, e.Subtype)
	}
	return e.Type
}
