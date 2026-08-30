package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mergeConflictMetricsSpy records merge-conflict-specific metric calls while
// delegating every other method to NoopMetrics.
type mergeConflictMetricsSpy struct {
	domain.NoopMetrics
	checks      map[string]int
	escalations map[string]int
}

func newMergeConflictMetricsSpy() *mergeConflictMetricsSpy {
	return &mergeConflictMetricsSpy{
		checks:      make(map[string]int),
		escalations: make(map[string]int),
	}
}

func (s *mergeConflictMetricsSpy) IncMergeConflictChecks(result string) { s.checks[result]++ }
func (s *mergeConflictMetricsSpy) IncMergeConflictEscalations(action string) {
	s.escalations[action]++
}

// mergeabilitySCM is a controllable SCMAdapter whose GetMergeability return
// value is supplied per call by a function field, so episodic tests can
// advance the head SHA between reconcile ticks. The call count lets tests
// assert that the mergeability read runs exactly once per due tick.
type mergeabilitySCM struct {
	fn    func() (domain.PRMergeStatus, error)
	calls int
}

var _ domain.SCMAdapter = (*mergeabilitySCM)(nil)

func (m *mergeabilitySCM) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	m.calls++
	if m.fn != nil {
		return m.fn()
	}
	return domain.PRMergeStatus{}, nil
}

func (m *mergeabilitySCM) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (m *mergeabilitySCM) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (m *mergeabilitySCM) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}
func (m *mergeabilitySCM) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}
func (m *mergeabilitySCM) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}
func (m *mergeabilitySCM) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *mergeabilitySCM) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}
func (m *mergeabilitySCM) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// statefulFingerprintStore is a ReconcileStore that genuinely persists the
// per-(issue,kind) fingerprint and dispatched flag, so the head-SHA dedup
// across consecutive ticks is exercised against real stored state rather
// than canned values. Only the fingerprint methods carry behavior; the rest
// satisfy the interface.
type statefulFingerprintStore struct {
	unsupportedReactionObservationStore

	fingerprints map[string]fingerprintRecord

	upsertCalls         int
	getCalls            int
	markDispatchedCalls int
	deleteCalls         int
	deleteRetryCalls    int

	upsertErr error
	getErr    error
	markErr   error
	deleteErr error
}

type fingerprintRecord struct {
	fingerprint string
	dispatched  bool
}

var _ ReconcileStore = (*statefulFingerprintStore)(nil)

func newStatefulFingerprintStore() *statefulFingerprintStore {
	return &statefulFingerprintStore{fingerprints: make(map[string]fingerprintRecord)}
}

func (s *statefulFingerprintStore) SaveRetryEntry(_ context.Context, _ persistence.RetryEntry) error {
	return nil
}

func (s *statefulFingerprintStore) UpsertReactionFingerprint(_ context.Context, issueID, kind, fingerprint string) error {
	s.upsertCalls++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	key := issueID + ":" + kind
	rec := s.fingerprints[key]
	// A changed fingerprint resets the dispatched flag, mirroring the
	// persistence-layer upsert contract.
	if rec.fingerprint != fingerprint {
		rec.dispatched = false
	}
	rec.fingerprint = fingerprint
	s.fingerprints[key] = rec
	return nil
}

func (s *statefulFingerprintStore) GetReactionFingerprint(_ context.Context, issueID, kind string) (string, bool, error) {
	s.getCalls++
	if s.getErr != nil {
		return "", false, s.getErr
	}
	rec := s.fingerprints[issueID+":"+kind]
	return rec.fingerprint, rec.dispatched, nil
}

func (s *statefulFingerprintStore) MarkReactionDispatched(_ context.Context, issueID, kind string) error {
	s.markDispatchedCalls++
	if s.markErr != nil {
		return s.markErr
	}
	key := issueID + ":" + kind
	rec := s.fingerprints[key]
	rec.dispatched = true
	s.fingerprints[key] = rec
	return nil
}

func (s *statefulFingerprintStore) DeleteReactionFingerprint(_ context.Context, issueID, kind string) error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.fingerprints, issueID+":"+kind)
	return nil
}

func (s *statefulFingerprintStore) DeleteRetryEntry(_ context.Context, _ string) error {
	s.deleteRetryCalls++
	return nil
}
func (s *statefulFingerprintStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

// has reports whether a fingerprint row exists for the issue+kind.
func (s *statefulFingerprintStore) has(issueID, kind string) bool {
	_, ok := s.fingerprints[issueID+":"+kind]
	return ok
}

// --- Test helpers ---

// mcBaseTime is a fixed reference time for merge-conflict reconcile tests.
var mcBaseTime = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

// newMergeConflictPending builds a PendingReaction with
// Kind=ReactionKindMergeConflict.
func newMergeConflictPending(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindMergeConflict,
		CreatedAt:  mcBaseTime,
		KindData: &MergeConflictReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   "feature/" + issueID,
			SHA:      "seed-sha",
		},
	}
}

// stateWithMergeConflict creates a State with one merge-conflict
// PendingReaction entry.
func stateWithMergeConflict(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	s.PendingReactions[rkey] = newMergeConflictPending(issueID, prNumber)
	s.Claimed[issueID] = struct{}{}
	return s
}

// defaultMergeConflictConfig returns a MergeConflictReactionConfig at the
// issue-mandated default max_retries: 1 with label escalation.
func defaultMergeConflictConfig() MergeConflictReactionConfig {
	return MergeConflictReactionConfig{
		Escalation:      "label",
		EscalationLabel: "needs-human",
		PollIntervalMS:  60000,
		MaxRetries:      1,
	}
}

// mergeConflictParams returns ReconcileParams wired for merge-conflict
// reconcile unit tests.
func mergeConflictParams(store ReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter:                  tracker,
		SCMAdapter:                      scm,
		MergeConflictConfig:             defaultMergeConflictConfig(),
		MergeConflictReactionConfigured: true,
		Store:                           store,
		OnRetryFire:                     noopRetryFire,
		Ctx:                             context.Background(),
		Logger:                          discardLogger(),
		NowFunc:                         func() time.Time { return mcBaseTime },
	}
}

// dirtyStatus builds a dirty PRMergeStatus with the given head SHA and base
// branch.
func dirtyStatus(headSHA, base string) domain.PRMergeStatus {
	return domain.PRMergeStatus{
		Mergeability: domain.MergeabilityDirty,
		HeadSHA:      headSHA,
		BranchName:   "feature/branch",
		BaseBranch:   base,
	}
}

// --- reconcileMergeConflicts guard tests ---

func TestReconcileMergeConflicts_NilAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-NA", 10)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	params := mergeConflictParams(store, nil, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("MC-NA", ReactionKindMergeConflict)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed with nil SCMAdapter; want no-op")
	}
	if len(metrics.checks) != 0 {
		t.Errorf("IncMergeConflictChecks called with nil adapter = %v, want no calls", metrics.checks)
	}
}

func TestReconcileMergeConflicts_NotConfigured(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-NC", 10)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{}
	params := mergeConflictParams(store, scm, nil)
	params.MergeConflictReactionConfigured = false

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("MC-NC", ReactionKindMergeConflict)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed when MergeConflictReactionConfigured=false; want no-op")
	}
	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 (not configured)", scm.calls)
	}
}

// --- 5.1.1 Dispatch (AC1) ---

func TestReconcileMergeConflicts_Dispatch(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-D1", 42)
	rkey := ReactionKey("MC-D1", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-1", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 1 {
		t.Errorf("GetMergeability calls = %d, want 1 (one read per due tick)", scm.calls)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch; want consumed")
	}
	retry, ok := state.RetryAttempts["MC-D1"]
	if !ok {
		t.Fatal("retry not scheduled after dirty dispatch; want scheduled")
	}
	if retry.ReactionKind != ReactionKindMergeConflict {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindMergeConflict)
	}
	if retry.ContinuationContext == nil {
		t.Fatal("RetryEntry.ContinuationContext is nil; want merge_conflict map")
	}
	if _, ok := retry.ContinuationContext["merge_conflict"]; !ok {
		t.Error(`ContinuationContext missing "merge_conflict" key`)
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1 (1 > 1 false at default max_retries: 1)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.checks["dispatched"] != 1 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}
	if metrics.escalations["label"] != 0 {
		t.Errorf("IncMergeConflictEscalations called = %v, want none on first dirty head", metrics.escalations)
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1 (marked on dispatch tick)", store.markDispatchedCalls)
	}
}

// --- 5.1.2 Unknown defers (AC12) ---

func TestReconcileMergeConflicts_UnknownDefers(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-U1", 10)
	rkey := ReactionKey("MC-U1", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Mergeability: domain.MergeabilityUnknown, HeadSHA: "head-u"}, nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on mergeability unknown; want re-enqueued")
	}
	wantRetryAt := mcBaseTime.Add(60 * time.Second)
	if !entry.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (poll interval defer)", entry.PendingRetryAt, wantRetryAt)
	}
	if _, ok := state.RetryAttempts["MC-U1"]; ok {
		t.Error("retry scheduled on mergeability unknown; want none (no dispatch)")
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts touched on mergeability unknown; want untouched")
	}
	if store.upsertCalls != 0 || store.deleteCalls != 0 {
		t.Errorf("fingerprint touched on mergeability unknown (upsert=%d, delete=%d); want neither", store.upsertCalls, store.deleteCalls)
	}
	if metrics.checks["unknown"] != 1 {
		t.Errorf(`IncMergeConflictChecks("unknown") = %d, want 1`, metrics.checks["unknown"])
	}
}

// --- 5.1.3 Not-dirty resets (AC8 branch N1) ---

func TestReconcileMergeConflicts_NotDirtyResets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state domain.MergeabilityState
	}{
		{"clean", domain.MergeabilityClean},
		{"unstable", domain.MergeabilityUnstable},
		{"blocked", domain.MergeabilityBlocked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithMergeConflict(t, "MC-N1", 10)
			rkey := ReactionKey("MC-N1", ReactionKindMergeConflict)
			state.ReactionAttempts[rkey] = 1 // pre-seeded prior-episode counter
			store := newStatefulFingerprintStore()
			store.fingerprints[rkey] = fingerprintRecord{fingerprint: "old-fp", dispatched: true}
			metrics := newMergeConflictMetricsSpy()
			scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
				return domain.PRMergeStatus{Mergeability: tt.state, HeadSHA: "head-clean", BaseBranch: "main"}, nil
			}}
			params := mergeConflictParams(store, scm, nil)

			reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

			if scm.calls != 1 {
				t.Errorf("GetMergeability calls = %d, want 1 (read runs every due tick, no cap before read)", scm.calls)
			}
			entry, ok := state.PendingReactions[rkey]
			if !ok {
				t.Fatal("PendingReactions entry dropped on not-dirty; want re-enqueued")
			}
			wantRetryAt := mcBaseTime.Add(60 * time.Second)
			if !entry.PendingRetryAt.Equal(wantRetryAt) {
				t.Errorf("PendingRetryAt = %v, want %v (poll interval defer)", entry.PendingRetryAt, wantRetryAt)
			}
			if store.deleteCalls != 1 {
				t.Errorf("DeleteReactionFingerprint calls = %d, want 1 on not-dirty (episode close)", store.deleteCalls)
			}
			if store.has("MC-N1", ReactionKindMergeConflict) {
				t.Error("fingerprint row still present after not-dirty; want deleted")
			}
			if _, ok := state.ReactionAttempts[rkey]; ok {
				t.Error("ReactionAttempts not reset after not-dirty; want deleted (episode close)")
			}
			if metrics.checks["clear"] != 1 {
				t.Errorf(`IncMergeConflictChecks("clear") = %d, want 1`, metrics.checks["clear"])
			}
		})
	}
}

// --- 5.1.4 Dedup same head (AC7) ---

func TestReconcileMergeConflicts_DedupSameHead(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-DEDUP", 10)
	rkey := ReactionKey("MC-DEDUP", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-same", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	// Tick 1: dispatch for head-same.
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	if metrics.checks["dispatched"] != 1 {
		t.Fatalf(`after tick 1: IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Fatalf("after tick 1: ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}

	// The dispatch consumes the slot; the worker exit would re-seed it. The
	// same-head re-observation must hit the dedup branch, so re-seed the slot
	// to model the next reconcile pass over a still-pending entry.
	state.PendingReactions[rkey] = newMergeConflictPending("MC-DEDUP", 10)

	// Tick 2: same head, already dispatched → dedup branch, no re-dispatch,
	// no re-increment.
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if metrics.checks["dispatched"] != 1 {
		t.Errorf(`after tick 2: IncMergeConflictChecks("dispatched") = %d, want 1 (same head deduped)`, metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("after tick 2: ReactionAttempts[%s] = %d, want 1 (dedup must not re-increment)", rkey, state.ReactionAttempts[rkey])
	}
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on same-head dedup; want re-enqueued")
	}
	if metrics.escalations["label"] != 0 {
		t.Errorf("IncMergeConflictEscalations = %v, want none on same-head dedup", metrics.escalations)
	}
	rec := store.fingerprints[rkey]
	if rec.fingerprint != buildMergeConflictFingerprint("head-same") {
		t.Errorf("stored fingerprint = %q, want digest of head-same", rec.fingerprint)
	}
}

// --- 5.1.5 Episodic re-arm with intervening clean tick (AC8) ---

func TestReconcileMergeConflicts_EpisodicReArm(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-REARM", 10)
	rkey := ReactionKey("MC-REARM", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()

	var head string
	now := mcBaseTime
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		switch head {
		case "H2-clean":
			return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: head, BaseBranch: "main"}, nil
		default:
			return dirtyStatus(head, "main"), nil
		}
	}}
	params := mergeConflictParams(store, scm, nil)
	params.NowFunc = func() time.Time { return now }

	// Tick 1: head H1 dirty → dispatch, attempts becomes 1.
	head = "H1"
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	if metrics.checks["dispatched"] != 1 {
		t.Fatalf("after tick 1: dispatched = %d, want 1", metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Fatalf("after tick 1: ReactionAttempts = %d, want 1", state.ReactionAttempts[rkey])
	}

	// Tick 2: head H2 clean → branch N1 resets the episode. A worker exit
	// re-seeds the slot and frees the tick-1 retry entry; advance the
	// clock past the poll interval so the re-enqueued entry is due.
	head = "H2-clean"
	now = now.Add(2 * time.Minute)
	CancelRetry(state, "MC-REARM")
	state.PendingReactions[rkey] = newMergeConflictPending("MC-REARM", 10)
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	if metrics.checks["clear"] != 1 {
		t.Fatalf("after tick 2: clear = %d, want 1", metrics.checks["clear"])
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Fatal("after tick 2: ReactionAttempts present, want deleted (episode close)")
	}
	if store.has("MC-REARM", ReactionKindMergeConflict) {
		t.Fatal("after tick 2: fingerprint present, want deleted")
	}

	// Tick 3: head H3 dirty (a new independent conflict) on a later poll of
	// the N1-re-enqueued entry → fresh dispatch, attempts becomes 1 again.
	head = "H3"
	now = now.Add(2 * time.Minute)
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if metrics.checks["dispatched"] != 2 {
		t.Errorf("after tick 3: dispatched = %d, want 2 (re-armed)", metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("after tick 3: ReactionAttempts = %d, want 1 (fresh episode after counter reset)", state.ReactionAttempts[rkey])
	}
	if len(metrics.escalations) != 0 {
		t.Errorf("escalations = %v, want none across the re-arm episode", metrics.escalations)
	}
}

// --- 5.1.6 Escalate on strict over-limit (AC10) ---

func TestReconcileMergeConflicts_Escalate(t *testing.T) {
	t.Parallel()

	issueID := "MC-ESC"
	state := stateWithMergeConflict(t, issueID, 77)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	tracker := &ciTrackerStub{}

	var head string
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus(head, "main"), nil
	}}
	params := mergeConflictParams(store, scm, tracker)

	// Tick 1: head H1 dirty → dispatch, attempts == 1 (1 > 1 false).
	head = "H1"
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	if state.ReactionAttempts[rkey] != 1 {
		t.Fatalf("after tick 1: ReactionAttempts = %d, want 1", state.ReactionAttempts[rkey])
	}
	if metrics.checks["dispatched"] != 1 {
		t.Fatalf("after tick 1: dispatched = %d, want 1", metrics.checks["dispatched"])
	}

	// The agent rebased to a NEW head H2 that is still dirty. A worker exit
	// frees the tick-1 retry entry and re-seeds the slot; run tick 2.
	head = "H2"
	CancelRetry(state, issueID)
	state.PendingReactions[rkey] = newMergeConflictPending(issueID, 77)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if scm.calls != 2 {
		t.Errorf("GetMergeability calls = %d, want 2 (read NOT skipped on tick 2)", scm.calls)
	}
	// Strict over-limit: attempts 2, 2 > 1 true → escalate. The counter is
	// deleted on the escalation exit (episodic reset), so it is ABSENT.
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[%s] present after escalation = %d, want absent (episodic reset)", rkey, state.ReactionAttempts[rkey])
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("merge-conflict slot still present after escalation; want deleted")
	}
	if metrics.escalations["label"] != 1 {
		t.Errorf(`IncMergeConflictEscalations("label") = %d, want 1 (exactly one escalation)`, metrics.escalations["label"])
	}
	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1", tracker.addLabelCalled)
	}
	// No second dispatch on the escalation tick.
	if metrics.checks["dispatched"] != 1 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1 (no dispatch on escalation tick)`, metrics.checks["dispatched"])
	}
	if store.deleteCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1 during escalation", store.deleteCalls)
	}
	// Claim must NOT be released by escalation.
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("state.Claimed cleared by merge-conflict escalation; want preserved")
	}
}

// --- 5.1.7 Escalation resets counter, re-arm with NO clean tick (AC15) ---

func TestReconcileMergeConflicts_EscalationResetsCounterReArm(t *testing.T) {
	t.Parallel()

	issueID := "MC-AC15"
	state := stateWithMergeConflict(t, issueID, 88)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	tracker := &ciTrackerStub{}

	var head string
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus(head, "main"), nil
	}}
	params := mergeConflictParams(store, scm, tracker)

	// Tick 1: H1 dirty → dispatch, attempts 1.
	head = "H1"
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	if state.ReactionAttempts[rkey] != 1 {
		t.Fatalf("after tick 1: ReactionAttempts = %d, want 1", state.ReactionAttempts[rkey])
	}

	// Tick 2: still-dirty new head H2 → attempts 2, 2 > 1 → escalate, counter
	// deleted. A worker exit frees the tick-1 retry entry before this tick.
	head = "H2"
	CancelRetry(state, issueID)
	state.PendingReactions[rkey] = newMergeConflictPending(issueID, 88)
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Fatalf("after tick 2: ReactionAttempts present, want absent (escalation reset)")
	}
	if metrics.escalations["label"] != 1 {
		t.Fatalf("after tick 2: escalations[label] = %d, want 1", metrics.escalations["label"])
	}

	// A normal worker exit re-seeds the merge-conflict slot. There is NO clean
	// (N1) observation between the escalation and the next conflict.
	state.PendingReactions[rkey] = newMergeConflictPending(issueID, 88)

	// Tick 3: a NEW dirty head H3 → because the escalation reset the counter,
	// attempts goes to 1 (not 2), 1 > 1 is false, so the orchestrator
	// DISPATCHES fresh rather than escalating. Without the escalation-time
	// counter reset, the counter would still read 2 and H3 would escalate at
	// attempts 3 with zero rebase dispatches.
	head = "H3"
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("after tick 3: ReactionAttempts = %d, want 1 (fresh episode, no intervening clean tick)", state.ReactionAttempts[rkey])
	}
	if metrics.checks["dispatched"] != 2 {
		t.Errorf("after tick 3: dispatched = %d, want 2 (fresh dispatch, not escalation)", metrics.checks["dispatched"])
	}
	if metrics.escalations["label"] != 1 {
		t.Errorf("after tick 3: escalations[label] = %d, want 1 (no second escalation)", metrics.escalations["label"])
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("after tick 3: slot still present, want consumed by fresh dispatch")
	}
}

// --- 5.1.8 Cross-kind isolation (AC2) ---

func TestReconcileMergeConflicts_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	issueID := "MC-ISO"
	state := NewState(5000, 4, nil, AgentTotals{})

	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.PendingReactions[ciKey] = &PendingReaction{
		IssueID: issueID, Identifier: issueID + "-ident", Kind: ReactionKindCI,
		CreatedAt: mcBaseTime, KindData: &CIReactionData{Branch: "feature/iso"},
	}
	state.ReactionAttempts[ciKey] = 3

	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[reviewKey] = &PendingReaction{
		IssueID: issueID, Identifier: issueID + "-ident", Kind: ReactionKindReview,
		CreatedAt: mcBaseTime, KindData: &ReviewReactionData{PRNumber: 5},
	}
	state.ReactionAttempts[reviewKey] = 2

	botKey := ReactionKey(issueID, ReactionKindBotReview)
	state.PendingReactions[botKey] = &PendingReaction{
		IssueID: issueID, Identifier: issueID + "-ident", Kind: ReactionKindBotReview,
		CreatedAt: mcBaseTime, KindData: &BotReviewReactionData{PRNumber: 5},
	}
	state.ReactionAttempts[botKey] = 4

	mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.PendingReactions[mergeKey] = &PendingReaction{
		IssueID: issueID, Identifier: issueID + "-ident", Kind: ReactionKindAutoMerge,
		CreatedAt: mcBaseTime, KindData: &AutoMergeReactionData{PRNumber: 5},
	}
	state.ReactionAttempts[mergeKey] = 1

	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[mcKey] = newMergeConflictPending(issueID, 5)
	state.Claimed[issueID] = struct{}{}

	store := newStatefulFingerprintStore()
	store.fingerprints[ciKey] = fingerprintRecord{fingerprint: "ci-fp", dispatched: true}
	store.fingerprints[reviewKey] = fingerprintRecord{fingerprint: "review-fp", dispatched: true}
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-iso", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	// merge-conflict dispatched.
	if metrics.checks["dispatched"] != 1 {
		t.Fatalf(`IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}

	// All four sibling slots survive untouched.
	for _, sib := range []struct {
		name string
		key  string
	}{
		{"ci", ciKey}, {"review", reviewKey}, {"bot-review", botKey}, {"merge", mergeKey},
	} {
		if _, ok := state.PendingReactions[sib.key]; !ok {
			t.Errorf("%s PendingReaction slot removed by merge-conflict dispatch; want untouched", sib.name)
		}
	}

	// Sibling counters untouched.
	if state.ReactionAttempts[ciKey] != 3 {
		t.Errorf("ReactionAttempts[ci] = %d, want 3 (untouched)", state.ReactionAttempts[ciKey])
	}
	if state.ReactionAttempts[reviewKey] != 2 {
		t.Errorf("ReactionAttempts[review] = %d, want 2 (untouched)", state.ReactionAttempts[reviewKey])
	}
	if state.ReactionAttempts[botKey] != 4 {
		t.Errorf("ReactionAttempts[bot-review] = %d, want 4 (untouched)", state.ReactionAttempts[botKey])
	}
	if state.ReactionAttempts[mergeKey] != 1 {
		t.Errorf("ReactionAttempts[merge] = %d, want 1 (untouched)", state.ReactionAttempts[mergeKey])
	}

	// Sibling fingerprints untouched (merge-conflict only deletes its own
	// kind on N1/escalation, and this tick dispatches, deleting none).
	if !store.has(issueID, ReactionKindCI) {
		t.Error("ci fingerprint deleted by merge-conflict dispatch; want untouched")
	}
	if !store.has(issueID, ReactionKindReview) {
		t.Error("review fingerprint deleted by merge-conflict dispatch; want untouched")
	}
}

// --- 5.1.9 Fetch error backs off (E1) ---

func TestReconcileMergeConflicts_FetchErrorBacksOff(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-ERR", 10)
	rkey := ReactionKey("MC-ERR", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{}, errors.New("upstream timeout")
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on fetch error; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 after first error", entry.PendingAttempts)
	}
	// Backoff floored at the poll interval (60s).
	minExpected := mcBaseTime.Add(60 * time.Second)
	if entry.PendingRetryAt.Before(minExpected) {
		t.Errorf("PendingRetryAt = %v, want >= %v (floored at poll interval)", entry.PendingRetryAt, minExpected)
	}
	if metrics.checks["error"] != 1 {
		t.Errorf(`IncMergeConflictChecks("error") = %d, want 1`, metrics.checks["error"])
	}
	// A fetch error is not a resolution attempt: the per-episode counter must
	// not increment, and nothing escalates.
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts incremented on fetch error; want untouched")
	}
	if len(metrics.escalations) != 0 {
		t.Errorf("escalations = %v, want none on fetch error", metrics.escalations)
	}
}

// --- 5.1.10 Empty head SHA does not dispatch (D1a) ---

func TestReconcileMergeConflicts_EmptyHeadDoesNotDispatch(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-NOHEAD", 10)
	rkey := ReactionKey("MC-NOHEAD", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("", "main"), nil // dirty but empty head
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on empty head SHA; want re-enqueued (deferred)")
	}
	wantRetryAt := mcBaseTime.Add(60 * time.Second)
	if !entry.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (poll interval defer)", entry.PendingRetryAt, wantRetryAt)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts incremented on empty head SHA; want untouched (deferral burns no attempt)")
	}
	if _, ok := state.RetryAttempts["MC-NOHEAD"]; ok {
		t.Error("retry scheduled on empty head SHA; want none (no dispatch)")
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 0 (guard precedes fingerprint)", store.upsertCalls)
	}
	if len(metrics.checks) != 0 {
		t.Errorf("IncMergeConflictChecks called = %v, want none on empty-head defer", metrics.checks)
	}
}

// --- 5.1.11 Empty base branch defers (AC16, D1b) ---

func TestReconcileMergeConflicts_EmptyBaseDefers(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-NOBASE", 10)
	rkey := ReactionKey("MC-NOBASE", ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-ok", ""), nil // dirty, head present, base empty
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on empty base branch; want re-enqueued (deferred)")
	}
	wantRetryAt := mcBaseTime.Add(60 * time.Second)
	if !entry.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (poll interval defer)", entry.PendingRetryAt, wantRetryAt)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts incremented on empty base branch; want untouched (deferral burns no attempt)")
	}
	if _, ok := state.RetryAttempts["MC-NOBASE"]; ok {
		t.Error("retry scheduled on empty base branch; want none (no dispatch)")
	}
	if len(metrics.escalations) != 0 {
		t.Errorf("escalations = %v, want none on empty-base defer", metrics.escalations)
	}
	if len(metrics.checks) != 0 {
		t.Errorf("IncMergeConflictChecks called = %v, want none on empty-base defer", metrics.checks)
	}
}

// TestReconcileMergeConflicts_DropOnAgeReleasesCounter verifies that the
// drop-on-age branch also deletes the merge-conflict reaction attempt
// counter, leaves a sibling kind's counter and the claim untouched, and
// performs no retry or fingerprint store call.
func TestReconcileMergeConflicts_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "MC-AGE-1"
	state := stateWithMergeConflict(t, issueID, 30)
	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.ReactionAttempts[mcKey] = 2
	state.PendingReactions[mcKey].CreatedAt = mcBaseTime.Add(-40 * time.Minute)
	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.ReactionAttempts[ciKey] = 5
	delete(state.Claimed, issueID)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{}
	params := mergeConflictParams(store, scm, nil)
	params.MergeConflictPendingTTL = 30 * time.Minute

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 (TTL exceeded before fetch)", scm.calls)
	}
	if _, ok := state.PendingReactions[mcKey]; ok {
		t.Error("PendingReactions[merge-conflict] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[mcKey]; ok {
		t.Error("ReactionAttempts[merge-conflict] present after drop-on-age; want removed")
	}
	if state.ReactionAttempts[ciKey] != 5 {
		t.Errorf("ReactionAttempts[ci] = %d, want 5 (untouched)", state.ReactionAttempts[ciKey])
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("Claimed present after drop-on-age; want absent")
	}
	if store.deleteRetryCalls != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", store.deleteRetryCalls)
	}
	if store.upsertCalls != 0 || store.getCalls != 0 || store.deleteCalls != 0 {
		t.Errorf("fingerprint calls = upsert:%d get:%d delete:%d, want all 0",
			store.upsertCalls, store.getCalls, store.deleteCalls)
	}
}

// TestReconcileMergeConflicts_WatchWindowZeroNeverDrops verifies that a
// MergeConflictPendingTTL of 0 (watch_window_ms: 0) never drops an entry on
// age, however old it is.
func TestReconcileMergeConflicts_WatchWindowZeroNeverDrops(t *testing.T) {
	t.Parallel()

	issueID := "MC-WW0"
	state := stateWithMergeConflict(t, issueID, 30)
	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[mcKey].CreatedAt = mcBaseTime.Add(-365 * 24 * time.Hour)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{}
	params := mergeConflictParams(store, scm, nil)
	params.MergeConflictPendingTTL = 0

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[mcKey]; !ok {
		t.Error("PendingReactions entry dropped with MergeConflictPendingTTL=0; want never dropped on age")
	}
}

// TestReconcileMergeConflicts_WatchWindowNonDefaultTakesEffect verifies that
// a configured window other than the default threshold actually gates the
// drop: an entry older than the configured window is dropped with its
// attempt counter released, and one younger survives.
func TestReconcileMergeConflicts_WatchWindowNonDefaultTakesEffect(t *testing.T) {
	t.Parallel()

	t.Run("older than configured window is dropped and counter released", func(t *testing.T) {
		t.Parallel()

		issueID := "MC-WWN-OLD"
		state := stateWithMergeConflict(t, issueID, 30)
		mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
		state.PendingReactions[mcKey].CreatedAt = mcBaseTime.Add(-6 * time.Minute)
		state.ReactionAttempts[mcKey] = 2

		store := newStatefulFingerprintStore()
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{}
		params := mergeConflictParams(store, scm, nil)
		params.MergeConflictPendingTTL = 5 * time.Minute

		reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[mcKey]; ok {
			t.Error("PendingReactions entry present past configured 5m window; want dropped")
		}
		if _, ok := state.ReactionAttempts[mcKey]; ok {
			t.Error("ReactionAttempts present past configured 5m window; want released")
		}
	})

	t.Run("younger than configured window survives", func(t *testing.T) {
		t.Parallel()

		issueID := "MC-WWN-NEW"
		state := stateWithMergeConflict(t, issueID, 30)
		mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
		state.PendingReactions[mcKey].CreatedAt = mcBaseTime.Add(-4 * time.Minute)

		store := newStatefulFingerprintStore()
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{}
		params := mergeConflictParams(store, scm, nil)
		params.MergeConflictPendingTTL = 5 * time.Minute

		reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[mcKey]; !ok {
			t.Error("PendingReactions entry dropped inside configured 5m window; want kept")
		}
	})
}

// TestReconcileMergeConflicts_WatchWindowElapsedLogsRenamedAttribute pins the
// drop-on-age log record: message text and the window_ms attribute name
// (renamed from ttl_ms). A regression that reverts the rename or the wording
// must fail this test.
func TestReconcileMergeConflicts_WatchWindowElapsedLogsRenamedAttribute(t *testing.T) {
	t.Parallel()

	issueID := "MC-WWLOG"
	state := stateWithMergeConflict(t, issueID, 30)
	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[mcKey].CreatedAt = mcBaseTime.Add(-31 * time.Minute)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{}
	params := mergeConflictParams(store, scm, nil)
	params.MergeConflictPendingTTL = 30 * time.Minute
	log, buf := logCapture()

	reconcileMergeConflicts(state, params, log, context.Background(), metrics)

	output := buf.String()
	const msg = "merge conflict watch window elapsed, dropping"
	assertLogLineHasIntAttr(t, output, msg, "window_ms", int(30*time.Minute/time.Millisecond))
	if strings.Contains(output, "ttl_ms") {
		t.Errorf("log output contains stale attribute %q: %s", "ttl_ms", output)
	}
	if strings.Contains(output, "exceeded ttl") {
		t.Errorf("log output contains stale message wording %q: %s", "exceeded ttl", output)
	}
}

// --- 5.1.12 Template map (AC6) ---

func TestBuildMergeConflictTemplateMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     *MergeConflictReactionData
		status   domain.PRMergeStatus
		wantBase string
	}{
		{
			name:     "develop base",
			data:     &MergeConflictReactionData{PRNumber: 42, Branch: "feature/x"},
			status:   domain.PRMergeStatus{HeadSHA: "abc123", BaseBranch: "develop"},
			wantBase: "develop",
		},
		{
			name:     "release base not hardcoded default",
			data:     &MergeConflictReactionData{PRNumber: 7, Branch: "feature/y"},
			status:   domain.PRMergeStatus{HeadSHA: "def456", BaseBranch: "release/2.0"},
			wantBase: "release/2.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildMergeConflictTemplateMap(tt.data, tt.status)

			if got["pr_number"] != tt.data.PRNumber {
				t.Errorf("buildMergeConflictTemplateMap()[pr_number] = %v, want %d", got["pr_number"], tt.data.PRNumber)
			}
			if got["branch"] != tt.data.Branch {
				t.Errorf("buildMergeConflictTemplateMap()[branch] = %v, want %q", got["branch"], tt.data.Branch)
			}
			if got["head_sha"] != tt.status.HeadSHA {
				t.Errorf("buildMergeConflictTemplateMap()[head_sha] = %v, want %q", got["head_sha"], tt.status.HeadSHA)
			}
			base, ok := got["base"]
			if !ok {
				t.Fatal("buildMergeConflictTemplateMap() missing base key; want present for missingkey=error")
			}
			if base != tt.wantBase {
				t.Errorf("buildMergeConflictTemplateMap()[base] = %v, want %q (the real base, not a hardcoded default)", base, tt.wantBase)
			}
		})
	}
}

// --- buildMergeConflictEscalationComment reports dispatched turns (attempts-1) ---

func TestBuildMergeConflictEscalationComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		data     *MergeConflictReactionData
		attempts int
		want     string
	}{
		{
			name:     "one dispatched turn at max_retries 1",
			data:     &MergeConflictReactionData{PRNumber: 42},
			attempts: 2,
			want:     "Sortie attempted 1 merge-conflict resolution turn(s) and could not clear the conflicts for PR #42. Manual rebase required.",
		},
		{
			name:     "zero dispatched turns at max_retries 0 (escalates without a rebase)",
			data:     &MergeConflictReactionData{PRNumber: 7},
			attempts: 1,
			want:     "Sortie attempted 0 merge-conflict resolution turn(s) and could not clear the conflicts for PR #7. Manual rebase required.",
		},
		{
			name:     "three dispatched turns at max_retries 3",
			data:     &MergeConflictReactionData{PRNumber: 99},
			attempts: 4,
			want:     "Sortie attempted 3 merge-conflict resolution turn(s) and could not clear the conflicts for PR #99. Manual rebase required.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildMergeConflictEscalationComment(tt.data, tt.attempts)
			if got != tt.want {
				t.Errorf("buildMergeConflictEscalationComment(attempts=%d) =\n  %q\nwant:\n  %q", tt.attempts, got, tt.want)
			}
		})
	}
}

// --- 5.1.13 Dispatch carries real base (AC6b orchestrator side) ---

func TestReconcileMergeConflicts_DispatchCarriesRealBase(t *testing.T) {
	t.Parallel()

	state := stateWithMergeConflict(t, "MC-BASE", 10)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-rel", "release/2.0"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	retry, ok := state.RetryAttempts["MC-BASE"]
	if !ok {
		t.Fatal("retry not scheduled; want dispatch carrying the real base")
	}
	raw, ok := retry.ContinuationContext["merge_conflict"]
	if !ok {
		t.Fatal(`ContinuationContext missing "merge_conflict" key`)
	}
	mergeContext, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("merge_conflict context type = %T, want map[string]any", raw)
	}
	if mergeContext["base"] != "release/2.0" {
		t.Errorf("ContinuationContext[merge_conflict][base] = %v, want %q (equals status.BaseBranch, no hardcoded default)",
			mergeContext["base"], "release/2.0")
	}
}

// --- 5.1.14 Coexists with auto-merge on the same tick (AC3) ---

func TestReconcileMergeConflicts_Coexists(t *testing.T) {
	t.Parallel()

	issueID := "MC-COEXIST"
	state := NewState(5000, 4, nil, AgentTotals{})

	// An auto-merge slot for the same PR, alongside the merge-conflict slot.
	mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.PendingReactions[mergeKey] = newAutoMergePending(issueID, 5)

	mcKey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[mcKey] = newMergeConflictPending(issueID, 5)
	state.Claimed[issueID] = struct{}{}

	store := newStatefulFingerprintStore()
	mcMetrics := newMergeConflictMetricsSpy()
	amMetrics := newAutoMergeMetricsSpy()

	// Dirty for both passes: auto-merge defers on dirty (no merge),
	// merge-conflict dispatches.
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return dirtyStatus("head-coexist", "main"), nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			t.Error("MergePR called while PR is dirty; auto-merge must defer, not merge")
			return domain.MergeResult{}, nil
		},
	}

	params := mergeConflictParams(store, scm, nil)
	params.AutoMergeConfig = defaultAutoMergeConfig()
	params.AutoMergeReactionConfigured = true

	// Run the merge-conflict pass then the auto-merge pass on the same tick,
	// in the wired order.
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), mcMetrics)
	reconcileAutoMerge(state, params, discardLogger(), context.Background(), amMetrics)

	// merge-conflict dispatched its continuation.
	if mcMetrics.checks["dispatched"] != 1 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1`, mcMetrics.checks["dispatched"])
	}
	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("merge-conflict continuation not scheduled on coexist tick; want scheduled")
	}
	// auto-merge deferred (its slot survives, no merge happened).
	if _, ok := state.PendingReactions[mergeKey]; !ok {
		t.Error("auto-merge slot consumed; want re-enqueued (deferred on dirty)")
	}
	if amMetrics.autoMerge["merged"] != 0 {
		t.Errorf(`IncAutoMergeReactions("merged") = %d, want 0 (no double action on dirty PR)`, amMetrics.autoMerge["merged"])
	}
}

// --- ReconcileRunningIssues wiring (AC5) ---

// TestReconcileRunningIssues_MergeConflictOrdering verifies that
// reconcileMergeConflicts is wired into ReconcileRunningIssues: a due
// merge-conflict entry triggers exactly one GetMergeability call per tick,
// and a not-yet-due entry triggers none.
func TestReconcileRunningIssues_MergeConflictOrdering(t *testing.T) {
	t.Parallel()

	t.Run("DueEntryTriggersOneRead", func(t *testing.T) {
		t.Parallel()

		state := stateWithMergeConflict(t, "MC-WIRE", 10)
		store := newStatefulFingerprintStore()
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
			return dirtyStatus("head-wire", "main"), nil
		}}
		params := mergeConflictParams(store, scm, nil)
		params.Metrics = metrics

		ReconcileRunningIssues(state, params)

		if scm.calls != 1 {
			t.Errorf("GetMergeability calls = %d, want 1 (one read per tick via ReconcileRunningIssues)", scm.calls)
		}
		if metrics.checks["dispatched"] != 1 {
			t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1 (reconcileMergeConflicts wired in)`, metrics.checks["dispatched"])
		}
	})

	t.Run("NotYetDueEntryTriggersNoRead", func(t *testing.T) {
		t.Parallel()

		state := stateWithMergeConflict(t, "MC-NOTDUE", 10)
		rkey := ReactionKey("MC-NOTDUE", ReactionKindMergeConflict)
		state.PendingReactions[rkey].PendingRetryAt = mcBaseTime.Add(5 * time.Minute)
		store := newStatefulFingerprintStore()
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
			return dirtyStatus("head-notdue", "main"), nil
		}}
		params := mergeConflictParams(store, scm, nil)
		params.Metrics = metrics

		ReconcileRunningIssues(state, params)

		if scm.calls != 0 {
			t.Errorf("GetMergeability calls = %d, want 0 (entry not yet due)", scm.calls)
		}
		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Error("PendingReactions entry dropped while not due; want re-enqueued")
		}
	})
}

// TestReconcileMergeConflicts_ForeignIncumbentDefers covers the deferral
// half of the retry-slot arbitration check at the top of the dirty
// branch: a foreign incumbent survives, MarkReactionDispatched is not
// called for the merge-conflict kind, and the merge-conflict
// ReactionAttempts counter does not move.
func TestReconcileMergeConflicts_ForeignIncumbentDefers(t *testing.T) {
	t.Parallel()

	const issueID = "MC-DEFER"
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      1,
		ReactionKind: ReactionKindCI,
	}

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-defer", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on a defer; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(mcBaseTime) {
		t.Errorf("CreatedAt = %v, want refreshed to %v", entry.CreatedAt, mcBaseTime)
	}
	incumbent := state.RetryAttempts[issueID]
	if incumbent.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q (incumbent unchanged)", incumbent.ReactionKind, ReactionKindCI)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[%s] = %d, want absent (a defer must not increment it)", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (no dispatch on a defer)", store.markDispatchedCalls)
	}
	if metrics.checks["dispatched"] != 0 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 0`, metrics.checks["dispatched"])
	}
}

// TestReconcileMergeConflicts_FreeSlotControlDispatches is the free-slot
// control paired with TestReconcileMergeConflicts_ForeignIncumbentDefers:
// the same dirty setup, but with no incumbent occupying the slot, results
// in a merge-conflict retry and exactly one MarkReactionDispatched call.
func TestReconcileMergeConflicts_FreeSlotControlDispatches(t *testing.T) {
	t.Parallel()

	const issueID = "MC-FREE"
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-free", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch; want consumed")
	}
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled on a free slot; want scheduled")
	}
	if retry.ReactionKind != ReactionKindMergeConflict {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindMergeConflict)
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// --- Attribution-gated per-episode reset ---

// mcAttributionStore wraps a *statefulFingerprintStore and overrides
// CountWorkerRunsCompletedSince with a configurable result, so a test can
// force a specific classifyHeadChange verdict while still exercising the
// real fingerprint upsert/get/dispatch semantics for the dedup path.
type mcAttributionStore struct {
	*statefulFingerprintStore

	countConfigured bool
	count           int
	countErr        error
	countCalls      int
}

var _ ReconcileStore = (*mcAttributionStore)(nil)

func (s *mcAttributionStore) CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error) {
	s.countCalls++
	if !s.countConfigured {
		return s.statefulFingerprintStore.CountWorkerRunsCompletedSince(ctx, issueID, since)
	}
	return s.count, s.countErr
}

// TestHandleMergeConflictDirty_AttributionGatedReset covers a new
// conflicting head classified positively not the orchestrator's own work
// resetting the per-episode counter before incrementing, so the reported
// attempt count is 1, and a head classified unknown leaving the counter
// to continue from its prior value. HeadRecordedAt is pre-seeded to
// simulate a boundary this process already recorded, since a live
// pending entry's own dirty-branch outcome always dispatches or
// escalates and is never itself re-enqueued.
func TestHandleMergeConflictDirty_AttributionGatedReset(t *testing.T) {
	t.Parallel()

	t.Run("a head change positively not the orchestrator's own work resets before incrementing", func(t *testing.T) {
		t.Parallel()

		const issueID = "MC-ATTR-NOTOURS"
		state := stateWithMergeConflict(t, issueID, 10)
		rkey := ReactionKey(issueID, ReactionKindMergeConflict)
		state.ReactionAttempts[rkey] = 5 // a prior-episode counter value
		entry := state.PendingReactions[rkey]
		entry.HeadRecordedAt = mcBaseTime.Add(-time.Hour)
		// No live session may be running for the predicate to reach the
		// store query at all.
		delete(state.Claimed, issueID)

		inner := newStatefulFingerprintStore()
		inner.fingerprints[issueID+":"+ReactionKindMergeConflict] = fingerprintRecord{
			fingerprint: buildMergeConflictFingerprint("head-old"), dispatched: true,
		}
		store := &mcAttributionStore{statefulFingerprintStore: inner, countConfigured: true, count: 0}
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
			return dirtyStatus("head-new", "main"), nil
		}}
		params := mergeConflictParams(store, scm, nil)
		params.MergeConflictConfig.MaxRetries = 10 // isolates the reset from an incidental budget exhaustion

		reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

		if got := state.ReactionAttempts[rkey]; got != 1 {
			t.Errorf("ReactionAttempts[%s] = %d, want 1 (reset from 5 then incremented once)", rkey, got)
		}
	})

	t.Run("a head change classified unknown continues from the prior value", func(t *testing.T) {
		t.Parallel()

		const issueID = "MC-ATTR-UNKNOWN"
		state := stateWithMergeConflict(t, issueID, 10)
		rkey := ReactionKey(issueID, ReactionKindMergeConflict)
		state.ReactionAttempts[rkey] = 5
		entry := state.PendingReactions[rkey]
		entry.HeadRecordedAt = mcBaseTime.Add(-time.Hour)
		delete(state.Claimed, issueID)

		inner := newStatefulFingerprintStore()
		inner.fingerprints[issueID+":"+ReactionKindMergeConflict] = fingerprintRecord{
			fingerprint: buildMergeConflictFingerprint("head-old"), dispatched: true,
		}
		// countConfigured left false: the embedded default answers
		// unknown via a non-nil error, matching every failure path.
		store := &mcAttributionStore{statefulFingerprintStore: inner}
		metrics := newMergeConflictMetricsSpy()
		scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
			return dirtyStatus("head-new", "main"), nil
		}}
		params := mergeConflictParams(store, scm, nil)
		params.MergeConflictConfig.MaxRetries = 10 // isolates the non-reset from an incidental budget exhaustion

		reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

		if got := state.ReactionAttempts[rkey]; got != 6 {
			t.Errorf("ReactionAttempts[%s] = %d, want 6 (continues from 5, incremented once)", rkey, got)
		}
	})
}

// TestHandleMergeConflictDirty_SameHeadDedupPrecedesAttributionQuery
// covers a same-head re-observation still returning before the counter
// increment, and now also before the attribution query, so no
// head-unchanged pass reaches the worker-run-count store method.
func TestHandleMergeConflictDirty_SameHeadDedupPrecedesAttributionQuery(t *testing.T) {
	t.Parallel()

	const issueID = "MC-ATTR-DEDUP"
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.ReactionAttempts[rkey] = 5
	entry := state.PendingReactions[rkey]
	entry.HeadRecordedAt = mcBaseTime.Add(-time.Hour)

	inner := newStatefulFingerprintStore()
	// The stored fingerprint already matches the head this tick
	// observes, and it was already dispatched for.
	inner.fingerprints[issueID+":"+ReactionKindMergeConflict] = fingerprintRecord{
		fingerprint: buildMergeConflictFingerprint("head-same"), dispatched: true,
	}
	store := &mcAttributionStore{statefulFingerprintStore: inner}
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("head-same", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if got := state.ReactionAttempts[rkey]; got != 5 {
		t.Errorf("ReactionAttempts[%s] = %d, want unchanged 5 (dedup must not increment)", rkey, got)
	}
	if store.countCalls != 0 {
		t.Errorf("CountWorkerRunsCompletedSince calls = %d, want 0 (dedup precedes the attribution query)", store.countCalls)
	}
}

// --- Triage gate integration ---

// mergeConflictTriageParams returns mergeConflictParams wired with a
// real workspace and the given triage script, so reactionTriageGate
// actually starts a subprocess for the pass's dirty head.
func mergeConflictTriageParams(t *testing.T, store ReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter, workspaceRoot, script string) ReconcileParams {
	t.Helper()
	params := mergeConflictParams(store, scm, tracker)
	params.WorkspaceRoot = workspaceRoot
	params.MergeConflictConfig.Triage = config.ReactionTriageConfig{Script: writeHookScript(t, script), TimeoutMS: 5000}
	return params
}

// runMergeConflictTriageToCompletion drives a pass that starts a triage
// run for issueID, waits for the subprocess to finish, then resets the
// entry's PendingRetryAt to the past so the next pass is immediately
// due.
func runMergeConflictTriageToCompletion(t *testing.T, state *State, params ReconcileParams, rkey string, metrics domain.Metrics) {
	t.Helper()
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}
}

// TestReconcileMergeConflicts_Triage_NoConfig_BehavesAsPinned verifies
// that a merge-conflict reaction with no triage block dispatches
// exactly as the pinned revision.
func TestReconcileMergeConflicts_Triage_NoConfig_BehavesAsPinned(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-OFF"
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-off", "main"), nil
	}}
	params := mergeConflictParams(store, scm, nil)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a scheduled continuation, want dropped (pinned behavior)")
	}
	if metrics.checks["dispatched"] != 1 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1 (the normal dispatch path marks it)", store.markDispatchedCalls)
	}
}

// TestReconcileMergeConflicts_Triage_WaitsWithoutProviderCall verifies
// that while a triage run is in flight, the pass re-enqueues without
// making a provider call and without incrementing PendingAttempts.
func TestReconcileMergeConflicts_Triage_WaitsWithoutProviderCall(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-WAIT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-wait", func() {})

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-wait", "main"), nil
	}}
	params := mergeConflictTriageParams(t, store, scm, nil, root, handledScript)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 while a triage run is in flight", scm.calls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped while waiting on triage, want re-enqueued")
	}
	if entry.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 (waiting is not a fetch error)", entry.PendingAttempts)
	}
}

// TestReconcileMergeConflicts_Triage_Handled verifies that a handled
// disposition marks the fingerprint dispatched and re-enqueues the
// entry with the poll interval, without spending a continuation,
// dispatching a rebase, or incrementing IncMergeConflictChecks("dispatched").
func TestReconcileMergeConflicts_Triage_Handled(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-HANDLED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-handled", "main"), nil
	}}
	params := mergeConflictTriageParams(t, store, scm, nil, root, handledScript)

	runMergeConflictTriageToCompletion(t, state, params, rkey, metrics)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if !entry.PendingRetryAt.After(mcBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v (re-enqueued with the poll interval)", entry.PendingRetryAt, mcBaseTime)
	}
	if !entry.HeadRecordedAt.IsZero() {
		t.Error("HeadRecordedAt set on a handled pass, want zero (the head-change block never ran)")
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a handled verdict must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.checks["dispatched"] != 0 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 0 on a handled pass`, metrics.checks["dispatched"])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileMergeConflicts_Triage_Escalate verifies that an escalate
// disposition invokes escalateMergeConflictFailure with
// EscalationTriggerTriage and the un-incremented attempt count.
func TestReconcileMergeConflicts_Triage_Escalate(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-ESCALATE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-escalate", "main"), nil
	}}
	params := mergeConflictTriageParams(t, store, scm, tracker, root, escalateTriageScript)

	runMergeConflictTriageToCompletion(t, state, params, rkey, metrics)

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (triage escalation uses the kind's own escalation action)", tracker.addLabelCalled)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a triage escalation must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (matches a budget escalation)")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileMergeConflicts_Triage_EpisodeCloseClearsHandledForNextEpisode
// pins cancelReactionTriage's detach-on-close contract: a memoized
// handled verdict from one episode must not survive into the next.
// Without it, a later episode that recomputes the identical fingerprint
// (a head reappearing after a revert, for instance) would replay the
// stale handled outcome from memory instead of running the newly
// configured command, silently suppressing the real verdict.
func TestReconcileMergeConflicts_Triage_EpisodeCloseClearsHandledForNextEpisode(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-EPISODE-CLOSE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-repeat", "main"), nil
	}}
	params := mergeConflictTriageParams(t, store, scm, nil, root, handledScript)

	// Episode 1: the PR is dirty at sha-repeat; the command answers
	// handled, so the verdict is memoized on pending.Triage rather than
	// re-run on the next pass over the same fingerprint.
	runMergeConflictTriageToCompletion(t, state, params, rkey, metrics)
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if entry.Triage == nil {
		t.Fatal("PendingReaction.Triage cleared inside its own episode, want the memoized handle retained")
	}
	entry.PendingRetryAt = time.Time{}

	// The episode closes: the PR resolves clean, taking the default
	// branch that cancels the retained handle before re-enqueueing.
	scm.fn = func() (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean}, nil
	}
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	entry, ok = state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped when the episode closed, want re-enqueued")
	}
	if entry.Triage != nil {
		t.Fatal("PendingReaction.Triage survived the episode close, want detached so a later identical fingerprint cannot replay it")
	}
	entry.PendingRetryAt = time.Time{}

	// Episode 2: the PR goes dirty again at the same head, recomputing
	// the identical fingerprint. The command now answers escalate; a
	// cleared handle must run it fresh rather than replay episode 1's
	// memoized handled verdict.
	scm.fn = func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-repeat", "main"), nil
	}
	params.MergeConflictConfig.Triage = config.ReactionTriageConfig{Script: writeHookScript(t, escalateTriageScript), TimeoutMS: 5000}

	runMergeConflictTriageToCompletion(t, state, params, rkey, metrics)
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (the replayed handled verdict from episode 1 must not suppress it)")
	}
	if store.markDispatchedCalls != 2 {
		t.Errorf("MarkReactionDispatched calls = %d, want 2 (one real verdict per episode: handled, then escalate)", store.markDispatchedCalls)
	}
}

// TestReconcileMergeConflicts_Triage_DispatchAgent_HeadChangeBlockRunsOnce
// verifies two things together: a dispatch-agent disposition falls
// through to the existing dispatch block, and classifyHeadChange's
// HeadRecordedAt write runs exactly once across the starting pass
// (which never reaches it) and the resuming pass (which does).
func TestReconcileMergeConflicts_Triage_DispatchAgent_HeadChangeBlockRunsOnce(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-DISPATCH"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{fn: func() (domain.PRMergeStatus, error) {
		return dirtyStatus("sha-dispatch", "main"), nil
	}}
	params := mergeConflictTriageParams(t, store, scm, nil, root, dispatchAgentTriageScript)

	// Starting pass: the gate returns triageWait above the head-change
	// block, so HeadRecordedAt must still be zero.
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	if !entry.HeadRecordedAt.IsZero() {
		t.Fatal("HeadRecordedAt set on the starting pass, want zero (the head-change block must not run before the gate resolves)")
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}

	// Resuming pass: dispatch-agent falls through, so the head-change
	// block runs exactly this once.
	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if metrics.checks["dispatched"] != 1 {
		t.Errorf(`IncMergeConflictChecks("dispatched") = %d, want 1`, metrics.checks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1 (the per-episode increment ran exactly once)", rkey, state.ReactionAttempts[rkey])
	}
	// dispatchMergeConflictContinuation itself calls MarkReactionDispatched
	// as part of its pre-existing dispatch path, independent of triage; the
	// gate's own markTriageDispatched runs only on handled or escalate.
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1 (from the normal dispatch path)", store.markDispatchedCalls)
	}
}

// TestReconcileMergeConflicts_Triage_CancelOnTTLDrop verifies that an
// in-flight triage run is cancelled before the entry is dropped on
// watch-window elapse. The TTL check runs ahead of the early
// wait-on-triage short-circuit, so it is the one drop path reachable
// while a run is genuinely still in flight; every other drop path
// (episode close, KindData mismatch) is reached only after the early
// short-circuit has already let a finished run through.
func TestReconcileMergeConflicts_Triage_CancelOnTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "MC-TRIAGE-DROP"
	state := stateWithMergeConflict(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindMergeConflict)
	spy := &triageCancelSpy{}
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-drop", spy.cancel)
	state.PendingReactions[rkey].CreatedAt = mcBaseTime.Add(-2 * time.Hour)

	store := newStatefulFingerprintStore()
	metrics := newMergeConflictMetricsSpy()
	scm := &mergeabilitySCM{}
	params := mergeConflictParams(store, scm, nil)
	params.MergeConflictPendingTTL = time.Hour

	reconcileMergeConflicts(state, params, discardLogger(), context.Background(), metrics)

	if spy.calls() != 1 {
		t.Errorf("Cancel called %d times, want 1 (the in-flight run must not outlive the dropped entry)", spy.calls())
	}
	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 (the TTL drop precedes the mergeability read)", scm.calls)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the watch window, want dropped")
	}
}
