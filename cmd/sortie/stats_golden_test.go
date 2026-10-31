package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
)

const goldenStatsDir = "testdata/golden_stats"

func goldenStatsRuns() []persistence.RunHistory {
	const (
		start   = "2026-01-10T10:00:00Z"
		ranFor  = "2026-01-10T10:10:00Z"
		ranLong = "2026-01-10T10:30:00Z"
	)
	runs := []persistence.RunHistory{
		{
			AgentAdapter: "claude-code", Status: "succeeded", TurnsCompleted: 3, CompletedAt: ranFor,
			RuleName: "implement", TemplateID: "tpl-a", ConfiguredModel: "claude-sonnet",
			InputTokens: 1_000_000, OutputTokens: 200_000, TotalTokens: 1_200_000, CacheReadTokens: 50_000, CacheWriteTokens: 10_000, TokensMeasured: true,
			EstimatedCostUSD: new(5.8725), CostStatus: "estimated",
			ChainID: "chain-1", StageTarget: "review", StageResult: "advanced",
			ReviewMetadata: new(`{"enabled":true,"total_iterations":2,"final_verdict":"pass","cap_reached":false}`),
		},
		{
			AgentAdapter: "claude-code", Status: "succeeded", TurnsCompleted: 5, CompletedAt: ranLong,
			RuleName: "review", TemplateID: "tpl-b", ConfiguredModel: "claude-opus", Trigger: new("ci_fix"),
			InputTokens: 2_000_000, OutputTokens: 400_000, TotalTokens: 2_400_000, TokensMeasured: true,
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			ChainID: "chain-1", StagePrevious: "implement", StagePreviousOutcome: "advanced",
			ReviewMetadata: new(`{"enabled":true,"total_iterations":3,"final_verdict":"iterate","cap_reached":true}`),
		},
		{
			AgentAdapter: "claude-code", Status: "failed", TurnsCompleted: 2, CompletedAt: "2026-01-10T10:05:00Z",
			RuleName: "implement", TemplateID: "tpl-a", ConfiguredModel: "claude-sonnet",
			InputTokens: 300_000, OutputTokens: 50_000, TotalTokens: 350_000, TokensMeasured: true,
			EstimatedCostUSD: new(1.65), CostStatus: "estimated",
			ChainID: "chain-2", StageResult: "failed",
		},
		{
			AgentAdapter: "claude-code", Status: "ci_failed", CompletedAt: start, Trigger: new(""),
			InputTokens: 100_000, OutputTokens: 10_000, TotalTokens: 110_000, TokensMeasured: true,
		},
		{
			AgentAdapter: "codex", Status: "succeeded", TurnsCompleted: 4, CompletedAt: "2026-01-10T10:20:00Z",
			RuleName: "implement", ConfiguredModel: "gpt-5",
			InputTokens: 500_000, OutputTokens: 100_000, TotalTokens: 600_000, TokensMeasured: true, CostStatus: "unpriced",
			ReviewMetadata: new(`not json`),
		},
		{
			Status: "failed", TurnsCompleted: 1, StartedAt: "yesterday", CompletedAt: ranFor,
		},
		{
			AgentAdapter: "claude-code", Status: "succeeded", TurnsCompleted: 2, CompletedAt: "2026-01-10T10:15:00Z",
			RuleName: "implement", TemplateID: "tpl-a", ConfiguredModel: "claude-sonnet",
			InputTokens: 400_000, OutputTokens: 80_000, TotalTokens: 480_000, TokensMeasured: true, UnaccountedTurns: 1,
			EstimatedCostUSD: new(2.015), CostStatus: "partial",
			UsageCompleteness: persistence.UsageLowerBound, UsageProvenance: "journal:records,output:live",
		},
		{
			AgentAdapter: "copilot", Status: "succeeded", TurnsCompleted: 1, CompletedAt: "2026-01-10T10:05:00Z",
			RuleName: "implement", CostStatus: "unmeasured",
			UsageCompleteness: persistence.UsageUnmeasured,
			ReportedAmounts:   []persistence.ReportedAmount{{Unit: "credits", Amount: 12.5}},
		},
		{
			AgentAdapter: "copilot", Status: "failed", TurnsCompleted: 1, CompletedAt: "2026-01-10T10:02:00Z",
			RuleName: "implement", CostStatus: "unmeasured",
			UsageCompleteness: persistence.UsageUnmeasured,
		},
		{
			AgentAdapter: "gemini", Status: "succeeded", TurnsCompleted: 3, CompletedAt: "2026-01-10T10:12:00Z",
			RuleName: "review", TemplateID: "tpl-b", ConfiguredModel: "gemini-x",
			InputTokens: 600_000, OutputTokens: 100_000, TotalTokens: 700_000, TokensMeasured: true,
			EstimatedCostUSD: new(0.4), CostStatus: "estimated",
			UsageCompleteness: persistence.UsageComplete, UsageProvenance: "protocol:live",
			ModelUsage: map[string]persistence.ModelTokens{"gemini-x": {InputTokens: 600_000, OutputTokens: 100_000}},
			ModelCosts: map[string]persistence.ModelCost{"gemini-x": {EstimatedCostUSD: new(0.4), CostStatus: "estimated", PriceSource: "agent"}},
		},
	}
	for i := range runs {
		runs[i].IssueID = fmt.Sprintf("ISS-%d", i+1)
		runs[i].Identifier = fmt.Sprintf("PROJ-%d", i+1)
		runs[i].Attempt = 1
		runs[i].Workspace = "/tmp/ws"
		if runs[i].StartedAt == "" {
			runs[i].StartedAt = start
		}
	}
	runs[1].IssueID = runs[0].IssueID
	return runs
}

func jsonEncodedText(t *testing.T, s string) string {
	t.Helper()

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", s, err)
	}
	return string(b[1 : len(b)-1])
}

func checkGoldenStats(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join(goldenStatsDir, name)
	if *recordGolden {
		if err := os.MkdirAll(goldenStatsDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", goldenStatsDir, err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden fixture %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden %s differs\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestRunStatsMatchesGolden(t *testing.T) {
	pinStatsNow(t, time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC))

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))
	dbPath := filepath.Join(dir, ".sortie.db")
	createStatsDB(t, dbPath, goldenStatsRuns()...)

	rangeArgs := []string{"--since", "2026-01-01T00:00:00Z", "--until", "2026-02-01T00:00:00Z", wfPath}
	tests := []struct {
		name   string
		golden string
		args   []string
		wf, db string
	}{
		{"json", "report.json", append([]string{"stats", "--format", "json"}, rangeArgs...), jsonEncodedText(t, wfPath), jsonEncodedText(t, dbPath)},
		{"text", "report.txt", append([]string{"stats"}, rangeArgs...), wfPath, dbPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(context.Background(), tt.args, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("run(%v) = %d, want 0; stderr: %s", tt.args, code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("run(%v) stderr = %q, want empty", tt.args, stderr.String())
			}
			normalized := strings.NewReplacer(tt.wf, "WORKFLOW_PATH", tt.db, "DB_PATH").Replace(stdout.String())
			checkGoldenStats(t, tt.golden, []byte(normalized))
		})
	}
}

func decodeReportWithoutClock(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var report map[string]any
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", body, err)
	}
	delete(report, "generated_at")
	return report
}

func TestStatsRouteEqualsCLIReport(t *testing.T) {
	t.Parallel()

	const since, until = "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"
	tests := []struct {
		name string
		runs []persistence.RunHistory
	}{
		{"golden database", goldenStatsRuns()},
		{"database without runs", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			wfPath := writeCustomWorkflowFile(t, dir, statsWorkflow(""))
			dbPath := filepath.Join(dir, ".sortie.db")
			createStatsDB(t, dbPath, tt.runs...)
			srv := server.New(server.Params{
				SnapshotFn: func() (orchestrator.RuntimeSnapshotResult, error) { return orchestrator.RuntimeSnapshotResult{}, nil },
				RefreshFn:  func() bool { return true },
				Stats:      statsSource{dbPath: dbPath, workflowPath: wfPath}.report,
			})
			ts := httptest.NewServer(srv.Mux())
			t.Cleanup(ts.Close)

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"stats", "--format", "json", "--since", since, "--until", until, wfPath}, &stdout, &stderr)
			resp, err := http.Get(ts.URL + "/api/v1/stats?since=" + since + "&until=" + until)
			if err != nil {
				t.Fatalf("GET /api/v1/stats: %v", err)
			}
			defer resp.Body.Close() //nolint:errcheck // test response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response body: %v", err)
			}

			if code != 0 || resp.StatusCode != http.StatusOK {
				t.Fatalf("CLI exit = %d (stderr %q), route status = %d, want 0 and 200", code, stderr.String(), resp.StatusCode)
			}
			cli, route := decodeReportWithoutClock(t, stdout.Bytes()), decodeReportWithoutClock(t, body)
			if !reflect.DeepEqual(route, cli) {
				t.Errorf("route report = %v, want the CLI report %v", route, cli)
			}
			if len(tt.runs) > 0 {
				return
			}
			summary, _ := route["summary"].(map[string]any)
			if byTrigger, ok := route["by_trigger"].([]any); summary["runs"] != 0.0 || summary["issues"] != 0.0 || !ok || len(byTrigger) != 0 {
				t.Errorf("empty report summary = %v, by_trigger = %v, want runs and issues 0 and by_trigger []", summary, route["by_trigger"])
			}
		})
	}
}
