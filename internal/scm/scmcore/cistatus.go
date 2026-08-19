// Package scmcore holds the decisions a source-control forge integration
// makes over normalized domain types rather than wire shapes: the CI
// verdict a set of check runs implies, the SCM error a tracker error maps
// to, the merge-conflict promotion and already-merged marker, and the
// bot-author and event-id primitives. It imports internal/domain only, so
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
// forge and both readers, the CI verdict and the merge gate, since both
// derive from [AggregateCIStatus].
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
	if len(runs) == 0 {
		return domain.CIStatusPending
	}

	anyFailing := false
	anyWithholds := false
	for _, run := range runs {
		if run.Status != domain.CheckRunStatusCompleted {
			anyWithholds = true
		} else if isInconclusiveConclusion(run.Conclusion) {
			anyWithholds = true
		}
		if IsFailingConclusion(run.Conclusion) {
			anyFailing = true
		}
	}

	if anyFailing {
		return domain.CIStatusFailing
	}
	if anyWithholds {
		return domain.CIStatusPending
	}
	return domain.CIStatusPassing
}

// FailingCount counts the runs AggregateCIStatus treats as failing.
func FailingCount(runs []domain.CheckRun) int {
	count := 0
	for _, run := range runs {
		if IsFailingConclusion(run.Conclusion) {
			count++
		}
	}
	return count
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
