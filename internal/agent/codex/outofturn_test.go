package codex

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestInitializeHandshake_BoundedByReadTimeout drives initializeHandshake
// against a runtime that answers nothing at all: with agent.read_timeout_ms
// set small and a context that never itself ends, the handshake call must
// still fail within that bound rather than waiting on the caller's own,
// unbounded context forever.
func TestInitializeHandshake_BoundedByReadTimeout(t *testing.T) {
	t.Parallel()

	state := openEndedHandshakeState(t, map[int64]string{})
	state.agentConfig.ReadTimeoutMS = 200

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- initializeHandshake(context.Background(), state) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("initializeHandshake() error = nil, want a timeout error")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("initializeHandshake() error = %v, want error wrapping context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("initializeHandshake() took %v, want close to the 200ms agent.read_timeout_ms bound", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initializeHandshake() did not return within 5s against a runtime that never answers initialize (want it bounded by agent.read_timeout_ms)")
	}
}

// mcpStartupFailedLine is a real mcpServer/startupStatus/updated
// notification captured from live codex-cli 0.153.4, reporting a
// declared MCP server that failed to start.
const mcpStartupFailedLine = `{"method":"mcpServer/startupStatus/updated","params":{"threadId":"01a09974-85ca-79c0-9d43-05e7d488a6df","name":"missing","status":"failed","error":"MCP client for ` + "`missing`" + ` failed to start: MCP startup failed: No such file or directory (os error 2)","failureReason":null}}`

// assertMCPStartupFailureLogged fails t unless output carries the
// WARN record reportMCPStartupFailure emits for mcpStartupFailedLine,
// with the mcp_server and reason fields it always carries.
func assertMCPStartupFailureLogged(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "MCP server failed to start") {
		t.Errorf("output missing the WARN message %q: %s", "MCP server failed to start", output)
	}
	if !strings.Contains(output, `mcp_server=missing`) {
		t.Errorf("output missing mcp_server=missing: %s", output)
	}
	if !strings.Contains(output, "MCP startup failed") {
		t.Errorf("output missing the failure reason: %s", output)
	}
}

// TestMCPStartupFailure_LoggedDuringThreadStartedWait drives startThread
// against a peer that answers thread/start and then, before sending
// thread/started, delivers a real captured MCP startup failure
// notification. The notification must be reported the same way the turn
// loop reports it, carrying the session id startThread has already
// resolved by that point.
func TestMCPStartupFailure_LoggedDuringThreadStartedWait(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	state := handshakeState(t,
		`{"id":1,"result":{"thread":{"id":"thread-abc"}}}`,
		mcpStartupFailedLine,
		`{"method":"thread/started","params":{"threadId":"thread-abc"}}`,
	)

	threadID, _, err := startThread(context.Background(), state, passthroughConfig{}, logger)
	if err != nil {
		t.Fatalf("startThread() error = %v", err)
	}
	if threadID != "thread-abc" {
		t.Fatalf("startThread() threadID = %q, want %q", threadID, "thread-abc")
	}

	output := buf.String()
	assertMCPStartupFailureLogged(t, output)
	if !strings.Contains(output, "session_id=thread-abc") {
		t.Errorf("output missing session_id=thread-abc (the thread id is already known at this point): %s", output)
	}
}

// TestMCPStartupFailure_LoggedWhenLeftQueuedAtHandshakeEnd reproduces a
// real captured MCP startup failure notification left queued in the
// inbox once the handshake has finished (state.threadID already set),
// and asserts drainHandshakeMessages reports it exactly as the turn
// loop would, scoped to the session.
func TestMCPStartupFailure_LoggedWhenLeftQueuedAtHandshakeEnd(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	state := &sessionState{threadID: "thread-xyz", inbox: jsonrpc.NewInbox[jsonrpc.Message]()}
	msg := jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "mcpServer/startupStatus/updated", Params: extractParams(t, mcpStartupFailedLine)}
	state.inbox.Put(msg)

	drainHandshakeMessages(state, logger)

	output := buf.String()
	assertMCPStartupFailureLogged(t, output)
	if !strings.Contains(output, "session_id=thread-xyz") {
		t.Errorf("output missing session_id=thread-xyz: %s", output)
	}
}

// TestMCPStartupFailure_LoggedDuringLoginWaitHasNoSessionScope drives
// authenticateIfNeeded's login wait against a peer that, before sending
// account/login/completed, delivers a real captured MCP startup failure
// notification. The thread id is not resolved yet at this point in the
// handshake, so the WARN record must carry no session_id: it is logged
// through StartSession's own logger, unscoped.
func TestMCPStartupFailure_LoggedDuringLoginWaitHasNoSessionScope(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "test-api-key")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	state := handshakeState(t,
		`{"id":1,"result":{"account":null}}`,
		`{"id":2,"result":{}}`,
		mcpStartupFailedLine,
		`{"method":"account/login/completed","params":{"success":true}}`,
	)

	if err := authenticateIfNeeded(context.Background(), state, logger); err != nil {
		t.Fatalf("authenticateIfNeeded() error = %v", err)
	}

	output := buf.String()
	assertMCPStartupFailureLogged(t, output)
	if strings.Contains(output, "session_id=") {
		t.Errorf("output carries session_id before the thread id is known, want none: %s", output)
	}
}

// TestHandleOutOfTurnMessage_ElicitationRequestDeclinedOnce checks that
// a mcpServer/elicitation/request arriving outside a turn is answered
// with a decline, exactly once.
func TestHandleOutOfTurnMessage_ElicitationRequestDeclinedOnce(t *testing.T) {
	t.Parallel()

	recorder := &capturingWriteCloser{}
	inbox := jsonrpc.NewInbox[jsonrpc.Message]()
	conn := jsonrpc.NewConn(recorder, strings.NewReader(""), jsonrpc.Deliver(inbox, identity))
	t.Cleanup(conn.Close)
	state := &sessionState{conn: conn}

	msg := jsonrpc.Message{Kind: jsonrpc.KindRequest, ID: jsonrpc.NumberID(501), Method: "mcpServer/elicitation/request"}
	handleOutOfTurnMessage(state, msg, discardTestLogger())

	if err := conn.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	recorder.mu.Lock()
	writes := append([]string(nil), recorder.writes...)
	recorder.mu.Unlock()

	if len(writes) != 1 {
		t.Fatalf("wrote %d replies, want exactly 1: %v", len(writes), writes)
	}
	if !strings.Contains(writes[0], `"id":501`) || !strings.Contains(writes[0], `"action":"decline"`) {
		t.Errorf("reply = %q, want an id=501 decline", writes[0])
	}
}

// TestHandleOutOfTurnMessage_UnrecognizedRequestAnsweredMethodNotFoundOnce
// checks that an unrecognized server request, currentTime/read per the
// live app-server's own schema, arriving outside a turn is answered
// with method-not-found exactly once, and that a notification of the
// same unrecognized method gets no reply at all.
func TestHandleOutOfTurnMessage_UnrecognizedRequestAnsweredMethodNotFoundOnce(t *testing.T) {
	t.Parallel()

	recorder := &capturingWriteCloser{}
	inbox := jsonrpc.NewInbox[jsonrpc.Message]()
	conn := jsonrpc.NewConn(recorder, strings.NewReader(""), jsonrpc.Deliver(inbox, identity))
	t.Cleanup(conn.Close)
	state := &sessionState{conn: conn}

	handleOutOfTurnMessage(state, jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "currentTime/read"}, discardTestLogger())
	handleOutOfTurnMessage(state, jsonrpc.Message{Kind: jsonrpc.KindRequest, ID: jsonrpc.NumberID(502), Method: "currentTime/read"}, discardTestLogger())

	if err := conn.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	recorder.mu.Lock()
	writes := append([]string(nil), recorder.writes...)
	recorder.mu.Unlock()

	if len(writes) != 1 {
		t.Fatalf("wrote %d replies, want exactly 1 (the notification must get none): %v", len(writes), writes)
	}
	if !strings.Contains(writes[0], `"id":502`) {
		t.Errorf("reply = %q, want it to answer id=502", writes[0])
	}
	if !strings.Contains(writes[0], strconv.Itoa(jsonrpc.MethodNotFoundCode)) {
		t.Errorf("reply = %q, want the method-not-found code %d", writes[0], jsonrpc.MethodNotFoundCode)
	}
}

// TestRunTurn_UnrecognizedRequestAnsweredMethodNotFoundOnce drives a
// full turn whose stream carries an unrecognized server request,
// currentTime/read, followed by an unrecognized notification, before
// turn/completed. The request must be answered with method-not-found
// exactly once; the notification must get no reply.
func TestRunTurn_UnrecognizedRequestAnsweredMethodNotFoundOnce(t *testing.T) {
	t.Parallel()

	recorder := &capturingWriteCloser{}
	fixture := strings.Join([]string{
		`{"id":1,"result":{"turn":{"id":"t1","status":"starting"}}}`,
		`{"id":503,"method":"currentTime/read","params":{"threadId":"thread-001"}}`,
		`{"method":"some/unsolicited-out-of-band","params":{}}`,
		`{"method":"turn/completed","params":{"turn":{"id":"t1","status":"completed"}}}`,
	}, "\n")
	state := makeTestStateWithStdin(t, []byte(fixture), recorder)
	adapter, _ := NewCodexAdapter()

	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	if err := state.conn.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	recorder.mu.Lock()
	writes := append([]string(nil), recorder.writes...)
	recorder.mu.Unlock()

	var replies []string
	for _, w := range writes {
		if strings.Contains(w, `"id":503`) {
			replies = append(replies, w)
		}
	}
	if len(replies) != 1 {
		t.Fatalf("wrote %d replies naming id=503, want exactly 1: %v", len(replies), writes)
	}
	if !strings.Contains(replies[0], strconv.Itoa(jsonrpc.MethodNotFoundCode)) {
		t.Errorf("reply = %q, want the method-not-found code %d", replies[0], jsonrpc.MethodNotFoundCode)
	}
}

// extractParams decodes the "params" member of a raw JSON-RPC line, for
// building a jsonrpc.Message by hand from a captured fixture line.
func extractParams(t *testing.T, line string) []byte {
	t.Helper()
	_, rest, ok := strings.Cut(line, `"params":`)
	if !ok {
		t.Fatalf("line %q carries no params member", line)
	}
	rest = strings.TrimSuffix(rest, "}")
	return []byte(rest)
}
