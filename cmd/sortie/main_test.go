package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	code := run(ctx, []string{"--port", "8080", wfPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "port=8080") {
		t.Errorf("stderr = %q, want to contain %q", stderr.String(), "port=8080")
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
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
		{"DEMO-1 turn 1 completed", `issue_identifier=DEMO-1 turn_number=1`},
		{"DEMO-1 turn 2 completed", `issue_identifier=DEMO-1 turn_number=2`},
		{"DEMO-2 turn 1 completed", `issue_identifier=DEMO-2 turn_number=1`},
		{"DEMO-2 turn 2 completed", `issue_identifier=DEMO-2 turn_number=2`},
		{"DEMO-1 worker exiting normally", `issue_identifier=DEMO-1 exit_kind=normal`},
		{"DEMO-2 worker exiting normally", `issue_identifier=DEMO-2 exit_kind=normal`},
		{"DEMO-1 handoff succeeded", `issue_identifier=DEMO-1 handoff_state=Done`},
		{"DEMO-2 handoff succeeded", `issue_identifier=DEMO-2 handoff_state=Done`},
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
