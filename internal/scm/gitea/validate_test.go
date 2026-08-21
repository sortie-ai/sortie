package gitea

import (
	"net/http/httptest"
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
			name:         "unbracketed ipv6 with port is invalid",
			endpoint:     "http://fd00::1:3000",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.invalid",
			wantSeverity: "error",
		},
		{
			name:         "unbracketed loopback is invalid",
			endpoint:     "http://::1/",
			wantCount:    1,
			wantCheck:    "tracker.endpoint.invalid",
			wantSeverity: "error",
		},
		{
			name:         "doubled port is invalid",
			endpoint:     "http://host:80:80/",
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

	t.Run("userinfo in a rejected endpoint never leaks into a message", func(t *testing.T) {
		t.Parallel()

		got := validateEndpoint("https://operator:s3cr3t@fd00::1:3000")

		if len(got) != 1 {
			t.Fatalf("validateEndpoint(userinfo, invalid host) = %d diags, want 1; diags: %v", len(got), got)
		}
		if got[0].Check != "tracker.endpoint.invalid" {
			t.Errorf("validateEndpoint(userinfo, invalid host) diag[0].Check = %q, want %q", got[0].Check, "tracker.endpoint.invalid")
		}
		assertNoMessageContains(t, got, "operator")
		assertNoMessageContains(t, got, "s3cr3t")
	})
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

// TestValidateQueryFilter proves the offline verdict cannot diverge from
// the construction verdict: a malformed tracker.query_filter fails both
// validateQueryFilter and NewGiteaAdapter with the same grammar, and a
// well-formed one passes both. The non-reserved-key case avoids the
// "labels" key so construction does not also need a labels-catalog route.
func TestValidateQueryFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantError bool
	}{
		{"empty is valid", "", false},
		{"non-reserved key is valid", "assignee=alice", false},
		{"reserved key state is rejected", "state=open", true},
		{"reserved key page is rejected", "page=2", true},
		{"unparseable query is rejected", "%zz", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags := validateQueryFilter(tt.raw)
			gotOffline := len(diags) != 0

			mux := newPreflightMux(t)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			config := validConfig(srv.URL)
			config["query_filter"] = tt.raw
			_, constructErr := NewGiteaAdapter(config)
			gotConstruct := constructErr != nil

			if gotOffline != tt.wantError {
				t.Errorf("validateQueryFilter(%q) produced diagnostics = %v, want %v", tt.raw, gotOffline, tt.wantError)
			}
			if gotConstruct != tt.wantError {
				t.Errorf("NewGiteaAdapter(query_filter=%q) error = %v, want error = %v", tt.raw, constructErr, tt.wantError)
			}
			if gotOffline != gotConstruct {
				t.Errorf("validateQueryFilter(%q) diverges from NewGiteaAdapter: offline error = %v, construction error = %v",
					tt.raw, gotOffline, gotConstruct)
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

	t.Run("untrimmed active state element is a warning, not an error", func(t *testing.T) {
		t.Setenv("SORTIE_GITEA_TOKEN", "")

		fields := registry.TrackerConfigFields{
			Kind:         "gitea",
			Project:      "owner/repo",
			Endpoint:     "https://gitea.example.com",
			APIKey:       "a1b2c3tokenvalue",
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
}

// TestValidateEndpointAgreesWithConstructor is a drift guard: validateEndpoint
// and NewGiteaAdapter both route through httpkit.ParseEndpoint, so the two
// verdicts can never disagree on whether an endpoint value is usable. srv
// carries the two construction-preflight routes so a case this table expects
// to be valid can also succeed all the way through NewGiteaAdapter; a case
// expected to be invalid never reaches the network, so sharing one server
// instance across every case is safe. t.Cleanup, not defer, closes srv so a
// parallel subtest cannot outlive it.
func TestValidateEndpointAgreesWithConstructor(t *testing.T) {
	srv := httptest.NewServer(newPreflightMux(t))
	t.Cleanup(srv.Close)

	tests := []struct {
		name     string
		endpoint func(base string) string
	}{
		{"empty", func(base string) string { return "" }},
		{"bare host with no scheme", func(base string) string { return "gitea.example.com" }},
		{"non-http scheme", func(base string) string { return "ftp://gitea.example.com" }},
		{"scheme with no host", func(base string) string { return "https://" }},
		{"unbracketed ipv6 with port", func(base string) string { return "http://fd00::1:3000" }},
		{"unbracketed loopback", func(base string) string { return "http://::1/" }},
		{"doubled port", func(base string) string { return "http://host:80:80/" }},
		{"userinfo credential leak", func(base string) string { return "https://operator:s3cr3t@fd00::1:3000" }},
		{"clean valid endpoint", func(base string) string { return base }},
		{"valid with trailing slash", func(base string) string { return base + "/" }},
		{"valid already suffixed with /api/v1", func(base string) string { return base + "/api/v1" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			endpoint := tt.endpoint(srv.URL)

			offlineRejected := len(diagsWithSeverity(validateEndpoint(endpoint), "error")) != 0

			_, constructErr := NewGiteaAdapter(validConfig(endpoint))
			constructRejected := constructErr != nil

			if offlineRejected != constructRejected {
				t.Errorf("validateEndpoint(%q) rejected = %v, NewGiteaAdapter(%q) rejected = %v; want agreement",
					endpoint, offlineRejected, endpoint, constructRejected)
			}
		})
	}
}
