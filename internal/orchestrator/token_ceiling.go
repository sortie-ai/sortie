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

// errCeilingStop distinguishes a ceiling stop from other cancellation causes.
var errCeilingStop = errors.New("budget ceiling reached")

// CeilingStopRequest records an in-flight budget ceiling's decision to stop
// a run, set by [requestTokenCeilingStop] or [enforceInFlightMoneyCeiling].
// The fields of the axis that did not decide the stop stay zero.
type CeilingStopRequest struct {
	// Reason is budgetReasonToken or budgetReasonMoney.
	Reason string

	// BudgetTokens is the ceiling in force when the stop was decided.
	BudgetTokens int

	// UsedTokens is IssueTokensCompleted plus SessionTokens at the decision,
	// in uncached tokens.
	UsedTokens int64

	// IssueTokensCompleted is the issue's completed-session sum of uncached
	// tokens at the decision.
	IssueTokensCompleted int64

	// SessionTokens is this session's own cumulative uncached tokens at the
	// decision.
	SessionTokens int64

	// BudgetUSD is the money ceiling in force when the stop was decided.
	BudgetUSD float64

	// UsedUSD is IssueCostCompleted plus SessionCostUSD at the decision.
	UsedUSD float64

	// IssueCostCompleted is the recorded cost of the issue's finished runs
	// at the decision.
	IssueCostCompleted float64

	// SessionCostUSD is this session's own estimate at the decision; 0
	// unless the session is priced estimated or partial.
	SessionCostUSD float64

	// CostComplete reports whether UsedUSD priced all of the issue's spend.
	CostComplete bool

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

// sessionUncachedTokens returns the running session's total tokens less its
// cache reads, floored at zero.
func (e *RunningEntry) sessionUncachedTokens() int64 {
	return domain.TokenUsage{TotalTokens: e.AgentTotalTokens, CacheReadTokens: e.CacheReadTokens}.Uncached()
}

// issueUncachedTokens returns the issue's completed-session uncached tokens
// plus the running session's own.
func (e *RunningEntry) issueUncachedTokens() int64 {
	return e.IssueTokensCompleted + e.sessionUncachedTokens()
}

// freezeIssueTokenBaseline reads the issue's completed-session usage onto
// the running entry, so [enforceInFlightTokenCeiling]'s pre-filter has a
// baseline from the first usage event onward and the entry serves the issue's
// spend whether or not a ceiling is set. Without a ceiling a failed read
// emits nothing. Must run on the single-writer event loop, after
// entry.UsageArrival and entry.CarriesTokenFigure have been assigned.
func freezeIssueTokenBaseline(ctx context.Context, state *State, issueID string, store issueTokenStore, logger *slog.Logger) {
	entry, ok := state.Running[issueID]
	if !ok {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)
	usage, err := store.TokenUsageByIssue(ctx, issueID)

	if state.MaxTokens > 0 {
		warnCeilingUnboundAtDispatch(log, state.MaxTokens, entry, err)
	}

	if err != nil && state.MaxCostUSD > 0 {
		log.Warn("prior spend unknown, money ceiling bounds this session only",
			slog.Float64("budget_usd", state.MaxCostUSD),
		)
	}

	if err != nil {
		return
	}
	entry.IssueTokensCompleted = usage.UncachedTokens
	entry.IssueUsage = &usage
	if entry.CarriesTokenFigure {
		reachTokenWarning(state, issueID, entry, logger)
	}
}

// warnCeilingUnboundAtDispatch records what the ceiling cannot bound at dispatch. A run
// that can carry no token figure is unbounded whatever the baseline read
// did, so a failed read rides on that record rather than claiming the
// ceiling bounds this session.
func warnCeilingUnboundAtDispatch(log *slog.Logger, budgetTokens int, entry *RunningEntry, readErr error) {
	switch {
	case !entry.CarriesTokenFigure:
		attrs := []any{
			slog.String("agent_kind", entry.AgentKind),
			slog.String("usage_arrival", string(entry.UsageArrival)),
			slog.Int("budget_tokens", budgetTokens),
		}
		if readErr != nil {
			attrs = append(attrs, slog.Any("error", readErr))
		}
		log.Warn("token ceiling cannot bound this run", attrs...)
	case readErr != nil:
		log.Warn("prior token spend unknown, token ceiling bounds this session only",
			slog.Any("error", readErr),
			slog.Int("budget_tokens", budgetTokens),
		)
	}
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
	if !ok || entry.CeilingStopRequest != nil {
		return
	}
	if !admitsUsageFigures(entry.UsageArrival) {
		return
	}
	if entry.issueUncachedTokens() < int64(ceiling) {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)

	usage, err := store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		// A completed sum is never negative, so a session whose own spend
		// already reaches the ceiling proves the breach without the failed
		// read, keeping a persistence outage from suspending the ceiling.
		if entry.sessionUncachedTokens() >= int64(ceiling) {
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
	entry.IssueTokensCompleted = usage.UncachedTokens
	entry.IssueUsage = &usage
	if entry.issueUncachedTokens() < int64(ceiling) {
		return
	}

	requestTokenCeilingStop(entry, ceiling, &usage, tokenSumConfirmedRead)
}

// evaluateTokenWarning evaluates the token warning threshold for one
// usage-bearing event, ahead of the ceiling's own evaluation. A
// reaching figure owes an incremental [sessionMetadataWriteInterval]
// write, recorded on entry so [Orchestrator.maybeWriteIncrementalMetadata]
// bypasses the throttle for it.
//
// Must run on the single-writer event loop, after [HandleAgentEvent]
// has applied the event's usage delta.
func evaluateTokenWarning(state *State, issueID string, event domain.AgentEvent, logger *slog.Logger) {
	if !hasUsage(event.Usage) {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok {
		return
	}
	if !admitsUsageFigures(entry.UsageArrival) {
		return
	}
	if reachTokenWarning(state, issueID, entry, logger) {
		entry.MetadataWritePending = true
	}
}

// reachTokenWarning reports whether entry's live per-issue figure has
// just reached state.TokenWarningThreshold, latching entry so the
// record is emitted at most once per run. Returns false when the
// threshold is disabled, already latched, the ceiling has already
// decided to stop this run, or the run's worker context is done: none
// of these runs can still be interrupted by the ceiling, so a warning
// would send the operator after a run that is already ending.
func reachTokenWarning(state *State, issueID string, entry *RunningEntry, logger *slog.Logger) bool {
	threshold := state.TokenWarningThreshold
	if threshold <= 0 || entry.TokenWarningReached {
		return false
	}
	if entry.CeilingStopRequest != nil {
		return false
	}
	select {
	case <-entry.WorkerDone:
		return false
	default:
	}
	used := entry.issueUncachedTokens()
	if used < int64(threshold) {
		return false
	}

	entry.TokenWarningReached = true
	log := issueTokenCeilingLogger(logger, issueID, entry)
	log.Warn("token warning threshold reached",
		slog.Int64("used_tokens", used),
		slog.Int("warning_tokens", threshold),
		slog.Int("budget_tokens", state.MaxTokens),
		slog.Int64("issue_tokens_completed", entry.IssueTokensCompleted),
		slog.Int64("session_tokens", entry.sessionUncachedTokens()),
		slog.String("ceiling_setting", ceilingSettingByBudgetReason[budgetReasonToken]),
		slog.String("warning_setting", "agent.token_warning_percent"),
	)
	return true
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
// worker context with [errCeilingStop]. usage is nil without a read.
func requestTokenCeilingStop(entry *RunningEntry, ceiling int, usage *persistence.IssueTokenUsage, sumSource string) {
	entry.CeilingStopRequest = &CeilingStopRequest{
		Reason:               budgetReasonToken,
		BudgetTokens:         ceiling,
		UsedTokens:           entry.issueUncachedTokens(),
		IssueTokensCompleted: entry.IssueTokensCompleted,
		SessionTokens:        entry.sessionUncachedTokens(),
		SumSource:            sumSource,
		Usage:                usage,
	}
	if entry.CeilingCancelFunc != nil {
		entry.CeilingCancelFunc()
	}
}

// reportCeilingStop counts and logs a decided stop once the run's
// exit confirms the ceiling ended it. Must be called at most once per run.
func reportCeilingStop(log *slog.Logger, metrics domain.Metrics, request *CeilingStopRequest) {
	metrics.IncRunsStoppedByBudget(request.Reason)

	if request.Reason == budgetReasonMoney {
		reportMoneyCeilingStop(log, request)
		return
	}

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

// reportMoneyCeilingStop logs a run the money ceiling stopped in flight.
func reportMoneyCeilingStop(log *slog.Logger, request *CeilingStopRequest) {
	attrs := []any{
		slog.String("reason", budgetReasonMoney),
		slog.Float64("used_usd", request.UsedUSD),
		slog.Float64("budget_usd", request.BudgetUSD),
		slog.Float64("issue_cost_completed", request.IssueCostCompleted),
		slog.Float64("session_cost_usd", request.SessionCostUSD),
		slog.Bool("cost_complete", request.CostComplete),
		slog.String("sum_source", request.SumSource),
		slog.String("ceiling_setting", ceilingSettingByBudgetReason[budgetReasonMoney]),
	}
	if request.Usage != nil {
		attrs = append(attrs, incompleteSpendAttrs(*request.Usage)...)
	}
	log.Warn("run stopped by money ceiling", attrs...)
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
		slog.Int64("used_tokens", usage.UncachedTokens),
		slog.Int64("budget_tokens", int64(budgetTokens)),
	}, incompleteSpendAttrs(usage)...)
	log.Warn("token budget cannot be fully evaluated, allowing dispatch", attrs...)
}

// warnCeilingMeasuredNothing records one ended run whose ceiling was set
// and whose kind can carry a token figure, yet recorded none. Must
// be called once per run from the exit path with the run's settled
// verdict. A run that can carry no token figure is announced at dispatch
// instead and skipped here.
func warnCeilingMeasuredNothing(log *slog.Logger, entry *RunningEntry, budgetTokens int, measured bool) {
	if budgetTokens <= 0 || measured || !entry.CarriesTokenFigure {
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
