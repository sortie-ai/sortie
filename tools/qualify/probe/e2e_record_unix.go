//go:build unix

package probe

import (
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/tools/qualify/e2e"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// endToEndTurnBound bounds the isolated end-to-end run's agent turn;
// the observation window is twice this.
const endToEndTurnBound = toolInductionTurnBound

// protocolLaunchTimeouts are the read, turn, and stall timeouts every
// orchestrator-driven collector step launches its harness with.
func protocolLaunchTimeouts() (readTimeoutMS, turnTimeoutMS, stallTimeoutMS int) {
	return 30000, int(endToEndTurnBound / time.Millisecond), 60000
}

// buildProtocolLaunch resolves one profile's protocol entry point into the
// command string and adapter both orchestrator-driven collector steps launch
// from.
func buildProtocolLaunch(coords Coordinates) (string, domain.AgentAdapter, error) {
	argv, err := coords.Profile.EntryArgs(evidence.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return "", nil, err
	}
	fullCommand := append([]string{coords.CommandPath}, argv...)

	adapter, err := clientprotocol.NewClientProtocolAdapter()
	if err != nil {
		return "", nil, err
	}
	return strings.Join(fullCommand, " "), adapter, nil
}

// teardownRun cancels the workflow, joins it within the shutdown deadline,
// registers every process group the harness's adapter observer captured with
// fixture's tracker, and reports whether every group exited clean and the
// first actual protocol session id the observer saw.
func teardownRun(cancel func(), done <-chan struct{}, harness *e2e.Harness, fixture *sharedFixture) (groupClean bool, sessionID string) {
	cancel()
	select {
	case <-done:
	case <-time.After(procgroup.ShutdownDeadline):
	}

	pgids := harness.Agent().PGIDs()
	for _, pgid := range pgids {
		fixture.tracker.register(pgid)
	}
	groupClean = true
	for _, pgid := range pgids {
		present, presentErr := procgroup.Present(pgid)
		if presentErr != nil || present {
			groupClean = false
		}
	}

	if ids := harness.Agent().SessionIDs(); len(ids) > 0 {
		sessionID = ids[0]
	}
	return groupClean, sessionID
}

// induceEndToEnd cancels and joins the run before group cleanliness is
// read, so a still-running worker is never mistaken for a leak.
func induceEndToEnd(t *testing.T, coords Coordinates, fixture *sharedFixture, identityName, identityVersion string) evidence.Record {
	t.Helper()

	command, adapter, err := buildProtocolLaunch(coords)
	if err != nil {
		return e2e.TerminalRecord(e2e.TerminalCondition{}, false, "", identityName, identityVersion)
	}

	readTimeoutMS, turnTimeoutMS, stallTimeoutMS := protocolLaunchTimeouts()
	budgets := e2e.Budgets{
		ReadTimeoutMS:  readTimeoutMS,
		TurnTimeoutMS:  turnTimeoutMS,
		StallTimeoutMS: stallTimeoutMS,
		Observation:    2 * endToEndTurnBound,
	}
	harness := e2e.NewHarnessWithAgent(t, adapter, command, "agent-client-protocol", budgets)

	cancel, done := e2e.StartWorkflow(t, harness)

	deadline := time.Now().Add(harness.Observation())
	condition := e2e.ObserveTerminalCondition(t, harness)
	for !condition.Reached() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		condition = e2e.ObserveTerminalCondition(t, harness)
	}

	groupClean, sessionID := teardownRun(cancel, done, harness, fixture)
	return e2e.TerminalRecord(condition, groupClean, sessionID, identityName, identityVersion)
}

// induceCeilingStop drives one live run through the real orchestrator under a
// one-token ceiling and returns what the run found: a spend figure the
// protocol surface reports outside its own extension point, graded against
// whether the ceiling stopped the run with no later dispatch. A construction
// failure grades not_observed/runtime_failed; it never fails the test.
func induceCeilingStop(t *testing.T, coords Coordinates, fixture *sharedFixture) evidence.Observation {
	t.Helper()

	command, adapter, err := buildProtocolLaunch(coords)
	if err != nil {
		return evidence.Observation{
			Grade:   evidence.GradeNotObserved,
			Outcome: evidence.OutcomeRuntimeFailed,
			Detail:  evidence.BoundDetail(err.Error()),
		}
	}

	readTimeoutMS, turnTimeoutMS, stallTimeoutMS := protocolLaunchTimeouts()
	budgets := e2e.Budgets{
		ReadTimeoutMS:  readTimeoutMS,
		TurnTimeoutMS:  turnTimeoutMS,
		StallTimeoutMS: stallTimeoutMS,
		Observation:    3 * endToEndTurnBound,
	}
	prompt := coords.Profile.ProbePrompts[promptKeySuccess]
	harness := e2e.NewCeilingHarness(t, adapter, command, "agent-client-protocol", prompt, budgets)

	cancel, done := e2e.StartWorkflow(t, harness)
	condition := e2e.ObserveCeilingStop(t, harness)

	_, sessionID := teardownRun(cancel, done, harness, fixture)
	return e2e.CeilingStopObservation(condition, sessionID)
}
