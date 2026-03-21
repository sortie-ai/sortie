package orchestrator

import (
	"cmp"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// SortForDispatch returns a new slice of issues sorted in dispatch priority
// order: priority ascending (nil sorts last), created_at oldest first (empty
// sorts last), identifier lexicographic tiebreaker. The input slice is not
// modified.
func SortForDispatch(issues []domain.Issue) []domain.Issue {
	if len(issues) == 0 {
		return nil
	}
	sorted := slices.Clone(issues)
	slices.SortStableFunc(sorted, compareDispatchOrder)
	return sorted
}

// compareDispatchOrder implements the three-key comparator for dispatch
// sorting. Returns negative if a should sort before b, positive if after,
// zero if equal.
func compareDispatchOrder(a, b domain.Issue) int {
	// Key 1: priority ascending, nil last.
	if c := comparePriority(a.Priority, b.Priority); c != 0 {
		return c
	}
	// Key 2: created_at oldest first, empty last.
	if c := compareCreatedAt(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	// Key 3: identifier lexicographic tiebreaker.
	return cmp.Compare(a.Identifier, b.Identifier)
}

// comparePriority compares two nullable integer priorities. Non-nil values
// sort ascending; nil values sort after all non-nil values.
func comparePriority(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return cmp.Compare(*a, *b)
	}
}

// compareCreatedAt compares two ISO-8601 timestamp strings. Non-empty values
// sort lexicographically (oldest first); empty values sort after all
// non-empty values.
func compareCreatedAt(a, b string) int {
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	default:
		return cmp.Compare(a, b)
	}
}

// ShouldDispatch reports whether an issue is eligible for dispatch given the
// current orchestrator state and configured active/terminal states. It
// evaluates issue-level eligibility rules: required fields, active state,
// not running, not claimed, and blocker rule. Capacity checks (global and
// per-state slot limits) are not included; the dispatch loop checks slot
// availability incrementally between dispatches via [HasAvailableSlots].
func ShouldDispatch(issue domain.Issue, state *State, activeStates, terminalStates []string) bool {
	// Rule 1: required fields.
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}

	activeSet := stateSet(activeStates)
	terminalSet := stateSet(terminalStates)
	normalizedState := strings.ToLower(issue.State)

	// Rule 2: state must be active and not terminal.
	if _, active := activeSet[normalizedState]; !active {
		return false
	}
	if _, terminal := terminalSet[normalizedState]; terminal {
		return false
	}

	// Rule 3: not already running.
	if _, running := state.Running[issue.ID]; running {
		return false
	}

	// Rule 4: not already claimed.
	if _, claimed := state.Claimed[issue.ID]; claimed {
		return false
	}

	// Rule 5: blocker rule — applies to issues in an active non-running
	// state. Rules 2 and 3 guarantee that precondition: by this point the
	// issue's state is active (Rule 2) and the issue is not in the Running
	// map (Rule 3). Any non-terminal blocker blocks dispatch.
	for _, blocker := range issue.BlockedBy {
		if blocker.State == "" {
			return false
		}
		if _, terminal := terminalSet[strings.ToLower(blocker.State)]; !terminal {
			return false
		}
	}

	return true
}

// stateSet builds a set of lowercase state names for O(1) membership testing.
func stateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
}
