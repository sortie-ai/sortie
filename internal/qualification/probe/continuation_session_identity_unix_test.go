//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

func publishedContinuationRows(t *testing.T, surface qualification.Surface, seed, recall qualification.Observation) (seedRow, recallRow qualification.Record) {
	t.Helper()

	fixture := qualification.NewFixture(qualification.FixtureQualified)
	if err := fixture.SetSessionContinuationObserved(surface, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(%s, %+v, %+v) error = %v, want nil", surface, seed, recall, err)
	}
	fixture.Finalize()

	path := qualification.WriteEvidenceFile(t, fixture.Records)
	if _, err := qualification.ValidateObservations(path); err != nil {
		t.Fatalf("ValidateObservations(%s) = %v, want nil", path, err)
	}
	published, err := qualification.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("ReadEvidenceFile(%s) = _, %v, want nil", path, err)
	}

	var foundSeed, foundRecall bool
	for _, rec := range published {
		if rec.Scenario != qualification.ScenarioContinuation || rec.Surface != surface {
			continue
		}
		switch rec.InputID {
		case qualification.InputContinuationSeed:
			seedRow, foundSeed = rec, true
		case qualification.InputContinuationRecall:
			recallRow, foundRecall = rec, true
		}
	}
	if !foundSeed || !foundRecall {
		t.Fatalf("the published evidence carries no continuation seed and recall pair for surface %s", surface)
	}
	return seedRow, recallRow
}

func TestPublishedSeedRowNamesNoSessionWhenTheEntryPointNeverResolved(t *testing.T) {
	t.Parallel()

	coords := continuationCoordinates("/nonexistent/runtime")
	seed, recall := induceNativeContinuation(t, coords, semanticFixture(t), qualification.SurfaceNativeJSON)

	seedRow, _ := publishedContinuationRows(t, qualification.SurfaceNativeJSON, seed, recall)
	if seedRow.SessionID != nil {
		t.Errorf("published seed row session_id = %q, want null: no launch ever ran to produce one", *seedRow.SessionID)
	}
}

func TestPublishedContinuationRowsNameNoSessionTheRuntimeNeverReported(t *testing.T) {
	t.Parallel()

	script := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{
		Stdout: `{"type":"result","status":"end_turn"}`,
	})
	seed, recall := induceNativeContinuation(t, continuationCoordinates(script), semanticFixture(t), semanticTestSurface)

	seedRow, recallRow := publishedContinuationRows(t, semanticTestSurface, seed, recall)
	if seedRow.SessionID != nil {
		t.Errorf("published seed row session_id = %q, want null: the runtime reported none", *seedRow.SessionID)
	}
	if recallRow.PriorSessionID != nil {
		t.Errorf("published recall row prior_session_id = %q, want null: there is no seed session to resume", *recallRow.PriorSessionID)
	}
}
