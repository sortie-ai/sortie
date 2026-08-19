package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mgcTransitionCall records one TransitionIssue invocation.
type mgcTransitionCall struct {
	issueID string
	target  string
}

// mgcLabelCall records one AddLabel invocation.
type mgcLabelCall struct {
	issueID string
	label   string
}

// mgcCommentCall records one CommentIssue invocation.
type mgcCommentCall struct {
	issueID string
	text    string
}

// mgcTrackerFake is a fully controllable domain.TrackerAdapter for
// merge-completion reconcile tests: FetchIssueStatesByIDs returns a
// canned map or error, TransitionIssue returns a canned or per-call
// error, and AddLabel/CommentIssue record every escalation call.
type mgcTrackerFake struct {
	states    map[string]string
	statesErr error

	statesCalls atomic.Int32

	transitionErr   error
	transitionErrFn func(callNum int) error
	transitionCalls []mgcTransitionCall

	addLabelErr   error
	commentErr    error
	addLabelCalls []mgcLabelCall
	commentCalls  []mgcCommentCall

	// addLabelCtx and commentCtx capture the context of the last
	// AddLabel/CommentIssue call so tests can verify it is the same
	// context handed to a follow-up store write, not an independent one.
	addLabelCtx context.Context
	commentCtx  context.Context
}

var _ domain.TrackerAdapter = (*mgcTrackerFake)(nil)

func (f *mgcTrackerFake) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	return nil, nil
}
func (f *mgcTrackerFake) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (f *mgcTrackerFake) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (f *mgcTrackerFake) FetchIssueStatesByIdentifiers(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (f *mgcTrackerFake) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	return nil, nil
}

func (f *mgcTrackerFake) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	f.statesCalls.Add(1)
	if f.statesErr != nil {
		return nil, f.statesErr
	}
	return f.states, nil
}

func (f *mgcTrackerFake) TransitionIssue(_ context.Context, issueID, target string) error {
	f.transitionCalls = append(f.transitionCalls, mgcTransitionCall{issueID: issueID, target: target})
	if f.transitionErrFn != nil {
		return f.transitionErrFn(len(f.transitionCalls))
	}
	return f.transitionErr
}

func (f *mgcTrackerFake) CommentIssue(ctx context.Context, issueID, text string) error {
	f.commentCalls = append(f.commentCalls, mgcCommentCall{issueID: issueID, text: text})
	f.commentCtx = ctx
	return f.commentErr
}

func (f *mgcTrackerFake) AddLabel(ctx context.Context, issueID, label string) error {
	f.addLabelCalls = append(f.addLabelCalls, mgcLabelCall{issueID: issueID, label: label})
	f.addLabelCtx = ctx
	return f.addLabelErr
}

// mgcSCMFake is a controllable domain.SCMAdapter whose GetMergeability
// return value is supplied by a function field, so tests can vary the
// observed merge state per call.
type mgcSCMFake struct {
	fn    func(prNumber int, owner, repo string) (domain.PRMergeStatus, error)
	calls atomic.Int32
}

var _ domain.SCMAdapter = (*mgcSCMFake)(nil)

func (m *mgcSCMFake) GetMergeability(_ context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	m.calls.Add(1)
	if m.fn != nil {
		return m.fn(prNumber, owner, repo)
	}
	return domain.PRMergeStatus{}, nil
}
func (m *mgcSCMFake) FetchPendingReviews(context.Context, int, string, string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (m *mgcSCMFake) FetchBotReviewComments(context.Context, int, string, string, []string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (m *mgcSCMFake) GetReviewDecision(context.Context, int, string, string) (domain.ReviewDecision, error) {
	return "", nil
}
func (m *mgcSCMFake) GetCIStatus(context.Context, int, string, string) (string, error) {
	return "", nil
}
func (m *mgcSCMFake) MergePR(context.Context, int, string, string, domain.MergeStrategy, string, string, string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}
func (m *mgcSCMFake) DeleteBranch(context.Context, string, string, string) error { return nil }
func (m *mgcSCMFake) ListLabelEvents(context.Context, int, string, string) ([]domain.LabelEvent, error) {
	return nil, nil
}
func (m *mgcSCMFake) RemoveLabel(context.Context, int, string, string, string) error { return nil }

// mgcFingerprintRecord is one stored (issue, kind) fingerprint row.
type mgcFingerprintRecord struct {
	fingerprint string
	dispatched  bool
	updatedAt   time.Time
}

// mgcStoreFake is a ReconcileStore that genuinely persists the
// per-(issue,kind) fingerprint and dispatched flag, and deletes by
// issue prefix for real, so the exactly-once latch and the
// cross-kind-clear interaction are exercised against real stored
// state rather than canned values.
type mgcStoreFake struct {
	fingerprints map[string]mgcFingerprintRecord

	upsertErr          error
	getErr             error
	markErr            error
	deleteErr          error
	observeErr         error
	markObservationErr error

	upsertCalls          int
	getCalls             int
	markCalls            int
	observeCalls         int
	markObservationCalls int
	deleteCalls          int
	deleteRetryCalls     int

	// markObservationCtx captures the context of the last
	// MarkReactionObservationDispatched call so tests can verify it is
	// the same context handed to the preceding tracker write, not an
	// independent one.
	markObservationCtx context.Context
}

var _ ReconcileStore = (*mgcStoreFake)(nil)

func newMGCStore() *mgcStoreFake {
	return &mgcStoreFake{fingerprints: make(map[string]mgcFingerprintRecord)}
}

func (s *mgcStoreFake) SaveRetryEntry(context.Context, persistence.RetryEntry) error { return nil }
func (s *mgcStoreFake) DeleteRetryEntry(context.Context, string) error {
	s.deleteRetryCalls++
	return nil
}
func (s *mgcStoreFake) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (s *mgcStoreFake) UpsertReactionFingerprint(_ context.Context, issueID, kind, fingerprint string) error {
	s.upsertCalls++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	key := issueID + ":" + kind
	rec := s.fingerprints[key]
	if rec.fingerprint != fingerprint {
		rec.dispatched = false
	}
	rec.fingerprint = fingerprint
	s.fingerprints[key] = rec
	return nil
}

func (s *mgcStoreFake) GetReactionFingerprint(_ context.Context, issueID, kind string) (string, bool, error) {
	s.getCalls++
	if s.getErr != nil {
		return "", false, s.getErr
	}
	rec := s.fingerprints[issueID+":"+kind]
	return rec.fingerprint, rec.dispatched, nil
}

func (s *mgcStoreFake) MarkReactionDispatched(_ context.Context, issueID, kind string) error {
	s.markCalls++
	if s.markErr != nil {
		return s.markErr
	}
	key := issueID + ":" + kind
	rec := s.fingerprints[key]
	rec.dispatched = true
	s.fingerprints[key] = rec
	return nil
}

func (s *mgcStoreFake) DeleteReactionFingerprint(_ context.Context, issueID, kind string) error {
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.fingerprints, issueID+":"+kind)
	return nil
}

func (s *mgcStoreFake) UpsertReactionObservation(
	_ context.Context,
	issueID, kind, fingerprint string,
	observedAt time.Time,
) (persistence.ReactionObservation, error) {
	s.observeCalls++
	if s.observeErr != nil {
		return persistence.ReactionObservation{}, s.observeErr
	}
	key := issueID + ":" + kind
	rec, exists := s.fingerprints[key]
	if !exists || rec.fingerprint != fingerprint {
		rec = mgcFingerprintRecord{
			fingerprint: fingerprint,
			updatedAt:   observedAt.UTC(),
		}
		s.fingerprints[key] = rec
	}
	return persistence.ReactionObservation{
		FirstObservedAt: rec.updatedAt,
		Dispatched:      rec.dispatched,
	}, nil
}

func (s *mgcStoreFake) MarkReactionObservationDispatched(ctx context.Context, issueID, kind, fingerprint string) error {
	s.markObservationCalls++
	s.markObservationCtx = ctx
	if s.markObservationErr != nil {
		return s.markObservationErr
	}
	key := issueID + ":" + kind
	rec := s.fingerprints[key]
	if rec.fingerprint != fingerprint {
		return nil
	}
	rec.dispatched = true
	s.fingerprints[key] = rec
	return nil
}

// CountWorkerRunsCompletedSince returns a non-nil error, matching the
// conservative default [unsupportedReactionObservationStore] supplies
// elsewhere in this package: this merge-completion test double is not
// expected to answer an attribution query.
func (s *mgcStoreFake) CountWorkerRunsCompletedSince(context.Context, string, time.Time) (int, error) {
	return 0, errors.New("worker run count is unsupported by this test double")
}

// has reports the fingerprint record for the issue's merge-completion
// row, the only kind this test file's store ever populates.
func (s *mgcStoreFake) has(issueID string) (mgcFingerprintRecord, bool) {
	rec, ok := s.fingerprints[issueID+":"+ReactionKindMergeCompletion]
	return rec, ok
}

func (s *mgcStoreFake) missingSHAObservation(issueID string) (mgcFingerprintRecord, bool) {
	rec, ok := s.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind]
	return rec, ok
}

// --- Test helpers ---

// mgcBaseTime is a fixed reference time for merge-completion reconcile
// tests.
var mgcBaseTime = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

// mgcTransportError builds a retryable *domain.TrackerError for tests
// that only need a transport-class failure, not a specific message.
func mgcTransportError() error {
	return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "unavailable"}
}

// newMGCPending builds a PendingReaction with Kind=ReactionKindMergeCompletion,
// due immediately (zero PendingRetryAt).
func newMGCPending(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindMergeCompletion,
		CreatedAt:  mgcBaseTime,
		KindData: &MergeCompletionReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
		},
	}
}

// mgcStateWithPending creates a State with one merge-completion
// PendingReaction entry, not claimed.
func mgcStateWithPending(issueID string, prNumber int) *State {
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	s.PendingReactions[rkey] = newMGCPending(issueID, prNumber)
	return s
}

// defaultMGCConfig returns a MergeCompletionReactionConfig with the
// ADR defaults except MaxRetries, which the failure-routing tests
// override per case.
func defaultMGCConfig() MergeCompletionReactionConfig {
	return MergeCompletionReactionConfig{
		TargetState:     "done",
		PollIntervalMS:  60000,
		Escalation:      "label",
		EscalationLabel: "needs-human",
		MaxRetries:      2,
	}
}

// mgcParams returns ReconcileParams wired for merge-completion reconcile
// unit tests: the tracker reports the issue in the handoff state, the
// target state is a member of TerminalStates, and NowFunc is fixed at
// mgcBaseTime.
func mgcParams(store ReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter:                    tracker,
		SCMAdapter:                        scm,
		HandoffState:                      "In Review",
		TerminalStates:                    []string{"Done"},
		MergeCompletionConfig:             defaultMGCConfig(),
		MergeCompletionReactionConfigured: true,
		Store:                             store,
		OnRetryFire:                       noopRetryFire,
		Ctx:                               context.Background(),
		NowFunc:                           func() time.Time { return mgcBaseTime },
	}
}

// mergedStatus returns a PRMergeStatus reporting a completed merge with
// the given commit identifier.
func mergedStatus(sha string) domain.PRMergeStatus {
	return domain.PRMergeStatus{Merged: true, MergeCommitSHA: sha}
}

func mergedMissingSHAStatus() domain.PRMergeStatus {
	return domain.PRMergeStatus{Merged: true}
}

func TestMergeCompletionPRIdentityNormalizesRepository(t *testing.T) {
	t.Parallel()

	data := &MergeCompletionReactionData{Owner: " Sortie-AI ", Repo: " Sortie ", PRNumber: 777}
	if got := mergeCompletionPRIdentity(data); got != "sortie-ai/sortie#777" {
		t.Errorf("mergeCompletionPRIdentity() = %q, want %q", got, "sortie-ai/sortie#777")
	}
}

// Merge observed: one reconcile tick with a merged PR and a matching
// handoff-state issue transitions exactly once, marks the fingerprint
// dispatched, removes the pending entry, and posts no comment.

func TestReconcileMergeCompletion_MergedTransitionsOnce(t *testing.T) {
	t.Parallel()

	issueID := "MGC-1"
	state := mgcStateWithPending(issueID, 10)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-1"), nil }}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}
	if tracker.transitionCalls[0].target != "done" {
		t.Errorf("TransitionIssue target = %q, want %q", tracker.transitionCalls[0].target, "done")
	}
	rec, ok := store.has(issueID)
	if !ok || !rec.dispatched || rec.fingerprint != "sha-1" {
		t.Errorf("fingerprint record = %+v, ok=%v, want dispatched=true fingerprint=sha-1", rec, ok)
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, exists := state.PendingReactions[rkey]; exists {
		t.Error("PendingReactions entry still present after successful transition, want removed")
	}
	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue calls = %d, want 0 (no comment on a successful transition)", len(tracker.commentCalls))
	}
}

// An unmerged PR performs no transition and re-enqueues at the poll
// interval.

func TestReconcileMergeCompletion_UnmergedReenqueues(t *testing.T) {
	t.Parallel()

	issueID := "MGC-2"
	state := mgcStateWithPending(issueID, 11)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Merged: false}, nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry missing, want present (re-enqueued)")
	}
	wantNotBefore := mgcBaseTime.Add(time.Duration(params.MergeCompletionConfig.PollIntervalMS) * time.Millisecond)
	if pending.PendingRetryAt.Before(wantNotBefore) {
		t.Errorf("PendingRetryAt = %v, want at least one poll interval after %v", pending.PendingRetryAt, mgcBaseTime)
	}
}

// Three consecutive ticks, with the entry re-seeded before each by a
// simulated worker exit, transition exactly once in total because the
// stored fingerprint equals the observed merge commit and is dispatched.

func TestReconcileMergeCompletion_RepeatTicksTransitionOnce(t *testing.T) {
	t.Parallel()

	issueID := "MGC-3"
	state := mgcStateWithPending(issueID, 12)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-3"), nil }}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("after tick 1: TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}

	// A worker re-runs on the same issue (e.g. after a human comment) and
	// exits normally, re-seeding the pending entry each time.
	for i := 2; i <= 3; i++ {
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		state.PendingReactions[rkey] = newMGCPending(issueID, 12)
		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("after tick %d: TransitionIssue calls = %d, want 1 (dedup by fingerprint latch)", i, len(tracker.transitionCalls))
		}
	}
}

// A restart recovery pass over an already-dispatched fingerprint row,
// followed by a reconcile tick observing the same merge commit,
// transitions zero times.

func TestReconcileMergeCompletion_RecoveryOverDispatchedFingerprintNoTransition(t *testing.T) {
	t.Parallel()

	issueID := "MGC-4"
	identifier := "PROJ-MGC4"
	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, identifier, domain.SCMMetadata{
		Branch:   "feature/mgc4",
		SHA:      "headsha",
		PushedAt: freshSCMTime(1),
		PRNumber: 13,
		Owner:    "owner",
		Repo:     "repo",
	})

	store := newMGCStore()
	store.fingerprints[issueID+":"+ReactionKindMergeCompletion] = mgcFingerprintRecord{fingerprint: "sha-4", dispatched: true}

	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun(issueID, identifier, "owner/repo#13", 1)

	recoverParams := PendingReactionRecoveryParams{
		WorkspaceRoot:                     wsRoot,
		TrackerAdapter:                    tracker,
		HandoffState:                      "In Review",
		TerminalStates:                    []string{"Done"},
		SCMAdapter:                        &mgcSCMFake{},
		MergeCompletionReactionConfigured: true,
		RecoveryLookback:                  PendingReactionRecoveryLookback,
		MaxCandidates:                     PendingReactionRecoveryMaxCandidates,
		NowFunc:                           func() time.Time { return recoveryNow },
		Logger:                            discardLogger(),
	}
	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, recoverParams)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.MergeCompletionRecovered != 1 {
		t.Fatalf("MergeCompletionRecovered = %d, want 1", result.MergeCompletionRecovered)
	}

	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-4"), nil }}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 (fingerprint already dispatched for this merge commit)", len(tracker.transitionCalls))
	}
}

// A new merge commit identifier resets the dispatched flag through the
// existing upsert and produces exactly one further transition.

func TestReconcileMergeCompletion_NewMergeCommitResetsLatch(t *testing.T) {
	t.Parallel()

	issueID := "MGC-5"
	state := mgcStateWithPending(issueID, 14)
	store := newMGCStore()
	store.fingerprints[issueID+":"+ReactionKindMergeCompletion] = mgcFingerprintRecord{fingerprint: "sha-old", dispatched: true}

	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-new"), nil }}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1 (a different merge commit re-arms the latch)", len(tracker.transitionCalls))
	}
	rec, ok := store.has(issueID)
	if !ok || rec.fingerprint != "sha-new" || !rec.dispatched {
		t.Errorf("fingerprint record = %+v, ok=%v, want fingerprint=sha-new dispatched=true", rec, ok)
	}
}

// A merged PR reporting no merge commit identifier performs no
// transition, re-enqueues at the poll interval, and emits a WARN.

func TestReconcileMergeCompletion_MergedNoCommitSHAWarns(t *testing.T) {
	t.Parallel()

	issueID := "MGC-6"
	state := mgcStateWithPending(issueID, 15)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Merged: true, MergeCommitSHA: ""}, nil
	}}
	params := mgcParams(store, scm, tracker)

	log, buf := logCapture()
	reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry missing, want present (re-enqueued)")
	}
	if !strings.Contains(buf.String(), "merge_completion merged pull request reported no merge commit") {
		t.Errorf("log output = %q, want a WARN naming the empty merge commit condition", buf.String())
	}
}

func TestReconcileMergeCompletion_LongReviewDoesNotStartMissingSHAClock(t *testing.T) {
	t.Parallel()

	issueID := "MGC-LONG-REVIEW"
	state := mgcStateWithPending(issueID, 30)
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	state.PendingReactions[rkey].CreatedAt = mgcBaseTime.Add(-7 * 24 * time.Hour)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Merged: false}, nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry missing after a week-long review, want re-enqueued")
	}
	if state.PendingReactions[rkey].PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 for an unmerged pull request", state.PendingReactions[rkey].PendingAttempts)
	}
	if store.observeCalls != 0 {
		t.Errorf("UpsertReactionObservation calls = %d, want 0 while Merged=false", store.observeCalls)
	}
	if _, ok := store.missingSHAObservation(issueID); ok {
		t.Error("missing-SHA observation exists while Merged=false, want absent")
	}
	if len(tracker.addLabelCalls) != 0 || len(tracker.commentCalls) != 0 {
		t.Errorf("escalation calls = label:%d comment:%d, want none", len(tracker.addLabelCalls), len(tracker.commentCalls))
	}
}

func TestReconcileMergeCompletion_FirstMissingSHAObservationStartsAtMergeObservation(t *testing.T) {
	t.Parallel()

	issueID := "MGC-FIRST-MISSING"
	state := mgcStateWithPending(issueID, 31)
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	state.PendingReactions[rkey].CreatedAt = mgcBaseTime.Add(-14 * 24 * time.Hour)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	observation, ok := store.missingSHAObservation(issueID)
	if !ok {
		t.Fatal("missing-SHA observation absent after first merged-without-SHA response")
	}
	if !observation.updatedAt.Equal(mgcBaseTime) {
		t.Errorf("first observation time = %v, want reconcile time %v (not PendingReaction.CreatedAt)", observation.updatedAt, mgcBaseTime)
	}
	if observation.dispatched {
		t.Error("missing-SHA observation dispatched during grace period, want false")
	}
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry missing during grace period, want re-enqueued")
	}
}

func TestReconcileMergeCompletion_MissingSHAUsesBackoffAndIgnoresTransitionRetryLimit(t *testing.T) {
	t.Parallel()

	issueID := "MGC-MISSING-BACKOFF"
	state := mgcStateWithPending(issueID, 32)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	params := mgcParams(store, scm, tracker)
	params.MergeCompletionConfig.MaxRetries = 0
	now := mgcBaseTime
	params.NowFunc = func() time.Time { return now }

	for tick := 1; tick <= 3; tick++ {
		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		pending, ok := state.PendingReactions[rkey]
		if !ok {
			t.Fatalf("tick %d: PendingReactions entry missing during grace period", tick)
		}
		if pending.PendingAttempts != tick {
			t.Errorf("tick %d: PendingAttempts = %d, want %d", tick, pending.PendingAttempts, tick)
		}
		if pending.PendingRetryAt.Before(now.Add(time.Minute)) {
			t.Errorf("tick %d: PendingRetryAt = %v, want at least poll interval after %v", tick, pending.PendingRetryAt, now)
		}
		now = now.Add(10 * time.Minute)
	}

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 while SHA is missing", len(tracker.transitionCalls))
	}
	if len(tracker.addLabelCalls) != 0 || len(tracker.commentCalls) != 0 {
		t.Errorf("escalation calls = label:%d comment:%d, want none before grace expiry even with max_retries=0", len(tracker.addLabelCalls), len(tracker.commentCalls))
	}
}

func TestReconcileMergeCompletion_SHAAppearsDuringGraceTransitionsAndCleansObservation(t *testing.T) {
	t.Parallel()

	issueID := "MGC-SHA-APPEARS"
	state := mgcStateWithPending(issueID, 33)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	status := mergedMissingSHAStatus()
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return status, nil }}
	params := mgcParams(store, scm, tracker)
	now := mgcBaseTime
	params.NowFunc = func() time.Time { return now }

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	status = mergedStatus("sha-appeared")
	now = now.Add(5 * time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1 after the SHA appears", len(tracker.transitionCalls))
	}
	if _, ok := store.missingSHAObservation(issueID); ok {
		t.Error("missing-SHA observation remains after real SHA appeared, want cleaned")
	}
	record, ok := store.has(issueID)
	if !ok || record.fingerprint != "sha-appeared" || !record.dispatched {
		t.Errorf("normal merge fingerprint = %+v, ok=%v, want sha-appeared dispatched=true", record, ok)
	}

	state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)] = newMGCPending(issueID, 33)
	now = now.Add(time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls after repeat SHA = %d, want 1 total", len(tracker.transitionCalls))
	}
}

func TestReconcileMergeCompletion_ObservationCleanupFailureDoesNotBlockTransition(t *testing.T) {
	t.Parallel()

	issueID := "MGC-CLEANUP-BEST-EFFORT"
	state := mgcStateWithPending(issueID, 44)
	store := newMGCStore()
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#44",
		updatedAt:   mgcBaseTime.Add(-10 * time.Minute),
	}
	store.deleteErr = mgcTransportError()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedStatus("sha-cleanup-best-effort"), nil
	}}
	params := mgcParams(store, scm, tracker)

	log, buf := logCapture()
	reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1 despite observation cleanup failure", len(tracker.transitionCalls))
	}
	record, ok := store.has(issueID)
	if !ok || record.fingerprint != "sha-cleanup-best-effort" || !record.dispatched {
		t.Errorf("normal merge fingerprint = %+v, ok=%v, want sha-cleanup-best-effort dispatched=true", record, ok)
	}
	if _, ok := store.missingSHAObservation(issueID); !ok {
		t.Error("missing-SHA observation absent after its best-effort delete failed, want retained")
	}
	if !strings.Contains(buf.String(), "failed to clear merge_completion missing-SHA observation") {
		t.Errorf("log output = %q, want cleanup failure warning", buf.String())
	}
}

func TestReconcileMergeCompletion_TransitionFailurePreservesObservationUntilLatch(t *testing.T) {
	t.Parallel()

	issueID := "MGC-CLEANUP-AFTER-LATCH"
	state := mgcStateWithPending(issueID, 45)
	store := newMGCStore()
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#45",
		updatedAt:   mgcBaseTime.Add(-10 * time.Minute),
	}
	tracker := &mgcTrackerFake{
		states:        map[string]string{issueID: "In Review"},
		transitionErr: mgcTransportError(),
	}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedStatus("sha-transition-retry"), nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if store.deleteCalls != 0 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 0 before the normal transition latches", store.deleteCalls)
	}
	if _, ok := store.missingSHAObservation(issueID); !ok {
		t.Error("missing-SHA observation absent after transition failure, want preserved until latch")
	}
	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; !ok {
		t.Error("PendingReactions entry missing after retryable transition failure, want re-enqueued")
	}
}

func TestReconcileMergeCompletion_MissingSHAExpiryEscalatesOnceAndStops(t *testing.T) {
	t.Parallel()

	issueID := "MGC-MISSING-EXPIRED"
	state := mgcStateWithPending(issueID, 34)
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	params := mgcParams(store, scm, tracker)
	params.MergeCompletionConfig.Escalation = "comment"
	now := mgcBaseTime
	params.NowFunc = func() time.Time { return now }

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	now = now.Add(31 * time.Minute)
	log, buf := logCapture()
	reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})
	state.TrackerOpsWg.Wait()

	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry remains after missing-SHA grace expiry, want removed")
	}
	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 for missing-SHA expiry", len(tracker.transitionCalls))
	}
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue calls = %d, want exactly 1", len(tracker.commentCalls))
	}
	comment := tracker.commentCalls[0].text
	for _, want := range []string{"owner/repo", "PR #34", "31m0s", "no merge commit identifier", "manually"} {
		if !strings.Contains(comment, want) {
			t.Errorf("escalation comment = %q, want it to contain %q", comment, want)
		}
	}
	observation, ok := store.missingSHAObservation(issueID)
	if !ok || !observation.dispatched {
		t.Errorf("missing-SHA observation = %+v, ok=%v, want dispatched=true after escalation delivery", observation, ok)
	}
	if !strings.Contains(buf.String(), "level=ERROR") ||
		!strings.Contains(buf.String(), "merge_completion stopped after merge commit identifier remained missing") {
		t.Errorf("log output = %q, want an ERROR naming the permanent polling stop", buf.String())
	}
	for _, want := range []string{"repository=owner/repo", "pr_number=34", "waited=31m0s", "manual_action="} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log output = %q, want operator context %q", buf.String(), want)
		}
	}

	// A fresh pending entry for the same PR stops from the durable marker and
	// never emits the escalation twice.
	state.PendingReactions[rkey] = newMGCPending(issueID, 34)
	now = now.Add(time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	state.TrackerOpsWg.Wait()
	if len(tracker.commentCalls) != 1 {
		t.Errorf("CommentIssue calls after fresh pending = %d, want 1 total", len(tracker.commentCalls))
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("fresh PendingReactions entry remains after already-delivered escalation, want stopped")
	}
}

func TestReconcileMergeCompletion_MissingSHAEscalationFailureStopsAndFreshPendingRetriesDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		escalation string
	}{
		{name: "label failure", escalation: "label"},
		{name: "comment failure", escalation: "comment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "MGC-MISSING-ESCALATION-" + tt.escalation
			state := mgcStateWithPending(issueID, 35)
			store := newMGCStore()
			tracker := &mgcTrackerFake{
				states:      map[string]string{issueID: "In Review"},
				addLabelErr: mgcTransportError(),
				commentErr:  mgcTransportError(),
			}
			scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
				return mergedMissingSHAStatus(), nil
			}}
			params := mgcParams(store, scm, tracker)
			params.MergeCompletionConfig.Escalation = tt.escalation
			now := mgcBaseTime
			params.NowFunc = func() time.Time { return now }

			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			now = now.Add(31 * time.Minute)
			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			state.TrackerOpsWg.Wait()

			rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Error("PendingReactions entry remains after failed escalation call, want stopped")
			}
			observation, ok := store.missingSHAObservation(issueID)
			if !ok || observation.dispatched {
				t.Errorf("missing-SHA observation = %+v, ok=%v, want dispatched=false after failed delivery", observation, ok)
			}

			calls := func() int {
				if tt.escalation == "label" {
					return len(tracker.addLabelCalls)
				}
				return len(tracker.commentCalls)
			}
			if got := calls(); got != 1 {
				t.Fatalf("external escalation calls after failure = %d, want 1", got)
			}

			// A later fresh entry in a new State simulates restart recovery and
			// retries only the undelivered operator signal.
			tracker.addLabelErr = nil
			tracker.commentErr = nil
			restartedState := mgcStateWithPending(issueID, 35)
			now = now.Add(time.Minute)
			reconcileMergeCompletion(restartedState, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			restartedState.TrackerOpsWg.Wait()
			if got := calls(); got != 2 {
				t.Fatalf("external escalation calls after fresh pending = %d, want 2", got)
			}
			observation, ok = store.missingSHAObservation(issueID)
			if !ok || !observation.dispatched {
				t.Errorf("missing-SHA observation after successful retry = %+v, ok=%v, want dispatched=true", observation, ok)
			}
			if !observation.updatedAt.Equal(mgcBaseTime) {
				t.Errorf("first observation after delivery retry = %v, want unchanged %v", observation.updatedAt, mgcBaseTime)
			}

			// Once delivery is recorded, another fresh entry stops without a
			// third tracker write.
			restartedState.PendingReactions[rkey] = newMGCPending(issueID, 35)
			now = now.Add(time.Minute)
			reconcileMergeCompletion(restartedState, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			restartedState.TrackerOpsWg.Wait()
			if got := calls(); got != 2 {
				t.Errorf("external escalation calls after delivered marker = %d, want 2 total", got)
			}
		})
	}
}

func TestReconcileMergeCompletion_MissingSHADeliveryMarkerFailureStillStops(t *testing.T) {
	t.Parallel()

	issueID := "MGC-MISSING-MARK-FAIL"
	state := mgcStateWithPending(issueID, 43)
	store := newMGCStore()
	store.markObservationErr = mgcTransportError()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	params := mgcParams(store, scm, tracker)
	now := mgcBaseTime
	params.NowFunc = func() time.Time { return now }

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	now = now.Add(31 * time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	state.TrackerOpsWg.Wait()

	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry remains after delivery marker write failed, want stopped")
	}
	observation, ok := store.missingSHAObservation(issueID)
	if !ok || observation.dispatched {
		t.Errorf("missing-SHA observation = %+v, ok=%v, want dispatched=false after mark failure", observation, ok)
	}
	if len(tracker.addLabelCalls) != 1 || len(tracker.commentCalls) != 0 {
		t.Errorf("external escalation calls = label:%d comment:%d, want 1,0 before the failed delivery mark", len(tracker.addLabelCalls), len(tracker.commentCalls))
	}

	state.PendingReactions[rkey] = newMGCPending(issueID, 43)
	now = now.Add(time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	state.TrackerOpsWg.Wait()
	if len(tracker.addLabelCalls) != 2 {
		t.Errorf("AddLabel calls after fresh pending = %d, want 2 because delivery was not durably marked", len(tracker.addLabelCalls))
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("fresh PendingReactions entry remains after repeated delivery marker failure, want stopped")
	}
}

func TestReconcileMergeCompletion_MissingSHAObservationSurvivesRestart(t *testing.T) {
	t.Parallel()

	issueID := "MGC-MISSING-RESTART"
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	now := mgcBaseTime
	params := mgcParams(store, scm, tracker)
	params.NowFunc = func() time.Time { return now }

	firstState := mgcStateWithPending(issueID, 36)
	reconcileMergeCompletion(firstState, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	// A new State simulates an orchestrator restart and recovery re-seed.
	restartedState := mgcStateWithPending(issueID, 36)
	now = now.Add(20 * time.Minute)
	reconcileMergeCompletion(restartedState, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	observation, ok := store.missingSHAObservation(issueID)
	if !ok || !observation.updatedAt.Equal(mgcBaseTime) {
		t.Errorf("observation after restart = %+v, ok=%v, want first observation time %v", observation, ok, mgcBaseTime)
	}

	now = mgcBaseTime.Add(31 * time.Minute)
	reconcileMergeCompletion(restartedState, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	restartedState.TrackerOpsWg.Wait()
	if len(tracker.addLabelCalls) != 1 {
		t.Errorf("AddLabel calls after total 31-minute wait across restart = %d, want 1", len(tracker.addLabelCalls))
	}
}

func TestReconcileMergeCompletion_RealSHAAfterEscalationTransitionsNormally(t *testing.T) {
	t.Parallel()

	issueID := "MGC-SHA-AFTER-ESCALATION"
	state := mgcStateWithPending(issueID, 37)
	store := newMGCStore()
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#37",
		dispatched:  true,
		updatedAt:   mgcBaseTime.Add(-time.Hour),
	}
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedStatus("sha-late"), nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1 after a real SHA appears", len(tracker.transitionCalls))
	}
	if _, ok := store.missingSHAObservation(issueID); ok {
		t.Error("escalated missing-SHA observation remains after real SHA appears, want cleaned")
	}
	record, ok := store.has(issueID)
	if !ok || record.fingerprint != "sha-late" || !record.dispatched {
		t.Errorf("normal merge fingerprint = %+v, ok=%v, want sha-late dispatched=true", record, ok)
	}
}

func TestReconcileMergeCompletion_NewPRIdentityStartsNewMissingSHAObservation(t *testing.T) {
	t.Parallel()

	issueID := "MGC-NEW-PR"
	state := mgcStateWithPending(issueID, 39)
	store := newMGCStore()
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#38",
		dispatched:  true,
		updatedAt:   mgcBaseTime.Add(-time.Hour),
	}
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return mergedMissingSHAStatus(), nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	observation, ok := store.missingSHAObservation(issueID)
	if !ok {
		t.Fatal("missing-SHA observation absent for new PR identity")
	}
	if observation.fingerprint != "owner/repo#39" || observation.dispatched || !observation.updatedAt.Equal(mgcBaseTime) {
		t.Errorf("new PR observation = %+v, want fingerprint=owner/repo#39 dispatched=false first_seen=%v", observation, mgcBaseTime)
	}
	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; !ok {
		t.Error("PendingReactions entry missing for new PR during fresh grace period")
	}
}

func TestReconcileMergeCompletion_NewUnmergedPRClearsOldObservationWithoutStartingOne(t *testing.T) {
	t.Parallel()

	issueID := "MGC-NEW-UNMERGED-PR"
	state := mgcStateWithPending(issueID, 42)
	store := newMGCStore()
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#41",
		dispatched:  true,
		updatedAt:   mgcBaseTime.Add(-time.Hour),
	}
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Merged: false}, nil
	}}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := store.missingSHAObservation(issueID); ok {
		t.Error("old missing-SHA observation remains after pending entry switched to a new unmerged PR")
	}
	if store.observeCalls != 0 {
		t.Errorf("UpsertReactionObservation calls = %d, want 0 before the new PR is merged", store.observeCalls)
	}
	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; !ok {
		t.Error("PendingReactions entry missing for new unmerged PR, want normal review wait")
	}
}

// A tracker state-read failure performs no forge call and backs off
// every due entry with an increased PendingAttempts.

func TestReconcileMergeCompletion_StateReadFailureBacksOffWithoutForgeCall(t *testing.T) {
	t.Parallel()

	issueID := "MGC-7"
	state := mgcStateWithPending(issueID, 16)
	store := newMGCStore()
	tracker := &mgcTrackerFake{statesErr: mgcTransportError()}
	scm := &mgcSCMFake{}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if got := scm.calls.Load(); got != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 (state read failed before any forge call)", got)
	}
	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry missing after a state-read failure, want re-enqueued")
	}
	if pending.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1", pending.PendingAttempts)
	}
}

// A transport-error transition retries with backoff while the call
// count stays within max_retries, then escalates exactly once after
// the count exceeds it.

func TestReconcileMergeCompletion_TransportErrorRetriesThenEscalates(t *testing.T) {
	t.Parallel()

	issueID := "MGC-8"
	state := mgcStateWithPending(issueID, 17)
	store := newMGCStore()
	tracker := &mgcTrackerFake{
		states:        map[string]string{issueID: "In Review"},
		transitionErr: mgcTransportError(),
	}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-8"), nil }}
	params := mgcParams(store, scm, tracker)
	params.MergeCompletionConfig.MaxRetries = 2

	now := mgcBaseTime
	params.NowFunc = func() time.Time { return now }

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("after tick 1: TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}
	if len(tracker.addLabelCalls) != 0 {
		t.Fatalf("after tick 1: AddLabel calls = %d, want 0 (no escalation before max_retries is exceeded)", len(tracker.addLabelCalls))
	}

	now = now.Add(10 * time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	if len(tracker.transitionCalls) != 2 {
		t.Fatalf("after tick 2: TransitionIssue calls = %d, want 2", len(tracker.transitionCalls))
	}
	if len(tracker.addLabelCalls) != 0 {
		t.Fatalf("after tick 2: AddLabel calls = %d, want 0", len(tracker.addLabelCalls))
	}

	now = now.Add(10 * time.Minute)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
	state.TrackerOpsWg.Wait()
	if len(tracker.transitionCalls) != 3 {
		t.Fatalf("after tick 3: TransitionIssue calls = %d, want 3", len(tracker.transitionCalls))
	}
	if len(tracker.addLabelCalls) != 1 {
		t.Errorf("after tick 3: AddLabel calls = %d, want 1 (escalation once attempts exceed max_retries)", len(tracker.addLabelCalls))
	}
}

// Auth and payload transition failures escalate immediately, consuming
// no retry budget and leaving no re-enqueued entry.

func TestReconcileMergeCompletion_AuthAndPayloadEscalateImmediately(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind domain.TrackerErrorKind
	}{
		{"auth error", domain.ErrTrackerAuth},
		{"payload error", domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "MGC-9-" + string(tt.kind)
			state := mgcStateWithPending(issueID, 18)
			store := newMGCStore()
			tracker := &mgcTrackerFake{
				states:        map[string]string{issueID: "In Review"},
				transitionErr: &domain.TrackerError{Kind: tt.kind, Message: "denied"},
			}
			scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-9"), nil }}
			params := mgcParams(store, scm, tracker)

			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			state.TrackerOpsWg.Wait()

			if len(tracker.transitionCalls) != 1 {
				t.Errorf("TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
			}
			if len(tracker.addLabelCalls) != 1 {
				t.Errorf("AddLabel calls = %d, want 1 (immediate escalation)", len(tracker.addLabelCalls))
			}
			rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Error("PendingReactions entry present after escalation, want dropped (no re-enqueue)")
			}
			if _, ok := state.ReactionAttempts[rkey]; ok {
				t.Error("ReactionAttempts counter present after escalation, want cleared")
			}
		})
	}
}

// A not-found transition stops without escalating, marks the
// fingerprint dispatched, and drops the entry.

func TestReconcileMergeCompletion_NotFoundStopsWithoutEscalating(t *testing.T) {
	t.Parallel()

	issueID := "MGC-11"
	state := mgcStateWithPending(issueID, 19)
	store := newMGCStore()
	tracker := &mgcTrackerFake{
		states:        map[string]string{issueID: "In Review"},
		transitionErr: &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "gone"},
	}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-11"), nil }}
	params := mgcParams(store, scm, tracker)

	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}
	if len(tracker.addLabelCalls) != 0 || len(tracker.commentCalls) != 0 {
		t.Errorf("escalation calls (label=%d, comment=%d), want 0", len(tracker.addLabelCalls), len(tracker.commentCalls))
	}
	rec, ok := store.has(issueID)
	if !ok || !rec.dispatched {
		t.Errorf("fingerprint record = %+v, ok=%v, want dispatched=true", rec, ok)
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry present after not-found stop, want dropped")
	}
}

// Escalation dispatches AddLabel for "label" and CommentIssue for
// "comment", never touches an issue-wide reaction slot, and a sibling CI
// pending entry for the same issue survives.

func TestReconcileMergeCompletion_EscalationDispatchesConfiguredAction(t *testing.T) {
	t.Parallel()

	t.Run("label escalation calls AddLabel with the configured label", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-12-LABEL"
		state := mgcStateWithPending(issueID, 20)
		ciKey := ReactionKey(issueID, ReactionKindCI)
		state.PendingReactions[ciKey] = &PendingReaction{
			IssueID: issueID, Identifier: issueID + "-ident", Kind: ReactionKindCI,
			CreatedAt: mgcBaseTime, KindData: &CIReactionData{Branch: "feature/x"},
		}

		store := newMGCStore()
		tracker := &mgcTrackerFake{
			states:        map[string]string{issueID: "In Review"},
			transitionErr: &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "denied"},
		}
		scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-12"), nil }}
		params := mgcParams(store, scm, tracker)
		params.MergeCompletionConfig.Escalation = "label"
		params.MergeCompletionConfig.EscalationLabel = "needs-human"

		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
		state.TrackerOpsWg.Wait()

		if len(tracker.addLabelCalls) != 1 || tracker.addLabelCalls[0].label != "needs-human" {
			t.Fatalf("AddLabel calls = %+v, want one call with label %q", tracker.addLabelCalls, "needs-human")
		}
		if len(tracker.commentCalls) != 0 {
			t.Errorf("CommentIssue calls = %d, want 0", len(tracker.commentCalls))
		}
		if _, ok := state.PendingReactions[ciKey]; !ok {
			t.Error("sibling CI PendingReaction removed by merge-completion escalation; want untouched")
		}
		if _, claimed := state.Claimed[issueID]; claimed {
			t.Error("state.Claimed gained an entry from merge-completion escalation; want untouched")
		}
	})

	t.Run("comment escalation calls CommentIssue naming the PR and target state", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-12-COMMENT"
		state := mgcStateWithPending(issueID, 21)
		store := newMGCStore()
		tracker := &mgcTrackerFake{
			states:        map[string]string{issueID: "In Review"},
			transitionErr: &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "no transition"},
		}
		scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-13"), nil }}
		params := mgcParams(store, scm, tracker)
		params.MergeCompletionConfig.Escalation = "comment"

		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
		state.TrackerOpsWg.Wait()

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue calls = %d, want 1", len(tracker.commentCalls))
		}
		text := tracker.commentCalls[0].text
		if !strings.Contains(text, "#21") {
			t.Errorf("escalation comment = %q, want it to name PR #21", text)
		}
		if !strings.Contains(text, "done") {
			t.Errorf("escalation comment = %q, want it to name the target state %q", text, "done")
		}
		if len(tracker.addLabelCalls) != 0 {
			t.Errorf("AddLabel calls = %d, want 0", len(tracker.addLabelCalls))
		}
	})
}

// Drop conditions: a non-handoff non-terminal state drops the entry
// with no forge call; a terminal state drops it with a distinct log
// message; a claimed issue re-enqueues without a forge call.

func TestReconcileMergeCompletion_DropConditions(t *testing.T) {
	t.Parallel()

	t.Run("non-handoff non-terminal state drops with no forge call", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-13-STALE"
		state := mgcStateWithPending(issueID, 22)
		store := newMGCStore()
		store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
			fingerprint: "owner/repo#22", updatedAt: mgcBaseTime,
		}
		tracker := &mgcTrackerFake{states: map[string]string{issueID: "Backlog"}}
		scm := &mgcSCMFake{}
		params := mgcParams(store, scm, tracker)

		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

		if got := scm.calls.Load(); got != 0 {
			t.Errorf("GetMergeability calls = %d, want 0", got)
		}
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry present, want dropped (state differs from handoff)")
		}
		if _, ok := store.missingSHAObservation(issueID); ok {
			t.Error("missing-SHA observation remains after issue left handoff state, want cleaned")
		}
	})

	t.Run("terminal state drops with the terminal-specific log message", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-13-TERMINAL"
		state := mgcStateWithPending(issueID, 23)
		store := newMGCStore()
		store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
			fingerprint: "owner/repo#23", updatedAt: mgcBaseTime,
		}
		tracker := &mgcTrackerFake{states: map[string]string{issueID: "Done"}}
		scm := &mgcSCMFake{}
		params := mgcParams(store, scm, tracker)

		log, buf := logCapture()
		reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})

		if got := scm.calls.Load(); got != 0 {
			t.Errorf("GetMergeability calls = %d, want 0", got)
		}
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry present, want dropped (issue already terminal)")
		}
		if !strings.Contains(buf.String(), "merge_completion issue already terminal, dropping") {
			t.Errorf("log output = %q, want the terminal-specific message", buf.String())
		}
		if strings.Contains(buf.String(), "left the handoff state") {
			t.Errorf("log output = %q, want the terminal message, not the left-the-handoff-state message", buf.String())
		}
		if _, ok := store.missingSHAObservation(issueID); ok {
			t.Error("missing-SHA observation remains after issue became terminal, want cleaned")
		}
	})

	t.Run("a claimed issue re-enqueues without a forge call", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-13-CLAIMED"
		state := mgcStateWithPending(issueID, 24)
		state.Claimed[issueID] = struct{}{}
		store := newMGCStore()
		tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
		scm := &mgcSCMFake{}
		params := mgcParams(store, scm, tracker)

		reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

		if got := scm.calls.Load(); got != 0 {
			t.Errorf("GetMergeability calls = %d, want 0", got)
		}
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Error("PendingReactions entry missing, want re-enqueued while claimed")
		}
	})
}

func TestReconcileMergeCompletion_DefinitiveStopCleansMissingSHAObservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		states     map[string]string
		mergeError error
	}{
		{
			name:   "issue missing from tracker response",
			states: map[string]string{},
		},
		{
			name:   "pull request not found",
			states: map[string]string{"MGC-CLEANUP": "In Review"},
			mergeError: &domain.SCMError{
				Kind:    domain.ErrSCMNotFound,
				Message: "gone",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "MGC-CLEANUP"
			state := mgcStateWithPending(issueID, 40)
			store := newMGCStore()
			store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
				fingerprint: "owner/repo#40", updatedAt: mgcBaseTime,
			}
			tracker := &mgcTrackerFake{states: tt.states}
			scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
				return domain.PRMergeStatus{}, tt.mergeError
			}}
			params := mgcParams(store, scm, tracker)

			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

			if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; ok {
				t.Error("PendingReactions entry remains after definitive stop, want removed")
			}
			if _, ok := store.missingSHAObservation(issueID); ok {
				t.Error("missing-SHA observation remains after definitive stop, want cleaned")
			}
		})
	}
}

// A MarkReactionDispatched failure drops the entry without a
// re-enqueue; a re-seeded second tick performs a second transition,
// the stated residue of this posture.

func TestReconcileMergeCompletion_MarkDispatchedFailureIsAcceptedResidue(t *testing.T) {
	t.Parallel()

	issueID := "MGC-22"
	state := mgcStateWithPending(issueID, 25)
	store := newMGCStore()
	store.markErr = mgcTransportError()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) { return mergedStatus("sha-22"), nil }}
	params := mgcParams(store, scm, tracker)

	log, buf := logCapture()
	reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("after tick 1: TransitionIssue calls = %d, want 1", len(tracker.transitionCalls))
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry present after a mark-dispatched failure, want dropped (not re-enqueued)")
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts counter present after a mark-dispatched failure, want cleared")
	}
	if !strings.Contains(buf.String(), "merge_completion dispatched mark failed") || !strings.Contains(buf.String(), "sha-22") {
		t.Errorf("log output = %q, want a WARN naming the merge commit sha-22", buf.String())
	}

	// A re-seeded second tick (a simulated worker exit) performs a second
	// transition: the accepted residue of this posture, since the row was
	// never durably marked dispatched.
	state.PendingReactions[rkey] = newMGCPending(issueID, 25)
	reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(tracker.transitionCalls) != 2 {
		t.Errorf("after tick 2: TransitionIssue calls = %d, want 2 (the accepted residue)", len(tracker.transitionCalls))
	}
}

// SweepWorkspaces collects a workspace as terminal only when the
// tracker-reported state is a member of the written TerminalStates
// list, not a defaulted one.

func TestReconcileMergeCompletion_SweepCollectsOnlyTheWrittenTerminalList(t *testing.T) {
	t.Parallel()

	t.Run("target_state present in terminal_states is collected as terminal", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-MGC-SWEEP-1"))
		tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-MGC-SWEEP-1": "done"}}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, SweepWorkspacesParams{
			WorkspaceRoot:  tmpDir,
			TrackerAdapter: tracker,
			TerminalStates: []string{"done"},
			Ctx:            context.Background(),
			Logger:         discardLogger(),
			Metrics:        &domain.NoopMetrics{},
		})

		assertSweepDirRemoved(t, filepath.Join(tmpDir, "PROJ-MGC-SWEEP-1"))
	})

	t.Run("the same state with terminal_states emptied is treated as non-terminal", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-MGC-SWEEP-2"))
		tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-MGC-SWEEP-2": "done"}}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, SweepWorkspacesParams{
			WorkspaceRoot:  tmpDir,
			TrackerAdapter: tracker,
			TerminalStates: nil,
			Ctx:            context.Background(),
			Logger:         discardLogger(),
			Metrics:        &domain.NoopMetrics{},
		})

		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-MGC-SWEEP-2"))
	})
}

// A reloaded terminal-states list that drops the configured target
// state produces exactly one WARN per onset, suppressed while the
// condition persists and cleared once the target is present again, with
// no change to any entry's disposition.

func TestReconcileMergeCompletion_TerminalDriftWarningFiresOncePerOnset(t *testing.T) {
	t.Parallel()

	issueID := "MGC-25"
	store := newMGCStore()
	tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
	scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
		return domain.PRMergeStatus{Merged: false}, nil
	}}

	state := mgcStateWithPending(issueID, 27)
	now := mgcBaseTime

	const driftMessage = "merge_completion target state is not in the configured terminal states"

	tick := func(terminalStates []string) string {
		rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; !ok {
			state.PendingReactions[rkey] = newMGCPending(issueID, 27)
		} else {
			state.PendingReactions[rkey].PendingRetryAt = time.Time{}
		}

		params := mgcParams(store, scm, tracker)
		params.TerminalStates = terminalStates
		params.NowFunc = func() time.Time { return now }

		log, buf := logCapture()
		reconcileMergeCompletion(state, params, log, context.Background(), &domain.NoopMetrics{})
		now = now.Add(10 * time.Minute)
		return buf.String()
	}

	if out := tick(nil); !strings.Contains(out, driftMessage) {
		t.Fatalf("tick 1 (drift onset) log = %q, want one WARN naming the drift", out)
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry missing after tick 1, want unchanged disposition (re-enqueued)")
	}

	if out := tick(nil); strings.Contains(out, driftMessage) {
		t.Errorf("tick 2 (same drift) log = %q, want no further WARN (suppressed)", out)
	}

	if out := tick([]string{"done"}); strings.Contains(out, driftMessage) {
		t.Errorf("tick 3 (drift cleared) log = %q, want no WARN", out)
	}

	if out := tick(nil); !strings.Contains(out, driftMessage) {
		t.Errorf("tick 4 (second onset) log = %q, want a second WARN", out)
	}
}

// TestReconcileTrackerState_TerminalReleasePreservesFingerprints verifies
// that generic terminal release leaves both the normal merge fingerprint and
// the internal missing-SHA observation byte-identical.
func TestReconcileTrackerState_TerminalReleasePreservesFingerprints(t *testing.T) {
	t.Parallel()

	issueID := "MGC-TERM"
	state := mgcStateWithPending(issueID, 10)
	state.Claimed[issueID] = struct{}{}

	store := newMGCStore()
	if err := store.UpsertReactionFingerprint(context.Background(), issueID, ReactionKindMergeCompletion, "sha-abc123"); err != nil {
		t.Fatalf("seed UpsertReactionFingerprint: %v", err)
	}
	if err := store.MarkReactionDispatched(context.Background(), issueID, ReactionKindMergeCompletion); err != nil {
		t.Fatalf("seed MarkReactionDispatched: %v", err)
	}
	before, ok := store.has(issueID)
	if !ok {
		t.Fatal("fingerprint row not seeded")
	}
	store.fingerprints[issueID+":"+mergeCompletionMissingSHAObservationKind] = mgcFingerprintRecord{
		fingerprint: "owner/repo#10",
		dispatched:  true,
		updatedAt:   mgcBaseTime,
	}
	beforeObservation, ok := store.missingSHAObservation(issueID)
	if !ok {
		t.Fatal("missing-SHA observation not seeded")
	}
	store.upsertCalls = 0
	store.getCalls = 0
	store.markCalls = 0
	store.deleteCalls = 0

	tracker := &mgcTrackerFake{states: map[string]string{issueID: "Done"}}
	scm := &mgcSCMFake{}
	params := mgcParams(store, scm, tracker)

	reconcileTrackerState(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if store.upsertCalls != 0 || store.getCalls != 0 || store.markCalls != 0 || store.deleteCalls != 0 {
		t.Errorf("fingerprint store calls = upsert:%d get:%d mark:%d delete:%d, want all zero",
			store.upsertCalls, store.getCalls, store.markCalls, store.deleteCalls)
	}
	after, ok := store.has(issueID)
	if !ok {
		t.Fatal("fingerprint row deleted by terminal release; want preserved")
	}
	if after != before {
		t.Errorf("fingerprint row = %+v, want unchanged %+v", after, before)
	}
	afterObservation, ok := store.missingSHAObservation(issueID)
	if !ok || afterObservation != beforeObservation {
		t.Errorf("missing-SHA observation = %+v, ok=%v, want unchanged %+v", afterObservation, ok, beforeObservation)
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived terminal release; want removed")
	}
}

// The tracker write (AddLabel or CommentIssue) and the follow-up durable
// marker write must share the exact same deadline context, not two
// independent 30-second timeouts, or the goroutine as a whole can run
// past trackerOpsDrainTimeout.

func TestReconcileMergeCompletion_MissingSHAEscalationSharesDeadlineWithMarkerWrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		escalation string
	}{
		{name: "label", escalation: "label"},
		{name: "comment", escalation: "comment"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "MGC-MISSING-DEADLINE-" + tt.escalation
			state := mgcStateWithPending(issueID, 55)
			store := newMGCStore()
			tracker := &mgcTrackerFake{states: map[string]string{issueID: "In Review"}}
			scm := &mgcSCMFake{fn: func(int, string, string) (domain.PRMergeStatus, error) {
				return mergedMissingSHAStatus(), nil
			}}
			params := mgcParams(store, scm, tracker)
			params.MergeCompletionConfig.Escalation = tt.escalation
			now := mgcBaseTime
			params.NowFunc = func() time.Time { return now }

			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			now = now.Add(31 * time.Minute)
			reconcileMergeCompletion(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})
			state.TrackerOpsWg.Wait()

			observation, ok := store.missingSHAObservation(issueID)
			if !ok || !observation.dispatched {
				t.Fatalf("missing-SHA observation = %+v, ok=%v, want dispatched=true after escalation delivery", observation, ok)
			}

			var trackerCtx context.Context
			switch tt.escalation {
			case "label":
				if len(tracker.addLabelCalls) != 1 {
					t.Fatalf("AddLabel calls = %d, want 1", len(tracker.addLabelCalls))
				}
				trackerCtx = tracker.addLabelCtx
			case "comment":
				if len(tracker.commentCalls) != 1 {
					t.Fatalf("CommentIssue calls = %d, want 1", len(tracker.commentCalls))
				}
				trackerCtx = tracker.commentCtx
			}

			if trackerCtx == nil || store.markObservationCtx == nil {
				t.Fatalf("captured contexts = tracker:%v marker:%v, want both non-nil", trackerCtx, store.markObservationCtx)
			}
			if trackerCtx != store.markObservationCtx {
				t.Errorf("MarkReactionObservationDispatched context = %v, want the identical context passed to the %s tracker write (one shared deadline, not two)", store.markObservationCtx, tt.escalation)
			}
		})
	}
}
