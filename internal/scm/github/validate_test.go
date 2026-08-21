package github

import (
	"strings"
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

// TestValidateEndpoint pins the offline verdict on tracker.endpoint. The
// unbracketed IPv6 rows are the shapes url.Parse rejects for carrying more
// than one colon in an unbracketed host, and are the reason this check
// exists: without it they pass sortie validate cleanly and fail later as a
// transport error.
func TestValidateEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		endpoint  string
		wantCount int
	}{
		{name: "empty endpoint is clean", endpoint: "", wantCount: 0},
		{name: "whitespace-only endpoint is clean", endpoint: "   ", wantCount: 0},
		{name: "public API host is valid", endpoint: "https://api.github.com", wantCount: 0},
		{name: "trailing slash is valid", endpoint: "https://api.github.com/", wantCount: 0},
		{name: "ghes api subpath is valid", endpoint: "https://github.example.com/api/v3", wantCount: 0},
		{name: "custom port is valid", endpoint: "https://github.example.com:8443", wantCount: 0},
		{name: "bracketed IPv6 literal is valid", endpoint: "https://[fd00::1]:3000", wantCount: 0},
		{name: "plain http is valid", endpoint: "http://github.example.com", wantCount: 0},
		{name: "unbracketed IPv6 literal with port is rejected", endpoint: "http://fd00::1:3000", wantCount: 1},
		{name: "unbracketed IPv6 loopback is rejected", endpoint: "http://::1/", wantCount: 1},
		{name: "doubled port is rejected", endpoint: "http://github.example.com:80:80/", wantCount: 1},
		{name: "host without scheme is rejected", endpoint: "github.example.com", wantCount: 1},
		{name: "unsupported scheme is rejected", endpoint: "ftp://github.example.com", wantCount: 1},
		{name: "scheme without host is rejected", endpoint: "https://", wantCount: 1},
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
			if got[0].Check != "tracker.endpoint.invalid" {
				t.Errorf("validateEndpoint(%q) diag[0].Check = %q, want %q", tt.endpoint, got[0].Check, "tracker.endpoint.invalid")
			}
			if got[0].Severity != "error" {
				t.Errorf("validateEndpoint(%q) diag[0].Severity = %q, want %q", tt.endpoint, got[0].Severity, "error")
			}
		})
	}

	t.Run("userinfo in endpoint never leaks into a message", func(t *testing.T) {
		t.Parallel()

		got := validateEndpoint("https://operator:s3cr3t@fd00::1:3000")

		if len(got) != 1 {
			t.Fatalf("validateEndpoint(userinfo) = %d diags, want 1; diags: %v", len(got), got)
		}
		for _, forbidden := range []string{"operator", "s3cr3t", "fd00::1:3000"} {
			if strings.Contains(got[0].Message, forbidden) {
				t.Errorf("validateEndpoint(userinfo) message = %q, must not contain %q", got[0].Message, forbidden)
			}
		}
	})
}

// TestValidateConfig_MalformedEndpoint pins the severity a malformed
// tracker.endpoint carries: error, because every GitHub constructor rejects
// the value outright rather than proceeding.
func TestValidateConfig_MalformedEndpoint(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Kind:     "github",
		Project:  "owner/repo",
		APIKey:   "tok",
		Endpoint: "http://fd00::1:3000",
	}

	got := validateConfig(fields)

	errs := diagsWithSeverity(got, "error")
	if len(errs) != 1 {
		t.Fatalf("validateConfig(malformed endpoint) errors = %v, want exactly one", errs)
	}
	if errs[0].Check != "tracker.endpoint.invalid" {
		t.Errorf("validateConfig(malformed endpoint) errors[0].Check = %q, want %q", errs[0].Check, "tracker.endpoint.invalid")
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
