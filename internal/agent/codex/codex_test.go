//go:build unix

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
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
				Work:              agentcore.WorkUnobservable,
			},
		},
		{
			name:          "already-cancelled context maps to cancellation",
			contextIsLive: false,
			wantEvidence: agentcore.TurnEvidence{
				Terminal:        agentcore.TerminalCancelled,
				TerminalMessage: "turn cancelled after the runtime reported status interrupted",
				Work:            agentcore.WorkUnobservable,
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
				Work:            agentcore.WorkUnobservable,
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
				Work:              agentcore.WorkUnobservable,
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
				Work:              agentcore.WorkUnobservable,
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
