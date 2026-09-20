package qualification

import "testing"

func TestRecallRowRecordsASessionThatCarriedNoMemory(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := NewFixture(FixtureNotObserved)
	seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: RecallSameSessionWithoutRecall, SessionID: session}

	if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, same-session-without-recall) error = %v, want nil", err)
	}

	rec := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
	if rec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if rec.Detail != RecallSameSessionWithoutRecall || rec.Grade != GradeGap {
		t.Errorf("recall record = {%s %s}, want {%s %s}", rec.Grade, rec.Detail, GradeGap, RecallSameSessionWithoutRecall)
	}
	if rec.SessionID == nil || *rec.SessionID != session {
		t.Errorf("recall record session_id = %v, want the session the turn ran in (%q)", rec.SessionID, session)
	}

	baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatal("no session continuation baseline after SetSessionContinuationObserved")
	}
	if baseline.Grade != GradeGap {
		t.Errorf("session continuation baseline = %s, want %s: the runtime kept the session and lost the memory", baseline.Grade, GradeGap)
	}
}

func TestRecallRowRejectsAnUnsupportedSameSessionClaim(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	tests := []struct {
		name   string
		recall Observation
	}{
		{
			name:   "a session other than the seed's own",
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: RecallSameSessionWithoutRecall, SessionID: "another-session"},
		},
		{
			name:   "no session at all",
			recall: Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: RecallSameSessionWithoutRecall},
		},
		{
			name:   "a usable grade",
			recall: Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: RecallSameSessionWithoutRecall, SessionID: session},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := NewFixture(FixtureNotObserved)
			seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "seed", SessionID: session}

			if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, seed, tt.recall); err == nil {
				t.Errorf("SetSessionContinuationObserved(protocol, seed, %+v) = nil error, want a rejection", tt.recall)
			}
		})
	}
}

func TestRecallRowDoesNotGradeADeclinedAnswerAsLostMemory(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := NewFixture(FixtureNotObserved)
	seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := Observation{Grade: GradeNotObserved, Outcome: OutcomeFixtureInductionFailed, Detail: RecallDeclined, SessionID: session}

	if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, declined) error = %v, want nil", err)
	}

	rec := fixture.FindFirst(MatchContinuation(SurfaceProtocol, InputContinuationRecall))
	if rec == nil {
		t.Fatal("no continuation recall record after SetSessionContinuationObserved")
	}
	if rec.Detail == RecallSameSessionWithoutRecall {
		t.Errorf("recall record detail = %s, want the declined answer kept apart from a session that lost its memory", rec.Detail)
	}
	if rec.Detail != RecallDeclined || rec.Grade != GradeNotObserved {
		t.Errorf("recall record = {%s %s}, want {%s %s}", rec.Grade, rec.Detail, GradeNotObserved, RecallDeclined)
	}
	if rec.SessionID == nil || *rec.SessionID != session {
		t.Errorf("recall record session_id = %v, want the session the turn ran in (%q)", rec.SessionID, session)
	}

	baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilitySessionContinuation))
	if baseline == nil {
		t.Fatal("no session continuation baseline after SetSessionContinuationObserved")
	}
	if baseline.Grade != GradeNotObserved {
		t.Errorf("session continuation baseline = %s, want %s: an answer that declined measured nothing", baseline.Grade, GradeNotObserved)
	}
}

func TestDeclinedRecallDoesNotBlockAVerdict(t *testing.T) {
	t.Parallel()

	const session = "live-seed-session"
	fixture := NewFixture(FixtureQualified)
	seed := Observation{Grade: GradeUsable, Outcome: OutcomePass, Detail: "the seed turn completed and left history", SessionID: session}
	recall := Observation{Grade: GradeNotObserved, Outcome: OutcomeFixtureInductionFailed, Detail: RecallDeclined, SessionID: session}
	if err := fixture.SetSessionContinuationObserved(SurfaceProtocol, seed, recall); err != nil {
		t.Fatalf("SetSessionContinuationObserved(protocol, seed, declined) error = %v, want nil", err)
	}
	fixture.Finalize()

	report := publishedReport(t, fixture.Records, fixture.Declarations())
	row := rowFor(report, CapabilitySessionContinuation)

	if row.Standing != StandingUnmeasured {
		t.Errorf("session_continuation parity standing = %s, want %s: a declined answer settles nothing about the transport", row.Standing, StandingUnmeasured)
	}
	if row.Conformance == StandingBelow {
		t.Errorf("session_continuation conformance standing = %s, want the row unmeasured rather than a shortfall the operator feels", row.Conformance)
	}
	if report.Conformance == VerdictNotQualified {
		t.Errorf("product conformance = %s, want no verdict resting on a model's refusal", report.Conformance)
	}
}
