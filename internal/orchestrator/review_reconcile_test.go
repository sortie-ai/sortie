package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// mockSCMAdapter is a controllable SCMAdapter for review reconcile tests.
type mockSCMAdapter struct {
	comments []domain.ReviewComment
	err      error
	calls    int

	botComments []domain.ReviewComment
	botErr      error
	botCalls    int
}

var _ domain.SCMAdapter = (*mockSCMAdapter)(nil)

func (m *mockSCMAdapter) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.comments, nil
}

func (m *mockSCMAdapter) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	m.botCalls++
	if m.botErr != nil {
		return nil, m.botErr
	}
	return m.botComments, nil
}

func (m *mockSCMAdapter) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}

func (m *mockSCMAdapter) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}

func (m *mockSCMAdapter) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{}, nil
}

func (m *mockSCMAdapter) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}

func (m *mockSCMAdapter) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockSCMAdapter) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}

func (m *mockSCMAdapter) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// reviewReconcileStore is a self-contained ReconcileStore for review tests.
type reviewReconcileStore struct {
	unsupportedReactionObservationStore

	savedEntries    []persistence.RetryEntry
	deletedIssueIDs []string

	saveRetryEntryErr   error
	deleteRetryEntryErr error

	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int

	getFingerprintResult     string
	getFingerprintDispatched bool
	getFingerprintErr        error
	upsertFingerprintErr     error
	markDispatchedErr        error
	deleteFingerprintErr     error
}

var _ ReconcileStore = (*reviewReconcileStore)(nil)

func (s *reviewReconcileStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.savedEntries = append(s.savedEntries, entry)
	return s.saveRetryEntryErr
}

func (s *reviewReconcileStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.deletedIssueIDs = append(s.deletedIssueIDs, issueID)
	return s.deleteRetryEntryErr
}

func (s *reviewReconcileStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (s *reviewReconcileStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	s.upsertFingerprintCalls++
	return s.upsertFingerprintErr
}

func (s *reviewReconcileStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	s.getFingerprintCalls++
	return s.getFingerprintResult, s.getFingerprintDispatched, s.getFingerprintErr
}

func (s *reviewReconcileStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	s.markDispatchedCalls++
	return s.markDispatchedErr
}

func (s *reviewReconcileStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	s.deleteFingerprintCalls++
	return s.deleteFingerprintErr
}

func (s *reviewReconcileStore) AddReactionHandedOffComments(_ context.Context, _, _ string, _ []string) error {
	return nil
}

func (s *reviewReconcileStore) ListReactionHandedOffComments(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

// reviewTrackerStub satisfies domain.TrackerAdapter for escalation tests.
type reviewTrackerStub struct {
	addLabelCalled    int
	commentIssueCalls int
}

var _ domain.TrackerAdapter = (*reviewTrackerStub)(nil)

func (s *reviewTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *reviewTrackerStub) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *reviewTrackerStub) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (s *reviewTrackerStub) CommentIssue(_ context.Context, _ string, _ string) error {
	s.commentIssueCalls++
	return nil
}
func (s *reviewTrackerStub) CommentIssueWithLiteral(_ context.Context, _, _, _ string) error {
	return nil
}
func (s *reviewTrackerStub) AddLabel(_ context.Context, _ string, _ string) error {
	s.addLabelCalled++
	return nil
}

// reviewMetricsSpy records review-specific metric calls.
type reviewMetricsSpy struct {
	domain.NoopMetrics
	reviewChecks      map[string]int
	reviewEscalations map[string]int
}

func newReviewMetricsSpy() *reviewMetricsSpy {
	return &reviewMetricsSpy{
		reviewChecks:      make(map[string]int),
		reviewEscalations: make(map[string]int),
	}
}

func (s *reviewMetricsSpy) IncReviewChecks(result string)      { s.reviewChecks[result]++ }
func (s *reviewMetricsSpy) IncReviewEscalations(action string) { s.reviewEscalations[action]++ }

// reviewBaseTime is a fixed reference for review reconcile tests.
var reviewBaseTime = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

// newReviewPendingEntry builds a PendingReaction with Kind=ReactionKindReview.
func newReviewPendingEntry(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindReview,
		CreatedAt:  reviewBaseTime,
		KindData: &ReviewReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   "feature/fix",
		},
	}
}

// stateWithReviewReaction creates a State with one review PendingReaction.
func stateWithReviewReaction(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, 0, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindReview)
	s.PendingReactions[rkey] = newReviewPendingEntry(issueID, prNumber)
	s.Claimed[issueID] = struct{}{}
	return s
}

// defaultReviewConfig returns a ReviewReactionConfig with sensible defaults.
func defaultReviewConfig() ReviewReactionConfig {
	return ReviewReactionConfig{
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		PollIntervalMS:       60000,
		DebounceMS:           30000,
		MaxContinuationTurns: 3,
	}
}

// Log messages asserted by the bot-allowlist exclusion table test, kept in
// sync with the literal strings reconcileReviewComments logs.
const (
	reviewExclusionDebugMsg = "review comments excluded by bot allowlist"
	reviewDispatchInfoMsg   = "review comments detected, scheduling review-fix dispatch"
)

// findLogLine returns the first line in logOutput whose msg attribute
// equals msg, or the empty string when no such line exists.
func findLogLine(logOutput, msg string) string {
	want := `msg="` + msg + `"`
	for line := range strings.SplitSeq(logOutput, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

// assertLogLineHasIntAttr fails the test unless logOutput contains a line
// for msg carrying key=want as a whole attribute (word-boundary matched,
// so a want of 1 cannot be satisfied by an actual value of 10).
func assertLogLineHasIntAttr(t *testing.T, logOutput, msg, key string, want int) {
	t.Helper()
	line := findLogLine(logOutput, msg)
	if line == "" {
		t.Fatalf("log output missing line with msg %q; log=%s", msg, logOutput)
	}
	pattern := regexp.MustCompile(fmt.Sprintf(`\b%s=%d\b`, regexp.QuoteMeta(key), want))
	if !pattern.MatchString(line) {
		t.Errorf("log line %q missing %s=%d", line, key, want)
	}
}

// assertLogLacksLine fails the test if logOutput contains any line for msg.
func assertLogLacksLine(t *testing.T, logOutput, msg string) {
	t.Helper()
	if line := findLogLine(logOutput, msg); line != "" {
		t.Errorf("log output contains unexpected line with msg %q: %s", msg, line)
	}
}

// reviewParams returns ReconcileParams wired for review reconcile tests.
func reviewParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter: tracker,
		SCMAdapter:     scm,
		ReviewConfig:   defaultReviewConfig(),
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            context.Background(),
		Logger:         discardLogger(),
		NowFunc:        func() time.Time { return reviewBaseTime },
	}
}

type fingerprintModelStore struct {
	reviewReconcileStore
	fingerprints map[string]*modelFingerprint
	rows         map[string]map[string]struct{}

	addCalls  [][]string
	listCalls int
	addErr    error
	listErr   error
}

var (
	_ ReconcileStore  = (*fingerprintModelStore)(nil)
	_ RetryTimerStore = (*fingerprintModelStore)(nil)
	_ WorkerExitStore = (*fingerprintModelStore)(nil)
)

type modelFingerprint struct {
	value      string
	dispatched bool
}

func newFingerprintModelStore() *fingerprintModelStore {
	return &fingerprintModelStore{
		fingerprints: make(map[string]*modelFingerprint),
		rows:         make(map[string]map[string]struct{}),
	}
}

func (s *fingerprintModelStore) AddReactionHandedOffComments(_ context.Context, issueID, kind string, commentIDs []string) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.addCalls = append(s.addCalls, slices.Clone(commentIDs))
	s.seedRows(issueID, kind, commentIDs...)
	return nil
}

func (s *fingerprintModelStore) ListReactionHandedOffComments(_ context.Context, issueID, kind string) ([]string, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.storedIDs(issueID, kind), nil
}

func (s *fingerprintModelStore) seedRows(issueID, kind string, commentIDs ...string) {
	key := ReactionKey(issueID, kind)
	if s.rows[key] == nil {
		s.rows[key] = make(map[string]struct{})
	}
	for _, id := range commentIDs {
		s.rows[key][id] = struct{}{}
	}
}

func (s *fingerprintModelStore) storedIDs(issueID, kind string) []string {
	return slices.Sorted(maps.Keys(s.rows[ReactionKey(issueID, kind)]))
}

func (s *fingerprintModelStore) CountRunHistoryByIssue(context.Context, string) (int, error) {
	return 0, nil
}

func (s *fingerprintModelStore) QueryConsecutiveHandoffAbsenceCounts(_ context.Context, issueIDs []string) (map[string]int, error) {
	return make(map[string]int, len(issueIDs)), nil
}

func (s *fingerprintModelStore) TokenUsageByIssue(context.Context, string) (persistence.IssueTokenUsage, error) {
	return persistence.IssueTokenUsage{}, nil
}

func (s *fingerprintModelStore) UpsertParkedIssue(context.Context, persistence.ParkedIssue) error {
	return nil
}

func (s *fingerprintModelStore) DeleteParkedIssue(context.Context, string) error { return nil }

func (s *fingerprintModelStore) ResetHandoffAbsenceSequence(context.Context, string) error {
	return nil
}

func (s *fingerprintModelStore) UpsertBudgetHoldNotice(context.Context, persistence.BudgetHoldNotice) error {
	return nil
}

func (s *fingerprintModelStore) UpsertAggregateMetrics(context.Context, persistence.AggregateMetrics) error {
	return nil
}

func (s *fingerprintModelStore) UpsertSessionMetadata(context.Context, persistence.SessionMetadata) error {
	return nil
}

func (s *fingerprintModelStore) UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error {
	if err := s.reviewReconcileStore.UpsertReactionFingerprint(ctx, issueID, kind, fingerprint); err != nil {
		return err
	}
	key := ReactionKey(issueID, kind)
	if current := s.fingerprints[key]; current == nil || current.value != fingerprint {
		s.fingerprints[key] = &modelFingerprint{value: fingerprint}
	}
	return nil
}

func (s *fingerprintModelStore) GetReactionFingerprint(ctx context.Context, issueID, kind string) (string, bool, error) {
	if _, _, err := s.reviewReconcileStore.GetReactionFingerprint(ctx, issueID, kind); err != nil {
		return "", false, err
	}
	current := s.fingerprints[ReactionKey(issueID, kind)]
	if current == nil {
		return "", false, nil
	}
	return current.value, current.dispatched, nil
}

func (s *fingerprintModelStore) MarkReactionDispatched(ctx context.Context, issueID, kind string) error {
	if err := s.reviewReconcileStore.MarkReactionDispatched(ctx, issueID, kind); err != nil {
		return err
	}
	if current := s.fingerprints[ReactionKey(issueID, kind)]; current != nil {
		current.dispatched = true
	}
	return nil
}

func (s *fingerprintModelStore) DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error {
	if err := s.reviewReconcileStore.DeleteReactionFingerprint(ctx, issueID, kind); err != nil {
		return err
	}
	delete(s.fingerprints, ReactionKey(issueID, kind))
	return nil
}

func (s *fingerprintModelStore) seedDispatched(issueID, kind string, comments []domain.ReviewComment) {
	s.fingerprints[ReactionKey(issueID, kind)] = &modelFingerprint{value: buildReviewFingerprint(comments), dispatched: true}
}

func summaryComment(id string) domain.ReviewComment {
	return domain.ReviewComment{ID: id, Body: "summary " + id, SubmittedAt: reviewBaseTime.Add(-time.Hour)}
}

func inlineComment(id string) domain.ReviewComment {
	return domain.ReviewComment{
		ID:          id,
		FilePath:    "internal/app/main.go",
		StartLine:   12,
		Body:        "fix " + id,
		SubmittedAt: reviewBaseTime.Add(-time.Hour),
	}
}

func outdatedComment(c domain.ReviewComment) domain.ReviewComment {
	c.Outdated = true
	return c
}

func seedHandedOff(state *State, rkey string, ids ...string) {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	state.ReactionHandedOffComments[rkey] = set
}

func assertHandedOff(t *testing.T, state *State, rkey string, want ...string) {
	t.Helper()
	set, present := state.ReactionHandedOffComments[rkey]
	got := slices.Sorted(maps.Keys(set))
	if len(want) == 0 {
		if present {
			t.Errorf("ReactionHandedOffComments[%q] = %v, want absent", rkey, got)
		}
		return
	}
	wantSorted := slices.Sorted(slices.Values(want))
	if !slices.Equal(got, wantSorted) {
		t.Errorf("ReactionHandedOffComments[%q] = %v, want %v", rkey, got, wantSorted)
	}
}

func assertHandedOffCached(t *testing.T, state *State, rkey string, want ...string) {
	t.Helper()
	set, present := state.ReactionHandedOffComments[rkey]
	if !present {
		t.Errorf("ReactionHandedOffComments[%q] absent, want a loaded set %v", rkey, want)
		return
	}
	got := slices.Sorted(maps.Keys(set))
	if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("ReactionHandedOffComments[%q] = %v, want %v", rkey, got, slices.Sorted(slices.Values(want)))
	}
}

func assertStoredHandedOff(t *testing.T, store *fingerprintModelStore, issueID, kind string, want ...string) {
	t.Helper()
	got := store.storedIDs(issueID, kind)
	if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Errorf("stored handed-off comments for %s = %v, want %v", ReactionKey(issueID, kind), got, slices.Sorted(slices.Values(want)))
	}
}

func assertFingerprintMarked(t *testing.T, store *fingerprintModelStore, issueID, kind string, comments []domain.ReviewComment, wantDispatched bool) {
	t.Helper()
	fp, dispatched, err := store.GetReactionFingerprint(context.Background(), issueID, kind)
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if want := buildReviewFingerprint(comments); fp != want {
		t.Errorf("stored fingerprint for %s = %q, want the fingerprint of %d comments %q", ReactionKey(issueID, kind), fp, len(comments), want)
	}
	if dispatched != wantDispatched {
		t.Errorf("fingerprint dispatched for %s = %v, want %v", ReactionKey(issueID, kind), dispatched, wantDispatched)
	}
}

func continuationIDs(t *testing.T, state *State, issueID, kind string) []string {
	t.Helper()
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatalf("RetryAttempts[%q] missing, want a scheduled continuation", issueID)
	}
	if retry.ReactionKind != kind {
		t.Errorf("RetryAttempts[%q].ReactionKind = %q, want %q", issueID, retry.ReactionKind, kind)
	}
	return slices.Sorted(slices.Values(continuationCommentIDs(kind, retry.ContinuationContext)))
}

func assertEscalations(t *testing.T, tracker *reviewTrackerStub, want int) {
	t.Helper()
	if tracker.addLabelCalled != want {
		t.Errorf("AddLabel calls = %d, want %d", tracker.addLabelCalled, want)
	}
	if tracker.commentIssueCalls != want {
		t.Errorf("routed escalation events = %d, want %d", tracker.commentIssueCalls, want)
	}
}

func makeDue(t *testing.T, state *State, rkey string) *PendingReaction {
	t.Helper()
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want a re-enqueued entry", rkey)
	}
	entry.PendingRetryAt = time.Time{}
	return entry
}

const commentsPromptTemplate = `Resolve {{ .issue.identifier }}.
{{ if .review_comments }}
Review comments:
{{ range .review_comments }}
- {{ .id }}
{{ end }}
{{ end }}
{{ if .bot_review_comments }}
Bot comments:
{{ range .bot_review_comments }}
- {{ .id }}
{{ end }}
{{ end }}`

func presentedByTemplate(t *testing.T, body string, issue domain.Issue, continuation map[string]any, freshRun bool) map[string][]string {
	t.Helper()
	_, presented, err := renderFirstTurnPrompt(mustParseTemplate(t, body), issue.ToTemplateMap(), 0, 3, continuation, freshRun)
	if err != nil {
		t.Fatalf("renderFirstTurnPrompt: %v", err)
	}
	return presented
}

func workerExitParams(store WorkerExitStore) HandleWorkerExitParams {
	return HandleWorkerExitParams{
		Store:             store,
		MaxRetryBackoffMS: 300_000,
		ActiveStates:      []string{"To Do", "In Progress"},
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return reviewBaseTime },
		Logger:            discardLogger(),
	}
}

func dispatchRetry(t *testing.T, state *State, store RetryTimerStore, issueID string) *RunningEntry {
	t.Helper()
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatalf("RetryAttempts[%q] missing, want a scheduled continuation", issueID)
	}
	retry.scheduledAt = time.Time{}
	tracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, retry.Identifier, "In Progress")}
	params := defaultRetryParams(t, &mockRetryStore{}, tracker)
	params.Store = store

	HandleRetryTimer(state, issueID, params)
	t.Cleanup(state.WorkerWg.Wait)

	running, ok := state.Running[issueID]
	if !ok {
		t.Fatalf("Running[%q] missing after the retry timer fired, want a dispatched run", issueID)
	}
	return running
}

func completeContinuation(t *testing.T, state *State, store *fingerprintModelStore, issueID, kind string, reseeded *PendingReaction) {
	t.Helper()
	running := dispatchRetry(t, state, store, issueID)
	presented := presentedByTemplate(t, commentsPromptTemplate, running.Issue, running.ContinuationContext, running.ReactionKind == "")

	HandleWorkerExit(state, WorkerResult{
		IssueID:           issueID,
		Identifier:        running.Identifier,
		ExitKind:          WorkerExitNormal,
		HandedOffComments: presented,
	}, workerExitParams(store))

	CancelRetry(state, issueID)
	state.PendingReactions[ReactionKey(issueID, kind)] = reseeded
}

func reviewEscalationParams(t *testing.T, store ReconcileStore, scm domain.SCMAdapter, tracker *reviewTrackerStub) ReconcileParams {
	t.Helper()
	params := reviewParams(&reviewReconcileStore{}, scm, tracker)
	params.Store = store
	params.Router = commentRouter(t, tracker, domain.EventEscalationReviewComments)
	params.BotReviewConfig.BotUsernames = []string{"review-bot"}
	return params
}

func spentReviewState(t *testing.T, issueID string, handedOff ...string) (*State, string) {
	t.Helper()
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[rkey] = defaultReviewConfig().MaxContinuationTurns
	if len(handedOff) > 0 {
		seedHandedOff(state, rkey, handedOff...)
	}
	return state, rkey
}

func TestReconcileReviewComments_NilAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-1", 42)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	params := reviewParams(store, nil, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("ISS-R-1", ReactionKindReview)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed with nil SCMAdapter; want no-op")
	}
	if len(metrics.reviewChecks) != 0 {
		t.Errorf("IncReviewChecks called with nil adapter; want no calls")
	}
}

func TestReconcileReviewComments_NoPendingReviewEntries(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, 0, nil, AgentTotals{})
	// Add a CI reaction entry, should not be processed by review reconcile.
	rkey := ReactionKey("ISS-R-CI", ReactionKindCI)
	state.PendingReactions[rkey] = &PendingReaction{
		Kind:      ReactionKindCI,
		IssueID:   "ISS-R-CI",
		CreatedAt: reviewBaseTime,
		KindData:  &CIReactionData{Branch: "main"},
	}

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (no review entries)", scm.calls)
	}
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("CI PendingReactions entry removed by review reconcile; want untouched")
	}
}

func TestReconcileReviewComments_PollThrottle(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-2", 10)
	rkey := ReactionKey("ISS-R-2", ReactionKindReview)
	// Set PendingRetryAt to 1 minute in the future relative to NowFunc.
	state.PendingReactions[rkey].PendingRetryAt = reviewBaseTime.Add(1 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on poll throttle; want re-enqueued")
	}
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (throttled)", scm.calls)
	}
}

func TestReconcileReviewComments_TTLExpired(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-3", 10)
	rkey := ReactionKey("ISS-R-3", ReactionKindReview)
	// Set CreatedAt 31 minutes before NowFunc.
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry retained after TTL expiry; want dropped")
	}
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (TTL exceeded)", scm.calls)
	}
}

func TestReconcileReviewComments_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "REV-AGE-1"
	state := stateWithReviewReaction(t, issueID, 10)
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[reviewKey] = defaultReviewConfig().MaxContinuationTurns - 1
	seedHandedOff(state, reviewKey, "rc-1", "rc-2")
	state.PendingReactions[reviewKey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.ReactionAttempts[ciKey] = 7
	delete(state.Claimed, issueID)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[reviewKey]; ok {
		t.Error("PendingReactions[review] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[reviewKey]; ok {
		t.Error("ReactionAttempts[review] present after drop-on-age below the budget; want removed")
	}
	assertHandedOff(t, state, reviewKey)
	if state.ReactionAttempts[ciKey] != 7 {
		t.Errorf("ReactionAttempts[ci] = %d, want 7 (untouched)", state.ReactionAttempts[ciKey])
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
		t.Errorf("FetchPendingReviews calls = %d, want 0 (TTL exceeded before fetch)", scm.calls)
	}
}

// TestReconcileReviewComments_WatchWindowZeroNeverDrops verifies that a
// ReviewPendingTTL of 0 (watch_window_ms: 0) never drops an entry on age,
// however old it is.
func TestReconcileReviewComments_WatchWindowZeroNeverDrops(t *testing.T) {
	t.Parallel()

	rkey := ReactionKey("ISS-R-WW0", ReactionKindReview)
	state := stateWithReviewReaction(t, "ISS-R-WW0", 10)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-365 * 24 * time.Hour)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 0

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with ReviewPendingTTL=0; want never dropped on age")
	}
}

// TestReconcileReviewComments_WatchWindowNonDefaultTakesEffect verifies that
// a configured window other than the default threshold actually gates the
// drop, not a hardcoded default: an entry older than the configured window
// is dropped with its attempt counter released, and one younger survives.
func TestReconcileReviewComments_WatchWindowNonDefaultTakesEffect(t *testing.T) {
	t.Parallel()

	t.Run("older than configured window is dropped and counter released", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("ISS-R-WWN-OLD", ReactionKindReview)
		state := stateWithReviewReaction(t, "ISS-R-WWN-OLD", 10)
		state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-6 * time.Minute)
		state.ReactionAttempts[rkey] = 2

		store := &reviewReconcileStore{}
		metrics := newReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := reviewParams(store, scm, nil)
		params.ReviewPendingTTL = 5 * time.Minute

		reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry present past configured 5m window; want dropped")
		}
		if _, ok := state.ReactionAttempts[rkey]; ok {
			t.Error("ReactionAttempts present past configured 5m window; want released")
		}
	})

	t.Run("younger than configured window survives", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("ISS-R-WWN-NEW", ReactionKindReview)
		state := stateWithReviewReaction(t, "ISS-R-WWN-NEW", 10)
		state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-4 * time.Minute)

		store := &reviewReconcileStore{}
		metrics := newReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := reviewParams(store, scm, nil)
		params.ReviewPendingTTL = 5 * time.Minute

		reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Error("PendingReactions entry dropped inside configured 5m window; want kept")
		}
	})
}

// TestReconcileReviewComments_WatchWindowElapsedLogsRenamedAttribute pins the
// drop-on-age log record: message text and the window_ms attribute name
// (renamed from ttl_ms). A regression that reverts the rename or the wording
// must fail this test.
func TestReconcileReviewComments_WatchWindowElapsedLogsRenamedAttribute(t *testing.T) {
	t.Parallel()

	rkey := ReactionKey("ISS-R-WWLOG", ReactionKindReview)
	state := stateWithReviewReaction(t, "ISS-R-WWLOG", 10)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute
	log, buf := logCapture()

	reconcileReviewComments(state, params, log, context.Background(), metrics)

	output := buf.String()
	const msg = "review watch window elapsed, dropping"
	assertLogLineHasIntAttr(t, output, msg, "window_ms", int(30*time.Minute/time.Millisecond))
	if strings.Contains(output, "ttl_ms") {
		t.Errorf("log output contains stale attribute %q: %s", "ttl_ms", output)
	}
	if strings.Contains(output, "exceeded ttl") {
		t.Errorf("log output contains stale message wording %q: %s", "exceeded ttl", output)
	}
}

func TestReconcileReviewComments_SCMFetchError_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-4", 10)
	rkey := ReactionKey("ISS-R-4", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{err: errors.New("connection timeout")}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on SCM fetch error; want re-enqueued")
	}
	// PendingAttempts should be incremented.
	entry := state.PendingReactions[rkey]
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 after first error", entry.PendingAttempts)
	}
	if !entry.PendingRetryAt.After(reviewBaseTime) {
		t.Error("PendingRetryAt not in future after SCM error; want backoff applied")
	}
	if metrics.reviewChecks["error"] != 1 {
		t.Errorf(`IncReviewChecks("error") = %d, want 1`, metrics.reviewChecks["error"])
	}
}

func TestReconcileReviewComments_NoActionableComments(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-5", 10)
	rkey := ReactionKey("ISS-R-5", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	// Empty slice, no actionable comments.
	scm := &mockSCMAdapter{comments: []domain.ReviewComment{}}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with no actionable comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-5"]; ok {
		t.Error("retry scheduled with no actionable comments; want none")
	}
}

func TestReconcileReviewComments_AllCommentsOutdated(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-6", 10)
	rkey := ReactionKey("ISS-R-6", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{
		comments: []domain.ReviewComment{
			{ID: "1", Outdated: true, SubmittedAt: reviewBaseTime.Add(-1 * time.Hour)},
			{ID: "2", Outdated: true, SubmittedAt: reviewBaseTime.Add(-2 * time.Hour)},
		},
	}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with all outdated comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-6"]; ok {
		t.Error("retry scheduled with all outdated comments; want none")
	}
}

func TestReconcileReviewComments_FingerprintMatchDispatched(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-7", 10)
	rkey := ReactionKey("ISS-R-7", ReactionKindReview)

	comments := []domain.ReviewComment{
		{ID: "100", Body: "fix this", SubmittedAt: reviewBaseTime.Add(-2 * time.Minute)},
	}
	fp := buildReviewFingerprint(comments)

	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Already dispatched -> re-enqueue but do not call MarkReactionDispatched.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped for already-dispatched fingerprint; want re-enqueued")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (already dispatched)", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-R-7"]; ok {
		t.Error("retry scheduled for already-dispatched fingerprint; want none")
	}
}

func TestReconcileReviewComments_NewFingerprint_Dispatches(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-8", 10)
	rkey := ReactionKey("ISS-R-8", ReactionKindReview)
	state.PendingReactions[rkey].RuleSettingsApplied = true

	// Comment submitted 5 minutes ago, outside the 30s debounce window (defaultReviewConfig).
	comments := []domain.ReviewComment{
		{ID: "200", Body: "needs fix", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Entry consumed (not re-enqueued as pending-check).
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch; want consumed")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-R-8"]; !ok {
		t.Fatal("retry not scheduled after review dispatch; want scheduled")
	}
	retry := state.RetryAttempts["ISS-R-8"]
	if !retry.RuleSettingsApplied {
		t.Error("RetryEntry.RuleSettingsApplied = false, want the pending reaction's flag carried")
	}
	if retry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want review_comments map")
	}
	if retry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindReview)
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
}

func TestReconcileReviewComments_DebounceWindowActive(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-9", 10)
	rkey := ReactionKey("ISS-R-9", ReactionKindReview)

	// Comment submitted 10 seconds ago; debounce window is 30s (defaultReviewConfig).
	recentTime := reviewBaseTime.Add(-10 * time.Second)
	comments := []domain.ReviewComment{
		{ID: "300", Body: "new comment", SubmittedAt: recentTime},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Debounced: re-enqueued, no retry scheduled.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped during debounce; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-9"]; ok {
		t.Error("retry scheduled during debounce window; want none")
	}
	// PendingRetryAt should be set to LastEventAt + debounceMS.
	entry := state.PendingReactions[rkey]
	expectedRetryAt := recentTime.Add(time.Duration(defaultReviewConfig().DebounceMS) * time.Millisecond)
	if !entry.PendingRetryAt.Equal(expectedRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, expectedRetryAt)
	}
}

func TestReconcileReviewComments_DebounceElapsed_Dispatches(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-10", 10)
	rkey := ReactionKey("ISS-R-10", ReactionKindReview)

	// Comment submitted 120 seconds ago; debounce window is 30s -> elapsed.
	comments := []domain.ReviewComment{
		{ID: "400", Body: "old enough", SubmittedAt: reviewBaseTime.Add(-120 * time.Second)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Dispatch should have happened.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after debounce elapsed; want consumed")
	}
	if _, ok := state.RetryAttempts["ISS-R-10"]; !ok {
		t.Error("retry not scheduled after debounce elapsed; want scheduled")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
	}
}

func TestReconcileReviewComments_TurnCapExceeded_Escalates(t *testing.T) {
	t.Parallel()

	state, rkey := spentReviewState(t, "ISS-R-11", "rc-old")

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: []domain.ReviewComment{summaryComment("rc-old"), summaryComment("rc-new")}}
	params := reviewEscalationParams(t, store, scm, tracker)
	log, buf := logCapture()

	reconcileReviewComments(state, params, log, context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after turn cap; want consumed")
	}
	if _, ok := state.Claimed["ISS-R-11"]; ok {
		t.Error("claim not released after turn cap escalation; want released")
	}
	if len(store.deletedIssueIDs) != 1 || store.deletedIssueIDs[0] != "ISS-R-11" {
		t.Errorf("DeleteRetryEntry calls = %v, want [ISS-R-11]", store.deletedIssueIDs)
	}
	if _, ok := state.RetryAttempts["ISS-R-11"]; ok {
		t.Error("retry still scheduled after escalation; want none")
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[%s] present after escalation; want deleted", rkey)
	}
	assertHandedOffCached(t, state, rkey, "rc-old")
	if scm.calls != 1 {
		t.Errorf("FetchPendingReviews calls = %d, want 1 (the budget is decided after the fetch)", scm.calls)
	}
	assertEscalations(t, tracker, 1)
	assertLogLineHasIntAttr(t, buf.String(), "review fix continuation turns exhausted, escalating", "turn_count", 3)
	assertLogLacksLine(t, buf.String(), "review triage requested escalation")
}

func TestReconcileReviewComments_TurnCapExceeded_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	issueID := "ISS-R-ISO"
	state, rkey := spentReviewState(t, issueID)
	mcKey := ReactionKey(issueID, ReactionKindMergeCompletion)
	state.PendingReactions[mcKey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindMergeCompletion,
		CreatedAt:  reviewBaseTime,
		KindData:   &MergeCompletionReactionData{PRNumber: 42, Owner: "owner", Repo: "repo"},
	}
	state.ReactionAttempts[mcKey] = 1
	seedHandedOff(state, ReactionKey(issueID, ReactionKindBotReview), "bc-1")

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: []domain.ReviewComment{summaryComment("rc-new")}}
	params := reviewEscalationParams(t, store, scm, tracker)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("review PendingReactions entry present after escalation; want consumed")
	}
	if _, ok := state.PendingReactions[mcKey]; !ok {
		t.Error("sibling merge-completion PendingReactions entry removed by review escalation; want untouched")
	}
	if state.ReactionAttempts[mcKey] != 1 {
		t.Error("sibling merge-completion ReactionAttempts counter altered by review escalation; want untouched")
	}
	assertHandedOff(t, state, ReactionKey(issueID, ReactionKindBotReview), "bc-1")
	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1 (the review kind's own fingerprint)", store.deleteFingerprintCalls)
	}
	if scm.calls != 1 {
		t.Errorf("FetchPendingReviews calls = %d, want 1", scm.calls)
	}
	assertEscalations(t, tracker, 1)
}

// TestReconcileReviewComments_BotAllowlistExclusion verifies that
// reconcileReviewComments drops a fetched comment whose author matches
// params.BotReviewConfig.BotUsernames before the fingerprint and the
// dispatch decision, leaving comments from non-allowlisted authors and
// comments with no known author untouched.
func TestReconcileReviewComments_BotAllowlistExclusion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		issueID           string
		comments          []domain.ReviewComment
		allowlist         []string
		wantDispatch      bool
		wantUpsertCalls   int
		wantSurvivingIDs  []string
		wantExcludedCount int
	}{
		{
			name:    "single allowlisted author excluded, no dispatch",
			issueID: "ISS-R-BOT-1",
			comments: []domain.ReviewComment{
				{ID: "1", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      false,
			wantUpsertCalls:   0,
			wantExcludedCount: 1,
		},
		{
			name:    "single non-allowlisted author dispatches as today",
			issueID: "ISS-R-BOT-2",
			comments: []domain.ReviewComment{
				{ID: "2", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"2"},
			wantExcludedCount: 0,
		},
		{
			name:    "mixed set dispatches only surviving comments",
			issueID: "ISS-R-BOT-3",
			comments: []domain.ReviewComment{
				{ID: "3", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
				{ID: "4", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"4"},
			wantExcludedCount: 1,
		},
		{
			name:    "case-differing allowlist entry still excludes",
			issueID: "ISS-R-BOT-4",
			comments: []domain.ReviewComment{
				{ID: "5", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"Houndci-Bot"},
			wantDispatch:      false,
			wantUpsertCalls:   0,
			wantExcludedCount: 1,
		},
		{
			name:    "nil allowlist excludes nothing",
			issueID: "ISS-R-BOT-5",
			comments: []domain.ReviewComment{
				{ID: "6", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         nil,
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"6"},
			wantExcludedCount: 0,
		},
		{
			name:    "empty reviewer not excluded even with empty-string allowlist entry",
			issueID: "ISS-R-BOT-6",
			comments: []domain.ReviewComment{
				{ID: "7", Reviewer: "", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{""},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"7"},
			wantExcludedCount: 0,
		},
		{
			name:    "outdated-and-allowlisted comment does not raise excluded_count",
			issueID: "ISS-R-BOT-7",
			comments: []domain.ReviewComment{
				{ID: "8", Reviewer: "houndci-bot", Outdated: true, SubmittedAt: reviewBaseTime.Add(-1 * time.Hour)},
				{ID: "9", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
				{ID: "10", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"10"},
			wantExcludedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithReviewReaction(t, tt.issueID, 10)
			rkey := ReactionKey(tt.issueID, ReactionKindReview)

			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewParams(store, scm, nil)
			params.BotReviewConfig.BotUsernames = tt.allowlist

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			reconcileReviewComments(state, params, logger, context.Background(), metrics)

			_, rePending := state.PendingReactions[rkey]
			if tt.wantDispatch && rePending {
				t.Error("PendingReactions entry re-enqueued; want consumed by dispatch")
			}
			if !tt.wantDispatch && !rePending {
				t.Error("PendingReactions entry consumed; want re-enqueued (all comments excluded)")
			}

			if store.upsertFingerprintCalls != tt.wantUpsertCalls {
				t.Errorf("UpsertReactionFingerprint calls = %d, want %d", store.upsertFingerprintCalls, tt.wantUpsertCalls)
			}

			wantDispatchCount := 0
			if tt.wantDispatch {
				wantDispatchCount = 1
			}
			if metrics.reviewChecks["dispatched"] != wantDispatchCount {
				t.Errorf(`IncReviewChecks("dispatched") = %d, want %d`, metrics.reviewChecks["dispatched"], wantDispatchCount)
			}

			logOutput := buf.String()
			if tt.wantExcludedCount > 0 {
				assertLogLineHasIntAttr(t, logOutput, reviewExclusionDebugMsg, "excluded_count", tt.wantExcludedCount)
			} else {
				assertLogLacksLine(t, logOutput, reviewExclusionDebugMsg)
			}

			if !tt.wantDispatch {
				return
			}

			retry, ok := state.RetryAttempts[tt.issueID]
			if !ok {
				t.Fatal("retry not scheduled after dispatch; want scheduled")
			}
			reviewCtx, ok := retry.ContinuationContext["review_comments"].([]map[string]any)
			if !ok {
				t.Fatalf("ContinuationContext[review_comments] = %#v, want []map[string]any", retry.ContinuationContext["review_comments"])
			}
			gotIDs := make([]string, len(reviewCtx))
			for i, m := range reviewCtx {
				gotIDs[i], _ = m["id"].(string)
			}
			if !slices.Equal(gotIDs, tt.wantSurvivingIDs) {
				t.Errorf("surviving comment ids = %v, want %v", gotIDs, tt.wantSurvivingIDs)
			}

			assertLogLineHasIntAttr(t, logOutput, reviewDispatchInfoMsg, "excluded_count", tt.wantExcludedCount)
		})
	}
}

func TestBuildReviewFingerprint_EmptyInput(t *testing.T) {
	t.Parallel()

	got := buildReviewFingerprint(nil)
	if got != "" {
		t.Errorf("buildReviewFingerprint(nil) = %q, want empty", got)
	}
	got = buildReviewFingerprint([]domain.ReviewComment{})
	if got != "" {
		t.Errorf("buildReviewFingerprint([]) = %q, want empty", got)
	}
}

func TestBuildReviewFingerprint_OrderIndependent(t *testing.T) {
	t.Parallel()

	commentsABC := []domain.ReviewComment{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	commentsCBA := []domain.ReviewComment{
		{ID: "c"}, {ID: "b"}, {ID: "a"},
	}

	fp1 := buildReviewFingerprint(commentsABC)
	fp2 := buildReviewFingerprint(commentsCBA)

	if fp1 == "" {
		t.Fatal("buildReviewFingerprint returned empty for non-empty input")
	}
	if fp1 != fp2 {
		t.Errorf("buildReviewFingerprint: different order produced different hashes:\n  abc: %q\n  cba: %q", fp1, fp2)
	}
}

func TestBuildReviewFingerprint_DifferentIDsProduceDifferentHash(t *testing.T) {
	t.Parallel()

	comments1 := []domain.ReviewComment{{ID: "aaa"}}
	comments2 := []domain.ReviewComment{{ID: "bbb"}}

	fp1 := buildReviewFingerprint(comments1)
	fp2 := buildReviewFingerprint(comments2)

	if fp1 == fp2 {
		t.Errorf("buildReviewFingerprint: different IDs produced same hash %q", fp1)
	}
}

func TestBuildReviewFingerprint_Deterministic(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{{ID: "x"}, {ID: "y"}}
	fp1 := buildReviewFingerprint(comments)
	fp2 := buildReviewFingerprint(comments)
	if fp1 != fp2 {
		t.Errorf("buildReviewFingerprint not deterministic: %q != %q", fp1, fp2)
	}
}

func TestBuildReviewTemplateMap_FieldMapping(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{
			ID:        "500",
			FilePath:  "main.go",
			StartLine: 10,
			EndLine:   15,
			Reviewer:  "alice",
			Body:      "Please refactor this.",
		},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 1 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 1", len(result))
	}

	m := result[0]
	wantFields := map[string]any{
		"id":         "500",
		"file":       "main.go",
		"start_line": 10,
		"end_line":   15,
		"reviewer":   "alice",
		"body":       "Please refactor this.",
	}
	for k, want := range wantFields {
		got, ok := m[k]
		if !ok {
			t.Errorf("buildReviewTemplateMap: key %q missing from result", k)
			continue
		}
		if got != want {
			t.Errorf("buildReviewTemplateMap[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestBuildReviewTemplateMap_ZeroLines(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{ID: "600", FilePath: "", StartLine: 0, EndLine: 0, Reviewer: "bob", Body: "PR comment"},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 1 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 1", len(result))
	}

	m := result[0]
	if m["start_line"] != 0 {
		t.Errorf("start_line = %v, want 0", m["start_line"])
	}
	if m["end_line"] != 0 {
		t.Errorf("end_line = %v, want 0", m["end_line"])
	}
	if m["file"] != "" {
		t.Errorf("file = %v, want empty string", m["file"])
	}
}

func TestBuildReviewTemplateMap_MultipleComments(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{ID: "1", Body: "first"},
		{ID: "2", Body: "second"},
		{ID: "3", Body: "third"},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 3 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 3", len(result))
	}
	for i, m := range result {
		if m["id"] != comments[i].ID {
			t.Errorf("result[%d][id] = %v, want %q", i, m["id"], comments[i].ID)
		}
	}
}

func TestBuildReviewReactionConfig_Defaults(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		MaxRetries:      2,
		Escalation:      "label",
		EscalationLabel: "needs-human",
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error: %v", err)
	}
	if got.PollIntervalMS != 120000 {
		t.Errorf("PollIntervalMS = %d, want 120000 (default)", got.PollIntervalMS)
	}
	if got.DebounceMS != 60000 {
		t.Errorf("DebounceMS = %d, want 60000 (default)", got.DebounceMS)
	}
	if got.MaxContinuationTurns != 3 {
		t.Errorf("MaxContinuationTurns = %d, want 3 (default)", got.MaxContinuationTurns)
	}
	if got.Escalation != "label" {
		t.Errorf("Escalation = %q, want %q", got.Escalation, "label")
	}
	if got.EscalationLabel != "needs-human" {
		t.Errorf("EscalationLabel = %q, want %q", got.EscalationLabel, "needs-human")
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSDefault(t *testing.T) {
	t.Parallel()

	got, err := BuildReviewReactionConfig(config.ReactionConfig{})
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.WatchWindowMS != reactionWatchWindowDefaultMS {
		t.Errorf("WatchWindowMS = %d, want %d (default)", got.WatchWindowMS, reactionWatchWindowDefaultMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSOverride(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 600000}}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.WatchWindowMS != 600000 {
		t.Errorf("WatchWindowMS = %d, want 600000", got.WatchWindowMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSZeroDisablesBound(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 0}}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error for watch_window_ms=0: %v", err)
	}
	if got.WatchWindowMS != 0 {
		t.Errorf("WatchWindowMS = %d, want 0", got.WatchWindowMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSNegative(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": -1}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for negative watch_window_ms, got nil")
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSAboveCeiling(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for watch_window_ms above ceiling, got nil")
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSNonNumeric(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": "soon"}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for non-numeric watch_window_ms, got nil")
	}
}

func TestBuildReviewReactionConfig_EscalationDefault(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{MaxRetries: 2}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.Escalation != "label" {
		t.Errorf("Escalation = %q, want %q (default)", got.Escalation, "label")
	}
	if got.EscalationLabel != "needs-human" {
		t.Errorf("EscalationLabel = %q, want %q (default)", got.EscalationLabel, "needs-human")
	}
}

func TestBuildReviewReactionConfig_ExtraFields(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		MaxRetries: 1,
		Escalation: "comment",
		Extra: map[string]any{
			"poll_interval_ms":       60000,
			"debounce_ms":            10000,
			"max_continuation_turns": 5,
		},
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.PollIntervalMS != 60000 {
		t.Errorf("PollIntervalMS = %d, want 60000", got.PollIntervalMS)
	}
	if got.DebounceMS != 10000 {
		t.Errorf("DebounceMS = %d, want 10000", got.DebounceMS)
	}
	if got.MaxContinuationTurns != 5 {
		t.Errorf("MaxContinuationTurns = %d, want 5", got.MaxContinuationTurns)
	}
}

func TestBuildReviewReactionConfig_PollIntervalBelowMinimum(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"poll_interval_ms": 10000, // below minimum 30000
		},
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for poll_interval_ms < 30000, got nil")
	}
}

func TestBuildReviewReactionConfig_DebounceZeroIsValid(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"debounce_ms": 0,
		},
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error for debounce_ms=0: %v", err)
	}
	if got.DebounceMS != 0 {
		t.Errorf("DebounceMS = %d, want 0", got.DebounceMS)
	}
}

func TestBuildReviewReactionConfig_MaxContinuationTurnsZero(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"max_continuation_turns": 0,
		},
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for max_continuation_turns=0, got nil")
	}
}

func TestBuildReviewReactionConfig_InvalidEscalation(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "webhook",
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for invalid escalation, got nil")
	}
}

// TestReconcileReviewComments_ZeroPollInterval_NoActionableComments verifies
// that a zero or negative PollIntervalMS falls back to reviewPendingBackoffBase
// when re-enqueuing after receiving no actionable review comments.
func TestReconcileReviewComments_ZeroPollInterval_NoActionableComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		pollIntervalMS int
	}{
		{"zero poll interval", 0},
		{"negative poll interval", -1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithReviewReaction(t, "ISS-R-ZP-NA", 10)
			rkey := ReactionKey("ISS-R-ZP-NA", ReactionKindReview)
			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			scm := &mockSCMAdapter{comments: []domain.ReviewComment{}}
			params := reviewParams(store, scm, nil)
			params.ReviewConfig.PollIntervalMS = tt.pollIntervalMS

			reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

			entry, ok := state.PendingReactions[rkey]
			if !ok {
				t.Fatal("PendingReactions entry dropped with no actionable comments; want re-enqueued")
			}
			want := reviewBaseTime.Add(reviewPendingBackoffBase)
			if !entry.PendingRetryAt.Equal(want) {
				t.Errorf("reconcileReviewComments(PollIntervalMS=%d) PendingRetryAt = %v, want %v (reviewPendingBackoffBase fallback)",
					tt.pollIntervalMS, entry.PendingRetryAt, want)
			}
		})
	}
}

// TestReconcileReviewComments_ZeroPollInterval_AlreadyDispatched verifies
// that a zero PollIntervalMS falls back to reviewPendingBackoffBase when
// re-enqueuing after a fingerprint match with dispatched=true.
func TestReconcileReviewComments_ZeroPollInterval_AlreadyDispatched(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-ZP-D", 10)
	rkey := ReactionKey("ISS-R-ZP-D", ReactionKindReview)

	comments := []domain.ReviewComment{
		{ID: "700", Body: "fix me", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	fp := buildReviewFingerprint(comments)

	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)
	params.ReviewConfig.PollIntervalMS = 0

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped for already-dispatched fingerprint; want re-enqueued")
	}
	want := reviewBaseTime.Add(reviewPendingBackoffBase)
	if !entry.PendingRetryAt.Equal(want) {
		t.Errorf("reconcileReviewComments(PollIntervalMS=0, dispatched=true) PendingRetryAt = %v, want %v (reviewPendingBackoffBase fallback)",
			entry.PendingRetryAt, want)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (already dispatched)", store.markDispatchedCalls)
	}
}

func TestComputeReviewPendingDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{"zero attempts returns 0", 0, 0, 0},
		{"negative attempts returns 0", -1, 0, 0},
		{"attempt 1 returns base*2", 1, reviewPendingBackoffBase * 2, reviewPendingBackoffBase * 3},
		{"very large attempt capped at max", 100, reviewPendingBackoffCap, reviewPendingBackoffCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeReactionPendingDelay(tt.attempts)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("computeReactionPendingDelay(%d) = %v, want in [%v, %v]",
					tt.attempts, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// TestReconcileReviewComments_ForeignIncumbentDefers verifies that a
// foreign incumbent occupying the retry slot leaves the review pending
// entry re-enqueued rather than dispatched, and that
// state.ReactionAttempts for the review kind is left unchanged.
func TestReconcileReviewComments_ForeignIncumbentDefers(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-DEFER"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      1,
		ReactionKind: ReactionKindLabelReview,
	}

	// Comment submitted 5 minutes ago, outside the debounce window.
	comments := []domain.ReviewComment{
		{ID: "900", Body: "needs fix", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on a defer; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(reviewBaseTime) {
		t.Errorf("CreatedAt = %v, want refreshed to %v", entry.CreatedAt, reviewBaseTime)
	}
	incumbent := state.RetryAttempts[issueID]
	if incumbent.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q (incumbent unchanged)", incumbent.ReactionKind, ReactionKindLabelReview)
	}
	if incumbent.Attempt != 1 {
		t.Errorf("RetryAttempts.Attempt = %d, want 1 (unchanged)", incumbent.Attempt)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[%s] = %d, want absent (a defer must not increment it)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 0 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 0 (no dispatch on a defer)`, metrics.reviewChecks["dispatched"])
	}
}

// reviewTriageParams returns reviewParams wired with a real workspace
// and the given triage script, so reactionTriageGate actually starts a
// subprocess for the pass's actionable comment set.
func reviewTriageParams(t *testing.T, store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter, workspaceRoot, script string) ReconcileParams {
	t.Helper()
	params := reviewParams(store, scm, tracker)
	params.WorkspaceRoot = workspaceRoot
	params.ReviewConfig.Triage = config.ReactionTriageConfig{Script: writeHookScript(t, script), TimeoutMS: 5000}
	return params
}

// oldEnoughReviewComments returns one actionable comment submitted well
// outside the default debounce window.
func oldEnoughReviewComments() []domain.ReviewComment {
	return []domain.ReviewComment{
		{ID: "rc-1", Body: "fix this", SubmittedAt: reviewBaseTime.Add(-time.Hour)},
	}
}

// runReviewTriageToCompletion drives a pass that starts a triage run for
// issueID, waits for the subprocess to finish, then resets the entry's
// PendingRetryAt to the past so the next pass is immediately due.
func runReviewTriageToCompletion(t *testing.T, state *State, params ReconcileParams, rkey string, metrics domain.Metrics) {
	t.Helper()
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}
}

// TestReconcileReviewComments_Triage_NoConfig_BehavesAsPinned verifies
// that a review reaction with no triage block dispatches exactly as the
// pinned revision.
func TestReconcileReviewComments_Triage_NoConfig_BehavesAsPinned(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-OFF"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewParams(store, scm, nil)
	// WorkspaceRoot and ReviewConfig.Triage are left at their zero values.

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a scheduled continuation, want dropped (pinned behavior)")
	}
	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_WaitsWithoutProviderCall verifies
// that while a triage run is in flight, the pass re-enqueues without
// making a provider call and without incrementing PendingAttempts.
func TestReconcileReviewComments_Triage_WaitsWithoutProviderCall(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-WAIT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-wait", func() {})

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, nil, root, handledScript)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 while a triage run is in flight", scm.calls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped while waiting on triage, want re-enqueued")
	}
	if entry.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 (waiting is not a fetch error)", entry.PendingAttempts)
	}
}

// TestReconcileReviewComments_Triage_Handled verifies that a handled
// disposition marks the fingerprint dispatched and re-enqueues the
// entry with the poll interval, without incrementing ReactionAttempts
// or IncReviewChecks("dispatched").
func TestReconcileReviewComments_Triage_Handled(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-HANDLED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, nil, root, handledScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if !entry.PendingRetryAt.After(reviewBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v (re-enqueued with the poll interval)", entry.PendingRetryAt, reviewBaseTime)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a handled verdict must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 0 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 0 on a handled pass`, metrics.reviewChecks["dispatched"])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_Escalate verifies that an escalate
// disposition invokes escalateReviewFailure with EscalationTriggerTriage
// and the un-incremented turn count.
func TestReconcileReviewComments_Triage_Escalate(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-ESCALATE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, tracker, root, escalateTriageScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (triage escalation uses the kind's own escalation action)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (matches a budget escalation)")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_DispatchAgent_ProceedsNormally
// verifies that a dispatch-agent disposition falls through to the
// existing dispatch block, incrementing IncReviewChecks("dispatched").
func TestReconcileReviewComments_Triage_DispatchAgent_ProceedsNormally(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-DISPATCH"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, nil, root, dispatchAgentTriageScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 for dispatch-agent", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_CancelOnTTLDrop verifies that an
// in-flight triage run is cancelled before the entry is dropped on TTL
// elapse.
func TestReconcileReviewComments_Triage_CancelOnTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-DROP"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	spy := &triageCancelSpy{}
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-drop", spy.cancel)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if spy.calls() != 1 {
		t.Errorf("Cancel called %d times, want 1 (the in-flight run must not outlive the dropped entry)", spy.calls())
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL, want dropped")
	}
}

// TestReconcileReviewComments_Triage_RepeatedHandled_StillAgesOut pins
// the bound review carries and ci does not: the TTL is measured from
// the entry's creation, so a succession of handled answers still ages
// the entry out once that TTL elapses.
func TestReconcileReviewComments_Triage_RepeatedHandled_StillAgesOut(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-AGESOUT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, nil, root, handledScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics) // applies: handled

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry dropped right after a handled verdict, want retained until TTL")
	}

	// Age the entry's creation time past the TTL and confirm it still
	// drops despite the retained handled outcome.
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
	state.PendingReactions[rkey].PendingRetryAt = time.Time{}
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL despite a handled verdict, want dropped")
	}
}

// TestReconcileReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode
// pins cancelReactionTriage's detach-on-close contract: a memoized
// handled verdict from one episode must not survive into the next.
// Without it, a later episode that recomputes the identical
// actionable-comment fingerprint (the same comment reappearing after
// the thread went quiet) would replay the stale handled outcome from
// memory instead of running the newly configured command, silently
// suppressing the real verdict.
func TestReconcileReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-EPISODE-CLOSE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, store, scm, tracker, root, handledScript)

	// Episode 1: one actionable comment; the command answers handled,
	// memoizing the verdict on pending.Triage rather than re-running it
	// on the next pass over the same comment set.
	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if entry.Triage == nil {
		t.Fatal("PendingReaction.Triage cleared inside its own episode, want the memoized handle retained")
	}
	entry.PendingRetryAt = time.Time{}

	// The episode closes: the comment set goes empty, taking the
	// no-actionable-comments branch that must cancel the retained
	// handle before re-enqueueing.
	scm.comments = nil

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok = state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped when the episode closed, want re-enqueued")
	}
	if entry.Triage != nil {
		t.Fatal("PendingReaction.Triage survived the episode close, want detached so a later identical comment set cannot replay it")
	}
	entry.PendingRetryAt = time.Time{}

	// Episode 2: the same comment reappears, recomputing the identical
	// fingerprint. The command now answers escalate; a cleared handle
	// must run it fresh rather than replay episode 1's memoized handled
	// verdict.
	scm.comments = oldEnoughReviewComments()
	params.ReviewConfig.Triage = config.ReactionTriageConfig{Script: writeHookScript(t, escalateTriageScript), TimeoutMS: 5000}

	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (the replayed handled verdict from episode 1 must not suppress the fresh escalate)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (the replayed handled verdict from episode 1 must not suppress it)")
	}
	if store.markDispatchedCalls != 2 {
		t.Errorf("MarkReactionDispatched calls = %d, want 2 (one real verdict per episode: handled, then escalate)", store.markDispatchedCalls)
	}
}

func TestRecordHandedOffComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cached     []string
		keyCached  bool
		ids        []string
		addErr     error
		wantCache  []string
		wantStored [][]string
	}{
		{name: "empty slice on an absent key", ids: nil},
		{name: "empty slice on a cached key", keyCached: true, cached: []string{"a"}, wantCache: []string{"a"}},
		{name: "absent key stores every ID and stays absent", ids: []string{"a", "b"}, wantStored: [][]string{{"a", "b"}}},
		{name: "cached key stores only the absent IDs", keyCached: true, cached: []string{"a"}, ids: []string{"a", "c"}, wantCache: []string{"a", "c"}, wantStored: [][]string{{"c"}}},
		{name: "cached key with every ID known writes nothing", keyCached: true, cached: []string{"a", "b"}, ids: []string{"b", "a"}, wantCache: []string{"a", "b"}},
		{name: "repeated ID is stored once", keyCached: true, ids: []string{"a", "a"}, wantCache: []string{"a"}, wantStored: [][]string{{"a"}}},
		{name: "store error keeps the cache", keyCached: true, ids: []string{"a"}, addErr: errors.New("disk full"), wantCache: []string{"a"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISS-H-1"
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			rkey := ReactionKey(issueID, ReactionKindReview)
			store := newFingerprintModelStore()
			store.addErr = tt.addErr
			if tt.keyCached {
				seedHandedOff(state, rkey, tt.cached...)
			}
			log, buf := logCapture()

			recordHandedOffComments(context.Background(), state, store, issueID, ReactionKindReview, tt.ids, log)

			if !slices.EqualFunc(store.addCalls, tt.wantStored, slices.Equal) {
				t.Errorf("AddReactionHandedOffComments batches = %v, want %v", store.addCalls, tt.wantStored)
			}
			if tt.keyCached {
				assertHandedOffCached(t, state, rkey, tt.wantCache...)
			} else {
				assertHandedOff(t, state, rkey)
			}
			if tt.addErr != nil && findLogLine(buf.String(), "failed to persist handed-off comments") == "" {
				t.Errorf("log output missing the persist warning; log=%s", buf.String())
			}
		})
	}
}

func TestRecordHandedOffComments_KeysAreIndependent(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-H-2"
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	botKey := ReactionKey(issueID, ReactionKindBotReview)
	seedHandedOff(state, reviewKey)
	seedHandedOff(state, botKey)
	store := newFingerprintModelStore()

	recordHandedOffComments(context.Background(), state, store, issueID, ReactionKindReview, []string{"a"}, discardLogger())
	recordHandedOffComments(context.Background(), state, store, issueID, ReactionKindBotReview, []string{"b"}, discardLogger())

	assertHandedOffCached(t, state, reviewKey, "a")
	assertHandedOffCached(t, state, botKey, "b")
	assertStoredHandedOff(t, store, issueID, ReactionKindReview, "a")
	assertStoredHandedOff(t, store, issueID, ReactionKindBotReview, "b")
}

func TestCarriesNewComment(t *testing.T) {
	t.Parallel()

	runningWith := func(kind string, ids ...string) *RunningEntry {
		comments := make([]domain.ReviewComment, len(ids))
		for i, id := range ids {
			comments[i] = summaryComment(id)
		}
		return &RunningEntry{ContinuationContext: map[string]any{commentTemplateKey(kind): buildReviewTemplateMap(comments)}}
	}

	tests := []struct {
		name     string
		kind     string
		seed     []string
		reported []string
		running  *RunningEntry
		input    []domain.ReviewComment
		want     bool
	}{
		{name: "absent set with comments", kind: ReactionKindReview, input: []domain.ReviewComment{summaryComment("a")}, want: true},
		{name: "absent set with no comments", kind: ReactionKindReview},
		{name: "present set with no comments", kind: ReactionKindReview, seed: []string{"a"}},
		{name: "every ID present", kind: ReactionKindReview, seed: []string{"a", "b", "c"}, input: []domain.ReviewComment{summaryComment("a"), inlineComment("b")}},
		{name: "one ID absent", kind: ReactionKindReview, seed: []string{"a", "b"}, input: []domain.ReviewComment{summaryComment("a"), inlineComment("c")}, want: true},
		{name: "reported ID is not new", kind: ReactionKindBotReview, seed: []string{"a"}, reported: []string{"r"}, input: []domain.ReviewComment{summaryComment("a"), summaryComment("r")}},
		{name: "running ID is not new", kind: ReactionKindReview, seed: []string{"a"}, running: runningWith(ReactionKindReview, "b"), input: []domain.ReviewComment{summaryComment("a"), summaryComment("b")}},
		{name: "running IDs count whatever the entry reaction kind", kind: ReactionKindBotReview, running: &RunningEntry{ReactionKind: ReactionKindReview, ContinuationContext: runningWith(ReactionKindBotReview, "b").ContinuationContext}, input: []domain.ReviewComment{summaryComment("b")}},
		{name: "running IDs under the other kind's key do not count", kind: ReactionKindReview, running: runningWith(ReactionKindBotReview, "b"), input: []domain.ReviewComment{summaryComment("b")}, want: true},
		{name: "running entry without a continuation counts nothing", kind: ReactionKindReview, running: &RunningEntry{}, input: []domain.ReviewComment{summaryComment("b")}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISS-H-3"
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			rkey := ReactionKey(issueID, tt.kind)
			if len(tt.seed) > 0 {
				seedHandedOff(state, rkey, tt.seed...)
			}
			if len(tt.reported) > 0 {
				state.ReactionReportedComments[rkey] = map[string]struct{}{}
				for _, id := range tt.reported {
					state.ReactionReportedComments[rkey][id] = struct{}{}
				}
			}
			if tt.running != nil {
				state.Running[issueID] = tt.running
			}

			got := carriesNewComment(state, issueID, tt.kind, tt.input)

			if got != tt.want {
				t.Errorf("carriesNewComment(%s, %v) = %v, want %v", tt.kind, tt.input, got, tt.want)
			}
			if len(tt.seed) > 0 {
				assertHandedOffCached(t, state, rkey, tt.seed...)
			} else {
				assertHandedOff(t, state, rkey)
			}
		})
	}
}

func TestReconcileReviewComments_SpentBudget_NoNewComment(t *testing.T) {
	t.Parallel()

	allowlisted := summaryComment("c2")
	allowlisted.Reviewer = "review-bot"

	tests := []struct {
		name      string
		handedOff []string
		comments  []domain.ReviewComment
		wantDebug bool
		wantCount int
	}{
		{
			name:      "strict subset of the handed-off set",
			handedOff: []string{"c1", "c2", "review-1"},
			comments:  []domain.ReviewComment{summaryComment("review-1"), inlineComment("c1")},
			wantDebug: true,
			wantCount: 2,
		},
		{
			name:      "identical to the handed-off set",
			handedOff: []string{"c1", "c2"},
			comments:  []domain.ReviewComment{inlineComment("c1"), inlineComment("c2")},
			wantDebug: true,
			wantCount: 2,
		},
		{
			name:      "the new comment is outdated",
			handedOff: []string{"c1"},
			comments:  []domain.ReviewComment{inlineComment("c1"), outdatedComment(inlineComment("c2"))},
			wantDebug: true,
			wantCount: 1,
		},
		{
			name:      "the new comment comes from an allowlisted bot",
			handedOff: []string{"c1"},
			comments:  []domain.ReviewComment{inlineComment("c1"), allowlisted},
			wantDebug: true,
			wantCount: 1,
		},
		{
			name:      "the provider returns nothing",
			handedOff: []string{"c1"},
			comments:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, rkey := spentReviewState(t, "ISS-R-SPENT", tt.handedOff...)
			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			tracker := &reviewTrackerStub{}
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewEscalationParams(t, store, scm, tracker)
			log, buf := logCapture()

			reconcileReviewComments(state, params, log, context.Background(), metrics)
			state.TrackerOpsWg.Wait()

			entry, ok := state.PendingReactions[rkey]
			if !ok {
				t.Fatal("PendingReactions entry missing, want re-enqueued")
			}
			if want := reviewBaseTime.Add(time.Minute); !entry.PendingRetryAt.Equal(want) {
				t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, want)
			}
			if scm.calls != 1 {
				t.Errorf("FetchPendingReviews calls = %d, want 1", scm.calls)
			}
			assertEscalations(t, tracker, 0)
			if state.ReactionAttempts[rkey] != 3 {
				t.Errorf("ReactionAttempts[%s] = %d, want 3 (unchanged)", rkey, state.ReactionAttempts[rkey])
			}
			assertHandedOff(t, state, rkey, tt.handedOff...)
			if _, ok := state.Claimed["ISS-R-SPENT"]; !ok {
				t.Error("claim released, want kept")
			}
			if _, ok := state.RetryAttempts["ISS-R-SPENT"]; ok {
				t.Error("retry scheduled, want none")
			}
			if len(store.deletedIssueIDs) != 0 || store.deleteFingerprintCalls != 0 {
				t.Errorf("DeleteRetryEntry calls = %d, DeleteReactionFingerprint calls = %d, want both 0",
					len(store.deletedIssueIDs), store.deleteFingerprintCalls)
			}
			if len(metrics.reviewChecks) != 0 || len(metrics.reviewEscalations) != 0 {
				t.Errorf("review metrics = checks:%v escalations:%v, want none", metrics.reviewChecks, metrics.reviewEscalations)
			}
			debugMsg := "comment set already handed off, not dispatching"
			if !tt.wantDebug {
				assertLogLacksLine(t, buf.String(), debugMsg)
				if store.markDispatchedCalls != 0 {
					t.Errorf("MarkReactionDispatched calls = %d, want 0 with no actionable comment", store.markDispatchedCalls)
				}
				return
			}
			assertLogLineHasIntAttr(t, buf.String(), debugMsg, "comment_count", tt.wantCount)
			if store.markDispatchedCalls != 1 {
				t.Errorf("MarkReactionDispatched calls = %d, want 1 (a settled set is marked dispatched)", store.markDispatchedCalls)
			}
		})
	}
}

func TestReconcileReviewComments_SpentBudget_NoDecisionBeforeObservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		dispatchedStore bool
		setup           func(state *State, scm *mockSCMAdapter)
		check           func(t *testing.T, entry *PendingReaction, metrics *reviewMetricsSpy)
	}{
		{
			name: "fetch error backs off",
			setup: func(_ *State, scm *mockSCMAdapter) {
				scm.err = errors.New("provider unavailable")
			},
			check: func(t *testing.T, entry *PendingReaction, metrics *reviewMetricsSpy) {
				t.Helper()
				if entry.PendingAttempts != 1 {
					t.Errorf("PendingAttempts = %d, want 1", entry.PendingAttempts)
				}
				if metrics.reviewChecks["error"] != 1 {
					t.Errorf(`IncReviewChecks("error") = %d, want 1`, metrics.reviewChecks["error"])
				}
			},
		},
		{
			name: "debounce window defers",
			setup: func(_ *State, scm *mockSCMAdapter) {
				fresh := inlineComment("c-new")
				fresh.SubmittedAt = reviewBaseTime.Add(-10 * time.Second)
				scm.comments = []domain.ReviewComment{inlineComment("c1"), fresh}
			},
			check: func(t *testing.T, entry *PendingReaction, _ *reviewMetricsSpy) {
				t.Helper()
				want := reviewBaseTime.Add(-10 * time.Second).Add(time.Duration(defaultReviewConfig().DebounceMS) * time.Millisecond)
				if !entry.PendingRetryAt.Equal(want) {
					t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, want)
				}
			},
		},
		{
			name: "occupied retry slot defers",
			setup: func(state *State, scm *mockSCMAdapter) {
				scm.comments = []domain.ReviewComment{inlineComment("c1"), inlineComment("c-new")}
				state.RetryAttempts["ISS-R-NODECIDE"] = &RetryEntry{
					IssueID:      "ISS-R-NODECIDE",
					Attempt:      1,
					ReactionKind: ReactionKindLabelReview,
				}
				state.PendingReactions[ReactionKey("ISS-R-NODECIDE", ReactionKindReview)].CreatedAt = reviewBaseTime.Add(-5 * time.Minute)
			},
			check: func(t *testing.T, entry *PendingReaction, _ *reviewMetricsSpy) {
				t.Helper()
				if !entry.CreatedAt.Equal(reviewBaseTime) {
					t.Errorf("CreatedAt = %v, want refreshed to %v", entry.CreatedAt, reviewBaseTime)
				}
			},
		},
		{
			name:            "dispatched fingerprint deduplicates",
			dispatchedStore: true,
			setup: func(_ *State, scm *mockSCMAdapter) {
				scm.comments = []domain.ReviewComment{inlineComment("c-new")}
			},
			check: func(t *testing.T, entry *PendingReaction, _ *reviewMetricsSpy) {
				t.Helper()
				if want := reviewBaseTime.Add(time.Minute); !entry.PendingRetryAt.Equal(want) {
					t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, rkey := spentReviewState(t, "ISS-R-NODECIDE", "c1")
			store := newFingerprintModelStore()
			metrics := newReviewMetricsSpy()
			tracker := &reviewTrackerStub{}
			scm := &mockSCMAdapter{}
			tt.setup(state, scm)
			if tt.dispatchedStore {
				store.seedDispatched("ISS-R-NODECIDE", ReactionKindReview, scm.comments)
			}
			params := reviewEscalationParams(t, store, scm, tracker)

			reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
			state.TrackerOpsWg.Wait()

			entry, ok := state.PendingReactions[rkey]
			if !ok {
				t.Fatal("PendingReactions entry missing, want re-enqueued")
			}
			tt.check(t, entry, metrics)
			if scm.calls != 1 {
				t.Errorf("FetchPendingReviews calls = %d, want 1", scm.calls)
			}
			assertEscalations(t, tracker, 0)
			if state.ReactionAttempts[rkey] != 3 {
				t.Errorf("ReactionAttempts[%s] = %d, want 3 (unchanged)", rkey, state.ReactionAttempts[rkey])
			}
			assertHandedOff(t, state, rkey, "c1")
			if _, ok := state.Claimed["ISS-R-NODECIDE"]; !ok {
				t.Error("claim released, want kept")
			}
		})
	}
}

func TestReconcileReviewComments_SpentBudget_NewCommentEscalatesOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		handedOff []string
		comments  []domain.ReviewComment
	}{
		{
			name:      "a summary item is the only new ID",
			handedOff: []string{"c1"},
			comments:  []domain.ReviewComment{inlineComment("c1"), summaryComment("review-9")},
		},
		{
			name:     "nothing was handed off",
			comments: []domain.ReviewComment{inlineComment("c1")},
		},
		{
			name:      "an inline comment next to handed-off ones",
			handedOff: []string{"c1"},
			comments:  []domain.ReviewComment{inlineComment("c1"), inlineComment("c2")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, rkey := spentReviewState(t, "ISS-R-ONCE", tt.handedOff...)
			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			tracker := &reviewTrackerStub{}
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewEscalationParams(t, store, scm, tracker)
			log, buf := logCapture()

			reconcileReviewComments(state, params, log, context.Background(), metrics)
			state.TrackerOpsWg.Wait()

			if scm.calls != 1 {
				t.Errorf("FetchPendingReviews calls = %d, want 1", scm.calls)
			}
			assertEscalations(t, tracker, 1)
			if metrics.reviewEscalations["label"] != 1 {
				t.Errorf(`IncReviewEscalations("label") = %d, want 1`, metrics.reviewEscalations["label"])
			}
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Error("PendingReactions entry present after escalation; want consumed")
			}
			if _, ok := state.Claimed["ISS-R-ONCE"]; ok {
				t.Error("claim kept after escalation; want released")
			}
			if _, ok := state.ReactionAttempts[rkey]; ok {
				t.Error("counter kept after escalation; want deleted")
			}
			assertHandedOffCached(t, state, rkey, tt.handedOff...)
			if store.deleteFingerprintCalls != 1 {
				t.Errorf("DeleteReactionFingerprint calls = %d, want 1", store.deleteFingerprintCalls)
			}
			if len(metrics.reviewChecks) != 0 {
				t.Errorf("review check metrics = %v, want none", metrics.reviewChecks)
			}
			assertLogLineHasIntAttr(t, buf.String(), "review fix continuation turns exhausted, escalating", "turn_count", 3)
			assertLogLineHasIntAttr(t, buf.String(), "review fix continuation turns exhausted, escalating", "max_continuation_turns", 3)
			assertLogLacksLine(t, buf.String(), "review triage requested escalation")
		})
	}
}

func TestReconcileReviewComments_SpentBudget_TriageGateNotReached(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		comments       []domain.ReviewComment
		wantEscalation int
	}{
		{"no new comment starts no run", []domain.ReviewComment{inlineComment("c1")}, 0},
		{"a new comment escalates without a run", []domain.ReviewComment{inlineComment("c1"), inlineComment("c2")}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISS-R-NOGATE"
			root := mustTriageWorkspace(t, issueID+"-ident")
			state, rkey := spentReviewState(t, issueID, "c1")
			tracker := &reviewTrackerStub{}
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewTriageParams(t, &reviewReconcileStore{}, scm, tracker, root, escalateTriageScript)
			params.Router = commentRouter(t, tracker, domain.EventEscalationReviewComments)

			reconcileReviewComments(state, params, discardLogger(), context.Background(), newReviewMetricsSpy())
			state.TrackerOpsWg.Wait()

			assertEscalations(t, tracker, tt.wantEscalation)
			if entry, ok := state.PendingReactions[rkey]; ok && entry.Triage != nil {
				t.Errorf("PendingReactions[%s].Triage = %+v, want no run started on a spent budget", rkey, entry.Triage)
			}
			if tt.wantEscalation == 0 {
				if _, ok := state.PendingReactions[rkey]; !ok {
					t.Error("PendingReactions entry missing, want re-enqueued")
				}
			}
		})
	}
}

func TestReconcileReviewComments_BelowBudget_OnlyANewCommentDispatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		comments    []domain.ReviewComment
		wantCarried []string
	}{
		{"a shrunk set settles", []domain.ReviewComment{inlineComment("c1")}, nil},
		{"a new comment dispatches with the remaining one", []domain.ReviewComment{inlineComment("c1"), inlineComment("c3")}, []string{"c1", "c3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISS-R-SUBSET"
			state := stateWithReviewReaction(t, issueID, 10)
			rkey := ReactionKey(issueID, ReactionKindReview)
			state.ReactionAttempts[rkey] = 1
			store := newFingerprintModelStore()
			store.seedRows(issueID, ReactionKindReview, "c1", "c2")
			store.seedDispatched(issueID, ReactionKindReview, []domain.ReviewComment{inlineComment("c1"), inlineComment("c2")})
			tracker := &reviewTrackerStub{}
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewEscalationParams(t, store, scm, tracker)
			metrics := newReviewMetricsSpy()

			reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

			assertEscalations(t, tracker, 0)
			if tt.wantCarried == nil {
				if _, ok := state.RetryAttempts[issueID]; ok {
					t.Error("retry scheduled for a set that only lost members; want none")
				}
				if state.ReactionAttempts[rkey] != 1 {
					t.Errorf("ReactionAttempts[%s] = %d, want 1 (unchanged)", rkey, state.ReactionAttempts[rkey])
				}
				if metrics.reviewChecks["dispatched"] != 0 {
					t.Errorf(`IncReviewChecks("dispatched") = %d, want 0`, metrics.reviewChecks["dispatched"])
				}
				assertFingerprintMarked(t, store, issueID, ReactionKindReview, tt.comments, true)
				return
			}
			if got := continuationIDs(t, state, issueID, ReactionKindReview); !slices.Equal(got, tt.wantCarried) {
				t.Errorf("continuation carries %v, want %v", got, tt.wantCarried)
			}
			if state.ReactionAttempts[rkey] != 2 {
				t.Errorf("ReactionAttempts[%s] = %d, want 2", rkey, state.ReactionAttempts[rkey])
			}
			if metrics.reviewChecks["dispatched"] != 1 {
				t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
			}
		})
	}
}

func TestReconcileReviewComments_HandedOff_NormalExitRecordsPresentedIDs(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-REC"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	store := newFingerprintModelStore()
	scm := &mockSCMAdapter{}
	allowlisted := summaryComment("bot-note")
	allowlisted.Reviewer = "review-bot"
	params := reviewEscalationParams(t, store, scm, &reviewTrackerStub{})
	metrics := newReviewMetricsSpy()

	scm.comments = []domain.ReviewComment{
		inlineComment("c1"),
		summaryComment("review-1"),
		outdatedComment(inlineComment("c-old")),
		allowlisted,
	}
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if got := continuationIDs(t, state, issueID, ReactionKindReview); !slices.Equal(got, []string{"c1", "review-1"}) {
		t.Errorf("continuation carries %v, want [c1 review-1]", got)
	}
	assertStoredHandedOff(t, store, issueID, ReactionKindReview)
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}

	completeContinuation(t, state, store, issueID, ReactionKindReview, newReviewPendingEntry(issueID, 10))

	assertStoredHandedOff(t, store, issueID, ReactionKindReview, "c1", "review-1")

	scm.comments = []domain.ReviewComment{summaryComment("review-1"), inlineComment("c2")}
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if got := continuationIDs(t, state, issueID, ReactionKindReview); !slices.Equal(got, []string{"c2", "review-1"}) {
		t.Errorf("continuation carries %v, want [c2 review-1]", got)
	}
	if state.ReactionAttempts[rkey] != 2 {
		t.Errorf("ReactionAttempts[%s] = %d, want 2", rkey, state.ReactionAttempts[rkey])
	}

	completeContinuation(t, state, store, issueID, ReactionKindReview, newReviewPendingEntry(issueID, 10))

	assertStoredHandedOff(t, store, issueID, ReactionKindReview, "c1", "c2", "review-1")
	assertHandedOffCached(t, state, rkey, "c1", "c2", "review-1")
}

func TestReconcileReviewComments_HandedOff_WatchWindowDropKeepsRowsAndSpentCounter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
	}{
		{"counter below the budget", 2},
		{"counter at the budget", 3},
		{"counter above the budget", 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "ISS-R-WWSPENT"
			state := stateWithReviewReaction(t, issueID, 10)
			rkey := ReactionKey(issueID, ReactionKindReview)
			botKey := ReactionKey(issueID, ReactionKindBotReview)
			state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
			state.ReactionAttempts[rkey] = tt.attempts
			seedHandedOff(state, rkey, "c1", "c2")
			seedHandedOff(state, botKey, "b1")
			store := newFingerprintModelStore()
			store.seedRows(issueID, ReactionKindReview, "c1", "c2")
			store.seedRows(issueID, ReactionKindBotReview, "b1")
			scm := &mockSCMAdapter{}
			params := reviewParams(&reviewReconcileStore{}, scm, nil)
			params.Store = store
			params.ReviewPendingTTL = 30 * time.Minute

			reconcileReviewComments(state, params, discardLogger(), context.Background(), newReviewMetricsSpy())

			if _, ok := state.PendingReactions[rkey]; ok {
				t.Error("PendingReactions entry present after the drop; want removed")
			}
			wantAttempts := tt.attempts
			if tt.attempts < defaultReviewConfig().MaxContinuationTurns {
				wantAttempts = 0
			}
			if state.ReactionAttempts[rkey] != wantAttempts {
				t.Errorf("ReactionAttempts[%s] = %d, want %d", rkey, state.ReactionAttempts[rkey], wantAttempts)
			}
			assertHandedOff(t, state, rkey)
			assertHandedOffCached(t, state, botKey, "b1")
			assertStoredHandedOff(t, store, issueID, ReactionKindReview, "c1", "c2")
			assertStoredHandedOff(t, store, issueID, ReactionKindBotReview, "b1")
			if scm.calls != 0 {
				t.Errorf("FetchPendingReviews calls = %d, want 0", scm.calls)
			}
		})
	}
}

func TestReconcileReviewComments_HandedOff_TriageEscalationKeepsSet(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-CLEAR"
	root := mustTriageWorkspace(t, issueID+"-ident")
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[rkey] = 1
	seedHandedOff(state, rkey, "rc-earlier")
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(t, &reviewReconcileStore{}, scm, tracker, root, escalateTriageScript)
	metrics := newReviewMetricsSpy()

	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Fatalf("AddLabel calls = %d, want 1", tracker.addLabelCalled)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Error("ReactionAttempts present after a triage escalation; want deleted")
	}
	assertHandedOffCached(t, state, rkey, "rc-earlier")
}

func TestReconcileReviewComments_HandedOff_ReseededEntryAfterSpentDrop(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-RESEED"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
	state.ReactionAttempts[rkey] = 3

	store := newFingerprintModelStore()
	store.seedRows(issueID, ReactionKindReview, "c1", "c2")
	store.seedDispatched(issueID, ReactionKindReview, []domain.ReviewComment{inlineComment("c1"), inlineComment("c2")})
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{}
	params := reviewEscalationParams(t, store, scm, tracker)
	params.ReviewPendingTTL = 30 * time.Minute
	metrics := newReviewMetricsSpy()
	pass := func() {
		reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
		state.TrackerOpsWg.Wait()
	}

	pass()

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Fatal("aged entry present after the drop; want removed")
	}
	state.PendingReactions[rkey] = newReviewPendingEntry(issueID, 10)

	for _, comments := range [][]domain.ReviewComment{
		{inlineComment("c1")},
		{inlineComment("c1"), inlineComment("c2")},
		{inlineComment("c2")},
	} {
		scm.comments = comments
		makeDue(t, state, rkey)

		pass()

		if _, ok := state.RetryAttempts[issueID]; ok {
			t.Fatalf("a continuation was scheduled for %d handed-off comments; want none", len(comments))
		}
		if state.ReactionAttempts[rkey] != 3 {
			t.Fatalf("ReactionAttempts[%s] = %d, want 3", rkey, state.ReactionAttempts[rkey])
		}
		assertEscalations(t, tracker, 0)
	}

	scm.comments = []domain.ReviewComment{inlineComment("c1"), inlineComment("c3")}
	makeDue(t, state, rkey)

	pass()

	assertEscalations(t, tracker, 1)
	assertStoredHandedOff(t, store, issueID, ReactionKindReview, "c1", "c2")
}

func TestReconcileReviewComments_NoEscalationAfterSuccessfulLastTurn(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-LAST"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	store := newFingerprintModelStore()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{}
	params := reviewEscalationParams(t, store, scm, tracker)
	metrics := newReviewMetricsSpy()
	log, buf := logCapture()
	const escalatingMsg = "review fix continuation turns exhausted, escalating"
	pass := func() {
		reconcileReviewComments(state, params, log, context.Background(), metrics)
		state.TrackerOpsWg.Wait()
	}

	turns := [][]domain.ReviewComment{
		{summaryComment("review-1"), inlineComment("c1")},
		{summaryComment("review-1"), outdatedComment(inlineComment("c1")), summaryComment("review-2"), inlineComment("c2")},
		{summaryComment("review-1"), summaryComment("review-2"), outdatedComment(inlineComment("c2")), summaryComment("review-3"), inlineComment("c3")},
	}
	for i, comments := range turns {
		scm.comments = comments
		pass()
		if state.ReactionAttempts[rkey] != i+1 {
			t.Fatalf("ReactionAttempts[%s] after turn %d = %d, want %d", rkey, i+1, state.ReactionAttempts[rkey], i+1)
		}
		completeContinuation(t, state, store, issueID, ReactionKindReview, newReviewPendingEntry(issueID, 10))
	}

	scm.comments = []domain.ReviewComment{
		summaryComment("review-1"), summaryComment("review-2"), summaryComment("review-3"), outdatedComment(inlineComment("c3")),
	}
	for poll := 1; poll <= 2; poll++ {
		pass()

		entry, ok := state.PendingReactions[rkey]
		if !ok {
			t.Fatalf("poll %d after the last turn consumed the entry; want it re-enqueued", poll)
		}
		if want := reviewBaseTime.Add(time.Minute); !entry.PendingRetryAt.Equal(want) {
			t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, want)
		}
		assertEscalations(t, tracker, 0)
		assertLogLacksLine(t, buf.String(), escalatingMsg)
		if state.ReactionAttempts[rkey] != 3 {
			t.Errorf("ReactionAttempts[%s] = %d, want 3", rkey, state.ReactionAttempts[rkey])
		}
		makeDue(t, state, rkey)
	}
	if want := 3 + 2; scm.calls != want {
		t.Errorf("FetchPendingReviews calls = %d, want %d", scm.calls, want)
	}

	scm.comments = append(scm.comments, inlineComment("c4"))
	pass()

	assertEscalations(t, tracker, 1)
	assertLogLineHasIntAttr(t, buf.String(), escalatingMsg, "turn_count", 3)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry present after the escalation; want consumed")
	}
	assertHandedOffCached(t, state, rkey, "c1", "c2", "c3", "review-1", "review-2", "review-3")
	assertStoredHandedOff(t, store, issueID, ReactionKindReview, "c1", "c2", "c3", "review-1", "review-2", "review-3")
}

var reviewFamilyKinds = []string{ReactionKindReview, ReactionKindBotReview}

type reactionCycle struct {
	t       *testing.T
	kind    string
	issueID string
	budget  int
	base    time.Time
	state   *State
	store   *fingerprintModelStore
	scm     *mockSCMAdapter
	tracker *reviewTrackerStub
	params  ReconcileParams
	log     *slog.Logger
	logBuf  *bytes.Buffer

	reviewMetrics *reviewMetricsSpy
	botMetrics    *botReviewMetricsSpy
}

func newReactionCycle(t *testing.T, kind string) *reactionCycle {
	t.Helper()
	c := &reactionCycle{
		t:       t,
		kind:    kind,
		store:   newFingerprintModelStore(),
		scm:     &mockSCMAdapter{},
		tracker: &reviewTrackerStub{},
	}
	c.log, c.logBuf = logCapture()
	switch kind {
	case ReactionKindReview:
		c.issueID = "ISS-R-CYCLE"
		c.budget = defaultReviewConfig().MaxContinuationTurns
		c.base = reviewBaseTime
		c.reviewMetrics = newReviewMetricsSpy()
		c.params = reviewEscalationParams(t, c.store, c.scm, c.tracker)
	case ReactionKindBotReview:
		c.issueID = "BOT-CYCLE"
		c.budget = defaultBotReviewConfig().MaxContinuationTurns
		c.base = botReviewBaseTime
		c.botMetrics = newBotReviewMetricsSpy()
		c.params = botReviewEscalationParams(t, c.store, c.scm, c.tracker)
	default:
		t.Fatalf("newReactionCycle: unsupported kind %q", kind)
	}
	c.restart()
	return c
}

func (c *reactionCycle) rkey() string { return ReactionKey(c.issueID, c.kind) }

func (c *reactionCycle) newPending() *PendingReaction {
	if c.kind == ReactionKindReview {
		return newReviewPendingEntry(c.issueID, 10)
	}
	return makeBotReviewPendingEntry(c.t, c.issueID, 10)
}

func (c *reactionCycle) restart() {
	c.state = NewState(5000, 4, 0, nil, AgentTotals{})
	c.state.Claimed[c.issueID] = struct{}{}
	c.state.PendingReactions[c.rkey()] = c.newPending()
}

func (c *reactionCycle) pass() {
	c.t.Helper()
	if c.kind == ReactionKindReview {
		reconcileReviewComments(c.state, c.params, c.log, context.Background(), c.reviewMetrics)
	} else {
		reconcileBotReviewComments(c.state, c.params, c.log, context.Background(), c.botMetrics)
	}
	c.state.TrackerOpsWg.Wait()
}

func (c *reactionCycle) setComments(comments []domain.ReviewComment) {
	if c.kind == ReactionKindReview {
		c.scm.comments = comments
	} else {
		c.scm.botComments = comments
	}
}

func (c *reactionCycle) poll(comments ...domain.ReviewComment) {
	c.t.Helper()
	c.setComments(comments)
	makeDue(c.t, c.state, c.rkey())
	c.pass()
}

func (c *reactionCycle) freshRun(comments ...domain.ReviewComment) map[string]any {
	c.t.Helper()
	identifier := c.issueID + "-ident"
	root := c.t.TempDir()
	writeRecoverySCM(c.t, root, identifier, domain.SCMMetadata{Branch: "feature/fix", PRNumber: 10, Owner: "owner", Repo: "repo"})
	ws, err := workspace.ComputePath(root, identifier)
	if err != nil {
		c.t.Fatalf("workspace.ComputePath: %v", err)
	}
	delete(c.state.PendingReactions, c.rkey())
	c.setComments(comments)
	issue := domain.Issue{ID: c.issueID, Identifier: identifier, State: "In Progress"}

	seed := freshRunSeed(context.Background(), c.state, freshRunSeedParams{
		SCMAdapter:          c.scm,
		Store:               c.store,
		WorkspaceRoot:       root,
		ReviewConfigured:    true,
		BotReviewConfigured: true,
	}, issue, c.log)

	DispatchIssue(WithContinuationContext(context.Background(), seed), c.state, issue, nil, "", func(context.Context, domain.Issue, *int) {})
	c.t.Cleanup(c.state.WorkerWg.Wait)
	c.state.Running[c.issueID].ContinuationContext = seed
	exitParams := workerExitParams(c.store)
	exitParams.SCMAdapter = c.scm
	exitParams.BotReviewReactionConfigured = true

	HandleWorkerExit(c.state, WorkerResult{
		IssueID:           c.issueID,
		Identifier:        identifier,
		ExitKind:          WorkerExitNormal,
		WorkspacePath:     ws.Path,
		HandedOffComments: presentedByTemplate(c.t, commentsPromptTemplate, issue, seed, true),
	}, exitParams)

	CancelRetry(c.state, c.issueID)
	return seed
}

func (c *reactionCycle) complete() {
	c.t.Helper()
	completeContinuation(c.t, c.state, c.store, c.issueID, c.kind, c.newPending())
}

func (c *reactionCycle) dispatchedChecks() int {
	if c.kind == ReactionKindReview {
		return c.reviewMetrics.reviewChecks["dispatched"]
	}
	return c.botMetrics.botReviewChecks["dispatched"]
}

func (c *reactionCycle) attempts() int { return c.state.ReactionAttempts[c.rkey()] }

func (c *reactionCycle) assertNoTurn(wantAttempts int) {
	c.t.Helper()
	if _, ok := c.state.RetryAttempts[c.issueID]; ok {
		c.t.Error("continuation scheduled; want none")
	}
	if got := c.attempts(); got != wantAttempts {
		c.t.Errorf("ReactionAttempts[%s] = %d, want %d", c.rkey(), got, wantAttempts)
	}
	assertEscalations(c.t, c.tracker, 0)
	if _, ok := c.state.PendingReactions[c.rkey()]; !ok {
		c.t.Error("PendingReactions entry consumed; want re-enqueued")
	}
}

func (c *reactionCycle) ageEntry() time.Time {
	created := c.base.Add(-5 * time.Minute)
	c.state.PendingReactions[c.rkey()].CreatedAt = created
	return created
}

func (c *reactionCycle) assertEntryWaitsOnePoll(wantCreatedAt time.Time) {
	c.t.Helper()
	entry, ok := c.state.PendingReactions[c.rkey()]
	if !ok {
		c.t.Fatal("PendingReactions entry consumed; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(wantCreatedAt) {
		c.t.Errorf("PendingReactions[%s].CreatedAt = %v, want %v (a settled pass must not extend the watch)", c.rkey(), entry.CreatedAt, wantCreatedAt)
	}
	if want := c.base.Add(time.Minute); !entry.PendingRetryAt.Equal(want) {
		c.t.Errorf("PendingReactions[%s].PendingRetryAt = %v, want %v", c.rkey(), entry.PendingRetryAt, want)
	}
	if entry.PendingAttempts != 0 {
		c.t.Errorf("PendingReactions[%s].PendingAttempts = %d, want 0", c.rkey(), entry.PendingAttempts)
	}
}

func (c *reactionCycle) assertDispatches(wantAttempts int, wantCarried ...string) {
	c.t.Helper()
	if got := continuationIDs(c.t, c.state, c.issueID, c.kind); !slices.Equal(got, slices.Sorted(slices.Values(wantCarried))) {
		c.t.Errorf("continuation carries %v, want %v", got, slices.Sorted(slices.Values(wantCarried)))
	}
	if got := c.attempts(); got != wantAttempts {
		c.t.Errorf("ReactionAttempts[%s] = %d, want %d", c.rkey(), got, wantAttempts)
	}
}

func TestReactionPass_ShrunkSetAfterNormalExitStartsNoTurn(t *testing.T) {
	t.Parallel()

	earlier := []domain.ReviewComment{inlineComment("a"), inlineComment("b"), inlineComment("c"), summaryComment("r")}
	shrunk := []domain.ReviewComment{outdatedComment(inlineComment("a")), outdatedComment(inlineComment("b")), inlineComment("c"), summaryComment("r")}
	remaining := []domain.ReviewComment{inlineComment("c"), summaryComment("r")}

	tests := []struct {
		name        string
		next        []domain.ReviewComment
		wantCarried []string
	}{
		{"a new comment dispatches with the remaining ones", append(slices.Clone(remaining), inlineComment("d")), []string{"c", "d", "r"}},
		{"a fresh PR-level item dispatches below the budget", append(slices.Clone(remaining), summaryComment("r2")), []string{"c", "r", "r2"}},
	}

	for _, kind := range reviewFamilyKinds {
		for _, tt := range tests {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				c := newReactionCycle(t, kind)
				c.poll(earlier...)
				c.assertDispatches(1, "a", "b", "c", "r")
				c.complete()

				c.poll(shrunk...)

				c.assertNoTurn(1)
				if got := c.dispatchedChecks(); got != 1 {
					t.Errorf("dispatched checks = %d, want 1", got)
				}
				assertFingerprintMarked(t, c.store, c.issueID, kind, remaining, true)

				c.poll(tt.next...)

				c.assertDispatches(2, tt.wantCarried...)
				if got := c.dispatchedChecks(); got != 2 {
					t.Errorf("dispatched checks = %d, want 2", got)
				}
			})
		}
	}
}

func TestReactionPass_SetWithNoNewCommentSettles(t *testing.T) {
	t.Parallel()

	earlierSet := []domain.ReviewComment{inlineComment("a"), inlineComment("b"), inlineComment("c"), summaryComment("r")}
	tests := []struct {
		name     string
		stored   []domain.ReviewComment
		fpErr    error
		comments []domain.ReviewComment
	}{
		{
			name:     "a recurring earlier set",
			stored:   []domain.ReviewComment{inlineComment("c")},
			comments: earlierSet,
		},
		{
			name:     "a failing fingerprint read",
			fpErr:    errors.New("fingerprint store unavailable"),
			comments: []domain.ReviewComment{inlineComment("c"), summaryComment("r")},
		},
	}

	for _, kind := range reviewFamilyKinds {
		for _, tt := range tests {
			for _, atBudget := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/at budget %v", kind, tt.name, atBudget), func(t *testing.T) {
					t.Parallel()

					c := newReactionCycle(t, kind)
					c.store.seedRows(c.issueID, kind, "a", "b", "c", "r")
					attempts := 1
					if atBudget {
						attempts = c.budget
					}
					c.state.ReactionAttempts[c.rkey()] = attempts
					if tt.stored != nil {
						c.store.seedDispatched(c.issueID, kind, tt.stored)
					}
					c.store.getFingerprintErr = tt.fpErr
					created := c.ageEntry()

					c.poll(tt.comments...)

					c.assertNoTurn(attempts)
					c.assertEntryWaitsOnePoll(created)
					if got := c.dispatchedChecks(); got != 0 {
						t.Errorf("dispatched checks = %d, want 0", got)
					}
					if c.store.markDispatchedCalls != 1 {
						t.Errorf("MarkReactionDispatched calls = %d, want 1", c.store.markDispatchedCalls)
					}
				})
			}
		}
	}
}

func TestReactionPass_SpentBudgetEscalation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		kind            string
		comments        []domain.ReviewComment
		wantEscalations int
		wantReported    []string
	}{
		{"review: a new inline comment escalates once", ReactionKindReview, []domain.ReviewComment{inlineComment("a"), inlineComment("n")}, 1, nil},
		{"review: a new PR-level item escalates once", ReactionKindReview, []domain.ReviewComment{inlineComment("a"), summaryComment("r2")}, 1, nil},
		{"bot-review: a new inline comment escalates once", ReactionKindBotReview, []domain.ReviewComment{inlineComment("a"), inlineComment("n")}, 1, []string{"a", "n"}},
		{"bot-review: a new PR-level item is reported without escalating", ReactionKindBotReview, []domain.ReviewComment{inlineComment("a"), summaryComment("r2")}, 0, []string{"a", "r2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newReactionCycle(t, tt.kind)
			c.store.seedRows(c.issueID, tt.kind, "a")
			c.state.ReactionAttempts[c.rkey()] = c.budget

			c.poll(tt.comments...)

			assertEscalations(t, c.tracker, tt.wantEscalations)
			if tt.kind == ReactionKindBotReview {
				assertReportedComments(t, c.state, c.rkey(), tt.wantReported...)
			}
			assertStoredHandedOff(t, c.store, c.issueID, tt.kind, "a")
			if tt.wantEscalations == 1 {
				return
			}

			if _, ok := c.state.PendingReactions[c.rkey()]; !ok {
				t.Fatal("PendingReactions entry consumed; want re-enqueued")
			}
			c.poll(tt.comments...)

			c.assertNoTurn(c.budget)
			assertFingerprintMarked(t, c.store, c.issueID, tt.kind, tt.comments, true)
		})
	}
}

func TestReactionPass_LoadFailureDefersTheDecision(t *testing.T) {
	t.Parallel()

	for _, kind := range reviewFamilyKinds {
		for _, atBudget := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/at budget %v", kind, atBudget), func(t *testing.T) {
				t.Parallel()

				c := newReactionCycle(t, kind)
				c.store.listErr = errors.New("handed-off store unavailable")
				attempts := 1
				if atBudget {
					attempts = c.budget
				}
				c.state.ReactionAttempts[c.rkey()] = attempts

				c.poll(inlineComment("a"), summaryComment("r"))

				c.assertNoTurn(attempts)
				if c.store.markDispatchedCalls != 0 {
					t.Errorf("MarkReactionDispatched calls = %d, want 0", c.store.markDispatchedCalls)
				}
				if want := c.base.Add(time.Minute); !c.state.PendingReactions[c.rkey()].PendingRetryAt.Equal(want) {
					t.Errorf("PendingRetryAt = %v, want %v", c.state.PendingReactions[c.rkey()].PendingRetryAt, want)
				}
				if findLogLine(c.logBuf.String(), "failed to load handed-off comments, deferring") == "" {
					t.Errorf("log output missing the load warning; log=%s", c.logBuf.String())
				}
			})
		}
	}
}

func TestReactionPass_RunningIDsHoldASubsetUntilTheRunEnds(t *testing.T) {
	t.Parallel()

	const (
		heldMsg    = "comment set held by a running turn, not dispatching"
		settledMsg = "comment set already handed off, not dispatching"
	)
	for _, kind := range reviewFamilyKinds {
		for _, atBudget := range []bool{false, true} {
			for _, restart := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/at budget %v/restart %v", kind, atBudget, restart), func(t *testing.T) {
					t.Parallel()

					c := newReactionCycle(t, kind)
					subset := []domain.ReviewComment{inlineComment("b"), inlineComment("c")}
					c.poll(inlineComment("a"), inlineComment("b"), inlineComment("c"))
					c.assertDispatches(1, "a", "b", "c")
					running := dispatchRetry(t, c.state, c.store, c.issueID)
					assertStoredHandedOff(t, c.store, c.issueID, kind)
					attempts := 1
					if atBudget {
						attempts = c.budget
						c.state.ReactionAttempts[c.rkey()] = attempts
					}
					c.state.PendingReactions[c.rkey()] = c.newPending()
					created := c.ageEntry()

					c.poll(subset...)

					c.assertNoTurn(attempts)
					c.assertEntryWaitsOnePoll(created)
					assertFingerprintMarked(t, c.store, c.issueID, kind, subset, false)
					if findLogLine(c.logBuf.String(), heldMsg) == "" {
						t.Errorf("log output missing %q; log=%s", heldMsg, c.logBuf.String())
					}
					assertLogLacksLine(t, c.logBuf.String(), settledMsg)

					HandleWorkerExit(c.state, WorkerResult{
						IssueID:    c.issueID,
						Identifier: running.Identifier,
						ExitKind:   WorkerExitCancelled,
					}, workerExitParams(c.store))
					assertStoredHandedOff(t, c.store, c.issueID, kind)
					if restart {
						c.restart()
						c.state.ReactionAttempts[c.rkey()] = attempts
					}

					c.poll(subset...)

					if atBudget {
						assertEscalations(t, c.tracker, 1)
						return
					}
					c.assertDispatches(attempts+1, "b", "c")
				})
			}
		}
	}
}

func TestReactionPass_RowsSettleARecoveredEntry(t *testing.T) {
	t.Parallel()

	disruptions := []struct {
		name string
		run  func(c *reactionCycle)
	}{
		{"fresh state over the same store", func(c *reactionCycle) { c.restart() }},
		{"watch window drop", func(c *reactionCycle) {
			c.state.PendingReactions[c.rkey()].CreatedAt = c.base.Add(-31 * time.Minute)
			c.params.ReviewPendingTTL = 30 * time.Minute
			c.params.BotReviewPendingTTL = 30 * time.Minute
			c.pass()
			c.params.ReviewPendingTTL = 0
			c.params.BotReviewPendingTTL = 0
			if _, ok := c.state.PendingReactions[c.rkey()]; ok {
				c.t.Fatal("aged entry present after the drop; want removed")
			}
			c.state.PendingReactions[c.rkey()] = c.newPending()
		}},
		{"terminal release", func(c *reactionCycle) {
			releaseTerminalIssueState(context.Background(), c.state, c.store, c.issueID, c.log)
			c.state.Claimed[c.issueID] = struct{}{}
			c.state.PendingReactions[c.rkey()] = c.newPending()
		}},
	}

	for _, kind := range reviewFamilyKinds {
		for _, d := range disruptions {
			t.Run(kind+"/"+d.name, func(t *testing.T) {
				t.Parallel()

				c := newReactionCycle(t, kind)
				c.poll(inlineComment("a"), inlineComment("b"), inlineComment("c"))
				c.complete()
				d.run(c)
				assertHandedOff(t, c.state, c.rkey())
				attempts := c.attempts()

				c.poll(inlineComment("b"), inlineComment("c"))

				c.assertNoTurn(attempts)
				assertHandedOffCached(t, c.state, c.rkey(), "a", "b", "c")
				assertStoredHandedOff(t, c.store, c.issueID, kind, "a", "b", "c")

				c.poll(inlineComment("b"), inlineComment("d"))

				c.assertDispatches(attempts+1, "b", "d")
			})
		}
	}
}

func hasLogLine(logOutput string, fragments ...string) bool {
	for line := range strings.SplitSeq(logOutput, "\n") {
		if !slices.ContainsFunc(fragments, func(f string) bool { return !strings.Contains(line, f) }) {
			return true
		}
	}
	return false
}

func TestFreshRunSeed(t *testing.T) {
	t.Parallel()

	triage := config.ReactionTriageConfig{Script: "triage.sh", TimeoutMS: 1000}
	reviewComments := []domain.ReviewComment{inlineComment("b"), summaryComment("a"), outdatedComment(inlineComment("old"))}
	botComments := []domain.ReviewComment{inlineComment("x")}

	tests := []struct {
		name         string
		noIdentity   bool
		reviewOff    bool
		reviewTriage bool
		comments     []domain.ReviewComment
		botFound     []domain.ReviewComment
		rows         map[string][]string
		fetchErr     error
		listErr      error
		want         map[string][]string
		wantWarnings []string
	}{
		{
			name:     "a new review comment seeds the whole actionable set",
			comments: reviewComments,
			want:     map[string][]string{ReactionKindReview: {"a", "b"}},
		},
		{
			name:     "both kinds are seeded",
			comments: reviewComments,
			botFound: botComments,
			want:     map[string][]string{ReactionKindReview: {"a", "b"}, ReactionKindBotReview: {"x"}},
		},
		{
			name:     "a new comment next to a given one seeds both",
			comments: reviewComments,
			rows:     map[string][]string{ReactionKindReview: {"a"}},
			want:     map[string][]string{ReactionKindReview: {"a", "b"}},
		},
		{
			name:     "every comment already given seeds nothing",
			comments: reviewComments,
			rows:     map[string][]string{ReactionKindReview: {"a", "b"}},
		},
		{
			name:     "only outdated comments seed nothing",
			comments: []domain.ReviewComment{outdatedComment(inlineComment("old"))},
		},
		{
			name:       "a workspace without a pull request identity seeds nothing",
			noIdentity: true,
			comments:   reviewComments,
			botFound:   botComments,
		},
		{
			name:         "a triage block keeps its kind out of the seed",
			reviewTriage: true,
			comments:     reviewComments,
			botFound:     botComments,
			want:         map[string][]string{ReactionKindBotReview: {"x"}},
		},
		{
			name:      "an unconfigured kind is not seeded",
			reviewOff: true,
			comments:  reviewComments,
			botFound:  botComments,
			want:      map[string][]string{ReactionKindBotReview: {"x"}},
		},
		{
			name:         "a fetch error skips only its kind",
			fetchErr:     errors.New("provider unavailable"),
			comments:     reviewComments,
			botFound:     botComments,
			want:         map[string][]string{ReactionKindBotReview: {"x"}},
			wantWarnings: []string{ReactionKindReview},
		},
		{
			name:         "a load error skips every kind that needs the set",
			listErr:      errors.New("handed-off store unavailable"),
			comments:     reviewComments,
			botFound:     botComments,
			wantWarnings: []string{ReactionKindReview, ReactionKindBotReview},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const identifier = "ISS-FRESH"
			root := t.TempDir()
			if !tt.noIdentity {
				writeRecoverySCM(t, root, identifier, domain.SCMMetadata{Branch: "feature/fix", PRNumber: 10, Owner: "owner", Repo: "repo"})
			}
			store := newFingerprintModelStore()
			store.listErr = tt.listErr
			for kind, ids := range tt.rows {
				store.seedRows("1", kind, ids...)
			}
			scm := &mockSCMAdapter{comments: tt.comments, err: tt.fetchErr, botComments: tt.botFound}
			params := freshRunSeedParams{
				SCMAdapter:          scm,
				Store:               store,
				WorkspaceRoot:       root,
				ReviewConfigured:    !tt.reviewOff,
				BotReviewConfigured: true,
			}
			if tt.reviewTriage {
				params.ReviewConfig.Triage = triage
			}
			log, buf := logCapture()

			got := freshRunSeed(context.Background(), NewState(5000, 4, 0, nil, AgentTotals{}), params, domain.Issue{ID: "1", Identifier: identifier}, log)

			if tt.want == nil && got != nil {
				t.Fatalf("freshRunSeed() = %v, want nil", got)
			}
			if len(got) != len(tt.want) {
				t.Errorf("freshRunSeed() holds %d keys, want %d", len(got), len(tt.want))
			}
			for _, kind := range reviewFamilyKinds {
				want, seeded := tt.want[kind]
				ids := slices.Sorted(slices.Values(continuationCommentIDs(kind, got)))
				if _, present := got[commentTemplateKey(kind)]; present != seeded || !slices.Equal(ids, want) {
					t.Errorf("freshRunSeed() %s comments = %v (present %v), want %v (present %v)", kind, ids, present, want, seeded)
				}
			}
			if tt.noIdentity && (scm.calls != 0 || scm.botCalls != 0) {
				t.Errorf("provider fetches = %d review, %d bot-review, want none without an identity", scm.calls, scm.botCalls)
			}
			const warning = "fresh run not given review comments"
			if got, want := strings.Count(buf.String(), `msg="`+warning+`"`), len(tt.wantWarnings); got != want {
				t.Errorf("%q logged %d times, want %d; log=%s", warning, got, want, buf.String())
			}
			for _, kind := range tt.wantWarnings {
				if !hasLogLine(buf.String(), `msg="`+warning+`"`, "reaction_kind="+kind) {
					t.Errorf("warning for %s missing; log=%s", kind, buf.String())
				}
			}
		})
	}
}

func TestFreshRun_PresentedCommentsSettleAtTheExitEntry(t *testing.T) {
	t.Parallel()

	for _, kind := range reviewFamilyKinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			c := newReactionCycle(t, kind)

			seed := c.freshRun(summaryComment("a"), inlineComment("b"))

			if got := slices.Sorted(slices.Values(continuationCommentIDs(kind, seed))); !slices.Equal(got, []string{"a", "b"}) {
				t.Fatalf("freshRunSeed() %s comments = %v, want [a b]", kind, got)
			}
			assertStoredHandedOff(t, c.store, c.issueID, kind, "a", "b")
			if _, ok := c.state.PendingReactions[c.rkey()]; !ok {
				t.Fatalf("PendingReactions[%s] missing, want the exit to seed the entry", c.rkey())
			}

			c.poll(inlineComment("b"))

			c.assertNoTurn(0)

			c.poll(inlineComment("b"), inlineComment("n"))

			c.assertDispatches(1, "b", "n")
			c.complete()
			c.state.ReactionAttempts[c.rkey()] = c.budget

			c.poll(inlineComment("b"), inlineComment("n"), inlineComment("z"))

			assertEscalations(t, c.tracker, 1)
			assertStoredHandedOff(t, c.store, c.issueID, kind, "a", "b", "n")
		})
	}
}

func TestReactionPass_ReplayOfTheReportedLog(t *testing.T) {
	t.Parallel()

	line := func(prefix string, n int) []domain.ReviewComment {
		comments := make([]domain.ReviewComment, n)
		for i := range comments {
			comments[i] = inlineComment(fmt.Sprintf("%s%d", prefix, i+1))
		}
		return comments
	}
	ids := func(comments []domain.ReviewComment) []string {
		out := make([]string, len(comments))
		for i, c := range comments {
			out[i] = c.ID
		}
		return out
	}
	join := func(groups ...[]domain.ReviewComment) []domain.ReviewComment {
		var all []domain.ReviewComment
		for _, g := range groups {
			all = append(all, g...)
		}
		return all
	}

	c := newReactionCycle(t, ReactionKindReview)
	turns := 1
	firstReview := join(line("first-", 3), []domain.ReviewComment{summaryComment("review-1")})
	remaining := line("first-", 3)[2:]
	secondLines := line("second-", 7)
	secondReview := join(remaining, secondLines, []domain.ReviewComment{summaryComment("review-2")})
	carried := join(remaining, []domain.ReviewComment{summaryComment("review-2")})
	thirdLines := line("third-", 4)

	c.freshRun(firstReview...)
	assertStoredHandedOff(t, c.store, c.issueID, ReactionKindReview, ids(firstReview)...)

	c.poll(remaining...)
	c.assertNoTurn(0)

	c.poll(secondReview...)
	c.assertDispatches(1, ids(secondReview)...)
	turns++
	c.complete()

	c.poll(carried...)
	c.assertNoTurn(1)

	c.poll(join(carried, thirdLines)...)
	c.assertDispatches(2, ids(join(carried, thirdLines))...)
	turns++
	c.complete()

	c.poll(carried...)
	c.assertNoTurn(2)

	if turns != 3 {
		t.Errorf("turns started = %d, want 3", turns)
	}
	if got := c.dispatchedChecks(); got != 2 {
		t.Errorf("turns counted against max_continuation_turns = %d, want 2", got)
	}
	assertEscalations(t, c.tracker, 0)
}

func TestSettleHandedOffCommentSet(t *testing.T) {
	t.Parallel()

	const (
		issueID    = "ISS-SETTLE"
		settledMsg = "comment set already handed off, not dispatching"
		heldMsg    = "comment set held by a running turn, not dispatching"
		loadMsg    = "failed to load handed-off comments, deferring"
		markMsg    = "failed to mark handed-off comment set dispatched"
	)
	tests := []struct {
		name          string
		kind          string
		rows          []string
		cached        []string
		reported      []string
		running       []string
		listErr       error
		markErr       error
		comments      []domain.ReviewComment
		want          bool
		wantMarks     int
		wantListCalls int
		wantMsg       string
	}{
		{name: "a load failure defers without marking", kind: ReactionKindReview, listErr: errors.New("unavailable"), comments: []domain.ReviewComment{summaryComment("a")}, want: true, wantListCalls: 1, wantMsg: loadMsg},
		{name: "rows covering the set settle and mark", kind: ReactionKindReview, rows: []string{"a", "b"}, comments: []domain.ReviewComment{summaryComment("a")}, want: true, wantMarks: 1, wantListCalls: 1, wantMsg: settledMsg},
		{name: "a cached key is not reloaded", kind: ReactionKindReview, cached: []string{"a"}, comments: []domain.ReviewComment{summaryComment("a")}, want: true, wantMarks: 1, wantMsg: settledMsg},
		{name: "reported IDs settle and mark", kind: ReactionKindBotReview, rows: []string{"a"}, reported: []string{"r"}, comments: []domain.ReviewComment{summaryComment("a"), summaryComment("r")}, want: true, wantMarks: 1, wantListCalls: 1, wantMsg: settledMsg},
		{name: "running IDs hold the set unmarked", kind: ReactionKindReview, running: []string{"a"}, comments: []domain.ReviewComment{summaryComment("a")}, want: true, wantListCalls: 1, wantMsg: heldMsg},
		{name: "a set covered partly by rows and partly by running IDs is held", kind: ReactionKindReview, rows: []string{"a"}, running: []string{"b"}, comments: []domain.ReviewComment{summaryComment("a"), summaryComment("b")}, want: true, wantListCalls: 1, wantMsg: heldMsg},
		{name: "a new comment does not settle", kind: ReactionKindReview, rows: []string{"a"}, comments: []domain.ReviewComment{summaryComment("a"), summaryComment("n")}, wantListCalls: 1},
		{name: "a failed mark still settles", kind: ReactionKindReview, rows: []string{"a"}, markErr: errors.New("write failed"), comments: []domain.ReviewComment{summaryComment("a")}, want: true, wantMarks: 1, wantListCalls: 1, wantMsg: markMsg},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, 0, nil, AgentTotals{})
			rkey := ReactionKey(issueID, tt.kind)
			store := newFingerprintModelStore()
			store.seedRows(issueID, tt.kind, tt.rows...)
			store.listErr = tt.listErr
			store.markDispatchedErr = tt.markErr
			if tt.cached != nil {
				seedHandedOff(state, rkey, tt.cached...)
			}
			if tt.reported != nil {
				state.ReactionReportedComments[rkey] = map[string]struct{}{}
				for _, id := range tt.reported {
					state.ReactionReportedComments[rkey][id] = struct{}{}
				}
			}
			if tt.running != nil {
				running := make([]domain.ReviewComment, len(tt.running))
				for i, id := range tt.running {
					running[i] = summaryComment(id)
				}
				state.Running[issueID] = &RunningEntry{ContinuationContext: map[string]any{commentTemplateKey(tt.kind): buildReviewTemplateMap(running)}}
			}
			log, buf := logCapture()

			got := settleHandedOffCommentSet(context.Background(), state, store, issueID, tt.kind, tt.comments, log)

			if got != tt.want {
				t.Errorf("settleHandedOffCommentSet(%v) = %v, want %v", tt.comments, got, tt.want)
			}
			if store.markDispatchedCalls != tt.wantMarks {
				t.Errorf("MarkReactionDispatched calls = %d, want %d", store.markDispatchedCalls, tt.wantMarks)
			}
			if store.listCalls != tt.wantListCalls {
				t.Errorf("ListReactionHandedOffComments calls = %d, want %d", store.listCalls, tt.wantListCalls)
			}
			if tt.wantMsg != "" && findLogLine(buf.String(), tt.wantMsg) == "" {
				t.Errorf("log output missing %q; log=%s", tt.wantMsg, buf.String())
			}
			if tt.listErr != nil {
				assertHandedOff(t, state, rkey)
			}
		})
	}
}
