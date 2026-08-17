package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	defaultHandoffAbsenceCeiling = 3
	defaultHandoffParkingLabel   = "needs-human"
	reviewCommentsConfigKey      = "review_comments"
)

// handoffAbsenceStore is the persistence surface shared by worker exit,
// retry firing, and ordinary polling. The run-history query is the durable
// eligibility gate; deleting the retry row stops a recovered timer from
// competing with that gate.
type handoffAbsenceStore interface {
	DeleteRetryEntry(ctx context.Context, issueID string) error
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
}

func handoffAbsenceCeiling(maxSessions int) int {
	if maxSessions > 0 {
		return maxSessions
	}
	return defaultHandoffAbsenceCeiling
}

// resolveHandoffParkingLabel captures only the review-comments escalation
// label name. The reaction need not be active and its escalation action does
// not participate in primary-dispatch parking.
func resolveHandoffParkingLabel(reactions map[string]config.ReactionConfig) string {
	if review, ok := reactions[reviewCommentsConfigKey]; ok && review.EscalationLabel != "" {
		return review.EscalationLabel
	}
	return defaultHandoffParkingLabel
}

func clearHandoffAbsenceGate(state *State, issueID string) {
	if state.BudgetExhaustedReason[issueID] != budgetReasonHandoffAbsence {
		return
	}
	delete(state.BudgetExhausted, issueID)
	delete(state.BudgetExhaustedReason, issueID)
}

// parkHandoffAbsence stops all queued work for the issue, records the durable
// runtime gate, releases the claim, and applies the operator's captured human
// parking label. The label write is best-effort: failure is logged but never
// reopens dispatch eligibility.
func parkHandoffAbsence(
	state *State,
	ctx context.Context,
	store handoffAbsenceStore,
	tracker domain.TrackerAdapter,
	issueID string,
	count int,
	ceiling int,
	label string,
	log *slog.Logger,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = slog.Default()
	}
	if label == "" {
		label = defaultHandoffParkingLabel
	}

	CancelRetry(state, issueID)
	delete(state.Claimed, issueID)
	state.BudgetExhausted[issueID] = struct{}{}
	state.BudgetExhaustedReason[issueID] = budgetReasonHandoffAbsence

	if err := store.DeleteRetryEntry(ctx, issueID); err != nil {
		log.Error("failed to delete retry entry after handoff absence parking",
			slog.Any("error", err),
		)
	}

	log.Warn("handoff absence ceiling reached, parking issue",
		slog.Int("consecutive_absences", count),
		slog.Int("absence_ceiling", ceiling),
		slog.String("escalation_label", label),
	)

	if tracker == nil {
		log.Warn("handoff absence escalation label failed",
			slog.String("escalation_label", label),
			slog.String("reason", "tracker adapter is nil"),
		)
		return
	}

	state.TrackerOpsWg.Go(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := tracker.AddLabel(dctx, issueID, label); err != nil {
			log.Warn("handoff absence escalation label failed",
				slog.String("escalation_label", label),
				slog.Any("error", err),
			)
		}
	})
}
