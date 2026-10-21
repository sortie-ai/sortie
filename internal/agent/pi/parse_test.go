package pi

import (
	"strings"
	"testing"

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
		{name: "user role decodes but is not an assistant response", event: rawRunEvent{Type: "message_end", Message: []byte(`{"role":"user","content":[{"type":"text","text":"go"}]}`)}, wantRole: "user", wantText: "go"},
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

func TestRawMessageFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message rawMessage
		want    string
	}{
		{name: "stop", message: rawMessage{StopReason: "stop"}},
		{name: "tool use", message: rawMessage{StopReason: "toolUse"}},
		{name: "no stop reason", message: rawMessage{}},
		{
			name:    "error carries its message",
			message: rawMessage{StopReason: "error", ErrorMessage: "upstream 529"},
			want:    "upstream 529",
		},
		{
			name:    "error without a message names the reason",
			message: rawMessage{StopReason: "error"},
			want:    "pi reported stop reason error",
		},
		{
			name:    "aborted without a message names the reason",
			message: rawMessage{StopReason: "aborted"},
			want:    "pi reported stop reason aborted",
		},
		{
			name:    "aborted prefers its message",
			message: rawMessage{StopReason: "aborted", ErrorMessage: "user stopped the run"},
			want:    "user stopped the run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.message.failure(); got != tt.want {
				t.Errorf("failure() = %q, want %q", got, tt.want)
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

func TestTokenUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		usage rawUsage
		want  domain.TokenUsage
	}{
		{
			// pi reports cacheRead and cacheWrite as subsets of input,
			// and reasoning as a subset of output. Counting any of them
			// twice would overstate the response.
			name:  "subsets are counted once",
			usage: rawUsage{Input: 100, Output: 20, CacheRead: 10, CacheWrite: 5, Reasoning: 30},
			want:  domain.TokenUsage{InputTokens: 115, OutputTokens: 20, CacheReadTokens: 10, TotalTokens: 135},
		},
		{
			name:  "zero usage",
			usage: rawUsage{},
			want:  domain.TokenUsage{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.usage.tokenUsage(); got != tt.want {
				t.Errorf("tokenUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAddUsage(t *testing.T) {
	t.Parallel()

	first := domain.TokenUsage{InputTokens: 90, OutputTokens: 18, CacheReadTokens: 5, TotalTokens: 108}
	second := domain.TokenUsage{InputTokens: 40, OutputTokens: 9, CacheReadTokens: 2, TotalTokens: 49}

	want := domain.TokenUsage{InputTokens: 130, OutputTokens: 27, CacheReadTokens: 7, TotalTokens: 157}
	if got := addUsage(first, second); got != want {
		t.Errorf("addUsage() = %+v, want %+v: the total is recomputed from the components", got, want)
	}
	if got := addUsage(domain.TokenUsage{}, domain.TokenUsage{}); got != (domain.TokenUsage{}) {
		t.Errorf("addUsage(zero, zero) = %+v, want the zero figure", got)
	}
}

func TestCompletedMessage_SystemStringContent(t *testing.T) {
	t.Parallel()
	event, err := parseRunEvent([]byte(`{"type":"message_end","message":{"role":"system","content":"be brief","sections":{"preamble":"x"}}}`))
	if err != nil {
		t.Fatalf("parseRunEvent() error = %v", err)
	}
	msg, err := event.completedMessage()
	if err != nil {
		t.Fatalf("completedMessage() error = %v", err)
	}
	if msg.Role != "system" {
		t.Errorf("Role = %q, want system", msg.Role)
	}
}
