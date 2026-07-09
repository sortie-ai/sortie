package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// labelReviewPendingBackoffBase is the floor for the label-review poll
// interval and, through it, for the per-tick deferral and fetch-error
// backoff delays. The exponential backoff itself is computed by
// [computeReactionPendingDelay]; this constant only bounds it from below.
const labelReviewPendingBackoffBase = 10 * time.Second

// labelReviewMarkLayout is a fixed-width RFC 3339 layout forcing all nine
// fractional-second digits. [time.RFC3339Nano] trims trailing zero
// fractional digits, which breaks lexical ordering between timestamps of
// differing precision; the zero placeholders keep marks lexically sortable
// in chronological order.
const labelReviewMarkLayout = "2006-01-02T15:04:05.000000000Z07:00"

// labelReviewMark formats a journal event as its opaque high-water-mark
// position: a fixed-width UTC timestamp joined to the entry id. Positions
// compare lexically by (At, ID).
func labelReviewMark(e domain.LabelEvent) string {
	return e.At.UTC().Format(labelReviewMarkLayout) + "|" + e.ID
}

// reconcileLabelReviewCommands polls the label-event journal for each
// label-review entry in state.PendingReactions and dispatches at most one
// read-only review session per confirmed labeling gesture. Called from
// [ReconcileRunningIssues] after the sibling reaction passes. Skipped
// entirely when the SCM adapter is nil or the label-review feature is not
// configured.
//
// The metrics parameter is accepted for signature parity with the sibling
// reconcile passes invoked positionally from [ReconcileRunningIssues]; this
// pass records no counter in this slice.
func reconcileLabelReviewCommands(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, _ domain.Metrics) {
	if params.SCMAdapter == nil || !params.LabelReviewReactionConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	pollInterval := max(time.Duration(params.LabelReviewConfig.PollIntervalMS)*time.Millisecond, labelReviewPendingBackoffBase)

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindLabelReview {
			continue
		}
		delete(state.PendingReactions, key)

		data, ok := pending.KindData.(*LabelReviewReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for label-review reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

		// A human gesture stays actionable regardless of age, so this kind
		// has no TTL and no drop-on-age branch.
		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		// The dispatched flag is not consulted for this kind. The read error
		// is: at-most-once rests solely on the mark, and this kind has no
		// turn cap to bound a duplicate, so when the mark cannot be read the
		// pass backs off rather than risk re-dispatching an already
		// acknowledged command with storedMark treated as empty.
		storedMark, _, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelReview)
		if fpErr != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("label-review fingerprint read failed, backing off without dispatch",
				slog.Int("pr_number", data.PRNumber),
				slog.Any("error", fpErr),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			continue
		}

		events, err := params.SCMAdapter.ListLabelEvents(ctx, data.PRNumber, data.Owner, data.Repo)
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("label-review journal fetch failed, retrying with backoff",
				slog.Int("pr_number", data.PRNumber),
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			continue
		}

		// Events strictly newer than the stored position. An empty
		// storedMark sorts before every real mark.
		var newEvents []domain.LabelEvent
		for _, e := range events {
			if labelReviewMark(e) > storedMark {
				newEvents = append(newEvents, e)
			}
		}
		if len(newEvents) == 0 {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		newestMark := labelReviewMark(latestLabelEvent(newEvents))

		// Burst collapse: all matching labeled events in the batch collapse
		// to at most one command. A self-authored-event filter is not run in
		// this slice because no orchestrator identity is wired; the structural
		// guarantee that Sortie never applies command labels makes its absence
		// a no-op.
		var matches []domain.LabelEvent
		for _, e := range newEvents {
			if e.Added && strings.EqualFold(e.Label, params.LabelReviewConfig.ReviewLabel) {
				matches = append(matches, e)
			}
		}

		labelPresentNow := labelPresent(events, params.LabelReviewConfig.ReviewLabel)
		commandConfirmed := len(matches) > 0 && labelPresentNow

		running := state.Running[pending.IssueID] != nil
		existingRetry, retryQueued := state.RetryAttempts[pending.IssueID]
		alreadyQueuedOrRunning := running || (retryQueued && existingRetry.ReactionKind == ReactionKindLabelReview)

		if commandConfirmed && !alreadyQueuedOrRunning {
			// Advance the mark BEFORE scheduling: at-most-once rests on the
			// persisted mark, so a crash in the window between the two loses
			// the command rather than duplicating it. The upsert is
			// best-effort: a write failure is logged and dispatch still
			// proceeds, degrading the durable guarantee to best-effort for
			// this tick, matching the sibling reactions.
			if upErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelReview, newestMark); upErr != nil {
				entryLog.Warn("failed to upsert label-review reaction fingerprint",
					slog.Any("error", upErr),
				)
			}

			newestMatch := latestLabelEvent(matches)
			data.HighWaterMark = newestMark
			data.LastActor = newestMatch.Actor

			CancelRetry(state, pending.IssueID)
			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:    pending.IssueID,
				Identifier: pending.Identifier,
				DisplayID:  pending.DisplayID,
				Attempt:    pending.Attempt,
				DelayMS:    continuationDelayMS,
				ContinuationContext: map[string]any{
					"label_review": buildLabelReviewMap(data, newestMatch.Actor, newestMatch.At),
				},
				ReactionKind: ReactionKindLabelReview,
				AgentKind:    pending.AgentKind,
				RuleName:     pending.RuleName,
				TemplateID:   pending.TemplateID,
			}, params.OnRetryFire)

			// Best-effort acknowledgment: the label's disappearance tells the
			// operator the command was accepted. A failure does not affect
			// dedup (the mark already advanced) and does not block re-enqueue.
			if rmErr := params.SCMAdapter.RemoveLabel(ctx, data.PRNumber, data.Owner, data.Repo, params.LabelReviewConfig.ReviewLabel); rmErr != nil {
				entryLog.Warn("label-review label removal failed",
					slog.Int("pr_number", data.PRNumber),
					slog.String("label", params.LabelReviewConfig.ReviewLabel),
					slog.Any("error", rmErr),
				)
			}

			entryLog.Info("label-review dispatched",
				slog.Int("pr_number", data.PRNumber),
				slog.String("actor", data.LastActor),
			)

			// Re-enqueue so the detection loop survives the dispatch,
			// decoupled from the scratch workspace the session runs in.
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// No dispatch (retraction, foreign labels, unlabeled-only, or
		// collapse into an outstanding command): advance the mark past the
		// examined events and re-enqueue at the next poll.
		if upErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelReview, newestMark); upErr != nil {
			entryLog.Warn("failed to upsert label-review reaction fingerprint",
				slog.Any("error", upErr),
			)
		}
		data.HighWaterMark = newestMark
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
	}
}

// latestLabelEvent returns the event with the greatest (At, ID) position.
// The input must be non-empty.
func latestLabelEvent(events []domain.LabelEvent) domain.LabelEvent {
	latest := events[0]
	latestMark := labelReviewMark(latest)
	for _, e := range events[1:] {
		if m := labelReviewMark(e); m > latestMark {
			latest = e
			latestMark = m
		}
	}
	return latest
}

// labelPresent reports whether the given label is present on the PR at
// detection time. The events slice is oldest-first, so the last matching
// entry is the most recent: the label is present when that entry is a
// labeled event, and absent when no entry matches or the last is unlabeled.
func labelPresent(events []domain.LabelEvent, label string) bool {
	present := false
	for _, e := range events {
		if strings.EqualFold(e.Label, label) {
			present = e.Added
		}
	}
	return present
}

// buildLabelReviewMap builds the continuation template context injected
// under the key "label_review". Every documented field is always present so
// a template referencing any sub-field renders under missingkey=error.
func buildLabelReviewMap(data *LabelReviewReactionData, actor string, requestedAt time.Time) map[string]any {
	return map[string]any{
		"pr_number":    data.PRNumber,
		"owner":        data.Owner,
		"repo":         data.Repo,
		"actor":        actor,
		"requested_at": requestedAt.UTC().Format(time.RFC3339),
	}
}
