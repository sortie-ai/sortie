//go:build unix

package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/qualification"
	"github.com/sortie-ai/sortie/internal/qualification/e2e"
)

// endToEndTurnBound bounds the isolated end-to-end run's agent turn;
// the observation window is twice this.
const endToEndTurnBound = toolInductionTurnBound

// induceEndToEnd drives the isolated file-tracker workflow harness with the
// real protocol adapter. The run is cancelled and joined before group
// cleanliness is read, so a still-running worker cannot be mistaken for a
// leak.
func induceEndToEnd(t *testing.T, coords Coordinates, fixture *sharedFixture, identityName, identityVersion string) qualification.Record {
	t.Helper()

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return e2e.TerminalRecord(e2e.TerminalCondition{}, false, "", identityName, identityVersion)
	}
	fullCommand := append([]string{coords.CommandPath}, argv...)

	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		return e2e.TerminalRecord(e2e.TerminalCondition{}, false, "", identityName, identityVersion)
	}

	budgets := e2e.Budgets{
		ReadTimeoutMS:  30000,
		TurnTimeoutMS:  int(endToEndTurnBound / time.Millisecond),
		StallTimeoutMS: 60000,
		Observation:    2 * endToEndTurnBound,
	}
	harness := e2e.NewHarnessWithAgent(t, adapter, strings.Join(fullCommand, " "), "agent-client-protocol", budgets)

	cancel, done := e2e.StartWorkflow(t, harness)

	deadline := time.Now().Add(harness.Observation())
	condition := e2e.ObserveTerminalCondition(t, harness)
	for !condition.Reached() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		condition = e2e.ObserveTerminalCondition(t, harness)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(qualification.ShutdownDeadline):
	}

	pgids := harness.Agent().PGIDs()
	for _, pgid := range pgids {
		fixture.tracker.register(pgid)
	}
	groupClean := true
	for _, pgid := range pgids {
		present, presentErr := qualification.ProcessGroupPresent(pgid)
		if presentErr != nil || present {
			groupClean = false
		}
	}

	sessionID := ""
	if ids := harness.Agent().SessionIDs(); len(ids) > 0 {
		sessionID = ids[0]
	}

	return e2e.TerminalRecord(condition, groupClean, sessionID, identityName, identityVersion)
}
