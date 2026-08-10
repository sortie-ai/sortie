package jira

import (
	"errors"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks Jira-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls, and it never
// places the api_key value in a diagnostic message.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	endpoint := strings.TrimRight(fields.Endpoint, "/")
	host, hostOK := jiraHost(endpoint)
	version, versionErr := normalizeAPIVersion(fields.APIVersion)

	diags = append(diags, validateEndpoint(fields.Endpoint, endpoint)...)
	diags = append(diags, validateAPIVersion(versionErr, version, host)...)
	diags = append(diags, validateAPIKeyFormat(fields.APIKey, host, hostOK, versionErr, version)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}

// validateAPIVersion checks tracker.api_version against the same rule
// [normalizeAPIVersion] and [checkHostVersion] enforce at construction.
// versionErr and version are the results of normalizing
// tracker.api_version; host is the endpoint host [jiraHost] derived. An
// invalid version reports tracker.api_version.invalid and suppresses the
// Cloud-conflict check, since the constructor never reaches
// [checkHostVersion] for a version it rejects.
func validateAPIVersion(versionErr error, version, host string) []registry.ValidationDiag {
	if versionErr != nil {
		var trackerErr *domain.TrackerError
		if errors.As(versionErr, &trackerErr) {
			return []registry.ValidationDiag{{
				Severity: "error",
				Check:    "tracker.api_version.invalid",
				Message:  trackerErr.Message,
			}}
		}
		return nil
	}

	if version == "2" && isCloudHost(host) {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.api_version.cloud_conflict",
			Message:  cloudVersionConflict(host).Message,
		}}
	}

	return nil
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

	if _, ok := jiraHost(trimmed); !ok {
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
// character of the key can never form a valid user:secret pair, a
// colon-free key is only valid against a non-Cloud endpoint, since a
// Cloud endpoint forces api_version 3, and a colon-free key against a
// host that is known not to be Cloud is only valid on api_version 2,
// since api_version 3 requires an email:token key. host and hostOK are
// the results of [jiraHost] on the trailing-slash-trimmed endpoint;
// versionErr and version are the results of normalizing
// tracker.api_version. An endpoint whose host cannot be classified,
// meaning hostOK is false, is skipped for the last check, since
// [validateEndpoint] already reports that fault and the remedy would be
// wrong for an unclassified Cloud host.
func validateAPIKeyFormat(apiKey, host string, hostOK bool, versionErr error, version string) []registry.ValidationDiag {
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

	if isCloudHost(host) {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.api_key.jira_cloud_format",
			Message:  "tracker.api_key has no colon, but the endpoint is Jira Cloud, which requires an email:token api_key",
		}}
	}

	if hostOK && versionErr == nil && version == "3" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.api_key.jira_v3_format",
			Message: `tracker.api_key has no colon, but tracker.api_version resolves to "3", which requires an email:token api_key; ` +
				`set tracker.api_version "2" for a Jira Server or Data Center personal access token`,
		}}
	}

	return nil
}
