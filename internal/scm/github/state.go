package github

import (
	"slices"
)

func isTerminalState(state string, terminalStates []string) bool {
	return slices.Contains(terminalStates, state)
}

func isActiveState(state string, activeStates []string) bool {
	return slices.Contains(activeStates, state)
}
