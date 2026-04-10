package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/sortie-ai/sortie/internal/domain"
)

var _ domain.Metrics = (*PromMetrics)(nil)

// PromMetrics implements [domain.Metrics] using prometheus/client_golang
// collectors registered on a dedicated [prometheus.Registry]. Construct
// via [NewPromMetrics]. All methods are safe for concurrent use.
type PromMetrics struct {
	registry *prometheus.Registry

	sessionsRunning       prometheus.Gauge
	sessionsRetrying      prometheus.Gauge
	slotsAvailable        prometheus.Gauge
	activeSessionsElapsed prometheus.Gauge

	tokensTotal            *prometheus.CounterVec
	agentRuntimeTotal      prometheus.Counter
	dispatchesTotal        *prometheus.CounterVec
	workerExitsTotal       *prometheus.CounterVec
	retriesTotal           *prometheus.CounterVec
	reconciliationActions  *prometheus.CounterVec
	pollCyclesTotal        *prometheus.CounterVec
	trackerRequestsTotal   *prometheus.CounterVec
	handoffTransitions     *prometheus.CounterVec
	dispatchTransitions    *prometheus.CounterVec
	trackerCommentsTotal   *prometheus.CounterVec
	toolCallsTotal         *prometheus.CounterVec
	ciStatusChecksTotal    *prometheus.CounterVec
	ciEscalationsTotal     *prometheus.CounterVec
	reviewChecksTotal      *prometheus.CounterVec
	reviewEscalationsTotal *prometheus.CounterVec

	selfReviewIterationsTotal      *prometheus.CounterVec
	selfReviewSessionsTotal        *prometheus.CounterVec
	selfReviewVerificationDuration *prometheus.HistogramVec
	selfReviewCapReachedTotal      prometheus.Counter

	pollDuration   prometheus.Histogram
	workerDuration *prometheus.HistogramVec

	sshHostUsage *prometheus.GaugeVec
}

// NewPromMetrics creates a [PromMetrics] that registers all Sortie
// collectors on a dedicated [prometheus.Registry]. The registry is
// accessible via [PromMetrics.Registry] for handler construction.
// version and goVersion populate the sortie_build_info gauge.
func NewPromMetrics(version, goVersion string) *PromMetrics {
	if version == "" {
		version = "dev"
	}

	reg := prometheus.NewRegistry()

	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())

	sessionsRunning := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "sessions_running",
		Help:      "Number of currently running agent sessions.",
	})

	sessionsRetrying := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "sessions_retrying",
		Help:      "Number of issues awaiting session retry.",
	})

	slotsAvailable := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "slots_available",
		Help:      "Remaining dispatch slots.",
	})

	activeSessionsElapsed := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "active_sessions_elapsed_seconds",
		Help:      "Sum of wall-clock elapsed seconds across all running sessions.",
	})

	tokensTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "tokens_total",
		Help:      "Cumulative LLM tokens consumed.",
	}, []string{"type"})

	agentRuntimeTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "agent_runtime_seconds_total",
		Help:      "Cumulative agent runtime in seconds.",
	})

	dispatchesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "dispatches_total",
		Help:      "Dispatch attempts and their outcomes.",
	}, []string{"outcome"})

	workerExitsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "worker_exits_total",
		Help:      "Worker exit events by type.",
	}, []string{"exit_type"})

	retriesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "retries_total",
		Help:      "Retry scheduling events by trigger.",
	}, []string{"trigger"})

	reconciliationActions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "reconciliation_actions_total",
		Help:      "Reconciliation outcomes per issue.",
	}, []string{"action"})

	pollCyclesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "poll_cycles_total",
		Help:      "Poll tick outcomes.",
	}, []string{"result"})

	trackerRequestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "tracker_requests_total",
		Help:      "Tracker adapter API calls.",
	}, []string{"operation", "result"})

	handoffTransitions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "handoff_transitions_total",
		Help:      "Handoff state transition outcomes.",
	}, []string{"result"})

	dispatchTransitions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "dispatch_transitions_total",
		Help:      "Dispatch-time in-progress transition attempts.",
	}, []string{"result"})

	trackerCommentsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "tracker_comments_total",
		Help:      "Tracker comment attempts by lifecycle point and result.",
	}, []string{"lifecycle", "result"})

	toolCallsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "tool_calls_total",
		Help:      "Agent tool call completions.",
	}, []string{"tool", "result"})

	pollDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "sortie",
		Name:      "poll_duration_seconds",
		Help:      "Time per complete poll cycle.",
		Buckets:   prometheus.ExponentialBuckets(0.1, 2, 10),
	})

	workerDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sortie",
		Name:      "worker_duration_seconds",
		Help:      "Wall-clock time per worker session.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 12),
	}, []string{"exit_type"})

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "build_info",
		Help:      "Build metadata for target identification.",
	}, []string{"version", "go_version"})
	buildInfo.WithLabelValues(version, goVersion).Set(1)

	sshHostUsage := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sortie",
		Name:      "ssh_host_usage",
		Help:      "Current session count per SSH host.",
	}, []string{"host"})

	ciStatusChecksTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "ci_status_checks_total",
		Help:      "CI status check outcomes.",
	}, []string{"result"})

	ciEscalationsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "ci_escalations_total",
		Help:      "CI escalation actions taken.",
	}, []string{"action"})

	reviewChecksTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "review_checks_total",
		Help:      "Review comment check outcomes.",
	}, []string{"result"})

	reviewEscalationsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "review_escalations_total",
		Help:      "Review reaction escalation outcomes.",
	}, []string{"action"})

	selfReviewIterationsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "self_review_iterations_total",
		Help:      "Self-review iterations by verdict.",
	}, []string{"verdict"})

	selfReviewSessionsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "self_review_sessions_total",
		Help:      "Self-review sessions by final verdict.",
	}, []string{"final_verdict"})

	selfReviewVerificationDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sortie",
		Name:      "self_review_verification_duration_seconds",
		Help:      "Per-command verification duration.",
		Buckets:   prometheus.ExponentialBuckets(10, 2, 12),
	}, []string{"command"})

	selfReviewCapReachedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "sortie",
		Name:      "self_review_cap_reached_total",
		Help:      "Self-review sessions that hit the iteration cap.",
	})

	reg.MustRegister(
		sessionsRunning,
		sessionsRetrying,
		slotsAvailable,
		activeSessionsElapsed,
		tokensTotal,
		agentRuntimeTotal,
		dispatchesTotal,
		workerExitsTotal,
		retriesTotal,
		reconciliationActions,
		pollCyclesTotal,
		trackerRequestsTotal,
		handoffTransitions,
		dispatchTransitions,
		trackerCommentsTotal,
		toolCallsTotal,
		pollDuration,
		workerDuration,
		buildInfo,
		sshHostUsage,
		ciStatusChecksTotal,
		ciEscalationsTotal,
		reviewChecksTotal,
		reviewEscalationsTotal,
		selfReviewIterationsTotal,
		selfReviewSessionsTotal,
		selfReviewVerificationDuration,
		selfReviewCapReachedTotal,
	)

	return &PromMetrics{
		registry:                       reg,
		sessionsRunning:                sessionsRunning,
		sessionsRetrying:               sessionsRetrying,
		slotsAvailable:                 slotsAvailable,
		activeSessionsElapsed:          activeSessionsElapsed,
		tokensTotal:                    tokensTotal,
		agentRuntimeTotal:              agentRuntimeTotal,
		dispatchesTotal:                dispatchesTotal,
		workerExitsTotal:               workerExitsTotal,
		retriesTotal:                   retriesTotal,
		reconciliationActions:          reconciliationActions,
		pollCyclesTotal:                pollCyclesTotal,
		trackerRequestsTotal:           trackerRequestsTotal,
		handoffTransitions:             handoffTransitions,
		dispatchTransitions:            dispatchTransitions,
		trackerCommentsTotal:           trackerCommentsTotal,
		toolCallsTotal:                 toolCallsTotal,
		pollDuration:                   pollDuration,
		workerDuration:                 workerDuration,
		sshHostUsage:                   sshHostUsage,
		ciStatusChecksTotal:            ciStatusChecksTotal,
		ciEscalationsTotal:             ciEscalationsTotal,
		reviewChecksTotal:              reviewChecksTotal,
		reviewEscalationsTotal:         reviewEscalationsTotal,
		selfReviewIterationsTotal:      selfReviewIterationsTotal,
		selfReviewSessionsTotal:        selfReviewSessionsTotal,
		selfReviewVerificationDuration: selfReviewVerificationDuration,
		selfReviewCapReachedTotal:      selfReviewCapReachedTotal,
	}
}

// Registry returns the dedicated [prometheus.Registry] used by this
// [PromMetrics] instance. Pass this to promhttp.HandlerFor to
// construct the /metrics HTTP handler.
func (p *PromMetrics) Registry() *prometheus.Registry {
	return p.registry
}

// SetRunningSessions records the current number of running agent sessions.
func (p *PromMetrics) SetRunningSessions(n int) {
	p.sessionsRunning.Set(float64(n))
}

// SetRetryingSessions records the current number of issues awaiting retry.
func (p *PromMetrics) SetRetryingSessions(n int) {
	p.sessionsRetrying.Set(float64(n))
}

// SetAvailableSlots records the remaining dispatch slots.
func (p *PromMetrics) SetAvailableSlots(n int) {
	p.slotsAvailable.Set(float64(n))
}

// SetActiveSessionsElapsed records the sum of wall-clock elapsed seconds
// across all running sessions.
func (p *PromMetrics) SetActiveSessionsElapsed(seconds float64) {
	p.activeSessionsElapsed.Set(seconds)
}

// AddTokens increments the cumulative token counter by count. Negative
// values are silently clamped to zero to prevent Prometheus counter panics.
func (p *PromMetrics) AddTokens(tokenType string, count int64) {
	if count <= 0 {
		return
	}
	p.tokensTotal.WithLabelValues(tokenType).Add(float64(count))
}

// AddAgentRuntime increments the cumulative agent runtime counter. Negative
// values are silently clamped to zero to prevent Prometheus counter panics.
func (p *PromMetrics) AddAgentRuntime(seconds float64) {
	if seconds <= 0 {
		return
	}
	p.agentRuntimeTotal.Add(seconds)
}

// IncDispatches increments the dispatch attempt counter.
func (p *PromMetrics) IncDispatches(outcome string) {
	p.dispatchesTotal.WithLabelValues(outcome).Inc()
}

// IncWorkerExits increments the worker exit counter.
func (p *PromMetrics) IncWorkerExits(exitType string) {
	p.workerExitsTotal.WithLabelValues(exitType).Inc()
}

// IncRetries increments the retry scheduling counter.
func (p *PromMetrics) IncRetries(trigger string) {
	p.retriesTotal.WithLabelValues(trigger).Inc()
}

// IncReconciliationActions increments the reconciliation outcome counter.
func (p *PromMetrics) IncReconciliationActions(action string) {
	p.reconciliationActions.WithLabelValues(action).Inc()
}

// IncPollCycles increments the poll tick outcome counter.
func (p *PromMetrics) IncPollCycles(outcome string) {
	p.pollCyclesTotal.WithLabelValues(outcome).Inc()
}

// IncTrackerRequests increments the tracker adapter API call counter.
func (p *PromMetrics) IncTrackerRequests(operation, outcome string) {
	p.trackerRequestsTotal.WithLabelValues(operation, outcome).Inc()
}

// IncHandoffTransitions increments the handoff state transition outcome counter.
func (p *PromMetrics) IncHandoffTransitions(outcome string) {
	p.handoffTransitions.WithLabelValues(outcome).Inc()
}

// IncDispatchTransitions increments the dispatch-time in-progress transition counter.
func (p *PromMetrics) IncDispatchTransitions(outcome string) {
	p.dispatchTransitions.WithLabelValues(outcome).Inc()
}

// IncTrackerComments increments the tracker comment attempt counter.
func (p *PromMetrics) IncTrackerComments(lifecycle, outcome string) {
	p.trackerCommentsTotal.WithLabelValues(lifecycle, outcome).Inc()
}

// IncToolCalls increments the tool call completion counter.
func (p *PromMetrics) IncToolCalls(tool, outcome string) {
	p.toolCallsTotal.WithLabelValues(tool, outcome).Inc()
}

// ObservePollDuration records the duration of a complete poll cycle in seconds.
func (p *PromMetrics) ObservePollDuration(seconds float64) {
	p.pollDuration.Observe(seconds)
}

// ObserveWorkerDuration records the wall-clock time of a worker session in seconds.
func (p *PromMetrics) ObserveWorkerDuration(exitType string, seconds float64) {
	p.workerDuration.WithLabelValues(exitType).Observe(seconds)
}

// SetSSHHostUsage records the current session count for the given SSH host.
func (p *PromMetrics) SetSSHHostUsage(host string, count int) {
	p.sshHostUsage.WithLabelValues(host).Set(float64(count))
}

// IncCIStatusChecks increments the CI status check outcome counter.
func (p *PromMetrics) IncCIStatusChecks(result string) {
	p.ciStatusChecksTotal.WithLabelValues(result).Inc()
}

// IncCIEscalations increments the CI escalation action counter.
func (p *PromMetrics) IncCIEscalations(action string) {
	p.ciEscalationsTotal.WithLabelValues(action).Inc()
}

// IncReviewChecks increments the review comment check outcome counter.
func (p *PromMetrics) IncReviewChecks(result string) {
	p.reviewChecksTotal.WithLabelValues(result).Inc()
}

// IncReviewEscalations increments the review escalation action counter.
func (p *PromMetrics) IncReviewEscalations(action string) {
	p.reviewEscalationsTotal.WithLabelValues(action).Inc()
}

// IncSelfReviewIterations increments the review iteration counter.
func (p *PromMetrics) IncSelfReviewIterations(verdict string) {
	p.selfReviewIterationsTotal.WithLabelValues(verdict).Inc()
}

// IncSelfReviewSessions increments the review session counter.
func (p *PromMetrics) IncSelfReviewSessions(finalVerdict string) {
	p.selfReviewSessionsTotal.WithLabelValues(finalVerdict).Inc()
}

// ObserveSelfReviewVerificationDuration records the duration of a
// verification command.
func (p *PromMetrics) ObserveSelfReviewVerificationDuration(command string, seconds float64) {
	if len(command) > 64 {
		command = command[:64]
	}
	p.selfReviewVerificationDuration.WithLabelValues(command).Observe(seconds)
}

// IncSelfReviewCapReached increments the cap-reached counter.
func (p *PromMetrics) IncSelfReviewCapReached() {
	p.selfReviewCapReachedTotal.Inc()
}
