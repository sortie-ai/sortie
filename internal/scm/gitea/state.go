package gitea

import (
	"log/slog"
	"strings"
)

// deriveState resolves the Sortie state from a Gitea issue's labels by scanning
// the configured active, terminal, and handoff states in config order. The
// first label match wins. When no state label is present, it falls back to the
// first configured state matching the native open/closed status, or passes the
// native state through unchanged.
//
// The scan collects every matching state label before returning so an issue
// carrying more than one configured state label is observable: deriveState logs
// a WARN naming issueIndex and the matched labels, then keeps the first. If log
// is nil, [slog.Default] is used. Label comparison is case-insensitive;
// activeStates, terminalStates, and handoffState are expected already lowercased.
func deriveState(labels []giteaLabel, nativeState string, activeStates, terminalStates []string, handoffState, issueIndex string, log *slog.Logger) string {
	if log == nil {
		log = slog.Default()
	}

	lowerSet := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		lowerSet[strings.ToLower(l.Name)] = struct{}{}
	}

	matched := make([]string, 0, len(activeStates)+len(terminalStates)+1)
	for _, s := range activeStates {
		if _, ok := lowerSet[s]; ok {
			matched = append(matched, s)
		}
	}
	for _, s := range terminalStates {
		if _, ok := lowerSet[s]; ok {
			matched = append(matched, s)
		}
	}
	if handoffState != "" {
		if _, ok := lowerSet[handoffState]; ok {
			matched = append(matched, handoffState)
		}
	}

	if len(matched) > 1 {
		log.Warn("kept first of multiple matching state labels",
			slog.String("issue_index", issueIndex),
			slog.Any("matched_labels", matched))
	}
	if len(matched) > 0 {
		return matched[0]
	}

	if nativeState == "open" && len(activeStates) > 0 {
		return activeStates[0]
	}
	if nativeState == "closed" && len(terminalStates) > 0 {
		return terminalStates[0]
	}

	return nativeState
}
