package linear

import (
	"os"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// validateConfig checks Linear-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	// Linear rejects an empty or padded configured state name at
	// construction (a present list element is matched against the team's
	// workflow states by exact case-insensitive name), so both faults are
	// errors here, unlike the sibling forge validators.
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityError)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityError)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}

// validateProject checks that tracker.project is a plausible Linear team
// key. Empty project is skipped because the required-field check runs
// earlier in the preflight pipeline. A team key carries no owner/repo
// grammar, so only whitespace and a slash are flagged; team-key
// existence and casing are left to the online preflight.
func validateProject(project string) []registry.ValidationDiag {
	if project == "" {
		return nil
	}

	if typeutil.HasWhitespace(project) {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be a Linear team key with no whitespace (e.g. "ENG")`,
		}}
	}

	if strings.Contains(project, "/") {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.project.format",
			Message:  `tracker.project is a Linear team key (e.g. "ENG"), not an owner/repo path; the "/" looks like a GitHub-style value`,
		}}
	}

	return nil
}

// validateAPIKeyHint produces advisory diagnostics for the resolved
// tracker.api_key. An empty key hints about the SORTIE_LINEAR_API_KEY
// environment variable; a non-empty key surrounded by whitespace is
// flagged because the key is sent verbatim in the Authorization header;
// a non-empty key lacking the lin_api_ prefix is flagged as likely
// wrong. The key value is never logged or placed in a diagnostic
// message.
func validateAPIKeyHint(apiKey string) []registry.ValidationDiag {
	trimmed := strings.TrimSpace(apiKey)

	if trimmed == "" {
		if os.Getenv("SORTIE_LINEAR_API_KEY") != "" {
			return []registry.ValidationDiag{{
				Severity: "warning",
				Check:    "tracker.api_key.sortie_linear_api_key_hint",
				Message:  "tracker.api_key is empty but SORTIE_LINEAR_API_KEY environment variable is set; consider using api_key: $SORTIE_LINEAR_API_KEY",
			}}
		}

		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.sortie_linear_api_key_missing",
			Message:  "tracker.api_key is empty and SORTIE_LINEAR_API_KEY environment variable is not set",
		}}
	}

	if apiKey != trimmed {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.linear_whitespace",
			Message:  "tracker.api_key has leading or trailing whitespace; the key is sent verbatim in the Authorization header, so surrounding whitespace will fail authentication",
		}}
	}

	if !strings.HasPrefix(trimmed, "lin_api_") {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.linear_prefix",
			Message:  `tracker.api_key does not start with "lin_api_"; Linear personal API keys carry that prefix`,
		}}
	}

	return nil
}
