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

func TestValidateEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		endpoint  string
		wantCount int
	}{
		{"empty endpoint produces no diagnostic", "", 0},
		{"whitespace-only endpoint produces no diagnostic", "   ", 0},
		{"valid endpoint produces no diagnostic", "https://api.linear.app/graphql", 0},
		{"valid custom endpoint produces no diagnostic", "https://self-hosted.example.com/graphql", 0},
		{"unsupported scheme is one error diagnostic", "ftp://example.com/graphql", 1},
		{"non-url string is one error diagnostic", "not a url at all", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateEndpoint(tt.endpoint)

			if len(got) != tt.wantCount {
				t.Fatalf("validateEndpoint(%q) = %d diags, want %d; diags: %v", tt.endpoint, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			if got[0].Severity != "error" {
				t.Errorf("validateEndpoint(%q) diag[0].Severity = %q, want %q", tt.endpoint, got[0].Severity, "error")
			}
			if got[0].Check != "tracker.endpoint.invalid" {
				t.Errorf("validateEndpoint(%q) diag[0].Check = %q, want %q", tt.endpoint, got[0].Check, "tracker.endpoint.invalid")
			}
		})
	}
}

// TestValidateEndpoint_MessageNamesFieldOnly locks a stricter guarantee than
// TestValidateEndpoint: the diagnostic message for a malformed endpoint must
// not echo any fragment of the configured value, not even the redacted host.
// This differs from the constructor's error message, which does carry the
// redacted form.
func TestValidateEndpoint_MessageNamesFieldOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
	}{
		{"unsupported scheme", "ftp://example.com/graphql"},
		{"non-url string", "not a url at all"},
		{"credential-bearing malformed url", "https://operator:s3cr3t@fd00::1:3000/graphql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateEndpoint(tt.endpoint)

			if len(got) != 1 {
				t.Fatalf("validateEndpoint(%q) = %d diags, want 1; diags: %v", tt.endpoint, len(got), got)
			}
			if strings.Contains(got[0].Message, tt.endpoint) {
				t.Errorf("validateEndpoint(%q) message %q echoes the configured value", tt.endpoint, got[0].Message)
			}
			if strings.Contains(got[0].Message, "example.com") || strings.Contains(got[0].Message, "fd00::1:3000") {
				t.Errorf("validateEndpoint(%q) message %q echoes a host fragment of the configured value", tt.endpoint, got[0].Message)
			}
			if strings.Contains(got[0].Message, "operator") || strings.Contains(got[0].Message, "s3cr3t") {
				t.Errorf("validateEndpoint(%q) message %q leaks credentials", tt.endpoint, got[0].Message)
			}
		})
	}
}

// TestValidateConfig_ValidEndpointOmittedKeepsOtherDiagnostics proves that a
// fully valid config whose endpoint is absent produces exactly the
// diagnostics its other fields warrant: no endpoint diagnostic is added, and
// no existing diagnostic is suppressed by the endpoint check running first.
func TestValidateConfig_ValidEndpointOmittedKeepsOtherDiagnostics(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Kind:           "linear",
		Project:        "E NG",
		APIKey:         "lin_api_testtoken123",
		ActiveStates:   []string{"Backlog"},
		TerminalStates: []string{"Done"},
	}

	got := validateConfig(fields)

	if hasDiagCheck(got, "tracker.endpoint.invalid") {
		t.Errorf("validateConfig(endpoint omitted) = %v, must not include tracker.endpoint.invalid", got)
	}
	if !hasDiagCheck(got, "tracker.project.format") {
		t.Errorf("validateConfig(endpoint omitted) = %v, want check %q unaffected by the endpoint check", got, "tracker.project.format")
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
			// Valid lin_api_-prefixed key, whitespace-free slash-free
			// project, non-overlapping clean state lists. InProgressState
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
			// Nil state lists are valid because the constructor applies
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
			// Whitespace in team key must produce one error on tracker.project.format.
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
			// Slash in team key must produce one warning on tracker.project.format.
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
			// Active ∩ terminal must produce tracker.states.overlap warning.
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

			// No in_progress_state check may ever appear.
			for i, d := range got {
				if strings.HasPrefix(d.Check, "tracker.in_progress_state") {
					t.Errorf("validateConfig(%q) diag[%d].Check = %q; Linear must not emit in_progress_state checks", tt.name, i, d.Check)
				}
			}
		})
	}
}

// TestValidateConfigEnvGated covers the API-key paths that depend on
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

		// validateAPIKeyHint emits tracker.api_key.sortie_linear_api_key_hint.
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

		// validateAPIKeyHint emits tracker.api_key.sortie_linear_api_key_missing.
		if !hasDiagCheck(got, "tracker.api_key.sortie_linear_api_key_missing") {
			t.Errorf("validateConfig(empty key, env unset) = %v, want check tracker.api_key.sortie_linear_api_key_missing", got)
		}
	})
}
