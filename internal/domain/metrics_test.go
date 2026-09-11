package domain

import "testing"

func TestNoopMetricsSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var m Metrics = &NoopMetrics{}

	m.SetRunningSessions(5)
	m.SetRetryingSessions(3)
	m.SetAvailableSlots(2)
	m.SetActiveSessionsElapsed(123.45)

	m.AddTokens("input", 1000)
	m.AddTokens("output", 500)

	m.AddAgentRuntime(60.5)

	m.IncDispatches("success")
	m.IncDispatches("error")

	m.IncWorkerExits("normal")
	m.IncWorkerExits("error")
	m.IncWorkerExits("cancelled")

	m.IncRetries("error")
	m.IncRetries("continuation")
	m.IncRetries("timer")
	m.IncRetries("stall")

	m.IncReconciliationActions("stop")
	m.IncReconciliationActions("cleanup")
	m.IncReconciliationActions("keep")

	m.IncPollCycles("success")
	m.IncPollCycles("error")
	m.IncPollCycles("skipped")

	m.IncTrackerRequests("fetch_candidates", "success")
	m.IncTrackerRequests("fetch_issue", "error")
	m.IncTrackerRequests("fetch_comments", "success")
	m.IncTrackerRequests("fetch_by_states", "success")
	m.IncTrackerRequests("fetch_states_by_ids", "error")
	m.IncTrackerRequests("fetch_states_by_identifiers", "success")
	m.IncTrackerRequests("transition", "error")
	m.IncTrackerRequests("comment", "success")

	m.IncHandoffTransitions("success")
	m.IncHandoffTransitions("error")
	m.IncHandoffTransitions("skipped")

	m.IncDispatchTransitions("success")
	m.IncDispatchTransitions("error")

	m.IncToolCalls("Bash", "success")
	m.IncToolCalls("Read", "error")

	m.ObservePollDuration(1.23)

	m.ObserveWorkerDuration("normal", 300.5)
	m.ObserveWorkerDuration("error", 10.0)
	m.ObserveWorkerDuration("cancelled", 45.2)

	m.IncBotReviewChecks("dispatched")
	m.IncBotReviewChecks("error")
	m.IncBotReviewChecks("skipped")

	m.IncBotReviewEscalations("label")
	m.IncBotReviewEscalations("comment")
	m.IncBotReviewEscalations("error")
}
