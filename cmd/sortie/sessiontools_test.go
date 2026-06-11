package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
)

// --- Test helpers ---

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
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: missing %q; got %v", label, w, got)
		}
	}
}

// assertContainsNone fails if any element of absent is present in got.
func assertContainsNone(t *testing.T, label string, got, absent []string) {
	t.Helper()
	for _, a := range absent {
		for _, g := range got {
			if g == a {
				t.Errorf("%s: unexpected tool %q present; got %v", label, a, got)
				break
			}
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

// --- Tests ---

// TestBuildSessionToolRegistry_AllToolsPresent verifies AC-3 served-side parity:
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
		SessionID:      "sess-1",
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

	// AC-3 cross-channel parity: tools/list must match the registry.
	mcpNames := toolNamesFromMCPServer(t, result)
	if len(mcpNames) != len(registryNames) {
		t.Errorf("tools/list len = %d, registry.Len() = %d; want equal", len(mcpNames), len(registryNames))
	}
	assertContainsAll(t, "tools/list", mcpNames, want)
}

// TestBuildSessionToolRegistry_GatingPreserved verifies AC-2: unset gates
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

// TestBuildSessionToolRegistry_MisconfiguredNotifier verifies AC-6: a
// misconfigured notifier backend (E-1) causes BuildSessionToolRegistry to
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

// TestBuildSessionToolRegistry_DBOpenDegradation verifies AC-7: a read-only
// open failure (E-2) produces a degraded success — workspace_history and
// cost_budget absent, result.Store nil, and the E-2 warning logged.
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

			// AC-7: no DB connection leaked.
			if result.Store != nil {
				_ = result.Store.Close()
				t.Errorf("BuildSessionToolRegistry(%q) result.Store non-nil, want nil", tt.name)
			}

			// AC-7: DB-backed tools absent after open failure.
			names := toolNamesFromResult(result)
			assertContainsNone(t, tt.name, names, []string{"workspace_history", "cost_budget"})

			// E-2 warning must have been emitted on the supplied logger.
			if !strings.Contains(logBuf.String(), "failed to open read-only db") {
				t.Errorf("BuildSessionToolRegistry(%q) E-2 warning not logged; got:\n%s", tt.name, logBuf.String())
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
