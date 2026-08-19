package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// labelFixDispatchedFlagStore returns a fixed mark and a hardcoded
// dispatched flag from GetReactionFingerprint, proving
// reconcileLabelFixCommands never reads the flag: a fresh event past the
// stored mark still dispatches regardless of its value.
type labelFixDispatchedFlagStore struct {
	unsupportedReactionObservationStore

	mark       string
	dispatched bool

	upsertCalls int
}

var _ ReconcileStore = (*labelFixDispatchedFlagStore)(nil)

func (s *labelFixDispatchedFlagStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	return s.mark, s.dispatched, nil
}

func (s *labelFixDispatchedFlagStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	s.upsertCalls++
	return nil
}

func (s *labelFixDispatchedFlagStore) SaveRetryEntry(_ context.Context, _ persistence.RetryEntry) error {
	return nil
}

func (s *labelFixDispatchedFlagStore) DeleteRetryEntry(_ context.Context, _ string) error {
	return nil
}

func (s *labelFixDispatchedFlagStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (s *labelFixDispatchedFlagStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	return nil
}

func (s *labelFixDispatchedFlagStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	return nil
}

// --- Test helpers ---
//
// These reuse the generic label-event and SCM/fingerprint-store test
// doubles already declared in label_review_reconcile_test.go
// (labelEvent, labelReviewBaseTime, labelReviewSCMFake,
// labelReviewSCMPanicOnOthers, labelReviewFingerprintStore,
// newLabelReviewFingerprintStore): none of that behavior is
// label-review-specific, so duplicating it per reaction kind would be
// pure repetition.

// newLabelFixPending builds a PendingReaction with Kind=ReactionKindLabelFix.
func newLabelFixPending(issueID string, prNumber int, branch string) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindLabelFix,
		CreatedAt:  labelReviewBaseTime,
		AgentKind:  "mock",
		RuleName:   "default",
		TemplateID: "tmpl-1",
		KindData: &LabelFixReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   branch,
		},
	}
}

// stateWithLabelFixPending creates a State with one label-fix
// PendingReaction entry.
func stateWithLabelFixPending(t *testing.T, issueID string, prNumber int, branch string) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	s.PendingReactions[rkey] = newLabelFixPending(issueID, prNumber, branch)
	return s
}

// defaultLabelFixConfig returns a LabelFixReactionConfig matching the
// config layer's own defaults.
func defaultLabelFixConfig() LabelFixReactionConfig {
	return LabelFixReactionConfig{
		Provider:       "github",
		FixLabel:       "sortie:fix",
		PollIntervalMS: 60000,
	}
}

// labelFixParams returns ReconcileParams wired for label-fix reconcile unit
// tests, with NowFunc fixed at labelReviewBaseTime.
func labelFixParams(store ReconcileStore, scm domain.SCMAdapter) ReconcileParams {
	return ReconcileParams{
		SCMAdapter:                 scm,
		LabelFixConfig:             defaultLabelFixConfig(),
		LabelFixReactionConfigured: true,
		Store:                      store,
		OnRetryFire:                noopRetryFire,
		Ctx:                        context.Background(),
		Logger:                     discardLogger(),
		NowFunc:                    func() time.Time { return labelReviewBaseTime },
	}
}

// --- reconcileLabelFixCommands guard tests ---

// TestReconcileLabelFixCommands_Disabled covers V6: a nil SCM adapter or an
// unconfigured feature returns immediately with zero ListLabelEvents calls,
// before any journal read.
func TestReconcileLabelFixCommands_Disabled(t *testing.T) {
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

			const issueID = "LF-DIS"
			state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-dis")
			rkey := ReactionKey(issueID, ReactionKindLabelFix)
			scm := &labelReviewSCMFake{}
			store := newLabelReviewFingerprintStore()
			params := labelFixParams(store, scm)
			if tt.nilAdapter {
				params.SCMAdapter = nil
			}
			params.LabelFixReactionConfigured = tt.configured

			reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

			if scm.listCalls != 0 {
				t.Errorf("ListLabelEvents calls = %d, want 0", scm.listCalls)
			}
			if _, ok := state.PendingReactions[rkey]; !ok {
				t.Error("PendingReactions entry removed while disabled; want untouched")
			}
		})
	}
}

// --- reconcileLabelFixCommands dispatch tests ---

// TestReconcileLabelFixCommands_Dispatch covers V1, V2, and A13: one
// matching labeled event confirmed with the label still present produces
// exactly one ScheduleRetry call carrying the label_fix continuation
// context (including branch), the acting user, the frozen dispatch fields,
// and a high-water mark in the shared mark format.
func TestReconcileLabelFixCommands_Dispatch(t *testing.T) {
	t.Parallel()

	const issueID = "LF-D1"
	state := stateWithLabelFixPending(t, issueID, 42, "feature/lf-42")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	event := labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute))
	scm := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled after a confirmed labeled event; want scheduled")
	}
	if retry.ReactionKind != ReactionKindLabelFix {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindLabelFix)
	}
	if retry.SessionID != "" {
		t.Errorf("RetryEntry.SessionID = %q, want empty (fresh session, not a resume)", retry.SessionID)
	}
	if retry.AgentKind != "mock" || retry.RuleName != "default" || retry.TemplateID != "tmpl-1" {
		t.Errorf("RetryEntry frozen dispatch fields = (%q, %q, %q), want (mock, default, tmpl-1)",
			retry.AgentKind, retry.RuleName, retry.TemplateID)
	}

	raw, ok := retry.ContinuationContext["label_fix"]
	if !ok {
		t.Fatal(`ContinuationContext missing "label_fix" key`)
	}
	lfMap, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("label_fix context type = %T, want map[string]any", raw)
	}
	if lfMap["pr_number"] != 42 {
		t.Errorf("label_fix[pr_number] = %v, want 42", lfMap["pr_number"])
	}
	if lfMap["owner"] != "owner" {
		t.Errorf("label_fix[owner] = %v, want %q", lfMap["owner"], "owner")
	}
	if lfMap["repo"] != "repo" {
		t.Errorf("label_fix[repo] = %v, want %q", lfMap["repo"], "repo")
	}
	if lfMap["branch"] != "feature/lf-42" {
		t.Errorf("label_fix[branch] = %v, want %q", lfMap["branch"], "feature/lf-42")
	}
	if lfMap["actor"] != "alice" {
		t.Errorf("label_fix[actor] = %v, want %q", lfMap["actor"], "alice")
	}

	// The reconcile itself re-enqueues the entry: repeatability does not
	// depend on a second call or worker-exit seeding.
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry removed after dispatch; want re-enqueued")
	}
	if scm.listCalls != 1 {
		t.Errorf("ListLabelEvents calls = %d, want 1", scm.listCalls)
	}

	data, ok := state.PendingReactions[rkey].KindData.(*LabelFixReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelFixReactionData", state.PendingReactions[rkey].KindData)
	}
	if data.LastActor != "alice" {
		t.Errorf("LabelFixReactionData.LastActor = %q, want %q", data.LastActor, "alice")
	}
	if data.HighWaterMark != labelReviewMark(event) {
		t.Errorf("LabelFixReactionData.HighWaterMark = %q, want %q (reuses the shared mark format)",
			data.HighWaterMark, labelReviewMark(event))
	}
}

// TestReconcileLabelFixCommands_ContinuesAfterAgeRemoval covers R31: after
// the periodic sweep removes a workspace by age, the label-fix reconcile
// pass still observes a matching label event and still schedules a retry
// carrying ReactionKindLabelFix, re-enqueuing the pending entry. The
// reconcile pass reads nothing from the workspace directory (R27), so its
// removal has no bearing on detection.
//
// The recreate-through-dispatch property (workspace.Ensure/Prepare
// returning CreatedNow == true so the read-only and read-write postures
// each obtain a workspace directory again) is covered by
// internal/workspace/workspace_test.go:279 (Ensure) and
// internal/workspace/lifecycle_test.go:165 (Prepare re-running
// after_create), not by this test.
func TestReconcileLabelFixCommands_ContinuesAfterAgeRemoval(t *testing.T) {
	t.Parallel()

	const issueID = "LF-AGE1"
	identifier := issueID + "-ident"
	tmpDir := t.TempDir()
	wsPath := filepath.Join(tmpDir, identifier)
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	state := stateWithLabelFixPending(t, issueID, 42, "feature/lf-age1")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)

	sweepParams := defaultSweepParams(t, tmpDir, &sweepTracker{})
	sweepParams.RetentionDays = config.WorkspaceRetentionMinDays
	sweepParams.Store = &sweepStoreDouble{}
	SweepWorkspaces(state, sweepParams)
	assertSweepDirRemoved(t, wsPath)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry removed by the sweep; want unaffected (test precondition)")
	}

	event := labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute))
	scm := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled after age removal; want scheduled")
	}
	if retry.ReactionKind != ReactionKindLabelFix {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindLabelFix)
	}
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry removed after dispatch; want re-enqueued")
	}
}

// TestReconcileLabelFixCommands_LabelAbsentAtDetection covers the
// retraction rule: a matching labeled event exists past the stored mark,
// but a later unlabeled event in the same batch means the label is not
// present at detection time. No dispatch fires, but the mark still
// advances to the newest examined event.
func TestReconcileLabelFixCommands_LabelAbsentAtDetection(t *testing.T) {
	t.Parallel()

	const issueID = "LF-RETRACT"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-retract")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	events := []domain.LabelEvent{
		labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-2*time.Minute)),
		labelEvent("2", "sortie:fix", "alice", false, labelReviewBaseTime.Add(-1*time.Minute)),
	}
	scm := &labelReviewSCMFake{events: events}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled despite the label being absent at detection time; want none")
	}
	mark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
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

// TestReconcileLabelFixCommands_BurstCollapse covers V3: multiple matching
// labeled events in one batch collapse to exactly one scheduled retry,
// carrying the latest match.
func TestReconcileLabelFixCommands_BurstCollapse(t *testing.T) {
	t.Parallel()

	const issueID = "LF-BURST"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-burst")
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-3*time.Minute)),
			labelEvent("2", "sortie:fix", "bob", true, labelReviewBaseTime.Add(-2*time.Minute)),
			labelEvent("3", "sortie:fix", "carol", true, labelReviewBaseTime.Add(-1*time.Minute)),
			// A foreign label newer than every match: it advances the
			// examined-events window but must not become the dispatch actor.
			labelEvent("4", "bug", "dave", true, labelReviewBaseTime.Add(-30*time.Second)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(state.RetryAttempts) != 1 {
		t.Fatalf("RetryAttempts count = %d, want 1 (burst collapses to one command)", len(state.RetryAttempts))
	}
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatalf("retry not scheduled for %s", issueID)
	}
	lfMap, ok := retry.ContinuationContext["label_fix"].(map[string]any)
	if !ok {
		t.Fatalf("label_fix context type = %T, want map[string]any", retry.ContinuationContext["label_fix"])
	}
	if lfMap["actor"] != "carol" {
		t.Errorf("label_fix[actor] = %v, want %q (the latest matching event's actor, ignoring the newer foreign label)", lfMap["actor"], "carol")
	}
}

// TestReconcileLabelFixCommands_DepthOneWhileRunning covers the depth-one
// invariant: a matching event arrives while a label-fix session is already
// running for the issue. No second dispatch or pending entry is created.
func TestReconcileLabelFixCommands_DepthOneWhileRunning(t *testing.T) {
	t.Parallel()

	const issueID = "LF-RUN"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-run")
	state.Running[issueID] = &RunningEntry{Identifier: issueID + "-ident"}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; dispatched {
		t.Error("retry scheduled while a label-fix session is already running; want depth-one collapse")
	}
	// The mark still advances even though nothing dispatches (falls into
	// the no-dispatch branch).
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
	if mark == "" {
		t.Error("stored mark is empty; want advanced even when collapsed into the running session")
	}
}

// TestReconcileLabelFixCommands_DepthOneWhileQueued covers the depth-one
// invariant for the queued (not yet running) case: a second matching event
// arrives while state.RetryAttempts already holds a label-fix-kind entry
// for the issue. The existing entry is left intact.
func TestReconcileLabelFixCommands_DepthOneWhileQueued(t *testing.T) {
	t.Parallel()

	const issueID = "LF-QUEUED"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-queued")
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		ReactionKind: ReactionKindLabelFix,
	}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(state.RetryAttempts) != 1 {
		t.Fatalf("RetryAttempts count = %d, want 1 (no second entry)", len(state.RetryAttempts))
	}
	retry := state.RetryAttempts[issueID]
	if retry.ContinuationContext != nil {
		t.Error("existing queued retry was replaced by a second dispatch; want collapsed (no ContinuationContext)")
	}
}

// TestReconcileLabelFixCommands_RepeatAfterCompletion verifies
// repeatability: after a first dispatch completes (the session exits,
// clearing state.RetryAttempts for the issue) and the label is
// re-applied, a second dispatch fires on a later tick driven entirely by
// the reconcile's own re-enqueue.
func TestReconcileLabelFixCommands_RepeatAfterCompletion(t *testing.T) {
	t.Parallel()

	const issueID = "LF-REPEAT"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-repeat")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-2*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

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
	// (the label re-applied after the fix completed).
	pending := state.PendingReactions[rkey]
	pending.PendingRetryAt = time.Time{}
	state.PendingReactions[rkey] = pending
	scm.events = append(scm.events, labelEvent("2", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)))

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("second dispatch did not fire after re-applying the label; want scheduled")
	}
	if scm.listCalls != 2 {
		t.Errorf("ListLabelEvents calls = %d, want 2 (one per tick)", scm.listCalls)
	}
}

// TestReconcileLabelFixCommands_AtMostOnceAcrossRestart verifies the
// at-most-once guarantee across a restart: the fingerprint is upserted
// before ScheduleRetry fires. A fresh reconcile call over a restored store
// (the mark it returns was upserted by the prior process) must not
// re-dispatch the same journal event.
func TestReconcileLabelFixCommands_AtMostOnceAcrossRestart(t *testing.T) {
	t.Parallel()

	const issueID = "LF-RESTART"
	event := labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute))
	store := newLabelReviewFingerprintStore()

	state1 := stateWithLabelFixPending(t, issueID, 10, "feature/lf-restart")
	scm1 := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	params1 := labelFixParams(store, scm1)

	reconcileLabelFixCommands(state1, params1, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state1.RetryAttempts[issueID]; !ok {
		t.Fatal("first dispatch did not fire; want scheduled")
	}
	persistedMark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
	if err != nil || persistedMark == "" {
		t.Fatalf("GetReactionFingerprint after dispatch = (%q, %v), want a non-empty persisted mark", persistedMark, err)
	}
	if persistedMark != labelReviewMark(event) {
		t.Fatalf("persisted mark = %q, want %q", persistedMark, labelReviewMark(event))
	}

	// Restart: a fresh state and reconcile call over the SAME store, whose
	// GetReactionFingerprint now returns the mark persisted above.
	state2 := stateWithLabelFixPending(t, issueID, 10, "feature/lf-restart")
	scm2 := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	params2 := labelFixParams(store, scm2)

	reconcileLabelFixCommands(state2, params2, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state2.RetryAttempts[issueID]; dispatched {
		t.Error("second process re-dispatched the same journal event; want no dispatch (mark already advanced past it)")
	}
}

// TestReconcileLabelFixCommands_DispatchedFlagIgnored verifies that
// reconcileLabelFixCommands never consults the dispatched flag returned by
// GetReactionFingerprint: at-most-once rests solely on the mark, so a
// store reporting dispatched=true does not suppress a dispatch for an
// event past the stored mark.
func TestReconcileLabelFixCommands_DispatchedFlagIgnored(t *testing.T) {
	t.Parallel()

	const issueID = "LF-FLAG"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-flag")
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := &labelFixDispatchedFlagStore{dispatched: true}
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("dispatch did not fire despite a fresh event past the mark; want the dispatched flag ignored")
	}
	if store.upsertCalls == 0 {
		t.Error("UpsertReactionFingerprint not called; want the mark advanced regardless of the dispatched flag")
	}
}

// TestReconcileLabelFixCommands_AcknowledgmentBestEffort covers V4: after a
// confirmed dispatch, RemoveLabel is called with the configured fix label;
// a failure logs a Warn and leaves dedup and the dispatch unaffected.
func TestReconcileLabelFixCommands_AcknowledgmentBestEffort(t *testing.T) {
	t.Parallel()

	const issueID = "LF-ACK"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-ack")
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
		removeErr: errors.New("insufficient scope"),
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reconcileLabelFixCommands(state, params, log, context.Background(), &domain.NoopMetrics{})

	if scm.removeCalls != 1 {
		t.Fatalf("RemoveLabel calls = %d, want 1", scm.removeCalls)
	}
	if scm.removedLabel[0] != "sortie:fix" {
		t.Errorf("RemoveLabel label = %q, want %q", scm.removedLabel[0], "sortie:fix")
	}
	if !strings.Contains(buf.String(), "label-fix label removal failed") {
		t.Errorf("log output = %q, want to contain the label-removal-failure warning", buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log output = %q, want the removal failure logged at WARN", buf.String())
	}
	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("dispatch did not fire despite the RemoveLabel failure; want unaffected")
	}
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
	if mark == "" {
		t.Error("stored mark is empty; want advanced regardless of the RemoveLabel failure")
	}
}

// TestReconcileLabelFixCommands_JournalReadError verifies that a
// ListLabelEvents error increments the per-entry backoff, re-enqueues the
// entry, and does not advance the mark.
func TestReconcileLabelFixCommands_JournalReadError(t *testing.T) {
	t.Parallel()

	const issueID = "LF-ERR"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-err")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	scm := &labelReviewSCMFake{listErr: errors.New("connection reset")}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

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

// TestReconcileLabelFixCommands_FingerprintReadError verifies that a
// fingerprint read failure backs off without dispatching. At-most-once
// rests solely on the mark, and this kind has no turn cap, so a command
// must never dispatch when the stored mark cannot be read.
func TestReconcileLabelFixCommands_FingerprintReadError(t *testing.T) {
	t.Parallel()

	const issueID = "LF-FPE"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-fpe")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	scm := &labelReviewSCMFake{}
	store := newLabelReviewFingerprintStore()
	store.getErr = errors.New("sqlite is locked")
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

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

// TestReconcileLabelFixCommands_NoTTLDrop verifies that a label-fix entry
// has no age-based drop branch: an entry whose CreatedAt is far older than
// every sibling kind's TTL is still processed normally on its next due
// tick.
func TestReconcileLabelFixCommands_NoTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "LF-OLD"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-old")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	state.PendingReactions[rkey].CreatedAt = labelReviewBaseTime.Add(-365 * 24 * time.Hour)

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, dispatched := state.RetryAttempts[issueID]; !dispatched {
		t.Error("year-old entry was not processed; want no TTL-drop branch for label-fix")
	}
	if _, stillPending := state.PendingReactions[rkey]; !stillPending {
		t.Error("PendingReactions entry dropped instead of re-enqueued; want no age-based drop")
	}
}

// TestReconcileLabelFixCommands_CrossKindIsolation covers A9: with ci,
// review, bot-review, merge, merge-conflict, and label-review entries all
// present for one issue, a label-fix dispatch mutates none of them.
func TestReconcileLabelFixCommands_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	const issueID = "LF-ISO"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-iso")

	ciEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindCI, KindData: &CIReactionData{Branch: "main"}}
	reviewEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindReview, KindData: &ReviewReactionData{PRNumber: 10}}
	botReviewEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindBotReview, KindData: &BotReviewReactionData{PRNumber: 10}}
	mergeEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindAutoMerge, KindData: &AutoMergeReactionData{PRNumber: 10}}
	mergeConflictEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindMergeConflict, KindData: &MergeConflictReactionData{PRNumber: 10}}
	labelReviewEntry := &PendingReaction{IssueID: issueID, Kind: ReactionKindLabelReview, KindData: &LabelReviewReactionData{PRNumber: 10}}

	state.PendingReactions[ReactionKey(issueID, ReactionKindCI)] = ciEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindReview)] = reviewEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindBotReview)] = botReviewEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)] = mergeEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindMergeConflict)] = mergeConflictEntry
	state.PendingReactions[ReactionKey(issueID, ReactionKindLabelReview)] = labelReviewEntry

	store := newLabelReviewFingerprintStore()
	if err := store.UpsertReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview, "sibling-fingerprint"); err != nil {
		t.Fatalf("seed sibling fingerprint: %v", err)
	}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Fatal("label-fix dispatch did not fire")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindCI)]; got != ciEntry {
		t.Error("CI PendingReaction entry mutated by the label-fix reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindReview)]; got != reviewEntry {
		t.Error("review PendingReaction entry mutated by the label-fix reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindBotReview)]; got != botReviewEntry {
		t.Error("bot-review PendingReaction entry mutated by the label-fix reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)]; got != mergeEntry {
		t.Error("auto-merge PendingReaction entry mutated by the label-fix reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeConflict)]; got != mergeConflictEntry {
		t.Error("merge-conflict PendingReaction entry mutated by the label-fix reconcile")
	}
	if got := state.PendingReactions[ReactionKey(issueID, ReactionKindLabelReview)]; got != labelReviewEntry {
		t.Error("label-review PendingReaction entry mutated by the label-fix reconcile")
	}
	siblingMark, _, err := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelReview)
	if err != nil {
		t.Fatalf("GetReactionFingerprint(label-review): %v", err)
	}
	if siblingMark != "sibling-fingerprint" {
		t.Errorf("label-review fingerprint = %q, want unchanged %q", siblingMark, "sibling-fingerprint")
	}
}

// TestReconcileLabelFixCommands_DispatchLogsActorAndPRNumber covers A11: a
// confirmed dispatch emits an Info log carrying the PR number and the
// acting user, unconditionally.
func TestReconcileLabelFixCommands_DispatchLogsActorAndPRNumber(t *testing.T) {
	t.Parallel()

	const issueID = "LF-LOG"
	state := stateWithLabelFixPending(t, issueID, 77, "feature/lf-log")
	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reconcileLabelFixCommands(state, params, log, context.Background(), &domain.NoopMetrics{})

	out := buf.String()
	if !strings.Contains(out, "label-fix dispatched") {
		t.Fatalf("log output = %q, want to contain the dispatch message", out)
	}
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("log output = %q, want the dispatch message logged at INFO", out)
	}
	if !strings.Contains(out, "pr_number=77") {
		t.Errorf("log output = %q, want pr_number=77 attribute", out)
	}
	if !strings.Contains(out, "actor=alice") {
		t.Errorf("log output = %q, want actor=alice attribute", out)
	}
}

// TestReconcileLabelFixCommands_JournalSubstrateNotSnapshot verifies that
// detection uses only ListLabelEvents (the journal substrate); no other
// SCMAdapter method is ever called for label-fix detection or dispatch.
func TestReconcileLabelFixCommands_JournalSubstrateNotSnapshot(t *testing.T) {
	t.Parallel()

	const issueID = "LF-JOURNAL"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-journal")
	scm := &labelReviewSCMPanicOnOthers{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("dispatch did not fire; want scheduled using only ListLabelEvents/RemoveLabel")
	}
}

// --- buildLabelFixMap tests ---

// TestBuildLabelFixMap_FieldMapping covers A13's sibling shape and the
// added branch coordinate: every documented field is present and mapped
// from LabelFixReactionData.
func TestBuildLabelFixMap_FieldMapping(t *testing.T) {
	t.Parallel()

	data := &LabelFixReactionData{PRNumber: 99, Owner: "acme", Repo: "widgets", Branch: "feature/99"}
	requestedAt := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)

	got := buildLabelFixMap(data, "alice", requestedAt)

	want := map[string]any{
		"pr_number":    99,
		"owner":        "acme",
		"repo":         "widgets",
		"branch":       "feature/99",
		"actor":        "alice",
		"requested_at": "2026-07-09T12:30:00Z",
	}
	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("buildLabelFixMap missing key %q", k)
			continue
		}
		if gotV != wantV {
			t.Errorf("buildLabelFixMap[%q] = %v, want %v", k, gotV, wantV)
		}
	}
	if len(got) != len(want) {
		t.Errorf("buildLabelFixMap has %d keys, want %d", len(got), len(want))
	}
}

// TestReconcileLabelFixCommands_ForeignIncumbentDefers mirrors
// TestReconcileLabelReviewCommands_ForeignIncumbentDefers for the
// label-fix kind: a confirmed command with a foreign incumbent occupying
// the retry slot defers instead of dispatching, leaving the fingerprint
// mark, the high-water mark, and the label untouched.
func TestReconcileLabelFixCommands_ForeignIncumbentDefers(t *testing.T) {
	t.Parallel()

	const issueID = "LF-FOREIGN"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-foreign")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      1,
		ReactionKind: ReactionKindCI,
	}

	scm := &labelReviewSCMFake{
		events: []domain.LabelEvent{
			labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute)),
		},
	}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on a defer; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(labelReviewBaseTime) {
		t.Errorf("CreatedAt = %v, want refreshed to %v", entry.CreatedAt, labelReviewBaseTime)
	}
	incumbent := state.RetryAttempts[issueID]
	if incumbent.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q (incumbent unchanged)", incumbent.ReactionKind, ReactionKindCI)
	}
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
	if mark != "" {
		t.Errorf("stored mark = %q, want empty (a defer must not upsert the fingerprint)", mark)
	}
	data, ok := entry.KindData.(*LabelFixReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelFixReactionData", entry.KindData)
	}
	if data.HighWaterMark != "" {
		t.Errorf("HighWaterMark = %q, want empty (a defer must not advance it)", data.HighWaterMark)
	}
	if scm.removeCalls != 0 {
		t.Errorf("RemoveLabel calls = %d, want 0 (a defer must not acknowledge the command)", scm.removeCalls)
	}
}

// TestReconcileLabelFixCommands_SameKindCollapseAdvancesMarkAndReenqueues
// mirrors the label-review same-kind collapse test: a same-kind
// incumbent collapses the dispatch, but the mark still advances and the
// pending entry still re-enqueues.
func TestReconcileLabelFixCommands_SameKindCollapseAdvancesMarkAndReenqueues(t *testing.T) {
	t.Parallel()

	const issueID = "LF-COLLAPSE"
	state := stateWithLabelFixPending(t, issueID, 10, "feature/lf-collapse")
	rkey := ReactionKey(issueID, ReactionKindLabelFix)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      1,
		ReactionKind: ReactionKindLabelFix,
	}

	event := labelEvent("1", "sortie:fix", "alice", true, labelReviewBaseTime.Add(-1*time.Minute))
	scm := &labelReviewSCMFake{events: []domain.LabelEvent{event}}
	store := newLabelReviewFingerprintStore()
	params := labelFixParams(store, scm)

	reconcileLabelFixCommands(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if len(state.RetryAttempts) != 1 {
		t.Fatalf("RetryAttempts count = %d, want 1 (no second dispatch)", len(state.RetryAttempts))
	}
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after collapse; want re-enqueued")
	}
	mark, _, _ := store.GetReactionFingerprint(context.Background(), issueID, ReactionKindLabelFix)
	if mark != labelReviewMark(event) {
		t.Errorf("stored mark = %q, want %q (advanced to the newest examined event)", mark, labelReviewMark(event))
	}
	data, ok := pending.KindData.(*LabelFixReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelFixReactionData", pending.KindData)
	}
	if data.HighWaterMark != labelReviewMark(event) {
		t.Errorf("HighWaterMark = %q, want %q", data.HighWaterMark, labelReviewMark(event))
	}
}
