package jsonrpc

import (
	"testing"
)

func TestParseMessage_Response(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":1,"result":{"serverInfo":{"name":"codex-app-server"}}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindResponse {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindResponse)
	}
	if msg.ID != 1 {
		t.Errorf("parseMessage(%q).ID = %d, want 1", line, msg.ID)
	}
}

func TestParseMessage_Notification(t *testing.T) {
	t.Parallel()

	line := []byte(`{"method":"thread/started","params":{"threadId":"abc-123"}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindNotification {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindNotification)
	}
	if msg.Method != "thread/started" {
		t.Errorf("parseMessage(%q).Method = %q, want %q", line, msg.Method, "thread/started")
	}
}

func TestParseMessage_ServerRequest(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":42,"method":"item/tool/call","params":{"tool":"my_tool","arguments":{}}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindRequest {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindRequest)
	}
	if msg.Method != "item/tool/call" {
		t.Errorf("parseMessage(%q).Method = %q, want %q", line, msg.Method, "item/tool/call")
	}
	if msg.ID != 42 {
		t.Errorf("parseMessage(%q).ID = %d, want 42", line, msg.ID)
	}
}

func TestParseMessage_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line []byte
	}{
		{"invalid JSON", []byte(`not json`)},
		{"truncated JSON", []byte(`{`)},
		{"empty object no method or id", []byte(`{}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := parseMessage(tt.line)

			if msg.Kind != KindMalformed {
				t.Errorf("parseMessage(%q).Kind = %v, want %v", tt.line, msg.Kind, KindMalformed)
			}
			if msg.Err == nil {
				t.Errorf("parseMessage(%q).Err = nil, want non-nil", tt.line)
			}
		})
	}
}

func TestParseMessage_ParamsNilWhenAbsent(t *testing.T) {
	t.Parallel()

	line := []byte(`{"method":"thread/started"}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Params != nil {
		t.Errorf("parseMessage(%q).Params = %q, want nil", line, msg.Params)
	}
}

func TestParseMessage_ErrorNilWhenAbsent(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":1,"result":{"ok":true}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Error != nil {
		t.Errorf("parseMessage(%q).Error = %v, want nil", line, msg.Error)
	}
}

func TestParseMessage_ZeroIDBesideMethodIsNotification(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":0,"method":"thread/started","params":{"threadId":"abc-123"}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindNotification {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindNotification)
	}
	if msg.ID != 0 {
		t.Errorf("parseMessage(%q).ID = %d, want 0", line, msg.ID)
	}
}
