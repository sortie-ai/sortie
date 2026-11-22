package orchestrator

import (
	"context"
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

// start fires the scheduled rebase's retry timer, which starts its worker.
func (p *rebasePath) start() {
	p.t.Helper()
	retry := p.state.RetryAttempts[p.issueID]
	if retry == nil {
		p.t.Fatal("no rebase scheduled to start")
	}
	retry.TimerHandle.Stop()
	retry.scheduledAt = time.Time{}
	params := defaultRetryParams(p.t, &mockRetryStore{}, nil)
	params.Store = p.store
	params.TrackerAdapter = p.tracker
	params.Router = nil
	params.ActiveStates = []string{"queue"}
	params.TerminalStates = []string{"done"}
	params.HandoffState = rebasePathHandoff
	HandleRetryTimer(p.state, p.issueID, params)
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
	CancelRetry(p.state, p.issueID)
	delete(p.state.Claimed, p.issueID)
	p.state.PendingReactions[ReactionKey(p.issueID, ReactionKindMergeConflict)] = newMergeConflictPending(p.issueID, 77)
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
// a new head, and that head conflicts again before any pass sees it clean.
// Each new conflict starts a new episode, so the budget never runs out.
func TestMergeConflictEpisode_SuccessfulRebasesNeverEscalate(t *testing.T) {
	t.Parallel()

	p := newRebasePath(t, 1)
	const rebases = 4
	for run := 1; run <= rebases; run++ {
		if !p.pass() {
			t.Fatalf("pass %d scheduled no rebase for a new conflict, want one", run)
		}
		p.start()
		p.exit(WorkerResult{ExitKind: WorkerExitNormal})
		p.head = "head-" + string(rune('1'+run))
	}

	if p.escalated() {
		t.Error("escalation stands after rebases that each resolved their conflict")
	}
	if got := p.state.ReactionAttempts[ReactionKey(p.issueID, ReactionKindMergeConflict)]; got != 1 {
		t.Errorf("ReactionAttempts = %d, want 1 (each new conflict is a new episode)", got)
	}
	if got := p.metrics.checks["dispatched"]; got != rebases {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want %d`, got, rebases)
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
		{name: "cancelled", result: WorkerResult{ExitKind: WorkerExitCancelled}},
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
