package qualification

import (
	"strings"
	"testing"
)

func caseLevelProfile(entries ...SurfaceNotInducible) RuntimeProfile {
	return RuntimeProfile{
		EntryPoints: map[Surface]EntryPoint{
			SurfaceProtocol:         {},
			SurfaceNativeJSON:       {},
			SurfaceNativeStreamJSON: {},
		},
		NotInducibleCases: entries,
	}
}

func setProtocolCase(T *testing.T, f *Fixture, capability Capability, caseID Case, grade Grade, outcome Outcome) {
	T.Helper()
	rec := f.FindFirst(MatchSemantic(SurfaceProtocol, capability, caseID))
	if rec == nil {
		T.Fatalf("no semantic record for %s %s %s", SurfaceProtocol, capability, caseID)
	}
	rec.Grade = grade
	rec.Outcome = outcome
	f.UpdateSemanticBaseline(SurfaceProtocol, capability)
}

func markNotInducible(T *testing.T, f *Fixture, surface Surface, capability Capability, caseID Case) {
	T.Helper()
	rec := f.FindFirst(MatchSemantic(surface, capability, caseID))
	if rec == nil {
		T.Fatalf("no semantic record for %s %s %s", surface, capability, caseID)
	}
	rec.Grade = GradeNotInducible
	rec.Outcome = OutcomeNotInducible
	rec.Detail = NotInducibleDetail
	rec.EvidencePath = nil
	rec.SessionID = nil
	f.UpdateSemanticBaseline(surface, capability)
}

func rowFor(report EligibilityReport, capability Capability) RowOutcome {
	for _, row := range report.Rows {
		if row.Label == string(capability) {
			return row
		}
	}
	return RowOutcome{}
}

func TestComparisonCountsChannelSilenceAgainstItsOwnSurface(T *testing.T) {
	T.Parallel()

	profile := caseLevelProfile(SurfaceNotInducible{
		Surface: SurfaceProtocol,
		Case:    CaseRuntimeFailure,
		Reason:  NotInducibleOutputSilentOnFailure,
	})
	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	markNotInducible(T, fixture, SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeFailure)

	row := rowFor(ExplainEligibility(fixture.Records, profile), CapabilityTurnDisposition)
	if row.Standing != StandingBelow {
		T.Errorf("turn_disposition standing = %s (cause %q), want below: the protocol surface reports nothing for runtime_failure while both native surfaces do, and dropping the row must not earn it a pass", row.Standing, row.Cause)
	}
	if !strings.Contains(row.Cause, string(CaseRuntimeFailure)) {
		T.Errorf("cause = %q, want it to name the case that fell short", row.Cause)
	}
}

func TestComparisonIgnoresCaseOnlyOneSideReports(T *testing.T) {
	T.Parallel()

	profile := caseLevelProfile(
		SurfaceNotInducible{Surface: SurfaceNativeJSON, Case: CaseHumanInput, Reason: NotInducibleTerminalVocabularyClosed},
		SurfaceNotInducible{Surface: SurfaceNativeStreamJSON, Case: CaseHumanInput, Reason: NotInducibleTerminalVocabularyClosed},
	)
	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	markNotInducible(T, fixture, SurfaceNativeJSON, CapabilityRetryClassification, CaseHumanInput)
	markNotInducible(T, fixture, SurfaceNativeStreamJSON, CapabilityRetryClassification, CaseHumanInput)
	setProtocolCase(T, fixture, CapabilityRetryClassification, CaseHumanInput, GradeGap, OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, profile), CapabilityRetryClassification)
	if row.Standing != StandingSatisfied {
		T.Errorf("retry_classification standing = %s (cause %q), want satisfied: the protocol surface graded a case neither native surface can report, and running the extra check must not count against it", row.Standing, row.Cause)
	}
}

func TestComparisonSkipsCaseNoMeasurerCanInduce(T *testing.T) {
	T.Parallel()

	profile := caseLevelProfile(
		SurfaceNotInducible{Surface: SurfaceNativeJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
		SurfaceNotInducible{Surface: SurfaceNativeStreamJSON, Case: CaseLimitReached, Reason: NotInducibleChannelTooSmall},
	)
	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	markNotInducible(T, fixture, SurfaceNativeJSON, CapabilityTurnDisposition, CaseLimitReached)
	markNotInducible(T, fixture, SurfaceNativeStreamJSON, CapabilityTurnDisposition, CaseLimitReached)
	setProtocolCase(T, fixture, CapabilityTurnDisposition, CaseLimitReached, GradeGap, OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, profile), CapabilityTurnDisposition)
	if row.Standing != StandingSatisfied {
		T.Errorf("turn_disposition standing = %s (cause %q), want satisfied: no native surface offers a reference for limit_reached, so the protocol reading of it decides nothing", row.Standing, row.Cause)
	}
}

func TestComparisonStillReportsAGenuineShortfall(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	setProtocolCase(T, fixture, CapabilityRetryClassification, CaseRetryableTransport, GradeGap, OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, caseLevelProfile()), CapabilityRetryClassification)
	if row.Standing != StandingBelow {
		T.Errorf("retry_classification standing = %s (cause %q), want below", row.Standing, row.Cause)
	}
}

func TestComparisonReportsAnUnmeasuredProtocolCase(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	setProtocolCase(T, fixture, CapabilityRetryClassification, CaseHumanInput, GradeNotObserved, OutcomeFixtureInductionFailed)

	row := rowFor(ExplainEligibility(fixture.Records, caseLevelProfile()), CapabilityRetryClassification)
	if row.Standing != StandingUnmeasured {
		T.Errorf("retry_classification standing = %s (cause %q), want unmeasured", row.Standing, row.Cause)
	}
}
