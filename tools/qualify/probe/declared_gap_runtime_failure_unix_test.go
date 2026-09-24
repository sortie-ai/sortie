//go:build unix

package probe

import (
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
)

// A declared gap stands in for an outcome a surface never produces, so
// it must not convert an induction that failed on a provider error into
// a confirmation.
func TestDeclarableSurfaceUnproducedRejectsARuntimeFailure(t *testing.T) {
	t.Parallel()

	measured := evidence.DeclarableSurfaces
	caseID := evidence.CaseLimitReached

	byRuntimeFailure := collectedObservations{semantic: map[evidence.Surface]map[evidence.Case]evidence.Observation{}}
	byInductionMiss := collectedObservations{semantic: map[evidence.Surface]map[evidence.Case]evidence.Observation{}}
	for _, surface := range measured {
		byRuntimeFailure.semantic[surface] = map[evidence.Case]evidence.Observation{
			caseID: {
				Grade:     evidence.GradeNotObserved,
				Outcome:   evidence.OutcomeRuntimeFailed,
				Detail:    "the provider failed the turn after the session started",
				SessionID: "sess-" + string(surface),
			},
		}
		byInductionMiss.semantic[surface] = map[evidence.Case]evidence.Observation{
			caseID: {
				Grade:     evidence.GradeNotObserved,
				Outcome:   evidence.OutcomeFixtureInductionFailed,
				Detail:    "the induction ran and the outcome never appeared",
				SessionID: "sess-" + string(surface),
			},
		}
	}

	if declarableSurfaceUnproduced(byRuntimeFailure, measured, caseID) {
		t.Error("declarableSurfaceUnproduced(...) = true for a runtime failure, want false: a provider error after a started session is not evidence that the outcome never occurs")
	}
	if !declarableSurfaceUnproduced(byInductionMiss, measured, caseID) {
		t.Error("declarableSurfaceUnproduced(...) = false for an induction that ran and saw nothing, want true: that is the shape a declaration stands in for")
	}
}

func TestDeclarableSurfaceUnproducedWithNotApplicable(t *testing.T) {
	t.Parallel()

	measured := evidence.DeclarableSurfaces
	caseID := evidence.CaseHumanInput

	unproducedNative := func(surface evidence.Surface) evidence.Observation {
		return evidence.Observation{
			Grade:     evidence.GradeNotObserved,
			Outcome:   evidence.OutcomeFixtureInductionFailed,
			Detail:    "the induction ran and the outcome never appeared",
			SessionID: "sess-" + string(surface),
		}
	}

	tests := []struct {
		name           string
		protocolObs    evidence.Observation
		wantUnproduced bool
	}{
		{
			name: "protocol not_applicable with a session",
			protocolObs: evidence.Observation{
				Grade:     evidence.GradeNotApplicable,
				Outcome:   evidence.OutcomeNotApplicable,
				Detail:    "the request offered a refusing option and was answered inside the protocol, so the turn went on and no human-input outcome arose",
				SessionID: "sess-protocol",
			},
			wantUnproduced: true,
		},
		{
			name: "protocol usable with a session",
			protocolObs: evidence.Observation{
				Grade:     evidence.GradeUsable,
				Outcome:   evidence.OutcomePass,
				Detail:    "the request offered no refusing option and the attempt ended requiring human input",
				SessionID: "sess-protocol",
			},
			wantUnproduced: false,
		},
		{
			name: "protocol gap with a session",
			protocolObs: evidence.Observation{
				Grade:     evidence.GradeGap,
				Outcome:   evidence.OutcomePass,
				Detail:    "the request offered no refusing option and the turn went on without ending requiring human input",
				SessionID: "sess-protocol",
			},
			wantUnproduced: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			collected := collectedObservations{semantic: map[evidence.Surface]map[evidence.Case]evidence.Observation{
				evidence.SurfaceProtocol:         {caseID: tc.protocolObs},
				evidence.SurfaceNativeJSON:       {caseID: unproducedNative(evidence.SurfaceNativeJSON)},
				evidence.SurfaceNativeStreamJSON: {caseID: unproducedNative(evidence.SurfaceNativeStreamJSON)},
			}}

			if got := declarableSurfaceUnproduced(collected, measured, caseID); got != tc.wantUnproduced {
				t.Errorf("declarableSurfaceUnproduced(...) = %v, want %v", got, tc.wantUnproduced)
			}
		})
	}
}
