package jsonrpc

import (
	"encoding/json"
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
	if !msg.ID.Equal(NumberID(1)) {
		t.Errorf("parseMessage(%q).ID = %s, want %s", line, msg.ID, NumberID(1))
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
	if !msg.ID.Equal(NumberID(42)) {
		t.Errorf("parseMessage(%q).ID = %s, want %s", line, msg.ID, NumberID(42))
	}
}

// TestParseMessage_StringIDRequest checks that a request whose id is a
// JSON string classifies as KindRequest, so the connection can answer
// it, and that the id re-serializes to the exact string that arrived.
func TestParseMessage_StringIDRequest(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":"req-abc-123","method":"session/request_permission","params":{}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindRequest {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindRequest)
	}
	if !msg.ID.Present() {
		t.Errorf("parseMessage(%q).ID.Present() = false, want true", line)
	}
	got, err := json.Marshal(msg.ID)
	if err != nil {
		t.Fatalf("json.Marshal(parseMessage(%q).ID) error = %v", line, err)
	}
	if want := `"req-abc-123"`; string(got) != want {
		t.Errorf("json.Marshal(parseMessage(%q).ID) = %s, want %s", line, got, want)
	}
}

// TestParseMessage_UnmatchedStringIDResponse checks that a response
// carrying a string id classifies as KindResponse rather than
// KindMalformed, so it can still reach the connection's handler as an
// unmatched response even though it never satisfies the numeric
// pending-call correlation.
func TestParseMessage_UnmatchedStringIDResponse(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":"unmatched-1","result":{"ok":true}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindResponse {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindResponse)
	}
	if _, isNumber := msg.ID.Number(); isNumber {
		t.Errorf("parseMessage(%q).ID.Number() reported a number, want a non-numeric id", line)
	}
	got, err := json.Marshal(msg.ID)
	if err != nil {
		t.Fatalf("json.Marshal(parseMessage(%q).ID) error = %v", line, err)
	}
	if want := `"unmatched-1"`; string(got) != want {
		t.Errorf("json.Marshal(parseMessage(%q).ID) = %s, want %s", line, got, want)
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

// TestParseMessage_ZeroIDBesideMethodIsRequest checks the deliberate
// reclassification from the previous behavior: an id of the JSON
// number zero is a present id, so a message carrying a method and a
// zero id is a request the connection must answer, not a
// notification.
func TestParseMessage_ZeroIDBesideMethodIsRequest(t *testing.T) {
	t.Parallel()

	line := []byte(`{"id":0,"method":"thread/started","params":{"threadId":"abc-123"}}`)
	msg := parseMessage(line)

	if msg.Err != nil {
		t.Fatalf("parseMessage(%q) error = %v", line, msg.Err)
	}
	if msg.Kind != KindRequest {
		t.Errorf("parseMessage(%q).Kind = %v, want %v", line, msg.Kind, KindRequest)
	}
	if !msg.ID.Present() {
		t.Errorf("parseMessage(%q).ID.Present() = false, want true", line)
	}
	if !msg.ID.Equal(NumberID(0)) {
		t.Errorf("parseMessage(%q).ID = %s, want %s", line, msg.ID, NumberID(0))
	}
}

// TestParseMessage_NullID checks that a JSON null id counts as
// absent: paired with a method it is a notification, and with no
// method it satisfies neither a request, a notification, nor a
// response, so it is malformed.
func TestParseMessage_NullID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     []byte
		wantKind Kind
	}{
		{
			name:     "null id with method is a notification",
			line:     []byte(`{"id":null,"method":"thread/started","params":{"threadId":"abc-123"}}`),
			wantKind: KindNotification,
		},
		{
			name:     "null id with no method is malformed",
			line:     []byte(`{"id":null,"result":{"ok":true}}`),
			wantKind: KindMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := parseMessage(tt.line)

			if msg.Kind != tt.wantKind {
				t.Errorf("parseMessage(%q).Kind = %v, want %v", tt.line, msg.Kind, tt.wantKind)
			}
			if msg.ID.Present() {
				t.Errorf("parseMessage(%q).ID.Present() = true, want false", tt.line)
			}
			switch tt.wantKind {
			case KindNotification:
				if msg.Err != nil {
					t.Errorf("parseMessage(%q).Err = %v, want nil", tt.line, msg.Err)
				}
			case KindMalformed:
				if msg.Err == nil {
					t.Errorf("parseMessage(%q).Err = nil, want non-nil", tt.line)
				}
			}
		})
	}
}
