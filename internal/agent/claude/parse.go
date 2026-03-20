package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/domain"
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

// rawContentBlock represents a single block inside an assistant
// message content array.
type rawContentBlock struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Name    string `json:"name,omitempty"`
	ID      string `json:"id,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// rawAssistantMessage is the nested message object inside an
// assistant-type event.
type rawAssistantMessage struct {
	Content []rawContentBlock `json:"content,omitempty"`
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

// normalizeUsage converts a raw usage payload from a Claude Code
// result event into a [domain.TokenUsage]. TotalTokens is computed as
// input + output.
func normalizeUsage(raw *rawUsage) domain.TokenUsage {
	if raw == nil {
		return domain.TokenUsage{}
	}
	return domain.TokenUsage{
		InputTokens:  raw.InputTokens,
		OutputTokens: raw.OutputTokens,
		TotalTokens:  raw.InputTokens + raw.OutputTokens,
	}
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
				parts = append(parts, truncate(b.Text, 200))
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

// summary returns a generic one-line summary from an event's type,
// subtype, and error fields.
func (e rawEvent) summary() string {
	if e.Subtype != "" {
		return fmt.Sprintf("%s/%s", e.Type, e.Subtype)
	}
	return e.Type
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
