package qualification

import (
	"strings"
	"testing"
)

func TestDeriveBaselineOutcome(t *testing.T) {
	t.Parallel()

	t.Run("a grade other than not_observed derives pass regardless of contributing", func(t *testing.T) {
		t.Parallel()

		for _, grade := range []Grade{GradeUsable, GradeGap, GradeCorroborationOnly, GradeDeclaredGap, GradeNotInducible} {
			contributing := []Outcome{OutcomeRuntimeFailed, OutcomeNotObserved}
			if got := DeriveBaselineOutcome(grade, contributing); got != OutcomePass {
				t.Errorf("DeriveBaselineOutcome(%s, %v) = %s, want %s", grade, contributing, got, OutcomePass)
			}
		}
	})

	t.Run("an empty contributing set derives not_observed", func(t *testing.T) {
		t.Parallel()

		if got := DeriveBaselineOutcome(GradeNotObserved, nil); got != OutcomeNotObserved {
			t.Errorf("DeriveBaselineOutcome(not_observed, nil) = %s, want %s", got, OutcomeNotObserved)
		}
	})

	rankTests := []struct {
		name         string
		contributing []Outcome
		want         Outcome
	}{
		{"only not_observed", []Outcome{OutcomeNotObserved}, OutcomeNotObserved},
		{"prerequisite_failed beats not_observed", []Outcome{OutcomeNotObserved, OutcomePrerequisiteFailed}, OutcomePrerequisiteFailed},
		{"fixture_induction_failed beats prerequisite_failed", []Outcome{OutcomePrerequisiteFailed, OutcomeFixtureInductionFailed}, OutcomeFixtureInductionFailed},
		{"runtime_failed beats fixture_induction_failed", []Outcome{OutcomeFixtureInductionFailed, OutcomeRuntimeFailed}, OutcomeRuntimeFailed},
		{
			name:         "runtime_failed wins over every other rank in reverse order",
			contributing: []Outcome{OutcomeRuntimeFailed, OutcomeFixtureInductionFailed, OutcomePrerequisiteFailed, OutcomeNotObserved},
			want:         OutcomeRuntimeFailed,
		},
		{
			name:         "runtime_failed wins over every other rank in forward order",
			contributing: []Outcome{OutcomeNotObserved, OutcomePrerequisiteFailed, OutcomeFixtureInductionFailed, OutcomeRuntimeFailed},
			want:         OutcomeRuntimeFailed,
		},
		{
			name:         "runtime_failed wins when buried in the middle",
			contributing: []Outcome{OutcomePrerequisiteFailed, OutcomeRuntimeFailed, OutcomeNotObserved, OutcomeFixtureInductionFailed},
			want:         OutcomeRuntimeFailed,
		},
	}
	for _, tt := range rankTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := DeriveBaselineOutcome(GradeNotObserved, tt.contributing); got != tt.want {
				t.Errorf("DeriveBaselineOutcome(not_observed, %v) = %s, want %s", tt.contributing, got, tt.want)
			}
		})
	}
}

func TestCheckDerivedBaselinesOutcomeMismatchControl(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	if err := fixture.SetSemanticObservation(SurfaceProtocol, CapabilityTurnDisposition, CaseRuntimeFailure,
		Observation{Grade: GradeNotObserved, Outcome: OutcomeFixtureInductionFailed, Detail: "runtime failure was not observed"}); err != nil {
		T.Fatalf("SetSemanticObservation(...) error = %v, want nil", err)
	}
	if err := fixture.SetSemanticObservation(SurfaceProtocol, CapabilityTurnDisposition, CaseLimitReached,
		Observation{Grade: GradeNotObserved, Outcome: OutcomePrerequisiteFailed, Detail: "limit reached was not observed"}); err != nil {
		T.Fatalf("SetSemanticObservation(...) error = %v, want nil", err)
	}
	fixture.Finalize()

	baseline := fixture.FindFirst(MatchBaseline(SurfaceProtocol, CapabilityTurnDisposition))
	if baseline == nil {
		T.Fatal("FindFirst(MatchBaseline(protocol, turn_disposition)) = nil, want the baseline record")
	}
	if baseline.Outcome != OutcomeFixtureInductionFailed {
		T.Fatalf("baseline.Outcome = %s, want %s before the control corrupts it: fixture_induction_failed must outrank prerequisite_failed", baseline.Outcome, OutcomeFixtureInductionFailed)
	}
	baseline.Outcome = OutcomePrerequisiteFailed

	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservations(path)
	if err == nil {
		T.Fatal("ValidateObservations() = nil error, want rejection of a baseline outcome that does not equal its own derivation")
	}
	if !strings.Contains(err.Error(), "baseline outcome") {
		T.Errorf("ValidateObservations() error = %v, want it to name the baseline outcome mismatch", err)
	}
}

func TestCheckDerivedBaselinesAllExcludedIsUnreachableControl(T *testing.T) {
	T.Parallel()

	fixture := NewFixture(FixtureQualified)
	fixture.Finalize()
	var notInducible []SurfaceNotInducible
	for _, surface := range measuredSurfaces(fixture.Declarations()) {
		declareNotInducible(fixture, surface, CapabilityTurnDisposition, CaseRuntimeRefusal)
		fixture.FindFirst(MatchSemantic(surface, CapabilityTurnDisposition, CaseRuntimeRefusal)).Detail = NotInducibleChannelTooSmall
		notInducible = append(notInducible, SurfaceNotInducible{Surface: surface, Case: CaseRuntimeRefusal, Reason: NotInducibleChannelTooSmall})
		for _, caseID := range CapabilityCases[CapabilityRetryClassification] {
			declareNotInducible(fixture, surface, CapabilityRetryClassification, caseID)
			if caseID != CaseUnknownOutcome {
				fixture.FindFirst(MatchSemantic(surface, CapabilityRetryClassification, caseID)).Detail = NotInducibleChannelTooSmall
				notInducible = append(notInducible, SurfaceNotInducible{Surface: surface, Case: caseID, Reason: NotInducibleChannelTooSmall})
			}
		}
	}
	path := WriteEvidenceFile(T, fixture.Records)
	_, err := ValidateObservationsWithDeclarations(path, RuntimeProfile{NotInducibleCases: notInducible})
	if err == nil {
		T.Fatal("ValidateObservationsWithDeclarations() = nil error, want rejection before the outcome derivation is ever reached")
	}
	if !strings.Contains(err.Error(), "has every case excluded") {
		T.Errorf("ValidateObservationsWithDeclarations() error = %v, want the all-excluded rejection, confirming the outcome comparison never runs against an empty contributing set", err)
	}
}
