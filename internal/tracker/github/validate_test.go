package github

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Test helpers ---

// hasDiagCheck reports whether any diag in the slice has the given check name.
func hasDiagCheck(diags []registry.ValidationDiag, check string) bool {
	for _, d := range diags {
		if d.Check == check {
			return true
		}
	}
	return false
}

// diagsWithSeverity returns the subset of diags with the given severity.
func diagsWithSeverity(diags []registry.ValidationDiag, severity string) []registry.ValidationDiag {
	var out []registry.ValidationDiag
	for _, d := range diags {
		if d.Severity == severity {
			out = append(out, d)
		}
	}
	return out
}

// --- Tests ---

func TestValidateProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		project         string
		wantCount       int    // expected number of diags
		wantCheck       string // expected check name (when wantCount > 0)
		wantMsgContains string // expected substring of message (when wantCount > 0)
	}{
		{
			name:      "empty project is skipped",
			project:   "",
			wantCount: 0,
		},
		{
			name:            "whitespace-only project is rejected",
			project:         "  ",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "owner/repo format",
		},
		{
			name:      "valid owner/repo",
			project:   "sortie-ai/sortie",
			wantCount: 0,
		},
		{
			name:      "valid single-char segments",
			project:   "o/r",
			wantCount: 0,
		},
		{
			name:      "valid uppercase",
			project:   "OWNER/REPO",
			wantCount: 0,
		},
		{
			name:      "valid with hyphens dots and digits",
			project:   "my-org/my.repo-v2",
			wantCount: 0,
		},
		{
			name:            "leading outer space in owner is rejected",
			project:         " owner/repo",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "must not contain whitespace",
		},
		{
			name:            "trailing outer space in repo is rejected",
			project:         "owner/repo ",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "must not contain whitespace",
		},
		{
			name:            "no slash",
			project:         "noslash",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "owner/repo format",
		},
		{
			name:            "multiple slashes",
			project:         "a/b/c",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "owner/repo format",
		},
		{
			name:            "empty owner segment",
			project:         "/repo",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "owner/repo format",
		},
		{
			name:            "empty repo segment",
			project:         "owner/",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "owner/repo format",
		},
		{
			name:            "space within owner segment",
			project:         "my org/repo",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "must not contain whitespace",
		},
		{
			name:            "space within repo segment",
			project:         "owner/my repo",
			wantCount:       1,
			wantCheck:       "tracker.project.format",
			wantMsgContains: "must not contain whitespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateProject(tt.project)

			if len(got) != tt.wantCount {
				t.Fatalf("validateProject(%q) = %d diags, want %d; diags: %v", tt.project, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			if got[0].Check != tt.wantCheck {
				t.Errorf("validateProject(%q) diag[0].Check = %q, want %q", tt.project, got[0].Check, tt.wantCheck)
			}
			if got[0].Severity != "error" {
				t.Errorf("validateProject(%q) diag[0].Severity = %q, want %q", tt.project, got[0].Severity, "error")
			}
			if !strings.Contains(got[0].Message, tt.wantMsgContains) {
				t.Errorf("validateProject(%q) diag[0].Message = %q, want to contain %q", tt.project, got[0].Message, tt.wantMsgContains)
			}
		})
	}
}

func TestValidateAPIKeyHint(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv to control GITHUB_TOKEN.

	t.Run("api_key set – no diagnostics", func(t *testing.T) {
		got := validateAPIKeyHint("ghp_mytoken")

		if len(got) != 0 {
			t.Errorf("validateAPIKeyHint(non-empty) = %v, want empty", got)
		}
	})

	t.Run("api_key empty GITHUB_TOKEN set – hint warning", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "ghp_envtoken")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with GITHUB_TOKEN set = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.github_token_hint" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.github_token_hint")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
	})

	t.Run("api_key empty GITHUB_TOKEN unset – missing warning", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with GITHUB_TOKEN unset = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.github_token_missing" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.github_token_missing")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
	})
}

func TestValidateStateLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		field     string
		states    []string
		wantCount int
		wantIdx   []int // indices expected to have warnings
	}{
		{
			name:      "nil slice – no warnings",
			field:     "tracker.active_states",
			states:    nil,
			wantCount: 0,
		},
		{
			name:      "all non-empty – no warnings",
			field:     "tracker.active_states",
			states:    []string{"backlog", "in-progress"},
			wantCount: 0,
		},
		{
			name:      "single empty at index 0",
			field:     "tracker.active_states",
			states:    []string{""},
			wantCount: 1,
			wantIdx:   []int{0},
		},
		{
			name:      "empty at index 1",
			field:     "tracker.terminal_states",
			states:    []string{"done", ""},
			wantCount: 1,
			wantIdx:   []int{1},
		},
		{
			name:      "whitespace-only element",
			field:     "tracker.active_states",
			states:    []string{"backlog", "  ", "done"},
			wantCount: 1,
			wantIdx:   []int{1},
		},
		{
			name:      "multiple empties",
			field:     "tracker.active_states",
			states:    []string{"", "backlog", ""},
			wantCount: 2,
			wantIdx:   []int{0, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateStateLabels(tt.field, tt.states)

			if len(got) != tt.wantCount {
				t.Fatalf("validateStateLabels(%q, %v) = %d diags, want %d; diags: %v", tt.field, tt.states, len(got), tt.wantCount, got)
			}
			for i, d := range got {
				wantCheck := tt.field + ".empty_element"
				if d.Check != wantCheck {
					t.Errorf("validateStateLabels diag[%d].Check = %q, want %q", i, d.Check, wantCheck)
				}
				if d.Severity != "warning" {
					t.Errorf("validateStateLabels diag[%d].Severity = %q, want %q", i, d.Severity, "warning")
				}
				if tt.wantIdx != nil {
					wantMsg := fmt.Sprintf("%s[%d]:", tt.field, tt.wantIdx[i])
					if !strings.Contains(d.Message, wantMsg) {
						t.Errorf("validateStateLabels diag[%d].Message = %q, want to contain %q", i, d.Message, wantMsg)
					}
				}
			}
		})
	}
}

func TestValidateStateOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		fields        registry.TrackerConfigFields
		wantChecks    []string // check names that MUST appear in diags
		wantDiagCount int      // expected total warning count
	}{
		{
			name: "no overlap – no diagnostics",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog", "in-progress"},
				TerminalStates: []string{"done", "wontfix"},
			},
			wantDiagCount: 0,
		},
		{
			name: "case-insensitive overlap on done",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog", "Done"},
				TerminalStates: []string{"done", "wontfix"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 1,
		},
		{
			name: "case-insensitive overlap uppercase active",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"BACKLOG"},
				TerminalStates: []string{"backlog"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 1,
		},
		{
			name: "empty active – no overlap",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{},
				TerminalStates: []string{"done"},
			},
			wantDiagCount: 0,
		},
		{
			name: "multiple overlaps sorted",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"a", "b"},
				TerminalStates: []string{"b", "c", "a"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 2,
		},
		{
			name: "empty labels in both slices – skipped by overlap check",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{""},
				TerminalStates: []string{""},
			},
			wantDiagCount: 0,
		},
		{
			name: "handoff_state in active_states",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"review", "backlog"},
				TerminalStates: []string{"done"},
				HandoffState:   "review",
			},
			wantChecks:    []string{"tracker.handoff_state.collision"},
			wantDiagCount: 1,
		},
		{
			name: "handoff_state in terminal_states",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"done"},
				HandoffState:   "done",
			},
			wantChecks:    []string{"tracker.handoff_state.collision"},
			wantDiagCount: 1,
		},
		{
			name: "in_progress_state in terminal_states",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done", "closed"},
				InProgressState: "closed",
			},
			wantChecks:    []string{"tracker.in_progress_state.collision"},
			wantDiagCount: 1,
		},
		{
			name: "in_progress_state collides with handoff_state",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done"},
				HandoffState:    "review",
				InProgressState: "review",
			},
			wantChecks:    []string{"tracker.in_progress_state.collision"},
			wantDiagCount: 1,
		},
		{
			name: "empty handoff_state is skipped",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"done"},
				HandoffState:   "",
			},
			wantDiagCount: 0,
		},
		{
			name: "empty in_progress_state is skipped",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done"},
				InProgressState: "",
			},
			wantDiagCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateStateOverlap(tt.fields)

			if len(got) != tt.wantDiagCount {
				t.Fatalf("validateStateOverlap() = %d diags, want %d; diags: %v", len(got), tt.wantDiagCount, got)
			}
			for _, check := range tt.wantChecks {
				if !hasDiagCheck(got, check) {
					t.Errorf("validateStateOverlap() missing diag with check %q; got: %v", check, got)
				}
			}
			// All returned diags must be warnings.
			for i, d := range got {
				if d.Severity != "warning" {
					t.Errorf("validateStateOverlap() diag[%d].Severity = %q, want %q", i, d.Severity, "warning")
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	// Fully valid fields should produce zero diagnostics.
	fields := registry.TrackerConfigFields{
		Kind:           "github",
		Project:        "owner/repo",
		APIKey:         "tok",
		ActiveStates:   []string{"backlog"},
		TerminalStates: []string{"done"},
	}

	got := validateConfig(fields)

	errors := diagsWithSeverity(got, "error")
	warnings := diagsWithSeverity(got, "warning")

	if len(errors) != 0 {
		t.Errorf("validateConfig(valid) errors = %v, want empty", errors)
	}
	if len(warnings) != 0 {
		t.Errorf("validateConfig(valid) warnings = %v, want empty", warnings)
	}
}
