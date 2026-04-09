package github

import "testing"

func TestExtractState(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		labels         []githubLabel
		nativeState    string
		activeStates   []string
		terminalStates []string
		handoffState   string
		want           string
	}{
		{
			name:           "single active label match",
			labels:         []githubLabel{{Name: "in-progress"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "in-progress",
		},
		{
			name:           "single terminal label match",
			labels:         []githubLabel{{Name: "done"}},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "done",
		},
		{
			// "backlog" precedes "review" in config order; backlog wins even though
			// "review" appears first in the label slice.
			name:           "multiple active labels first config-order wins",
			labels:         []githubLabel{{Name: "review"}, {Name: "backlog"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "backlog",
		},
		{
			// Active state scan occurs before terminal state scan.
			name:           "active label beats terminal label",
			labels:         []githubLabel{{Name: "done"}, {Name: "backlog"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "backlog",
		},
		{
			// No matching label on open issue → fallback to first active state.
			name:           "no state labels open issue fallback first active",
			labels:         []githubLabel{{Name: "bug"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "backlog",
		},
		{
			// No matching label on closed issue → fallback to first terminal state.
			name:           "no state labels closed issue fallback first terminal",
			labels:         []githubLabel{{Name: "bug"}},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "done",
		},
		{
			// Empty config with open native state → passthrough "open".
			name:           "empty config open passthrough native",
			labels:         []githubLabel{},
			nativeState:    "open",
			activeStates:   nil,
			terminalStates: nil,
			handoffState:   "",
			want:           "open",
		},
		{
			// Upper-case label should match lower-case config entry.
			name:           "uppercase label matches lowercase config",
			labels:         []githubLabel{{Name: "IN-PROGRESS"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "in-progress",
		},
		{
			// Handoff label on an open issue returns the handoff state, not activeStates[0].
			// This prevents FetchCandidateIssues from treating the issue as dispatchable.
			name:           "handoff label only open issue",
			labels:         []githubLabel{{Name: "review"}},
			nativeState:    "open",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			// Case-insensitive: uppercase label on issue matches lowercase handoffState config.
			name:           "handoff label uppercase open issue",
			labels:         []githubLabel{{Name: "REVIEW"}},
			nativeState:    "open",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			// When handoffState is empty, an unrecognized label on an open issue
			// still falls back to activeStates[0] — pre-fix behavior preserved.
			name:           "handoff not configured falls back to active",
			labels:         []githubLabel{{Name: "review"}},
			nativeState:    "open",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "backlog",
		},
		{
			// Active state scan happens before handoff; active label wins regardless
			// of label order returned by the GitHub API.
			name:           "active label beats handoff when both present",
			labels:         []githubLabel{{Name: "in-progress"}, {Name: "review"}},
			nativeState:    "open",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := extractState(tt.labels, tt.nativeState, tt.activeStates, tt.terminalStates, tt.handoffState)
			if got != tt.want {
				t.Errorf("extractState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsTerminalState(t *testing.T) {
	t.Parallel()

	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		state          string
		terminalStates []string
		want           bool
	}{
		{"done is terminal", "done", terminal, true},
		{"wontfix is terminal", "wontfix", terminal, true},
		{"active state not terminal", "in-progress", terminal, false},
		{"empty state not terminal", "", terminal, false},
		{"empty terminal list", "done", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isTerminalState(tt.state, tt.terminalStates)
			if got != tt.want {
				t.Errorf("isTerminalState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestIsActiveState(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}

	tests := []struct {
		name         string
		state        string
		activeStates []string
		want         bool
	}{
		{"backlog is active", "backlog", active, true},
		{"in-progress is active", "in-progress", active, true},
		{"review is active", "review", active, true},
		{"terminal state not active", "done", active, false},
		{"empty state not active", "", active, false},
		{"empty active list", "backlog", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isActiveState(tt.state, tt.activeStates)
			if got != tt.want {
				t.Errorf("isActiveState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestFindCurrentStateLabel(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		labels         []githubLabel
		activeStates   []string
		terminalStates []string
		handoffState   string
		want           string
	}{
		{
			name:           "active state label found",
			labels:         []githubLabel{{Name: "in-progress"}, {Name: "bug"}},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "in-progress",
		},
		{
			name:           "terminal state label found",
			labels:         []githubLabel{{Name: "done"}},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "done",
		},
		{
			name:           "uppercase label returned lowercased",
			labels:         []githubLabel{{Name: "BACKLOG"}},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "backlog",
		},
		{
			name:           "no state label returns empty string",
			labels:         []githubLabel{{Name: "bug"}, {Name: "priority"}},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "",
		},
		{
			name:           "empty labels returns empty string",
			labels:         nil,
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "",
			want:           "",
		},
		{
			// Handoff label present and handoffState configured → handoff state returned.
			name:           "handoff state label found",
			labels:         []githubLabel{{Name: "review"}},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			// Case-insensitive: uppercase label matches lowercase handoffState config.
			name:           "handoff label uppercase found",
			labels:         []githubLabel{{Name: "REVIEW"}},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			// When handoffState is empty, a label that matches no configured state
			// returns empty (no fallback inside findCurrentStateLabel).
			name:           "handoff not configured returns empty",
			labels:         []githubLabel{{Name: "review"}},
			activeStates:   []string{"backlog"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "",
		},
		{
			// Active scan precedes handoff scan; active label wins regardless of
			// the order GitHub returns labels in the API response.
			name:           "active beats handoff when both present",
			labels:         []githubLabel{{Name: "in-progress"}, {Name: "review"}},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := findCurrentStateLabel(tt.labels, tt.activeStates, tt.terminalStates, tt.handoffState)
			if got != tt.want {
				t.Errorf("findCurrentStateLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
