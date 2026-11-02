// Package scmcore holds the decisions a source-control forge integration
// makes over normalized domain types rather than wire shapes: the CI
// verdict a set of check runs implies, the SCM error a tracker error maps
// to, the merge-conflict promotion and already-merged marker, the
// bot-author and event-id primitives, and the cache that remembers
// whether a comment author has write access. It imports internal/domain only, so
// every forge adapter can depend on it without depending on a sibling
// forge. The orchestrator also depends on it directly for bot-author
// classification, without depending on any forge adapter.
package scmcore

import "github.com/sortie-ai/sortie/internal/domain"

// CIGate is the merge-gate CI conclusion a source-control adapter's
// auto-merge precondition read returns.
type CIGate string

const (
	// CIGateSuccess indicates every required check completed without a
	// failing conclusion.
	CIGateSuccess CIGate = "success"

	// CIGatePending indicates at least one required check has not
	// completed and none has failed.
	CIGatePending CIGate = "pending"

	// CIGateFailing indicates at least one required check completed
	// with a failing conclusion.
	CIGateFailing CIGate = "failing"

	// CIGateAbsent indicates the gate read found no required checks at
	// all, which a caller treats as "nothing to wait on" rather than as
	// a passing or failing verdict.
	CIGateAbsent CIGate = ""
)

// IsFailingConclusion reports whether conclusion is one of the two
// conclusions the aggregate and the merge gate both treat as failing.
//
// Callers that need the rule for something other than a verdict, such as
// selecting which run's log to fetch, use this rather than restating the
// conclusion set.
func IsFailingConclusion(conclusion domain.CheckConclusion) bool {
	switch conclusion {
	case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut:
		return true
	default:
		return false
	}
}

// isInconclusiveConclusion reports whether conclusion means the run
// reached no result, so it is neither a passing nor a failing statement
// about the commit and therefore withholds a passing verdict without
// producing a failing one.
//
// Adding a conclusion to this predicate withholds green from every
// reader that folds the normalized run set: the CI verdict on every
// forge, and the merge gate wherever it derives from
// [AggregateCIStatus]. A forge whose merge gate maps a platform-level
// pipeline aggregate directly does not pass through this predicate, and
// has to be kept in step at that mapping instead.
func isInconclusiveConclusion(conclusion domain.CheckConclusion) bool {
	return conclusion == domain.CheckConclusionCancelled
}

// AggregateCIStatus reports the pipeline verdict a normalized check-run
// set implies, answering in this order: failing when any run's
// conclusion satisfies [IsFailingConclusion]; otherwise pending when any
// run has not completed or any completed run's conclusion satisfies
// isInconclusiveConclusion; otherwise passing. The empty or nil set
// answers pending. A completed run whose conclusion is cancelled holds
// the aggregate at pending unless another run forces failing.
func AggregateCIStatus(runs []domain.CheckRun) domain.CIStatus {
	tally := TallyCheckRuns(runs)
	switch {
	case tally.Failing > 0:
		return domain.CIStatusFailing
	case tally.Pending > 0, len(runs) == 0:
		return domain.CIStatusPending
	default:
		return domain.CIStatusPassing
	}
}

// CheckRunTally counts a check-run set by the outcome [AggregateCIStatus]
// reads from each run; every run is counted once.
type CheckRunTally struct {
	Failing int
	Pending int
	Passing int
}

// TallyCheckRuns counts a run as failing when [IsFailingConclusion] holds,
// otherwise pending when it has not completed or concluded cancelled,
// otherwise passing.
func TallyCheckRuns(runs []domain.CheckRun) CheckRunTally {
	var tally CheckRunTally
	for _, run := range runs {
		switch {
		case IsFailingConclusion(run.Conclusion):
			tally.Failing++
		case run.Status != domain.CheckRunStatusCompleted, isInconclusiveConclusion(run.Conclusion):
			tally.Pending++
		default:
			tally.Passing++
		}
	}
	return tally
}

// FailingCount counts the runs AggregateCIStatus treats as failing.
func FailingCount(runs []domain.CheckRun) int {
	return TallyCheckRuns(runs).Failing
}

// MergeGate reports the merge-gate conclusion a normalized check-run set
// implies, over the same rule AggregateCIStatus applies, so one forge
// cannot answer "is CI green" two ways. It returns CIGateAbsent for an
// empty set, which is the "no required checks exist" signal a
// source-control adapter's CI read documents.
func MergeGate(runs []domain.CheckRun) CIGate {
	if len(runs) == 0 {
		return CIGateAbsent
	}
	switch AggregateCIStatus(runs) {
	case domain.CIStatusFailing:
		return CIGateFailing
	case domain.CIStatusPassing:
		return CIGateSuccess
	default:
		return CIGatePending
	}
}
