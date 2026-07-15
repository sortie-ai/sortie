package gitea

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

// assertNoMessageContains fails the test if any diag's Message contains substr.
func assertNoMessageContains(t *testing.T, diags []registry.ValidationDiag, substr string) {
	t.Helper()
	for i, d := range diags {
		if strings.Contains(d.Message, substr) {
			t.Errorf("diag[%d].Message = %q, must not contain %q", i, d.Message, substr)
		}
	}
}

// --- Tests ---

func TestValidateEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		endpoint     string
		wantCount    int
		wantCheck    string
		wantSeverity string
	}{
		{
			name:         "empty endpoint is missing",
			endpoint:     "",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.missing",
			wantSeverity: "error",
		},
		{
			name:         "no scheme is invalid",
			endpoint:     "gitea.example.com",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.invalid",
			wantSeverity: "error",
		},
		{
			name:         "non-http scheme is invalid",
			endpoint:     "ftp://host",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.invalid",
			wantSeverity: "error",
		},
		{
			name:         "empty host is invalid",
			endpoint:     "https://",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.invalid",
			wantSeverity: "error",
		},
		{
			name:      "https with subpath is valid",
			endpoint:  "https://host/gitea",
			wantCount: 0,
		},
		{
			name:      "https with custom port is valid",
			endpoint:  "https://host:3000",
			wantCount: 0,
		},
		{
			name:      "https with IPv6 literal is valid",
			endpoint:  "https://[::1]:3000",
			wantCount: 0,
		},
		{
			name:      "clean https endpoint is valid",
			endpoint:  "https://gitea.example.com",
			wantCount: 0,
		},
		{
			name:         "http scheme is insecure",
			endpoint:     "http://host",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.insecure",
			wantSeverity: "warning",
		},
		{
			name:         "api/v1 suffix is redundant",
			endpoint:     "https://host/api/v1",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.api_suffix",
			wantSeverity: "warning",
		},
		{
			name:         "api/v1 suffix with trailing slash is redundant",
			endpoint:     "https://host/api/v1/",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.api_suffix",
			wantSeverity: "warning",
		},
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
			if got[0].Check != tt.wantCheck {
				t.Errorf("validateEndpoint(%q) diag[0].Check = %q, want %q", tt.endpoint, got[0].Check, tt.wantCheck)
			}
			if got[0].Severity != tt.wantSeverity {
				t.Errorf("validateEndpoint(%q) diag[0].Severity = %q, want %q", tt.endpoint, got[0].Severity, tt.wantSeverity)
			}
		})
	}

	t.Run("userinfo in endpoint never leaks into a message", func(t *testing.T) {
		t.Parallel()

		got := validateEndpoint("https://user:secret@host")

		if len(got) != 0 {
			t.Fatalf("validateEndpoint(userinfo) = %d diags, want 0; diags: %v", len(got), got)
		}
		assertNoMessageContains(t, got, "secret")
	})
}

func TestValidateProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		project   string
		wantCount int
	}{
		{name: "empty project is skipped", project: "", wantCount: 0},
		{name: "whitespace-only project is rejected", project: "  ", wantCount: 1},
		{name: "no slash", project: "noslash", wantCount: 1},
		{name: "multiple slashes", project: "a/b/c", wantCount: 1},
		{name: "empty owner segment", project: "/repo", wantCount: 1},
		{name: "empty repo segment", project: "owner/", wantCount: 1},
		{name: "space within owner segment", project: "my org/repo", wantCount: 1},
		{name: "space within repo segment", project: "owner/my repo", wantCount: 1},
		{name: "leading outer space in owner", project: " owner/repo", wantCount: 1},
		{name: "trailing outer space in repo", project: "owner/repo ", wantCount: 1},
		{name: "valid owner/repo", project: "sortie-ai/sortie", wantCount: 0},
		{name: "valid uppercase", project: "OWNER/REPO", wantCount: 0},
		{name: "valid with hyphens dots and digits", project: "my-org/my.repo-v2", wantCount: 0},
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
			if got[0].Check != "tracker.project.format" {
				t.Errorf("validateProject(%q) diag[0].Check = %q, want %q", tt.project, got[0].Check, "tracker.project.format")
			}
			if got[0].Severity != "error" {
				t.Errorf("validateProject(%q) diag[0].Severity = %q, want %q", tt.project, got[0].Severity, "error")
			}
		})
	}
}

func TestValidateAPIKeyHint(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv to control SORTIE_GITEA_TOKEN.

	t.Run("clean key produces no diagnostics", func(t *testing.T) {
		got := validateAPIKeyHint("a1b2c3")

		if len(got) != 0 {
			t.Errorf("validateAPIKeyHint(%q) = %v, want empty", "a1b2c3", got)
		}
	})

	t.Run("empty key with SORTIE_GITEA_TOKEN set reports hint warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "envTokenValue777")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_GITEA_TOKEN set = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_gitea_token_hint" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.sortie_gitea_token_hint")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
		assertNoMessageContains(t, got, "envTokenValue777")
	})

	t.Run("empty key with SORTIE_GITEA_TOKEN unset reports missing warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_GITEA_TOKEN unset = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_gitea_token_missing" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q", got[0].Check, "tracker.api_key.sortie_gitea_token_missing")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
	})

	t.Run("whitespace-padded key reports whitespace warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "")

		const padded = " zzzUniqueTokenValue999 "
		got := validateAPIKeyHint(padded)

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(%q) = %d diags, want 1; diags: %v", padded, len(got), got)
		}
		if got[0].Check != "tracker.api_key.gitea_whitespace" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Check = %q, want %q", padded, got[0].Check, "tracker.api_key.gitea_whitespace")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Severity = %q, want %q", padded, got[0].Severity, "warning")
		}
		assertNoMessageContains(t, got, "zzzUniqueTokenValue999")
	})

	t.Run("no check name references a token prefix", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "some-token-value")

		for _, apiKey := range []string{"", "a1b2c3", " tok "} {
			for _, d := range validateAPIKeyHint(apiKey) {
				if strings.Contains(d.Check, "prefix") {
					t.Errorf("validateAPIKeyHint(%q) diag.Check = %q, must not contain %q", apiKey, d.Check, "prefix")
				}
			}
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
	}{
		{name: "nil slice has no warnings", field: "tracker.active_states", states: nil, wantCount: 0},
		{name: "all non-empty has no warnings", field: "tracker.active_states", states: []string{"backlog", "in-progress"}, wantCount: 0},
		{name: "single empty at index 0", field: "tracker.active_states", states: []string{""}, wantCount: 1},
		{name: "empty at index 1", field: "tracker.terminal_states", states: []string{"done", ""}, wantCount: 1},
		{name: "whitespace-only element", field: "tracker.active_states", states: []string{"backlog", "  ", "done"}, wantCount: 1},
		{name: "multiple empties", field: "tracker.active_states", states: []string{"", "backlog", ""}, wantCount: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateStateLabels(tt.field, tt.states)

			if len(got) != tt.wantCount {
				t.Fatalf("validateStateLabels(%q, %v) = %d diags, want %d; diags: %v", tt.field, tt.states, len(got), tt.wantCount, got)
			}

			wantCheck := tt.field + ".empty_element"
			for i, d := range got {
				if d.Check != wantCheck {
					t.Errorf("validateStateLabels(%q) diag[%d].Check = %q, want %q", tt.field, i, d.Check, wantCheck)
				}
				if d.Severity != "warning" {
					t.Errorf("validateStateLabels(%q) diag[%d].Severity = %q, want %q", tt.field, i, d.Severity, "warning")
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
				ActiveStates:   []string{"done"},
				TerminalStates: []string{"Done"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 1,
		},
		{
			name: "multiple overlaps are sorted",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"a", "b"},
				TerminalStates: []string{"b", "c", "a"},
			},
			wantChecks:    []string{"tracker.states.overlap"},
			wantDiagCount: 2,
		},
		{
			name: "empty-string elements are skipped",
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
			name: "empty handoff_state is skipped",
			fields: registry.TrackerConfigFields{
				ActiveStates:   []string{"backlog"},
				TerminalStates: []string{"done"},
				HandoffState:   "",
			},
			wantDiagCount: 0,
		},
		{
			name: "in_progress_state is not a Gitea concern",
			fields: registry.TrackerConfigFields{
				ActiveStates:    []string{"backlog"},
				TerminalStates:  []string{"done", "closed"},
				InProgressState: "closed",
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
			for i, d := range got {
				if d.Severity != "warning" {
					t.Errorf("validateStateOverlap() diag[%d].Severity = %q, want %q", i, d.Severity, "warning")
				}
				if strings.HasPrefix(d.Check, "tracker.in_progress_state") {
					t.Errorf("validateStateOverlap() diag[%d].Check = %q, must not begin with %q", i, d.Check, "tracker.in_progress_state")
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv to control SORTIE_GITEA_TOKEN.

	t.Run("fully valid configuration produces zero diagnostics", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:            "gitea",
			Project:         "sortie-ai/sortie",
			Endpoint:        "https://gitea.example.com",
			APIKey:          "a1b2c3tokenvalue",
			ActiveStates:    []string{"backlog", "in-progress"},
			TerminalStates:  []string{"done", "wontfix"},
			InProgressState: "done",
		}

		got := validateConfig(fields)

		errs := diagsWithSeverity(got, "error")
		warnings := diagsWithSeverity(got, "warning")
		if len(errs) != 0 {
			t.Errorf("validateConfig(valid) errors = %v, want empty", errs)
		}
		if len(warnings) != 0 {
			t.Errorf("validateConfig(valid) warnings = %v, want empty", warnings)
		}
	})

	t.Run("nil state lists produce zero diagnostics", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:     "gitea",
			Project:  "owner/repo",
			Endpoint: "https://gitea.example.com",
			APIKey:   "a1b2c3tokenvalue",
		}

		got := validateConfig(fields)

		if len(got) != 0 {
			t.Errorf("validateConfig(nil state lists) = %v, want empty", got)
		}
	})
}
