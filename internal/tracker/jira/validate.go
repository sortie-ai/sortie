package jira

import (
	"net/url"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks Jira-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls, and it never
// places the api_key value in a diagnostic message.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	endpoint := strings.TrimRight(fields.Endpoint, "/")
	diags = append(diags, validateEndpoint(fields.Endpoint, endpoint)...)
	diags = append(diags, validateAPIKeyFormat(fields.APIKey, endpoint)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}

// validateEndpoint checks the shape of tracker.endpoint against the same
// rule [NewJiraAdapter] enforces at construction, in the same order: an
// empty endpoint, then the "/rest/api/" suffix on the trailing-slash-
// trimmed value, then the URL shape. trimmed is raw with its trailing "/"
// removed, matching the trim [NewJiraAdapter] applies before either later
// check.
func validateEndpoint(raw, trimmed string) []registry.ValidationDiag {
	if raw == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.missing",
			Message:  "tracker.endpoint is required for jira; there is no default host",
		}}
	}

	if strings.Contains(trimmed, "/rest/api/") {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.api_suffix",
			Message:  `tracker.endpoint must be the Jira base URL without "/rest/api/..." path`,
		}}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.invalid",
			Message:  "tracker.endpoint must be a URL with a scheme and host",
		}}
	}

	return nil
}

// validateAPIKeyFormat checks the shape of tracker.api_key against the
// same rules [resolveAuth] and [checkHostVersion] enforce at
// construction: a colon whose position is the first or the last
// character of the key can never form a valid user:secret pair, and a
// colon-free key is only valid against a non-Cloud endpoint, since a
// Cloud endpoint forces api_version 3, which requires a colon-separated
// key. endpoint is the trailing-slash-trimmed value [validateEndpoint]
// also checks; a value that fails to parse as a URL is skipped here,
// since [validateEndpoint] already reports that fault.
func validateAPIKeyFormat(apiKey, endpoint string) []registry.ValidationDiag {
	if apiKey == "" {
		return nil
	}

	idx := strings.Index(apiKey, ":")
	if idx >= 0 {
		if idx < 1 || idx == len(apiKey)-1 {
			return []registry.ValidationDiag{{
				Severity: "error",
				Check:    "tracker.api_key.jira_format",
				Message:  "tracker.api_key contains a colon at its first or last character, which can never form a valid user:secret pair",
			}}
		}
		return nil
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.HasSuffix(host, ".atlassian.net") {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.api_key.jira_cloud_format",
			Message:  "tracker.api_key has no colon, but the endpoint is Jira Cloud, which requires an email:token api_key",
		}}
	}

	return nil
}
