package linear

import (
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
		name         string
		project      string
		wantCount    int
		wantCheck    string
		wantSeverity string
	}{
		{
			name:      "empty project is skipped",
			project:   "",
			wantCount: 0,
		},
		{
			name:         "internal whitespace is an error",
			project:      "E NG",
			wantCount:    1,
			wantCheck:    "tracker.project.format",
			wantSeverity: "error",
		},
		{
			name:         "leading space is an error",
			project:      " ENG",
			wantCount:    1,
			wantCheck:    "tracker.project.format",
			wantSeverity: "error",
		},
		{
			name:         "tab is an error",
			project:      "E\tNG",
			wantCount:    1,
			wantCheck:    "tracker.project.format",
			wantSeverity: "error",
		},
		{
			name:         "slash is a warning",
			project:      "org/repo",
			wantCount:    1,
			wantCheck:    "tracker.project.format",
			wantSeverity: "warning",
		},
		{
			name:      "clean uppercase team key",
			project:   "ENG",
			wantCount: 0,
		},
		{
			name:      "clean mixed-case team key",
			project:   "SOR",
			wantCount: 0,
		},
		{
			name:      "clean lowercase team key",
			project:   "eng",
			wantCount: 0,
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
			if got[0].Severity != tt.wantSeverity {
				t.Errorf("validateProject(%q) diag[0].Severity = %q, want %q", tt.project, got[0].Severity, tt.wantSeverity)
			}
		})
	}
}

func TestValidateAPIKeyHint(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv to control SORTIE_LINEAR_API_KEY.

	t.Run("non-empty lin_api_ prefixed key – no diagnostics", func(t *testing.T) {
		got := validateAPIKeyHint("lin_api_testtoken")

		if len(got) != 0 {
			t.Errorf("validateAPIKeyHint(non-empty lin_api_ key) = %v, want empty", got)
		}
	})

	t.Run("empty key with SORTIE_LINEAR_API_KEY set – hint warning", func(t *testing.T) {
		const envVal = "lin_api_fromenv"
		t.Setenv("SORTIE_LINEAR_API_KEY", envVal)

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_LINEAR_API_KEY set = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_linear_api_key_hint" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.sortie_linear_api_key_hint")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
		// Secret-leakage guard: the env var value must not appear in any message.
		if strings.Contains(got[0].Message, envVal) {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Message contains the env var value %q; messages must not echo secret values", envVal)
		}
	})

	t.Run("empty key with SORTIE_LINEAR_API_KEY unset – missing warning", func(t *testing.T) {
		t.Setenv("SORTIE_LINEAR_API_KEY", "")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_LINEAR_API_KEY unset = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_linear_api_key_missing" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.sortie_linear_api_key_missing")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
	})

	t.Run("non-empty key missing lin_api_ prefix – prefix warning", func(t *testing.T) {
		const keyVal = "notavalidkey"
		got := validateAPIKeyHint(keyVal)

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(%q) = %d diags, want 1; diags: %v", keyVal, len(got), got)
		}
		if got[0].Check != "tracker.api_key.linear_prefix" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Check = %q, want %q", keyVal, got[0].Check, "tracker.api_key.linear_prefix")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Severity = %q, want %q", keyVal, got[0].Severity, "warning")
		}
		// Secret-leakage guard: the key value must not appear in any message.
		if strings.Contains(got[0].Message, keyVal) {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Message contains the key value; messages must not echo secret values", keyVal)
		}
	})

	t.Run("non-empty key with trailing whitespace – whitespace warning", func(t *testing.T) {
		const keyVal = "lin_api_padded "
		got := validateAPIKeyHint(keyVal)

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(%q) = %d diags, want 1; diags: %v", keyVal, len(got), got)
		}
		if got[0].Check != "tracker.api_key.linear_whitespace" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Check = %q, want %q", keyVal, got[0].Check, "tracker.api_key.linear_whitespace")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Severity = %q, want %q", keyVal, got[0].Severity, "warning")
		}
		// Secret-leakage guard: the key value must not appear in any message.
		if strings.Contains(got[0].Message, strings.TrimSpace(keyVal)) {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Message contains the key value; messages must not echo secret values", keyVal)
		}
	})

	t.Run("whitespace-only key treated as empty – SORTIE_LINEAR_API_KEY set – hint warning", func(t *testing.T) {
		t.Setenv("SORTIE_LINEAR_API_KEY", "lin_api_fromenv")

		got := validateAPIKeyHint("   ")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(whitespace-only) with SORTIE_LINEAR_API_KEY set = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_linear_api_key_hint" {
			t.Errorf("validateAPIKeyHint(whitespace-only) diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.sortie_linear_api_key_hint")
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
		wantCheck string // suffix appended to field; empty means no diags expected
	}{
		{
			name:      "nil slice yields no warnings",
			field:     "tracker.active_states",
			states:    nil,
			wantCount: 0,
		},
		{
			name:      "all non-empty elements yield no warnings",
			field:     "tracker.active_states",
			states:    []string{"Backlog", "In Progress", "Done"},
			wantCount: 0,
		},
		{
			name:      "single empty element at index 0",
			field:     "tracker.active_states",
			states:    []string{""},
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "empty element at index 1",
			field:     "tracker.active_states",
			states:    []string{"Backlog", ""},
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "whitespace-only element maps to empty_element not untrimmed_element",
			field:     "tracker.active_states",
			states:    []string{"Backlog", "   ", "Done"},
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "multiple empty elements",
			field:     "tracker.active_states",
			states:    []string{"", "Backlog", ""},
			wantCount: 2,
			wantCheck: "empty_element",
		},
		// untrimmed_element: non-empty strings with leading or trailing whitespace.
		{
			name:      "trailing-space element yields untrimmed_element error",
			field:     "tracker.active_states",
			states:    []string{"Todo "},
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "leading-space element yields untrimmed_element error",
			field:     "tracker.active_states",
			states:    []string{" Todo"},
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "trailing-tab element yields untrimmed_element error",
			field:     "tracker.active_states",
			states:    []string{"Todo\t"},
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "clean element yields no diag",
			field:     "tracker.active_states",
			states:    []string{"Todo"},
			wantCount: 0,
		},
		// Exercise the terminal_states field name so both check-key paths are covered.
		{
			name:      "terminal_states nil slice yields no warnings",
			field:     "tracker.terminal_states",
			states:    nil,
			wantCount: 0,
		},
		{
			name:      "terminal_states single empty element",
			field:     "tracker.terminal_states",
			states:    []string{""},
			wantCount: 1,
			wantCheck: "empty_element",
		},
		{
			name:      "terminal_states trailing-space element yields untrimmed_element error",
			field:     "tracker.terminal_states",
			states:    []string{"Done "},
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
		{
			name:      "terminal_states leading-space element yields untrimmed_element error",
			field:     "tracker.terminal_states",
			states:    []string{" Done"},
			wantCount: 1,
			wantCheck: "untrimmed_element",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateStateLabels(tt.field, tt.states)

			if len(got) != tt.wantCount {
				t.Fatalf("validateStateLabels(%q, %v) = %d diags, want %d; diags: %v", tt.field, tt.states, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			wantCheck := tt.field + "." + tt.wantCheck
			for i, d := range got {
				if d.Check != wantCheck {
					t.Errorf("validateStateLabels(%q, ...) diag[%d].Check = %q, want %q", tt.field, i, d.Check, wantCheck)
				}
				if d.Severity != "error" {
					t.Errorf("validateStateLabels(%q, ...) diag[%d].Severity = %q, want %q", tt.field, i, d.Severity, "error")
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
		wantDiagCount int
		wantChecks    []string // check names that MUST appear in diags
	}{
		{
			name: "no overlap – no diagnostics",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"Backlog", "In Progress"},
				TerminalStates: []string{"Done", "Canceled"},
			},
			wantDiagCount: 0,
		},
		{
			name: "case-insensitive overlap – active Done terminal done",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"Backlog", "Done"},
				TerminalStates: []string{"done", "Canceled"},
			},
			wantDiagCount: 1,
			wantChecks:    []string{"tracker.states.overlap"},
		},
		{
			name: "case-insensitive overlap – active backlog terminal BACKLOG",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"BACKLOG"},
			},
			wantDiagCount: 1,
			wantChecks:    []string{"tracker.states.overlap"},
		},
		{
			name: "multiple overlaps are all reported and sorted",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"a", "b", "c"},
				TerminalStates: []string{"c", "b", "d"},
			},
			wantDiagCount: 2,
			wantChecks:    []string{"tracker.states.overlap"},
		},
		{
			name: "empty-string elements in both slices are skipped by overlap check",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{""},
				TerminalStates: []string{""},
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
			name: "handoff_state present in active_states – collision warning",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"review", "backlog"},
				TerminalStates: []string{"Done"},
				HandoffState:   "review",
			},
			wantDiagCount: 1,
			wantChecks:    []string{"tracker.handoff_state.collision"},
		},
		{
			name: "handoff_state present in terminal_states – collision warning",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"done", "canceled"},
				HandoffState:   "done",
			},
			wantDiagCount: 1,
			wantChecks:    []string{"tracker.handoff_state.collision"},
		},
		{
			name: "handoff_state case-insensitive match in active_states",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"Review"},
				TerminalStates: []string{"Done"},
				HandoffState:   "REVIEW",
			},
			wantDiagCount: 1,
			wantChecks:    []string{"tracker.handoff_state.collision"},
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
		// AC-7: InProgressState populated but must generate no in_progress_state diag.
		{
			name: "in_progress_state populated – no in_progress_state diagnostic emitted",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done"},
				InProgressState: "in progress",
			},
			wantDiagCount: 0,
		},
		{
			name: "in_progress_state in terminal_states – no in_progress_state diagnostic",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done", "in progress"},
				InProgressState: "in progress",
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
			// AC-7: no in_progress_state check may ever appear.
			for i, d := range got {
				if strings.HasPrefix(d.Check, "tracker.in_progress_state") {
					t.Errorf("validateStateOverlap() diag[%d].Check = %q begins with tracker.in_progress_state; Linear validator must not emit this check", i, d.Check)
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		fields        registry.TrackerConfigFields
		wantChecks    []string // check names that MUST appear
		wantErrCount  int      // expected error-severity diag count
		wantWarnCount int      // expected warning-severity diag count
	}{
		{
			// AC-5: valid lin_api_-prefixed key, whitespace-free slash-free
			// project, non-overlapping clean state lists. AC-7: InProgressState
			// is populated to prove the validator ignores it.
			name: "fully valid config with explicit state lists – zero diagnostics",
			fields: registry.TrackerConfigFields{
				Kind:            "linear",
				Project:         "ENG",
				APIKey:          "lin_api_testtoken123",
				ActiveStates:    []string{"Backlog", "In Progress"},
				TerminalStates:  []string{"Done", "Canceled"},
				InProgressState: "In Progress",
			},
			wantErrCount:  0,
			wantWarnCount: 0,
		},
		{
			// AC-5: nil state lists are valid because the constructor applies
			// defaultActiveStates / defaultTerminalStates.
			name: "valid config with nil state lists – zero diagnostics (defaults-apply path)",
			fields: registry.TrackerConfigFields{
				Kind:    "linear",
				Project: "SOR",
				APIKey:  "lin_api_anothertoken",
			},
			wantErrCount:  0,
			wantWarnCount: 0,
		},
		{
			// AC-3: whitespace in team key must produce one error on tracker.project.format.
			name: "whitespace project – error diagnostic",
			fields: registry.TrackerConfigFields{
				Kind:    "linear",
				Project: "E NG",
				APIKey:  "lin_api_testtoken123",
			},
			wantChecks:   []string{"tracker.project.format"},
			wantErrCount: 1,
		},
		{
			// AC-3: slash in team key must produce one warning on tracker.project.format.
			name: "slash project – warning diagnostic",
			fields: registry.TrackerConfigFields{
				Kind:    "linear",
				Project: "owner/repo",
				APIKey:  "lin_api_testtoken123",
			},
			wantChecks:    []string{"tracker.project.format"},
			wantWarnCount: 1,
		},
		{
			// AC-4: active ∩ terminal must produce tracker.states.overlap warning.
			name: "overlapping active and terminal states – overlap warning",
			fields: registry.TrackerConfigFields{
				Kind:           "linear",
				Project:        "ENG",
				APIKey:         "lin_api_testtoken123",
				ActiveStates:   []string{"Done"},
				TerminalStates: []string{"done", "Canceled"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantWarnCount: 1,
		},
		{
			// An empty element in a present active_states list aborts adapter
			// construction, so it must produce a tracker.active_states.empty_element
			// error rather than a warning.
			name: "present active list with empty element – empty_element error",
			fields: registry.TrackerConfigFields{
				Kind:           "linear",
				Project:        "ENG",
				APIKey:         "lin_api_testtoken123",
				ActiveStates:   []string{"Backlog", ""},
				TerminalStates: []string{"Done"},
			},
			wantChecks:   []string{"tracker.active_states.empty_element"},
			wantErrCount: 1,
		},
		{
			// An untrimmed element in a present active_states list cannot match
			// a team state by exact comparison, so it must produce a
			// tracker.active_states.untrimmed_element error. No in_progress_state
			// diagnostic may appear (Linear has no such config key).
			name: "present active list with untrimmed element – untrimmed_element error",
			fields: registry.TrackerConfigFields{
				Kind:           "linear",
				Project:        "ENG",
				APIKey:         "lin_api_testtoken123",
				ActiveStates:   []string{"Todo "},
				TerminalStates: []string{"Done"},
			},
			wantChecks:   []string{"tracker.active_states.untrimmed_element"},
			wantErrCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(tt.fields)

			errs := diagsWithSeverity(got, "error")
			warns := diagsWithSeverity(got, "warning")

			if len(errs) != tt.wantErrCount {
				t.Errorf("validateConfig(%q) errors = %v, want %d", tt.name, errs, tt.wantErrCount)
			}
			if len(warns) != tt.wantWarnCount {
				t.Errorf("validateConfig(%q) warnings = %v, want %d", tt.name, warns, tt.wantWarnCount)
			}
			for _, check := range tt.wantChecks {
				if !hasDiagCheck(got, check) {
					t.Errorf("validateConfig(%q) = %v, want check %q", tt.name, got, check)
				}
			}

			// AC-7: no in_progress_state check may ever appear.
			for i, d := range got {
				if strings.HasPrefix(d.Check, "tracker.in_progress_state") {
					t.Errorf("validateConfig(%q) diag[%d].Check = %q; Linear must not emit in_progress_state checks", tt.name, i, d.Check)
				}
			}
		})
	}
}

// TestValidateConfigEnvGated covers the AC-1 and AC-5 paths that depend on
// SORTIE_LINEAR_API_KEY. These subtests use t.Setenv so the parent must not
// call t.Parallel().
func TestValidateConfigEnvGated(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv.

	t.Run("empty api_key with SORTIE_LINEAR_API_KEY set – hint warning", func(t *testing.T) {
		t.Setenv("SORTIE_LINEAR_API_KEY", "lin_api_fromenv")

		fields := registry.TrackerConfigFields{
			Kind:    "linear",
			Project: "ENG",
			APIKey:  "",
		}

		got := validateConfig(fields)

		// AC-1: validateAPIKeyHint emits tracker.api_key.sortie_linear_api_key_hint.
		if !hasDiagCheck(got, "tracker.api_key.sortie_linear_api_key_hint") {
			t.Errorf("validateConfig(empty key, env set) = %v, want check tracker.api_key.sortie_linear_api_key_hint", got)
		}
	})

	t.Run("empty api_key with SORTIE_LINEAR_API_KEY unset – missing warning", func(t *testing.T) {
		t.Setenv("SORTIE_LINEAR_API_KEY", "")

		fields := registry.TrackerConfigFields{
			Kind:    "linear",
			Project: "ENG",
			APIKey:  "",
		}

		got := validateConfig(fields)

		// AC-1: validateAPIKeyHint emits tracker.api_key.sortie_linear_api_key_missing.
		if !hasDiagCheck(got, "tracker.api_key.sortie_linear_api_key_missing") {
			t.Errorf("validateConfig(empty key, env unset) = %v, want check tracker.api_key.sortie_linear_api_key_missing", got)
		}
	})
}
