package orchestrator

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
)

// AcknowledgeStatus is what a request to acknowledge an attention item did.
type AcknowledgeStatus int

const (
	AcknowledgeRecorded  AcknowledgeStatus = iota + 1 // the item is acknowledged now
	AcknowledgeAlready                                // the item was acknowledged before; nothing changed
	AcknowledgeNotFound                               // no current item has the ID; nothing changed
	AcknowledgeFailed                                 // the store did not save the acknowledgement; nothing changed
	AcknowledgeDraining                               // shutdown began before the loop took the request; nothing changed
	AcknowledgeWithdrawn                              // the caller's context ended before the loop took the request; nothing changed
)

// AcknowledgeResult is the answer to a request to acknowledge an attention
// item. Cause and AcknowledgedAt are set only with [AcknowledgeRecorded] and
// [AcknowledgeAlready].
type AcknowledgeResult struct {
	Status         AcknowledgeStatus
	Cause          AttentionCause
	AcknowledgedAt time.Time // UTC: when the item was first acknowledged
}

// AcknowledgeFunc returns a function that records that a person has seen the
// attention item with the given ID. The function is safe for concurrent use
// and returns once the event loop has taken the request, ctx has ended, or
// shutdown has begun.
func (o *Orchestrator) AcknowledgeFunc() func(ctx context.Context, itemID string) AcknowledgeResult {
	return func(ctx context.Context, itemID string) AcknowledgeResult {
		var result AcknowledgeResult
		switch o.runOnLoop(ctx, func(loopCtx context.Context) {
			result = o.acknowledgeOnLoop(loopCtx, itemID)
		}) {
		case loopRan:
			return result
		case loopDraining:
			return AcknowledgeResult{Status: AcknowledgeDraining}
		default:
			return AcknowledgeResult{Status: AcknowledgeWithdrawn}
		}
	}
}

// acknowledgeOnLoop acknowledges the current attention item with the given
// ID. It matches the ID against the causes derived now, so a cleared item is
// not found and no row is written for it. The map changes only after the
// store saved the row. Must run on the event loop.
func (o *Orchestrator) acknowledgeOnLoop(ctx context.Context, itemID string) AcknowledgeResult {
	now := time.Now().UTC()
	health := o.Health()
	causes := AttentionCauses(RuntimeSnapshot(o.state, now), &health)
	i := slices.IndexFunc(causes, func(c AttentionCause) bool { return c.ID == itemID })
	if i < 0 {
		return AcknowledgeResult{Status: AcknowledgeNotFound}
	}
	cause := causes[i]

	if at, held := o.state.AttentionAcknowledgements[itemID]; held {
		return AcknowledgeResult{Status: AcknowledgeAlready, Cause: cause, AcknowledgedAt: at}
	}

	log := attentionLogger(o.logger, cause)
	err := o.store.SaveAttentionAcknowledgement(ctx, storage.AttentionAcknowledgement{ItemID: itemID, AcknowledgedAt: now})
	if err != nil {
		log.Error("failed to persist attention acknowledgement",
			slog.String("item_id", itemID),
			slog.String("kind", cause.Kind),
			slog.Any("error", err),
		)
		return AcknowledgeResult{Status: AcknowledgeFailed}
	}

	o.state.AttentionAcknowledgements[itemID] = now
	log.Info("attention item acknowledged",
		slog.String("item_id", itemID),
		slog.String("kind", cause.Kind),
	)
	o.notifyObservers()
	return AcknowledgeResult{Status: AcknowledgeRecorded, Cause: cause, AcknowledgedAt: now}
}

// attentionLogger scopes log to the issue the cause names; a process-wide
// cause names none.
func attentionLogger(log *slog.Logger, cause AttentionCause) *slog.Logger {
	switch {
	case cause.Parked != nil:
		return logging.WithIssue(log, cause.Parked.IssueID, cause.Parked.Identifier)
	case cause.Budget != nil:
		return logging.WithIssue(log, cause.Budget.IssueID, cause.Budget.Identifier)
	case cause.Reaction != nil:
		return logging.WithIssue(log, cause.Reaction.IssueID, cause.Reaction.Identifier)
	default:
		return log
	}
}

// pruneAttentionAcknowledgements deletes the acknowledgement of every item
// the state no longer derives, so an acknowledgement never outlives its
// episode. It belongs on a tick that read the candidate issues: before that
// read after a restart the budget holds are not rebuilt yet, and a prune would
// drop their acknowledgements. A failed delete keeps the map and the rows for
// the next such tick. Must run on the event loop.
func (o *Orchestrator) pruneAttentionAcknowledgements(ctx context.Context) {
	if len(o.state.AttentionAcknowledgements) == 0 {
		return
	}
	now := time.Now().UTC()
	health := o.Health()
	live := make(map[string]struct{})
	for _, cause := range AttentionCauses(RuntimeSnapshot(o.state, now), &health) {
		live[cause.ID] = struct{}{}
	}

	var stale []string
	for id := range o.state.AttentionAcknowledgements {
		if _, ok := live[id]; !ok {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return
	}

	if err := o.store.DeleteAttentionAcknowledgements(ctx, stale); err != nil {
		o.logger.Warn("failed to delete attention acknowledgements", slog.Any("error", err))
		return
	}
	for _, id := range stale {
		delete(o.state.AttentionAcknowledgements, id)
	}
}

// PopulateAttentionAcknowledgements loads persisted acknowledgements into
// state.AttentionAcknowledgements. Called during startup recovery, before the
// event loop starts, beside [PopulateBudgetHoldNotices]. A row with an empty
// ID or a zero time, which is how the store reports a time it could not
// parse, is skipped.
func PopulateAttentionAcknowledgements(state *State, rows []storage.AttentionAcknowledgement, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}

	for _, row := range rows {
		if row.ItemID == "" || row.AcknowledgedAt.IsZero() {
			log.Warn("skipping malformed attention acknowledgement record")
			continue
		}
		state.AttentionAcknowledgements[row.ItemID] = row.AcknowledgedAt.UTC()
	}
}
