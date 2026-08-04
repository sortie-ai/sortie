package gitlab

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestDeriveState(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		labels         []string
		nativeState    string
		activeStates   []string
		terminalStates []string
		handoffState   string
		want           string
	}{
		{
			name:           "single active label match",
			labels:         []string{"in-progress"},
			nativeState:    "opened",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "single terminal label match",
			labels:         []string{"done"},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			// "backlog" precedes "review" in config order; backlog wins even
			// though "review" appears first in the label slice.
			name:           "multiple active labels first config-order wins",
			labels:         []string{"review", "backlog"},
			nativeState:    "opened",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			// Active-state scan runs before the terminal-state scan.
			name:           "active label beats terminal label",
			labels:         []string{"done", "backlog"},
			nativeState:    "opened",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "no state label opened issue falls back to first active",
			labels:         []string{"bug"},
			nativeState:    "opened",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "no state label closed issue falls back to first terminal",
			labels:         []string{"bug"},
			nativeState:    "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:           "empty config opened passes native state through",
			labels:         nil,
			nativeState:    "opened",
			activeStates:   nil,
			terminalStates: nil,
			want:           "opened",
		},
		{
			name:           "empty config closed passes native state through",
			labels:         nil,
			nativeState:    "closed",
			activeStates:   nil,
			terminalStates: nil,
			want:           "closed",
		},
		{
			name:           "uppercase label matches lowercase config",
			labels:         []string{"IN-PROGRESS"},
			nativeState:    "opened",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			// A handoff label on an open issue must return the handoff
			// state, not activeStates[0], so a handed-off issue is not
			// re-dispatched as a fresh candidate.
			name:           "handoff label only on an open issue",
			labels:         []string{"review"},
			nativeState:    "opened",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			name:           "handoff not configured falls back to active",
			labels:         []string{"review"},
			nativeState:    "opened",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "backlog",
		},
		{
			// Active-state scan happens before the handoff check regardless
			// of label order in the response.
			name:           "active label beats handoff when both present",
			labels:         []string{"in-progress", "review"},
			nativeState:    "opened",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := deriveState(tt.labels, tt.nativeState, tt.activeStates, tt.terminalStates, tt.handoffState, "1", nil)
			if got != tt.want {
				t.Errorf("deriveState(%v, %q) = %q, want %q", tt.labels, tt.nativeState, got, tt.want)
			}
		})
	}
}

func TestDeriveState_MultiMatchLogsWarning(t *testing.T) {
	t.Parallel()

	// Two distinct configured labels on one issue (an active label and a
	// terminal label), not a handoff overlap: this is the config-drift
	// case the WARN exists to surface.
	labels := []string{"backlog", "done"}
	active := []string{"backlog", "in-progress"}
	terminal := []string{"done", "wontfix"}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := deriveState(labels, "opened", active, terminal, "", "77", log)

	// Active states are scanned before terminal states, so "backlog" is
	// the first match and wins despite "done" also matching.
	if got != "backlog" {
		t.Errorf("deriveState(multi-match) = %q, want %q (first match by scan order)", got, "backlog")
	}

	output := buf.String()
	if !strings.Contains(output, "kept first of multiple matching state labels") {
		t.Errorf("log output missing multi-match WARN message\noutput: %s", output)
	}
	if !strings.Contains(output, "iid=77") {
		t.Errorf("log output missing iid=77\noutput: %s", output)
	}
	if !strings.Contains(output, "backlog") || !strings.Contains(output, "done") {
		t.Errorf("log output should name both matched labels (backlog, done)\noutput: %s", output)
	}
}

func TestDeriveState_NilLoggerUsesDefaultWithoutPanic(t *testing.T) {
	t.Parallel()

	// A multi-match scenario with log=nil must fall back to slog.Default()
	// rather than panic on a nil receiver.
	labels := []string{"backlog", "done"}

	got := deriveState(labels, "opened", []string{"backlog"}, []string{"done"}, "", "1", nil)
	if got != "backlog" {
		t.Errorf("deriveState(nil logger) = %q, want %q", got, "backlog")
	}
}
