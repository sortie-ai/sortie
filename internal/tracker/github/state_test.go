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
		want           string
	}{
		{
			name:           "single active label match",
			labels:         []githubLabel{{Name: "in-progress"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "single terminal label match",
			labels:         []githubLabel{{Name: "done"}},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
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
			want:           "backlog",
		},
		{
			// Active state scan occurs before terminal state scan.
			name:           "active label beats terminal label",
			labels:         []githubLabel{{Name: "done"}, {Name: "backlog"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			// No matching label on open issue → fallback to first active state.
			name:           "no state labels open issue fallback first active",
			labels:         []githubLabel{{Name: "bug"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			// No matching label on closed issue → fallback to first terminal state.
			name:           "no state labels closed issue fallback first terminal",
			labels:         []githubLabel{{Name: "bug"}},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			// Empty config with open native state → passthrough "open".
			name:           "empty config open passthrough native",
			labels:         []githubLabel{},
			nativeState:    "open",
			activeStates:   nil,
			terminalStates: nil,
			want:           "open",
		},
		{
			// Upper-case label should match lower-case config entry.
			name:           "uppercase label matches lowercase config",
			labels:         []githubLabel{{Name: "IN-PROGRESS"}},
			nativeState:    "open",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := extractState(tt.labels, tt.nativeState, tt.activeStates, tt.terminalStates)
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
		want           string
	}{
		{
			name:           "active state label found",
			labels:         []githubLabel{{Name: "in-progress"}, {Name: "bug"}},
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "terminal state label found",
			labels:         []githubLabel{{Name: "done"}},
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:           "uppercase label returned lowercased",
			labels:         []githubLabel{{Name: "BACKLOG"}},
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "no state label returns empty string",
			labels:         []githubLabel{{Name: "bug"}, {Name: "priority"}},
			activeStates:   active,
			terminalStates: terminal,
			want:           "",
		},
		{
			name:           "empty labels returns empty string",
			labels:         nil,
			activeStates:   active,
			terminalStates: terminal,
			want:           "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := findCurrentStateLabel(tt.labels, tt.activeStates, tt.terminalStates)
			if got != tt.want {
				t.Errorf("findCurrentStateLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}
