//go:build unix

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// nopWriteCloser is an io.WriteCloser that discards all writes.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// countingDoneContext records each evaluation of Done so a test can detect
// whether a closed cancellation channel keeps a select loop running.
type countingDoneContext struct {
	context.Context
	calls atomic.Int64
}

func (c *countingDoneContext) Done() <-chan struct{} {
	c.calls.Add(1)
	return c.Context.Done()
}

// interruptTrackingWriteCloser observes best-effort turn/interrupt requests.
type interruptTrackingWriteCloser struct {
	interrupts chan struct{}
	count      atomic.Int64
}

func (w *interruptTrackingWriteCloser) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"method":"turn/interrupt"`)) {
		w.count.Add(1)
		select {
		case w.interrupts <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (*interruptTrackingWriteCloser) Close() error { return nil }

// loadFixture reads testdata/<name> and returns its bytes.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	return data
}

// makeTestState builds a sessionState backed by in-memory pipes, safe
// for use in RunTurn and handleToolCall unit tests that do not launch a
// real subprocess.
func makeTestState(fixtureData []byte) *sessionState {
	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		stdin:      nopWriteCloser{},
		stdout:     io.NopCloser(bytes.NewReader(nil)),
		msgCh:      make(chan parsedMessage, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
		acc:        agentcore.NewRunUsage(),
	}

	go func() {
		defer close(state.readerDone)
		defer close(state.msgCh)
		scanner := bufio.NewScanner(bytes.NewReader(fixtureData))
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for scanner.Scan() {
			msg := parseMessage(scanner.Bytes())
			select {
			case state.msgCh <- msg:
			case <-state.stopCh:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case state.msgCh <- parsedMessage{Err: err}:
			case <-state.stopCh:
			}
		}
	}()

	return state
}

// makeTestStateWithStdin behaves like makeTestState but installs stdin so a
// test can capture the exact bytes RunTurn writes back to the app-server.
func makeTestStateWithStdin(fixtureData []byte, stdin io.WriteCloser) *sessionState {
	state := makeTestState(fixtureData)
	state.stdin = stdin
	return state
}

// fakeSession wraps state in a domain.Session suitable for RunTurn.
func fakeSession(state *sessionState) domain.Session {
	return domain.Session{
		ID:       state.threadID,
		AgentPID: "12345",
		Internal: state,
	}
}

// collectEvents is an OnEvent callback that appends to a slice.
func collectEvents(events *[]domain.AgentEvent) func(domain.AgentEvent) {
	var mu sync.Mutex
	return func(e domain.AgentEvent) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

// firstEventOfType returns the first event with the given type, or the
// zero value if none was found.
func firstEventOfType(events []domain.AgentEvent, t domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == t {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

func TestRunTurn_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	session := domain.Session{Internal: "not-a-session-state"}
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_SuccessfulTurn(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}
	if result.SessionID != "thread-001" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "thread-001")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event, none found")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal: agentcore.TerminalSuccess,
		Work:     agentcore.WorkUnobservable,
	}, result, err)
}

// filterEventsOfType returns every event of the given type, in order.
func filterEventsOfType(events []domain.AgentEvent, typ domain.AgentEventType) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestRunTurn_TokenUsageUpdated drives RunTurn against a session
// captured from codex 0.121.0 carrying two thread/tokenUsage/updated
// notifications for the current turn followed by a turn/completed with
// no usage member (the app-server protocol carries none there). It
// asserts two token_usage events are emitted, the run-cumulative
// snapshot after the second matches the notification's total minus the
// zero baseline of a fresh thread, the turn_completed event carries the
// same snapshot, and TurnResult.Usage equals it.
func TestRunTurn_TokenUsageUpdated(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "token_usage_updated.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 2 {
		t.Fatalf("token_usage event count = %d, want 2", len(tokenUsageEvents))
	}

	wantSnapshot := domain.TokenUsage{InputTokens: 27549, OutputTokens: 82, CacheReadTokens: 27392, TotalTokens: 27631}
	if got := tokenUsageEvents[1].Usage; got != wantSnapshot {
		t.Errorf("second token_usage event Usage = %+v, want %+v", got, wantSnapshot)
	}

	completed, ok := firstEventOfType(events, domain.EventTurnCompleted)
	if !ok {
		t.Fatal("expected turn_completed event, none found")
	}
	if completed.Usage != wantSnapshot {
		t.Errorf("turn_completed.Usage = %+v, want %+v", completed.Usage, wantSnapshot)
	}
	if result.Usage != wantSnapshot {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantSnapshot)
	}

	agenttest.AssertUsageContract(t, events)
}

// TestRunTurn_TokenUsageUpdated_ResumedThreadBaseline drives a thread
// whose first matching notification already reports a non-zero
// total.totalTokens because the thread resumed from a prior run. It
// asserts a preceding notification carrying another turn's id
// contributes nothing and only raises the baseline, and that the first
// reported snapshot for the current turn is the difference between that
// notification's total and last breakdowns.
func TestRunTurn_TokenUsageUpdated_ResumedThreadBaseline(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-002\"}}\n" +
		// A notification from an earlier turn of the resumed thread:
		// contributes nothing to this turn and only raises the baseline.
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-001\",\"tokenUsage\":{\"last\":{\"totalTokens\":5000,\"inputTokens\":4000,\"cachedInputTokens\":1000,\"outputTokens\":1000,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":5000,\"inputTokens\":4000,\"cachedInputTokens\":1000,\"outputTokens\":1000,\"reasoningOutputTokens\":0}}}}\n" +
		// The current turn's first matching notification: total is
		// already thread-cumulative from the resumed session.
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-002\",\"tokenUsage\":{\"last\":{\"totalTokens\":13846,\"inputTokens\":13818,\"cachedInputTokens\":13692,\"outputTokens\":28,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":27631,\"inputTokens\":27549,\"cachedInputTokens\":27392,\"outputTokens\":82,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"completed\"}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "continue",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 1 {
		t.Fatalf("token_usage event count = %d, want 1 (the other-turn notification contributes nothing)", len(tokenUsageEvents))
	}

	// The baseline is total minus last (27549-13818, 82-28, 27392-13692),
	// so the reported snapshot equals last exactly: total_tokens 13846.
	wantSnapshot := domain.TokenUsage{InputTokens: 13818, OutputTokens: 28, CacheReadTokens: 13692, TotalTokens: 13846}
	if got := tokenUsageEvents[0].Usage; got != wantSnapshot {
		t.Errorf("token_usage event Usage = %+v, want %+v (baseline is total minus last)", got, wantSnapshot)
	}
	if result.Usage != wantSnapshot {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantSnapshot)
	}
}

// TestRunTurn_TokenUsageUpdated_EmptyTurnIDAdoptsFirstNotification
// drives a turn whose turn/start response body fails to unmarshal,
// leaving the adapter's current turn id empty, followed by two
// thread/tokenUsage/updated notifications sharing one non-empty turnId.
// It asserts the adapter adopts the first notification's turnId rather
// than treating every notification as belonging to another turn, so
// both notifications contribute and the run-cumulative snapshot after
// the second reports the full total rather than zero.
func TestRunTurn_TokenUsageUpdated_EmptyTurnIDAdoptsFirstNotification(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":\"malformed turn/start body\"}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-777\",\"tokenUsage\":{\"last\":{\"totalTokens\":13785,\"inputTokens\":13731,\"cachedInputTokens\":13700,\"outputTokens\":54,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":13785,\"inputTokens\":13731,\"cachedInputTokens\":13700,\"outputTokens\":54,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-777\",\"tokenUsage\":{\"last\":{\"totalTokens\":13846,\"inputTokens\":13818,\"cachedInputTokens\":13692,\"outputTokens\":28,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":27631,\"inputTokens\":27549,\"cachedInputTokens\":27392,\"outputTokens\":82,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-777\",\"status\":\"completed\"}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := filterEventsOfType(events, domain.EventTokenUsage)
	if len(tokenUsageEvents) != 2 {
		t.Fatalf("token_usage event count = %d, want 2 (both notifications must contribute once the empty turn id adopts the first)", len(tokenUsageEvents))
	}

	wantSnapshot := domain.TokenUsage{InputTokens: 27549, OutputTokens: 82, TotalTokens: 27631}
	got := tokenUsageEvents[1].Usage
	if got.InputTokens != wantSnapshot.InputTokens || got.OutputTokens != wantSnapshot.OutputTokens || got.TotalTokens != wantSnapshot.TotalTokens {
		t.Errorf("second token_usage event Usage = %+v, want input/output/total %+v (not zero)", got, wantSnapshot)
	}
	if result.Usage.TotalTokens != wantSnapshot.TotalTokens {
		t.Errorf("TurnResult.Usage.TotalTokens = %d, want %d", result.Usage.TotalTokens, wantSnapshot.TotalTokens)
	}
}

func TestRunTurn_FirstTurnEmitsSessionStarted(t *testing.T) {
	t.Parallel()

	// turnCount=0 → incremented to 1 inside RunTurn → EventSessionStarted.
	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "hello",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventSessionStarted)
	if !ok {
		t.Fatal("expected EventSessionStarted on first turn, not found")
	}
	if e.SessionID != "thread-001" {
		t.Errorf("EventSessionStarted.SessionID = %q, want %q", e.SessionID, "thread-001")
	}
}

func TestRunTurn_SubsequentTurnEmitsNotification(t *testing.T) {
	t.Parallel()

	// Pre-set turnCount=1 so the adapter sees this as the second turn.
	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	state.turnCount = 1
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "continue",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if _, ok := firstEventOfType(events, domain.EventSessionStarted); ok {
		t.Error("did not expect EventSessionStarted on subsequent turn")
	}
}

func TestRunTurn_FailedTurnContextWindowExceeded(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_failed.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err == nil {
		t.Fatal("RunTurn() expected error, got nil")
	}
	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnFailed)
	}
	if _, ok := firstEventOfType(events, domain.EventTurnFailed); !ok {
		t.Error("expected EventTurnFailed event, none found")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event on failed turn, none found")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   "Context window exceeded",
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

// TestRunTurn_StdoutClosedBeforeTurnCompleted pins the channel-closed
// abort arm: the disposition and kind are unchanged from before the
// shared decision, and the arm now emits exactly one turn_failed event
// where it emitted none before.
func TestRunTurn_StdoutClosedBeforeTurnCompleted(t *testing.T) {
	t.Parallel()

	// Only the turn/start response — no turn/completed — so the
	// background goroutine closes msgCh before turn/completed arrives.
	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{}}\n"
	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})
	requireAgentError(t, err, domain.ErrPortExit)

	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != "subprocess stdout closed unexpectedly" {
		t.Errorf("turn_failed Message = %q, want %q", turnFailedEvents[0].Message, "subprocess stdout closed unexpectedly")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   "subprocess stdout closed unexpectedly",
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

func TestRunTurn_StdoutEOFBeforeTurnStartResponse(t *testing.T) {
	t.Parallel()

	// Empty fixture: msgCh closes before any turn/start response arrives.
	// Tests the !ok path in the session-scoped response-wait loop.
	state := makeTestState(nil)
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

// TestRunTurn_TurnStartErrorResponse pins the turn/start error abort
// arm: the disposition and kind are unchanged from before the shared
// decision, and the arm now emits exactly one turn_failed event where
// it emitted none before.
func TestRunTurn_TurnStartErrorResponse(t *testing.T) {
	t.Parallel()

	// turn/start response carries an error — RunTurn should return ErrTurnFailed.
	fixture := "{\"id\":1,\"error\":{\"code\":-32000,\"message\":\"thread not found\"}}\n"
	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	})
	requireAgentError(t, err, domain.ErrTurnFailed)

	const wantMessage = "turn/start error: thread not found"
	turnFailedEvents := filterEventsOfType(events, domain.EventTurnFailed)
	if len(turnFailedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(turnFailedEvents))
	}
	if turnFailedEvents[0].Message != wantMessage {
		t.Errorf("turn_failed Message = %q, want %q", turnFailedEvents[0].Message, wantMessage)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   wantMessage,
		Work:              agentcore.WorkUnobservable,
	}, result, err)
}

func TestRunTurn_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	// readResponse returns context.Canceled → wrapped in ErrPortExit.
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

// TestRunTurn_CancelledMainLoopWaitsForCompletion sends cancellation only
// after the turn/start response and a notification have entered the main
// event loop. With no further messages pending, the closed Done channel must
// be disabled after one interrupt; otherwise the loop keeps evaluating it.
func TestRunTurn_CancelledMainLoopWaitsForCompletion(t *testing.T) {
	t.Parallel()

	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &countingDoneContext{Context: parentCtx}

	state := newInterruptedStatusState()
	close(state.readerDone)
	stdin := &interruptTrackingWriteCloser{interrupts: make(chan struct{}, 1)}
	state.stdin = stdin

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})

	adapter, _ := NewCodexAdapter(map[string]any{})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(domain.AgentEvent) {},
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		close(state.msgCh)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	// The unbuffered sends return only after RunTurn has received each
	// message. The second message therefore proves that its main event loop,
	// rather than the turn/start response loop, is running before cancellation.
	state.msgCh <- parseMessage([]byte(`{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`))
	state.msgCh <- parseMessage([]byte(`{"method":"turn/started","params":{"turnId":"turn-001"}}`))

	cancel()
	select {
	case <-stdin.interrupts:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not send turn/interrupt after cancellation")
	}

	callsAfterInterrupt := ctx.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := ctx.calls.Load(); got != callsAfterInterrupt {
		t.Errorf("ctx.Done() call count changed with no pending message: got %d, want %d", got, callsAfterInterrupt)
	}
	if got := stdin.count.Load(); got != 1 {
		t.Errorf("turn/interrupt request count = %d, want 1", got)
	}

	// A terminal event must still be received and mapped after cancellation.
	select {
	case state.msgCh <- parseMessage([]byte(`{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"interrupted"}}}`)):
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not wait for turn/completed after cancellation")
	}

	select {
	case got := <-outcomeCh:
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return after turn/completed")
	}
}

func TestRunTurn_ItemStartedAndCompletedEmitsToolResult(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_items.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "run command",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from item tracking, not found")
	}
	if e.ToolName != "ls -la" {
		t.Errorf("ToolResult.ToolName = %q, want %q", e.ToolName, "ls -la")
	}
}

func TestRunTurn_AgentMessageTextEmitsNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_agent_message.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "explain",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// item/completed with agentMessage type and non-empty text emits EventNotification.
	notifs := 0
	for _, e := range events {
		if e.Type == domain.EventNotification {
			notifs++
		}
	}
	if notifs == 0 {
		t.Error("expected at least one EventNotification for agentMessage text, found none")
	}
}

func TestRunTurn_MiscNotifications(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// some/unknown/method → EventOtherMessage
	if _, ok := firstEventOfType(events, domain.EventOtherMessage); !ok {
		t.Error("expected EventOtherMessage for unknown notification method, not found")
	}
	// turn/plan/updated → EventNotification
	found := false
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == "plan updated" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'plan updated' EventNotification, not found")
	}
}

func TestRunTurn_ToolCallWithNilRegistry(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{}) // no tool_registry

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventUnsupportedToolCall)
	if !ok {
		t.Fatal("expected EventUnsupportedToolCall with nil registry, not found")
	}
	if e.ToolName != "create_issue" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "create_issue")
	}
}

func TestRunTurn_ToolCallWithRegisteredTool(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:   "create_issue",
		result: json.RawMessage(`{"id":"123"}`),
	})

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from registered tool, not found")
	}
	if e.ToolError {
		t.Error("EventToolResult.ToolError = true, want false")
	}
	if e.ToolName != "create_issue" {
		t.Errorf("EventToolResult.ToolName = %q, want %q", e.ToolName, "create_issue")
	}
}

func TestRunTurn_ToolCallWithToolError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:    "create_issue",
		execErr: errors.New("tracker unavailable"),
	})

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from failed tool, not found")
	}
	if !e.ToolError {
		t.Error("EventToolResult.ToolError = false, want true")
	}
}

func TestRunTurn_ToolCallToolNotInRegistry(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	// Registry has no "create_issue" tool.

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if _, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall for unregistered tool, not found")
	}
}

// --- Human-only-request recognition and refusal ---

// runTurnFixtureWithServerRequest builds a turn/start response, a
// turn/started notification, and one server request (a method carrying
// both "id" and "method") for the given method and request id, followed
// by the given trailing lines (e.g. a turn/completed notification, or
// none when the request is expected to end the attempt before any more
// input is read).
func runTurnFixtureWithServerRequest(requestID int64, method string, trailing ...string) []byte {
	fixture := fmt.Sprintf(
		"{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n"+
			"{\"method\":\"turn/started\",\"params\":{}}\n"+
			"{\"id\":%d,\"method\":%q,\"params\":{}}\n",
		requestID, method,
	)
	for _, line := range trailing {
		fixture += line + "\n"
	}
	return []byte(fixture)
}

const turnCompletedLine = `{"method":"turn/completed","params":{"turn":{"id":"turn-001","status":"completed"}}}`

// TestRunTurn_PermissionRequestDeniedContinues drives the two current-form
// permission requests through the turn loop and asserts the exact bytes
// written back to the app-server for that request id, and that the turn
// continues to turn/completed rather than ending.
func TestRunTurn_PermissionRequestDeniedContinues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
	}{
		{name: "item/commandExecution/requestApproval", method: "item/commandExecution/requestApproval", requestID: 20},
		{name: "item/fileChange/requestApproval", method: "item/fileChange/requestApproval", requestID: 21},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(runTurnFixtureWithServerRequest(tt.requestID, tt.method, turnCompletedLine), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("ExitReason = %q, want %q (turn must continue past the refusal)", result.ExitReason, domain.EventTurnCompleted)
			}

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			want := fmt.Sprintf(`{"id":%d,"result":{"decision":"decline"}}`, tt.requestID) + "\n"
			if write != want {
				t.Errorf("response for %s = %q, want %q", tt.method, write, want)
			}
		})
	}
}

// TestRunTurn_LegacyPermissionRequestDeniedContinues drives the two legacy
// ReviewDecision-form permission requests through the turn loop and asserts
// the exact denial payload the schema declares, and that the turn
// continues rather than ending.
func TestRunTurn_LegacyPermissionRequestDeniedContinues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
	}{
		{name: "applyPatchApproval", method: "applyPatchApproval", requestID: 22},
		{name: "execCommandApproval", method: "execCommandApproval", requestID: 23},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(runTurnFixtureWithServerRequest(tt.requestID, tt.method, turnCompletedLine), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("ExitReason = %q, want %q (turn must continue past the refusal)", result.ExitReason, domain.EventTurnCompleted)
			}

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			want := fmt.Sprintf(`{"id":%d,"result":{"decision":{"denied":{"rejection":%q}}}}`, tt.requestID, codexRefusalMessage) + "\n"
			if write != want {
				t.Errorf("response for %s = %q, want %q", tt.method, write, want)
			}
		})
	}
}

// TestRunTurn_PermissionRequestEmitsNotification asserts that a continued
// permission refusal emits RefusalPosture.Notice as an EventNotification,
// so the operator learns why the turn kept going even though nothing was
// granted.
func TestRunTurn_PermissionRequestEmitsNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(runTurnFixtureWithServerRequest(24, "item/commandExecution/requestApproval", turnCompletedLine))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending).NoticeWithDetail("")

	var found bool
	for _, e := range filterEventsOfType(events, domain.EventNotification) {
		if e.Message == wantNotice {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no EventNotification with Message = %q found", wantNotice)
	}
}

// TestRunTurn_HumanInputRequestEndsAttempt drives each ClassHumanInput
// method through the turn loop and asserts the exact bytes written back
// for that request id, and that the attempt ends with the
// human-input-required outcome rather than continuing or reading further
// input.
func TestRunTurn_HumanInputRequestEndsAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		method    string
		requestID int64
		detail    string
		wantWrite string
	}{
		{
			name:      "mcpServer/elicitation/request",
			method:    "mcpServer/elicitation/request",
			requestID: 30,
			detail:    detailAnswerToQuestion,
			wantWrite: `{"id":30,"result":{"action":"decline"}}` + "\n",
		},
		{
			name:      "item/permissions/requestApproval",
			method:    "item/permissions/requestApproval",
			requestID: 31,
			detail:    detailWiderAccess,
			wantWrite: `{"id":31,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n",
		},
		{
			name:      "item/tool/requestUserInput",
			method:    "item/tool/requestUserInput",
			requestID: 32,
			detail:    detailAnswerToQuestion,
			wantWrite: `{"id":32,"error":{"code":-32001,"message":"sortie refuses requests that only a person could answer"}}` + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := &capturingWriteCloser{}
			state := makeTestStateWithStdin(runTurnFixtureWithServerRequest(tt.requestID, tt.method), stdin)
			adapter, _ := NewCodexAdapter(map[string]any{})

			result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})

			dispositiontest.AssertDispositionContract(t, agentcore.HumanInputEvidence(tt.detail), result, err)

			write, ok := stdin.find(fmt.Sprintf(`{"id":%d,`, tt.requestID))
			if !ok {
				t.Fatalf("no response written for request id %d", tt.requestID)
			}
			if write != tt.wantWrite {
				t.Errorf("response for %s = %q, want %q", tt.method, write, tt.wantWrite)
			}
		})
	}
}

// TestRunTurn_HumanInputRequestDrainsInFlightToolCall pins the
// refusal path: with a tool call in flight when a ClassHumanInput
// request arrives, the terminal return must close and drain toolEventCh
// so the tool goroutine's send completes rather than leaking. If the
// drain were skipped, RunTurn would return before the delayed tool
// finishes and its EventToolResult would never be observed by this test,
// because OnEvent is only ever called while RunTurn itself is still
// running.
func TestRunTurn_HumanInputRequestDrainsInFlightToolCall(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "slow_tool", result: json.RawMessage(`"ok"`), delay: 50 * time.Millisecond})

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{}}\n" +
		"{\"id\":41,\"method\":\"item/tool/call\",\"params\":{\"tool\":\"slow_tool\",\"arguments\":{}}}\n" +
		"{\"id\":42,\"method\":\"mcpServer/elicitation/request\",\"params\":{}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	})

	dispositiontest.AssertDispositionContract(t, agentcore.HumanInputEvidence(detailAnswerToQuestion), result, err)

	if _, ok := firstEventOfType(events, domain.EventToolResult); !ok {
		t.Fatal("in-flight tool call's EventToolResult was not drained before the terminal return")
	}
}

// TestRunTurn_CancelledReturnsWithinBoundWhenTurnCompletedNeverArrives
// pins the post-cancellation bound: after the best-effort turn/interrupt,
// the loop must return once read_timeout_ms elapses rather than reading
// until stdout closes. The runtime's own turn/completed never arrives in
// this test.
func TestRunTurn_CancelledReturnsWithinBoundWhenTurnCompletedNeverArrives(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := newInterruptedStatusState()
	state.agentConfig = domain.AgentConfig{ReadTimeoutMS: 50}
	close(state.readerDone)
	stdin := &interruptTrackingWriteCloser{interrupts: make(chan struct{}, 1)}
	state.stdin = stdin

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})

	adapter, _ := NewCodexAdapter(map[string]any{})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "go",
			OnEvent: func(domain.AgentEvent) {},
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		close(state.msgCh)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	state.msgCh <- parseMessage([]byte(`{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`))
	state.msgCh <- parseMessage([]byte(`{"method":"turn/started","params":{"turnId":"turn-001"}}`))

	start := time.Now()
	cancel()

	select {
	case <-stdin.interrupts:
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not send turn/interrupt after cancellation")
	}

	// turn/completed is never sent. RunTurn must return once the
	// configured read_timeout_ms bound elapses, not merely eventually.
	select {
	case got := <-outcomeCh:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("RunTurn returned after %v, want well within the 50ms read_timeout_ms bound", elapsed)
		}
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn did not return within the bound; it kept reading after cancellation with no turn/completed")
	}
}

// TestRunTurn_CancelledDrainsInFlightToolCall asserts that when the
// post-cancellation bound elapses with a tool call in flight, the timeout
// return still closes and drains toolEventCh before calling FinalizeTurn.
func TestRunTurn_CancelledDrainsInFlightToolCall(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "slow_tool", result: json.RawMessage(`"ok"`), delay: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := newInterruptedStatusState()
	state.agentConfig = domain.AgentConfig{ReadTimeoutMS: 200}
	close(state.readerDone)

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan outcome, 1)
	finished := make(chan struct{})
	var events []domain.AgentEvent

	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	go func() {
		defer close(finished)
		result, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
			Prompt:  "use tool",
			OnEvent: collectEvents(&events),
		})
		outcomeCh <- outcome{result: result, err: err}
	}()

	t.Cleanup(func() {
		select {
		case <-finished:
			return
		default:
		}
		close(state.msgCh)
		select {
		case <-finished:
		case <-time.After(time.Second):
		}
	})

	state.msgCh <- parseMessage([]byte(`{"id":1,"result":{"turn":{"id":"turn-001","status":"starting"}}}`))
	state.msgCh <- parseMessage([]byte(`{"method":"turn/started","params":{"turnId":"turn-001"}}`))
	state.msgCh <- parseMessage([]byte(`{"id":60,"method":"item/tool/call","params":{"tool":"slow_tool","arguments":{}}}`))

	cancel()

	select {
	case got := <-outcomeCh:
		if got.result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
		}
		requireAgentError(t, got.err, domain.ErrTurnCancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn did not return within the bound with a tool call in flight")
	}

	if _, ok := firstEventOfType(events, domain.EventToolResult); !ok {
		t.Fatal("in-flight tool call's EventToolResult was not drained before the cancellation-timeout return")
	}
}

// --- handleToolCall direct tests ---

func TestHandleToolCall_InvalidParams(t *testing.T) {
	t.Parallel()

	a, _ := NewCodexAdapter(map[string]any{})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 42},
		Notification: rpcNotification{
			Method: "item/tool/call",
			Params: json.RawMessage(`not-valid-json`),
		},
	}

	// Should not panic; writes an error response to stdin (discarded).
	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	// Invalid params: no event emitted (returns early after sendResponse).
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestHandleToolCall_NilRegistryEmitsUnsupported(t *testing.T) {
	t.Parallel()

	a, _ := NewCodexAdapter(map[string]any{})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{}}`),
		},
	}

	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	if e, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall with nil registry")
	} else if e.ToolName != "my_tool" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "my_tool")
	}
}

func TestHandleToolCall_ToolNotFound(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"unknown_tool","arguments":{}}`),
		},
	}

	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	if _, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall for unregistered tool")
	}
}

func TestHandleToolCall_ToolSuccess(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "my_tool", result: json.RawMessage(`"ok"`)})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 7},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{"x":1}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult on success")
	}
	if e.ToolError {
		t.Error("EventToolResult.ToolError = true, want false")
	}
	if e.ToolName != "my_tool" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "my_tool")
	}
}

func TestHandleToolCall_ToolError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "my_tool", execErr: errors.New("service down")})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 7},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult on tool error")
	}
	if !e.ToolError {
		t.Error("EventToolResult.ToolError = false, want true")
	}
	if e.Message != "service down" {
		t.Errorf("Message = %q, want %q", e.Message, "service down")
	}
}

// --- StopSession ---

func TestStopSession_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: "wrong"})
	if err == nil {
		t.Fatal("StopSession() expected error for wrong internal type, got nil")
	}
}

func TestRunTurn_MultiTurnNoRace(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-001\"}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-001\",\"tokenUsage\":{\"last\":{\"totalTokens\":150,\"inputTokens\":100,\"cachedInputTokens\":10,\"outputTokens\":50,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":150,\"inputTokens\":100,\"cachedInputTokens\":10,\"outputTokens\":50,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"completed\"}}}\n" +
		"{\"id\":2,\"result\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-002\"}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-002\",\"tokenUsage\":{\"last\":{\"totalTokens\":300,\"inputTokens\":200,\"cachedInputTokens\":20,\"outputTokens\":100,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":300,\"inputTokens\":200,\"cachedInputTokens\":20,\"outputTokens\":100,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"completed\"}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(1) ExitReason = %v, want %v", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(2) error = %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(2) ExitReason = %v, want %v", result2.ExitReason, domain.EventTurnCompleted)
	}
}

func TestRunTurn_StdoutEOFBetweenTurns(t *testing.T) {
	t.Parallel()

	// One complete turn in the fixture. After the first RunTurn drains the
	// channel, the reader goroutine closes msgCh on EOF. A second RunTurn
	// call receives !ok immediately and returns ErrPortExit — the
	// "stdout EOF between turns" behavior introduced by the session-scoped
	// reader refactoring (previously undetected).
	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-001\"}}\n" +
		"{\"method\":\"thread/tokenUsage/updated\",\"params\":{\"threadId\":\"thread-001\",\"turnId\":\"turn-001\",\"tokenUsage\":{\"last\":{\"totalTokens\":15,\"inputTokens\":10,\"cachedInputTokens\":0,\"outputTokens\":5,\"reasoningOutputTokens\":0},\"total\":{\"totalTokens\":15,\"inputTokens\":10,\"cachedInputTokens\":0,\"outputTokens\":5,\"reasoningOutputTokens\":0}}}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"completed\"}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	}); err != nil {
		t.Fatalf("RunTurn(1) unexpected error: %v", err)
	}

	// After the first turn, the fixture is exhausted and msgCh is closed.
	// The second call must return ErrPortExit immediately.
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestStopSession_NilState(t *testing.T) {
	t.Parallel()

	// State with nil proc and nil waitCh — StopSession should return nil.
	state := &sessionState{
		stdin:  nopWriteCloser{},
		waitCh: nil,
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}
}

func TestStopSession_WithActiveReaderGoroutine(t *testing.T) {
	t.Parallel()

	// Provide more messages than the channel buffer (16) so the reader
	// goroutine is blocked on a channel send when StopSession closes stopCh.
	line := []byte("{\"method\":\"turn/started\",\"params\":{}}\n")
	state := makeTestState(bytes.Repeat(line, 20))
	// Simulate the subprocess having already exited so waitCh does not block.
	close(state.waitCh)

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	// StopSession waits on readerDone internally; it must be closed on return.
	select {
	case <-state.readerDone:
		// OK
	default:
		t.Error("readerDone should be closed after StopSession")
	}
}

// --- fakeTool for tool registry tests ---

type fakeTool struct {
	name    string
	result  json.RawMessage
	execErr error
	delay   time.Duration
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return "fake tool for testing" }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakeTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.result, f.execErr
}

// --- Race-detection tests ---

func TestHandleToolCall_EventsSerialized(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "slow_tool", result: json.RawMessage(`"ok"`), delay: time.Millisecond})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"slow_tool","arguments":{}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != domain.EventToolResult {
		t.Errorf("event type = %v, want EventToolResult", events[0].Type)
	}
}

func TestRunTurn_ToolCallEventSerialization(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:   "create_issue",
		result: json.RawMessage(`{"id":"42"}`),
		delay:  time.Millisecond,
	})
	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}
	if _, ok := firstEventOfType(events, domain.EventToolResult); !ok {
		t.Error("expected EventToolResult, not found")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage, not found")
	}
}
