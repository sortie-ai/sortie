package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// --- Test doubles ---

// mockCIProvider is a controllable CIStatusProvider for CI reconcile tests.
type mockCIProvider struct {
	result domain.CIResult
	err    error
	calls  int

	lastRef []string
}

var _ domain.CIStatusProvider = (*mockCIProvider)(nil)

func (m *mockCIProvider) FetchCIStatus(_ context.Context, ref string) (domain.CIResult, error) {
	m.calls++
	m.lastRef = append(m.lastRef, ref)
	return m.result, m.err
}

// ciReconcileSCM is a controllable SCMAdapter whose GetMergeability
// results are supplied per call by a function field, so a scenario can
// advance the head SHA between reconcile passes. The call count and the
// most recently observed argument triple let tests assert that the pass
// reads mergeability exactly once per due tick with the pending entry's
// own PR identity, never a value baked into the fake.
type ciReconcileSCM struct {
	fn     func() (domain.PRMergeStatus, error)
	result domain.PRMergeStatus
	err    error

	calls        int
	lastPRNumber int
	lastOwner    string
	lastRepo     string
}

var _ domain.SCMAdapter = (*ciReconcileSCM)(nil)

func (s *ciReconcileSCM) GetMergeability(_ context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	s.calls++
	s.lastPRNumber = prNumber
	s.lastOwner = owner
	s.lastRepo = repo
	if s.fn != nil {
		return s.fn()
	}
	return s.result, s.err
}

func (s *ciReconcileSCM) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (s *ciReconcileSCM) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return nil, nil
}
func (s *ciReconcileSCM) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}
func (s *ciReconcileSCM) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}
func (s *ciReconcileSCM) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}
func (s *ciReconcileSCM) DeleteBranch(_ context.Context, _, _, _ string) error { return nil }
func (s *ciReconcileSCM) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}
func (s *ciReconcileSCM) RemoveLabel(_ context.Context, _ int, _, _, _ string) error { return nil }

// ciDefaultHead is the head SHA a default ciReconcileSCM reports. Most
// tests that are not specifically about head-change detection seed the
// fingerprint store with this same value, so the pass treats the head as
// already observed rather than incidentally exercising an epoch
// transition on every single-pass test.
const ciDefaultHead = "sha-current"

// defaultCISCM returns a ciReconcileSCM reporting an open pull request at
// ciDefaultHead.
func defaultCISCM() *ciReconcileSCM {
	return &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: ciDefaultHead}}
}

// ciReconcileStoreCall records one method invocation for the fingerprint
// call-order assertion: a store that upserts before it reads
// passes every other assertion in this file, so the order must be
// checked explicitly.
type ciReconcileStoreCall string

const (
	ciCallGetFingerprint    ciReconcileStoreCall = "get_fingerprint"
	ciCallUpsertFingerprint ciReconcileStoreCall = "upsert_fingerprint"
)

// ciReconcileStore records calls to ReconcileStore methods and returns
// configurable errors. Parallel to mockReconcileStore but distinct so
// ci_reconcile_test.go is self-contained.
type ciReconcileStore struct {
	unsupportedReactionObservationStore

	savedEntries    []persistence.RetryEntry
	deletedIssueIDs []string
	runHistories    []persistence.RunHistory

	saveRetryEntryErr   error
	deleteRetryEntryErr error
	appendRunHistoryErr error

	// Fingerprint dedup fields.
	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int

	upsertFingerprintErr     error
	getFingerprintResult     string
	getFingerprintDispatched bool
	getFingerprintErr        error
	markDispatchedErr        error
	deleteFingerprintErr     error

	upsertedFingerprints []string
	callOrder            []ciReconcileStoreCall

	// count and countErr override the embedded
	// unsupportedReactionObservationStore default for
	// CountWorkerRunsCompletedSince when countConfigured is true, so a
	// test can force a specific classifyHeadChange attribution.
	countConfigured bool
	count           int
	countErr        error
}

var _ ReconcileStore = (*ciReconcileStore)(nil)

func (s *ciReconcileStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.savedEntries = append(s.savedEntries, entry)
	return s.saveRetryEntryErr
}

func (s *ciReconcileStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.deletedIssueIDs = append(s.deletedIssueIDs, issueID)
	return s.deleteRetryEntryErr
}

func (s *ciReconcileStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.runHistories = append(s.runHistories, run)
	return run, s.appendRunHistoryErr
}

func (s *ciReconcileStore) UpsertReactionFingerprint(_ context.Context, _, _, fingerprint string) error {
	s.upsertFingerprintCalls++
	s.upsertedFingerprints = append(s.upsertedFingerprints, fingerprint)
	s.callOrder = append(s.callOrder, ciCallUpsertFingerprint)
	if s.upsertFingerprintErr == nil {
		// Mirrors the persistence-layer upsert contract, so a
		// multi-pass test observes the same head on its next
		// GetReactionFingerprint call rather than a value frozen at
		// construction time.
		s.getFingerprintResult = fingerprint
		s.getFingerprintDispatched = false
	}
	return s.upsertFingerprintErr
}

func (s *ciReconcileStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	s.getFingerprintCalls++
	s.callOrder = append(s.callOrder, ciCallGetFingerprint)
	return s.getFingerprintResult, s.getFingerprintDispatched, s.getFingerprintErr
}

func (s *ciReconcileStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	s.markDispatchedCalls++
	return s.markDispatchedErr
}

func (s *ciReconcileStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	s.deleteFingerprintCalls++
	return s.deleteFingerprintErr
}

func (s *ciReconcileStore) CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error) {
	if !s.countConfigured {
		return s.unsupportedReactionObservationStore.CountWorkerRunsCompletedSince(ctx, issueID, since)
	}
	return s.count, s.countErr
}

// ciTrackerStub is a no-panic TrackerAdapter for CI reconcile tests.
// Escalation goroutines may call AddLabel or CommentIssue; all other
// methods return zero values.
type ciTrackerStub struct {
	addLabelCalled    int
	commentIssueCalls int
	addLabelErr       error
	commentIssueErr   error
	lastComment       string
}

var _ domain.TrackerAdapter = (*ciTrackerStub)(nil)

func (s *ciTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *ciTrackerStub) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *ciTrackerStub) TransitionIssue(_ context.Context, _ string, _ string) error { return nil }
func (s *ciTrackerStub) CommentIssue(_ context.Context, _ string, text string) error {
	s.commentIssueCalls++
	s.lastComment = text
	return s.commentIssueErr
}
func (s *ciTrackerStub) AddLabel(_ context.Context, _ string, _ string) error {
	s.addLabelCalled++
	return s.addLabelErr
}

// ciMetricsSpy records calls to CI-specific metric methods while delegating
// all other methods to NoopMetrics.
type ciMetricsSpy struct {
	domain.NoopMetrics
	ciStatusChecks   map[string]int
	ciEscalations    map[string]int
	retriesByTrigger map[string]int
}

func newCIMetricsSpy() *ciMetricsSpy {
	return &ciMetricsSpy{
		ciStatusChecks:   make(map[string]int),
		ciEscalations:    make(map[string]int),
		retriesByTrigger: make(map[string]int),
	}
}

func (s *ciMetricsSpy) IncCIStatusChecks(result string) { s.ciStatusChecks[result]++ }
func (s *ciMetricsSpy) IncCIEscalations(action string)  { s.ciEscalations[action]++ }
func (s *ciMetricsSpy) IncRetries(trigger string)       { s.retriesByTrigger[trigger]++ }

// --- Test helpers ---

// ciBaseTime is a fixed reference for CI reconcile tests.
var ciBaseTime = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// newPendingEntry builds a PendingReaction for a test CI issue, with pull
// request identity populated so a live GetMergeability call has a real
// argument triple to receive.
func newPendingEntry(issueID, identifier, branch string, attempt int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: identifier,
		DisplayID:  identifier,
		Attempt:    attempt,
		Kind:       ReactionKindCI,
		CreatedAt:  ciBaseTime,
		KindData: &CIReactionData{
			PRNumber: 99,
			Owner:    "acme",
			Repo:     "widgets",
			Branch:   branch,
			SHA:      "seed-sha",
		},
	}
}

// defaultCIFeedback returns a CIFeedbackConfig with max_retries=2, label escalation.
func defaultCIFeedback() config.CIFeedbackConfig {
	return config.CIFeedbackConfig{
		Kind:            "github",
		MaxRetries:      2,
		Escalation:      "label",
		EscalationLabel: "needs-human",
	}
}

// stateWithPendingReaction creates a State with one CI PendingReaction entry.
func stateWithPendingReaction(t *testing.T, issueID, branch string, attempt int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindCI)
	s.PendingReactions[rkey] = newPendingEntry(issueID, issueID+"-ident", branch, attempt)
	s.Claimed[issueID] = struct{}{}
	return s
}

// reseedCIPendingEntry installs a fresh CI PendingReaction for issueID,
// mirroring what HandleWorkerExit does when a dispatched continuation's
// worker exits: a brand-new entry with HeadRecordedAt zero, because a
// head recorded before the continuation ran cannot bound the query for
// the process observing the new one. Used to drive a multi-commit
// scenario across several reconcile passes without dispatching a real
// worker.
func reseedCIPendingEntry(state *State, issueID, branch string, attempt int, now time.Time) {
	entry := newPendingEntry(issueID, issueID+"-ident", branch, attempt)
	entry.CreatedAt = now
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = entry
	state.Claimed[issueID] = struct{}{}
}

// ciParams returns ReconcileParams wired for CI reconcile unit tests. scm
// may be nil to exercise the SCMAdapter-nil guard; every other test
// supplies a *ciReconcileSCM (commonly [defaultCISCM]).
func ciParams(t *testing.T, store *ciReconcileStore, ci domain.CIStatusProvider, tracker domain.TrackerAdapter, scm domain.SCMAdapter) ReconcileParams {
	t.Helper()
	return ReconcileParams{
		TrackerAdapter: tracker,
		CIProvider:     ci,
		CIFeedback:     defaultCIFeedback(),
		CIWatchWindow:  24 * time.Hour,
		SCMAdapter:     scm,
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            context.Background(),
		Logger:         discardLogger(),
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
		NowFunc:        func() time.Time { return ciBaseTime },
	}
}

// --- Guard tests ---

func TestReconcileCIStatus_NilCIProvider(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-1", "feature/fix", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	params := ciParams(t, store, nil, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-1", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry consumed when CIProvider is nil; want no-op")
	}
	if len(metrics.ciStatusChecks) != 0 {
		t.Errorf("IncCIStatusChecks called with nil provider; want no calls")
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times with nil provider; want 0", len(store.runHistories))
	}
}

// TestReconcileCIStatus_NilSCMAdapter verifies that a nil SCMAdapter is a
// no-op for the whole phase, the same as a nil CIProvider, rather than a
// panic on the head read.
func TestReconcileCIStatus_NilSCMAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-NOSCM", "feature/fix", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-NOSCM", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry consumed when SCMAdapter is nil; want no-op")
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times with nil SCMAdapter; want 0", ci.calls)
	}
}

// --- The live head, never the frozen ref ---

// TestReconcileCIStatus_RefIsLiveHead_NeverFrozenBranchOrSHA verifies that
// the ref passed to FetchCIStatus always equals the freshly-read
// PRMergeStatus.HeadSHA, never CIReactionData.SHA or CIReactionData.Branch,
// including when all three differ.
func TestReconcileCIStatus_RefIsLiveHead_NeverFrozenBranchOrSHA(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-REF", "frozen-branch", 1)
	// CIReactionData.SHA is "seed-sha" (set by newPendingEntry); the live
	// head reported by GetMergeability is deliberately a third value.
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "live-head-sha"}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if len(ci.lastRef) != 1 {
		t.Fatalf("FetchCIStatus calls = %d, want 1", len(ci.lastRef))
	}
	if got := ci.lastRef[0]; got != "live-head-sha" {
		t.Errorf("FetchCIStatus ref = %q, want %q (the live head)", got, "live-head-sha")
	}
	if got := ci.lastRef[0]; got == "frozen-branch" || got == "seed-sha" {
		t.Errorf("FetchCIStatus ref = %q, want neither CIReactionData.Branch nor CIReactionData.SHA", got)
	}
	if scm.lastPRNumber != 99 || scm.lastOwner != "acme" || scm.lastRepo != "widgets" {
		t.Errorf("GetMergeability(prNumber=%d, owner=%q, repo=%q), want (99, acme, widgets)",
			scm.lastPRNumber, scm.lastOwner, scm.lastRepo)
	}
}

// --- Fingerprint read-before-write ordering ---

// TestReconcileCIStatus_FingerprintReadPrecedesWrite_CallOrder verifies
// that GetReactionFingerprint is called before UpsertReactionFingerprint
// on a head-changed pass. An implementation that upserts first and then
// reads would read back the value it just wrote and never detect a head
// change, so this assertion is on call order rather than end state.
func TestReconcileCIStatus_FingerprintReadPrecedesWrite_CallOrder(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-ORDER", "main", 1)
	store := &ciReconcileStore{getFingerprintResult: "sha-A"}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "sha-B"}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if len(store.callOrder) < 2 {
		t.Fatalf("store call order = %v, want at least [get, upsert]", store.callOrder)
	}
	if store.callOrder[0] != ciCallGetFingerprint {
		t.Errorf("first fingerprint call = %q, want %q", store.callOrder[0], ciCallGetFingerprint)
	}
	getIdx, upsertIdx := -1, -1
	for i, c := range store.callOrder {
		if c == ciCallGetFingerprint && getIdx == -1 {
			getIdx = i
		}
		if c == ciCallUpsertFingerprint && upsertIdx == -1 {
			upsertIdx = i
		}
	}
	if getIdx == -1 || upsertIdx == -1 || getIdx > upsertIdx {
		t.Errorf("GetReactionFingerprint index=%d, UpsertReactionFingerprint index=%d; want get before upsert", getIdx, upsertIdx)
	}
	if len(store.upsertedFingerprints) != 1 || store.upsertedFingerprints[0] != "sha-B" {
		t.Errorf("upserted fingerprints = %v, want [sha-B]", store.upsertedFingerprints)
	}

	// A same-head pass performs no upsert: with the row already holding
	// the live head, storedHead == status.HeadSHA and the epoch branch
	// never runs.
	store2 := &ciReconcileStore{getFingerprintResult: "sha-B"}
	state2 := stateWithPendingReaction(t, "ISS-CI-ORDER-2", "main", 1)
	ci2 := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	scm2 := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "sha-B"}}
	params2 := ciParams(t, store2, ci2, nil, scm2)

	reconcileCIStatus(state2, params2, discardLogger(), context.Background(), metrics)

	if store2.upsertFingerprintCalls != 0 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 0 when the stored head already matches the live head", store2.upsertFingerprintCalls)
	}
}

// --- The core defect and the keeps-passing watch ---

// TestReconcileCIStatus_EarlierPassingLaterFailing_DispatchesOneContinuation
// covers the core defect this change fixes: a pull request whose earlier
// head passed and whose later head fails dispatches one CI-fix
// continuation within budget. Under the frozen-ref behavior this change
// replaces, the passing pass would have retired the watch and the second
// pass would never run.
func TestReconcileCIStatus_EarlierPassingLaterFailing_DispatchesOneContinuation(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-CORE"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "head-A"}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
		t.Fatal("PendingReactions entry retired after a passing head; want the watch to continue")
	}
	if len(store.runHistories) != 0 {
		t.Fatalf("AppendRunHistory called %d times after a passing head; want 0", len(store.runHistories))
	}

	// The head advances and now fails.
	entry := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	entry.PendingRetryAt = time.Time{}
	scm.result = domain.PRMergeStatus{HeadSHA: "head-B"}
	ci.result = domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1 (one continuation dispatched)", len(store.runHistories))
	}
	if store.runHistories[0].Status != "ci_failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "ci_failed")
	}
	retryEntry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("continuation not scheduled after the later head failed; want scheduled")
	}
	if retryEntry.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q", retryEntry.ReactionKind, ReactionKindCI)
	}
	if len(ci.lastRef) != 2 || ci.lastRef[1] != "head-B" {
		t.Errorf("FetchCIStatus refs = %v, want the second call against head-B", ci.lastRef)
	}
}

// TestReconcileCIStatus_KeepsPassing_NoDispatchNoAttemptNoRunHistory
// covers the case where a pull request whose head keeps passing across several
// passes dispatches nothing, spends no attempt, writes no run_history
// row, and makes no tracker call.
func TestReconcileCIStatus_KeepsPassing_NoDispatchNoAttemptNoRunHistory(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-KEEPPASS"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "head-steady"}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, tracker, scm)

	for range 3 {
		entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
		if ok {
			entry.PendingRetryAt = time.Time{}
		}
		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	}

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry retired across repeated passing passes; want the watch to continue")
	}
	if _, ok := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("ReactionAttempts set after repeated passing; want unset")
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times across a keeps-passing watch; want 0", len(store.runHistories))
	}
	if tracker.commentIssueCalls != 0 || tracker.addLabelCalled != 0 {
		t.Errorf("tracker called (comment=%d, label=%d) across a keeps-passing watch; want 0/0", tracker.commentIssueCalls, tracker.addLabelCalled)
	}
}

// --- Steady-state status handling ---

func TestReconcileCIStatus_FetchError_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-2", "main", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{err: errors.New("network timeout")}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-2", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry dropped on FetchCIStatus error; want re-enqueued")
	}
	if metrics.ciStatusChecks["error"] != 1 {
		t.Errorf(`IncCIStatusChecks("error") = %d, want 1`, metrics.ciStatusChecks["error"])
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times on fetch error; want 0", len(store.runHistories))
	}
}

func TestReconcileCIStatus_Pending_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-4", "feature/wip", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-4", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry not re-enqueued after pending; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-CI-4"]; ok {
		t.Error("retry scheduled after CI pending; want none")
	}
	if metrics.ciStatusChecks["pending"] != 1 {
		t.Errorf(`IncCIStatusChecks("pending") = %d, want 1`, metrics.ciStatusChecks["pending"])
	}
}

// TestReconcileCIStatus_CancelledOnly_NoRetrySpent covers the case where a
// ref's only completed checks are a cancelled run alongside a successful
// one. The fixture's Status and FailingCount are derived from the declared
// check-run set via scmcore.AggregateCIStatus and scmcore.FailingCount,
// rather than written as literals, so a classifier regression reddens this
// test rather than passing it vacuously. Guards the shipped cancelled-check
// behavior against this change.
func TestReconcileCIStatus_CancelledOnly_NoRetrySpent(t *testing.T) {
	t.Parallel()

	runs := []domain.CheckRun{
		{Name: "build", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionCancelled},
		{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionSuccess},
	}

	state := stateWithPendingReaction(t, "ISS-CI-CANCELLED", "feature/cancelled-only", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       scmcore.AggregateCIStatus(runs),
		FailingCount: scmcore.FailingCount(runs),
		CheckRuns:    runs,
	}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.ReactionAttempts[ReactionKey("ISS-CI-CANCELLED", ReactionKindCI)]; ok {
		t.Error("ReactionAttempts set after cancelled-only result; want unchanged (unset)")
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory call count = %d, want 0", len(store.runHistories))
	}
	if _, ok := state.RetryAttempts["ISS-CI-CANCELLED"]; ok {
		t.Error("retry scheduled after cancelled-only result; want none")
	}
	entry, ok := state.PendingReactions[ReactionKey("ISS-CI-CANCELLED", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after cancelled-only result; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingReactions entry PendingAttempts = %d, want 1", entry.PendingAttempts)
	}
	if metrics.ciStatusChecks["pending"] != 1 {
		t.Errorf(`IncCIStatusChecks("pending") = %d, want 1`, metrics.ciStatusChecks["pending"])
	}
}

// TestReconcileCIStatus_CancelledWithFailure_StillSpendsRetry covers the
// shape in which a cancellation coexists with a genuine failure on the same
// ref: the pass still takes the failing arm. Guards the shipped cancelled-check
// behavior against this change.
func TestReconcileCIStatus_CancelledWithFailure_StillSpendsRetry(t *testing.T) {
	t.Parallel()

	runs := []domain.CheckRun{
		{Name: "build", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionCancelled},
		{Name: "test", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
	}

	state := stateWithPendingReaction(t, "ISS-CI-CANCELLED-FAIL", "feature/cancelled-with-failure", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       scmcore.AggregateCIStatus(runs),
		FailingCount: scmcore.FailingCount(runs),
		CheckRuns:    runs,
	}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if state.ReactionAttempts[ReactionKey("ISS-CI-CANCELLED-FAIL", ReactionKindCI)] != 1 {
		t.Errorf("ReactionAttempts[ISS-CI-CANCELLED-FAIL] = %d, want 1", state.ReactionAttempts[ReactionKey("ISS-CI-CANCELLED-FAIL", ReactionKindCI)])
	}
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "ci_failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "ci_failed")
	}
	if _, ok := state.RetryAttempts["ISS-CI-CANCELLED-FAIL"]; !ok {
		t.Error("retry not scheduled after cancelled-with-failure result; want scheduled")
	}
}

func TestReconcileCIStatus_Failing_UnderMaxRetries(t *testing.T) {
	t.Parallel()

	// ReactionAttempts starts at 0; maxRetries=2 → no escalation after increment to 1.
	state := stateWithPendingReaction(t, "ISS-CI-5", "feature/break", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       domain.CIStatusFailing,
		FailingCount: 2,
		CheckRuns: []domain.CheckRun{
			{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
		},
	}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Entry consumed (not re-enqueued as pending-check).
	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-5", ReactionKindCI)]; ok {
		t.Error("PendingReactions entry re-enqueued on CI failure; want consumed")
	}

	// RunHistory appended with "ci_failed".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "ci_failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "ci_failed")
	}
	if store.runHistories[0].IssueID != "ISS-CI-5" {
		t.Errorf("RunHistory.IssueID = %q, want %q", store.runHistories[0].IssueID, "ISS-CI-5")
	}
	if !store.runHistories[0].TokensMeasured {
		t.Error("RunHistory.TokensMeasured = false, want true (no agent session ran, so zero spend is exact)")
	}

	// SaveRetryEntry NOT called: CI fix retries are in-memory until HandleRetryTimer.
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times; want 0 (in-memory retry only)", len(store.savedEntries))
	}

	// In-memory retry scheduled with CI failure context.
	entry, ok := state.RetryAttempts["ISS-CI-5"]
	if !ok {
		t.Fatal("retry not scheduled after CI failure; want scheduled")
	}
	if entry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want continuation map")
	}

	// ReactionAttempts incremented.
	if state.ReactionAttempts[ReactionKey("ISS-CI-5", ReactionKindCI)] != 1 {
		t.Errorf("ReactionAttempts[ISS-CI-5] = %d, want 1", state.ReactionAttempts[ReactionKey("ISS-CI-5", ReactionKindCI)])
	}

	// Metrics.
	if metrics.ciStatusChecks["failing"] != 1 {
		t.Errorf(`IncCIStatusChecks("failing") = %d, want 1`, metrics.ciStatusChecks["failing"])
	}
	if metrics.retriesByTrigger["ci_fix"] != 1 {
		t.Errorf(`IncRetries("ci_fix") = %d, want 1`, metrics.retriesByTrigger["ci_fix"])
	}

	// Claim preserved.
	if _, ok := state.Claimed["ISS-CI-5"]; !ok {
		t.Error("claim released after CI failure under max retries; want preserved")
	}
}

// TestReconcileCIStatus_Failing_RunHistoryCompletedAtIsUTC covers R9: the
// CI-failure writer formats a UTC time with time.RFC3339, so the
// persisted value ends in "Z" and parses back as RFC3339.
func TestReconcileCIStatus_Failing_RunHistoryCompletedAtIsUTC(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-UTC", "feature/break", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       domain.CIStatusFailing,
		FailingCount: 1,
		CheckRuns: []domain.CheckRun{
			{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
		},
	}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}
	completedAt := store.runHistories[0].CompletedAt
	if !strings.HasSuffix(completedAt, "Z") {
		t.Errorf("RunHistory.CompletedAt = %q, want suffix %q", completedAt, "Z")
	}
	if _, err := time.Parse(time.RFC3339, completedAt); err != nil {
		t.Errorf("time.Parse(RFC3339, %q): %v", completedAt, err)
	}
}

// --- Escalation as a per-epoch soft stop ---

func TestReconcileCIStatus_Failing_ExceedsMaxRetries_Escalates(t *testing.T) {
	t.Parallel()

	// ReactionAttempts at 2; after increment → 3 > maxRetries(2) → escalate.
	state := stateWithPendingReaction(t, "ISS-CI-6", "feature/broken", 3)
	state.ReactionAttempts[ReactionKey("ISS-CI-6", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       domain.CIStatusFailing,
		FailingCount: 1,
	}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// DeleteRetryEntry called for the issue.
	if len(store.deletedIssueIDs) != 1 || store.deletedIssueIDs[0] != "ISS-CI-6" {
		t.Errorf("DeleteRetryEntry calls = %v, want [ISS-CI-6]", store.deletedIssueIDs)
	}

	// Claim released.
	if _, ok := state.Claimed["ISS-CI-6"]; ok {
		t.Error("claim not released after CI escalation; want released")
	}

	// The pending entry, its counter, and its fingerprint all survive
	// the soft stop: the counter must stay over budget so no further
	// continuation dispatches until a new epoch resets it, and the
	// fingerprint row is the epoch record.
	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-6", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry removed after escalation; want re-enqueued (soft stop)")
	}
	if state.ReactionAttempts[ReactionKey("ISS-CI-6", ReactionKindCI)] != 3 {
		t.Errorf("ReactionAttempts[ISS-CI-6] = %d after escalation, want 3 (kept over budget)", state.ReactionAttempts[ReactionKey("ISS-CI-6", ReactionKindCI)])
	}
	if store.deleteFingerprintCalls != 0 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 0 (fingerprint is the epoch record)", store.deleteFingerprintCalls)
	}

	// Wait for the async escalation goroutine before reading metrics.
	state.TrackerOpsWg.Wait()

	// Escalation metric incremented (label mode from defaultCIFeedback).
	if metrics.ciEscalations["label"] != 1 {
		t.Errorf(`IncCIEscalations("label") = %d, want 1`, metrics.ciEscalations["label"])
	}

	// RunHistory appended for the failing attempt.
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}

	// No continuation scheduled.
	if _, ok := state.RetryAttempts["ISS-CI-6"]; ok {
		t.Error("retry scheduled after escalation; want none")
	}
}

func TestReconcileCIStatus_Failing_CommentEscalation(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-7", "main", 1)
	state.ReactionAttempts[ReactionKey("ISS-CI-7", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Wait for the async escalation goroutine before reading metrics.
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["comment"] != 1 {
		t.Errorf(`IncCIEscalations("comment") = %d, want 1`, metrics.ciEscalations["comment"])
	}
	// Claim released in both escalation modes.
	if _, ok := state.Claimed["ISS-CI-7"]; ok {
		t.Error("claim not released after comment escalation")
	}
	// The entry survives the soft stop.
	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-7", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry removed after comment escalation; want re-enqueued (soft stop)")
	}
}

// TestReconcileCIStatus_EscalationSoftStop_SameHeadDoesNotReescalate
// covers the case where, after escalation, a later pass on the same head neither
// escalates again nor dispatches a continuation.
func TestReconcileCIStatus_EscalationSoftStop_SameHeadDoesNotReescalate(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-SOFTSTOP"
	state := stateWithPendingReaction(t, issueID, "main", 3)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["label"] != 1 {
		t.Fatalf(`IncCIEscalations("label") after first pass = %d, want 1`, metrics.ciEscalations["label"])
	}

	// A later pass on the very same head.
	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry missing before the second pass; want present (soft stop)")
	}
	entry.PendingRetryAt = time.Time{}

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["label"] != 1 {
		t.Errorf(`IncCIEscalations("label") after second pass on the same head = %d, want 1 (no re-escalation)`, metrics.ciEscalations["label"])
	}
	if _, ok := state.RetryAttempts[issueID]; ok {
		t.Error("a continuation was scheduled on the same head after escalation; want none")
	}
}

// TestReconcileCIStatus_CommentEscalation_RecursOncePerHead_ThenAgainAfterNotOursHeadChange
// covers the case where, under escalation: comment, an exhausted budget posts one
// comment across several passes on one head, and one further comment
// after a head change positively not attributed to the orchestrator's
// own work.
func TestReconcileCIStatus_CommentEscalation_RecursOncePerHead_ThenAgainAfterNotOursHeadChange(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-COMMENT-RECUR"
	state := stateWithPendingReaction(t, issueID, "main", 3)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 2
	// The fingerprint starts empty so the first pass triggers a genuine
	// epoch transition and records HeadRecordedAt, which the later
	// notOurs transition needs as its non-zero boundary.
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := defaultCISCM()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker, scm)
	params.CIFeedback.Escalation = "comment"

	for i := range 3 {
		entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
		if i > 0 && !ok {
			t.Fatalf("pass %d: PendingReactions entry missing; want present (soft stop)", i)
		}
		if ok {
			entry.PendingRetryAt = time.Time{}
		}
		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
		state.TrackerOpsWg.Wait()
	}
	if metrics.ciEscalations["comment"] != 1 {
		t.Fatalf(`IncCIEscalations("comment") across 3 passes on one head = %d, want 1`, metrics.ciEscalations["comment"])
	}

	// The head changes and the store reports zero worker sessions
	// completed since the boundary: classifyHeadChange answers
	// positively not the orchestrator's own work, resetting the counter
	// and the escalation flag, so the budget re-exhausts across the
	// next several passes and escalates again.
	scm.result = domain.PRMergeStatus{HeadSHA: "head-after-push"}
	store.countConfigured = true
	store.count = 0

	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry missing before the head-change pass; want present (soft stop)")
	}
	entry.PendingRetryAt = time.Time{}
	now := ciBaseTime
	for i := range 3 {
		now = now.Add(time.Duration(i) * time.Minute)
		if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
			// A non-escalating failure consumes the entry, converting
			// it into a continuation dispatch; the next worker exit
			// would normally reseed the watch, modeled here directly.
			reseedCIPendingEntry(state, issueID, "main", 3, now)
			delete(state.RetryAttempts, issueID)
		}
		e := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
		e.PendingRetryAt = time.Time{}
		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
		state.TrackerOpsWg.Wait()
	}

	if metrics.ciEscalations["comment"] != 2 {
		t.Errorf(`IncCIEscalations("comment") after a head change not attributed to the orchestrator = %d, want 2`, metrics.ciEscalations["comment"])
	}
}

// TestBuildCIEscalationComment: the escalation comment
// names exactly the checks the verdict counted as failing.
func TestBuildCIEscalationComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		result         domain.CIResult
		ref            string
		attempts       int
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "basic format with failing count and details URL",
			result: domain.CIResult{
				FailingCount: 2,
				CheckRuns: []domain.CheckRun{
					{
						Name:       "test",
						Status:     domain.CheckRunStatusCompleted,
						Conclusion: domain.CheckConclusionFailure,
						DetailsURL: "https://ci.example.com/runs/42",
					},
				},
			},
			ref:      "abc1234",
			attempts: 3,
			wantContains: []string{
				"CI fix retries exhausted",
				"abc1234",
				"3 CI-fix continuation(s)",
				"Failing checks:",
				"2",
				"test",
				"https://ci.example.com/runs/42",
				"Manual intervention required",
			},
		},
		{
			name: "zero failing count omits count line",
			result: domain.CIResult{
				FailingCount: 0,
				CheckRuns:    []domain.CheckRun{},
			},
			ref:      "main",
			attempts: 1,
			wantContains: []string{
				"CI fix retries exhausted",
				"main",
				"1 CI-fix continuation(s)",
				"Manual intervention required",
			},
		},
		{
			name: "only failure check runs included",
			result: domain.CIResult{
				FailingCount: 2,
				CheckRuns: []domain.CheckRun{
					{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionSuccess},
					{Name: "test", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
					{Name: "deploy", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionTimedOut},
					{Name: "e2e", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionCancelled},
				},
			},
			ref:            "feature/x",
			attempts:       2,
			wantContains:   []string{"test", "deploy"},
			wantNotContain: []string{"lint", "e2e"},
		},
		{
			name: "check run without details URL omits link",
			result: domain.CIResult{
				FailingCount: 1,
				CheckRuns: []domain.CheckRun{
					{Name: "build", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure, DetailsURL: ""},
				},
			},
			ref:            "sha9999",
			attempts:       1,
			wantContains:   []string{"build"},
			wantNotContain: []string{"details"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildCIEscalationComment(tt.result, tt.ref, tt.attempts)

			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildCIEscalationComment() output missing %q\ngot: %s", want, got)
				}
			}
			for _, notWant := range tt.wantNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("buildCIEscalationComment() output unexpectedly contains %q\ngot: %s", notWant, got)
				}
			}
		})
	}
}

// TestReconcileCIStatus_NoPersonAttributingLanguage verifies that no log
// line, comment, or label emitted on a head-change path contains a
// person-attributing phrase. The assertion enumerates the emitted
// strings rather than searching for a denylist.
func TestReconcileCIStatus_NoPersonAttributingLanguage(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"a person pushed", "person pushed", "human push", "manual commit", "someone pushed",
	}

	const issueID = "ISS-CI-NOATTR"
	state := stateWithPendingReaction(t, issueID, "main", 3)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: "head-old"}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "head-new"}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker, scm)
	params.CIFeedback.Escalation = "comment"

	handler := &sweepLogHandler{}
	params.Logger = slog.New(handler)

	reconcileCIStatus(state, params, slog.New(handler), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	emitted := make([]string, 0, len(handler.records)+1)
	for _, rec := range handler.records {
		emitted = append(emitted, rec.message)
		for _, v := range rec.attrs {
			if s, ok := v.(string); ok {
				emitted = append(emitted, s)
			}
		}
	}
	if tracker.commentIssueCalls > 0 {
		emitted = append(emitted, tracker.lastComment)
	}

	for _, text := range emitted {
		lower := strings.ToLower(text)
		for _, phrase := range forbidden {
			if strings.Contains(lower, phrase) {
				t.Errorf("emitted text %q contains forbidden person-attributing phrase %q", text, phrase)
			}
		}
	}
}

// --- TrackerOpsWg lifecycle tests ---

// blockingCITracker is a TrackerAdapter whose AddLabel and CommentIssue
// methods block on channel gates. Used to pace fire-and-forget goroutines
// spawned by escalateCIFailure so TrackerOpsWg tracking can be verified.
type blockingCITracker struct {
	addLabelGate chan struct{} // if non-nil, AddLabel blocks until closed
	commentGate  chan struct{} // if non-nil, CommentIssue blocks until closed
}

var _ domain.TrackerAdapter = (*blockingCITracker)(nil)

func (b *blockingCITracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (b *blockingCITracker) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (b *blockingCITracker) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (b *blockingCITracker) AddLabel(ctx context.Context, _ string, _ string) error {
	if b.addLabelGate != nil {
		select {
		case <-b.addLabelGate:
		case <-ctx.Done():
		}
	}
	return nil
}
func (b *blockingCITracker) CommentIssue(ctx context.Context, _ string, _ string) error {
	if b.commentGate != nil {
		select {
		case <-b.commentGate:
		case <-ctx.Done():
		}
	}
	return nil
}

// TestEscalateCIFailure_LabelTracksTrackerOps verifies that the AddLabel
// goroutine spawned by the label escalation path increments TrackerOpsWg
// before starting and decrements it on return, so Wait() blocks during the
// call and resolves once it completes.
func TestEscalateCIFailure_LabelTracksTrackerOps(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	tracker := &blockingCITracker{addLabelGate: gate}

	// ReactionAttempts=2 with maxRetries=2 means next increment (→3) exceeds
	// the limit and triggers escalation.
	state := stateWithPendingReaction(t, "ESC-WG-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-WG-1", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	waitDone := make(chan struct{})
	go func() {
		state.TrackerOpsWg.Wait()
		close(waitDone)
	}()

	// TrackerOpsWg must not resolve while AddLabel blocks on the gate.
	select {
	case <-waitDone:
		t.Fatal("TrackerOpsWg.Wait() returned before AddLabel goroutine completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Release the gate to let AddLabel return and Done() fire.
	close(gate)

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TrackerOpsWg.Wait() did not return after AddLabel goroutine completed")
	}
}

// TestEscalateCIFailure_CommentTracksTrackerOps verifies that the
// CommentIssue goroutine spawned by the comment escalation path increments
// TrackerOpsWg before starting and decrements it on return.
func TestEscalateCIFailure_CommentTracksTrackerOps(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	tracker := &blockingCITracker{commentGate: gate}

	state := stateWithPendingReaction(t, "ESC-WG-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-WG-2", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	waitDone := make(chan struct{})
	go func() {
		state.TrackerOpsWg.Wait()
		close(waitDone)
	}()

	// TrackerOpsWg must not resolve while CommentIssue blocks on the gate.
	select {
	case <-waitDone:
		t.Fatal("TrackerOpsWg.Wait() returned before CommentIssue goroutine completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Release the gate to let CommentIssue return and Done() fire.
	close(gate)

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TrackerOpsWg.Wait() did not return after CommentIssue goroutine completed")
	}
}

// --- Backoff tests ---

func TestComputeCIPendingDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{"attempt 0 is immediate", 0, 0},
		{"attempt 1 is base×2", 1, 20 * time.Second},
		{"attempt 2 is base×4", 2, 40 * time.Second},
		{"attempt 3 is base×8", 3, 80 * time.Second},
		{"attempt 4 is base×16", 4, 160 * time.Second},
		{"attempt 5 is capped at 5 minutes", 5, 5 * time.Minute},
		{"large attempt is capped at 5 minutes", 100, 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := computeCIPendingDelay(ciPendingBackoffBaseDefault, tt.attempts)
			if got != tt.want {
				t.Errorf("computeCIPendingDelay(ciPendingBackoffBaseDefault, %d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}

func TestComputeCIPendingDelay_CustomBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		base     time.Duration
		attempts int
		want     time.Duration
	}{
		// 5s base: 5s * 2^n
		{"5s base attempt 1", 5 * time.Second, 1, 10 * time.Second},
		{"5s base attempt 2", 5 * time.Second, 2, 20 * time.Second},
		{"5s base attempt 3", 5 * time.Second, 3, 40 * time.Second},
		{"5s base attempt 4", 5 * time.Second, 4, 80 * time.Second},
		{"5s base attempt 5", 5 * time.Second, 5, 160 * time.Second},
		{"5s base attempt 6 capped", 5 * time.Second, 6, ciPendingBackoffCap},
		// 30s base: 30s * 2^n
		{"30s base attempt 1", 30 * time.Second, 1, 60 * time.Second},
		{"30s base attempt 2", 30 * time.Second, 2, 120 * time.Second},
		{"30s base attempt 3", 30 * time.Second, 3, 240 * time.Second},
		{"30s base attempt 4 capped", 30 * time.Second, 4, ciPendingBackoffCap},
		{"30s base attempt 5 capped", 30 * time.Second, 5, ciPendingBackoffCap},
		// 60s base: 60s * 2^n
		{"60s base attempt 1", 60 * time.Second, 1, 120 * time.Second},
		{"60s base attempt 2", 60 * time.Second, 2, 240 * time.Second},
		{"60s base attempt 3 capped", 60 * time.Second, 3, ciPendingBackoffCap},
		// large base already exceeds cap on attempt 1
		{"4m base attempt 1 capped", 4 * time.Minute, 1, ciPendingBackoffCap},
		// large attempt value always caps regardless of base
		{"large attempt capped", 30 * time.Second, 100, ciPendingBackoffCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := computeCIPendingDelay(tt.base, tt.attempts)
			if got != tt.want {
				t.Errorf("computeCIPendingDelay(%v, %d) = %v, want %v", tt.base, tt.attempts, got, tt.want)
			}
		})
	}
}

func TestComputeCIPendingDelay_ZeroBase(t *testing.T) {
	t.Parallel()

	// Zero and negative base must fall back to ciPendingBackoffBaseDefault.
	bases := []time.Duration{0, -1 * time.Second, -5 * time.Second}

	for _, base := range bases {
		t.Run(base.String(), func(t *testing.T) {
			t.Parallel()

			for attempts := 0; attempts <= 5; attempts++ {
				got := computeCIPendingDelay(base, attempts)
				want := computeCIPendingDelay(ciPendingBackoffBaseDefault, attempts)
				if got != want {
					t.Errorf("computeCIPendingDelay(%v, %d) = %v, want %v (same as default base)", base, attempts, got, want)
				}
			}
		})
	}
}

func TestReconcileCIStatus_BackoffUsesStatePollInterval(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	// Use a non-default poll interval of 30s (30000ms).
	entry := newPendingEntry("ISS-PPI-1", "ISS-PPI-1-ident", "feature/ppi", 1)
	entry.PendingAttempts = 1

	state := NewState(30000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-PPI-1", ReactionKindCI)] = entry
	state.Claimed["ISS-PPI-1"] = struct{}{}

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-PPI-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after CIStatusPending; want re-enqueued")
	}

	wantAttempts := 2
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	// With a 30s poll interval, retry at = now + 30s * 2^2 = now + 120s.
	wantDelay := computeCIPendingDelay(30*time.Second, wantAttempts)
	wantRetryAt := now.Add(wantDelay)
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (30s base backoff)", got.PendingRetryAt, wantRetryAt)
	}

	// Confirm it is NOT the old 10s-based schedule.
	oldSchedule := now.Add(computeCIPendingDelay(ciPendingBackoffBaseDefault, wantAttempts))
	if got.PendingRetryAt.Equal(oldSchedule) {
		t.Errorf("PendingRetryAt = %v matches old 10s-base schedule; want 30s-base schedule", got.PendingRetryAt)
	}
}

func TestReconcileCIStatus_BackoffSkip(t *testing.T) {
	t.Parallel()

	now := ciBaseTime
	futureRetry := now.Add(2 * time.Minute)

	entry := newPendingEntry("ISS-SKIP-1", "ISS-SKIP-1-ident", "feature/skip", 1)
	entry.PendingAttempts = 2
	entry.PendingRetryAt = futureRetry

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-SKIP-1", ReactionKindCI)] = entry
	state.Claimed["ISS-SKIP-1"] = struct{}{}

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	scm := defaultCISCM()
	params := ciParams(t, store, ci, nil, scm)
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// FetchCIStatus must not be called when PendingRetryAt is in the future.
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times during backoff window; want 0", ci.calls)
	}
	// GetMergeability must not be called either: the backoff gate runs
	// before the head read.
	if scm.calls != 0 {
		t.Errorf("GetMergeability called %d times during backoff window; want 0", scm.calls)
	}

	// Entry must be re-enqueued with identical PendingAttempts and PendingRetryAt.
	got, ok := state.PendingReactions[ReactionKey("ISS-SKIP-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry dropped during backoff skip; want re-enqueued")
	}
	if got.PendingAttempts != 2 {
		t.Errorf("PendingAttempts = %d, want 2 (unchanged)", got.PendingAttempts)
	}
	if !got.PendingRetryAt.Equal(futureRetry) {
		t.Errorf("PendingRetryAt = %v, want %v (unchanged)", got.PendingRetryAt, futureRetry)
	}
}

func TestReconcileCIStatus_BackoffIncrements_OnPending(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	entry := newPendingEntry("ISS-BIP-1", "ISS-BIP-1-ident", "feature/wip", 1)
	entry.PendingAttempts = 2

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-BIP-1", ReactionKindCI)] = entry
	state.Claimed["ISS-BIP-1"] = struct{}{}

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-BIP-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after CIStatusPending; want re-enqueued")
	}

	wantAttempts := 3
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	wantRetryAt := now.Add(computeCIPendingDelay(5*time.Second, wantAttempts))
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", got.PendingRetryAt, wantRetryAt)
	}

	if metrics.ciStatusChecks["pending"] != 1 {
		t.Errorf(`IncCIStatusChecks("pending") = %d, want 1`, metrics.ciStatusChecks["pending"])
	}
}

func TestReconcileCIStatus_BackoffIncrements_OnError(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	entry := newPendingEntry("ISS-BIE-1", "ISS-BIE-1-ident", "feature/err", 1)
	entry.PendingAttempts = 1

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-BIE-1", ReactionKindCI)] = entry
	state.Claimed["ISS-BIE-1"] = struct{}{}

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{err: errors.New("transient network error")}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-BIE-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after fetch error; want re-enqueued")
	}

	wantAttempts := 2
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	wantRetryAt := now.Add(computeCIPendingDelay(5*time.Second, wantAttempts))
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", got.PendingRetryAt, wantRetryAt)
	}

	if metrics.ciStatusChecks["error"] != 1 {
		t.Errorf(`IncCIStatusChecks("error") = %d, want 1`, metrics.ciStatusChecks["error"])
	}
}

// --- Watch window ---

// TestReconcileCIStatus_WatchWindow_MeasuredFromHeadRecordedAt covers
// an entry whose age past the last recorded head exceeds
// watch_window_ms is dropped with its counter, and one whose head moved
// inside the window is not.
func TestReconcileCIStatus_WatchWindow_MeasuredFromHeadRecordedAt(t *testing.T) {
	t.Parallel()

	t.Run("age past HeadRecordedAt beyond the window drops the entry and its counter", func(t *testing.T) {
		t.Parallel()

		const issueID = "ISS-CI-WW-DROP"
		entry := newPendingEntry(issueID, issueID+"-ident", "main", 1)
		entry.HeadRecordedAt = ciBaseTime
		entry.CreatedAt = ciBaseTime.Add(-10 * 24 * time.Hour) // far older than HeadRecordedAt

		state := NewState(5000, 4, nil, AgentTotals{})
		state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = entry
		state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
		state.Claimed[issueID] = struct{}{}

		store := &ciReconcileStore{}
		metrics := newCIMetricsSpy()
		scm := defaultCISCM()
		ci := &mockCIProvider{}
		params := ciParams(t, store, ci, nil, scm)
		params.CIWatchWindow = 30 * time.Minute
		params.NowFunc = func() time.Time { return ciBaseTime.Add(31 * time.Minute) }

		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; ok {
			t.Error("PendingReactions entry present after watch window elapsed since HeadRecordedAt; want dropped")
		}
		if _, ok := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; ok {
			t.Error("ReactionAttempts present after watch window elapsed; want dropped")
		}
		if scm.calls != 0 {
			t.Errorf("GetMergeability called %d times after watch window elapsed; want 0", scm.calls)
		}
	})

	t.Run("head moved inside the window is not dropped", func(t *testing.T) {
		t.Parallel()

		const issueID = "ISS-CI-WW-KEEP"
		entry := newPendingEntry(issueID, issueID+"-ident", "main", 1)
		entry.HeadRecordedAt = ciBaseTime
		entry.CreatedAt = ciBaseTime.Add(-10 * 24 * time.Hour)

		state := NewState(5000, 4, nil, AgentTotals{})
		state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = entry
		state.Claimed[issueID] = struct{}{}

		store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
		metrics := newCIMetricsSpy()
		scm := defaultCISCM()
		ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
		params := ciParams(t, store, ci, nil, scm)
		params.CIWatchWindow = 30 * time.Minute
		params.NowFunc = func() time.Time { return ciBaseTime.Add(29 * time.Minute) }

		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
			t.Error("PendingReactions entry dropped inside the watch window; want kept")
		}
		if scm.calls != 1 {
			t.Errorf("GetMergeability called %d times; want 1 (entry survived to the head read)", scm.calls)
		}
	})
}

// TestReconcileCIStatus_WatchWindow_ZeroNeverDrops verifies that
// watch_window_ms: 0 never drops the entry on age.
func TestReconcileCIStatus_WatchWindow_ZeroNeverDrops(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-WW-ZERO"
	entry := newPendingEntry(issueID, issueID+"-ident", "main", 1)
	entry.CreatedAt = ciBaseTime.Add(-365 * 24 * time.Hour)

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = entry
	state.Claimed[issueID] = struct{}{}

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.CIWatchWindow = 0
	params.NowFunc = func() time.Time { return ciBaseTime }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry dropped with CIWatchWindow=0; want never dropped on age")
	}
}

// TestReconcileCIStatus_DropOnAgeReleasesCounter verifies that the
// drop-on-age branch also deletes the ci reaction attempt counter, leaves
// a sibling kind's counter and the claim untouched, and performs no retry
// or fingerprint store call.
func TestReconcileCIStatus_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "CI-AGE-1"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.ReactionAttempts[ciKey] = 2
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[reviewKey] = 4
	delete(state.Claimed, issueID)

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := defaultCISCM()
	params := ciParams(t, store, &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}, nil, scm)
	params.CIWatchWindow = 30 * time.Minute
	params.NowFunc = func() time.Time { return ciBaseTime.Add(31 * time.Minute) }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ciKey]; ok {
		t.Error("PendingReactions[ci] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[ciKey]; ok {
		t.Error("ReactionAttempts[ci] present after drop-on-age; want removed")
	}
	if state.ReactionAttempts[reviewKey] != 4 {
		t.Errorf("ReactionAttempts[review] = %d, want 4 (untouched)", state.ReactionAttempts[reviewKey])
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("Claimed present after drop-on-age; want absent")
	}
	if len(store.deletedIssueIDs) != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", len(store.deletedIssueIDs))
	}
	if store.upsertFingerprintCalls != 0 || store.getFingerprintCalls != 0 || store.deleteFingerprintCalls != 0 {
		t.Errorf("fingerprint calls = upsert:%d get:%d delete:%d, want all 0",
			store.upsertFingerprintCalls, store.getFingerprintCalls, store.deleteFingerprintCalls)
	}
	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 (watch window exceeded before read)", scm.calls)
	}
}

// --- Watch-end conditions ---

func TestReconcileCIStatus_Merged_EndsWatch(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-MERGED"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "merged-sha", Merged: true, MergeCommitSHA: "mc-sha"}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("PendingReactions entry present after merged pull request; want dropped")
	}
	if _, ok := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("ReactionAttempts present after merged pull request; want dropped")
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times for a merged pull request; want 0", ci.calls)
	}
}

func TestReconcileCIStatus_NotFound_EndsWatch(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-NOTFOUND"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{err: &domain.SCMError{Kind: domain.ErrSCMNotFound, Message: "pull request not found"}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("PendingReactions entry present after ErrSCMNotFound; want dropped")
	}
	if _, ok := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("ReactionAttempts present after ErrSCMNotFound; want dropped")
	}
}

func TestReconcileCIStatus_OtherSCMError_BacksOff(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-SCMERR"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{err: &domain.SCMError{Kind: domain.ErrSCMTransport, Message: "connection reset"}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a transient SCM error; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 (backoff, not a watch-end condition)", entry.PendingAttempts)
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times after a mergeability fetch error; want 0", ci.calls)
	}
}

// TestReconcileCIStatus_EmptyHeadSHA_ReEnqueuesNoEpoch verifies that an
// empty status.HeadSHA re-enqueues, records no head, opens no epoch, and
// leaves both counters untouched.
func TestReconcileCIStatus_EmptyHeadSHA_ReEnqueuesNoEpoch(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-EMPTYHEAD"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: ""}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry dropped on empty HeadSHA; want re-enqueued")
	}
	if !entry.HeadRecordedAt.IsZero() {
		t.Errorf("HeadRecordedAt = %v, want zero (no epoch opened on empty head)", entry.HeadRecordedAt)
	}
	if state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1 (untouched)", issueID, state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)])
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1", entry.PendingAttempts)
	}
	if store.upsertFingerprintCalls != 0 || store.getFingerprintCalls != 0 {
		t.Errorf("fingerprint calls = get:%d upsert:%d, want both 0", store.getFingerprintCalls, store.upsertFingerprintCalls)
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times on empty head; want 0", ci.calls)
	}
}

// TestReconcileCIStatus_Closed_EndsWatch verifies that a pull request
// reported closed and not merged drops the entry and its counter on the
// next pass, not at window expiry.
func TestReconcileCIStatus_Closed_EndsWatch(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-CLOSED"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "closed-sha", Closed: true, Merged: false}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("PendingReactions entry present after a closed, unmerged pull request; want dropped")
	}
	if _, ok := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("ReactionAttempts present after a closed, unmerged pull request; want dropped")
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times for a closed pull request; want 0", ci.calls)
	}
}

// TestReconcileCIStatus_ClosedAndMerged_EndsWatchThroughMergedBranch
// verifies that a pull request reported both closed and
// merged ends the watch through the merged branch, logging the merge
// rather than a close.
func TestReconcileCIStatus_ClosedAndMerged_EndsWatchThroughMergedBranch(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-CLOSEDMERGED"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "merged-sha", Closed: true, Merged: true}}
	ci := &mockCIProvider{}
	handler := &sweepLogHandler{}
	params := ciParams(t, store, ci, nil, scm)
	params.Logger = slog.New(handler)

	reconcileCIStatus(state, params, slog.New(handler), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; ok {
		t.Error("PendingReactions entry present after a merged pull request; want dropped")
	}
	if _, ok := handler.findByMessage("ci watch ended: pull request merged"); !ok {
		t.Error(`log missing "ci watch ended: pull request merged" message; want the merged branch, not the closed branch`)
	}
	if handler.countByMessage("ci watch ended: pull request closed without merging") != 0 {
		t.Error(`log unexpectedly contains the closed-without-merging message for a merged pull request`)
	}
}

// --- Fingerprint dedup tests ---

// TestReconcileCIStatus_DedupSkip verifies that when GetReactionFingerprint
// returns the current head with dispatched=true, no FetchCIStatus call is
// made and the entry is re-enqueued.
func TestReconcileCIStatus_DedupSkip(t *testing.T) {
	t.Parallel()

	const head = "sha-already-done"
	state := stateWithPendingReaction(t, "ISS-FP-1", "main", 1)
	store := &ciReconcileStore{
		getFingerprintResult:     head,
		getFingerprintDispatched: true,
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: head}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times when entry already dispatched; want 0", ci.calls)
	}
	if _, ok := state.PendingReactions[ReactionKey("ISS-FP-1", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry consumed after dedup skip; want re-enqueued")
	}
	if store.upsertFingerprintCalls != 0 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 0 (head unchanged)", store.upsertFingerprintCalls)
	}
	if store.getFingerprintCalls != 1 {
		t.Errorf("GetReactionFingerprint calls = %d, want 1", store.getFingerprintCalls)
	}
}

// TestReconcileCIStatus_FingerprintReset verifies that when the stored
// fingerprint differs from the live head (head changed), the pass upserts
// the new head and proceeds to FetchCIStatus.
func TestReconcileCIStatus_FingerprintReset(t *testing.T) {
	t.Parallel()

	const newHead = "sha-new"
	state := stateWithPendingReaction(t, "ISS-FP-2", "main", 1)
	store := &ciReconcileStore{
		getFingerprintResult:     "sha-old",
		getFingerprintDispatched: true,
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: newHead}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 1 {
		t.Errorf("FetchCIStatus calls = %d, want 1 (head changed, dedup must not skip)", ci.calls)
	}
	if len(store.upsertedFingerprints) != 1 || store.upsertedFingerprints[0] != newHead {
		t.Errorf("upserted fingerprints = %v, want [%s]", store.upsertedFingerprints, newHead)
	}
}

// TestReconcileCIStatus_FingerprintGetError_Continues verifies that when
// GetReactionFingerprint returns an error the reconcile loop continues and
// FetchCIStatus is still called (best-effort dedup pattern).
func TestReconcileCIStatus_FingerprintGetError_Continues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-FP-4", "main", 1)
	store := &ciReconcileStore{
		getFingerprintErr: errors.New("db unavailable"),
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 1 {
		t.Errorf("FetchCIStatus calls = %d, want 1 even when GetReactionFingerprint errors", ci.calls)
	}
}

// TestReconcileCIStatus_UpgradePath_PreExistingFingerprintNotLiveHead
// verifies that a pre-existing fingerprint row holding a value that is not
// the live head (the pre-change frozen-ref semantics) produces exactly
// one unknown transition: the entry re-arms and no counter resets.
func TestReconcileCIStatus_UpgradePath_PreExistingFingerprintNotLiveHead(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-UPGRADE"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 1
	store := &ciReconcileStore{getFingerprintResult: "frozen-ref-from-prior-version"}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "current-live-head"}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if store.upsertFingerprintCalls != 1 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 1 (one upgrade transition)", store.upsertFingerprintCalls)
	}
	if state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1 (unknown attribution must not reset)", issueID, state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)])
	}
	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry missing after upgrade transition; want re-enqueued")
	}
	if entry.HeadRecordedAt.IsZero() {
		t.Error("HeadRecordedAt still zero after the upgrade transition; want set")
	}
}

// --- One head read, one epoch transition, one status fetch per tick ---

// TestReconcileCIStatus_OneHeadReadOneEpochTransitionOneStatusFetchPerTick
// verifies that across one poll interval (one call to reconcileCIStatus),
// exactly one head read, one epoch transition, and one FetchCIStatus call
// occur for the pending entry, even though the underlying adapter is
// primed to answer three different heads across successive calls.
func TestReconcileCIStatus_OneHeadReadOneEpochTransitionOneStatusFetchPerTick(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-ONETICK"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	heads := []string{"head-1", "head-2", "head-3"}
	call := 0
	scm := &ciReconcileSCM{fn: func() (domain.PRMergeStatus, error) {
		head := heads[min(call, len(heads)-1)]
		call++
		return domain.PRMergeStatus{HeadSHA: head}, nil
	}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, scm)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 1 {
		t.Errorf("GetMergeability calls = %d, want 1 (one head read per due tick)", scm.calls)
	}
	if store.upsertFingerprintCalls != 1 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 1 (one epoch transition)", store.upsertFingerprintCalls)
	}
	if ci.calls != 1 {
		t.Errorf("FetchCIStatus calls = %d, want 1", ci.calls)
	}
}

// --- Dispatch marking ---

// TestReconcileCIStatus_Failing_DoesNotMarkDispatched verifies that
// handleCIFailure never calls MarkReactionDispatched: CI dispatch tracking
// is entirely fingerprint-based on the live head, with no separate
// dispatched marker.
func TestReconcileCIStatus_Failing_DoesNotMarkDispatched(t *testing.T) {
	t.Parallel()

	// ReactionAttempts=0 → under maxRetries=2, so handleCIFailure schedules retry.
	state := stateWithPendingReaction(t, "ISS-FP-5", "main", 1)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, nil, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-FP-5"]; !ok {
		t.Error("retry not scheduled after CI failure; want scheduled")
	}
}

// --- Escalation error metrics ---

func TestEscalateCIFailure_LabelFailure_IncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	tracker := &ciTrackerStub{addLabelErr: errors.New("tracker unavailable")}

	state := stateWithPendingReaction(t, "ESC-ERR-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-ERR-1", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["error"] != 1 {
		t.Errorf(`IncCIEscalations("error") = %d, want 1 on label tracker failure`, metrics.ciEscalations["error"])
	}
	if metrics.ciEscalations["label"] != 0 {
		t.Errorf(`IncCIEscalations("label") = %d, want 0 on label tracker failure`, metrics.ciEscalations["label"])
	}
}

func TestEscalateCIFailure_CommentFailure_IncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	tracker := &ciTrackerStub{commentIssueErr: errors.New("tracker unavailable")}

	state := stateWithPendingReaction(t, "ESC-ERR-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-ERR-2", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["error"] != 1 {
		t.Errorf(`IncCIEscalations("error") = %d, want 1 on comment tracker failure`, metrics.ciEscalations["error"])
	}
	if metrics.ciEscalations["comment"] != 0 {
		t.Errorf(`IncCIEscalations("comment") = %d, want 0 on comment tracker failure`, metrics.ciEscalations["comment"])
	}
}

// TestEscalateCIFailure_NilTracker_ZeroIncrements verifies that when
// TrackerAdapter is nil the escalation path spawns no goroutine and
// IncCIEscalations is never called.
func TestEscalateCIFailure_NilTracker_ZeroIncrements(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ESC-NIL-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-NIL-1", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil, defaultCISCM()) // nil TrackerAdapter
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if len(metrics.ciEscalations) != 0 {
		t.Errorf("IncCIEscalations called with nil TrackerAdapter; want zero increments, got %v", metrics.ciEscalations)
	}
}

// TestEscalateCIFailure_NilTracker_ZeroIncrements_Comment verifies the same
// zero-increment guarantee for the comment escalation path with nil tracker.
func TestEscalateCIFailure_NilTracker_ZeroIncrements_Comment(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ESC-NIL-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-NIL-2", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil, defaultCISCM()) // nil TrackerAdapter
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if len(metrics.ciEscalations) != 0 {
		t.Errorf("IncCIEscalations called with nil TrackerAdapter; want zero increments, got %v", metrics.ciEscalations)
	}
}

// TestEscalateCIFailure_FingerprintSurvivesEscalation verifies that
// escalateCIFailure no longer calls DeleteReactionFingerprint: the row is
// the epoch record and must survive.
func TestEscalateCIFailure_FingerprintSurvivesEscalation(t *testing.T) {
	t.Parallel()

	// ReactionAttempts=2, maxRetries=2 → next increment (→3) triggers escalation.
	state := stateWithPendingReaction(t, "ISS-FP-6", "main", 3)
	state.ReactionAttempts[ReactionKey("ISS-FP-6", ReactionKindCI)] = 2
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Wait for any escalation goroutine to finish.
	state.TrackerOpsWg.Wait()

	if store.deleteFingerprintCalls != 0 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 0 during CI escalation (fingerprint is the epoch record)", store.deleteFingerprintCalls)
	}
	// Claim must be released.
	if _, ok := state.Claimed["ISS-FP-6"]; ok {
		t.Error("claim not released after escalation")
	}
}

// TestReconcileCIStatus_Failing_ExceedsMaxRetries_CrossKindIsolation verifies
// that CI escalation touches only the CI reaction's own pending entry and
// counter. A sibling merge-completion entry parked on the same issue
// (seeded from the same worker exit as the CI reaction) must survive
// untouched.
func TestReconcileCIStatus_Failing_ExceedsMaxRetries_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	issueID := "ISS-CI-ISO"
	state := stateWithPendingReaction(t, issueID, "main", 3)
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 2

	mcKey := ReactionKey(issueID, ReactionKindMergeCompletion)
	state.PendingReactions[mcKey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindMergeCompletion,
		CreatedAt:  ciBaseTime,
		KindData:   &MergeCompletionReactionData{PRNumber: 42, Owner: "owner", Repo: "repo"},
	}
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindMergeCompletion)] = 1

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker, defaultCISCM())

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if _, ok := state.PendingReactions[mcKey]; !ok {
		t.Error("sibling merge-completion PendingReactions entry removed by CI escalation; want untouched")
	}
	if state.ReactionAttempts[ReactionKey(issueID, ReactionKindMergeCompletion)] != 1 {
		t.Error("sibling merge-completion ReactionAttempts counter altered by CI escalation; want untouched")
	}
	if store.deleteFingerprintCalls != 0 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 0 (escalation no longer deletes the epoch record)", store.deleteFingerprintCalls)
	}
}

// --- Retry-slot arbitration tests ---

// TestReconcileCIStatus_Failing_DeferralLeavesNoOrphanedRow seeds a
// persisted retry row and a matching in-memory continuation entry, then
// drives the CI pass with a failing status for the same issue. The
// continuation entry must stay the incumbent, no ci retry may replace
// it, and the persisted row must be left exactly as seeded: a
// displacement that cancelled the in-memory entry without deleting its
// persisted row would leave an orphan, which this test would catch via
// LoadRetryEntries.
func TestReconcileCIStatus_Failing_DeferralLeavesNoOrphanedRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openInMemoryStore(t)

	const issueID = "ISS-CI-ORPHAN"
	seeded := persistence.RetryEntry{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Attempt:    5,
		DueAtMs:    ciBaseTime.UnixMilli() + 60_000,
	}
	if err := store.SaveRetryEntry(ctx, seeded); err != nil {
		t.Fatalf("SaveRetryEntry: %v", err)
	}

	state := stateWithPendingReaction(t, issueID, "main", 1)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Attempt:    5,
		DueAtMS:    seeded.DueAtMs,
		// ReactionKind empty marks a continuation incumbent.
	}

	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ReconcileParams{
		CIProvider:     ci,
		CIFeedback:     defaultCIFeedback(),
		CIWatchWindow:  24 * time.Hour,
		SCMAdapter:     defaultCISCM(),
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            ctx,
		Logger:         discardLogger(),
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
	}

	reconcileCIStatus(state, params, discardLogger(), ctx, metrics)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("RetryAttempts entry removed while a continuation incumbent occupied the slot; want preserved")
	}
	if incumbent.ReactionKind != "" {
		t.Errorf("RetryAttempts.ReactionKind = %q, want empty (continuation entry unchanged)", incumbent.ReactionKind)
	}
	if incumbent.Attempt != 5 {
		t.Errorf("RetryAttempts.Attempt = %d, want 5 (unchanged)", incumbent.Attempt)
	}

	rows, err := store.LoadRetryEntries(ctx)
	if err != nil {
		t.Fatalf("LoadRetryEntries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LoadRetryEntries returned %d rows, want 1", len(rows))
	}
	if rows[0].IssueID != issueID {
		t.Errorf("persisted row IssueID = %q, want %q", rows[0].IssueID, issueID)
	}
	if rows[0].Attempt != seeded.Attempt {
		t.Errorf("persisted row Attempt = %d, want %d (unchanged)", rows[0].Attempt, seeded.Attempt)
	}
	if rows[0].DueAtMs != seeded.DueAtMs {
		t.Errorf("persisted row DueAtMs = %d, want %d (unchanged)", rows[0].DueAtMs, seeded.DueAtMs)
	}
}

// TestReconcileCIStatus_Pending_LeavesCreatedAtUnchanged covers the
// negative side of the CreatedAt refresh rule: the CIStatusPending arm
// is the pass's own transient backoff, not an arbitration deferral, so
// it must not touch CreatedAt even though PendingAttempts advances.
func TestReconcileCIStatus_Pending_LeavesCreatedAtUnchanged(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TTL-NEG"
	state := stateWithPendingReaction(t, issueID, "feature/ttl", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)
	seededCreatedAt := ciBaseTime.Add(-5 * time.Minute)
	state.PendingReactions[rkey].CreatedAt = seededCreatedAt

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.NowFunc = func() time.Time { return ciBaseTime }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on CI pending; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(seededCreatedAt) {
		t.Errorf("CreatedAt = %v, want unchanged %v", entry.CreatedAt, seededCreatedAt)
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 (the tick ran)", entry.PendingAttempts)
	}
}

// --- End-to-end attribution tests against a real persistence store ---

// TestReconcileCIStatus_CIFailedRunHistoryExcludedFromAttribution verifies
// that a run_history row with status ci_failed and completed_at inside
// the attribution interval does not make the next head change unknown.
// The ci_failed row is dispatched through handleCIFailure itself, as
// part of a real escalation soft stop (a real CI failure, not a
// synthesized fixture), so this test fails against an implementation
// that counts that row as a worker session.
func TestReconcileCIStatus_CIFailedRunHistoryExcludedFromAttribution(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := openInMemoryStore(t)

	const issueID = "ISS-CI-EXCLUDE"
	state := stateWithPendingReaction(t, issueID, "main", 3)
	// Pre-seeded at the budget: the pass below escalates immediately,
	// writing exactly one ci_failed row through handleCIFailure and
	// soft-stopping (the entry survives with its HeadRecordedAt set).
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] = 2
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "head-1"}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	tracker := &ciTrackerStub{}
	metrics := newCIMetricsSpy()

	now := ciBaseTime
	params := ReconcileParams{
		CIProvider:     ci,
		CIFeedback:     defaultCIFeedback(),
		CIWatchWindow:  24 * time.Hour,
		SCMAdapter:     scm,
		TrackerAdapter: tracker,
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            ctx,
		Logger:         discardLogger(),
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
		NowFunc:        func() time.Time { return now },
	}

	reconcileCIStatus(state, params, discardLogger(), ctx, metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["label"] != 1 {
		t.Fatalf(`IncCIEscalations("label") = %d, want 1 (escalation soft stop wrote the ci_failed row)`, metrics.ciEscalations["label"])
	}
	entry, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry missing after the soft stop; want present")
	}
	if entry.HeadRecordedAt.IsZero() {
		t.Fatal("HeadRecordedAt still zero after the soft stop; want set")
	}

	// The head changes; no worker session other than the ci_failed
	// write occurred inside the interval.
	now = ciBaseTime.Add(10 * time.Minute)
	entry.PendingRetryAt = time.Time{}
	scm.result = domain.PRMergeStatus{HeadSHA: "head-2"}

	reconcileCIStatus(state, params, discardLogger(), ctx, metrics)

	if got := state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)]; got != 1 {
		t.Errorf("ReactionAttempts after a head change with only a ci_failed row in the interval = %d, want 1 (the counter reset to 0 and then incremented once; the ci_failed row must not count as a worker session)", got)
	}
}

// TestReconcileCIStatus_ThreeSelfPushedFixCommits_EscalateAfterTotalBudget
// verifies that three successive self-pushed fix commits, each failing,
// escalate after max_retries continuations in total rather than
// max_retries per commit. Each fix commit's own worker exit reseeds the
// watch with HeadRecordedAt zero, so every post-reseed head observation
// answers unknown on the zero-boundary path and the counter never
// resets, exhausting the budget across, not within, a single commit.
func TestReconcileCIStatus_ThreeSelfPushedFixCommits_EscalateAfterTotalBudget(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TOTALBUDGET"
	state := stateWithPendingReaction(t, issueID, "main", 1)
	store := &ciReconcileStore{}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "head-0"}}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	tracker := &ciTrackerStub{}
	metrics := newCIMetricsSpy()
	params := ciParams(t, store, ci, tracker, scm)

	heads := []string{"head-0", "head-1", "head-2"}
	var now time.Time
	for i, head := range heads {
		now = ciBaseTime.Add(time.Duration(i) * 10 * time.Minute)
		if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; !ok {
			reseedCIPendingEntry(state, issueID, "main", 1, now)
			delete(state.RetryAttempts, issueID)
		}
		scm.result = domain.PRMergeStatus{HeadSHA: head}
		entry := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]
		entry.PendingRetryAt = time.Time{}

		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	}

	state.TrackerOpsWg.Wait()

	if state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)] != 3 {
		t.Errorf("ReactionAttempts after three self-pushed fix commits = %d, want 3 (accumulated, not reset per commit)",
			state.ReactionAttempts[ReactionKey(issueID, ReactionKindCI)])
	}
	if metrics.ciEscalations["label"] != 1 {
		t.Errorf(`IncCIEscalations("label") = %d, want 1 (escalated once the total budget was exceeded)`, metrics.ciEscalations["label"])
	}
}

// TestReconcileCIStatus_UpsertFailure_DefersEpochTransition verifies that a
// failed fingerprint upsert defers the epoch transition instead of applying
// it against a durable record that did not advance. The transition restarts
// the watch clock and re-arms the once-per-epoch escalation, so applying it
// on every pass while the write keeps failing would leave the entry unable
// to age out and would let the escalation fire once per pass.
func TestReconcileCIStatus_UpsertFailure_DefersEpochTransition(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-UPSERTFAIL"
	recordedAt := time.Now().UTC().Add(-time.Hour)

	state := stateWithPendingReaction(t, issueID, "main", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)
	state.PendingReactions[rkey].HeadRecordedAt = recordedAt
	state.PendingReactions[rkey].EscalatedForCurrentHead = true
	state.ReactionAttempts[rkey] = 1

	store := &ciReconcileStore{
		getFingerprintResult: "old-head",
		upsertFingerprintErr: errors.New("database is locked"),
	}
	metrics := newCIMetricsSpy()
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "new-head"}}
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, scm)

	// Two passes: one failed write is a deferral, and a second proves the
	// age basis does not creep forward while the write keeps failing.
	for range 2 {
		if entry, ok := state.PendingReactions[rkey]; ok {
			entry.PendingRetryAt = time.Time{}
		}
		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	}

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a failed fingerprint upsert; want re-enqueued")
	}
	if !entry.HeadRecordedAt.Equal(recordedAt) {
		t.Errorf("HeadRecordedAt = %v, want %v (unchanged: the durable head never advanced)", entry.HeadRecordedAt, recordedAt)
	}
	if !entry.EscalatedForCurrentHead {
		t.Error("EscalatedForCurrentHead cleared while the durable head never advanced; the soft stop would re-escalate every pass")
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1 (untouched by a deferred transition)", issueID, state.ReactionAttempts[rkey])
	}
	if entry.PendingAttempts != 2 {
		t.Errorf("PendingAttempts = %d, want 2 (backoff advanced once per failed pass)", entry.PendingAttempts)
	}
	if store.upsertFingerprintCalls != 2 {
		t.Errorf("upsert attempts = %d, want 2 (retried on the next pass)", store.upsertFingerprintCalls)
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times after a failed upsert; want 0 (the pass ends before the status read)", ci.calls)
	}
}

// --- Triage gate integration ---

// escalateTriageScript and dispatchAgentTriageScript answer "escalate"
// and "dispatch-agent" respectively; handledScript (defined in
// reaction_triage_test.go) answers "handled".
var (
	escalateTriageScript      = triageVerdictScript("escalate")
	dispatchAgentTriageScript = triageVerdictScript("dispatch-agent")
)

// mustPendingReaction returns state.PendingReactions[rkey], failing the
// test immediately rather than letting a caller dereference a nil
// pointer when a reconcile pass consumed or never created the entry.
func mustPendingReaction(t *testing.T, state *State, rkey string) *PendingReaction {
	t.Helper()
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry == nil {
		t.Fatalf("PendingReactions[%s] = nil, want a retained entry", rkey)
	}
	return entry
}

// ciTriageParams returns ciParams wired with a real workspace and the
// given triage script, so reactionTriageGate actually starts a
// subprocess for the pass's failing entry.
func ciTriageParams(t *testing.T, store *ciReconcileStore, ci domain.CIStatusProvider, tracker domain.TrackerAdapter, scm domain.SCMAdapter, workspaceRoot, script string) ReconcileParams {
	t.Helper()
	params := ciParams(t, store, ci, tracker, scm)
	params.WorkspaceRoot = workspaceRoot
	params.CITriage = config.ReactionTriageConfig{Script: script, TimeoutMS: 5000}
	return params
}

// runCITriageToCompletion drives a pass that starts a triage run for
// issueID, waits for the subprocess to finish, then resets the entry's
// PendingRetryAt to the past so the next pass is immediately due.
func runCITriageToCompletion(t *testing.T, state *State, params ReconcileParams, rkey string, metrics domain.Metrics) {
	t.Helper()
	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}
}

// TestReconcileCIStatus_Triage_NoConfig_BehavesAsPinned verifies that a
// ci_failure reaction with no triage block dispatches exactly as the
// pinned revision, with no extra provider call, no subprocess, and no
// change to any counter or entry.
func TestReconcileCIStatus_Triage_NoConfig_BehavesAsPinned(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-OFF"
	state := stateWithPendingReaction(t, issueID, "feature/off", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)
	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	// WorkspaceRoot and CITriage are left at their zero values.

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// handleCIFailure's under-budget branch schedules a continuation and
	// does not re-insert the entry: this is the pinned behavior a
	// disabled triage gate must reproduce exactly.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a scheduled continuation, want dropped (pinned behavior)")
	}
	if len(store.runHistories) != 1 {
		t.Errorf("AppendRunHistory calls = %d, want 1 (handleCIFailure's normal dispatch path)", len(store.runHistories))
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
	if _, scheduled := state.RetryAttempts[issueID]; !scheduled {
		t.Error("no retry scheduled, want the normal CI-fix continuation")
	}
}

// TestReconcileCIStatus_Triage_WaitsWithoutProviderCall verifies that
// while a triage run is in flight, the pass re-enqueues without making
// a provider call and without incrementing PendingAttempts.
func TestReconcileCIStatus_Triage_WaitsWithoutProviderCall(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-WAIT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/wait", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)
	state.PendingReactions[rkey].Triage = inFlightTriageRun("sha-wait", func() {})

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, nil, scm, root, handledScript)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("GetMergeability calls = %d, want 0 while a triage run is in flight", scm.calls)
	}
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus calls = %d, want 0 while a triage run is in flight", ci.calls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped while waiting on triage, want re-enqueued")
	}
	if entry.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 (waiting is not a fetch error)", entry.PendingAttempts)
	}
}

// TestReconcileCIStatus_Triage_Handled verifies that a handled
// disposition marks the fingerprint dispatched and re-enqueues the
// entry with the already-dispatched branch's delay, without
// incrementing ReactionAttempts, calling ScheduleRetry, or appending a
// run_history row.
func TestReconcileCIStatus_Triage_Handled(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-HANDLED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/handled", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, nil, scm, root, handledScript)

	runCITriageToCompletion(t, state, params, rkey, metrics)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 (the already-dispatched branch's delay)", entry.PendingAttempts)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a handled verdict must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory calls = %d, want 0", len(store.runHistories))
	}
	if _, scheduled := state.RetryAttempts[issueID]; scheduled {
		t.Error("a retry was scheduled for a handled verdict, want none")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileCIStatus_Triage_Handled_NoSecondRunHistoryOnReplay extends
// the handled scenario across a third pass over the same unchanged
// fingerprint, verifying the memoized outcome is re-applied rather than
// re-run, and that run_history stays empty.
func TestReconcileCIStatus_Triage_Handled_NoSecondRunHistoryOnReplay(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-REPLAY"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/replay", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, nil, scm, root, handledScript)

	runCITriageToCompletion(t, state, params, rkey, metrics)
	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics) // applies: triageHandled

	for range 2 {
		if entry, ok := state.PendingReactions[rkey]; ok {
			entry.PendingRetryAt = time.Time{}
		}
		reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics) // memoized replays
	}

	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory calls = %d, want 0 across repeated passes over one fingerprint", len(store.runHistories))
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1 (the retained handle is re-applied from memory)", store.markDispatchedCalls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped across replays, want re-enqueued")
	}
	if entry.Triage == nil {
		t.Fatal("PendingReaction.Triage cleared across replays, want the memoized handle retained")
	}
}

// TestReconcileCIStatus_Triage_Escalate verifies that an escalate
// disposition marks the fingerprint dispatched and invokes
// escalateCIFailure with EscalationTriggerTriage and the un-incremented
// attempt count, without spending a continuation.
func TestReconcileCIStatus_Triage_Escalate(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-ESCALATE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/escalate", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, tracker, scm, root, escalateTriageScript)

	runCITriageToCompletion(t, state, params, rkey, metrics)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (triage escalation uses the kind's own escalation action)", tracker.addLabelCalled)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a triage escalation must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("claim not released after a triage escalation, want released")
	}
}

// TestReconcileCIStatus_Triage_Escalate_CommentTextNamesTriage verifies
// that on the comment escalation action, a triage-triggered
// escalation's posted text states that a triage command requested it
// and does not claim a budget was exhausted.
func TestReconcileCIStatus_Triage_Escalate_CommentTextNamesTriage(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-COMMENT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/comment", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, tracker, scm, root, escalateTriageScript)
	params.CIFeedback.Escalation = "comment"

	runCITriageToCompletion(t, state, params, rkey, metrics)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.commentIssueCalls != 1 {
		t.Fatalf("CommentIssue calls = %d, want 1", tracker.commentIssueCalls)
	}
	if !strings.Contains(tracker.lastComment, "triage") {
		t.Errorf("comment = %q, want it to name the triage command", tracker.lastComment)
	}
	if strings.Contains(tracker.lastComment, "retries exhausted") || strings.Contains(tracker.lastComment, "retry budget") {
		t.Errorf("comment = %q, want no claim that a budget was exhausted", tracker.lastComment)
	}
}

// TestReconcileCIStatus_Triage_DispatchAgent_ProceedsNormally verifies
// that a dispatch-agent disposition leaves every counter, fingerprint,
// and entry exactly as the pass would with no triage configured,
// falling through to the existing dispatch block.
func TestReconcileCIStatus_Triage_DispatchAgent_ProceedsNormally(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-DISPATCH"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/dispatch", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	store := &ciReconcileStore{getFingerprintResult: ciDefaultHead}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := defaultCISCM()
	params := ciTriageParams(t, store, ci, nil, scm, root, dispatchAgentTriageScript)

	runCITriageToCompletion(t, state, params, rkey, metrics)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if len(store.runHistories) != 1 {
		t.Errorf("AppendRunHistory calls = %d, want 1 (falls through to handleCIFailure)", len(store.runHistories))
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 for dispatch-agent", store.markDispatchedCalls)
	}
	if _, scheduled := state.RetryAttempts[issueID]; !scheduled {
		t.Error("no retry scheduled for a dispatch-agent verdict, want the normal CI-fix continuation")
	}
}

// TestReconcileCIStatus_Triage_CancelOnWatchWindowDrop verifies that an
// in-flight triage run is cancelled before the entry is dropped on
// watch-window elapse.
func TestReconcileCIStatus_Triage_CancelOnWatchWindowDrop(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-DROP"
	state := stateWithPendingReaction(t, issueID, "feature/drop", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)
	spy := &triageCancelSpy{}
	state.PendingReactions[rkey].Triage = inFlightTriageRun("sha-drop", spy.cancel)
	// ciBaseTime is the fixed clock ciParams' NowFunc reports; the age
	// basis must be measured against it, not the real wall clock.
	state.PendingReactions[rkey].HeadRecordedAt = ciBaseTime.Add(-48 * time.Hour)

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil, defaultCISCM())
	params.CIWatchWindow = time.Hour

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if spy.calls() != 1 {
		t.Errorf("Cancel called %d times, want 1 (the in-flight run must not outlive the dropped entry)", spy.calls())
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the watch window, want dropped")
	}
}

// TestReconcileCIStatus_Triage_UnboundedAcrossSuccessiveHeads pins the
// accepted risk that ci carries and its siblings do not: the watch
// window is measured from the last recorded head, so a handled verdict
// on each of two successive heads leaves the entry present past the
// window measured from the first head.
func TestReconcileCIStatus_Triage_UnboundedAcrossSuccessiveHeads(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CI-TRIAGE-UNBOUNDED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithPendingReaction(t, issueID, "feature/unbounded", 1)
	rkey := ReactionKey(issueID, ReactionKindCI)

	const window = time.Second
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// The entry's age is measured from CreatedAt until a head is
	// recorded; align it with the mocked clock so the watch window
	// check has a meaningful reference point from the first tick.
	state.PendingReactions[rkey].CreatedAt = base
	now := base
	nowFunc := func() time.Time { return now }

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	scm := &ciReconcileSCM{result: domain.PRMergeStatus{HeadSHA: "sha-head-1"}}
	params := ciTriageParams(t, store, ci, nil, scm, root, handledScript)
	params.NowFunc = nowFunc
	params.CIWatchWindow = window

	// First head: the epoch transition records HeadRecordedAt at "now".
	runCITriageToCompletion(t, state, params, rkey, metrics)
	// Applies the handled verdict for the first head.
	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if got := mustPendingReaction(t, state, rkey).HeadRecordedAt; !got.Equal(base) {
		t.Fatalf("HeadRecordedAt after the first head = %v, want %v", got, base)
	}

	// A second head arrives, comfortably inside the window measured from
	// the first head, and moves HeadRecordedAt forward.
	now = base.Add(window / 2)
	scm.result = domain.PRMergeStatus{HeadSHA: "sha-head-2"}
	runCITriageToCompletion(t, state, params, rkey, metrics)
	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	secondHeadRecordedAt := mustPendingReaction(t, state, rkey).HeadRecordedAt
	if !secondHeadRecordedAt.Equal(now) {
		t.Fatalf("HeadRecordedAt after the second head = %v, want %v", secondHeadRecordedAt, now)
	}

	// Advance past the window measured from the FIRST head, but still
	// inside the window measured from the SECOND (latest) head.
	now = base.Add(window + window/5)
	if elapsed := now.Sub(base); elapsed <= window {
		t.Fatalf("test setup error: elapsed from the first head = %v, want > %v", elapsed, window)
	}
	if elapsed := now.Sub(secondHeadRecordedAt); elapsed >= window {
		t.Fatalf("test setup error: elapsed from the second head = %v, want < %v", elapsed, window)
	}
	state.PendingReactions[rkey].PendingRetryAt = time.Time{}

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("entry dropped even though the watch window is measured from the last recorded head, not the first")
	}
}
