package issuekit

import (
	"log/slog"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// LabelStates is the configured state vocabulary a label-modelled tracker
// derives issue state from. Every entry is lowercased by the caller before
// construction.
type LabelStates struct {
	Active   []string
	Terminal []string
	Handoff  string
}

// DefaultActiveLabelStates returns the active-state list a label-modelled
// tracker applies when the workflow configures no list. Each call returns
// a copy so no caller can mutate a shared slice.
func DefaultActiveLabelStates() []string {
	return []string{"backlog", "in-progress", "review"}
}

// DefaultTerminalLabelStates returns the terminal-state list a
// label-modelled tracker applies when the workflow configures no list.
// Each call returns a copy so no caller can mutate a shared slice.
func DefaultTerminalLabelStates() []string {
	return []string{"done", "wontfix"}
}

// DeriveLabelState resolves issue state from the labels present on an
// issue, scanning states.Active, then states.Terminal, then
// states.Handoff, and returning the first configured state label found.
// When more than one configured state label is present, DeriveLabelState
// logs a WARN naming issueIdentifier and the matched labels, then keeps
// the first match.
//
// When no configured state label is present, DeriveLabelState falls back
// to states.Active[0] when nativeState equals openNative, to
// states.Terminal[0] when nativeState equals closedNative, and to
// nativeState unchanged otherwise. openNative and closedNative are the
// adapter's own spelling for the platform's open and closed status, so a
// platform reporting "opened" supplies its own spelling. If log is nil,
// [slog.Default] is used.
func DeriveLabelState(
	labels []string,
	nativeState, openNative, closedNative string,
	states LabelStates,
	issueIdentifier string,
	log *slog.Logger,
) string {
	if log == nil {
		log = slog.Default()
	}

	present := typeutil.LowerSet(labels)

	matched := make([]string, 0, len(states.Active)+len(states.Terminal)+1)
	for _, s := range states.Active {
		if _, ok := present[s]; ok {
			matched = append(matched, s)
		}
	}
	for _, s := range states.Terminal {
		if _, ok := present[s]; ok {
			matched = append(matched, s)
		}
	}
	if states.Handoff != "" {
		if _, ok := present[states.Handoff]; ok {
			matched = append(matched, states.Handoff)
		}
	}

	if len(matched) > 1 {
		log.Warn("kept first of multiple matching state labels",
			slog.String("issue_identifier", issueIdentifier),
			slog.Any("matched_labels", matched))
	}
	if len(matched) > 0 {
		return matched[0]
	}

	if nativeState == openNative && len(states.Active) > 0 {
		return states.Active[0]
	}
	if nativeState == closedNative && len(states.Terminal) > 0 {
		return states.Terminal[0]
	}

	return nativeState
}

// CurrentLabelState reports the first configured state label present on
// the issue, scanning states.Active, then states.Terminal, then
// states.Handoff, with no fallback to a native status. Empty when the
// issue carries none.
func CurrentLabelState(labels []string, states LabelStates) string {
	present := typeutil.LowerSet(labels)

	for _, s := range states.Active {
		if _, ok := present[s]; ok {
			return s
		}
	}
	for _, s := range states.Terminal {
		if _, ok := present[s]; ok {
			return s
		}
	}
	if states.Handoff != "" {
		if _, ok := present[states.Handoff]; ok {
			return states.Handoff
		}
	}

	return ""
}
