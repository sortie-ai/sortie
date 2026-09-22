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
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

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
				want := stopReasonEvidence(tt.reason)
				dispositiontest.AssertDispositionContract(t, want, outcome.result, outcome.err)
				agenttest.AssertSessionIDContract(t, []string{fakeSession(state).ID}, events, outcome.result)
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
		name     string
		cancel   bool
		wantErr  error
		wantKind domain.AgentErrorKind
	}{
		{
			name:     "missing pump verdict times out",
			wantKind: domain.ErrResponseTimeout,
		},
		{
			name:    "missing pump verdict observes context cancellation",
			cancel:  true,
			wantErr: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := &sessionState{
				agentConfig: domain.AgentConfig{ReadTimeoutMS: 50},
				inbox:       jsonrpc.NewInbox[pumpItem](),
				caps:        newCapabilityRecord(false, false),
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{
				Prompt:  "go",
				OnEvent: func(domain.AgentEvent) {},
			})
			if tt.cancel {
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

			select {
			case <-state.inbox.Ready():
				item, ok := state.inbox.Take()
				if !ok || item.control == nil || item.control.startTurn == nil {
					t.Fatalf("runTurn() queued item = %+v, ok=%v, want a startTurn control", item, ok)
				}
			case <-time.After(awaitTimeout):
				t.Fatal("runTurn() queued no startTurn control, want it put before the verdict wait")
			}
		})
	}
}

func takeQueuedItem(t *testing.T, state *sessionState) pumpItem {
	t.Helper()
	select {
	case <-state.inbox.Ready():
	case <-time.After(awaitTimeout):
		t.Fatal("nothing was queued in the session inbox within awaitTimeout, want the startTurn control")
	}
	item, ok := state.inbox.Take()
	if !ok {
		t.Fatal("Take() ok = false, want the queued startTurn control")
	}
	return item
}

func TestDelayedTurnVerdictCannotBlockPump(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 20},
		inbox:       jsonrpc.NewInbox[pumpItem](),
		logger:      discardLogger(),
		caps:        newCapabilityRecord(false, false),
	}

	outcome := awaitOutcome(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}))
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) || agentErr.Kind != domain.ErrResponseTimeout {
		t.Fatalf("runTurn() error = %v, want *domain.AgentError kind %q", outcome.err, domain.ErrResponseTimeout)
	}
	item := takeQueuedItem(t, state)
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

func TestAbandonedTurnIsNotStarted(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 20},
		inbox:       jsonrpc.NewInbox[pumpItem](),
		logger:      discardLogger(),
		caps:        newCapabilityRecord(false, false),
	}

	outcome := awaitOutcome(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}))
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) || agentErr.Kind != domain.ErrResponseTimeout {
		t.Fatalf("runTurn() error = %v, want *domain.AgentError kind %q", outcome.err, domain.ErrResponseTimeout)
	}

	item := takeQueuedItem(t, state)
	// state.conn is nil here, so a pump that starts the turn anyway
	// reaches the prompt send and panics rather than failing quietly.
	pump := &pumpState{state: state}
	pump.handleStartTurn(item.control.startTurn)

	if pump.activeTurn != nil {
		t.Error("handleStartTurn() set activeTurn for a turn whose caller stopped waiting, want the turn left unstarted")
	}
}

// This kind's normalization table never emits domain.EventTokenUsage, so a
// usage_update observed mid-turn must still leave the measurement contract empty.
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

func TestAssertUsageReporting(t *testing.T) {
	t.Parallel()

	measuredEvents, measuredOutcome := measuredSessionTurn(t)
	unmeasuredEvents, unmeasuredOutcome := unmeasuredSessionTurn(t)

	agenttest.AssertUsageReporting(t, "agent-client-protocol", []agenttest.UsageReportingCase{
		{
			Name:   "a local session whose source proved out settles one figure per turn",
			Events: measuredEvents,
			Result: measuredOutcome.result,
		},
		{
			Name:   "a session with no measurement source reports no figure",
			Remote: true,
			Events: unmeasuredEvents,
			Result: unmeasuredOutcome.result,
		},
	})
}

func measuredSessionTurn(t *testing.T) ([]domain.AgentEvent, turnOutcome) {
	t.Helper()

	reader := &fakeUsageReader{
		recognizes: true,
		drains: []fakeDrain{{
			found: true,
			usage: agentcore.RecoveredUsage{
				Run:   domain.TokenUsage{InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280, CacheReadTokens: 900},
				Model: "model-of-record",
			},
		}},
	}
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes,
		discardLogger(), withUsageReader(reader))
	out := newOutboundReader(outPr)
	publishHandshake(state, "0.59.0")
	markSessionKnown(state)

	return measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))
}

func unmeasuredSessionTurn(t *testing.T) ([]domain.AgentEvent, turnOutcome) {
	t.Helper()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	return measuredTurn(t, state, inPw, out, quotaMeta(1100, 70))
}

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

// A zero-value closeSessionID means the handshake never advertised
// session/close, so the step writes and logs nothing.
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
