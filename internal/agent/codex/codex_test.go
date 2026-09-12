//go:build unix

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// requireAgentError asserts err is a *domain.AgentError with the given Kind.
func requireAgentError(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

func TestNewCodexAdapter(t *testing.T) {
	t.Parallel()

	t.Run("nil config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(nil)
		if err != nil {
			t.Fatalf("NewCodexAdapter(nil) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("empty config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCodexAdapter(empty) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("all passthrough fields stored", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{
			"model":           "o4-mini",
			"effort":          "high",
			"approval_policy": "never",
			"thread_sandbox":  "workspaceWrite",
			"personality":     "helpful",
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := adapter.(*CodexAdapter)
		if a.passthrough.Model != "o4-mini" {
			t.Errorf("passthrough.Model = %q, want %q", a.passthrough.Model, "o4-mini")
		}
		if a.passthrough.Effort != "high" {
			t.Errorf("passthrough.Effort = %q, want %q", a.passthrough.Effort, "high")
		}
		if a.passthrough.ApprovalPolicy != "never" {
			t.Errorf("passthrough.ApprovalPolicy = %q, want %q", a.passthrough.ApprovalPolicy, "never")
		}
		if a.passthrough.ThreadSandbox != "workspaceWrite" {
			t.Errorf("passthrough.ThreadSandbox = %q, want %q", a.passthrough.ThreadSandbox, "workspaceWrite")
		}
		if a.passthrough.Personality != "helpful" {
			t.Errorf("passthrough.Personality = %q, want %q", a.passthrough.Personality, "helpful")
		}
	})

	t.Run("tool_registry config key is not read", func(t *testing.T) {
		t.Parallel()
		reg := domain.NewToolRegistry()
		withKey, err := NewCodexAdapter(map[string]any{
			"tool_registry": reg,
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		without, err := NewCodexAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := withKey.(*CodexAdapter)
		b := without.(*CodexAdapter)
		if !reflect.DeepEqual(a.passthrough, b.passthrough) {
			t.Errorf("adapter constructed with tool_registry present = %+v, want identical to %+v", a.passthrough, b.passthrough)
		}
	})
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("codex")
	if err != nil {
		t.Fatalf(`registry.Agents.Get("codex") error = %v`, err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*CodexAdapter); !ok {
		t.Errorf("factory() type = %T, want *CodexAdapter", adapter)
	}
}

func TestStartSession_EmptyWorkspace(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_NonexistentPath(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: "/nonexistent/path/that/does/not/exist/codex-test",
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_WorkspaceIsFile(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpFile,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_BinaryNotFound(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

// TestRunTurn_UsageMeasured_AbsentWhenNoTokenUsageNotification drives a
// turn whose stream carries no thread/tokenUsage/updated notification and
// asserts the run is reported unmeasured.
func TestRunTurn_UsageMeasured_AbsentWhenNoTokenUsageNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertMeasurementAbsent(t, events, result)
}

// TestRunTurn_UsageMeasured_TrueOnTokenUsageNotification drives a turn
// whose stream carries a thread/tokenUsage/updated notification and
// asserts the run is reported measured.
func TestRunTurn_UsageMeasured_TrueOnTokenUsageNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if !result.UsageMeasured {
		t.Error("RunTurn().UsageMeasured = false, want true when a thread/tokenUsage/updated notification carried a token-usage object")
	}
}

// TestRunTurn_CompletedTurnReturnsUntypedNilError pins that a completed
// turn's returned error interface is genuinely nil, not a typed-nil
// *domain.AgentError promoted to a non-nil error interface.
func TestRunTurn_CompletedTurnReturnsUntypedNilError(t *testing.T) {
	t.Parallel()

	state := makeTestState(t, loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil (not a typed-nil *domain.AgentError)", err)
	}
}

// atomicErrContext behaves like context.Background() to every consumer
// that selects on Done() (the channel is nil and never closes), but its
// Err() method reports context.Canceled once cancelled is set, so a
// test can drive a code path that reads ctx.Err() synchronously without
// racing RunTurn's own <-ctx.Done() select case.
type atomicErrContext struct {
	context.Context
	cancelled *atomic.Bool
}

func (c atomicErrContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// newInterruptedStatusState builds a sessionState wired to a real
// jsonrpc.Conn over an io.Pipe, whose peer answers the turn/start call
// only once it observes codex write it (required so the response
// cannot be misrouted as unmatched, ahead of Call's own pending-map
// registration), and whose msgCh is unbuffered so the test driving it
// can set the cancellation flag at the exact point before the
// turn/completed notification: an unbuffered send only returns once
// RunTurn's own goroutine has received it, and the Go memory model
// guarantees everything the test did before that receive (including
// setting the cancellation flag) happened before the send returns.
// stdin, when non-nil, receives every line codex writes, including
// the turn/start request itself.
func newInterruptedStatusState(t *testing.T, stdin io.Writer) *sessionState {
	t.Helper()

	sig := newSignalingWriter(stdin)
	inPr, inPw := io.Pipe()
	t.Cleanup(func() {
		_ = inPr.Close()
		_ = inPw.Close()
	})

	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		msgCh:      make(chan jsonrpc.Message),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}
	state.conn = jsonrpc.NewConn(sig, inPr, sessionHandler(state))
	go watchTermination(state)
	state.turnPhase.Store(true)

	go func() {
		<-sig.done
		_, _ = fmt.Fprintln(inPw, `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`)
	}()

	return state
}

// TestRunTurn_InterruptedStatus pins the context-gated mapping for a
// turn/completed notification reporting status interrupted: cancelled
// only when the turn context is already done, failed (preserving the
// retryable classification) when the context is still live.
func TestRunTurn_InterruptedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		contextIsLive bool
		wantEvidence  agentcore.TurnEvidence
	}{
		{
			name:          "live context maps to failure, preserving the retryable classification",
			contextIsLive: true,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   "turn interrupted",
			},
		},
		{
			name:          "already-cancelled context maps to cancellation",
			contextIsLive: false,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: "turn cancelled after the runtime reported status interrupted",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter(map[string]any{})
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			if !tt.contextIsLive {
				cancelled.Store(true)
			}

			// This send returns only once RunTurn's main loop is ready to
			// receive it, which happens strictly after the turn/start call
			// has returned and after the ctx.Err() fast-path check that
			// follows it.
			state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(`{"turn":{"id":"turn-001","status":"interrupted"}}`)}

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, tt.wantEvidence, result, err)
		})
	}
}

// TestRunTurn_CompletedNotificationUnderCancelledContext pins the
// cancelled-context mapping table: whatever status word (or absence of
// one) the runtime reports in a turn/completed notification once the
// orchestrator has already cancelled the turn, the disposition is a
// cancellation carrying a message that names both facts.
func TestRunTurn_CompletedNotificationUnderCancelledContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		params      string
		wantMessage string
	}{
		{
			name:        "completed status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"completed"}}`,
			wantMessage: "turn cancelled after the runtime reported status completed",
		},
		{
			name:        "interrupted status with no error object",
			params:      `{"turn":{"id":"turn-001","status":"interrupted"}}`,
			wantMessage: "turn cancelled after the runtime reported status interrupted",
		},
		{
			name:        "failed status carrying a turn.error object",
			params:      `{"turn":{"id":"turn-001","status":"failed","error":{"message":"context window exceeded","codexErrorInfo":"ContextWindowExceeded"}}}`,
			wantMessage: "turn cancelled after the runtime reported status failed: context window exceeded",
		},
		{
			name:        "unrecognized status",
			params:      `{"turn":{"id":"turn-001","status":"queued_for_review"}}`,
			wantMessage: "turn cancelled after the runtime reported status queued_for_review",
		},
		{
			name:        "payload with no status member",
			params:      `{"turn":{"id":"turn-001"}}`,
			wantMessage: "turn cancelled after the runtime reported no status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newInterruptedStatusState(t, nil)

			var cancelled atomic.Bool
			ctx := atomicErrContext{Context: context.Background(), cancelled: &cancelled}

			type outcome struct {
				result domain.TurnResult
				err    error
			}
			outcomeCh := make(chan outcome, 1)

			adapter, _ := NewCodexAdapter(map[string]any{})
			go func() {
				result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
					Prompt:  "go",
					OnEvent: func(domain.AgentEvent) {},
				})
				outcomeCh <- outcome{result, err}
			}()

			cancelled.Store(true)

			// This send returns only once RunTurn's main loop is ready to
			// receive it, which happens strictly after the turn/start call
			// has returned and after the ctx.Err() fast-path check that
			// follows it.
			state.msgCh <- jsonrpc.Message{Kind: jsonrpc.KindNotification, Method: "turn/completed", Params: json.RawMessage(tt.params)}

			got := <-outcomeCh
			result, err := got.result, got.err

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: tt.wantMessage,
			}, result, err)
		})
	}
}

// TestRunTurn_FailedOrUnrecognizedStatus pins that a failed status with
// no turn.error object, and any status other than completed,
// interrupted, and failed, both produce turn_failed with a non-nil
// error and the message "turn <status>", unchanged from before the
// shared decision.
func TestRunTurn_FailedOrUnrecognizedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		wantMessage string
	}{
		{
			name:        "failed status with no turn.error object",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"failed"}}}` + "\n",
			wantMessage: "turn failed",
		},
		{
			name:        "unrecognized status",
			fixture:     `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"queued_for_review"}}}` + "\n",
			wantMessage: "turn queued_for_review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" + tt.fixture
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrTurnFailed {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
			}
			if agentErr.Message != tt.wantMessage {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMessage)
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   tt.wantMessage,
			}, result, err)
		})
	}
}

// TestRunTurn_EmptyTurnStatus pins both ways a turn/completed
// notification can arrive without a status word: params that fail to
// unmarshal into an object, and a turn object that omits the status
// member. Neither builds a terminal message from the empty status, so
// agentcore.DecideTurn's own fallback message applies.
func TestRunTurn_EmptyTurnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params string
	}{
		{"params do not unmarshal into an object", `[1,2,3]`},
		{"turn object omits the status member", `{"turn":{"id":"turn-001"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := `{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}` + "\n" +
				`{"method":"turn/completed","params":` + tt.params + `}` + "\n"
			state := makeTestState(t, []byte(fixture))
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrTurnFailed {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
			}
			if agentErr.Message != "turn failed" {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, "turn failed")
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
			}, result, err)
		})
	}
}

// TestRunTurn_StdoutParseFailure pins that a stdout line that fails to
// parse produces the same text on both the emitted event and the
// returned error, prefixed "stdout read error: ", and preserves the
// underlying parse error on the unwrap chain.
func TestRunTurn_StdoutParseFailure(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"not valid json\n"
	state := makeTestState(t, []byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if !strings.HasPrefix(agentErr.Message, "stdout read error: ") {
		t.Errorf("AgentError.Message = %q, want prefix %q", agentErr.Message, "stdout read error: ")
	}

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != agentErr.Message {
		t.Errorf("turn_failed Message = %q, want the same text as AgentError.Message %q", turnFailedEvents[0].Message, agentErr.Message)
	}
}

// entrySignalingWriter wraps an io.Writer and closes done the instant
// Write is entered, before delegating to the wrapped writer. Unlike
// signalingWriter, which signals once Write returns, this lets a test
// observe that a write has been parked inside a call that never
// returns on its own.
type entrySignalingWriter struct {
	w    io.Writer
	once sync.Once
	done chan struct{}
}

func newEntrySignalingWriter(w io.Writer) *entrySignalingWriter {
	return &entrySignalingWriter{w: w, done: make(chan struct{})}
}

func (s *entrySignalingWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.done) })
	return s.w.Write(p)
}

// TestStopSession_ReturnsWhileWriteParked checks that StopSession
// returns while another goroutine is parked writing on the session's
// connection, proving that closeConnAndStop's call to conn.Close does
// not wait for that write. The fixture leaves no process handle and
// no wait channel, and readerDone already closed, so no other bounded
// wait in StopSession can substitute for that proof.
func TestStopSession_ReturnsWhileWriteParked(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	sig := newEntrySignalingWriter(pw)

	readerDone := make(chan struct{})
	close(readerDone)

	state := &sessionState{
		msgCh:      make(chan jsonrpc.Message),
		readerDone: readerDone,
		stopCh:     make(chan struct{}),
	}
	state.conn = jsonrpc.NewConn(sig, strings.NewReader(""), sessionHandler(state))

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- state.conn.Notify("parked/notify", nil)
	}()

	// Registered before the assertions below, so a t.Fatal on the
	// regression path still releases the parked write.
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
		select {
		case <-writeErr:
		case <-time.After(2 * time.Second):
		}
	})

	select {
	case <-sig.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the write to enter the wrapped writer")
	}

	stopErr := make(chan error, 1)
	go func() {
		stopErr <- (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	}()

	select {
	case err := <-stopErr:
		if err != nil {
			t.Errorf("StopSession() error = %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StopSession() did not return while a write was parked, want it to return well short of its own 5-second graceful-wait ceiling")
	}
}

// startFakeCodexProcess writes scriptBody, touching a readiness marker
// right after its leading trap statement so a caller's subsequent
// SignalGraceful cannot race the shell installing the trap, starts it
// in its own process group, and wires a minimal sessionState around it
// with an already-closed readerDone (this harness has no reader
// goroutine for StopSession to wait for).
func startFakeCodexProcess(t *testing.T, scriptBody string, stopGraceMS int) *sessionState {
	t.Helper()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	trapLine, rest, ok := strings.Cut(scriptBody, "\n")
	if !ok || !strings.HasPrefix(trapLine, "trap ") {
		t.Fatalf("startFakeCodexProcess: scriptBody must start with a trap statement, got %q", scriptBody)
	}
	script := trapLine + "\ntouch '" + readyPath + "'\n" + rest
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() { procutil.KillProcessGroup(cmd.Process.Pid) }) //nolint:errcheck // best-effort cleanup

	readerDone := make(chan struct{})
	close(readerDone)

	waitCh := make(chan struct{})
	state := &sessionState{
		agentConfig: domain.AgentConfig{StopGraceMS: stopGraceMS},
		proc:        cmd.Process,
		waitCh:      waitCh,
		readerDone:  readerDone,
		stopCh:      make(chan struct{}),
	}
	go func() {
		cmd.Wait() //nolint:errcheck,gosec // best-effort reap; exit state is irrelevant here
		close(waitCh)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("startFakeCodexProcess: readiness marker %q did not appear within 5s", readyPath)
	return nil
}

// TestStopSession_ConfiguredGraceBoundsTheWait asserts that a
// configured agent.stop_grace_ms bounds StopSession's graceful wait,
// not the built-in five-second default.
func TestStopSession_ConfiguredGraceBoundsTheWait(t *testing.T) {
	t.Parallel()

	state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 200)

	start := time.Now()
	err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("StopSession() = %v, want nil", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("StopSession() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded the wait, not DefaultStopGrace)", elapsed)
	}
}

// TestStopSession_EscalationLogging asserts the escalation records
// codex's StopSession emits: Debug on an exit inside the grace, and
// Warn naming the outcome, the configured ceiling and the elapsed wait
// when the phase ends without one. Both escalation outcomes are
// reachable here, because StopSession ends the phase on whichever of
// the grace and the caller's deadline arrives first.
func TestStopSession_EscalationLogging(t *testing.T) {
	// No t.Parallel(): installs a global slog default.

	t.Run("exit_inside_grace_emits_debug_and_no_warn", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than this exit needs. The subtest proves
		// which record the clean-exit path emits, not that the grace bounds
		// anything, and a tight bound races the runner's scheduler instead
		// of testing the code.
		state := startFakeCodexProcess(t, `trap 'exit 0' TERM
while :; do :; done`, 30000)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent exited during the graceful phase") {
			t.Errorf("StopSession() did not log the exited-inside-grace Debug record: %s", output)
		}
		if !strings.Contains(output, `outcome=exited`) {
			t.Errorf("StopSession()'s Debug record missing outcome=exited: %s", output)
		}
		if strings.Contains(output, "level=WARN") {
			t.Errorf("StopSession() logged a Warn record for a clean exit, want none: %s", output)
		}
	})

	t.Run("grace_elapsed_emits_warn_with_outcome_and_grace", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 150)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent did not exit inside the graceful period and was force-terminated") {
			t.Errorf("StopSession() did not log the grace-elapsed Warn record: %s", output)
		}
		if !strings.Contains(output, `outcome="grace elapsed"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="grace elapsed": %s`, output)
		}
		if !strings.Contains(output, "grace=") {
			t.Errorf("StopSession()'s Warn record missing the configured grace ceiling: %s", output)
		}
		if !strings.Contains(output, "elapsed=") {
			t.Errorf("StopSession()'s Warn record missing the elapsed wait: %s", output)
		}
	})

	t.Run("caller_deadline_ends_the_phase_and_is_reported", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than the deadline, so only a
		// StopSession that reads its context can end this phase. When
		// it ignored the context, this arm waited out the whole grace
		// and then reported success.
		state := startFakeCodexProcess(t, `trap '' TERM
while :; do :; done`, 30000)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := (&CodexAdapter{}).StopSession(ctx, domain.Session{Internal: state})
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("StopSession() = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > 10*time.Second {
			t.Errorf("StopSession() returned after %v, want the caller's deadline to end the phase far below the 30s grace", elapsed)
		}

		output := buf.String()
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="caller deadline": %s`, output)
		}
		if !strings.Contains(output, "grace=30s") {
			t.Errorf("StopSession()'s Warn record did not report the configured 30s ceiling: %s", output)
		}
	})
}

// writeFakeAppServerScriptTerminalParked creates a script that fakes the
// codex app-server handshake exactly as writeFakeAppServerScript does,
// then reads a decimal fill count from its own standard input, writes
// that many filler notifications as a single write, and only after a
// second line arrives on its standard input does it write the terminal
// turn/completed notification and exit. Properties P10 and P11 drive
// state.msgCh directly against this script rather than through RunTurn,
// because a draining consumer cannot hold the reader parked and a
// parked reader cannot deliver turn/start's own response in the first
// place.
func writeFakeAppServerScriptTerminalParked(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	content := `read -r _init_req
printf '{"id":1,"result":{}}\n'
read -r _initialized_notif
read -r _account_read_req
printf '{"id":2,"result":{}}\n'
read -r _thread_start_req
printf '{"id":3,"result":{"thread":{"id":"fake-thread-1"}}}\n'
printf '{"method":"thread/started","params":{}}\n'
read -r COUNT
i=0
FILL=""
while [ "$i" -lt "$COUNT" ]; do
  FILL="${FILL}{\"method\":\"filler/notification\",\"params\":{\"i\":$i}}
"
  i=$((i+1))
done
printf '%s' "$FILL"
read -r _go
printf '{"method":"turn/completed","params":{"turn":{"id":"t1","status":"completed"}}}\n'
`
	return agenttest.WriteScript(t, dir, "fake-codex-app-server-terminal-parked", content)
}

// runTerminalMessageFixture drives writeFakeAppServerScriptTerminalParked
// through the five-step sequence properties P10 and P11 share: it fills
// state.msgCh to capacity plus one so the connection's reader is parked
// delivering the last filler message, waits for that to be observed,
// signals the script to write its terminal message and exit, waits for
// state.waitCh (Reaper.Done) so the reap is observed before the reader
// can resume, and only then drains state.msgCh until it closes.
//
// closeStdoutAfterReap reproduces, locally in this test's own wiring,
// the truncation exec.Cmd.Wait used to perform before the ownership
// move: closing the standard-output read end at the instant the reap is
// observed. It is never applied in production code. Property P10 calls
// this with false; P11, the negative control, calls it with true and
// expects the terminal message to be lost.
//
// It returns whether a turn/completed notification was among the
// messages drained.
func runTerminalMessageFixture(t *testing.T, closeStdoutAfterReap bool) bool {
	t.Helper()
	t.Setenv("CODEX_API_KEY", "")

	script := writeFakeAppServerScriptTerminalParked(t)

	adapter := &CodexAdapter{drainGrace: 10 * time.Second}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	t.Cleanup(func() {
		_ = adapter.StopSession(context.Background(), session)
	})

	// The capacity travels over the wire rather than as a literal: a
	// second copy of codex.go's make(chan jsonrpc.Message, 16) would
	// silently stop exercising the park if that capacity ever changes.
	capacity := cap(state.msgCh)
	if _, err := fmt.Fprintf(state.stdin, "%d\n", capacity+1); err != nil {
		t.Fatalf("write fill count: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(state.msgCh) < capacity {
		if time.Now().After(deadline) {
			t.Fatalf("len(state.msgCh) = %d, want %d within 5s (the reader must be parked delivering the fill)", len(state.msgCh), capacity)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := fmt.Fprintln(state.stdin, "go"); err != nil {
		t.Fatalf("write go signal: %v", err)
	}

	select {
	case <-state.waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("state.waitCh (Reaper.Done) did not close within 5s")
	}

	if closeStdoutAfterReap {
		if state.pipes == nil {
			t.Fatal("state.pipes is nil at the reap, want the pipes still owned by the session")
		}
		if err := state.pipes.CloseStdout(); err != nil {
			t.Fatalf("CloseStdout() = %v", err)
		}
	}

	var sawCompleted bool
	drainDeadline := time.After(5 * time.Second)
drain:
	for {
		select {
		case msg, ok := <-state.msgCh:
			if !ok {
				break drain
			}
			if msg.Kind == jsonrpc.KindNotification && msg.Method == "turn/completed" {
				sawCompleted = true
			}
		case <-drainDeadline:
			t.Fatal("draining state.msgCh did not finish within 5s")
		}
	}

	return sawCompleted
}

// TestStartSession_TerminalMessageDeliveredWhileReaderParked covers
// property P10: a session whose runtime writes a terminal message and
// exits while the connection's reader is parked mid-dispatch still
// delivers that message to state.msgCh, because the reap no longer
// closes the read end out from under it.
func TestStartSession_TerminalMessageDeliveredWhileReaderParked(t *testing.T) {
	if !runTerminalMessageFixture(t, false) {
		t.Error("turn/completed was not delivered to state.msgCh, want it recovered once the reader resumed")
	}
}

// TestStartSession_ReapClosingStdoutTruncatesTerminalMessage is
// property P11, the negative control for P10: closing the standard-
// output read end at the instant the reap is observed, which is where
// exec.Cmd.Wait closed it before this change, loses the terminal
// message P10 recovers. A green result here means the fixture never
// parked the reader in the first place, and the fixture rather than the
// production change would be what to fix.
func TestStartSession_ReapClosingStdoutTruncatesTerminalMessage(t *testing.T) {
	if runTerminalMessageFixture(t, true) {
		t.Error("turn/completed was delivered despite closing the read end at the reap, want it lost (negative control did not reproduce the truncation)")
	}
}
