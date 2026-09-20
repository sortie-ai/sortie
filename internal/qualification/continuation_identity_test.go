package qualification

import "testing"

func TestSeedRowRecordsASurfaceThatNamesNoSession(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureNotObserved)
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
		t.Fatalf("SetSessionContinuationObserved(native_json, seed without a session, ...) error = %v, want nil", err)
	}

	seedRec := fixture.FindFirst(MatchContinuation(SurfaceNativeJSON, InputContinuationSeed))
	if seedRec == nil {
		t.Fatal("no continuation seed record after SetSessionContinuationObserved")
	}
	if seedRec.SessionID != nil {
		t.Errorf("seed record session_id = %q, want null: the surface named no session", *seedRec.SessionID)
	}
	if seedRec.Grade != GradeUsable {
		t.Errorf("seed record grade = %s, want %s: the turn completed even though no session was named", seedRec.Grade, GradeUsable)
	}

	recallRec := fixture.FindFirst(MatchContinuation(SurfaceNativeJSON, InputContinuationRecall))
	if recallRec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if recallRec.PriorSessionID != nil {
		t.Errorf("recall record prior_session_id = %q, want null: there is no seed session to point at", *recallRec.PriorSessionID)
	}
}

func TestSeedRowWithoutASessionRejectsAFreshFallbackRecall(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureNotObserved)
	seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "the seed launch named no session"}
	recall := Observation{
		Grade:     GradeGap,
		Outcome:   OutcomePass,
		Detail:    RecallFreshFallback,
		SessionID: "some-other-session",
	}

	if err := fixture.SetSessionContinuationObserved(SurfaceNativeJSON, seed, recall); err == nil {
		t.Error("SetSessionContinuationObserved(native_json, seed without a session, fresh fallback recall) = nil error, want a rejection")
	}
}
