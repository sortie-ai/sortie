package orchestrator

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestNewState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		pollIntervalMS       int
		maxConcurrentAgents  int
		maxTokens            int
		maxConcurrentByState map[string]int
		totals               AgentTotals
		wantMaxByStateLen    int
		checkAlias           bool
	}{
		{
			name:                 "nil state limits map becomes empty non-nil map",
			pollIntervalMS:       5000,
			maxConcurrentAgents:  10,
			maxTokens:            0,
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
			maxTokens:           250000,
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

			s := NewState(tt.pollIntervalMS, tt.maxConcurrentAgents, tt.maxTokens, tt.maxConcurrentByState, tt.totals)

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
			if s.MaxTokens != tt.maxTokens {
				t.Errorf("MaxTokens = %d, want %d", s.MaxTokens, tt.maxTokens)
			}
			if s.AgentTotals != tt.totals {
				t.Errorf("AgentTotals = %+v, want %+v", s.AgentTotals, tt.totals)
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

		state := NewState(5000, 10, 0, nil, AgentTotals{SecondsRunning: 42.5})
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
		if result.UsageWindows == nil {
			t.Error("UsageWindows = nil, want non-nil empty map")
		}
		if len(result.UsageWindows) != 0 {
			t.Errorf("len(UsageWindows) = %d, want 0", len(result.UsageWindows))
		}
	})

	t.Run("running sessions with computed seconds_running", func(t *testing.T) {
		t.Parallel()

		startA := fixedNow.Add(-60 * time.Second)
		startB := fixedNow.Add(-120 * time.Second)
		eventTime := fixedNow.Add(-10 * time.Second)

		state := NewState(5000, 10, 0, nil, AgentTotals{
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

		wantSeconds := 100.0 + 60.0 + 120.0
		if math.Abs(result.AgentTotals.SecondsRunning-wantSeconds) > 0.001 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want %f", result.AgentTotals.SecondsRunning, wantSeconds)
		}

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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

	t.Run("kind with every window reset", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		older := fixedNow.Add(-3 * time.Hour)
		newest := fixedNow.Add(-time.Hour)
		state.UsageWindows = map[string][]domain.UsageWindow{
			"claude-code": {
				{Key: "five_hour", Label: "5-hour limit", ResetsAt: fixedNow.Add(-30 * time.Minute), ReportedAt: older},
				{Key: "seven_day", Label: "Weekly limit", ResetsAt: fixedNow, ReportedAt: newest},
			},
		}

		result := RuntimeSnapshot(state, fixedNow)

		got, ok := result.UsageWindows["claude-code"]
		if !ok {
			t.Fatal("UsageWindows[claude-code] missing, want the kind served after all its windows reset")
		}
		if got.Windows == nil {
			t.Error("Windows = nil, want non-nil empty slice")
		}
		if len(got.Windows) != 0 {
			t.Errorf("len(Windows) = %d, want 0 (windows reset at or before now are not served)", len(got.Windows))
		}
		if !got.ReportedAt.Equal(newest) {
			t.Errorf("ReportedAt = %v, want %v (newest report of every held window)", got.ReportedAt, newest)
		}
	})

	t.Run("clock skew guard future StartedAt", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{SecondsRunning: 50.0})
		state.Running["future-issue"] = &RunningEntry{
			Identifier: "MT-400",
			Issue:      domain.Issue{ID: "future-issue", State: "In Progress"},
			StartedAt:  fixedNow.Add(10 * time.Second),
		}

		result := RuntimeSnapshot(state, fixedNow)

		if result.AgentTotals.SecondsRunning != 50.0 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want 50.0 (future StartedAt should contribute 0)", result.AgentTotals.SecondsRunning)
		}
	})

	t.Run("zero timestamp guard", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{SecondsRunning: 50.0})
		state.Running["zero-ts"] = &RunningEntry{
			Identifier: "MT-500",
			Issue:      domain.Issue{ID: "zero-ts", State: "In Progress"},
			StartedAt:  time.Time{},
		}

		result := RuntimeSnapshot(state, fixedNow)

		if result.AgentTotals.SecondsRunning != 50.0 {
			t.Errorf("AgentTotals.SecondsRunning = %f, want 50.0 (zero StartedAt should contribute 0)", result.AgentTotals.SecondsRunning)
		}
	})

	t.Run("WorkspacePath copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

	t.Run("extended fields copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{CacheReadTokens: 999})
		state.Running["ext-1"] = &RunningEntry{
			Identifier:      "MT-EXT",
			Issue:           domain.Issue{ID: "ext-1", State: "In Progress"},
			StartedAt:       fixedNow.Add(-10 * time.Second),
			CacheReadTokens: 2500,
			ModelName:       "claude-sonnet-4-20250514",
			APIRequestCount: 7,
			RequestsByModel: map[string]int{"claude-sonnet-4-20250514": 5, "claude-opus-4-20250514": 2},

			// The breakdown reaches the snapshot only when the request
			// count is a measurement and attribution names a model.
			UsageArrival:     registry.UsageArrivalIncremental,
			UsageAttribution: registry.UsageAttributionPerModel,
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

		if result.AgentTotals.CacheReadTokens != 999 {
			t.Errorf("AgentTotals.CacheReadTokens = %d, want 999", result.AgentTotals.CacheReadTokens)
		}
	})

	t.Run("RequestsByModel snapshot is an isolated copy", func(t *testing.T) {
		t.Parallel()

		rbm := map[string]int{"model-a": 3}
		state := NewState(5000, 10, 0, nil, AgentTotals{})
		state.Running["iso-1"] = &RunningEntry{
			Identifier:       "MT-ISO",
			Issue:            domain.Issue{ID: "iso-1", State: "In Progress"},
			StartedAt:        fixedNow.Add(-5 * time.Second),
			RequestsByModel:  rbm,
			APIRequestCount:  3,
			UsageArrival:     registry.UsageArrivalIncremental,
			UsageAttribution: registry.UsageAttributionPerModel,
		}

		result := RuntimeSnapshot(state, fixedNow)

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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

	// RuntimeSnapshot is the only site that resolves the request verdict,
	// so these cases must run through it rather than a hand-built
	// SnapshotRunningEntry, which would bypass the gate.
	t.Run("RequestsByModel absent when the request verdict is false", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		state.Running["unmeasured"] = &RunningEntry{
			Identifier:       "MT-UNMEASURED",
			Issue:            domain.Issue{ID: "unmeasured", State: "In Progress"},
			StartedAt:        fixedNow.Add(-1 * time.Second),
			TurnCount:        2,
			APIRequestCount:  0,
			RequestsByModel:  map[string]int{"claude-sonnet-4-20250514": 5},
			UsageArrival:     registry.UsageArrivalIncremental,
			UsageAttribution: registry.UsageAttributionPerModel,
		}

		result := RuntimeSnapshot(state, fixedNow)

		snap := result.Running[0]
		if snap.APIRequestsMeasured {
			t.Fatal("APIRequestsMeasured = true, want false (turns began, count is zero)")
		}
		if snap.RequestsByModel != nil {
			t.Errorf("RequestsByModel = %v, want nil when the request verdict is false", snap.RequestsByModel)
		}
	})

	t.Run("RequestsByModel absent when attribution does not name a model", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		state.Running["session-total"] = &RunningEntry{
			Identifier:       "MT-SESSTOTAL",
			Issue:            domain.Issue{ID: "session-total", State: "In Progress"},
			StartedAt:        fixedNow.Add(-1 * time.Second),
			APIRequestCount:  4,
			RequestsByModel:  map[string]int{"claude-sonnet-4-20250514": 4},
			UsageArrival:     registry.UsageArrivalIncremental,
			UsageAttribution: registry.UsageAttributionSessionTotal,
		}

		result := RuntimeSnapshot(state, fixedNow)

		snap := result.Running[0]
		if !snap.APIRequestsMeasured {
			t.Fatal("APIRequestsMeasured = false, want true (count above zero)")
		}
		if snap.RequestsByModel != nil {
			t.Errorf("RequestsByModel = %v, want nil when attribution does not name a model", snap.RequestsByModel)
		}
	})

	t.Run("ToolTimeMs and APITimeMs copied to snapshot", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})

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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		// Out of order, with empty Identifiers, so the sort falls through
		// to the IssueID tiebreaker.
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		state.BudgetExhausted["ISS-X"] = &BudgetExhaustedEntry{}

		result := RuntimeSnapshot(state, fixedNow)

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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		entry := &BudgetExhaustedEntry{Identifier: "PROJ-9", Reason: budgetReasonSession, UsedSessions: 3}
		state.BudgetExhausted["iss-9"] = entry

		result := RuntimeSnapshot(state, fixedNow)
		if len(result.BudgetExhausted) != 1 {
			t.Fatalf("len(BudgetExhausted) = %d, want 1", len(result.BudgetExhausted))
		}
		snapshotted := result.BudgetExhausted[0]

		// Mutate the pointee after the snapshot: a pointer-copying
		// snapshot would observe this; a value-copied one must not.
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

		state := NewState(5000, 10, 0, nil, AgentTotals{})
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

func TestRuntimeSnapshot_ConfiguredSettingsBesideTheReportedModel(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.Running["ISS-1"] = &RunningEntry{
		Issue: domain.Issue{ID: "ISS-1"}, StartedAt: now, RuleName: "cheap",
		ConfiguredModel: "configured-model", ConfiguredEffort: "low", ModelName: "reported-model",
	}

	result := RuntimeSnapshot(state, now)

	if len(result.Running) != 1 {
		t.Fatalf("len(Running) = %d, want 1", len(result.Running))
	}
	if got := result.Running[0]; got.RuleName != "cheap" || got.ConfiguredModel != "configured-model" || got.ConfiguredEffort != "low" || got.ModelName != "reported-model" {
		t.Errorf("snapshot rule, configured model, configured effort, model_name = %q, %q, %q, %q, want %q, %q, %q, %q",
			got.RuleName, got.ConfiguredModel, got.ConfiguredEffort, got.ModelName, "cheap", "configured-model", "low", "reported-model")
	}
}

func TestRuntimeSnapshot_StagePosition(t *testing.T) {
	t.Parallel()

	chains := [][]string{{"specify", "implement"}, {"triage", "implement"}}
	tests := []struct {
		name     string
		chains   [][]string
		rule     string
		previous string
		hops     int
		want     *SnapshotStagePosition
	}{
		{"no chains configured", nil, "specify", "", 0, nil},
		{"first stage of the first chain", chains, "specify", "", 0,
			&SnapshotStagePosition{Chain: chains[0], Current: "specify", HopCeiling: 4}},
		{"previous rule selects the chain that lists it before the rule", chains, "implement", "triage", 2,
			&SnapshotStagePosition{Chain: chains[1], Current: "implement", HopCount: 2, HopCeiling: 4}},
		{"previous rule on no chain falls back to the first chain holding the rule", chains, "implement", "gone", 1,
			&SnapshotStagePosition{Chain: chains[0], Current: "implement", HopCount: 1, HopCeiling: 4}},
		{"rule on no chain", chains, "review", "", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, 0, nil, AgentTotals{})
			state.StageChains, state.HopCeiling = tt.chains, 4
			lineage := StageLineage{PreviousRule: tt.previous}
			agent := RuleAgent{AgentKind: "claude-code", ConfiguredModel: "sonnet"}
			state.RuleAgents = map[string]RuleAgent{tt.rule: agent}
			state.Running["run"] = &RunningEntry{Issue: domain.Issue{ID: "run"}, RuleName: tt.rule, StageLineage: lineage}
			state.RetryAttempts["retry"] = &RetryEntry{IssueID: "retry", RuleName: tt.rule, StageLineage: lineage}
			state.BudgetExhausted["budget"] = &BudgetExhaustedEntry{RuleName: tt.rule, StageLineage: lineage}
			state.Parked["parked"] = &ParkedEntry{RuleName: tt.rule, StageLineage: lineage}
			state.PendingReactions[ReactionKey("pending", ReactionKindCI)] = &PendingReaction{IssueID: "pending", Kind: ReactionKindCI, RuleName: tt.rule, StageLineage: lineage}
			state.StageHops["hop"] = &StageHopEntry{SourceRule: tt.previous, TargetRule: tt.rule, Count: tt.hops}
			if tt.hops > 0 {
				for _, id := range []string{"run", "retry", "budget", "parked", "pending"} {
					state.StageHops[id] = &StageHopEntry{Count: tt.hops}
				}
			}

			result := RuntimeSnapshot(state, time.Time{})

			hopIndex := slices.IndexFunc(result.BetweenStages, func(row SnapshotStageHop) bool { return row.IssueID == "hop" })
			if len(result.Running) != 1 || len(result.Retrying) != 1 || len(result.BudgetExhausted) != 1 || len(result.Parked) != 1 || len(result.PendingReactions) != 1 || hopIndex < 0 {
				t.Fatalf("RuntimeSnapshot rows = %d running, %d retrying, %d budget, %d parked, %d pending, between stages %+v, want 1 of each and the hop row",
					len(result.Running), len(result.Retrying), len(result.BudgetExhausted), len(result.Parked), len(result.PendingReactions), result.BetweenStages)
			}
			hop := result.BetweenStages[hopIndex]
			if hop.RuleName != tt.rule || hop.AgentKind != agent.AgentKind || hop.ConfiguredModel != agent.ConfiguredModel {
				t.Errorf("BetweenStages hop rule, agent kind, model = %q, %q, %q, want %q, %q, %q",
					hop.RuleName, hop.AgentKind, hop.ConfiguredModel, tt.rule, agent.AgentKind, agent.ConfiguredModel)
			}
			for kind, got := range map[string]*SnapshotStagePosition{
				"Running":          result.Running[0].StagePosition,
				"Retrying":         result.Retrying[0].StagePosition,
				"BudgetExhausted":  result.BudgetExhausted[0].StagePosition,
				"Parked":           result.Parked[0].StagePosition,
				"PendingReactions": result.PendingReactions[0].StagePosition,
				"BetweenStages":    hop.StagePosition,
			} {
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("%s[0].StagePosition (chains %v, rule %q, previous %q) = %+v, want %+v", kind, tt.chains, tt.rule, tt.previous, got, tt.want)
				}
			}
		})
	}
}

func TestRecordIssueRead(t *testing.T) {
	t.Parallel()

	now := time.Now()
	steps := []struct {
		name      string
		startedAt time.Time
		read      string
		want      string
	}{
		{"first read after dispatch", now, "ai:in-progress", "ai:in-progress"},
		{"equal start is refused", now, "ai:done", "ai:in-progress"},
		{"older start is refused", now.Add(-time.Second), "ai:done", "ai:in-progress"},
	}
	entry := &RunningEntry{Issue: domain.Issue{State: "ai:ready", Labels: []string{"ai:ready"}}}

	for _, step := range steps {
		recordIssueRead(entry, domain.IssueStateAndLabels{State: step.read, Labels: []string{step.read}}, step.startedAt)

		if entry.Issue.State != step.want || !slices.Equal(entry.Issue.Labels, []string{step.want}) {
			t.Errorf("%s: recordIssueRead state, labels = %q, %q, want %q for both", step.name, entry.Issue.State, entry.Issue.Labels, step.want)
		}
	}
}

func TestRuntimeSnapshot_PendingReactionWatchEndsAt(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	windows := map[string]time.Duration{ReactionKindCI: 24 * time.Hour, ReactionKindReview: 30 * time.Minute}
	tests := []struct {
		name  string
		entry PendingReaction
		want  *time.Time
	}{
		{"ci counts from the recorded head", PendingReaction{Kind: ReactionKindCI, HeadRecordedAt: created.Add(10 * time.Minute)}, at(24*time.Hour + 10*time.Minute)},
		{"ci without a recorded head counts from creation", PendingReaction{Kind: ReactionKindCI}, at(24 * time.Hour)},
		{"review counts from the restart", PendingReaction{Kind: ReactionKindReview, WatchRestartedAt: created.Add(20 * time.Minute)}, at(50 * time.Minute)},
		{"kind without a window", PendingReaction{Kind: ReactionKindMergeCompletion}, nil},
		{"review awaiting approval has no end", PendingReaction{Kind: ReactionKindReview, LastCheck: &ReactionCheck{Comments: &CommentCheck{AwaitingApproval: true}}}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, 0, nil, AgentTotals{})
			state.ReactionWatchWindows = windows
			entry := tt.entry
			entry.IssueID, entry.CreatedAt = "ISS-1", created
			state.PendingReactions[ReactionKey(entry.IssueID, entry.Kind)] = &entry

			rows := RuntimeSnapshot(state, created).PendingReactions

			if len(rows) != 1 {
				t.Fatalf("len(PendingReactions) = %d, want 1", len(rows))
			}
			got := rows[0].WatchEndsAt
			if (got == nil) != (tt.want == nil) || got != nil && !got.Equal(*tt.want) {
				t.Errorf("PendingReactions[0].WatchEndsAt for kind %q = %v, want %v", entry.Kind, got, tt.want)
			}
		})
	}
}

func TestRuntimeSnapshot_MasksRegisteredValueInIssueIdentity(t *testing.T) {
	t.Parallel()

	value := eventTestSecret(t)
	redact.Add("test.state issue identity", value)
	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.Running["ISS-1"] = &RunningEntry{
		Issue: domain.Issue{
			ID: "ISS-1", Title: "rotate " + value, Labels: []string{"ops", "leak-" + value},
			IssueType: "type-" + value, State: "state-" + value,
		},
		Continuation: ContinuationTarget{PRNumber: 7, Owner: "acme", Repo: "repo-" + value, Attempt: 1},
	}
	state.RetryAttempts["ISS-2"] = &RetryEntry{
		IssueID: "ISS-2",
		IssueIdentity: IssueIdentity{
			Title: "rotate " + value, IssueType: "type-" + value,
			Labels: []string{"ops", "leak-" + value}, State: "state-" + value,
		},
	}
	state.BudgetExhausted["ISS-5"] = &BudgetExhaustedEntry{
		Identifier: "PROJ-5", IssueIdentity: IssueIdentity{State: "state-" + value},
	}
	state.Parked["ISS-3"] = &ParkedEntry{
		Identifier: "PROJ-3", IssueIdentity: IssueIdentity{Title: "rotate " + value, IssueType: "type-" + value, Labels: []string{"ops", "leak-" + value}},
	}
	state.StageHops["ISS-6"] = &StageHopEntry{
		Identifier: "PROJ-6",
		IssueIdentity: IssueIdentity{
			Title: "rotate " + value, URL: "https://forge.example.com/" + value,
			IssueType: "type-" + value, Labels: []string{"ops", "leak-" + value}, State: "state-" + value,
		},
	}
	state.PendingReactions[ReactionKey("ISS-4", ReactionKindCI)] = &PendingReaction{
		IssueID:    "ISS-4",
		Identifier: "PROJ-4",
		IssueIdentity: IssueIdentity{
			IssueType: "type-" + value, Labels: []string{"ops", "leak-" + value}, State: "state-" + value,
		},
		Kind:           ReactionKindCI,
		PullRequestURL: "https://forge.example.com/" + value,
		KindData:       &CIReactionData{PRNumber: 7, Owner: "acme", Repo: "widgets", Branch: "feature/" + value},
		LastCheck:      &ReactionCheck{CI: &CICheck{Status: "failing", FailingChecks: []string{"build-" + value}, Failing: 1}},
	}

	result := RuntimeSnapshot(state, now)

	if len(result.Running) != 1 || len(result.Retrying) != 1 || len(result.Parked) != 1 || len(result.BudgetExhausted) != 1 {
		t.Fatalf("RuntimeSnapshot rows = %d running, %d retrying, %d parked, %d budget, want 1, 1, 1, 1",
			len(result.Running), len(result.Retrying), len(result.Parked), len(result.BudgetExhausted))
	}
	running, retrying := result.Running[0], result.Retrying[0]
	if want := "rotate " + redact.Marker; running.IssueTitle != want {
		t.Errorf("Running[0].IssueTitle = %q, want %q", running.IssueTitle, want)
	}
	if want := []string{"ops", "leak-" + redact.Marker}; !slices.Equal(running.IssueLabels, want) {
		t.Errorf("Running[0].IssueLabels = %q, want %q", running.IssueLabels, want)
	}
	if want := "type-" + redact.Marker; running.IssueType != want {
		t.Errorf("Running[0].IssueType = %q, want %q", running.IssueType, want)
	}
	if want := "state-" + redact.Marker; running.State != want {
		t.Errorf("Running[0].State = %q, want %q", running.State, want)
	}
	if want := "acme/repo-" + redact.Marker; running.Continuation == nil || running.Continuation.Repository != want {
		t.Errorf("Running[0].Continuation = %+v, want repository %q", running.Continuation, want)
	}
	if want := "rotate " + redact.Marker; retrying.IssueTitle != want {
		t.Errorf("Retrying[0].IssueTitle = %q, want %q", retrying.IssueTitle, want)
	}
	if want := "type-" + redact.Marker; retrying.IssueType != want {
		t.Errorf("Retrying[0].IssueType = %q, want %q", retrying.IssueType, want)
	}
	if want := []string{"ops", "leak-" + redact.Marker}; !slices.Equal(retrying.IssueLabels, want) {
		t.Errorf("Retrying[0].IssueLabels = %q, want %q", retrying.IssueLabels, want)
	}
	if want := "state-" + redact.Marker; retrying.State != want {
		t.Errorf("Retrying[0].State = %q, want %q", retrying.State, want)
	}
	if want := "state-" + redact.Marker; result.BudgetExhausted[0].State != want {
		t.Errorf("BudgetExhausted[0].State = %q, want %q", result.BudgetExhausted[0].State, want)
	}
	if want := "rotate " + redact.Marker; result.Parked[0].IssueTitle != want {
		t.Errorf("Parked[0].IssueTitle = %q, want %q", result.Parked[0].IssueTitle, want)
	}
	if len(result.BetweenStages) != 1 {
		t.Fatalf("len(BetweenStages) = %d, want 1", len(result.BetweenStages))
	}
	if len(result.PendingReactions) != 1 {
		t.Fatalf("len(PendingReactions) = %d, want 1", len(result.PendingReactions))
	}
	type issueFacts struct {
		IssueType string
		Labels    []string
	}
	wantFacts := issueFacts{"type-" + redact.Marker, []string{"ops", "leak-" + redact.Marker}}
	parked, hopRow, reaction := result.Parked[0], result.BetweenStages[0], result.PendingReactions[0]
	for name, got := range map[string]issueFacts{
		"Parked[0]":           {parked.IssueType, parked.IssueLabels},
		"BetweenStages[0]":    {hopRow.IssueType, hopRow.IssueLabels},
		"PendingReactions[0]": {reaction.IssueType, reaction.IssueLabels},
	} {
		if !reflect.DeepEqual(got, wantFacts) {
			t.Errorf("%s issue type, labels = %+v, want %+v", name, got, wantFacts)
		}
	}
	for name, got := range map[string]string{"BetweenStages[0]": hopRow.State, "PendingReactions[0]": reaction.State} {
		if want := "state-" + redact.Marker; got != want {
			t.Errorf("%s.State = %q, want %q", name, got, want)
		}
	}
	if hop, wantTitle, wantURL := result.BetweenStages[0], "rotate "+redact.Marker, "https://forge.example.com/"+redact.Marker; hop.IssueTitle != wantTitle || hop.IssueURL != wantURL {
		t.Errorf("BetweenStages[0] title, url = %q, %q, want %q, %q", hop.IssueTitle, hop.IssueURL, wantTitle, wantURL)
	}
	row := result.PendingReactions[0]
	if want := "feature/" + redact.Marker; row.PullRequest.Branch != want {
		t.Errorf("PendingReactions[0].PullRequest.Branch = %q, want %q", row.PullRequest.Branch, want)
	}
	if want := "https://forge.example.com/" + redact.Marker; row.PullRequest.URL != want {
		t.Errorf("PendingReactions[0].PullRequest.URL = %q, want %q", row.PullRequest.URL, want)
	}
	if want := []string{"build-" + redact.Marker}; !slices.Equal(row.LastCheck.CI.FailingChecks, want) {
		t.Errorf("PendingReactions[0].LastCheck.CI.FailingChecks = %q, want %q", row.LastCheck.CI.FailingChecks, want)
	}
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

			state := NewState(5000, 4, 0, nil, AgentTotals{})
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

func TestWatchWindowMS(t *testing.T) {
	t.Parallel()

	const def = reactionWatchWindowDefaultMS

	tests := []struct {
		name        string
		extra       map[string]any
		want        int
		wantErr     bool
		wantErrText string
	}{
		{
			name:  "absent key returns default",
			extra: map[string]any{},
			want:  def,
		},
		{
			name:  "nil extra returns default",
			extra: nil,
			want:  def,
		},
		{
			name:  "explicit zero means no time limit",
			extra: map[string]any{"watch_window_ms": 0},
			want:  0,
		},
		{
			name:  "valid override",
			extra: map[string]any{"watch_window_ms": 3600000},
			want:  3600000,
		},
		{
			name:  "value exactly at the ceiling is valid",
			extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS)},
			want:  int(config.MaxWatchWindowMS),
		},
		{
			name:        "negative value is rejected with exact message",
			extra:       map[string]any{"watch_window_ms": -1},
			wantErr:     true,
			wantErrText: "watch_window_ms must be non-negative, got -1",
		},
		{
			name:    "value above the ceiling is rejected with exact message",
			extra:   map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1},
			wantErr: true,
			wantErrText: fmt.Sprintf(
				"watch_window_ms must not exceed %d (about 292 years); use 0 for no time limit, got %d",
				config.MaxWatchWindowMS, config.MaxWatchWindowMS+1,
			),
		},
		{
			name:    "non-numeric value is rejected",
			extra:   map[string]any{"watch_window_ms": "soon"},
			wantErr: true,
		},
		{
			name:    "fractional value is rejected",
			extra:   map[string]any{"watch_window_ms": 1.5},
			wantErr: true,
		},
		{
			name:    "NaN is rejected",
			extra:   map[string]any{"watch_window_ms": math.NaN()},
			wantErr: true,
		},
		{
			name:  "uint64 in range is accepted",
			extra: map[string]any{"watch_window_ms": uint64(3600000)},
			want:  3600000,
		},
		{
			name:    "uint64 out of range is rejected with the range diagnostic",
			extra:   map[string]any{"watch_window_ms": uint64(9223372036854775808)},
			wantErr: true,
			wantErrText: "invalid watch_window_ms: " +
				"value is outside the range an integer setting accepts, -9223372036854775808 to 9223372036854775807",
		},
		{
			name:    "float64 out of range is rejected with the range diagnostic",
			extra:   map[string]any{"watch_window_ms": float64(1e20)},
			wantErr: true,
			wantErrText: "invalid watch_window_ms: " +
				"value is outside the range an integer setting accepts, -9223372036854775808 to 9223372036854775807",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := watchWindowMS(tt.extra, def)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("watchWindowMS(%v, %d) = %d, want error", tt.extra, def, got)
				}
				if tt.wantErrText != "" && err.Error() != tt.wantErrText {
					t.Errorf("watchWindowMS(%v, %d) error = %q, want %q", tt.extra, def, err.Error(), tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("watchWindowMS(%v, %d) unexpected error: %v", tt.extra, def, err)
			}
			if got != tt.want {
				t.Errorf("watchWindowMS(%v, %d) = %d, want %d", tt.extra, def, got, tt.want)
			}
		})
	}
}

func TestBuildAutoMergeReactionConfig_DefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rc        config.ReactionConfig
		want      AutoMergeReactionConfig
		wantErr   bool
		wantErrIs error
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "watch_window_ms override",
			rc:   config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 600000}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
				WatchWindowMS:   600000,
			},
		},
		{
			name: "watch_window_ms zero disables the bound",
			rc:   config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 0}},
			want: AutoMergeReactionConfig{
				Strategy:        domain.StrategySquash,
				RequireCI:       true,
				DeleteBranch:    true,
				PollIntervalMS:  60000,
				Escalation:      "comment",
				EscalationLabel: "needs-human",
				WatchWindowMS:   0,
			},
		},
		{
			name:    "watch_window_ms negative errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": -1}},
			wantErr: true,
		},
		{
			name:    "watch_window_ms above ceiling errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1}},
			wantErr: true,
		},
		{
			name:    "watch_window_ms non-numeric errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": "soon"}},
			wantErr: true,
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
			name:      "poll_interval_ms out of range as uint64",
			rc:        config.ReactionConfig{Extra: map[string]any{"poll_interval_ms": uint64(9223372036854775808)}},
			wantErr:   true,
			wantErrIs: config.ErrIntegerOutOfRange,
		},
		{
			name:      "poll_interval_ms out of range as float64",
			rc:        config.ReactionConfig{Extra: map[string]any{"poll_interval_ms": float64(1e20)}},
			wantErr:   true,
			wantErrIs: config.ErrIntegerOutOfRange,
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
				if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
					t.Errorf("BuildAutoMergeReactionConfig() error = %v, want it to satisfy errors.Is(err, %v)", err, tt.wantErrIs)
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

func TestNewState_AutoMergePreflightFlagDefaultsFalse(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, 0, nil, AgentTotals{})
	if s.AutoMergePreflightFailed {
		t.Error("NewState().AutoMergePreflightFailed = true, want false")
	}
}

func TestNewState_AutoMergePreflightRetryDueAtDefaultsZero(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, 0, nil, AgentTotals{})
	if !s.AutoMergePreflightRetryDueAt.IsZero() {
		t.Errorf("NewState().AutoMergePreflightRetryDueAt = %v, want zero", s.AutoMergePreflightRetryDueAt)
	}
}

func TestNewState_AutoMergeAuthLoggedInitialized(t *testing.T) {
	t.Parallel()

	s := NewState(5000, 4, 0, nil, AgentTotals{})
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

			state := NewState(5000, 4, 0, nil, AgentTotals{})
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
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
				WatchWindowMS:   reactionWatchWindowDefaultMS,
			},
		},
		{
			name: "watch_window_ms override",
			rc:   config.ReactionConfig{MaxRetries: 1, Extra: map[string]any{"watch_window_ms": 600000}},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      1,
				WatchWindowMS:   600000,
			},
		},
		{
			name: "watch_window_ms zero disables the bound",
			rc:   config.ReactionConfig{MaxRetries: 1, Extra: map[string]any{"watch_window_ms": 0}},
			want: MergeConflictReactionConfig{
				Escalation:      "label",
				EscalationLabel: "needs-human",
				PollIntervalMS:  60000,
				MaxRetries:      1,
				WatchWindowMS:   0,
			},
		},
		{
			name:    "watch_window_ms negative errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": -1}},
			wantErr: true,
		},
		{
			name:    "watch_window_ms above ceiling errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1}},
			wantErr: true,
		},
		{
			name:    "watch_window_ms non-numeric errors",
			rc:      config.ReactionConfig{Extra: map[string]any{"watch_window_ms": "soon"}},
			wantErr: true,
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

// TestBuildMergeConflictReactionConfig_EmptyEscalationDefaultsToLabel checks
// the direct-call safety net, independent of the config-layer default.
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

func TestReactionKindPins(t *testing.T) {
	t.Parallel()

	t.Run("isKnownReactionKind is true for exactly the registered kinds", func(t *testing.T) {
		t.Parallel()

		for kind := range reactionKinds {
			t.Run(kind, func(t *testing.T) {
				t.Parallel()
				if !isKnownReactionKind(kind) {
					t.Errorf("isKnownReactionKind(%q) = false, want true (registered in reactionKinds)", kind)
				}
			})
		}

		if isKnownReactionKind("totally-unregistered-kind") {
			t.Error(`isKnownReactionKind("totally-unregistered-kind") = true, want false (unregistered kind)`)
		}
	})

	t.Run("pullRequestOf reads the pull request of exactly the registered kinds", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			kind   string
			data   any
			number int
		}{
			{ReactionKindCI, &CIReactionData{PRNumber: 1}, 1},
			{ReactionKindReview, &ReviewReactionData{PRNumber: 2}, 2},
			{ReactionKindBotReview, &BotReviewReactionData{PRNumber: 3}, 3},
			{ReactionKindAutoMerge, &AutoMergeReactionData{PRNumber: 4}, 4},
			{ReactionKindMergeConflict, &MergeConflictReactionData{PRNumber: 5}, 5},
			{ReactionKindLabelReview, &LabelReviewReactionData{PRNumber: 6}, 6},
			{ReactionKindLabelFix, &LabelFixReactionData{PRNumber: 7}, 7},
			{ReactionKindMergeCompletion, &MergeCompletionReactionData{PRNumber: 8}, 8},
		}

		covered := make(map[string]bool, len(tests))
		for _, tt := range tests {
			covered[tt.kind] = true
			if _, ok := reactionKinds[tt.kind]; !ok {
				t.Errorf("table kind %q is not registered in reactionKinds", tt.kind)
			}
			if got := pullRequestOf(&PendingReaction{Kind: tt.kind, KindData: tt.data}).Number; got != tt.number {
				t.Errorf("pullRequestOf(%q data).Number = %d, want %d", tt.kind, got, tt.number)
			}
		}
		for kind := range reactionKinds {
			if !covered[kind] {
				t.Errorf("registered kind %q has no pullRequestOf table row", kind)
			}
		}
	})

	t.Run("reactionKindPinsWorkspace returns true for an unregistered kind", func(t *testing.T) {
		t.Parallel()

		if got := reactionKindPinsWorkspace("totally-unregistered-kind"); !got {
			t.Error(`reactionKindPinsWorkspace("totally-unregistered-kind") = false, want true (unregistered kind retains)`)
		}
	})
}

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
			// The escalation check runs before any tracker prerequisite,
			// so the zero-value tracker still reaches it.
			wantErr:     true,
			wantErrText: "invalid escalation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildMergeCompletionReactionConfig(tt.rc, tt.tracker)
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

func TestPopulateParked(t *testing.T) {
	t.Parallel()

	t.Run("loads a row with an empty parked_state", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		rows := []persistence.ParkedIssue{
			{IssueID: "id-2", Identifier: "PROJ-2", Reason: parkReasonHandoffAbsence, Label: "needs-human", ParkedAt: "2026-08-17T01:00:00Z"},
		}

		PopulateParked(state, rows, nil)

		if entry, ok := state.Parked["id-2"]; !ok || entry.ParkedState != "" {
			t.Errorf("state.Parked[id-2] = %+v, want the row loaded with an empty ParkedState", entry)
		}
	})

	t.Run("skips a row with an empty issue_id and logs a warning", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 4, 0, nil, AgentTotals{})
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

		state := NewState(5000, 4, 0, nil, AgentTotals{})
		PopulateParked(state, []persistence.ParkedIssue{
			{IssueID: "id-4", Identifier: "PROJ-4", Reason: parkReasonAgentBlocked, ParkedAt: "2026-08-17T00:00:00Z"},
		}, nil)

		issue := domain.Issue{ID: "id-4", Identifier: "PROJ-4", Title: "T", State: "To Do"}
		if ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
			t.Error("ShouldDispatch dispatched an issue PopulateParked loaded into state.Parked")
		}
	})
}

func TestRuntimeSnapshot_ParkedFields(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	t.Run("empty Parked is a non-nil empty list", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})

		result := RuntimeSnapshot(state, fixedNow)

		if result.ParkedCount != 0 {
			t.Errorf("ParkedCount = %d, want 0", result.ParkedCount)
		}
		if result.Parked == nil || len(result.Parked) != 0 {
			t.Errorf("Parked = %#v, want a non-nil empty list", result.Parked)
		}
	})

	t.Run("non-empty Parked sorted by identifier and counted", func(t *testing.T) {
		t.Parallel()

		state := NewState(5000, 10, 0, nil, AgentTotals{})
		state.Parked["ISS-C"] = &ParkedEntry{Identifier: "PROJ-C", Reason: parkReasonAgentBlocked}
		state.Parked["ISS-A"] = &ParkedEntry{Identifier: "PROJ-A", Reason: parkReasonHandoffAbsence}
		state.Parked["ISS-B"] = &ParkedEntry{Identifier: "PROJ-B", Reason: parkReasonAgentBlocked}

		result := RuntimeSnapshot(state, fixedNow)

		if result.ParkedCount != 3 {
			t.Errorf("ParkedCount = %d, want 3", result.ParkedCount)
		}
		want := []string{"PROJ-A", "PROJ-B", "PROJ-C"}
		if len(result.Parked) != len(want) {
			t.Fatalf("len(Parked) = %d, want %d", len(result.Parked), len(want))
		}
		for i, identifier := range want {
			if result.Parked[i].Identifier != identifier {
				t.Errorf("Parked[%d].Identifier = %q, want %q", i, result.Parked[i].Identifier, identifier)
			}
		}
	})
}

func TestRuntimeSnapshot_UsageDispositionFields(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		arrival          registry.UsageArrival
		attribution      registry.UsageAttribution
		usageMeasured    bool
		lastAgentEvent   domain.AgentEventType
		wantTokenPending bool
	}{
		{
			name:             "turn_end, measured, turn in flight: pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventNotification,
			wantTokenPending: true,
		},
		{
			name:             "turn_end, measured, turn completed: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventTurnCompleted,
			wantTokenPending: false,
		},
		{
			name:             "turn_end, measured, turn failed: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventTurnFailed,
			wantTokenPending: false,
		},
		{
			name:             "turn_end, measured, turn cancelled: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventTurnCancelled,
			wantTokenPending: false,
		},
		{
			name:             "turn_end, measured, turn ended with error: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventTurnEndedWithError,
			wantTokenPending: false,
		},
		{
			name:             "turn_end, measured, turn input required: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventTurnInputRequired,
			wantTokenPending: false,
		},
		{
			name:             "turn_end, not measured, turn in flight: not pending",
			arrival:          registry.UsageArrivalTurnEnd,
			attribution:      registry.UsageAttributionSessionTotal,
			usageMeasured:    false,
			lastAgentEvent:   domain.EventNotification,
			wantTokenPending: false,
		},
		{
			name:             "incremental, measured, turn in flight: not pending",
			arrival:          registry.UsageArrivalIncremental,
			attribution:      registry.UsageAttributionPerModel,
			usageMeasured:    true,
			lastAgentEvent:   domain.EventNotification,
			wantTokenPending: false,
		},
		{
			name:             "none, not measured, turn in flight: not pending",
			arrival:          registry.UsageArrivalNone,
			attribution:      registry.UsageAttributionNone,
			usageMeasured:    false,
			lastAgentEvent:   domain.EventNotification,
			wantTokenPending: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, 0, nil, AgentTotals{})
			state.Running["ISS-USAGE"] = &RunningEntry{
				Identifier:       "PROJ-1",
				Issue:            domain.Issue{ID: "ISS-USAGE", State: "In Progress"},
				StartedAt:        fixedNow.Add(-time.Minute),
				UsageArrival:     tt.arrival,
				UsageAttribution: tt.attribution,
				UsageMeasured:    tt.usageMeasured,
				LastAgentEvent:   tt.lastAgentEvent,
			}

			result := RuntimeSnapshot(state, fixedNow)

			if len(result.Running) != 1 {
				t.Fatalf("len(Running) = %d, want 1", len(result.Running))
			}
			got := result.Running[0]
			if got.UsageArrival != tt.arrival {
				t.Errorf("UsageArrival = %q, want %q", got.UsageArrival, tt.arrival)
			}
			if got.UsageAttribution != tt.attribution {
				t.Errorf("UsageAttribution = %q, want %q", got.UsageAttribution, tt.attribution)
			}
			if got.TokensPending != tt.wantTokenPending {
				t.Errorf("TokensPending = %v, want %v", got.TokensPending, tt.wantTokenPending)
			}
		})
	}
}

func TestRuntimeSnapshot_AgentTotalsExclusionCounts(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	state := NewState(5000, 4, 0, nil, AgentTotals{UnmeasuredSessions: 5})
	state.Running["unreported-inc"] = &RunningEntry{
		Identifier:         "PROJ-1",
		Issue:              domain.Issue{ID: "unreported-inc", State: "In Progress"},
		StartedAt:          fixedNow.Add(-time.Minute),
		UsageArrival:       registry.UsageArrivalIncremental,
		CarriesTokenFigure: true,
	}
	state.Running["unreported-turnend"] = &RunningEntry{
		Identifier:         "PROJ-2",
		Issue:              domain.Issue{ID: "unreported-turnend", State: "In Progress"},
		StartedAt:          fixedNow.Add(-time.Minute),
		UsageArrival:       registry.UsageArrivalTurnEnd,
		CarriesTokenFigure: true,
	}
	state.Running["no-arrival-1"] = &RunningEntry{
		Identifier:   "PROJ-3",
		Issue:        domain.Issue{ID: "no-arrival-1", State: "In Progress"},
		StartedAt:    fixedNow.Add(-time.Minute),
		UsageArrival: registry.UsageArrivalNone,
	}
	state.Running["no-arrival-2"] = &RunningEntry{
		Identifier:   "PROJ-4",
		Issue:        domain.Issue{ID: "no-arrival-2", State: "In Progress"},
		StartedAt:    fixedNow.Add(-time.Minute),
		UsageArrival: registry.UsageArrivalNone,
	}
	state.Running["measured"] = &RunningEntry{
		Identifier:         "PROJ-5",
		Issue:              domain.Issue{ID: "measured", State: "In Progress"},
		StartedAt:          fixedNow.Add(-time.Minute),
		UsageArrival:       registry.UsageArrivalIncremental,
		UsageMeasured:      true,
		CarriesTokenFigure: true,
	}

	result := RuntimeSnapshot(state, fixedNow)

	if result.AgentTotals.RunningUnreported != 2 {
		t.Errorf("AgentTotals.RunningUnreported = %d, want 2", result.AgentTotals.RunningUnreported)
	}
	if result.AgentTotals.RunningNonReporting != 2 {
		t.Errorf("AgentTotals.RunningNonReporting = %d, want 2", result.AgentTotals.RunningNonReporting)
	}
	if result.AgentTotals.UnmeasuredSessions != 5 {
		t.Errorf("AgentTotals.UnmeasuredSessions = %d, want 5 (passthrough of the persisted counter)", result.AgentTotals.UnmeasuredSessions)
	}
}

func TestApiRequestsMeasured(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		arrival         registry.UsageArrival
		turnCount       int
		apiRequestCount int
		want            bool
	}{
		{"incremental, no turn, zero count: measured", registry.UsageArrivalIncremental, 0, 0, true},
		{"incremental, no turn, positive count: measured", registry.UsageArrivalIncremental, 0, 5, true},
		{"incremental, one turn, zero count: unmeasured", registry.UsageArrivalIncremental, 1, 0, false},
		{"incremental, many turns, zero count: unmeasured", registry.UsageArrivalIncremental, 3, 0, false},
		{"incremental, turns began, positive count: measured", registry.UsageArrivalIncremental, 1, 7, true},
		{"turn_end, no turn, zero count: unmeasured", registry.UsageArrivalTurnEnd, 0, 0, false},
		{"turn_end, turns began, positive count: unmeasured", registry.UsageArrivalTurnEnd, 5, 9, false},
		{"none, no turn, zero count: unmeasured", registry.UsageArrivalNone, 0, 0, false},
		{"none, turns began, positive count: unmeasured", registry.UsageArrivalNone, 3, 4, false},
		{"undeclared arrival: unmeasured", registry.UsageArrivalUndeclared, 0, 0, false},
		{"arrival outside the declared set: unmeasured", registry.UsageArrival("bogus"), 0, 9, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := apiRequestsMeasured(tt.arrival, tt.turnCount, tt.apiRequestCount)
			if got != tt.want {
				t.Errorf("apiRequestsMeasured(%q, %d, %d) = %v, want %v",
					tt.arrival, tt.turnCount, tt.apiRequestCount, got, tt.want)
			}
		})
	}
}

// TestApiRequestsMeasuredSingleDerivationSite requires exactly one call to
// ReportsDuringTurn, in state.go: a second call site is evidence the rule
// was reproduced rather than reused. It cannot catch a reimplementation
// that spells the predicate out as a direct comparison, since that
// spelling is live elsewhere for presentation wording. The scanned-file
// count is a negative control, so a walk that reaches nothing fails here
// rather than passing in silence.
func TestApiRequestsMeasuredSingleDerivationSite(t *testing.T) {
	t.Parallel()

	const needle = "ReportsDuringTurn("
	type callSite struct {
		file  string
		count int
	}
	var sites []callSite
	scanned := 0

	for _, root := range []string{
		filepath.Join("..", "..", "cmd"),
		filepath.Join("..", "..", "internal"),
	} {
		err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(walkPath, ".go") || strings.HasSuffix(walkPath, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(walkPath)
			if readErr != nil {
				return readErr
			}
			scanned++
			count := 0
			for line := range strings.SplitSeq(string(data), "\n") {
				// Skip the method's own declaration, not a call site.
				if strings.Contains(line, ") "+needle) {
					continue
				}
				count += strings.Count(line, needle)
			}
			if count > 0 {
				sites = append(sites, callSite{file: filepath.ToSlash(walkPath), count: count})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir(%q): %v", root, err)
		}
	}

	if scanned < 100 {
		t.Fatalf("scanned %d non-test files, want the walk to reach the whole tree", scanned)
	}

	want := filepath.ToSlash(filepath.Join("..", "..", "internal", "orchestrator", "state.go"))
	if len(sites) != 1 || sites[0].file != want || sites[0].count != 1 {
		t.Errorf("ReportsDuringTurn call sites = %+v, want exactly one call, in %s", sites, want)
	}
}

func TestRuntimeSnapshot_TokensAwaited(t *testing.T) {
	t.Parallel()

	fixedNow := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name             string
		arrival          registry.UsageArrival
		turnCount        int
		lastEvent        domain.AgentEventType
		usageMeasured    bool
		carriesNoFigure  bool
		wantAwaited      bool
		wantUnreported   int
		wantNonReporting int
	}{
		{
			name:           "turn_end before any turn begins",
			arrival:        registry.UsageArrivalTurnEnd,
			wantAwaited:    true,
			wantUnreported: 1,
		},
		{
			name:           "turn_end inside its first turn",
			arrival:        registry.UsageArrivalTurnEnd,
			turnCount:      1,
			lastEvent:      domain.EventOtherMessage,
			wantAwaited:    true,
			wantUnreported: 1,
		},
		{
			name:             "turn_end after a turn ended with nothing recorded",
			arrival:          registry.UsageArrivalTurnEnd,
			turnCount:        1,
			lastEvent:        domain.EventTurnCompleted,
			wantNonReporting: 1,
		},
		{
			name:             "turn_end once a second turn has begun",
			arrival:          registry.UsageArrivalTurnEnd,
			turnCount:        2,
			lastEvent:        domain.EventOtherMessage,
			wantNonReporting: 1,
		},
		{
			name:           "incremental before any turn begins",
			arrival:        registry.UsageArrivalIncremental,
			wantAwaited:    true,
			wantUnreported: 1,
		},
		{
			name:             "incremental once a turn has begun with nothing recorded",
			arrival:          registry.UsageArrivalIncremental,
			turnCount:        1,
			lastEvent:        domain.EventOtherMessage,
			wantNonReporting: 1,
		},
		{
			name:          "measured session is neither awaited nor excluded",
			arrival:       registry.UsageArrivalTurnEnd,
			turnCount:     1,
			lastEvent:     domain.EventTurnCompleted,
			usageMeasured: true,
		},
		{
			name:             "turn_end session that carries no token figure is non-reporting",
			arrival:          registry.UsageArrivalTurnEnd,
			carriesNoFigure:  true,
			wantNonReporting: 1,
		},
		{
			name:             "arrival none is non-reporting whatever the turn",
			arrival:          registry.UsageArrivalNone,
			wantNonReporting: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(5000, 4, 0, nil, AgentTotals{})
			state.Running["iss"] = &RunningEntry{
				Identifier:         "PROJ-1",
				Issue:              domain.Issue{ID: "iss", State: "In Progress"},
				StartedAt:          fixedNow.Add(-time.Minute),
				TurnCount:          tt.turnCount,
				LastAgentEvent:     tt.lastEvent,
				UsageArrival:       tt.arrival,
				UsageMeasured:      tt.usageMeasured,
				CarriesTokenFigure: tt.arrival.ReportsAnyFigure() && !tt.carriesNoFigure,
			}

			result := RuntimeSnapshot(state, fixedNow)

			if got := result.Running[0].TokensAwaited; got != tt.wantAwaited {
				t.Errorf("TokensAwaited = %v, want %v", got, tt.wantAwaited)
			}
			if got := result.AgentTotals.RunningUnreported; got != tt.wantUnreported {
				t.Errorf("AgentTotals.RunningUnreported = %d, want %d", got, tt.wantUnreported)
			}
			if got := result.AgentTotals.RunningNonReporting; got != tt.wantNonReporting {
				t.Errorf("AgentTotals.RunningNonReporting = %d, want %d", got, tt.wantNonReporting)
			}
		})
	}
}

func reactionEscalationBuilders() map[string]func(escalation string) (string, error) {
	tracker := config.TrackerConfig{
		HandoffState:   "in-review",
		ActiveStates:   []string{"doing"},
		TerminalStates: []string{"done"},
	}
	rc := func(escalation string) config.ReactionConfig {
		return config.ReactionConfig{Escalation: escalation, Extra: map[string]any{"target_state": "done"}}
	}

	return map[string]func(string) (string, error){
		"review": func(escalation string) (string, error) {
			got, err := BuildReviewReactionConfig(rc(escalation))
			return got.Escalation, err
		},
		"bot review": func(escalation string) (string, error) {
			got, err := BuildBotReviewReactionConfig(rc(escalation))
			return got.Escalation, err
		},
		"auto merge": func(escalation string) (string, error) {
			got, err := BuildAutoMergeReactionConfig(rc(escalation))
			return got.Escalation, err
		},
		"merge conflict": func(escalation string) (string, error) {
			got, err := BuildMergeConflictReactionConfig(rc(escalation))
			return got.Escalation, err
		},
		"merge completion": func(escalation string) (string, error) {
			got, err := BuildMergeCompletionReactionConfig(rc(escalation), tracker)
			return got.Escalation, err
		},
	}
}

func TestReactionConfigBuilders_Escalation(t *testing.T) {
	t.Parallel()

	for name, build := range reactionEscalationBuilders() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, escalation := range []string{"label", "comment", "none"} {
				got, err := build(escalation)
				if err != nil {
					t.Errorf("build(%q) error = %v, want nil", escalation, err)
					continue
				}
				if got != escalation {
					t.Errorf("build(%q).Escalation = %q, want %q", escalation, got, escalation)
				}
			}

			_, err := build("email")
			if err == nil {
				t.Fatal(`build("email") error = nil, want an invalid escalation error`)
			}
			if want := `must be "label", "comment", or "none", got "email"`; !strings.Contains(err.Error(), want) {
				t.Errorf(`build("email") error = %q, want it to contain %q`, err, want)
			}
		})
	}
}

func TestBuildAutoMergeReactionConfig_OmittedEscalationResolvesToLabel(t *testing.T) {
	t.Parallel()

	cfg, err := config.NewServiceConfig(map[string]any{
		"reactions": map[string]any{"auto_merge": map[string]any{"provider": "github"}},
	})
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	got, err := BuildAutoMergeReactionConfig(cfg.Reactions["auto_merge"])
	if err != nil {
		t.Fatalf("BuildAutoMergeReactionConfig: %v", err)
	}
	if got.Escalation != "label" {
		t.Errorf("BuildAutoMergeReactionConfig(omitted escalation).Escalation = %q, want %q", got.Escalation, "label")
	}
}
