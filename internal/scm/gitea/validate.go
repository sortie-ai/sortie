package gitea

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks Gitea-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateEndpoint(fields.Endpoint)...)
	diags = append(diags, validateProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	diags = append(diags, validateStateLabels("tracker.active_states", fields.ActiveStates)...)
	diags = append(diags, validateStateLabels("tracker.terminal_states", fields.TerminalStates)...)
	diags = append(diags, validateStateOverlap(fields)...)

	return diags
}

// containsWhitespace reports whether s contains any whitespace
// characters.
func containsWhitespace(s string) bool {
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}

// toLowerSet builds a set of lowercased strings from a slice.
func toLowerSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = struct{}{}
	}
	return m
}

// validateEndpoint checks that tracker.endpoint is present and shaped
// like an absolute http(s) URL with a host.
//
// A self-hosted Gitea instance has no default host, so an empty endpoint
// is a blocking error and a value that does not parse to an absolute
// http(s) URL with a host is a blocking error. A plain-http endpoint and
// an endpoint already ending in /api/v1 each yield an advisory warning.
// Messages never echo the raw value, which can embed userinfo credentials.
func validateEndpoint(endpoint string) []registry.ValidationDiag {
	if endpoint == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.missing",
			Message:  "tracker.endpoint is required for gitea; there is no default host",
		}}
	}

	parsed, err := url.Parse(endpoint)
	if err != nil ||
		(strings.ToLower(parsed.Scheme) != "http" && strings.ToLower(parsed.Scheme) != "https") ||
		parsed.Host == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.endpoint.invalid",
			Message:  `tracker.endpoint must be an absolute http(s) URL with a host (e.g. "https://gitea.example.com")`,
		}}
	}

	var diags []registry.ValidationDiag
	if strings.ToLower(parsed.Scheme) == "http" {
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

// validateProject checks that tracker.project is in owner/repo format.
// Empty project is skipped because the required-field check runs earlier
// in the preflight pipeline. Whitespace in either half is flagged because
// a Gitea owner or repository name cannot contain spaces and would fail
// the online preflight.
func validateProject(project string) []registry.ValidationDiag {
	if project == "" {
		return nil
	}

	if strings.Count(project, "/") != 1 {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be in owner/repo format (e.g. "sortie-ai/sortie")`,
		}}
	}

	owner, repo, _ := strings.Cut(project, "/")

	if owner == "" || repo == "" {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be in owner/repo format (e.g. "sortie-ai/sortie")`,
		}}
	}

	if containsWhitespace(owner) || containsWhitespace(repo) {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  "tracker.project owner and repo must not contain whitespace",
		}}
	}

	return nil
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

// validateStateLabels checks for empty or whitespace-only elements in a
// state label list.
//
// An empty element is a warning, not an error, because Gitea creates a
// missing state label on demand, so the element is inert rather than fatal
// at construction. An absent or wholly empty list yields no diagnostic
// because the adapter applies defaults in that case.
func validateStateLabels(field string, states []string) []registry.ValidationDiag {
	var diags []registry.ValidationDiag
	for i, s := range states {
		if strings.TrimSpace(s) == "" {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    field + ".empty_element",
				Message:  fmt.Sprintf("%s[%d]: empty state label will never match any issue", field, i),
			})
		}
	}
	return diags
}

// validateStateOverlap detects state names shared between active_states
// and terminal_states, and a handoff_state that collides with either
// list. All comparisons are case-insensitive. There is no
// in_progress_state arm because in_progress_state is not a Gitea config
// key.
func validateStateOverlap(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	activeSet := toLowerSet(fields.ActiveStates)
	terminalSet := toLowerSet(fields.TerminalStates)

	var overlap []string
	for name := range activeSet {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if _, ok := terminalSet[name]; ok {
			overlap = append(overlap, name)
		}
	}
	slices.Sort(overlap)
	for _, name := range overlap {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.states.overlap",
			Message:  fmt.Sprintf("tracker.active_states and tracker.terminal_states overlap on %q; an issue in state %q would match both sets", name, name),
		})
	}

	handoff := strings.TrimSpace(fields.HandoffState)
	if hs := strings.ToLower(handoff); hs != "" {
		if _, ok := activeSet[hs]; ok {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.handoff_state.collision",
				Message:  fmt.Sprintf("tracker.handoff_state %q must not appear in active_states (would cause immediate re-dispatch after handoff)", handoff),
			})
		}
		if _, ok := terminalSet[hs]; ok {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.handoff_state.collision",
				Message:  fmt.Sprintf("tracker.handoff_state %q must not appear in terminal_states (handoff is not terminal)", handoff),
			})
		}
	}

	return diags
}
