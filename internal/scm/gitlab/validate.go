package gitlab

import (
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks GitLab-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateEndpoint(fields.Endpoint)...)
	diags = append(diags, validateProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	// The untrimmed-element diagnostic is warning-severity because
	// [NewGitLabAdapter] lowercases each configured state without trimming
	// and construction proceeds, so a padded value never aborts startup;
	// it only ever fails to match a normalized issue label at dispatch time.
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)
	diags = append(diags, validateQueryFilter(fields.QueryFilter)...)

	return diags
}

// containsFold reports whether s contains substr, comparing
// case-insensitively.
func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// isAllASCIIDigits reports whether every byte of s is an ASCII digit.
// It returns false for an empty string, a signed value such as "+42",
// and any non-digit byte, and true for a digit string too long to fit
// an int.
func isAllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// validateEndpoint checks the shape of tracker.endpoint when present.
//
// GitLab defaults to the GitLab.com host, so an empty or whitespace-only
// endpoint yields no diagnostic; [NewGitLabAdapter] substitutes
// https://gitlab.com for that case. A present value that does not parse
// to an absolute http(s) URL with a host is a blocking error, matching
// the shape [NewGitLabAdapter] rejects before any network call. A plain
// http endpoint and a redundant /api/v4 suffix each yield an advisory
// warning. Messages never echo the raw value, which can embed userinfo
// credentials.
func validateEndpoint(endpoint string) []registry.ValidationDiag {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.invalid",
			Message:  `tracker.endpoint must be an absolute http(s) URL with a host (e.g. "https://gitlab.example.com")`,
		}}
	}

	var diags []registry.ValidationDiag
	if parsed.Scheme == "http" {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.endpoint.insecure",
			Message:  "tracker.endpoint uses http; the access token travels in cleartext in the PRIVATE-TOKEN header, use https",
		})
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/api/v4") {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.endpoint.api_suffix",
			Message:  "tracker.endpoint already ends in /api/v4; the adapter appends it automatically",
		})
	}
	return diags
}

// validateProject checks that tracker.project is a numeric project id or
// a namespace path.
//
// Empty project is skipped because the generic required-field check runs
// earlier in the preflight pipeline. GitLab subgroup paths nest to any
// depth, so this helper carries no exactly-one-slash rule, unlike the
// owner/repo grammar the GitHub and Gitea validators enforce.
func validateProject(project string) []registry.ValidationDiag {
	if project == "" {
		return nil
	}

	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project is whitespace only; set a numeric project id or a namespace path such as "group/project"`,
		}}
	}

	if isAllASCIIDigits(trimmed) {
		return nil
	}

	if typeutil.HasWhitespace(trimmed) {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  "tracker.project must not contain whitespace",
		}}
	}

	if containsFold(trimmed, "%2f") {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must not be percent-encoded; write "group/project" and let the adapter encode it`,
		}}
	}

	if !strings.Contains(trimmed, "/") {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be a namespace path such as "group/project" or "group/subgroup/project", or a numeric project id`,
		}}
	}

	if slices.Contains(strings.Split(trimmed, "/"), "") {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  "tracker.project must not contain an empty path segment or a leading or trailing slash",
		}}
	}

	return nil
}

// validateAPIKeyHint produces advisory diagnostics for the resolved
// tracker.api_key. An empty key hints about the SORTIE_GITLAB_TOKEN
// environment variable; a non-empty key surrounded by whitespace is
// flagged because the key is sent verbatim in the PRIVATE-TOKEN header.
// There is no token-prefix or token-length check, because the prefix is
// an administrator-writable application setting. The key value is never
// logged or placed in a diagnostic message.
func validateAPIKeyHint(apiKey string) []registry.ValidationDiag {
	trimmed := strings.TrimSpace(apiKey)

	if trimmed == "" {
		if os.Getenv("SORTIE_GITLAB_TOKEN") != "" {
			return []registry.ValidationDiag{{
				Severity: "warning",
				Check:    "tracker.api_key.sortie_gitlab_token_hint",
				Message:  "tracker.api_key is empty but SORTIE_GITLAB_TOKEN environment variable is set; consider using api_key: $SORTIE_GITLAB_TOKEN",
			}}
		}

		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.sortie_gitlab_token_missing",
			Message:  "tracker.api_key is empty and SORTIE_GITLAB_TOKEN environment variable is not set",
		}}
	}

	if apiKey != trimmed {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.gitlab_whitespace",
			Message:  "tracker.api_key has leading or trailing whitespace; the value is sent verbatim in the PRIVATE-TOKEN header and a server that does not strip it will reject the credential",
		}}
	}

	return nil
}

// validateQueryFilter checks the shape of tracker.query_filter by
// reusing [parseQueryFilter], the same grammar [NewGitLabAdapter] enforces
// at construction, so the offline verdict cannot drift from the
// construction verdict.
func validateQueryFilter(raw string) []registry.ValidationDiag {
	_, err := parseQueryFilter(raw)
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
