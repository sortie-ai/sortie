package registry_test

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// hasDiagCheck reports whether any diag in the slice has the given check name.
func hasDiagCheck(diags []registry.ValidationDiag, check string) bool {
	for _, d := range diags {
		if d.Check == check {
			return true
		}
	}
	return false
}

func TestValidateProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		project         string
		wantCount       int
		wantCheck       string
		wantMsgContains string
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

			got := registry.DiagOwnerRepoProject(tt.project)

			if len(got) != tt.wantCount {
				t.Fatalf("DiagOwnerRepoProject(%q) = %d diags, want %d; diags: %v", tt.project, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			if got[0].Check != tt.wantCheck {
				t.Errorf("DiagOwnerRepoProject(%q) diag[0].Check = %q, want %q", tt.project, got[0].Check, tt.wantCheck)
			}
			if got[0].Severity != string(registry.SeverityError) {
				t.Errorf("DiagOwnerRepoProject(%q) diag[0].Severity = %q, want %q", tt.project, got[0].Severity, registry.SeverityError)
			}
			if !strings.Contains(got[0].Message, tt.wantMsgContains) {
				t.Errorf("DiagOwnerRepoProject(%q) diag[0].Message = %q, want to contain %q", tt.project, got[0].Message, tt.wantMsgContains)
			}
		})
	}
}

func TestValidateStateLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		field     string
		states    []string
		sev       registry.DiagSeverity
		wantCount int
		wantCheck string // suffix appended to field; empty means no diags expected
	}{
		{
			name:      "nil slice has no diagnostics",
			field:     "tracker.active_states",
			states:    nil,
			sev:       registry.SeverityWarning,
			wantCount: 0,
		},
		{
			name:      "empty slice has no diagnostics",
			field:     "tracker.active_states",
			states:    []string{},
			sev:       registry.SeverityWarning,
			wantCount: 0,
		},
		{
			name:      "all clean has no diagnostics",
			field:     "tracker.active_states",
			states:    []string{"backlog", "in-progress"},
			sev:       registry.SeverityWarning,
			wantCount: 0,
		},
		{
			name:      "single empty element at index 0",
			field:     "tracker.active_states",
			states:    []string{""},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "empty element at index 1",
			field:     "tracker.terminal_states",
			states:    []string{"done", ""},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "whitespace-only element maps to empty_element",
			field:     "tracker.active_states",
			states:    []string{"backlog", "  ", "done"},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "whitespace-only element, terminal_states",
			field:     "tracker.terminal_states",
			states:    []string{"done", "   "},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "multiple empties",
			field:     "tracker.active_states",
			states:    []string{"", "backlog", ""},
			sev:       registry.SeverityWarning,
			wantCount: 2,
			wantCheck: "empty_element",
		},
		{
			name:      "non-empty leading-padded element",
			field:     "tracker.active_states",
			states:    []string{" review"},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "non-empty trailing-padded element",
			field:     "tracker.terminal_states",
			states:    []string{"done "},
			sev:       registry.SeverityWarning,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "nil slice yields no diagnostics (error severity)",
			field:     "tracker.active_states",
			states:    nil,
			sev:       registry.SeverityError,
			wantCount: 0,
		},
		{
			name:      "all non-empty elements yield no diagnostics (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Backlog", "In Progress", "Done"},
			sev:       registry.SeverityError,
			wantCount: 0,
		},
		{
			name:      "single empty element at index 0 (error severity)",
			field:     "tracker.active_states",
			states:    []string{""},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "empty element at index 1 (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Backlog", ""},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "whitespace-only element maps to empty_element not untrimmed_element (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Backlog", "   ", "Done"},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "multiple empty elements (error severity)",
			field:     "tracker.active_states",
			states:    []string{"", "Backlog", ""},
			sev:       registry.SeverityError,
			wantCount: 2,
			wantCheck: "empty_element",
		},
		{
			name:      "trailing-space element yields untrimmed_element (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Todo "},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "leading-space element yields untrimmed_element (error severity)",
			field:     "tracker.active_states",
			states:    []string{" Todo"},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "trailing-tab element yields untrimmed_element (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Todo\t"},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "clean element yields no diag (error severity)",
			field:     "tracker.active_states",
			states:    []string{"Todo"},
			sev:       registry.SeverityError,
			wantCount: 0,
		},
		{
			name:      "terminal_states nil slice yields no diagnostics (error severity)",
			field:     "tracker.terminal_states",
			states:    nil,
			sev:       registry.SeverityError,
			wantCount: 0,
		},
		{
			name:      "terminal_states single empty element (error severity)",
			field:     "tracker.terminal_states",
			states:    []string{""},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "terminal_states trailing-space element yields untrimmed_element (error severity)",
			field:     "tracker.terminal_states",
			states:    []string{"Done "},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "terminal_states leading-space element yields untrimmed_element (error severity)",
			field:     "tracker.terminal_states",
			states:    []string{" Done"},
			sev:       registry.SeverityError,
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := registry.DiagStateLabelElements(tt.field, tt.states, tt.sev)

			if len(got) != tt.wantCount {
				t.Fatalf("DiagStateLabelElements(%q, %v, %v) = %d diags, want %d; diags: %v", tt.field, tt.states, tt.sev, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			wantCheck := tt.field + "." + tt.wantCheck
			for i, d := range got {
				if d.Check != wantCheck {
					t.Errorf("DiagStateLabelElements(%q, ...) diag[%d].Check = %q, want %q", tt.field, i, d.Check, wantCheck)
				}
				if d.Severity != string(tt.sev) {
					t.Errorf("DiagStateLabelElements(%q, ...) diag[%d].Severity = %q, want %q", tt.field, i, d.Severity, tt.sev)
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
		wantChecks    []string
		wantDiagCount int
	}{
		{
			name: "no overlap",
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
			name: "case-insensitive overlap – lowercase active, uppercase terminal",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"done"},
				TerminalStates: []string{"Done"},
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
			name: "case-insensitive overlap – active Done terminal done",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"Backlog", "Done"},
				TerminalStates: []string{"done", "Canceled"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 1,
		},
		{
			name: "case-insensitive overlap – active backlog terminal BACKLOG",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"BACKLOG"},
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
			name: "empty active slice – no overlap",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{},
				TerminalStates: []string{"Done"},
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
			name: "multiple overlaps are all reported and sorted",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"a", "b", "c"},
				TerminalStates: []string{"c", "b", "d"},
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
			name: "handoff_state and in_progress_state are not this hook's concern",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done"},
				HandoffState:    "done",
				InProgressState: "done",
			},
			wantDiagCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := registry.DiagStateOverlap(tt.fields)

			if len(got) != tt.wantDiagCount {
				t.Fatalf("DiagStateOverlap() = %d diags, want %d; diags: %v", len(got), tt.wantDiagCount, got)
			}
			for _, check := range tt.wantChecks {
				if !hasDiagCheck(got, check) {
					t.Errorf("DiagStateOverlap() missing diag with check %q; got: %v", check, got)
				}
			}
			for i, d := range got {
				if d.Severity != string(registry.SeverityWarning) {
					t.Errorf("DiagStateOverlap() diag[%d].Severity = %q, want %q", i, d.Severity, registry.SeverityWarning)
				}
				if strings.HasPrefix(d.Check, "tracker.handoff_state") {
					t.Errorf("DiagStateOverlap() diag[%d].Check = %q, must not begin with %q", i, d.Check, "tracker.handoff_state")
				}
				if strings.HasPrefix(d.Check, "tracker.in_progress_state") {
					t.Errorf("DiagStateOverlap() diag[%d].Check = %q, must not begin with %q", i, d.Check, "tracker.in_progress_state")
				}
			}
		})
	}
}
