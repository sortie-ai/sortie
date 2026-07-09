package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// labelReviewSCMFake is a controllable domain.SCMAdapter for label-review
// reconcile tests. ListLabelEvents and RemoveLabel are the only methods
// this reconcile pass exercises; every other method is a plain stub.
type labelReviewSCMFake struct {
	events  []domain.LabelEvent
	listErr error

	removeErr error

	listCalls    int
	removeCalls  int
	removedLabel []string
}

var _ domain.SCMAdapter = (*labelReviewSCMFake)(nil)

func (f *labelReviewSCMFake) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.events, nil
}

func (f *labelReviewSCMFake) RemoveLabel(_ context.Context, _ int, _, _, label string) error {
	f.removeCalls++
	f.removedLabel = append(f.removedLabel, label)
	return f.removeErr
}

func (f *labelReviewSCMFake) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}

func (f *labelReviewSCMFake) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return nil, nil
}

func (f *labelReviewSCMFake) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}

func (f *labelReviewSCMFake) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}

func (f *labelReviewSCMFake) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{}, nil
}

func (f *labelReviewSCMFake) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}

func (f *labelReviewSCMFake) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

// labelReviewSCMPanicOnOthers panics if any SCMAdapter method other than
// ListLabelEvents or RemoveLabel is called. Used to assert that label-review
// detection reads only the journal substrate, never a label-snapshot method
// (domain.SCMAdapter exposes no such method, but a future accidental import
// of one would be caught here).
type labelReviewSCMPanicOnOthers struct {
	events []domain.LabelEvent
}

var _ domain.SCMAdapter = (*labelReviewSCMPanicOnOthers)(nil)

func (p *labelReviewSCMPanicOnOthers) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return p.events, nil
}

func (p *labelReviewSCMPanicOnOthers) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

func (p *labelReviewSCMPanicOnOthers) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	panic("FetchPendingReviews must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	panic("FetchBotReviewComments must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	panic("GetReviewDecision must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	panic("GetCIStatus must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	panic("GetMergeability must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	panic("MergePR must not be called by the label-review reconcile")
}

func (p *labelReviewSCMPanicOnOthers) DeleteBranch(_ context.Context, _, _, _ string) error {
	panic("DeleteBranch must not be called by the label-review reconcile")
}

// labelReviewFingerprintStore is a genuinely-stateful ReconcileStore keyed
// by issue+kind, so restart-simulation and repeat-after-completion tests
// read back a mark upserted by a prior reconcile call rather than a canned
// value. Only the fingerprint methods carry behavior; the rest satisfy the
// interface.
type labelReviewFingerprintStore struct {
	marks  map[string]string
	getErr error

	upsertCalls int
	getCalls    int
}

var _ ReconcileStore = (*labelReviewFingerprintStore)(nil)

func newLabelReviewFingerprintStore() *labelReviewFingerprintStore {
	return &labelReviewFingerprintStore{marks: make(map[string]string)}
}

func (s *labelReviewFingerprintStore) UpsertReactionFingerprint(_ context.Context, issueID, kind, mark string) error {
	s.upsertCalls++
	s.marks[issueID+":"+kind] = mark
	return nil
}

func (s *labelReviewFingerprintStore) GetReactionFingerprint(_ context.Context, issueID, kind string) (string, bool, error) {
	s.getCalls++
	if s.getErr != nil {
		return "", false, s.getErr
	}
	return s.marks[issueID+":"+kind], false, nil
}

func (s *labelReviewFingerprintStore) SaveRetryEntry(_ context.Context, _ persistence.RetryEntry) error {
	return nil
}

func (s *labelReviewFingerprintStore) DeleteRetryEntry(_ context.Context, _ string) error {
	return nil
}

func (s *labelReviewFingerprintStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (s *labelReviewFingerprintStore) DeleteReactionFingerprintsByIssue(_ context.Context, _ string) error {
	return nil
}

func (s *labelReviewFingerprintStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	return nil
}

func (s *labelReviewFingerprintStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	return nil
}

// --- Test helpers ---

// labelReviewBaseTime is a fixed reference time for label-review reconcile
// tests.
var labelReviewBaseTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// labelEvent builds a domain.LabelEvent for test fixtures.
func labelEvent(id, label, actor string, added bool, at time.Time) domain.LabelEvent {
	return domain.LabelEvent{ID: id, Label: label, Actor: actor, Added: added, At: at}
}

// newLabelReviewPending builds a PendingReaction with Kind=ReactionKindLabelReview.
func newLabelReviewPending(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindLabelReview,
		CreatedAt:  labelReviewBaseTime,
		AgentKind:  "mock",
		RuleName:   "default",
		TemplateID: "tmpl-1",
		KindData: &LabelReviewReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
		},
	}
}

// stateWithLabelReviewPending creates a State with one label-review
// PendingReaction entry.
func stateWithLabelReviewPending(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	s.PendingReactions[rkey] = newLabelReviewPending(issueID, prNumber)
	return s
}

// defaultLabelReviewConfig returns a LabelReviewReactionConfig matching the
// config layer's own defaults.
func defaultLabelReviewConfig() LabelReviewReactionConfig {
	return LabelReviewReactionConfig{
		Provider:       "github",
		ReviewLabel:    "sortie:review",
		PollIntervalMS: 60000,
	}
}

// labelReviewParams returns ReconcileParams wired for label-review reconcile
// unit tests, with NowFunc fixed at labelReviewBaseTime.
func labelReviewParams(store ReconcileStore, scm domain.SCMAdapter) ReconcileParams {
	return ReconcileParams{
		SCMAdapter:                    scm,
		LabelReviewConfig:             defaultLabelReviewConfig(),
		LabelReviewReactionConfigured: true,
		Store:                         store,
		OnRetryFire:                   noopRetryFire,
		Ctx:                           context.Background(),
		Logger:                        discardLogger(),
		NowFunc:                       func() time.Time { return labelReviewBaseTime },
	}
}

// --- reconcileLabelReviewCommands guard tests ---

// TestReconcileLabelReviewCommands_Disabled covers V3: a nil SCM adapter or
// an unconfigured feature returns immediately with zero ListLabelEvents
// calls, before any journal read.
func TestReconcileLabelReviewCommands_Disabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		nilAdapter bool
		configured bool
	}{
		{name: "nil SCM adapter", nilAdapter: true, configured: true},
		{name: "feature not configured", nilAdapter: false, configured: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "LR-DIS"
			state := stateWithLabelReviewPending(t, issueID, 10)
			rkey := ReactionKey(issueID, ReactionKindLabelReview)
			scm := &labelReviewSCMFake{}
			store := newLabelReviewFingerprintStore()
			params := labelReviewParams(store, scm)
			if tt.nilAdapter {
				params.SCMAdapter = nil
			}
			params.LabelReviewReactionConfigured = tt.configured

			reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

			if scm.listCalls != 0 {
				t.Errorf("ListLabelEvents calls = %d, want 0", scm.listCalls)
			}
			if _, ok := state.PendingReactions[rkey]; !ok {
				t.Error("PendingReactions entry removed while disabled; want untouched")
			}
		})
	}
}

// --- reconcileLabelReviewCommands dispatch tests ---

// TestReconcileLabelReviewCommands_Dispatch covers V1: one matching labeled
// event confirmed with the label still present produces exactly one
// ScheduleRetry call carrying the label_review continuation context, the
// acting user, and the frozen dispatch fields.
func TestReconcileLabelReviewCommands_Dispatch(t *testing.T) {
	t.Parallel()

	const issueID = "LR-D1"
	state := stateWithLabelReviewPending(t, issueID, 42)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled after a confirmed labeled event; want scheduled")
	}
	if retry.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindLabelReview)
	}
	if retry.SessionID != "" {
		t.Errorf("RetryEntry.SessionID = %q, want empty (fresh session, not a resume)", retry.SessionID)
	}
	if retry.AgentKind != "mock" || retry.RuleName != "default" || retry.TemplateID != "tmpl-1" {
		t.Errorf("RetryEntry frozen dispatch fields = (%q, %q, %q), want (mock, default, tmpl-1)",
			retry.AgentKind, retry.RuleName, retry.TemplateID)
	}

	raw, ok := retry.ContinuationContext["label_review"]
	if !ok {
		t.Fatal(`ContinuationContext missing "label_review" key`)
	}
	lrMap, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("label_review context type = %T, want map[string]any", raw)
	}
	if lrMap["pr_number"] != 42 {
		t.Errorf("label_review[pr_number] = %v, want 42", lrMap["pr_number"])
	}
	if lrMap["owner"] != "owner" {
		t.Errorf("label_review[owner] = %v, want %q", lrMap["owner"], "owner")
	}
	if lrMap["repo"] != "repo" {
		t.Errorf("label_review[repo] = %v, want %q", lrMap["repo"], "repo")
	}
	if lrMap["actor"] != "alice" {
		t.Errorf("label_review[actor] = %v, want %q", lrMap["actor"], "alice")
	}

	// The reconcile itself re-enqueues the entry: repeatability does not
	// depend on a second call or worker-exit seeding.
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry removed after dispatch; want re-enqueued")
	}
	if scm.listCalls != 1 {
		t.Errorf("ListLabelEvents calls = %d, want 1", scm.listCalls)
	}

	data, ok := state.PendingReactions[rkey].KindData.(*LabelReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelReviewReactionData", state.PendingReactions[rkey].KindData)
	}
	if data.LastActor != "alice" {
		t.Errorf("LabelReviewReactionData.LastActor = %q, want %q", data.LastActor, "alice")
	}
	if data.HighWaterMark == "" {
		t.Error("LabelReviewReactionData.HighWaterMark is empty, want advanced")
	}
}

// TestReconcileLabelReviewCommands_LabelAbsentAtDetection covers V4 and the
// retraction rule: a matching labeled event exists past the stored mark,
// but a later unlabeled event in the same batch means the label is not
// present at detection time. No dispatch fires, but the mark still advances
// to the newest examined event.
func TestReconcileLabelReviewCommands_LabelAbsentAtDetection(t *testing.T) {
	t.Parallel()

	const issueID = "LR-RETRACT"
	state := stateWithLabelReviewPending(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	events := []domain.LabelEvent{
		labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-2*time.Minute)),
		labelEvent("2", "sortie:review", "alice", false, labelReviewBaseTime.Add(-1*time.Minute)),
	}
	scm := &labelReviewSCMFake{events: events}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled despite the label being absent at detection time; want none")
	}
	mark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	wantMark := labelReviewMark(events[1])
	if mark != wantMark {
		t.Errorf("stored mark = %q, want %q (advanced past the newest examined event)", mark, wantMark)
	}
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry removed after retraction; want re-enqueued")
	}
}

// TestReconcileLabelReviewCommands_BurstCollapse verifies that multiple
// matching labeled events in one batch collapse to exactly one scheduled
// retry, carrying the latest match.
func TestReconcileLabelReviewCommands_BurstCollapse(t *testing.T) {
	t.Parallel()

	const issueID = "LR-BURST"
	state := stateWithLabelReviewPending(t, issueID, 10)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-3*time.Minute)),
			labelEvent("2", "sortie:review", "bob", true, labelReviewBaseTime.Add(-2*time.Minute)),
			labelEvent("3", "sortie:review", "carol", true, labelReviewBaseTime.Add(-1*time.Minute)),
			// A foreign label newer than every match: it advances the
			// examined-events window but must not become the dispatch actor.
			labelEvent("4", "bug", "dave", true, labelReviewBaseTime.Add(-30*time.Second)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(state.RetryAttempts) != 1 {
		t.Fatalf("RetryAttempts count = %d, want 1 (burst collapses to one command)", len(state.RetryAttempts))
	}
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatalf("retry not scheduled for %s", issueID)
	}
	lrMap, ok := retry.ContinuationContext["label_review"].(map[string]any)
	if !ok {
		t.Fatalf("label_review context type = %T, want map[string]any", retry.ContinuationContext["label_review"])
	}
	if lrMap["actor"] != "carol" {
		t.Errorf("label_review[actor] = %v, want %q (the latest matching event's actor, ignoring the newer foreign label)", lrMap["actor"], "carol")
	}
}

// TestReconcileLabelReviewCommands_DepthOneWhileRunning covers V2 and the
// depth-one invariant: a matching event arrives while a label-review
// session is already running for the issue. No second dispatch or pending
// entry is created.
func TestReconcileLabelReviewCommands_DepthOneWhileRunning(t *testing.T) {
	t.Parallel()

	const issueID = "LR-RUN"
	state := stateWithLabelReviewPending(t, issueID, 10)
	state.Running[issueID] = &RunningEntry{Identifier: issueID + "-ident"}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled while a label-review session is already running; want depth-one collapse")
	}
	// The mark still advances even though nothing dispatches (falls into
	// the no-dispatch branch).
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if mark == "" {
		t.Error("stored mark is empty; want advanced even when collapsed into the running session")
	}
}

// TestReconcileLabelReviewCommands_DepthOneWhileQueued covers the depth-one
// invariant for the queued (not yet running) case: a second matching event
// arrives while state.RetryAttempts already holds a label-review-kind entry
// for the issue. The existing entry is left intact.
func TestReconcileLabelReviewCommands_DepthOneWhileQueued(t *testing.T) {
	t.Parallel()

	const issueID = "LR-QUEUED"
	state := stateWithLabelReviewPending(t, issueID, 10)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		ReactionKind: ReactionKindLabelReview,
	}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(state.RetryAttempts) != 1 {
		t.Fatalf("RetryAttempts count = %d, want 1 (no second entry)", len(state.RetryAttempts))
	}
	retry := state.RetryAttempts[issueID]
	if retry.ContinuationContext != nil {
		t.Error("existing queued retry was replaced by a second dispatch; want collapsed (no ContinuationContext)")
	}
}

// TestReconcileLabelReviewCommands_RepeatAfterCompletion covers A10: after a
// first dispatch completes (the session exits, clearing state.RetryAttempts
// for the issue) and the label is re-applied, a second dispatch fires on a
// later tick driven entirely by the reconcile's own re-enqueue.
func TestReconcileLabelReviewCommands_RepeatAfterCompletion(t *testing.T) {
	t.Parallel()

	const issueID = "LR-REPEAT"
	state := stateWithLabelReviewPending(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-2*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Fatal("first dispatch did not fire; want scheduled")
	}

	// Simulate the dispatched session completing: HandleWorkerExit clears
	// the retry entry on worker exit. This reconcile pass never performs
	// that clearing itself.
	CancelRetry(state, issueID)

	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Fatal("PendingReactions entry missing after the first dispatch; want re-enqueued regardless of dispatch")
	}
	// Advance past the poll throttle and add a second, newer labeled event
	// (the label re-applied after the review completed).
	pending := state.PendingReactions[rkey]
	pending.PendingRetryAt = time.Time{}
	state.PendingReactions[rkey] = pending
	scm.events = append(scm.events, labelEvent("2", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)))

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("second dispatch did not fire after re-applying the label; want scheduled")
	}
	if scm.listCalls != 2 {
		t.Errorf("ListLabelEvents calls = %d, want 2 (one per tick)", scm.listCalls)
	}
}

// TestReconcileLabelReviewCommands_AtMostOnceAcrossRestart verifies the
// at-most-once guarantee across a restart: the fingerprint is upserted
// before ScheduleRetry fires. A fresh reconcile call over a restored store
// (the mark it returns was upserted by the prior process) must not
// re-dispatch the same journal event.
func TestReconcileLabelReviewCommands_AtMostOnceAcrossRestart(t *testing.T) {
	t.Parallel()

	const issueID = "LR-RESTART"
	event := labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute))
	store := newLabelReviewFingerprintStore()

	state1 := stateWithLabelReviewPending(t, issueID, 10)
	scm1 := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	params1 := labelReviewParams(store, scm1)

	reconcileLabelReviewCommands(state1, params1, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state1.RetryAttempts[issueID]; !ok {
		t.Fatal("first dispatch did not fire; want scheduled")
	}
	persistedMark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if err != nil || persistedMark == "" {
		t.Fatalf("GetReactionFingerprint after dispatch = (%q, %v), want a non-empty persisted mark", persistedMark, err)
	}
	if persistedMark != labelReviewMark(event) {
		t.Fatalf("persisted mark = %q, want %q", persistedMark, labelReviewMark(event))
	}

	// Restart: a fresh state and reconcile call over the SAME store, whose
	// GetReactionFingerprint now returns the mark persisted above.
	state2 := stateWithLabelReviewPending(t, issueID, 10)
	scm2 := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	params2 := labelReviewParams(store, scm2)

	reconcileLabelReviewCommands(state2, params2, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state2.RetryAttempts[issueID]; dispatched {
		t.Error("second process re-dispatched the same journal event; want no dispatch (mark already advanced past it)")
	}
}

// TestReconcileLabelReviewCommands_AcknowledgmentBestEffort verifies the
// best-effort acknowledgment contract: after a confirmed dispatch,
// RemoveLabel is called with the configured review label; a failure logs a
// Warn and leaves dedup and the dispatch unaffected.
func TestReconcileLabelReviewCommands_AcknowledgmentBestEffort(t *testing.T) {
	t.Parallel()

	const issueID = "LR-ACK"
	state := stateWithLabelReviewPending(t, issueID, 10)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
		removeErr: errors.New("insufficient scope"),
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reconcileLabelReviewCommands(state, params, log, context.Background(), &domain.NoopMetrics{})

	if scm.removeCalls != 1 {
		t.Fatalf("RemoveLabel calls = %d, want 1", scm.removeCalls)
	}
	if scm.removedLabel[0] != "sortie:review" {
		t.Errorf("RemoveLabel label = %q, want %q", scm.removedLabel[0], "sortie:review")
	}
	if !strings.Contains(buf.String(), "label-review label removal failed") {
		t.Errorf("log output = %q, want to contain the label-removal-failure warning", buf.String())
	}
	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("dispatch did not fire despite the RemoveLabel failure; want unaffected")
	}
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if mark == "" {
		t.Error("stored mark is empty; want advanced regardless of the RemoveLabel failure")
	}
}

// TestReconcileLabelReviewCommands_SelfAuthoredEventsExcluded documents that,
// absent an orchestrator identity source in this slice, the self-authored
// exclusion is a no-op: an event authored by any actor, including a
// plausible bot identity, still confirms the command.
func TestReconcileLabelReviewCommands_SelfAuthoredEventsExcluded(t *testing.T) {
	t.Parallel()

	const issueID = "LR-SELF"
	state := stateWithLabelReviewPending(t, issueID, 10)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "sortie-bot", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("dispatch did not fire for a bot-authored event; want the no-op filter to allow it through")
	}
	lrMap, ok := retry.ContinuationContext["label_review"].(map[string]any)
	if !ok {
		t.Fatalf("label_review context type = %T, want map[string]any", retry.ContinuationContext["label_review"])
	}
	if lrMap["actor"] != "sortie-bot" {
		t.Errorf("label_review[actor] = %v, want %q", lrMap["actor"], "sortie-bot")
	}
}

// TestReconcileLabelReviewCommands_JournalReadError verifies that a
// ListLabelEvents error increments the per-entry backoff, re-enqueues the
// entry, and does not advance the mark.
func TestReconcileLabelReviewCommands_JournalReadError(t *testing.T) {
	t.Parallel()

	const issueID = "LR-ERR"
	state := stateWithLabelReviewPending(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	scm := &labelReviewSCMFake{listErr: errors.New("connection reset")}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a journal read error; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1", entry.PendingAttempts)
	}
	if !entry.PendingRetryAt.After(labelReviewBaseTime) {
		t.Error("PendingRetryAt not advanced after a journal read error; want backoff applied")
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 0 (mark must not advance on a read error)", store.upsertCalls)
	}
	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled despite the journal read error; want none")
	}
}

// TestReconcileLabelReviewCommands_FingerprintReadError verifies that a
// fingerprint read failure backs off without dispatching. At-most-once
// rests solely on the mark, and this kind has no turn cap, so a command
// must never dispatch when the stored mark cannot be read.
func TestReconcileLabelReviewCommands_FingerprintReadError(t *testing.T) {
	t.Parallel()

	const issueID = "LR-FPE"
	state := stateWithLabelReviewPending(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	scm := &labelReviewSCMFake{}
	store := newLabelReviewFingerprintStore()
	store.getErr = errors.New("sqlite is locked")
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a fingerprint read error; want re-enqueued")
	}
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1", entry.PendingAttempts)
	}
	if !entry.PendingRetryAt.After(labelReviewBaseTime) {
		t.Error("PendingRetryAt not advanced after a fingerprint read error; want backoff applied")
	}
	if scm.listCalls != 0 {
		t.Errorf("ListLabelEvents calls = %d, want 0 (no journal read when the mark is unreadable)", scm.listCalls)
	}
	if store.upsertCalls != 0 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 0", store.upsertCalls)
	}
	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled despite the fingerprint read error; want none")
	}
}

// TestReconcileLabelReviewCommands_NoTTLDrop verifies that a label-review
// entry has no age-based drop branch: an entry whose CreatedAt is far older
// than every sibling kind's TTL is still processed normally on its next due
// tick.
func TestReconcileLabelReviewCommands_NoTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "LR-OLD"
	state := stateWithLabelReviewPending(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindLabelReview)
	state.PendingReactions[rkey].CreatedAt = labelReviewBaseTime.Add(-365 * 24 * time.Hour)

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; !dispatched {
		t.Error("year-old entry was not processed; want no TTL-drop branch for label-review")
	}
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry dropped instead of re-enqueued; want no age-based drop")
	}
}

// TestReconcileLabelReviewCommands_CrossKindIsolation covers A6: with ci,
// review, bot-review, merge, and merge-conflict entries all present for one
// issue, a label-review dispatch mutates none of them.
func TestReconcileLabelReviewCommands_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	const issueID = "LR-ISO"
	state := stateWithLabelReviewPending(t, issueID, 10)

	ciEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindCI, KindData: &CIReactionData{Branch: "main"}}
	reviewEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindReview, KindData: &ReviewReactionData{PRNumber: 10}}
	botReviewEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindBotReview, KindData: &BotReviewReactionData{PRNumber: 10}}
	mergeEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindAutoMerge, KindData: &AutoMergeReactionData{PRNumber: 10}}
	mergeConflictEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindMergeConflict, KindData: &MergeConflictReactionData{PRNumber: 10}}

	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = ciEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindReview)] = reviewEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindBotReview)] = botReviewEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)] = mergeEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindMergeConflict)] = mergeConflictEntry

	store := newLabelReviewFingerprintStore()
	if err := store.UpsertReactionFingerprint(context.Background(), issueID, ReactionKindReview, "sibling-fingerprint"); err != nil {
		t.Fatalf("seed sibling fingerprint: %v", err)
	}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Fatal("label-review dispatch did not fire")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; got != ciEntry {
		t.Error("CI PendingReaction entry mutated by the label-review reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindReview)]; got != reviewEntry {
		t.Error("review PendingReaction entry mutated by the label-review reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindBotReview)]; got != botReviewEntry {
		t.Error("bot-review PendingReaction entry mutated by the label-review reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)]; got != mergeEntry {
		t.Error("auto-merge PendingReaction entry mutated by the label-review reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeConflict)]; got != mergeConflictEntry {
		t.Error("merge-conflict PendingReaction entry mutated by the label-review reconcile")
	}
	siblingMark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindReview)
	if err != nil {
		t.Fatalf("GetReactionFingerprint(review): %v", err)
	}
	if siblingMark != "sibling-fingerprint" {
		t.Errorf("review fingerprint = %q, want unchanged %q", siblingMark, "sibling-fingerprint")
	}
}

// TestReconcileLabelReviewCommands_DispatchLogsActorAndPRNumber covers A11
// and the first half of A12: a confirmed dispatch emits an Info log
// carrying the PR number and the acting user, unconditionally.
func TestReconcileLabelReviewCommands_DispatchLogsActorAndPRNumber(t *testing.T) {
	t.Parallel()

	const issueID = "LR-LOG"
	state := stateWithLabelReviewPending(t, issueID, 77)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reconcileLabelReviewCommands(state, params, log, context.Background(), &domain.NoopMetrics{})

	out := buf.String()
	if !strings.Contains(out, "label-review dispatched") {
		t.Fatalf("log output = %q, want to contain the dispatch message", out)
	}
	if !strings.Contains(out, "pr_number=77") {
		t.Errorf("log output = %q, want pr_number=77 attribute", out)
	}
	if !strings.Contains(out, "actor=alice") {
		t.Errorf("log output = %q, want actor=alice attribute", out)
	}
}

// TestReconcileLabelReviewCommands_JournalSubstrateNotSnapshot covers V5:
// detection uses only ListLabelEvents (the journal substrate); no other
// SCMAdapter method is ever called for label-review detection or dispatch.
func TestReconcileLabelReviewCommands_JournalSubstrateNotSnapshot(t *testing.T) {
	t.Parallel()

	const issueID = "LR-JOURNAL"
	state := stateWithLabelReviewPending(t, issueID, 10)
	scm := &labelReviewSCMPanicOnOthers{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:review", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelReviewParams(store, scm)

	reconcileLabelReviewCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("dispatch did not fire; want scheduled using only ListLabelEvents/RemoveLabel")
	}
}

// --- labelReviewMark tests ---

func TestLabelReviewMark_FixedWidthAndOrdering(t *testing.T) {
	t.Parallel()

	earlier := domain.LabelEvent{ID: "1", At: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}
	later := domain.LabelEvent{ID: "2", At: time.Date(2026, 7, 9, 12, 0, 0, 100_000_000, time.UTC)} // +0.1s

	gotEarlier := labelReviewMark(earlier)
	gotLater := labelReviewMark(later)

	wantEarlier := "2026-07-09T12:00:00.000000000Z|1"
	if gotEarlier != wantEarlier {
		t.Errorf("labelReviewMark(%v) = %q, want %q", earlier, gotEarlier, wantEarlier)
	}
	if gotLater <= gotEarlier {
		t.Errorf("labelReviewMark ordering: %q is not lexically after %q, want strictly after", gotLater, gotEarlier)
	}
}

func TestLabelReviewMark_NonUTCNormalizedBeforeCompare(t *testing.T) {
	t.Parallel()

	loc := time.FixedZone("UTC+2", 2*60*60)
	e := domain.LabelEvent{ID: "1", At: time.Date(2026, 7, 9, 14, 0, 0, 0, loc)} // 12:00 UTC

	got := labelReviewMark(e)
	want := "2026-07-09T12:00:00.000000000Z|1"
	if got != want {
		t.Errorf("labelReviewMark(non-UTC input) = %q, want %q (normalized to UTC)", got, want)
	}
}

// --- buildLabelReviewMap tests ---

func TestBuildLabelReviewMap_FieldMapping(t *testing.T) {
	t.Parallel()

	data := &LabelReviewReactionData{PRNumber: 99, Owner: "acme", Repo: "widgets"}
	requestedAt := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	got := buildLabelReviewMap(data, "alice", requestedAt)

	want := map[string]any{
		"pr_number":    99,
		"owner":        "acme",
		"repo":         "widgets",
		"actor":        "alice",
		"requested_at": "2026-07-09T12:30:00Z",
	}
	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("buildLabelReviewMap missing key %q", k)
			continue
		}
		if gotV != wantV {
			t.Errorf("buildLabelReviewMap[%q] = %v, want %v", k, gotV, wantV)
		}
	}
	if len(got) != len(want) {
		t.Errorf("buildLabelReviewMap has %d keys, want %d", len(got), len(want))
	}
}
