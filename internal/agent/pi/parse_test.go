package pi

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestParseRunEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     string
		wantType string
		wantErr  string
	}{
		{name: "framing event", line: `{"type":"agent_start"}`, wantType: "agent_start"},
		{name: "session header keeps id and cwd", line: `{"type":"session","version":3,"id":"s1","cwd":"/ws"}`, wantType: "session"},
		{name: "unknown type still decodes", line: `{"type":"future_event","payload":{}}`, wantType: "future_event"},
		{name: "plain text line", line: "pi: warming up", wantErr: "parse run event"},
		{name: "truncated object", line: `{"type":"agent_start"`, wantErr: "parse run event"},
		{name: "json array", line: `[{"type":"agent_start"}]`, wantErr: "parse run event"},
		{name: "json string", line: `"agent_start"`, wantErr: "parse run event"},
		{name: "empty line", line: "", wantErr: "parse run event"},
		{name: "type is not a string", line: `{"type":7}`, wantErr: "parse run event"},
		{name: "no type field", line: `{"cwd":"/ws"}`, wantErr: "no type field"},
		{name: "empty type", line: `{"type":""}`, wantErr: "no type field"},
		{name: "json null decodes to no type", line: `null`, wantErr: "no type field"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			event, err := parseRunEvent([]byte(tt.line))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseRunEvent(%q) error = nil, want %q", tt.line, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("parseRunEvent(%q) error = %q, want it to contain %q", tt.line, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRunEvent(%q) error = %v", tt.line, err)
			}
			if event.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", event.Type, tt.wantType)
			}
		})
	}
}

func TestSessionHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		event    rawRunEvent
		wantID   string
		wantCWD  string
		wantErr  string
		wantBoth bool
	}{
		{name: "header", event: rawRunEvent{Type: "session", ID: "s1", CWD: "/ws"}, wantID: "s1", wantCWD: "/ws"},
		{name: "not a session", event: rawRunEvent{Type: "agent_start"}, wantErr: "is not a session header"},
		{name: "no id", event: rawRunEvent{Type: "session", CWD: "/ws"}, wantErr: "carries no id"},
		{name: "no cwd", event: rawRunEvent{Type: "session", ID: "s1"}, wantErr: "carries no cwd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id, cwd, err := tt.event.sessionHeader()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("sessionHeader() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("sessionHeader() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("sessionHeader() error = %v", err)
			}
			if id != tt.wantID || cwd != tt.wantCWD {
				t.Errorf("sessionHeader() = (%q, %q), want (%q, %q)", id, cwd, tt.wantID, tt.wantCWD)
			}
		})
	}
}

func TestCompletedMessage(t *testing.T) {
	t.Parallel()

	const assistant = `{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":5,"output":2,"cacheRead":1,"cacheWrite":0,"reasoning":1},"model":"m1","stopReason":"stop"}`

	tests := []struct {
		name     string
		event    rawRunEvent
		wantRole string
		wantText string
		wantErr  string
	}{
		{name: "message_end", event: rawRunEvent{Type: "message_end", Message: []byte(assistant)}, wantRole: "assistant", wantText: "hi"},
		{name: "turn_end", event: rawRunEvent{Type: "turn_end", Message: []byte(assistant)}, wantRole: "assistant", wantText: "hi"},
		{name: "user role with block content is not read", event: rawRunEvent{Type: "message_end", Message: []byte(`{"role":"user","content":[{"type":"text","text":"go"}]}`)}, wantRole: "user"},
		{name: "user role with string content", event: rawRunEvent{Type: "message_end", Message: []byte(`{"role":"user","content":"go"}`)}, wantRole: "user"},
		{name: "custom role with string content", event: rawRunEvent{Type: "turn_end", Message: []byte(`{"role":"custom","content":"note"}`)}, wantRole: "custom"},
		{name: "system role with string content", event: rawRunEvent{Type: "message_end", Message: []byte(`{"role":"system","content":"be brief","sections":{"preamble":"x"}}`)}, wantRole: "system"},
		{name: "wrong event type", event: rawRunEvent{Type: "agent_end"}, wantErr: "carries no completed message"},
		{name: "no payload", event: rawRunEvent{Type: "turn_end"}, wantErr: "missing payload"},
		{name: "payload is a string", event: rawRunEvent{Type: "message_end", Message: []byte(`"nope"`)}, wantErr: "message"},
		{name: "content is not a list", event: rawRunEvent{Type: "message_end", Message: []byte(`{"role":"assistant","content":"hi"}`)}, wantErr: "message"},
		{name: "no role", event: rawRunEvent{Type: "turn_end", Message: []byte(`{"content":[]}`)}, wantErr: "carries no role"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg, err := tt.event.completedMessage()
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("completedMessage() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("completedMessage() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("completedMessage() error = %v", err)
			}
			if msg.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", msg.Role, tt.wantRole)
			}
			if got := msg.text(); got != tt.wantText {
				t.Errorf("text() = %q, want %q", got, tt.wantText)
			}
		})
	}
}

func TestRawMessageText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message rawMessage
		want    string
	}{
		{name: "no content"},
		{
			name:    "text blocks concatenate in order",
			message: rawMessage{Content: []rawContent{{Type: "text", Text: "Hello, "}, {Type: "text", Text: "world!"}}},
			want:    "Hello, world!",
		},
		{
			name:    "thinking and tool calls are not text",
			message: rawMessage{Content: []rawContent{{Type: "thinking", Thinking: "deliberate"}, {Type: "toolCall"}, {Type: "text", Text: "done"}}},
			want:    "done",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.message.text(); got != tt.want {
				t.Errorf("text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRawMessageHasOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content []rawContent
		want    bool
	}{
		{name: "no content"},
		{name: "empty text", content: []rawContent{{Type: "text"}}},
		{name: "empty thinking", content: []rawContent{{Type: "thinking"}}},
		{name: "empty text and thinking", content: []rawContent{{Type: "thinking"}, {Type: "text"}}},
		{name: "text", content: []rawContent{{Type: "text", Text: "x"}}, want: true},
		{name: "thinking", content: []rawContent{{Type: "thinking", Thinking: "x"}}, want: true},
		{name: "tool call", content: []rawContent{{Type: "toolCall"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := (rawMessage{Content: tt.content}).hasOutput(); got != tt.want {
				t.Errorf("hasOutput() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUsageReading(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		message   rawMessage
		wantOK    bool
		wantID    string
		wantUsage domain.TokenUsage
	}{
		{
			// Cache tokens sit outside pi's input count and reasoning
			// tokens inside its output count, so neither is added twice.
			name:      "cache joins input and reasoning stays in output",
			message:   rawMessage{ResponseID: "r1", Usage: &rawUsage{Input: 100, Output: 20, CacheRead: 10, CacheWrite: 5, Reasoning: 30}},
			wantOK:    true,
			wantID:    "r1",
			wantUsage: domain.TokenUsage{InputTokens: 115, OutputTokens: 20, CacheReadTokens: 10, CacheWriteTokens: 5, TotalTokens: 135},
		},
		{
			name:      "the timestamp identifies a response with no response id",
			message:   rawMessage{Timestamp: 1792571823362, Usage: &rawUsage{Input: 1, Output: 2}},
			wantOK:    true,
			wantID:    "1792571823362",
			wantUsage: domain.TokenUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
		},
		{name: "a response with no usage has no report", message: rawMessage{ResponseID: "r1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reading, ok := tt.message.usageReading()
			if ok != tt.wantOK {
				t.Fatalf("usageReading() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if reading.Identity != tt.wantID {
				t.Errorf("Identity = %q, want %q", reading.Identity, tt.wantID)
			}
			got, normalized := usage.Normalize(reading, usageSources.Live.Spend)
			if !normalized || got != tt.wantUsage {
				t.Errorf("Normalize() = %+v, %v, want %+v", got, normalized, tt.wantUsage)
			}
		})
	}
}

func TestRawMessageModelName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		message rawMessage
		want    string
	}{
		{message: rawMessage{Provider: "anthropic", Model: "claude-sonnet-4-5"}, want: "anthropic/claude-sonnet-4-5"},
		{message: rawMessage{Model: "claude-sonnet-4-5"}, want: "claude-sonnet-4-5"},
		{message: rawMessage{Provider: "gateway", Model: "coding-low", ResponseModel: "claude-haiku-4-5"}, want: "gateway/claude-haiku-4-5"},
		{message: rawMessage{Model: "coding-low", ResponseModel: "claude-haiku-4-5"}, want: "claude-haiku-4-5"},
		{message: rawMessage{Provider: "gateway", ResponseModel: "claude-haiku-4-5"}, want: "gateway/claude-haiku-4-5"},
		{message: rawMessage{Provider: "anthropic"}},
		{message: rawMessage{}},
	}

	for _, tt := range tests {
		if got := tt.message.modelName(); got != tt.want {
			t.Errorf("modelName(%+v) = %q, want %q", tt.message, got, tt.want)
		}
	}
}

func TestCacheWarmResponse(t *testing.T) {
	t.Parallel()

	warm := `{"type":"entry_appended","entry":{"type":"usage","id":"w1","kind":"cache_warm","provider":"anthropic","model":"claude-haiku-4-5","usage":{"input":0,"output":1,"cacheRead":90,"cacheWrite":2}}}`
	msg, ok, err := mustParseEvent(t, warm).cacheWarmResponse()
	if err != nil || !ok {
		t.Fatalf("cacheWarmResponse() = (%+v, %v, %v), want a warming response", msg, ok, err)
	}
	if msg.modelName() != "anthropic/claude-haiku-4-5" || msg.ResponseID != "w1" || msg.Usage == nil || *msg.Usage != (rawUsage{Output: 1, CacheRead: 90, CacheWrite: 2}) {
		t.Errorf("cacheWarmResponse() message = %+v, want the entry's model, id, and counts", msg)
	}

	for name, line := range map[string]string{
		"another entry type": `{"type":"entry_appended","entry":{"type":"context_edit","id":"e1"}}`,
		"another usage kind": `{"type":"entry_appended","entry":{"type":"usage","id":"u1","kind":"other"}}`,
		"custom entry":       `{"type":"entry_appended","entry":{"type":"custom","id":"c1","customType":"x","data":{"usage":7}}}`,
	} {
		if _, ok, err := mustParseEvent(t, line).cacheWarmResponse(); ok || err != nil {
			t.Errorf("%s: cacheWarmResponse() = (%v, %v), want no warming response", name, ok, err)
		}
	}

	for name, line := range map[string]string{
		"no entry":       `{"type":"entry_appended"}`,
		"entry not json": `{"type":"entry_appended","entry":7}`,
		"no usage":       `{"type":"entry_appended","entry":{"type":"usage","id":"w1","kind":"cache_warm"}}`,
	} {
		if _, _, err := mustParseEvent(t, line).cacheWarmResponse(); err == nil {
			t.Errorf("%s: cacheWarmResponse() error = nil, want a decode error", name)
		}
	}
}

func mustParseEvent(t *testing.T, line string) *rawRunEvent {
	t.Helper()
	event, err := parseRunEvent([]byte(line))
	if err != nil {
		t.Fatalf("parseRunEvent(%q) error = %v", line, err)
	}
	return &event
}
