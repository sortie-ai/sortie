package domain

// Metrics abstracts metric recording so the orchestrator and adapter
// packages remain decoupled from any concrete telemetry library.
//
// The observability layer (internal/server) provides a Prometheus-backed
// implementation; a [NoopMetrics] implementation is provided for use
// when the HTTP server is disabled and in unit tests. All methods
// must be safe for concurrent use. Implementations must not block or
// perform I/O beyond in-memory counter/gauge mutation.
type Metrics interface {
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

	// AddTokens increments the cumulative token counter by count.
	// tokenType is "input", "output", or "cache_read"
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
	// exitType is "normal", "error", "cancelled", or "soft_stop"
	// (sortie_worker_exits_total{exit_type} counter).
	IncWorkerExits(exitType string)

	// IncRetries increments the retry scheduling counter.
	// trigger is "error", "continuation", "timer", "stall", or
	// "ci_fix" (sortie_retries_total{trigger} counter).
	IncRetries(trigger string)

	// IncReconciliationActions increments the reconciliation outcome
	// counter. action is "stop", "cleanup", "keep", "sweep_cleanup", or
	// "sweep_expired" (sortie_reconciliation_actions_total{action}
	// counter).
	IncReconciliationActions(action string)

	// IncPollCycles increments the poll tick outcome counter.
	// result is "success", "error", or "skipped"
	// (sortie_poll_cycles_total{result} counter).
	IncPollCycles(result string)

	// IncTrackerRequests increments the tracker adapter API call
	// counter. operation is one of "fetch_candidates", "fetch_issue",
	// "fetch_comments", "fetch_by_states", "fetch_states_by_ids",
	// "fetch_states_by_identifiers", "fetch_blockers", "transition",
	// "comment", or "add_label".
	// result is "success" or "error"
	// (sortie_tracker_requests_total{operation,result} counter).
	IncTrackerRequests(operation string, result string)

	// IncHandoffTransitions increments the handoff state transition
	// outcome counter. result is "success", "error", "skipped", or
	// "withheld" (sortie_handoff_transitions_total{result} counter).
	IncHandoffTransitions(result string)

	// IncIssueParks increments the issue park counter. reason is
	// "agent_blocked" or "handoff_absence"
	// (sortie_issue_parks_total{reason} counter).
	IncIssueParks(reason string)

	// IncDispatchTransitions increments the dispatch-time in-progress
	// transition counter. result is "success", "error", or "skipped"
	// (sortie_dispatch_transitions_total{result} counter).
	IncDispatchTransitions(result string)

	// IncTrackerComments increments the tracker comment attempt counter.
	// lifecycle includes "dispatch", "completion", "failure", and
	// "budget_hold", among others the project may add.
	// result is "success" or "error"
	// (sortie_tracker_comments_total{lifecycle,result} counter).
	IncTrackerComments(lifecycle string, result string)

	// IncToolCalls increments the tool call completion counter.
	// tool is the tool name (e.g. "Bash", "tracker_api").
	// result is "success" or "error"
	// (sortie_tool_calls_total{tool,result} counter).
	IncToolCalls(tool string, result string)

	// ObservePollDuration records the duration of a complete poll
	// cycle in seconds (sortie_poll_duration_seconds histogram).
	ObservePollDuration(seconds float64)

	// ObserveWorkerDuration records the wall-clock time of a worker
	// session in seconds. exitType is "normal", "error", or
	// "cancelled" (sortie_worker_duration_seconds{exit_type}
	// histogram).
	ObserveWorkerDuration(exitType string, seconds float64)

	// SetSSHHostUsage records the current session count for the
	// given SSH host (sortie_ssh_host_usage{host} gauge).
	SetSSHHostUsage(host string, count int)

	// IncCIStatusChecks increments the CI status check counter.
	// result is "passing", "pending", "failing", or "error"
	// (sortie_ci_status_checks_total{result} counter).
	IncCIStatusChecks(result string)

	// IncCIEscalations increments the CI escalation counter.
	// action is "label", "comment", or "error"
	// (sortie_ci_escalations_total{action} counter).
	IncCIEscalations(action string)

	// IncSelfReviewIterations increments the review iteration counter.
	// verdict is "pass", "iterate", or "none"
	// (sortie_self_review_iterations_total{verdict} counter).
	IncSelfReviewIterations(verdict string)

	// IncSelfReviewSessions increments the review session counter.
	// finalVerdict is "pass", "iterate", or "none"
	// (sortie_self_review_sessions_total{final_verdict} counter).
	IncSelfReviewSessions(finalVerdict string)

	// ObserveSelfReviewVerificationDuration records the duration of a
	// single verification command in seconds.
	// command is truncated to the first 64 characters before use as label
	// (sortie_self_review_verification_duration_seconds{command} histogram).
	ObserveSelfReviewVerificationDuration(command string, seconds float64)

	// IncSelfReviewCapReached increments the cap-reached counter
	// (sortie_self_review_cap_reached_total counter).
	IncSelfReviewCapReached()

	// IncReviewChecks increments the review comment check counter.
	// result is "dispatched", "error", or "skipped"
	// (sortie_review_checks_total{result} counter).
	IncReviewChecks(result string)

	// IncReviewEscalations increments the review escalation counter.
	// action is "label", "comment", or "error"
	// (sortie_review_escalations_total{action} counter).
	IncReviewEscalations(action string)

	// IncBotReviewChecks increments the bot review comment check counter.
	// result is "dispatched", "error", or "skipped"
	// (sortie_bot_review_checks_total{result} counter).
	IncBotReviewChecks(result string)

	// IncBotReviewEscalations increments the bot review escalation
	// counter. action is "label", "comment", or "error"
	// (sortie_bot_review_escalations_total{action} counter).
	IncBotReviewEscalations(action string)

	// IncAutoMergeReactions increments the auto-merge reaction outcome
	// counter. result is "merged", "error", or "escalated"
	// (sortie_reactions_auto_merge_total{result} counter).
	IncAutoMergeReactions(result string)

	// IncMergeConflictChecks increments the merge-conflict check counter.
	// result is "dispatched", "error", "unknown", or "clear"
	// (sortie_merge_conflict_checks_total{result} counter).
	IncMergeConflictChecks(result string)

	// IncMergeConflictEscalations increments the merge-conflict
	// escalation counter. action is "label", "comment", or "error"
	// (sortie_merge_conflict_escalations_total{action} counter).
	IncMergeConflictEscalations(action string)

	// IncDispatchRuleMatch increments the dispatch rule match counter.
	// layer is one of "rule", "default", or "fallback" identifying
	// which resolution layer produced the selection. rule is the
	// matched rule name; empty rule names report as "<none>" to keep
	// the label cardinality bounded.
	// (sortie_dispatch_rule_match_total{layer,rule} counter).
	IncDispatchRuleMatch(layer string, rule string)

	// IncCandidateHolds increments the counter of candidates the
	// dispatch loop held. reason is "blocked_by",
	// "blockers_unresolved", "blockers_not_read", or
	// "blockers_incomplete"
	// (sortie_candidate_holds_total{reason} counter).
	IncCandidateHolds(reason string)

	// IncBudgetExhaustions increments the counter of issues that entered
	// the per-issue budget exhausted set. reason is one of the
	// dispatch-gate reason values, and the vocabulary may gain a third
	// value later. Called once per entry from whichever lane discovered
	// it (sortie_budget_exhaustions_total{reason} counter).
	IncBudgetExhaustions(reason string)

	// SetBudgetExhaustedIssues records how many issues are currently
	// held out of dispatch under the given reason, one of the
	// dispatch-gate reason values whose vocabulary may gain a third
	// value later. Called for every known reason value on every
	// recompute, so a reason that no longer holds any issue reports
	// zero rather than keeping its last value
	// (sortie_budget_exhausted_issues{reason} gauge).
	SetBudgetExhaustedIssues(reason string, count int)
}

// NoopMetrics is a [Metrics] implementation where every method is a no-op.
//
// Used when the HTTP server is disabled and in unit tests that do not
// assert on metric values.
type NoopMetrics struct{}

var _ Metrics = (*NoopMetrics)(nil)

func (*NoopMetrics) SetRunningSessions(int)                                {}
func (*NoopMetrics) SetRetryingSessions(int)                               {}
func (*NoopMetrics) SetAvailableSlots(int)                                 {}
func (*NoopMetrics) SetActiveSessionsElapsed(float64)                      {}
func (*NoopMetrics) AddTokens(string, int64)                               {}
func (*NoopMetrics) AddAgentRuntime(float64)                               {}
func (*NoopMetrics) IncDispatches(string)                                  {}
func (*NoopMetrics) IncWorkerExits(string)                                 {}
func (*NoopMetrics) IncRetries(string)                                     {}
func (*NoopMetrics) IncReconciliationActions(string)                       {}
func (*NoopMetrics) IncPollCycles(string)                                  {}
func (*NoopMetrics) IncTrackerRequests(string, string)                     {}
func (*NoopMetrics) IncHandoffTransitions(string)                          {}
func (*NoopMetrics) IncIssueParks(string)                                  {}
func (*NoopMetrics) IncDispatchTransitions(string)                         {}
func (*NoopMetrics) IncTrackerComments(string, string)                     {}
func (*NoopMetrics) IncToolCalls(string, string)                           {}
func (*NoopMetrics) ObservePollDuration(float64)                           {}
func (*NoopMetrics) ObserveWorkerDuration(string, float64)                 {}
func (*NoopMetrics) SetSSHHostUsage(string, int)                           {}
func (*NoopMetrics) IncCIStatusChecks(string)                              {}
func (*NoopMetrics) IncCIEscalations(string)                               {}
func (*NoopMetrics) IncSelfReviewIterations(string)                        {}
func (*NoopMetrics) IncSelfReviewSessions(string)                          {}
func (*NoopMetrics) ObserveSelfReviewVerificationDuration(string, float64) {}
func (*NoopMetrics) IncSelfReviewCapReached()                              {}
func (*NoopMetrics) IncReviewChecks(string)                                {}
func (*NoopMetrics) IncReviewEscalations(string)                           {}
func (*NoopMetrics) IncBotReviewChecks(string)                             {}
func (*NoopMetrics) IncBotReviewEscalations(string)                        {}
func (*NoopMetrics) IncAutoMergeReactions(string)                          {}
func (*NoopMetrics) IncMergeConflictChecks(string)                         {}
func (*NoopMetrics) IncMergeConflictEscalations(string)                    {}
func (*NoopMetrics) IncDispatchRuleMatch(string, string)                   {}
func (*NoopMetrics) IncCandidateHolds(string)                              {}
func (*NoopMetrics) IncBudgetExhaustions(string)                           {}
func (*NoopMetrics) SetBudgetExhaustedIssues(string, int)                  {}

// MetricsSetter is implemented by adapters that accept a [Metrics]
// recorder for self-instrumentation.
//
// The wiring code calls SetMetrics after adapter construction, before
// orchestrator operations; some startup cleanup calls on the adapter
// may run before metrics are configured. Not safe to call concurrently
// with adapter operations.
type MetricsSetter interface {
	SetMetrics(m Metrics)
}
