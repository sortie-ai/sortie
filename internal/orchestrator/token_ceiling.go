package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// errTokenCeilingStop distinguishes a ceiling stop from other cancellation causes.
var errTokenCeilingStop = errors.New("token ceiling reached")

// TokenCeilingStopRequest records the in-flight token ceiling's decision
// to stop a run, set by [requestTokenCeilingStop].
type TokenCeilingStopRequest struct {
	// BudgetTokens is the ceiling in force when the stop was decided.
	BudgetTokens int

	// UsedTokens is IssueTokensCompleted plus SessionTokens at the decision.
	UsedTokens int64

	// IssueTokensCompleted is the issue's completed-session sum at the decision.
	IssueTokensCompleted int64

	// SessionTokens is this session's own cumulative spend at the decision.
	SessionTokens int64

	// SumSource is tokenSumConfirmedRead or tokenSumSessionSpendAlone.
	SumSource string

	// Usage is the confirming read behind the decision, nil for tokenSumSessionSpendAlone.
	Usage *persistence.IssueTokenUsage
}

// issueTokenStore is the persistence contract [freezeIssueTokenBaseline]
// and [enforceInFlightTokenCeiling] need.
type issueTokenStore interface {
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
}

// freezeIssueTokenBaseline reads the issue's completed-session token sum
// onto the running entry so [enforceInFlightTokenCeiling]'s pre-filter has
// a baseline from the first usage event onward. Must run on the
// single-writer event loop, after entry.UsageArrival has been assigned.
func freezeIssueTokenBaseline(ctx context.Context, state *State, issueID string, store issueTokenStore, logger *slog.Logger) {
	if state.MaxTokens <= 0 {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)
	usage, err := store.TokenUsageByIssue(ctx, issueID)

	// A run whose arrival reports no figure is unbounded whatever the
	// baseline read did, so a failed read rides on that record rather than
	// claiming the ceiling bounds this session.
	switch {
	case !entry.UsageArrival.ReportsAnyFigure():
		attrs := []any{
			slog.String("agent_kind", entry.AgentKind),
			slog.String("usage_arrival", string(entry.UsageArrival)),
			slog.Int("budget_tokens", state.MaxTokens),
		}
		if err != nil {
			attrs = append(attrs, slog.Any("error", err))
		}
		log.Warn("token ceiling cannot bound this run", attrs...)
	case err != nil:
		log.Warn("prior token spend unknown, token ceiling bounds this session only",
			slog.Any("error", err),
			slog.Int("budget_tokens", state.MaxTokens),
		)
	}

	if err != nil {
		return
	}
	entry.IssueTokensCompleted = usage.TotalTokens
}

// enforceInFlightTokenCeiling requests a stop of the run when the issue's
// completed-session sum plus this session's cumulative spend reaches the
// ceiling. An integer pre-filter against the frozen baseline keeps every
// event free of I/O; only an event crossing the pre-filter triggers a
// confirming read. A failed read leaves the run going unless this
// session's own spend has reached the ceiling, which needs no read.
//
// Must run on the single-writer event loop, after [HandleAgentEvent] has
// applied the event's usage delta.
func enforceInFlightTokenCeiling(ctx context.Context, state *State, issueID string, event domain.AgentEvent, store issueTokenStore, logger *slog.Logger) {
	ceiling := state.MaxTokens
	if ceiling <= 0 {
		return
	}
	if !hasUsage(event.Usage) {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok || entry.TokenCeilingStopRequest != nil {
		return
	}
	if !admitsUsageFigures(entry.UsageArrival) {
		return
	}
	if entry.IssueTokensCompleted+entry.AgentTotalTokens < int64(ceiling) {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)

	usage, err := store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		// A completed sum is never negative, so a session whose own spend
		// already reaches the ceiling proves the breach without the failed
		// read, keeping a persistence outage from suspending the ceiling.
		if entry.AgentTotalTokens >= int64(ceiling) {
			requestTokenCeilingStop(entry, ceiling, nil, tokenSumSessionSpendAlone)
			return
		}
		if !entry.TokenCeilingQueryWarned {
			entry.TokenCeilingQueryWarned = true
			log.Warn("in-flight token ceiling check failed, run continues",
				slog.Any("error", err),
				slog.Int("budget_tokens", ceiling),
			)
		}
		return
	}
	entry.IssueTokensCompleted = usage.TotalTokens
	if entry.IssueTokensCompleted+entry.AgentTotalTokens < int64(ceiling) {
		return
	}

	requestTokenCeilingStop(entry, ceiling, &usage, tokenSumConfirmedRead)
}

// sum_source values for the stop record. A confirmed read carries an exact
// completed sum; a session-spend-alone stop carries only the entry's
// baseline, which makes used_tokens a lower bound and leaves the counts
// behind an incomplete sum unknown.
const (
	tokenSumConfirmedRead     = "confirmed_read"
	tokenSumSessionSpendAlone = "session_spend_alone"
)

// requestTokenCeilingStop records the ceiling's decision and cancels the
// worker context with [errTokenCeilingStop]. usage is nil without a read.
func requestTokenCeilingStop(entry *RunningEntry, ceiling int, usage *persistence.IssueTokenUsage, sumSource string) {
	entry.TokenCeilingStopRequest = &TokenCeilingStopRequest{
		BudgetTokens:         ceiling,
		UsedTokens:           entry.IssueTokensCompleted + entry.AgentTotalTokens,
		IssueTokensCompleted: entry.IssueTokensCompleted,
		SessionTokens:        entry.AgentTotalTokens,
		SumSource:            sumSource,
		Usage:                usage,
	}
	if entry.TokenCeilingCancelFunc != nil {
		entry.TokenCeilingCancelFunc()
	}
}

// reportTokenCeilingStop counts and logs a decided stop once the run's
// exit confirms the ceiling ended it. Must be called at most once per run.
func reportTokenCeilingStop(log *slog.Logger, metrics domain.Metrics, request *TokenCeilingStopRequest) {
	metrics.IncRunsStoppedByBudget(budgetReasonToken)

	attrs := []any{
		slog.String("reason", budgetReasonToken),
		slog.Int64("used_tokens", request.UsedTokens),
		slog.Int("budget_tokens", request.BudgetTokens),
		slog.Int64("issue_tokens_completed", request.IssueTokensCompleted),
		slog.Int64("session_tokens", request.SessionTokens),
		slog.String("sum_source", request.SumSource),
		slog.String("ceiling_setting", ceilingSettingByBudgetReason[budgetReasonToken]),
	}
	if request.Usage != nil {
		attrs = append(attrs, incompleteSpendAttrs(*request.Usage)...)
	}
	log.Warn("run stopped by token ceiling", attrs...)
}

// incompleteSpendAttrs names the two counts that keep a summed token total
// from being an issue's whole spend: sessions whose usage was never
// recorded and turns that spent an amount nothing reported. Both are
// always emitted together.
func incompleteSpendAttrs(usage persistence.IssueTokenUsage) []any {
	return []any{
		slog.Int("unmeasured_sessions", usage.UnmeasuredSessions),
		slog.Int("unaccounted_turns", usage.UnaccountedTurns),
	}
}

// warnTokenBudgetIncomplete records one issue whose summed spend is below
// the ceiling but only a lower bound, so the dispatch proceeds anyway.
func warnTokenBudgetIncomplete(log *slog.Logger, usage persistence.IssueTokenUsage, budgetTokens int) {
	attrs := append([]any{
		slog.Int64("used_tokens", usage.TotalTokens),
		slog.Int64("budget_tokens", int64(budgetTokens)),
	}, incompleteSpendAttrs(usage)...)
	log.Warn("token budget cannot be fully evaluated, allowing dispatch", attrs...)
}

// warnCeilingMeasuredNothing records one ended run whose ceiling was set
// and whose arrival declared that figures report, yet recorded none. Must
// be called once per run from the exit path with the run's settled
// verdict. A kind whose arrival reports no figure is announced at dispatch
// instead and skipped here.
func warnCeilingMeasuredNothing(log *slog.Logger, entry *RunningEntry, budgetTokens int, measured bool) {
	if budgetTokens <= 0 || measured || !entry.UsageArrival.ReportsAnyFigure() {
		return
	}
	log.Warn("run reported no token usage, token ceiling could not bound it",
		slog.String("agent_kind", entry.AgentKind),
		slog.String("usage_arrival", string(entry.UsageArrival)),
		slog.Int("budget_tokens", budgetTokens),
	)
}

// issueTokenCeilingLogger derives the issue- and session-scoped logger for
// this file's records, mirroring [HandleAgentEvent]'s derivation so the two
// record sets on one run agree on their identifying attributes.
func issueTokenCeilingLogger(logger *slog.Logger, issueID string, entry *RunningEntry) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	log := logging.WithIssue(logger, issueID, entry.Identifier)
	if entry.SessionID != "" {
		log = logging.WithSession(log, entry.SessionID)
	}
	return log
}

// tokenCeilingStopError builds the run_history error text for a run the
// token ceiling stopped in flight.
func tokenCeilingStopError(usedTokens int64, budgetTokens int) error {
	return fmt.Errorf("token ceiling reached: used %d of %d tokens", usedTokens, budgetTokens)
}
