package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

// recordReactionActivity records activity for each kind in the pass lineage
// names. A nil store, or a lineage without a chain identifier or a dispatch
// time, names no pass, so nothing is written. A failed write is logged and
// the caller proceeds: the act it describes has already happened.
func recordReactionActivity(
	ctx context.Context,
	store storage.ReactionActivityStore,
	issueID string,
	lineage StageLineage,
	activity string,
	kinds []string,
	now time.Time,
	log *slog.Logger,
) {
	if store == nil || lineage.ChainID == "" || lineage.DispatchedAt.IsZero() || len(kinds) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	records := make([]storage.ReactionActivity, 0, len(kinds))
	for _, kind := range kinds {
		records = append(records, storage.ReactionActivity{
			IssueID:    issueID,
			ChainID:    lineage.ChainID,
			Kind:       kind,
			Activity:   activity,
			RecordedAt: now,
		})
	}
	if err := store.RecordReactionActivity(ctx, records); err != nil {
		log.Error("failed to record reaction activity",
			slog.String("activity", activity),
			slog.Any("error", err),
		)
	}
}

// scheduleReactionContinuation schedules params through [ScheduleRetry] and
// records one continuation of params.ReactionKind in the pass
// params.StageLineage names, logging through params.Logger. Every
// continuation a reconcile pass schedules goes through it.
func scheduleReactionContinuation(
	ctx context.Context,
	state *State,
	store storage.ReactionActivityStore,
	params ScheduleRetryParams,
	onFire func(issueID string),
	now time.Time,
) {
	ScheduleRetry(state, params, onFire)
	recordReactionActivity(ctx, store, params.IssueID, params.StageLineage,
		storage.ReactionActivityContinuation, []string{params.ReactionKind}, now, params.Logger)
}
