package orchestrator

import (
	"testing"

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
