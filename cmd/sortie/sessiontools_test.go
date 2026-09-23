package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
)

// webhookBackend returns a NotificationBackend pointing at a live httptest
// server. The server is closed via t.Cleanup.
func webhookBackend(t *testing.T) config.NotificationBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return config.NotificationBackend{
		Kind:   "webhook",
		Config: map[string]any{"url": srv.URL},
	}
}

// seedDB creates a minimal SQLite database at dbPath with the schema migrated.
func seedDB(t *testing.T, dbPath string) {
	t.Helper()
	ctx := context.Background()
	rw, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("seedDB: Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("seedDB: Migrate: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("seedDB: Close: %v", err)
	}
}

// closeResult closes result.Store if non-nil. Call via t.Cleanup.
func closeResult(t *testing.T, result SessionToolRegistry) {
	t.Helper()
	if result.Store != nil {
		if err := result.Store.Close(); err != nil {
			t.Errorf("SessionToolRegistry.Store.Close: %v", err)
		}
	}
}

// toolNamesFromResult returns the tool names from the registry inside result.
func toolNamesFromResult(result SessionToolRegistry) []string {
	names := make([]string, 0, result.Registry.Len())
	for _, tool := range result.Registry.List() {
		names = append(names, tool.Name())
	}
	return names
}

// toolNamesFromMCPServer sends tools/list to an mcpserver.Server built from
// result.Registry and returns the reported tool names.
func toolNamesFromMCPServer(t *testing.T, result SessionToolRegistry) []string {
	t.Helper()
	req := buildMCPRequest(t, "tools/list", 1, nil)
	logger := slog.New(slog.DiscardHandler)
	var outBuf bytes.Buffer
	srv := mcpserver.NewServer(result.Registry, strings.NewReader(req), &outBuf, logger, "test")
	if err := srv.Serve(context.Background()); err != nil {
		t.Fatalf("toolNamesFromMCPServer: Serve: %v", err)
	}
	resps := parseMCPResponses(t, outBuf.Bytes())
	if len(resps) == 0 {
		t.Fatal("toolNamesFromMCPServer: no response from MCP server")
	}
	if resps[0]["error"] != nil {
		t.Fatalf("toolNamesFromMCPServer: JSON-RPC error: %v", resps[0]["error"])
	}
	rmap, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("toolNamesFromMCPServer: result is not an object: %v", resps[0]["result"])
	}
	tools, _ := rmap["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if m, ok := tool.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				names = append(names, name)
			}
		}
	}
	return names
}

// testLogger returns a slog.Logger that captures output at Debug level into buf.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// assertContainsAll fails if any element of want is absent from got.
func assertContainsAll(t *testing.T, label string, got, want []string) {
	t.Helper()
	for _, w := range want {
		found := slices.Contains(got, w)
		if !found {
			t.Errorf("%s: missing %q; got %v", label, w, got)
		}
	}
}

// assertContainsNone fails if any element of absent is present in got.
func assertContainsNone(t *testing.T, label string, got, absent []string) {
	t.Helper()
	for _, a := range absent {
		if slices.Contains(got, a) {
			t.Errorf("%s: unexpected tool %q present; got %v", label, a, got)
		}
	}
}

// stubTrackerAdapter satisfies domain.TrackerAdapter for tests that only
// require tracker_api registration without exercising any adapter methods.
type stubTrackerAdapter struct{}

var _ domain.TrackerAdapter = (*stubTrackerAdapter)(nil)

func (s *stubTrackerAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *stubTrackerAdapter) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *stubTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *stubTrackerAdapter) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *stubTrackerAdapter) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *stubTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *stubTrackerAdapter) TransitionIssue(_ context.Context, _ string, _ string) error { return nil }
func (s *stubTrackerAdapter) CommentIssue(_ context.Context, _ string, _ string) error    { return nil }
func (s *stubTrackerAdapter) AddLabel(_ context.Context, _ string, _ string) error        { return nil }

// TestBuildSessionToolRegistry_AllToolsPresent verifies served-side parity:
// all five expected tools appear in the built registry and in the names served
// over tools/list for an equivalent session.
func TestBuildSessionToolRegistry_AllToolsPresent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	seedDB(t, dbPath)

	params := SessionToolParams{
		TrackerAdapter: &stubTrackerAdapter{},
		Project:        "TESTPROJ",
		WorkspacePath:  tmpDir,
		DBPath:         dbPath,
		IssueID:        "issue-1",
		MaxTokens:      100000,
		MaxSessions:    10,
		Notifications:  []config.NotificationBackend{webhookBackend(t)},
	}

	result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry(full) error = %v, want nil", err)
	}
	t.Cleanup(func() { closeResult(t, result) })

	want := []string{"tracker_api", "sortie_status", "workspace_history", "cost_budget", "notify_operator"}

	registryNames := toolNamesFromResult(result)
	assertContainsAll(t, "registry", registryNames, want)

	// Cross-channel parity: tools/list must match the registry.
	mcpNames := toolNamesFromMCPServer(t, result)
	if len(mcpNames) != len(registryNames) {
		t.Errorf("tools/list len = %d, registry.Len() = %d; want equal", len(mcpNames), len(registryNames))
	}
	assertContainsAll(t, "tools/list", mcpNames, want)
}

// TestBuildSessionToolRegistry_TokenWarningThreshold verifies that
// SessionToolParams.TokenWarningThreshold reaches the registered
// cost_budget tool's behavior, and that a zero value produces a
// pre-change byte-identical result.
func TestBuildSessionToolRegistry_TokenWarningThreshold(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "warning.db")
	seedDB(t, dbPath)

	baseParams := SessionToolParams{
		TrackerAdapter: &stubTrackerAdapter{},
		Project:        "TESTPROJ",
		WorkspacePath:  tmpDir,
		DBPath:         dbPath,
		IssueID:        "issue-warning",
		MaxTokens:      1000,
		MaxSessions:    10,
	}

	executeCostBudget := func(t *testing.T, params SessionToolParams) map[string]any {
		t.Helper()
		result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
		if err != nil {
			t.Fatalf("BuildSessionToolRegistry: %v", err)
		}
		t.Cleanup(func() { closeResult(t, result) })

		tool, ok := result.Registry.Get("cost_budget")
		if !ok {
			t.Fatal("cost_budget tool not registered")
		}
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("cost_budget Execute: %v", err)
		}
		var top map[string]any
		if err := json.Unmarshal(out, &top); err != nil {
			t.Fatalf("unmarshal cost_budget response %q: %v", out, err)
		}
		data, ok := top["data"].(map[string]any)
		if !ok {
			t.Fatalf("cost_budget data = %T, want map", top["data"])
		}
		return data
	}

	t.Run("non-zero threshold reaches the cost_budget tool", func(t *testing.T) {
		t.Parallel()

		params := baseParams
		params.TokenWarningThreshold = 800

		data := executeCostBudget(t, params)

		got, ok := data["warning_tokens"].(float64)
		if !ok || int64(got) != 800 {
			t.Errorf("cost_budget data.warning_tokens = %v, want 800", data["warning_tokens"])
		}
	})

	t.Run("zero threshold produces a pre-change byte-identical result", func(t *testing.T) {
		t.Parallel()

		params := baseParams
		params.TokenWarningThreshold = 0

		data := executeCostBudget(t, params)

		for _, key := range []string{"warning_tokens", "warning_reached"} {
			if _, present := data[key]; present {
				t.Errorf("cost_budget data has key %q, want absent when TokenWarningThreshold is 0", key)
			}
		}
	})
}

// TestBuildSessionToolRegistry_GatingPreserved verifies that unset gates
// remove the corresponding tools from the built registry.
func TestBuildSessionToolRegistry_GatingPreserved(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "gate.db")
	seedDB(t, dbPath)

	tests := []struct {
		name        string
		params      SessionToolParams
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name: "no project disables tracker_api",
			params: SessionToolParams{
				TrackerAdapter: &stubTrackerAdapter{},
				Project:        "",
				WorkspacePath:  tmpDir,
			},
			wantAbsent:  []string{"tracker_api"},
			wantPresent: []string{"sortie_status"},
		},
		{
			name: "nil tracker adapter disables tracker_api",
			params: SessionToolParams{
				TrackerAdapter: nil,
				Project:        "PROJ",
				WorkspacePath:  tmpDir,
			},
			wantAbsent:  []string{"tracker_api"},
			wantPresent: []string{"sortie_status"},
		},
		{
			name: "no notifications disables notify_operator",
			params: SessionToolParams{
				TrackerAdapter: &stubTrackerAdapter{},
				Project:        "PROJ",
				WorkspacePath:  tmpDir,
				Notifications:  nil,
			},
			wantAbsent:  []string{"notify_operator"},
			wantPresent: []string{"tracker_api", "sortie_status"},
		},
		{
			name: "empty workspace path disables sortie_status",
			params: SessionToolParams{
				TrackerAdapter: &stubTrackerAdapter{},
				Project:        "PROJ",
				WorkspacePath:  "",
			},
			wantAbsent:  []string{"sortie_status"},
			wantPresent: []string{"tracker_api"},
		},
		{
			name: "missing dbpath disables db tools",
			params: SessionToolParams{
				TrackerAdapter: &stubTrackerAdapter{},
				Project:        "PROJ",
				WorkspacePath:  tmpDir,
				DBPath:         "",
				IssueID:        "issue-1",
			},
			wantAbsent:  []string{"workspace_history", "cost_budget"},
			wantPresent: []string{"tracker_api", "sortie_status"},
		},
		{
			name: "missing issue id disables db tools",
			params: SessionToolParams{
				TrackerAdapter: &stubTrackerAdapter{},
				Project:        "PROJ",
				WorkspacePath:  tmpDir,
				DBPath:         dbPath,
				IssueID:        "",
			},
			wantAbsent:  []string{"workspace_history", "cost_budget"},
			wantPresent: []string{"tracker_api", "sortie_status"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), tt.params)
			if err != nil {
				t.Fatalf("BuildSessionToolRegistry(%q) error = %v, want nil", tt.name, err)
			}
			t.Cleanup(func() { closeResult(t, result) })

			names := toolNamesFromResult(result)
			assertContainsNone(t, tt.name, names, tt.wantAbsent)
			assertContainsAll(t, tt.name, names, tt.wantPresent)
		})
	}
}

// TestBuildSessionToolRegistry_MisconfiguredNotifier verifies that a
// misconfigured notifier backend causes BuildSessionToolRegistry to
// return a non-nil error and no store is leaked.
func TestBuildSessionToolRegistry_MisconfiguredNotifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend config.NotificationBackend
	}{
		{
			name: "unknown notifier kind",
			backend: config.NotificationBackend{
				Kind:   "no-such-backend-xyz",
				Config: map[string]any{"url": "https://example.com"},
			},
		},
		{
			name: "webhook empty url",
			backend: config.NotificationBackend{
				Kind:   "webhook",
				Config: map[string]any{"url": ""},
			},
		},
		{
			name: "slack empty webhook_url",
			backend: config.NotificationBackend{
				Kind:   "slack",
				Config: map[string]any{"webhook_url": ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			params := SessionToolParams{
				Notifications: []config.NotificationBackend{tt.backend},
			}

			result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
			if err == nil {
				t.Fatalf("BuildSessionToolRegistry(%q) error = nil, want non-nil", tt.name)
			}
			// Confirm no store connection is leaked on construction failure.
			if result.Store != nil {
				_ = result.Store.Close()
				t.Errorf("BuildSessionToolRegistry(%q) result.Store non-nil on error, want nil", tt.name)
			}
		})
	}
}

// TestBuildSessionToolRegistry_DBOpenDegradation verifies that a read-only
// open failure produces a degraded success; workspace_history and
// cost_budget absent, result.Store nil, and the degradation warning logged.
func TestBuildSessionToolRegistry_DBOpenDegradation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a file that is not a valid SQLite database so OpenReadOnly fails.
	garbagePath := filepath.Join(tmpDir, "garbage.db")
	if err := os.WriteFile(garbagePath, []byte("this is not a valid sqlite database\x00\x01\x02"), 0o600); err != nil {
		t.Fatalf("write garbage db: %v", err)
	}

	tests := []struct {
		name    string
		dbPath  string
		issueID string
	}{
		{
			name:    "nonexistent database file",
			dbPath:  filepath.Join(tmpDir, "nonexistent", "no.db"),
			issueID: "issue-1",
		},
		{
			name:    "garbage database file",
			dbPath:  garbagePath,
			issueID: "issue-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			logger := testLogger(&logBuf)

			params := SessionToolParams{
				DBPath:  tt.dbPath,
				IssueID: tt.issueID,
			}

			result, err := BuildSessionToolRegistry(context.Background(), logger, params)
			if err != nil {
				t.Fatalf("BuildSessionToolRegistry(%q) error = %v, want nil (degraded success)", tt.name, err)
			}

			// No DB connection leaked.
			if result.Store != nil {
				_ = result.Store.Close()
				t.Errorf("BuildSessionToolRegistry(%q) result.Store non-nil, want nil", tt.name)
			}

			// DB-backed tools absent after open failure.
			names := toolNamesFromResult(result)
			assertContainsNone(t, tt.name, names, []string{"workspace_history", "cost_budget"})

			// The degradation warning must have been emitted on the supplied logger.
			if !strings.Contains(logBuf.String(), "failed to open read-only db") {
				t.Errorf("BuildSessionToolRegistry(%q) degradation warning not logged; got:\n%s", tt.name, logBuf.String())
			}
		})
	}
}

// TestBuildSessionToolRegistry_NilLoggerDoesNotPanic confirms that passing a
// nil logger is safe and falls back to slog.Default().
func TestBuildSessionToolRegistry_NilLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	result, err := BuildSessionToolRegistry(context.Background(), nil, SessionToolParams{})
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry(nil logger) error = %v, want nil", err)
	}
	if result.Registry == nil {
		t.Error("BuildSessionToolRegistry(nil logger) result.Registry = nil, want non-nil")
	}
	if result.Store != nil {
		_ = result.Store.Close()
	}
}

// TestBuildSessionToolRegistry_EmptyParamsEmptyRegistry confirms that empty
// SessionToolParams produces an empty but non-nil registry with no error and
// no store.
func TestBuildSessionToolRegistry_EmptyParamsEmptyRegistry(t *testing.T) {
	t.Parallel()

	result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), SessionToolParams{})
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry(empty) error = %v, want nil", err)
	}
	if result.Registry == nil {
		t.Fatal("BuildSessionToolRegistry(empty) result.Registry = nil, want non-nil")
	}
	if result.Registry.Len() != 0 {
		t.Errorf("BuildSessionToolRegistry(empty) registry.Len() = %d, want 0", result.Registry.Len())
	}
	if result.Store != nil {
		_ = result.Store.Close()
		t.Errorf("BuildSessionToolRegistry(empty) result.Store non-nil, want nil")
	}
}

// TestBuildSessionToolRegistry_StoreOpenedThenNotifierError covers the
// branch at sessiontools.go:142-144: the read-only store opens successfully
// (DBPath+IssueID are valid) but buildNotifyTool returns an error due to a
// misconfigured backend. The builder must close the open store and return
// the zero-value SessionToolRegistry with a non-nil error so no store
// connection is leaked.
func TestBuildSessionToolRegistry_StoreOpenedThenNotifierError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend config.NotificationBackend
	}{
		{
			name: "unknown notifier kind with open store",
			backend: config.NotificationBackend{
				Kind:   "no-such-backend-xyz",
				Config: map[string]any{"url": "https://example.com"},
			},
		},
		{
			name: "webhook empty url with open store",
			backend: config.NotificationBackend{
				Kind:   "webhook",
				Config: map[string]any{"url": ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "storeopen.db")
			seedDB(t, dbPath)

			params := SessionToolParams{
				DBPath:        dbPath,
				IssueID:       "issue-storeopen",
				Notifications: []config.NotificationBackend{tt.backend},
			}

			result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
			if err == nil {
				t.Fatalf("BuildSessionToolRegistry(%q) error = nil, want non-nil", tt.name)
			}
			if result.Store != nil {
				_ = result.Store.Close()
				t.Errorf("BuildSessionToolRegistry(%q) result.Store non-nil on error, want nil (store leaked)", tt.name)
			}
			if result.Registry != nil {
				t.Errorf("BuildSessionToolRegistry(%q) result.Registry non-nil on error, want nil", tt.name)
			}
		})
	}
}

func TestBuildSessionToolRegistry_NotifyOperatorWithoutIdentity(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".sortie"), 0o750); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}

	tests := []struct {
		name          string
		workspacePath string
		dispatchID    string
	}{
		{name: "missing workspace path", workspacePath: "", dispatchID: "dispatch-1"},
		{name: "missing dispatch id", workspacePath: tmpDir, dispatchID: ""},
		{name: "both missing", workspacePath: "", dispatchID: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var captured []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			params := SessionToolParams{
				WorkspacePath: tt.workspacePath,
				DispatchID:    tt.dispatchID,
				Notifications: []config.NotificationBackend{
					{Kind: "webhook", Config: map[string]any{"url": srv.URL}},
				},
			}

			result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
			if err != nil {
				t.Fatalf("BuildSessionToolRegistry(%q) error = %v, want nil", tt.name, err)
			}
			t.Cleanup(func() { closeResult(t, result) })

			names := toolNamesFromResult(result)
			if !slices.Contains(names, "notify_operator") {
				t.Fatalf("BuildSessionToolRegistry(%q) tool names = %v, want notify_operator present", tt.name, names)
			}

			tool, ok := result.Registry.Get("notify_operator")
			if !ok {
				t.Fatalf("BuildSessionToolRegistry(%q): notify_operator not retrievable from registry", tt.name)
			}

			raw, execErr := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B"}`))
			if execErr != nil {
				t.Fatalf("Execute: %v", execErr)
			}
			var result2 struct {
				Success bool `json:"success"`
				Error   struct {
					Kind string `json:"kind"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &result2); err != nil {
				t.Fatalf("unmarshal Execute result: %v", err)
			}
			if result2.Success {
				t.Fatalf("Execute result success = true, want false: %s", raw)
			}
			if result2.Error.Kind != "state_unavailable" {
				t.Errorf("Execute result error.kind = %q, want %q", result2.Error.Kind, "state_unavailable")
			}

			if captured != nil {
				t.Errorf("backend received a request %q, want none reached without dispatch identity", captured)
			}
		})
	}
}

func TestBuildSessionToolRegistry_CreatesNoFileBeforeExecute(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, ".sortie"), 0o750); err != nil {
		t.Fatalf("MkdirAll(.sortie): %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	params := SessionToolParams{
		WorkspacePath: tmpDir,
		DispatchID:    "dispatch-construction",
		Notifications: []config.NotificationBackend{
			{Kind: "webhook", Config: map[string]any{"url": srv.URL}},
		},
	}

	result, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry error = %v, want nil", err)
	}
	t.Cleanup(func() { closeResult(t, result) })

	entries, readErr := os.ReadDir(filepath.Join(tmpDir, ".sortie", "notification_slots"))
	if readErr == nil {
		t.Errorf("notification_slots directory exists with entries %v before Execute, want absent", entries)
	} else if !os.IsNotExist(readErr) {
		t.Fatalf("ReadDir(notification_slots) unexpected error: %v", readErr)
	}
}
