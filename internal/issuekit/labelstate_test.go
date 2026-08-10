package issuekit

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestDeriveLabelState merges the per-adapter derivation suites Gitea and
// GitLab carried before this package took over label-state derivation. Every
// case passes the open/closed spelling its source adapter passed: "open" and
// "closed" for the cases sourced from Gitea, "opened" and "closed" for the
// cases sourced from GitLab. GitHub's former TestExtractState cases are not
// repeated here: GitHub spelled its native open and closed states the same
// way Gitea did, so every one of its cases duplicates a Gitea-sourced case
// already present in this table.
func TestDeriveLabelState(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		labels         []string
		nativeState    string
		openNative     string
		closedNative   string
		activeStates   []string
		terminalStates []string
		handoffState   string
		want           string
	}{
		{
			name:           "gitea: single active label match",
			labels:         []string{"in-progress"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "gitea: single terminal label match",
			labels:         []string{"done"},
			nativeState:    "closed",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			// "backlog" precedes "review" in config order; backlog wins even
			// though "review" appears first in the label slice.
			name:           "gitea: multiple active labels first config-order wins",
			labels:         []string{"review", "backlog"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			// Active-state scan runs before the terminal-state scan.
			name:           "gitea: active label beats terminal label",
			labels:         []string{"done", "backlog"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitea: no state labels open issue fallback first active",
			labels:         []string{"bug"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitea: no state labels closed issue fallback first terminal",
			labels:         []string{"bug"},
			nativeState:    "closed",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:         "gitea: empty config open passthrough native",
			labels:       nil,
			nativeState:  "open",
			openNative:   "open",
			closedNative: "closed",
			want:         "open",
		},
		{
			name:         "gitea: empty config closed passthrough native",
			labels:       nil,
			nativeState:  "closed",
			openNative:   "open",
			closedNative: "closed",
			want:         "closed",
		},
		{
			name:           "gitea: uppercase label matches lowercase config",
			labels:         []string{"IN-PROGRESS"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			// A handoff label on an open issue must return the handoff
			// state, not activeStates[0], so a handed-off issue is not
			// re-dispatched as a fresh candidate.
			name:           "gitea: handoff label only open issue",
			labels:         []string{"review"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			name:           "gitea: handoff label uppercase open issue",
			labels:         []string{"REVIEW"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			name:           "gitea: handoff not configured falls back to active",
			labels:         []string{"review"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "backlog",
		},
		{
			// Active-state scan happens before the handoff check regardless
			// of label order in the response.
			name:           "gitea: active label beats handoff when both present",
			labels:         []string{"in-progress", "review"},
			nativeState:    "open",
			openNative:     "open",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
		{
			name:           "gitlab: single active label match",
			labels:         []string{"in-progress"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "gitlab: single terminal label match",
			labels:         []string{"done"},
			nativeState:    "closed",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:           "gitlab: multiple active labels first config-order wins",
			labels:         []string{"review", "backlog"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitlab: active label beats terminal label",
			labels:         []string{"done", "backlog"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitlab: no state label opened issue falls back to first active",
			labels:         []string{"bug"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitlab: no state label closed issue falls back to first terminal",
			labels:         []string{"bug"},
			nativeState:    "closed",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:         "gitlab: empty config opened passes native state through",
			labels:       nil,
			nativeState:  "opened",
			openNative:   "opened",
			closedNative: "closed",
			want:         "opened",
		},
		{
			name:         "gitlab: empty config closed passes native state through",
			labels:       nil,
			nativeState:  "closed",
			openNative:   "opened",
			closedNative: "closed",
			want:         "closed",
		},
		{
			name:           "gitlab: uppercase label matches lowercase config",
			labels:         []string{"IN-PROGRESS"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "gitlab: handoff label only on an open issue",
			labels:         []string{"review"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			name:           "gitlab: handoff not configured falls back to active",
			labels:         []string{"review"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "backlog",
		},
		{
			name:           "gitlab: active label beats handoff when both present",
			labels:         []string{"in-progress", "review"},
			nativeState:    "opened",
			openNative:     "opened",
			closedNative:   "closed",
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			states := LabelStates{Active: tt.activeStates, Terminal: tt.terminalStates, Handoff: tt.handoffState}
			got := DeriveLabelState(tt.labels, tt.nativeState, tt.openNative, tt.closedNative, states, "1", nil)
			if got != tt.want {
				t.Errorf("DeriveLabelState(%v, %q) = %q, want %q", tt.labels, tt.nativeState, got, tt.want)
			}
		})
	}
}

// TestDeriveLabelState_MultiMatchLogsWarning merges the Gitea and GitLab
// multi-match WARN cases. Both now assert the shared issue_identifier
// attribute (R21) in place of Gitea's former issue_index and GitLab's former
// iid; the returned state, the message string, and the matched-label
// assertions are unchanged from the two source tests.
func TestDeriveLabelState_MultiMatchLogsWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		nativeState  string
		openNative   string
		closedNative string
	}{
		{"gitea open/closed spelling", "open", "open", "closed"},
		{"gitlab opened/closed spelling", "opened", "opened", "closed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Two distinct configured labels on one issue (an active label
			// and a terminal label), not a handoff overlap: this is the
			// config-drift case the WARN exists to surface.
			labels := []string{"backlog", "done"}
			states := LabelStates{
				Active:   []string{"backlog", "in-progress"},
				Terminal: []string{"done", "wontfix"},
			}

			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			got := DeriveLabelState(labels, tt.nativeState, tt.openNative, tt.closedNative, states, "77", log)

			// Active states are scanned before terminal states, so "backlog"
			// is the first match and wins despite "done" also matching.
			if got != "backlog" {
				t.Errorf("DeriveLabelState(multi-match) = %q, want %q (first match by scan order)", got, "backlog")
			}

			output := buf.String()
			if !strings.Contains(output, "kept first of multiple matching state labels") {
				t.Errorf("log output missing multi-match WARN message\noutput: %s", output)
			}
			if !strings.Contains(output, "issue_identifier=77") {
				t.Errorf("log output missing issue_identifier=77\noutput: %s", output)
			}
			if !strings.Contains(output, "backlog") || !strings.Contains(output, "done") {
				t.Errorf("log output should name both matched labels (backlog, done)\noutput: %s", output)
			}
		})
	}
}

// TestDeriveLabelState_NilLoggerUsesDefaultWithoutPanic merges the Gitea and
// GitLab nil-logger cases.
func TestDeriveLabelState_NilLoggerUsesDefaultWithoutPanic(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		nativeState  string
		openNative   string
		closedNative string
	}{
		{"gitea open/closed spelling", "open", "open", "closed"},
		{"gitlab opened/closed spelling", "opened", "opened", "closed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A multi-match scenario with log=nil must fall back to
			// slog.Default() rather than panic on a nil receiver.
			labels := []string{"backlog", "done"}
			states := LabelStates{Active: []string{"backlog"}, Terminal: []string{"done"}}

			got := DeriveLabelState(labels, tt.nativeState, tt.openNative, tt.closedNative, states, "1", nil)
			if got != "backlog" {
				t.Errorf("DeriveLabelState(nil logger) = %q, want %q", got, "backlog")
			}
		})
	}
}

// TestDefaultActiveLabelStates_ReturnsFreshCopy proves a caller mutating
// the returned slice cannot corrupt a later caller's default list.
func TestDefaultActiveLabelStates_ReturnsFreshCopy(t *testing.T) {
	t.Parallel()

	want := []string{"backlog", "in-progress", "review"}
	got := DefaultActiveLabelStates()
	if len(got) != len(want) {
		t.Fatalf("DefaultActiveLabelStates() = %v, want %v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("DefaultActiveLabelStates()[%d] = %q, want %q", i, got[i], s)
		}
	}

	got[0] = "mutated"
	again := DefaultActiveLabelStates()
	if again[0] != "backlog" {
		t.Errorf("DefaultActiveLabelStates() after caller mutation = %v, want a fresh copy unaffected by the earlier mutation", again)
	}
}

// TestDefaultTerminalLabelStates_ReturnsFreshCopy proves a caller
// mutating the returned slice cannot corrupt a later caller's default
// list.
func TestDefaultTerminalLabelStates_ReturnsFreshCopy(t *testing.T) {
	t.Parallel()

	want := []string{"done", "wontfix"}
	got := DefaultTerminalLabelStates()
	if len(got) != len(want) {
		t.Fatalf("DefaultTerminalLabelStates() = %v, want %v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("DefaultTerminalLabelStates()[%d] = %q, want %q", i, got[i], s)
		}
	}

	got[0] = "mutated"
	again := DefaultTerminalLabelStates()
	if again[0] != "done" {
		t.Errorf("DefaultTerminalLabelStates() after caller mutation = %v, want a fresh copy unaffected by the earlier mutation", again)
	}
}

// TestCurrentLabelState merges the GitHub and GitLab TestFindCurrentStateLabel
// suites; a GitLab case duplicating a GitHub case exactly (same labels, same
// configured state lists, same expected result) appears once.
func TestCurrentLabelState(t *testing.T) {
	t.Parallel()

	active := []string{"backlog", "in-progress", "review"}
	terminal := []string{"done", "wontfix"}

	tests := []struct {
		name           string
		labels         []string
		activeStates   []string
		terminalStates []string
		handoffState   string
		want           string
	}{
		{
			name:           "github: active state label found",
			labels:         []string{"in-progress", "bug"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "github: terminal state label found",
			labels:         []string{"done"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "done",
		},
		{
			name:           "github: uppercase label returned lowercased",
			labels:         []string{"BACKLOG"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "github: no state label returns empty string",
			labels:         []string{"bug", "priority"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "",
		},
		{
			name:           "github: empty labels returns empty string",
			labels:         nil,
			activeStates:   active,
			terminalStates: terminal,
			want:           "",
		},
		{
			// Handoff label present and handoffState configured -> handoff
			// state returned.
			name:           "github: handoff state label found",
			labels:         []string{"review"},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			name:           "github: handoff label uppercase found",
			labels:         []string{"REVIEW"},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "review",
		},
		{
			// When handoffState is empty, a label that matches no
			// configured state returns empty (no fallback).
			name:           "github: handoff not configured returns empty",
			labels:         []string{"review"},
			activeStates:   []string{"backlog"},
			terminalStates: []string{"done"},
			handoffState:   "",
			want:           "",
		},
		{
			// Active scan precedes handoff scan; active label wins
			// regardless of the order the platform returns labels in.
			name:           "github: active beats handoff when both present",
			labels:         []string{"in-progress", "review"},
			activeStates:   []string{"backlog", "in-progress"},
			terminalStates: []string{"done"},
			handoffState:   "review",
			want:           "in-progress",
		},
		{
			name:           "gitlab: active match",
			labels:         []string{"in-progress"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "in-progress",
		},
		{
			name:           "gitlab: handoff match",
			labels:         []string{"needs-review"},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "needs-review",
			want:           "needs-review",
		},
		{
			name:           "gitlab: config-order precedence: active beats terminal",
			labels:         []string{"done", "backlog"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "backlog",
		},
		{
			name:           "gitlab: config-order precedence: active beats handoff",
			labels:         []string{"review", "in-progress"},
			activeStates:   active,
			terminalStates: terminal,
			handoffState:   "review",
			want:           "in-progress",
		},
		{
			name:           "gitlab: no configured label present",
			labels:         []string{"bug"},
			activeStates:   active,
			terminalStates: terminal,
			want:           "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			states := LabelStates{Active: tt.activeStates, Terminal: tt.terminalStates, Handoff: tt.handoffState}
			got := CurrentLabelState(tt.labels, states)
			if got != tt.want {
				t.Errorf("CurrentLabelState(%v, handoff=%q) = %q, want %q", tt.labels, tt.handoffState, got, tt.want)
			}
		})
	}
}
