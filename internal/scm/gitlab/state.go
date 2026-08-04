package gitlab

import (
	"log/slog"
	"strings"
)

// deriveState resolves the Sortie state from a GitLab issue's labels by
// scanning the configured active, terminal, and handoff states in config
// order. The first label match wins. When no state label is present, it
// falls back to the first configured state matching the native
// opened/closed status, or passes the native state through unchanged.
//
// The scan collects every matching state label before returning so an
// issue carrying more than one configured state label is observable:
// deriveState logs a WARN naming iid and the matched labels, then keeps
// the first. If log is nil, [slog.Default] is used. Label comparison is
// case-insensitive; activeStates, terminalStates, and handoffState are
// expected already lowercased.
func deriveState(labels []string, nativeState string, activeStates, terminalStates []string, handoffState, iid string, log *slog.Logger) string {
	if log == nil {
		log = slog.Default()
	}

	present := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		present[strings.ToLower(l)] = struct{}{}
	}

	matched := make([]string, 0, len(activeStates)+len(terminalStates)+1)
	for _, s := range activeStates {
		if _, ok := present[s]; ok {
			matched = append(matched, s)
		}
	}
	for _, s := range terminalStates {
		if _, ok := present[s]; ok {
			matched = append(matched, s)
		}
	}
	if handoffState != "" {
		if _, ok := present[handoffState]; ok {
			matched = append(matched, handoffState)
		}
	}

	if len(matched) > 1 {
		log.Warn("kept first of multiple matching state labels",
			slog.String("iid", iid),
			slog.Any("matched_labels", matched))
	}
	if len(matched) > 0 {
		return matched[0]
	}

	if nativeState == "opened" && len(activeStates) > 0 {
		return activeStates[0]
	}
	if nativeState == "closed" && len(terminalStates) > 0 {
		return terminalStates[0]
	}

	return nativeState
}

// findCurrentStateLabel returns the lowercased name of the first
// configured state label present on labels, scanning activeStates then
// terminalStates then handoffState in config order, or "" when the issue
// carries none. Unlike [deriveState] it reports only the first match and
// never falls back to the native state, because the transition flow uses
// it to locate the single state label being replaced.
func findCurrentStateLabel(labels []string, activeStates, terminalStates []string, handoffState string) string {
	present := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		present[strings.ToLower(l)] = struct{}{}
	}

	for _, s := range activeStates {
		if _, ok := present[s]; ok {
			return s
		}
	}
	for _, s := range terminalStates {
		if _, ok := present[s]; ok {
			return s
		}
	}
	if handoffState != "" {
		if _, ok := present[handoffState]; ok {
			return handoffState
		}
	}
	return ""
}

// labelVariants returns every entry of labels whose lowercased form
// equals lowered, in input order. It is how the transition removes a
// case-variant residue in the same request that applies the swap.
func labelVariants(labels []string, lowered string) []string {
	var out []string
	for _, l := range labels {
		if strings.ToLower(l) == lowered {
			out = append(out, l)
		}
	}
	return out
}
