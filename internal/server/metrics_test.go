package server

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// --- Test helpers ---

func newTestMetrics(t *testing.T) *PromMetrics {
	t.Helper()
	m := NewPromMetrics("1.0.0-test", "go1.26.1")
	if m == nil {
		t.Fatal("NewPromMetrics() returned nil")
	}
	return m
}

func gatherFamilies(t *testing.T, m *PromMetrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Registry().Gather() error: %v", err)
	}
	result := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		result[f.GetName()] = f
	}
	return result
}

func requireFamily(t *testing.T, families map[string]*dto.MetricFamily, name string) *dto.MetricFamily {
	t.Helper()
	f, ok := families[name]
	if !ok {
		t.Fatalf("metric family %q not found in gathered families", name)
	}
	return f
}

func gaugeValue(t *testing.T, families map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	f := requireFamily(t, families, name)
	metrics := f.GetMetric()
	if len(metrics) == 0 {
		t.Fatalf("metric family %q has no metrics", name)
	}
	g := metrics[0].GetGauge()
	if g == nil {
		t.Fatalf("metric family %q: first metric has no gauge", name)
	}
	return g.GetValue()
}

func counterValue(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	f := requireFamily(t, families, name)
	for _, m := range f.GetMetric() {
		if matchLabels(m.GetLabel(), labels) {
			c := m.GetCounter()
			if c == nil {
				t.Fatalf("metric family %q: matching metric has no counter", name)
			}
			return c.GetValue()
		}
	}
	t.Fatalf("metric family %q: no metric matches labels %v", name, labels)
	return 0
}

func histogramStats(t *testing.T, families map[string]*dto.MetricFamily, name string, labels map[string]string) (uint64, float64) {
	t.Helper()
	f := requireFamily(t, families, name)
	for _, m := range f.GetMetric() {
		if matchLabels(m.GetLabel(), labels) {
			h := m.GetHistogram()
			if h == nil {
				t.Fatalf("metric family %q: matching metric has no histogram", name)
			}
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	t.Fatalf("metric family %q: no metric matches labels %v", name, labels)
	return 0, 0
}

func matchLabels(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(want) == 0 && len(pairs) == 0 {
		return true
	}
	if len(want) == 0 {
		return true
	}
	found := 0
	for _, lp := range pairs {
		if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
			found++
		}
	}
	return found == len(want)
}

// --- Tests ---

func TestNewPromMetrics(t *testing.T) {
	t.Parallel()

	m := NewPromMetrics("1.0.0", "go1.26.1")
	if m == nil {
		t.Fatal("NewPromMetrics() returned nil")
	}
	if m.Registry() == nil {
		t.Fatal("Registry() returned nil")
	}

	// Exercise every labeled metric once so they appear in Gather().
	// Prometheus CounterVec/HistogramVec are lazily initialized per
	// label combination and won't appear until first observation.
	m.AddTokens("input", 1)
	m.IncDispatches("success")
	m.IncWorkerExits("normal")
	m.IncRetries("error")
	m.IncReconciliationActions("keep")
	m.IncPollCycles("success")
	m.IncTrackerRequests("fetch_candidates", "success")
	m.IncHandoffTransitions("success")
	m.ObserveWorkerDuration("normal", 1)

	families := gatherFamilies(t, m)

	sortieMetrics := []string{
		"sortie_sessions_running",
		"sortie_sessions_retrying",
		"sortie_slots_available",
		"sortie_active_sessions_elapsed_seconds",
		"sortie_tokens_total",
		"sortie_agent_runtime_seconds_total",
		"sortie_dispatches_total",
		"sortie_worker_exits_total",
		"sortie_retries_total",
		"sortie_reconciliation_actions_total",
		"sortie_poll_cycles_total",
		"sortie_tracker_requests_total",
		"sortie_handoff_transitions_total",
		"sortie_poll_duration_seconds",
		"sortie_worker_duration_seconds",
		"sortie_build_info",
	}

	for _, name := range sortieMetrics {
		if _, ok := families[name]; !ok {
			t.Errorf("expected metric family %q not found", name)
		}
	}

	// Verify Go runtime and process collectors are registered.
	var hasGo, hasProcess bool
	for name := range families {
		if strings.HasPrefix(name, "go_") {
			hasGo = true
		}
		if strings.HasPrefix(name, "process_") {
			hasProcess = true
		}
	}
	if !hasGo {
		t.Error("no go_* metric families found; Go runtime collector not registered")
	}
	if !hasProcess {
		t.Error("no process_* metric families found; process collector not registered")
	}
}

func TestPromMetricsGauges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		metric string
		set    func(m *PromMetrics, v int)
		first  int
		second int
	}{
		{
			name:   "sessions running",
			metric: "sortie_sessions_running",
			set:    func(m *PromMetrics, v int) { m.SetRunningSessions(v) },
			first:  5,
			second: 12,
		},
		{
			name:   "sessions retrying",
			metric: "sortie_sessions_retrying",
			set:    func(m *PromMetrics, v int) { m.SetRetryingSessions(v) },
			first:  3,
			second: 0,
		},
		{
			name:   "slots available",
			metric: "sortie_slots_available",
			set:    func(m *PromMetrics, v int) { m.SetAvailableSlots(v) },
			first:  10,
			second: 7,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestMetrics(t)
			tt.set(m, tt.first)

			families := gatherFamilies(t, m)
			if got := gaugeValue(t, families, tt.metric); got != float64(tt.first) {
				t.Errorf("%s after Set(%d) = %v, want %v", tt.metric, tt.first, got, float64(tt.first))
			}

			tt.set(m, tt.second)
			families = gatherFamilies(t, m)
			if got := gaugeValue(t, families, tt.metric); got != float64(tt.second) {
				t.Errorf("%s after Set(%d) = %v, want %v", tt.metric, tt.second, got, float64(tt.second))
			}
		})
	}

	t.Run("active sessions elapsed", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.SetActiveSessionsElapsed(123.45)

		families := gatherFamilies(t, m)
		if got := gaugeValue(t, families, "sortie_active_sessions_elapsed_seconds"); got != 123.45 {
			t.Errorf("sortie_active_sessions_elapsed_seconds = %v, want 123.45", got)
		}

		m.SetActiveSessionsElapsed(0)
		families = gatherFamilies(t, m)
		if got := gaugeValue(t, families, "sortie_active_sessions_elapsed_seconds"); got != 0 {
			t.Errorf("sortie_active_sessions_elapsed_seconds after Set(0) = %v, want 0", got)
		}
	})
}

func TestPromMetricsCounters(t *testing.T) {
	t.Parallel()

	t.Run("IncDispatches", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncDispatches("success")

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_dispatches_total", map[string]string{"outcome": "success"}); got != 1 {
			t.Errorf("sortie_dispatches_total{outcome=success} = %v, want 1", got)
		}

		m.IncDispatches("success")
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_dispatches_total", map[string]string{"outcome": "success"}); got != 2 {
			t.Errorf("sortie_dispatches_total{outcome=success} = %v, want 2", got)
		}

		m.IncDispatches("error")
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_dispatches_total", map[string]string{"outcome": "error"}); got != 1 {
			t.Errorf("sortie_dispatches_total{outcome=error} = %v, want 1", got)
		}
	})

	t.Run("IncWorkerExits", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncWorkerExits("normal")
		m.IncWorkerExits("error")
		m.IncWorkerExits("cancelled")

		families := gatherFamilies(t, m)
		for _, exitType := range []string{"normal", "error", "cancelled"} {
			if got := counterValue(t, families, "sortie_worker_exits_total", map[string]string{"exit_type": exitType}); got != 1 {
				t.Errorf("sortie_worker_exits_total{exit_type=%s} = %v, want 1", exitType, got)
			}
		}
	})

	t.Run("IncRetries", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncRetries("error")
		m.IncRetries("error")
		m.IncRetries("continuation")

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_retries_total", map[string]string{"trigger": "error"}); got != 2 {
			t.Errorf("sortie_retries_total{trigger=error} = %v, want 2", got)
		}
		if got := counterValue(t, families, "sortie_retries_total", map[string]string{"trigger": "continuation"}); got != 1 {
			t.Errorf("sortie_retries_total{trigger=continuation} = %v, want 1", got)
		}
	})

	t.Run("IncReconciliationActions", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncReconciliationActions("stop")
		m.IncReconciliationActions("cleanup")
		m.IncReconciliationActions("keep")

		families := gatherFamilies(t, m)
		for _, action := range []string{"stop", "cleanup", "keep"} {
			if got := counterValue(t, families, "sortie_reconciliation_actions_total", map[string]string{"action": action}); got != 1 {
				t.Errorf("sortie_reconciliation_actions_total{action=%s} = %v, want 1", action, got)
			}
		}
	})

	t.Run("IncPollCycles", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncPollCycles("success")
		m.IncPollCycles("success")
		m.IncPollCycles("error")

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_poll_cycles_total", map[string]string{"result": "success"}); got != 2 {
			t.Errorf("sortie_poll_cycles_total{result=success} = %v, want 2", got)
		}
		if got := counterValue(t, families, "sortie_poll_cycles_total", map[string]string{"result": "error"}); got != 1 {
			t.Errorf("sortie_poll_cycles_total{result=error} = %v, want 1", got)
		}
	})

	t.Run("IncTrackerRequests", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncTrackerRequests("fetch_candidates", "success")
		m.IncTrackerRequests("fetch_issue", "error")

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_tracker_requests_total", map[string]string{"operation": "fetch_candidates", "result": "success"}); got != 1 {
			t.Errorf("sortie_tracker_requests_total{operation=fetch_candidates,result=success} = %v, want 1", got)
		}
		if got := counterValue(t, families, "sortie_tracker_requests_total", map[string]string{"operation": "fetch_issue", "result": "error"}); got != 1 {
			t.Errorf("sortie_tracker_requests_total{operation=fetch_issue,result=error} = %v, want 1", got)
		}
	})

	t.Run("IncHandoffTransitions", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.IncHandoffTransitions("success")
		m.IncHandoffTransitions("success")
		m.IncHandoffTransitions("skipped")

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_handoff_transitions_total", map[string]string{"result": "success"}); got != 2 {
			t.Errorf("sortie_handoff_transitions_total{result=success} = %v, want 2", got)
		}
		if got := counterValue(t, families, "sortie_handoff_transitions_total", map[string]string{"result": "skipped"}); got != 1 {
			t.Errorf("sortie_handoff_transitions_total{result=skipped} = %v, want 1", got)
		}
	})

	t.Run("AddTokens", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.AddTokens("input", 100)

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_tokens_total", map[string]string{"type": "input"}); got != 100 {
			t.Errorf("sortie_tokens_total{type=input} = %v, want 100", got)
		}

		m.AddTokens("output", 50)
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_tokens_total", map[string]string{"type": "output"}); got != 50 {
			t.Errorf("sortie_tokens_total{type=output} = %v, want 50", got)
		}
		// input unchanged
		if got := counterValue(t, families, "sortie_tokens_total", map[string]string{"type": "input"}); got != 100 {
			t.Errorf("sortie_tokens_total{type=input} after output add = %v, want 100", got)
		}

		// Negative values clamped — no panic.
		m.AddTokens("input", -10)
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_tokens_total", map[string]string{"type": "input"}); got != 100 {
			t.Errorf("sortie_tokens_total{type=input} after negative add = %v, want 100", got)
		}

		// Zero is a no-op.
		m.AddTokens("input", 0)
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_tokens_total", map[string]string{"type": "input"}); got != 100 {
			t.Errorf("sortie_tokens_total{type=input} after zero add = %v, want 100", got)
		}
	})

	t.Run("AddAgentRuntime", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.AddAgentRuntime(60.5)

		families := gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_agent_runtime_seconds_total", nil); got != 60.5 {
			t.Errorf("sortie_agent_runtime_seconds_total = %v, want 60.5", got)
		}

		// Negative clamped — no panic.
		m.AddAgentRuntime(-1.0)
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_agent_runtime_seconds_total", nil); got != 60.5 {
			t.Errorf("sortie_agent_runtime_seconds_total after negative = %v, want 60.5", got)
		}

		// Zero is a no-op.
		m.AddAgentRuntime(0)
		families = gatherFamilies(t, m)
		if got := counterValue(t, families, "sortie_agent_runtime_seconds_total", nil); got != 60.5 {
			t.Errorf("sortie_agent_runtime_seconds_total after zero = %v, want 60.5", got)
		}
	})
}

func TestPromMetricsHistograms(t *testing.T) {
	t.Parallel()

	t.Run("ObservePollDuration", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.ObservePollDuration(0.5)
		m.ObservePollDuration(1.0)
		m.ObservePollDuration(2.0)

		families := gatherFamilies(t, m)
		count, sum := histogramStats(t, families, "sortie_poll_duration_seconds", nil)
		if count != 3 {
			t.Errorf("sortie_poll_duration_seconds sample_count = %d, want 3", count)
		}
		if sum != 3.5 {
			t.Errorf("sortie_poll_duration_seconds sample_sum = %v, want 3.5", sum)
		}
	})

	t.Run("ObserveWorkerDuration", func(t *testing.T) {
		t.Parallel()

		m := newTestMetrics(t)
		m.ObserveWorkerDuration("normal", 120.0)
		m.ObserveWorkerDuration("normal", 300.0)
		m.ObserveWorkerDuration("error", 45.0)

		families := gatherFamilies(t, m)

		normalCount, normalSum := histogramStats(t, families, "sortie_worker_duration_seconds", map[string]string{"exit_type": "normal"})
		if normalCount != 2 {
			t.Errorf("sortie_worker_duration_seconds{exit_type=normal} sample_count = %d, want 2", normalCount)
		}
		if normalSum != 420.0 {
			t.Errorf("sortie_worker_duration_seconds{exit_type=normal} sample_sum = %v, want 420", normalSum)
		}

		errorCount, errorSum := histogramStats(t, families, "sortie_worker_duration_seconds", map[string]string{"exit_type": "error"})
		if errorCount != 1 {
			t.Errorf("sortie_worker_duration_seconds{exit_type=error} sample_count = %d, want 1", errorCount)
		}
		if errorSum != 45.0 {
			t.Errorf("sortie_worker_duration_seconds{exit_type=error} sample_sum = %v, want 45", errorSum)
		}
	})
}

func TestPromMetricsBuildInfo(t *testing.T) {
	t.Parallel()

	m := NewPromMetrics("1.2.3", "go1.26.1")
	families := gatherFamilies(t, m)
	f := requireFamily(t, families, "sortie_build_info")

	if got := f.GetType(); got != dto.MetricType_GAUGE {
		t.Errorf("sortie_build_info type = %v, want GAUGE", got)
	}

	metrics := f.GetMetric()
	if len(metrics) != 1 {
		t.Fatalf("sortie_build_info metric count = %d, want 1", len(metrics))
	}

	metric := metrics[0]
	if got := metric.GetGauge().GetValue(); got != 1.0 {
		t.Errorf("sortie_build_info value = %v, want 1", got)
	}

	labels := make(map[string]string)
	for _, lp := range metric.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	if got := labels["version"]; got != "1.2.3" {
		t.Errorf("sortie_build_info version label = %q, want %q", got, "1.2.3")
	}
	if got := labels["go_version"]; got != "go1.26.1" {
		t.Errorf("sortie_build_info go_version label = %q, want %q", got, "go1.26.1")
	}
}

func TestPromMetricsBuildInfoDefaultVersion(t *testing.T) {
	t.Parallel()

	m := NewPromMetrics("", "go1.26.1")
	families := gatherFamilies(t, m)
	f := requireFamily(t, families, "sortie_build_info")

	metrics := f.GetMetric()
	if len(metrics) != 1 {
		t.Fatalf("sortie_build_info metric count = %d, want 1", len(metrics))
	}

	for _, lp := range metrics[0].GetLabel() {
		if lp.GetName() == "version" {
			if got := lp.GetValue(); got != "dev" {
				t.Errorf("sortie_build_info version label = %q, want %q", got, "dev")
			}
			return
		}
	}
	t.Fatal("sortie_build_info: version label not found")
}

func TestPromMetricsDedicatedRegistry(t *testing.T) {
	t.Parallel()

	m := newTestMetrics(t)

	// Sortie metrics present on the dedicated registry.
	families := gatherFamilies(t, m)
	if _, ok := families["sortie_sessions_running"]; !ok {
		t.Error("sortie_sessions_running not found on dedicated registry")
	}

	// Global registry must NOT contain sortie_* metrics.
	globalFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("DefaultGatherer.Gather() error: %v", err)
	}
	for _, f := range globalFamilies {
		if strings.HasPrefix(f.GetName(), "sortie_") {
			t.Errorf("sortie metric %q found on global default registry; expected isolation", f.GetName())
		}
	}
}
