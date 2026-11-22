package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// recoveryRetryOrchestrator builds an orchestrator whose ticks reach the
// reconcile pass, with recovery as its pending reaction recovery.
func recoveryRetryOrchestrator(t *testing.T, recovery PendingReactionRecoveryFunc, logs *lockedBuf, once bool) *Orchestrator {
	t.Helper()
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(context.Context) ([]domain.Issue, error) {
			return nil, nil
		},
	}
	wm := &stubWorkflowManager{config: lifecycleConfig(t.TempDir())}
	regs := passingPreflightRegistries()
	return NewOrchestrator(OrchestratorParams{
		State:           NewState(60000, 2, 0, nil, AgentTotals{}),
		Logger:          slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
		Once:                    once,
		PendingReactionRecovery: recovery,
	})
}

// recoveryRetryRecords returns the level of each log record with msg.
func recoveryRetryRecords(t *testing.T, logs *lockedBuf, msg string) []string {
	t.Helper()
	var levels []string
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", line, err)
		}
		if rec.Msg == msg {
			levels = append(levels, rec.Level)
		}
	}
	return levels
}

// TestRetryPendingReactionRecovery covers a startup recovery whose tracker
// read failed: each tick runs it again until it succeeds, and then never
// again, so the pull requests waiting in the handoff state get their watches
// back without a restart.
func TestRetryPendingReactionRecovery(t *testing.T) {
	t.Parallel()

	const failures = 2
	var calls int
	key := ReactionKey("REC-1", ReactionKindMergeConflict)
	recovery := func(_ context.Context, state *State) (PendingReactionRecoveryResult, error) {
		calls++
		if calls <= failures {
			return PendingReactionRecoveryResult{}, errors.New("tracker unavailable")
		}
		state.PendingReactions[key] = &PendingReaction{IssueID: "REC-1", Kind: ReactionKindMergeConflict}
		return PendingReactionRecoveryResult{MergeConflictRecovered: 1}, nil
	}
	logs := &lockedBuf{}
	o := recoveryRetryOrchestrator(t, recovery, logs, false)

	for range failures + 3 {
		o.handleTick(context.Background())
	}

	if calls != failures+1 {
		t.Errorf("recovery calls = %d, want %d (until the first success, then none)", calls, failures+1)
	}
	if _, ok := o.state.PendingReactions[key]; !ok {
		t.Errorf("PendingReactions[%s] absent, want the recovered watch", key)
	}
	if got := recoveryRetryRecords(t, logs, "pending reaction recovery retry failed"); strings.Join(got, ",") != "WARN,DEBUG" {
		t.Errorf("failed retry records = %v, want [WARN DEBUG] (a warning, then debug until the next warning)", got)
	}
	if got := recoveryRetryRecords(t, logs, "pending reaction recovery completed"); len(got) != 1 || got[0] != "INFO" {
		t.Errorf("completed records = %v, want one INFO", got)
	}
}

func TestRetryPendingReactionRecovery_WarnsEveryNthFailure(t *testing.T) {
	t.Parallel()

	recovery := func(context.Context, *State) (PendingReactionRecoveryResult, error) {
		return PendingReactionRecoveryResult{}, errors.New("tracker unavailable")
	}
	logs := &lockedBuf{}
	o := recoveryRetryOrchestrator(t, recovery, logs, false)
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	o.pendingReactionRecoveryNow = func() time.Time { return now }

	for range pendingReactionRecoveryWarnEvery + 1 {
		o.retryPendingReactionRecovery(context.Background())
		now = now.Add(pendingReactionRecoveryBackoffCap)
	}

	levels := recoveryRetryRecords(t, logs, "pending reaction recovery retry failed")
	var warns int
	for _, level := range levels {
		if level == "WARN" {
			warns++
		}
	}
	if len(levels) != pendingReactionRecoveryWarnEvery+1 || warns != 2 {
		t.Errorf("failed retry records = %d with %d warnings, want %d with 2", len(levels), warns, pendingReactionRecoveryWarnEvery+1)
	}
	if o.pendingReactionRecovery == nil {
		t.Error("pending recovery cleared after failures only, want kept until it succeeds")
	}
}

func TestRetryPendingReactionRecovery_Skipped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		once bool
		ctx  func() context.Context
	}{
		{name: "once mode watches no pull request", once: true, ctx: context.Background},
		{name: "a cancelled context is no failure", ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var calls int
			recovery := func(ctx context.Context, _ *State) (PendingReactionRecoveryResult, error) {
				calls++
				return PendingReactionRecoveryResult{}, ctx.Err()
			}
			logs := &lockedBuf{}
			o := recoveryRetryOrchestrator(t, recovery, logs, tt.once)

			o.retryPendingReactionRecovery(tt.ctx())

			if tt.once && calls != 0 {
				t.Errorf("recovery calls = %d in once mode, want 0", calls)
			}
			if o.pendingReactionRecoveryFailures != 0 {
				t.Errorf("failed retries = %d, want 0", o.pendingReactionRecoveryFailures)
			}
			if o.pendingReactionRecovery == nil {
				t.Error("pending recovery cleared, want kept")
			}
		})
	}
}

func TestPendingReactionRecoveryDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 0},
		{failures: pendingReactionRecoveryEveryTick, want: 0},
		{failures: pendingReactionRecoveryEveryTick + 1, want: 30 * time.Second},
		{failures: pendingReactionRecoveryEveryTick + 2, want: time.Minute},
		{failures: pendingReactionRecoveryEveryTick + 4, want: 4 * time.Minute},
		{failures: pendingReactionRecoveryEveryTick + 5, want: 5 * time.Minute},
		{failures: 100, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		if got := pendingReactionRecoveryDelay(tt.failures); got != tt.want {
			t.Errorf("pendingReactionRecoveryDelay(%d) = %v, want %v", tt.failures, got, tt.want)
		}
	}
}

// TestRetryPendingReactionRecovery_BacksOffAndBoundsEachRun covers the
// retries past the first few failures: a tick inside the backoff runs
// nothing, the first tick past it runs one, and every run gets a deadline.
func TestRetryPendingReactionRecovery_BacksOffAndBoundsEachRun(t *testing.T) {
	t.Parallel()

	var calls int
	recovery := func(ctx context.Context, _ *State) (PendingReactionRecoveryResult, error) {
		calls++
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > pendingReactionRecoveryTimeout {
			t.Errorf("recovery run deadline = %v, %v; want one within %v", deadline, ok, pendingReactionRecoveryTimeout)
		}
		return PendingReactionRecoveryResult{}, errors.New("tracker unavailable")
	}
	o := recoveryRetryOrchestrator(t, recovery, &lockedBuf{}, false)
	now := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	o.pendingReactionRecoveryNow = func() time.Time { return now }

	// The first failures retry on every tick.
	for range pendingReactionRecoveryEveryTick + 1 {
		o.retryPendingReactionRecovery(context.Background())
	}
	if calls != pendingReactionRecoveryEveryTick+1 {
		t.Fatalf("recovery calls over consecutive ticks = %d, want %d", calls, pendingReactionRecoveryEveryTick+1)
	}

	// Then the next waits out its backoff.
	now = now.Add(pendingReactionRecoveryBackoffBase - time.Second)
	o.retryPendingReactionRecovery(context.Background())
	if calls != pendingReactionRecoveryEveryTick+1 {
		t.Errorf("recovery calls inside the backoff = %d, want %d", calls, pendingReactionRecoveryEveryTick+1)
	}
	now = now.Add(time.Second)
	o.retryPendingReactionRecovery(context.Background())
	if calls != pendingReactionRecoveryEveryTick+2 {
		t.Errorf("recovery calls once the backoff passed = %d, want %d", calls, pendingReactionRecoveryEveryTick+2)
	}
}
