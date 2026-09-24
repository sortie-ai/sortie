package evidence

import "testing"

func TestObservationUnproduced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		obs  Observation
		want bool
	}{
		{
			name: "not_applicable is unproduced",
			obs:  Observation{Grade: GradeNotApplicable, Outcome: OutcomeNotApplicable},
			want: true,
		},
		{
			name: "not_observed with fixture_induction_failed is unproduced",
			obs:  Observation{Grade: GradeNotObserved, Outcome: OutcomeFixtureInductionFailed},
			want: true,
		},
		{
			name: "not_observed with runtime_failed is a launch failure, not unproduced",
			obs:  Observation{Grade: GradeNotObserved, Outcome: OutcomeRuntimeFailed},
			want: false,
		},
		{
			name: "usable is produced",
			obs:  Observation{Grade: GradeUsable, Outcome: OutcomePass},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ObservationUnproduced(tt.obs); got != tt.want {
				t.Errorf("ObservationUnproduced(%+v) = %v, want %v", tt.obs, got, tt.want)
			}
		})
	}
}
