package eval

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
)

func TestRecallRowRecordsASessionThatCarriedNoMemory(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := evidencetest.NewFixture(evidencetest.FixtureNotObserved)
	seed := evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: evidence.RecallSameSessionWithoutRecall, SessionID: session}

	if err := fixture.SetSessionContinuationObserved(evidence.SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, same-session-without-recall) error = %v, want nil", err)
	}

	rec := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
	if rec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if rec.Detail != evidence.RecallSameSessionWithoutRecall || rec.Grade != evidence.GradeGap {
		t.Errorf("recall record = {%s %s}, want {%s %s}", rec.Grade, rec.Detail, evidence.GradeGap, evidence.RecallSameSessionWithoutRecall)
	}
	if rec.SessionID == nil || *rec.SessionID != session {
		t.Errorf("recall record session_id = %v, want the session the turn ran in (%q)", rec.SessionID, session)
	}

	baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatal("no session continuation baseline after SetSessionContinuationObserved")
	}
	if baseline.Grade != evidence.GradeGap {
		t.Errorf("session continuation baseline = %s, want %s: the runtime kept the session and lost the memory", baseline.Grade, evidence.GradeGap)
	}
}

func TestRecallRowRejectsAnUnsupportedSameSessionClaim(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	tests := []struct {
		name   string
		recall evidence.Observation
	}{
		{
			name:   "a session other than the seed's own",
			recall: evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: evidence.RecallSameSessionWithoutRecall, SessionID: "another-session"},
		},
		{
			name:   "no session at all",
			recall: evidence.Observation{Grade: evidence.GradeGap, Outcome: evidence.OutcomePass, Detail: evidence.RecallSameSessionWithoutRecall},
		},
		{
			name:   "a usable grade",
			recall: evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: evidence.RecallSameSessionWithoutRecall, SessionID: session},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := evidencetest.NewFixture(evidencetest.FixtureNotObserved)
			seed := evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "seed", SessionID: session}

			if err := fixture.SetSessionContinuationObserved(evidence.SurfaceProtocol, seed, tt.recall); err == nil {
				t.Errorf("SetSessionContinuationObserved(protocol, seed, %+v) = nil error, want a rejection", tt.recall)
			}
		})
	}
}

func TestRecallRowDoesNotGradeADeclinedAnswerAsLostMemory(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := evidencetest.NewFixture(evidencetest.FixtureNotObserved)
	seed := evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: evidence.RecallDeclined, SessionID: session}

	if err := fixture.SetSessionContinuationObserved(evidence.SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, declined) error = %v, want nil", err)
	}

	rec := fixture.FindFirst(evidencetest.MatchContinuation(evidence.SurfaceProtocol, evidence.InputContinuationRecall))
	if rec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if rec.Detail == evidence.RecallSameSessionWithoutRecall {
		t.Errorf("recall record detail = %s, want the declined answer kept apart from a session that lost its memory", rec.Detail)
	}
	if rec.Detail != evidence.RecallDeclined || rec.Grade != evidence.GradeNotObserved {
		t.Errorf("recall record = {%s %s}, want {%s %s}", rec.Grade, rec.Detail, evidence.GradeNotObserved, evidence.RecallDeclined)
	}
	if rec.SessionID == nil || *rec.SessionID != session {
		t.Errorf("recall record session_id = %v, want the session the turn ran in (%q)", rec.SessionID, session)
	}

	baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatal("no session continuation baseline after SetSessionContinuationObserved")
	}
	if baseline.Grade != evidence.GradeNotObserved {
		t.Errorf("session continuation baseline = %s, want %s: an answer that declined measured nothing", baseline.Grade, evidence.GradeNotObserved)
	}
}

func TestDeclinedRecallDoesNotBlockAVerdict(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	seed := evidence.Observation{Grade: evidence.GradeUsable, Outcome: evidence.OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: evidence.RecallDeclined, SessionID: session}
	if err := fixture.SetSessionContinuationObserved(evidence.SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, declined) error = %v, want nil", err)
	}
	fixture.Finalize()

	report := publishedReport(t, fixture.Records, fixture.Declarations())
	row := rowFor(report, evidence.CapabilitySessionContinuation)

	if row.Standing != StandingUnmeasured {
		t.Errorf("session_continuation parity standing = %s, want %s: a declined answer settles nothing about the transport", row.Standing, StandingUnmeasured)
	}
	if row.Conformance == StandingBelow {
		t.Errorf("session_continuation conformance standing = %s, want the row unmeasured rather than a shortfall the operator feels", row.Conformance)
	}
	if report.Conformance == evidence.VerdictNotQualified {
		t.Errorf("product conformance = %s, want no verdict resting on a model's refusal", report.Conformance)
	}
}
