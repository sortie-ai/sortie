package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/storage/sqlite"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
	"github.com/sortie-ai/sortie/internal/tool/notify"
	"github.com/sortie-ai/sortie/internal/tool/status"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

var agentMessageEvents = []domain.EventType{domain.EventAgentMessage}

func TestRunMCPServer_MissingWorkflow_ReturnsOne(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runMCPServer(no flags) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--workflow flag is required") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "--workflow flag is required")
	}
}

func TestRunMCPServer_InvalidWorkflowPath_ReturnsOne(t *testing.T) {
	// Not parallel: calls logging.Setup which sets the global slog default.
	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"--workflow", "/nonexistent/WORKFLOW.md"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runMCPServer(nonexistent path) = %d, want 1", code)
	}
}

func TestRunMCPServer_HTTPWithoutBearer_ReturnsOne(t *testing.T) {
	for _, tt := range []struct {
		name  string
		unset bool
	}{
		{"bearer unset", true},
		{"bearer empty", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(mcpconfig.BearerEnv, "")
			if tt.unset {
				os.Unsetenv(mcpconfig.BearerEnv) //nolint:errcheck // t.Setenv restores the original value on cleanup
			}

			var stdout, stderr bytes.Buffer
			code := runMCPServer(context.Background(), []string{"--http", "--workflow", filepath.Join(t.TempDir(), "WORKFLOW.md")}, &stdout, &stderr)

			if code != 1 {
				t.Errorf("runMCPServer(--http, %s) = %d, want 1", tt.name, code)
			}
			if !strings.Contains(stderr.String(), mcpconfig.BearerEnv) {
				t.Errorf("runMCPServer(--http, %s) stderr = %q, want to contain %q", tt.name, stderr.String(), mcpconfig.BearerEnv)
			}
			if stdout.Len() != 0 {
				t.Errorf("runMCPServer(--http, %s) stdout = %q, want empty", tt.name, stdout.String())
			}
		})
	}
}

// writeMCPStateFile writes a state.json file to <dir>/.sortie/ for use in
// MCP server smoke tests.
func writeMCPStateFile(t *testing.T, dir string, data map[string]any) {
	t.Helper()
	dotSortie := filepath.Join(dir, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dotSortie, err)
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("json.Marshal state: %v", err)
	}
	dst := filepath.Join(dotSortie, "state.json")
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", dst, err)
	}
}

func serveMCP(t *testing.T, reg *domain.ToolRegistry, method string, params any) map[string]any {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	lines := make(chan []byte, 2)
	go func() {
		done <- mcpserver.NewServer(reg, slog.New(slog.DiscardHandler), "test").Serve(waittest.Context(t), inR, outW)
		_ = outW.Close()
	}()
	go func() {
		for scanner := bufio.NewScanner(outR); scanner.Scan(); {
			lines <- bytes.Clone(scanner.Bytes())
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		if err := waittest.Receive(t, done, "Serve to return"); err != nil {
			t.Errorf("Serve() = %v, want nil on end of input", err)
		}
	})

	write := func(request map[string]any) {
		t.Helper()
		b, err := json.Marshal(request)
		if err != nil {
			t.Fatalf("marshal %v: %v", request, err)
		}
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write %v: %v", request, err)
		}
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"},
	}})
	waittest.Receive(t, lines, "initialize response")
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})

	var resp struct {
		Result map[string]any
		Error  json.RawMessage
	}
	line := waittest.Receive(t, lines, method+" response")
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if resp.Error != nil {
		t.Fatalf("%s error = %s, want a result", method, resp.Error)
	}
	return resp.Result
}

func TestMCPServer_StatusTool_Dispatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeMCPStateFile(t, dir, map[string]any{
		"turn_number":       7,
		"max_turns":         10,
		"attempt":           nil,
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"input_tokens":      int64(5000),
		"output_tokens":     int64(1200),
		"total_tokens":      int64(6200),
		"cache_read_tokens": int64(800),
		"tokens_measured":   true,
	})

	reg := domain.NewToolRegistry()
	reg.Register(status.New(func(name string, maxBytes int64) ([]byte, error) {
		return workspacekit.ReadSortieFile(dir, name, maxBytes)
	}))

	result := serveMCP(t, reg, "tools/call", map[string]any{
		"name":      "sortie_status",
		"arguments": map[string]any{},
	})

	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want non-empty array", result["content"])
	}
	text, ok := content[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("content[0].text is not a string: %v", content[0])
	}

	var statusResp map[string]any
	if err := json.Unmarshal([]byte(text), &statusResp); err != nil {
		t.Fatalf("unmarshal status response %q: %v", text, err)
	}

	data, ok := statusResp["data"].(map[string]any)
	if !ok {
		t.Fatalf("statusResp[\"data\"] = %T %v, want map[string]any", statusResp["data"], statusResp["data"])
	}

	if got, ok := data["turn_number"].(float64); !ok || int(got) != 7 {
		t.Errorf("data.turn_number = %v, want 7", data["turn_number"])
	}
	if got, ok := data["max_turns"].(float64); !ok || int(got) != 10 {
		t.Errorf("data.max_turns = %v, want 10", data["max_turns"])
	}
	if got, ok := data["turns_remaining"].(float64); !ok || int(got) != 3 {
		t.Errorf("data.turns_remaining = %v, want 3", data["turns_remaining"])
	}

	tokens, ok := data["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("data.tokens is not an object: %v", data["tokens"])
	}
	if got, ok := tokens["input_tokens"].(float64); !ok || got != 5000 {
		t.Errorf("data.tokens.input_tokens = %v, want 5000", tokens["input_tokens"])
	}
	if got, ok := tokens["output_tokens"].(float64); !ok || got != 1200 {
		t.Errorf("data.tokens.output_tokens = %v, want 1200", tokens["output_tokens"])
	}
	if got, ok := tokens["total_tokens"].(float64); !ok || got != 6200 {
		t.Errorf("data.tokens.total_tokens = %v, want 6200", tokens["total_tokens"])
	}
	if got, ok := tokens["cache_read_tokens"].(float64); !ok || got != 800 {
		t.Errorf("data.tokens.cache_read_tokens = %v, want 800", tokens["cache_read_tokens"])
	}
}

func TestMCPServerShortHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runMCPServer([-h]) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--workflow") {
		t.Errorf("runMCPServer([-h]) stdout = %q, want to contain %q", stdout.String(), "--workflow")
	}
	if stderr.Len() != 0 {
		t.Errorf("runMCPServer([-h]) stderr = %q, want empty", stderr.String())
	}
}

// seedBudgetStore creates a migrated SQLite database with two completed
// run_history rows (400 + 200 total tokens) for issue "iss-1" and a
// session_metadata row for the live session "sess-live" carrying 150
// total tokens and the given dispatch ID, then reopens it read-only as
// the sidecar does.
func seedBudgetStore(t *testing.T, dispatchID string) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "budget.db")

	rw, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for i, total := range []int64{400, 200} {
		run := storage.RunHistory{
			IssueID:      "iss-1",
			Identifier:   "PROJ-1",
			Attempt:      i + 1,
			AgentAdapter: "mock",
			Workspace:    "/tmp/ws/PROJ-1",
			StartedAt:    fmt.Sprintf("2026-03-19T10:%02d:00Z", i),
			CompletedAt:  fmt.Sprintf("2026-03-19T10:%02d:30Z", i),
			Status:       "succeeded",
			TotalTokens:  total,
		}
		if _, err := rw.AppendRunHistory(ctx, run); err != nil {
			t.Fatalf("AppendRunHistory(attempt %d): %v", i+1, err)
		}
	}

	meta := storage.SessionMetadata{
		IssueID:     "iss-1",
		SessionID:   "sess-live",
		DispatchID:  dispatchID,
		TotalTokens: 150,
		UpdatedAt:   "2026-03-19T10:02:00Z",
	}
	for _, unaccounted := range []int{0, 2} {
		meta.UnaccountedTurns = unaccounted
		if err := rw.UpsertSessionMetadata(ctx, meta); err != nil {
			t.Fatalf("UpsertSessionMetadata(unaccounted %d): %v", unaccounted, err)
		}
	}

	if err := rw.Close(); err != nil {
		t.Fatalf("Close read-write store: %v", err)
	}

	ro, err := sqlite.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := ro.Close(); err != nil {
			t.Errorf("Close read-only store: %v", err)
		}
	})
	return ro
}

func TestBuildBudgetQuery(t *testing.T) {
	t.Parallel()

	t.Run("matching running dispatch id adds running total", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t, "dispatch-live"))

		usage, err := query(context.Background(), "iss-1", "dispatch-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedUncachedTokens != 600 {
			t.Errorf("CompletedUncachedTokens = %d, want 600", usage.CompletedUncachedTokens)
		}
		if usage.CompletedSessions != 2 {
			t.Errorf("CompletedSessions = %d, want 2", usage.CompletedSessions)
		}
		if usage.RunningUncachedTokens != 150 {
			t.Errorf("RunningUncachedTokens = %d, want 150 (dispatch_id matches)", usage.RunningUncachedTokens)
		}
		if usage.UnaccountedTurns != 2 {
			t.Errorf("UnaccountedTurns = %d, want 2 (running row's count after the conflict update)", usage.UnaccountedTurns)
		}
	})

	t.Run("stale dispatch row with different id is excluded", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t, "dispatch-live"))

		// The stored row belongs to "dispatch-live"; the live dispatch is a
		// newer one, so the stale row must not be double counted.
		usage, err := query(context.Background(), "iss-1", "dispatch-new")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedUncachedTokens != 600 {
			t.Errorf("CompletedUncachedTokens = %d, want 600", usage.CompletedUncachedTokens)
		}
		if usage.RunningUncachedTokens != 0 {
			t.Errorf("RunningUncachedTokens = %d, want 0 (stale dispatch row)", usage.RunningUncachedTokens)
		}
		if usage.UnaccountedTurns != 0 {
			t.Errorf("UnaccountedTurns = %d, want 0 (stale dispatch row)", usage.UnaccountedTurns)
		}
	})

	t.Run("session_id match with a cleared dispatch_id contributes nothing", func(t *testing.T) {
		t.Parallel()

		// The row's session_id equals the value queried below, mirroring
		// the pre-dispatch-ID session-exit write, but buildBudgetQuery
		// MUST NOT match on session_id.
		query := buildBudgetQuery(seedBudgetStore(t, ""))

		usage, err := query(context.Background(), "iss-1", "sess-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.RunningUncachedTokens != 0 {
			t.Errorf("RunningUncachedTokens = %d, want 0 (dispatch_id cleared, not a session_id match)", usage.RunningUncachedTokens)
		}
		if usage.RunningMeasured {
			t.Error("RunningMeasured = true, want false")
		}
	})

	t.Run("empty running dispatch id contributes zero", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t, ""))

		usage, err := query(context.Background(), "iss-1", "")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.RunningUncachedTokens != 0 {
			t.Errorf("RunningUncachedTokens = %d, want 0 (no running dispatch id)", usage.RunningUncachedTokens)
		}
	})

	t.Run("issue with no history returns zero usage", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t, "dispatch-live"))

		usage, err := query(context.Background(), "iss-none", "dispatch-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedUncachedTokens != 0 || usage.CompletedSessions != 0 || usage.RunningUncachedTokens != 0 {
			t.Errorf("usage = %+v, want all-zero", usage)
		}
	})
}

func seedBudgetMeasurementStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "budget-measurement.db")

	rw, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	runs := []storage.RunHistory{
		{
			IssueID: "iss-mixed", Identifier: "PROJ-MIXED", Attempt: 1, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-MIXED", StartedAt: "2026-03-19T10:00:00Z", CompletedAt: "2026-03-19T10:00:30Z",
			Status: "succeeded", TotalTokens: 400, TokensMeasured: true,
		},
		{
			IssueID: "iss-mixed", Identifier: "PROJ-MIXED", Attempt: 2, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-MIXED", StartedAt: "2026-03-19T10:01:00Z", CompletedAt: "2026-03-19T10:01:30Z",
			Status: "succeeded", TotalTokens: 0, TokensMeasured: false,
		},
		{
			IssueID: "iss-zero", Identifier: "PROJ-ZERO", Attempt: 1, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-ZERO", StartedAt: "2026-03-19T10:00:00Z", CompletedAt: "2026-03-19T10:00:30Z",
			Status: "succeeded", TotalTokens: 500, TokensMeasured: true,
		},
		{
			IssueID: "iss-priced", Identifier: "PROJ-PRICED", Attempt: 1, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-PRICED", StartedAt: "2026-03-19T10:00:00Z", CompletedAt: "2026-03-19T10:00:30Z",
			Status: "succeeded", TotalTokens: 300, TokensMeasured: true, EstimatedCostUSD: new(0.5), CostStatus: "estimated",
		},
		{
			IssueID: "iss-priced-gap", Identifier: "PROJ-GAP", Attempt: 1, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-GAP", StartedAt: "2026-03-19T10:00:00Z", CompletedAt: "2026-03-19T10:00:30Z",
			Status: "succeeded", TotalTokens: 300, TokensMeasured: true, EstimatedCostUSD: new(0.5), CostStatus: "estimated",
		},
		{
			IssueID: "iss-priced-gap", Identifier: "PROJ-GAP", Attempt: 2, AgentAdapter: "mock",
			Workspace: "/tmp/ws/PROJ-GAP", StartedAt: "2026-03-19T10:01:00Z", CompletedAt: "2026-03-19T10:01:30Z",
			Status: "succeeded", TotalTokens: 200, TokensMeasured: true,
		},
	}
	for i, run := range runs {
		if _, err := rw.AppendRunHistory(ctx, run); err != nil {
			t.Fatalf("AppendRunHistory(%d): %v", i, err)
		}
	}

	metas := []storage.SessionMetadata{
		{IssueID: "iss-mixed", SessionID: "sess-mixed-live", DispatchID: "dispatch-mixed-live", TotalTokens: 50, UpdatedAt: "2026-03-19T10:02:00Z"},
		{IssueID: "iss-zero", SessionID: "sess-zero-live", DispatchID: "dispatch-zero-live", TotalTokens: 0, UpdatedAt: "2026-03-19T10:02:00Z"},
		{IssueID: "iss-priced", SessionID: "sess-priced-live", DispatchID: "dispatch-priced-live", TotalTokens: 100, EstimatedCostUSD: new(0.125), CostStatus: "estimated", UpdatedAt: "2026-03-19T10:02:00Z"},
		{IssueID: "iss-priced-gap", SessionID: "sess-gap-live", DispatchID: "dispatch-gap-live", TotalTokens: 100, EstimatedCostUSD: new(0.125), CostStatus: "estimated", UpdatedAt: "2026-03-19T10:02:00Z"},
	}
	for _, meta := range metas {
		if err := rw.UpsertSessionMetadata(ctx, meta); err != nil {
			t.Fatalf("UpsertSessionMetadata(%s): %v", meta.IssueID, err)
		}
	}

	if err := rw.Close(); err != nil {
		t.Fatalf("Close read-write store: %v", err)
	}

	ro, err := sqlite.OpenReadOnly(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := ro.Close(); err != nil {
			t.Errorf("Close read-only store: %v", err)
		}
	})
	return ro
}

func TestBuildBudgetQuery_Measurement(t *testing.T) {
	t.Parallel()

	t.Run("UnmeasuredSessions comes from TokenUsageByIssue", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetMeasurementStore(t))

		usage, err := query(context.Background(), "iss-mixed", "dispatch-mixed-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.UnmeasuredSessions != 1 {
			t.Errorf("UnmeasuredSessions = %d, want 1", usage.UnmeasuredSessions)
		}
		if usage.CompletedUncachedTokens != 400 {
			t.Errorf("CompletedUncachedTokens = %d, want 400 (the unmeasured row contributes zero)", usage.CompletedUncachedTokens)
		}
		if !usage.RunningMeasured {
			t.Error("RunningMeasured = false, want true (session_metadata row matches the running dispatch id)")
		}
		if usage.RunningUncachedTokens != 50 {
			t.Errorf("RunningUncachedTokens = %d, want 50", usage.RunningUncachedTokens)
		}
	})

	t.Run("RunningMeasured false when no session_metadata row matches", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetMeasurementStore(t))

		usage, err := query(context.Background(), "iss-mixed", "dispatch-does-not-exist")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.RunningMeasured {
			t.Error("RunningMeasured = true, want false (no matching session_metadata row)")
		}
		if usage.RunningUncachedTokens != 0 {
			t.Errorf("RunningUncachedTokens = %d, want 0", usage.RunningUncachedTokens)
		}
	})

	t.Run("RunningMeasured true when the running session reported a measurement of zero", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetMeasurementStore(t))

		usage, err := query(context.Background(), "iss-zero", "dispatch-zero-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.UnmeasuredSessions != 0 {
			t.Errorf("UnmeasuredSessions = %d, want 0", usage.UnmeasuredSessions)
		}
		if !usage.RunningMeasured {
			t.Error("RunningMeasured = false, want true (the session_metadata row exists, reporting zero)")
		}
		if usage.RunningUncachedTokens != 0 {
			t.Errorf("RunningUncachedTokens = %d, want 0", usage.RunningUncachedTokens)
		}
		if usage.CompletedUncachedTokens != 500 {
			t.Errorf("CompletedUncachedTokens = %d, want 500", usage.CompletedUncachedTokens)
		}
	})
}

func TestBuildBudgetQuery_UsedUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		issueID    string
		dispatchID string
		recorded   pricing.RecordedCost
		session    pricing.SessionCost
		complete   bool
	}{
		{
			name:       "recorded run plus the running dispatch's estimate",
			issueID:    "iss-priced",
			dispatchID: "dispatch-priced-live",
			recorded:   pricing.RecordedCost{USD: 0.5, Priced: 1},
			session:    pricing.SessionCost{USD: new(0.125), Status: pricing.CostEstimated, Spent: true},
			complete:   true,
		},
		{
			name:       "another dispatch's row adds nothing",
			issueID:    "iss-priced",
			dispatchID: "dispatch-other",
			recorded:   pricing.RecordedCost{USD: 0.5, Priced: 1},
		},
		{
			name:       "an unpriced recorded run keeps the figure incomplete",
			issueID:    "iss-priced-gap",
			dispatchID: "dispatch-gap-live",
			recorded:   pricing.RecordedCost{USD: 0.5, Priced: 1, Unpriced: 1},
			session:    pricing.SessionCost{USD: new(0.125), Status: pricing.CostEstimated, Spent: true},
			complete:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			query := buildBudgetQuery(seedBudgetMeasurementStore(t))
			wantUSD, wantStatus := pricing.IssueCost(tt.recorded, tt.session, tt.complete)

			usage, err := query(context.Background(), tt.issueID, tt.dispatchID)
			if err != nil {
				t.Fatalf("buildBudgetQuery query: %v", err)
			}

			if usage.UsedUSD != wantUSD || usage.UsedUSDComplete != (wantStatus == pricing.CostEstimated) {
				t.Errorf("query(%q, %q) = UsedUSD %v, UsedUSDComplete %v, want %v, %v",
					tt.issueID, tt.dispatchID, usage.UsedUSD, usage.UsedUSDComplete, wantUSD, wantStatus == pricing.CostEstimated)
			}
		})
	}
}

// TestSessionToolParamsFromEnv covers env-to-SessionToolParams mapping:
// every variable runMCPServer read before the dispatch ID change maps to
// the same field, SORTIE_DISPATCH_ID maps to DispatchID, and a
// map-backed getenv fully determines the result while the process
// environment holds different SORTIE_* values.
//
// No t.Parallel: uses t.Setenv which is incompatible with t.Parallel.
func TestSessionToolParamsFromEnv(t *testing.T) {
	env := map[string]string{
		"SORTIE_WORKSPACE":          "/ws",
		"SORTIE_DB_PATH":            "/db.sqlite",
		"SORTIE_ISSUE_ID":           "issue-1",
		"SORTIE_ISSUE_IDENTIFIER":   "PROJ-1",
		"SORTIE_DISPATCH_ID":        "dispatch-1",
		"SORTIE_ATTEMPT":            "3",
		"SORTIE_SESSION_AGENT_KIND": "mock",
	}
	getenv := func(key string) string { return env[key] }

	// The process environment holds different SORTIE_* values than the
	// map, so a result that matches the map rather than the process
	// proves getenv is the only source consulted.
	t.Setenv("SORTIE_WORKSPACE", "/process-ws")
	t.Setenv("SORTIE_DB_PATH", "/process-db.sqlite")
	t.Setenv("SORTIE_ISSUE_ID", "process-issue")
	t.Setenv("SORTIE_ISSUE_IDENTIFIER", "PROC-1")
	t.Setenv("SORTIE_DISPATCH_ID", "process-dispatch")
	t.Setenv("SORTIE_ATTEMPT", "9")
	t.Setenv("SORTIE_SESSION_AGENT_KIND", "process-agent")

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{Project: "PROJ"},
		Agent:   config.AgentConfig{MaxTokens: 1000, MaxSessions: 5, TokenWarningPercent: 80},
	}
	tracker := &stubTrackerAdapter{}

	params := sessionToolParamsFromEnv(getenv, cfg, tracker)

	if params.WorkspacePath != "/ws" {
		t.Errorf("WorkspacePath = %q, want %q", params.WorkspacePath, "/ws")
	}
	if params.DBPath != "/db.sqlite" {
		t.Errorf("DBPath = %q, want %q", params.DBPath, "/db.sqlite")
	}
	if params.IssueID != "issue-1" {
		t.Errorf("IssueID = %q, want %q", params.IssueID, "issue-1")
	}
	if params.Identifier != "PROJ-1" {
		t.Errorf("Identifier = %q, want %q", params.Identifier, "PROJ-1")
	}
	if params.DispatchID != "dispatch-1" {
		t.Errorf("DispatchID = %q, want %q", params.DispatchID, "dispatch-1")
	}
	if params.Attempt == nil || *params.Attempt != 3 {
		t.Errorf("Attempt = %v, want pointer to 3", params.Attempt)
	}
	if params.AgentKind != "mock" {
		t.Errorf("AgentKind = %q, want %q", params.AgentKind, "mock")
	}
	if params.Project != "PROJ" {
		t.Errorf("Project = %q, want %q", params.Project, "PROJ")
	}
	if params.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d, want 1000", params.MaxTokens)
	}
	if params.MaxSessions != 5 {
		t.Errorf("MaxSessions = %d, want 5", params.MaxSessions)
	}
	if want := cfg.Agent.TokenWarningThreshold(); params.TokenWarningThreshold != want {
		t.Errorf("TokenWarningThreshold = %d, want %d (cfg.Agent.TokenWarningThreshold())", params.TokenWarningThreshold, want)
	}
	if params.TrackerAdapter != tracker {
		t.Error("TrackerAdapter does not equal the adapter passed in")
	}
}

// TestSessionToolParamsFromEnv_Attempt covers the SORTIE_ATTEMPT parse
// rule in isolation: a non-integer value and an absent value both leave
// Attempt nil, matching runMCPServer's behavior before this change.
func TestSessionToolParamsFromEnv_Attempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  map[string]string
	}{
		{"absent leaves Attempt nil", map[string]string{}},
		{"non-integer leaves Attempt nil", map[string]string{"SORTIE_ATTEMPT": "not-a-number"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string { return tt.env[key] }
			params := sessionToolParamsFromEnv(getenv, config.ServiceConfig{}, nil)
			if params.Attempt != nil {
				t.Errorf("Attempt = %v, want nil", *params.Attempt)
			}
		})
	}
}

func testNotifySessionIDFunc() string { return "" }

type noopSlotClaim struct{}

func (noopSlotClaim) Commit()  {}
func (noopSlotClaim) Release() {}

func testAlwaysReserveSlot(int) (notify.SlotClaim, bool, error) { return noopSlotClaim{}, true, nil }

func TestBuildNotifyTool_EmptyBackends_ReturnsNilNil(t *testing.T) {
	t.Parallel()

	tool, err := buildNotifyTool(nil, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err != nil {
		t.Fatalf("buildNotifyTool(nil) error = %v, want nil", err)
	}
	if tool != nil {
		t.Errorf("buildNotifyTool(nil) tool = %v, want nil", tool)
	}
}

func TestBuildNotifyTool_EmptySlice_ReturnsNilNil(t *testing.T) {
	t.Parallel()

	tool, err := buildNotifyTool([]config.NotificationBackend{}, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err != nil {
		t.Fatalf("buildNotifyTool(empty) error = %v, want nil", err)
	}
	if tool != nil {
		t.Errorf("buildNotifyTool(empty) tool = %v, want nil", tool)
	}
}

func TestBuildNotifyTool_ValidWebhookBackend_ReturnsNonNilTool(t *testing.T) {
	t.Parallel()

	// Use a real httptest server so the webhook constructor does not
	// reject the URL. The server URL is non-empty so construction succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backends := []config.NotificationBackend{
		{
			Kind:   "webhook",
			Events: agentMessageEvents,
			Config: map[string]any{"url": srv.URL},
		},
	}

	tool, err := buildNotifyTool(backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err != nil {
		t.Fatalf("buildNotifyTool(webhook) error = %v, want nil", err)
	}
	if tool == nil {
		t.Fatal("buildNotifyTool(webhook) tool = nil, want non-nil")
	}
	if tool.Name() != "notify_operator" {
		t.Errorf("tool.Name() = %q, want %q", tool.Name(), "notify_operator")
	}
}

func TestBuildNotifyTool_PropagatesSessionID(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read posted body: %v", err)
		}
		captured = b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backends := []config.NotificationBackend{
		{Kind: "webhook", Events: agentMessageEvents, Config: map[string]any{"url": srv.URL}},
	}
	env := notify.NotificationEnvelopeContext{DispatchID: "dispatch-reaches-tool"}
	sessionID := func() string { return "session-reaches-tool" }

	tool, err := buildNotifyTool(backends, env, sessionID, testAlwaysReserveSlot)
	if err != nil {
		t.Fatalf("buildNotifyTool: %v", err)
	}
	if tool == nil {
		t.Fatal("buildNotifyTool tool = nil, want non-nil")
	}

	raw, execErr := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B"}`))
	if execErr != nil {
		t.Fatalf("Execute: %v", execErr)
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal Execute result: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute result success = false: %s", raw)
	}

	var body map[string]any
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("unmarshal posted body %q: %v", captured, err)
	}
	if got, _ := body["dispatch_id"].(string); got != env.DispatchID {
		t.Errorf("posted body[dispatch_id] = %q, want %q", got, env.DispatchID)
	}
	if got, _ := body["session_id"].(string); got != sessionID() {
		t.Errorf("posted body[session_id] = %q, want %q", got, sessionID())
	}
}

func TestBuildNotifyTool_ValidSlackBackend_ReturnsNonNilTool(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backends := []config.NotificationBackend{
		{
			Kind:   "slack",
			Events: agentMessageEvents,
			Config: map[string]any{"webhook_url": srv.URL},
		},
	}

	tool, err := buildNotifyTool(backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err != nil {
		t.Fatalf("buildNotifyTool(slack) error = %v, want nil", err)
	}
	if tool == nil {
		t.Fatal("buildNotifyTool(slack) tool = nil, want non-nil")
	}
}

func TestBuildNotifyTool_UnknownKind_ReturnsError(t *testing.T) {
	t.Parallel()

	backends := []config.NotificationBackend{
		{
			Kind:   "no-such-backend-kind-xyz",
			Events: agentMessageEvents,
			Config: map[string]any{"url": "https://example.com"},
		},
	}

	tool, err := buildNotifyTool(backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err == nil {
		t.Fatal("buildNotifyTool(unknown kind) error = nil, want non-nil error")
	}
	if tool != nil {
		t.Errorf("buildNotifyTool(unknown kind) tool = %v, want nil on error", tool)
	}
}

func TestBuildNotifyTool_EmptyRequiredSecret_ReturnsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		kind   string
		config map[string]any
	}{
		{
			name:   "webhook empty url",
			kind:   "webhook",
			config: map[string]any{"url": ""},
		},
		{
			name:   "slack empty webhook_url",
			kind:   "slack",
			config: map[string]any{"webhook_url": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backends := []config.NotificationBackend{
				{Kind: tt.kind, Events: agentMessageEvents, Config: tt.config},
			}

			tool, err := buildNotifyTool(backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
			if err == nil {
				t.Fatalf("buildNotifyTool(%q, empty secret) error = nil, want fatal constructor error", tt.kind)
			}
			if tool != nil {
				t.Errorf("buildNotifyTool(%q, empty secret) tool = %v, want nil on error", tt.kind, tool)
			}
		})
	}
}

func TestBuildNotifyTool_PartialFailureIsTotal(t *testing.T) {
	t.Parallel()

	// First backend valid, second backend has an unknown kind. The result
	// must be an error, not a partial set of backends.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backends := []config.NotificationBackend{
		{Kind: "webhook", Events: agentMessageEvents, Config: map[string]any{"url": srv.URL}},
		{Kind: "unknown-kind-for-partial-test", Events: agentMessageEvents, Config: map[string]any{"url": srv.URL}},
	}

	tool, err := buildNotifyTool(backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, testAlwaysReserveSlot)
	if err == nil {
		t.Fatal("buildNotifyTool(partial failure) = nil error, want non-nil (no partial registration)")
	}
	if tool != nil {
		t.Errorf("buildNotifyTool(partial failure) tool = %v, want nil", tool)
	}
}

func TestResolveNotificationCap_AllZero_ReturnsDefault(t *testing.T) {
	t.Parallel()

	backends := []config.NotificationBackend{
		{Kind: "webhook", MaxPerSession: 0},
		{Kind: "slack", MaxPerSession: 0},
	}

	got := resolveNotificationCap(backends)
	if got != defaultMaxPerSession {
		t.Errorf("resolveNotificationCap(all-zero) = %d, want default %d", got, defaultMaxPerSession)
	}
}

func TestResolveNotificationCap_EmptySlice_ReturnsDefault(t *testing.T) {
	t.Parallel()

	got := resolveNotificationCap(nil)
	if got != defaultMaxPerSession {
		t.Errorf("resolveNotificationCap(nil) = %d, want default %d", got, defaultMaxPerSession)
	}
}

func TestResolveNotificationCap_SingleNonZero_ReturnsThatValue(t *testing.T) {
	t.Parallel()

	backends := []config.NotificationBackend{
		{Kind: "webhook", MaxPerSession: 15},
	}

	got := resolveNotificationCap(backends)
	if got != 15 {
		t.Errorf("resolveNotificationCap({15}) = %d, want 15", got)
	}
}

func TestResolveNotificationCap_MultipleNonZero_ReturnsMax(t *testing.T) {
	t.Parallel()

	backends := []config.NotificationBackend{
		{Kind: "webhook", MaxPerSession: 5},
		{Kind: "slack", MaxPerSession: 30},
	}

	got := resolveNotificationCap(backends)
	if got != 30 {
		t.Errorf("resolveNotificationCap({5,30}) = %d, want 30", got)
	}
}

func TestResolveNotificationCap_MixedZeroAndNonZero_ReturnsNonZeroMax(t *testing.T) {
	t.Parallel()

	backends := []config.NotificationBackend{
		{Kind: "webhook", MaxPerSession: 0},
		{Kind: "slack", MaxPerSession: 10},
	}

	got := resolveNotificationCap(backends)
	if got != 10 {
		t.Errorf("resolveNotificationCap({0,10}) = %d, want 10", got)
	}
}

// mcpServerWorkflow returns a minimal WORKFLOW.md body suitable for
// runMCPServer tests. No tracker section is included; only what is
// required to pass workflow.Load and config.NewServiceConfig without
// error. The notifications list is injected as an optional YAML block
// so tests can control whether buildNotifyTool succeeds.
func mcpServerWorkflow(notificationsYAML string) []byte {
	base := "---\npolling:\n  interval_ms: 30000\nagent:\n  kind: mock\n"
	if notificationsYAML != "" {
		base += notificationsYAML + "\n"
	}
	base += "---\nDo something.\n"
	return []byte(base)
}

// emptyStdin gives Serve its own handle, as it closes stdin. Not for parallel
// tests: it swaps the process-wide os.Stdin.
func emptyStdin(t *testing.T) {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		_ = f.Close()
	})
}

// TestRunMCPServer_SuccessPath exercises the section of runMCPServer
// starting at line 106: SORTIE_ATTEMPT parsing, env-to-SessionToolParams
// mapping, BuildSessionToolRegistry call, and the final Serve invocation.
// Under go test, os.Stdin is /dev/null so Serve returns on EOF and the
// function exits 0.
//
// No t.Parallel: uses t.Setenv which is incompatible with t.Parallel.
func TestRunMCPServer_SuccessPath_ReturnsZero(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	seedDB(t, dbPath)

	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, mcpServerWorkflow(""), 0o644); err != nil {
		t.Fatalf("WriteFile WORKFLOW.md: %v", err)
	}

	// Wire all optional env vars so the covered block is fully exercised.
	t.Setenv("SORTIE_ATTEMPT", "2")
	t.Setenv("SORTIE_WORKSPACE", dir)
	t.Setenv("SORTIE_DB_PATH", dbPath)
	t.Setenv("SORTIE_ISSUE_ID", "issue-99")
	t.Setenv("SORTIE_ISSUE_IDENTIFIER", "PROJ-99")
	t.Setenv("SORTIE_SESSION_AGENT_KIND", "mock")

	emptyStdin(t)

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"--workflow", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runMCPServer(success) = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestRunMCPServer_BuilderError_ReturnsOne verifies that a misconfigured
// notifier backend in the workflow causes BuildSessionToolRegistry to
// return an error, which runMCPServer logs and returns 1 for.
//
// No t.Parallel: uses t.Setenv which is incompatible with t.Parallel.
func TestRunMCPServer_BuilderError_ReturnsOne(t *testing.T) {
	dir := t.TempDir()

	badNotifications := `notifications:
  - kind: no-such-backend-xyz
    url: https://example.com`
	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, mcpServerWorkflow(badNotifications), 0o644); err != nil {
		t.Fatalf("WriteFile WORKFLOW.md: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"--workflow", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("runMCPServer(builder error) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to build session tool registry") {
		t.Errorf("runMCPServer(builder error) stderr = %q, want to contain %q",
			stderr.String(), "failed to build session tool registry")
	}
}

// TestRunMCPServer_AttemptEnvParsed verifies that a non-integer
// SORTIE_ATTEMPT is silently ignored (attempt stays nil) rather than
// causing an error, and runMCPServer still returns 0.
//
// No t.Parallel: uses t.Setenv which is incompatible with t.Parallel.
func TestRunMCPServer_InvalidAttemptIgnored_ReturnsZero(t *testing.T) {
	dir := t.TempDir()

	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, mcpServerWorkflow(""), 0o644); err != nil {
		t.Fatalf("WriteFile WORKFLOW.md: %v", err)
	}

	t.Setenv("SORTIE_ATTEMPT", "not-a-number")

	emptyStdin(t)

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"--workflow", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runMCPServer(invalid attempt) = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestBuildSessionToolRegistry_EnvFree asserts that BuildSessionToolRegistry
// returns an identical tool-name set whether or not SORTIE_* env vars are
// set. The PR moved all env reads out of the builder; this test locks that
// property.
//
// No t.Parallel: uses t.Setenv which is incompatible with t.Parallel.
func TestBuildSessionToolRegistry_EnvFree(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "envfree.db")
	seedDB(t, dbPath)

	params := SessionToolParams{
		TrackerAdapter: &stubTrackerAdapter{},
		Project:        "ENVFREE",
		WorkspacePath:  tmpDir,
		DBPath:         dbPath,
		IssueID:        "issue-envfree",
		MaxTokens:      50000,
		MaxSessions:    5,
		Notifications:  []config.NotificationBackend{webhookBackend(t)},
	}

	// Build without any SORTIE_* vars in scope (they are not set here).
	result1, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry(no env) error = %v, want nil", err)
	}
	t.Cleanup(func() { closeResult(t, result1) })
	names1 := toolNamesFromResult(result1)
	sort.Strings(names1)

	// Build again with a full complement of SORTIE_* vars. The result must
	// be identical because the builder ignores the process environment.
	t.Setenv("SORTIE_WORKSPACE", tmpDir)
	t.Setenv("SORTIE_DB_PATH", dbPath)
	t.Setenv("SORTIE_ISSUE_ID", "env-issue-override")
	t.Setenv("SORTIE_ISSUE_IDENTIFIER", "ENV-1")
	t.Setenv("SORTIE_ATTEMPT", "3")
	t.Setenv("SORTIE_SESSION_AGENT_KIND", "env-agent")

	result2, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry(with env) error = %v, want nil", err)
	}
	t.Cleanup(func() { closeResult(t, result2) })
	names2 := toolNamesFromResult(result2)
	sort.Strings(names2)

	if !slices.Equal(names1, names2) {
		t.Errorf("BuildSessionToolRegistry tool names differ when SORTIE_* env set:\n  no env: %v\n with env: %v",
			names1, names2)
	}
}

func TestBuildNotifyTool_SelectsOnlyAgentMessageSubscribers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	webhook := func(maxPerSession int, events ...domain.EventType) config.NotificationBackend {
		return config.NotificationBackend{
			Kind: "webhook", MaxPerSession: maxPerSession, Events: events, EventsDeclared: true,
			Config: map[string]any{"url": srv.URL},
		}
	}
	unconstructible := config.NotificationBackend{
		Kind: "no-such-backend-kind-xyz", Events: []domain.EventType{domain.EventBudgetHeld}, EventsDeclared: true,
	}

	tests := []struct {
		name     string
		backends []config.NotificationBackend
		wantTool bool
		wantCap  int
	}{
		{
			name:     "only orchestrator events registers nothing and builds nothing",
			backends: []config.NotificationBackend{unconstructible, webhook(3, domain.EventSessionFailed)},
		},
		{
			name:     "a tracker comment entry registers nothing",
			backends: []config.NotificationBackend{{Kind: domain.TrackerCommentKind, Events: []domain.EventType{domain.EventBudgetHeld}, EventsDeclared: true}},
		},
		{
			name:     "the cap comes from the agent message subset",
			backends: []config.NotificationBackend{webhook(2, domain.EventAgentMessage), webhook(9, domain.EventSessionFailed), unconstructible},
			wantTool: true,
			wantCap:  2,
		},
		{
			name:     "an entry written without events keeps its default cap",
			backends: []config.NotificationBackend{{Kind: "webhook", Events: agentMessageEvents, MaxPerSession: 0, Config: map[string]any{"url": srv.URL}}},
			wantTool: true,
			wantCap:  defaultMaxPerSession,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotLimit int
			reserve := func(limit int) (notify.SlotClaim, bool, error) {
				gotLimit = limit
				return noopSlotClaim{}, true, nil
			}

			tool, err := buildNotifyTool(tt.backends, notify.NotificationEnvelopeContext{}, testNotifySessionIDFunc, reserve)
			if err != nil {
				t.Fatalf("buildNotifyTool(%s) error = %v, want nil", tt.name, err)
			}
			if (tool != nil) != tt.wantTool {
				t.Fatalf("buildNotifyTool(%s) tool = %v, want tool: %v", tt.name, tool, tt.wantTool)
			}
			if tool == nil {
				return
			}

			if _, err := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B"}`)); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if gotLimit != tt.wantCap {
				t.Errorf("slot limit = %d, want %d", gotLimit, tt.wantCap)
			}
		})
	}
}
