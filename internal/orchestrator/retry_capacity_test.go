package orchestrator

import (
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/waittest"
)

// TestHandleRetryTimer_CapacityShortageKeepsAttempt fires a retry several
// times while every orchestrator slot is taken. A full slot is no failed
// attempt: each fire rechecks one poll interval later at the same attempt,
// instead of backing off toward the cap and starting the run at an attempt
// it never made.
func TestHandleRetryTimer_CapacityShortageKeepsAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reactionKind string
	}{
		{name: "continuation retry"},
		{name: "reaction retry", reactionKind: ReactionKindMergeConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "CAP-1"
			const pollMS = 30_000
			state := retryState(t, id, id, 3)
			state.RetryAttempts[id].ReactionKind = tt.reactionKind
			state.MaxConcurrentAgents = 1
			state.Running["OTHER-1"] = &RunningEntry{
				Identifier: "OTHER-1",
				Issue:      candidateIssue("OTHER-1", "OTHER-1", "To Do"),
			}

			store := &mockRetryStore{}
			tracker := &mockRetryTracker{fetchedIssue: candidateIssue(id, id, "To Do")}
			params := defaultRetryParams(t, store, tracker)
			params.PollIntervalMS = pollMS

			for fire := 1; fire <= 4; fire++ {
				HandleRetryTimer(state, id, params)

				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("fire %d: RetryAttempts[%s] missing, want re-armed", fire, id)
				}
				if entry.TimerHandle != nil {
					entry.TimerHandle.Stop()
				}
				if entry.Attempt != 3 {
					t.Errorf("fire %d: Attempt = %d, want 3 (a full slot is no failed attempt)", fire, entry.Attempt)
				}
				if entry.scheduledDelayMS != pollMS {
					t.Errorf("fire %d: scheduledDelayMS = %d, want %d (the poll interval)", fire, entry.scheduledDelayMS, pollMS)
				}
				if !entry.waitsForCapacity {
					t.Errorf("fire %d: waitsForCapacity = false, want true", fire)
				}
				if entry.ReactionKind != tt.reactionKind {
					t.Errorf("fire %d: ReactionKind = %q, want %q", fire, entry.ReactionKind, tt.reactionKind)
				}
				// The next fire is the timer firing at its due time.
				entry.scheduledAt = time.Time{}
			}
			if _, running := state.Running[id]; running {
				t.Errorf("Running[%s] present, want absent (no free slot)", id)
			}
		})
	}
}

// TestWakeCapacityWaitingRetries covers the worker-exit wake: the retries
// that found no free slot are made due at once, earliest due first, within
// the free slots less those the exits just reserved for their own retries,
// and within each waiter's per-state limit. No other retry moves.
func TestWakeCapacityWaitingRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		maxAgents  int
		reserved   int
		stateLimit map[string]int
		sameState  bool // both waiters wait in To Do
		wantWoken  []string
	}{
		{name: "one free slot wakes the earliest waiting retry", maxAgents: 2, wantWoken: []string{"WAIT-SOON"}},
		{name: "two free slots wake both waiting retries", maxAgents: 3, wantWoken: []string{"WAIT-SOON", "WAIT-LATE"}},
		{name: "no free slot wakes nothing", maxAgents: 1},
		{name: "a slot an exit's continuation reserved is not given away", maxAgents: 2, reserved: 1},
		{name: "a full per-state limit skips its waiter", maxAgents: 3, stateLimit: map[string]int{"review": 1}, wantWoken: []string{"WAIT-LATE"}},
		{name: "a per-state limit counts the waiters woken before", maxAgents: 4, stateLimit: map[string]int{"to do": 1}, sameState: true, wantWoken: []string{"WAIT-SOON"}},
		{name: "per-state limits with room wake each waiter", maxAgents: 4, stateLimit: map[string]int{"review": 2, "to do": 1}, wantWoken: []string{"WAIT-SOON", "WAIT-LATE"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultWorkerConfig(t.TempDir())
			cfg.Polling = config.PollingConfig{IntervalMS: int(time.Hour / time.Millisecond)}
			state := NewState(cfg.Polling.IntervalMS, tt.maxAgents, 0, nil, AgentTotals{})
			state.MaxConcurrentByState = tt.stateLimit
			state.Running["BUSY-1"] = &RunningEntry{
				Identifier: "BUSY-1",
				Issue:      candidateIssue("BUSY-1", "BUSY-1", "Review"),
			}
			o := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  &mockTrackerAdapter{},
				AgentAdapter:    &mockAgentAdapter{},
				WorkflowManager: &stubWorkflowManager{config: cfg, template: mustParseTemplate(t, "do {{ .issue.identifier }}")},
				Store:           &stubStore{},
				PreflightParams: passingPreflightRegistries(),
			})

			schedule := func(id, issueState string, delay time.Duration, waits bool) {
				state.Claimed[id] = struct{}{}
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:    id,
					Identifier: id,
					Attempt:    2,
					DelayMS:    delay.Milliseconds(),
				}, o.onRetryFire)
				if waits {
					markWaitsForCapacity(state, id, issueState)
				}
			}
			schedule("WAIT-LATE", "To Do", 20*time.Minute, true)
			soonState := "Review"
			if tt.sameState {
				soonState = "To Do"
			}
			schedule("WAIT-SOON", soonState, 10*time.Minute, true)
			schedule("BACKOFF-1", "To Do", 5*time.Minute, false)
			t.Cleanup(func() {
				for _, entry := range state.RetryAttempts {
					entry.TimerHandle.Stop()
				}
			})
			dueBefore := map[string]int64{}
			for id, entry := range state.RetryAttempts {
				dueBefore[id] = entry.DueAtMS
			}

			o.wakeCapacityWaitingRetries(t.Context(), tt.reserved)

			woken := map[string]bool{}
			for range tt.wantWoken {
				woken[waittest.Receive(t, o.retryTimerCh, "woken retry fire")] = true
			}
			for _, id := range tt.wantWoken {
				if !woken[id] {
					t.Errorf("retry %s not fired, want woken (fired %v)", id, woken)
				}
			}
			for id, entry := range state.RetryAttempts {
				if woken[id] {
					if entry.scheduledDelayMS != 0 || entry.waitsForCapacity {
						t.Errorf("%s: scheduledDelayMS = %d, waitsForCapacity = %v, want 0, false", id, entry.scheduledDelayMS, entry.waitsForCapacity)
					}
					if entry.Attempt != 2 {
						t.Errorf("%s: Attempt = %d, want 2 (unchanged)", id, entry.Attempt)
					}
					continue
				}
				if entry.DueAtMS != dueBefore[id] {
					t.Errorf("%s: DueAtMS moved from %d to %d, want unchanged", id, dueBefore[id], entry.DueAtMS)
				}
			}
		})
	}
}

// TestWakeCapacityWaitingRetries_FiresDoNotOvercommit runs the fires the
// wake makes due through the retry timer: no more of them start than there
// are slots, and a woken retry that still finds none re-arms at the same
// attempt.
func TestWakeCapacityWaitingRetries_FiresDoNotOvercommit(t *testing.T) {
	t.Parallel()

	cfg := defaultWorkerConfig(t.TempDir())
	cfg.Polling = config.PollingConfig{IntervalMS: 30_000}
	state := NewState(cfg.Polling.IntervalMS, 2, 0, nil, AgentTotals{})
	state.Running["BUSY-1"] = &RunningEntry{Identifier: "BUSY-1", Issue: candidateIssue("BUSY-1", "BUSY-1", "To Do")}
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg, template: mustParseTemplate(t, "do {{ .issue.identifier }}")},
		Store:           &stubStore{},
		PreflightParams: passingPreflightRegistries(),
	})
	ids := []string{"WAIT-A", "WAIT-B", "WAIT-C"}
	for i, id := range ids {
		state.Claimed[id] = struct{}{}
		ScheduleRetry(state, ScheduleRetryParams{IssueID: id, Identifier: id, Attempt: 3, DelayMS: (time.Duration(i+1) * time.Minute).Milliseconds()}, o.onRetryFire)
		markWaitsForCapacity(state, id, "To Do")
	}
	t.Cleanup(func() {
		for _, entry := range state.RetryAttempts {
			entry.TimerHandle.Stop()
		}
	})

	o.wakeCapacityWaitingRetries(t.Context(), 0)
	fired := waittest.Receive(t, o.retryTimerCh, "woken retry fire")
	if fired != "WAIT-A" {
		t.Fatalf("woken retry = %s, want WAIT-A (earliest due)", fired)
	}

	tracker := &mockRetryTracker{}
	params := defaultRetryParams(t, &mockRetryStore{}, tracker)
	params.PollIntervalMS = 30_000
	// The freed slot goes to the woken retry; a second fire, as a stale
	// wake or an overlapping timer would bring, finds none.
	for _, id := range []string{"WAIT-A", "WAIT-B"} {
		tracker.fetchedIssue = candidateIssue(id, id, "To Do")
		state.RetryAttempts[id].scheduledAt = time.Time{}
		HandleRetryTimer(state, id, params)
	}

	if state.Running["WAIT-A"] == nil {
		t.Error("woken retry WAIT-A not started on the freed slot")
	}
	if got := len(state.Running); got != 2 {
		t.Errorf("running sessions = %d, want 2 (max_concurrent_agents)", got)
	}
	retry, ok := state.RetryAttempts["WAIT-B"]
	if !ok {
		t.Fatal("WAIT-B dropped, want re-armed for capacity")
	}
	if retry.Attempt != 3 || !retry.waitsForCapacity {
		t.Errorf("WAIT-B Attempt = %d, waitsForCapacity = %v, want 3, true", retry.Attempt, retry.waitsForCapacity)
	}
}
