package orchestrator

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles shared by the cross-pass contention tests ---

// retrySlotCI is a controllable CIStatusProvider for the cross-pass
// contention tests in this file.
type retrySlotCI struct {
	result domain.CIResult
	err    error
}

var _ domain.CIStatusProvider = (*retrySlotCI)(nil)

func (c *retrySlotCI) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	return c.result, c.err
}

// retrySlotSCM is a single controllable SCMAdapter covering every method
// the review, bot-review, merge-conflict, and label-command passes read,
// so one double can drive a scenario spanning several of those passes.
type retrySlotSCM struct {
	reviewComments []domain.ReviewComment
	reviewErr      error
	botComments    []domain.ReviewComment
	mergeStatus    domain.PRMergeStatus
	mergeErr       error
	labelEvents    []domain.LabelEvent
	labelErr       error

	removeLabelCalls int
	removedLabels    []string
}

var _ domain.SCMAdapter = (*retrySlotSCM)(nil)

func (s *retrySlotSCM) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return s.reviewComments, s.reviewErr
}

func (s *retrySlotSCM) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return s.botComments, nil
}

func (s *retrySlotSCM) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}

func (s *retrySlotSCM) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}

func (s *retrySlotSCM) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return s.mergeStatus, s.mergeErr
}

func (s *retrySlotSCM) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}

func (s *retrySlotSCM) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *retrySlotSCM) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return s.labelEvents, s.labelErr
}

func (s *retrySlotSCM) RemoveLabel(_ context.Context, _ int, _, _, label string) error {
	s.removeLabelCalls++
	s.removedLabels = append(s.removedLabels, label)
	return nil
}

// retrySlotStore is a stateful ReconcileStore fake shared by every test in
// this file. Fingerprint reads reflect prior upserts within the same test,
// so "unchanged" and "advanced" assertions exercise real state rather than
// a canned return value.
type retrySlotStore struct {
	unsupportedReactionObservationStore

	marks      map[string]string
	dispatched map[string]bool

	runHistories []persistence.RunHistory
	savedEntries []persistence.RetryEntry
	deletedIDs   []string

	upsertCalls   int
	getCalls      int
	markCalls     int
	deleteFPCalls int
}

var _ ReconcileStore = (*retrySlotStore)(nil)

func newRetrySlotStore() *retrySlotStore {
	return &retrySlotStore{
		marks:      make(map[string]string),
		dispatched: make(map[string]bool),
	}
}

func (s *retrySlotStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.savedEntries = append(s.savedEntries, entry)
	return nil
}

func (s *retrySlotStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.deletedIDs = append(s.deletedIDs, issueID)
	return nil
}

func (s *retrySlotStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.runHistories = append(s.runHistories, run)
	return run, nil
}

func (s *retrySlotStore) UpsertReactionFingerprint(_ context.Context, issueID, kind, fingerprint string) error {
	s.upsertCalls++
	key := issueID + ":" + kind
	if s.marks[key] != fingerprint {
		s.dispatched[key] = false
	}
	s.marks[key] = fingerprint
	return nil
}

func (s *retrySlotStore) GetReactionFingerprint(_ context.Context, issueID, kind string) (string, bool, error) {
	s.getCalls++
	key := issueID + ":" + kind
	return s.marks[key], s.dispatched[key], nil
}

func (s *retrySlotStore) MarkReactionDispatched(_ context.Context, issueID, kind string) error {
	s.markCalls++
	s.dispatched[issueID+":"+kind] = true
	return nil
}

func (s *retrySlotStore) DeleteReactionFingerprint(_ context.Context, issueID, kind string) error {
	s.deleteFPCalls++
	key := issueID + ":" + kind
	delete(s.marks, key)
	delete(s.dispatched, key)
	return nil
}

// hasRunHistoryStatus reports whether any recorded run_history row carries
// the given status.
func (s *retrySlotStore) hasRunHistoryStatus(status string) bool {
	for _, run := range s.runHistories {
		if run.Status == status {
			return true
		}
	}
	return false
}

// --- Test helpers ---

// retrySlotBaseTime is a fixed reference time for the cross-pass
// contention tests in this file.
func retrySlotBaseTime() time.Time {
	return time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
}

// retrySlotParams returns a ReconcileParams wired for every pass that can
// contend for the retry slot: CI, review, merge-conflict, label-review,
// and label-fix. Bot-review, auto-merge, and merge-completion stay
// disabled because no scenario in this file needs them, and enabling an
// unused pass would only add a source of accidental interference.
func retrySlotParams(store ReconcileStore, ci domain.CIStatusProvider, scm domain.SCMAdapter, tracker domain.TrackerAdapter, now func() time.Time) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Done"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       noopRetryFire,
		NowFunc:           now,
		Ctx:               context.Background(),
		Logger:            discardLogger(),

		CIProvider:   ci,
		CIFeedback:   config.CIFeedbackConfig{MaxRetries: 2, Escalation: "label", EscalationLabel: "needs-human"},
		CIPendingTTL: ciPendingDefaultTTL,

		SCMAdapter:       scm,
		ReviewConfig:     ReviewReactionConfig{Escalation: "label", EscalationLabel: "needs-human", PollIntervalMS: 60_000, DebounceMS: 30_000, MaxContinuationTurns: 3},
		ReviewPendingTTL: reviewPendingDefaultTTL,

		MergeConflictConfig:             MergeConflictReactionConfig{Escalation: "label", EscalationLabel: "needs-human", PollIntervalMS: 60_000, MaxRetries: 1},
		MergeConflictReactionConfigured: true,
		MergeConflictPendingTTL:         mergeConflictPendingDefaultTTL,

		LabelReviewConfig:             LabelReviewReactionConfig{Provider: "github", ReviewLabel: "sortie:review", PollIntervalMS: 60_000},
		LabelReviewReactionConfigured: true,

		LabelFixConfig:             LabelFixReactionConfig{Provider: "github", FixLabel: "sortie:fix", PollIntervalMS: 60_000},
		LabelFixReactionConfigured: true,
	}
}

func retrySlotCIPending(issueID string, createdAt time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindCI,
		CreatedAt:  createdAt,
		KindData:   &CIReactionData{Branch: "feature/x"},
	}
}

func retrySlotReviewPending(issueID string, createdAt time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindReview,
		CreatedAt:  createdAt,
		KindData:   &ReviewReactionData{PRNumber: 20, Owner: "acme", Repo: "widgets", Branch: "feature/y"},
	}
}

func retrySlotMergeConflictPending(issueID string, createdAt time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindMergeConflict,
		CreatedAt:  createdAt,
		KindData:   &MergeConflictReactionData{PRNumber: 30, Owner: "acme", Repo: "widgets", Branch: "feature/z"},
	}
}

func retrySlotLabelReviewPending(issueID string, createdAt time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindLabelReview,
		CreatedAt:  createdAt,
		KindData:   &LabelReviewReactionData{PRNumber: 10, Owner: "acme", Repo: "widgets"},
	}
}

func retrySlotLabelFixPending(issueID string, createdAt time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindLabelFix,
		CreatedAt:  createdAt,
		KindData:   &LabelFixReactionData{PRNumber: 40, Owner: "acme", Repo: "widgets", Branch: "feature/w"},
	}
}

// --- Label-review defers to a ci incumbent it did not create ---

func TestRetrySlot_LabelReviewDefersToCI(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC1"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = retrySlotCIPending(issueID, now)
	lrKey := ReactionKey(issueID, ReactionKindLabelReview)
	state.PendingReactions[lrKey] = retrySlotLabelReviewPending(issueID, now)

	store := newRetrySlotStore()
	ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	scm := &retrySlotSCM{
		labelEvents: []domain.LabelEvent{labelEvent("1", "sortie:review", "alice", true, now.Add(-1*time.Minute))},
	}
	params := retrySlotParams(store, ci, scm, nil, func() time.Time { return now })

	preTickMark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)

	ReconcileRunningIssues(state, params)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("no incumbent occupies the slot after the tick")
	}
	if incumbent.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q", incumbent.ReactionKind, ReactionKindCI)
	}
	if _, ok := incumbent.ContinuationContext["ci_failure"]; !ok {
		t.Error("incumbent ContinuationContext missing ci_failure key")
	}
	if _, ok := state.PendingReactions[lrKey]; !ok {
		t.Error("label-review pending entry consumed; want re-enqueued")
	}
	postTickMark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if postTickMark != preTickMark {
		t.Errorf("label-review fingerprint = %q, want unchanged %q", postTickMark, preTickMark)
	}
	if scm.removeLabelCalls != 0 {
		t.Errorf("RemoveLabel calls = %d, want 0", scm.removeLabelCalls)
	}
}

// --- Review defers to a queued label-review retry ---

func TestRetrySlot_ReviewDefersToLabelReview(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC2"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindLabelReview}
	rKey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[rKey] = retrySlotReviewPending(issueID, now)

	store := newRetrySlotStore()
	scm := &retrySlotSCM{
		reviewComments: []domain.ReviewComment{{ID: "900", Body: "needs fix", SubmittedAt: now.Add(-5 * time.Minute)}},
	}
	params := retrySlotParams(store, &retrySlotCI{}, scm, nil, func() time.Time { return now })

	ReconcileRunningIssues(state, params)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("no incumbent occupies the slot after the tick")
	}
	if incumbent.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q (unchanged)", incumbent.ReactionKind, ReactionKindLabelReview)
	}
	if _, ok := state.PendingReactions[rKey]; !ok {
		t.Error("review pending entry consumed; want re-enqueued")
	}
	if _, ok := state.ReactionAttempts[rKey]; ok {
		t.Errorf("ReactionAttempts[%s] present, want absent (a defer must not increment it)", rKey)
	}
}

// --- CI and label-fix deferrals in both directions ---

func TestRetrySlot_CIAndLabelFixDeferrals(t *testing.T) {
	t.Parallel()

	t.Run("label-fix incumbent defers ci", func(t *testing.T) {
		t.Parallel()

		const issueID = "RS-AC3-A"
		now := retrySlotBaseTime()
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindLabelFix}
		ciKey := ReactionKey(issueID, ReactionKindCI)
		state.PendingReactions[ciKey] = retrySlotCIPending(issueID, now)

		store := newRetrySlotStore()
		ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
		params := retrySlotParams(store, ci, &retrySlotSCM{}, nil, func() time.Time { return now })

		ReconcileRunningIssues(state, params)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok || incumbent.ReactionKind != ReactionKindLabelFix {
			t.Fatalf("incumbent = %+v, want label-fix unchanged", incumbent)
		}
		if _, ok := state.PendingReactions[ciKey]; !ok {
			t.Error("ci pending entry consumed; want re-enqueued")
		}
		if _, ok := state.ReactionAttempts[ciKey]; ok {
			t.Errorf("ReactionAttempts[%s] present, want absent", ciKey)
		}
		if store.hasRunHistoryStatus("ci_failed") {
			t.Error("a ci_failed run_history row was appended on a defer; want none")
		}
	})

	t.Run("ci incumbent defers label-fix", func(t *testing.T) {
		t.Parallel()

		const issueID = "RS-AC3-B"
		now := retrySlotBaseTime()
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindCI}
		lfKey := ReactionKey(issueID, ReactionKindLabelFix)
		state.PendingReactions[lfKey] = retrySlotLabelFixPending(issueID, now)

		store := newRetrySlotStore()
		scm := &retrySlotSCM{
			labelEvents: []domain.LabelEvent{labelEvent("1", "sortie:fix", "alice", true, now.Add(-1*time.Minute))},
		}
		params := retrySlotParams(store, &retrySlotCI{}, scm, nil, func() time.Time { return now })

		preTickMark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)

		ReconcileRunningIssues(state, params)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok || incumbent.ReactionKind != ReactionKindCI {
			t.Fatalf("incumbent = %+v, want ci unchanged", incumbent)
		}
		entry, ok := state.PendingReactions[lfKey]
		if !ok {
			t.Fatal("label-fix pending entry consumed; want re-enqueued")
		}
		postTickMark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
		if postTickMark != preTickMark {
			t.Errorf("label-fix fingerprint = %q, want unchanged %q", postTickMark, preTickMark)
		}
		data, ok := entry.KindData.(*LabelFixReactionData)
		if !ok {
			t.Fatalf("KindData type = %T, want *LabelFixReactionData", entry.KindData)
		}
		if data.HighWaterMark != "" {
			t.Errorf("HighWaterMark = %q, want empty (a defer must not advance it)", data.HighWaterMark)
		}
		if scm.removeLabelCalls != 0 {
			t.Errorf("RemoveLabel calls = %d, want 0", scm.removeLabelCalls)
		}
	})
}

// --- Merge-conflict defers to a ci incumbent (cross-pass half) ---

func TestRetrySlot_MergeConflictDefersToCI(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC4"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindCI}
	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[mcKey] = retrySlotMergeConflictPending(issueID, now)

	store := newRetrySlotStore()
	scm := &retrySlotSCM{
		mergeStatus: domain.PRMergeStatus{Mergeability: domain.MergeabilityDirty, HeadSHA: "head-sha", BaseBranch: "main"},
	}
	params := retrySlotParams(store, &retrySlotCI{}, scm, nil, func() time.Time { return now })

	ReconcileRunningIssues(state, params)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok || incumbent.ReactionKind != ReactionKindCI {
		t.Fatalf("incumbent = %+v, want ci unchanged", incumbent)
	}
	if _, ok := state.PendingReactions[mcKey]; !ok {
		t.Error("merge-conflict pending entry consumed; want re-enqueued")
	}
	if _, ok := state.ReactionAttempts[mcKey]; ok {
		t.Errorf("ReactionAttempts[%s] present, want absent", mcKey)
	}
}

// --- Liveness with a handoff state configured ---

func TestRetrySlot_LivenessWithHandoffConfigured(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC6"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = retrySlotCIPending(issueID, now)
	state.PendingReactions[ReactionKey(issueID, ReactionKindLabelReview)] = retrySlotLabelReviewPending(issueID, now)

	store := newRetrySlotStore()
	ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	scm := &retrySlotSCM{
		labelEvents: []domain.LabelEvent{labelEvent("1", "sortie:review", "alice", true, now.Add(-1*time.Minute))},
	}
	tracker := &mockReconcileTracker{states: map[string]string{}}
	params := retrySlotParams(store, ci, scm, tracker, func() time.Time { return now })
	params.HandoffState = "Human Review"

	ReconcileRunningIssues(state, params)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok || incumbent.ReactionKind != ReactionKindCI {
		t.Fatalf("incumbent after tick 1 = %+v, want ci", incumbent)
	}

	// Fire the incumbent's own timer: the CI-fix worker dispatches. A real
	// timer fire only reaches HandleRetryTimer after its full delay
	// elapses; clearing the monotonic staleness fields models that
	// without the test sleeping out the continuation delay.
	incumbent.scheduledAt = time.Time{}
	incumbent.scheduledDelayMS = 0

	retryStore := &mockRetryStore{}
	retryTracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, issueID+"-ident", "In Progress")}
	retryParams := defaultRetryParams(t, retryStore, retryTracker)
	retryParams.HandoffState = "Human Review"

	HandleRetryTimer(state, issueID, retryParams)

	if _, running := state.Running[issueID]; !running {
		t.Fatal("CI-fix worker did not dispatch")
	}

	// That worker exits normally on the handoff path with a free slot,
	// releasing the claim.
	exitStore := &mockExitStore{}
	exitTracker := &mockTrackerAdapter{}
	exitParams := defaultExitParams(t, exitStore)
	exitParams.TrackerAdapter = exitTracker
	exitParams.HandoffState = "Human Review"
	exitParams.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, exitParams)

	if _, ok := state.Claimed[issueID]; ok {
		t.Fatal("claim held after the CI-fix worker's handoff exit, want released")
	}
	if _, ok := state.RetryAttempts[issueID]; ok {
		t.Fatal("retry entry present after the CI-fix worker's handoff exit, want free")
	}

	// Second tick: the label is still present and the slot is free.
	ReconcileRunningIssues(state, params)

	lrIncumbent, ok := state.RetryAttempts[issueID]
	if !ok || lrIncumbent.ReactionKind != ReactionKindLabelReview {
		t.Fatalf("incumbent after tick 2 = %+v, want label-review", lrIncumbent)
	}
	if _, ok := lrIncumbent.ContinuationContext["label_review"]; !ok {
		t.Error("incumbent ContinuationContext missing label_review key")
	}
	if scm.removeLabelCalls != 1 {
		t.Errorf("RemoveLabel calls = %d, want 1 across both ticks", scm.removeLabelCalls)
	}
}

// --- Liveness with no handoff state configured ---

func TestRetrySlot_LivenessNoHandoffConfigured(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC6B"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.Running[issueID] = &RunningEntry{
		Identifier: issueID + "-ident",
		StartedAt:  now,
		Issue:      candidateIssue(issueID, issueID+"-ident", "In Progress"),
	}
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = retrySlotCIPending(issueID, now)

	store := newRetrySlotStore()
	ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := retrySlotParams(store, ci, &retrySlotSCM{}, nil, func() time.Time { return now })

	// Tick while the continuation worker runs: ci takes the free slot.
	ReconcileRunningIssues(state, params)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok || incumbent.ReactionKind != ReactionKindCI {
		t.Fatalf("incumbent while the worker runs = %+v, want ci", incumbent)
	}

	// The continuation worker's own normal exit defers to the ci incumbent.
	exitStore := &mockExitStore{}
	exitParams := defaultExitParams(t, exitStore)
	exitParams.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, exitParams)

	ciEntry, ok := state.RetryAttempts[issueID]
	if !ok || ciEntry.ReactionKind != ReactionKindCI {
		t.Fatalf("ci incumbent after the continuation worker's exit = %+v, want survived", ciEntry)
	}

	// Fire the ci incumbent's own timer: it dispatches. A real timer fire
	// only reaches HandleRetryTimer after its full delay elapses; clearing
	// the monotonic staleness fields models that without the test
	// sleeping out the continuation delay.
	ciEntry.scheduledAt = time.Time{}
	ciEntry.scheduledDelayMS = 0

	retryStore := &mockRetryStore{}
	retryTracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, issueID+"-ident", "In Progress")}
	retryParams := defaultRetryParams(t, retryStore, retryTracker)

	HandleRetryTimer(state, issueID, retryParams)

	if _, running := state.Running[issueID]; !running {
		t.Fatal("ci-fix worker did not dispatch")
	}

	// That worker's own exit schedules the continuation into the freed slot.
	exitStore2 := &mockExitStore{}
	exitParams2 := defaultExitParams(t, exitStore2)
	exitParams2.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, exitParams2)

	continuation, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("continuation not scheduled into the freed slot")
	}
	if continuation.ReactionKind != "" {
		t.Errorf("RetryAttempts.ReactionKind = %q, want empty (continuation)", continuation.ReactionKind)
	}
}

// --- The deferral record names an empty-kind continuation incumbent
// with the literal "continuation" ---

func TestRetrySlot_DeferralRecordReportsContinuationForEmptyKindIncumbent(t *testing.T) {
	t.Parallel()

	const issueID = "RS-AC13"
	now := retrySlotBaseTime()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[issueID] = struct{}{}
	state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 5} // continuation: empty ReactionKind
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = retrySlotCIPending(issueID, now)

	store := newRetrySlotStore()
	ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	handler := &sweepLogHandler{}
	params := retrySlotParams(store, ci, &retrySlotSCM{}, nil, func() time.Time { return now })
	params.Logger = slog.New(handler)

	ReconcileRunningIssues(state, params)

	rec, ok := handler.findByMessage("retry slot occupied, deferring")
	if !ok {
		t.Fatal(`"retry slot occupied, deferring" not logged`)
	}
	if got := stringAttr(t, rec, "incumbent_kind"); got != "continuation" {
		t.Errorf("incumbent_kind = %q, want %q", got, "continuation")
	}
}

// --- CreatedAt advances on every arbitration deferral, for both a
// TTL-bearing kind and a kind with no TTL ---

func TestRetrySlot_TTLRefreshOnArbitrationDeferral(t *testing.T) {
	t.Parallel()

	t.Run("ci pending deferring behind a label-fix incumbent", func(t *testing.T) {
		t.Parallel()

		const issueID = "RS-AC24-CI"
		tick1 := retrySlotBaseTime()
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindLabelFix}
		ciKey := ReactionKey(issueID, ReactionKindCI)
		state.PendingReactions[ciKey] = retrySlotCIPending(issueID, tick1.Add(-29*time.Minute))

		store := newRetrySlotStore()
		ci := &retrySlotCI{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
		now := tick1
		handler := &sweepLogHandler{}
		params := retrySlotParams(store, ci, &retrySlotSCM{}, nil, func() time.Time { return now })
		params.Logger = slog.New(handler)

		ticks := []time.Time{tick1, tick1.Add(15 * time.Minute), tick1.Add(32 * time.Minute)}
		for _, tickNow := range ticks {
			now = tickNow
			ReconcileRunningIssues(state, params)

			entry, ok := state.PendingReactions[ciKey]
			if !ok {
				t.Fatalf("ci pending entry dropped at tick %v; want re-enqueued", tickNow)
			}
			if !entry.CreatedAt.Equal(tickNow) {
				t.Errorf("CreatedAt at tick %v = %v, want %v", tickNow, entry.CreatedAt, tickNow)
			}
		}
		if handler.countByMessage("ci pending entry exceeded ttl, dropping") != 0 {
			t.Error("TTL-drop Warn emitted despite the CreatedAt refresh; want none")
		}
	})

	t.Run("label-review pending deferring behind a ci incumbent", func(t *testing.T) {
		t.Parallel()

		const issueID = "RS-AC24-LR"
		tick1 := retrySlotBaseTime()
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, Attempt: 1, ReactionKind: ReactionKindCI}
		lrKey := ReactionKey(issueID, ReactionKindLabelReview)
		state.PendingReactions[lrKey] = retrySlotLabelReviewPending(issueID, tick1.Add(-29*time.Minute))

		store := newRetrySlotStore()
		scm := &retrySlotSCM{
			labelEvents: []domain.LabelEvent{labelEvent("1", "sortie:review", "alice", true, tick1.Add(-40*time.Minute))},
		}
		now := tick1
		params := retrySlotParams(store, &retrySlotCI{}, scm, nil, func() time.Time { return now })

		ticks := []time.Time{tick1, tick1.Add(15 * time.Minute), tick1.Add(32 * time.Minute)}
		for _, tickNow := range ticks {
			now = tickNow
			ReconcileRunningIssues(state, params)

			entry, ok := state.PendingReactions[lrKey]
			if !ok {
				t.Fatalf("label-review pending entry dropped at tick %v; want re-enqueued", tickNow)
			}
			if !entry.CreatedAt.Equal(tickNow) {
				t.Errorf("CreatedAt at tick %v = %v, want %v", tickNow, entry.CreatedAt, tickNow)
			}
		}
	})
}
