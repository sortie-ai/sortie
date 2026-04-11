package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/logging"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
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

// --- resolveDBPath tests ---

func TestResolveDBPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX path conventions")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("YAML double-quoted strings interpret backslash path separators as escape sequences")
	}
	workflowDir := t.TempDir()
	dbDir := t.TempDir()

	writeIssuesFixture(t, workflowDir)
	dbFile := filepath.Join(dbDir, "custom.db")
	wfPath := writeWorkflowFileWithDBPath(t, workflowDir, dbFile)

	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
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
	ctx, cancel := context.WithTimeout(context.Background(), runTestTimeout)
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
			wantEnabled: false,
		},
		{
			name:        "extensions int port",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantPort:    8080,
			wantEnabled: true,
		},
		{
			name:        "extensions float64 port (JSON decode)",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(8080)}},
			wantPort:    8080,
			wantEnabled: true,
		},
		{
			name:        "no server in extensions",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{},
			wantPort:    7678,
			wantEnabled: true,
		},
		{
			name:        "nil extensions",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  nil,
			wantPort:    7678,
			wantEnabled: true,
		},
		{
			name:        "server extension without port key",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"other": "value"}},
			wantPort:    7678,
			wantEnabled: true,
		},
		{
			name:        "server extension port is string (unsupported type)",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": "8080"}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "server extension is not a map",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": "not-a-map"},
			wantPort:    7678,
			wantEnabled: true,
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
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(8080.9)}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions float64 above 65535 rejected",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(99999)}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions negative int rejected",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": -1}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions int above 65535 rejected",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 70000}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
		},
		{
			name:        "extensions int exactly 65535 accepted",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 65535}},
			wantPort:    65535,
			wantEnabled: true,
		},
		{
			name:        "extensions float64 exactly 65535 accepted",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(65535)}},
			wantPort:    65535,
			wantEnabled: true,
		},
		{
			name:        "no flags no extension returns default",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  nil,
			wantPort:    7678,
			wantEnabled: true,
		},
		{
			name:        "extension port 0 disables server",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 0}},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "flag port 0 disables regardless of extension",
			portFlag:    0,
			portFlagSet: true,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "extension overrides default when no flag",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 9090}},
			wantPort:    9090,
			wantEnabled: true,
		},
		{
			name:        "flag overrides extension",
			portFlag:    9090,
			portFlagSet: true,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantPort:    9090,
			wantEnabled: true,
		},
		{
			name:        "extension float64 port 0 disables",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": float64(0)}},
			wantPort:    0,
			wantEnabled: false,
		},
		{
			name:        "extension port is string type returns error",
			portFlag:    7678,
			portFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": "8080"}},
			wantPort:    0,
			wantEnabled: false,
			wantErr:     true,
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

func TestResolveServerHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hostFlag    string
		hostFlagSet bool
		extensions  map[string]any
		wantHost    string
		wantErr     bool
	}{
		{
			name:        "default when no flag and no extension",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  nil,
			wantHost:    "127.0.0.1",
		},
		{
			name:        "flag overrides default",
			hostFlag:    "0.0.0.0",
			hostFlagSet: true,
			extensions:  nil,
			wantHost:    "0.0.0.0",
		},
		{
			name:        "extension overrides default",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"host": "0.0.0.0"}},
			wantHost:    "0.0.0.0",
		},
		{
			name:        "flag wins over extension",
			hostFlag:    "10.0.0.1",
			hostFlagSet: true,
			extensions:  map[string]any{"server": map[string]any{"host": "0.0.0.0"}},
			wantHost:    "10.0.0.1",
		},
		{
			name:        "extension not a string returns error",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"host": 123}},
			wantHost:    "",
			wantErr:     true,
		},
		{
			name:        "extension invalid IP returns error",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"host": "not-an-ip"}},
			wantHost:    "",
			wantErr:     true,
		},
		{
			name:        "extension empty string returns error",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"host": ""}},
			wantHost:    "",
			wantErr:     true,
		},
		{
			name:        "flag invalid IP returns error",
			hostFlag:    "hostname",
			hostFlagSet: true,
			extensions:  nil,
			wantHost:    "",
			wantErr:     true,
		},
		{
			name:        "absent server key returns default",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"tracker": map[string]any{"kind": "file"}},
			wantHost:    "127.0.0.1",
		},
		{
			name:        "server extension not a map returns default",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": "not-map"},
			wantHost:    "127.0.0.1",
		},
		{
			name:        "absent host key returns default",
			hostFlag:    "127.0.0.1",
			hostFlagSet: false,
			extensions:  map[string]any{"server": map[string]any{"port": 8080}},
			wantHost:    "127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotHost, gotErr := resolveServerHost(tt.hostFlag, tt.hostFlagSet, tt.extensions)
			if gotHost != tt.wantHost {
				t.Errorf("resolveServerHost() host = %q, want %q", gotHost, tt.wantHost)
			}
			if (gotErr != nil) != tt.wantErr {
				t.Errorf("resolveServerHost() err = %v, wantErr %v", gotErr, tt.wantErr)
			}
		})
	}
}

// --- Validate subcommand tests (Plan Phase 5) ---

// writeCustomWorkflowFile writes the given YAML front matter and prompt
// body as a WORKFLOW.md in dir and returns the absolute path to the
// created file. It calls filepath.Abs so the returned path is
// absolute regardless of whether dir is relative or absolute.

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

// --- resolveLogFormat tests ---

func TestResolveLogFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		flagValue  string
		flagSet    bool
		extensions map[string]any
		wantFormat logging.Format
		wantErr    bool
	}{
		{
			name:       "flag set: json",
			flagValue:  "json",
			flagSet:    true,
			wantFormat: logging.FormatJSON,
		},
		{
			name:       "flag set: text",
			flagValue:  "text",
			flagSet:    true,
			wantFormat: logging.FormatText,
		},
		{
			name:      "flag set: invalid",
			flagValue: "bogus",
			flagSet:   true,
			wantErr:   true,
		},
		{
			name:       "flag set overrides config",
			flagValue:  "text",
			flagSet:    true,
			extensions: map[string]any{"logging": map[string]any{"format": "json"}},
			wantFormat: logging.FormatText,
		},
		{
			name:       "extension: json",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"format": "json"}},
			wantFormat: logging.FormatJSON,
		},
		{
			name:       "extension: case insensitive",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"format": "JSON"}},
			wantFormat: logging.FormatJSON,
		},
		{
			name:       "extension: invalid string",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"format": "yaml"}},
			wantErr:    true,
		},
		{
			name:       "extension: non-string type (int)",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"format": 42}},
			wantErr:    true,
		},
		{
			name:       "no logging block",
			flagSet:    false,
			extensions: map[string]any{},
			wantFormat: logging.FormatText,
		},
		{
			name:       "nil extensions",
			flagSet:    false,
			extensions: nil,
			wantFormat: logging.FormatText,
		},
		{
			name:       "logging block without format key",
			flagSet:    false,
			extensions: map[string]any{"logging": map[string]any{"level": "info"}},
			wantFormat: logging.FormatText,
		},
		{
			name:       "logging block is not a map",
			flagSet:    false,
			extensions: map[string]any{"logging": "not-a-map"},
			wantFormat: logging.FormatText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveLogFormat(tt.flagValue, tt.flagSet, tt.extensions)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveLogFormat(%q, %v, ...) = %v, want error", tt.flagValue, tt.flagSet, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLogFormat(%q, %v, ...) unexpected error: %v", tt.flagValue, tt.flagSet, err)
			}
			if got != tt.wantFormat {
				t.Errorf("resolveLogFormat(%q, %v, ...) = %v, want %v", tt.flagValue, tt.flagSet, got, tt.wantFormat)
			}
		})
	}
}

// --- --log-format CLI flag integration tests ---

// minimalWorkflowWithLogFormat returns a WORKFLOW.md with the given format
// set in the logging.format extension key.
