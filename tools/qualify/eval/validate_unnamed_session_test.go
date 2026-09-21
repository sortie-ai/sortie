package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func unnamedSeedFixture(t *testing.T) *evidencetest.Fixture {
	t.Helper()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	seed := evidence.Observation{
		Grade:   evidence.GradeUsable,
		Outcome: evidence.OutcomePass,
		Detail:  "the seed launch completed a turn on a surface that reports no session identity",
	}
	recall := evidence.Observation{
		Grade:   evidence.GradeNotObserved,
		Outcome: evidence.OutcomePrerequisiteFailed,
		Detail:  evidence.RecallPreconditionUnmet,
	}
	if err := fixture.SetSessionContinuationObserved(evidence.SurfaceNativeJSON, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(native_json, seed without a session, ...) error = %v, want nil", err)
	}
	fixture.Finalize()
	return fixture
}

func TestPublishedEvidenceAdmitsASeedThatNamesNoSession(t *testing.T) {
	t.Parallel()

	path := evidencetest.WriteEvidenceFile(t, unnamedSeedFixture(t).Records)
	if _, err := ValidateObservations(path, profile.RuntimeProfile{}); err != nil {
		t.Errorf("ValidateObservations(%s) = %v, want nil: a surface that names no session must be publishable", path, err)
	}
}

func TestPublishedEvidenceRejectsARecallPointingAtNoSeedSession(t *testing.T) {
	t.Parallel()

	fixture := unnamedSeedFixture(t)
	recallRec := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceNativeJSON, evidence.InputContinuationRecall))
	if recallRec == nil {
		t.Fatal("no continuation recall record for native_json")
	}
	recallRec.PriorSessionID = new("sess-native-json-seed")
	invalid := evidencetest.WriteEvidenceFile(t, fixture.Records)

	_, err := ValidateObservations(invalid, profile.RuntimeProfile{})
	if err == nil {
		t.Fatalf("ValidateObservations(%s) = nil error, want a rejection: the seed named no session for this prior id to resolve to", invalid)
	}
	if !strings.Contains(err.Error(), "prior_session_id") {
		t.Errorf("ValidateObservations() error = %v, want it to name prior_session_id", err)
	}
}
