package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

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
			input:  []domain.Issue{issueWithPriority("A-1", new(1), "2025-01-01T00:00:00Z")},
			wantID: []string{"A-1"},
		},
		{
			name: "priority ordering ascending",
			input: []domain.Issue{
				issueWithPriority("P3", new(3), "2025-01-01T00:00:00Z"),
				issueWithPriority("P1", new(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("P2", new(2), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"P1", "P2", "P3"},
		},
		{
			name: "nil priority sorts last",
			input: []domain.Issue{
				issueWithPriority("P2", new(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("NIL", nil, "2025-01-01T00:00:00Z"),
				issueWithPriority("P1", new(1), "2025-01-01T00:00:00Z"),
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
				issueWithPriority("NEW", new(2), "2025-12-01T00:00:00Z"),
				issueWithPriority("OLD", new(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("MID", new(2), "2025-06-01T00:00:00Z"),
			},
			wantID: []string{"OLD", "MID", "NEW"},
		},
		{
			name: "empty created_at sorts last",
			input: []domain.Issue{
				issueWithPriority("EMPTY", new(1), ""),
				issueWithPriority("HAS", new(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"HAS", "EMPTY"},
		},
		{
			name: "both empty created_at falls through to identifier",
			input: []domain.Issue{
				issueWithPriority("B-1", new(1), ""),
				issueWithPriority("A-1", new(1), ""),
			},
			wantID: []string{"A-1", "B-1"},
		},
		{
			name: "identifier tiebreaker lexicographic",
			input: []domain.Issue{
				issueWithPriority("C-1", new(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("A-1", new(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("B-1", new(1), "2025-01-01T00:00:00Z"),
			},
			wantID: []string{"A-1", "B-1", "C-1"},
		},
		{
			name: "full three-key composite",
			input: []domain.Issue{
				issueWithPriority("Z-1", new(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("A-1", nil, "2025-01-01T00:00:00Z"),
				issueWithPriority("B-1", new(1), "2025-06-01T00:00:00Z"),
				issueWithPriority("C-1", new(1), "2025-01-01T00:00:00Z"),
				issueWithPriority("D-1", nil, ""),
			},
			// P1 created oldest: C-1, then B-1; P2: Z-1; nil+dated: A-1; nil+empty: D-1
			wantID: []string{"C-1", "B-1", "Z-1", "A-1", "D-1"},
		},
		{
			name: "input slice not modified",
			input: []domain.Issue{
				issueWithPriority("B", new(2), "2025-01-01T00:00:00Z"),
				issueWithPriority("A", new(1), "2025-01-01T00:00:00Z"),
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

// TestShouldDispatchParkedIssue verifies that both ShouldDispatch and
// ShouldDispatchWithSets return false for a parked issue that satisfies
// every other gate, with state.BudgetExhausted empty — matching a
// deployment where both agent.max_sessions and agent.max_tokens are zero,
// so the park gate alone, not a budget gate, accounts for the refusal.
func TestShouldDispatchParkedIssue(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "PARK-GATE", Identifier: "PARK-GATE", Title: "T", State: "To Do"}
	active := []string{"To Do"}
	terminal := []string{"Done"}

	newParkedState := func() *State {
		s := NewState(1000, 10, nil, AgentTotals{})
		s.Parked[issue.ID] = &ParkedEntry{Identifier: issue.Identifier, Reason: parkReasonAgentBlocked}
		return s
	}

	t.Run("ShouldDispatch", func(t *testing.T) {
		t.Parallel()

		s := newParkedState()
		if len(s.BudgetExhausted) != 0 {
			t.Fatalf("BudgetExhausted = %v, want empty", s.BudgetExhausted)
		}
		if got := ShouldDispatch(issue, s, active, terminal); got {
			t.Errorf("ShouldDispatch(%q) = %t, want false for a parked issue", issue.Identifier, got)
		}
	})

	t.Run("ShouldDispatchWithSets", func(t *testing.T) {
		t.Parallel()

		s := newParkedState()
		activeSet := stateSet(active)
		terminalSet := stateSet(terminal)
		if len(s.BudgetExhausted) != 0 {
			t.Fatalf("BudgetExhausted = %v, want empty", s.BudgetExhausted)
		}
		if got := ShouldDispatchWithSets(issue, s, activeSet, terminalSet); got {
			t.Errorf("ShouldDispatchWithSets(%q) = %t, want false for a parked issue", issue.Identifier, got)
		}
	})
}

// TestShouldDispatch_ReopenAfterTerminalRelease verifies that releasing an
// issue's claim through [releaseTerminalIssueState] makes it dispatchable
// again once the tracker reports it back in an active state, and that the
// same evaluation returns false beforehand while the claim is still held.
func TestShouldDispatch_ReopenAfterTerminalRelease(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	issue := domain.Issue{ID: "REOPEN-1", Identifier: "REOPEN-1", Title: "T", State: "To Do"}

	state := NewState(1000, 10, nil, AgentTotals{})
	state.Claimed[issue.ID] = struct{}{}

	if ShouldDispatch(issue, state, active, terminal) {
		t.Fatal("ShouldDispatch = true while the claim is still held before release; want false")
	}

	store := &mockReconcileStore{}
	releaseTerminalIssueState(context.Background(), state, store, issue.ID, discardLogger())

	if len(state.Running) != 0 || len(state.RetryAttempts) != 0 || len(state.BudgetExhausted) != 0 {
		t.Fatalf("state not empty after release: running=%d retry=%d budget=%d",
			len(state.Running), len(state.RetryAttempts), len(state.BudgetExhausted))
	}

	if !ShouldDispatch(issue, state, active, terminal) {
		t.Error("ShouldDispatch = false after terminal release and reopen into an active state; want true")
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
		{
			name: "unresolved blockers block dispatch even with no listed blockers",
			issue: domain.Issue{
				ID:                 "1",
				BlockedBy:          []domain.BlockerRef{},
				BlockersUnresolved: true,
			},
			terminalStates: terminal,
			want:           true,
		},
		{
			name: "resolved empty blocker list does not block dispatch",
			issue: domain.Issue{
				ID:                 "1",
				BlockedBy:          []domain.BlockerRef{},
				BlockersUnresolved: false,
			},
			terminalStates: terminal,
			want:           false,
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
		{name: "pointer to 0 returns 1", current: new(0), want: 1},
		{name: "pointer to 1 returns 2", current: new(1), want: 2},
		{name: "pointer to 5 returns 6", current: new(5), want: 6},
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
			IssueID:      "ISS-1",
			Attempt:      1,
			ReactionKind: ReactionKindCI,
			TimerHandle:  oldTimer,
		}

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		ScheduleRetry(s, ScheduleRetryParams{
			IssueID:      "ISS-1",
			Identifier:   "ISS-1",
			Attempt:      2,
			DelayMS:      1000,
			Error:        "retry again",
			ReactionKind: ReactionKindReview,
			Logger:       log,
		}, func(_ string) {})

		entry := s.RetryAttempts["ISS-1"]
		if entry.Attempt != 2 {
			t.Errorf("RetryEntry.Attempt = %d, want %d", entry.Attempt, 2)
		}
		// Old timer should have been stopped.
		if oldTimer.Stop() {
			t.Error("old timer was not stopped by ScheduleRetry")
		}

		output := buf.String()
		if !strings.Contains(output, "level=WARN") {
			t.Errorf("log output = %q, want a Warn-level record", output)
		}
		if !strings.Contains(output, "retry slot displaced") {
			t.Errorf("log output = %q, want message %q", output, "retry slot displaced")
		}
		if !strings.Contains(output, "challenger_kind=review") {
			t.Errorf("log output = %q, want challenger_kind=review", output)
		}
		if !strings.Contains(output, "incumbent_kind=ci") {
			t.Errorf("log output = %q, want incumbent_kind=ci", output)
		}
		if !strings.Contains(output, "incumbent_attempt=1") {
			t.Errorf("log output = %q, want incumbent_attempt=1", output)
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

// --- Tests for retrySlotIncumbent ---

func TestRetrySlotIncumbent(t *testing.T) {
	t.Parallel()

	t.Run("nil on free slot", func(t *testing.T) {
		t.Parallel()

		s := newTestState()

		got := retrySlotIncumbent(s, "ISS-FREE")

		if got != nil {
			t.Errorf("retrySlotIncumbent(free slot) = %v, want nil", got)
		}
	})

	t.Run("returns the stored entry on an occupied slot", func(t *testing.T) {
		t.Parallel()

		s := newTestState()
		entry := &RetryEntry{IssueID: "ISS-1", Attempt: 3, ReactionKind: ReactionKindCI}
		s.RetryAttempts["ISS-1"] = entry

		got := retrySlotIncumbent(s, "ISS-1")

		if got != entry {
			t.Errorf("retrySlotIncumbent(occupied slot) = %v, want %v (the stored entry)", got, entry)
		}
	})
}

// --- Tests for logRetrySlotDeferral ---

func TestLogRetrySlotDeferral(t *testing.T) {
	t.Parallel()

	t.Run("emits one Debug record with the expected attributes", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		incumbent := &RetryEntry{ReactionKind: ReactionKindCI, Attempt: 4, DueAtMS: 123456}

		logRetrySlotDeferral(log, "review", incumbent)

		output := buf.String()
		if !strings.Contains(output, "level=DEBUG") {
			t.Errorf("log output = %q, want a Debug-level record", output)
		}
		if !strings.Contains(output, "retry slot occupied, deferring") {
			t.Errorf("log output = %q, want message %q", output, "retry slot occupied, deferring")
		}
		if !strings.Contains(output, "challenger_kind=review") {
			t.Errorf("log output = %q, want challenger_kind=review", output)
		}
		if !strings.Contains(output, "incumbent_kind=ci") {
			t.Errorf("log output = %q, want incumbent_kind=ci", output)
		}
		if !strings.Contains(output, "incumbent_attempt=4") {
			t.Errorf("log output = %q, want incumbent_attempt=4", output)
		}
		if !strings.Contains(output, "incumbent_due_at_ms=123456") {
			t.Errorf("log output = %q, want incumbent_due_at_ms=123456", output)
		}
	})

	t.Run("nil logger falls back to slog.Default without panicking", func(t *testing.T) {
		t.Parallel()

		incumbent := &RetryEntry{ReactionKind: ReactionKindCI, Attempt: 1}

		logRetrySlotDeferral(nil, "review", incumbent)
	})

	t.Run("empty incumbent reaction kind reports the literal continuation", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		incumbent := &RetryEntry{ReactionKind: "", Attempt: 1}

		logRetrySlotDeferral(log, "ci", incumbent)

		output := buf.String()
		if !strings.Contains(output, "incumbent_kind=continuation") {
			t.Errorf("log output = %q, want incumbent_kind=continuation", output)
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
		workerDone := make(chan struct{})

		DispatchIssue(context.Background(), s, testIssue("ISS-R"), new(3), "", func(_ context.Context, _ domain.Issue, _ *int) {
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

		type workerArgs struct {
			ctx     context.Context
			issue   domain.Issue
			attempt *int
		}
		ch := make(chan workerArgs, 1)

		DispatchIssue(context.Background(), s, issue, new(2), "", func(ctx context.Context, iss domain.Issue, att *int) {
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

		DispatchIssue(context.Background(), s, testIssue("ISS-X"), new(2), "", func(_ context.Context, _ domain.Issue, _ *int) {
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

// --- EvaluateCandidate ---

// fakeBlockerResolver is a test double for BlockerResolver. Both
// functions default to a no-op when nil: NeedsRead reports false and
// Resolve returns the issue unchanged. Every Resolve call is recorded
// in callOrder, in the order EvaluateCandidate makes them.
type fakeBlockerResolver struct {
	needsReadFn func(domain.Issue) bool
	resolveFn   func(context.Context, domain.Issue) (domain.Issue, error)
	callOrder   []string
}

func (f *fakeBlockerResolver) NeedsRead(issue domain.Issue) bool {
	if f.needsReadFn == nil {
		return false
	}
	return f.needsReadFn(issue)
}

func (f *fakeBlockerResolver) Resolve(ctx context.Context, issue domain.Issue) (domain.Issue, error) {
	f.callOrder = append(f.callOrder, issue.ID)
	if f.resolveFn == nil {
		return issue, nil
	}
	return f.resolveFn(ctx, issue)
}

func TestEvaluateCandidate(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	readErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "boom"}

	tests := []struct {
		name            string
		issue           domain.Issue
		resolver        *fakeBlockerResolver
		wantDispatch    bool
		wantReason      SkipReason
		wantErr         error
		wantResolverLen int
	}{
		{
			name:            "ineligible candidate makes zero resolver calls",
			issue:           domain.Issue{ID: "", Identifier: "X-1", Title: "T", State: "To Do"},
			resolver:        &fakeBlockerResolver{needsReadFn: func(domain.Issue) bool { return true }},
			wantDispatch:    false,
			wantReason:      SkipIneligible,
			wantResolverLen: 0,
		},
		{
			name:         "eligible with no blockers dispatches",
			issue:        domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "To Do"},
			wantDispatch: true,
			wantReason:   SkipNone,
		},
		{
			name: "eligible with only a terminal blocker dispatches",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "b", State: "Done"}},
			},
			wantDispatch: true,
			wantReason:   SkipNone,
		},
		{
			name: "eligible with a non-terminal blocker holds as blocked_by",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "b", State: "To Do"}},
			},
			wantDispatch: false,
			wantReason:   SkipBlockedBy,
		},
		{
			name: "resolver read failure holds as blockers_unresolved and carries the error",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true,
			},
			resolver: &fakeBlockerResolver{
				needsReadFn: func(domain.Issue) bool { return true },
				resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
					issue.BlockersUnresolved = true
					return issue, readErr
				},
			},
			wantDispatch:    false,
			wantReason:      SkipBlockersUnresolved,
			wantErr:         readErr,
			wantResolverLen: 1,
		},
		{
			name: "producer-declared incomplete list with no read attempted",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true,
			},
			resolver:        &fakeBlockerResolver{needsReadFn: func(domain.Issue) bool { return false }},
			wantDispatch:    false,
			wantReason:      SkipBlockersIncomplete,
			wantResolverLen: 0,
		},
		{
			name: "a resolver error holds the candidate even when it leaves the flag false",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true,
			},
			resolver: &fakeBlockerResolver{
				needsReadFn: func(domain.Issue) bool { return true },
				resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
					issue.BlockersUnresolved = false
					return issue, readErr
				},
			},
			wantDispatch:    false,
			wantReason:      SkipBlockersUnresolved,
			wantErr:         readErr,
			wantResolverLen: 1,
		},
		{
			name: "nil resolver reproduces today's ShouldDispatchWithSets behavior when blocked",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "b", State: "To Do"}},
			},
			resolver:     nil,
			wantDispatch: false,
			wantReason:   SkipBlockedBy,
		},
		{
			name:         "nil resolver reproduces today's ShouldDispatchWithSets behavior when eligible",
			issue:        domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "To Do"},
			resolver:     nil,
			wantDispatch: true,
			wantReason:   SkipNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewState(1000, 10, nil, AgentTotals{})
			pass := &TickResolution{}

			var resolver BlockerResolver
			if tt.resolver != nil {
				resolver = tt.resolver
			}

			got := EvaluateCandidate(context.Background(), tt.issue, s, activeSet, terminalSet, resolver, pass)

			if got.Dispatch != tt.wantDispatch {
				t.Errorf("EvaluateCandidate(%q).Dispatch = %t, want %t", tt.issue.Identifier, got.Dispatch, tt.wantDispatch)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("EvaluateCandidate(%q).Reason = %q, want %q", tt.issue.Identifier, got.Reason, tt.wantReason)
			}
			if tt.wantErr != nil {
				if !errors.Is(got.Err, tt.wantErr) {
					t.Errorf("EvaluateCandidate(%q).Err = %v, want %v", tt.issue.Identifier, got.Err, tt.wantErr)
				}
			} else if got.Err != nil {
				t.Errorf("EvaluateCandidate(%q).Err = %v, want nil", tt.issue.Identifier, got.Err)
			}
			if tt.resolver != nil && len(tt.resolver.callOrder) != tt.wantResolverLen {
				t.Errorf("resolver calls = %d, want %d", len(tt.resolver.callOrder), tt.wantResolverLen)
			}
		})
	}
}

// TestEvaluateCandidate_ParityWithShouldDispatchWithSets pins that for
// an issue whose blockers are already resolved, EvaluateCandidate's
// dispatch decision agrees with the independent ShouldDispatchWithSets
// oracle. Both predicates are exercised over the same table so a
// divergence in either fails the test.
func TestEvaluateCandidate_ParityWithShouldDispatchWithSets(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	issues := []domain.Issue{
		{ID: "1", Identifier: "X-1", Title: "T", State: "To Do"},
		{ID: "2", Identifier: "X-2", Title: "T", State: "Backlog"},
		{
			ID: "3", Identifier: "X-3", Title: "T", State: "To Do",
			BlockedBy: []domain.BlockerRef{{ID: "b", State: "To Do"}},
		},
		{
			ID: "4", Identifier: "X-4", Title: "T", State: "To Do",
			BlockedBy: []domain.BlockerRef{{ID: "b", State: "Done"}},
		},
		{ID: "", Identifier: "X-5", Title: "T", State: "To Do"},
	}

	for _, issue := range issues {
		t.Run(issue.Identifier, func(t *testing.T) {
			t.Parallel()

			s := NewState(1000, 10, nil, AgentTotals{})
			pass := &TickResolution{}

			decision := EvaluateCandidate(context.Background(), issue, s, activeSet, terminalSet, nil, pass)
			want := ShouldDispatchWithSets(issue, s, activeSet, terminalSet)

			if decision.Dispatch != want {
				t.Errorf("EvaluateCandidate(%q).Dispatch = %t, ShouldDispatchWithSets(%q) = %t, want equal",
					issue.Identifier, decision.Dispatch, issue.Identifier, want)
			}
		})
	}
}

func TestClassifyBlockerFailureClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		err            error
		wantDeployment bool
	}{
		{
			name:           "ErrNoBlockerReader is deployment class",
			err:            domain.ErrNoBlockerReader,
			wantDeployment: true,
		},
		{
			name:           "auth error is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAuth},
			wantDeployment: true,
		},
		{
			name:           "not found error is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Status: 404},
			wantDeployment: true,
		},
		{
			name:           "payload error is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerPayload},
			wantDeployment: true,
		},
		{
			name:           "api error with status 403 is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 403},
			wantDeployment: true,
		},
		{
			name:           "api error with status 429 is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 429},
			wantDeployment: true,
		},
		{
			name:           "api error with status 423 is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 423},
			wantDeployment: true,
		},
		{
			name:           "api error with status 405 is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 405},
			wantDeployment: true,
		},
		{
			name:           "api error with status 410 is deployment class",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 410},
			wantDeployment: true,
		},
		{
			name:           "api error with status 500 stays transient",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerAPI, Status: 500},
			wantDeployment: false,
		},
		{
			name:           "transport error stays transient",
			err:            &domain.TrackerError{Kind: domain.ErrTrackerTransport},
			wantDeployment: false,
		},
		{
			name:           "cancelled context stays transient",
			err:            context.Canceled,
			wantDeployment: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyBlockerFailureClass(tt.err)
			if got != tt.wantDeployment {
				t.Errorf("classifyBlockerFailureClass(%v) = %t, want %t", tt.err, got, tt.wantDeployment)
			}
		})
	}
}

// TestEvaluateCandidate_HaltLatch pins that once a deployment-class
// failure halts a pass, every later candidate needing a read is held
// with zero further resolver calls, and the halting error is recorded
// only against the candidate whose own read produced it.
func TestEvaluateCandidate_HaltLatch(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	deploymentErr := &domain.TrackerError{Kind: domain.ErrTrackerAuth}

	s := NewState(1000, 10, nil, AgentTotals{})
	pass := &TickResolution{}

	resolver := &fakeBlockerResolver{
		needsReadFn: func(domain.Issue) bool { return true },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = true
			return issue, deploymentErr
		},
	}

	first := domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true}
	second := domain.Issue{ID: "2", Identifier: "X-2", Title: "T", State: "To Do", BlockersUnresolved: true}
	third := domain.Issue{ID: "3", Identifier: "X-3", Title: "T", State: "To Do", BlockersUnresolved: true}

	got1 := EvaluateCandidate(context.Background(), first, s, activeSet, terminalSet, resolver, pass)
	if got1.Reason != SkipBlockersUnresolved || !errors.Is(got1.Err, deploymentErr) {
		t.Fatalf("first candidate decision = %+v, want reason %q carrying the halting error", got1, SkipBlockersUnresolved)
	}
	if !pass.halted {
		t.Fatal("pass.halted = false after a deployment-class failure, want true")
	}

	got2 := EvaluateCandidate(context.Background(), second, s, activeSet, terminalSet, resolver, pass)
	got3 := EvaluateCandidate(context.Background(), third, s, activeSet, terminalSet, resolver, pass)

	if len(resolver.callOrder) != 1 {
		t.Errorf("resolver calls after halt = %d, want 1 (only the candidate that halted the pass)", len(resolver.callOrder))
	}
	if got2.Reason != SkipBlockersUnresolved || got2.Err != nil {
		t.Errorf("halt-skipped candidate decision = %+v, want reason %q with no per-issue error", got2, SkipBlockersUnresolved)
	}
	if got3.Reason != SkipBlockersUnresolved || got3.Err != nil {
		t.Errorf("halt-skipped candidate decision = %+v, want reason %q with no per-issue error", got3, SkipBlockersUnresolved)
	}
	if pass.heldUnread != 2 {
		t.Errorf("pass.heldUnread = %d, want 2", pass.heldUnread)
	}
}

// TestEvaluateCandidate_TransientFailureDoesNotHalt pins that a
// transient-class failure holds only the candidate it happened to,
// and the pass keeps reading later candidates.
func TestEvaluateCandidate_TransientFailureDoesNotHalt(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	transientErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport}

	s := NewState(1000, 10, nil, AgentTotals{})
	pass := &TickResolution{}

	resolver := &fakeBlockerResolver{
		needsReadFn: func(domain.Issue) bool { return true },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = true
			return issue, transientErr
		},
	}

	first := domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "To Do", BlockersUnresolved: true}
	second := domain.Issue{ID: "2", Identifier: "X-2", Title: "T", State: "To Do", BlockersUnresolved: true}

	EvaluateCandidate(context.Background(), first, s, activeSet, terminalSet, resolver, pass)
	if pass.halted {
		t.Fatal("pass.halted = true after a transient-class failure, want false")
	}

	EvaluateCandidate(context.Background(), second, s, activeSet, terminalSet, resolver, pass)
	if len(resolver.callOrder) != 2 {
		t.Errorf("resolver calls = %d, want 2 (transient failure does not halt the pass)", len(resolver.callOrder))
	}
}

// --- Read budget window ---

// budgetWindowIssueCount is the fixed number of needy candidates
// budgetWindowIssues returns: more than maxBlockerReadsPerPass, so the
// read budget binds in every test that uses this fixture.
const budgetWindowIssueCount = 6

// budgetWindowIssues returns budgetWindowIssueCount needy candidates
// that stay eligible and needy across as many passes as the caller
// drives: nothing in the table claims, runs, or exhausts them, so only
// a read outcome changes their state.
func budgetWindowIssues() []domain.Issue {
	issues := make([]domain.Issue, budgetWindowIssueCount)
	for i := range issues {
		id := fmt.Sprintf("N-%d", i+1)
		issues[i] = domain.Issue{ID: id, Identifier: id, Title: "T", State: "To Do", BlockersUnresolved: true}
	}
	return issues
}

// advanceBlockerReadOffset delegates to the production rule so these
// tests cannot pass against a stale copy of it.
func advanceBlockerReadOffset(pass *TickResolution) int {
	return nextBlockerReadOffset(pass)
}

// TestEvaluateCandidate_ReadBudgetWindow pins the read-budget window
// over a fixture with more needy candidates than maxBlockerReadsPerPass:
// each pass reads exactly the budget, holds the remainder as
// blockers_not_read, and the offset rotation reaches every needy
// candidate within the starvation bound.
func TestEvaluateCandidate_ReadBudgetWindow(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)
	transientErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport}

	const needyCount = 6
	issues := budgetWindowIssues()

	resolver := &fakeBlockerResolver{
		needsReadFn: func(issue domain.Issue) bool { return issue.BlockersUnresolved },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = true
			return issue, transientErr
		},
	}

	s := NewState(1000, 10, nil, AgentTotals{})
	offset := 0

	wantPasses := (needyCount + maxBlockerReadsPerPass - 1) / maxBlockerReadsPerPass
	var perPassReads [][]string
	var perPassNotRead []int

	for passNum := range wantPasses {
		pass := &TickResolution{offset: offset}
		before := len(resolver.callOrder)
		notRead := 0

		for _, issue := range issues {
			decision := EvaluateCandidate(context.Background(), issue, s, activeSet, terminalSet, resolver, pass)
			if decision.Reason == SkipBlockersNotRead {
				notRead++
			}
		}

		perPassReads = append(perPassReads, resolver.callOrder[before:])
		perPassNotRead = append(perPassNotRead, notRead)
		offset = advanceBlockerReadOffset(pass)

		if passNum < wantPasses-1 {
			if pass.reads != maxBlockerReadsPerPass {
				t.Fatalf("pass %d: reads = %d, want the full budget %d (not yet the last pass)", passNum, pass.reads, maxBlockerReadsPerPass)
			}
		}
	}

	if len(perPassReads[0]) != maxBlockerReadsPerPass {
		t.Errorf("pass 0 reads = %d, want %d", len(perPassReads[0]), maxBlockerReadsPerPass)
	}
	if perPassNotRead[0] != needyCount-maxBlockerReadsPerPass {
		t.Errorf("pass 0 blockers_not_read count = %d, want %d", perPassNotRead[0], needyCount-maxBlockerReadsPerPass)
	}

	totalReads := 0
	for _, reads := range perPassReads {
		totalReads += len(reads)
	}
	if totalReads != needyCount {
		t.Errorf("total reads across %d passes = %d, want %d (every needy candidate read within the starvation bound)", wantPasses, totalReads, needyCount)
	}

	// The candidates a read was attempted on, across every pass in
	// order, are pass 1's four followed by pass 2's two: the rotation
	// covers the whole population without repeating anyone early.
	var gotOrder []string
	for _, reads := range perPassReads {
		gotOrder = append(gotOrder, reads...)
	}
	wantOrder := []string{"N-1", "N-2", "N-3", "N-4", "N-5", "N-6"}
	if !equalStringSlice(gotOrder, wantOrder) {
		t.Errorf("read order across passes = %v, want %v", gotOrder, wantOrder)
	}
}

// TestEvaluateCandidate_OffsetResetsOnCapacityBreak pins that the
// offset-advance rule resets to zero on a pass that ends by walking
// only part of the list (a capacity break), not only on a pass that
// completes its walk without exhausting the budget.
func TestEvaluateCandidate_OffsetResetsOnCapacityBreak(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	issues := budgetWindowIssues()
	resolver := &fakeBlockerResolver{
		needsReadFn: func(issue domain.Issue) bool { return issue.BlockersUnresolved },
		resolveFn: func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
			issue.BlockersUnresolved = false
			return issue, nil
		},
	}

	s := NewState(1000, 10, nil, AgentTotals{})
	pass := &TickResolution{offset: 0}

	// A capacity break stops the walk after two candidates, well under
	// the read budget.
	for _, issue := range issues[:2] {
		EvaluateCandidate(context.Background(), issue, s, activeSet, terminalSet, resolver, pass)
	}

	if pass.reads != 2 {
		t.Fatalf("pass.reads = %d, want 2 before checking the reset rule", pass.reads)
	}
	if got := advanceBlockerReadOffset(pass); got != 0 {
		t.Errorf("offset after a capacity break with %d reads (below budget) = %d, want 0", pass.reads, got)
	}
}

// TestEvaluateCandidate_DispatchedOrderPreservingSubsequence pins that
// a budget-bounded pass's dispatched issues form an order-preserving
// subsequence of what an equivalent budget-lifted resolution (the same
// candidates fully read across enough passes) would have dispatched.
// Whole-sequence equality is not asserted: it is not satisfiable once
// the budget binds.
func TestEvaluateCandidate_DispatchedOrderPreservingSubsequence(t *testing.T) {
	t.Parallel()

	active := []string{"To Do"}
	terminal := []string{"Done"}
	activeSet := stateSet(active)
	terminalSet := stateSet(terminal)

	issues := budgetWindowIssues()
	resolveOK := func(_ context.Context, issue domain.Issue) (domain.Issue, error) {
		issue.BlockersUnresolved = false
		return issue, nil
	}

	// Budget-bounded run: one pass, offset 0.
	boundedResolver := &fakeBlockerResolver{
		needsReadFn: func(issue domain.Issue) bool { return issue.BlockersUnresolved },
		resolveFn:   resolveOK,
	}
	boundedState := NewState(1000, 10, nil, AgentTotals{})
	boundedPass := &TickResolution{offset: 0}
	var boundedDispatched []string
	for _, issue := range issues {
		decision := EvaluateCandidate(context.Background(), issue, boundedState, activeSet, terminalSet, boundedResolver, boundedPass)
		if decision.Dispatch {
			boundedDispatched = append(boundedDispatched, issue.ID)
		}
	}

	// Budget-lifted run: enough passes over an independent state to
	// read every needy candidate at least once.
	liftedResolver := &fakeBlockerResolver{
		needsReadFn: func(issue domain.Issue) bool { return issue.BlockersUnresolved },
		resolveFn:   resolveOK,
	}
	liftedState := NewState(1000, 10, nil, AgentTotals{})
	var liftedDispatched []string
	offset := 0
	wantPasses := (len(issues) + maxBlockerReadsPerPass - 1) / maxBlockerReadsPerPass
	for range wantPasses {
		pass := &TickResolution{offset: offset}
		for _, issue := range issues {
			decision := EvaluateCandidate(context.Background(), issue, liftedState, activeSet, terminalSet, liftedResolver, pass)
			if decision.Dispatch {
				liftedDispatched = append(liftedDispatched, issue.ID)
			}
		}
		offset = advanceBlockerReadOffset(pass)
	}

	if len(boundedDispatched) == 0 {
		t.Fatal("bounded run dispatched nothing; the test fixture does not exercise the budget")
	}
	if len(boundedDispatched) >= len(liftedDispatched) {
		t.Fatalf("bounded dispatched %v, lifted dispatched %v: the budget must strictly reduce what one pass dispatches for this assertion to have teeth",
			boundedDispatched, liftedDispatched)
	}
	if !isOrderPreservingSubsequence(boundedDispatched, liftedDispatched) {
		t.Errorf("bounded dispatched %v is not an order-preserving subsequence of lifted dispatched %v", boundedDispatched, liftedDispatched)
	}
}

// isOrderPreservingSubsequence reports whether sub appears in full,
// in order, within full (not necessarily contiguous).
func isOrderPreservingSubsequence(sub, full []string) bool {
	i := 0
	for _, v := range full {
		if i < len(sub) && sub[i] == v {
			i++
		}
	}
	return i == len(sub)
}
