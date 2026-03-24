package orchestrator

import (
	"math"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestNewState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		pollIntervalMS       int
		maxConcurrentAgents  int
		maxConcurrentByState map[string]int
		totals               AgentTotals
		wantMaxByStateLen    int
		checkAlias           bool
	}{
		{
			name:                 "nil state limits map becomes empty non-nil map",
			pollIntervalMS:       5000,
			maxConcurrentAgents:  10,
			maxConcurrentByState: nil,
			totals: AgentTotals{
				InputTokens:    1,
				OutputTokens:   2,
				TotalTokens:    3,
				SecondsRunning: 4.5,
			},
			wantMaxByStateLen: 0,
			checkAlias:        false,
		},
		{
			name:                "non-nil state limits map is stored as-is",
			pollIntervalMS:      1000,
			maxConcurrentAgents: 6,
			maxConcurrentByState: map[string]int{
				"to do": 2,
			},
			totals: AgentTotals{
				InputTokens:    10,
				OutputTokens:   20,
				TotalTokens:    30,
				SecondsRunning: 40.25,
			},
			wantMaxByStateLen: 1,
			checkAlias:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewState(tt.pollIntervalMS, tt.maxConcurrentAgents, tt.maxConcurrentByState, tt.totals)

			if s == nil {
				t.Fatal("NewState() = nil, want non-nil")
			}
			if s.PollIntervalMS != tt.pollIntervalMS {
				t.Errorf("PollIntervalMS = %d, want %d", s.PollIntervalMS, tt.pollIntervalMS)
			}
			if s.MaxConcurrentAgents != tt.maxConcurrentAgents {
				t.Errorf("MaxConcurrentAgents = %d, want %d", s.MaxConcurrentAgents, tt.maxConcurrentAgents)
			}
			if s.AgentTotals != tt.totals {
				t.Errorf("AgentTotals = %+v, want %+v", s.AgentTotals, tt.totals)
			}
			if s.AgentRateLimits != nil {
				t.Errorf("AgentRateLimits = %v, want nil", s.AgentRateLimits)
			}

			if s.MaxConcurrentByState == nil {
				t.Fatal("MaxConcurrentByState = nil, want non-nil")
			}
			if len(s.MaxConcurrentByState) != tt.wantMaxByStateLen {
				t.Errorf("len(MaxConcurrentByState) = %d, want %d", len(s.MaxConcurrentByState), tt.wantMaxByStateLen)
			}

			if s.Running == nil {
				t.Fatal("Running = nil, want non-nil")
			}
			if s.Claimed == nil {
				t.Fatal("Claimed = nil, want non-nil")
			}
			if s.RetryAttempts == nil {
				t.Fatal("RetryAttempts = nil, want non-nil")
			}
			if s.Completed == nil {
				t.Fatal("Completed = nil, want non-nil")
			}

			if tt.checkAlias {
				tt.maxConcurrentByState["in progress"] = 3
				if got := s.MaxConcurrentByState["in progress"]; got != 3 {
					t.Errorf("MaxConcurrentByState aliasing check = %d, want 3", got)
				}
			}
		})
	}
}

func TestRunningCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		running map[string]*RunningEntry
		want    int
	}{
		{
			name:    "empty running map",
			running: map[string]*RunningEntry{},
			want:    0,
		},
		{
			name: "three running entries",
			running: map[string]*RunningEntry{
				"1": {Issue: domain.Issue{State: "To Do"}},
				"2": {Issue: domain.Issue{State: "In Progress"}},
				"3": {Issue: domain.Issue{State: "Done"}},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &State{Running: tt.running}
			got := s.RunningCount()
			if got != tt.want {
				t.Errorf("RunningCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRunningCountByState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		running map[string]*RunningEntry
		state   string
		want    int
	}{
		{
			name:    "empty running map",
			running: map[string]*RunningEntry{},
			state:   "in progress",
			want:    0,
		},
		{
			name: "case-insensitive match with mixed states",
			running: map[string]*RunningEntry{
				"1": {Issue: domain.Issue{State: "To Do"}},
				"2": {Issue: domain.Issue{State: "In Progress"}},
				"3": {Issue: domain.Issue{State: "in progress"}},
			},
			state: "IN PROGRESS",
			want:  2,
		},
		{
			name: "absent state",
			running: map[string]*RunningEntry{
				"1": {Issue: domain.Issue{State: "To Do"}},
				"2": {Issue: domain.Issue{State: "In Progress"}},
			},
			state: "blocked",
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := RunningCountByState(tt.running, tt.state)
			if got != tt.want {
				t.Errorf("RunningCountByState(..., %q) = %d, want %d", tt.state, got, tt.want)
			}
		})
	}
}

// runningSnapshotMap builds a lookup map from a Running snapshot slice
// keyed by IssueID. Handles non-deterministic map iteration order.
func runningSnapshotMap(t *testing.T, entries []SnapshotRunningEntry) map[string]SnapshotRunningEntry {
	t.Helper()
	m := make(map[string]SnapshotRunningEntry, len(entries))
	for _, e := range entries {
		if _, dup := m[e.IssueID]; dup {
			t.Fatalf("duplicate IssueID %q in Running snapshot", e.IssueID)
		}
		m[e.IssueID] = e
	}
	return m
}

// retrySnapshotMap builds a lookup map from a Retrying snapshot slice
// keyed by IssueID.
func retrySnapshotMap(t *testing.T, entries []SnapshotRetryEntry) map[string]SnapshotRetryEntry {
	t.Helper()
	m := make(map[string]SnapshotRetryEntry, len(entries))
	for _, e := range entries {
		if _, dup := m[e.IssueID]; dup {
			t.Fatalf("duplicate IssueID %q in Retrying snapshot", e.IssueID)
		}
		m[e.IssueID] = e
	}
	return m
}

func TestRuntimeSnapshot(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("empty state", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{SecondsRunning: 42.5})
		result := RuntimeSnapshot(state, fixedNow)

		if !result.GeneratedAt.Equal(fixedNow) {
			t.Errorf("GeneratedAt = %v, want %v", result.GeneratedAt, fixedNow)
		}
		if result.Running == nil {
			t.Fatal("Running = nil, want non-nil empty slice")
		}
		if len(result.Running) != 0 {
			t.Errorf("len(Running) = %d, want 0", len(result.Running))
		}
		if result.Retrying == nil {
			t.Fatal("Retrying = nil, want non-nil empty slice")
		}
		if len(result.Retrying) != 0 {
			t.Errorf("len(Retrying) = %d, want 0", len(result.Retrying))
		}
		if result.AgentTotals.SecondsRunning != 42.5 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want 42.5", result.AgentTotals.SecondsRunning)
		}
		if result.RateLimits != nil {
			t.Errorf("RateLimits = %v, want nil", result.RateLimits)
		}
	})

	t.Run("running sessions with computed seconds_running", func(t *testing.T) {
		t.Parallel()

		startA := fixedNow.Add(-60 * time.Second)    // 60s ago
		startB := fixedNow.Add(-120 * time.Second)   // 120s ago
		eventTime := fixedNow.Add(-10 * time.Second) // 10s ago

		state := NewState(5000, 10, nil, AgentTotals{
			InputTokens:    500,
			OutputTokens:   200,
			TotalTokens:    700,
			SecondsRunning: 100.0,
		})
		state.Running["issue-a"] = &RunningEntry{
			Identifier:         "MT-100",
			Issue:              domain.Issue{ID: "issue-a", State: "In Progress"},
			SessionID:          "sess-a",
			TurnCount:          3,
			LastAgentEvent:     domain.EventTurnCompleted,
			LastAgentTimestamp: eventTime,
			LastAgentMessage:   "Working on tests",
			StartedAt:          startA,
			AgentInputTokens:   100,
			AgentOutputTokens:  50,
			AgentTotalTokens:   150,
		}
		state.Running["issue-b"] = &RunningEntry{
			Identifier:         "MT-200",
			Issue:              domain.Issue{ID: "issue-b", State: "To Do"},
			SessionID:          "sess-b",
			TurnCount:          7,
			LastAgentEvent:     domain.EventNotification,
			LastAgentTimestamp: eventTime,
			LastAgentMessage:   "Generating code",
			StartedAt:          startB,
			AgentInputTokens:   400,
			AgentOutputTokens:  150,
			AgentTotalTokens:   550,
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 2 {
			t.Fatalf("len(Running) = %d, want 2", len(result.Running))
		}

		byID := runningSnapshotMap(t, result.Running)

		// Verify entry A fields
		a := byID["issue-a"]
		if a.Identifier != "MT-100" {
			t.Errorf("entry A Identifier = %q, want %q", a.Identifier, "MT-100")
		}
		if a.State != "In Progress" {
			t.Errorf("entry A State = %q, want %q", a.State, "In Progress")
		}
		if a.SessionID != "sess-a" {
			t.Errorf("entry A SessionID = %q, want %q", a.SessionID, "sess-a")
		}
		if a.TurnCount != 3 {
			t.Errorf("entry A TurnCount = %d, want %d", a.TurnCount, 3)
		}
		if a.LastAgentEvent != domain.EventTurnCompleted {
			t.Errorf("entry A LastAgentEvent = %q, want %q", a.LastAgentEvent, domain.EventTurnCompleted)
		}
		if !a.LastAgentTimestamp.Equal(eventTime) {
			t.Errorf("entry A LastAgentTimestamp = %v, want %v", a.LastAgentTimestamp, eventTime)
		}
		if a.LastAgentMessage != "Working on tests" {
			t.Errorf("entry A LastAgentMessage = %q, want %q", a.LastAgentMessage, "Working on tests")
		}
		if !a.StartedAt.Equal(startA) {
			t.Errorf("entry A StartedAt = %v, want %v", a.StartedAt, startA)
		}
		if a.AgentInputTokens != 100 {
			t.Errorf("entry A AgentInputTokens = %d, want %d", a.AgentInputTokens, 100)
		}
		if a.AgentOutputTokens != 50 {
			t.Errorf("entry A AgentOutputTokens = %d, want %d", a.AgentOutputTokens, 50)
		}
		if a.AgentTotalTokens != 150 {
			t.Errorf("entry A AgentTotalTokens = %d, want %d", a.AgentTotalTokens, 150)
		}

		// Verify entry B fields
		b := byID["issue-b"]
		if b.Identifier != "MT-200" {
			t.Errorf("entry B Identifier = %q, want %q", b.Identifier, "MT-200")
		}
		if b.TurnCount != 7 {
			t.Errorf("entry B TurnCount = %d, want %d", b.TurnCount, 7)
		}
		if b.AgentTotalTokens != 550 {
			t.Errorf("entry B AgentTotalTokens = %d, want %d", b.AgentTotalTokens, 550)
		}

		// Verify computed seconds_running: 100.0 + 60.0 + 120.0 = 280.0
		wantSeconds := 100.0 + 60.0 + 120.0
		if math.Abs(result.AgentTotals.SecondsRunning-wantSeconds) > 0.001 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want %f", result.AgentTotals.SecondsRunning, wantSeconds)
		}

		// Verify aggregate token fields are copied
		if result.AgentTotals.InputTokens != 500 {
			t.Errorf("AgentTotals.InputTokens = %d, want %d", result.AgentTotals.InputTokens, 500)
		}
		if result.AgentTotals.OutputTokens != 200 {
			t.Errorf("AgentTotals.OutputTokens = %d, want %d", result.AgentTotals.OutputTokens, 200)
		}
		if result.AgentTotals.TotalTokens != 700 {
			t.Errorf("AgentTotals.TotalTokens = %d, want %d", result.AgentTotals.TotalTokens, 700)
		}
	})

	t.Run("retry queue populated", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.RetryAttempts["retry-1"] = &RetryEntry{
			IssueID:    "retry-1",
			Identifier: "MT-301",
			Attempt:    2,
			DueAtMS:    1711276800000,
			Error:      "no available orchestrator slots",
		}
		state.RetryAttempts["retry-2"] = &RetryEntry{
			IssueID:    "retry-2",
			Identifier: "MT-302",
			Attempt:    5,
			DueAtMS:    1711276900000,
			Error:      "agent timeout",
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Retrying) != 2 {
			t.Fatalf("len(Retrying) = %d, want 2", len(result.Retrying))
		}

		byID := retrySnapshotMap(t, result.Retrying)

		r1 := byID["retry-1"]
		if r1.Identifier != "MT-301" {
			t.Errorf("retry-1 Identifier = %q, want %q", r1.Identifier, "MT-301")
		}
		if r1.Attempt != 2 {
			t.Errorf("retry-1 Attempt = %d, want %d", r1.Attempt, 2)
		}
		if r1.DueAtMS != 1711276800000 {
			t.Errorf("retry-1 DueAtMS = %d, want %d", r1.DueAtMS, 1711276800000)
		}
		if r1.Error != "no available orchestrator slots" {
			t.Errorf("retry-1 Error = %q, want %q", r1.Error, "no available orchestrator slots")
		}

		r2 := byID["retry-2"]
		if r2.Identifier != "MT-302" {
			t.Errorf("retry-2 Identifier = %q, want %q", r2.Identifier, "MT-302")
		}
		if r2.Attempt != 5 {
			t.Errorf("retry-2 Attempt = %d, want %d", r2.Attempt, 5)
		}
	})

	t.Run("rate limits present with isolation", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		origData := map[string]any{
			"requests_remaining": 42,
			"reset_at":           "2026-03-24T13:00:00Z",
		}
		state.AgentRateLimits = &RateLimitSnapshot{
			Data:       origData,
			ReceivedAt: fixedNow,
		}

		result := RuntimeSnapshot(state, fixedNow)

		if result.RateLimits == nil {
			t.Fatal("RateLimits = nil, want non-nil")
		}
		if got, ok := result.RateLimits["requests_remaining"]; !ok || got != 42 {
			t.Errorf("RateLimits[requests_remaining] = %v, want 42", got)
		}
		if got, ok := result.RateLimits["reset_at"]; !ok || got != "2026-03-24T13:00:00Z" {
			t.Errorf("RateLimits[reset_at] = %v, want %q", got, "2026-03-24T13:00:00Z")
		}

		// Mutate original after snapshot — snapshot must be unaffected.
		origData["injected_key"] = "should not appear"
		if _, leaked := result.RateLimits["injected_key"]; leaked {
			t.Error("RateLimits contains injected_key after original mutation — shallow copy isolation failed")
		}
	})

	t.Run("rate limits nil", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		result := RuntimeSnapshot(state, fixedNow)

		if result.RateLimits != nil {
			t.Errorf("RateLimits = %v, want nil", result.RateLimits)
		}
	})

	t.Run("clock skew guard future StartedAt", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{SecondsRunning: 50.0})
		state.Running["future-issue"] = &RunningEntry{
			Identifier: "MT-400",
			Issue:      domain.Issue{ID: "future-issue", State: "In Progress"},
			StartedAt:  fixedNow.Add(10 * time.Second), // 10s in the future
		}

		result := RuntimeSnapshot(state, fixedNow)

		// The future entry must contribute 0, not a negative value.
		if result.AgentTotals.SecondsRunning != 50.0 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want 50.0 (future StartedAt should contribute 0)", result.AgentTotals.SecondsRunning)
		}
	})

	t.Run("zero timestamp guard", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{SecondsRunning: 50.0})
		state.Running["zero-ts"] = &RunningEntry{
			Identifier: "MT-500",
			Issue:      domain.Issue{ID: "zero-ts", State: "In Progress"},
			StartedAt:  time.Time{}, // zero value
		}

		result := RuntimeSnapshot(state, fixedNow)

		// Zero timestamp must contribute 0, not decades of elapsed.
		if result.AgentTotals.SecondsRunning != 50.0 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want 50.0 (zero StartedAt should contribute 0)", result.AgentTotals.SecondsRunning)
		}
	})

	t.Run("WorkspacePath copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["ws-issue"] = &RunningEntry{
			Identifier:    "MT-600",
			Issue:         domain.Issue{ID: "ws-issue", State: "In Progress"},
			StartedAt:     fixedNow.Add(-30 * time.Second),
			WorkspacePath: "/tmp/workspaces/mt-600",
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		if result.Running[0].WorkspacePath != "/tmp/workspaces/mt-600" {
			t.Errorf("WorkspacePath = %q, want %q", result.Running[0].WorkspacePath, "/tmp/workspaces/mt-600")
		}
	})

	t.Run("empty WorkspacePath preserved", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["no-ws"] = &RunningEntry{
			Identifier: "MT-700",
			Issue:      domain.Issue{ID: "no-ws", State: "To Do"},
			StartedAt:  fixedNow.Add(-10 * time.Second),
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		if result.Running[0].WorkspacePath != "" {
			t.Errorf("WorkspacePath = %q, want empty string", result.Running[0].WorkspacePath)
		}
	})
}
