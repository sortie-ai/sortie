package github

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks GitHub-specific configuration constraints and
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

// validateProject checks that tracker.project is in owner/repo
// format. Empty project is skipped (generic preflight Check 4
// handles the required-field case). Validation uses the raw value
// without trimming so behavior matches [NewGitHubAdapter].
func validateProject(project string) []registry.ValidationDiag {
	if strings.TrimSpace(project) == "" {
		return nil
	}

	if strings.Count(project, "/") != 1 {
		return []registry.ValidationDiag{{
			Severity: "error",
			Check:    "tracker.project.format",
			Message:  `tracker.project must be in owner/repo format (e.g. "sortie-ai/sortie")`,
		}}
	}

	parts := strings.SplitN(project, "/", 2)
	owner := parts[0]
	repo := parts[1]

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

// validateStateLabels checks for empty or whitespace-only elements in
// a state label list.
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

// validateStateOverlap detects collisions between active_states,
// terminal_states, handoff_state, and in_progress_state.
func validateStateOverlap(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	activeSet := toLowerSet(fields.ActiveStates)
	terminalSet := toLowerSet(fields.TerminalStates)

	// 5a: active_states ∩ terminal_states.
	var overlap []string
	for label := range activeSet {
		if strings.TrimSpace(label) == "" {
			continue
		}
		if _, ok := terminalSet[label]; ok {
			overlap = append(overlap, label)
		}
	}
	sort.Strings(overlap)
	for _, label := range overlap {
		diags = append(diags, registry.ValidationDiag{
			Severity: "warning",
			Check:    "tracker.states.overlap",
			Message:  fmt.Sprintf("tracker.active_states and tracker.terminal_states overlap on %q; an issue in state %q would match both sets", label, label),
		})
	}

	// 5b: handoff_state collisions.
	if hs := strings.ToLower(strings.TrimSpace(fields.HandoffState)); hs != "" {
		if _, ok := activeSet[hs]; ok {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.handoff_state.collision",
				Message:  fmt.Sprintf("tracker.handoff_state %q must not appear in active_states (would cause immediate re-dispatch after handoff)", fields.HandoffState),
			})
		}
		if _, ok := terminalSet[hs]; ok {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.handoff_state.collision",
				Message:  fmt.Sprintf("tracker.handoff_state %q must not appear in terminal_states (handoff is not terminal)", fields.HandoffState),
			})
		}
	}

	// 5c: in_progress_state collisions.
	if ips := strings.ToLower(strings.TrimSpace(fields.InProgressState)); ips != "" {
		if _, ok := terminalSet[ips]; ok {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.in_progress_state.collision",
				Message:  fmt.Sprintf("tracker.in_progress_state %q must not appear in terminal_states", fields.InProgressState),
			})
		}
		hs := strings.ToLower(strings.TrimSpace(fields.HandoffState))
		if hs != "" && ips == hs {
			diags = append(diags, registry.ValidationDiag{
				Severity: "warning",
				Check:    "tracker.in_progress_state.collision",
				Message:  fmt.Sprintf("tracker.in_progress_state must not collide with tracker.handoff_state (%q)", fields.HandoffState),
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
