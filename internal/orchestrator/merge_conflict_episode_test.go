package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage/sqlite"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// rebasePath drives one issue's merge-conflict reaction through the real
// lanes against a real store: the reconcile pass that schedules a rebase,
// the retry timer that starts its worker, and the exit that ends it.
type rebasePath struct {
	t       *testing.T
	issueID string
	store   *sqlite.Store
	state   *State
	head    string
	now     time.Time
	tracker *mockTrackerAdapter
	metrics *mergeConflictMetricsSpy
	config  MergeConflictReactionConfig
}

const rebasePathHandoff = "queue-pr"

func newRebasePath(t *testing.T, maxRetries int) *rebasePath {
	t.Helper()
	p := &rebasePath{
		t:       t,
		issueID: "MC-PATH",
		store:   openParkStore(t),
		head:    "head-1",
		now:     mcBaseTime,
		metrics: newMergeConflictMetricsSpy(),
	}
	p.tracker = &mockTrackerAdapter{
		fetchIssueByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
			return domain.Issue{ID: id, Identifier: id + "-ident", Title: "rebase me", State: rebasePathHandoff}, nil
		},
	}
	p.config = defaultMergeConflictConfig()
	p.config.MaxRetries = maxRetries
	p.state = stateWithMergeConflict(t, p.issueID, 77)
	return p
}

// restart replaces the in-memory state as a new process would hold it after
// startup: the stale marks released and the watch recovered, nothing else.
func (p *rebasePath) restart() {
	p.t.Helper()
	ReleaseStaleMergeConflictMarks(context.Background(), p.store, false, discardLogger())
	p.state = stateWithMergeConflict(p.t, p.issueID, 77)
}

// pass runs one merge-conflict reconcile pass with the pull request
// conflicting at p.head, and reports whether it scheduled a rebase.
func (p *rebasePath) pass() bool {
	p.t.Helper()
	p.now = p.now.Add(2 * time.Minute)
	params := mergeConflictParams(p.store, &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus(p.head, "main"), nil
	}}, p.tracker)
	params.MergeConflictConfig = p.config
	params.NowFunc = func() time.Time { return p.now }
	reconcileMergeConflicts(p.state, params, discardLogger(), context.Background(), p.metrics)
	p.state.TrackerOpsWg.Wait()
	_, scheduled := p.state.RetryAttempts[p.issueID]
	return scheduled
}

// cleanPass runs one pass that reads the pull request clean.
func (p *rebasePath) cleanPass() {
	p.t.Helper()
	p.now = p.now.Add(2 * time.Minute)
	params := mergeConflictParams(p.store, &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: p.head, BaseBranch: "main"}, nil
	}}, p.tracker)
	params.MergeConflictConfig = p.config
	params.NowFunc = func() time.Time { return p.now }
	reconcileMergeConflicts(p.state, params, discardLogger(), context.Background(), p.metrics)
	p.watch()
}

// watch seeds the merge-conflict watch again, as a later exit or recovery
// would; a no-op while one is held.
func (p *rebasePath) watch() {
	key := ReactionKey(p.issueID, ReactionKindMergeConflict)
	if _, ok := p.state.PendingReactions[key]; !ok {
		p.state.PendingReactions[key] = newMergeConflictPending(p.issueID, 77)
	}
}

// drop fires the scheduled rebase's retry timer with the issue paused past
// the maximum dwell, which drops the retry without starting it.
func (p *rebasePath) drop() {
	p.t.Helper()
	retry := p.state.RetryAttempts[p.issueID]
	if retry == nil {
		p.t.Fatal("no rebase scheduled to drop")
	}
	retry.TimerHandle.Stop()
	retry.scheduledAt = time.Time{}
	retry.pausedSinceMS = time.Now().Add(-2 * pausedRetryMaxDwell).UnixMilli()
	params := p.retryParams()
	params.TrackerAdapter = &mockTrackerAdapter{fetchIssueByIDFn: func(_ context.Context, id string) (domain.Issue, error) {
		return domain.Issue{ID: id, Identifier: id + "-ident", Title: "rebase me", State: "Backlog"}, nil
	}}
	HandleRetryTimer(p.state, p.issueID, params)
	if p.state.RetryAttempts[p.issueID] != nil || p.state.Running[p.issueID] != nil {
		p.t.Fatal("rebase retry not dropped")
	}
	delete(p.state.Claimed, p.issueID)
	p.watch()
}

func (p *rebasePath) retryParams() HandleRetryTimerParams {
	params := defaultRetryParams(p.t, &mockRetryStore{}, nil)
	params.Store = p.store
	params.TrackerAdapter = p.tracker
	params.Router = nil
	params.ActiveStates = []string{"queue"}
	params.TerminalStates = []string{"done"}
	params.HandoffState = rebasePathHandoff
	return params
}

// start fires the scheduled rebase's retry timer, which starts its worker.
func (p *rebasePath) start() {
	p.t.Helper()
	retry := p.state.RetryAttempts[p.issueID]
	if retry == nil {
		p.t.Fatal("no rebase scheduled to start")
	}
	retry.TimerHandle.Stop()
	retry.scheduledAt = time.Time{}
	HandleRetryTimer(p.state, p.issueID, p.retryParams())
	if p.state.Running[p.issueID] == nil {
		p.t.Fatal("rebase worker not started by the retry timer")
	}
}

// exit ends the running rebase's worker with result's disposition, then
// seeds the watch again as the exit of a reaction continuation does.
func (p *rebasePath) exit(result WorkerResult) {
	p.t.Helper()
	result.IssueID = p.issueID
	result.Identifier = p.issueID + "-ident"
	result.AgentAdapter = "mock"
	params := defaultExitParams(p.t, nil)
	params.Store = p.store
	params.TrackerAdapter = p.tracker
	params.HandoffState = rebasePathHandoff
	params.ActiveStates = []string{"queue"}
	params.TerminalStates = []string{"done"}
	params.NowFunc = func() time.Time { return p.now }
	HandleWorkerExit(p.state, result, params)
	if retry := p.state.RetryAttempts[p.issueID]; retry != nil {
		retry.TimerHandle.Stop()
	}
	p.watch()
}

func (p *rebasePath) escalated() bool {
	pending := p.state.PendingReactions[ReactionKey(p.issueID, ReactionKindMergeConflict)]
	return pending != nil && pending.escalationStands()
}

// TestMergeConflictEpisode_UnresolvedRebaseEscalatesAtCap covers rebases that
// exit without replacing the conflicting head: each one lifts the head's
// mark, so the next pass counts another attempt and the episode escalates at
// the cap rather than skipping the head as already dispatched.
func TestMergeConflictEpisode_UnresolvedRebaseEscalatesAtCap(t *testing.T) {
	t.Parallel()

	const maxRetries = 2
	p := newRebasePath(t, maxRetries)
	for run := 1; run <= maxRetries; run++ {
		if !p.pass() {
			t.Fatalf("pass %d scheduled no rebase, want one (attempt %d of %d)", run, run, maxRetries)
		}
		p.start()
		p.exit(WorkerResult{ExitKind: WorkerExitNormal})
	}

	if p.pass() {
		t.Fatal("pass after the last allowed rebase scheduled another, want an escalation")
	}
	if !p.escalated() {
		t.Error("escalation does not stand after max_retries unresolved rebases")
	}
	if got := p.metrics.checks["dispatched"]; got != maxRetries {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want %d`, got, maxRetries)
	}
}

// TestMergeConflictEpisode_SuccessfulRebasesNeverEscalate covers a pull
// request whose base keeps moving: every rebase exits normally having pushed
// a new head that a pass reads clean, and the base then moves again. Each
// conflict is a new episode, so the budget never runs out.
func TestMergeConflictEpisode_SuccessfulRebasesNeverEscalate(t *testing.T) {
	t.Parallel()

	p := newRebasePath(t, 1)
	const rebases = 6
	for run := 1; run <= rebases; run++ {
		if !p.pass() {
			t.Fatalf("pass %d scheduled no rebase for a new conflict, want one", run)
		}
		p.start()
		p.exit(WorkerResult{ExitKind: WorkerExitNormal})
		p.head = fmt.Sprintf("head-%d", run+1)
		p.cleanPass()
	}

	if p.escalated() {
		t.Error("escalation stands after rebases that each resolved their conflict")
	}
	if got := p.metrics.checks["dispatched"]; got != rebases {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want %d`, got, rebases)
	}
}

// TestMergeConflictEpisode_RebasedHeadsThatKeepConflictingEscalate covers
// both shapes of a head that conflicts again right after a rebase that
// pushed and exited normally, with no clean read between: each starts a new
// episode, up to three times the budget, and the next escalates. A rebase
// that cannot resolve the conflict and keeps pushing ends there rather than
// looping for good.
func TestMergeConflictEpisode_RebasedHeadsThatKeepConflictingEscalate(t *testing.T) {
	t.Parallel()

	for _, maxRetries := range []int{1, 2} {
		t.Run(fmt.Sprintf("max_retries %d", maxRetries), func(t *testing.T) {
			t.Parallel()

			p := newRebasePath(t, maxRetries)
			allowed := 1 + mergeConflictEpisodeCap(maxRetries)
			for run := 1; run <= allowed; run++ {
				if !p.pass() {
					t.Fatalf("pass %d scheduled no rebase, want one (%d allowed)", run, allowed)
				}
				p.start()
				p.exit(WorkerResult{ExitKind: WorkerExitNormal})
				p.head = fmt.Sprintf("head-%d", run+1)
			}
			if p.pass() {
				t.Fatal("pass after the episode cap scheduled another rebase, want an escalation")
			}
			if !p.escalated() {
				t.Error("escalation does not stand after the episode cap")
			}
			if got := p.metrics.checks["dispatched"]; got != allowed {
				t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want %d`, got, allowed)
			}
		})
	}
}

// TestMergeConflictEpisode_NoRunSpendsNoAttempt covers rebases that never
// run a real attempt: lost to a restart before they start, dropped by the
// retry timer, cut off by a shutdown, or refused by a usage limit across
// restarts. None of them escalates, however many times it happens.
func TestMergeConflictEpisode_NoRunSpendsNoAttempt(t *testing.T) {
	t.Parallel()

	usageLimited := &domain.AgentError{Kind: domain.ErrUsageLimit, Message: "usage limit reached"}
	tests := []struct {
		name  string
		cycle func(p *rebasePath)
	}{
		{name: "restart between schedule and start", cycle: func(p *rebasePath) { p.restart() }},
		{name: "retry timer drops the rebase", cycle: func(p *rebasePath) { p.drop() }},
		{name: "shutdown cancels the rebase", cycle: func(p *rebasePath) {
			p.start()
			p.exit(WorkerResult{ExitKind: WorkerExitCancelled, CancelCause: errCancelShutdown})
		}},
		{name: "usage limit refuses the rebase, then a restart", cycle: func(p *rebasePath) {
			p.start()
			p.exit(WorkerResult{ExitKind: WorkerExitError, Error: usageLimited})
			p.restart()
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newRebasePath(t, 1)
			for cycle := 1; cycle <= 4; cycle++ {
				if !p.pass() {
					t.Fatalf("cycle %d: pass scheduled no rebase, want one (no attempt was spent)", cycle)
				}
				tt.cycle(p)
			}
			if p.escalated() {
				t.Error("escalation stands with no rebase that ran")
			}
			episode, err := p.store.GetReactionEpisode(context.Background(), p.issueID, ReactionKindMergeConflict)
			if err != nil || episode.Attempts != 0 {
				t.Errorf("persisted episode = %+v, %v; want 0 attempts", episode, err)
			}
		})
	}
}

// TestMergeConflictEpisode_GaveUpEscalatesAtOnce covers a rebase whose agent
// reports it needs a person, or that nothing was needed, and leaves the head
// in place: the next pass escalates without running it again. One that
// pushed first is watched as any other.
func TestMergeConflictEpisode_GaveUpEscalatesAtOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reason       workspace.StatusSignal
		pushed       bool
		wantEscalate bool
	}{
		{name: "needs review on the same head", reason: workspace.StatusNeedsHumanReview, wantEscalate: true},
		{name: "no change needed on the same head", reason: workspace.StatusNoChangeNeeded, wantEscalate: true},
		{name: "needs review after pushing", reason: workspace.StatusNeedsHumanReview, pushed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newRebasePath(t, 3)
			if !p.pass() {
				t.Fatal("first pass scheduled no rebase")
			}
			p.start()
			p.exit(WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(tt.reason)})
			if tt.pushed {
				p.head = "head-pushed"
			}

			scheduled := p.pass()
			if p.escalated() != tt.wantEscalate {
				t.Errorf("escalation stands = %v, want %v", p.escalated(), tt.wantEscalate)
			}
			if scheduled == tt.wantEscalate {
				t.Errorf("second pass scheduled a rebase = %v, want %v", scheduled, !tt.wantEscalate)
			}
		})
	}
}

// TestMergeConflictEpisode_RestartKeepsBudget covers a process stopped while
// a rebase worker ran, so its exit was never handled. The next process
// releases the stale mark at startup, so the head is not skipped for good,
// and resumes the episode's attempt counter from the store, so the restart
// grants no fresh budget.
func TestMergeConflictEpisode_RestartKeepsBudget(t *testing.T) {
	t.Parallel()

	p := newRebasePath(t, 1)
	if !p.pass() {
		t.Fatal("first pass scheduled no rebase")
	}
	p.start()

	episode, err := p.store.GetReactionEpisode(context.Background(), p.issueID, ReactionKindMergeConflict)
	if err != nil || !episode.Dispatched || episode.Attempts != 1 {
		t.Fatalf("episode before the stop = %+v, %v; want dispatched with 1 attempt", episode, err)
	}

	p.restart()
	episode, err = p.store.GetReactionEpisode(context.Background(), p.issueID, ReactionKindMergeConflict)
	if err != nil || episode.Dispatched {
		t.Fatalf("episode after startup = %+v, %v; want the stale mark released", episode, err)
	}

	if p.pass() {
		t.Fatal("pass after the restart scheduled another rebase, want the spent budget to escalate")
	}
	if !p.escalated() {
		t.Error("escalation does not stand after the restart")
	}
}

func TestMergeConflictExitOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result WorkerResult
		want   string
	}{
		{name: "normal", result: WorkerResult{ExitKind: WorkerExitNormal}, want: mergeConflictExitNormal},
		{name: "needs review", result: WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusNeedsHumanReview)}, want: mergeConflictExitGaveUp},
		{name: "no change needed", result: WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusNoChangeNeeded)}, want: mergeConflictExitGaveUp},
		{name: "blocked", result: WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusBlocked)}},
		{name: "error", result: WorkerResult{ExitKind: WorkerExitError}},
		{name: "cancelled by a stall", result: WorkerResult{ExitKind: WorkerExitCancelled, CancelCause: errCancelStall}},
		{name: "cancelled by a shutdown", result: WorkerResult{ExitKind: WorkerExitCancelled, CancelCause: errCancelShutdown}, want: mergeConflictExitCancelled},
		{name: "cancelled with the parent context", result: WorkerResult{ExitKind: WorkerExitCancelled, CancelCause: context.Canceled}, want: mergeConflictExitCancelled},
		{name: "refused by a usage limit", result: WorkerResult{ExitKind: WorkerExitError, Error: &domain.AgentError{Kind: domain.ErrUsageLimit, Message: "limit"}}, want: mergeConflictExitUsageLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := mergeConflictExitOf(tt.result); got != tt.want {
				t.Errorf("mergeConflictExitOf(%+v) = %q, want %q", tt.result, got, tt.want)
			}
		})
	}
}

// TestMergeConflictEpisode_EndsWithoutCleanRead covers the two ways an
// episode ends with no clean read, the watch window elapsing and the issue's
// runtime state being released: each deletes the row, so the next watch does
// not resume the spent counter.
func TestMergeConflictEpisode_EndsWithoutCleanRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		end  func(p *rebasePath)
	}{
		{name: "watch window elapses", end: func(p *rebasePath) {
			p.config.WatchWindowMS = 1
			params := mergeConflictParams(p.store, &mergeabilitySCM{}, p.tracker)
			params.MergeConflictConfig = p.config
			params.MergeConflictPendingTTL = time.Millisecond
			params.NowFunc = func() time.Time { return p.now.Add(time.Hour) }
			reconcileMergeConflicts(p.state, params, discardLogger(), context.Background(), p.metrics)
		}},
		{name: "issue runtime state released", end: func(p *rebasePath) {
			releaseIssueRuntimeState(context.Background(), p.state, p.store, p.issueID, discardLogger())
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := newRebasePath(t, 2)
			if !p.pass() {
				t.Fatal("first pass scheduled no rebase")
			}
			p.start()
			p.exit(WorkerResult{ExitKind: WorkerExitNormal})
			if episode, _ := p.store.GetReactionEpisode(context.Background(), p.issueID, ReactionKindMergeConflict); episode.Attempts != 1 {
				t.Fatalf("persisted attempts before the end = %d, want 1", episode.Attempts)
			}

			tt.end(p)

			if episode, err := p.store.GetReactionEpisode(context.Background(), p.issueID, ReactionKindMergeConflict); err != nil || episode.Found {
				t.Errorf("episode after the end = %+v, %v; want no row", episode, err)
			}
		})
	}
}
