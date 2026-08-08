package orchestrator

import (
	"context"
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

	addLabelCalls []mgcLabelCall
	commentCalls  []mgcCommentCall
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

func (f *mgcTrackerFake) CommentIssue(_ context.Context, issueID, text string) error {
	f.commentCalls = append(f.commentCalls, mgcCommentCall{issueID: issueID, text: text})
	return nil
}

func (f *mgcTrackerFake) AddLabel(_ context.Context, issueID, label string) error {
	f.addLabelCalls = append(f.addLabelCalls, mgcLabelCall{issueID: issueID, label: label})
	return nil
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
}

// mgcStoreFake is a ReconcileStore that genuinely persists the
// per-(issue,kind) fingerprint and dispatched flag, and deletes by
// issue prefix for real, so the exactly-once latch and the
// cross-kind-clear interaction are exercised against real stored
// state rather than canned values.
type mgcStoreFake struct {
	fingerprints map[string]mgcFingerprintRecord

	upsertErr error
	getErr    error
	markErr   error

	upsertCalls      int
	getCalls         int
	markCalls        int
	deleteCalls      int
	deleteRetryCalls int
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
	delete(s.fingerprints, issueID+":"+kind)
	return nil
}

// has reports the fingerprint record for the issue's merge-completion
// row, the only kind this test file's store ever populates.
func (s *mgcStoreFake) has(issueID string) (mgcFingerprintRecord, bool) {
	rec, ok := s.fingerprints[issueID+":"+ReactionKindMergeCompletion]
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
	})

	t.Run("terminal state drops with the terminal-specific log message", func(t *testing.T) {
		t.Parallel()

		issueID := "MGC-13-TERMINAL"
		state := mgcStateWithPending(issueID, 23)
		store := newMGCStore()
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
// that releasing a merge-completion pending entry through the terminal
// tracker-state path performs no reaction_fingerprints read or write,
// leaving a dispatched fingerprint row byte-identical.
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
	store.upsertCalls = 0
	store.getCalls = 0
	store.markCalls = 0
	store.deleteCalls = 0

	tracker := &mgcTrackerFake{states: map[string]string{issueID: "Done"}}
	scm := &mgcSCMFake{}
	params := mgcParams(store, scm, tracker)

	reconcileTrackerState(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if store.upsertCalls != 0 || store.getCalls != 0 || store.markCalls != 0 || store.deleteCalls != 0 {
		t.Errorf("fingerprint store calls = upsert:%d get:%d mark:%d delete:%d, want all 0",
			store.upsertCalls, store.getCalls, store.markCalls, store.deleteCalls)
	}
	after, ok := store.has(issueID)
	if !ok {
		t.Fatal("fingerprint row deleted by terminal release; want preserved")
	}
	if after != before {
		t.Errorf("fingerprint row = %+v, want unchanged %+v", after, before)
	}
	rkey := ReactionKey(issueID, ReactionKindMergeCompletion)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived terminal release; want removed")
	}
}
