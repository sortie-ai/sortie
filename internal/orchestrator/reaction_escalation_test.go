package orchestrator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

const (
	escIssue   = "iss-1"
	escHandoff = "In Review"
)

var escBase = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

type escSCM struct {
	ciReconcileSCM
	reviews    []domain.ReviewComment
	mergeCalls atomic.Int32
}

func (s *escSCM) FetchPendingReviews(context.Context, int, string, string) ([]domain.ReviewComment, error) {
	return s.reviews, nil
}

func (s *escSCM) FetchBotReviewComments(context.Context, int, string, string, []string) ([]domain.ReviewComment, error) {
	return s.reviews, nil
}

func (s *escSCM) GetReviewDecision(context.Context, int, string, string) (domain.ReviewDecision, error) {
	return domain.ReviewDecisionApproved, nil
}

func (s *escSCM) MergePR(context.Context, int, string, string, domain.MergeStrategy, string, string, string) (domain.MergeResult, error) {
	s.mergeCalls.Add(1)
	return domain.MergeResult{SHA: "merged"}, nil
}

type escTracker struct {
	*mockTrackerAdapter
	labels atomic.Int32
}

func (t *escTracker) AddLabel(context.Context, string, string) error {
	t.labels.Add(1)
	return nil
}

func (t *escTracker) actions() int {
	t.commentMu.Lock()
	defer t.commentMu.Unlock()
	return int(t.labels.Load()) + len(t.commentCalls) + len(t.transitionCalls)
}

type escActivitySpy struct {
	OrchestratorStore
	log reactionActivityLog
}

func (s *escActivitySpy) RecordReactionActivity(ctx context.Context, records []storage.ReactionActivity) error {
	return s.log.RecordReactionActivity(ctx, records)
}

type escWorld struct {
	t       *testing.T
	state   *State
	params  ReconcileParams
	store   OrchestratorStore
	scm     *escSCM
	ci      *mockCIProvider
	tracker *escTracker
	kind    string
	key     string
	at      time.Time
	now     time.Time
}

type escRow struct {
	name    string
	trigger ReactionEscalationTrigger
	change  func(*escWorld)
	ends    bool
}

type escRig struct {
	data        any
	counters    map[ReactionEscalationTrigger]int
	subjects    map[ReactionEscalationTrigger]string
	status      domain.PRMergeStatus
	setup       func(*escWorld, ReactionEscalationTrigger)
	lift        func(*ReconcileParams)
	pass        func(*State, ReconcileParams)
	endsOnMerge bool
	rows        []escRow
}

func escComment(id, file string) domain.ReviewComment {
	return domain.ReviewComment{ID: id, FilePath: file, Reviewer: "alice", Body: "fix " + id}
}

func newEscWorld(t *testing.T, kind string, rig escRig, trigger ReactionEscalationTrigger) *escWorld {
	t.Helper()
	w := &escWorld{
		t:       t,
		state:   NewState(5000, 4, 0, nil, AgentTotals{}),
		store:   openInMemoryStore(t),
		scm:     &escSCM{ciReconcileSCM: ciReconcileSCM{result: rig.status}},
		ci:      &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}},
		tracker: &escTracker{mockTrackerAdapter: &mockTrackerAdapter{}},
		kind:    kind,
		key:     ReactionKey(escIssue, kind),
		at:      escBase.Add(time.Hour),
		now:     escBase.Add(48 * time.Hour),
	}
	w.tracker.fetchStatesFn = func(context.Context, []string) (map[string]string, error) {
		return map[string]string{escIssue: escHandoff}, nil
	}
	w.params = ReconcileParams{
		Ctx: context.Background(), Logger: discardLogger(), Store: w.store, OnRetryFire: noopRetryFire,
		SCMAdapter: w.scm, TrackerAdapter: w.tracker, CIProvider: w.ci,
		HandoffState: escHandoff, TerminalStates: []string{"Done"},
		NowFunc: func() time.Time { return w.now },
	}
	w.state.PendingReactions[w.key] = &PendingReaction{
		IssueID: escIssue, Identifier: "PROJ-1", Kind: kind, CreatedAt: escBase, KindData: rig.data,
		EscalatedAt: w.at, EscalationTrigger: trigger, EscalationSubject: rig.subjects[trigger],
	}
	w.state.ReactionAttempts[w.key] = rig.counters[trigger]
	rec := storage.ReactionEscalation{
		IssueID: escIssue, Kind: kind, Subject: rig.subjects[trigger], EscalatedAt: &w.at,
		Trigger: string(trigger), Attempts: rig.counters[trigger], PRNumber: 99, PROwner: "acme", PRRepo: "widgets",
	}
	if err := w.store.SaveReactionEscalation(context.Background(), rec); err != nil {
		t.Fatalf("SaveReactionEscalation: %v", err)
	}
	rig.setup(w, trigger)
	return w
}

func (w *escWorld) escalationStands(t *testing.T) bool {
	t.Helper()
	entry := w.state.PendingReactions[w.key]
	entryStands := entry != nil && entry.EscalatedAt.Equal(w.at)
	rec, _, err := w.store.GetReactionEscalation(context.Background(), escIssue, w.kind)
	if err != nil {
		t.Fatalf("GetReactionEscalation: %v", err)
	}
	recordStands := rec.EscalatedAt != nil && rec.EscalatedAt.Equal(w.at)
	if entryStands != recordStands {
		t.Errorf("escalation stands on the entry = %t, in the record = %t, want them equal", entryStands, recordStands)
	}
	return entryStands
}

func (w *escWorld) runPasses(rig escRig, passes int) {
	for range passes {
		rig.pass(w.state, w.params)
		w.state.TrackerOpsWg.Wait()
		w.now = w.now.Add(time.Hour)
	}
}

func escRigs() map[string]escRig {
	budgetAndTriage := func() map[ReactionEscalationTrigger]int {
		return map[ReactionEscalationTrigger]int{EscalationTriggerBudget: 2, EscalationTriggerTriage: 1}
	}
	commentSet := func(a ...domain.ReviewComment) func(*escWorld) {
		return func(w *escWorld) { w.scm.reviews = a }
	}
	handedOff := func(kind string) func(*escWorld) {
		return func(w *escWorld) {
			if err := w.store.AddReactionHandedOffComments(context.Background(), escIssue, kind, []string{"c1"}); err != nil {
				w.t.Fatalf("AddReactionHandedOffComments: %v", err)
			}
		}
	}
	const n, o, r, b, s = 99, "acme", "widgets", "feat", "head-1"
	open := domain.PRMergeStatus{HeadSHA: "head-1", Mergeability: domain.MergeabilityClean}

	return map[string]escRig{
		ReactionKindCI: {
			data:     &CIReactionData{PRNumber: n, Owner: o, Repo: r, Branch: b, SHA: s},
			counters: budgetAndTriage(),
			subjects: map[ReactionEscalationTrigger]string{EscalationTriggerBudget: "head-1", EscalationTriggerTriage: "head-1"},
			status:   open,
			setup: func(w *escWorld, _ ReactionEscalationTrigger) {
				w.params.CIFeedback.MaxRetries = 2
				w.params.CIWatchWindow = time.Hour
				if err := w.store.UpsertReactionFingerprint(context.Background(), escIssue, ReactionKindCI, "head-1"); err != nil {
					w.t.Fatalf("UpsertReactionFingerprint: %v", err)
				}
			},
			lift: func(p *ReconcileParams) { p.CIFeedback.MaxRetries = 3 },
			pass: func(st *State, p ReconcileParams) {
				reconcileCIStatus(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			endsOnMerge: true,
			rows: []escRow{
				{name: "passing verdict", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) { w.ci.result = domain.CIResult{Status: domain.CIStatusPassing} }},
				{name: "passing verdict after triage", trigger: EscalationTriggerTriage, ends: true, change: func(w *escWorld) { w.ci.result = domain.CIResult{Status: domain.CIStatusPassing} }},
				{name: "new head", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) { w.scm.result.HeadSHA = "head-2" }},
			},
		},
		ReactionKindReview: {
			data:     &ReviewReactionData{PRNumber: n, Owner: o, Repo: r, Branch: b, SHA: s},
			counters: budgetAndTriage(),
			subjects: map[ReactionEscalationTrigger]string{
				EscalationTriggerBudget: buildReviewFingerprint([]domain.ReviewComment{escComment("c1", "a.go")}),
				EscalationTriggerTriage: buildReviewFingerprint([]domain.ReviewComment{escComment("c1", "a.go")}),
			},
			status: open,
			setup: func(w *escWorld, _ ReactionEscalationTrigger) {
				w.params.ReviewConfig = ReviewReactionConfig{Escalation: "label", MaxContinuationTurns: 2}
				w.params.ReviewPendingTTL = time.Hour
				commentSet(escComment("c1", "a.go"))(w)
			},
			lift: func(p *ReconcileParams) { p.ReviewConfig.MaxContinuationTurns = 3 },
			pass: func(st *State, p ReconcileParams) {
				reconcileReviewComments(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			endsOnMerge: true,
			rows: []escRow{
				{name: "comment set emptied", trigger: EscalationTriggerBudget, ends: true, change: commentSet()},
				{name: "every comment handed to a run", trigger: EscalationTriggerBudget, ends: true, change: handedOff(ReactionKindReview)},
				{name: "comment set changed after triage", trigger: EscalationTriggerTriage, ends: true, change: commentSet(escComment("c1", "a.go"), escComment("c2", "b.go"))},
			},
		},
		ReactionKindBotReview: {
			data:     &BotReviewReactionData{PRNumber: n, Owner: o, Repo: r, Branch: b, SHA: s},
			counters: budgetAndTriage(),
			subjects: map[ReactionEscalationTrigger]string{
				EscalationTriggerBudget: buildReviewFingerprint([]domain.ReviewComment{escComment("c1", "a.go")}),
				EscalationTriggerTriage: buildReviewFingerprint([]domain.ReviewComment{escComment("c1", "a.go")}),
			},
			status: open,
			setup: func(w *escWorld, _ ReactionEscalationTrigger) {
				w.params.BotReviewConfigured = true
				w.params.BotReviewConfig = BotReviewReactionConfig{Escalation: "label", MaxContinuationTurns: 2}
				w.params.BotReviewPendingTTL = time.Hour
				commentSet(escComment("c1", "a.go"))(w)
				recordReportedComments(w.state, escIssue, w.scm.reviews)
			},
			lift: func(p *ReconcileParams) { p.BotReviewConfig.MaxContinuationTurns = 3 },
			pass: func(st *State, p ReconcileParams) {
				reconcileBotReviewComments(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			endsOnMerge: true,
			rows: []escRow{
				{name: "only a summary comment left", trigger: EscalationTriggerBudget, ends: true, change: commentSet(escComment("s1", ""))},
				{name: "every inline comment handed to a run", trigger: EscalationTriggerBudget, ends: true, change: handedOff(ReactionKindBotReview)},
				{name: "comment set changed after triage", trigger: EscalationTriggerTriage, ends: true, change: commentSet(escComment("c1", "a.go"), escComment("c2", "b.go"))},
			},
		},
		ReactionKindMergeConflict: {
			data:     &MergeConflictReactionData{PRNumber: n, Owner: o, Repo: r, Branch: b, SHA: s},
			counters: budgetAndTriage(),
			subjects: map[ReactionEscalationTrigger]string{
				EscalationTriggerBudget: buildMergeConflictFingerprint("head-1"),
				EscalationTriggerTriage: buildMergeConflictFingerprint("head-1"),
			},
			status: domain.PRMergeStatus{HeadSHA: "head-1", BaseBranch: "main", Mergeability: domain.MergeabilityDirty},
			setup: func(w *escWorld, _ ReactionEscalationTrigger) {
				w.params.MergeConflictReactionConfigured = true
				w.params.MergeConflictConfig = MergeConflictReactionConfig{Escalation: "label", MaxRetries: 2}
				w.params.MergeConflictPendingTTL = time.Hour
			},
			lift: func(p *ReconcileParams) { p.MergeConflictConfig.MaxRetries = 3 },
			pass: func(st *State, p ReconcileParams) {
				reconcileMergeConflicts(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			endsOnMerge: true,
			rows: []escRow{
				{name: "conflict resolved", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) { w.scm.result.Mergeability = domain.MergeabilityClean }},
				{name: "new conflicting head", trigger: EscalationTriggerTriage, ends: true, change: func(w *escWorld) { w.scm.result.HeadSHA = "head-2" }},
			},
		},
		ReactionKindAutoMerge: {
			data:     &AutoMergeReactionData{PRNumber: n, Owner: o, Repo: r, Branch: b, SHA: s},
			counters: map[ReactionEscalationTrigger]int{EscalationTriggerBudget: 2, EscalationTriggerRefused: 1},
			subjects: map[ReactionEscalationTrigger]string{EscalationTriggerBudget: "", EscalationTriggerRefused: "merge-fp"},
			status:   open,
			setup: func(w *escWorld, _ ReactionEscalationTrigger) {
				w.params.AutoMergeReactionConfigured = true
				w.params.AutoMergeConfig = AutoMergeReactionConfig{Escalation: "label", MaxRetries: 2}
				w.params.AutoMergePendingTTL = time.Hour
			},
			lift: func(p *ReconcileParams) { p.AutoMergeConfig.MaxRetries = 0 },
			pass: func(st *State, p ReconcileParams) {
				reconcileAutoMerge(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			endsOnMerge: true,
		},
		ReactionKindMergeCompletion: {
			data: &MergeCompletionReactionData{PRNumber: n, Owner: o, Repo: r},
			counters: map[ReactionEscalationTrigger]int{
				EscalationTriggerBudget: 3, EscalationTriggerRefused: 1, EscalationTriggerMissingMergeCommit: 0,
			},
			subjects: map[ReactionEscalationTrigger]string{
				EscalationTriggerBudget: "abc123", EscalationTriggerRefused: "abc123", EscalationTriggerMissingMergeCommit: "acme/widgets#99",
			},
			status: domain.PRMergeStatus{HeadSHA: "head-1", Merged: true, MergeCommitSHA: "abc123"},
			setup: func(w *escWorld, trigger ReactionEscalationTrigger) {
				if trigger == EscalationTriggerMissingMergeCommit {
					w.scm.result.MergeCommitSHA = ""
				}
				w.params.MergeCompletionReactionConfigured = true
				w.params.MergeCompletionConfig = MergeCompletionReactionConfig{TargetState: "Done", Escalation: "label", MaxRetries: 2}
			},
			lift: func(p *ReconcileParams) { p.MergeCompletionConfig.MaxRetries = 3 },
			pass: func(st *State, p ReconcileParams) {
				reconcileMergeCompletion(st, p, discardLogger(), context.Background(), &domain.NoopMetrics{})
			},
			rows: []escRow{
				{name: "issue missing from the tracker", trigger: EscalationTriggerRefused, ends: true, change: func(w *escWorld) {
					w.tracker.fetchStatesFn = func(context.Context, []string) (map[string]string, error) { return map[string]string{}, nil }
				}},
				{name: "issue outside the handoff state", trigger: EscalationTriggerRefused, ends: true, change: func(w *escWorld) {
					w.tracker.fetchStatesFn = func(context.Context, []string) (map[string]string, error) {
						return map[string]string{escIssue: "In Progress"}, nil
					}
					w.tracker.fetchIssueByIDFn = func(context.Context, string) (domain.Issue, error) {
						return domain.Issue{ID: escIssue, State: "In Progress"}, nil
					}
				}},
				{name: "merge commit reported", trigger: EscalationTriggerMissingMergeCommit, ends: true, change: func(w *escWorld) {
					w.scm.result.MergeCommitSHA = "abc123"
				}},
			},
		},
	}
}

func TestReactionEscalation(t *testing.T) {
	t.Parallel()

	rigs := escRigs()
	for _, kind := range EscalatingReactionKinds() {
		rig, ok := rigs[kind]
		if !ok {
			t.Errorf("EscalatingReactionKinds() returned %q, which has no escalation rows", kind)
			continue
		}
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			rows := standingRows(rig)
			rows = append(rows, escRow{name: "ceiling lifted", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) { rig.lift(&w.params) }})
			rows = append(rows, escRow{name: "pull request not found", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) {
				w.scm.err = &domain.SCMError{Kind: domain.ErrSCMNotFound, Message: "no such pull request"}
			}})
			if rig.endsOnMerge {
				rows = append(rows,
					escRow{name: "pull request merged", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) {
						w.scm.result = domain.PRMergeStatus{HeadSHA: "head-1", Merged: true, MergeCommitSHA: "m1"}
					}},
					escRow{name: "pull request closed", trigger: EscalationTriggerBudget, ends: true, change: func(w *escWorld) {
						w.scm.result = domain.PRMergeStatus{HeadSHA: "head-1", Closed: true}
					}},
				)
			}
			rows = append(rows, rig.rows...)

			for _, tt := range rows {
				t.Run(tt.name+"/"+string(tt.trigger), func(t *testing.T) {
					t.Parallel()

					w := newEscWorld(t, kind, rig, tt.trigger)
					if tt.change != nil {
						tt.change(w)
					}

					w.runPasses(rig, 2)

					if got := w.escalationStands(t); got == tt.ends {
						t.Errorf("%s escalation after two passes (%s) stands = %t, want %t", kind, tt.name, got, !tt.ends)
					}
					if tt.ends {
						return
					}
					if got := w.state.ReactionAttempts[w.key]; got != rig.counters[tt.trigger] {
						t.Errorf("ReactionAttempts after a standing pass = %d, want %d", got, rig.counters[tt.trigger])
					}
					if n := len(w.state.RetryAttempts); n != 0 || w.scm.mergeCalls.Load() != 0 || w.tracker.actions() != 0 {
						t.Errorf("standing pass dispatched %d retries, %d merges and applied %d tracker actions, want none", n, w.scm.mergeCalls.Load(), w.tracker.actions())
					}
				})
			}
		})
	}
}

func standingRows(rig escRig) []escRow {
	var rows []escRow
	for _, trigger := range []ReactionEscalationTrigger{EscalationTriggerBudget, EscalationTriggerTriage, EscalationTriggerRefused, EscalationTriggerMissingMergeCommit} {
		if _, ok := rig.counters[trigger]; ok {
			rows = append(rows, escRow{name: "no ending fact", trigger: trigger})
		}
	}
	return rows
}

func TestBeginEscalation_RecordsWhenItBegins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	spy := &escActivitySpy{OrchestratorStore: openInMemoryStore(t)}
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	lineage := StageLineage{ChainID: "chain-esc", DispatchedAt: escBase}
	pending := &PendingReaction{IssueID: escIssue, Identifier: "PROJ-1", Kind: ReactionKindCI, StageLineage: lineage}
	begin := func() bool {
		return beginEscalation(ctx, state, spy, pending, EscalationTriggerBudget, "head-1", escBase, discardLogger())
	}

	if !begin() {
		t.Fatal("beginEscalation() = false, want true for the first escalation on a subject")
	}
	begin()
	got := spy.log.activities(storage.ReactionActivityEscalation)
	if len(got) != 1 || got[0].IssueID != escIssue || got[0].ChainID != lineage.ChainID || got[0].Kind != ReactionKindCI {
		t.Fatalf("escalation records after begin and a call while it stands = %+v, want one for %q kind %q chain %q", got, escIssue, ReactionKindCI, lineage.ChainID)
	}

	endEscalation(ctx, spy, pending, discardLogger())
	if begin() {
		t.Error("beginEscalation() = true, want false: the subject is latched")
	}
	if got := spy.log.activities(storage.ReactionActivityEscalation); len(got) != 2 {
		t.Errorf("escalation records after a latched subject begins again = %d, want 2", len(got))
	}
}

func TestReactionEscalation_EndsOutsideHandoff(t *testing.T) {
	t.Parallel()

	type read struct {
		state string
		err   error
	}
	tests := []struct {
		name      string
		listed    string
		handoff   string
		drive     func(*State)
		reads     []read
		wantEnded []bool
		wantReads int
	}{
		{name: "undriven issue the direct read confirms", listed: "In Progress", handoff: escHandoff, reads: []read{{state: "In Progress"}}, wantEnded: []bool{true}, wantReads: 1},
		{name: "sibling continuation running", listed: "In Progress", handoff: escHandoff, drive: func(s *State) { s.Running[escIssue] = &RunningEntry{} }, wantEnded: []bool{false}},
		{name: "sibling retry pending", listed: "In Progress", handoff: escHandoff, drive: func(s *State) { s.RetryAttempts[escIssue] = &RetryEntry{} }, wantEnded: []bool{false}},
		{name: "claimed", listed: "In Progress", handoff: escHandoff, drive: func(s *State) { s.Claimed[escIssue] = struct{}{} }, wantEnded: []bool{false}},
		{name: "parked", listed: "In Progress", handoff: escHandoff, drive: func(s *State) { s.Parked[escIssue] = &ParkedEntry{} }, wantEnded: []bool{false}},
		{name: "listing trails a handoff write", listed: "In Progress", handoff: escHandoff, reads: []read{{state: escHandoff}}, wantEnded: []bool{false, false}, wantReads: 2},
		{name: "direct read fails", listed: "In Progress", handoff: escHandoff, reads: []read{{err: errors.New("unavailable")}}, wantEnded: []bool{false, false}, wantReads: 2},
		{name: "direct read finds a terminal state", listed: "In Progress", handoff: escHandoff, reads: []read{{state: "Done"}}, wantEnded: []bool{false}, wantReads: 1},
		{name: "no state ends at the second reading", listed: "", handoff: escHandoff, reads: []read{{}}, wantEnded: []bool{false, true}, wantReads: 2},
		{name: "handoff state unset", listed: "In Progress", handoff: "", wantEnded: []bool{false}},
		{name: "listed state is the handoff state", listed: "in review", handoff: escHandoff, wantEnded: []bool{false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := openInMemoryStore(t)
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			for _, kind := range EscalatingReactionKinds() {
				key := ReactionKey(escIssue, kind)
				at := escBase
				state.PendingReactions[key] = &PendingReaction{IssueID: escIssue, Identifier: "PROJ-1", Kind: kind, EscalatedAt: at}
				state.ReactionAttempts[key] = 2
				rec := storage.ReactionEscalation{IssueID: escIssue, Kind: kind, Subject: "subject", EscalatedAt: &at, Trigger: string(EscalationTriggerBudget)}
				if err := store.SaveReactionEscalation(ctx, rec); err != nil {
					t.Fatalf("SaveReactionEscalation: %v", err)
				}
			}
			recordReportedComments(state, escIssue, []domain.ReviewComment{escComment("c1", "a.go")})
			if tt.drive != nil {
				tt.drive(state)
			}
			var reads int
			tracker := &mockTrackerAdapter{fetchIssueByIDFn: func(context.Context, string) (domain.Issue, error) {
				r := read{state: "In Progress"}
				if len(tt.reads) > 0 {
					r = tt.reads[min(reads, len(tt.reads)-1)]
				}
				reads++
				return domain.Issue{ID: escIssue, State: r.state}, r.err
			}}

			for call, wantEnded := range tt.wantEnded {
				endOutsideHandoff(ctx, state, store, tracker, escIssue, tt.listed, tt.handoff, []string{"Done"}, discardLogger())

				standing, err := store.ListStandingReactionEscalations(ctx)
				if err != nil {
					t.Fatalf("ListStandingReactionEscalations: %v", err)
				}
				wantStanding := len(EscalatingReactionKinds())
				if wantEnded {
					wantStanding = 0
				}
				if len(standing) != wantStanding || (wantEnded && len(state.PendingReactions) != 0) {
					t.Fatalf("endOutsideHandoff call %d left %d standing records and %d entries, want %d records (ended = %t)", call+1, len(standing), len(state.PendingReactions), wantStanding, wantEnded)
				}
				if wantEnded && (len(state.ReactionAttempts) != 0 || len(state.ReactionReportedComments) != 0) {
					t.Errorf("endOutsideHandoff left counters %v and reported sets %v, want none", state.ReactionAttempts, state.ReactionReportedComments)
				}
			}
			if reads != tt.wantReads {
				t.Errorf("FetchIssueByID calls = %d, want %d", reads, tt.wantReads)
			}
		})
	}
}
