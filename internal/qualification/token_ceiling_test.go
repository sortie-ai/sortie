package qualification

import (
	"testing"
)

func TestTokenBaselineGradeOnNoRecordsIsNotAProvenZero(t *testing.T) {
	t.Parallel()

	if got := tokenBaselineGrade(nil); got != GradeNotObserved {
		t.Errorf("tokenBaselineGrade(nil) = %s, want %s", got, GradeNotObserved)
	}
}

func TestTokenBaselineGradeReportsAFailedSentinelRegardlessOfOrder(t *testing.T) {
	t.Parallel()

	sourceRecord := &Record{Grade: GradeUsable, Outcome: OutcomePass, EvidencePath: new("/usage/total")}
	failed := &Record{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed}

	if got := tokenBaselineGrade([]*Record{sourceRecord, failed}); got != GradeNotObserved {
		t.Errorf("tokenBaselineGrade(source, failed sentinel) = %s, want %s", got, GradeNotObserved)
	}
}

func TestSetTokenInventoryRejectsAnUnattributedPath(t *testing.T) {
	t.Parallel()

	fixture := NewFixture(FixtureQualified)
	err := fixture.SetTokenInventory(SurfaceProtocol, "",
		[]TokenObservation{{EvidencePath: "/turn/result/usage", Kind: "spend"}},
		Observation{Grade: GradeGap, Outcome: OutcomePass, Detail: "the inventory completed"}, nil)
	if err == nil {
		t.Error("SetTokenInventory(protocol, \"\", ...) = nil error, want rejection of a resolved path no session is named for")
	}
}
