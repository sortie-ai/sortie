package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/storage"
)

// recordedOf maps the cost aggregates of an issue's usage read onto the
// input of [pricing.IssueCost].
func recordedOf(usage storage.IssueTokenUsage) pricing.RecordedCost {
	return pricing.RecordedCost{
		USD:      usage.RecordedCostUSD,
		Priced:   usage.PricedSessions,
		Partial:  usage.PartialSessions,
		Unpriced: usage.UnpricedSessions,
	}
}

// sessionCost prices the running session's spend so far under rates.
func (e *RunningEntry) sessionCost(rates *pricing.Rates) pricing.SessionCost {
	spend := spendOfRunningEntry(e)
	usd, status := pricing.PriceSession(spend, rates)
	return pricing.SessionCost{USD: usd, Status: status, Spent: hasUsage(spend.Totals)}
}

// sessionEstimateUSD returns the session's estimate, or 0 when the session
// is priced neither estimated nor partial.
func sessionEstimateUSD(session pricing.SessionCost) float64 {
	if session.USD == nil || (session.Status != pricing.CostEstimated && session.Status != pricing.CostPartial) {
		return 0
	}
	return *session.USD
}

// issueCost folds the issue's finished runs, as of the last read on this
// entry, and the running session's estimate into the issue's estimated cost.
// It performs no I/O.
func (e *RunningEntry) issueCost(rates *pricing.Rates) (usd float64, status pricing.CostStatus) {
	var recorded pricing.RecordedCost
	spendComplete := false
	if e.IssueUsage != nil {
		recorded = recordedOf(*e.IssueUsage)
		spendComplete = !e.IssueUsage.SpendIncomplete() && e.UsageMeasured && e.UnaccountedTurns == 0
	}
	return pricing.IssueCost(recorded, e.sessionCost(rates), spendComplete)
}

// setMoneyUsage fills the money-axis fields from one read of the issue's run
// history, and the token-axis fields too when no ceiling has filled them, so
// a money hold can state the sessions a budget ceiling stopped mid-run. It
// copies the figures, so the entry never aliases the caller's usage value.
func (e *BudgetExhaustedEntry) setMoneyUsage(usage storage.IssueTokenUsage) {
	used, status := pricing.IssueCost(recordedOf(usage), pricing.SessionCost{}, !usage.SpendIncomplete())
	complete := status == pricing.CostEstimated
	e.UsedUSD = &used
	e.UsedUSDComplete = &complete
	if e.UsedTokens == nil {
		e.setTokenUsage(usage)
	}
}

// warnMoneyBudgetIncomplete records one issue whose estimated cost is below
// the ceiling but only a lower bound, so the dispatch proceeds anyway.
func warnMoneyBudgetIncomplete(log *slog.Logger, usage storage.IssueTokenUsage, budgetUSD float64) {
	used, _ := pricing.IssueCost(recordedOf(usage), pricing.SessionCost{}, !usage.SpendIncomplete())
	attrs := append([]any{
		slog.Float64("used_usd", used),
		slog.Float64("budget_usd", budgetUSD),
	}, incompleteSpendAttrs(usage)...)
	attrs = append(attrs,
		slog.Int("partial_sessions", usage.PartialSessions),
		slog.Int("unpriced_sessions", usage.UnpricedSessions),
	)
	log.Warn("money budget cannot be fully evaluated, allowing dispatch", attrs...)
}

// moneyCeilingStopError builds the run_history error text for a run the
// money ceiling stopped in flight.
func moneyCeilingStopError(usedUSD, budgetUSD float64) error {
	return fmt.Errorf("money ceiling reached: used %s of %s", pricing.FormatUSD(usedUSD), pricing.FormatUSD(budgetUSD))
}

// enforceInFlightMoneyCeiling requests a stop of the run when the issue's
// estimated cost, its finished runs plus this session's live estimate,
// reaches the money ceiling. The estimate against the frozen baseline keeps
// every event free of I/O; only an event reaching the ceiling triggers a
// confirming read. A failed read leaves the run going unless this session's
// own estimate has reached the ceiling, which needs no read.
//
// Must run on the single-writer event loop, after [HandleAgentEvent] has
// applied the event's usage delta.
func enforceInFlightMoneyCeiling(ctx context.Context, state *State, issueID string, event domain.AgentEvent, store storage.RunHistoryStore, logger *slog.Logger) {
	ceiling := state.MaxCostUSD
	if ceiling <= 0 {
		return
	}
	if !hasUsage(event.Usage) {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok || entry.CeilingStopRequest != nil {
		return
	}
	if !admitsUsageFigures(entry.UsageArrival) {
		return
	}
	if used, _ := entry.issueCost(state.Rates); !pricing.CeilingReached(used, ceiling) {
		return
	}

	usage, err := store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		// A completed cost is never negative, so a session whose own
		// estimate already reaches the ceiling proves the breach without the
		// failed read, keeping a persistence outage from suspending the
		// ceiling.
		if pricing.CeilingReached(sessionEstimateUSD(entry.sessionCost(state.Rates)), ceiling) {
			requestMoneyCeilingStop(entry, ceiling, state.Rates, nil, tokenSumSessionSpendAlone)
			return
		}
		if !entry.MoneyCeilingQueryWarned {
			entry.MoneyCeilingQueryWarned = true
			issueTokenCeilingLogger(logger, issueID, entry).Warn("in-flight money ceiling check failed, run continues",
				slog.Any("error", err),
				slog.Float64("budget_usd", ceiling),
			)
		}
		return
	}
	entry.IssueTokensCompleted = usage.UncachedTokens
	entry.IssueUsage = &usage
	if used, _ := entry.issueCost(state.Rates); !pricing.CeilingReached(used, ceiling) {
		return
	}

	requestMoneyCeilingStop(entry, ceiling, state.Rates, &usage, tokenSumConfirmedRead)
}

// requestMoneyCeilingStop records the money ceiling's decision and cancels
// the worker context with [errCeilingStop]. usage is nil without a read.
func requestMoneyCeilingStop(entry *RunningEntry, ceiling float64, rates *pricing.Rates, usage *storage.IssueTokenUsage, sumSource string) {
	used, status := entry.issueCost(rates)
	var recorded pricing.RecordedCost
	if entry.IssueUsage != nil {
		recorded = recordedOf(*entry.IssueUsage)
	}
	entry.CeilingStopRequest = &CeilingStopRequest{
		Reason:             budgetReasonMoney,
		BudgetUSD:          ceiling,
		UsedUSD:            used,
		IssueCostCompleted: recorded.USD,
		SessionCostUSD:     sessionEstimateUSD(entry.sessionCost(rates)),
		CostComplete:       status == pricing.CostEstimated,
		SumSource:          sumSource,
		Usage:              usage,
	}
	if entry.CeilingCancelFunc != nil {
		entry.CeilingCancelFunc()
	}
}

// ceilingStopError builds the run_history error text for a run the request's
// ceiling stopped, from the entry's figures at exit.
func ceilingStopError(state *State, entry *RunningEntry, request *CeilingStopRequest) error {
	if request.Reason == budgetReasonMoney {
		used, _ := entry.issueCost(state.Rates)
		return moneyCeilingStopError(used, request.BudgetUSD)
	}
	return tokenCeilingStopError(entry.issueUncachedTokens(), request.BudgetTokens)
}
