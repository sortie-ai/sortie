package clientprotocol

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestRunTurnFinalize carries the stop-reason cases and the
// concurrent-RunTurn rejection case in one test.
func TestRunTurnFinalize(t *testing.T) {
	t.Parallel()

	t.Run("stop reasons", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			reason stopReason
		}{
			{"refusal maps to the non-retryable refused kind rather than the retryable default", stopReasonRefusal},
			{"a stop reason outside the pinned set settles as a non-retried error carrying the value received", stopReason("some_future_reason")},
			{"cancelled with no cancellation on either side reports failed", stopReasonCancelled},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
				out := newOutboundReader(outPr)
				markSessionKnown(state)

				var events []domain.AgentEvent
				outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

				promptID := out.awaitMethod(t, methodSessionPrompt)
				respondLine(t, inPw, promptID, promptResponse{StopReason: tt.reason})

				outcome := awaitOutcome(t, outcomeCh)
				want := stopReasonEvidence(tt.reason, agentcore.WorkAbsent)
				dispositiontest.AssertDispositionContract(t, want, outcome.result, outcome.err)
			})
		}
	})

	t.Run("cancelled stop reason after an orchestrator cancellation reports cancelled", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		markSessionKnown(state)

		ctx, cancel := context.WithCancel(context.Background())
		var events []domain.AgentEvent
		outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

		promptID := out.awaitMethod(t, methodSessionPrompt)
		cancel()
		out.awaitMethod(t, methodSessionCancel)

		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})

		outcome := awaitOutcome(t, outcomeCh)
		dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
			Terminal: agentcore.TerminalCancelled,
			Work:     agentcore.WorkAbsent,
		}, outcome.result, outcome.err)
	})

	t.Run("a second concurrent RunTurn is rejected rather than opening a second prompt request", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		markSessionKnown(state)

		var firstEvents []domain.AgentEvent
		firstOutcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "first", OnEvent: collectEvents(&firstEvents)})
		promptID := out.awaitMethod(t, methodSessionPrompt)

		var secondEvents []domain.AgentEvent
		_, err := runTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
			Prompt:  "second",
			OnEvent: collectEvents(&secondEvents),
		})
		if err == nil {
			t.Fatal("second RunTurn() error = nil, want a rejection")
		}
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) {
			t.Fatalf("second RunTurn() error = %v, want *domain.AgentError", err)
		}
		if agentErr.Kind != domain.ErrResponseError {
			t.Errorf("second RunTurn() error kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
		}
		if len(secondEvents) != 0 {
			t.Errorf("second RunTurn() delivered %d events, want 0: a rejected turn opens no prompt request", len(secondEvents))
		}

		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
		firstOutcome := awaitOutcome(t, firstOutcomeCh)
		if firstOutcome.err != nil {
			t.Fatalf("first RunTurn() error = %v, want nil: the rejected concurrent call must not disturb it", firstOutcome.err)
		}
	})
}

func TestRunTurnPreTurnWaits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fillQueue  bool
		cancel     bool
		wantErr    error
		wantKind   domain.AgentErrorKind
		wantQueued bool
	}{
		{
			name:      "blocked publication times out",
			fillQueue: true,
			wantKind:  domain.ErrResponseTimeout,
		},
		{
			name:      "blocked publication observes context cancellation",
			fillQueue: true,
			cancel:    true,
			wantErr:   context.Canceled,
		},
		{
			name:       "missing pump verdict times out",
			wantKind:   domain.ErrResponseTimeout,
			wantQueued: true,
		},
		{
			name:       "missing pump verdict observes context cancellation",
			cancel:     true,
			wantErr:    context.Canceled,
			wantQueued: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := &sessionState{
				agentConfig: domain.AgentConfig{ReadTimeoutMS: 50},
				itemCh:      make(chan pumpItem, 1),
				caps:        newCapabilityRecord(false),
			}
			if tt.fillQueue {
				state.itemCh <- pumpItem{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if tt.cancel {
				if tt.wantQueued {
					// Wait for the publication instead of hoping a fixed
					// window covers it: drain the item runTurn published
					// and restore it, so the cancellation below lands on
					// the verdict wait rather than racing the send. The
					// assertion further down drains it again.
					state.itemCh <- <-state.itemCh
				}
				// With the queue full the send can never complete, so the
				// cancellation is observed whenever it arrives.
				cancel()
			}

			outcome := awaitOutcome(t, outcomeCh)

			if tt.wantErr != nil {
				if !errors.Is(outcome.err, tt.wantErr) {
					t.Fatalf("runTurn() error = %v, want errors.Is(%v)", outcome.err, tt.wantErr)
				}
			} else {
				var agentErr *domain.AgentError
				if !errors.As(outcome.err, &agentErr) {
					t.Fatalf("runTurn() error = %v, want *domain.AgentError", outcome.err)
				}
				if agentErr.Kind != tt.wantKind {
					t.Errorf("runTurn() AgentError.Kind = %q, want %q", agentErr.Kind, tt.wantKind)
				}
			}
			if tt.wantQueued {
				select {
				case item := <-state.itemCh:
					if item.control == nil || item.control.startTurn == nil {
						t.Fatalf("runTurn() queued item = %+v, want startTurn control", item)
					}
				case <-time.After(awaitTimeout):
					t.Fatal("runTurn() queued no startTurn control, want publication before verdict wait")
				}
			}
		})
	}
}

func TestDelayedTurnVerdictCannotBlockPump(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 20},
		itemCh:      make(chan pumpItem, 1),
		logger:      discardLogger(),
		caps:        newCapabilityRecord(false),
	}

	outcome := awaitOutcome(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}))
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) || agentErr.Kind != domain.ErrResponseTimeout {
		t.Fatalf("runTurn() error = %v, want *domain.AgentError kind %q", outcome.err, domain.ErrResponseTimeout)
	}
	item := <-state.itemCh
	pump := &pumpState{
		state: state,
		activeTurn: &activeTurn{
			done: make(chan struct{}),
		},
	}
	delivered := make(chan struct{})
	go func() {
		pump.handleStartTurn(item.control.startTurn)
		close(delivered)
	}()

	select {
	case <-delivered:
	case <-time.After(awaitTimeout):
		t.Fatal("handleStartTurn() blocked delivering a delayed verdict, want capacity-one reply delivery")
	}
}

// TestAbandonedTurnIsNotStarted pins that a turn whose caller stopped
// waiting is never started. runTurn returns on its own bound while the
// startTurn control message is still queued, so the pump can reach that
// message with nothing left to read the turn: starting it would prompt
// the agent for work no one collects, and would leave activeTurn set so
// that every later turn is rejected as already in flight.
func TestAbandonedTurnIsNotStarted(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 20},
		itemCh:      make(chan pumpItem, 1),
		logger:      discardLogger(),
		caps:        newCapabilityRecord(false),
	}

	outcome := awaitOutcome(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}))
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) || agentErr.Kind != domain.ErrResponseTimeout {
		t.Fatalf("runTurn() error = %v, want *domain.AgentError kind %q", outcome.err, domain.ErrResponseTimeout)
	}

	item := <-state.itemCh
	// state.conn is nil here, so a pump that starts the turn anyway
	// reaches the prompt send and panics rather than failing quietly.
	pump := &pumpState{state: state}
	pump.handleStartTurn(item.control.startTurn)

	if pump.activeTurn != nil {
		t.Error("handleStartTurn() set activeTurn for a turn whose caller stopped waiting, want the turn left unstarted")
	}
}

// TestNoCounterSessionReportsUnmeasured confirms a session with no
// counters emits no token_usage event and reports the run unmeasured.
// This kind's normalization table never emits domain.EventTokenUsage,
// so a usage_update observed mid-turn must still leave the turn's
// measurement contract empty.
func TestNoCounterSessionReportsUnmeasured(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"usage_update","used":12000,"size":200000}}}`)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	agenttest.AssertUsageContract(t, events)
	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
}

// TestAssertUsageReporting proves agent-client-protocol's registered
// usage-reporting declaration (none, none) against its own real event
// stream: a usage_update mid-turn is a debug-log-only normalization
// arm, so the turn's measurement contract stays empty.
func TestAssertUsageReporting(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"usage_update","used":12000,"size":200000}}}`)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	agenttest.AssertUsageReporting(t, "agent-client-protocol", []agenttest.UsageReportingCase{
		{Name: "usage_update observed mid-turn reports no measurement", Events: events, Result: outcome.result},
	})
}

// indexOfStepName returns the index of name in names, failing t if it
// is not present.
func indexOfStepName(t *testing.T, names []string, name string) int {
	t.Helper()
	for i, n := range names {
		if n == name {
			return i
		}
	}
	t.Fatalf("defaultTeardownOrder() step names = %v, want %q present", names, name)
	return -1
}

// TestDefaultTeardownOrderIncludesCloseSession confirms the fixed step
// order places close_session immediately after answer_open and before
// both signal_graceful and kill_process_group, asserted on the
// returned step names rather than on timing: a reversal of the order
// fails this assertion structurally.
func TestDefaultTeardownOrderIncludesCloseSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	steps := defaultTeardownOrder(ctx, ctx, time.Second)

	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = step.name
	}

	answerOpenIdx := indexOfStepName(t, names, "answer_open")
	closeSessionIdx := indexOfStepName(t, names, "close_session")
	signalGracefulIdx := indexOfStepName(t, names, "signal_graceful")
	killIdx := indexOfStepName(t, names, "kill_process_group")

	if closeSessionIdx != answerOpenIdx+1 {
		t.Errorf("defaultTeardownOrder() step names = %v, want close_session immediately after answer_open", names)
	}
	if closeSessionIdx >= signalGracefulIdx {
		t.Errorf("defaultTeardownOrder() step names = %v, want close_session before signal_graceful", names)
	}
	if closeSessionIdx >= killIdx {
		t.Errorf("defaultTeardownOrder() step names = %v, want close_session before kill_process_group", names)
	}
}

// TestCloseSessionSkipsWhenIdentifierEmpty confirms the step returns
// immediately, writing nothing and logging nothing, when
// closeSessionID is left at its zero value: the handshake never
// advertised session/close, so there is nothing to close.
func TestCloseSessionSkipsWhenIdentifierEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, _ := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)

	closeSession(context.Background(), context.Background(), time.Second)(state)

	select {
	case line := <-out.ch:
		t.Errorf("closeSession() wrote %s, want no bytes written when closeSessionID is empty", line)
	case <-time.After(100 * time.Millisecond):
	}

	if buf.Len() != 0 {
		t.Errorf("closeSession() logged %q, want no record when closeSessionID is empty", buf.String())
	}
}

// TestCloseSessionBoundedWhenAgentNeverAnswers confirms an agent that
// reads the session/close request and never answers it cannot hold
// the step past closeCallBound(grace): grace is configured well below
// the default so the property costs no default-length wait.
func TestCloseSessionBoundedWhenAgentNeverAnswers(t *testing.T) {
	t.Parallel()

	state, outPr, _ := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.closeSessionID = "sess-test"

	const grace = 200 * time.Millisecond
	bound := closeCallBound(grace)

	start := time.Now()
	closeSession(context.Background(), context.Background(), grace)(state)
	elapsed := time.Since(start)

	out.awaitMethod(t, methodSessionClose)

	const schedulingOverhead = time.Second
	if wantBound := bound + schedulingOverhead; elapsed > wantBound {
		t.Errorf("closeSession() step took %v, want at most %v (closeCallBound(%v) plus scheduling overhead)", elapsed, wantBound, grace)
	}
}

// TestDoInitializeVersionPin confirms the version pin: a response
// carrying a version other than 1 ends the session. This exercises
// the enforcement point startSession relies on to tear the session
// down on a mismatched handshake.
func TestDoInitializeVersionPin(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	type initOutcome struct {
		err *domain.AgentError
	}
	ch := make(chan initOutcome, 1)
	go func() {
		_, err := doInitialize(context.Background(), state)
		ch <- initOutcome{err: err}
	}()

	id := out.awaitMethod(t, methodInitialize)
	respondLine(t, inPw, id, initializeResponse{ProtocolVersion: protocolVersion(2)})

	select {
	case outcome := <-ch:
		if outcome.err == nil {
			t.Fatal("doInitialize() error = nil, want an error for an unpinned protocol version")
		}
		if outcome.err.Kind != domain.ErrResponseError {
			t.Errorf("doInitialize() error kind = %q, want %q", outcome.err.Kind, domain.ErrResponseError)
		}
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for doInitialize to return")
	}
}
