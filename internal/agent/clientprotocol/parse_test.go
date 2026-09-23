package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestQuoteJSONRPCError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *jsonrpc.Error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "no data", err: &jsonrpc.Error{Message: "invalid API key"}, want: "invalid API key"},
		{name: "null data", err: &jsonrpc.Error{Message: "invalid API key", Data: json.RawMessage(`null`)}, want: "invalid API key"},
		{
			name: "string data decodes",
			err:  &jsonrpc.Error{Message: "invalid API key", Data: json.RawMessage(`"the bearer token was rejected"`)},
			want: "invalid API key: the bearer token was rejected",
		},
		{
			name: "non-string data renders as compact JSON",
			err:  &jsonrpc.Error{Message: "invalid API key", Data: json.RawMessage(`{"reason":"expired","retryable":false}`)},
			want: `invalid API key: {"reason":"expired","retryable":false}`,
		},
		{
			name: "truncates to 500 runes",
			err:  &jsonrpc.Error{Message: strings.Repeat("x", 600)},
			want: strings.Repeat("x", 500) + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := quoteJSONRPCError(tt.err); got != tt.want {
				t.Errorf("quoteJSONRPCError(%+v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestHandshake_ErrorQuotesData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method      string
		call        func(*sessionState) *domain.AgentError
		code        string
		wantMessage string
	}{
		{
			method: methodInitialize,
			call: func(s *sessionState) *domain.AgentError {
				_, err := doInitialize(context.Background(), s)
				return err
			},
			code:        "-32603",
			wantMessage: "initialize error -32603: internal error: the invalid bearer token",
		},
		{
			method: methodSessionNew,
			call: func(s *sessionState) *domain.AgentError {
				_, err := doNewSession(context.Background(), s, "/ws", nil)
				return err
			},
			code:        "-32603",
			wantMessage: "session/new error -32603: internal error: the invalid bearer token",
		},
		{method: methodSessionNew, call: func(s *sessionState) *domain.AgentError {
			_, err := doNewSession(context.Background(), s, "/ws", nil)
			return err
		}, code: "-32000", wantMessage: "the agent runtime refused its credential: internal error: the invalid bearer token"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.code, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			ch := make(chan *domain.AgentError, 1)
			go func() { ch <- tt.call(state) }()

			id := out.awaitMethod(t, tt.method)
			sendLine(t, inPw, `{"jsonrpc":"2.0","id":`+string(id)+`,"error":{"code":`+tt.code+`,"message":"internal error","data":"the invalid bearer token"}}`)

			select {
			case err := <-ch:
				if err == nil || err.Message != tt.wantMessage {
					t.Errorf("%s error = %v, want message %q", tt.method, err, tt.wantMessage)
				}
			case <-time.After(awaitTimeout):
				t.Fatalf("timed out waiting for %s to return", tt.method)
			}
		})
	}
}

func toolCallJSON(nameJSON string) string {
	extra := ""
	if nameJSON != "" {
		extra = `,"name":` + nameJSON
	}
	return fmt.Sprintf(`{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"Read a file","kind":"read","status":"pending"%s}`, extra)
}

func toolCallUpdateJSON(nameJSON string) string {
	extra := ""
	if nameJSON != "" {
		extra = `,"name":` + nameJSON
	}
	return fmt.Sprintf(`{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","title":"Read a file"%s}`, extra)
}

func TestParseSessionUpdateMalformedToolCallName(t *testing.T) {
	t.Parallel()

	dropped := []struct {
		name     string
		nameJSON string
	}{
		{"a JSON number", `42`},
		{"a JSON object", `{"foo":"bar"}`},
		{"a JSON array", `[1,2,3]`},
		{"a JSON boolean", `true`},
	}
	kept := []struct {
		name     string
		nameJSON string
	}{
		{"a JSON string", `"custom-tool"`},
		{"JSON null", `null`},
	}

	for _, variant := range []struct {
		name  string
		build func(nameJSON string) string
	}{
		{"tool_call", toolCallJSON},
		{"tool_call_update", toolCallUpdateJSON},
	} {
		t.Run(variant.name, func(t *testing.T) {
			t.Parallel()

			baseline := variant.build("")
			wantEvent, wantFound := parseSessionUpdate(json.RawMessage(baseline))
			if !wantFound {
				t.Fatalf("parseSessionUpdate(%s) found = false, want true", baseline)
			}

			for _, tt := range dropped {
				t.Run(tt.name+" decodes as if name were absent", func(t *testing.T) {
					t.Parallel()

					raw := variant.build(tt.nameJSON)
					gotEvent, gotFound := parseSessionUpdate(json.RawMessage(raw))
					if gotFound != wantFound {
						t.Fatalf("parseSessionUpdate(%s) found = %v, want %v", raw, gotFound, wantFound)
					}
					if !reflect.DeepEqual(gotEvent, wantEvent) {
						t.Errorf("parseSessionUpdate(%s) = %+v, want %+v (same as no name at all)", raw, gotEvent, wantEvent)
					}
				})
			}

			for _, tt := range kept {
				t.Run(tt.name+" decodes unchanged", func(t *testing.T) {
					t.Parallel()

					raw := variant.build(tt.nameJSON)
					_, gotFound := parseSessionUpdate(json.RawMessage(raw))
					if gotFound != wantFound {
						t.Fatalf("parseSessionUpdate(%s) found = %v, want %v", raw, gotFound, wantFound)
					}
				})
			}
		})
	}
}

func TestToolCallLifecycleSurvivesMalformedName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		toolCallName       string
		toolCallUpdateName string
	}{
		{"malformed name on tool_call", `{"nested":true}`, ""},
		{"malformed name on tool_call_update", "", `[1,2,3]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := agentcore.NewToolTracker()

			beginRaw := toolCallJSON(tt.toolCallName)
			beginEvent, found := parseSessionUpdate(json.RawMessage(beginRaw))
			if !found {
				t.Fatalf("parseSessionUpdate(%s) found = false, want true", beginRaw)
			}
			if got := applySessionUpdate(tracker, beginEvent); got.hasEvent {
				t.Fatalf("applySessionUpdate(tool_call) = %+v, want hasEvent=false", got)
			}

			updateRaw := toolCallUpdateJSON(tt.toolCallUpdateName)
			updateEvent, found := parseSessionUpdate(json.RawMessage(updateRaw))
			if !found {
				t.Fatalf("parseSessionUpdate(%s) found = false, want true", updateRaw)
			}
			got := applySessionUpdate(tracker, updateEvent)
			if !got.hasEvent || got.event.Type != domain.EventToolResult {
				t.Fatalf("applySessionUpdate(tool_call_update) = %+v, want a tool_result event", got)
			}
			if got.event.ToolName != "read" {
				t.Errorf("applySessionUpdate(tool_call_update) ToolName = %q, want %q: the call's kind, not its malformed name", got.event.ToolName, "read")
			}
		})
	}
}
