package orchestrator

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
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
				return
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
			if s.BudgetExhausted == nil {
				t.Fatal("BudgetExhausted = nil, want non-nil")
			}
			if s.BudgetHoldNoticed == nil {
				t.Fatal("BudgetHoldNoticed = nil, want non-nil")
			}
			if len(s.BudgetHoldNoticed) != 0 {
				t.Errorf("len(BudgetHoldNoticed) = %d, want 0", len(s.BudgetHoldNoticed))
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

	// --- Extended token metric snapshot tests ---

	t.Run("extended fields copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{CacheReadTokens: 999})
		state.Running["ext-1"] = &RunningEntry{
			Identifier:      "MT-EXT",
			Issue:           domain.Issue{ID: "ext-1", State: "In Progress"},
			StartedAt:       fixedNow.Add(-10 * time.Second),
			CacheReadTokens: 2500,
			ModelName:       "claude-sonnet-4-20250514",
			APIRequestCount: 7,
			RequestsByModel: map[string]int{"claude-sonnet-4-20250514": 5, "claude-opus-4-20250514": 2},
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		snap := result.Running[0]
		if snap.CacheReadTokens != 2500 {
			t.Errorf("CacheReadTokens = %d, want 2500", snap.CacheReadTokens)
		}
		if snap.ModelName != "claude-sonnet-4-20250514" {
			t.Errorf("ModelName = %q, want %q", snap.ModelName, "claude-sonnet-4-20250514")
		}
		if snap.APIRequestCount != 7 {
			t.Errorf("APIRequestCount = %d, want 7", snap.APIRequestCount)
		}
		if len(snap.RequestsByModel) != 2 {
			t.Errorf("len(RequestsByModel) = %d, want 2", len(snap.RequestsByModel))
		}
		if snap.RequestsByModel["claude-sonnet-4-20250514"] != 5 {
			t.Errorf("RequestsByModel[sonnet] = %d, want 5", snap.RequestsByModel["claude-sonnet-4-20250514"])
		}

		// AgentTotals.CacheReadTokens must come from state.AgentTotals.
		if result.AgentTotals.CacheReadTokens != 999 {
			t.Errorf("AgentTotals.CacheReadTokens = %d, want 999", result.AgentTotals.CacheReadTokens)
		}
	})

	t.Run("RequestsByModel snapshot is an isolated copy", func(t *testing.T) {
		t.Parallel()

		rbm := map[string]int{"model-a": 3}
		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["iso-1"] = &RunningEntry{
			Identifier:      "MT-ISO",
			Issue:           domain.Issue{ID: "iso-1", State: "In Progress"},
			StartedAt:       fixedNow.Add(-5 * time.Second),
			RequestsByModel: rbm,
		}

		result := RuntimeSnapshot(state, fixedNow)

		// Mutate the source map after snapshot.
		rbm["model-a"] = 999
		rbm["model-b"] = 1

		snap := result.Running[0]
		if snap.RequestsByModel["model-a"] != 3 {
			t.Errorf("after mutation: RequestsByModel[model-a] = %d, want 3 (copy isolation)", snap.RequestsByModel["model-a"])
		}
		if _, exists := snap.RequestsByModel["model-b"]; exists {
			t.Error("after mutation: RequestsByModel[model-b] exists, want absent")
		}
	})

	t.Run("nil RequestsByModel produces nil in snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["nil-rbm"] = &RunningEntry{
			Identifier:      "MT-NIL",
			Issue:           domain.Issue{ID: "nil-rbm", State: "In Progress"},
			StartedAt:       fixedNow.Add(-1 * time.Second),
			RequestsByModel: nil,
		}

		result := RuntimeSnapshot(state, fixedNow)

		snap := result.Running[0]
		if snap.RequestsByModel != nil {
			t.Errorf("RequestsByModel = %v, want nil", snap.RequestsByModel)
		}
	})

	// --- Timing fields snapshot tests ---

	t.Run("ToolTimeMs and APITimeMs copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["timing-1"] = &RunningEntry{
			Identifier: "MT-TIM",
			Issue:      domain.Issue{ID: "timing-1", State: "In Progress"},
			StartedAt:  fixedNow.Add(-60 * time.Second),
			ToolTimeMs: 4500,
			APITimeMs:  12000,
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		snap := result.Running[0]
		if snap.ToolTimeMs != 4500 {
			t.Errorf("ToolTimeMs = %d, want 4500", snap.ToolTimeMs)
		}
		if snap.APITimeMs != 12000 {
			t.Errorf("APITimeMs = %d, want 12000", snap.APITimeMs)
		}
	})

	t.Run("zero timing fields preserved in snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["zero-time"] = &RunningEntry{
			Identifier: "MT-ZT",
			Issue:      domain.Issue{ID: "zero-time", State: "In Progress"},
			StartedAt:  fixedNow.Add(-10 * time.Second),
		}

		result := RuntimeSnapshot(state, fixedNow)

		snap := result.Running[0]
		if snap.ToolTimeMs != 0 {
			t.Errorf("ToolTimeMs = %d, want 0", snap.ToolTimeMs)
		}
		if snap.APITimeMs != 0 {
			t.Errorf("APITimeMs = %d, want 0", snap.APITimeMs)
		}
	})

	t.Run("empty BudgetExhausted produces zero count and empty non-nil slice", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})

		result := RuntimeSnapshot(state, fixedNow)

		if result.BudgetExhaustedCount != 0 {
			t.Errorf("BudgetExhaustedCount = %d, want 0", result.BudgetExhaustedCount)
		}
		if result.BudgetExhausted == nil || len(result.BudgetExhausted) != 0 {
			t.Errorf("BudgetExhausted = %v, want empty non-nil slice", result.BudgetExhausted)
		}
	})

	t.Run("non-empty BudgetExhausted sorted and counted", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		// Insert out-of-order to verify sorting; empty Identifier on every
		// entry means the sort falls through to the IssueID tiebreaker.
		state.BudgetExhausted["ISS-C"] = &BudgetExhaustedEntry{}
		state.BudgetExhausted["ISS-A"] = &BudgetExhaustedEntry{}
		state.BudgetExhausted["ISS-B"] = &BudgetExhaustedEntry{}

		result := RuntimeSnapshot(state, fixedNow)

		if result.BudgetExhaustedCount != 3 {
			t.Errorf("BudgetExhaustedCount = %d, want 3", result.BudgetExhaustedCount)
		}
		want := []string{"ISS-A", "ISS-B", "ISS-C"}
		if len(result.BudgetExhausted) != len(want) {
			t.Fatalf("len(BudgetExhausted) = %d, want %d", len(result.BudgetExhausted), len(want))
		}
		for i, id := range want {
			if result.BudgetExhausted[i].IssueID != id {
				t.Errorf("BudgetExhausted[%d].IssueID = %q, want %q", i, result.BudgetExhausted[i].IssueID, id)
			}
		}
	})

	t.Run("BudgetExhausted snapshot is a copy isolated from mutation", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.BudgetExhausted["ISS-X"] = &BudgetExhaustedEntry{}

		result := RuntimeSnapshot(state, fixedNow)

		// Mutate source after snapshot.
		state.BudgetExhausted["ISS-Y"] = &BudgetExhaustedEntry{}

		if result.BudgetExhaustedCount != 1 {
			t.Errorf("BudgetExhaustedCount after source mutation = %d, want 1 (snapshot isolation)", result.BudgetExhaustedCount)
		}
		if len(result.BudgetExhausted) != 1 {
			t.Errorf("len(BudgetExhausted) after source mutation = %d, want 1 (snapshot isolation)", len(result.BudgetExhausted))
		}
	})

	t.Run("BudgetExhausted entry reason projected for every exhausted issue", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.BudgetExhausted["ISS-A"] = &BudgetExhaustedEntry{Reason: budgetReasonToken}
		state.BudgetExhausted["ISS-B"] = &BudgetExhaustedEntry{Reason: budgetReasonSession}

		result := RuntimeSnapshot(state, fixedNow)

		reasons := make(map[string]string, len(result.BudgetExhausted))
		for _, entry := range result.BudgetExhausted {
			reasons[entry.IssueID] = entry.Reason
		}
		if got := reasons["ISS-A"]; got != budgetReasonToken {
			t.Errorf("BudgetExhausted[ISS-A].Reason = %q, want %q", got, budgetReasonToken)
		}
		if got := reasons["ISS-B"]; got != budgetReasonSession {
			t.Errorf("BudgetExhausted[ISS-B].Reason = %q, want %q", got, budgetReasonSession)
		}
	})

	t.Run("BudgetExhausted sorted by Identifier ascending with IssueID as tiebreaker", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.BudgetExhausted["iss-z"] = &BudgetExhaustedEntry{Identifier: "PROJ-2"}
		state.BudgetExhausted["iss-a"] = &BudgetExhaustedEntry{Identifier: "PROJ-1"}
		// Two entries share an Identifier: the IssueID breaks the tie.
		state.BudgetExhausted["iss-y"] = &BudgetExhaustedEntry{Identifier: "PROJ-1"}

		result := RuntimeSnapshot(state, fixedNow)

		want := []struct {
			identifier string
			issueID    string
		}{
			{"PROJ-1", "iss-a"},
			{"PROJ-1", "iss-y"},
			{"PROJ-2", "iss-z"},
		}
		if len(result.BudgetExhausted) != len(want) {
			t.Fatalf("len(BudgetExhausted) = %d, want %d", len(result.BudgetExhausted), len(want))
		}
		for i, w := range want {
			got := result.BudgetExhausted[i]
			if got.Identifier != w.identifier || got.IssueID != w.issueID {
				t.Errorf("BudgetExhausted[%d] = {Identifier: %q, IssueID: %q}, want {Identifier: %q, IssueID: %q}",
					i, got.Identifier, got.IssueID, w.identifier, w.issueID)
			}
		}
	})

	t.Run("BudgetExhausted entries are value-copied, not aliased to the state pointer", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		entry := &BudgetExhaustedEntry{Identifier: "PROJ-9", Reason: budgetReasonSession, UsedSessions: 3}
		state.BudgetExhausted["iss-9"] = entry

		result := RuntimeSnapshot(state, fixedNow)
		if len(result.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(result.BudgetExhausted))
		}
		snapshotted := result.BudgetExhausted[0]

		// Mutate the pointee after the snapshot was taken. A snapshot that
		// merely copied the pointer would observe this change; a
		// value-copied snapshot must not.
		entry.Reason = budgetReasonToken
		entry.UsedSessions = 99

		if snapshotted.Reason != budgetReasonSession {
			t.Errorf("snapshot Reason = %q after source mutation, want %q (value-copy isolation)", snapshotted.Reason, budgetReasonSession)
		}
		if snapshotted.UsedSessions != 3 {
			t.Errorf("snapshot UsedSessions = %d after source mutation, want 3 (value-copy isolation)", snapshotted.UsedSessions)
		}
	})

	t.Run("BudgetExhausted entry with an empty Identifier is copied through without a fallback", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.BudgetExhausted["iss-noident"] = &BudgetExhaustedEntry{Reason: budgetReasonSession}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(result.BudgetExhausted))
		}
		got := result.BudgetExhausted[0]
		if got.Identifier != "" {
			t.Errorf("Identifier = %q, want empty (no fallback to IssueID at the state layer)", got.Identifier)
		}
		if got.IssueID != "iss-noident" {
			t.Errorf("IssueID = %q, want %q", got.IssueID, "iss-noident")
		}
	})

	t.Run("DisplayID propagated from Issue.DisplayID to running snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["gh-9"] = &RunningEntry{
			Identifier: "9",
			Issue:      domain.Issue{ID: "gh-9", State: "In Progress", DisplayID: "owner/repo#9"},
			StartedAt:  fixedNow.Add(-30 * time.Second),
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		if result.Running[0].DisplayID != "owner/repo#9" {
			t.Errorf("Running[0].DisplayID = %q, want %q", result.Running[0].DisplayID, "owner/repo#9")
		}
	})

	t.Run("empty Issue.DisplayID produces empty DisplayID in running snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Running["jira-1"] = &RunningEntry{
			Identifier: "PROJ-1",
			Issue:      domain.Issue{ID: "jira-1", State: "In Progress", DisplayID: ""},
			StartedAt:  fixedNow.Add(-10 * time.Second),
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Running) != 1 {
			t.Fatalf("len(Running) = %d, want 1", len(result.Running))
		}
		if result.Running[0].DisplayID != "" {
			t.Errorf("Running[0].DisplayID = %q, want empty string", result.Running[0].DisplayID)
		}
	})

	t.Run("DisplayID propagated from RetryEntry to retrying snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.RetryAttempts["gh-7"] = &RetryEntry{
			IssueID:    "gh-7",
			Identifier: "7",
			DisplayID:  "owner/repo#7",
			Attempt:    1,
			DueAtMS:    fixedNow.Add(5 * time.Minute).UnixMilli(),
			Error:      "timeout",
		}

		result := RuntimeSnapshot(state, fixedNow)

		if len(result.Retrying) != 1 {
			t.Fatalf("len(Retrying) = %d, want 1", len(result.Retrying))
		}
		if result.Retrying[0].DisplayID != "owner/repo#7" {
			t.Errorf("Retrying[0].DisplayID = %q, want %q", result.Retrying[0].DisplayID, "owner/repo#7")
		}
	})
}

func TestRuntimeSnapshot_WorkflowFile(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		workflowFile string
		wantFile     string
	}{
		{
			name:         "workflow file propagated to snapshot",
			workflowFile: "WORKFLOW.md",
			wantFile:     "WORKFLOW.md",
		},
		{
			name:         "custom workflow filename propagated",
			workflowFile: "backend.WORKFLOW.md",
			wantFile:     "backend.WORKFLOW.md",
		},
		{
			name:         "empty workflow file preserved as empty",
			workflowFile: "",
			wantFile:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, nil, AgentTotals{})
			state.Running["ISS-1"] = &RunningEntry{
				Identifier:   "PROJ-1",
				Issue:        domain.Issue{ID: "ISS-1", State: "In Progress"},
				StartedAt:    fixedNow.Add(-5 * time.Minute),
				WorkflowFile: tt.workflowFile,
			}

			result := RuntimeSnapshot(state, fixedNow)

			if len(result.Running) != 1 {
				t.Fatalf("len(Running) = %d, want 1", len(result.Running))
			}
			got := result.Running[0].WorkflowFile
			if got != tt.wantFile {
				t.Errorf("WorkflowFile = %q, want %q", got, tt.wantFile)
			}
		})
	}
}

// --- isKnownReactionKind tests ---

func TestIsKnownReactionKind_AcceptsAutoMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{ReactionKindCI, true},
		{ReactionKindReview, true},
		{ReactionKindAutoMerge, true},
		{"unknown", false},
		{"", false},
		{"merge_comments", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			if got := isKnownReactionKind(tt.kind); got != tt.want {
				t.Errorf("isKnownReactionKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

// --- BuildAutoMergeReactionConfig tests ---

func TestBuildAutoMergeReactionConfig_DefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rc      config.ReactionConfig
		want    AutoMergeReactionConfig
		wantErr bool
	}{
		{
			name: "all defaults",
			rc:   config.ReactionConfig{MaxRetries: 2},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
				MaxRetries:      2,
			},
		},
		{
			name: "strategy override to merge",
			rc:   config.ReactionConfig{Extra: map[string]any{"strategy": "merge"}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategyMerge,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "strategy override to rebase",
			rc:   config.ReactionConfig{Extra: map[string]any{"strategy": "rebase"}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategyRebase,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "require_ci false",
			rc:   config.ReactionConfig{Extra: map[string]any{"require_ci": false}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       false,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "delete_branch false",
			rc:   config.ReactionConfig{Extra: map[string]any{"delete_branch": false}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    false,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "poll_interval_ms override",
			rc:   config.ReactionConfig{Extra: map[string]any{"poll_interval_ms": 120000}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  120000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "escalation label with custom label",
			rc: config.ReactionConfig{
				Escalation:      "label",
				EscalationLabel: "auto-merge-failed",
			},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "label",
				EscalationLabel: "auto-merge-failed",
			},
		},
		{
			name: "max_retries from rc",
			rc:   config.ReactionConfig{MaxRetries: 5},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
				MaxRetries:      5,
			},
		},
		{
			name:    "invalid strategy",
			rc:      config.ReactionConfig{Extra: map[string]any{"strategy": "fast-forward"}},
			wantErr: true,
		},
		{
			name:    "poll_interval_ms below minimum",
			rc:      config.ReactionConfig{Extra: map[string]any{"poll_interval_ms": 10000}},
			wantErr: true,
		},
		{
			name:    "invalid escalation value",
			rc:      config.ReactionConfig{Escalation: "webhook"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildAutoMergeReactionConfig(tt.rc)
			if tt.wantErr {
				if err == nil {
					t.Fatal("BuildAutoMergeReactionConfig() = nil error, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildAutoMergeReactionConfig() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("BuildAutoMergeReactionConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// --- NewState auto-merge flag defaults ---

func TestNewState_AutoMergePreflightFlagDefaultsFalse(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, nil, AgentTotals{})
	if s.AutoMergePreflightFailed {
		t.Error("NewState().AutoMergePreflightFailed = true, want false")
	}
}

func TestNewState_AutoMergePreflightRetryDueAtDefaultsZero(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, nil, AgentTotals{})
	if !s.AutoMergePreflightRetryDueAt.IsZero() {
		t.Errorf("NewState().AutoMergePreflightRetryDueAt = %v, want zero", s.AutoMergePreflightRetryDueAt)
	}
}

func TestNewState_AutoMergeAuthLoggedInitialized(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, nil, AgentTotals{})
	if s.AutoMergeAuthLogged == nil {
		t.Error("NewState().AutoMergeAuthLogged = nil, want non-nil map")
	}
}

func TestAutoMergePreflightRetryDelay_DefaultsFiveMinutes(t *testing.T) {
	t.Parallel()

	if AutoMergePreflightRetryDelay != 5*time.Minute {
		t.Errorf("AutoMergePreflightRetryDelay = %v, want 5m0s", AutoMergePreflightRetryDelay)
	}
}

func TestRuntimeSnapshot_SelfReviewFields(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name            string
		active          bool
		iteration       int
		wantSRActive    bool
		wantSRIteration int
	}{
		{
			name:            "not in self-review phase",
			active:          false,
			iteration:       0,
			wantSRActive:    false,
			wantSRIteration: 0,
		},
		{
			name:            "self-review active iteration 1",
			active:          true,
			iteration:       1,
			wantSRActive:    true,
			wantSRIteration: 1,
		},
		{
			name:            "self-review active iteration 3",
			active:          true,
			iteration:       3,
			wantSRActive:    true,
			wantSRIteration: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, nil, AgentTotals{})
			state.Running["ISS-SR"] = &RunningEntry{
				Identifier:          "PROJ-99",
				Issue:               domain.Issue{ID: "ISS-SR", State: "In Progress"},
				StartedAt:           fixedNow.Add(-2 * time.Minute),
				SelfReviewActive:    tt.active,
				SelfReviewIteration: tt.iteration,
			}

			result := RuntimeSnapshot(state, fixedNow)

			if len(result.Running) != 1 {
				t.Fatalf("len(Running) = %d, want 1", len(result.Running))
			}
			got := result.Running[0]
			if got.SelfReviewActive != tt.wantSRActive {
				t.Errorf("SelfReviewActive = %v, want %v", got.SelfReviewActive, tt.wantSRActive)
			}
			if got.SelfReviewIteration != tt.wantSRIteration {
				t.Errorf("SelfReviewIteration = %d, want %d", got.SelfReviewIteration, tt.wantSRIteration)
			}
		})
	}
}

// --- isKnownReactionKind merge-conflict case ---

func TestIsKnownReactionKind_AcceptsMergeConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{ReactionKindMergeConflict, true},
		{"merge-conflict", true},
		{ReactionKindBotReview, true},
		{ReactionKindAutoMerge, true},
		{"merge_conflicts", false}, // the YAML key, not the runtime discriminator
		{"merge_conflict", false},  // the template variable, not the discriminator
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			if got := isKnownReactionKind(tt.kind); got != tt.want {
				t.Errorf("isKnownReactionKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

// --- BuildMergeConflictReactionConfig tests ---

func TestBuildMergeConflictReactionConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rc      config.ReactionConfig
		want    MergeConflictReactionConfig
		wantErr bool
	}{
		{
			name: "all defaults",
			rc:   config.ReactionConfig{MaxRetries: 1},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      1,
			},
		},
		{
			name: "max_retries consumed verbatim from rc",
			rc:   config.ReactionConfig{MaxRetries: 3},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      3,
			},
		},
		{
			name: "max_retries zero is consumed verbatim (no default re-application)",
			rc:   config.ReactionConfig{MaxRetries: 0},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      0,
			},
		},
		{
			name: "explicit comment escalation",
			rc:   config.ReactionConfig{Escalation: "comment", MaxRetries: 1},
			want: MergeConflictReactionConfig{
				Escalation:      "comment",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      1,
			},
		},
		{
			name: "custom escalation label",
			rc:   config.ReactionConfig{Escalation: "label", EscalationLabel: "conflict-stuck", MaxRetries: 1},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "conflict-stuck",
				PollIntervalMS:  60000,
				MaxRetries:      1,
			},
		},
		{
			name: "poll_interval_ms override above floor",
			rc:   config.ReactionConfig{MaxRetries: 1, Extra: map[string]any{"poll_interval_ms": 120000}},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  120000,
				MaxRetries:      1,
			},
		},
		{
			name: "poll_interval_ms exactly at floor is valid",
			rc:   config.ReactionConfig{MaxRetries: 1, Extra: map[string]any{"poll_interval_ms": 30000}},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  30000,
				MaxRetries:      1,
			},
		},
		{
			name:    "poll_interval_ms below floor errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"poll_interval_ms": 29999}},
			wantErr: true,
		},
		{
			name:    "invalid escalation value errors",
			rc:      config.ReactionConfig{Escalation: "webhook"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildMergeConflictReactionConfig(tt.rc)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BuildMergeConflictReactionConfig(%+v) = %+v, want error", tt.rc, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildMergeConflictReactionConfig(%+v) unexpected error: %v", tt.rc, err)
			}
			if got != tt.want {
				t.Errorf("BuildMergeConflictReactionConfig(%+v) = %+v, want %+v", tt.rc, got, tt.want)
			}
		})
	}
}

// TestBuildMergeConflictReactionConfig_EmptyEscalationDefaultsToLabel verifies
// that an empty escalation defaults to "label" (the direct-call safety net,
// independent of the config-layer default).
func TestBuildMergeConflictReactionConfig_EmptyEscalationDefaultsToLabel(t *testing.T) {
	t.Parallel()

	got, err := BuildMergeConflictReactionConfig(config.ReactionConfig{MaxRetries: 1})
	if err != nil {
		t.Fatalf("BuildMergeConflictReactionConfig: %v", err)
	}
	if got.Escalation != "label" {
		t.Errorf("Escalation = %q, want %q (empty defaults to label)", got.Escalation, "label")
	}
	if got.EscalationLabel != "needs-human" {
		t.Errorf("EscalationLabel = %q, want %q (empty defaults to needs-human)", got.EscalationLabel, "needs-human")
	}
}

// --- isKnownReactionKind label-review case ---

func TestIsKnownReactionKind_AcceptsLabelReview(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{ReactionKindLabelReview, true},
		{"label-review", true},
		{ReactionKindMergeConflict, true},
		{ReactionKindAutoMerge, true},
		{"label_commands", false}, // the YAML key, not the runtime discriminator
		{"label_review", false},   // the template variable, not the discriminator
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			if got := isKnownReactionKind(tt.kind); got != tt.want {
				t.Errorf("isKnownReactionKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

// --- reactionKindPins / isKnownReactionKind / reactionKindPinsWorkspace ---

// TestReactionKindPins covers R11: isKnownReactionKind and
// reactionKindPinsWorkspace both derive from the single reactionKindPins
// map, so the set of known kinds is asserted by iterating the map itself
// rather than by a second hand-written list that could diverge from it.
func TestReactionKindPins(t *testing.T) {
	t.Parallel()

	t.Run("isKnownReactionKind is true for exactly the registered kinds", func(t *testing.T) {
		t.Parallel()

		for kind := range reactionKindPins {
			t.Run(kind, func(t *testing.T) {
				t.Parallel()
				if !isKnownReactionKind(kind) {
					t.Errorf("isKnownReactionKind(%q) = false, want true (registered in reactionKindPins)", kind)
				}
			})
		}

		if isKnownReactionKind("totally-unregistered-kind") {
			t.Error(`isKnownReactionKind("totally-unregistered-kind") = true, want false (unregistered kind)`)
		}
	})

	t.Run("reactionKindPinsWorkspace returns the registered value for each registered kind", func(t *testing.T) {
		t.Parallel()

		for kind, wantPins := range reactionKindPins {
			t.Run(kind, func(t *testing.T) {
				t.Parallel()
				if got := reactionKindPinsWorkspace(kind); got != wantPins {
					t.Errorf("reactionKindPinsWorkspace(%q) = %v, want %v", kind, got, wantPins)
				}
			})
		}
	})

	t.Run("reactionKindPinsWorkspace returns true for an unregistered kind", func(t *testing.T) {
		t.Parallel()

		if got := reactionKindPinsWorkspace("totally-unregistered-kind"); !got {
			t.Error(`reactionKindPinsWorkspace("totally-unregistered-kind") = false, want true (unregistered kind retains)`)
		}
	})
}

// --- BuildLabelReviewReactionConfig tests ---

func TestBuildLabelReviewReactionConfig(t *testing.T) {
	t.Parallel()

	cfg := config.LabelCommandsConfig{
		Provider:       "github",
		ReviewLabel:    "sortie:review",
		FixLabel:       "sortie:fix",
		PollIntervalMS: 45000,
	}

	got := BuildLabelReviewReactionConfig(cfg)

	want := LabelReviewReactionConfig{
		Provider:       "github",
		ReviewLabel:    "sortie:review",
		PollIntervalMS: 45000,
	}
	if got != want {
		t.Errorf("BuildLabelReviewReactionConfig(%+v) = %+v, want %+v", cfg, got, want)
	}
}

// --- isKnownReactionKind label-fix case ---

func TestIsKnownReactionKind_AcceptsLabelFix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{ReactionKindLabelFix, true},
		{"label-fix", true},
		{ReactionKindLabelReview, true},
		{"label_commands", false}, // the YAML key, not the runtime discriminator
		{"label_fix", false},      // the template variable, not the discriminator
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			if got := isKnownReactionKind(tt.kind); got != tt.want {
				t.Errorf("isKnownReactionKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

// --- isKnownReactionKind merge-completion case ---

// TestIsKnownReactionKind_AcceptsMergeCompletion verifies that the kind is
// registered in reactionKindPins, and that reactionKindPinsWorkspace reports
// false for it so a pending entry of this kind does not exclude its
// workspace from sweep candidacy.
func TestIsKnownReactionKind_AcceptsMergeCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{ReactionKindMergeCompletion, true},
		{"merge-completion", true},
		{"reactions.merge_completion", false}, // the YAML key, not the runtime discriminator
		{"merge_completion", false},           // the template variable, not the discriminator
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			if got := isKnownReactionKind(tt.kind); got != tt.want {
				t.Errorf("isKnownReactionKind(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}

	if got := reactionKindPinsWorkspace(ReactionKindMergeCompletion); got {
		t.Errorf("reactionKindPinsWorkspace(%q) = %v, want false", ReactionKindMergeCompletion, got)
	}
}

// --- BuildMergeCompletionReactionConfig tests ---

// TestBuildMergeCompletionReactionConfig covers the nine validation
// failures of the stated evaluation order, the ADR-default success case,
// and the two escalation edge cases asserted against a hand-built
// config.ReactionConfig only.
func TestBuildMergeCompletionReactionConfig(t *testing.T) {
	t.Parallel()

	validTracker := config.TrackerConfig{
		HandoffState:   "in-review",
		ActiveStates:   []string{"doing"},
		TerminalStates: []string{"done"},
	}

	tests := []struct {
		name        string
		rc          config.ReactionConfig
		tracker     config.TrackerConfig
		meta        registry.TrackerMeta
		want        MergeCompletionReactionConfig
		wantErr     bool
		wantErrText string
	}{
		{
			name:        "unset tracker.handoff_state errors naming the field",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": "done"}},
			tracker:     config.TrackerConfig{TerminalStates: []string{"done"}},
			wantErr:     true,
			wantErrText: "tracker.handoff_state",
		},
		{
			name:        "empty tracker.terminal_states errors naming the field",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": "done"}},
			tracker:     config.TrackerConfig{HandoffState: "in-review"},
			wantErr:     true,
			wantErrText: "tracker.terminal_states",
		},
		{
			name:        "absent target_state errors",
			rc:          config.ReactionConfig{},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "target_state is required",
		},
		{
			name:        "non-string target_state errors naming the type",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": 123}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "invalid target_state",
		},
		{
			name:        "target_state equal to handoff_state errors",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": "in-review"}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "must not equal tracker.handoff_state",
		},
		{
			name:        "target_state present in active_states errors",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": "doing"}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "must not be a member of tracker.active_states",
		},
		{
			name:        "target_state absent from terminal_states errors",
			rc:          config.ReactionConfig{Extra: map[string]any{"target_state": "wontfix"}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "must be a member of tracker.terminal_states",
		},
		{
			name: "non-integer poll_interval_ms errors",
			rc: config.ReactionConfig{Extra: map[string]any{
				"target_state":     "done",
				"poll_interval_ms": "soon",
			}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "invalid poll_interval_ms",
		},
		{
			name: "poll_interval_ms below floor errors",
			rc: config.ReactionConfig{Extra: map[string]any{
				"target_state":     "done",
				"poll_interval_ms": 29999,
			}},
			tracker:     validTracker,
			wantErr:     true,
			wantErrText: "poll_interval_ms must be >= 30000",
		},
		{
			name:    "ADR defaults with only target_state set",
			rc:      config.ReactionConfig{Extra: map[string]any{"target_state": "done"}},
			tracker: validTracker,
			want: MergeCompletionReactionConfig{
				TargetState:     "done",
				PollIntervalMS:  60000,
				Escalation:      "label",
				EscalationLabel: "needs-human",
			},
		},
		{
			name: "invalid escalation errors against a hand-built config with no tracker set",
			rc:   config.ReactionConfig{Escalation: "webhook"},
			// tracker is the zero value: the escalation check runs before
			// any tracker prerequisite, so this is a unit-level assertion
			// only, not an end-to-end one.
			wantErr:     true,
			wantErrText: "invalid escalation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildMergeCompletionReactionConfig(tt.rc, tt.tracker, tt.meta)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BuildMergeCompletionReactionConfig(%+v, %+v) = %+v, want error", tt.rc, tt.tracker, got)
				}
				if tt.wantErrText != "" && !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("BuildMergeCompletionReactionConfig(%+v, %+v) error = %q, want substring %q", tt.rc, tt.tracker, err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildMergeCompletionReactionConfig(%+v, %+v) unexpected error: %v", tt.rc, tt.tracker, err)
			}
			if got != tt.want {
				t.Errorf("BuildMergeCompletionReactionConfig(%+v, %+v) = %+v, want %+v", tt.rc, tt.tracker, got, tt.want)
			}
		})
	}
}

// TestBuildMergeCompletionReactionConfig_StateListFallbackAsymmetry
// verifies that the terminal list never falls back to registry.TrackerMeta
// defaults, while the active list does, and the fallback only ever makes
// the active check stricter.
func TestBuildMergeCompletionReactionConfig_StateListFallbackAsymmetry(t *testing.T) {
	t.Parallel()

	t.Run("terminal defaults never fill in an empty written list", func(t *testing.T) {
		t.Parallel()

		rc := config.ReactionConfig{Extra: map[string]any{"target_state": "done"}}
		tracker := config.TrackerConfig{HandoffState: "in-review"}
		meta := registry.TrackerMeta{DefaultTerminalStates: []string{"done"}}

		_, err := BuildMergeCompletionReactionConfig(rc, tracker, meta)
		if err == nil {
			t.Fatal("BuildMergeCompletionReactionConfig(...) = nil error, want error (terminal defaults must not fill in)")
		}
		if !strings.Contains(err.Error(), "tracker.terminal_states") {
			t.Errorf("BuildMergeCompletionReactionConfig(...) error = %q, want substring %q", err.Error(), "tracker.terminal_states")
		}
	})

	t.Run("active defaults fill in and reject a target drawn from them", func(t *testing.T) {
		t.Parallel()

		rc := config.ReactionConfig{Extra: map[string]any{"target_state": "doing"}}
		tracker := config.TrackerConfig{HandoffState: "in-review", TerminalStates: []string{"doing", "done"}}
		meta := registry.TrackerMeta{DefaultActiveStates: []string{"doing"}}

		_, err := BuildMergeCompletionReactionConfig(rc, tracker, meta)
		if err == nil {
			t.Fatal("BuildMergeCompletionReactionConfig(...) = nil error, want error (active defaults must fill in and reject)")
		}
		if !strings.Contains(err.Error(), "tracker.active_states") {
			t.Errorf("BuildMergeCompletionReactionConfig(...) error = %q, want substring %q", err.Error(), "tracker.active_states")
		}
	})

	t.Run("a target outside the active defaults and inside the written terminal list validates", func(t *testing.T) {
		t.Parallel()

		rc := config.ReactionConfig{Extra: map[string]any{"target_state": "done"}}
		tracker := config.TrackerConfig{HandoffState: "in-review", TerminalStates: []string{"done"}}
		meta := registry.TrackerMeta{DefaultActiveStates: []string{"doing"}}

		got, err := BuildMergeCompletionReactionConfig(rc, tracker, meta)
		if err != nil {
			t.Fatalf("BuildMergeCompletionReactionConfig(...) unexpected error: %v", err)
		}
		if got.TargetState != "done" {
			t.Errorf("BuildMergeCompletionReactionConfig(...).TargetState = %q, want %q", got.TargetState, "done")
		}
	})
}

// --- BuildLabelFixReactionConfig tests ---

func TestBuildLabelFixReactionConfig(t *testing.T) {
	t.Parallel()

	cfg := config.LabelCommandsConfig{
		Provider:       "github",
		ReviewLabel:    "sortie:review",
		FixLabel:       "sortie:fix",
		PollIntervalMS: 45000,
	}

	got := BuildLabelFixReactionConfig(cfg)

	want := LabelFixReactionConfig{
		Provider:       "github",
		FixLabel:       "sortie:fix",
		PollIntervalMS: 45000,
	}
	if got != want {
		t.Errorf("BuildLabelFixReactionConfig(%+v) = %+v, want %+v", cfg, got, want)
	}
}

// TestPopulateParked verifies that PopulateParked loads persisted rows into
// state.Parked, skips a row with an empty issue_id while logging a
// warning, loads a row with an empty parked_state rather than skipping it,
// and that the resulting state.Parked is honored by ShouldDispatch's gate.
func TestPopulateParked(t *testing.T) {
	t.Parallel()

	t.Run("loads rows into state.Parked", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		rows := []persistence.ParkedIssue{
			{
				IssueID:      "id-1",
				Identifier:   "PROJ-1",
				DisplayID:    "PROJ-1-display",
				Reason:       parkReasonAgentBlocked,
				ParkedState:  "In Progress",
				Label:        "needs-human",
				LabelApplied: true,
				ParkedAt:     "2026-08-17T00:00:00Z",
			},
			{
				IssueID:     "id-2",
				Identifier:  "PROJ-2",
				Reason:      parkReasonHandoffAbsence,
				ParkedState: "",
				Label:       "needs-human",
				ParkedAt:    "2026-08-17T01:00:00Z",
			},
		}

		PopulateParked(state, rows, nil)

		if len(state.Parked) != 2 {
			t.Fatalf("len(state.Parked) = %d, want 2", len(state.Parked))
		}

		entry1, ok := state.Parked["id-1"]
		if !ok {
			t.Fatal("state.Parked missing id-1")
		}
		if entry1.Identifier != "PROJ-1" || entry1.DisplayID != "PROJ-1-display" ||
			entry1.Reason != parkReasonAgentBlocked || entry1.ParkedState != "In Progress" ||
			entry1.Label != "needs-human" || !entry1.LabelApplied {
			t.Errorf("state.Parked[id-1] = %+v, want fields to match the persisted row", entry1)
		}
		if !entry1.ParkedAt.Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("state.Parked[id-1].ParkedAt = %v, want 2026-08-17T00:00:00Z", entry1.ParkedAt)
		}

		// A row loaded with an empty parked_state is loaded, not skipped:
		// only an empty issue_id is a malformed row.
		entry2, ok := state.Parked["id-2"]
		if !ok {
			t.Fatal("state.Parked missing id-2")
		}
		if entry2.ParkedState != "" {
			t.Errorf("state.Parked[id-2].ParkedState = %q, want empty", entry2.ParkedState)
		}
	})

	t.Run("skips a row with an empty issue_id and logs a warning", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		rows := []persistence.ParkedIssue{
			{IssueID: "", Identifier: "PROJ-MALFORMED", Reason: parkReasonAgentBlocked, ParkedAt: "2026-08-17T00:00:00Z"},
			{IssueID: "id-3", Identifier: "PROJ-3", Reason: parkReasonAgentBlocked, ParkedAt: "2026-08-17T00:00:00Z"},
		}
		var logs bytes.Buffer

		PopulateParked(state, rows, debugLogger(t, &logs))

		if len(state.Parked) != 1 {
			t.Fatalf("len(state.Parked) = %d, want 1 (the malformed row skipped)", len(state.Parked))
		}
		if _, ok := state.Parked["id-3"]; !ok {
			t.Error("state.Parked missing id-3")
		}
		if !strings.Contains(logs.String(), "skipping malformed park record") {
			t.Errorf("missing malformed-row warning\nlogs: %s", logs.String())
		}
	})

	t.Run("resulting state.Parked is honored by ShouldDispatch", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, nil, AgentTotals{})
		PopulateParked(state, []persistence.ParkedIssue{
			{IssueID: "id-4", Identifier: "PROJ-4", Reason: parkReasonAgentBlocked, ParkedAt: "2026-08-17T00:00:00Z"},
		}, nil)

		issue := domain.Issue{ID: "id-4", Identifier: "PROJ-4", Title: "T", State: "To Do"}
		if ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
			t.Error("ShouldDispatch dispatched an issue PopulateParked loaded into state.Parked")
		}
	})
}

// TestRuntimeSnapshot_ParkedFields verifies that RuntimeSnapshot reports
// ParkedCount always, including zero, and Parked/ParkedReason only when
// state.Parked is non-empty, mirroring the BudgetExhausted* projection.
func TestRuntimeSnapshot_ParkedFields(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("empty Parked produces zero count and nil slices", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})

		result := RuntimeSnapshot(state, fixedNow)

		if result.ParkedCount != 0 {
			t.Errorf("ParkedCount = %d, want 0", result.ParkedCount)
		}
		if result.Parked != nil {
			t.Errorf("Parked = %v, want nil", result.Parked)
		}
		if result.ParkedReason != nil {
			t.Errorf("ParkedReason = %v, want nil", result.ParkedReason)
		}
	})

	t.Run("non-empty Parked sorted, counted, and reasoned", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, nil, AgentTotals{})
		state.Parked["ISS-C"] = &ParkedEntry{Identifier: "PROJ-C", Reason: parkReasonAgentBlocked}
		state.Parked["ISS-A"] = &ParkedEntry{Identifier: "PROJ-A", Reason: parkReasonHandoffAbsence}
		state.Parked["ISS-B"] = &ParkedEntry{Identifier: "PROJ-B", Reason: parkReasonAgentBlocked}

		result := RuntimeSnapshot(state, fixedNow)

		if result.ParkedCount != 3 {
			t.Errorf("ParkedCount = %d, want 3", result.ParkedCount)
		}
		want := []string{"ISS-A", "ISS-B", "ISS-C"}
		if len(result.Parked) != len(want) {
			t.Fatalf("len(Parked) = %d, want %d", len(result.Parked), len(want))
		}
		for i, id := range want {
			if result.Parked[i] != id {
				t.Errorf("Parked[%d] = %q, want %q", i, result.Parked[i], id)
			}
		}
		if got := result.ParkedReason["ISS-A"]; got != parkReasonHandoffAbsence {
			t.Errorf("ParkedReason[ISS-A] = %q, want %q", got, parkReasonHandoffAbsence)
		}
		if got := result.ParkedReason["ISS-C"]; got != parkReasonAgentBlocked {
			t.Errorf("ParkedReason[ISS-C] = %q, want %q", got, parkReasonAgentBlocked)
		}
	})
}
