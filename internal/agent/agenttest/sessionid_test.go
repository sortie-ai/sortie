package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestAssertSessionIDContract_Passing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		heldSessionIDs []string
		events         []domain.AgentEvent
		result         domain.TurnResult
	}{
		{
			name:           "empty result",
			heldSessionIDs: []string{"S0"},
			result:         domain.TurnResult{},
		},
		{
			name:           "result equals the current identifier",
			heldSessionIDs: []string{"S0"},
			events: []domain.AgentEvent{
				{Type: domain.EventSessionStarted, SessionID: "S1"},
			},
			result: domain.TurnResult{SessionID: "S1"},
		},
		{
			name:           "result names a value absent from the history",
			heldSessionIDs: []string{"S0"},
			result:         domain.TurnResult{SessionID: "S2"},
		},
		{
			name:           "empty relayed session id contributes nothing to the history",
			heldSessionIDs: []string{"S0"},
			events: []domain.AgentEvent{
				{Type: domain.EventSessionStarted, SessionID: ""},
			},
			result: domain.TurnResult{SessionID: "S0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertSessionIDContract(reporter, tt.heldSessionIDs, tt.events, tt.result)

			if len(reporter.errors) != 0 {
				t.Errorf("assertSessionIDContract(%s) recorded failures %v, want none", tt.name, reporter.errors)
			}
		})
	}
}

func TestAssertSessionIDContract_Violating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		heldSessionIDs []string
		events         []domain.AgentEvent
		result         domain.TurnResult
	}{
		{
			name:           "superseded entry from the current turn's relayed event",
			heldSessionIDs: []string{"S0"},
			events: []domain.AgentEvent{
				{Type: domain.EventSessionStarted, SessionID: "S1"},
			},
			result: domain.TurnResult{SessionID: "S0"},
		},
		{
			name:           "superseded entry from an earlier turn",
			heldSessionIDs: []string{"S0", "S1"},
			result:         domain.TurnResult{SessionID: "S0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &fakeReporter{}
			assertSessionIDContract(reporter, tt.heldSessionIDs, tt.events, tt.result)

			if len(reporter.errors) != 1 {
				t.Errorf("assertSessionIDContract(%s) recorded %d failures for a superseded SessionID, want exactly 1: %v", tt.name, len(reporter.errors), reporter.errors)
			}
		})
	}
}

func TestAssertSessionIDContract_ExportedWrapper(t *testing.T) {
	t.Parallel()

	AssertSessionIDContract(t,
		[]string{"S0"},
		[]domain.AgentEvent{{Type: domain.EventSessionStarted, SessionID: "S1"}},
		domain.TurnResult{SessionID: "S1"},
	)
}
