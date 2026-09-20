package qualification

import (
	"strings"
	"testing"
)

func unnamedSeedFixture(T *testing.T) *Fixture {
	T.Helper()

	fixture := NewFixture(FixtureQualified)
	seed := Observation{
		Grade:   GradeUsable,
		Outcome: OutcomePass,
		Detail:  "the seed launch completed a turn on a surface that reports no session identity",
	}
	recall := Observation{
		Grade:   GradeNotObserved,
		Outcome: OutcomePrerequisiteFailed,
		Detail:  RecallPreconditionUnmet,
	}
	if err := fixture.SetSessionContinuationObserved(SurfaceNativeJSON, seed, recall); err != nil {
		T.Fatalf("SetSessionContinuationObserved(native_json, seed without a session, ...) error = %v, want nil", err)
	}
	fixture.Finalize()
	return fixture
}

func TestPublishedEvidenceAdmitsASeedThatNamesNoSession(T *testing.T) {
	T.Parallel()

	path := WriteEvidenceFile(T, unnamedSeedFixture(T).Records)
	if _, err := ValidateObservations(path); err != nil {
		T.Errorf("ValidateObservations(%s) = %v, want nil: a surface that names no session must be publishable", path, err)
	}
}

func TestPublishedEvidenceRejectsARecallPointingAtNoSeedSession(T *testing.T) {
	T.Parallel()

	fixture := unnamedSeedFixture(T)
	recallRec := fixture.FindFirst(MatchContinuation(SurfaceNativeJSON, InputContinuationRecall))
	if recallRec == nil {
		T.Fatal("no continuation recall record for native_json")
	}
	recallRec.PriorSessionID = new("sess-native-json-seed")
	invalid := WriteEvidenceFile(T, fixture.Records)

	_, err := ValidateObservations(invalid)
	if err == nil {
		T.Fatalf("ValidateObservations(%s) = nil error, want a rejection: the seed named no session for this prior id to resolve to", invalid)
	}
	if !strings.Contains(err.Error(), "prior_session_id") {
		T.Errorf("ValidateObservations() error = %v, want it to name prior_session_id", err)
	}
}
