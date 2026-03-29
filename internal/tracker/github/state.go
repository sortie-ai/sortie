package github

import "strings"

// extractState derives the Sortie state from GitHub issue labels by
// scanning configured active and terminal states in config order.
// The first label match wins. When no state label is found, the
// function falls back to the first configured state matching the
// native open/closed status, or passes through the native state
// unchanged as a last resort.
func extractState(labels []githubLabel, nativeState string, activeStates, terminalStates []string) string {
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

// findCurrentStateLabel returns the first issue label (lowercased)
// that matches any configured active or terminal state. Returns an
// empty string when no state label is found.
func findCurrentStateLabel(labels []githubLabel, activeStates, terminalStates []string) string {
	all := make(map[string]struct{}, len(activeStates)+len(terminalStates))
	for _, s := range activeStates {
		all[s] = struct{}{}
	}
	for _, s := range terminalStates {
		all[s] = struct{}{}
	}

	for _, l := range labels {
		lower := strings.ToLower(l.Name)
		if _, ok := all[lower]; ok {
			return lower
		}
	}
	return ""
}
