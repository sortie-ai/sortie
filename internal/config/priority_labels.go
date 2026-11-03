package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/issuekit"
)

func buildPriorityLabels(m map[string]any, tracker TrackerConfig) (map[string]int, error) {
	raw, present := m["priority_labels"]
	if !present {
		return nil, nil
	}
	switch tracker.Kind {
	case "gitea", "github", "gitlab":
	default:
		return nil, &ConfigError{Field: "tracker.priority_labels", Message: "only supported for gitea, github, and gitlab"}
	}
	if raw == nil {
		return nil, &ConfigError{Field: "tracker.priority_labels", Message: "expected mapping, got null"}
	}
	priorities, err := issuekit.ParsePriorityLabels(raw)
	if err != nil {
		return nil, &ConfigError{Field: "tracker.priority_labels", Message: strings.TrimPrefix(err.Error(), "priority_labels: ")}
	}
	states := append([]string{}, tracker.ActiveStates...)
	states = append(states, tracker.TerminalStates...)
	states = append(states, tracker.HandoffState, tracker.InProgressState, tracker.NoChangeState)
	for _, name := range slices.Sorted(maps.Keys(priorities)) {
		for _, state := range states {
			if name == strings.ToLower(state) {
				return nil, &ConfigError{Field: "tracker.priority_labels", Message: fmt.Sprintf("label %q is also a workflow state label", name)}
			}
		}
	}
	return priorities, nil
}
