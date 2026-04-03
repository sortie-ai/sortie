package orchestrator

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Test doubles ---

// spyMetrics records all domain.Metrics method calls for assertion.
type spyMetrics struct {
	mu sync.Mutex

	runningSessions      []int
	retryingSessions     []int
	availableSlots       []int
	activeSessionElapsed []float64
	tokens               []tokenCall
	agentRuntime         []float64
	dispatches           []string
	workerExits          []string
	retries              []string
	reconciliationActs   []string
	pollCycles           []string
	trackerRequests      []trackerReqCall
	handoffTransitions   []string
	dispatchTransitions  []string
	toolCalls            []toolCallCall
	pollDurations        []float64
	workerDurations      []workerDurCall
	sshHostUsage         []sshHostUsageCall
	trackerComments      []trackerCommentCall
}

type tokenCall struct {
	tokenType string
	count     int64
}

type trackerReqCall struct {
	operation string
	result    string
}

type workerDurCall struct {
	exitType string
	seconds  float64
}

type sshHostUsageCall struct {
	host  string
	count int
}

type toolCallCall struct {
	tool   string
	result string
}

var _ domain.Metrics = (*spyMetrics)(nil)

func (s *spyMetrics) SetRunningSessions(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runningSessions = append(s.runningSessions, n)
}

func (s *spyMetrics) SetRetryingSessions(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryingSessions = append(s.retryingSessions, n)
}

func (s *spyMetrics) SetAvailableSlots(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.availableSlots = append(s.availableSlots, n)
}

func (s *spyMetrics) SetActiveSessionsElapsed(seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeSessionElapsed = append(s.activeSessionElapsed, seconds)
}

func (s *spyMetrics) AddTokens(tokenType string, count int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, tokenCall{tokenType, count})
}

func (s *spyMetrics) AddAgentRuntime(seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentRuntime = append(s.agentRuntime, seconds)
}

func (s *spyMetrics) IncDispatches(outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatches = append(s.dispatches, outcome)
}

func (s *spyMetrics) IncWorkerExits(exitType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerExits = append(s.workerExits, exitType)
}

func (s *spyMetrics) IncRetries(trigger string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries = append(s.retries, trigger)
}

func (s *spyMetrics) IncReconciliationActions(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciliationActs = append(s.reconciliationActs, action)
}

func (s *spyMetrics) IncPollCycles(result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCycles = append(s.pollCycles, result)
}

func (s *spyMetrics) IncTrackerRequests(operation string, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackerRequests = append(s.trackerRequests, trackerReqCall{operation, result})
}

func (s *spyMetrics) IncHandoffTransitions(result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handoffTransitions = append(s.handoffTransitions, result)
}

func (s *spyMetrics) IncDispatchTransitions(result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatchTransitions = append(s.dispatchTransitions, result)
}

func (s *spyMetrics) IncToolCalls(tool string, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCalls = append(s.toolCalls, toolCallCall{tool, result})
}

func (s *spyMetrics) ObservePollDuration(seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollDurations = append(s.pollDurations, seconds)
}

func (s *spyMetrics) ObserveWorkerDuration(exitType string, seconds float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workerDurations = append(s.workerDurations, workerDurCall{exitType, seconds})
}

func (s *spyMetrics) SetSSHHostUsage(host string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sshHostUsage = append(s.sshHostUsage, sshHostUsageCall{host, count})
}

type trackerCommentCall struct {
	lifecycle string
	result    string
}

func (s *spyMetrics) IncTrackerComments(lifecycle string, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackerComments = append(s.trackerComments, trackerCommentCall{lifecycle, result})
}

func (s *spyMetrics) IncCIStatusChecks(_ string) {}

func (s *spyMetrics) IncCIEscalations(_ string) {}

// --- Tests ---

func TestActiveElapsedSeconds(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		entries map[string]*RunningEntry
		want    float64
	}{
		{
			name:    "empty running map",
			entries: map[string]*RunningEntry{},
			want:    0,
		},
		{
			name: "single entry 60s ago",
			entries: map[string]*RunningEntry{
				"A-1": {StartedAt: now.Add(-60 * time.Second)},
			},
			want: 60,
		},
		{
			name: "two entries",
			entries: map[string]*RunningEntry{
				"A-1": {StartedAt: now.Add(-30 * time.Second)},
				"A-2": {StartedAt: now.Add(-90 * time.Second)},
			},
			want: 120,
		},
		{
			name: "zero StartedAt skipped",
			entries: map[string]*RunningEntry{
				"A-1": {StartedAt: now.Add(-10 * time.Second)},
				"A-2": {StartedAt: time.Time{}},
			},
			want: 10,
		},
		{
			name: "future StartedAt clamped to zero",
			entries: map[string]*RunningEntry{
				"A-1": {StartedAt: now.Add(5 * time.Second)},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, nil, AgentTotals{})
			state.Running = tt.entries

			got := ActiveElapsedSeconds(state, now)
			if got != tt.want {
				t.Errorf("ActiveElapsedSeconds() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateGauges(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	spy := &spyMetrics{}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running["A-1"] = &RunningEntry{StartedAt: now.Add(-30 * time.Second)}
	state.Running["A-2"] = &RunningEntry{StartedAt: now.Add(-60 * time.Second)}
	state.RetryAttempts["B-1"] = &RetryEntry{IssueID: "B-1"}

	o := &Orchestrator{
		state:    state,
		metrics:  spy,
		hostPool: NewHostPool(nil, 0),
	}
	o.updateGauges(now)

	if len(spy.runningSessions) != 1 || spy.runningSessions[0] != 2 {
		t.Errorf("SetRunningSessions calls = %v, want [2]", spy.runningSessions)
	}
	if len(spy.retryingSessions) != 1 || spy.retryingSessions[0] != 1 {
		t.Errorf("SetRetryingSessions calls = %v, want [1]", spy.retryingSessions)
	}
	// MaxConcurrentAgents=4, Running=2 → available=2
	if len(spy.availableSlots) != 1 || spy.availableSlots[0] != 2 {
		t.Errorf("SetAvailableSlots calls = %v, want [2]", spy.availableSlots)
	}
	// 30 + 60 = 90
	if len(spy.activeSessionElapsed) != 1 || spy.activeSessionElapsed[0] != 90 {
		t.Errorf("SetActiveSessionsElapsed calls = %v, want [90]", spy.activeSessionElapsed)
	}
	// No SSH hosts configured → no SetSSHHostUsage calls.
	if len(spy.sshHostUsage) != 0 {
		t.Errorf("SetSSHHostUsage calls = %v, want empty (local mode)", spy.sshHostUsage)
	}
}

func TestUpdateGauges_SSH(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	spy := &spyMetrics{}

	hp := NewHostPool([]string{"host-a", "host-b"}, 2)
	hp.AcquireHost("ISS-1", "host-a")
	hp.AcquireHost("ISS-2", "host-a")
	hp.AcquireHost("ISS-3", "host-b")

	state := NewState(5000, 4, nil, AgentTotals{})

	o := &Orchestrator{
		state:    state,
		metrics:  spy,
		hostPool: hp,
	}
	o.updateGauges(now)

	// Should have 2 calls — one per host.
	if len(spy.sshHostUsage) != 2 {
		t.Fatalf("SetSSHHostUsage call count = %d, want 2", len(spy.sshHostUsage))
	}

	usageMap := make(map[string]int)
	for _, c := range spy.sshHostUsage {
		usageMap[c.host] = c.count
	}
	if usageMap["host-a"] != 2 {
		t.Errorf("host-a usage = %d, want 2", usageMap["host-a"])
	}
	if usageMap["host-b"] != 1 {
		t.Errorf("host-b usage = %d, want 1", usageMap["host-b"])
	}
}

func TestHandleAgentEvent_TokenMetrics(t *testing.T) {
	t.Parallel()

	t.Run("positive deltas emit AddTokens", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		state, _ := newStateWithEntry("TOK-1")

		HandleAgentEvent(state, "TOK-1", domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		}, slog.Default(), spy)

		if len(spy.tokens) != 2 {
			t.Fatalf("AddTokens call count = %d, want 2", len(spy.tokens))
		}
		if spy.tokens[0].tokenType != "input" || spy.tokens[0].count != 100 {
			t.Errorf("tokens[0] = %+v, want {input 100}", spy.tokens[0])
		}
		if spy.tokens[1].tokenType != "output" || spy.tokens[1].count != 50 {
			t.Errorf("tokens[1] = %+v, want {output 50}", spy.tokens[1])
		}
	})

	t.Run("zero deltas skip AddTokens", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		state, entry := newStateWithEntry("TOK-2")
		// Pre-fill so deltas are zero.
		entry.LastReportedInputTokens = 100
		entry.LastReportedOutputTokens = 50
		entry.LastReportedTotalTokens = 150

		HandleAgentEvent(state, "TOK-2", domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		}, slog.Default(), spy)

		if len(spy.tokens) != 0 {
			t.Errorf("AddTokens call count = %d, want 0 (zero delta)", len(spy.tokens))
		}
	})

	t.Run("non-token event does not emit AddTokens", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		state, _ := newStateWithEntry("TOK-3")

		HandleAgentEvent(state, "TOK-3", domain.AgentEvent{
			Type:      domain.EventNotification,
			Timestamp: time.Now().UTC(),
		}, slog.Default(), spy)

		if len(spy.tokens) != 0 {
			t.Errorf("AddTokens call count = %d, want 0", len(spy.tokens))
		}
	})

	t.Run("nil metrics defaults to noop without panic", func(t *testing.T) {
		t.Parallel()

		state, _ := newStateWithEntry("TOK-4")

		// Should not panic.
		HandleAgentEvent(state, "TOK-4", domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     domain.TokenUsage{InputTokens: 50, OutputTokens: 25, TotalTokens: 75},
		}, slog.Default(), nil)
	})
}

func TestHandleWorkerExitMetrics(t *testing.T) {
	t.Parallel()

	t.Run("normal exit records exit type and duration", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-1", nil)
		state.Running["EX-1"].Issue = domain.Issue{
			ID: "EX-1", Identifier: "EX-1-ident", State: "In Progress",
		}

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-1",
			Identifier: "EX-1-ident",
			ExitKind:   WorkerExitNormal,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(120 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
			ActiveStates:      []string{"In Progress"},
		})

		if len(spy.workerExits) != 1 || spy.workerExits[0] != "normal" {
			t.Errorf("IncWorkerExits = %v, want [normal]", spy.workerExits)
		}
		if len(spy.workerDurations) != 1 {
			t.Fatalf("ObserveWorkerDuration call count = %d, want 1", len(spy.workerDurations))
		}
		if spy.workerDurations[0].exitType != "normal" {
			t.Errorf("workerDurations[0].exitType = %q, want %q", spy.workerDurations[0].exitType, "normal")
		}
		if spy.workerDurations[0].seconds != 120 {
			t.Errorf("workerDurations[0].seconds = %v, want 120", spy.workerDurations[0].seconds)
		}
		if len(spy.agentRuntime) != 1 || spy.agentRuntime[0] != 120 {
			t.Errorf("AddAgentRuntime = %v, want [120]", spy.agentRuntime)
		}
	})

	t.Run("error exit records error type", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-2", nil)

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-2",
			Identifier: "EX-2-ident",
			ExitKind:   WorkerExitError,
			Error:      &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "test"},
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.workerExits) != 1 || spy.workerExits[0] != "error" {
			t.Errorf("IncWorkerExits = %v, want [error]", spy.workerExits)
		}
		if len(spy.retries) != 1 || spy.retries[0] != "error" {
			t.Errorf("IncRetries = %v, want [error]", spy.retries)
		}
	})

	t.Run("cancelled exit records cancelled type", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-3", nil)

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-3",
			Identifier: "EX-3-ident",
			ExitKind:   WorkerExitCancelled,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(10 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.workerExits) != 1 || spy.workerExits[0] != "cancelled" {
			t.Errorf("IncWorkerExits = %v, want [cancelled]", spy.workerExits)
		}
	})

	t.Run("continuation retry emits IncRetries(continuation)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-4", nil)
		state.Running["EX-4"].Issue = domain.Issue{
			ID: "EX-4", Identifier: "EX-4-ident", State: "In Progress",
		}

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-4",
			Identifier: "EX-4-ident",
			ExitKind:   WorkerExitNormal,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
			ActiveStates:      []string{"In Progress"},
		})

		if len(spy.retries) != 1 || spy.retries[0] != "continuation" {
			t.Errorf("IncRetries = %v, want [continuation]", spy.retries)
		}
	})

	t.Run("handoff success emits IncHandoffTransitions(success)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-5", nil)
		state.Running["EX-5"].Issue = domain.Issue{
			ID: "EX-5", Identifier: "EX-5-ident", State: "In Progress",
		}

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-5",
			Identifier: "EX-5-ident",
			ExitKind:   WorkerExitNormal,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
			ActiveStates:      []string{"In Progress"},
			HandoffState:      "In Review",
			TrackerAdapter:    &mockTrackerAdapter{},
		})

		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "success" {
			t.Errorf("IncHandoffTransitions = %v, want [success]", spy.handoffTransitions)
		}
	})

	t.Run("handoff with nil adapter emits error", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockExitStore{}
		state := exitState(t, "EX-6", nil)
		state.Running["EX-6"].Issue = domain.Issue{
			ID: "EX-6", Identifier: "EX-6-ident", State: "In Progress",
		}

		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-6",
			Identifier: "EX-6-ident",
			ExitKind:   WorkerExitNormal,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
			Logger:            discardLogger(),
			Metrics:           spy,
			ActiveStates:      []string{"In Progress"},
			HandoffState:      "In Review",
			TrackerAdapter:    nil,
		})

		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "error" {
			t.Errorf("IncHandoffTransitions = %v, want [error]", spy.handoffTransitions)
		}
	})

	t.Run("nil metrics defaults to noop without panic", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "EX-7", nil)

		// Should not panic.
		HandleWorkerExit(state, WorkerResult{
			IssueID:    "EX-7",
			Identifier: "EX-7-ident",
			ExitKind:   WorkerExitNormal,
		}, HandleWorkerExitParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
			Logger:            discardLogger(),
		})
	})
}

func TestHandleRetryTimerMetrics(t *testing.T) {
	t.Parallel()

	t.Run("successful dispatch emits IncDispatches(success)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		state := NewState(5000, 4, nil, AgentTotals{})

		issueID := "RT-1"
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:    issueID,
			Identifier: "RT-1-ident",
			Attempt:    1,
			DueAtMS:    time.Now().UnixMilli(),
		}

		HandleRetryTimer(state, issueID, HandleRetryTimerParams{
			Store: store,
			TrackerAdapter: &mockRetryTracker{
				candidates: []domain.Issue{
					{ID: issueID, Identifier: "RT-1-ident", Title: "Test", State: "To Do"},
				},
			},
			ActiveStates:      []string{"To Do"},
			TerminalStates:    []string{"Done"},
			MaxRetryBackoffMS: 300_000,
			MakeWorkerFn: func(_, _ string) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					// no-op worker
				}
			},
			OnRetryFire: noopRetryFire,
			Logger:      discardLogger(),
			Metrics:     spy,
		})

		if len(spy.dispatches) != 1 || spy.dispatches[0] != "success" {
			t.Errorf("IncDispatches = %v, want [success]", spy.dispatches)
		}
		if len(spy.retries) != 0 {
			t.Errorf("IncRetries = %v, want [] (no reschedule)", spy.retries)
		}
	})

	t.Run("worker still running emits IncRetries(timer)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		state := NewState(5000, 4, nil, AgentTotals{})

		issueID := "RT-2"
		state.Claimed[issueID] = struct{}{}
		state.Running[issueID] = &RunningEntry{}
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:    issueID,
			Identifier: "RT-2-ident",
			Attempt:    1,
			DueAtMS:    time.Now().UnixMilli(),
		}

		HandleRetryTimer(state, issueID, HandleRetryTimerParams{
			Store:             store,
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.retries) != 1 || spy.retries[0] != "timer" {
			t.Errorf("IncRetries = %v, want [timer]", spy.retries)
		}
	})

	t.Run("fetch failure emits IncRetries(timer)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		state := NewState(5000, 4, nil, AgentTotals{})

		issueID := "RT-3"
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:    issueID,
			Identifier: "RT-3-ident",
			Attempt:    1,
			DueAtMS:    time.Now().UnixMilli(),
		}

		HandleRetryTimer(state, issueID, HandleRetryTimerParams{
			Store: store,
			TrackerAdapter: &mockRetryTracker{
				fetchErr: context.DeadlineExceeded,
			},
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.retries) != 1 || spy.retries[0] != "timer" {
			t.Errorf("IncRetries = %v, want [timer]", spy.retries)
		}
	})

	t.Run("no slots emits IncRetries(timer)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		// Max 1 concurrent, with 1 already running.
		state := NewState(5000, 1, nil, AgentTotals{})
		state.Running["OTHER"] = &RunningEntry{Issue: domain.Issue{State: "To Do"}}

		issueID := "RT-4"
		state.Claimed[issueID] = struct{}{}
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:    issueID,
			Identifier: "RT-4-ident",
			Attempt:    1,
			DueAtMS:    time.Now().UnixMilli(),
		}

		HandleRetryTimer(state, issueID, HandleRetryTimerParams{
			Store: store,
			TrackerAdapter: &mockRetryTracker{
				candidates: []domain.Issue{
					{ID: issueID, Identifier: "RT-4-ident", Title: "Test", State: "To Do"},
				},
			},
			ActiveStates:      []string{"To Do"},
			TerminalStates:    []string{"Done"},
			MaxRetryBackoffMS: 300_000,
			OnRetryFire:       noopRetryFire,
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.retries) != 1 || spy.retries[0] != "timer" {
			t.Errorf("IncRetries = %v, want [timer]", spy.retries)
		}
	})
}

func TestReconcileMetrics(t *testing.T) {
	t.Parallel()

	t.Run("stall detection emits IncRetries(stall)", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		state := NewState(5000, 4, nil, AgentTotals{})
		// Entry started 10 minutes ago with no agent activity.
		state.Running["ST-1"] = &RunningEntry{
			Identifier: "ST-1-ident",
			StartedAt:  now.Add(-10 * time.Minute),
		}

		ReconcileRunningIssues(state, ReconcileParams{
			TrackerAdapter: &mockReconcileTracker{
				states: map[string]string{"ST-1": "In Progress"},
			},
			ActiveStates:      []string{"In Progress"},
			TerminalStates:    []string{"Done"},
			StallTimeoutMS:    60_000, // 60s threshold, 10min elapsed
			MaxRetryBackoffMS: 300_000,
			Store:             store,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return now },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.retries) != 1 || spy.retries[0] != "stall" {
			t.Errorf("IncRetries = %v, want [stall]", spy.retries)
		}
	})

	t.Run("terminal issue emits cleanup action", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		state := NewState(5000, 4, nil, AgentTotals{})
		cancelled := false
		state.Running["TM-1"] = &RunningEntry{
			Identifier: "TM-1-ident",
			StartedAt:  now.Add(-30 * time.Second),
			CancelFunc: func() { cancelled = true },
		}

		ReconcileRunningIssues(state, ReconcileParams{
			TrackerAdapter: &mockReconcileTracker{
				states: map[string]string{"TM-1": "Done"},
			},
			ActiveStates:      []string{"In Progress"},
			TerminalStates:    []string{"Done"},
			StallTimeoutMS:    0, // disable stall detection
			MaxRetryBackoffMS: 300_000,
			Store:             store,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return now },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.reconciliationActs) != 1 || spy.reconciliationActs[0] != "cleanup" {
			t.Errorf("IncReconciliationActions = %v, want [cleanup]", spy.reconciliationActs)
		}
		if !cancelled {
			t.Error("CancelFunc not called for terminal issue")
		}
	})

	t.Run("active issue emits keep action", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["AC-1"] = &RunningEntry{
			Identifier: "AC-1-ident",
			StartedAt:  now.Add(-10 * time.Second),
			Issue:      domain.Issue{State: "In Progress"},
		}

		ReconcileRunningIssues(state, ReconcileParams{
			TrackerAdapter: &mockReconcileTracker{
				states: map[string]string{"AC-1": "In Progress"},
			},
			ActiveStates:      []string{"In Progress"},
			TerminalStates:    []string{"Done"},
			StallTimeoutMS:    0,
			MaxRetryBackoffMS: 300_000,
			Store:             store,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return now },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.reconciliationActs) != 1 || spy.reconciliationActs[0] != "keep" {
			t.Errorf("IncReconciliationActions = %v, want [keep]", spy.reconciliationActs)
		}
	})

	t.Run("non-active non-terminal emits stop action", func(t *testing.T) {
		t.Parallel()

		spy := &spyMetrics{}
		store := &mockRetryStore{}
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["NA-1"] = &RunningEntry{
			Identifier: "NA-1-ident",
			StartedAt:  now.Add(-10 * time.Second),
		}

		ReconcileRunningIssues(state, ReconcileParams{
			TrackerAdapter: &mockReconcileTracker{
				states: map[string]string{"NA-1": "Blocked"},
			},
			ActiveStates:      []string{"In Progress"},
			TerminalStates:    []string{"Done"},
			StallTimeoutMS:    0,
			MaxRetryBackoffMS: 300_000,
			Store:             store,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return now },
			Logger:            discardLogger(),
			Metrics:           spy,
		})

		if len(spy.reconciliationActs) != 1 || spy.reconciliationActs[0] != "stop" {
			t.Errorf("IncReconciliationActions = %v, want [stop]", spy.reconciliationActs)
		}
	})

	t.Run("nil metrics defaults to noop without panic", func(t *testing.T) {
		t.Parallel()

		store := &mockRetryStore{}
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["NP-1"] = &RunningEntry{
			Identifier: "NP-1-ident",
			StartedAt:  now.Add(-10 * time.Second),
		}

		// Should not panic.
		ReconcileRunningIssues(state, ReconcileParams{
			TrackerAdapter: &mockReconcileTracker{
				states: map[string]string{"NP-1": "In Progress"},
			},
			ActiveStates:      []string{"In Progress"},
			TerminalStates:    []string{"Done"},
			StallTimeoutMS:    0,
			MaxRetryBackoffMS: 300_000,
			Store:             store,
			OnRetryFire:       noopRetryFire,
			NowFunc:           func() time.Time { return now },
			Logger:            discardLogger(),
		})
	})
}

func TestMapExitKindToExitType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind WorkerExitKind
		want string
	}{
		{name: "normal", kind: WorkerExitNormal, want: "normal"},
		{name: "error", kind: WorkerExitError, want: "error"},
		{name: "cancelled", kind: WorkerExitCancelled, want: "cancelled"},
		{name: "unknown defaults to error", kind: WorkerExitKind("unknown"), want: "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapExitKindToExitType(tt.kind)
			if got != tt.want {
				t.Errorf("mapExitKindToExitType(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}
