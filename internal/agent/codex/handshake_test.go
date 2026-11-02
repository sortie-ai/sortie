package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func discardTestLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newHandshakeConn's peer closes the read side once every line is consumed.
func newHandshakeConn(t *testing.T, recorder *capturingWriteCloser, lines ...string) *sessionState {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
		_ = inPw.Close()
	})

	var w io.Writer = outPw
	if recorder != nil {
		w = io.MultiWriter(recorder, outPw)
	}

	state := &sessionState{
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
		ledger:     adaptNewLedger(t),
	}
	state.conn = jsonrpc.NewConn(w, inPr, jsonrpc.Deliver(state.inbox, identity))
	go watchTermination(state)

	segments := splitFixtureSegments([]byte(strings.Join(lines, "\n")))
	startFixturePeer(t, outPr, inPw, segments)

	return state
}

func handshakeState(t *testing.T, lines ...string) *sessionState {
	t.Helper()
	return newHandshakeConn(t, nil, lines...)
}

func handshakeStateWithRecorder(t *testing.T, recorder *capturingWriteCloser, lines ...string) *sessionState {
	t.Helper()
	return newHandshakeConn(t, recorder, lines...)
}

// openEndedHandshakeState's peer never closes the read side, like a live
// runtime gone quiet, so only a wait loop's own context ends the call.
func openEndedHandshakeState(t *testing.T, repliesByID map[int64]string) *sessionState {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		inbox:      jsonrpc.NewInbox[jsonrpc.Message](),
		readerDone: make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
		ledger:     adaptNewLedger(t),
	}
	state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, identity))
	t.Cleanup(state.conn.Close)
	go watchTermination(state)

	go func() {
		scanOutboundLines(outPr, func(line []byte) {
			id := peekRequestID(line)
			reply, ok := repliesByID[id]
			if !ok {
				return
			}
			_, _ = fmt.Fprintln(inPw, reply)
		})
	}()

	return state
}

// authWaitState runs no watchTermination goroutine, so the test alone
// controls state.inbox; Call never takes from it, so the test cannot
// race Call's resolution.
func authWaitState(t *testing.T, repliesByID map[int64]string) *sessionState {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		target: agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		inbox:  jsonrpc.NewInbox[jsonrpc.Message](),
		acc:    agentcore.NewRunUsage(),
		ledger: adaptNewLedger(t),
	}
	state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, identity))

	go func() {
		scanOutboundLines(outPr, func(line []byte) {
			id := peekRequestID(line)
			reply, ok := repliesByID[id]
			if !ok {
				return
			}
			_, _ = fmt.Fprintln(inPw, reply)
		})
	}()

	return state
}

func TestInitializeHandshake_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"codex-app-server"}}}`)

	if err := initializeHandshake(context.Background(), state); err != nil {
		t.Fatalf("initializeHandshake() error = %v", err)
	}
}

func TestInitializeHandshake_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"error":{"code":-32600,"message":"invalid request"}}`)

	err := initializeHandshake(context.Background(), state)
	if err == nil {
		t.Fatal("initializeHandshake() expected error for error response, got nil")
	}
	if !strings.Contains(err.Error(), "initialize error") {
		t.Errorf("initializeHandshake() error = %q, want 'initialize error'", err.Error())
	}
}

func TestInitializeHandshake_EOF(t *testing.T) {
	t.Parallel()

	state := handshakeState(t)

	err := initializeHandshake(context.Background(), state)
	if err == nil {
		t.Fatal("initializeHandshake() expected error on EOF, got nil")
	}
}

func TestAuthenticateIfNeeded_AlreadyLoggedIn(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"account":{"id":"user-1","email":"user@example.com"}}}`)

	if err := authenticateIfNeeded(context.Background(), state, discardTestLogger()); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil for logged-in account", err)
	}
}

func TestAuthenticateIfNeeded_NullAccountNoAPIKey(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"account":null}}`)

	if err := authenticateIfNeeded(context.Background(), state, discardTestLogger()); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil when API key absent", err)
	}
}

func TestAuthenticateIfNeeded_AccountReadError(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"error":{"code":-32000,"message":"server error"}}`)

	err := authenticateIfNeeded(context.Background(), state, discardTestLogger())
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for account/read error response")
	}
}

func TestAuthenticateIfNeeded_LoginSuccess(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "test-api-key-12345")

	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":true}}`,
	)

	if err := authenticateIfNeeded(context.Background(), state, discardTestLogger()); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil on successful login", err)
	}
}

func TestAuthenticateIfNeeded_LoginResponseError(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "invalid-key")

	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"error":{"code":-32001,"message":"invalid API key"}}`,
	)

	err := authenticateIfNeeded(context.Background(), state, discardTestLogger())
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for login failure")
	}
}

func TestAuthenticateIfNeeded_LoginCompletedFailed(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "bad-key")

	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":false}}`,
	)

	err := authenticateIfNeeded(context.Background(), state, discardTestLogger())
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for failed login completion")
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != domain.ErrCredentialUnverified {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, domain.ErrCredentialUnverified)
	}
	const wantMessage = "the agent runtime refused its credential: the login did not succeed"
	if ae.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", ae.Message, wantMessage)
	}
}

func TestStartThread_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	threadID, _, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger())
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-abc" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-abc")
	}
}

func TestStartThread_DefaultApprovalPolicyIsNever(t *testing.T) {
	t.Parallel()

	stdin := &capturingWriteCloser{}
	state := handshakeStateWithRecorder(t, stdin,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	if _, _, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger()); err != nil {
		t.Fatalf("startThread() error = %v", err)
	}

	write, ok := stdin.find(`{"method":"thread/start"`)
	if !ok {
		t.Fatal("startThread() wrote no thread/start request")
	}

	var req struct {
		Method string `json:"method"`
		Params struct {
			ApprovalPolicy string `json:"approvalPolicy"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(write), &req); err != nil {
		t.Fatalf("unmarshal written thread/start request: %v", err)
	}
	if req.Params.ApprovalPolicy != "never" {
		t.Errorf("thread/start approvalPolicy = %q, want %q", req.Params.ApprovalPolicy, "never")
	}
}

func TestStartThread_WithModelAndPersonality(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-xyz"}}}`,
		`{"method":"thread/started","params":{}}`,
	)
	pt := passthroughConfig{
		Model:          "o4-mini",
		Personality:    "concise",
		ApprovalPolicy: "auto",
		ThreadSandbox:  "workspaceWrite",
	}

	threadID, _, _, err := startThread(context.Background(), state, pt, discardTestLogger())
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-xyz" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-xyz")
	}
}

func TestStartThread_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"error":{"code":-32000,"message":"workspace not found"}}`)

	_, _, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger())
	if err == nil {
		t.Fatal("startThread() expected error for error response")
	}
}

func TestStartThread_EmptyThreadID(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"thread":{"id":""}}}`)

	_, _, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger())
	if err == nil {
		t.Fatal("startThread() expected error for empty thread ID")
	}
}

func TestStartThread_FixtureThreadStartResponse(t *testing.T) {
	t.Parallel()

	lines := strings.Split(strings.TrimRight(string(loadFixture(t, "thread_start_response.jsonl")), "\n"), "\n")
	// Captured when thread/start was the session's fourth call; here it is the first.
	lines[0] = strings.Replace(lines[0], `"id":4,`, `"id":1,`, 1)
	state := handshakeState(t, lines...)

	_, model, _, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger())
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if model != "gpt-5.6-sol" {
		t.Errorf("startThread() model = %q, want %q", model, "gpt-5.6-sol")
	}
}

func TestStartThread_NoModelMember(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	_, model, path, err := startThread(context.Background(), state, passthroughConfig{}, discardTestLogger())
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if model != "" {
		t.Errorf("startThread() model = %q, want empty", model)
	}
	if path != "" {
		t.Errorf("startThread() path = %q, want empty for an absent thread.path", path)
	}
}

func TestResumeThread(t *testing.T) {
	t.Parallel()

	const reportedPath = "<CODEX_HOME>/sessions/2026/10/30/rollout-existing-thread-id.jsonl"
	tests := []struct {
		name      string
		lines     []string
		wantModel string
		wantPath  string
		wantErr   error
	}{
		{name: "result with model and path", lines: []string{`{"id":1,"result":{"model":"gpt-5.6-sol","thread":{"id":"existing-thread-id","path":"` + reportedPath + `"}}}`}, wantModel: "gpt-5.6-sol", wantPath: reportedPath},
		{name: "error response", lines: []string{`{"id":1,"error":{"code":-32002,"message":"thread not found"}}`}, wantErr: errResumeRejected},
		{name: "result that does not decode", lines: []string{`{"id":1,"result":{"model":123}}`}},
		{name: "connection lost before an answer", wantErr: errConnectionLost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model, path, err := resumeThread(context.Background(), handshakeState(t, tt.lines...), "existing-thread-id")

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("resumeThread() error = %v, want %v", err, tt.wantErr)
				}
				if rejected := errors.Is(err, errResumeRejected); rejected != (tt.wantErr == errResumeRejected) {
					t.Errorf("errors.Is(resumeThread() error, errResumeRejected) = %v, want %v", rejected, !rejected)
				}
				return
			}
			if err != nil {
				t.Fatalf("resumeThread() error = %v", err)
			}
			if model != tt.wantModel || path != tt.wantPath {
				t.Errorf("resumeThread() = %q, %q, want %q, %q", model, path, tt.wantModel, tt.wantPath)
			}
		})
	}
}

func TestProbeResume_TimeoutIsNotAStartFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), -1)
	t.Cleanup(cancel)

	signal, failure := openEndedHandshakeState(t, nil).probeResume(ctx, "existing-thread-id")

	if failure != nil || signal.Verdict != agentcore.ResumeUnknown {
		t.Errorf("probeResume(expired context) = verdict %v, failure %v, want verdict %v and no failure", signal.Verdict, failure, agentcore.ResumeUnknown)
	}
}

func TestAuthenticateIfNeeded_ContextCancelledDuringLoginWait(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "test-key")

	state := openEndedHandshakeState(t, map[int64]string{
		1: `{"id":1,"result":{"account":null}}`,
		2: `{"id":2,"result":{}}`,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- authenticateIfNeeded(ctx, state, discardTestLogger())
	}()

	cancel()

	if err := waittest.Receive(t, done, "authenticateIfNeeded return"); !errors.Is(err, context.Canceled) {
		t.Errorf("authenticateIfNeeded() = %v, want context.Canceled", err)
	}
}

func TestStartThread_ContextCancelledDuringNotificationWait(t *testing.T) {
	t.Parallel()

	state := openEndedHandshakeState(t, map[int64]string{
		1: `{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := startThread(ctx, state, passthroughConfig{}, discardTestLogger())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("startThread() = %v, want context.Canceled", err)
	}
}
