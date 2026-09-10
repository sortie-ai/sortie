package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// issueTokenStore is the persistence contract [freezeIssueTokenBaseline]
// and [enforceInFlightTokenCeiling] need. Satisfied by
// [OrchestratorStore] and by [RetryTimerStore].
type issueTokenStore interface {
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
}

// freezeIssueTokenBaseline reads the issue's completed-session token sum
// onto the running entry, so [enforceInFlightTokenCeiling]'s pre-filter
// has a baseline from the first usage event onward. It reports the two
// conditions under which the ceiling cannot bound the run: an arrival
// disposition that reports no figure at all, and a baseline read that
// fails.
//
// Called from both dispatch lanes, after entry.UsageArrival has been
// assigned. Must run on the orchestrator's single-writer event loop.
// Takes the unscoped logger and derives the issue-scoped one itself, as
// [HandleAgentEvent] does, so a record's shape does not depend on which
// lane dispatched the run.
func freezeIssueTokenBaseline(ctx context.Context, state *State, issueID string, store issueTokenStore, logger *slog.Logger) {
	if state.MaxTokens <= 0 {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)
	if !entry.UsageArrival.ReportsAnyFigure() {
		log.Warn("token ceiling cannot bound this run",
			slog.String("agent_kind", entry.AgentKind),
			slog.String("usage_arrival", string(entry.UsageArrival)),
			slog.Int("budget_tokens", state.MaxTokens),
		)
	}
	usage, err := store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		log.Warn("prior token spend unknown, token ceiling bounds this session only",
			slog.Any("error", err),
			slog.Int("budget_tokens", state.MaxTokens),
		)
		return
	}
	entry.IssueTokensCompleted = usage.TotalTokens
}

// enforceInFlightTokenCeiling evaluates the per-issue token ceiling
// against the issue's completed-session sum plus this session's
// cumulative spend, and stops the run when the sum reaches the ceiling.
// An integer pre-filter against the baseline frozen by
// [freezeIssueTokenBaseline] keeps every event free of I/O; only an
// event that crosses the pre-filter triggers a confirming read, which
// makes the kill decision exact.
//
// Must be called from the orchestrator's single-writer event loop,
// after [HandleAgentEvent] has applied the event's usage delta. Takes
// the unscoped logger and derives the issue-scoped one itself, as
// [HandleAgentEvent] does.
func enforceInFlightTokenCeiling(ctx context.Context, state *State, issueID string, event domain.AgentEvent, store issueTokenStore, metrics domain.Metrics, logger *slog.Logger) {
	ceiling := state.MaxTokens
	if ceiling <= 0 {
		return
	}
	if !hasUsage(event.Usage) {
		return
	}
	entry, ok := state.Running[issueID]
	if !ok || entry.TokenCeilingStopped {
		return
	}
	if entry.IssueTokensCompleted+entry.AgentTotalTokens < int64(ceiling) {
		return
	}
	log := issueTokenCeilingLogger(logger, issueID, entry)

	usage, err := store.TokenUsageByIssue(ctx, issueID)
	if err != nil {
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
	used := usage.TotalTokens + entry.AgentTotalTokens
	if used < int64(ceiling) {
		return
	}

	entry.TokenCeilingStopped = true
	metrics.IncRunsStoppedByBudget(budgetReasonToken)
	log.Warn("run stopped by token ceiling",
		slog.String("reason", budgetReasonToken),
		slog.Int64("used_tokens", used),
		slog.Int("budget_tokens", ceiling),
		slog.Int64("issue_tokens_completed", usage.TotalTokens),
		slog.Int64("session_tokens", entry.AgentTotalTokens),
		slog.Int("unmeasured_sessions", usage.UnmeasuredSessions),
		slog.String("ceiling_setting", ceilingSettingByBudgetReason[budgetReasonToken]),
	)
	if entry.CancelFunc != nil {
		entry.CancelFunc()
	}
}

// issueTokenCeilingLogger derives the logger every record in this file
// is emitted through: issue context always, session context once the
// entry carries a session. It mirrors the derivation [HandleAgentEvent]
// performs, so the two sets of records on one run agree on their
// identifying attributes.
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
// token ceiling stopped in flight, naming the used and budgeted token
// figures.
func tokenCeilingStopError(usedTokens int64, budgetTokens int) error {
	return fmt.Errorf("token ceiling reached: used %d of %d tokens", usedTokens, budgetTokens)
}
