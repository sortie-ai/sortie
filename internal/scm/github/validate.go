package github

import (
	"os"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks GitHub-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, registry.DiagOwnerRepoProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	// The untrimmed-element diagnostic is warning-severity because
	// [NewGitHubAdapter] lowercases each configured state without trimming
	// and construction proceeds, so a padded value never aborts startup;
	// it only ever fails to match a normalized issue label at dispatch time.
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}

// validateAPIKeyHint produces advisory diagnostics when api_key is
// empty, hinting about the GITHUB_TOKEN environment variable.
func validateAPIKeyHint(apiKey string) []registry.ValidationDiag {
	if strings.TrimSpace(apiKey) != "" {
		return nil
	}

	if os.Getenv("GITHUB_TOKEN") != "" {
		return []registry.ValidationDiag{{
			Severity: "warning",
			Check:    "tracker.api_key.github_token_hint",
			Message:  "tracker.api_key is empty but GITHUB_TOKEN environment variable is set; consider using api_key: $GITHUB_TOKEN",
		}}
	}

	return []registry.ValidationDiag{{
		Severity: "warning",
		Check:    "tracker.api_key.github_token_missing",
		Message:  "tracker.api_key is empty and GITHUB_TOKEN environment variable is not set",
	}}
}
