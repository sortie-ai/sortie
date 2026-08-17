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

// IsFailingConclusion reports whether conclusion is one of the three
// conclusions the aggregate and the merge gate both treat as failing.
//
// Callers that need the rule for something other than a verdict, such as
// selecting which run's log to fetch, use this rather than restating the
// conclusion set.
func IsFailingConclusion(conclusion domain.CheckConclusion) bool {
	switch conclusion {
	case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut, domain.CheckConclusionCancelled:
		return true
	default:
		return false
	}
}

// AggregateCIStatus reports the pipeline verdict a normalized check-run
// set implies: pending for an empty set, failing when any run's
// conclusion is failure, timed_out, or cancelled, passing when every run
// has completed with none of those conclusions, and pending otherwise.
func AggregateCIStatus(runs []domain.CheckRun) domain.CIStatus {
	if len(runs) == 0 {
		return domain.CIStatusPending
	}

	allCompleted := true
	anyFailed := false
	for _, run := range runs {
		if run.Status != domain.CheckRunStatusCompleted {
			allCompleted = false
		}
		if IsFailingConclusion(run.Conclusion) {
			anyFailed = true
		}
	}

	if anyFailed {
		return domain.CIStatusFailing
	}
	if allCompleted {
		return domain.CIStatusPassing
	}
	return domain.CIStatusPending
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
