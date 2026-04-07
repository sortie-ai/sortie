package server

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// promqlMetricRE matches sortie_ metric name tokens in PromQL expressions.
var promqlMetricRE = regexp.MustCompile(`\bsortie_[a-z_]+`)

// knownSortieMetrics is the canonical list of sortie_ metric family names
// registered by NewPromMetrics. Add new entries here when adding metrics
// to metrics.go, and ensure the Grafana dashboard also covers them.
var knownSortieMetrics = []string{
	"sortie_build_info",
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
	"sortie_poll_duration_seconds",
	"sortie_tracker_requests_total",
	"sortie_handoff_transitions_total",
	"sortie_dispatch_transitions_total",
	"sortie_tracker_comments_total",
	"sortie_tool_calls_total",
	"sortie_worker_duration_seconds",
	"sortie_ssh_host_usage",
	"sortie_ci_status_checks_total",
	"sortie_ci_escalations_total",
}

// stripHistogramSuffix removes PromQL-specific histogram suffixes so that
// references like sortie_worker_duration_seconds_bucket map back to the
// registered family name sortie_worker_duration_seconds.
func stripHistogramSuffix(name string) string {
	for _, sfx := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, sfx) {
			return strings.TrimSuffix(name, sfx)
		}
	}
	return name
}

// TestGrafanaDashboardCoversAllMetrics parses examples/grafana-dashboard.json
// and verifies that every registered sortie_ metric name appears in at least
// one panel's PromQL expression. This guards against metric-to-panel drift
// when new metrics are added to metrics.go.
func TestGrafanaDashboardCoversAllMetrics(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../examples/grafana-dashboard.json")
	if err != nil {
		t.Fatalf("reading grafana dashboard: %v", err)
	}

	var dash struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("parsing grafana dashboard JSON: %v", err)
	}

	// Collect all sortie_ base metric names referenced across all panels.
	referenced := make(map[string]bool, len(knownSortieMetrics))
	for _, panel := range dash.Panels {
		for _, target := range panel.Targets {
			for _, match := range promqlMetricRE.FindAllString(target.Expr, -1) {
				referenced[stripHistogramSuffix(match)] = true
			}
		}
	}

	for _, name := range knownSortieMetrics {
		if !referenced[name] {
			t.Errorf("metric %q is not referenced in any Grafana panel", name)
		}
	}
}
