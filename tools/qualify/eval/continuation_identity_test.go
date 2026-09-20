package eval

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func TestSeedRowRecordsASurfaceThatNamesNoSession(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureNotObserved)
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

	seedRec := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceNativeJSON, evidence.InputContinuationSeed))
	if seedRec == nil {
		t.Fatal("no continuation seed record after SetSessionContinuationObserved")
	}
	if seedRec.SessionID != nil {
		t.Errorf("seed record session_id = %q, want null: the surface named no session", *seedRec.SessionID)
	}
	if seedRec.Grade != evidence.GradeUsable {
		t.Errorf("seed record grade = %s, want %s: the turn completed even though no session was named", seedRec.Grade, evidence.GradeUsable)
	}

	recallRec := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceNativeJSON, evidence.InputContinuationRecall))
	if recallRec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if recallRec.PriorSessionID != nil {
		t.Errorf("recall record prior_session_id = %q, want null: there is no seed session to point at", *recallRec.PriorSessionID)
	}
}

func TestSeedRowWithoutASessionRejectsAFreshFallbackRecall(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureNotObserved)
	seed := evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed launch named no session"}
	recall := evidence.Observation{
		Grade:     evidence.GradeGap,
		Outcome:   evidence.OutcomePass,
		Detail:    evidence.RecallFreshFallback,
		SessionID: "some-other-session",
	}

	if err := fixture.SetSessionContinuationObserved(evidence.SurfaceNativeJSON, seed, recall); err == nil {
		t.Error("SetSessionContinuationObserved(native_json, seed without a session, fresh fallback recall) = nil error, want a rejection")
	}
}
