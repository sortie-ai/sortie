package orchestrator

import (
	"context"
	"log/slog"
	"time"
)

// pendingReactionRecoveryWarnEvery is how many consecutive failed retries of
// pending reaction recovery pass between two warnings; the failures between
// them log at debug.
const pendingReactionRecoveryWarnEvery = 20

// pendingReactionRecoveryTimeout bounds one retry of pending reaction
// recovery, so a tracker that hangs cannot stall the tick that runs it.
const pendingReactionRecoveryTimeout = time.Minute

// pendingReactionRecoveryEveryTick is how many consecutive failed retries
// run on every tick before the retries back off.
const pendingReactionRecoveryEveryTick = 5

// pendingReactionRecoveryBackoffBase and pendingReactionRecoveryBackoffCap
// bound the wait between retries once they back off: the base after the
// first failure past [pendingReactionRecoveryEveryTick], doubling after each
// later one up to the cap.
const (
	pendingReactionRecoveryBackoffBase = 30 * time.Second
	pendingReactionRecoveryBackoffCap  = 5 * time.Minute
)

// pendingReactionRecoveryDelay is the wait before the retry that follows
// failures consecutive failed retries: none for the first
// [pendingReactionRecoveryEveryTick], then doubling from the base up to the
// cap.
func pendingReactionRecoveryDelay(failures int) time.Duration {
	if failures <= pendingReactionRecoveryEveryTick {
		return 0
	}
	delay := pendingReactionRecoveryBackoffBase
	for range failures - pendingReactionRecoveryEveryTick - 1 {
		delay *= 2
		if delay >= pendingReactionRecoveryBackoffCap {
			return pendingReactionRecoveryBackoffCap
		}
	}
	return delay
}

// PendingReactionRecoveryFunc runs pending reaction recovery against state,
// as [RecoverPendingReactions] does at startup. It is called on the event
// loop.
type PendingReactionRecoveryFunc func(ctx context.Context, state *State) (PendingReactionRecoveryResult, error)

// retryPendingReactionRecovery runs the recovery a failed startup left
// pending until it succeeds: on each tick for the first few failures, then
// backing off up to five minutes, each run bounded by
// [pendingReactionRecoveryTimeout]. Startup recovery rebuilds the
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
	now := o.recoveryClock()
	if now.Before(o.pendingReactionRecoveryNextAt) {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, pendingReactionRecoveryTimeout)
	result, err := o.pendingReactionRecovery(runCtx, o.state)
	cancel()
	if err != nil {
		if canceled(ctx, err) {
			return
		}
		o.pendingReactionRecoveryFailures++
		o.pendingReactionRecoveryNextAt = now.Add(pendingReactionRecoveryDelay(o.pendingReactionRecoveryFailures))
		level := slog.LevelDebug
		if o.pendingReactionRecoveryFailures%pendingReactionRecoveryWarnEvery == 1 {
			level = slog.LevelWarn
		}
		o.logger.LogAttrs(ctx, level, "pending reaction recovery retry failed",
			slog.Int("failed_retries", o.pendingReactionRecoveryFailures),
			slog.Duration("next_retry_in", pendingReactionRecoveryDelay(o.pendingReactionRecoveryFailures)),
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

// recoveryClock reads the clock the recovery retry backs off against.
func (o *Orchestrator) recoveryClock() time.Time {
	if o.pendingReactionRecoveryNow != nil {
		return o.pendingReactionRecoveryNow()
	}
	return time.Now()
}
