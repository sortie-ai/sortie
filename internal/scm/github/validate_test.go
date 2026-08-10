package github

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// --- Test helpers ---

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

// TestValidateConfig_UntrimmedStateElement pins the severity an untrimmed
// tracker.active_states element carries: warning, because NewGitHubAdapter
// lowercases each configured state without trimming and construction
// proceeds regardless.
func TestValidateConfig_UntrimmedStateElement(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Kind:         "github",
		Project:      "owner/repo",
		APIKey:       "tok",
		ActiveStates: []string{" backlog"},
	}

	got := validateConfig(fields)

	warnings := diagsWithSeverity(got, "warning")
	var found bool
	for _, d := range warnings {
		if d.Check == "tracker.active_states.untrimmed_element" {
			found = true
		}
	}
	if !found {
		t.Errorf("validateConfig(untrimmed active state) warnings = %v, want tracker.active_states.untrimmed_element", warnings)
	}
	if errs := diagsWithSeverity(got, "error"); len(errs) != 0 {
		t.Errorf("validateConfig(untrimmed active state) errors = %v, want empty (construction proceeds)", errs)
	}
}
