package github

import "strings"

// extractState derives the Sortie state from GitHub issue labels by
// scanning configured active, terminal, and handoff states in config
// order. The first label match wins. When no state label is found,
// the function falls back to the first configured state matching the
// native open/closed status, or passes through the native state
// unchanged as a last resort. Pass an empty handoffState when handoff
// is not configured.
func extractState(labels []githubLabel, nativeState string, activeStates, terminalStates []string, handoffState string) string {
	lowerSet := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		lowerSet[strings.ToLower(l.Name)] = struct{}{}
	}

	for _, s := range activeStates {
		if _, ok := lowerSet[s]; ok {
			return s
		}
	}

	for _, s := range terminalStates {
		if _, ok := lowerSet[s]; ok {
			return s
		}
	}

	if handoffState != "" {
		if _, ok := lowerSet[handoffState]; ok {
			return handoffState
		}
	}

	if nativeState == "open" && len(activeStates) > 0 {
		return activeStates[0]
	}
	if nativeState == "closed" && len(terminalStates) > 0 {
		return terminalStates[0]
	}

	return nativeState
}

// isTerminalState returns true when the given lowercased state
// appears in terminalStates.
func isTerminalState(state string, terminalStates []string) bool {
	for _, s := range terminalStates {
		if s == state {
			return true
		}
	}
	return false
}

// isActiveState returns true when the given lowercased state appears
// in activeStates.
func isActiveState(state string, activeStates []string) bool {
	for _, s := range activeStates {
		if s == state {
			return true
		}
	}
	return false
}

// findCurrentStateLabel returns the first configured state (active,
// then terminal, then handoff — config order) whose label is present
// on the issue. Returns an empty string when no state label is found.
// Pass an empty handoffState when handoff is not configured.
func findCurrentStateLabel(labels []githubLabel, activeStates, terminalStates []string, handoffState string) string {
	lowerSet := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		lowerSet[strings.ToLower(l.Name)] = struct{}{}
	}

	for _, s := range activeStates {
		if _, ok := lowerSet[s]; ok {
			return s
		}
	}

	for _, s := range terminalStates {
		if _, ok := lowerSet[s]; ok {
			return s
		}
	}

	if handoffState != "" {
		if _, ok := lowerSet[handoffState]; ok {
			return handoffState
		}
	}

	return ""
}
