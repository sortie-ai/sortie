package eval

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func TestConclusionsFromRecordsReportsNotApplicableAsExcluded(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	sessionID := evidencetest.FixtureSession(evidence.SurfaceProtocol, "permission")
	detail := "the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose"
	obs := evidence.Observation{
		Grade:        evidence.GradeNotApplicable,
		Outcome:      evidence.OutcomeNotApplicable,
		Detail:       detail,
		SessionID:    sessionID,
		EvidencePath: evidence.SemanticEvidencePath(evidence.SurfaceProtocol),
	}
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, obs); err != nil {
		t.Fatalf("SetSemanticObservation(protocol human_input) error = %v, want nil", err)
	}

	conclusions, err := conclusionsFromRecords(fixture.Records, evidence.VerdictQualified, threeSurfaceProfile())
	if err != nil {
		t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
	}

	want := "retry_classification human_input: not applicable on protocol: " + detail
	if !slices.Contains(conclusions.Excluded, want) {
		t.Errorf("Excluded = %v, want it to contain %q", conclusions.Excluded, want)
	}
	for _, line := range conclusions.Unobserved {
		if strings.Contains(line, string(evidence.SurfaceProtocol)) && strings.Contains(line, string(evidence.CaseHumanInput)) {
			t.Errorf("Unobserved = %v, want no protocol human_input line", conclusions.Unobserved)
		}
	}
}
