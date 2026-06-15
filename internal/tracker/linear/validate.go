package linear

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks Linear-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter instance or make network calls.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	diags = append(diags, validateStateLabels("tracker.active_states", fields.ActiveStates)...)
	diags = append(diags, validateStateLabels("tracker.terminal_states", fields.TerminalStates)...)
	diags = append(diags, validateStateOverlap(fields)...)

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

	if containsWhitespace(project) {
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

// validateStateLabels checks for empty, whitespace-only, or untrimmed
// elements in a state name list. An absent or wholly empty list yields no
// diagnostic because the adapter applies defaults in that case.
//
// Both faults are errors, not warnings: a present list element is matched
// against the team's workflow states by exact case-insensitive name, so an
// empty name or one carrying leading or trailing whitespace can never match
// and aborts adapter construction before any issue is dispatched.
func validateStateLabels(field string, states []string) []registry.ValidationDiag {
	var diags []registry.ValidationDiag
	for i, s := range states {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    field + ".empty_element",
				Message:  fmt.Sprintf("%s[%d]: empty state name can never match a team state", field, i),
			})
			continue
		}
		if trimmed != s {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    field + ".untrimmed_element",
				Message:  fmt.Sprintf("%s[%d]: state name has leading or trailing whitespace and can never match a team state", field, i),
			})
		}
	}
	return diags
}

// validateStateOverlap detects state names shared between active_states
// and terminal_states, and a handoff_state that collides with either
// list. All comparisons are case-insensitive. There is no
// in_progress_state arm because in_progress_state is not a Linear config
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
