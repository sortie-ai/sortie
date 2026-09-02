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

// TestParseMessage_NullID checks that an explicit JSON null id is a
// present id rather than an absent one. A notification is a request
// with no id member at all, so a null id beside a method is a request
// that still expects an answer, and a null id with no method is the
// response form a peer sends when it could not read the id it was
// answering.
func TestParseMessage_NullID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		line        []byte
		wantKind    Kind
		wantErrPayl bool
	}{
		{
			name:     "null id with method is a request",
			line:     []byte(`{"id":null,"method":"thread/started","params":{"threadId":"abc-123"}}`),
			wantKind: KindRequest,
		},
		{
			name:        "null id with no method is a response carrying its error",
			line:        []byte(`{"id":null,"error":{"code":-32700,"message":"parse error"}}`),
			wantKind:    KindResponse,
			wantErrPayl: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := parseMessage(tt.line)

			if msg.Kind != tt.wantKind {
				t.Errorf("parseMessage(%q).Kind = %v, want %v", tt.line, msg.Kind, tt.wantKind)
			}
			if !msg.ID.Present() {
				t.Errorf("parseMessage(%q).ID.Present() = false, want true", tt.line)
			}
			if !msg.ID.IsNull() {
				t.Errorf("parseMessage(%q).ID.IsNull() = false, want true", tt.line)
			}
			if _, ok := msg.ID.Number(); ok {
				t.Errorf("parseMessage(%q).ID.Number() reported a number, want none", tt.line)
			}
			if msg.Err != nil {
				t.Errorf("parseMessage(%q).Err = %v, want nil", tt.line, msg.Err)
			}
			if got := msg.Error != nil; got != tt.wantErrPayl {
				t.Errorf("parseMessage(%q) carried error payload = %v, want %v", tt.line, got, tt.wantErrPayl)
			}
		})
	}
}

// TestParseMessage_NumberIDBeyondInt64Form checks that a number this
// package cannot hold as a plain integer is still a valid id. Such a
// request used to fail the envelope decode and arrive as malformed,
// so it was never answered and the peer waited forever.
func TestParseMessage_NumberIDBeyondInt64Form(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line []byte
		want string
	}{
		{name: "exponent form", line: []byte(`{"method":"x","id":1e3}`), want: "1e3"},
		{name: "fraction form", line: []byte(`{"method":"x","id":1.0}`), want: "1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := parseMessage(tt.line)

			if msg.Kind != KindRequest {
				t.Fatalf("parseMessage(%q).Kind = %v, want %v (Err=%v)", tt.line, msg.Kind, KindRequest, msg.Err)
			}
			if _, ok := msg.ID.Number(); ok {
				t.Errorf("parseMessage(%q).ID.Number() reported a number, want none", tt.line)
			}
			got, err := msg.ID.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("MarshalJSON() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestID_MarshalJSONRoundTripsWireForm checks that every id form an
// answer must echo is reproduced byte for byte, and that an absent id
// refuses to render at all.
func TestID_MarshalJSONRoundTripsWireForm(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    []byte
		want    string
		wantErr bool
	}{
		{name: "plain number", line: []byte(`{"method":"x","id":7}`), want: "7"},
		{name: "zero", line: []byte(`{"method":"x","id":0}`), want: "0"},
		{name: "string", line: []byte(`{"method":"x","id":"req-1"}`), want: `"req-1"`},
		{name: "null", line: []byte(`{"method":"x","id":null}`), want: "null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseMessage(tt.line).ID.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("MarshalJSON() = %s, want %s", got, tt.want)
			}
		})
	}

	t.Run("absent id refuses to render", func(t *testing.T) {
		t.Parallel()

		if _, err := (ID{}).MarshalJSON(); err == nil {
			t.Error("ID{}.MarshalJSON() error = nil, want non-nil")
		}
	})
}
