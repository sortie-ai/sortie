package dispositiontest

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestAssertDispositionContract_Passing exercises the exported
// AssertDispositionContract against a real *testing.T for representative
// (evidence, result, err) triples that agree with what
// [agentcore.DecideTurn] assigns to the same evidence. None must report
// a failure.
func TestAssertDispositionContract_Passing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ev     agentcore.TurnEvidence
		result domain.TurnResult
		err    error
	}{
		{
			name:   "terminal cancelled with matching kind and message",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
			result: domain.TurnResult{ExitReason: domain.EventTurnCancelled},
			err:    &domain.AgentError{Kind: domain.ErrTurnCancelled, Message: "context cancelled"},
		},
		{
			name:   "terminal success carries no error",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess, TerminalMessage: "All done."},
			result: domain.TurnResult{ExitReason: domain.EventTurnCompleted},
		},
		{
			name:   "work present completes with no error",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalAbsent, ExitObserved: true, Work: agentcore.WorkPresent},
			result: domain.TurnResult{ExitReason: domain.EventTurnCompleted},
		},
		{
			name:   "zero work carries the matching AgentError",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalAbsent, ExitObserved: true, Work: agentcore.WorkAbsent},
			result: domain.TurnResult{ExitReason: domain.EventTurnFailed},
			err:    &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "agent exited without producing output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			AssertDispositionContract(t, tt.ev, tt.result, tt.err)
		})
	}
}

// TestAssertDispositionContract_Violating drives the exported
// AssertDispositionContract through a bare *testing.T stand-in for each
// failure arm the assertion defines. A value obtained from new(testing.T)
// rather than from t.Run has no parent, so its own Fail() never reaches
// this test's failure state, letting a deliberately-violating input be
// driven through the real, exported entry point without reddening this
// test. Each case must leave the stand-in Failed().
func TestAssertDispositionContract_Violating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		ev     agentcore.TurnEvidence
		result domain.TurnResult
		err    error
	}{
		{
			name:   "ExitReason does not match",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess, TerminalMessage: "All done."},
			result: domain.TurnResult{ExitReason: domain.EventTurnFailed},
		},
		{
			name:   "an error is wanted but err is nil",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
			result: domain.TurnResult{ExitReason: domain.EventTurnCancelled},
		},
		{
			name:   "no error is wanted but err is non-nil",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess, TerminalMessage: "All done."},
			result: domain.TurnResult{ExitReason: domain.EventTurnCompleted},
			err:    &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "unexpected"},
		},
		{
			name:   "err does not carry a *domain.AgentError",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
			result: domain.TurnResult{ExitReason: domain.EventTurnCancelled},
			err:    errors.New("plain error"),
		},
		{
			name:   "AgentError.Kind does not match",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
			result: domain.TurnResult{ExitReason: domain.EventTurnCancelled},
			err:    &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "context cancelled"},
		},
		{
			name:   "AgentError.Message does not match",
			ev:     agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
			result: domain.TurnResult{ExitReason: domain.EventTurnCancelled},
			err:    &domain.AgentError{Kind: domain.ErrTurnCancelled, Message: "a different message"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stand := new(testing.T)
			AssertDispositionContract(stand, tt.ev, tt.result, tt.err)

			if !stand.Failed() {
				t.Errorf("AssertDispositionContract(%s) did not fail, want a failure", tt.name)
			}
		})
	}
}

// TestAssertDispositionContract_TypedNilAgentErrorPanics pins the
// documented hazard AssertDispositionContract's own doc comment
// describes: a caller handing back a typed-nil *domain.AgentError as a
// non-nil error interface. err != nil then reports true, so the
// assertion proceeds to read a field through the nil pointer
// errors.As extracts, which panics rather than silently passing the
// case. A stand-in cannot make this assertion "fail" the ordinary way,
// since nothing here reaches its own t.Errorf call; recover() confirms
// the panic in place of a Failed() check.
func TestAssertDispositionContract_TypedNilAgentErrorPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("AssertDispositionContract(typed-nil *domain.AgentError) did not panic, want panic")
		}
	}()

	var typedNil *domain.AgentError
	var err error = typedNil

	stand := new(testing.T)
	AssertDispositionContract(stand, agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "context cancelled"},
		domain.TurnResult{ExitReason: domain.EventTurnCancelled}, err)
}

// TestAssertWorkEvidenceConsistent_Passing exercises the exported
// AssertWorkEvidenceConsistent against a real *testing.T for every way
// the check is a no-op or agrees with a work-present disposition: no
// tool result observed, a tool result observed but the turn did not
// fail, a tool result observed with a failure err does not carry as a
// *domain.AgentError, and a tool result observed alongside an unrelated
// failure message. None must report a failure.
func TestAssertWorkEvidenceConsistent_Passing(t *testing.T) {
	t.Parallel()

	toolResultEvents := []domain.AgentEvent{{Type: domain.EventToolResult}}

	tests := []struct {
		name   string
		events []domain.AgentEvent
		result domain.TurnResult
		err    error
	}{
		{
			name:   "no tool result observed",
			events: []domain.AgentEvent{{Type: domain.EventNotification}},
			result: domain.TurnResult{ExitReason: domain.EventTurnFailed},
			err:    &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "agent exited without producing output"},
		},
		{
			name:   "tool result observed but the turn did not fail",
			events: toolResultEvents,
			result: domain.TurnResult{ExitReason: domain.EventTurnCompleted},
		},
		{
			name:   "tool result observed and turn failed but err is not a *domain.AgentError",
			events: toolResultEvents,
			result: domain.TurnResult{ExitReason: domain.EventTurnFailed},
			err:    errors.New("plain error"),
		},
		{
			name:   "tool result observed and turn failed with an unrelated message",
			events: toolResultEvents,
			result: domain.TurnResult{ExitReason: domain.EventTurnFailed},
			err:    &domain.AgentError{Kind: domain.ErrResponseError, Message: "invalid api key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			AssertWorkEvidenceConsistent(t, tt.events, tt.result, tt.err)
		})
	}
}

// TestAssertWorkEvidenceConsistent_Violating drives the exported
// AssertWorkEvidenceConsistent through a bare *testing.T stand-in for a
// turn that observed a tool result and still reported one of the two
// no-work failures: the zero-work message exactly, the zero-work message
// with a WorkDetail suffix (the strings.HasPrefix arm), and the
// work-unobservable message exactly. Each case must leave the stand-in
// Failed().
func TestAssertWorkEvidenceConsistent_Violating(t *testing.T) {
	t.Parallel()

	toolResultEvents := []domain.AgentEvent{{Type: domain.EventToolResult}}

	zeroWork := agentcore.DecideTurn(agentcore.TurnEvidence{ExitObserved: true, Work: agentcore.WorkAbsent})
	unobservable := agentcore.DecideTurn(agentcore.TurnEvidence{ExitObserved: true, Work: agentcore.WorkUnobservable})

	tests := []struct {
		name    string
		message string
	}{
		{name: "zero-work message exactly", message: zeroWork.ErrorMessage},
		{name: "zero-work message with a WorkDetail suffix", message: zeroWork.ErrorMessage + ": no message from the agent and no tool call"},
		{name: "work-unobservable message exactly", message: unobservable.ErrorMessage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stand := new(testing.T)
			AssertWorkEvidenceConsistent(stand, toolResultEvents, domain.TurnResult{ExitReason: domain.EventTurnFailed},
				&domain.AgentError{Kind: domain.ErrTurnFailed, Message: tt.message})

			if !stand.Failed() {
				t.Errorf("AssertWorkEvidenceConsistent(%s) did not fail, want a failure", tt.name)
			}
		})
	}
}
