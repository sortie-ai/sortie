package gitlab

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
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
		{name: "empty endpoint is clean", endpoint: "", wantCount: 0},
		{name: "whitespace-only endpoint is clean", endpoint: "   ", wantCount: 0},
		{
			name:         "no scheme is invalid",
			endpoint:     "gitlab.example.com",
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
		{name: "https with subpath is valid", endpoint: "https://host/gitlab", wantCount: 0},
		{name: "https with custom port is valid", endpoint: "https://host:8443", wantCount: 0},
		{name: "https with IPv6 literal is valid", endpoint: "https://[::1]:8443", wantCount: 0},
		{name: "https with trailing slash is valid", endpoint: "https://host/", wantCount: 0},
		{
			name:         "http scheme is insecure",
			endpoint:     "http://gitlab.example.com",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.insecure",
			wantSeverity: "warning",
		},
		{
			name:         "api/v4 suffix is redundant",
			endpoint:     "https://gitlab.example.com/api/v4",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.api_suffix",
			wantSeverity: "warning",
		},
		{
			name:         "api/v4 suffix with trailing slash is redundant",
			endpoint:     "https://gitlab.example.com/api/v4/",
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

		assertNoMessageContains(t, got, "secret")
		assertNoMessageContains(t, got, "user:secret@host")
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
		{name: "whitespace-only project is rejected", project: "   ", wantCount: 1},
		{name: "whitespace within value is rejected", project: "my project", wantCount: 1},
		{name: "pre-encoded slash lowercase is rejected", project: "group%2fproject", wantCount: 1},
		{name: "pre-encoded slash uppercase is rejected", project: "group%2Fproject", wantCount: 1},
		{name: "single segment non-numeric is rejected", project: "project", wantCount: 1},
		{name: "leading slash empty segment is rejected", project: "/group/project", wantCount: 1},
		{name: "trailing slash empty segment is rejected", project: "group/project/", wantCount: 1},
		{name: "doubled slash empty segment is rejected", project: "group//project", wantCount: 1},
		{name: "namespace path is valid", project: "group/project", wantCount: 0},
		{name: "nested subgroup path is valid", project: "group/subgroup/project", wantCount: 0},
		{name: "numeric project id is valid", project: "42", wantCount: 0},
		{name: "padded numeric project id is valid", project: " 42 ", wantCount: 0},
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
	// No t.Parallel(): subtests use t.Setenv to control SORTIE_GITLAB_TOKEN.

	t.Run("clean key produces no diagnostics", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		got := validateAPIKeyHint("glpat-a1b2c3")

		if len(got) != 0 {
			t.Errorf("validateAPIKeyHint(%q) = %v, want empty", "glpat-a1b2c3", got)
		}
	})

	t.Run("empty key with SORTIE_GITLAB_TOKEN set reports hint warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "envTokenValue777")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_GITLAB_TOKEN set = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_gitlab_token_hint" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q",
				got[0].Check, "tracker.api_key.sortie_gitlab_token_hint")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
		assertNoMessageContains(t, got, "envTokenValue777")
	})

	t.Run("empty key with SORTIE_GITLAB_TOKEN unset reports missing warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		got := validateAPIKeyHint("")

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(\"\") with SORTIE_GITLAB_TOKEN unset = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.api_key.sortie_gitlab_token_missing" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Check = %q, want %q",
				got[0].Check, "tracker.api_key.sortie_gitlab_token_missing")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(\"\") diag[0].Severity = %q, want %q", got[0].Severity, "warning")
		}
	})

	t.Run("whitespace-padded key reports whitespace warning", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		const padded = " zzzUniqueTokenValue999 "
		got := validateAPIKeyHint(padded)

		if len(got) != 1 {
			t.Fatalf("validateAPIKeyHint(%q) = %d diags, want 1; diags: %v", padded, len(got), got)
		}
		if got[0].Check != "tracker.api_key.gitlab_whitespace" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Check = %q, want %q", padded, got[0].Check, "tracker.api_key.gitlab_whitespace")
		}
		if got[0].Severity != "warning" {
			t.Errorf("validateAPIKeyHint(%q) diag[0].Severity = %q, want %q", padded, got[0].Severity, "warning")
		}
		assertNoMessageContains(t, got, "zzzUniqueTokenValue999")
	})

	t.Run("no check name references a token prefix or length", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "glpat-some-token-value")

		for _, apiKey := range []string{"", "glpat-a1b2c3", " glpat-tok "} {
			for _, d := range validateAPIKeyHint(apiKey) {
				if strings.Contains(d.Check, "prefix") {
					t.Errorf("validateAPIKeyHint(%q) diag.Check = %q, must not contain %q", apiKey, d.Check, "prefix")
				}
				if strings.Contains(d.Check, "length") {
					t.Errorf("validateAPIKeyHint(%q) diag.Check = %q, must not contain %q", apiKey, d.Check, "length")
				}
			}
		}
	})
}

func TestValidateQueryFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantCount int
	}{
		{name: "unset filter is clean", raw: "", wantCount: 0},
		{name: "whitespace-only filter is clean", raw: "   ", wantCount: 0},
		{name: "valid filter is clean", raw: "scope=assigned_to_me&not[labels]=needs-triage", wantCount: 0},
		{name: "reserved key is rejected", raw: "state=closed", wantCount: 1},
		{name: "unknown key is rejected", raw: "labelz=backlog", wantCount: 1},
		{name: "empty comma segment is rejected", raw: "labels=bug,", wantCount: 1},
		{name: "repeated non-array key is rejected", raw: "scope=all&scope=assigned_to_me", wantCount: 1},
		{name: "colliding spellings are rejected", raw: "labels=a&labels[]=b", wantCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateQueryFilter(tt.raw)

			if len(got) != tt.wantCount {
				t.Fatalf("validateQueryFilter(%q) = %d diags, want %d; diags: %v", tt.raw, len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}

			if got[0].Check != "tracker.query_filter.invalid" {
				t.Errorf("validateQueryFilter(%q) diag[0].Check = %q, want %q",
					tt.raw, got[0].Check, "tracker.query_filter.invalid")
			}
			if got[0].Severity != "error" {
				t.Errorf("validateQueryFilter(%q) diag[0].Severity = %q, want %q", tt.raw, got[0].Severity, "error")
			}

			// The diagnostic message must equal, byte for byte, the
			// *domain.TrackerError.Message parseQueryFilter returns for the
			// same input, never err.Error(), so the tracker: wrapper and any
			// wrapped url.ParseQuery text stay out of operator output.
			_, err := parseQueryFilter(tt.raw)
			if err == nil {
				t.Fatalf("parseQueryFilter(%q) unexpected nil error", tt.raw)
			}
			var trackerErr *domain.TrackerError
			if !errors.As(err, &trackerErr) {
				t.Fatalf("parseQueryFilter(%q) error type = %T, want *domain.TrackerError", tt.raw, err)
			}
			if got[0].Message != trackerErr.Message {
				t.Errorf("validateQueryFilter(%q) diag[0].Message = %q, want %q (parseQueryFilter Message, verbatim)",
					tt.raw, got[0].Message, trackerErr.Message)
			}
			if !strings.HasPrefix(got[0].Message, "gitlab: ") {
				t.Errorf("validateQueryFilter(%q) diag[0].Message = %q, want prefix %q", tt.raw, got[0].Message, "gitlab: ")
			}
			if strings.Contains(got[0].Message, "tracker: ") {
				t.Errorf("validateQueryFilter(%q) diag[0].Message = %q, must not contain the TrackerError.Error wrapper %q",
					tt.raw, got[0].Message, "tracker: ")
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	// No t.Parallel(): subtests use t.Setenv to control SORTIE_GITLAB_TOKEN.

	t.Run("fully valid configuration produces zero diagnostics", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:           "gitlab",
			Project:        "group/project",
			Endpoint:       "https://gitlab.example.com",
			APIKey:         "glpat-a1b2c3tokenvalue",
			ActiveStates:   []string{"backlog", "in-progress"},
			TerminalStates: []string{"done", "wontfix"},
			QueryFilter:    "scope=assigned_to_me",
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

	t.Run("nil endpoint, one state list, and no query filter produce zero diagnostics", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:         "gitlab",
			Project:      "42",
			APIKey:       "glpat-a1b2c3tokenvalue",
			ActiveStates: []string{"backlog", "in-progress"},
		}

		got := validateConfig(fields)

		if len(got) != 0 {
			t.Errorf("validateConfig(minimal valid) = %v, want empty", got)
		}
	})

	t.Run("both state lists empty produces no state-element or overlap diagnostic", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "glpat-clean-token-value")

		// Asserted at the unit level only: orchestrator.ValidateConfigForPromotion
		// rejects a workflow whose active_states and terminal_states are both
		// empty before sortie validate ever reaches this hook, so no end-to-end
		// command-level assertion exists for this input.
		fields := registry.TrackerConfigFields{
			Kind:    "gitlab",
			Project: "group/project",
			APIKey:  "glpat-clean-token-value",
		}

		got := validateConfig(fields)

		for _, d := range got {
			if strings.Contains(d.Check, "active_states") || strings.Contains(d.Check, "terminal_states") ||
				d.Check == "tracker.states.overlap" {
				t.Errorf("validateConfig(both state lists empty) unexpected diag %v", d)
			}
		}
	})

	t.Run("untrimmed active state element is a warning, not an error", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:         "gitlab",
			Project:      "group/project",
			APIKey:       "glpat-a1b2c3tokenvalue",
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
	})

	t.Run("opens no socket even when the endpoint targets a live listener", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatal("validateConfig must not issue any HTTP request")
		}))
		defer srv.Close()

		fields := registry.TrackerConfigFields{
			Kind:           "gitlab",
			Project:        "not a valid project",
			Endpoint:       srv.URL,
			APIKey:         "",
			ActiveStates:   []string{"done"},
			TerminalStates: []string{"Done"},
			QueryFilter:    "state=closed",
		}

		got := validateConfig(fields)

		if len(got) == 0 {
			t.Fatal("validateConfig(multi-fault input) = empty, want at least one diagnostic")
		}
	})

	t.Run("deterministic order across repeated calls", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:           "gitlab",
			Project:        "my project",
			Endpoint:       "ftp://host",
			APIKey:         " padded-key ",
			ActiveStates:   []string{"", "done"},
			TerminalStates: []string{"Done"},
			QueryFilter:    "state=closed",
		}

		first := validateConfig(fields)
		second := validateConfig(fields)

		wantChecks := []string{
			"tracker.endpoint.invalid",
			"tracker.project.format",
			"tracker.api_key.gitlab_whitespace",
			"tracker.active_states.empty_element",
			"tracker.states.overlap",
			"tracker.query_filter.invalid",
		}

		if len(first) != len(wantChecks) {
			t.Fatalf("validateConfig() = %d diags, want %d; diags: %v", len(first), len(wantChecks), first)
		}
		for i, want := range wantChecks {
			if first[i].Check != want {
				t.Errorf("validateConfig() diag[%d].Check = %q, want %q", i, first[i].Check, want)
			}
		}
		if len(second) != len(first) {
			t.Fatalf("validateConfig() second call = %d diags, want %d (same as first call)", len(second), len(first))
		}
		for i := range first {
			if second[i] != first[i] {
				t.Errorf("validateConfig() second call diag[%d] = %v, want %v (identical to first call)", i, second[i], first[i])
			}
		}
	})

	t.Run("secret values never appear in any message", func(t *testing.T) {
		t.Setenv("SORTIE_GITLAB_TOKEN", "")

		const key = "supersecrettokenvalue"
		fields := registry.TrackerConfigFields{
			Kind:     "gitlab",
			Project:  "group/project",
			Endpoint: "https://user:supersecrettokenvalue@host",
			APIKey:   " " + key + " ",
		}

		got := validateConfig(fields)

		assertNoMessageContains(t, got, key)
	})
}
