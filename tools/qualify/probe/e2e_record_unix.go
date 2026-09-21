//go:build unix

package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"

	"github.com/sortie-ai/sortie/tools/qualify/e2e"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// endToEndTurnBound bounds the isolated end-to-end run's agent turn;
// the observation window is twice this.
const endToEndTurnBound = toolInductionTurnBound

// induceEndToEnd cancels and joins the run before group cleanliness is
// read, so a still-running worker is never mistaken for a leak.
func induceEndToEnd(t *testing.T, coords Coordinates, fixture *sharedFixture, identityName, identityVersion string) evidence.Record {
	t.Helper()

	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
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
	case <-time.After(procgroup.ShutdownDeadline):
	}

	pgids := harness.Agent().PGIDs()
	for _, pgid := range pgids {
		fixture.tracker.register(pgid)
	}
	groupClean := true
	for _, pgid := range pgids {
		present, presentErr := procgroup.Present(pgid)
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
