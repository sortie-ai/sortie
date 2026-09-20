package qualification

import (
	"strings"
	"testing"
)

const seedSessionID = "seed-1"

func sameSessionRecallRecord(grade Grade, sessionID string) *Record {
	priorSessionID := seedSessionID
	rec := &Record{
		Scenario:       ScenarioContinuation,
		Surface:        SurfaceProtocol,
		Capability:     CapabilitySessionContinuation,
		InputID:        InputContinuationRecall,
		Grade:          grade,
		Outcome:        OutcomePass,
		Detail:         RecallSameSessionWithoutRecall,
		PriorSessionID: &priorSessionID,
	}
	if sessionID != "" {
		rec.SessionID = &sessionID
	}
	return rec
}

func TestRecallDetailExpressesSameSessionWithoutRecall(T *testing.T) {
	T.Parallel()

	v := &setValidation{}
	rec := sameSessionRecallRecord(GradeGap, seedSessionID)
	if err := v.checkRecallRecord(rec); err != nil {
		T.Errorf("checkRecallRecord() error = %v, want nil: a completed recall in the seed's own session with no memory is a gap, not an absent observation", err)
	}
}

func TestRecallDetailSameSessionWithoutRecallRejections(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name      string
		rec       *Record
		wantCause string
	}{
		{
			name:      "graded usable",
			rec:       sameSessionRecallRecord(GradeUsable, seedSessionID),
			wantCause: "requires classification gap",
		},
		{
			name:      "a session distinct from the seed's own",
			rec:       sameSessionRecallRecord(GradeGap, "other-1"),
			wantCause: "equal non-null actual and prior session ids",
		},
		{
			name:      "no session at all",
			rec:       sameSessionRecallRecord(GradeGap, ""),
			wantCause: "equal non-null actual and prior session ids",
		},
	}

	for _, tc := range tests {
		T.Run(tc.name, func(T *testing.T) {
			T.Parallel()
			err := (&setValidation{}).checkRecallRecord(tc.rec)
			if err == nil {
				T.Fatalf("checkRecallRecord() = nil error, want rejection")
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				T.Errorf("checkRecallRecord() error = %v, want it to name %q", err, tc.wantCause)
			}
		})
	}
}

func declinedRecallRecord(grade Grade, outcome Outcome, sessionID string) *Record {
	rec := sameSessionRecallRecord(grade, sessionID)
	rec.Outcome = outcome
	rec.Detail = RecallDeclined
	return rec
}

func TestRecallDetailExpressesADeclinedAnswer(T *testing.T) {
	T.Parallel()

	v := &setValidation{}
	rec := declinedRecallRecord(GradeNotObserved, OutcomeFixtureInductionFailed, seedSessionID)
	if err := v.checkRecallRecord(rec); err != nil {
		T.Errorf("checkRecallRecord() error = %v, want nil: an answer that declined reports neither memory nor its loss", err)
	}
}

func TestRecallDetailDeclinedAnswerRejections(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name      string
		rec       *Record
		wantCause string
	}{
		{
			name:      "graded gap",
			rec:       declinedRecallRecord(GradeGap, OutcomeFixtureInductionFailed, seedSessionID),
			wantCause: "requires classification not_observed",
		},
		{
			name:      "reported as a completed observation",
			rec:       declinedRecallRecord(GradeNotObserved, OutcomePass, seedSessionID),
			wantCause: "requires verdict fixture_induction_failed",
		},
		{
			name:      "a session distinct from the seed's own",
			rec:       declinedRecallRecord(GradeNotObserved, OutcomeFixtureInductionFailed, "other-1"),
			wantCause: "equal non-null actual and prior session ids",
		},
		{
			name:      "no session at all",
			rec:       declinedRecallRecord(GradeNotObserved, OutcomeFixtureInductionFailed, ""),
			wantCause: "equal non-null actual and prior session ids",
		},
	}

	for _, tc := range tests {
		T.Run(tc.name, func(T *testing.T) {
			T.Parallel()
			err := (&setValidation{}).checkRecallRecord(tc.rec)
			if err == nil {
				T.Fatalf("checkRecallRecord() = nil error, want rejection")
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				T.Errorf("checkRecallRecord() error = %v, want it to name %q", err, tc.wantCause)
			}
		})
	}
}
