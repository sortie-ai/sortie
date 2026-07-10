package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- PopulateRetries tests ---

func TestPopulateRetries(t *testing.T) {
	t.Parallel()

	errMsg := "simulated error"

	t.Run("populates state maps from persisted entries", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})

		entries := []persistence.PendingRetry{
			{
				Entry: persistence.RetryEntry{
					IssueID:    "id-1",
					Identifier: "PROJ-1",
					Attempt:    2,
					DueAtMs:    10000,
					Error:      &errMsg,
				},
				RemainingMs: 5000,
			},
			{
				Entry: persistence.RetryEntry{
					IssueID:    "id-2",
					Identifier: "PROJ-2",
					Attempt:    1,
					DueAtMs:    3000,
					Error:      nil,
				},
				RemainingMs: 0,
			},
			{
				Entry: persistence.RetryEntry{
					IssueID:    "id-3",
					Identifier: "PROJ-3",
					Attempt:    3,
					DueAtMs:    20000,
					Error:      &errMsg,
				},
				RemainingMs: 15000,
			},
		}

		PopulateRetries(state, entries, nil)

		if len(state.RetryAttempts) != 3 {
			t.Fatalf("RetryAttempts count = %d, want 3", len(state.RetryAttempts))
		}
		if len(state.Claimed) != 3 {
			t.Fatalf("Claimed count = %d, want 3", len(state.Claimed))
		}

		// Verify entry fields.
		for _, pending := range entries {
			e := pending.Entry
			got, ok := state.RetryAttempts[e.IssueID]
			if !ok {
				t.Errorf("RetryAttempts missing %q", e.IssueID)
				continue
			}
			if got.IssueID != e.IssueID {
				t.Errorf("IssueID = %q, want %q", got.IssueID, e.IssueID)
			}
			if got.Identifier != e.Identifier {
				t.Errorf("Identifier = %q, want %q", got.Identifier, e.Identifier)
			}
			if got.Attempt != e.Attempt {
				t.Errorf("Attempt = %d, want %d", got.Attempt, e.Attempt)
			}
			if got.DueAtMS != e.DueAtMs {
				t.Errorf("DueAtMS = %d, want %d", got.DueAtMS, e.DueAtMs)
			}
			if got.TimerHandle != nil {
				t.Errorf("TimerHandle should be nil for %q", e.IssueID)
			}
			if got.scheduledDelayMS != pending.RemainingMs {
				t.Errorf("scheduledDelayMS = %d, want %d", got.scheduledDelayMS, pending.RemainingMs)
			}
			if _, claimed := state.Claimed[e.IssueID]; !claimed {
				t.Errorf("issue %q should be claimed", e.IssueID)
			}
		}

		// Verify error field handling.
		if state.RetryAttempts["id-1"].Error != errMsg {
			t.Errorf("id-1 Error = %q, want %q", state.RetryAttempts["id-1"].Error, errMsg)
		}
		if state.RetryAttempts["id-2"].Error != "" {
			t.Errorf("id-2 Error = %q, want empty", state.RetryAttempts["id-2"].Error)
		}
	})

	t.Run("empty entries is no-op", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		PopulateRetries(state, nil, nil)

		if len(state.RetryAttempts) != 0 {
			t.Errorf("RetryAttempts count = %d, want 0", len(state.RetryAttempts))
		}
		if len(state.Claimed) != 0 {
			t.Errorf("Claimed count = %d, want 0", len(state.Claimed))
		}
	})
}

// --- Buffer sizing tests ---

func TestRetryTimerChBuffer_AccountsForPrePopulatedRetries(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})

	// Pre-populate 100 retry entries.
	for i := range 100 {
		id := "id-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		state.RetryAttempts[id] = &RetryEntry{IssueID: id}
		state.Claimed[id] = struct{}{}
	}

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
	})

	// max(4*2, 64, 100) = 100
	if cap(o.retryTimerCh) < 100 {
		t.Errorf("retryTimerCh cap = %d, want >= 100", cap(o.retryTimerCh))
	}
}

func TestRetryTimerChBuffer_DefaultWithoutRetries(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
	})

	// max(4*2, 64, 0) = 64
	if cap(o.retryTimerCh) != 64 {
		t.Errorf("retryTimerCh cap = %d, want 64", cap(o.retryTimerCh))
	}
}

// --- activateReconstructedRetries tests ---

func TestActivateReconstructedRetries(t *testing.T) {
	t.Parallel()

	t.Run("delay-0 entries sent to channel", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id-1"] = &RetryEntry{
			IssueID:          "id-1",
			TimerHandle:      nil,
			scheduledDelayMS: 0,
		}
		state.Claimed["id-1"] = struct{}{}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.activateReconstructedRetries()

		select {
		case id := <-o.retryTimerCh:
			if id != "id-1" {
				t.Errorf("retryTimerCh received %q, want %q", id, "id-1")
			}
		default:
			t.Fatal("retryTimerCh is empty, expected delay-0 entry")
		}
	})

	t.Run("future-delay entries get timer handle", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id-2"] = &RetryEntry{
			IssueID:          "id-2",
			TimerHandle:      nil,
			scheduledDelayMS: 60000,
		}
		state.Claimed["id-2"] = struct{}{}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.activateReconstructedRetries()

		entry := state.RetryAttempts["id-2"]
		if entry.TimerHandle == nil {
			t.Fatal("TimerHandle should be non-nil for future-delay entry")
		}
		entry.TimerHandle.Stop()

		// Channel should be empty (no immediate fire).
		select {
		case id := <-o.retryTimerCh:
			t.Errorf("retryTimerCh should be empty, got %q", id)
		default:
		}
	})

	t.Run("mixed entries handled correctly", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id-now"] = &RetryEntry{
			IssueID:          "id-now",
			TimerHandle:      nil,
			scheduledDelayMS: 0,
		}
		state.RetryAttempts["id-later"] = &RetryEntry{
			IssueID:          "id-later",
			TimerHandle:      nil,
			scheduledDelayMS: 30000,
		}
		state.Claimed["id-now"] = struct{}{}
		state.Claimed["id-later"] = struct{}{}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.activateReconstructedRetries()

		// id-now should be in channel.
		select {
		case id := <-o.retryTimerCh:
			if id != "id-now" {
				t.Errorf("retryTimerCh received %q, want %q", id, "id-now")
			}
		default:
			t.Fatal("retryTimerCh is empty, expected id-now")
		}

		// id-later should have a timer.
		if state.RetryAttempts["id-later"].TimerHandle == nil {
			t.Error("id-later should have non-nil TimerHandle")
		} else {
			state.RetryAttempts["id-later"].TimerHandle.Stop()
		}
	})

	t.Run("already-activated entries are skipped", func(t *testing.T) {
		t.Parallel()

		existingTimer := time.NewTimer(time.Hour)
		defer existingTimer.Stop()

		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id-active"] = &RetryEntry{
			IssueID:          "id-active",
			TimerHandle:      existingTimer,
			scheduledDelayMS: 5000,
		}
		state.Claimed["id-active"] = struct{}{}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.activateReconstructedRetries()

		// Timer should be unchanged.
		if state.RetryAttempts["id-active"].TimerHandle != existingTimer {
			t.Error("TimerHandle should not be replaced for already-active entry")
		}

		// Channel should be empty.
		select {
		case id := <-o.retryTimerCh:
			t.Errorf("retryTimerCh should be empty, got %q", id)
		default:
		}
	})

	t.Run("empty RetryAttempts is no-op", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.activateReconstructedRetries()

		select {
		case id := <-o.retryTimerCh:
			t.Errorf("retryTimerCh should be empty, got %q", id)
		default:
		}
	})
}

func TestPopulateRetries_SessionID(t *testing.T) {
	t.Parallel()

	sessID := "sess-abc"
	state := NewState(5000, 4, nil, AgentTotals{})
	entries := []persistence.PendingRetry{
		{
			Entry: persistence.RetryEntry{
				IssueID:    "id-sess",
				Identifier: "PROJ-SESS",
				Attempt:    1,
				DueAtMs:    10000,
				SessionID:  &sessID,
			},
			RemainingMs: 5000,
		},
	}

	PopulateRetries(state, entries, nil)

	got, ok := state.RetryAttempts["id-sess"]
	if !ok {
		t.Fatal("RetryAttempts[id-sess] missing after PopulateRetries")
	}
	if got.SessionID != sessID {
		t.Errorf("PopulateRetries_SessionID: SessionID = %q, want %q", got.SessionID, sessID)
	}
}

func TestPopulateRetries_SessionID_Nil(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	entries := []persistence.PendingRetry{
		{
			Entry: persistence.RetryEntry{
				IssueID:    "id-nosess",
				Identifier: "PROJ-NOSESS",
				Attempt:    1,
				DueAtMs:    10000,
				SessionID:  nil,
			},
			RemainingMs: 0,
		},
	}

	PopulateRetries(state, entries, nil)

	got, ok := state.RetryAttempts["id-nosess"]
	if !ok {
		t.Fatal("RetryAttempts[id-nosess] missing after PopulateRetries")
	}
	if got.SessionID != "" {
		t.Errorf("PopulateRetries_SessionID_Nil: SessionID = %q, want empty", got.SessionID)
	}
}

// --- RecoverPendingReactions tests ---

// recoveryNow is a fixed instant used as the NowFunc reference for recovery tests.
var recoveryNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// writeRecoverySCM writes meta as .sortie/scm.json inside <wsRoot>/<key>.
// The directory is created if absent. key is the sanitized identifier (e.g. "PROJ-1").
func writeRecoverySCM(t *testing.T, wsRoot, key string, meta domain.SCMMetadata) {
	t.Helper()
	dir := filepath.Join(wsRoot, key, ".sortie")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("writeSCMMetadata MkdirAll: %v", err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("writeSCMMetadata Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scm.json"), data, 0o644); err != nil {
		t.Fatalf("writeSCMMetadata WriteFile: %v", err)
	}
}

// freshSCMTime returns an RFC3339 timestamp that is within the 30-day lookback.
func freshSCMTime(daysAgo int) string {
	return recoveryNow.Add(-time.Duration(daysAgo) * 24 * time.Hour).UTC().Format(time.RFC3339)
}

// freshRun builds a persistence.RunHistory for recovery tests.
func freshRun(issueID, identifier, displayID string, attempt int) persistence.RunHistory {
	return persistence.RunHistory{
		IssueID:      issueID,
		Identifier:   identifier,
		DisplayID:    displayID,
		Attempt:      attempt,
		AgentAdapter: "mock",
		Workspace:    "/ws/" + identifier,
		StartedAt:    freshSCMTime(2),
		CompletedAt:  freshSCMTime(2),
		Status:       "succeeded",
	}
}

// recoveryTrackerStub satisfies domain.TrackerAdapter; returns a configurable state map.
type recoveryTrackerStub struct {
	states     map[string]string
	err        error
	fetchedIDs []string
}

var _ domain.TrackerAdapter = (*recoveryTrackerStub)(nil)

func (s *recoveryTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *recoveryTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *recoveryTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *recoveryTrackerStub) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	s.fetchedIDs = append(s.fetchedIDs, ids...)
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		if st, ok := s.states[id]; ok {
			result[id] = st
		}
	}
	return result, nil
}
func (s *recoveryTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *recoveryTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *recoveryTrackerStub) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (s *recoveryTrackerStub) CommentIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (s *recoveryTrackerStub) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

// panicSCMAdapter panics if any method is called, asserting recovery makes no SCM calls.
type panicSCMAdapter struct{}

var _ domain.SCMAdapter = (*panicSCMAdapter)(nil)

func (p *panicSCMAdapter) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	panic("FetchPendingReviews must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	panic("FetchBotReviewComments must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	panic("GetReviewDecision must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	panic("GetCIStatus must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	panic("GetMergeability must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	panic("MergePR must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) DeleteBranch(_ context.Context, _, _, _ string) error {
	panic("DeleteBranch must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	panic("ListLabelEvents must not be called during RecoverPendingReactions")
}

func (p *panicSCMAdapter) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	panic("RemoveLabel must not be called during RecoverPendingReactions")
}

// panicCIProvider panics if FetchCIStatus is called, asserting recovery makes no CI fetch.
type panicCIProvider struct{}

var _ domain.CIStatusProvider = (*panicCIProvider)(nil)

func (p *panicCIProvider) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	panic("FetchCIStatus must not be called during RecoverPendingReactions")
}

// stubSCMForRecovery is a non-nil SCMAdapter that satisfies the interface without panicking,
// used when a non-nil adapter is required for the recovery guard but the fetch must not fire.
type stubSCMForRecovery struct{}

var _ domain.SCMAdapter = (*stubSCMForRecovery)(nil)

func (s *stubSCMForRecovery) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	return nil, nil
}

func (s *stubSCMForRecovery) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	return []domain.ReviewComment{}, nil
}

func (s *stubSCMForRecovery) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}

func (s *stubSCMForRecovery) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}

func (s *stubSCMForRecovery) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{}, nil
}

func (s *stubSCMForRecovery) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}

func (s *stubSCMForRecovery) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *stubSCMForRecovery) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}

func (s *stubSCMForRecovery) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// stubCIForRecovery is a non-nil CIStatusProvider for recovery guard, never called during recovery.
type stubCIForRecovery struct{}

var _ domain.CIStatusProvider = (*stubCIForRecovery)(nil)

func (s *stubCIForRecovery) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	return domain.CIResult{}, nil
}

// defaultRecoveryParams builds PendingReactionRecoveryParams with sensible defaults for tests.
func defaultRecoveryParams(wsRoot string, tracker domain.TrackerAdapter) PendingReactionRecoveryParams {
	return PendingReactionRecoveryParams{
		WorkspaceRoot:    wsRoot,
		TrackerAdapter:   tracker,
		HandoffState:     "In Review",
		TerminalStates:   []string{"Done", "Closed"},
		SCMAdapter:       &stubSCMForRecovery{},
		CIProvider:       &stubCIForRecovery{},
		RecoveryLookback: PendingReactionRecoveryLookback,
		MaxCandidates:    PendingReactionRecoveryMaxCandidates,
		NowFunc:          func() time.Time { return recoveryNow },
		Logger:           discardLogger(),
	}
}

func TestRecoverPendingReactions_RecreatesReviewAfterRestart(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-1", domain.SCMMetadata{
		Branch:   "feature/fix",
		SHA:      "abc123",
		PushedAt: freshSCMTime(1),
		PRNumber: 42,
		Owner:    "owner",
		Repo:     "repo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-1", "PROJ-1", "owner/repo#42", 2)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 1 {
		t.Errorf("ReviewRecovered = %d, want 1", result.ReviewRecovered)
	}

	rkey := ReactionKey("ISS-1", ReactionKindReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	if pr.IssueID != "ISS-1" {
		t.Errorf("PendingReaction.IssueID = %q, want %q", pr.IssueID, "ISS-1")
	}
	if pr.Attempt != 2 {
		t.Errorf("PendingReaction.Attempt = %d, want 2", pr.Attempt)
	}
	if pr.DisplayID != "owner/repo#42" {
		t.Errorf("PendingReaction.DisplayID = %q, want %q", pr.DisplayID, "owner/repo#42")
	}
	rd, ok := pr.KindData.(*ReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *ReviewReactionData", pr.KindData)
	}
	if rd.PRNumber != 42 {
		t.Errorf("ReviewReactionData.PRNumber = %d, want 42", rd.PRNumber)
	}
	if rd.Owner != "owner" {
		t.Errorf("ReviewReactionData.Owner = %q, want %q", rd.Owner, "owner")
	}
	if rd.Repo != "repo" {
		t.Errorf("ReviewReactionData.Repo = %q, want %q", rd.Repo, "repo")
	}
	if rd.Branch != "feature/fix" {
		t.Errorf("ReviewReactionData.Branch = %q, want %q", rd.Branch, "feature/fix")
	}
	if rd.SHA != "abc123" {
		t.Errorf("ReviewReactionData.SHA = %q, want %q", rd.SHA, "abc123")
	}
	// Issue must not be claimed.
	if _, claimed := state.Claimed["ISS-1"]; claimed {
		t.Error("ISS-1 found in state.Claimed after recovery, want not claimed")
	}
}

func TestRecoverPendingReactions_RecreatesCIAfterRestart(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-2", domain.SCMMetadata{
		Branch:   "feature/ci-fix",
		SHA:      "deadbeef",
		PushedAt: freshSCMTime(1),
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-2", "PROJ-2", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.CIRecovered != 1 {
		t.Errorf("CIRecovered = %d, want 1", result.CIRecovered)
	}

	rkey := ReactionKey("ISS-2", ReactionKindCI)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	ci, ok := pr.KindData.(*CIReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *CIReactionData", pr.KindData)
	}
	if ci.Branch != "feature/ci-fix" {
		t.Errorf("CIReactionData.Branch = %q, want %q", ci.Branch, "feature/ci-fix")
	}
	if ci.SHA != "deadbeef" {
		t.Errorf("CIReactionData.SHA = %q, want %q", ci.SHA, "deadbeef")
	}
	// Issue must not be claimed.
	if _, claimed := state.Claimed["ISS-2"]; claimed {
		t.Error("ISS-2 found in state.Claimed after CI recovery, want not claimed")
	}
}

func TestRecoverPendingReactions_SkipsWhenHandoffStateEmpty(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-3": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-3", "PROJ-3", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.HandoffState = ""

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 || result.CIRecovered != 0 {
		t.Errorf("recovered = review:%d ci:%d, want 0/0 when HandoffState is empty",
			result.ReviewRecovered, result.CIRecovered)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
	if len(tracker.fetchedIDs) != 0 {
		t.Error("FetchIssueStatesByIDs was called with empty HandoffState, want no call")
	}
}

func TestRecoverPendingReactions_SkipsWhenProvidersNil(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-4": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-4", "PROJ-4", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.SCMAdapter = nil
	params.CIProvider = nil

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 || result.CIRecovered != 0 {
		t.Errorf("recovered = review:%d ci:%d, want 0/0 when both providers are nil",
			result.ReviewRecovered, result.CIRecovered)
	}
	if len(tracker.fetchedIDs) != 0 {
		t.Error("FetchIssueStatesByIDs was called with nil providers, want no call")
	}
}

func TestRecoverPendingReactions_SkipsNonHandoffState(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-5", domain.SCMMetadata{
		Branch:   "feature/x",
		PushedAt: freshSCMTime(1),
		PRNumber: 10,
		Owner:    "o",
		Repo:     "r",
	})

	// Tracker returns "In Progress" — not the handoff state "In Review".
	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-5": "In Progress"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-5", "PROJ-5", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 {
		t.Errorf("ReviewRecovered = %d, want 0 for non-handoff state", result.ReviewRecovered)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_SkipsTerminalState(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-6", domain.SCMMetadata{
		Branch:   "feature/done",
		PushedAt: freshSCMTime(1),
		PRNumber: 99,
		Owner:    "o",
		Repo:     "r",
	})

	// Tracker returns "Done" — a configured terminal state.
	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-6": "Done"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-6", "PROJ-6", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 {
		t.Errorf("ReviewRecovered = %d, want 0 for terminal state", result.ReviewRecovered)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_SkipsClaimedOrRetryingIssue(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	for _, id := range []string{"PROJ-7", "PROJ-8", "PROJ-9"} {
		writeRecoverySCM(t, wsRoot, id, domain.SCMMetadata{
			Branch: "feature/x", PushedAt: freshSCMTime(1), PRNumber: 1, Owner: "o", Repo: "r",
		})
	}

	tracker := &recoveryTrackerStub{states: map[string]string{
		"ISS-CLAIMED": "In Review",
		"ISS-RETRY":   "In Review",
		"ISS-RUNNING": "In Review",
	}}
	state := NewState(5000, 4, nil, AgentTotals{})
	// Seed claimed, retry, and running.
	state.Claimed["ISS-CLAIMED"] = struct{}{}
	state.RetryAttempts["ISS-RETRY"] = &RetryEntry{IssueID: "ISS-RETRY", Identifier: "PROJ-8"}
	state.Claimed["ISS-RETRY"] = struct{}{}
	state.Running["ISS-RUNNING"] = &RunningEntry{}

	runs := []persistence.RunHistory{
		freshRun("ISS-CLAIMED", "PROJ-7", "", 1),
		freshRun("ISS-RETRY", "PROJ-8", "", 1),
		freshRun("ISS-RUNNING", "PROJ-9", "", 1),
	}
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, runs, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 {
		t.Errorf("ReviewRecovered = %d, want 0 for claimed/retry/running issues", result.ReviewRecovered)
	}
	// Retry state unchanged.
	if _, ok := state.RetryAttempts["ISS-RETRY"]; !ok {
		t.Error("state.RetryAttempts[ISS-RETRY] was removed by recovery, want unchanged")
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_LimitsTrackerFetchCandidates(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	// Generate 250 runs; recovery should cap at PendingReactionRecoveryMaxCandidates (200).
	const total = 250
	runs := make([]persistence.RunHistory, total)
	stateMap := make(map[string]string, total)
	for i := range total {
		issueID := fmt.Sprintf("ISS-%04d", i)
		identifier := fmt.Sprintf("PROJ-%04d", i)
		runs[i] = freshRun(issueID, identifier, "", 1)
		stateMap[issueID] = "In Review"
		writeRecoverySCM(t, wsRoot, identifier, domain.SCMMetadata{
			Branch:   "feature/x",
			PushedAt: freshSCMTime(1),
			PRNumber: i + 1,
			Owner:    "o",
			Repo:     "r",
		})
	}

	tracker := &recoveryTrackerStub{states: stateMap}
	state := NewState(5000, 4, nil, AgentTotals{})
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, runs, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.CapSkipped != total-PendingReactionRecoveryMaxCandidates {
		t.Errorf("CapSkipped = %d, want %d", result.CapSkipped, total-PendingReactionRecoveryMaxCandidates)
	}
	if len(tracker.fetchedIDs) > PendingReactionRecoveryMaxCandidates {
		t.Errorf("FetchIssueStatesByIDs received %d IDs, want <= %d",
			len(tracker.fetchedIDs), PendingReactionRecoveryMaxCandidates)
	}
}

func TestRecoverPendingReactions_SkipsStaleSCMActivity(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	// Stale pushed_at (> 30 days ago).
	writeRecoverySCM(t, wsRoot, "PROJ-STALE", domain.SCMMetadata{
		Branch:   "feature/stale",
		PushedAt: freshSCMTime(35),
		PRNumber: 1,
		Owner:    "o",
		Repo:     "r",
	})
	// Malformed pushed_at.
	writeRecoverySCM(t, wsRoot, "PROJ-MALFORMED", domain.SCMMetadata{
		Branch:   "feature/malformed",
		PushedAt: "not-a-date",
		PRNumber: 2,
		Owner:    "o",
		Repo:     "r",
	})
	// Absent pushed_at + stale completed_at (fallback).
	runStaleCompleted := freshRun("ISS-STALE-COMPLETED", "PROJ-STALE-COMPLETED", "", 1)
	runStaleCompleted.CompletedAt = freshSCMTime(35)
	writeRecoverySCM(t, wsRoot, "PROJ-STALE-COMPLETED", domain.SCMMetadata{
		Branch:   "feature/stale-completed",
		PRNumber: 3,
		Owner:    "o",
		Repo:     "r",
		// PushedAt empty
	})

	tracker := &recoveryTrackerStub{states: map[string]string{
		"ISS-STALE":           "In Review",
		"ISS-MALFORMED":       "In Review",
		"ISS-STALE-COMPLETED": "In Review",
	}}
	state := NewState(5000, 4, nil, AgentTotals{})
	runs := []persistence.RunHistory{
		freshRun("ISS-STALE", "PROJ-STALE", "", 1),
		freshRun("ISS-MALFORMED", "PROJ-MALFORMED", "", 1),
		runStaleCompleted,
	}
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, runs, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.StaleSkipped != 3 {
		t.Errorf("StaleSkipped = %d, want 3 (stale pushed_at, malformed pushed_at, stale completed_at)", result.StaleSkipped)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_DoesNotOverwriteExistingReview(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-EXISTING", domain.SCMMetadata{
		Branch:   "feature/new",
		PushedAt: freshSCMTime(1),
		PRNumber: 77,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-EXISTING": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	// Pre-populate a pending reaction — recovery must not overwrite it.
	existing := &PendingReaction{
		IssueID: "ISS-EXISTING", Kind: ReactionKindReview,
		KindData: &ReviewReactionData{PRNumber: 9999, Owner: "old", Repo: "old", Branch: "old"},
	}
	state.PendingReactions[ReactionKey("ISS-EXISTING", ReactionKindReview)] = existing

	run := freshRun("ISS-EXISTING", "PROJ-EXISTING", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 {
		t.Errorf("ReviewRecovered = %d, want 0 (existing entry preserved)", result.ReviewRecovered)
	}

	rkey := ReactionKey("ISS-EXISTING", ReactionKindReview)
	got := state.PendingReactions[rkey]
	if got != existing {
		t.Error("existing PendingReaction pointer was replaced by recovery; want unchanged")
	}
	rd := got.KindData.(*ReviewReactionData)
	if rd.PRNumber != 9999 {
		t.Errorf("PRNumber = %d, want 9999 (original value preserved)", rd.PRNumber)
	}
}

func TestRecoverPendingReactions_ProviderFetchesNotCalled(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-NOFETCH", domain.SCMMetadata{
		Branch:   "feature/nofetch",
		PushedAt: freshSCMTime(1),
		PRNumber: 5,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-NOFETCH": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-NOFETCH", "PROJ-NOFETCH", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	// Use panic adapters to assert no fetch methods are called during recovery.
	params.SCMAdapter = &panicSCMAdapter{}
	params.CIProvider = &panicCIProvider{}

	// Must not panic; FetchPendingReviews / FetchCIStatus are not called.
	_, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
}

func TestRecoverPendingReactions_InvalidSCMMetadataSkips(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	// No .sortie/scm.json written — ReadSCMMetadata returns zero value.

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-NOMETA": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-NOMETA", "PROJ-NOMETA", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 0 || result.CIRecovered != 0 {
		t.Errorf("recovered = review:%d ci:%d, want 0/0 for missing scm.json",
			result.ReviewRecovered, result.CIRecovered)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_TrackerFetchError(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-FETCHERR", domain.SCMMetadata{
		Branch:   "feature/err",
		PushedAt: freshSCMTime(1),
		PRNumber: 3,
		Owner:    "o",
		Repo:     "r",
	})

	fetchErr := errors.New("tracker unavailable")
	tracker := &recoveryTrackerStub{err: fetchErr}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-FETCHERR", "PROJ-FETCHERR", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err == nil {
		t.Fatal("RecoverPendingReactions tracker fetch error = nil, want error")
	}
	if !errors.Is(err, fetchErr) {
		t.Errorf("RecoverPendingReactions err = %v, want wrapping %v", err, fetchErr)
	}
	if result.ReviewRecovered != 0 || result.CIRecovered != 0 {
		t.Errorf("recovered = review:%d ci:%d, want 0/0 on tracker fetch error",
			result.ReviewRecovered, result.CIRecovered)
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions len = %d, want 0 on tracker fetch error", len(state.PendingReactions))
	}
}

func TestRecoverPendingReactions_DispatchedFingerprintStillDedups(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-DEDUP", domain.SCMMetadata{
		Branch:   "feature/dedup",
		PushedAt: freshSCMTime(1),
		PRNumber: 7,
		Owner:    "owner",
		Repo:     "repo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-DEDUP": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-DEDUP", "PROJ-DEDUP", "owner/repo#7", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	// Recover the review reaction.
	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 1 {
		t.Fatalf("ReviewRecovered = %d, want 1", result.ReviewRecovered)
	}

	// Seed the store so the fingerprint appears already dispatched.
	comments := []domain.ReviewComment{
		{ID: "fp-1", Body: "lgtm", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	fp := buildReviewFingerprint(comments)
	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}

	// Run reconcile with the already-dispatched fingerprint.
	scm := &mockSCMAdapter{comments: comments}
	reconcileParams := reviewParams(store, scm, nil)
	reconcileReviewComments(state, reconcileParams, discardLogger(), context.Background(), newReviewMetricsSpy())

	// The entry should be re-enqueued (fingerprint already dispatched), not trigger a new dispatch.
	rkey := ReactionKey("ISS-DEDUP", ReactionKindReview)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[ISS-DEDUP:review] removed for dispatched fingerprint; want re-enqueued")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (already dispatched)", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-DEDUP"]; ok {
		t.Error("retry scheduled for already-dispatched fingerprint; want none")
	}
}

func TestRecoverPendingReactions_RecoveredReviewDispatchesContinuation(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-DISPATCH", domain.SCMMetadata{
		Branch:   "feature/cont",
		PushedAt: freshSCMTime(1),
		PRNumber: 11,
		Owner:    "owner",
		Repo:     "repo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-DISPATCH": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-DISPATCH", "PROJ-DISPATCH", "owner/repo#11", 1)
	params := defaultRecoveryParams(wsRoot, tracker)

	// Recover.
	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.ReviewRecovered != 1 {
		t.Fatalf("ReviewRecovered = %d, want 1", result.ReviewRecovered)
	}

	// Issue was NOT claimed before reconcile.
	if _, claimed := state.Claimed["ISS-DISPATCH"]; claimed {
		t.Error("ISS-DISPATCH claimed before reconcile; want not claimed at recovery time")
	}

	// New comment outside the debounce window.
	comments := []domain.ReviewComment{
		{ID: "c-new", Body: "please fix", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	scm := &mockSCMAdapter{comments: comments}
	reconcileParams := reviewParams(store, scm, nil)

	reconcileReviewComments(state, reconcileParams, discardLogger(), context.Background(), newReviewMetricsSpy())

	// After dispatch, entry removed from pending reactions and a retry is scheduled.
	rkey := ReactionKey("ISS-DISPATCH", ReactionKindReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[ISS-DISPATCH:review] present after dispatch; want consumed")
	}
	retry, ok := state.RetryAttempts["ISS-DISPATCH"]
	if !ok {
		t.Fatal("RetryAttempts[ISS-DISPATCH] missing after review dispatch; want scheduled")
	}
	if retry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want review_comments map")
	}
	if retry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindReview)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
	}
}

// --- Auto-merge recovery tests ---

// TestRecoverPendingReactions_RecoversAutoMergeKindWhenConfigured verifies that
// a run history entry with full PR metadata is recovered as a merge-kind
// PendingReaction when AutoMergeReactionConfigured is true.
func TestRecoverPendingReactions_RecoversAutoMergeKindWhenConfigured(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-AM1", domain.SCMMetadata{
		Branch:   "feature/am-1",
		SHA:      "deadbeef",
		PushedAt: freshSCMTime(1),
		PRNumber: 77,
		Owner:    "corp",
		Repo:     "api",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-AM1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-AM1", "PROJ-AM1", "corp/api#77", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.AutoMergeReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}

	if result.AutoMergeRecovered != 1 {
		t.Fatalf("AutoMergeRecovered = %d, want 1", result.AutoMergeRecovered)
	}

	rkey := ReactionKey("ISS-AM1", ReactionKindAutoMerge)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[ISS-AM1:merge] missing after recovery; want present")
	}

	mergeData, ok := pr.KindData.(*AutoMergeReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *AutoMergeReactionData", pr.KindData)
	}
	if mergeData.PRNumber != 77 {
		t.Errorf("AutoMergeReactionData.PRNumber = %d, want 77", mergeData.PRNumber)
	}
	if mergeData.Owner != "corp" {
		t.Errorf("AutoMergeReactionData.Owner = %q, want %q", mergeData.Owner, "corp")
	}
	if mergeData.Repo != "api" {
		t.Errorf("AutoMergeReactionData.Repo = %q, want %q", mergeData.Repo, "api")
	}
	if mergeData.Branch != "feature/am-1" {
		t.Errorf("AutoMergeReactionData.Branch = %q, want %q", mergeData.Branch, "feature/am-1")
	}
}

// TestRecoverPendingReactions_SkipsAutoMergeWhenNotConfigured verifies that no
// merge-kind PendingReaction is created when AutoMergeReactionConfigured is
// false, even with full PR metadata present (spec Test 6).
func TestRecoverPendingReactions_SkipsAutoMergeWhenNotConfigured(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-AM2", domain.SCMMetadata{
		Branch:   "feature/am-2",
		SHA:      "cafebabe",
		PushedAt: freshSCMTime(1),
		PRNumber: 88,
		Owner:    "corp",
		Repo:     "api",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-AM2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-AM2", "PROJ-AM2", "corp/api#88", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.AutoMergeReactionConfigured = false // not configured

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}

	if result.AutoMergeRecovered != 0 {
		t.Errorf("AutoMergeRecovered = %d, want 0 when not configured", result.AutoMergeRecovered)
	}

	rkey := ReactionKey("ISS-AM2", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[ISS-AM2:merge] present despite AutoMergeReactionConfigured=false")
	}
}

// TestRecoverPendingReactions_SkipsAutoMergeWhenMissingPRMetadata verifies
// that recovery skips the merge-kind entry when SCM metadata is incomplete.
func TestRecoverPendingReactions_SkipsAutoMergeWhenMissingPRMetadata(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	// Write minimal SCM metadata with no PR fields.
	writeRecoverySCM(t, wsRoot, "PROJ-AM3", domain.SCMMetadata{
		Branch:   "feature/no-pr",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		// PRNumber intentionally omitted (zero).
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-AM3": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-AM3", "PROJ-AM3", "no-pr", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.AutoMergeReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}

	if result.AutoMergeRecovered != 0 {
		t.Errorf("AutoMergeRecovered = %d, want 0 when PR metadata missing", result.AutoMergeRecovered)
	}

	rkey := ReactionKey("ISS-AM3", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[ISS-AM3:merge] present despite missing PR metadata")
	}
}

// --- bot-review startup recovery tests (6.6 → R11) ---

// TestRecoverPendingReactions_RecreatesBotReviewAfterRestart verifies R11:
// a run_history row plus SCMMetadata with full PR identity and
// BotReviewReactionConfigured=true reconstructs a bot-review PendingReaction
// and increments BotReviewRecovered.
func TestRecoverPendingReactions_RecreatesBotReviewAfterRestart(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BR1", domain.SCMMetadata{
		Branch:   "feature/bot-fix",
		SHA:      "deadcafe",
		PushedAt: freshSCMTime(1),
		PRNumber: 55,
		Owner:    "botowner",
		Repo:     "botrepo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BR1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BR1", "PROJ-BR1", "botowner/botrepo#55", 3)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 1 {
		t.Errorf("BotReviewRecovered = %d, want 1", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BR1", ReactionKindBotReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	if pr.Kind != ReactionKindBotReview {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindBotReview)
	}
	if pr.IssueID != "ISS-BR1" {
		t.Errorf("PendingReaction.IssueID = %q, want %q", pr.IssueID, "ISS-BR1")
	}
	if pr.Attempt != 3 {
		t.Errorf("PendingReaction.Attempt = %d, want 3", pr.Attempt)
	}
	brd, ok := pr.KindData.(*BotReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *BotReviewReactionData", pr.KindData)
	}
	if brd.PRNumber != 55 {
		t.Errorf("BotReviewReactionData.PRNumber = %d, want 55", brd.PRNumber)
	}
	if brd.Owner != "botowner" {
		t.Errorf("BotReviewReactionData.Owner = %q, want %q", brd.Owner, "botowner")
	}
	if brd.Repo != "botrepo" {
		t.Errorf("BotReviewReactionData.Repo = %q, want %q", brd.Repo, "botrepo")
	}
	if brd.Branch != "feature/bot-fix" {
		t.Errorf("BotReviewReactionData.Branch = %q, want %q", brd.Branch, "feature/bot-fix")
	}
	if brd.SHA != "deadcafe" {
		t.Errorf("BotReviewReactionData.SHA = %q, want %q", brd.SHA, "deadcafe")
	}
	// Issue must not be claimed.
	if _, claimed := state.Claimed["ISS-BR1"]; claimed {
		t.Error("ISS-BR1 found in state.Claimed after recovery, want not claimed")
	}
}

// TestRecoverPendingReactions_BotReviewNotRecoveredWhenFlagFalse verifies R11:
// BotReviewReactionConfigured=false → no bot-review entry is reconstructed, even
// with full PR metadata present.
func TestRecoverPendingReactions_BotReviewNotRecoveredWhenFlagFalse(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BR2", domain.SCMMetadata{
		Branch:   "feature/bot-disabled",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 10,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BR2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BR2", "PROJ-BR2", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = false // not configured

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 0 {
		t.Errorf("BotReviewRecovered = %d, want 0 when flag is false", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BR2", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("bot-review PendingReactions entry created with BotReviewReactionConfigured=false; want absent")
	}
}

// TestRecoverPendingReactions_BotReviewMissingPRNumber verifies R11: a row missing
// PRNumber (zero) recovers no bot-review entry even when BotReviewReactionConfigured=true.
func TestRecoverPendingReactions_BotReviewMissingPRNumber(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BRNPR", domain.SCMMetadata{
		Branch:   "feature/no-pr",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		// PRNumber intentionally zero.
		Owner: "o",
		Repo:  "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BRNPR": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BRNPR", "PROJ-BRNPR", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 0 {
		t.Errorf("BotReviewRecovered = %d, want 0 for missing PRNumber", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BRNPR", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("bot-review PendingReactions entry created with zero PRNumber; want absent")
	}
}

// TestRecoverPendingReactions_BotReviewMissingOwner verifies R11: a row missing
// Owner recovers no bot-review entry.
func TestRecoverPendingReactions_BotReviewMissingOwner(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BRNOWN", domain.SCMMetadata{
		Branch:   "feature/no-owner",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 5,
		// Owner intentionally empty.
		Repo: "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BRNOWN": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BRNOWN", "PROJ-BRNOWN", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 0 {
		t.Errorf("BotReviewRecovered = %d, want 0 for missing Owner", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BRNOWN", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("bot-review PendingReactions entry created with empty Owner; want absent")
	}
}

// TestRecoverPendingReactions_BotReviewMissingRepo verifies R11: a row missing
// Repo recovers no bot-review entry.
func TestRecoverPendingReactions_BotReviewMissingRepo(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BRNREP", domain.SCMMetadata{
		Branch:   "feature/no-repo",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 5,
		Owner:    "o",
		// Repo intentionally empty.
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BRNREP": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BRNREP", "PROJ-BRNREP", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 0 {
		t.Errorf("BotReviewRecovered = %d, want 0 for missing Repo", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BRNREP", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("bot-review PendingReactions entry created with empty Repo; want absent")
	}
}

// TestRecoverPendingReactions_BotReviewMissingBranch verifies R11: a row missing
// Branch recovers no bot-review entry.
func TestRecoverPendingReactions_BotReviewMissingBranch(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-BRNBRA", domain.SCMMetadata{
		// Branch intentionally empty.
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 5,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-BRNBRA": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-BRNBRA", "PROJ-BRNBRA", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.BotReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.BotReviewRecovered != 0 {
		t.Errorf("BotReviewRecovered = %d, want 0 for missing Branch", result.BotReviewRecovered)
	}

	rkey := ReactionKey("ISS-BRNBRA", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("bot-review PendingReactions entry created with empty Branch; want absent")
	}
}

// --- merge-conflict recovery tests ---

func TestRecoverPendingReactions_MergeConflict(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-MC1", domain.SCMMetadata{
		Branch:   "feature/mc-fix",
		SHA:      "facefeed",
		PushedAt: freshSCMTime(1),
		PRNumber: 55,
		Owner:    "mcowner",
		Repo:     "mcrepo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-MC1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-MC1", "PROJ-MC1", "mcowner/mcrepo#55", 3)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.MergeConflictReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.MergeConflictRecovered != 1 {
		t.Errorf("MergeConflictRecovered = %d, want 1", result.MergeConflictRecovered)
	}

	rkey := ReactionKey("ISS-MC1", ReactionKindMergeConflict)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	if pr.Kind != ReactionKindMergeConflict {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindMergeConflict)
	}
	if pr.Attempt != 3 {
		t.Errorf("PendingReaction.Attempt = %d, want 3", pr.Attempt)
	}
	mcd, ok := pr.KindData.(*MergeConflictReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *MergeConflictReactionData", pr.KindData)
	}
	if mcd.PRNumber != 55 {
		t.Errorf("MergeConflictReactionData.PRNumber = %d, want 55", mcd.PRNumber)
	}
	if mcd.Owner != "mcowner" {
		t.Errorf("MergeConflictReactionData.Owner = %q, want %q", mcd.Owner, "mcowner")
	}
	if mcd.Repo != "mcrepo" {
		t.Errorf("MergeConflictReactionData.Repo = %q, want %q", mcd.Repo, "mcrepo")
	}
	if mcd.Branch != "feature/mc-fix" {
		t.Errorf("MergeConflictReactionData.Branch = %q, want %q", mcd.Branch, "feature/mc-fix")
	}
	if mcd.SHA != "facefeed" {
		t.Errorf("MergeConflictReactionData.SHA = %q, want %q", mcd.SHA, "facefeed")
	}
	// Issue must not be claimed by recovery.
	if _, claimed := state.Claimed["ISS-MC1"]; claimed {
		t.Error("ISS-MC1 found in state.Claimed after recovery, want not claimed")
	}
}

// TestRecoverPendingReactions_MergeConflictNotRecoveredWhenFlagFalse verifies
// that MergeConflictReactionConfigured=false reconstructs no merge-conflict
// entry, even with full PR metadata present.
func TestRecoverPendingReactions_MergeConflictNotRecoveredWhenFlagFalse(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-MC2", domain.SCMMetadata{
		Branch:   "feature/mc-disabled",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 10,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-MC2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-MC2", "PROJ-MC2", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.MergeConflictReactionConfigured = false // not configured

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.MergeConflictRecovered != 0 {
		t.Errorf("MergeConflictRecovered = %d, want 0 when flag is false", result.MergeConflictRecovered)
	}

	rkey := ReactionKey("ISS-MC2", ReactionKindMergeConflict)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("merge-conflict PendingReactions entry created with MergeConflictReactionConfigured=false; want absent")
	}
}

// TestRecoverPendingReactions_LabelReview verifies that a handoff-state
// issue whose recovered SCM metadata carries a PR number, owner, and repo
// recovers one label-review pending entry with the frozen dispatch fields
// and increments LabelReviewRecovered.
//
// The fixture includes a branch even though the label-review recovery
// clause itself imposes no branch requirement (recovery.go's per-kind
// clause omits the meta.Branch != "" guard the sibling kinds carry):
// workspace.ReadSCMMetadata unconditionally returns a zero value whenever
// the decoded branch is empty, and RecoverPendingReactions skips the whole
// run when that zero value comes back, before any per-kind clause runs. A
// workspace's scm.json is written only by a normal (non-read-only)
// session, and such a session always operates on a branch, so this
// reflects the only reachable production case.
func TestRecoverPendingReactions_LabelReview(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-LR1", domain.SCMMetadata{
		Branch:   "feature/lr-fix",
		SHA:      "facefeed",
		PushedAt: freshSCMTime(1),
		PRNumber: 55,
		Owner:    "lrowner",
		Repo:     "lrrepo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-LR1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-LR1", "PROJ-LR1", "lrowner/lrrepo#55", 2)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.LabelReviewReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.LabelReviewRecovered != 1 {
		t.Errorf("LabelReviewRecovered = %d, want 1", result.LabelReviewRecovered)
	}

	rkey := ReactionKey("ISS-LR1", ReactionKindLabelReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	if pr.Kind != ReactionKindLabelReview {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindLabelReview)
	}
	if pr.Attempt != 2 {
		t.Errorf("PendingReaction.Attempt = %d, want 2", pr.Attempt)
	}
	if pr.AgentKind != run.AgentAdapter {
		t.Errorf("PendingReaction.AgentKind = %q, want %q (frozen from the recovered run)", pr.AgentKind, run.AgentAdapter)
	}
	lrd, ok := pr.KindData.(*LabelReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelReviewReactionData", pr.KindData)
	}
	if lrd.PRNumber != 55 {
		t.Errorf("LabelReviewReactionData.PRNumber = %d, want 55", lrd.PRNumber)
	}
	if lrd.Owner != "lrowner" {
		t.Errorf("LabelReviewReactionData.Owner = %q, want %q", lrd.Owner, "lrowner")
	}
	if lrd.Repo != "lrrepo" {
		t.Errorf("LabelReviewReactionData.Repo = %q, want %q", lrd.Repo, "lrrepo")
	}
	if _, claimed := state.Claimed["ISS-LR1"]; claimed {
		t.Error("ISS-LR1 found in state.Claimed after recovery, want not claimed")
	}
}

// TestRecoverPendingReactions_LabelReviewNotRecoveredWhenFlagFalse verifies
// that LabelReviewReactionConfigured=false reconstructs no label-review
// entry, even with full PR metadata present.
func TestRecoverPendingReactions_LabelReviewNotRecoveredWhenFlagFalse(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-LR2", domain.SCMMetadata{
		Branch:   "feature/lr-disabled",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 10,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-LR2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-LR2", "PROJ-LR2", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.LabelReviewReactionConfigured = false

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.LabelReviewRecovered != 0 {
		t.Errorf("LabelReviewRecovered = %d, want 0 when flag is false", result.LabelReviewRecovered)
	}

	rkey := ReactionKey("ISS-LR2", ReactionKindLabelReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("label-review PendingReactions entry created with LabelReviewReactionConfigured=false; want absent")
	}
}

// TestRecoverPendingReactions_LabelReviewMissingPRMetadata verifies that no
// label-review entry is recovered when owner, repo, or PR number is
// missing from the recovered SCM metadata.
func TestRecoverPendingReactions_LabelReviewMissingPRMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issueID    string
		identifier string
		meta       domain.SCMMetadata
	}{
		{
			name:       "missing PR number",
			issueID:    "ISS-LR3-NOPR",
			identifier: "PROJ-LR3-NOPR",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), Owner: "o", Repo: "r"},
		},
		{
			name:       "missing owner",
			issueID:    "ISS-LR3-NOOWNER",
			identifier: "PROJ-LR3-NOOWNER",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), PRNumber: 10, Repo: "r"},
		},
		{
			name:       "missing repo",
			issueID:    "ISS-LR3-NOREPO",
			identifier: "PROJ-LR3-NOREPO",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), PRNumber: 10, Owner: "o"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsRoot := t.TempDir()
			writeRecoverySCM(t, wsRoot, tt.identifier, tt.meta)

			tracker := &recoveryTrackerStub{states: map[string]string{tt.issueID: "In Review"}}
			state := NewState(5000, 4, nil, AgentTotals{})
			run := freshRun(tt.issueID, tt.identifier, "", 1)
			params := defaultRecoveryParams(wsRoot, tracker)
			params.LabelReviewReactionConfigured = true

			result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
			if err != nil {
				t.Fatalf("RecoverPendingReactions: %v", err)
			}
			if result.LabelReviewRecovered != 0 {
				t.Errorf("LabelReviewRecovered = %d, want 0 (%s)", result.LabelReviewRecovered, tt.name)
			}
		})
	}
}

// TestRecoverPendingReactions_LabelFix verifies that a handoff-state issue
// whose recovered SCM metadata carries a PR number, owner, repo, and a
// non-empty branch recovers one label-fix entry, carrying the branch and
// the frozen dispatch fields, and increments LabelFixRecovered.
func TestRecoverPendingReactions_LabelFix(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-LF1", domain.SCMMetadata{
		Branch:   "feature/lf-fix",
		SHA:      "facefeed",
		PushedAt: freshSCMTime(1),
		PRNumber: 66,
		Owner:    "lfowner",
		Repo:     "lfrepo",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-LF1": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-LF1", "PROJ-LF1", "lfowner/lfrepo#66", 2)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.LabelFixReactionConfigured = true

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.LabelFixRecovered != 1 {
		t.Errorf("LabelFixRecovered = %d, want 1", result.LabelFixRecovered)
	}

	rkey := ReactionKey("ISS-LF1", ReactionKindLabelFix)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatalf("PendingReactions[%q] missing, want present", rkey)
	}
	if pr.Kind != ReactionKindLabelFix {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindLabelFix)
	}
	if pr.Attempt != 2 {
		t.Errorf("PendingReaction.Attempt = %d, want 2", pr.Attempt)
	}
	if pr.AgentKind != run.AgentAdapter {
		t.Errorf("PendingReaction.AgentKind = %q, want %q (frozen from the recovered run)", pr.AgentKind, run.AgentAdapter)
	}
	lfd, ok := pr.KindData.(*LabelFixReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *LabelFixReactionData", pr.KindData)
	}
	if lfd.PRNumber != 66 {
		t.Errorf("LabelFixReactionData.PRNumber = %d, want 66", lfd.PRNumber)
	}
	if lfd.Owner != "lfowner" {
		t.Errorf("LabelFixReactionData.Owner = %q, want %q", lfd.Owner, "lfowner")
	}
	if lfd.Repo != "lfrepo" {
		t.Errorf("LabelFixReactionData.Repo = %q, want %q", lfd.Repo, "lfrepo")
	}
	if lfd.Branch != "feature/lf-fix" {
		t.Errorf("LabelFixReactionData.Branch = %q, want %q", lfd.Branch, "feature/lf-fix")
	}
	if _, claimed := state.Claimed["ISS-LF1"]; claimed {
		t.Error("ISS-LF1 found in state.Claimed after recovery, want not claimed")
	}
}

// TestRecoverPendingReactions_LabelFixNotRecoveredWhenFlagFalse verifies
// that LabelFixReactionConfigured=false reconstructs no label-fix entry,
// even with full branch-bearing PR metadata present.
func TestRecoverPendingReactions_LabelFixNotRecoveredWhenFlagFalse(t *testing.T) {
	t.Parallel()

	wsRoot := t.TempDir()
	writeRecoverySCM(t, wsRoot, "PROJ-LF2", domain.SCMMetadata{
		Branch:   "feature/lf-disabled",
		SHA:      "abc",
		PushedAt: freshSCMTime(1),
		PRNumber: 10,
		Owner:    "o",
		Repo:     "r",
	})

	tracker := &recoveryTrackerStub{states: map[string]string{"ISS-LF2": "In Review"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	run := freshRun("ISS-LF2", "PROJ-LF2", "", 1)
	params := defaultRecoveryParams(wsRoot, tracker)
	params.LabelFixReactionConfigured = false

	result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
	if err != nil {
		t.Fatalf("RecoverPendingReactions: %v", err)
	}
	if result.LabelFixRecovered != 0 {
		t.Errorf("LabelFixRecovered = %d, want 0 when flag is false", result.LabelFixRecovered)
	}

	rkey := ReactionKey("ISS-LF2", ReactionKindLabelFix)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("label-fix PendingReactions entry created with LabelFixReactionConfigured=false; want absent")
	}
}

// TestRecoverPendingReactions_LabelFixMissingPRMetadata verifies that no
// label-fix entry is recovered when owner, repo, PR number, or branch is
// missing from the recovered SCM metadata. The missing-branch case is the
// fix-specific difference from label-review: label-review recovery omits
// the branch requirement, but a fix session checks out that branch, so
// recovery must require it.
func TestRecoverPendingReactions_LabelFixMissingPRMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issueID    string
		identifier string
		meta       domain.SCMMetadata
	}{
		{
			name:       "missing PR number",
			issueID:    "ISS-LF3-NOPR",
			identifier: "PROJ-LF3-NOPR",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), Owner: "o", Repo: "r"},
		},
		{
			name:       "missing owner",
			issueID:    "ISS-LF3-NOOWNER",
			identifier: "PROJ-LF3-NOOWNER",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), PRNumber: 10, Repo: "r"},
		},
		{
			name:       "missing repo",
			issueID:    "ISS-LF3-NOREPO",
			identifier: "PROJ-LF3-NOREPO",
			meta:       domain.SCMMetadata{Branch: "feature/x", PushedAt: freshSCMTime(1), PRNumber: 10, Owner: "o"},
		},
		{
			name:       "missing branch",
			issueID:    "ISS-LF3-NOBRANCH",
			identifier: "PROJ-LF3-NOBRANCH",
			meta:       domain.SCMMetadata{PushedAt: freshSCMTime(1), PRNumber: 10, Owner: "o", Repo: "r"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsRoot := t.TempDir()
			writeRecoverySCM(t, wsRoot, tt.identifier, tt.meta)

			tracker := &recoveryTrackerStub{states: map[string]string{tt.issueID: "In Review"}}
			state := NewState(5000, 4, nil, AgentTotals{})
			run := freshRun(tt.issueID, tt.identifier, "", 1)
			params := defaultRecoveryParams(wsRoot, tracker)
			params.LabelFixReactionConfigured = true

			result, err := RecoverPendingReactions(context.Background(), state, []persistence.RunHistory{run}, params)
			if err != nil {
				t.Fatalf("RecoverPendingReactions: %v", err)
			}
			if result.LabelFixRecovered != 0 {
				t.Errorf("LabelFixRecovered = %d, want 0 (%s)", result.LabelFixRecovered, tt.name)
			}

			rkey := ReactionKey(tt.issueID, ReactionKindLabelFix)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Errorf("label-fix PendingReactions entry present despite incomplete SCM metadata (%s)", tt.name)
			}
		})
	}
}
