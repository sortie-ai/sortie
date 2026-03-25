package domain

// Metrics abstracts metric recording so the orchestrator and adapter
// packages remain decoupled from any concrete telemetry library. The
// Observability Layer (internal/server) provides a Prometheus-backed
// implementation; a [NoopMetrics] implementation is provided for use
// when the HTTP server is disabled and in unit tests. All methods
// must be safe for concurrent use. Implementations must not block or
// perform I/O beyond in-memory counter/gauge mutation.
type Metrics interface {
	// --- Gauges (point-in-time state) ---

	// SetRunningSessions records the current number of running agent
	// sessions (sortie_sessions_running gauge).
	SetRunningSessions(n int)

	// SetRetryingSessions records the current number of issues
	// awaiting retry (sortie_sessions_retrying gauge).
	SetRetryingSessions(n int)

	// SetAvailableSlots records the remaining dispatch slots
	// (sortie_slots_available gauge).
	SetAvailableSlots(n int)

	// SetActiveSessionsElapsed records the sum of wall-clock elapsed
	// seconds across all running sessions
	// (sortie_active_sessions_elapsed_seconds gauge).
	SetActiveSessionsElapsed(seconds float64)

	// --- Counters (monotonically increasing) ---

	// AddTokens increments the cumulative token counter by count.
	// tokenType is "input" or "output"
	// (sortie_tokens_total{type} counter).
	AddTokens(tokenType string, count int64)

	// AddAgentRuntime increments the cumulative agent runtime counter
	// (sortie_agent_runtime_seconds_total counter).
	AddAgentRuntime(seconds float64)

	// IncDispatches increments the dispatch attempt counter.
	// outcome is "success" or "error"
	// (sortie_dispatches_total{outcome} counter).
	IncDispatches(outcome string)

	// IncWorkerExits increments the worker exit counter.
	// exitType is "normal", "error", or "cancelled"
	// (sortie_worker_exits_total{exit_type} counter).
	IncWorkerExits(exitType string)

	// IncRetries increments the retry scheduling counter.
	// trigger is "error", "continuation", "timer", or "stall"
	// (sortie_retries_total{trigger} counter).
	IncRetries(trigger string)

	// IncReconciliationActions increments the reconciliation outcome
	// counter. action is "stop", "cleanup", or "keep"
	// (sortie_reconciliation_actions_total{action} counter).
	IncReconciliationActions(action string)

	// IncPollCycles increments the poll tick outcome counter.
	// result is "success", "error", or "skipped"
	// (sortie_poll_cycles_total{result} counter).
	IncPollCycles(result string)

	// IncTrackerRequests increments the tracker adapter API call
	// counter. operation is one of "fetch_candidates", "fetch_issue",
	// "fetch_comments", "fetch_by_states", "fetch_states_by_ids",
	// "fetch_states_by_identifiers", "transition".
	// result is "success" or "error"
	// (sortie_tracker_requests_total{operation,result} counter).
	IncTrackerRequests(operation string, result string)

	// IncHandoffTransitions increments the handoff state transition
	// outcome counter. result is "success", "error", or "skipped"
	// (sortie_handoff_transitions_total{result} counter).
	IncHandoffTransitions(result string)

	// --- Histograms (distributions) ---

	// ObservePollDuration records the duration of a complete poll
	// cycle in seconds (sortie_poll_duration_seconds histogram).
	ObservePollDuration(seconds float64)

	// ObserveWorkerDuration records the wall-clock time of a worker
	// session in seconds. exitType is "normal", "error", or
	// "cancelled" (sortie_worker_duration_seconds{exit_type}
	// histogram).
	ObserveWorkerDuration(exitType string, seconds float64)
}

// NoopMetrics is a [Metrics] implementation where every method is a
// no-op. Used when the HTTP server is disabled and in unit tests that
// do not assert on metric values.
type NoopMetrics struct{}

var _ Metrics = (*NoopMetrics)(nil)

func (*NoopMetrics) SetRunningSessions(int)                {}
func (*NoopMetrics) SetRetryingSessions(int)               {}
func (*NoopMetrics) SetAvailableSlots(int)                 {}
func (*NoopMetrics) SetActiveSessionsElapsed(float64)      {}
func (*NoopMetrics) AddTokens(string, int64)               {}
func (*NoopMetrics) AddAgentRuntime(float64)               {}
func (*NoopMetrics) IncDispatches(string)                  {}
func (*NoopMetrics) IncWorkerExits(string)                 {}
func (*NoopMetrics) IncRetries(string)                     {}
func (*NoopMetrics) IncReconciliationActions(string)       {}
func (*NoopMetrics) IncPollCycles(string)                  {}
func (*NoopMetrics) IncTrackerRequests(string, string)     {}
func (*NoopMetrics) IncHandoffTransitions(string)          {}
func (*NoopMetrics) ObservePollDuration(float64)           {}
func (*NoopMetrics) ObserveWorkerDuration(string, float64) {}

// MetricsSetter is implemented by adapters that accept a [Metrics]
// recorder for self-instrumentation. The wiring code calls SetMetrics
// after adapter construction, before orchestrator operations; some
// startup cleanup calls on the adapter may run before metrics are
// configured. Not safe to call concurrently with adapter operations.
type MetricsSetter interface {
	SetMetrics(m Metrics)
}
