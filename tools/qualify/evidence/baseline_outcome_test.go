package evidence

import "testing"

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
