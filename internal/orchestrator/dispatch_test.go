package orchestrator

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

func intPtr(v int) *int {
	return &v
}

func issueWithPriority(id string, p *int, createdAt string) domain.Issue {
	return domain.Issue{
		ID:         id,
		Identifier: id,
		Title:      "title-" + id,
		State:      "To Do",
		Priority:   p,
		CreatedAt:  createdAt,
	}
}

func identifiers(issues []domain.Issue) []string {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.Identifier
	}
	return ids
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSortForDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  []domain.Issue
		wantID []string // expected identifiers in sorted order
	}{
		{
			name:   "empty input returns nil",
			input:  nil,
			wantID: nil,
		},
		{
			name:   "single issue",
			input:  []domain.Issue{issueWithPriority("A-1", intPtr(1), "2025-01-01T00:00:00Z")},
			wantID: []string{"A-1"},
		},
		{
			name: "priority ordering ascending",
			input: []domain.Issue{
				issueWithPriority("P3", intPtr(3), "2025-01-01T00:00:00Z"),
				issueWithPriority("P1", intPtr(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("P2", intPtr(2), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"P1", "P2", "P3"},
		},
		{
			name: "nil priority sorts last",
			input: []domain.Issue{
				issueWithPriority("P2", intPtr(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("NIL", nil, "2025-01-01T00:00:00Z"),
				issueWithPriority("P1", intPtr(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"P1", "P2", "NIL"},
		},
		{
			name: "all nil priorities fall through to created_at",
			input: []domain.Issue{
				issueWithPriority("C", nil, "2025-03-01T00:00:00Z"),
				issueWithPriority("A", nil, "2025-01-01T00:00:00Z"),
				issueWithPriority("B", nil, "2025-02-01T00:00:00Z"),
			},
			wantID: []string{"A", "B", "C"},
		},
		{
			name: "same priority created_at tiebreaker oldest first",
			input: []domain.Issue{
				issueWithPriority("NEW", intPtr(2), "2025-12-01T00:00:00Z"),
				issueWithPriority("OLD", intPtr(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("MID", intPtr(2), "2025-06-01T00:00:00Z"),
			},
			wantID: []string{"OLD", "MID", "NEW"},
		},
		{
			name: "empty created_at sorts last",
			input: []domain.Issue{
				issueWithPriority("EMPTY", intPtr(1), ""),
				issueWithPriority("HAS", intPtr(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"HAS", "EMPTY"},
		},
		{
			name: "both empty created_at falls through to identifier",
			input: []domain.Issue{
				issueWithPriority("B-1", intPtr(1), ""),
				issueWithPriority("A-1", intPtr(1), ""),
			},
			wantID: []string{"A-1", "B-1"},
		},
		{
			name: "identifier tiebreaker lexicographic",
			input: []domain.Issue{
				issueWithPriority("C-1", intPtr(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("A-1", intPtr(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("B-1", intPtr(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"A-1", "B-1", "C-1"},
		},
		{
			name: "full three-key composite",
			input: []domain.Issue{
				issueWithPriority("Z-1", intPtr(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("A-1", nil, "2025-01-01T00:00:00Z"),
				issueWithPriority("B-1", intPtr(1), "2025-06-01T00:00:00Z"),
				issueWithPriority("C-1", intPtr(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("D-1", nil, ""),
			},
			// P1 created oldest: C-1, then B-1; P2: Z-1; nil+dated: A-1; nil+empty: D-1
			wantID: []string{"C-1", "B-1", "Z-1", "A-1", "D-1"},
		},
		{
			name: "input slice not modified",
			input: []domain.Issue{
				issueWithPriority("B", intPtr(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("A", intPtr(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"A", "B"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Capture original order for mutation check.
			var origIDs []string
			for _, issue := range tt.input {
				origIDs = append(origIDs, issue.Identifier)
			}

			got := SortForDispatch(tt.input)
			gotIDs := identifiers(got)

			if tt.wantID == nil {
				if got != nil {
					t.Fatalf("SortForDispatch() = %v, want nil", gotIDs)
				}
				return
			}
			if !equalStringSlice(gotIDs, tt.wantID) {
				t.Errorf("SortForDispatch() identifiers = %v, want %v", gotIDs, tt.wantID)
			}

			// Verify input was not modified.
			afterIDs := identifiers(tt.input)
			if !equalStringSlice(afterIDs, origIDs) {
				t.Errorf("input modified: was %v, now %v", origIDs, afterIDs)
			}
		})
	}
}

func TestShouldDispatch(t *testing.T) {
	t.Parallel()

	active := []string{"To Do", "In Progress"}
	terminal := []string{"Done", "Closed"}

	baseIssue := domain.Issue{
		ID:         "1",
		Identifier: "TEST-1",
		Title:      "Test issue",
		State:      "To Do",
	}

	tests := []struct {
		name           string
		issue          domain.Issue
		activeStates   []string
		terminalStates []string
		setupState     func(*State)
		want           bool
	}{
		{
			name:           "missing ID",
			issue:          domain.Issue{ID: "", Identifier: "X-1", Title: "T", State: "To Do"},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "missing identifier",
			issue:          domain.Issue{ID: "1", Identifier: "", Title: "T", State: "To Do"},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "missing title",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "", State: "To Do"},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "missing state",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: ""},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "state not in active states",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "Backlog"},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "state in terminal set even if also in active set",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "Done"},
			activeStates:   []string{"Done"},
			terminalStates: []string{"Done"},
			want:           false,
		},
		{
			name:           "case-insensitive state matching",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "to do"},
			activeStates:   []string{"To Do"},
			terminalStates: []string{"Done"},
			want:           true,
		},
		{
			name:           "already running",
			issue:          baseIssue,
			activeStates:   active,
			terminalStates: terminal,
			setupState: func(s *State) {
				s.Running["1"] = &RunningEntry{Issue: baseIssue}
			},
			want: false,
		},
		{
			name:           "already claimed but not running",
			issue:          baseIssue,
			activeStates:   active,
			terminalStates: terminal,
			setupState: func(s *State) {
				s.Claimed["1"] = struct{}{}
			},
			want: false,
		},
		{
			name: "blocker with empty state blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: ""}},
			},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name: "blocker with active non-terminal state blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: "In Progress"}},
			},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name: "blocker with terminal state allows dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: "Done"}},
			},
			activeStates:   active,
			terminalStates: terminal,
			want:           true,
		},
		{
			name: "multiple blockers one non-terminal blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "Done"},
					{ID: "3", State: "In Progress"},
				},
			},
			activeStates:   active,
			terminalStates: terminal,
			want:           false,
		},
		{
			name: "no blockers allows dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{},
			},
			activeStates:   active,
			terminalStates: terminal,
			want:           true,
		},
		{
			name:           "fully eligible issue",
			issue:          baseIssue,
			activeStates:   active,
			terminalStates: terminal,
			want:           true,
		},
		{
			name:           "multiple active states second state eligible",
			issue:          domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "In Progress"},
			activeStates:   []string{"To Do", "In Progress"},
			terminalStates: []string{"Done"},
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewState(1000, 10, nil, AgentTotals{})
			if tt.setupState != nil {
				tt.setupState(s)
			}

			got := ShouldDispatch(tt.issue, s, tt.activeStates, tt.terminalStates)
			if got != tt.want {
				t.Errorf("ShouldDispatch(%q) = %t, want %t", tt.issue.Identifier, got, tt.want)
			}
		})
	}
}

func TestIsBlockedByNonTerminal(t *testing.T) {
	t.Parallel()

	terminal := []string{"Done", "Closed"}

	tests := []struct {
		name           string
		issue          domain.Issue
		terminalStates []string
		want           bool
	}{
		{
			name:           "no blockers",
			issue:          domain.Issue{ID: "1", BlockedBy: nil},
			terminalStates: terminal,
			want:           false,
		},
		{
			name:           "empty blockers slice",
			issue:          domain.Issue{ID: "1", BlockedBy: []domain.BlockerRef{}},
			terminalStates: terminal,
			want:           false,
		},
		{
			name: "all blockers terminal",
			issue: domain.Issue{
				ID: "1",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "Done"},
					{ID: "3", State: "Closed"},
				},
			},
			terminalStates: terminal,
			want:           false,
		},
		{
			name: "one blocker non-terminal",
			issue: domain.Issue{
				ID: "1",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "Done"},
					{ID: "3", State: "In Progress"},
				},
			},
			terminalStates: terminal,
			want:           true,
		},
		{
			name: "blocker with empty state treated as non-terminal",
			issue: domain.Issue{
				ID: "1",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: ""},
				},
			},
			terminalStates: terminal,
			want:           true,
		},
		{
			name: "case-insensitive terminal matching",
			issue: domain.Issue{
				ID: "1",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "done"},
				},
			},
			terminalStates: []string{"Done"},
			want:           false,
		},
		{
			name: "empty terminal states list blocks all",
			issue: domain.Issue{
				ID: "1",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "Done"},
				},
			},
			terminalStates: nil,
			want:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := IsBlockedByNonTerminal(tt.issue, tt.terminalStates)
			if got != tt.want {
				t.Errorf("IsBlockedByNonTerminal(issue %q) = %t, want %t", tt.issue.ID, got, tt.want)
			}
		})
	}
}

func TestStateSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  []string
		check  string
		wantIn bool
	}{
		{
			name:   "case folding to lowercase",
			input:  []string{"To Do", "IN PROGRESS"},
			check:  "to do",
			wantIn: true,
		},
		{
			name:   "empty input produces empty set",
			input:  []string{},
			check:  "anything",
			wantIn: false,
		},
		{
			name:   "nil input produces empty set",
			input:  nil,
			check:  "anything",
			wantIn: false,
		},
		{
			name:   "exact match required after lowering",
			input:  []string{"Done"},
			check:  "don",
			wantIn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			set := stateSet(tt.input)
			_, got := set[tt.check]
			if got != tt.wantIn {
				t.Errorf("stateSet(%v)[%q] = %t, want %t", tt.input, tt.check, got, tt.wantIn)
			}
		})
	}
}

// --- Test helpers for 6.3 functions ---

func testIssue(id string) domain.Issue {
	return domain.Issue{
		ID:         id,
		Identifier: id,
		Title:      "title-" + id,
		State:      "To Do",
	}
}

func newTestState() *State {
	return NewState(1000, 10, nil, AgentTotals{})
}

// --- Tests for NextAttempt ---

func TestNextAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current *int
		want    int
	}{
		{name: "nil returns 1", current: nil, want: 1},
		{name: "pointer to 0 returns 1", current: intPtr(0), want: 1},
		{name: "pointer to 1 returns 2", current: intPtr(1), want: 2},
		{name: "pointer to 5 returns 6", current: intPtr(5), want: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := NextAttempt(tt.current)
			if got != tt.want {
				t.Errorf("NextAttempt(%v) = %d, want %d", tt.current, got, tt.want)
			}
		})
	}
}

// --- Tests for CancelRetry ---

func TestCancelRetry(t *testing.T) {
	t.Parallel()

	t.Run("no-op when entry does not exist", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		CancelRetry(s, "nonexistent")

		if len(s.RetryAttempts) != 0 {
			t.Errorf("RetryAttempts length = %d, want 0", len(s.RetryAttempts))
		}
	})

	t.Run("stops timer and removes entry", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		timer := time.AfterFunc(time.Hour, func() {})
		s.RetryAttempts["ISS-1"] = &RetryEntry{
			IssueID:     "ISS-1",
			Identifier:  "ISS-1",
			Attempt:     1,
			DueAtMS:     time.Now().UnixMilli() + 3600000,
			Error:       "some error",
			TimerHandle: timer,
		}

		CancelRetry(s, "ISS-1")

		if _, exists := s.RetryAttempts["ISS-1"]; exists {
			t.Error("CancelRetry(ISS-1) did not remove entry from RetryAttempts")
		}
		// Timer.Stop returns false if already stopped; confirm it was stopped.
		if timer.Stop() {
			t.Error("CancelRetry(ISS-1) did not stop the timer")
		}
	})

	t.Run("nil timer handle does not panic", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		s.RetryAttempts["ISS-2"] = &RetryEntry{
			IssueID:     "ISS-2",
			Identifier:  "ISS-2",
			Attempt:     1,
			DueAtMS:     time.Now().UnixMilli(),
			TimerHandle: nil,
		}

		CancelRetry(s, "ISS-2")

		if _, exists := s.RetryAttempts["ISS-2"]; exists {
			t.Error("CancelRetry(ISS-2) did not remove entry with nil TimerHandle")
		}
	})

	t.Run("does not modify claimed set", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		s.Claimed["ISS-3"] = struct{}{}
		s.RetryAttempts["ISS-3"] = &RetryEntry{
			IssueID:     "ISS-3",
			TimerHandle: nil,
		}

		CancelRetry(s, "ISS-3")

		if _, claimed := s.Claimed["ISS-3"]; !claimed {
			t.Error("CancelRetry(ISS-3) removed entry from Claimed, want preserved")
		}
	})
}

// --- Tests for ScheduleRetry ---

func TestScheduleRetry(t *testing.T) {
	t.Parallel()

	t.Run("fresh schedule creates correct entry", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		before := time.Now().UnixMilli()

		ScheduleRetry(s, ScheduleRetryParams{
			IssueID:    "ISS-1",
			Identifier: "ISS-1",
			Attempt:    2,
			DelayMS:    5000,
			Error:      "some error",
		}, func(_ string) {})

		after := time.Now().UnixMilli()

		entry, exists := s.RetryAttempts["ISS-1"]
		if !exists {
			t.Fatal("ScheduleRetry() did not create RetryAttempts entry")
		}
		if entry.IssueID != "ISS-1" {
			t.Errorf("RetryEntry.IssueID = %q, want %q", entry.IssueID, "ISS-1")
		}
		if entry.Identifier != "ISS-1" {
			t.Errorf("RetryEntry.Identifier = %q, want %q", entry.Identifier, "ISS-1")
		}
		if entry.Attempt != 2 {
			t.Errorf("RetryEntry.Attempt = %d, want %d", entry.Attempt, 2)
		}
		if entry.Error != "some error" {
			t.Errorf("RetryEntry.Error = %q, want %q", entry.Error, "some error")
		}
		if entry.TimerHandle == nil {
			t.Error("RetryEntry.TimerHandle = nil, want non-nil")
		}
		// DueAtMS should be between before+5000 and after+5000.
		wantMin := before + 5000
		wantMax := after + 5000
		if entry.DueAtMS < wantMin || entry.DueAtMS > wantMax {
			t.Errorf("RetryEntry.DueAtMS = %d, want between %d and %d", entry.DueAtMS, wantMin, wantMax)
		}
		// Clean up timer.
		entry.TimerHandle.Stop()
	})

	t.Run("replaces existing retry and stops old timer", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		oldTimer := time.AfterFunc(time.Hour, func() {})
		s.RetryAttempts["ISS-1"] = &RetryEntry{
			IssueID:     "ISS-1",
			Attempt:     1,
			TimerHandle: oldTimer,
		}

		ScheduleRetry(s, ScheduleRetryParams{
			IssueID:    "ISS-1",
			Identifier: "ISS-1",
			Attempt:    2,
			DelayMS:    1000,
			Error:      "retry again",
		}, func(_ string) {})

		entry := s.RetryAttempts["ISS-1"]
		if entry.Attempt != 2 {
			t.Errorf("RetryEntry.Attempt = %d, want %d", entry.Attempt, 2)
		}
		// Old timer should have been stopped.
		if oldTimer.Stop() {
			t.Error("old timer was not stopped by ScheduleRetry")
		}
		// Clean up new timer.
		entry.TimerHandle.Stop()
	})

	t.Run("timer fires callback with correct issueID", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		var mu sync.Mutex
		var fired string

		ScheduleRetry(s, ScheduleRetryParams{
			IssueID:    "ISS-FIRE",
			Identifier: "ISS-FIRE",
			Attempt:    1,
			DelayMS:    1, // fires quickly
		}, func(issueID string) {
			mu.Lock()
			fired = issueID
			mu.Unlock()
		})

		// Wait for timer to fire.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		got := fired
		mu.Unlock()

		if got != "ISS-FIRE" {
			t.Errorf("onFire callback received issueID = %q, want %q", got, "ISS-FIRE")
		}
	})

	t.Run("zero delay fires nearly immediately", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		ch := make(chan string, 1)

		ScheduleRetry(s, ScheduleRetryParams{
			IssueID:    "ISS-ZERO",
			Identifier: "ISS-ZERO",
			Attempt:    1,
			DelayMS:    0,
		}, func(issueID string) {
			ch <- issueID
		})

		select {
		case got := <-ch:
			if got != "ISS-ZERO" {
				t.Errorf("onFire callback received issueID = %q, want %q", got, "ISS-ZERO")
			}
		case <-time.After(time.Second):
			t.Error("zero-delay timer did not fire within 1 second")
		}
	})
}

// --- Tests for DispatchIssue ---

func TestDispatchIssue(t *testing.T) {
	t.Parallel()

	t.Run("first dispatch claims issue and creates running entry", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		issue := testIssue("ISS-1")
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, issue, nil, "", func(_ context.Context, _ domain.Issue, _ *int) {
			close(workerDone)
		})

		// Wait for worker to execute.
		select {
		case <-workerDone:
		case <-time.After(time.Second):
			t.Fatal("worker goroutine did not execute within 1 second")
		}

		// Issue must be claimed.
		if _, claimed := s.Claimed[issue.ID]; !claimed {
			t.Error("DispatchIssue() did not add issue to Claimed set")
		}

		// Running entry must exist.
		entry, exists := s.Running[issue.ID]
		if !exists {
			t.Fatal("DispatchIssue() did not create Running entry")
		}

		// Running count.
		if got := len(s.Running); got != 1 {
			t.Errorf("len(Running) = %d, want 1", got)
		}

		// Verify initial fields.
		if entry.Identifier != issue.Identifier {
			t.Errorf("RunningEntry.Identifier = %q, want %q", entry.Identifier, issue.Identifier)
		}
		if entry.Issue.ID != issue.ID {
			t.Errorf("RunningEntry.Issue.ID = %q, want %q", entry.Issue.ID, issue.ID)
		}
		if entry.SessionID != "" {
			t.Errorf("RunningEntry.SessionID = %q, want empty", entry.SessionID)
		}
		if entry.ThreadID != "" {
			t.Errorf("RunningEntry.ThreadID = %q, want empty", entry.ThreadID)
		}
		if entry.TurnID != "" {
			t.Errorf("RunningEntry.TurnID = %q, want empty", entry.TurnID)
		}
		if entry.AgentPID != "" {
			t.Errorf("RunningEntry.AgentPID = %q, want empty", entry.AgentPID)
		}
		if entry.LastAgentEvent != "" {
			t.Errorf("RunningEntry.LastAgentEvent = %q, want empty", entry.LastAgentEvent)
		}
		if !entry.LastAgentTimestamp.IsZero() {
			t.Errorf("RunningEntry.LastAgentTimestamp = %v, want zero", entry.LastAgentTimestamp)
		}
		if entry.LastAgentMessage != "" {
			t.Errorf("RunningEntry.LastAgentMessage = %q, want empty", entry.LastAgentMessage)
		}
		if entry.AgentInputTokens != 0 {
			t.Errorf("RunningEntry.AgentInputTokens = %d, want 0", entry.AgentInputTokens)
		}
		if entry.AgentOutputTokens != 0 {
			t.Errorf("RunningEntry.AgentOutputTokens = %d, want 0", entry.AgentOutputTokens)
		}
		if entry.AgentTotalTokens != 0 {
			t.Errorf("RunningEntry.AgentTotalTokens = %d, want 0", entry.AgentTotalTokens)
		}
		if entry.LastReportedInputTokens != 0 {
			t.Errorf("RunningEntry.LastReportedInputTokens = %d, want 0", entry.LastReportedInputTokens)
		}
		if entry.LastReportedOutputTokens != 0 {
			t.Errorf("RunningEntry.LastReportedOutputTokens = %d, want 0", entry.LastReportedOutputTokens)
		}
		if entry.LastReportedTotalTokens != 0 {
			t.Errorf("RunningEntry.LastReportedTotalTokens = %d, want 0", entry.LastReportedTotalTokens)
		}
		if entry.TurnCount != 0 {
			t.Errorf("RunningEntry.TurnCount = %d, want 0", entry.TurnCount)
		}

		// RetryAttempt nil for first dispatch.
		if entry.RetryAttempt != nil {
			t.Errorf("RunningEntry.RetryAttempt = %v, want nil", entry.RetryAttempt)
		}
	})

	t.Run("StartedAt is recent UTC", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		before := time.Now().UTC()
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, testIssue("ISS-T"), nil, "", func(_ context.Context, _ domain.Issue, _ *int) {
			close(workerDone)
		})
		<-workerDone

		after := time.Now().UTC()
		entry := s.Running["ISS-T"]

		if entry.StartedAt.Before(before) || entry.StartedAt.After(after) {
			t.Errorf("RunningEntry.StartedAt = %v, want between %v and %v", entry.StartedAt, before, after)
		}
	})

	t.Run("retry dispatch sets RetryAttempt", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		attempt := 3
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, testIssue("ISS-R"), &attempt, "", func(_ context.Context, _ domain.Issue, _ *int) {
			close(workerDone)
		})
		<-workerDone

		entry := s.Running["ISS-R"]
		if entry.RetryAttempt == nil {
			t.Fatal("RunningEntry.RetryAttempt = nil, want non-nil")
		}
		if *entry.RetryAttempt != 3 {
			t.Errorf("RunningEntry.RetryAttempt = %d, want 3", *entry.RetryAttempt)
		}
	})

	t.Run("CancelFunc is non-nil", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, testIssue("ISS-C"), nil, "", func(_ context.Context, _ domain.Issue, _ *int) {
			close(workerDone)
		})
		<-workerDone

		entry := s.Running["ISS-C"]
		if entry.CancelFunc == nil {
			t.Error("RunningEntry.CancelFunc = nil, want non-nil")
		}
	})

	t.Run("worker receives valid context issue and attempt", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		issue := testIssue("ISS-W")
		attempt := 2

		type workerArgs struct {
			ctx     context.Context
			issue   domain.Issue
			attempt *int
		}
		ch := make(chan workerArgs, 1)

		DispatchIssue(context.Background(), s, issue, &attempt, "", func(ctx context.Context, iss domain.Issue, att *int) {
			ch <- workerArgs{ctx: ctx, issue: iss, attempt: att}
		})

		select {
		case args := <-ch:
			if args.ctx == nil {
				t.Error("worker received nil context")
			}
			if args.issue.ID != issue.ID {
				t.Errorf("worker received issue.ID = %q, want %q", args.issue.ID, issue.ID)
			}
			if args.attempt == nil || *args.attempt != 2 {
				t.Errorf("worker received attempt = %v, want pointer to 2", args.attempt)
			}
		case <-time.After(time.Second):
			t.Fatal("worker goroutine did not execute within 1 second")
		}
	})

	t.Run("worker context is cancellable via stored CancelFunc", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		ch := make(chan context.Context, 1)

		DispatchIssue(context.Background(), s, testIssue("ISS-CTX"), nil, "", func(ctx context.Context, _ domain.Issue, _ *int) {
			ch <- ctx
		})

		var workerCtx context.Context
		select {
		case workerCtx = <-ch:
		case <-time.After(time.Second):
			t.Fatal("worker did not execute within 1 second")
		}

		// Cancel via the stored CancelFunc.
		entry := s.Running["ISS-CTX"]
		entry.CancelFunc()

		select {
		case <-workerCtx.Done():
			// expected
		case <-time.After(time.Second):
			t.Error("worker context was not cancelled by CancelFunc")
		}
	})

	t.Run("existing retry entry is cleared on dispatch", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		timer := time.AfterFunc(time.Hour, func() {})
		s.RetryAttempts["ISS-X"] = &RetryEntry{
			IssueID:     "ISS-X",
			Identifier:  "ISS-X",
			Attempt:     1,
			TimerHandle: timer,
		}
		s.Claimed["ISS-X"] = struct{}{}
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, testIssue("ISS-X"), intPtr(2), "", func(_ context.Context, _ domain.Issue, _ *int) {
			close(workerDone)
		})
		<-workerDone

		if _, exists := s.RetryAttempts["ISS-X"]; exists {
			t.Error("DispatchIssue() did not clear existing retry entry")
		}
		// Timer should have been stopped.
		if timer.Stop() {
			t.Error("DispatchIssue() did not stop existing retry timer")
		}
	})

	t.Run("nil workerFn panics", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("DispatchIssue(nil workerFn) did not panic")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("panic value type = %T, want string", r)
			}
			if msg != "DispatchIssue: nil WorkerFunc" {
				t.Errorf("panic message = %q, want %q", msg, "DispatchIssue: nil WorkerFunc")
			}
		}()

		DispatchIssue(context.Background(), s, testIssue("ISS-P"), nil, "", nil)
	})
}
