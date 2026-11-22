package orchestrator

import (
	"context"
	"log/slog"
)

// pendingReactionRecoveryWarnEvery is how many consecutive failed retries of
// pending reaction recovery pass between two warnings; the failures between
// them log at debug.
const pendingReactionRecoveryWarnEvery = 20

// PendingReactionRecoveryFunc runs pending reaction recovery against state,
// as [RecoverPendingReactions] does at startup. It is called on the event
// loop.
type PendingReactionRecoveryFunc func(ctx context.Context, state *State) (PendingReactionRecoveryResult, error)

// retryPendingReactionRecovery runs the recovery a failed startup left
// pending, once per tick until it succeeds. Startup recovery rebuilds the
// watches of every pull request sitting in the handoff state, and nothing
// else does for an issue no worker exit reaches again, so one failed tracker
// read at startup would otherwise leave those pull requests unwatched for
// the process lifetime. A failure warns on the first retry and on every
// [pendingReactionRecoveryWarnEvery]th after it, and logs at debug between.
// Must run on the event loop, before the reconcile pass.
func (o *Orchestrator) retryPendingReactionRecovery(ctx context.Context) {
	if o.pendingReactionRecovery == nil || o.once != nil {
		return
	}
	result, err := o.pendingReactionRecovery(ctx, o.state)
	if err != nil {
		if canceled(ctx, err) {
			return
		}
		o.pendingReactionRecoveryFailures++
		level := slog.LevelDebug
		if o.pendingReactionRecoveryFailures%pendingReactionRecoveryWarnEvery == 1 {
			level = slog.LevelWarn
		}
		o.logger.LogAttrs(ctx, level, "pending reaction recovery retry failed",
			slog.Int("failed_retries", o.pendingReactionRecoveryFailures),
			slog.Any("error", err),
		)
		return
	}
	o.pendingReactionRecovery = nil
	o.logger.LogAttrs(ctx, slog.LevelInfo, "pending reaction recovery completed",
		slog.Bool("retried", true),
		slog.Int("failed_retries", o.pendingReactionRecoveryFailures),
		slog.Int("candidates", result.Candidates),
		slog.Int("cap_skipped", result.CapSkipped),
		slog.Int("state_checked", result.StateChecked),
		slog.Int("review_recovered", result.ReviewRecovered),
		slog.Int("ci_recovered", result.CIRecovered),
		slog.Int("auto_merge_recovered", result.AutoMergeRecovered),
		slog.Int("bot_review_recovered", result.BotReviewRecovered),
		slog.Int("merge_conflict_recovered", result.MergeConflictRecovered),
		slog.Int("label_review_recovered", result.LabelReviewRecovered),
		slog.Int("label_fix_recovered", result.LabelFixRecovered),
		slog.Int("merge_completion_recovered", result.MergeCompletionRecovered),
		slog.Int("stale_skipped", result.StaleSkipped),
		slog.Int("skipped", result.Skipped),
	)
}
