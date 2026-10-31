package agentcore

import (
	"context"
	"time"

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
	provenance   []domain.UsageProvenance
	amounts      []domain.ReportedAmount
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
// returns the figure it produced, or nil when the settlement has none. It keeps
// the settlement's amounts whether or not a figure came with them.
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
	if settlement.Estimated {
		s.MarkEstimated()
	}
	s.amounts = settlement.Amounts
	if !settlement.Figure {
		return nil
	}
	s.provenance = settlement.Provenance
	return &RecoveredUsage{Run: settlement.Run, ByModel: settlement.ByModel, Model: settlement.Model}
}

// SettleSpend settles one turn through the ledger and returns the figure it
// produced, or nil when it has none. It calls read, bounded by timeout, only
// when the ledger needs the records. ledger must be non-nil, read reports
// every failure as OK false, and the call is not safe for concurrent use with
// other calls on the same ledger.
func SettleSpend(ctx context.Context, ledger *usage.Ledger, end usage.TurnEnd, live *usage.SpendReading,
	read func(context.Context) usage.RecordsRead, timeout time.Duration, spend *TurnSpend) *RecoveredUsage {
	records := usage.RecordsRead{}
	if ledger.NeedsRecords(end, live) {
		readCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		records = read(readCtx)
	}
	return spend.ApplySettlement(ledger.Settle(end, live, records))
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
	result.UsageProvenance = s.provenance
	result.ReportedAmounts = s.amounts
}
