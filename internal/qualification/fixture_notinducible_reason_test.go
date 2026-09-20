package qualification

import "testing"

func publishedSemanticRow(T *testing.T, fixture *Fixture, surface Surface, capability Capability, caseID Case) (caseRow, baselineRow Record) {
	T.Helper()

	published, err := ReadEvidenceFile(WriteEvidenceFile(T, fixture.Records))
	if err != nil {
		T.Fatalf("ReadEvidenceFile(...) = _, %v, want nil", err)
	}
	var foundCase, foundBaseline bool
	for _, rec := range published {
		switch {
		case rec.Scenario == ScenarioSemanticProbe && rec.Surface == surface && rec.Capability == capability &&
			rec.SemanticCase != nil && *rec.SemanticCase == caseID:
			caseRow, foundCase = rec, true
		case rec.Scenario == ScenarioSurfaceBaseline && rec.Surface == surface && rec.Capability == capability:
			baselineRow, foundBaseline = rec, true
		}
	}
	if !foundCase || !foundBaseline {
		T.Fatalf("the published evidence carries no %s %s row for surface %s", capability, caseID, surface)
	}
	return caseRow, baselineRow
}

func TestPublishedRowCarriesTheReasonACaseWasNotInduced(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.SetSemanticNotInducible(SurfaceNativeJSON, CapabilityTurnDisposition, CaseCancellation, NotInducibleTerminalAtExitOnly)
	fixture.Finalize()

	caseRow, _ := publishedSemanticRow(T, fixture, SurfaceNativeJSON, CapabilityTurnDisposition, CaseCancellation)
	if caseRow.Detail != NotInducibleTerminalAtExitOnly {
		T.Errorf("published cancellation row detail = %q, want %q", caseRow.Detail, NotInducibleTerminalAtExitOnly)
	}
}

func TestPublishedBaselineKeepsAChannelSilentCasesObligation(T *testing.T) {
	T.Parallel()

	T.Run("the surface's own silence", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotInducible(SurfaceNativeJSON, CapabilityTurnDisposition, CaseCancellation, NotInducibleTerminalAtExitOnly)
		fixture.Finalize()

		_, baselineRow := publishedSemanticRow(T, fixture, SurfaceNativeJSON, CapabilityTurnDisposition, CaseCancellation)
		if baselineRow.Grade != GradeGap {
			T.Errorf("published turn_disposition baseline = %s, want %s: the surface reports nothing on a condition it does reach", baselineRow.Grade, GradeGap)
		}
	})

	T.Run("the measurer's own reach", func(T *testing.T) {
		T.Parallel()

		fixture := NewFixture(FixtureQualified)
		fixture.SetSemanticNotInducible(SurfaceNativeJSON, CapabilityTurnDisposition, CaseLimitReached, NotInducibleChannelTooSmall)
		fixture.Finalize()

		_, baselineRow := publishedSemanticRow(T, fixture, SurfaceNativeJSON, CapabilityTurnDisposition, CaseLimitReached)
		if baselineRow.Grade != GradeUsable {
			T.Errorf("published turn_disposition baseline = %s, want %s: a condition this measurer cannot create is no shortfall of the surface", baselineRow.Grade, GradeUsable)
		}
	})
}
