package eval

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func TestCheckDerivedBaselinesOutcomeMismatchControl(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeFailure,
		evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomeFixtureInductionFailed, Detail: "runtime failure was not observed"}); err != nil {
		t.Fatalf("SetSemanticObservation(...) error = %v, want nil", err)
	}
	if err := fixture.SetSemanticObservation(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition, evidence.CaseLimitReached,
		evidence.Observation{Grade: evidence.GradeNotObserved, Outcome: evidence.OutcomePrerequisiteFailed, Detail: "limit reached was not observed"}); err != nil {
		t.Fatalf("SetSemanticObservation(...) error = %v, want nil", err)
	}
	fixture.Finalize()

	baseline := fixture.FindFirst(evidence.MatchBaseline(evidence.SurfaceProtocol, evidence.CapabilityTurnDisposition))
	if baseline == nil {
		t.Fatal("FindFirst(MatchBaseline(protocol, turn_disposition)) = nil, want the baseline record")
	}
	if baseline.Outcome != evidence.OutcomeFixtureInductionFailed {
		t.Fatalf("baseline.Outcome = %s, want %s before the control corrupts it: fixture_induction_failed must outrank prerequisite_failed", baseline.Outcome, evidence.OutcomeFixtureInductionFailed)
	}
	baseline.Outcome = evidence.OutcomePrerequisiteFailed

	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	_, err := ValidateObservations(path, profile.RuntimeProfile{})
	if err == nil {
		t.Fatal("ValidateObservations() = nil error, want rejection of a baseline outcome that does not equal its own derivation")
	}
	if !strings.Contains(err.Error(), "baseline outcome") {
		t.Errorf("ValidateObservations() error = %v, want it to name the baseline outcome mismatch", err)
	}
}

func TestCheckDerivedBaselinesAllExcludedIsUnreachableControl(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.Finalize()
	var notInducible []profile.SurfaceNotInducible
	for _, surface := range fixture.Declarations().DeclaredMeasuredSurfaces() {
		fixture.SetSemanticNotInducible(surface, evidence.CapabilityTurnDisposition, evidence.CaseRuntimeRefusal, evidence.NotInducibleChannelTooSmall)
		notInducible = append(notInducible, profile.SurfaceNotInducible{Surface: surface, Case: evidence.CaseRuntimeRefusal, Reason: evidence.NotInducibleChannelTooSmall})
		for _, caseID := range evidence.CapabilityCases[evidence.CapabilityRetryClassification] {
			// CaseUnknownOutcome is catalog-wide not-inducible already, not
			// a profile-level declaration.
			if caseID == evidence.CaseUnknownOutcome {
				fixture.SetSemanticNotInducible(surface, evidence.CapabilityRetryClassification, caseID, evidence.NotInducibleDetail)
				continue
			}
			fixture.SetSemanticNotInducible(surface, evidence.CapabilityRetryClassification, caseID, evidence.NotInducibleChannelTooSmall)
			notInducible = append(notInducible, profile.SurfaceNotInducible{Surface: surface, Case: caseID, Reason: evidence.NotInducibleChannelTooSmall})
		}
	}
	path := evidencetest.WriteEvidenceFile(t, fixture.Records)
	_, err := ValidateObservations(path, profile.RuntimeProfile{NotInducibleCases: notInducible})
	if err == nil {
		t.Fatal("ValidateObservations() = nil error, want rejection before the outcome derivation is ever reached")
	}
	if !strings.Contains(err.Error(), "has every case excluded") {
		t.Errorf("ValidateObservations() error = %v, want the all-excluded rejection, confirming the outcome comparison never runs against an empty contributing set", err)
	}
}
