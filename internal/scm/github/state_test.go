package github

import "testing"

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
