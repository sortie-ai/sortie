package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// --- helpers ---

func alwaysRegistered(_ string) bool { return true }

func neverRegistered(_ string) bool { return false }

func mkDispatchDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// writeFile creates a file at the given absolute path with content.
func writeFile(t *testing.T, absPath string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("MkdirAll %q: %v", filepath.Dir(absPath), err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %q: %v", absPath, err)
	}
}

func requireConfigError(t *testing.T, err error) *ConfigError {
	t.Helper()
	if err == nil {
		t.Fatal("BuildDispatchConfig() = nil error, want *ConfigError")
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("BuildDispatchConfig() error type = %T, want *ConfigError", err)
	}
	return ce
}

// --- TestBuildDispatchConfig ---

func TestBuildDispatchConfig_NilOrAbsent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  map[string]any
	}{
		{name: "nil raw map returns zero value", raw: nil},
		{name: "absent dispatch key returns zero value", raw: map[string]any{"tracker": map[string]any{}}},
		{name: "nil dispatch value returns zero value", raw: map[string]any{"dispatch": nil}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := mkDispatchDir(t)
			got, err := BuildDispatchConfig(tt.raw, dir, alwaysRegistered)

			if err != nil {
				t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
			}
			if len(got.Rules) != 0 {
				t.Errorf("BuildDispatchConfig() Rules = %v, want empty", got.Rules)
			}
			if got.Default.AgentKind != "" || got.Default.TemplateID != "" {
				t.Errorf("BuildDispatchConfig() Default = %+v, want zero", got.Default)
			}
		})
	}
}

func TestBuildDispatchConfig_HappyPath(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)
	bugTmpl := filepath.Join(dir, "prompts", "bug.md")
	writeFile(t, bugTmpl, "You are a bug-fixing agent for {{ .issue.identifier }}.")

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":     "bug",
					"match":    map[string]any{"labels": []any{"bug"}},
					"template": "prompts/bug.md",
					"agent":    "claude-code",
				},
			},
			"default": map[string]any{
				"agent": "claude-code",
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("Rules count = %d, want 1", len(got.Rules))
	}
	if got.Rules[0].Name != "bug" {
		t.Errorf("Rules[0].Name = %q, want %q", got.Rules[0].Name, "bug")
	}
	if got.Rules[0].Selection.AgentKind != "claude-code" {
		t.Errorf("Rules[0].Selection.AgentKind = %q, want %q", got.Rules[0].Selection.AgentKind, "claude-code")
	}
	if got.Rules[0].Selection.TemplateID != bugTmpl {
		t.Errorf("Rules[0].Selection.TemplateID = %q, want %q", got.Rules[0].Selection.TemplateID, bugTmpl)
	}
	if got.Default.AgentKind != "claude-code" {
		t.Errorf("Default.AgentKind = %q, want %q", got.Default.AgentKind, "claude-code")
	}
}

func TestBuildDispatchConfig_CatchAllNoMatchBlock(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)
	tmpl := filepath.Join(dir, "catch.md")
	writeFile(t, tmpl, "catch all template")

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"template": "catch.md",
				},
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("Rules count = %d, want 1", len(got.Rules))
	}
	if !got.Rules[0].IsCatchAll {
		t.Errorf("Rules[0].IsCatchAll = false, want true for rule without match block")
	}
}

func TestBuildDispatchConfig_ANDSemantics(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":  "combined",
					"match": map[string]any{"labels": []any{"bug"}, "issue_type": []any{"Bug"}},
					"agent": "claude-code",
				},
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	if len(got.Rules[0].Match.Labels) != 1 || got.Rules[0].Match.Labels[0] != "bug" {
		t.Errorf("Match.Labels = %v, want [bug]", got.Rules[0].Match.Labels)
	}
	if len(got.Rules[0].Match.IssueType) != 1 || got.Rules[0].Match.IssueType[0] != "Bug" {
		t.Errorf("Match.IssueType = %v, want [Bug]", got.Rules[0].Match.IssueType)
	}
}

func TestBuildDispatchConfig_ORWithinKey(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":  "multi-label",
					"match": map[string]any{"labels": []any{"bug", "regression"}},
					"agent": "claude-code",
				},
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	if len(got.Rules[0].Match.Labels) != 2 {
		t.Errorf("Match.Labels = %v, want [bug regression]", got.Rules[0].Match.Labels)
	}
}

func TestBuildDispatchConfig_PriorityPredicates(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	tests := []struct {
		name    string
		priMap  map[string]any
		wantOp  string
		wantVal int
	}{
		{"eq", map[string]any{"eq": 1}, "eq", 1},
		{"lt", map[string]any{"lt": 3}, "lt", 3},
		{"lte", map[string]any{"lte": 2}, "lte", 2},
		{"gt", map[string]any{"gt": 5}, "gt", 5},
		{"gte", map[string]any{"gte": 4}, "gte", 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":  "prio-rule",
							"match": map[string]any{"priority": tt.priMap},
							"agent": "mock",
						},
					},
				},
			}

			got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

			if err != nil {
				t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
			}
			p := got.Rules[0].Match.Priority
			if p == nil {
				t.Fatal("Priority = nil, want non-nil")
			}
			if p.Op != tt.wantOp {
				t.Errorf("Priority.Op = %q, want %q", p.Op, tt.wantOp)
			}
			if p.Value != tt.wantVal {
				t.Errorf("Priority.Value = %d, want %d", p.Value, tt.wantVal)
			}
		})
	}
}

func TestBuildDispatchConfig_PriorityInPredicate(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":  "in-rule",
					"match": map[string]any{"priority": map[string]any{"in": []any{1, 2, 3}}},
					"agent": "mock",
				},
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	p := got.Rules[0].Match.Priority
	if p == nil {
		t.Fatal("Priority = nil, want non-nil")
	}
	if p.Op != "in" {
		t.Errorf("Priority.Op = %q, want %q", p.Op, "in")
	}
	if len(p.Values) != 3 {
		t.Errorf("Priority.Values = %v, want [1 2 3]", p.Values)
	}
}

func TestBuildDispatchConfig_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       map[string]any
		setup     func(dir string)
		wantField string
		wantMsg   string
	}{
		{
			name:      "dispatch is not a map",
			raw:       map[string]any{"dispatch": "not-a-map"},
			wantField: "dispatch",
		},
		{
			name: "dispatch.rules is not a sequence",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": "not-a-list",
				},
			},
			wantField: "dispatch.rules",
		},
		{
			name: "rule element is not a map",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{"not-a-map"},
				},
			},
			wantField: "dispatch.rules[0]",
		},
		{
			name: "rule has no match, agent, or template",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{"name": "empty-rule"},
					},
				},
			},
			wantField: "dispatch.rules[0]",
		},
		{
			name: "rule name with invalid characters",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":  "Invalid Rule!",
							"agent": "mock",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].name",
		},
		{
			name: "duplicate rule names",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{"name": "dup", "agent": "mock"},
						map[string]any{"name": "dup", "agent": "mock"},
					},
				},
			},
			wantField: "dispatch.rules[1]",
		},
		{
			name: "catch-all not at last position",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{"agent": "mock"},
						map[string]any{"name": "after", "agent": "mock"},
					},
				},
			},
			wantField: "dispatch.rules[0]",
			wantMsg:   "unreachable_rules",
		},
		{
			name: "invalid glob in labels",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":  "bad-glob",
							"match": map[string]any{"labels": []any{"[unclosed"}},
							"agent": "mock",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].match.labels[0]",
		},
		{
			name: "unknown priority operator",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":  "bad-prio",
							"match": map[string]any{"priority": map[string]any{"ne": 1}},
							"agent": "mock",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].match.priority",
		},
		{
			name: "unknown agent kind",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":  "bad-agent",
							"agent": "nonexistent-agent",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].agent",
			wantMsg:   "unknown agent kind",
		},
		{
			name: "unknown default agent kind",
			raw: map[string]any{
				"dispatch": map[string]any{
					"default": map[string]any{
						"agent": "unknown-kind",
					},
				},
			},
			wantField: "dispatch.default.agent",
			wantMsg:   "unknown agent kind",
		},
		{
			name: "absolute template path rejected",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":     "abs-path",
							"template": "/etc/passwd",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].template",
			wantMsg:   "must be relative to WORKFLOW.md",
		},
		{
			name: "tilde-prefix template path rejected",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":     "tilde-path",
							"template": "~/templates/bug.md",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].template",
			wantMsg:   "must be relative to WORKFLOW.md",
		},
		{
			name: "missing template file",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":     "missing-tmpl",
							"template": "nonexistent/file.md",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].template",
			wantMsg:   "cannot read template",
		},
		{
			name: "parent traversal template path rejected",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":     "traversal",
							"template": "../traversal-target.md",
						},
					},
				},
			},
			setup: func(dir string) {
				// Create the target outside the workflow dir so EvalSymlinks resolves
				// it but the containment check then rejects it.
				parent := filepath.Dir(dir)
				writeFile(t, filepath.Join(parent, "traversal-target.md"), "outside content")
			},
			wantField: "dispatch.rules[0].template",
			wantMsg:   "escapes",
		},
		{
			name: "unknown rule key",
			raw: map[string]any{
				"dispatch": map[string]any{
					"rules": []any{
						map[string]any{
							"name":    "valid",
							"agent":   "mock",
							"unknown": "field",
						},
					},
				},
			},
			wantField: "dispatch.rules[0].unknown",
			wantMsg:   "unknown key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := mkDispatchDir(t)
			if tt.setup != nil {
				tt.setup(dir)
			}

			_, err := BuildDispatchConfig(tt.raw, dir, neverRegistered)

			ce := requireConfigError(t, err)
			if ce.Field != tt.wantField {
				t.Errorf("ConfigError.Field = %q, want %q", ce.Field, tt.wantField)
			}
			if tt.wantMsg != "" && ce.Message == "" {
				t.Errorf("ConfigError.Message is empty, want substring %q", tt.wantMsg)
			}
		})
	}
}

func TestBuildDispatchConfig_SinglePassFirstError(t *testing.T) {
	t.Parallel()

	// Two errors present: invalid glob in rule[0] AND another error in rule[1].
	// Only the first error (invalid glob at rules[0]) should be returned
	// because parseDispatchRules fails on the first bad rule it encounters.
	dir := mkDispatchDir(t)

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":  "bad-glob",
					"match": map[string]any{"labels": []any{"[invalid"}},
					"agent": "mock",
				},
				// This rule also has an error (unknown key), but it is never reached.
				map[string]any{
					"name":    "second",
					"agent":   "mock",
					"unknown": "field",
				},
			},
		},
	}

	_, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	ce := requireConfigError(t, err)
	if ce.Field != "dispatch.rules[0].match.labels[0]" {
		t.Errorf("ConfigError.Field = %q, want %q (first error wins)", ce.Field, "dispatch.rules[0].match.labels[0]")
	}
}

func TestBuildDispatchConfig_TemplateResolvedToAbsPath(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)
	relPath := "tmpl.md"
	writeFile(t, filepath.Join(dir, relPath), "template body")

	raw := map[string]any{
		"dispatch": map[string]any{
			"default": map[string]any{
				"template": relPath,
			},
		},
	}

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered)

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}

	wantAbs := filepath.Join(dir, relPath)
	if got.Default.TemplateID != wantAbs {
		t.Errorf("Default.TemplateID = %q, want %q", got.Default.TemplateID, wantAbs)
	}
}

func TestBuildDispatchConfig_NilProbeIsPermissive(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	raw := map[string]any{
		"dispatch": map[string]any{
			"rules": []any{
				map[string]any{
					"name":  "any-kind",
					"agent": "totally-unregistered-kind",
				},
			},
		},
	}

	_, err := BuildDispatchConfig(raw, dir, nil)

	if err != nil {
		t.Errorf("BuildDispatchConfig() with nil probe error = %v, want nil (permissive)", err)
	}
}
