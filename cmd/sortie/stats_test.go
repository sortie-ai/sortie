package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
)

// fullCaps reports every optional run_history column group present, so
// the "full" projection and every summary and group figure are computed.
var fullCaps = persistence.RunHistoryCapabilities{
	HasTurnsCompleted:   true,
	HasReviewMetadata:   true,
	HasRuleRouting:      true,
	HasTokens:           true,
	HasTokenMeasurement: true,
}

// fixedNow is a deterministic generatedAt value for aggregator-level
// tests that call statsAggregator.report directly.
var fixedNow = time.Date(2026, 8, 7, 9, 15, 0, 0, time.UTC)

// rangeFloats returns the ascending integers from lo to hi inclusive, as
// float64, for the worked nearest-rank percentile examples.
func rangeFloats(lo, hi int) []float64 {
	out := make([]float64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, float64(i))
	}
	return out
}

// addRows folds each row into agg via statsAggregator.add, failing the
// test immediately on an unexpected error (add always returns nil).
func addRows(t *testing.T, agg *statsAggregator, rows ...persistence.RunStatsRow) {
	t.Helper()
	for _, r := range rows {
		if err := agg.add(r); err != nil {
			t.Fatalf("statsAggregator.add(%+v): %v", r, err)
		}
	}
}

// findGroup returns the statsGroup named name in groups, failing the
// test when no such group exists.
func findGroup(t *testing.T, groups []statsGroup, name string) statsGroup {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %q not found in %+v", name, groups)
	return statsGroup{}
}

// reviewMetaPtr marshals m and returns a pointer to the resulting JSON,
// matching the shape ScanRunHistoryRange returns for a non-NULL
// review_metadata column.
func reviewMetaPtr(t *testing.T, m domain.ReviewMetadata) *string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal(%+v): %v", m, err)
	}
	return new(string(b))
}

// statsWorkflow returns a minimal WORKFLOW.md whose front matter is
// extended with extra, for stats tests that need a specific top-level
// key such as db_path without a full tracker/agent configuration.
func statsWorkflow(extra string) []byte {
	return fmt.Appendf(nil, `---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
%s---
Do {{ .issue.title }}.
`, extra)
}

// createStatsDB creates a fully migrated SQLite database at dbPath and
// appends each of runs, then closes the store so a later read-only open
// sees committed data.
func createStatsDB(t *testing.T, dbPath string, runs ...persistence.RunHistory) {
	t.Helper()
	ctx := context.Background()
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open(%q): %v", dbPath, err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, r := range runs {
		if _, err := store.AppendRunHistory(ctx, r); err != nil {
			t.Fatalf("AppendRunHistory: %v", err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// createLegacyStatsDB creates dbPath with only the migration-001
// run_history columns (no schema_migrations tracking) and inserts one
// row, matching a database written by a pre-migration-010 binary. The
// modernc.org/sqlite driver is already registered process-wide via
// internal/persistence's import.
func createLegacyStatsDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE run_history (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		issue_id      TEXT    NOT NULL,
		identifier    TEXT    NOT NULL,
		attempt       INTEGER NOT NULL,
		agent_adapter TEXT    NOT NULL,
		workspace     TEXT    NOT NULL,
		started_at    TEXT    NOT NULL,
		completed_at  TEXT    NOT NULL,
		status        TEXT    NOT NULL,
		error         TEXT
	)`); err != nil {
		t.Fatalf("create migration-001 run_history table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO run_history (issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status)
		 VALUES ('ISS-1', 'PROJ-1', 1, 'mock', '/tmp/ws', '2026-01-01T00:00:00Z', '2026-01-01T00:10:00Z', 'succeeded')`,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
}

// newStatsWorkspace creates a temp directory holding a minimal
// WORKFLOW.md and a database built by build, and returns the workflow
// path.
//
// Every caller gets its own directory, so parallel subtests never share
// one database file. Two read-only opens of the same file race to recover
// its write-ahead log, and the loser gets SQLITE_BUSY; on Linux the race
// is almost always won in time, on Windows it is not.
func newStatsWorkspace(t *testing.T, build func(t *testing.T, dbPath string)) string {
	t.Helper()
	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))
	build(t, filepath.Join(dir, ".sortie.db"))
	return wfPath
}

// pinStatsNow overrides the package-level statsNow for the life of the
// calling test, following the serverShutdownTimeout precedent. A test
// that calls this must not also call t.Parallel().
func pinStatsNow(t *testing.T, now time.Time) {
	t.Helper()
	orig := statsNow
	statsNow = func() time.Time { return now }
	t.Cleanup(func() { statsNow = orig })
}

// --- percentile/mean worked examples and summary normalization ---

func TestStatsSummaryFormulas(t *testing.T) {
	t.Parallel()

	t.Run("percentile and mean over the worked sample sets", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			samples  []float64
			wantP50  float64
			wantP95  float64
			wantMean float64
		}{
			{name: "single sample", samples: []float64{7}, wantP50: 7, wantP95: 7, wantMean: 7},
			{name: "four samples", samples: []float64{10, 20, 30, 40}, wantP50: 20, wantP95: 40, wantMean: 25},
			{name: "twenty samples", samples: rangeFloats(1, 20), wantP50: 10, wantP95: 19, wantMean: 10.5},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := durationStats(tt.samples)

				if got.Samples != len(tt.samples) {
					t.Fatalf("durationStats(%v).Samples = %d, want %d", tt.samples, got.Samples, len(tt.samples))
				}
				if got.P50 == nil || *got.P50 != tt.wantP50 {
					t.Errorf("durationStats(%v).P50 = %v, want %v", tt.samples, got.P50, tt.wantP50)
				}
				if got.P95 == nil || *got.P95 != tt.wantP95 {
					t.Errorf("durationStats(%v).P95 = %v, want %v", tt.samples, got.P95, tt.wantP95)
				}
				if got.Mean == nil || *got.Mean != tt.wantMean {
					t.Errorf("durationStats(%v).Mean = %v, want %v", tt.samples, got.Mean, tt.wantMean)
				}
			})
		}
	})

	t.Run("duration and turns normalize on succeeded rows; cost per succeeded run divides all spend", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{"mock": server.TokenRateConfig{InputPerMtok: new(float64(10))}}
		agg := newStatsAggregator(fullCaps, rates)

		addRows(t, agg,
			persistence.RunStatsRow{
				Status: "succeeded", AgentAdapter: "mock", TurnsCompleted: 4,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
				InputTokens: 1_000_000, TokensMeasured: true,
			},
			persistence.RunStatsRow{
				Status: "failed", AgentAdapter: "mock", TurnsCompleted: 8,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T01:00:00Z",
				InputTokens: 500_000, TokensMeasured: true,
			},
		)

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.Duration.Samples != 1 {
			t.Fatalf("Summary.Duration.Samples = %d, want 1 (succeeded rows only)", report.Summary.Duration.Samples)
		}
		if got, want := *report.Summary.Duration.P50, 600.0; got != want {
			t.Errorf("Summary.Duration.P50 = %v, want %v", got, want)
		}
		if report.Summary.MeanTurnsSucceeded == nil || *report.Summary.MeanTurnsSucceeded != 4 {
			t.Errorf("Summary.MeanTurnsSucceeded = %v, want 4 (succeeded rows only)", report.Summary.MeanTurnsSucceeded)
		}
		if report.Summary.CostUSD == nil || *report.Summary.CostUSD != 15 {
			t.Fatalf("Summary.CostUSD = %v, want 15 (all rows: $10 + $5)", report.Summary.CostUSD)
		}
		if report.Summary.CostPerSucceededRunUSD == nil || *report.Summary.CostPerSucceededRunUSD != 15 {
			t.Errorf("Summary.CostPerSucceededRunUSD = %v, want 15 (cost_usd / succeeded, not succeeded-only spend / succeeded)",
				report.Summary.CostPerSucceededRunUSD)
		}
	})
}

// --- ci_failed row disclosure ---

func TestStatsGroupDisclosure(t *testing.T) {
	t.Parallel()

	rates := server.TokenRates{"mock": server.TokenRateConfig{InputPerMtok: new(float64(10))}}
	agg := newStatsAggregator(fullCaps, rates)

	addRows(t, agg, persistence.RunStatsRow{
		Status:         "ci_failed",
		StartedAt:      "2026-01-01T00:00:00Z",
		CompletedAt:    "2026-01-01T00:00:00Z",
		TokensMeasured: true,
	})

	report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

	adapterGroup := findGroup(t, report.ByAdapter, "<none>")
	if adapterGroup.Duration.Samples != 1 {
		t.Errorf("<none> adapter group Duration.Samples = %d, want 1", adapterGroup.Duration.Samples)
	}
	if adapterGroup.Duration.P50 == nil || *adapterGroup.Duration.P50 != 0 {
		t.Errorf("<none> adapter group Duration.P50 = %v, want 0 (zero-duration sample)", adapterGroup.Duration.P50)
	}
	if adapterGroup.Tokens == nil || adapterGroup.Tokens.Total != 0 {
		t.Errorf("<none> adapter group Tokens = %v, want Total 0", adapterGroup.Tokens)
	}
	if adapterGroup.CostUSD != nil {
		t.Errorf("<none> adapter group CostUSD = %v, want nil (no rate entry for an empty agent_adapter)", *adapterGroup.CostUSD)
	}
	if got := formatCostDash(adapterGroup.CostUSD); got != "-" {
		t.Errorf("formatCostDash(nil) = %q, want %q", got, "-")
	}

	ruleGroup := findGroup(t, report.ByRule, "<none>")
	if ruleGroup.Runs != 1 {
		t.Errorf("<none> rule group Runs = %d, want 1", ruleGroup.Runs)
	}
	templateGroup := findGroup(t, report.ByTemplate, "<none>")
	if templateGroup.Runs != 1 {
		t.Errorf("<none> template group Runs = %d, want 1", templateGroup.Runs)
	}

	if report.Summary.ZeroDurationRuns != 1 {
		t.Errorf("Summary.ZeroDurationRuns = %d, want 1", report.Summary.ZeroDurationRuns)
	}
}

// --- cost derivation across three rate configurations ---

func TestStatsCostDerivation(t *testing.T) {
	t.Parallel()

	makeRow := func(adapter string, input int64) persistence.RunStatsRow {
		return persistence.RunStatsRow{
			Status: "succeeded", AgentAdapter: adapter,
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
			InputTokens: input, TokensMeasured: true,
		}
	}

	t.Run("every adapter priced", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{
			"claude-code": server.TokenRateConfig{InputPerMtok: new(float64(10))},
			"codex":       server.TokenRateConfig{InputPerMtok: new(float64(20))},
		}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, makeRow("claude-code", 1_000_000), makeRow("codex", 1_000_000))

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.CostUnpricedRuns != 0 {
			t.Errorf("Summary.CostUnpricedRuns = %d, want 0", report.Summary.CostUnpricedRuns)
		}
		if report.Summary.CostUSD == nil || *report.Summary.CostUSD != 30 {
			t.Fatalf("Summary.CostUSD = %v, want 30", report.Summary.CostUSD)
		}
		if footnotes := statsFootnotes(report.Summary); len(footnotes) != 0 {
			t.Errorf("statsFootnotes = %v, want none", footnotes)
		}
	})

	t.Run("one adapter priced, one not", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{
			"claude-code": server.TokenRateConfig{InputPerMtok: new(float64(10))},
		}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, makeRow("claude-code", 1_000_000), makeRow("codex", 1_000_000))

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.CostUnpricedRuns != 1 {
			t.Errorf("Summary.CostUnpricedRuns = %d, want 1", report.Summary.CostUnpricedRuns)
		}
		if report.Summary.CostUSD == nil || *report.Summary.CostUSD != 10 {
			t.Fatalf("Summary.CostUSD = %v, want 10 (priced row only)", report.Summary.CostUSD)
		}
		footnotes := statsFootnotes(report.Summary)
		found := slices.ContainsFunc(footnotes, func(f string) bool {
			return strings.Contains(f, "the cost figures skip 1 of these runs")
		})
		if !found {
			t.Errorf("statsFootnotes = %v, want an unpriced-run footnote", footnotes)
		}
	})

	t.Run("no token_rates configured at all", func(t *testing.T) {
		t.Parallel()

		agg := newStatsAggregator(fullCaps, nil)
		addRows(t, agg, makeRow("claude-code", 1_000_000), makeRow("codex", 1_000_000))

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.CostUSD != nil {
			t.Errorf("Summary.CostUSD = %v, want nil", *report.Summary.CostUSD)
		}
		if report.Summary.CostPerSucceededRunUSD != nil {
			t.Errorf("Summary.CostPerSucceededRunUSD = %v, want nil", *report.Summary.CostPerSucceededRunUSD)
		}
		if report.Summary.CostUnpricedRuns != 0 {
			t.Errorf("Summary.CostUnpricedRuns = %d, want 0 even though the range holds rows", report.Summary.CostUnpricedRuns)
		}
		if footnotes := statsFootnotes(report.Summary); len(footnotes) != 0 {
			t.Errorf("statsFootnotes = %v, want none (the cost column is already dropped)", footnotes)
		}

		var stdout, stderr bytes.Buffer
		renderStatsText(&stdout, &stderr, report)
		out := stdout.String()
		if !strings.Contains(out, "not estimated; set token_rates in the workflow") {
			t.Errorf("renderStatsText stdout = %q, want the not-estimated line", out)
		}
		if strings.Contains(out, "the cost figures skip") {
			t.Errorf("renderStatsText stdout = %q, want no unpriced disclosure", out)
		}
	})

	t.Run("all runs unmeasured with token_rates configured renders a dash for both figures", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{"kiro": server.TokenRateConfig{InputPerMtok: new(float64(10))}}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, persistence.RunStatsRow{
			Status: "succeeded", AgentAdapter: "kiro",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
			TokensMeasured: false,
		})

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)
		if report.Summary.CostUSD != nil {
			t.Errorf("Summary.CostUSD = %v, want nil (no measured run in range)", *report.Summary.CostUSD)
		}

		var stdout, stderr bytes.Buffer
		renderStatsText(&stdout, &stderr, report)
		out := stdout.String()
		if !strings.Contains(out, "cost (measured runs)    -   per succeeded run -") {
			t.Errorf("renderStatsText stdout = %q, want the dash arm, not the token_rates remedy", out)
		}
		if strings.Contains(out, "not estimated; set token_rates") {
			t.Errorf("renderStatsText stdout = %q, want no token_rates remedy naming a cause that is not the actual one", out)
		}
	})

	t.Run("a mixed range with no rates configured still names token_rates as the remedy", func(t *testing.T) {
		t.Parallel()

		agg := newStatsAggregator(fullCaps, nil)
		addRows(t, agg,
			makeRow("claude-code", 1_000_000),
			persistence.RunStatsRow{
				Status: "succeeded", AgentAdapter: "kiro",
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
				TokensMeasured: false,
			},
		)

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		var stdout, stderr bytes.Buffer
		renderStatsText(&stdout, &stderr, report)
		out := stdout.String()
		if !strings.Contains(out, "not estimated; set token_rates in the workflow") {
			t.Errorf("renderStatsText stdout = %q, want the token_rates remedy for a range with a measured run", out)
		}
	})

	t.Run("a range with a measured priced run renders numbers", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{"claude-code": server.TokenRateConfig{InputPerMtok: new(float64(10))}}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, makeRow("claude-code", 1_000_000))

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		var stdout, stderr bytes.Buffer
		renderStatsText(&stdout, &stderr, report)
		out := stdout.String()
		if !strings.Contains(out, "cost (measured runs)    $10.00   per succeeded run $10.00") {
			t.Errorf("renderStatsText stdout = %q, want the priced numbers line", out)
		}
	})
}

// --- stored total_tokens is reported unchanged ---

func TestStatsTokenReporting(t *testing.T) {
	t.Parallel()

	t.Run("stored total_tokens is reported unchanged", func(t *testing.T) {
		t.Parallel()

		agg := newStatsAggregator(fullCaps, nil)
		addRows(t, agg, persistence.RunStatsRow{
			Status: "succeeded", AgentAdapter: "mock",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
			InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10,
			TotalTokens:    999, // deliberately inconsistent with input+output+cache_read = 160
			TokensMeasured: true,
		})

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.Tokens == nil || report.Summary.Tokens.Total != 999 {
			t.Errorf("Summary.Tokens.Total = %v, want 999 (stored total, never recomputed)", report.Summary.Tokens)
		}
		adapterGroup := findGroup(t, report.ByAdapter, "mock")
		if adapterGroup.Tokens == nil || adapterGroup.Tokens.Total != 999 {
			t.Errorf("by_adapter[mock].Tokens.Total = %v, want 999", adapterGroup.Tokens)
		}
	})

	// measuredUnmeasuredRows returns a measured run of 1000 total tokens,
	// a measured-zero run, and an unmeasured run on a distinct adapter.
	measuredUnmeasuredRows := func() []persistence.RunStatsRow {
		return []persistence.RunStatsRow{
			{
				Status: "succeeded", AgentAdapter: "claude-code",
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
				InputTokens: 1_000_000, TotalTokens: 1_000_000, TokensMeasured: true,
			},
			{
				Status: "succeeded", AgentAdapter: "claude-code",
				StartedAt: "2026-01-01T01:00:00Z", CompletedAt: "2026-01-01T01:10:00Z",
				TokensMeasured: true, // a measurement of zero
			},
			{
				Status: "succeeded", AgentAdapter: "kiro",
				StartedAt: "2026-01-01T02:00:00Z", CompletedAt: "2026-01-01T02:10:00Z",
				TokensMeasured: false,
			},
		}
	}

	t.Run("JSON form folds a measured, measured-zero, and unmeasured run", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{
			"claude-code": server.TokenRateConfig{InputPerMtok: new(float64(10))},
			"kiro":        server.TokenRateConfig{InputPerMtok: new(float64(10))},
		}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, measuredUnmeasuredRows()...)

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.Tokens == nil || report.Summary.Tokens.Total != 1_000_000 {
			t.Errorf("Summary.Tokens.Total = %v, want 1000000 (measured runs only)", report.Summary.Tokens)
		}
		if report.Summary.TokensUnmeasuredRuns != 1 {
			t.Errorf("Summary.TokensUnmeasuredRuns = %d, want 1", report.Summary.TokensUnmeasuredRuns)
		}
		if report.Summary.CostUSD == nil || *report.Summary.CostUSD != 10 {
			t.Fatalf("Summary.CostUSD = %v, want 10 (prices only the measured claude-code runs)", report.Summary.CostUSD)
		}
		if report.Summary.CostPerSucceededRunUSD == nil || *report.Summary.CostPerSucceededRunUSD != 5 {
			t.Errorf("Summary.CostPerSucceededRunUSD = %v, want 5 (10 / 2 succeeded measured runs)", report.Summary.CostPerSucceededRunUSD)
		}

		kiroGroup := findGroup(t, report.ByAdapter, "kiro")
		if kiroGroup.Tokens != nil {
			t.Errorf("by_adapter[kiro].Tokens = %+v, want nil (no measured run)", kiroGroup.Tokens)
		}
		if kiroGroup.CostUSD != nil {
			t.Errorf("by_adapter[kiro].CostUSD = %v, want nil", *kiroGroup.CostUSD)
		}
		if kiroGroup.TokensUnmeasuredRuns != 1 {
			t.Errorf("by_adapter[kiro].TokensUnmeasuredRuns = %d, want 1", kiroGroup.TokensUnmeasuredRuns)
		}
	})

	t.Run("text form labels the measured-runs lines and dashes the unmeasured adapter row", func(t *testing.T) {
		t.Parallel()

		rates := server.TokenRates{
			"claude-code": server.TokenRateConfig{InputPerMtok: new(float64(10))},
			"kiro":        server.TokenRateConfig{InputPerMtok: new(float64(10))},
		}
		agg := newStatsAggregator(fullCaps, rates)
		addRows(t, agg, measuredUnmeasuredRows()...)
		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		var stdout, stderr bytes.Buffer
		renderStatsText(&stdout, &stderr, report)
		out := stdout.String()

		if !strings.Contains(out, "tokens (measured runs)") {
			t.Errorf("stdout = %q, want the %q label", out, "tokens (measured runs)")
		}
		if !strings.Contains(out, "cost (measured runs)") {
			t.Errorf("stdout = %q, want the %q label", out, "cost (measured runs)")
		}
		if strings.Contains(out, "tokens (all runs)") || strings.Contains(out, "cost (all runs)") {
			t.Errorf("stdout = %q, want no trace of the old \"(all runs)\" labels", out)
		}

		kiroLine := ""
		for line := range strings.SplitSeq(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "kiro") {
				kiroLine = line
				break
			}
		}
		if kiroLine == "" {
			t.Fatalf("stdout = %q, want a kiro row in the by-coding-agent table", out)
		}
		fields := strings.Fields(kiroLine)
		if len(fields) == 0 || fields[len(fields)-1] != "-" || fields[len(fields)-2] != "-" {
			t.Errorf("kiro row = %q, want its total-tokens and cost columns to both be %q", kiroLine, "-")
		}

		if !strings.Contains(out, "the token and cost figures skip 1 of these runs") {
			t.Errorf("stdout = %q, want the unmeasured-runs footnote", out)
		}
	})

	t.Run("footnote order places the unmeasured note after duration notes and before the unpriced-cost note", func(t *testing.T) {
		t.Parallel()

		summary := statsSummary{
			ZeroDurationRuns:     1,
			DurationExcludedRuns: 1,
			TokensUnmeasuredRuns: 1,
			CostUnpricedRuns:     1,
		}

		notes := statsFootnotes(summary)
		if len(notes) != 4 {
			t.Fatalf("statsFootnotes(%+v) = %v, want 4 lines", summary, notes)
		}
		unmeasuredIdx := slices.IndexFunc(notes, func(n string) bool {
			return strings.Contains(n, "the token and cost figures skip")
		})
		unpricedIdx := slices.IndexFunc(notes, func(n string) bool {
			return strings.Contains(n, "the cost figures skip")
		})
		if unmeasuredIdx == -1 || unpricedIdx == -1 {
			t.Fatalf("statsFootnotes = %v, want both the unmeasured and unpriced notes present", notes)
		}
		if unmeasuredIdx != 2 {
			t.Errorf("unmeasured note at index %d, want 2 (after the two duration notes)", unmeasuredIdx)
		}
		if unmeasuredIdx >= unpricedIdx {
			t.Errorf("unmeasured note at index %d, unpriced note at index %d, want unmeasured before unpriced", unmeasuredIdx, unpricedIdx)
		}
	})

	t.Run("base tier reports zero unmeasured runs", func(t *testing.T) {
		t.Parallel()

		baseCaps := persistence.RunHistoryCapabilities{}
		agg := newStatsAggregator(baseCaps, nil)
		addRows(t, agg, persistence.RunStatsRow{
			Status: "succeeded", AgentAdapter: "kiro",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
		})

		report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

		if report.Summary.TokensUnmeasuredRuns != 0 {
			t.Errorf("Summary.TokensUnmeasuredRuns = %d, want 0 on the base tier (absent record, not zero unmeasured runs)", report.Summary.TokensUnmeasuredRuns)
		}
	})
}

// --- self-review aggregation ---

func TestStatsSelfReview(t *testing.T) {
	t.Parallel()

	agg := newStatsAggregator(fullCaps, nil)
	addRows(t, agg,
		persistence.RunStatsRow{
			Status: "succeeded", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "pass", TotalIterations: 1}),
		},
		persistence.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "iterate", TotalIterations: 3, CapReached: true}),
		},
		persistence.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "", TotalIterations: 2}),
		},
		persistence.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: new("{not-json"),
		},
	)

	report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

	sr := report.SelfReview
	if sr == nil {
		t.Fatal("SelfReview = nil, want non-nil on the full tier")
	}
	if sr.RunsWithMetadata != 3 {
		t.Errorf("RunsWithMetadata = %d, want 3", sr.RunsWithMetadata)
	}
	if sr.UnparsedRuns != 1 {
		t.Errorf("UnparsedRuns = %d, want 1", sr.UnparsedRuns)
	}
	if sr.CapReachedRuns != 1 {
		t.Errorf("CapReachedRuns = %d, want 1", sr.CapReachedRuns)
	}
	if sr.MeanIterations == nil || *sr.MeanIterations != 2 {
		t.Errorf("MeanIterations = %v, want 2 ((1+3+2)/3)", sr.MeanIterations)
	}

	wantVerdicts := map[string]int{"iterate": 1, "none": 1, "pass": 1}
	gotVerdicts := make(map[string]int, len(sr.ByFinalVerdict))
	var verdictOrder []string
	for _, vc := range sr.ByFinalVerdict {
		gotVerdicts[vc.Verdict] = vc.Runs
		verdictOrder = append(verdictOrder, vc.Verdict)
	}
	if !maps.Equal(gotVerdicts, wantVerdicts) {
		t.Errorf("ByFinalVerdict = %v, want %v", gotVerdicts, wantVerdicts)
	}
	if !slices.IsSorted(verdictOrder) {
		t.Errorf("ByFinalVerdict order = %v, want ascending by verdict", verdictOrder)
	}
}

// --- data-driven status set and group ordering ---

func TestStatsStatusOrdering(t *testing.T) {
	t.Parallel()

	agg := newStatsAggregator(fullCaps, nil)
	for _, status := range []string{"weird_status", "succeeded", "succeeded", "failed", "cancelled"} {
		addRows(t, agg, persistence.RunStatsRow{
			Status: status, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
		})
	}

	report := agg.report(fixedNow, "/wf", "/db", nil, nil, nil)

	// "cancelled", "failed", and "weird_status" all have runs=1; ties break
	// ascending by byte comparison: "cancelled" < "failed" < "weird_status".
	want := []string{"succeeded", "cancelled", "failed", "weird_status"}
	var got []string
	for _, g := range report.ByStatus {
		got = append(got, g.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ByStatus order = %v, want %v", got, want)
	}

	weird := findGroup(t, report.ByStatus, "weird_status")
	if weird.Runs != 1 {
		t.Errorf("weird_status group Runs = %d, want 1 (status set must not be hard-coded)", weird.Runs)
	}
}

// --- JSON report contract and byte-stability ---

func TestRunStatsJSON(t *testing.T) {
	// No t.Parallel: pinStatsNow mutates the package-level statsNow.
	pinStatsNow(t, time.Date(2026, 8, 7, 9, 15, 0, 0, time.UTC))

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))
	dbPath := filepath.Join(dir, ".sortie.db")
	createStatsDB(t, dbPath,
		persistence.RunHistory{
			IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z", Status: "succeeded", TurnsCompleted: 3,
		},
		persistence.RunHistory{
			IssueID: "ISS-2", Identifier: "PROJ-2", Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:20:00Z", Status: "failed", TurnsCompleted: 5,
		},
	)

	var stdout1, stderr1 bytes.Buffer
	code := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout1, &stderr1)
	if code != 0 {
		t.Fatalf("run(stats --format json) = %d, want 0; stderr: %s", code, stderr1.String())
	}
	if stderr1.Len() != 0 {
		t.Errorf("stderr = %q, want empty on the success path", stderr1.String())
	}

	var report statsReport
	if err := json.Unmarshal(stdout1.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout1.String(), err)
	}
	if report.SchemaTier != "full" {
		t.Errorf("SchemaTier = %q, want %q", report.SchemaTier, "full")
	}
	if report.WorkflowPath != wfPath {
		t.Errorf("WorkflowPath = %q, want %q", report.WorkflowPath, wfPath)
	}
	if report.DBPath != dbPath {
		t.Errorf("DBPath = %q, want %q", report.DBPath, dbPath)
	}
	if report.Summary.Runs != 2 {
		t.Errorf("Summary.Runs = %d, want 2", report.Summary.Runs)
	}
	if report.ByStatus == nil || report.ByAdapter == nil || report.ByRule == nil || report.ByTemplate == nil || report.Warnings == nil {
		t.Errorf("report slices must never be nil: ByStatus=%v ByAdapter=%v ByRule=%v ByTemplate=%v Warnings=%v",
			report.ByStatus, report.ByAdapter, report.ByRule, report.ByTemplate, report.Warnings)
	}
	if !strings.Contains(stdout1.String(), `"warnings":[]`) {
		t.Errorf("stdout = %q, want %q", stdout1.String(), `"warnings":[]`)
	}

	var stdout2, stderr2 bytes.Buffer
	code2 := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout2, &stderr2)
	if code2 != 0 {
		t.Fatalf("second run(stats --format json) = %d, want 0; stderr: %s", code2, stderr2.String())
	}
	if stdout1.String() != stdout2.String() {
		t.Errorf("stats --format json output is not byte-stable across two runs:\nfirst:  %q\nsecond: %q",
			stdout1.String(), stdout2.String())
	}
}

// --- empty range ---

func TestRunStatsEmptyRange(t *testing.T) {
	t.Parallel()

	newWorkspace := func(t *testing.T) string {
		t.Helper()
		return newStatsWorkspace(t, func(t *testing.T, dbPath string) {
			createStatsDB(t, dbPath, persistence.RunHistory{
				IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z", Status: "succeeded",
			})
		})
	}

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		wfPath := newWorkspace(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--since", "2099-01-01T00:00:00Z", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats) = %d, want 0; stderr: %s", code, stderr.String())
		}
		for _, want := range []string{
			"covering:  2099-01-01T00:00:00Z onward",
			"No runs finished in this range.",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("stdout = %q, want to contain %q", stdout.String(), want)
			}
		}
		for _, absent := range []string{"by outcome", "by coding agent", "note:"} {
			if strings.Contains(stdout.String(), absent) {
				t.Errorf("stdout contains %q, want no breakdown or footnote for an empty range", absent)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		wfPath := newWorkspace(t)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--format", "json", "--since", "2099-01-01T00:00:00Z", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats --format json) = %d, want 0; stderr: %s", code, stderr.String())
		}

		var report statsReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
		}
		if report.Summary.Runs != 0 {
			t.Errorf("Summary.Runs = %d, want 0", report.Summary.Runs)
		}
		if report.Summary.Duration.Samples != 0 || report.Summary.Duration.P50 != nil {
			t.Errorf("Summary.Duration = %+v, want zero samples and null figures", report.Summary.Duration)
		}
		if report.Summary.MeanTurnsSucceeded != nil {
			t.Errorf("Summary.MeanTurnsSucceeded = %v, want nil", *report.Summary.MeanTurnsSucceeded)
		}
		if report.Summary.CostUSD != nil {
			t.Errorf("Summary.CostUSD = %v, want nil", *report.Summary.CostUSD)
		}
		for name, got := range map[string][]statsGroup{
			"ByStatus": report.ByStatus, "ByAdapter": report.ByAdapter,
			"ByRule": report.ByRule, "ByTemplate": report.ByTemplate,
		} {
			if got == nil || len(got) != 0 {
				t.Errorf("%s = %v, want a non-nil empty slice", name, got)
			}
		}
		if report.SelfReview == nil {
			t.Fatal("SelfReview = nil, want non-nil on the full tier even with zero rows")
		}
		if report.SelfReview.RunsWithMetadata != 0 {
			t.Errorf("SelfReview.RunsWithMetadata = %d, want 0", report.SelfReview.RunsWithMetadata)
		}
		if report.SelfReview.ByFinalVerdict == nil {
			t.Error("SelfReview.ByFinalVerdict = nil, want a non-nil empty slice")
		}
	})
}

// --- usage errors ---

func TestRunStatsUsageErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))

	tests := []struct {
		name string
		args []string
	}{
		{name: "invalid format", args: []string{"--format", "xml", wfPath}},
		{name: "since zero duration", args: []string{"--since", "0h", wfPath}},
		{name: "since negative duration", args: []string{"--since", "-1h", wfPath}},
		{name: "since bare number without unit", args: []string{"--since", "24", wfPath}},
		{name: "since timestamp without offset", args: []string{"--since", "2026-07-01T00:00:00", wfPath}},
		{name: "since not before until", args: []string{"--since", "2026-07-02T00:00:00Z", "--until", "2026-07-01T00:00:00Z", wfPath}},
		{name: "too many positional arguments", args: []string{wfPath, wfPath}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			args := append([]string{"stats"}, tt.args...)
			code := run(context.Background(), args, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("run(stats %v) = %d, want 1; stderr: %s", tt.args, code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("run(stats %v) stdout = %q, want empty", tt.args, stdout.String())
			}
			lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || lines[0] == "" {
				t.Errorf("run(stats %v) stderr = %q, want exactly one line", tt.args, stderr.String())
			}
		})
	}
}

// --- concurrent read against a live writable store ---

func TestRunStatsAgainstLiveWriter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))
	dbPath := filepath.Join(dir, ".sortie.db")

	ctx := context.Background()
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := store.AppendRunHistory(ctx, persistence.RunHistory{
		IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
		StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z", Status: "succeeded",
	}); err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}

	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat(%q): %v", dbPath, err)
	}

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"stats", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(stats) against a live writable store = %d, want 0; stderr: %s", code, stderr.String())
	}

	var report statsReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if report.Summary.Runs != 1 {
		t.Errorf("Summary.Runs = %d, want 1", report.Summary.Runs)
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("os.Stat(%q): %v", dbPath, err)
	}
	if before.Size() != after.Size() {
		t.Errorf("database file size changed across the invocation: before %d, after %d", before.Size(), after.Size())
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("database file mtime changed across the invocation: before %v, after %v", before.ModTime(), after.ModTime())
	}
}

// --- degraded schema tier ---

func TestDegradedSchemaWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		caps   persistence.RunHistoryCapabilities
		want   []string
		absent []string
	}{
		{
			name: "nothing recorded names every group once",
			caps: persistence.RunHistoryCapabilities{},
			want: []string{
				"before sortie recorded turns, self-review results, dispatch-rule routing, tokens and cost",
				"Run sortie once with this workflow to add them.",
			},
			absent: []string{"does carry", "falls back"},
		},
		{
			// The shape this repository's own database has: turns and
			// self-review are stored, rule routing and tokens are not. The
			// report still drops all four, so the warning must say so.
			name: "partly migrated discloses the figures it drops anyway",
			caps: persistence.RunHistoryCapabilities{HasTurnsCompleted: true, HasReviewMetadata: true},
			want: []string{
				"before sortie recorded dispatch-rule routing, tokens and cost",
				"also leaves out turns, self-review results, which this database does carry",
			},
		},
		{
			name: "only tokens absent still discloses the other three",
			caps: persistence.RunHistoryCapabilities{
				HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true,
			},
			want: []string{
				"before sortie recorded tokens and cost",
				"also leaves out turns, self-review results, dispatch-rule routing, which this database does carry",
			},
		},
		{
			name: "only the measurement record absent names it in plain language",
			caps: persistence.RunHistoryCapabilities{
				HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true, HasTokens: true,
			},
			want: []string{
				"before sortie recorded which runs the coding agent could measure",
				"also leaves out turns, self-review results, dispatch-rule routing, tokens and cost, which this database does carry",
			},
			absent: []string{"tokens_measured", "run_history"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := degradedSchemaWarning(tt.caps)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("degradedSchemaWarning(%+v) = %q, want to contain %q", tt.caps, got, want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("degradedSchemaWarning(%+v) = %q, want it not to contain %q", tt.caps, got, absent)
				}
			}
		})
	}
}

func TestRunStatsDegradedSchema(t *testing.T) {
	t.Parallel()

	t.Run("text", func(t *testing.T) {
		t.Parallel()

		wfPath := newStatsWorkspace(t, createLegacyStatsDB)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats) = %d, want 0; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "was written before sortie recorded") {
			t.Errorf("stderr = %q, want a degraded-schema warning", stderr.String())
		}
		if !strings.HasPrefix(stderr.String(), "\nwarning: ") {
			t.Errorf("stderr = %q, want a blank line before the first warning so it does not run into the report",
				stderr.String())
		}
		if !strings.HasSuffix(stdout.String(), "\n") || strings.HasSuffix(stdout.String(), "\n\n") {
			t.Errorf("stdout = %q, want exactly one trailing newline: the separator belongs to stderr",
				stdout.String())
		}
		out := stdout.String()
		for _, absent := range []string{"by dispatch rule", "by prompt template", "self review", "turns (succeeded)", "tokens (measured runs)"} {
			if strings.Contains(out, absent) {
				t.Errorf("stdout contains %q, want it absent on the base tier", absent)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()

		wfPath := newStatsWorkspace(t, createLegacyStatsDB)
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats --format json) = %d, want 0; stderr: %s", code, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty in JSON mode (warnings travel in the envelope)", stderr.String())
		}

		var report statsReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
		}
		if report.SchemaTier != "base" {
			t.Errorf("SchemaTier = %q, want %q", report.SchemaTier, "base")
		}
		found := slices.ContainsFunc(report.Warnings, func(w string) bool {
			return strings.Contains(w, "was written before sortie recorded")
		})
		if !found {
			t.Errorf("Warnings = %v, want a degraded-schema warning", report.Warnings)
		}
		if len(report.ByRule) != 0 {
			t.Errorf("ByRule = %v, want empty on the base tier", report.ByRule)
		}
		if len(report.ByTemplate) != 0 {
			t.Errorf("ByTemplate = %v, want empty on the base tier", report.ByTemplate)
		}
		if report.SelfReview != nil {
			t.Errorf("SelfReview = %v, want nil on the base tier", report.SelfReview)
		}
		if report.Summary.Tokens != nil {
			t.Errorf("Summary.Tokens = %v, want nil on the base tier", report.Summary.Tokens)
		}
		if report.Summary.CostUSD != nil {
			t.Errorf("Summary.CostUSD = %v, want nil on the base tier", *report.Summary.CostUSD)
		}
		if report.Summary.TokensUnmeasuredRuns != 0 {
			t.Errorf("Summary.TokensUnmeasuredRuns = %d, want 0 on the base tier", report.Summary.TokensUnmeasuredRuns)
		}
	})
}

// --- config-resolution errors ---

func TestRunStatsConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing workflow file", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "/nonexistent/sortie-test-workflow.md"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(stats) = %d, want 1; stderr: %s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "workflow") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "workflow")
		}
	})

	t.Run("front matter value config.NewServiceConfig rejects", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow("db_path: 42\n"))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(stats) = %d, want 1; stderr: %s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "db_path") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "db_path")
		}
	})

	t.Run("missing database file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(stats) = %d, want 1; stderr: %s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "sortie stats:") {
			t.Errorf("stderr = %q, want the %q diagnostic prefix", stderr.String(), "sortie stats:")
		}
	})
}

// --- db_path resolution ---

func TestRunStatsDBPathResolution(t *testing.T) {
	// No t.Parallel: the SORTIE_DB_PATH subtest uses t.Setenv, which
	// requires the calling test and every ancestor to be non-parallel.

	oneRun := func(t *testing.T) []persistence.RunHistory {
		t.Helper()
		return []persistence.RunHistory{{
			IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, AgentAdapter: "mock", Workspace: "/tmp/ws",
			StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z", Status: "succeeded",
		}}
	}

	t.Run("absolute db_path", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		dbDir := t.TempDir()
		dbPath := filepath.Join(dbDir, "custom.db")
		createStatsDB(t, dbPath, oneRun(t)...)
		wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(fmt.Sprintf("db_path: %q\n", dbPath)))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats) = %d, want 0; stderr: %s", code, stderr.String())
		}
		var report statsReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
		}
		if report.DBPath != dbPath {
			t.Errorf("DBPath = %q, want %q", report.DBPath, dbPath)
		}
		if report.Summary.Runs != 1 {
			t.Errorf("Summary.Runs = %d, want 1", report.Summary.Runs)
		}
	})

	t.Run("relative db_path resolves against the workflow directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		dbPath := filepath.Join(dir, "custom.db")
		createStatsDB(t, dbPath, oneRun(t)...)
		wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow("db_path: custom.db\n"))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats) = %d, want 0; stderr: %s", code, stderr.String())
		}
		var report statsReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
		}
		if report.DBPath != dbPath {
			t.Errorf("DBPath = %q, want %q (resolved against the workflow directory)", report.DBPath, dbPath)
		}
	})

	t.Run("SORTIE_DB_PATH environment override", func(t *testing.T) {
		// No t.Parallel: t.Setenv requires a sequential test.

		dir := t.TempDir()
		dbDir := t.TempDir()
		dbPath := filepath.Join(dbDir, "env-override.db")
		createStatsDB(t, dbPath, oneRun(t)...)
		t.Setenv("SORTIE_DB_PATH", dbPath)
		wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))

		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"stats", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(stats) = %d, want 0; stderr: %s", code, stderr.String())
		}
		var report statsReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
		}
		if report.DBPath != dbPath {
			t.Errorf("DBPath = %q, want %q (from SORTIE_DB_PATH)", report.DBPath, dbPath)
		}
	})
}
