package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func caseLevelProfile(entries ...profile.SurfaceNotInducible) profile.RuntimeProfile {
	return profile.RuntimeProfile{
		EntryPoints: map[evidence.Surface]profile.EntryPoint{
			evidence.SurfaceProtocol:         {},
			evidence.SurfaceNativeJSON:       {},
			evidence.SurfaceNativeStreamJSON: {},
		},
		NotInducibleCases: entries,
	}
}

func setProtocolCase(t *testing.T, f *evidencetest.Fixture, capability evidence.Capability, caseID evidence.Case, grade evidence.Grade, outcome evidence.Outcome) {
	t.Helper()
	rec := f.FindFirst(evidencetest.MatchSemantic(evidence.SurfaceProtocol, capability, caseID))
	if rec == nil {
		t.Fatalf("no semantic record for %s %s %s", evidence.SurfaceProtocol, capability, caseID)
	}
	rec.Grade = grade
	rec.Outcome = outcome
	f.UpdateSemanticBaseline(evidence.SurfaceProtocol, capability)
}

func rowFor(report EligibilityReport, capability evidence.Capability) RowOutcome {
	for _, row := range report.Rows {
		if row.Label == string(capability) {
			return row
		}
	}
	return RowOutcome{}
}

func TestComparisonCountsChannelSilenceAgainstItsOwnSurface(t *testing.T) {
	t.Parallel()

	p := caseLevelProfile(profile.SurfaceNotInducible{
		Surface: evidence.SurfaceProtocol,
		Case:    evidence.CaseRuntimeFailure,
		Reason:  evidence.NotInducibleOutputSilentOnFailure,
	})
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	fixture.SetSemanticNotInducible(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeFailure, evidence.NotInducibleDetail)

	row := rowFor(ExplainEligibility(fixture.Records, p), evidence.CapabilityTurnDisposition)
	if row.Standing != StandingBelow {
		t.Errorf("turn_disposition standing = %s (cause %q), want below: the protocol surface reports nothing for runtime_failure while both native surfaces do, and dropping the row must not earn it a pass", row.Standing, row.Cause)
	}
	if !strings.Contains(row.Cause, string(evidence.CaseRuntimeFailure)) {
		t.Errorf("cause = %q, want it to name the case that fell short", row.Cause)
	}
}

func TestComparisonIgnoresCaseOnlyOneSideReports(t *testing.T) {
	t.Parallel()

	p := caseLevelProfile(
		profile.SurfaceNotInducible{Surface: evidence.SurfaceNativeJSON, Case: evidence.CaseHumanInput, Reason: evidence.NotInducibleTerminalVocabularyClosed},
		profile.SurfaceNotInducible{Surface: evidence.SurfaceNativeStreamJSON, Case: evidence.CaseHumanInput, Reason: evidence.NotInducibleTerminalVocabularyClosed},
	)
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleDetail)
	fixture.SetSemanticNotInducible(evidence.SurfaceNativeStreamJSON, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.NotInducibleDetail)
	setProtocolCase(t, fixture, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.GradeGap, evidence.OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, p), evidence.CapabilityRetryClassification)
	if row.Standing != StandingSatisfied {
		t.Errorf("retry_classification standing = %s (cause %q), want satisfied: the protocol surface graded a case neither native surface can report, and running the extra check must not count against it", row.Standing, row.Cause)
	}
}

func TestComparisonSkipsCaseNoMeasurerCanInduce(t *testing.T) {
	t.Parallel()

	p := caseLevelProfile(
		profile.SurfaceNotInducible{Surface: evidence.SurfaceNativeJSON, Case: evidence.CaseLimitReached, Reason: evidence.NotInducibleChannelTooSmall},
		profile.SurfaceNotInducible{Surface: evidence.SurfaceNativeStreamJSON, Case: evidence.CaseLimitReached, Reason: evidence.NotInducibleChannelTooSmall},
	)
	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	fixture.SetSemanticNotInducible(evidence.SurfaceNativeJSON, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached, evidence.NotInducibleDetail)
	fixture.SetSemanticNotInducible(evidence.SurfaceNativeStreamJSON, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached, evidence.NotInducibleDetail)
	setProtocolCase(t, fixture, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached, evidence.GradeGap, evidence.OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, p), evidence.CapabilityTurnDisposition)
	if row.Standing != StandingSatisfied {
		t.Errorf("turn_disposition standing = %s (cause %q), want satisfied: no native surface offers a reference for limit_reached, so the protocol reading of it decides nothing", row.Standing, row.Cause)
	}
}

func TestComparisonStillReportsAGenuineShortfall(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	setProtocolCase(t, fixture, evidence.CapabilityRetryClassification, evidence.CaseRetryableTransport, evidence.GradeGap, evidence.OutcomePass)

	row := rowFor(ExplainEligibility(fixture.Records, caseLevelProfile()), evidence.CapabilityRetryClassification)
	if row.Standing != StandingBelow {
		t.Errorf("retry_classification standing = %s (cause %q), want below", row.Standing, row.Cause)
	}
}

func TestComparisonReportsAnUnmeasuredProtocolCase(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	setProtocolCase(t, fixture, evidence.CapabilityRetryClassification, evidence.CaseHumanInput, evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed)

	row := rowFor(ExplainEligibility(fixture.Records, caseLevelProfile()), evidence.CapabilityRetryClassification)
	if row.Standing != StandingUnmeasured {
		t.Errorf("retry_classification standing = %s (cause %q), want unmeasured", row.Standing, row.Cause)
	}
}
