//go:build unix

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// newHandshakeConn builds a sessionState wired to a real jsonrpc.Conn
// whose peer replays lines (joined as a flat JSONL fixture), for
// testing the handshake helpers in isolation. Once every scripted
// line has been consumed, the peer closes the connection's read side
// cleanly. recorder, when non-nil, receives a copy of every line the
// connection writes.
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
		msgCh:      make(chan jsonrpc.Message, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(w, inPr, sessionHandler(state))
	go watchTermination(state)

	segments := splitFixtureSegments([]byte(strings.Join(lines, "\n")))
	startFixturePeer(t, outPr, inPw, segments)

	return state
}

// handshakeState builds a sessionState like newHandshakeConn, closing
// the connection's read side cleanly once lines are exhausted.
func handshakeState(t *testing.T, lines ...string) *sessionState {
	t.Helper()
	return newHandshakeConn(t, nil, lines...)
}

// handshakeStateWithRecorder behaves like handshakeState but records
// every line the connection writes into recorder, so a test can
// assert on the exact outbound bytes.
func handshakeStateWithRecorder(t *testing.T, recorder *capturingWriteCloser, lines ...string) *sessionState {
	t.Helper()
	return newHandshakeConn(t, recorder, lines...)
}

// openEndedHandshakeState builds a sessionState like handshakeState,
// except its peer never closes the connection's read side after
// delivering repliesByID: like a still-running process that has
// simply gone quiet, it leaves the stream open so a wait loop's own
// context cancellation, not a spurious end of stream, is what ends
// the call.
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
		msgCh:      make(chan jsonrpc.Message, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(outPw, inPr, sessionHandler(state))
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

// authWaitState builds a sessionState like openEndedHandshakeState,
// resolving exactly repliesByID's calls and never terminating on its
// own, but without a watchTermination goroutine racing the test's own
// direct control of state.msgCh: jsonrpc.Conn.Call does not read
// state.msgCh at all, so closing it, or pushing a message onto it,
// directly from the test cannot race Call's own resolution — the two
// channels are entirely independent. This is the deterministic way to
// end a msgCh-reading wait loop from a test: any attempt to close, or
// force an error on, the underlying connection instead would race
// Call's own pending-response delivery on the same connection.
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
		msgCh:  make(chan jsonrpc.Message, 16),
		stopCh: make(chan struct{}),
		acc:    agentcore.NewRunUsage(),
	}
	// The handler is a no-op, never sessionHandler: every message the
	// test cares about is delivered by acting on state.msgCh directly
	// (closing it, or pushing a message), and once a test has closed
	// it, a real sessionHandler would panic sending into it once the
	// connection eventually errors at t.Cleanup.
	state.conn = jsonrpc.NewConn(outPw, inPr, func(jsonrpc.Message) {})

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

// --- initializeHandshake ---

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

	state := handshakeState(t) // empty fixture → immediate EOF

	err := initializeHandshake(context.Background(), state)
	if err == nil {
		t.Fatal("initializeHandshake() expected error on EOF, got nil")
	}
}

// --- authenticateIfNeeded ---

func TestAuthenticateIfNeeded_AlreadyLoggedIn(t *testing.T) {
	t.Parallel()

	// account/read response with non-null account.
	state := handshakeState(t, `{"id":1,"result":{"account":{"id":"user-1","email":"user@example.com"}}}`)

	if err := authenticateIfNeeded(context.Background(), state); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil for logged-in account", err)
	}
}

func TestAuthenticateIfNeeded_NullAccountNoAPIKey(t *testing.T) {
	t.Parallel()

	// account/read response with null account — CODEX_API_KEY not set → return nil.
	state := handshakeState(t, `{"id":1,"result":{"account":null}}`)

	if err := authenticateIfNeeded(context.Background(), state); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil when API key absent", err)
	}
}

func TestAuthenticateIfNeeded_AccountReadError(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"error":{"code":-32000,"message":"server error"}}`)

	err := authenticateIfNeeded(context.Background(), state)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for account/read error response")
	}
}

func TestAuthenticateIfNeeded_LoginSuccess(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "test-api-key-12345")

	// id=1: account/read → null account
	// id=2: account/login/start → success response
	// then: login/completed notification
	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":true}}`,
	)

	if err := authenticateIfNeeded(context.Background(), state); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v, want nil on successful login", err)
	}
}

func TestAuthenticateIfNeeded_LoginResponseError(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "invalid-key")

	// id=1: account/read → null
	// id=2: account/login/start → error
	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"error":{"code":-32001,"message":"invalid API key"}}`,
	)

	err := authenticateIfNeeded(context.Background(), state)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for login failure")
	}
}

func TestAuthenticateIfNeeded_LoginCompletedFailed(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "bad-key")

	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		`{"method":"account/login/completed","params":{"success":false}}`,
	)

	err := authenticateIfNeeded(context.Background(), state)
	if err == nil {
		t.Fatal("authenticateIfNeeded() expected error for failed login completion")
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != domain.ErrResponseError {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, domain.ErrResponseError)
	}
}

// --- startThread ---

func TestStartThread_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	threadID, _, err := startThread(context.Background(), state, passthroughConfig{})
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-abc" {
		t.Errorf("startThread() threadID = %q, want %q", threadID, "thread-abc")
	}
}

// TestStartThread_DefaultApprovalPolicyIsNever asserts the non-interactive
// launch posture: under default configuration, thread/start carries
// approvalPolicy "never" so the app-server does not ask interactively.
func TestStartThread_DefaultApprovalPolicyIsNever(t *testing.T) {
	t.Parallel()

	stdin := &capturingWriteCloser{}
	state := handshakeStateWithRecorder(t, stdin,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	if _, _, err := startThread(context.Background(), state, passthroughConfig{}); err != nil {
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

	threadID, _, err := startThread(context.Background(), state, pt)
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

	_, _, err := startThread(context.Background(), state, passthroughConfig{})
	if err == nil {
		t.Fatal("startThread() expected error for error response")
	}
}

func TestStartThread_EmptyThreadID(t *testing.T) {
	t.Parallel()

	// Response with empty thread ID.
	state := handshakeState(t, `{"id":1,"result":{"thread":{"id":""}}}`)

	_, _, err := startThread(context.Background(), state, passthroughConfig{})
	if err == nil {
		t.Fatal("startThread() expected error for empty thread ID")
	}
}

// TestStartThread_FixtureThreadStartResponse drives startThread against
// the captured thread_start_response.jsonl fixture (thread/start
// response, then thread/started notification, in file order) and
// asserts the returned model equals the fixture's own "model" member.
//
// The fixture's response line carries the request id (4) observed at
// capture time, when thread/start was the fourth call of that
// session. handshakeState wires startThread to a bare connection
// whose own request numbering starts at 1, so the response id is
// rewritten to 1 here, in memory, to correlate with that request;
// testdata/thread_start_response.jsonl itself is untouched.
func TestStartThread_FixtureThreadStartResponse(t *testing.T) {
	t.Parallel()

	lines := strings.Split(strings.TrimRight(string(loadFixture(t, "thread_start_response.jsonl")), "\n"), "\n")
	lines[0] = strings.Replace(lines[0], `"id":4,`, `"id":1,`, 1)
	state := handshakeState(t, lines...)

	_, model, err := startThread(context.Background(), state, passthroughConfig{})
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if model != "gpt-5.6-sol" {
		t.Errorf("startThread() model = %q, want %q", model, "gpt-5.6-sol")
	}
}

// TestStartThread_NoModelMember drives a thread/start response whose
// result carries no "model" member and asserts startThread returns an
// empty model alongside a nil error.
func TestStartThread_NoModelMember(t *testing.T) {
	t.Parallel()

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	_, model, err := startThread(context.Background(), state, passthroughConfig{})
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if model != "" {
		t.Errorf("startThread() model = %q, want empty", model)
	}
}

// --- resumeThread ---

func TestResumeThread_Success(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"model":"gpt-5.6-sol"}}`)

	model, err := resumeThread(context.Background(), state, "existing-thread-id")
	if err != nil {
		t.Fatalf("resumeThread() error = %v", err)
	}
	if model != "gpt-5.6-sol" {
		t.Errorf("resumeThread() model = %q, want %q", model, "gpt-5.6-sol")
	}
}

func TestResumeThread_ErrorResponse(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"error":{"code":-32002,"message":"thread not found"}}`)

	_, err := resumeThread(context.Background(), state, "nonexistent-thread")
	if err == nil {
		t.Fatal("resumeThread() expected error for error response")
	}
}

// TestResumeThread_UnmarshalFailureReturnsEmptyModel drives a
// thread/resume response whose result carries a "model" member of the
// wrong wire type, so it fails to unmarshal into threadResult, and
// asserts that failure does not turn a successful resume into an
// error: resumeThread returns an empty model and a nil error.
func TestResumeThread_UnmarshalFailureReturnsEmptyModel(t *testing.T) {
	t.Parallel()

	state := handshakeState(t, `{"id":1,"result":{"model":123}}`)

	model, err := resumeThread(context.Background(), state, "existing-thread-id")
	if err != nil {
		t.Fatalf("resumeThread() error = %v", err)
	}
	if model != "" {
		t.Errorf("resumeThread() model = %q, want empty", model)
	}
}

func TestAuthenticateIfNeeded_ContextCancelledDuringLoginWait(t *testing.T) {
	// No t.Parallel() — uses t.Setenv.
	t.Setenv("CODEX_API_KEY", "test-key")

	// pw stays open — the peer answers both calls but the app-server
	// never sends account/login/completed, so authenticateIfNeeded
	// blocks in its notification wait until ctx is cancelled.
	state := openEndedHandshakeState(t, map[int64]string{
		1: `{"id":1,"result":{"account":null}}`,
		2: `{"id":2,"result":{}}`,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- authenticateIfNeeded(ctx, state)
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("authenticateIfNeeded() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authenticateIfNeeded() did not return after context cancel")
	}
}

func TestStartThread_ContextCancelledDuringNotificationWait(t *testing.T) {
	t.Parallel()

	// pw stays open — the peer answers thread/start but the app-server
	// never sends thread/started.
	state := openEndedHandshakeState(t, map[int64]string{
		1: `{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := startThread(ctx, state, passthroughConfig{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("startThread() = %v, want context.Canceled", err)
	}
}
