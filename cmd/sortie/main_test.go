package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/sortie-ai/sortie/internal/config"
)

// minimalWorkflow returns a minimal valid WORKFLOW.md content that
// includes tracker (file) and agent (mock) config needed for the
// full startup sequence.
func minimalWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
---
Do {{ .issue.title }}.
`)
}

// writeWorkflowFile creates a WORKFLOW.md in dir and returns its absolute path.
func writeWorkflowFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, minimalWorkflow(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeIssuesFixture creates a minimal issues.json fixture in dir for
// the file tracker adapter.
func writeIssuesFixture(t *testing.T, dir string) {
	t.Helper()
	issues := []map[string]any{
		{
			"id": "10001", "identifier": "PROJ-1",
			"title": "Test issue", "state": "To Do",
			"priority": 1, "labels": []string{},
			"comments": []any{}, "blocked_by": []any{},
		},
	}
	data, err := json.Marshal(issues)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "issues.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupRunDir creates a temp directory with WORKFLOW.md and issues.json
// fixture, sets CWD to that directory, and returns the workflow path.
func setupRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	return writeWorkflowFile(t, dir)
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q", out, "sortie "+Version)
	}
	if !strings.Contains(out, "Copyright") {
		t.Errorf("stdout = %q, want to contain %q", out, "Copyright")
	}
	if !strings.Contains(out, "warranty") {
		t.Errorf("stdout = %q, want to contain %q", out, "warranty")
	}
}

func TestRunDumpVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("stdout = %q, want %q", got, Version)
	}
}

func TestRunDumpVersionOverridesVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "-dumpversion"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("-dumpversion should take precedence; stdout = %q, want %q", got, Version)
	}
}

func TestRunVersionIgnoresWorkflowPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{"--version", "/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--unknown"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain usage text")
	}
}

func TestRunTooManyArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"a", "b"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "too many arguments") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "too many arguments")
	}
}

func TestRunNonexistentPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"/nonexistent/workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunMissingDefaultWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "sortie:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "sortie:")
	}
}

func TestRunValidWorkflowWithTimeout(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunAlreadyCancelledContext(t *testing.T) {
	// With a pre-cancelled context, the DB open fails immediately.
	// The startup sequence correctly returns exit code 1.
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
}

func TestRunPortFlagLogged(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{"--port", "0", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "port=0") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "port=0")
	}
	if !strings.Contains(stderr.String(), "http server listening") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server listening")
	}
}

func TestResolveWorkflowPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		args    []string
		wantEnd string
		wantErr bool
	}{
		{
			name:    "no args defaults to WORKFLOW.md",
			args:    []string{},
			wantEnd: "WORKFLOW.md",
		},
		{
			name:    "single arg returns absolute",
			args:    []string{"my-file.md"},
			wantEnd: fmt.Sprintf("%s/my-file.md", cwd),
		},
		{
			name:    "absolute arg returned as-is",
			args:    []string{"/tmp/wf.md"},
			wantEnd: "/tmp/wf.md",
		},
		{
			name:    "two args returns error",
			args:    []string{"a", "b"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWorkflowPath(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !filepath.IsAbs(got) {
				t.Errorf("path %q is not absolute", got)
			}
			if tt.wantEnd != "" && !strings.HasSuffix(got, filepath.Base(tt.wantEnd)) {
				t.Errorf("path = %q, want to end with %q", got, filepath.Base(tt.wantEnd))
			}
			if len(tt.args) == 1 && filepath.IsAbs(tt.args[0]) {
				if got != tt.wantEnd {
					t.Errorf("path = %q, want %q", got, tt.wantEnd)
				}
			}
		})
	}
}

func TestRunDatabaseCreatedNextToWorkflow(t *testing.T) {
	// The database must be created adjacent to WORKFLOW.md, not in the
	// process working directory. Set CWD to a separate temp directory
	// so the two locations differ.
	workflowDir := t.TempDir()
	cwdDir := t.TempDir()
	t.Chdir(cwdDir)

	writeIssuesFixture(t, workflowDir)
	wfPath := writeWorkflowFile(t, workflowDir)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// .sortie.db must exist next to WORKFLOW.md.
	dbNextToWorkflow := filepath.Join(workflowDir, ".sortie.db")
	if _, err := os.Stat(dbNextToWorkflow); err != nil {
		t.Errorf("expected database at %s, got error: %v", dbNextToWorkflow, err)
	}

	// .sortie.db must NOT exist in the process CWD.
	dbInCwd := filepath.Join(cwdDir, ".sortie.db")
	if _, err := os.Stat(dbInCwd); err == nil {
		t.Errorf("database should not exist in CWD at %s", dbInCwd)
	}
}

func TestRunPreflightFailureSkipsDBCreation(t *testing.T) {
	// When preflight validation fails (here: missing tracker.kind),
	// the database file must not be created. This exercises the
	// startup ordering: preflight runs before DB open.
	workflowDir := t.TempDir()

	// Write a workflow that loads and starts but fails preflight
	// because tracker.kind is absent.
	content := []byte(`---
polling:
  interval_ms: 30000
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
	wfPath := filepath.Join(workflowDir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (preflight should fail); stderr: %s", code, stderr.String())
	}

	// .sortie.db must NOT exist — DB open should not have run.
	dbPath := filepath.Join(workflowDir, ".sortie.db")
	if _, err := os.Stat(dbPath); err == nil {
		t.Errorf("database file should not exist at %s when preflight fails", dbPath)
	}
}

// --- resolveDBPath tests ---

func TestResolveDBPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfgPath     string
		workflowDir string
		want        string
	}{
		{
			name:        "empty falls back to default",
			cfgPath:     "",
			workflowDir: "/project",
			want:        "/project/.sortie.db",
		},
		{
			name:        "absolute path used as-is",
			cfgPath:     "/data/custom.db",
			workflowDir: "/project",
			want:        "/data/custom.db",
		},
		{
			name:        "relative path joined with workflowDir",
			cfgPath:     "subdir/my.db",
			workflowDir: "/project",
			want:        "/project/subdir/my.db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolveDBPath(tt.cfgPath, tt.workflowDir)
			if got != tt.want {
				t.Errorf("resolveDBPath(%q, %q) = %q, want %q", tt.cfgPath, tt.workflowDir, got, tt.want)
			}
		})
	}
}

// --- Database path integration tests ---

// writeWorkflowFileWithDBPath creates a WORKFLOW.md in dir with a
// custom db_path field and returns its absolute path.
func writeWorkflowFileWithDBPath(t *testing.T, dir, dbPath string) string {
	t.Helper()
	content := fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
db_path: "%s"
file:
  path: issues.json
---
Do {{ .issue.title }}.
`, dbPath)
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunDatabaseCustomPath(t *testing.T) {
	workflowDir := t.TempDir()
	dbDir := t.TempDir()

	writeIssuesFixture(t, workflowDir)
	dbFile := filepath.Join(dbDir, "custom.db")
	wfPath := writeWorkflowFileWithDBPath(t, workflowDir, dbFile)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// custom.db must exist at the configured absolute path.
	if _, err := os.Stat(dbFile); err != nil {
		t.Errorf("expected database at %s, got error: %v", dbFile, err)
	}

	// .sortie.db must NOT exist next to WORKFLOW.md.
	defaultDB := filepath.Join(workflowDir, ".sortie.db")
	if _, err := os.Stat(defaultDB); err == nil {
		t.Errorf("default database should not exist at %s", defaultDB)
	}
}

func TestRunDatabaseRelativePath(t *testing.T) {
	workflowDir := t.TempDir()
	cwdDir := t.TempDir()
	t.Chdir(cwdDir)

	writeIssuesFixture(t, workflowDir)

	// Create the subdirectory inside the workflow directory.
	subdir := filepath.Join(workflowDir, "data")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	wfPath := writeWorkflowFileWithDBPath(t, workflowDir, "data/my.db")

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// data/my.db must exist inside the workflow directory.
	relDB := filepath.Join(workflowDir, "data", "my.db")
	if _, err := os.Stat(relDB); err != nil {
		t.Errorf("expected database at %s, got error: %v", relDB, err)
	}

	// data/ must NOT exist in CWD — confirms resolution against workflow dir.
	cwdData := filepath.Join(cwdDir, "data")
	if _, err := os.Stat(cwdData); err == nil {
		t.Errorf("data/ should not exist in CWD at %s", cwdData)
	}

	// .sortie.db must NOT exist next to WORKFLOW.md.
	defaultDB := filepath.Join(workflowDir, ".sortie.db")
	if _, err := os.Stat(defaultDB); err == nil {
		t.Errorf("default database should not exist at %s", defaultDB)
	}
}

// --- Config map completeness tests ---

// toSnakeCase converts a PascalCase field name to snake_case, handling
// acronyms like "MS", "API", "ID" correctly: APIKey → api_key,
// TurnTimeoutMS → turn_timeout_ms, MaxConcurrentByState → max_concurrent_by_state.
func toSnakeCase(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := runes[i-1]
				if unicode.IsLower(prev) {
					b.WriteRune('_')
				} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					b.WriteRune('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestTrackerConfigMapCompleteness(t *testing.T) {
	t.Parallel()

	m := trackerConfigMap(config.TrackerConfig{})
	rt := reflect.TypeOf(config.TrackerConfig{})

	for _, field := range reflect.VisibleFields(rt) {
		if !field.IsExported() {
			continue
		}
		key := toSnakeCase(field.Name)
		if _, ok := m[key]; !ok {
			t.Errorf("trackerConfigMap missing key %q for field %s", key, field.Name)
		}
	}
}

func TestAgentConfigMapCompleteness(t *testing.T) {
	t.Parallel()

	m := agentConfigMap(config.AgentConfig{})
	rt := reflect.TypeOf(config.AgentConfig{})

	// Orchestrator-only fields are intentionally excluded from the
	// adapter config map. They are consumed by the orchestrator via
	// typed config.AgentConfig and would shadow adapter extension
	// keys of the same name during mergeExtensions.
	excluded := map[string]bool{
		"MaxTurns":             true,
		"MaxConcurrentAgents":  true,
		"MaxRetryBackoffMS":    true,
		"MaxConcurrentByState": true,
		"MaxSessions":          true,
	}

	for _, field := range reflect.VisibleFields(rt) {
		if !field.IsExported() || excluded[field.Name] {
			continue
		}
		key := toSnakeCase(field.Name)
		if _, ok := m[key]; !ok {
			t.Errorf("agentConfigMap missing key %q for field %s", key, field.Name)
		}
	}
}

func TestAgentConfigMapExcludesOrchestratorFields(t *testing.T) {
	t.Parallel()

	cfg := config.AgentConfig{
		Kind:                 "claude-code",
		Command:              "claude",
		TurnTimeoutMS:        3600000,
		ReadTimeoutMS:        5000,
		StallTimeoutMS:       300000,
		MaxConcurrentAgents:  10,
		MaxTurns:             20,
		MaxRetryBackoffMS:    300000,
		MaxConcurrentByState: map[string]int{"open": 5},
	}

	m := agentConfigMap(cfg)

	excluded := []string{
		"max_turns",
		"max_concurrent_agents",
		"max_retry_backoff_ms",
		"max_concurrent_agents_by_state",
		"max_sessions",
	}
	for _, key := range excluded {
		if _, ok := m[key]; ok {
			t.Errorf("agentConfigMap contains orchestrator-only key %q", key)
		}
	}

	required := []string{
		"kind",
		"command",
		"turn_timeout_ms",
		"read_timeout_ms",
		"stall_timeout_ms",
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("agentConfigMap missing required key %q", key)
		}
	}
}

// --- mergeExtensions tests ---

func TestMergeExtensions(t *testing.T) {
	t.Parallel()

	t.Run("copies extension keys", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		extensions := map[string]any{
			"file": map[string]any{"path": "issues.json", "extra": 42},
		}

		mergeExtensions(dst, extensions, "file")

		if dst["path"] != "issues.json" {
			t.Errorf("path = %v, want %q", dst["path"], "issues.json")
		}
		if dst["extra"] != 42 {
			t.Errorf("extra = %v, want 42", dst["extra"])
		}
	})

	t.Run("does not overwrite existing keys", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file", "path": "original.json"}
		extensions := map[string]any{
			"file": map[string]any{"path": "overridden.json"},
		}

		mergeExtensions(dst, extensions, "file")

		if dst["path"] != "original.json" {
			t.Errorf("path = %v, want %q (should not overwrite)", dst["path"], "original.json")
		}
	})

	t.Run("missing kind is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "jira"}
		extensions := map[string]any{
			"file": map[string]any{"path": "issues.json"},
		}

		mergeExtensions(dst, extensions, "jira")

		if _, ok := dst["path"]; ok {
			t.Error("path should not be set when kind has no extensions")
		}
	})

	t.Run("nil extensions is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		mergeExtensions(dst, nil, "file")

		if len(dst) != 1 {
			t.Errorf("dst has %d keys, want 1", len(dst))
		}
	})

	t.Run("non-map extension value is no-op", func(t *testing.T) {
		t.Parallel()

		dst := map[string]any{"kind": "file"}
		extensions := map[string]any{
			"file": "not a map",
		}

		mergeExtensions(dst, extensions, "file")

		if len(dst) != 1 {
			t.Errorf("dst has %d keys, want 1", len(dst))
		}
	})

	t.Run("adapter max_turns passthrough", func(t *testing.T) {
		t.Parallel()

		dst := agentConfigMap(config.AgentConfig{MaxTurns: 5})
		extensions := map[string]any{
			"claude-code": map[string]any{"max_turns": float64(50)},
		}

		mergeExtensions(dst, extensions, "claude-code")

		got, ok := dst["max_turns"]
		if !ok {
			t.Fatal("max_turns not present after mergeExtensions")
		}
		if got != float64(50) {
			t.Errorf("max_turns = %v, want 50 (adapter value, not orchestrator value)", got)
		}
	})
}

// --- mergeTrackerCredentials tests ---

func TestMergeTrackerCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dst  map[string]any
		tc   config.TrackerConfig
		want map[string]any
	}{
		{
			name: "sets all three keys when dst is empty",
			dst:  map[string]any{},
			tc:   config.TrackerConfig{APIKey: "tok", Project: "PROJ", Endpoint: "https://api.example.com"},
			want: map[string]any{
				"api_key":  "tok",
				"project":  "PROJ",
				"endpoint": "https://api.example.com",
			},
		},
		{
			name: "skips keys already present in dst",
			dst: map[string]any{
				"api_key":  "existing",
				"project":  "existing",
				"endpoint": "existing",
			},
			tc: config.TrackerConfig{APIKey: "new", Project: "new", Endpoint: "new"},
			want: map[string]any{
				"api_key":  "existing",
				"project":  "existing",
				"endpoint": "existing",
			},
		},
		{
			name: "skips empty tracker fields",
			dst:  map[string]any{},
			tc:   config.TrackerConfig{APIKey: "", Project: "", Endpoint: ""},
			want: map[string]any{},
		},
		{
			name: "sets only api_key and project when endpoint empty",
			dst:  map[string]any{},
			tc:   config.TrackerConfig{APIKey: "tok", Project: "PROJ", Endpoint: ""},
			want: map[string]any{
				"api_key": "tok",
				"project": "PROJ",
			},
		},
		{
			name: "partial dst: sets only absent keys",
			dst:  map[string]any{"api_key": "kept"},
			tc:   config.TrackerConfig{APIKey: "ignored", Project: "PROJ", Endpoint: "https://api.example.com"},
			want: map[string]any{
				"api_key":  "kept",
				"project":  "PROJ",
				"endpoint": "https://api.example.com",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mergeTrackerCredentials(tt.dst, tt.tc)

			for k, wantVal := range tt.want {
				if got, ok := tt.dst[k]; !ok {
					t.Errorf("dst[%q] missing, want %q", k, wantVal)
				} else if got != wantVal {
					t.Errorf("dst[%q] = %q, want %q", k, got, wantVal)
				}
			}
			for k := range tt.dst {
				if _, ok := tt.want[k]; !ok {
					t.Errorf("dst has unexpected key %q", k)
				}
			}
		})
	}
}

func TestMergeTrackerCredentialsExtensionsWin(t *testing.T) {
	t.Parallel()

	// Extensions key wins over tracker key.
	dst := map[string]any{}
	mergeExtensions(dst, map[string]any{"github": map[string]any{"api_key": "ext-tok"}}, "github")
	mergeTrackerCredentials(dst, config.TrackerConfig{APIKey: "tracker-tok", Project: "PROJ"})

	if got := dst["api_key"]; got != "ext-tok" {
		t.Errorf("api_key = %q, want %q (extensions value must win)", got, "ext-tok")
	}
	if got := dst["project"]; got != "PROJ" {
		t.Errorf("project = %q, want %q (tracker value fills absent key)", got, "PROJ")
	}
}

// --- Kind-match guard wiring tests ---

func TestKindMatchGuardWiring(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		trackerKind string
		ciKind      string
		tc          config.TrackerConfig
		extensions  map[string]any
		wantKeys    map[string]any
		absentKeys  []string
	}{
		{
			name:        "kinds match with no extensions",
			trackerKind: "github",
			ciKind:      "github",
			tc:          config.TrackerConfig{APIKey: "gh-tok", Project: "sortie-ai/sortie"},
			extensions:  map[string]any{},
			wantKeys: map[string]any{
				"api_key": "gh-tok",
				"project": "sortie-ai/sortie",
			},
		},
		{
			name:        "kinds match with extensions override",
			trackerKind: "github",
			ciKind:      "github",
			tc:          config.TrackerConfig{APIKey: "tok1", Project: "sortie-ai/sortie"},
			extensions:  map[string]any{"github": map[string]any{"api_key": "tok2"}},
			wantKeys: map[string]any{
				"api_key": "tok2",
			},
		},
		{
			name:        "kinds differ",
			trackerKind: "jira",
			ciKind:      "github",
			tc:          config.TrackerConfig{APIKey: "jira-tok"},
			extensions:  map[string]any{},
			absentKeys:  []string{"api_key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapterCfgMap := make(map[string]any)
			mergeExtensions(adapterCfgMap, tt.extensions, tt.ciKind)
			if tt.ciKind == tt.trackerKind {
				mergeTrackerCredentials(adapterCfgMap, tt.tc)
			}

			for k, wantVal := range tt.wantKeys {
				if got, ok := adapterCfgMap[k]; !ok {
					t.Errorf("adapterCfgMap[%q] missing, want %q", k, wantVal)
				} else if got != wantVal {
					t.Errorf("adapterCfgMap[%q] = %q, want %q", k, got, wantVal)
				}
			}
			for _, k := range tt.absentKeys {
				if _, ok := adapterCfgMap[k]; ok {
					t.Errorf("adapterCfgMap[%q] present, want absent", k)
				}
			}
		})
	}
}

// --- Quick-start documentation integration test ---

// quickStartWorkflow returns WORKFLOW.md content matching the
// https://docs.sortie-ai.com/getting-started/quick-start/ tutorial,
// with workspace.root overridden to use the provided temp directory
// for test isolation.
func quickStartWorkflow(issuesPath, workspaceRoot string) []byte {
	return []byte(fmt.Sprintf(`---
tracker:
  kind: file
  project: DEMO
  active_states:
    - "To Do"
  handoff_state: "Done"

file:
  path: %s

agent:
  kind: mock
  max_turns: 2

polling:
  interval_ms: 500

workspace:
  root: %s
---

Fix the following issue.

**{{ .issue.identifier }}**: {{ .issue.title }}

{{ .issue.description }}
`, issuesPath, workspaceRoot))
}

// quickStartIssues returns issues.json content matching the
// https://docs.sortie-ai.com/getting-started/quick-start/ tutorial.
func quickStartIssues() []byte {
	return []byte(`[
  {
    "id": "1",
    "identifier": "DEMO-1",
    "title": "Add input validation to signup form",
    "description": "The signup form accepts empty email addresses. Add validation before submission.",
    "state": "To Do",
    "priority": 1
  },
  {
    "id": "2",
    "identifier": "DEMO-2",
    "title": "Fix off-by-one error in pagination",
    "description": "Page 2 repeats the last item from page 1. The offset calculation is wrong.",
    "state": "To Do",
    "priority": 2
  }
]`)
}

// TestQuickStartScenario is an integration test that exercises the exact
// workflow described in https://docs.sortie-ai.com/getting-started/quick-start/ end-to-end:
// two issues dispatched with mock agent, two turns each, handoff to "Done".
func TestQuickStartScenario(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wsRoot := filepath.Join(dir, "workspaces")

	issuesPath := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(issuesPath, quickStartIssues(), 0o644); err != nil {
		t.Fatal(err)
	}

	wfPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(wfPath, quickStartWorkflow(issuesPath, wsRoot), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr lockedBuf
	// 5 seconds: mock-agent turns complete in <1 s; polling interval is
	// 500 ms so the second tick confirming zero candidates arrives quickly.
	// Extra headroom avoids flakiness under -race in CI.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}

	logs := stderr.String()

	// Verify key lifecycle events from the quick-start scenario.
	checks := []struct {
		name   string
		substr string
	}{
		{"sortie started", `msg="sortie started"`},
		{"tick completed with 2 candidates", `msg="tick completed" candidates=2`},
		{"DEMO-1 session started", `msg="agent session started" issue_id=1 issue_identifier=DEMO-1`},
		{"DEMO-2 session started", `msg="agent session started" issue_id=2 issue_identifier=DEMO-2`},
		{"DEMO-1 turn 1 completed", `issue_identifier=DEMO-1 session_id=mock-session-001 turn_number=1`},
		{"DEMO-1 turn 2 completed", `issue_identifier=DEMO-1 session_id=mock-session-001 turn_number=2`},
		{"DEMO-2 turn 1 completed", `issue_identifier=DEMO-2 session_id=mock-session-001 turn_number=1`},
		{"DEMO-2 turn 2 completed", `issue_identifier=DEMO-2 session_id=mock-session-001 turn_number=2`},
		{"DEMO-1 worker exiting normally", `issue_identifier=DEMO-1 session_id=mock-session-001 exit_kind=normal`},
		{"DEMO-2 worker exiting normally", `issue_identifier=DEMO-2 session_id=mock-session-001 exit_kind=normal`},
		{"DEMO-1 handoff succeeded", `issue_identifier=DEMO-1 session_id=mock-session-001 handoff_state=Done`},
		{"DEMO-2 handoff succeeded", `issue_identifier=DEMO-2 session_id=mock-session-001 handoff_state=Done`},
		{"second tick finds zero candidates", `msg="tick completed" candidates=0`},
	}
	for _, c := range checks {
		if !strings.Contains(logs, c.substr) {
			t.Errorf("%s: expected log substring %q not found in output:\n%s", c.name, c.substr, logs)
		}
	}

	// .sortie.db must be created next to WORKFLOW.md.
	if _, err := os.Stat(filepath.Join(dir, ".sortie.db")); err != nil {
		t.Errorf("expected .sortie.db next to WORKFLOW.md: %v", err)
	}
}

// lockedBuf is a concurrency-safe bytes.Buffer for log capture in tests
// where background goroutines also write log output via slog.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// errWriter is a test double that always returns a fixed error from Write.
type errWriter struct{ err error }

func (lb *lockedBuf) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func (lb *lockedBuf) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}

func (e errWriter) Write(_ []byte) (int, error) { return 0, e.err }

func TestResolveServerPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		portFlag    int
		portFlagSet bool
		extensions  map[string]any
		wantPort    int
		wantEnabled bool
		wantErr     bool
	}{
		{
			name:        "flag set overrides everything",
			portFlag:    9090,
			portFlagSet: true,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantPort:    9090,
			wantEnabled: true,
		},
		{
			name:        "flag set with zero port",
			portFlag:    0,
			portFlagSet: true,
			extensions:  nil,
			wantPort:    0,
			wantEnabled: true,
		},
		{
			name:        "extensions int port",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantPort:    8080,
			wantEnabled: true,
		},
		{
			name:        "extensions float64 port (JSON decode)",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(8080)}},
			wantPort:    8080,
			wantEnabled: true,
		},
		{
			name:        "no server in extensions",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "nil extensions",
			portFlag:    0,
			portFlagSet: false,
			extensions:  nil,
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "server extension without port key",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"other": "value"}},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "server extension port is string (unsupported type)",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": "8080"}},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "server extension is not a map",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": "not-a-map"},
			wantPort:    0,
			wantEnabled: false,
		},

		// --- Boundary and invalid port regression tests ---

		{
			name:        "flag negative port rejected",
			portFlag:    -1,
			portFlagSet: true,
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "flag port above 65535 rejected",
			portFlag:    70000,
			portFlagSet: true,
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "flag port exactly 65535 accepted",
			portFlag:    65535,
			portFlagSet: true,
			wantPort:    65535,
			wantEnabled: true,
		},
		{
			name:        "flag port 1 accepted",
			portFlag:    1,
			portFlagSet: true,
			wantPort:    1,
			wantEnabled: true,
		},
		{
			name:        "extensions fractional float64 rejected",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(8080.9)}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions float64 above 65535 rejected",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(99999)}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions negative int rejected",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": -1}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions int above 65535 rejected",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 70000}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions int exactly 65535 accepted",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 65535}},
			wantPort:    65535,
			wantEnabled: true,
		},
		{
			name:        "extensions float64 exactly 65535 accepted",
			portFlag:    0,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(65535)}},
			wantPort:    65535,
			wantEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPort, gotEnabled, gotErr := resolveServerPort(tt.portFlag, tt.portFlagSet, tt.extensions)
			if gotPort != tt.wantPort {
				t.Errorf("resolveServerPort() port = %d, want %d", gotPort, tt.wantPort)
			}
			if gotEnabled != tt.wantEnabled {
				t.Errorf("resolveServerPort() enabled = %v, want %v", gotEnabled, tt.wantEnabled)
			}
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("resolveServerPort() err = %v, wantErr %v", gotErr, tt.wantErr)
			}
		})
	}
}

// --- Validate subcommand tests (Plan Phase 5) ---

// writeCustomWorkflowFile writes the given YAML front matter and prompt
// body as a WORKFLOW.md in dir and returns the absolute path to the
// created file. It calls filepath.Abs so the returned path is
// absolute regardless of whether dir is relative or absolute.
func writeCustomWorkflowFile(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	absPath, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("filepath.Abs(%q): %v", p, err)
	}
	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return absPath
}

// noTrackerKindWorkflow is a minimal workflow with active/terminal
// states set (to pass ValidateConfigForPromotion) but tracker.kind
// absent (to trigger the preflight check).
func noTrackerKindWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// unknownTrackerKindWorkflow is a minimal workflow with an unregistered
// tracker kind, used to trigger the tracker_adapter preflight check.
func unknownTrackerKindWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: nonexistent
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// jiraEmptyAPIKeyWorkflow returns a workflow using the jira tracker with
// an api_key referencing SORTIE_TEST_NONEXISTENT_VAR_198, which must be
// unset (or empty) when the test runs. The jira adapter requires an API
// key, so os.ExpandEnv resolving to "" triggers tracker.api_key preflight.
func jiraEmptyAPIKeyWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: jira
  api_key: "$SORTIE_TEST_NONEXISTENT_VAR_198"
  project: TEST
  active_states:
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

func TestValidateValidWorkflow(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text format produces no output on success)", stdout.String())
	}
}

func TestValidateValidWorkflowJSON(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if out.Errors == nil {
		t.Errorf("validateOutput.Errors = nil, want [] (must not be null in JSON)")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty slice", out.Errors)
	}

	// Verify the raw JSON contains "errors":[] not "errors":null.
	raw := stdout.String()
	if !strings.Contains(raw, `"errors":[]`) {
		t.Errorf("JSON output = %q, want to contain %q", raw, `"errors":[]`)
	}

	// Warnings must be a non-null empty array in JSON output.
	if out.Warnings == nil {
		t.Errorf("validateOutput.Warnings = nil, want [] (must not be null in JSON)")
	}
	if len(out.Warnings) != 0 {
		t.Errorf("validateOutput.Warnings = %v, want empty slice", out.Warnings)
	}
	if !strings.Contains(raw, `"warnings":[]`) {
		t.Errorf("JSON output = %q, want to contain %q", raw, `"warnings":[]`)
	}
}

func TestValidateDefaultPath(t *testing.T) {
	// setupRunDir sets cwd to a temp dir that contains WORKFLOW.md.
	setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// No explicit path — resolveWorkflowPath defaults to ./WORKFLOW.md.
	code := run(ctx, []string{"validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestValidateMissingFile(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "/nonexistent/sortie-test-workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate /nonexistent) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "workflow") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "workflow")
	}
}

func TestValidateMissingFileJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", "/nonexistent/sortie-test-workflow.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json /nonexistent) = %d, want 1", code)
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one diagnostic")
	}
	if len(out.Errors) > 0 && !strings.Contains(out.Errors[0].Check, "workflow") {
		t.Errorf("validateOutput.Errors[0].Check = %q, want to contain %q", out.Errors[0].Check, "workflow")
	}
}

func TestValidateMissingTrackerKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tracker.kind") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.kind")
	}
}

func TestValidateMissingTrackerKindJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1", code)
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}

	found := false
	for _, d := range out.Errors {
		if d.Check == "tracker.kind" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "tracker.kind")
	}
}

func TestValidateUnregisteredAdapter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "tracker_adapter") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker_adapter")
	}
}

func TestValidateInvalidFormat(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "xml"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format xml) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid --format") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "invalid --format")
	}
}

func TestValidateExplicitTextFormat(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "text", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format text) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestValidateHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// --help must exit 0 — it is not a failure.
	code := run(ctx, []string{"validate", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --help) = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "format") {
		t.Errorf("stderr = %q, want usage text containing %q", stderr.String(), "format")
	}
}

func TestValidateUnknownFlagText(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// An unknown flag in text mode must be routed through emitDiags, not
	// printed directly by the flag package. stderr must contain the
	// "args: " prefix that emitDiags emits, and stdout must be empty.
	code := run(ctx, []string{"validate", "--unknown-flag-xyz"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --unknown-flag-xyz) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "args: ") {
		t.Errorf("stderr = %q, want to contain %q (emitDiags prefix)", stderr.String(), "args: ")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty for text-mode error", stdout.String())
	}
}

func TestValidateUnknownFlagJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// --format is parsed before --unknown-flag-xyz, so *format is "json"
	// when the parse error is returned. emitDiags must write structured
	// JSON to stdout; stderr must remain empty.
	code := run(ctx, []string{"validate", "--format", "json", "--unknown-flag-xyz"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json --unknown-flag-xyz) = %d, want 1", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty for JSON-mode error", stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one diagnostic")
	}
	if len(out.Errors) > 0 && out.Errors[0].Check != "args" {
		t.Errorf("validateOutput.Errors[0].Check = %q, want %q", out.Errors[0].Check, "args")
	}
}

func TestValidateTooManyArgs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "a.md", "b.md"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate a.md b.md) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "too many arguments") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "too many arguments")
	}
}

func TestValidateUnresolvedEnvVar(t *testing.T) {
	// t.Parallel omitted: t.Setenv requires a sequential test.

	// Ensure the test env var expands to empty string. Using t.Setenv
	// with "" has the same expansion result as the var being unset — both
	// cause os.ExpandEnv to produce "". t.Setenv restores the original
	// value after the test.
	t.Setenv("SORTIE_TEST_NONEXISTENT_VAR_198", "")

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, jiraEmptyAPIKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
	}
	// os.ExpandEnv produces "" for the unset var, then preflight check 3
	// catches the empty api_key for the jira adapter.
	if !strings.Contains(stderr.String(), "tracker.api_key") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.api_key")
	}
}

func TestValidateDoesNotCreateDB(t *testing.T) {
	wfPath := setupRunDir(t)
	wfDir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}

	// The validate subcommand must not open the database.
	dbPath := filepath.Join(wfDir, ".sortie.db")
	if _, err := os.Stat(dbPath); err == nil {
		t.Errorf("database file %s must not be created by validate subcommand", dbPath)
	}
}

func TestValidateDoesNotStartWatcher(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	// The validate subcommand must return promptly — no filesystem
	// watcher goroutine is started (mgr.Start is never called).
	start := time.Now()
	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	// 30 s is generous enough to remain stable on slow CI runners while
	// still catching the case where a watcher goroutine blocks the return.
	const maxDuration = 30 * time.Second
	if elapsed > maxDuration {
		t.Errorf("run(validate) took %v, want < %v (possible watcher goroutine started)", elapsed, maxDuration)
	}
}

// --- Front matter warning integration tests ---

// typoTopLevelKeyWorkflow returns a workflow with the "trackers" typo at the
// top level (unknown_key warning) and a valid tracker.kind so preflight passes.
func typoTopLevelKeyWorkflow() []byte {
	return []byte(`---
trackers:
  kind: file
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// typoSubKeyWorkflow returns a workflow with an unknown sub-key inside the
// tracker section (unknown_sub_key warning). Preflight passes.
func typoSubKeyWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
  typo_endpoint: "should not be here"
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// typeMismatchWorkflow returns a workflow where hooks.timeout_ms is a
// non-numeric string (type_mismatch warning). Preflight passes.
func typeMismatchWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
hooks:
  timeout_ms: not-a-number
---
Do {{ .issue.title }}.
`)
}

// nonPositiveHooksTimeoutWorkflow returns a workflow where hooks.timeout_ms
// is -1 (semantic type_mismatch warning: non-positive). Preflight passes.
func nonPositiveHooksTimeoutWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
hooks:
  timeout_ms: -1
---
Do {{ .issue.title }}.
`)
}

// errorAndWarningWorkflow returns a workflow with the "trackers" typo
// (warning) and no tracker.kind (error). ValidateConfigForPromotion
// passes because active_states is set; preflight fails on tracker.kind.
func errorAndWarningWorkflow() []byte {
	return []byte(`---
trackers:
  kind: file
tracker:
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// TestValidateWarningTypoTopLevelKeyText asserts that a typo top-level key
// produces exit 0 (valid), an empty stdout (text mode), and the warning
// written to stderr with the "warning:" prefix.
func TestValidateWarningTypoTopLevelKeyText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoTopLevelKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text mode, no errors)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "trackers") {
		t.Errorf("stderr = %q, want to contain %q (typo key name)", stderr.String(), "trackers")
	}
}

// TestValidateWarningTypoTopLevelKeyJSON asserts that a typo top-level key
// in JSON mode produces exit 0, valid=true, empty errors slice, and a single
// warning diagnostic with the expected fields.
func TestValidateWarningTypoTopLevelKeyJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoTopLevelKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (JSON mode, no fallback)", stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("validateOutput.Warnings = %v (len %d), want 1", out.Warnings, len(out.Warnings))
	}
	if out.Warnings[0].Severity != "warning" {
		t.Errorf("warnings[0].Severity = %q, want %q", out.Warnings[0].Severity, "warning")
	}
	if out.Warnings[0].Check != "unknown_key" {
		t.Errorf("warnings[0].Check = %q, want %q", out.Warnings[0].Check, "unknown_key")
	}
	if !strings.Contains(out.Warnings[0].Message, "trackers") {
		t.Errorf("warnings[0].Message = %q, want to contain %q", out.Warnings[0].Message, "trackers")
	}
}

// TestValidateWarningTypoSubKeyText asserts that an unknown sub-key inside
// a known section produces exit 0 and a warning on stderr.
func TestValidateWarningTypoSubKeyText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typoSubKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "unknown_sub_key") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown_sub_key")
	}
}

// TestValidateWarningTypeMismatchText asserts that a type-mismatched field
// produces exit 0 and a warning on stderr.
func TestValidateWarningTypeMismatchText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, typeMismatchWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning:") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "warning:")
	}
	if !strings.Contains(stderr.String(), "type_mismatch") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "type_mismatch")
	}
}

// TestValidateWarningNonPositiveHooksTimeout asserts that a non-positive
// hooks.timeout_ms produces exit 0 and a semantic warning on stderr.
func TestValidateWarningNonPositiveHooksTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, nonPositiveHooksTimeoutWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-positive") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "non-positive")
	}
}

// TestValidateErrorAndWarningsTogether asserts that a workflow with both a
// warning (typo top-level key) and an error (missing tracker.kind) produces
// exit 1 with both diagnostic categories in the JSON output.
func TestValidateErrorAndWarningsTogether(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, errorAndWarningWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}
	if len(out.Errors) == 0 {
		t.Errorf("validateOutput.Errors is empty, want at least one error diagnostic")
	}
	if len(out.Warnings) == 0 {
		t.Errorf("validateOutput.Warnings is empty, want at least one warning diagnostic")
	}
	// The error must be a preflight "tracker.kind" check.
	foundTrackerKind := false
	for _, d := range out.Errors {
		if d.Check == "tracker.kind" {
			foundTrackerKind = true
			if d.Severity != "error" {
				t.Errorf("errors[tracker.kind].Severity = %q, want %q", d.Severity, "error")
			}
		}
	}
	if !foundTrackerKind {
		t.Errorf("validateOutput.Errors = %v, want a diagnostic with check %q", out.Errors, "tracker.kind")
	}
	// The warning must be an "unknown_key" for "trackers".
	foundTrackers := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_key" && strings.Contains(w.Message, "trackers") {
			foundTrackers = true
			if w.Severity != "warning" {
				t.Errorf("warnings[unknown_key].Severity = %q, want %q", w.Severity, "warning")
			}
		}
	}
	if !foundTrackers {
		t.Errorf("validateOutput.Warnings = %v, want a warning with check %q containing %q", out.Warnings, "unknown_key", "trackers")
	}
}

// --- Template static analysis warning tests ---

// dotContextWorkflow returns a workflow whose prompt triggers WarnDotContext:
// .issue.title referenced inside {{ range }} where dot is the element.
func dotContextWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ range .issue.labels }}{{ .issue.title }}{{ end }}
`)
}

// unknownVarWorkflow returns a workflow whose prompt triggers WarnUnknownVar:
// .config is not in the template data contract.
func unknownVarWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ .config }}
`)
}

// unknownFieldWorkflow returns a workflow whose prompt triggers WarnUnknownField:
// .run.nonexistent is not a valid sub-field of run.
func unknownFieldWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ .run.nonexistent }}
`)
}

// multipleTemplateWarningWorkflow returns a workflow whose prompt triggers
// both WarnDotContext (.issue.title inside range) and WarnUnknownVar ($.config).
func multipleTemplateWarningWorkflow() []byte {
	return []byte(`---
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
---
{{ range .issue.labels }}{{ .issue.title }}{{ $.config }}{{ end }}
`)
}

// TestValidateTemplateDotContextText verifies that a dot-context misuse
// produces exit 0, empty stdout, and a "dot_context" warning on stderr.
func TestValidateTemplateDotContextText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, dotContextWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty (text mode, warnings only)", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "dot_context") {
		t.Errorf("stderr = %q, want to contain %q", got, "dot_context")
	}
}

// TestValidateTemplateDotContextJSON verifies that a dot-context misuse
// produces valid=true, empty errors, and a warning with check="dot_context"
// in JSON output.
func TestValidateTemplateDotContextJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, dotContextWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "dot_context" {
			found = true
			if w.Severity != "warning" {
				t.Errorf("warnings[dot_context].Severity = %q, want %q", w.Severity, "warning")
			}
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "dot_context")
	}
}

// TestValidateTemplateUnknownVarText verifies that an unknown top-level
// variable produces exit 0 and an "unknown_var" warning on stderr.
func TestValidateTemplateUnknownVarText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownVarWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "unknown_var") {
		t.Errorf("stderr = %q, want to contain %q", got, "unknown_var")
	}
}

// TestValidateTemplateUnknownVarJSON verifies that an unknown top-level
// variable produces valid=true and a warning with check="unknown_var" in JSON.
func TestValidateTemplateUnknownVarJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownVarWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_var" {
			found = true
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "unknown_var")
	}
}

// TestValidateTemplateUnknownFieldText verifies that an unknown sub-field
// produces exit 0 and an "unknown_field" warning on stderr.
func TestValidateTemplateUnknownFieldText(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownFieldWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "warning:") {
		t.Errorf("stderr = %q, want to contain %q", got, "warning:")
	}
	if !strings.Contains(got, "unknown_field") {
		t.Errorf("stderr = %q, want to contain %q", got, "unknown_field")
	}
}

// TestValidateTemplateUnknownFieldJSON verifies that an unknown sub-field
// produces valid=true and a warning with check="unknown_field" in JSON.
func TestValidateTemplateUnknownFieldJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, unknownFieldWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	found := false
	for _, w := range out.Warnings {
		if w.Check == "unknown_field" {
			found = true
		}
	}
	if !found {
		t.Errorf("validateOutput.Warnings = %v, want at least one entry with check=%q", out.Warnings, "unknown_field")
	}
}

// TestValidateTemplateMultipleWarnings verifies that a prompt triggering
// multiple warning classes reports all of them without changing the exit code.
func TestValidateTemplateMultipleWarnings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, multipleTemplateWarningWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if !out.Valid {
		t.Errorf("validateOutput.Valid = false, want true")
	}
	if len(out.Errors) != 0 {
		t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
	}
	hasDotContext := false
	hasUnknownVar := false
	for _, w := range out.Warnings {
		if w.Check == "dot_context" {
			hasDotContext = true
		}
		if w.Check == "unknown_var" {
			hasUnknownVar = true
		}
	}
	if !hasDotContext {
		t.Errorf("validateOutput.Warnings = %v, want at least one %q warning", out.Warnings, "dot_context")
	}
	if !hasUnknownVar {
		t.Errorf("validateOutput.Warnings = %v, want at least one %q warning", out.Warnings, "unknown_var")
	}
}

// TestValidateTemplateCleanNoWarnings verifies that a well-formed workflow
// produces no template warnings.
func TestValidateTemplateCleanNoWarnings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, minimalWorkflow())

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"validate", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	got := stderr.String()
	if strings.Contains(got, "dot_context") || strings.Contains(got, "unknown_var") || strings.Contains(got, "unknown_field") {
		t.Errorf("stderr = %q, want no template warnings for a clean workflow", got)
	}
}

// --- writeJSON / emitDiags error-path tests ---

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	t.Run("success returns nil", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		if err := writeJSON(&buf, validateOutput{Valid: true, Errors: []validateDiag{}}); err != nil {
			t.Errorf("writeJSON() unexpected error: %v", err)
		}
		if buf.Len() == 0 {
			t.Error("writeJSON() wrote nothing to the buffer")
		}
	})

	t.Run("writer failure is returned as error", func(t *testing.T) {
		t.Parallel()

		w := errWriter{err: fmt.Errorf("disk full")}
		if err := writeJSON(w, validateOutput{Valid: false, Errors: []validateDiag{}}); err == nil {
			t.Error("writeJSON() expected error from failing writer, got nil")
		}
	})
}

func TestEmitDiagsJSONFallback(t *testing.T) {
	t.Parallel()

	// When stdout fails to accept JSON, emitDiags must fall back to
	// plain-text diagnostics on stderr so the caller still sees the error.
	diags := []validateDiag{
		{Severity: "error", Check: "tracker.kind", Message: "tracker kind is required"},
	}
	var stderr bytes.Buffer
	emitDiags(errWriter{err: fmt.Errorf("disk full")}, &stderr, "json", diags, nil)

	got := stderr.String()
	if !strings.Contains(got, "tracker.kind") {
		t.Errorf("stderr = %q, want to contain %q (fallback text)", got, "tracker.kind")
	}
	if !strings.Contains(got, "tracker kind is required") {
		t.Errorf("stderr = %q, want to contain %q (fallback text)", got, "tracker kind is required")
	}
}

func TestRunValidateJSONSuccessStdoutFails(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	//
	// When the success-path JSON write fails and there are no errors or
	// warnings to fall back on, emitDiags has nothing to print to stderr.
	// runValidate still returns 0 (the workflow is valid; the I/O failure
	// is best-effort output delivery).
	wfPath := setupRunDir(t)

	var stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath},
		errWriter{err: fmt.Errorf("disk full")}, &stderr)
	if code != 0 {
		t.Fatalf("run(validate --format json) with failing stdout = %d, want 0; stderr: %s",
			code, stderr.String())
	}
	// No per-diag fallback lines when there are no errors or warnings.
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (no diags to fall back on)", stderr.String())
	}
}

// --- OS signal and server shutdown edge-case tests ---

// TestRunSIGINTCleanShutdown verifies that run() returns 0 when the process
// receives SIGINT via signal.NotifyContext. Uses the helper-subprocess
// pattern to avoid delivering OS signals to the test runner's own process.
//
// Subprocess mode is activated by SORTIE_TEST_SIGINT_HELPER=1.
func TestRunSIGINTCleanShutdown(t *testing.T) {
	if os.Getenv("SORTIE_TEST_SIGINT_HELPER") == "1" {
		// --- subprocess ---
		// This code runs as a subprocess when the parent test injects the
		// env var. signal.NotifyContext handles SIGINT by cancelling ctx,
		// which causes run() to shut down cleanly.
		dir := os.Getenv("SORTIE_TEST_SIGINT_DIR")
		wfPath := filepath.Join(dir, "WORKFLOW.md")
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		os.Exit(run(ctx, []string{wfPath}, os.Stdout, os.Stderr))
		return // unreachable — silences staticcheck
	}

	// --- parent test ---
	dir := t.TempDir()
	writeIssuesFixture(t, dir)
	writeWorkflowFile(t, dir)

	cmd := exec.Command(os.Args[0], "-test.run=TestRunSIGINTCleanShutdown", "-test.v")
	cmd.Env = append(os.Environ(),
		"SORTIE_TEST_SIGINT_HELPER=1",
		"SORTIE_TEST_SIGINT_DIR="+dir,
	)
	var subStderr lockedBuf
	cmd.Stdout = io.Discard
	cmd.Stderr = &subStderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start: %v", err)
	}

	// Poll subprocess stderr until "sortie started" appears — confirming
	// the orchestrator event loop is running before we send SIGINT.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(subStderr.String(), "sortie started") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(subStderr.String(), "sortie started") {
		cmd.Process.Kill() //nolint:errcheck // best-effort cleanup
		t.Fatalf("subprocess did not reach 'sortie started' within 5 s; stderr:\n%s", subStderr.String())
	}

	// Send SIGINT — should trigger context cancellation and clean shutdown.
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("Signal(SIGINT): %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				t.Errorf("subprocess exited with code %d after SIGINT, want 0; stderr:\n%s",
					exitErr.ExitCode(), subStderr.String())
			} else {
				t.Errorf("subprocess Wait: %v; stderr:\n%s", waitErr, subStderr.String())
			}
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill() //nolint:errcheck // best-effort cleanup
		t.Errorf("subprocess did not exit within 5 s after SIGINT; stderr:\n%s", subStderr.String())
	}
}

// TestRunServerShutdownError covers the logger.Error("http server shutdown
// error", ...) branch that fires when srv.Shutdown returns an error because
// active connections are still open when the shutdown context expires.
//
// The test uses an incomplete HTTP request to hold a connection in the
// "active" state, preventing immediate shutdown, and a short
// serverShutdownTimeout override to make the context expire quickly.
func TestRunServerShutdownError(t *testing.T) {
	// No t.Parallel: mutates package-level serverShutdownTimeout.
	orig := serverShutdownTimeout
	serverShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serverShutdownTimeout = orig })

	wfPath := setupRunDir(t)

	var stderr lockedBuf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan int, 1)
	go func() {
		result <- run(ctx, []string{"--port", "0", wfPath}, io.Discard, &stderr)
	}()

	// Wait until the HTTP server reports its bound address.
	var addr string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if log := stderr.String(); strings.Contains(log, "http server listening") {
			if i := strings.Index(log, "addr="); i >= 0 {
				rest := log[i+5:]
				if end := strings.IndexAny(rest, " \t\n\r"); end >= 0 {
					addr = rest[:end]
				} else {
					addr = strings.TrimSpace(rest)
				}
				// slog.TextHandler may quote string values (addr="host:port").
				addr = strings.Trim(addr, "\"")
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		cancel()
		t.Fatal("HTTP server did not start or log its address within 3 s")
	}

	// Open a TCP connection and send an incomplete HTTP request (no
	// trailing \r\n\r\n). The server goroutine is waiting to finish
	// reading the request headers, keeping the connection "active" from
	// http.Server.Shutdown's perspective.
	conn, dialErr := net.DialTimeout("tcp", addr, time.Second)
	if dialErr != nil {
		cancel()
		t.Fatalf("dial %s: %v", addr, dialErr)
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	//nolint:errcheck // test write — errors are unrecoverable here
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n"))

	// Give the server goroutine time to register the connection as active.
	time.Sleep(20 * time.Millisecond)

	// Cancel the run context to trigger the shutdown sequence.
	cancel()

	select {
	case code := <-result:
		// shutdown errors are logged but do not change the exit code.
		if code != 0 {
			t.Errorf("run() = %d, want 0 (shutdown error is non-fatal); stderr:\n%s",
				code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run() did not return within 3 s after context cancel; stderr:\n%s", stderr.String())
	}

	if !strings.Contains(stderr.String(), "http server shutdown error") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "http server shutdown error")
	}
}

// TestRunReadOnlyWorkflowDir covers the persistence.Open error path that
// fires when the database file cannot be created because the workflow
// directory has no write permission.
func TestRunReadOnlyWorkflowDir(t *testing.T) {
	// No t.Parallel: calls t.Chdir via setupRunDir, and mutates permissions.
	if os.Getuid() == 0 {
		t.Skip("skipping: root bypasses filesystem permission checks")
	}

	workflowDir := t.TempDir()
	writeIssuesFixture(t, workflowDir)
	writeWorkflowFile(t, workflowDir)
	t.Chdir(workflowDir)

	// Make the directory read-only: traversable and readable, but no writes.
	// This prevents creating .sortie.db while still allowing the workflow
	// file and issues fixture to be read by the startup sequence.
	if err := os.Chmod(workflowDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(workflowDir, 0o755) //nolint:errcheck // cleanup
	})

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wfPath := filepath.Join(workflowDir, "WORKFLOW.md")
	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (DB must not be created in read-only dir); stderr: %s",
			code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to open database") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "failed to open database")
	}
}

// --- resolveLogLevel tests ---

func TestResolveLogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		flagValue  string
		flagSet    bool
		extensions map[string]any
		wantLevel  slog.Level
		wantErr    bool
	}{
		{
			name:      "flag set: debug",
			flagValue: "debug",
			flagSet:   true,
			wantLevel: slog.LevelDebug,
		},
		{
			name:       "flag set: error",
			flagValue:  "error",
			flagSet:    true,
			extensions: nil,
			wantLevel:  slog.LevelError,
		},
		{
			name:      "flag set: invalid",
			flagValue: "bogus",
			flagSet:   true,
			wantErr:   true,
		},
		{
			name:       "flag set overrides config",
			flagValue:  "debug",
			flagSet:    true,
			extensions: map[string]any{"logging": map[string]any{"level": "error"}},
			wantLevel:  slog.LevelDebug,
		},
		{
			name:       "extension: warn",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": "warn"}},
			wantLevel:  slog.LevelWarn,
		},
		{
			name:       "extension: case insensitive",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": "DEBUG"}},
			wantLevel:  slog.LevelDebug,
		},
		{
			name:       "extension: invalid string",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": "bogus"}},
			wantErr:    true,
		},
		{
			name:       "extension: non-string type (int)",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": 42}},
			wantErr:    true,
		},
		{
			name:       "no logging block",
			flagSet:    false,
			extensions: map[string]any{},
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "nil extensions",
			flagSet:    false,
			extensions: nil,
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "logging block without level key",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"format": "json"}},
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "logging block is not a map",
			flagSet:    false,
			extensions: map[string]any{"logging": "not-a-map"},
			wantLevel:  slog.LevelInfo,
		},
		{
			name:       "extension: null value (YAML null)",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": nil}},
			wantLevel:  slog.LevelInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveLogLevel(tt.flagValue, tt.flagSet, tt.extensions)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveLogLevel(%q, %v, ...) = %v, want error", tt.flagValue, tt.flagSet, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLogLevel(%q, %v, ...) unexpected error: %v", tt.flagValue, tt.flagSet, err)
			}
			if got != tt.wantLevel {
				t.Errorf("resolveLogLevel(%q, %v, ...) = %v, want %v", tt.flagValue, tt.flagSet, got, tt.wantLevel)
			}
		})
	}
}

// --- --log-level CLI flag integration tests ---

// minimalWorkflowWithLogLevel returns a WORKFLOW.md with the given level
// set in the logging.level extension key.
func minimalWorkflowWithLogLevel(level string) []byte {
	return []byte(fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
logging:
  level: %s
---
Do {{ .issue.title }}.
`, level))
}

// writeWorkflowFileWithContent writes the given content as WORKFLOW.md
// in dir and returns its absolute path.
func writeWorkflowFileWithContent(t *testing.T, dir string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunLogLevelDebug(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{"--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "level=DEBUG")
	}
	if !strings.Contains(stderr.String(), "log_level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q (startup attr)", stderr.String(), "log_level=DEBUG")
	}
}

func TestRunLogLevelWarn(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{"--log-level", "warn", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	// INFO-level startup line must be suppressed at warn level.
	if strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want no INFO lines at warn level", stderr.String())
	}
}

func TestRunLogLevelInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--log-level", "bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `unknown log level "bogus"`) {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), `unknown log level "bogus"`)
	}
}

func TestRunLogLevelEmpty(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--log-level", ""}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown log level") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log level")
	}
}

func TestRunLogLevelFromExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("warn"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	// INFO-level lines must be suppressed when extension sets warn.
	if strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want no INFO lines when logging.level=warn", stderr.String())
	}
}

func TestRunLogLevelFlagOverridesExtension(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	// Extension requests error level; flag requests debug — flag must win.
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("error"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := run(ctx, []string{"--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want to contain %q (flag wins over extension)", stderr.String(), "level=DEBUG")
	}
}

func TestRunLogLevelExtensionInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeIssuesFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithLogLevel("bogus"))

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown log level") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "unknown log level")
	}
}

func TestRunLogLevelDefault(t *testing.T) {
	wfPath := setupRunDir(t)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// No --log-level flag and no extension — default is info.
	code := run(ctx, []string{wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "level=INFO") {
		t.Errorf("stderr = %q, want to contain INFO-level startup line", stderr.String())
	}
	// DEBUG-level lines must be absent at the default info level.
	if strings.Contains(stderr.String(), "level=DEBUG") {
		t.Errorf("stderr = %q, want no DEBUG lines at default info level", stderr.String())
	}
}

func TestRunLogLevelVersionIgnoredInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Version fast path must exit 0 even when --log-level is invalid.
	code := run(ctx, []string{"--version", "--log-level", "invalid"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (version fast path ignores invalid log level)", code)
	}
	if !strings.Contains(stdout.String(), "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q", stdout.String(), "sortie "+Version)
	}
}

func TestRunLogLevelDumpVersionIgnoredInvalid(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// -dumpversion fast path must exit 0 even when --log-level is invalid.
	code := run(ctx, []string{"-dumpversion", "--log-level", "invalid"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (-dumpversion fast path ignores invalid log level)", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != Version {
		t.Errorf("-dumpversion stdout = %q, want %q", got, Version)
	}
}

// --- --dry-run flag tests ---

// assertNoDatabaseFile verifies no .sortie.db exists in workflowDir after a
// dry-run. Every dry-run test calls this to enforce the read-only invariant:
// --dry-run must never open or create the SQLite database.
func assertNoDatabaseFile(t *testing.T, workflowDir string) {
	t.Helper()
	dbPath := filepath.Join(workflowDir, ".sortie.db")
	_, err := os.Stat(dbPath)
	if err == nil {
		t.Errorf("database file %s must not exist after dry-run", dbPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed to stat database file %s: %v", dbPath, err)
	}
}

// minimalWorkflowWithSSH returns a minimal WORKFLOW.md with a worker SSH
// config: one host (host-a) capped at one concurrent agent per host.
func minimalWorkflowWithSSH() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
worker:
  ssh_hosts:
    - host-a
  max_concurrent_agents_per_host: 1
---
Do {{ .issue.title }}.
`)
}

// writeThreeIssueFixture writes a three-issue issues.json to dir for
// testing SSH host capacity limits.
func writeThreeIssueFixture(t *testing.T, dir string) {
	t.Helper()
	data := []byte(`[
{"id":"10001","identifier":"PROJ-1","title":"Issue 1","state":"To Do","labels":[],"comments":[],"blocked_by":[]},
{"id":"10002","identifier":"PROJ-2","title":"Issue 2","state":"To Do","labels":[],"comments":[],"blocked_by":[]},
{"id":"10003","identifier":"PROJ-3","title":"Issue 3","state":"To Do","labels":[],"comments":[],"blocked_by":[]}
]`)
	if err := os.WriteFile(filepath.Join(dir, "issues.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDryRunExitZero(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run: complete") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: complete")
	}
	if !strings.Contains(stderr.String(), "dry-run: candidate") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: candidate")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunNoCandidates(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "issues.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfPath := writeWorkflowFile(t, dir)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run, empty issues) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "candidates_fetched=0") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "candidates_fetched=0")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunNoDatabaseFile(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run) = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Primary safety invariant: --dry-run must never open or create the database.
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunInvalidWorkflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Unclosed inline sequence triggers a YAML parse error.
	invalid := []byte("---\n{key: [unclosed\n---\nDo {{ .issue.title }}.\n")
	wfPath := writeCustomWorkflowFile(t, dir, invalid)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, invalid YAML) = %d, want 1; stderr: %s", code, stderr.String())
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunPreflightFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, noTrackerKindWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, missing tracker.kind) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "preflight") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "preflight")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunWithLogLevel(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", "--log-level", "debug", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run --log-level debug) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dry-run: complete") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "dry-run: complete")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunPortIgnored(t *testing.T) {
	// No t.Parallel: setupRunDir calls t.Chdir.
	wfPath := setupRunDir(t)
	dir := filepath.Dir(wfPath)

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", "--port", "0", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run --port 0) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "http server listening") {
		t.Errorf("stderr = %q, must not contain %q (HTTP server must not start in dry-run mode)", stderr.String(), "http server listening")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunTrackerFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missingPath := filepath.Join(dir, "does_not_exist.json")

	content := fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: %s
---
Do {{ .issue.title }}.
`, missingPath)
	wfPath := writeCustomWorkflowFile(t, dir, []byte(content))

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(--dry-run, missing issues file) = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to fetch") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "failed to fetch")
	}
	assertNoDatabaseFile(t, dir)
}

func TestRunDryRunWithVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// --version fast path must win over --dry-run, exiting 0 with the banner.
	code := run(ctx, []string{"--version", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--version --dry-run) = %d, want 0; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "sortie "+Version) {
		t.Errorf("stdout = %q, want to contain %q (version banner)", stdout.String(), "sortie "+Version)
	}
}

func TestRunDryRunSSHHostCapacity(t *testing.T) {
	// No t.Parallel: uses t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)
	writeThreeIssueFixture(t, dir)
	wfPath := writeWorkflowFileWithContent(t, dir, minimalWorkflowWithSSH())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"--dry-run", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(--dry-run, SSH config + 3 issues) = %d, want 0; stderr: %s", code, stderr.String())
	}

	logs := stderr.String()

	// Section 3.5: with max_concurrent_agents_per_host=1, only the first
	// candidate acquires the SSH host; subsequent candidates are blocked.
	dispatched := strings.Count(logs, "would_dispatch=true")
	if dispatched > 1 {
		t.Errorf("would_dispatch=true count = %d, want at most 1 (SSH host capacity=1)", dispatched)
	}
	if !strings.Contains(logs, "ssh_hosts_at_capacity") {
		t.Errorf("stderr does not contain %q; full output:\n%s", "ssh_hosts_at_capacity", logs)
	}
	assertNoDatabaseFile(t, dir)
}

// --- GitHub validate tests ---

// githubInvalidProjectWorkflow is a minimal GitHub workflow where
// tracker.project is not in owner/repo format (no slash), used to
// trigger the tracker.project.format preflight diagnostic.
func githubInvalidProjectWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "tok"
  project: "notvalid"
  active_states:
    - backlog
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// githubStateOverlapWorkflow is a minimal GitHub workflow where
// active_states and terminal_states overlap on "done", used to
// trigger the tracker.states.overlap warning.
func githubStateOverlapWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "tok"
  project: "sortie-ai/sortie"
  active_states:
    - backlog
    - done
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// githubMissingAPIKeyWorkflow is a minimal GitHub workflow where
// tracker.api_key references an unset environment variable so it
// resolves to empty, used to trigger the api_key preflight error and
// the tracker.api_key.github_token_hint warning.
func githubMissingAPIKeyWorkflow() []byte {
	return []byte(`---
polling:
  interval_ms: 30000
tracker:
  kind: github
  api_key: "$SORTIE_TEST_NONEXISTENT_VAR_303"
  project: "sortie-ai/sortie"
  active_states:
    - backlog
  terminal_states:
    - done
agent:
  kind: mock
---
Do {{ .issue.title }}.
`)
}

// TestValidateGitHubInvalidProject verifies that sortie validate exits 1
// and emits a tracker.project.format error when tracker.project is not
// in owner/repo format.
func TestValidateGitHubInvalidProject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubInvalidProjectWorkflow())

	t.Run("text output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate) = %d, want 1; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "tracker.project.format") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.project.format")
		}
	})

	t.Run("json output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("run(validate --format json) = %d, want 1; stderr: %s", code, stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		if out.Valid {
			t.Errorf("validateOutput.Valid = true, want false")
		}

		found := false
		for _, e := range out.Errors {
			if e.Check == "tracker.project.format" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("validateOutput.Errors = %v, want entry with check %q", out.Errors, "tracker.project.format")
		}
	})
}

// TestValidateGitHubStateOverlapWarning verifies that sortie validate exits 0
// with a tracker.states.overlap warning when active_states and terminal_states
// share a label.
func TestValidateGitHubStateOverlapWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubStateOverlapWorkflow())

	t.Run("text output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(validate) = %d, want 0; stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "tracker.states.overlap") {
			t.Errorf("stderr = %q, want to contain %q", stderr.String(), "tracker.states.overlap")
		}
	})

	t.Run("json output", func(t *testing.T) {
		t.Parallel()

		var stdout, stderr bytes.Buffer
		ctx := context.Background()

		code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run(validate --format json) = %d, want 0; stderr: %s", code, stderr.String())
		}

		var out validateOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
		}
		if !out.Valid {
			t.Errorf("validateOutput.Valid = false, want true")
		}
		if len(out.Errors) != 0 {
			t.Errorf("validateOutput.Errors = %v, want empty", out.Errors)
		}

		found := false
		for _, w := range out.Warnings {
			if w.Check == "tracker.states.overlap" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("validateOutput.Warnings = %v, want entry with check %q", out.Warnings, "tracker.states.overlap")
		}
	})
}

// TestValidateGitHubTokenHintWarning verifies that sortie validate exits 1
// (generic tracker.api_key error) and also emits the
// tracker.api_key.github_token_hint advisory warning when GITHUB_TOKEN is set.
func TestValidateGitHubTokenHintWarning(t *testing.T) {
	// No t.Parallel(): uses t.Setenv to control GITHUB_TOKEN.
	t.Setenv("GITHUB_TOKEN", "ghp_test_token_validate_hint")

	dir := t.TempDir()
	wfPath := writeCustomWorkflowFile(t, dir, githubMissingAPIKeyWorkflow())

	var stdout, stderr bytes.Buffer
	ctx := context.Background()

	code := run(ctx, []string{"validate", "--format", "json", wfPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(validate --format json) = %d, want 1 (generic api_key error); stderr: %s", code, stderr.String())
	}

	var out validateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error: %v", stdout.String(), err)
	}
	if out.Valid {
		t.Errorf("validateOutput.Valid = true, want false")
	}

	// Generic tracker.api_key error must be present.
	foundErr := false
	for _, e := range out.Errors {
		if e.Check == "tracker.api_key" {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Errorf("validateOutput.Errors = %v, want entry with check %q", out.Errors, "tracker.api_key")
	}

	// GITHUB_TOKEN hint warning must also be present.
	foundWarn := false
	for _, w := range out.Warnings {
		if w.Check == "tracker.api_key.github_token_hint" {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Errorf("validateOutput.Warnings = %v, want entry with check %q", out.Warnings, "tracker.api_key.github_token_hint")
	}
}
