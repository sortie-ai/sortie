package evidence

import "testing"

func TestTokenBaselineGradeOnNoRecordsIsNotAProvenZero(t *testing.T) {
	t.Parallel()

	if got := TokenBaselineGrade(nil); got != GradeNotObserved {
		t.Errorf("TokenBaselineGrade(nil) = %s, want %s", got, GradeNotObserved)
	}
}

func TestTokenBaselineGradeReportsAFailedSentinelRegardlessOfOrder(t *testing.T) {
	t.Parallel()

	sourceRecord := &Record{Grade: GradeUsable, Outcome: OutcomePass, EvidencePath: new("/usage/total")}
	failed := &Record{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed}

	if got := TokenBaselineGrade([]*Record{sourceRecord, failed}); got != GradeNotObserved {
		t.Errorf("TokenBaselineGrade(source, failed sentinel) = %s, want %s", got, GradeNotObserved)
	}
}
