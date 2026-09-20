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
