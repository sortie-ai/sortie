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
// that found no free slot are made due at once, earliest due first and no
// more of them than there are free slots, and no other retry moves.
func TestWakeCapacityWaitingRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		maxAgents int
		wantWoken []string
	}{
		{name: "one free slot wakes the earliest waiting retry", maxAgents: 2, wantWoken: []string{"WAIT-SOON"}},
		{name: "two free slots wake both waiting retries", maxAgents: 3, wantWoken: []string{"WAIT-SOON", "WAIT-LATE"}},
		{name: "no free slot wakes nothing", maxAgents: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := defaultWorkerConfig(t.TempDir())
			cfg.Polling = config.PollingConfig{IntervalMS: int(time.Hour / time.Millisecond)}
			state := NewState(cfg.Polling.IntervalMS, tt.maxAgents, 0, nil, AgentTotals{})
			state.Running["BUSY-1"] = &RunningEntry{
				Identifier: "BUSY-1",
				Issue:      candidateIssue("BUSY-1", "BUSY-1", "To Do"),
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

			schedule := func(id string, delay time.Duration, waits bool) {
				state.Claimed[id] = struct{}{}
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:    id,
					Identifier: id,
					Attempt:    2,
					DelayMS:    delay.Milliseconds(),
				}, o.onRetryFire)
				state.RetryAttempts[id].waitsForCapacity = waits
			}
			schedule("WAIT-LATE", 20*time.Minute, true)
			schedule("WAIT-SOON", 10*time.Minute, true)
			schedule("BACKOFF-1", 5*time.Minute, false)
			t.Cleanup(func() {
				for _, entry := range state.RetryAttempts {
					entry.TimerHandle.Stop()
				}
			})
			dueBefore := map[string]int64{}
			for id, entry := range state.RetryAttempts {
				dueBefore[id] = entry.DueAtMS
			}

			o.wakeCapacityWaitingRetries(t.Context())

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
