package gitea

import (
	"errors"
	"os"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks Gitea-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateEndpoint(fields.Endpoint)...)
	diags = append(diags, registry.DiagOwnerRepoProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	// The untrimmed-element diagnostic is warning-severity because
	// [NewGiteaAdapter] lowercases each configured state without trimming
	// and construction proceeds, so a padded value never aborts startup;
	// it only ever fails to match a normalized issue label at dispatch time.
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)
	diags = append(diags, validateQueryFilter(fields.QueryFilter)...)

	return diags
}

// validateQueryFilter checks the shape of tracker.query_filter by reusing
// [parseGiteaQueryFilter], the same grammar [NewGiteaAdapter] enforces at
// construction, so the offline verdict cannot drift from the construction
// verdict.
func validateQueryFilter(raw string) []registry.ValidationDiag {
	_, err := parseGiteaQueryFilter(raw)
	if err == nil {
		return nil
	}

	message := err.Error()
	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
		message = trackerErr.Message
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "tracker.query_filter.invalid",
		Message:  message,
	}}
}

// validateEndpoint checks that tracker.endpoint is present and shaped
// like an absolute http(s) URL with a host.
//
// A self-hosted Gitea instance has no default host, so an empty endpoint
// is a blocking error and a value that does not parse to an absolute
// http(s) URL with a host is a blocking error. A plain-http endpoint and
// an endpoint already ending in /api/v1 each yield an advisory warning.
// The verdict comes from [httpkit.ParseEndpoint], the same helper the
// constructors use, so the offline verdict cannot drift from the
// construction verdict. Messages never echo the raw value, which can
// embed userinfo credentials.
func validateEndpoint(endpoint string) []registry.ValidationDiag {
	if endpoint == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.missing",
			Message:  "tracker.endpoint is required for gitea; there is no default host",
		}}
	}

	parsed, ok := httpkit.ParseEndpoint(endpoint)
	if !ok {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.invalid",
			Message:  `tracker.endpoint must be an absolute http(s) URL with a host (e.g. "https://gitea.example.com")`,
		}}
	}

	var diags []registry.ValidationDiag
	if parsed.Scheme == "http" {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.endpoint.insecure",
			Message:  "tracker.endpoint uses http; the token travels in cleartext, use https",
		})
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/api/v1") {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.endpoint.api_suffix",
			Message:  "tracker.endpoint already ends in /api/v1; the adapter appends it automatically",
		})
	}
	return diags
}

// validateAPIKeyHint produces advisory diagnostics for the resolved
// tracker.api_key. An empty key hints about the SORTIE_GITEA_TOKEN
// environment variable; a non-empty key surrounded by whitespace is
// flagged because the key is sent verbatim in the Authorization header.
// The key value is never logged or placed in a diagnostic message.
func validateAPIKeyHint(apiKey string) []registry.ValidationDiag {
	trimmed := strings.TrimSpace(apiKey)

	if trimmed == "" {
		if os.Getenv("SORTIE_GITEA_TOKEN") != "" {
			return []registry.ValidationDiag{{
				Severity: "warning",
				Check:    "tracker.api_key.sortie_gitea_token_hint",
				Message:  "tracker.api_key is empty but SORTIE_GITEA_TOKEN environment variable is set; consider using api_key: $SORTIE_GITEA_TOKEN",
			}}
		}

		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.sortie_gitea_token_missing",
			Message:  "tracker.api_key is empty and SORTIE_GITEA_TOKEN environment variable is not set",
		}}
	}

	if apiKey != trimmed {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.gitea_whitespace",
			Message:  "tracker.api_key has leading or trailing whitespace; the key is sent verbatim in the Authorization: token header, so surrounding whitespace will fail authentication",
		}}
	}

	return nil
}
