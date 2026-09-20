//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func publishedContinuationRows(t *testing.T, surface evidence.Surface, seed, recall gradedObservation) (seedRow, recallRow evidence.Record) {
	t.Helper()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	if err := fixture.SetSessionContinuationObserved(surface, seed.obs, recall.obs); err != nil {
		t.Fatalf("SetSessionContinuationObserved(%s, %+v, %+v) error = %v, want nil", surface, seed.obs, recall.obs, err)
	}
	fixture.Finalize()

	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	published, err := evidence.ReadEvidenceFile(path)
	if err != nil {
		t.Fatalf("ReadEvidenceFile(%s) = _, %v, want nil", path, err)
	}

	var foundSeed, foundRecall bool
	for _, rec := range published {
		if rec.Scenario != evidence.ScenarioContinuation || rec.Surface != surface {
			continue
		}
		switch rec.InputID {
		case evidence.InputContinuationSeed:
			seedRow, foundSeed = rec, true
		case evidence.InputContinuationRecall:
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
	seed, recall := induceNativeContinuation(t, coords, semanticFixture(t), evidence.SurfaceNativeJSON)

	seedRow, _ := publishedContinuationRows(t, evidence.SurfaceNativeJSON, seed, recall)
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
