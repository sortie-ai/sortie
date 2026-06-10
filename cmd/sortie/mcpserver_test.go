package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
	"github.com/sortie-ai/sortie/internal/tool/status"
)

func TestRunMCPServer_Help_ReturnsZero(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMCPServer(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("runMCPServer(--help) = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "--workflow PATH") {
		t.Errorf("stdout = %q, want to contain %q", stdout.String(), "--workflow PATH")
	}
}

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

// buildMCPRequest constructs a newline-terminated JSON-RPC 2.0 request string.
func buildMCPRequest(t *testing.T, method string, id any, params any) string {
	t.Helper()
	type req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	b, err := json.Marshal(req{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		t.Fatalf("buildMCPRequest(%q): %v", method, err)
	}
	return string(b) + "\n"
}

// parseMCPResponses splits newline-delimited JSON into a slice of maps.
func parseMCPResponses(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var results []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("parseMCPResponses: unmarshal %q: %v", line, err)
		}
		results = append(results, m)
	}
	return results
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
	})

	reg := domain.NewToolRegistry()
	reg.Register(status.New(dir))

	input := buildMCPRequest(t, "tools/call", 1, map[string]any{
		"name":      "sortie_status",
		"arguments": map[string]any{},
	})

	var outBuf bytes.Buffer
	logger := slog.New(slog.DiscardHandler)
	srv := mcpserver.NewServer(reg, strings.NewReader(input), &outBuf, logger, "test")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	resps := parseMCPResponses(t, outBuf.Bytes())
	if len(resps) != 1 {
		t.Fatalf("response count = %d, want 1", len(resps))
	}
	resp := resps[0]

	if resp["error"] != nil {
		t.Fatalf("JSON-RPC error: %v", resp["error"])
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %v", resp["result"])
	}
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

	if got, ok := statusResp["turn_number"].(float64); !ok || int(got) != 7 {
		t.Errorf("turn_number = %v, want 7", statusResp["turn_number"])
	}
	if got, ok := statusResp["max_turns"].(float64); !ok || int(got) != 10 {
		t.Errorf("max_turns = %v, want 10", statusResp["max_turns"])
	}
	if got, ok := statusResp["turns_remaining"].(float64); !ok || int(got) != 3 {
		t.Errorf("turns_remaining = %v, want 3", statusResp["turns_remaining"])
	}

	tokens, ok := statusResp["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens is not an object: %v", statusResp["tokens"])
	}
	if got, ok := tokens["input_tokens"].(float64); !ok || got != 5000 {
		t.Errorf("tokens.input_tokens = %v, want 5000", tokens["input_tokens"])
	}
	if got, ok := tokens["output_tokens"].(float64); !ok || got != 1200 {
		t.Errorf("tokens.output_tokens = %v, want 1200", tokens["output_tokens"])
	}
	if got, ok := tokens["total_tokens"].(float64); !ok || got != 6200 {
		t.Errorf("tokens.total_tokens = %v, want 6200", tokens["total_tokens"])
	}
	if got, ok := tokens["cache_read_tokens"].(float64); !ok || got != 800 {
		t.Errorf("tokens.cache_read_tokens = %v, want 800", tokens["cache_read_tokens"])
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
// session_metadata row for the live session "sess-live" carrying 150 total
// tokens, then reopens it read-only as the sidecar does.
func seedBudgetStore(t *testing.T) *persistence.Store {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "budget.db")

	rw, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for i, total := range []int64{400, 200} {
		run := persistence.RunHistory{
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

	meta := persistence.SessionMetadata{
		IssueID:     "iss-1",
		SessionID:   "sess-live",
		TotalTokens: 150,
		UpdatedAt:   "2026-03-19T10:02:00Z",
	}
	if err := rw.UpsertSessionMetadata(ctx, meta); err != nil {
		t.Fatalf("UpsertSessionMetadata: %v", err)
	}

	if err := rw.Close(); err != nil {
		t.Fatalf("Close read-write store: %v", err)
	}

	ro, err := persistence.OpenReadOnly(ctx, dbPath)
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

	t.Run("matching running session id adds running total", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t))

		usage, err := query(context.Background(), "iss-1", "sess-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedTotalTokens != 600 {
			t.Errorf("CompletedTotalTokens = %d, want 600", usage.CompletedTotalTokens)
		}
		if usage.CompletedSessions != 2 {
			t.Errorf("CompletedSessions = %d, want 2", usage.CompletedSessions)
		}
		if usage.RunningTotalTokens != 150 {
			t.Errorf("RunningTotalTokens = %d, want 150 (session_id matches)", usage.RunningTotalTokens)
		}
	})

	t.Run("stale session row with different id is excluded", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t))

		// The stored row belongs to "sess-live"; the live session is a
		// newer one, so the stale row must not be double counted.
		usage, err := query(context.Background(), "iss-1", "sess-new")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedTotalTokens != 600 {
			t.Errorf("CompletedTotalTokens = %d, want 600", usage.CompletedTotalTokens)
		}
		if usage.RunningTotalTokens != 0 {
			t.Errorf("RunningTotalTokens = %d, want 0 (stale session row)", usage.RunningTotalTokens)
		}
	})

	t.Run("empty running session id contributes zero", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t))

		usage, err := query(context.Background(), "iss-1", "")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.RunningTotalTokens != 0 {
			t.Errorf("RunningTotalTokens = %d, want 0 (no running session id)", usage.RunningTotalTokens)
		}
	})

	t.Run("issue with no history returns zero usage", func(t *testing.T) {
		t.Parallel()

		query := buildBudgetQuery(seedBudgetStore(t))

		usage, err := query(context.Background(), "iss-none", "sess-live")
		if err != nil {
			t.Fatalf("buildBudgetQuery query: %v", err)
		}
		if usage.CompletedTotalTokens != 0 || usage.CompletedSessions != 0 || usage.RunningTotalTokens != 0 {
			t.Errorf("usage = %+v, want all-zero", usage)
		}
	})
}

func TestBuildNotifyTool_EmptyBackends_ReturnsNilNil(t *testing.T) {
	t.Parallel()

	tool, err := buildNotifyTool(nil)
	if err != nil {
		t.Fatalf("buildNotifyTool(nil) error = %v, want nil", err)
	}
	if tool != nil {
		t.Errorf("buildNotifyTool(nil) tool = %v, want nil", tool)
	}
}

func TestBuildNotifyTool_EmptySlice_ReturnsNilNil(t *testing.T) {
	t.Parallel()

	tool, err := buildNotifyTool([]config.NotificationBackend{})
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
			Config: map[string]any{"url": srv.URL},
		},
	}

	tool, err := buildNotifyTool(backends)
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

func TestBuildNotifyTool_ValidSlackBackend_ReturnsNonNilTool(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	backends := []config.NotificationBackend{
		{
			Kind:   "slack",
			Config: map[string]any{"webhook_url": srv.URL},
		},
	}

	tool, err := buildNotifyTool(backends)
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
			Config: map[string]any{"url": "https://example.com"},
		},
	}

	tool, err := buildNotifyTool(backends)
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
				{Kind: tt.kind, Config: tt.config},
			}

			tool, err := buildNotifyTool(backends)
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
		{Kind: "webhook", Config: map[string]any{"url": srv.URL}},
		{Kind: "unknown-kind-for-partial-test", Config: map[string]any{"url": srv.URL}},
	}

	tool, err := buildNotifyTool(backends)
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
