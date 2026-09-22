package agenttest

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// AssertSessionIDContract fails t when result.SessionID names an
// identifier the session has already replaced.
//
// The replacement history is heldSessionIDs, the identifiers the session
// held before the turn, oldest first, followed by the SessionID of each
// [domain.EventSessionStarted] event in events, in delivery order, with
// empty values skipped; its last entry is the current identifier.
// AssertSessionIDContract passes when result.SessionID is empty, equals
// the current identifier, or names a value absent from the history, a
// replacement the turn result itself reports first.
func AssertSessionIDContract(t *testing.T, heldSessionIDs []string, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()
	assertSessionIDContract(t, heldSessionIDs, events, result)
}

func assertSessionIDContract(t contractReporter, heldSessionIDs []string, events []domain.AgentEvent, result domain.TurnResult) {
	t.Helper()

	var history []string
	for _, id := range heldSessionIDs {
		if id != "" {
			history = append(history, id)
		}
	}
	for _, event := range events {
		if event.Type == domain.EventSessionStarted && event.SessionID != "" {
			history = append(history, event.SessionID)
		}
	}

	if result.SessionID == "" || len(history) == 0 {
		return
	}
	current := history[len(history)-1]
	if result.SessionID == current {
		return
	}
	if slices.Contains(history[:len(history)-1], result.SessionID) {
		t.Errorf("result.SessionID = %q, want the current identifier %q or one absent from the history %v",
			result.SessionID, current, history)
	}
}
