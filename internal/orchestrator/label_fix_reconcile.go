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

// reconcileLabelFixCommands polls the label-event journal for each
// label-fix entry in state.PendingReactions and dispatches at most one
// full fix session per confirmed labeling gesture. Called from
// [ReconcileRunningIssues] after [reconcileLabelReviewCommands]. Skipped
// entirely when the SCM adapter is nil or the label-fix feature is not
// configured.
//
// The metrics parameter is accepted for signature parity with the sibling
// reconcile passes invoked positionally from [ReconcileRunningIssues]; this
// pass records no counter in this slice.
func reconcileLabelFixCommands(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, _ domain.Metrics) {
	if params.SCMAdapter == nil || !params.LabelFixReactionConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	pollInterval := max(time.Duration(params.LabelFixConfig.PollIntervalMS)*time.Millisecond, labelReviewPendingBackoffBase)

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindLabelFix {
			continue
		}
		delete(state.PendingReactions, key)

		data, ok := pending.KindData.(*LabelFixReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for label-fix reaction",
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
		storedMark, _, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelFix)
		if fpErr != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("label-fix fingerprint read failed, backing off without dispatch",
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
			entryLog.Warn("label-fix journal fetch failed, retrying with backoff",
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
			if e.Added && strings.EqualFold(e.Label, params.LabelFixConfig.FixLabel) {
				matches = append(matches, e)
			}
		}

		labelPresentNow := labelPresent(events, params.LabelFixConfig.FixLabel)
		commandConfirmed := len(matches) > 0 && labelPresentNow

		running := state.Running[pending.IssueID] != nil
		incumbent := retrySlotIncumbent(state, pending.IssueID)

		// A foreign incumbent defers before the same-kind collapse check
		// runs: evaluating the collapse arm first would advance the mark
		// past the labeling event and swallow the gesture.
		if commandConfirmed && incumbent != nil && incumbent.ReactionKind != ReactionKindLabelFix {
			logRetrySlotDeferral(entryLog, ReactionKindLabelFix, incumbent)
			pending.CreatedAt = now
			state.PendingReactions[key] = pending
			continue
		}

		sameKindQueuedOrRunning := running || (incumbent != nil && incumbent.ReactionKind == ReactionKindLabelFix)

		if commandConfirmed && !sameKindQueuedOrRunning {
			// Advance the mark BEFORE scheduling: at-most-once rests on the
			// persisted mark, so a crash in the window between the two loses
			// the command rather than duplicating it. The upsert is
			// best-effort: a write failure is logged and dispatch still
			// proceeds, degrading the durable guarantee to best-effort for
			// this tick, matching the sibling reactions.
			if upErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelFix, newestMark); upErr != nil {
				entryLog.Warn("failed to upsert label-fix reaction fingerprint",
					slog.Any("error", upErr),
				)
			}

			newestMatch := latestLabelEvent(matches)
			data.HighWaterMark = newestMark
			data.LastActor = newestMatch.Actor

			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:    pending.IssueID,
				Identifier: pending.Identifier,
				DisplayID:  pending.DisplayID,
				Attempt:    pending.Attempt,
				DelayMS:    continuationDelayMS,
				ContinuationContext: map[string]any{
					"label_fix": buildLabelFixMap(data, newestMatch.Actor, newestMatch.At),
				},
				ReactionKind: ReactionKindLabelFix,
				AgentKind:    pending.AgentKind,
				RuleName:     pending.RuleName,
				TemplateID:   pending.TemplateID,
				Logger:       entryLog,
			}, params.OnRetryFire)

			// Best-effort acknowledgment: the label's disappearance tells the
			// operator the command was accepted. A failure does not affect
			// dedup (the mark already advanced) and does not block re-enqueue.
			if rmErr := params.SCMAdapter.RemoveLabel(ctx, data.PRNumber, data.Owner, data.Repo, params.LabelFixConfig.FixLabel); rmErr != nil {
				entryLog.Warn("label-fix label removal failed",
					slog.Int("pr_number", data.PRNumber),
					slog.String("label", params.LabelFixConfig.FixLabel),
					slog.Any("error", rmErr),
				)
			}

			entryLog.Info("label-fix dispatched",
				slog.Int("pr_number", data.PRNumber),
				slog.String("actor", data.LastActor),
			)

			// Re-enqueue so the detection loop survives the dispatch,
			// decoupled from the per-issue workspace the session runs in.
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// No dispatch (retraction, foreign labels, unlabeled-only, or
		// collapse into an outstanding command): advance the mark past the
		// examined events and re-enqueue at the next poll.
		if upErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindLabelFix, newestMark); upErr != nil {
			entryLog.Warn("failed to upsert label-fix reaction fingerprint",
				slog.Any("error", upErr),
			)
		}
		data.HighWaterMark = newestMark
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
	}
}

// buildLabelFixMap builds the continuation template context injected under
// the key "label_fix". It mirrors [buildLabelReviewMap] and adds branch, the
// PR head branch the fix session checks out and pushes to. Every documented
// field is always present so a template referencing any sub-field renders
// under missingkey=error.
func buildLabelFixMap(data *LabelFixReactionData, actor string, requestedAt time.Time) map[string]any {
	return map[string]any{
		"pr_number":    data.PRNumber,
		"owner":        data.Owner,
		"repo":         data.Repo,
		"branch":       data.Branch,
		"actor":        actor,
		"requested_at": requestedAt.UTC().Format(time.RFC3339),
	}
}
