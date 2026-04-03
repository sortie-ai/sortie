package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// reconcileCIStatus polls CI status for each entry in state.PendingCICheck.
// Called from ReconcileRunningIssues after reconcileTrackerState. Skipped
// entirely when params.CIProvider is nil.
func reconcileCIStatus(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.CIProvider == nil {
		return
	}

	for issueID, pending := range state.PendingCICheck {
		delete(state.PendingCICheck, issueID)

		entryLog := logging.WithIssue(log, issueID, pending.Identifier)

		ref := pending.SHA
		if ref == "" {
			ref = pending.Branch
		}

		result, err := params.CIProvider.FetchCIStatus(ctx, ref)
		if err != nil {
			entryLog.Warn("CI status fetch failed, will retry next tick",
				slog.String("ref", ref),
				slog.Any("error", err),
			)
			metrics.IncCIStatusChecks("error")
			state.PendingCICheck[issueID] = pending
			continue
		}

		metrics.IncCIStatusChecks(string(result.Status))

		switch result.Status {
		case domain.CIStatusPassing:
			delete(state.CIFixAttempts, issueID)
			entryLog.Info("CI passing, no action needed",
				slog.String("ref", ref),
			)

		case domain.CIStatusPending:
			state.PendingCICheck[issueID] = pending
			entryLog.Debug("CI pending, will re-check next tick",
				slog.String("ref", ref),
			)

		case domain.CIStatusFailing:
			handleCIFailure(state, params, pending, result, ref, entryLog, ctx, metrics)
		}
	}
}

// handleCIFailure records a CI failure in run_history, increments the
// CI fix attempt counter, and either schedules a CI-fix dispatch or
// escalates if max retries is exceeded.
func handleCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingCICheckEntry,
	result domain.CIResult,
	ref string,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	now := time.Now().UTC()

	ciRunHistory := persistence.RunHistory{
		IssueID:     pending.IssueID,
		Identifier:  pending.Identifier,
		DisplayID:   pending.DisplayID,
		Attempt:     pending.Attempt,
		StartedAt:   now.Format(time.RFC3339),
		CompletedAt: now.Format(time.RFC3339),
		Status:      "ci_failed",
		Error:       stringPtr("CI checks failed on ref " + ref),
	}
	if _, err := params.Store.AppendRunHistory(ctx, ciRunHistory); err != nil {
		log.Error("failed to persist CI failure run history",
			slog.Any("error", err),
		)
	}

	state.CIFixAttempts[pending.IssueID]++
	attempts := state.CIFixAttempts[pending.IssueID]

	maxRetries := params.CIFeedback.MaxRetries

	if attempts > maxRetries {
		escalateCIFailure(state, params, pending, result, ref, attempts, log, ctx, metrics)
		return
	}

	ciContext := result.ToTemplateMap()

	CancelRetry(state, pending.IssueID)

	nextAttempt := pending.Attempt

	ScheduleRetry(state, ScheduleRetryParams{
		IssueID:          pending.IssueID,
		Identifier:       pending.Identifier,
		DisplayID:        pending.DisplayID,
		Attempt:          nextAttempt,
		DelayMS:          continuationDelayMS,
		Error:            "",
		LastSSHHost:      pending.LastSSHHost,
		CIFailureContext: ciContext,
	}, params.OnRetryFire)
	metrics.IncRetries(triggerCIFix)

	log.Info("CI failure detected, scheduling CI fix dispatch",
		slog.String("ref", ref),
		slog.Int("failing_count", result.FailingCount),
		slog.Int("ci_fix_attempt", attempts),
		slog.Int("max_retries", maxRetries),
	)
}

// escalateCIFailure handles the case where CI fix retries are exhausted.
// It applies the configured escalation action (label or comment), cancels
// the retry, and releases the claim.
func escalateCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingCICheckEntry,
	result domain.CIResult,
	ref string,
	attempts int,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	log.Warn("CI fix retries exhausted, escalating",
		slog.String("ref", ref),
		slog.Int("attempts", attempts),
		slog.Int("max_retries", params.CIFeedback.MaxRetries),
	)

	metrics.IncCIEscalations(params.CIFeedback.Escalation)

	switch params.CIFeedback.Escalation {
	case "label":
		label := params.CIFeedback.EscalationLabel
		if label == "" {
			label = "needs-human"
		}
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log

			go func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("CI escalation label failed",
						slog.Any("error", err),
					)
					m.IncCIEscalations("error")
				}
			}()
		}

	case "comment", "":
		commentText := buildCIEscalationComment(result, ref, attempts)
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			ct := commentText

			go func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
					escalLog.Warn("CI escalation comment failed",
						slog.Any("error", err),
					)
					m.IncCIEscalations("error")
				}
			}()
		}
	}

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during CI escalation",
			slog.Any("error", err),
		)
	}

	delete(state.Claimed, pending.IssueID)
	delete(state.CIFixAttempts, pending.IssueID)
}

// buildCIEscalationComment builds a plain-text escalation comment for
// CI failures that exceeded the retry budget. Plain text is used so the
// comment renders consistently across all tracker adapters.
func buildCIEscalationComment(result domain.CIResult, ref string, attempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI fix retries exhausted\n\n")
	fmt.Fprintf(&b, "Sortie attempted %d CI-fix continuation(s) on ref %s but CI is still failing.\n\n", attempts, ref)

	if result.FailingCount > 0 {
		fmt.Fprintf(&b, "Failing checks: %d\n", result.FailingCount)
	}

	for _, cr := range result.CheckRuns {
		switch cr.Conclusion {
		case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut, domain.CheckConclusionCancelled:
			fmt.Fprintf(&b, "- %s: %s", cr.Name, cr.Conclusion)
			if cr.DetailsURL != "" {
				fmt.Fprintf(&b, " (details: %s)", cr.DetailsURL)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\nManual intervention required.")
	return b.String()
}
