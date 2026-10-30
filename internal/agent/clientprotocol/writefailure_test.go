package clientprotocol

import (
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

// alwaysFailWriter fails every Write with err.
type alwaysFailWriter struct{ err error }

func (w alwaysFailWriter) Write(p []byte) (int, error) { return 0, w.err }

// newFailWriteSession is newTestSession with its connection's writer replaced
// by one that fails every write, so a turn's prompt send fails asynchronously
// after handleStartTurn's SendRequest has enqueued and returned.
func newFailWriteSession(t *testing.T, agentConfig domain.AgentConfig, writeErr error) (*sessionState, *io.PipeWriter) {
	t.Helper()

	inPr, inPw := io.Pipe()
	state := &sessionState{
		agentConfig: agentConfig,
		caps:        newCapabilityRecord(),
		usage:       agentcore.NewTurnEndUsage(),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      discardLogger(),
		origins:     &sessionOrigins{},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(alwaysFailWriter{err: writeErr}, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(8<<20))

	go runPump(state)
	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		state.conn.Close()
		_ = inPr.Close()
	})

	return state, inPw
}

func TestPump_WriteFailureDuringTurn_StreamEndWinsWithinBound(t *testing.T) {
	t.Parallel()

	writeErr := errors.New("boom")
	state, inPw := newFailWriteSession(t, domain.AgentConfig{ReadTimeoutMS: 300}, writeErr)
	markSessionKnown(state)

	turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

	// Wait for the write to fail before ending the stream, so the turn is
	// genuinely active when the stream ends rather than racing acceptance.
	waittest.Receive(t, state.conn.WriteFailed(), "the prompt send's write to fail")

	_ = inPw.Close()

	outcome := waittest.Receive(t, turnCh, "turn outcome")
	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("runTurn() error = %v (%T), want *domain.AgentError", outcome.err, outcome.err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != streamEndedMessage {
		t.Errorf("AgentError.Message = %q, want %q (the stream-end path, not the send-failure message)", agentErr.Message, streamEndedMessage)
	}
}

func TestPump_WriteFailureDuringTurn_SendFailureWinsAfterBound(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const bound = 300 * time.Millisecond
		writeErr := errors.New("boom")
		state, _ := newFailWriteSession(t, domain.AgentConfig{ReadTimeoutMS: int(bound.Milliseconds())}, writeErr)
		markSessionKnown(state)

		start := time.Now()
		turnCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

		outcome := waittest.Receive(t, turnCh, "turn outcome")
		elapsed := time.Since(start)

		var agentErr *domain.AgentError
		if !errors.As(outcome.err, &agentErr) {
			t.Fatalf("runTurn() error = %v (%T), want *domain.AgentError", outcome.err, outcome.err)
		}
		if agentErr.Kind != domain.ErrPortExit {
			t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
		}
		if agentErr.Message != promptSendFailedMessage {
			t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, promptSendFailedMessage)
		}
		if elapsed < bound {
			t.Errorf("runTurn() ended after %v, want it to wait the full %v bound rather than finalizing at once", elapsed, bound)
		}
	})
}

func TestPumpState_WriteFailureWithNoActiveTurn(t *testing.T) {
	t.Parallel()

	p := &pumpState{}

	if ch := p.armWriteFailedDeadline(); ch != nil {
		t.Errorf("armWriteFailedDeadline() = %v, want nil when no turn is active", ch)
	}

	p.handleWriteFailed()

	if p.activeTurn != nil {
		t.Error("activeTurn became non-nil from handleWriteFailed with none active, want it to stay nil")
	}
}
