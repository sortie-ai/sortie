package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

const seedSessionID = "seed-1"

func sameSessionRecallRecord(grade evidence.Grade, sessionID string) *evidence.Record {
	priorSessionID := seedSessionID
	rec := &evidence.Record{
		Scenario:       evidence.ScenarioContinuation,
		Surface:        evidence.SurfaceProtocol,
		Capability:     evidence.CapabilitySessionContinuation,
		InputID:        evidence.InputContinuationRecall,
		Grade:          grade,
		Outcome:        evidence.OutcomePass,
		Detail:         evidence.RecallSameSessionWithoutRecall,
		PriorSessionID: &priorSessionID,
	}
	if sessionID != "" {
		rec.SessionID = &sessionID
	}
	return rec
}

func TestRecallDetailExpressesSameSessionWithoutRecall(t *testing.T) {
	t.Parallel()

	v := &setValidation{}
	rec := sameSessionRecallRecord(evidence.GradeGap, seedSessionID)
	if err := v.checkRecallRecord(rec); err != nil {
		t.Errorf("checkRecallRecord() error = %v, want nil: a completed recall in the seed's own session with no memory is a gap, not an absent observation", err)
	}
}

func TestRecallDetailSameSessionWithoutRecallRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rec       *evidence.Record
		wantCause string
	}{
		{
			name:      "graded usable",
			rec:       sameSessionRecallRecord(evidence.GradeUsable, seedSessionID),
			wantCause: "requires classification gap",
		},
		{
			name:      "a session distinct from the seed's own",
			rec:       sameSessionRecallRecord(evidence.GradeGap, "other-1"),
			wantCause: "equal non-null actual and prior session ids",
		},
		{
			name:      "no session at all",
			rec:       sameSessionRecallRecord(evidence.GradeGap, ""),
			wantCause: "equal non-null actual and prior session ids",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (&setValidation{}).checkRecallRecord(tc.rec)
			if err == nil {
				t.Fatalf("checkRecallRecord() = nil error, want rejection")
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				t.Errorf("checkRecallRecord() error = %v, want it to name %q", err, tc.wantCause)
			}
		})
	}
}

func declinedRecallRecord(grade evidence.Grade, outcome evidence.Outcome, sessionID string) *evidence.Record {
	rec := sameSessionRecallRecord(grade, sessionID)
	rec.Outcome = outcome
	rec.Detail = evidence.RecallDeclined
	return rec
}

func TestRecallDetailExpressesADeclinedAnswer(t *testing.T) {
	t.Parallel()

	v := &setValidation{}
	rec := declinedRecallRecord(evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed, seedSessionID)
	if err := v.checkRecallRecord(rec); err != nil {
		t.Errorf("checkRecallRecord() error = %v, want nil: an answer that declined reports neither memory nor its loss", err)
	}
}

func TestRecallDetailDeclinedAnswerRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rec       *evidence.Record
		wantCause string
	}{
		{
			name:      "graded gap",
			rec:       declinedRecallRecord(evidence.GradeGap, evidence.OutcomeFixtureInductionFailed, seedSessionID),
			wantCause: "requires classification not_observed",
		},
		{
			name:      "reported as a completed observation",
			rec:       declinedRecallRecord(evidence.GradeNotObserved, evidence.OutcomePass, seedSessionID),
			wantCause: "requires verdict fixture_induction_failed",
		},
		{
			name:      "a session distinct from the seed's own",
			rec:       declinedRecallRecord(evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed, "other-1"),
			wantCause: "equal non-null actual and prior session ids",
		},
		{
			name:      "no session at all",
			rec:       declinedRecallRecord(evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed, ""),
			wantCause: "equal non-null actual and prior session ids",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (&setValidation{}).checkRecallRecord(tc.rec)
			if err == nil {
				t.Fatalf("checkRecallRecord() = nil error, want rejection")
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				t.Errorf("checkRecallRecord() error = %v, want it to name %q", err, tc.wantCause)
			}
		})
	}
}
