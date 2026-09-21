package clientprotocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
