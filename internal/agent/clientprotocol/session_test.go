package clientprotocol

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func TestStartSessionWrongTypedMCPConfigFailsBeforeLaunch(t *testing.T) {
	t.Parallel()

	settings := map[string]any{mcpConfigKey: 123}
	adapter, _ := NewClientProtocolAdapter()

	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-never-launched"},
		Settings:      settings,
	})

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok || agentErr.Kind != domain.ErrAgentNotFound {
		t.Fatalf("StartSession(mcp_config=123) error = %v, want *domain.AgentError of kind %q", err, domain.ErrAgentNotFound)
	}
	fault, ok := errors.AsType[*typeutil.TypeFault](err)
	if !ok || fault.Key != mcpConfigKey {
		t.Fatalf("StartSession(mcp_config=123) error = %v, want a wrapped *typeutil.TypeFault on %q", err, mcpConfigKey)
	}
	diags := validateConfig(registry.AgentConfigFields{Kind: "agent-client-protocol", Passthrough: settings})
	if len(diags) != 1 || diags[0].Message != agentErr.Message {
		t.Errorf("validateConfig(mcp_config=123) = %+v, want one diagnostic carrying the text StartSession refused with: %q", diags, agentErr.Message)
	}
}

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

				outcome := waittest.Receive(t, outcomeCh, "turn outcome")
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

		outcome := waittest.Receive(t, outcomeCh, "turn outcome")
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
		firstOutcome := waittest.Receive(t, firstOutcomeCh, "turn outcome")
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
				caps:        newCapabilityRecord(),
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

			outcome := waittest.Receive(t, outcomeCh, "turn outcome")

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

			if item := takeQueuedItem(t, state); item.control == nil || item.control.startTurn == nil {
				t.Fatalf("runTurn() queued item = %+v, want a startTurn control", item)
			}
		})
	}
}

func takeQueuedItem(t *testing.T, state *sessionState) pumpItem {
	t.Helper()
	waittest.Receive(t, state.inbox.Ready(), "the startTurn control in the session inbox")
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
		caps:        newCapabilityRecord(),
	}

	outcome := waittest.Receive(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}), "turn outcome")
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

	waittest.Receive(t, delivered, "handleStartTurn() to deliver a delayed verdict on a capacity-one reply")
}

func TestAbandonedTurnIsNotStarted(t *testing.T) {
	t.Parallel()

	state := &sessionState{
		agentConfig: domain.AgentConfig{ReadTimeoutMS: 20},
		inbox:       jsonrpc.NewInbox[pumpItem](),
		logger:      discardLogger(),
		caps:        newCapabilityRecord(),
	}

	outcome := waittest.Receive(t, runTurnAsyncCtx(context.Background(), state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	}), "turn outcome")
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

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	agenttest.AssertUsageContract(t, events)
	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
}

func TestAssertUsageReporting(t *testing.T) {
	t.Parallel()

	remoteEvents, remoteOutcome := remoteLedgerSessionTurn(t)

	agenttest.AssertUsageReporting(t, "agent-client-protocol", []agenttest.UsageReportingCase{
		{
			Name:   "a remote session settles one figure per turn from the prompt result",
			Events: remoteEvents,
			Result: remoteOutcome.result,
		},
	})
}

func remoteLedgerSessionTurn(t *testing.T) ([]domain.AgentEvent, turnOutcome) {
	t.Helper()

	live := spendReading(1100, 70, 0, "", "")
	declaration := scriptedSpendDeclaration(&live, nil, nil)
	ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
	if err != nil {
		t.Fatalf("usage.NewLedger() error = %v, want nil", err)
	}
	ledger.Boundary(usage.RecordsRead{})
	state, out, inPw := newDeclaredSession(t, declaration, ledger)

	return measuredTurn(t, state, inPw, out, nil)
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

func TestCloseSessionSkipsWhenIdentifierEmpty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, _ := newTestSessionWithLogger(t, domain.AgentConfig{}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)

	closeSession(context.Background(), context.Background(), time.Second)(state)

	waittest.Never(t, out.ch, 100*time.Millisecond, "a line written by closeSession() when closeSessionID is empty")

	if buf.Len() != 0 {
		t.Errorf("closeSession() logged %q, want no record when closeSessionID is empty", buf.String())
	}
}

func TestCloseSessionBoundedWhenAgentNeverAnswers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		state, outPr, _ := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		state.closeSessionID = "sess-test"

		const grace = 200 * time.Millisecond
		bound := closeCallBound(grace)

		start := time.Now()
		closeSession(context.Background(), context.Background(), grace)(state)
		elapsed := time.Since(start)

		out.awaitMethod(t, methodSessionClose)

		if elapsed > bound {
			t.Errorf("closeSession() step took %v, want at most %v (closeCallBound(%v))", elapsed, bound, grace)
		}
	})
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

	outcome := waittest.Receive(t, ch, "doInitialize to return")
	if outcome.err == nil {
		t.Fatal("doInitialize() error = nil, want an error for an unpinned protocol version")
	}
	if outcome.err.Kind != domain.ErrResponseError {
		t.Errorf("doInitialize() error kind = %q, want %q", outcome.err.Kind, domain.ErrResponseError)
	}
}
