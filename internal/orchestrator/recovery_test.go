package orchestrator

import (
	"testing"
	"time"

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

		PopulateRetries(state, entries)

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
		PopulateRetries(state, nil)

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

	PopulateRetries(state, entries)

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

	PopulateRetries(state, entries)

	got, ok := state.RetryAttempts["id-nosess"]
	if !ok {
		t.Fatal("RetryAttempts[id-nosess] missing after PopulateRetries")
	}
	if got.SessionID != "" {
		t.Errorf("PopulateRetries_SessionID_Nil: SessionID = %q, want empty", got.SessionID)
	}
}
