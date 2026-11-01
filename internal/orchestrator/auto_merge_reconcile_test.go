package orchestrator

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/storage"
)

// controlledSCMAdapter is a full SCMAdapter where each method is individually
// controllable via function fields. Used by auto-merge reconcile tests that
// need to simulate different combinations of API responses and errors.
type controlledSCMAdapter struct {
	fetchPendingReviewsFn    func(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error)
	fetchBotReviewCommentsFn func(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error)
	getReviewDecisionFn      func(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error)
	getCIStatusFn            func(ctx context.Context, prNumber int, owner, repo string) (string, error)
	getMergeabilityFn        func(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error)
	mergePRFn                func(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error)
	deleteBranchFn           func(ctx context.Context, owner, repo, branch string) error
}

var _ domain.SCMAdapter = (*controlledSCMAdapter)(nil)

func (c *controlledSCMAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	if c.fetchPendingReviewsFn != nil {
		return c.fetchPendingReviewsFn(ctx, prNumber, owner, repo)
	}
	return nil, nil
}

func (c *controlledSCMAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	if c.fetchBotReviewCommentsFn != nil {
		return c.fetchBotReviewCommentsFn(ctx, prNumber, owner, repo, botUsernames)
	}
	return nil, nil
}

func (c *controlledSCMAdapter) GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error) {
	if c.getReviewDecisionFn != nil {
		return c.getReviewDecisionFn(ctx, prNumber, owner, repo)
	}
	return domain.ReviewDecisionApproved, nil
}

func (c *controlledSCMAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	if c.getCIStatusFn != nil {
		return c.getCIStatusFn(ctx, prNumber, owner, repo)
	}
	return "success", nil
}

func (c *controlledSCMAdapter) GetMergeability(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	if c.getMergeabilityFn != nil {
		return c.getMergeabilityFn(ctx, prNumber, owner, repo)
	}
	return domain.PRMergeStatus{
		Mergeability: domain.MergeabilityClean,
		HeadSHA:      "abc123",
	}, nil
}

func (c *controlledSCMAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	if c.mergePRFn != nil {
		return c.mergePRFn(ctx, prNumber, owner, repo, strategy, commitTitle, commitMessage, expectedHeadSHA)
	}
	return domain.MergeResult{SHA: "merged-sha"}, nil
}

func (c *controlledSCMAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	if c.deleteBranchFn != nil {
		return c.deleteBranchFn(ctx, owner, repo, branch)
	}
	return nil
}

func (c *controlledSCMAdapter) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}

func (c *controlledSCMAdapter) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// autoMergeMetricsSpy records auto-merge metric calls.
type autoMergeMetricsSpy struct {
	domain.NoopMetrics
	autoMerge map[string]int
}

func newAutoMergeMetricsSpy() *autoMergeMetricsSpy {
	return &autoMergeMetricsSpy{autoMerge: make(map[string]int)}
}

func (s *autoMergeMetricsSpy) IncAutoMergeReactions(result string) { s.autoMerge[result]++ }

// autoMergeBaseTime is a fixed reference for auto-merge reconcile tests.
var autoMergeBaseTime = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

// newAutoMergePending builds a PendingReaction with Kind=ReactionKindAutoMerge.
func newAutoMergePending(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID,
		Attempt:    1,
		Kind:       ReactionKindAutoMerge,
		CreatedAt:  autoMergeBaseTime,
		KindData: &AutoMergeReactionData{
			PRNumber: prNumber,
			Owner:    "corp",
			Repo:     "api",
			Branch:   "feature/" + issueID,
			SHA:      "abc123",
		},
	}
}

// stateWithAutoMergePending creates a State with one auto-merge PendingReaction.
func stateWithAutoMergePending(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, 0, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindAutoMerge)
	s.PendingReactions[rkey] = newAutoMergePending(issueID, prNumber)
	return s
}

// defaultAutoMergeConfig returns an AutoMergeReactionConfig suitable for
// happy-path tests.
func defaultAutoMergeConfig() AutoMergeReactionConfig {
	return AutoMergeReactionConfig{
		Strategy:       domain.StrategySquash,
		RequireCI:      true,
		DeleteBranch:   false,
		PollIntervalMS: 60000,
		Escalation:     "comment",
		MaxRetries:     5,
	}
}

// autoMergeParams returns ReconcileParams wired for auto-merge reconcile
// tests with the given SCM adapter and optional tracker.
func autoMergeParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter:              tracker,
		SCMAdapter:                  scm,
		AutoMergeConfig:             defaultAutoMergeConfig(),
		AutoMergeReactionConfigured: true,
		Store:                       store,
		OnRetryFire:                 noopRetryFire,
		Ctx:                         context.Background(),
		Logger:                      discardLogger(),
		NowFunc:                     func() time.Time { return autoMergeBaseTime },
	}
}

// TestReconcileAutoMerge_NilAdapter verifies that reconcileAutoMerge is a no-op
// when the SCM adapter is nil.
func TestReconcileAutoMerge_NilAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-NA", 10)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	params := autoMergeParams(store, nil, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("AM-NA", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed with nil adapter; want no-op")
	}
	if len(metrics.autoMerge) != 0 {
		t.Errorf("IncAutoMergeReactions called = %v, want no calls", metrics.autoMerge)
	}
}

// TestReconcileAutoMerge_NotConfigured verifies that reconcileAutoMerge is a
// no-op when AutoMergeReactionConfigured is false.
func TestReconcileAutoMerge_NotConfigured(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-NC", 10)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	scm := &controlledSCMAdapter{}
	params := autoMergeParams(store, scm, nil)
	params.AutoMergeReactionConfigured = false

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("AM-NC", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed when not configured; want no-op")
	}
}

// TestReconcileAutoMerge_CIPending verifies that the pending entry is
// re-enqueued without merging when CI is pending.
func TestReconcileAutoMerge_CIPending(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-CI", 43)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-ci",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "pending", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCalled {
		t.Error("MergePR called when CI is pending; want deferred")
	}

	rkey := ReactionKey("AM-CI", ReactionKindAutoMerge)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[AM-CI:merge] removed when CI pending; want re-enqueued")
	}
	if !pending.PendingRetryAt.After(autoMergeBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v", pending.PendingRetryAt, autoMergeBaseTime)
	}
}

// TestReconcileAutoMerge_DraftPR verifies that a draft PR is not merged.
func TestReconcileAutoMerge_DraftPR(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-DR", 44)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-draft",
				Draft:        true,
			}, nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCalled {
		t.Error("MergePR called for draft PR; want skipped")
	}

	rkey := ReactionKey("AM-DR", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[AM-DR:merge] removed for draft PR; want re-enqueued")
	}
}

// TestReconcileAutoMerge_ChangesRequested verifies that the merge is deferred
// when review decision is CHANGES_REQUESTED.
func TestReconcileAutoMerge_ChangesRequested(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-CR", 45)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-cr",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionChangesRequested, nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCalled {
		t.Error("MergePR called when review requires changes; want deferred")
	}

	rkey := ReactionKey("AM-CR", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[AM-CR:merge] removed when changes requested; want re-enqueued")
	}
}

// TestReconcileAutoMerge_FingerprintDedup verifies that a second tick with the
// same headSHA and review decision does not re-call MergePR.
func TestReconcileAutoMerge_FingerprintDedup(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-FP", 46)
	store := &reviewReconcileStore{
		getFingerprintResult:     buildAutoMergeFingerprint("sha-fp", domain.ReviewDecisionApproved),
		getFingerprintDispatched: true,
	}
	metrics := newAutoMergeMetricsSpy()

	var mergeCallCount int
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-fp",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCallCount++
			return domain.MergeResult{SHA: "sha-merged"}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCallCount != 0 {
		t.Errorf("MergePR called %d times for fingerprint-deduped attempt; want 0", mergeCallCount)
	}

	rkey := ReactionKey("AM-FP", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[AM-FP:merge] removed on fingerprint dedup; want re-enqueued")
	}
}

func TestReconcileAutoMerge_MaxRetriesEscalation(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-MAX", 47)
	rkey := ReactionKey("AM-MAX", ReactionKindAutoMerge)
	state.ReactionAttempts[rkey] = 5 // already at MaxRetries (5)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	tracker := &reviewTrackerStub{}
	var mergeCalls int
	scm := &controlledSCMAdapter{
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalls++
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, tracker)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	entry := assertEscalationBegan(t, state, rkey, EscalationTriggerBudget)
	if entry.EscalationSubject != "" {
		t.Errorf("EscalationSubject = %q, want empty for the count check", entry.EscalationSubject)
	}
	if state.ReactionAttempts[rkey] != 5 {
		t.Errorf("ReactionAttempts[%s] = %d, want 5 (kept)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.autoMerge["escalated"] != 1 {
		t.Errorf("IncAutoMergeReactions(escalated) = %d, want 1", metrics.autoMerge["escalated"])
	}

	makeDue(t, state, rkey)
	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.autoMerge["escalated"] != 1 || mergeCalls != 0 {
		t.Errorf("standing pass: escalated = %d, MergePR calls = %d, want 1, 0", metrics.autoMerge["escalated"], mergeCalls)
	}
	assertEscalationBegan(t, state, rkey, EscalationTriggerBudget)
}

func TestReconcileAutoMerge_MergedActions(t *testing.T) {
	t.Parallel()

	alreadyMerged := &domain.SCMError{Kind: domain.ErrSCMConflict, Message: "already merged"}

	tests := []struct {
		name         string
		mergeErr     error
		deleteBranch bool
	}{
		{name: "merge succeeded deletes branch", deleteBranch: true},
		{name: "already merged deletes branch", mergeErr: alreadyMerged, deleteBranch: true},
		{name: "already merged keeps branch", mergeErr: alreadyMerged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "AM-MA"
			state := stateWithAutoMergePending(t, issueID, 48)
			ciKey := ReactionKey(issueID, ReactionKindCI)
			reviewKey := ReactionKey(issueID, ReactionKindReview)
			state.PendingReactions[ciKey] = &PendingReaction{IssueID: issueID, Kind: ReactionKindCI}
			state.PendingReactions[reviewKey] = &PendingReaction{IssueID: issueID, Kind: ReactionKindReview}
			state.RetryAttempts[issueID] = &RetryEntry{IssueID: issueID, ReactionKind: ReactionKindReview}
			state.Claimed[issueID] = struct{}{}

			var deletedBranches []string
			scm := &controlledSCMAdapter{
				mergePRFn: func(context.Context, int, string, string, domain.MergeStrategy, string, string, string) (domain.MergeResult, error) {
					return domain.MergeResult{SHA: "merge-sha"}, tt.mergeErr
				},
				deleteBranchFn: func(_ context.Context, owner, repo, branch string) error {
					deletedBranches = append(deletedBranches, owner+"/"+repo+"/"+branch)
					return nil
				},
			}
			slack := &notifierSpy{}
			store := &reviewReconcileStore{}
			metrics := newAutoMergeMetricsSpy()
			params := autoMergeParams(store, scm, nil)
			params.AutoMergeConfig.DeleteBranch = tt.deleteBranch
			params.Router = mustRouter(t, nil, spyLookup(slack), route.Inputs{Entries: []config.NotificationBackend{
				subscribe("slack", domain.EventAutoMergeMerged),
			}})

			reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
			state.TrackerOpsWg.Wait()

			if _, ok := state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)]; ok {
				t.Errorf("PendingReactions[%s:merge] present after merge, want removed", issueID)
			}
			if metrics.autoMerge["merged"] != 1 {
				t.Errorf("IncAutoMergeReactions(merged) = %d, want 1", metrics.autoMerge["merged"])
			}
			if store.markDispatchedCalls != 1 || store.deleteFingerprintCalls != 1 {
				t.Errorf("MarkReactionDispatched, DeleteReactionFingerprint calls = %d, %d, want 1, 1",
					store.markDispatchedCalls, store.deleteFingerprintCalls)
			}

			if len(store.outcomes) != 1 || store.outcomes[0].Kind != storage.IssueOutcomeMerged || store.outcomes[0].IssueID != issueID {
				t.Errorf("RecordIssueOutcome calls = %+v, want one %q outcome for %q", store.outcomes, storage.IssueOutcomeMerged, issueID)
			}

			sent := slack.notifications()
			if len(sent) != 1 || sent[0].Envelope.EventType != domain.EventAutoMergeMerged || sent[0].Envelope.IssueID != issueID {
				t.Errorf("notifications = %+v, want one %q for %q", sent, domain.EventAutoMergeMerged, issueID)
			}

			var wantBranches []string
			if tt.deleteBranch {
				wantBranches = []string{"corp/api/feature/" + issueID}
			}
			if !slices.Equal(deletedBranches, wantBranches) {
				t.Errorf("DeleteBranch calls = %q, want %q", deletedBranches, wantBranches)
			}

			for _, key := range []string{ciKey, reviewKey} {
				if _, ok := state.PendingReactions[key]; !ok {
					t.Errorf("PendingReactions[%s] removed by merge, want intact", key)
				}
			}
			if _, ok := state.RetryAttempts[issueID]; !ok {
				t.Errorf("RetryAttempts[%s] removed by merge, want intact", issueID)
			}
			if _, ok := state.Claimed[issueID]; !ok {
				t.Errorf("Claimed[%s] released by merge, want intact", issueID)
			}
			if len(store.deletedIssueIDs) != 0 {
				t.Errorf("DeleteRetryEntry calls = %q, want none", store.deletedIssueIDs)
			}
		})
	}
}

func TestReconcileAutoMerge_AuthEscalatesImmediately(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-AU", 49)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	tracker := &reviewTrackerStub{}

	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-auth",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			return domain.MergeResult{}, &domain.SCMError{Kind: domain.ErrSCMAuth, Message: "401 unauthorized"}
		},
	}
	params := autoMergeParams(store, scm, tracker)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	state.TrackerOpsWg.Wait()

	rkey := ReactionKey("AM-AU", ReactionKindAutoMerge)
	if entry := assertEscalationBegan(t, state, rkey, EscalationTriggerRefused); entry.EscalationSubject == "" {
		t.Error("EscalationSubject is empty, want the merge fingerprint of the refused call")
	}

	if metrics.autoMerge["escalated"] != 1 {
		t.Errorf("IncAutoMergeReactions(escalated) = %d, want 1", metrics.autoMerge["escalated"])
	}
}

// TestReconcileAutoMerge_AuthDedupe verifies that ErrSCMAuth on MergePR emits
// the ERROR log at most once per issue ID across multiple ticks.
func TestReconcileAutoMerge_AuthDedupe(t *testing.T) {
	t.Parallel()

	var buf logBuf
	log := buf.logger()

	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-dedup",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			return domain.MergeResult{}, &domain.SCMError{Kind: domain.ErrSCMAuth, Message: "401"}
		},
	}
	tracker := &reviewTrackerStub{}
	store := &reviewReconcileStore{}
	params := autoMergeParams(store, scm, tracker)

	state := stateWithAutoMergePending(t, "AM-DD", 50)
	reconcileAutoMerge(state, params, log, context.Background(), newAutoMergeMetricsSpy())
	state.TrackerOpsWg.Wait()

	buf.reset()

	rkey := ReactionKey("AM-DD", ReactionKindAutoMerge)
	state.PendingReactions[rkey] = newAutoMergePending("AM-DD", 50)
	metrics2 := newAutoMergeMetricsSpy()
	reconcileAutoMerge(state, params, log, context.Background(), metrics2)
	state.TrackerOpsWg.Wait()

	if buf.contains("auto_merge runtime auth failure") {
		t.Error("auth failure ERROR logged on second tick; want deduplicated (one log per issue lifetime)")
	}
}

// TestReconcileAutoMerge_Conflict405Reenqueues verifies that an ErrSCMConflict
// (e.g. HTTP 405 method not allowed) re-enqueues rather than escalating
// immediately.
func TestReconcileAutoMerge_Conflict405Reenqueues(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-405", 51)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-405",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			return domain.MergeResult{}, &domain.SCMError{
				Kind:    domain.ErrSCMConflict,
				Message: "method not allowed: branch protection requires status checks",
			}
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("AM-405", ReactionKindAutoMerge)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[AM-405:merge] removed after 405 conflict; want re-enqueued")
	}
	if !pending.PendingRetryAt.After(autoMergeBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v", pending.PendingRetryAt, autoMergeBaseTime)
	}
	if metrics.autoMerge["escalated"] != 0 {
		t.Errorf("IncAutoMergeReactions(escalated) = %d, want 0 (not yet escalated)", metrics.autoMerge["escalated"])
	}
}

// TestReconcileAutoMerge_DoesNotMarkDispatchedOnTransientFailure verifies that
// MarkReactionDispatched is invoked only on a successful merge. A transient
// MergePR failure on tick one must leave the dispatched flag clear so the
// fingerprint dedup branch does not short-circuit the retry attempt on tick
// two; the second tick then succeeds and records the dispatch exactly once.
func TestReconcileAutoMerge_DoesNotMarkDispatchedOnTransientFailure(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-RT", 99)
	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCallCount int
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{
				Mergeability: domain.MergeabilityClean,
				HeadSHA:      "sha-retry",
			}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCallCount++
			if mergeCallCount == 1 {
				return domain.MergeResult{}, &domain.SCMError{
					Kind:    domain.ErrSCMTransport,
					Message: "dial timeout",
				}
			}
			return domain.MergeResult{SHA: "merge-sha-retry"}, nil
		},
	}
	tracker := &reviewTrackerStub{}
	params := autoMergeParams(store, scm, tracker)

	// Tick one: transient failure. UpsertReactionFingerprint is invoked
	// before MergePR; GetReactionFingerprint then reads back the stored
	// fingerprint. Configure the stub so the read returns the value that
	// was just upserted but with dispatched=false, modeling the persisted
	// state at the start of tick two.
	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if mergeCallCount != 1 {
		t.Fatalf("MergePR call count after tick one = %d, want 1", mergeCallCount)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls after transient failure = %d, want 0", store.markDispatchedCalls)
	}

	rkey := ReactionKey("AM-RT", ReactionKindAutoMerge)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[AM-RT:merge] removed after transient failure; want re-enqueued")
	}

	// Tick two: clear the backoff so the entry is eligible immediately,
	// configure the stub fingerprint to match the stored value with
	// dispatched=false so the dedup short-circuit does NOT trigger. The
	// second MergePR call succeeds and the dispatched flag is set.
	pending.PendingRetryAt = autoMergeBaseTime
	store.getFingerprintResult = buildAutoMergeFingerprint("sha-retry", domain.ReviewDecisionApproved)
	store.getFingerprintDispatched = false

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if mergeCallCount != 2 {
		t.Errorf("MergePR call count after tick two = %d, want 2 (dedup should not block retry)", mergeCallCount)
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls after successful merge = %d, want 1", store.markDispatchedCalls)
	}
	if metrics.autoMerge["merged"] != 1 {
		t.Errorf("IncAutoMergeReactions(merged) = %d, want 1", metrics.autoMerge["merged"])
	}
	if _, stillPending := state.PendingReactions[rkey]; stillPending {
		t.Error("PendingReactions[AM-RT:merge] still present after successful retry; want removed")
	}
}

// TestReconcileAutoMerge_CrossKindIsolationOnEscalation verifies that
// escalation does not remove review-kind retry entries.
func TestReconcileAutoMerge_CrossKindIsolationOnEscalation(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-XKE", 53)
	rkey := ReactionKey("AM-XKE", ReactionKindAutoMerge)
	state.ReactionAttempts[rkey] = 5 // at MaxRetries

	// Seed a review-kind retry entry for the same issue.
	state.RetryAttempts["AM-XKE"] = &RetryEntry{
		IssueID:      "AM-XKE",
		Identifier:   "AM-XKE-ident",
		ReactionKind: ReactionKindReview,
	}
	state.Claimed["AM-XKE"] = struct{}{}

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &controlledSCMAdapter{}
	params := autoMergeParams(store, scm, tracker)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	assertEscalationBegan(t, state, rkey, EscalationTriggerBudget)

	// Review-kind retry entry must be intact.
	retryEntry, ok := state.RetryAttempts["AM-XKE"]
	if !ok {
		t.Fatal("RetryAttempts[AM-XKE] (review kind) removed by escalation; want intact")
	}
	if retryEntry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts[AM-XKE].ReactionKind = %q, want %q", retryEntry.ReactionKind, ReactionKindReview)
	}

	// Claim must be intact.
	if _, ok := state.Claimed["AM-XKE"]; !ok {
		t.Error("state.Claimed[AM-XKE] released by escalation; want intact")
	}
}

func TestReconcileAutoMerge_PreflightFailed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		escalated     bool
		wantRemaining bool
	}{
		{name: "an entry with no escalation is dropped", wantRemaining: false},
		{name: "a standing escalation survives", escalated: true, wantRemaining: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithAutoMergePending(t, "AM-PF", 54)
			state.AutoMergePreflightFailed = true
			rkey := ReactionKey("AM-PF", ReactionKindAutoMerge)
			if tt.escalated {
				entry := state.PendingReactions[rkey]
				entry.EscalatedAt = autoMergeBaseTime
				entry.EscalationTrigger = EscalationTriggerRefused
				entry.EscalationSubject = "merge-fp"
			}

			var mergeCalled bool
			scm := &controlledSCMAdapter{
				mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
					mergeCalled = true
					return domain.MergeResult{}, nil
				},
			}
			params := autoMergeParams(&reviewReconcileStore{}, scm, nil)

			reconcileAutoMerge(state, params, discardLogger(), context.Background(), newAutoMergeMetricsSpy())

			if mergeCalled {
				t.Error("MergePR called despite preflight failure; want skipped")
			}
			entry, ok := state.PendingReactions[rkey]
			if ok != tt.wantRemaining {
				t.Fatalf("PendingReactions[AM-PF:merge] present = %t after the preflight skip, want %t", ok, tt.wantRemaining)
			}
			if ok && entry.EscalatedAt.IsZero() {
				t.Error("EscalatedAt is zero, want the escalation still standing")
			}
		})
	}
}

// TestReconcileAutoMerge_PreflightRetrySucceeds verifies that when the retry
// due-at has passed and the retry verifier succeeds, the preflight flag is
// cleared and pending entries are processed in the same tick.
func TestReconcileAutoMerge_PreflightRetrySucceeds(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-PR", 55)
	state.AutoMergePreflightFailed = true
	// Set the retry due-at in the past so it fires on the first tick.
	state.AutoMergePreflightRetryDueAt = autoMergeBaseTime.Add(-1 * time.Second)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: "sha-pr"}, nil
		},
		getReviewDecisionFn: func(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
			return domain.ReviewDecisionApproved, nil
		},
		getCIStatusFn: func(_ context.Context, _ int, _, _ string) (string, error) {
			return "success", nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{SHA: "sha-merged-pr"}, nil
		},
	}
	tracker := &reviewTrackerStub{}

	// Use a scope verifier adapter that passes.
	verifier := &scopeVerifierAdapter{scm: scm}
	params := autoMergeParams(store, verifier, tracker)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if state.AutoMergePreflightFailed {
		t.Error("AutoMergePreflightFailed = true after successful retry; want false")
	}
	if !state.AutoMergePreflightRetryDueAt.IsZero() {
		t.Errorf("AutoMergePreflightRetryDueAt = %v after retry; want zero", state.AutoMergePreflightRetryDueAt)
	}
	if !mergeCalled {
		t.Error("MergePR not called after successful preflight retry; want called in same tick")
	}
}

// TestReconcileAutoMerge_PreflightRetryExhausts verifies that when the retry
// fails again with a transport error, the due-at is cleared but the flag stays
// set.
func TestReconcileAutoMerge_PreflightRetryExhausts(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-RE", 56)
	state.AutoMergePreflightFailed = true
	state.AutoMergePreflightRetryDueAt = autoMergeBaseTime.Add(-1 * time.Second)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	// Transport-failing verifier.
	verifier := &scopeVerifierAdapter{
		verifyErr: &domain.SCMError{Kind: domain.ErrSCMTransport, Message: "network timeout"},
	}
	realSCM := &controlledSCMAdapter{
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	verifier.scm = realSCM
	params := autoMergeParams(store, verifier, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if !state.AutoMergePreflightFailed {
		t.Error("AutoMergePreflightFailed = false after transport-failing retry; want true (sticky)")
	}
	if !state.AutoMergePreflightRetryDueAt.IsZero() {
		t.Errorf("AutoMergePreflightRetryDueAt = %v after exhausted retry; want zero", state.AutoMergePreflightRetryDueAt)
	}
	if mergeCalled {
		t.Error("MergePR called after exhausted preflight retry; want skipped")
	}
}

// TestReconcileAutoMerge_PendingRetryAt verifies that an entry with a future
// PendingRetryAt is re-enqueued without calling any SCM methods.
func TestReconcileAutoMerge_PendingRetryAt(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-RT", 57)
	rkey := ReactionKey("AM-RT", ReactionKindAutoMerge)
	state.PendingReactions[rkey].PendingRetryAt = autoMergeBaseTime.Add(10 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	var mergeabilityCalled bool
	scm := &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			mergeabilityCalled = true
			return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: "sha-rt"}, nil
		},
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeabilityCalled {
		t.Error("GetMergeability called for a not-yet-due entry; want skipped")
	}
	if mergeCalled {
		t.Error("MergePR called for a not-yet-due entry; want skipped")
	}
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[AM-RT:merge] removed for future PendingRetryAt; want preserved")
	}
}

// TestReconcileAutoMerge_TTLExpiry verifies that an entry older than the
// configured TTL is dropped without merging.
func TestReconcileAutoMerge_TTLExpiry(t *testing.T) {
	t.Parallel()

	state := stateWithAutoMergePending(t, "AM-TTL", 58)
	// Set entry age beyond TTL by backdating its creation.
	rkey := ReactionKey("AM-TTL", ReactionKindAutoMerge)
	state.PendingReactions[rkey].CreatedAt = autoMergeBaseTime.Add(-40 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)
	params.AutoMergePendingTTL = 30 * time.Minute

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCalled {
		t.Error("MergePR called for TTL-expired entry; want dropped")
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[AM-TTL:merge] still present after TTL expiry; want dropped")
	}
}

// TestReconcileAutoMerge_DropOnAgeReleasesCounter verifies that the
// drop-on-age branch also deletes the merge reaction attempt counter,
// leaves a sibling kind's counter and the claim untouched, and performs
// no retry or fingerprint store call.
func TestReconcileAutoMerge_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "AM-AGE-1"
	state := stateWithAutoMergePending(t, issueID, 20)
	mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.ReactionAttempts[mergeKey] = 3
	state.PendingReactions[mergeKey].CreatedAt = autoMergeBaseTime.Add(-40 * time.Minute)
	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.ReactionAttempts[ciKey] = 6

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()

	var mergeCalled bool
	scm := &controlledSCMAdapter{
		mergePRFn: func(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			mergeCalled = true
			return domain.MergeResult{}, nil
		},
	}
	params := autoMergeParams(store, scm, nil)
	params.AutoMergePendingTTL = 30 * time.Minute

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if mergeCalled {
		t.Error("MergePR called for TTL-expired entry; want dropped before fetch")
	}
	if _, ok := state.PendingReactions[mergeKey]; ok {
		t.Error("PendingReactions[merge] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[mergeKey]; ok {
		t.Error("ReactionAttempts[merge] present after drop-on-age; want removed")
	}
	if state.ReactionAttempts[ciKey] != 6 {
		t.Errorf("ReactionAttempts[ci] = %d, want 6 (untouched)", state.ReactionAttempts[ciKey])
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
}

// dirtyMergeabilitySCM is a controlledSCMAdapter whose PR is reported
// dirty, so any entry that survives the watch-window check is deferred
// (re-enqueued) rather than merged and removed. This isolates the
// watch-window drop path from the unrelated successful-merge removal path
// in the auto-merge watch-window tests below.
func dirtyMergeabilitySCM(calls *int) *controlledSCMAdapter {
	return &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
			*calls++
			return domain.PRMergeStatus{Mergeability: domain.MergeabilityDirty, HeadSHA: "abc123"}, nil
		},
	}
}

// TestReconcileAutoMerge_WatchWindowZeroNeverDrops verifies that an
// AutoMergePendingTTL of 0 (watch_window_ms: 0) never drops an entry on age,
// however old it is.
func TestReconcileAutoMerge_WatchWindowZeroNeverDrops(t *testing.T) {
	t.Parallel()

	issueID := "AM-WW0"
	state := stateWithAutoMergePending(t, issueID, 20)
	mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
	state.PendingReactions[mergeKey].CreatedAt = autoMergeBaseTime.Add(-365 * 24 * time.Hour)

	store := &reviewReconcileStore{}
	metrics := newAutoMergeMetricsSpy()
	var calls int
	scm := dirtyMergeabilitySCM(&calls)
	params := autoMergeParams(store, scm, nil)
	params.AutoMergePendingTTL = 0

	reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[mergeKey]; !ok {
		t.Error("PendingReactions entry dropped with AutoMergePendingTTL=0; want never dropped on age")
	}
	if calls != 1 {
		t.Errorf("GetMergeability calls = %d, want 1 (entry survived the watch-window check)", calls)
	}
}

// TestReconcileAutoMerge_WatchWindowNonDefaultTakesEffect verifies that a
// configured window other than the default threshold actually gates the
// drop: an entry older than the configured window is dropped with its
// attempt counter released, and one younger survives.
func TestReconcileAutoMerge_WatchWindowNonDefaultTakesEffect(t *testing.T) {
	t.Parallel()

	t.Run("older than configured window is dropped and counter released", func(t *testing.T) {
		t.Parallel()

		issueID := "AM-WWN-OLD"
		state := stateWithAutoMergePending(t, issueID, 20)
		mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
		state.PendingReactions[mergeKey].CreatedAt = autoMergeBaseTime.Add(-6 * time.Minute)
		state.ReactionAttempts[mergeKey] = 2

		store := &reviewReconcileStore{}
		metrics := newAutoMergeMetricsSpy()
		var calls int
		scm := dirtyMergeabilitySCM(&calls)
		params := autoMergeParams(store, scm, nil)
		params.AutoMergePendingTTL = 5 * time.Minute

		reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[mergeKey]; ok {
			t.Error("PendingReactions entry present past configured 5m window; want dropped")
		}
		if _, ok := state.ReactionAttempts[mergeKey]; ok {
			t.Error("ReactionAttempts present past configured 5m window; want released")
		}
		if calls != 0 {
			t.Errorf("GetMergeability calls = %d, want 0 (watch window exceeded before fetch)", calls)
		}
	})

	t.Run("younger than configured window survives", func(t *testing.T) {
		t.Parallel()

		issueID := "AM-WWN-NEW"
		state := stateWithAutoMergePending(t, issueID, 20)
		mergeKey := ReactionKey(issueID, ReactionKindAutoMerge)
		state.PendingReactions[mergeKey].CreatedAt = autoMergeBaseTime.Add(-4 * time.Minute)

		store := &reviewReconcileStore{}
		metrics := newAutoMergeMetricsSpy()
		var calls int
		scm := dirtyMergeabilitySCM(&calls)
		params := autoMergeParams(store, scm, nil)
		params.AutoMergePendingTTL = 5 * time.Minute

		reconcileAutoMerge(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[mergeKey]; !ok {
			t.Error("PendingReactions entry dropped inside configured 5m window; want kept")
		}
		if calls != 1 {
			t.Errorf("GetMergeability calls = %d, want 1 (entry survived the watch-window check)", calls)
		}
	})
}

// TestBuildAutoMergeFingerprint verifies the SHA-256 hex fingerprint builder.
func TestBuildAutoMergeFingerprint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		headSHA  string
		decision domain.ReviewDecision
		wantLen  int
		wantNone bool
	}{
		{
			name:     "empty headSHA returns empty string",
			headSHA:  "",
			decision: domain.ReviewDecisionApproved,
			wantNone: true,
		},
		{
			name:     "non-empty SHA produces 64-char hex",
			headSHA:  "abc123",
			decision: domain.ReviewDecisionApproved,
			wantLen:  64,
		},
		{
			name:     "different decision produces different fingerprint",
			headSHA:  "abc123",
			decision: domain.ReviewDecisionChangesRequested,
			wantLen:  64,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildAutoMergeFingerprint(tt.headSHA, tt.decision)
			if tt.wantNone {
				if got != "" {
					t.Errorf("buildAutoMergeFingerprint(%q, %q) = %q, want empty", tt.headSHA, tt.decision, got)
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("buildAutoMergeFingerprint(%q, %q) len = %d, want %d", tt.headSHA, tt.decision, len(got), tt.wantLen)
			}
		})
	}

	// Verify that the same inputs always produce the same output.
	fp1 := buildAutoMergeFingerprint("deadbeef", domain.ReviewDecisionApproved)
	fp2 := buildAutoMergeFingerprint("deadbeef", domain.ReviewDecisionApproved)
	if fp1 != fp2 {
		t.Errorf("fingerprint not deterministic: %q != %q", fp1, fp2)
	}

	// Verify that a SHA change produces a different fingerprint.
	fp3 := buildAutoMergeFingerprint("cafebabe", domain.ReviewDecisionApproved)
	if fp1 == fp3 {
		t.Error("fingerprint collision between different SHA values; want distinct")
	}
}

// TestComputeAutoMergePendingDelay verifies the exponential-backoff formula.
func TestComputeAutoMergePendingDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{name: "attempt 0 returns zero", attempts: 0, want: 0},
		{name: "attempt 1 returns 20s", attempts: 1, want: 20 * time.Second},
		{name: "attempt 2 returns 40s", attempts: 2, want: 40 * time.Second},
		{name: "attempt 3 returns 80s", attempts: 3, want: 80 * time.Second},
		{name: "attempt 4 returns 160s", attempts: 4, want: 160 * time.Second},
		{name: "attempt 5 returns cap", attempts: 5, want: 5 * time.Minute},
		{name: "attempt 100 returns cap", attempts: 100, want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeAutoMergePendingDelay(tt.attempts)
			if got != tt.want {
				t.Errorf("computeAutoMergePendingDelay(%d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}

// logBuf captures structured log output for assertions.
type logBuf struct {
	buf buf
}

type buf struct {
	b []byte
}

func (b *buf) Write(p []byte) (int, error) {
	b.b = append(b.b, p...)
	return len(p), nil
}

func (l *logBuf) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *logBuf) contains(substr string) bool {
	return strings.Contains(string(l.buf.b), substr)
}

func (l *logBuf) reset() {
	l.buf.b = nil
}

// scopeVerifierAdapter wraps a controlledSCMAdapter and additionally
// implements AutoMergeScopeVerifier so the preflight retry path can run.
type scopeVerifierAdapter struct {
	scm       domain.SCMAdapter
	verifyErr error
}

var _ domain.SCMAdapter = (*scopeVerifierAdapter)(nil)
var _ AutoMergeScopeVerifier = (*scopeVerifierAdapter)(nil)

func (s *scopeVerifierAdapter) FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]domain.ReviewComment, error) {
	if s.scm != nil {
		return s.scm.FetchPendingReviews(ctx, prNumber, owner, repo)
	}
	return nil, nil
}

func (s *scopeVerifierAdapter) FetchBotReviewComments(ctx context.Context, prNumber int, owner, repo string, botUsernames []string) ([]domain.ReviewComment, error) {
	if s.scm != nil {
		return s.scm.FetchBotReviewComments(ctx, prNumber, owner, repo, botUsernames)
	}
	return []domain.ReviewComment{}, nil
}

func (s *scopeVerifierAdapter) GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (domain.ReviewDecision, error) {
	if s.scm != nil {
		return s.scm.GetReviewDecision(ctx, prNumber, owner, repo)
	}
	return domain.ReviewDecisionApproved, nil
}

func (s *scopeVerifierAdapter) GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error) {
	if s.scm != nil {
		return s.scm.GetCIStatus(ctx, prNumber, owner, repo)
	}
	return "success", nil
}

func (s *scopeVerifierAdapter) GetMergeability(ctx context.Context, prNumber int, owner, repo string) (domain.PRMergeStatus, error) {
	if s.scm != nil {
		return s.scm.GetMergeability(ctx, prNumber, owner, repo)
	}
	return domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: "sha-verifier"}, nil
}

func (s *scopeVerifierAdapter) MergePR(ctx context.Context, prNumber int, owner, repo string, strategy domain.MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (domain.MergeResult, error) {
	if s.scm != nil {
		return s.scm.MergePR(ctx, prNumber, owner, repo, strategy, commitTitle, commitMessage, expectedHeadSHA)
	}
	return domain.MergeResult{SHA: "merged-verifier"}, nil
}

func (s *scopeVerifierAdapter) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	if s.scm != nil {
		return s.scm.DeleteBranch(ctx, owner, repo, branch)
	}
	return nil
}

func (s *scopeVerifierAdapter) ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]domain.LabelEvent, error) {
	if s.scm != nil {
		return s.scm.ListLabelEvents(ctx, prNumber, owner, repo)
	}
	return nil, nil
}

func (s *scopeVerifierAdapter) RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error {
	if s.scm != nil {
		return s.scm.RemoveLabel(ctx, prNumber, owner, repo, label)
	}
	return nil
}

func (s *scopeVerifierAdapter) VerifyAutoMergeScopes(_ context.Context, _ bool) ([]string, []string, error) {
	if s.verifyErr != nil {
		return nil, nil, s.verifyErr
	}
	return []string{"repo"}, nil, nil
}
