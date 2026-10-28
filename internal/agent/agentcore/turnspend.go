package agentcore

import (
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TurnSpend is one turn's evidence for the shared spend rule. The zero
// value is a turn that has observed nothing. Whoever starts a turn owns
// its TurnSpend and never resets it; the mark methods are idempotent and
// order-independent. It is not safe for concurrent use.
type TurnSpend struct {
	modelReached bool
	settled      bool
	sessionTotal bool
	estimated    bool
}

// MarkModelReached records that the runtime served this turn output: a
// message or delta, a reasoning item, a tool call, a sub-agent task
// event, or a usage figure of this turn. A request-start announcement,
// the user's own or a replayed message, and session setup are not output.
func (s *TurnSpend) MarkModelReached() {
	s.modelReached = true
}

// MarkSettled records that the adapter holds a figure the runtime
// reported as covering the whole turn.
func (s *TurnSpend) MarkSettled() {
	s.settled = true
}

// MarkSessionTotal records that the settled figure is the runtime's
// running session total, holding what every earlier completed turn of
// the session spent.
func (s *TurnSpend) MarkSessionTotal() {
	s.sessionTotal = true
}

// MarkEstimated records that the turn's figure rests on an estimated
// resume baseline.
func (s *TurnSpend) MarkEstimated() {
	s.estimated = true
}

// ApplySettlement marks the turn's evidence from a ledger settlement and
// returns the figure it produced, or nil when the settlement has none.
func (s *TurnSpend) ApplySettlement(settlement usage.Settlement) *RecoveredUsage {
	if settlement.Spent {
		s.MarkModelReached()
	}
	if settlement.Complete {
		s.MarkSettled()
	}
	if settlement.CoversSession {
		s.MarkSessionTotal()
	}
	if !settlement.Figure {
		return nil
	}
	return &RecoveredUsage{Run: settlement.Run, ByModel: settlement.ByModel, Model: settlement.Model}
}

// ModelReached reports whether the runtime served this turn output.
func (s TurnSpend) ModelReached() bool {
	return s.modelReached
}

// applyTo writes the turn's spend fields onto result and is the only
// place that does. completed is true only for a turn the runtime ended
// as completed.
func (s TurnSpend) applyTo(result *domain.TurnResult, completed bool) {
	unsettled := s.modelReached && !s.settled
	result.SpendUnaccounted = s.estimated || unsettled
	result.SpendCoverable = completed && unsettled && !s.estimated
	result.CoversSession = s.settled && s.sessionTotal && !s.estimated
}
