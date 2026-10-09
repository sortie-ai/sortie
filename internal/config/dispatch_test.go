package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func alwaysRegistered(_ string) bool { return true }

func neverRegistered(_ string) bool { return false }

func mkDispatchDir(t *testing.T) string {
	t.Helper()
	// Resolve the temp dir through EvalSymlinks so test paths match what
	// BuildDispatchConfig's resolveWorkflowDir produces. On Windows,
	// t.TempDir() may return an 8.3 short name (e.g. RUNNER~1) which
	// EvalSymlinks canonicalizes to the long form (e.g. runneradmin);
	// without canonicalization here, expected paths built by joining onto
	// the raw temp dir disagree byte-for-byte with the resolved paths
	// production returns.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return dir
}

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
			got, err := BuildDispatchConfig(tt.raw, dir, alwaysRegistered, "")

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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

func TestBuildDispatchConfig_PartialsExpansion(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(mkDispatchDir(t), "wf[prod]")
	for _, name := range []string{"a.md", "b.md", ".#a.md"} {
		writeFile(t, filepath.Join(dir, "partials", name), "x")
	}
	entries := []any{filepath.Join("partials", "b.md"), "partials/*.md"}

	got, err := BuildDispatchConfig(partialsRaw(entries), dir, alwaysRegistered, "")

	if err != nil {
		t.Fatalf("BuildDispatchConfig(%v) error = %v, want nil", entries, err)
	}
	want := []DispatchPartial{
		{Path: filepath.Join(dir, "partials", "b.md"), Name: "./partials/b.md"},
		{Path: filepath.Join(dir, "partials", "a.md"), Name: "./partials/a.md"},
	}
	if !reflect.DeepEqual(got.Partials, want) {
		t.Errorf("BuildDispatchConfig(%v) Partials = %+v, want %+v", entries, got.Partials, want)
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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

			got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

// TestBuildDispatchConfig_PriorityPredicateRange covers the range
// classification at parsePriorityPredicate's scalar-operator arm and
// its "in" sequence arm: a uint64 or float64 outside the platform int
// range is rejected with the same diagnostic text an out-of-range
// coerceIntField caller reports, while an in-range value of either
// type is accepted.
func TestBuildDispatchConfig_PriorityPredicateRange(t *testing.T) {
	t.Parallel()

	dir := mkDispatchDir(t)

	t.Run("scalar operator out of range", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name string
			val  any
		}{
			{"uint64", uint64(9223372036854775808)},
			{"float64", float64(1e20)},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				raw := map[string]any{
					"dispatch": map[string]any{
						"rules": []any{
							map[string]any{
								"name":  "prio-rule",
								"match": map[string]any{"priority": map[string]any{"gt": tt.val}},
								"agent": "mock",
							},
						},
					},
				}

				_, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")
				ce := requireConfigError(t, err)
				if ce.Field != "dispatch.rules[0].match.priority.gt" {
					t.Errorf("ConfigError.Field = %q, want %q", ce.Field, "dispatch.rules[0].match.priority.gt")
				}
				if ce.Message != ErrIntegerOutOfRange.Error() {
					t.Errorf("ConfigError.Message = %q, want %q", ce.Message, ErrIntegerOutOfRange.Error())
				}
			})
		}
	})

	t.Run("in sequence element out of range", func(t *testing.T) {
		t.Parallel()

		raw := map[string]any{
			"dispatch": map[string]any{
				"rules": []any{
					map[string]any{
						"name":  "in-rule",
						"match": map[string]any{"priority": map[string]any{"in": []any{1, uint64(9223372036854775808)}}},
						"agent": "mock",
					},
				},
			},
		}

		_, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")
		ce := requireConfigError(t, err)
		if ce.Field != "dispatch.rules[0].match.priority.in[1]" {
			t.Errorf("ConfigError.Field = %q, want %q", ce.Field, "dispatch.rules[0].match.priority.in[1]")
		}
		if ce.Message != ErrIntegerOutOfRange.Error() {
			t.Errorf("ConfigError.Message = %q, want %q", ce.Message, ErrIntegerOutOfRange.Error())
		}
	})

	t.Run("in-range uint64 and float64 accepted", func(t *testing.T) {
		t.Parallel()

		raw := map[string]any{
			"dispatch": map[string]any{
				"rules": []any{
					map[string]any{
						"name":  "prio-rule",
						"match": map[string]any{"priority": map[string]any{"gt": uint64(5)}},
						"agent": "mock",
					},
				},
			},
		}

		got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")
		if err != nil {
			t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
		}
		if got.Rules[0].Match.Priority.Value != 5 {
			t.Errorf("Priority.Value = %d, want 5", got.Rules[0].Match.Priority.Value)
		}
	})
}

func partialsRaw(entries any) map[string]any {
	return map[string]any{"dispatch": map[string]any{"partials": entries}}
}

func TestBuildDispatchConfig_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       map[string]any
		setup     func(t *testing.T, dir string)
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
			name:      "non-string agent",
			raw:       map[string]any{"dispatch": map[string]any{"rules": []any{map[string]any{"name": "bad-agent", "agent": 7}}}},
			wantField: "dispatch.rules[0].agent",
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
			setup: func(t *testing.T, dir string) {
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
		{
			name:      "partials entry absolute",
			raw:       partialsRaw([]any{filepath.Join(os.TempDir(), "p.md")}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials entry tilde",
			raw:       partialsRaw([]any{"~/p.md"}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials entry leaves the tree",
			raw:       partialsRaw([]any{"../p.md"}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials entry not a string",
			raw:       partialsRaw([]any{42}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials malformed pattern",
			raw:       partialsRaw([]any{"partials/["}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials pattern matches nothing",
			raw:       partialsRaw([]any{"partials/*.md"}),
			wantField: "dispatch.partials[0]",
		},
		{
			name:      "partials pattern selects a directory",
			raw:       partialsRaw([]any{"partials/*"}),
			setup:     func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "partials", "sub", "x.md"), "x") },
			wantField: "dispatch.partials[0]",
		},
		{
			name: "partials pattern selects a symlink to outside the tree",
			raw:  partialsRaw([]any{"partials/*.md"}),
			setup: func(t *testing.T, dir string) {
				outside := filepath.Join(filepath.Dir(dir), "secret.md")
				writeFile(t, outside, "secret")
				writeFile(t, filepath.Join(dir, "partials", "a.md"), "x")
				if err := os.Symlink(outside, filepath.Join(dir, "partials", "b.md")); err != nil {
					t.Skipf("os.Symlink(%q) = %v", outside, err)
				}
			},
			wantField: "dispatch.partials[0]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := mkDispatchDir(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}

			_, err := BuildDispatchConfig(tt.raw, dir, neverRegistered, "")

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

	_, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

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

	got, err := BuildDispatchConfig(raw, dir, alwaysRegistered, "")

	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}

	wantAbs := filepath.Join(dir, relPath)
	if got.Default.TemplateID != wantAbs {
		t.Errorf("Default.TemplateID = %q, want %q", got.Default.TemplateID, wantAbs)
	}
}

// TestBuildDispatchConfig_TemplateIDIsCanonicalAcrossSymlinks locks the
// invariant that BuildDispatchConfig returns the canonical,
// EvalSymlinks-resolved absolute path for every template, even when
// the workflow directory itself is reached through a symlink. Windows
// CI runners exposed this drift via 8.3 short-name paths (a
// t.TempDir() value such as RUNNER~1\... canonicalized to
// runneradmin\... once EvalSymlinks ran); the symlink wiring below
// reproduces the same drift on Linux and macOS so the contract is
// verifiable on every supported runner. Skips gracefully on hosts
// where os.Symlink is unsupported or forbidden.
func TestBuildDispatchConfig_TemplateIDIsCanonicalAcrossSymlinks(t *testing.T) {
	t.Parallel()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()) error = %v, want nil", err)
	}
	canonical := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(canonical, "prompts"), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v, want nil", filepath.Join(canonical, "prompts"), err)
	}
	bugPath := filepath.Join(canonical, "prompts", "bug.md")
	if err := os.WriteFile(bugPath, []byte("body"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v, want nil", bugPath, err)
	}

	via := filepath.Join(base, "via")
	if err := os.Symlink(canonical, via); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("os.Symlink(%q -> %q) unsupported on this filesystem: %v", canonical, via, err)
		}
		t.Skipf("os.Symlink(%q -> %q) = %v (skipping symlink-based assertion)", canonical, via, err)
	}

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
		},
	}

	got, err := BuildDispatchConfig(raw, via, alwaysRegistered, "")
	if err != nil {
		t.Fatalf("BuildDispatchConfig(via=%q) error = %v, want nil", via, err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("BuildDispatchConfig(via=%q) Rules count = %d, want 1", via, len(got.Rules))
	}
	wantTemplate := filepath.Join(canonical, "prompts", "bug.md")
	if got.Rules[0].Selection.TemplateID != wantTemplate {
		t.Errorf("BuildDispatchConfig(via=%q) Rules[0].Selection.TemplateID = %q, want %q (canonical, symlink-resolved)",
			via, got.Rules[0].Selection.TemplateID, wantTemplate)
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

	_, err := BuildDispatchConfig(raw, dir, nil, "")

	if err != nil {
		t.Errorf("BuildDispatchConfig() with nil probe error = %v, want nil (permissive)", err)
	}
}

func kindsRegistered(kinds ...string) func(string) bool {
	return func(kind string) bool { return slices.Contains(kinds, kind) }
}

func dispatchRaw(defaultAgent string, rules ...map[string]any) map[string]any {
	seq := make([]any, len(rules))
	for i, rule := range rules {
		seq[i] = rule
	}
	dispatch := map[string]any{"rules": seq}
	if defaultAgent != "" {
		dispatch["default"] = map[string]any{"agent": defaultAgent}
	}
	return map[string]any{"dispatch": dispatch}
}

func TestBuildDispatchConfig_RuleSettingsBlockFaults(t *testing.T) {
	t.Parallel()

	const (
		fromAgentKind  = ", taken from agent.kind"
		wrongKind      = `settings block for agent kind "kind-b", but this rule runs agent kind "kind-a"`
		notMappingTail = "; write {} for an empty block"
		notMappingHead = "a rule's settings block must hold the kind's settings as keys, got "
		noCommand      = "agent.command belongs to the default agent kind; a dispatch rule cannot set a command"
	)
	rule := func(block map[string]any) map[string]any {
		block["name"] = "r"
		return block
	}
	kindA := func(block any) map[string]any { return rule(map[string]any{"kind-a": block}) }
	type faultCase struct {
		name      string
		raw       map[string]any
		wantField string
		wantMsg   string
	}
	tests := []faultCase{
		{name: "block for another kind than the rule's agent", raw: dispatchRaw("", rule(map[string]any{"agent": "kind-a", "kind-b": map[string]any{}})), wantField: "dispatch.rules[0].kind-b", wantMsg: wrongKind},
		{name: "block for another kind than dispatch.default.agent", raw: dispatchRaw("kind-b", kindA(map[string]any{})), wantField: "dispatch.rules[0].kind-a", wantMsg: `settings block for agent kind "kind-a", but this rule runs agent kind "kind-b", taken from dispatch.default.agent`},
		{name: "block for another kind than agent.kind", raw: dispatchRaw("", rule(map[string]any{"kind-b": map[string]any{}})), wantField: "dispatch.rules[0].kind-b", wantMsg: wrongKind + fromAgentKind},
		{name: "block is a bare YAML null", raw: dispatchRaw("", kindA(nil)), wantField: "dispatch.rules[0].kind-a", wantMsg: notMappingHead + "no value" + notMappingTail},
		{name: "block is a text value", raw: dispatchRaw("", kindA("m")), wantField: "dispatch.rules[0].kind-a", wantMsg: notMappingHead + "a text value" + notMappingTail},
		{name: "block is a list", raw: dispatchRaw("", kindA([]any{"m"})), wantField: "dispatch.rules[0].kind-a", wantMsg: notMappingHead + "a list" + notMappingTail},
		{name: "block writes kind", raw: dispatchRaw("", kindA(map[string]any{"kind": "kind-b"})), wantField: "dispatch.rules[0].kind-a.kind", wantMsg: "a rule chooses its agent kind with its agent key"},
		{name: "block writes command", raw: dispatchRaw("", kindA(map[string]any{"command": "x"})), wantField: "dispatch.rules[0].kind-a.command", wantMsg: noCommand},
		{name: "rule with a block has no name", raw: dispatchRaw("", map[string]any{"kind-a": map[string]any{"model": "m"}}), wantField: "dispatch.rules[0]", wantMsg: "a rule that carries a settings block must have a name"},
		{name: "rule named default carries a block", raw: dispatchRaw("", map[string]any{"name": "default", "kind-a": map[string]any{"model": "m"}}), wantField: "dispatch.rules[0].name", wantMsg: `"default" is the name run history and statistics give the dispatch.default selection; a rule that carries a settings block must use another name`},
		{name: "a wrong-kind block is reported before a non-mapping one", raw: dispatchRaw("", rule(map[string]any{"kind-b": nil})), wantField: "dispatch.rules[0].kind-b", wantMsg: wrongKind + fromAgentKind},
		{name: "a non-mapping block is reported before a reserved key", raw: dispatchRaw("", kindA("x")), wantField: "dispatch.rules[0].kind-a", wantMsg: notMappingHead + "a text value" + notMappingTail},
		{name: "a reserved key is reported before a missing name", raw: dispatchRaw("", map[string]any{"kind-a": map[string]any{"command": "x"}}), wantField: "dispatch.rules[0].kind-a.command", wantMsg: noCommand},
		{name: "first failing rule in YAML order is returned", raw: dispatchRaw("", map[string]any{"name": "ok", "match": map[string]any{"labels": []any{"x"}}, "kind-a": map[string]any{}}, map[string]any{"name": "bad", "kind-a": "x"}), wantField: "dispatch.rules[1].kind-a", wantMsg: notMappingHead + "a text value" + notMappingTail},
		{name: "a key that names no registered kind stays unknown", raw: dispatchRaw("", rule(map[string]any{"match": map[string]any{"labels": []any{"x"}}, "no-such-kind": map[string]any{}})), wantField: "dispatch.rules[0].no-such-kind", wantMsg: "unknown key"},
		{name: "a rule with none of the four carries", raw: dispatchRaw("", rule(map[string]any{})), wantField: "dispatch.rules[0]", wantMsg: "rule must specify at least one of match, stage, agent, template, or a settings block"},
		{name: "dispatch.default names a registered kind", raw: map[string]any{"dispatch": map[string]any{"default": map[string]any{"kind-a": map[string]any{}}}}, wantField: "dispatch.default.kind-a", wantMsg: "dispatch.default carries no settings block; the top-level kind-a block holds the default settings"},
		{name: "dispatch.default names no registered kind", raw: map[string]any{"dispatch": map[string]any{"default": map[string]any{"no-such-kind": map[string]any{}}}}, wantField: "dispatch.default.no-such-kind", wantMsg: "unknown key"},
	}
	for _, key := range []string{"turn_timeout_ms", "read_timeout_ms", "stall_timeout_ms", "stop_grace_ms"} {
		tests = append(tests, faultCase{
			name:      "block writes " + key,
			raw:       dispatchRaw("", kindA(map[string]any{key: nil})),
			wantField: "dispatch.rules[0].kind-a." + key,
			wantMsg:   "agent." + key + " is workflow-wide; a dispatch rule cannot override it",
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := BuildDispatchConfig(tt.raw, mkDispatchDir(t), kindsRegistered("kind-a", "kind-b"), "kind-a")

			ce := requireConfigError(t, err)
			if ce.Field != tt.wantField || ce.Message != tt.wantMsg {
				t.Errorf("BuildDispatchConfig() error = {Field:%q Message:%q}, want {Field:%q Message:%q}", ce.Field, ce.Message, tt.wantField, tt.wantMsg)
			}
		})
	}
}

func TestBuildDispatchConfig_RuleSettingsBlockStored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		raw          map[string]any
		wantKind     string
		wantSettings map[string]any
	}{
		{name: "rule agent names the kind", raw: dispatchRaw("kind-a", map[string]any{"name": "r", "agent": "kind-b", "kind-b": map[string]any{"model": "m"}}), wantKind: "kind-b", wantSettings: map[string]any{"model": "m"}},
		{name: "dispatch.default.agent names the kind when the rule has no agent", raw: dispatchRaw("kind-b", map[string]any{"name": "r", "kind-b": map[string]any{"effort": nil}}), wantKind: "kind-b", wantSettings: map[string]any{"effort": nil}},
		{name: "agent.kind names the kind when nothing else does", raw: dispatchRaw("", map[string]any{"name": "r", "kind-a": map[string]any{}}), wantKind: "kind-a", wantSettings: map[string]any{}},
		{name: "a rule without a block carries none", raw: dispatchRaw("", map[string]any{"name": "r", "agent": "kind-a"}), wantKind: "", wantSettings: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := BuildDispatchConfig(tt.raw, mkDispatchDir(t), kindsRegistered("kind-a", "kind-b"), "kind-a")

			if err != nil {
				t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
			}
			if rule := got.Rules[0]; rule.SettingsKind != tt.wantKind || !reflect.DeepEqual(rule.Settings, tt.wantSettings) {
				t.Errorf("Rules[0] SettingsKind, Settings = %q, %#v, want %q, %#v", rule.SettingsKind, rule.Settings, tt.wantKind, tt.wantSettings)
			}
		})
	}
}

func titleRule(name string, match map[string]any) map[string]any {
	return map[string]any{"name": name, "match": match}
}

func mustBuildDispatch(t *testing.T, raw map[string]any) DispatchConfig {
	t.Helper()
	got, err := BuildDispatchConfig(raw, mkDispatchDir(t), alwaysRegistered, "kind-a")
	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v, want nil", err)
	}
	return got
}

func TestBuildDispatchConfig_TitlePhrases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		match     map[string]any
		wantTitle []string
	}{
		{name: "a scalar becomes a one-element list", match: map[string]any{"title": "Fix"}, wantTitle: []string{"Fix"}},
		{name: "a scalar is stored as written", match: map[string]any{"title": "  Fix   Login "}, wantTitle: []string{"  Fix   Login "}},
		{name: "a list keeps YAML order", match: map[string]any{"title": []any{"zeta", "alpha", "mid"}}, wantTitle: []string{"zeta", "alpha", "mid"}},
		{name: "phrases are stored as written", match: map[string]any{"title": []any{"  Fix   Login ", "fix", "fix", "[INFRA]", "\u00a0wip:"}}, wantTitle: []string{"  Fix   Login ", "fix", "fix", "[INFRA]", "\u00a0wip:"}},
		{name: "an absent key leaves Title nil", match: map[string]any{"labels": []any{"bug"}}, wantTitle: nil},
		{name: "title beside labels stores both", match: map[string]any{"title": "fix", "labels": []any{"bug"}}, wantTitle: []string{"fix"}},
		{name: "asterisk, question mark and brackets are ordinary text", match: map[string]any{"title": []any{"*", "*infra*", "?", "[abc]"}}, wantTitle: []string{"*", "*infra*", "?", "[abc]"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mustBuildDispatch(t, dispatchRaw("", titleRule("r", tt.match)))

			if !reflect.DeepEqual(got.Rules[0].Match.Title, tt.wantTitle) {
				t.Errorf("Rules[0].Match.Title = %#v, want %#v", got.Rules[0].Match.Title, tt.wantTitle)
			}
		})
	}

	t.Run("title beside labels keeps the labels", func(t *testing.T) {
		t.Parallel()

		got := mustBuildDispatch(t, dispatchRaw("", titleRule("r", map[string]any{"title": "fix", "labels": []any{"bug"}})))

		if !slices.Equal(got.Rules[0].Match.Labels, []string{"bug"}) {
			t.Errorf("Rules[0].Match.Labels = %v, want [bug]", got.Rules[0].Match.Labels)
		}
	})

	t.Run("a title-only rule is not a catch-all and may precede a catch-all", func(t *testing.T) {
		t.Parallel()

		got := mustBuildDispatch(t, dispatchRaw("",
			titleRule("by-title", map[string]any{"title": "[infra]"}),
			map[string]any{"name": "rest", "agent": "kind-a"},
		))

		if len(got.Rules) != 2 {
			t.Fatalf("Rules count = %d, want 2", len(got.Rules))
		}
		if got.Rules[0].IsCatchAll {
			t.Errorf("Rules[0].IsCatchAll = true, want false for a title-only rule")
		}
		if !got.Rules[1].IsCatchAll {
			t.Errorf("Rules[1].IsCatchAll = false, want true for the rule without match")
		}
	})

	nullish := []struct {
		name          string
		match         map[string]any
		wantCatchAll  bool
		wantNoLabels  bool
		wantTitleKept bool
	}{
		{name: "labels null alone still leaves the rule a catch-all", match: map[string]any{"labels": nil}, wantCatchAll: true, wantNoLabels: true},
		{name: "labels empty list alone still leaves the rule a catch-all", match: map[string]any{"labels": []any{}}, wantCatchAll: true, wantNoLabels: true},
		{name: "labels null beside title leaves labels out of the match", match: map[string]any{"labels": nil, "title": "fix"}, wantNoLabels: true, wantTitleKept: true},
		{name: "labels empty list beside title leaves labels out of the match", match: map[string]any{"labels": []any{}, "title": "fix"}, wantNoLabels: true, wantTitleKept: true},
	}
	for _, tt := range nullish {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mustBuildDispatch(t, dispatchRaw("", titleRule("r", tt.match)))

			rule := got.Rules[0]
			if rule.IsCatchAll != tt.wantCatchAll {
				t.Errorf("Rules[0].IsCatchAll = %v, want %v", rule.IsCatchAll, tt.wantCatchAll)
			}
			if tt.wantNoLabels && len(rule.Match.Labels) != 0 {
				t.Errorf("Rules[0].Match.Labels = %v, want empty", rule.Match.Labels)
			}
			if tt.wantTitleKept && !slices.Equal(rule.Match.Title, []string{"fix"}) {
				t.Errorf("Rules[0].Match.Title = %v, want [fix]", rule.Match.Title)
			}
		})
	}
}

func TestBuildDispatchConfig_TitleFaults(t *testing.T) {
	t.Parallel()

	const (
		needsPhrase = "needs at least one phrase; remove the key to leave the title out of the match"
		blankPhrase = "a phrase needs a character other than white space"
		hintList    = `; quote a phrase that starts with "[", as in "[infra]"`
		hintMap     = `; quote a phrase that contains ": ", as in "fix: typo"`
		hintQuote   = "; quote the phrase"
		titleField  = "dispatch.rules[0].match.title"
	)
	scalarFault := func(shape, hint string) string {
		return "expected a phrase or a list of phrases, got " + shape + hint
	}
	elemFault := func(shape, hint string) string {
		return "expected a phrase, got " + shape + hint
	}
	onlyTitle := func(title any) map[string]any {
		return dispatchRaw("", titleRule("r", map[string]any{"title": title}))
	}

	tests := []struct {
		name      string
		raw       map[string]any
		wantField string
		wantMsg   string
	}{
		{name: "T-1 null", raw: onlyTitle(nil), wantField: titleField, wantMsg: needsPhrase},
		{name: "T-1 empty list", raw: onlyTitle([]any{}), wantField: titleField, wantMsg: needsPhrase},
		{name: "T-1 reports the rule index", raw: dispatchRaw("", titleRule("ok", map[string]any{"labels": []any{"bug"}}), titleRule("bad", map[string]any{"title": nil})), wantField: "dispatch.rules[1].match.title", wantMsg: needsPhrase},

		{name: "T-2 integer", raw: onlyTitle(404), wantField: titleField, wantMsg: scalarFault("a number", hintQuote)},
		{name: "T-2 float", raw: onlyTitle(1.5), wantField: titleField, wantMsg: scalarFault("a number", hintQuote)},
		{name: "T-2 true/false value", raw: onlyTitle(true), wantField: titleField, wantMsg: scalarFault("a true/false value", hintQuote)},
		{name: "T-2 map", raw: onlyTitle(map[string]any{"fix": "typo"}), wantField: titleField, wantMsg: scalarFault("a map", hintMap)},
		{name: "T-2 timestamp", raw: onlyTitle(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), wantField: titleField, wantMsg: scalarFault("a value of an unexpected type", hintQuote)},

		{name: "T-3 list element", raw: onlyTitle([]any{"fix", []any{"infra"}}), wantField: titleField + "[1]", wantMsg: elemFault("a list", hintList)},
		{name: "T-3 map element", raw: onlyTitle([]any{map[string]any{"fix": "typo"}}), wantField: titleField + "[0]", wantMsg: elemFault("a map", hintMap)},
		{name: "T-3 number element", raw: onlyTitle([]any{"a", "b", 404}), wantField: titleField + "[2]", wantMsg: elemFault("a number", hintQuote)},
		{name: "T-3 true/false element", raw: onlyTitle([]any{true}), wantField: titleField + "[0]", wantMsg: elemFault("a true/false value", hintQuote)},
		{name: "T-3 timestamp element", raw: onlyTitle([]any{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}), wantField: titleField + "[0]", wantMsg: elemFault("a value of an unexpected type", hintQuote)},
		{name: "T-3 null element carries no hint", raw: onlyTitle([]any{"fix", nil}), wantField: titleField + "[1]", wantMsg: elemFault("no value", "")},

		{name: "T-4 empty scalar", raw: onlyTitle(""), wantField: titleField + "[0]", wantMsg: blankPhrase},
		{name: "T-4 spaces scalar", raw: onlyTitle("   "), wantField: titleField + "[0]", wantMsg: blankPhrase},
		{name: "T-4 tab and newline", raw: onlyTitle("\t\n"), wantField: titleField + "[0]", wantMsg: blankPhrase},
		{name: "T-4 no-break space", raw: onlyTitle("\u00a0"), wantField: titleField + "[0]", wantMsg: blankPhrase},
		{name: "T-4 ideographic space", raw: onlyTitle("\u3000"), wantField: titleField + "[0]", wantMsg: blankPhrase},
		{name: "T-4 blank phrase in a list", raw: onlyTitle([]any{"fix", "", "wip:"}), wantField: titleField + "[1]", wantMsg: blankPhrase},
		{name: "T-4 first blank phrase wins", raw: onlyTitle([]any{"fix", " ", "\u3000"}), wantField: titleField + "[1]", wantMsg: blankPhrase},

		{name: "a fault in labels is reported before a fault in title", raw: dispatchRaw("", titleRule("r", map[string]any{"title": []any{}, "labels": []any{"[unclosed"}})), wantField: "dispatch.rules[0].match.labels[0]", wantMsg: ""},
		{name: "an unrecognized key still fails as an unknown match key", raw: dispatchRaw("", titleRule("r", map[string]any{"titel": "fix"})), wantField: "dispatch.rules[0].match.titel", wantMsg: "unknown match key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := BuildDispatchConfig(tt.raw, mkDispatchDir(t), alwaysRegistered, "kind-a")

			ce := requireConfigError(t, err)
			if ce.Field != tt.wantField {
				t.Errorf("BuildDispatchConfig() error Field = %q, want %q", ce.Field, tt.wantField)
			}
			if tt.wantMsg != "" && ce.Message != tt.wantMsg {
				t.Errorf("BuildDispatchConfig() error Message = %q, want %q", ce.Message, tt.wantMsg)
			}
		})
	}
}

func stagedRule(name, stage string) map[string]any {
	return map[string]any{"name": name, "stage": stage}
}

func TestBuildDispatchConfig_StageFaults(t *testing.T) {
	t.Parallel()

	const (
		needsLabel = "needs a label with a character other than white space"
		withMatch  = "a rule with a stage label is selected by that label and cannot also carry match"
		needsName  = "a rule that carries a stage label must have a name"
		unreach    = "unreachable_rules: catch-all rule at index 0 precedes rule at index "
	)
	tests := []struct {
		name      string
		raw       map[string]any
		wantField string
		wantMsg   string
	}{
		{name: "number", raw: dispatchRaw("", map[string]any{"name": "r", "stage": 5}), wantField: "dispatch.rules[0].stage", wantMsg: "expected a label, got a number"},
		{name: "boolean", raw: dispatchRaw("", map[string]any{"name": "r", "stage": true}), wantField: "dispatch.rules[0].stage", wantMsg: "expected a label, got a true/false value"},
		{name: "list", raw: dispatchRaw("", map[string]any{"name": "r", "stage": []any{"a"}}), wantField: "dispatch.rules[0].stage", wantMsg: "expected a label, got a list"},
		{name: "map", raw: dispatchRaw("", map[string]any{"name": "r", "stage": map[string]any{"a": "b"}}), wantField: "dispatch.rules[0].stage", wantMsg: "expected a label, got a map"},
		{name: "bare null", raw: dispatchRaw("", map[string]any{"name": "r", "stage": nil}), wantField: "dispatch.rules[0].stage", wantMsg: needsLabel},
		{name: "empty string", raw: dispatchRaw("", stagedRule("r", "")), wantField: "dispatch.rules[0].stage", wantMsg: needsLabel},
		{name: "spaces only", raw: dispatchRaw("", stagedRule("r", "  ")), wantField: "dispatch.rules[0].stage", wantMsg: needsLabel},
		{name: "tab and newline only", raw: dispatchRaw("", stagedRule("r", "\t\n")), wantField: "dispatch.rules[0].stage", wantMsg: needsLabel},
		{name: "empty match beside stage", raw: dispatchRaw("", map[string]any{"name": "r", "stage": "s", "match": map[string]any{}}), wantField: "dispatch.rules[0]", wantMsg: withMatch},
		{name: "populated match beside stage", raw: dispatchRaw("", map[string]any{"name": "r", "stage": "s", "match": map[string]any{"labels": []any{"bug"}}}), wantField: "dispatch.rules[0]", wantMsg: withMatch},
		{name: "staged rule without a name", raw: dispatchRaw("", map[string]any{"stage": "s"}), wantField: "dispatch.rules[0]", wantMsg: needsName},
		{name: "staged rule with an empty name", raw: dispatchRaw("", map[string]any{"name": "", "stage": "s"}), wantField: "dispatch.rules[0]", wantMsg: needsName},
		{name: "duplicate label differing in case", raw: dispatchRaw("", stagedRule("plan", "stage-plan"), stagedRule("again", "Stage-Plan")), wantField: "dispatch.rules[1].stage", wantMsg: `duplicate stage label "Stage-Plan" (first at index 0)`},
		{name: "duplicate label names the first rule that holds it", raw: dispatchRaw("", stagedRule("a", "x"), map[string]any{"name": "b", "agent": "kind-a"}, stagedRule("c", "y"), stagedRule("d", "Y")), wantField: "dispatch.rules[3].stage", wantMsg: `duplicate stage label "Y" (first at index 2)`},
		{name: "duplicate label is reported before a catch-all in front of it", raw: dispatchRaw("", map[string]any{"agent": "kind-a"}, stagedRule("a", "x"), stagedRule("b", "X")), wantField: "dispatch.rules[2].stage", wantMsg: `duplicate stage label "X" (first at index 1)`},
		{name: "catch-all before a rule without a stage", raw: dispatchRaw("", map[string]any{"agent": "kind-a"}, map[string]any{"name": "tail", "agent": "kind-a"}), wantField: "dispatch.rules[0]", wantMsg: unreach + "1"},
		{name: "catch-all names the first later rule without a stage", raw: dispatchRaw("", map[string]any{"agent": "kind-a"}, stagedRule("a", "x"), stagedRule("b", "y"), map[string]any{"name": "tail", "agent": "kind-a"}), wantField: "dispatch.rules[0]", wantMsg: unreach + "3"},
		{name: "stage under dispatch.default", raw: map[string]any{"dispatch": map[string]any{"default": map[string]any{"stage": "s"}}}, wantField: "dispatch.default.stage", wantMsg: "unknown key"},
		{name: "stage inside match", raw: dispatchRaw("", map[string]any{"name": "r", "match": map[string]any{"stage": "s"}}), wantField: "dispatch.rules[0].match.stage", wantMsg: "unknown match key"},
		{name: "null is reported before match", raw: dispatchRaw("", map[string]any{"name": "r", "stage": nil, "match": map[string]any{}}), wantField: "dispatch.rules[0].stage", wantMsg: needsLabel},
		{name: "shape is reported before match", raw: dispatchRaw("", map[string]any{"name": "r", "stage": 5, "match": map[string]any{"labels": []any{"bug"}}}), wantField: "dispatch.rules[0].stage", wantMsg: "expected a label, got a number"},
		{name: "match is reported before a missing name", raw: dispatchRaw("", map[string]any{"stage": "s", "match": map[string]any{"labels": []any{"bug"}}}), wantField: "dispatch.rules[0]", wantMsg: withMatch},
		{name: "a rule fault is reported before a duplicate label", raw: dispatchRaw("", stagedRule("a", "x"), stagedRule("b", "X"), map[string]any{"stage": "z"}), wantField: "dispatch.rules[2]", wantMsg: needsName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := BuildDispatchConfig(tt.raw, mkDispatchDir(t), kindsRegistered("kind-a"), "kind-a")

			ce := requireConfigError(t, err)
			if ce.Field != tt.wantField || ce.Message != tt.wantMsg {
				t.Errorf("BuildDispatchConfig() error = {Field:%q Message:%q}, want {Field:%q Message:%q}", ce.Field, ce.Message, tt.wantField, tt.wantMsg)
			}
		})
	}
}

func TestBuildDispatchConfig_StageAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        map[string]any
		wantStages []string
		wantCatch  []bool
	}{
		{
			name:       "catch-all first, staged rule second",
			raw:        dispatchRaw("", map[string]any{"agent": "kind-a"}, stagedRule("plan", "stage-plan")),
			wantStages: []string{"", "stage-plan"},
			wantCatch:  []bool{true, false},
		},
		{
			name:       "catch-all followed only by staged rules",
			raw:        dispatchRaw("", map[string]any{"agent": "kind-a"}, stagedRule("a", "x"), stagedRule("b", "y")),
			wantStages: []string{"", "x", "y"},
			wantCatch:  []bool{true, false, false},
		},
		{
			name:       "rule holding only a name and a stage",
			raw:        dispatchRaw("", stagedRule("plan", "stage-plan")),
			wantStages: []string{"stage-plan"},
			wantCatch:  []bool{false},
		},
		{
			name:       "the name default on a staged rule",
			raw:        dispatchRaw("", stagedRule("default", "stage-plan")),
			wantStages: []string{"stage-plan"},
			wantCatch:  []bool{false},
		},
		{
			name:       "match null beside stage",
			raw:        dispatchRaw("", map[string]any{"name": "r", "stage": "s", "match": nil}),
			wantStages: []string{"s"},
			wantCatch:  []bool{false},
		},
		{
			name:       "label kept as written",
			raw:        dispatchRaw("", stagedRule("r", " Stage-*[x] ")),
			wantStages: []string{" Stage-*[x] "},
			wantCatch:  []bool{false},
		},
		{
			name:       "stage value with an environment reference stays literal",
			raw:        dispatchRaw("", stagedRule("r", "$SORTIE_STAGE_LABEL_NEVER_SET")),
			wantStages: []string{"$SORTIE_STAGE_LABEL_NEVER_SET"},
			wantCatch:  []bool{false},
		},
		{
			name:       "a rule without a stage keeps its catch-all status",
			raw:        dispatchRaw("", titleRule("by-title", map[string]any{"title": "x"}), map[string]any{"name": "rest", "agent": "kind-a"}),
			wantStages: []string{"", ""},
			wantCatch:  []bool{false, true},
		},
		{
			name:       "staged rules may sit before and after the ordered rules",
			raw:        dispatchRaw("", stagedRule("a", "x"), titleRule("by-title", map[string]any{"title": "x"}), stagedRule("b", "y"), map[string]any{"name": "rest", "agent": "kind-a"}),
			wantStages: []string{"x", "", "y", ""},
			wantCatch:  []bool{false, false, false, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mustBuildDispatch(t, tt.raw)

			var stages []string
			var catchAll []bool
			for _, rule := range got.Rules {
				stages = append(stages, rule.Stage)
				catchAll = append(catchAll, rule.IsCatchAll)
			}
			if !slices.Equal(stages, tt.wantStages) {
				t.Errorf("Rules Stage = %q, want %q", stages, tt.wantStages)
			}
			if !slices.Equal(catchAll, tt.wantCatch) {
				t.Errorf("Rules IsCatchAll = %v, want %v", catchAll, tt.wantCatch)
			}
		})
	}
}

func TestStageLabelsEqual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "identical", a: "stage-plan", b: "stage-plan", want: true},
		{name: "differing in case", a: "Stage-Plan", b: "stage-plan", want: true},
		{name: "upper against lower with spaces inside", a: "In Progress", b: "in progress", want: true},
		{name: "leading space is part of the label", a: " a", b: "a", want: false},
		{name: "trailing space is part of the label", a: "a ", b: "a", want: false},
		{name: "different labels", a: "stage-plan", b: "stage-implement", want: false},
		{name: "glob character is literal", a: "stage-*", b: "stage-plan", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := StageLabelsEqual(tt.a, tt.b)

			if got != tt.want {
				t.Errorf("StageLabelsEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestRuleSettingsKeys_StageIsARuleKey(t *testing.T) {
	t.Parallel()

	rule := map[string]any{"name": "r", "stage": "s", "match": nil, "agent": "kind-a", "template": "t.md", "kind-a": map[string]any{}}

	got := ruleSettingsKeys(rule)

	if _, ok := got["stage"]; ok {
		t.Errorf("ruleSettingsKeys(%v) holds %q, want it omitted", rule, "stage")
	}
	if _, ok := got["kind-a"]; !ok || len(got) != 1 {
		t.Errorf("ruleSettingsKeys(%v) = %v, want only kind-a", rule, got)
	}
}

func TestResolveRuleBlockEnvRefs_LeavesStageLiteral(t *testing.T) {
	t.Parallel()

	const literal = "$SORTIE_STAGE_LABEL_NEVER_SET"
	raw := map[string]any{"dispatch": map[string]any{"rules": []any{
		map[string]any{"name": "r", "stage": literal, "kind-a": map[string]any{"model": literal}},
	}}}

	snapshot := resolveRuleBlockEnvRefs(raw, nil)

	rule := raw["dispatch"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	if rule["stage"] != literal {
		t.Errorf("rule stage after resolveRuleBlockEnvRefs = %q, want %q", rule["stage"], literal)
	}
	if got := rule["kind-a"].(map[string]any)["model"]; got == literal {
		t.Errorf("settings block value after resolveRuleBlockEnvRefs = %q, want the resolved value", got)
	}
	if _, ok := snapshot["dispatch.rules[0].stage"]; ok {
		t.Errorf("snapshot = %v, want no entry for dispatch.rules[0].stage", snapshot)
	}
	if _, ok := snapshot["dispatch.rules[0].kind-a.model"]; !ok {
		t.Errorf("snapshot = %v, want an entry for dispatch.rules[0].kind-a.model", snapshot)
	}
}

func TestBuildDispatchConfig_NextLinks(t *testing.T) {
	t.Parallel()

	chained := func(name, stage, next string) map[string]any {
		return map[string]any{"name": name, "stage": stage, "next": next}
	}
	tests := []struct {
		name      string
		raw       map[string]any
		wantField string
		wantMsg   string
		wantNexts []string
	}{
		{name: "two-rule chain is accepted", raw: dispatchRaw("", chained("a", "x", "b"), stagedRule("b", "y")), wantNexts: []string{"b", ""}},
		{name: "cycle", raw: dispatchRaw("", chained("a", "x", "b"), chained("b", "y", "a")), wantField: "dispatch.rules[0].next", wantMsg: "next links form a cycle: a -> b -> a"},
		{name: "unknown rule", raw: dispatchRaw("", chained("a", "x", "ghost")), wantField: "dispatch.rules[0].next", wantMsg: `next "ghost" names no dispatch rule`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.wantField == "" {
				got := mustBuildDispatch(t, tt.raw)

				var nexts []string
				for _, rule := range got.Rules {
					nexts = append(nexts, rule.Next)
				}
				if !slices.Equal(nexts, tt.wantNexts) {
					t.Errorf("BuildDispatchConfig() Rules Next = %q, want %q", nexts, tt.wantNexts)
				}
				return
			}

			_, err := BuildDispatchConfig(tt.raw, mkDispatchDir(t), kindsRegistered("kind-a"), "kind-a")

			ce := requireConfigError(t, err)
			if ce.Field != tt.wantField || ce.Message != tt.wantMsg {
				t.Errorf("BuildDispatchConfig() error = {Field:%q Message:%q}, want {Field:%q Message:%q}", ce.Field, ce.Message, tt.wantField, tt.wantMsg)
			}
		})
	}
}
