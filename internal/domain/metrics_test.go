package domain

import "testing"

func TestNoopMetricsSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var m Metrics = &NoopMetrics{}

	// Gauges
	m.SetRunningSessions(5)
	m.SetRetryingSessions(3)
	m.SetAvailableSlots(2)
	m.SetActiveSessionsElapsed(123.45)

	// Counters — AddTokens
	m.AddTokens("input", 1000)
	m.AddTokens("output", 500)

	// Counters — AddAgentRuntime
	m.AddAgentRuntime(60.5)

	// Counters — IncDispatches
	m.IncDispatches("success")
	m.IncDispatches("error")

	// Counters — IncWorkerExits
	m.IncWorkerExits("normal")
	m.IncWorkerExits("error")
	m.IncWorkerExits("cancelled")

	// Counters — IncRetries
	m.IncRetries("error")
	m.IncRetries("continuation")
	m.IncRetries("timer")
	m.IncRetries("stall")

	// Counters — IncReconciliationActions
	m.IncReconciliationActions("stop")
	m.IncReconciliationActions("cleanup")
	m.IncReconciliationActions("keep")

	// Counters — IncPollCycles
	m.IncPollCycles("success")
	m.IncPollCycles("error")
	m.IncPollCycles("skipped")

	// Counters — IncTrackerRequests (all 7 operations)
	m.IncTrackerRequests("fetch_candidates", "success")
	m.IncTrackerRequests("fetch_issue", "error")
	m.IncTrackerRequests("fetch_comments", "success")
	m.IncTrackerRequests("fetch_by_states", "success")
	m.IncTrackerRequests("fetch_states_by_ids", "error")
	m.IncTrackerRequests("fetch_states_by_identifiers", "success")
	m.IncTrackerRequests("transition", "error")

	// Counters — IncHandoffTransitions
	m.IncHandoffTransitions("success")
	m.IncHandoffTransitions("error")
	m.IncHandoffTransitions("skipped")

	// Histograms — ObservePollDuration
	m.ObservePollDuration(1.23)

	// Histograms — ObserveWorkerDuration
	m.ObserveWorkerDuration("normal", 300.5)
	m.ObserveWorkerDuration("error", 10.0)
	m.ObserveWorkerDuration("cancelled", 45.2)
}
