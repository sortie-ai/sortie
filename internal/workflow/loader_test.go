package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		content              []byte // nil means do not create the file
		wantErr              bool
		wantKind             ErrorKind
		wantConfig           map[string]any
		wantPrompt           string
		wantFrontMatterLines int
		skipFileCreate       bool
	}{
		{
			name:    "HappyPath",
			content: []byte("---\ntracker:\n  kind: jira\n  project: SORT\nagent:\n  kind: claude-code\n---\n# Task\n\nFix the bug in {{ .issue.title }}.\n"),
			wantConfig: map[string]any{
				"tracker": map[string]any{"kind": "jira", "project": "SORT"},
				"agent":   map[string]any{"kind": "claude-code"},
			},
			wantPrompt:           "# Task\n\nFix the bug in {{ .issue.title }}.",
			wantFrontMatterLines: 7,
		},
		{
			name:                 "NoFrontMatter",
			content:              []byte("# Just a prompt\n\nDo the thing.\n"),
			wantConfig:           map[string]any{},
			wantPrompt:           "# Just a prompt\n\nDo the thing.",
			wantFrontMatterLines: 0,
		},
		{
			name:                 "EmptyFrontMatter",
			content:              []byte("---\n---\nprompt body\n"),
			wantConfig:           map[string]any{},
			wantPrompt:           "prompt body",
			wantFrontMatterLines: 2,
		},
		{
			name:           "MissingFile",
			skipFileCreate: true,
			wantErr:        true,
			wantKind:       ErrMissingFile,
		},
		{
			name:     "InvalidYAML",
			content:  []byte("---\n: : : invalid\n\t\tbad yaml {{{\n---\nprompt\n"),
			wantErr:  true,
			wantKind: ErrParseError,
		},
		{
			name:     "NonMapYAML_List",
			content:  []byte("---\n- a\n- b\n- c\n---\nprompt\n"),
			wantErr:  true,
			wantKind: ErrFrontMatterNotMap,
		},
		{
			name:     "NonMapYAML_Scalar",
			content:  []byte("---\n42\n---\nprompt\n"),
			wantErr:  true,
			wantKind: ErrFrontMatterNotMap,
		},
		{
			name:    "CRLFLineEndings",
			content: []byte("---\r\nkey: value\r\n---\r\nthe prompt\r\n"),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt:           "the prompt",
			wantFrontMatterLines: 3,
		},
		{
			name:    "TrailingWhitespaceOnDelimiters",
			content: []byte("---   \nkey: value\n---\t  \nthe prompt\n"),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt:           "the prompt",
			wantFrontMatterLines: 3,
		},
		{
			name:    "NoClosingDelimiter",
			content: []byte("---\nkey: value\n"),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt:           "",
			wantFrontMatterLines: 2,
		},
		{
			// YAML 1.1 treats bare NO, ON, YES as booleans. yaml.v3 follows
			// YAML 1.2 core schema where these decode as strings when the
			// target is map[string]any. Unquoted null is a genuine YAML null
			// in both versions — the map entry is present with a nil value.
			name:    "YAML12StringPreservation",
			content: []byte("---\na: NO\nb: ON\nc: YES\n---\nprompt\n"),
			wantConfig: map[string]any{
				"a": "NO",
				"b": "ON",
				"c": "YES",
			},
			wantPrompt:           "prompt",
			wantFrontMatterLines: 5,
		},
		{
			name:                 "PromptBodyTrimming",
			content:              []byte("---\nk: v\n---\n\n\n  prompt text  \n\n\n"),
			wantConfig:           map[string]any{"k": "v"},
			wantPrompt:           "prompt text",
			wantFrontMatterLines: 3,
		},
		{
			name:                 "GoTemplateDirectivesPreserved",
			content:              []byte("---\nk: v\n---\n{{ .issue.title }} — {{ if .attempt }}retry{{ end }}\n"),
			wantConfig:           map[string]any{"k": "v"},
			wantPrompt:           "{{ .issue.title }} — {{ if .attempt }}retry{{ end }}",
			wantFrontMatterLines: 3,
		},
		{
			name:    "UTF8BOMStripping",
			content: append([]byte("\xef\xbb\xbf"), []byte("---\nkey: value\n---\nprompt\n")...),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt:           "prompt",
			wantFrontMatterLines: 3,
		},
		{
			name:                 "EmptyFile",
			content:              []byte(""),
			wantConfig:           map[string]any{},
			wantPrompt:           "",
			wantFrontMatterLines: 0,
		},
		{
			name:                 "DelimiterOnlyNoTrailingNewline",
			content:              []byte("---"),
			wantConfig:           map[string]any{},
			wantPrompt:           "---",
			wantFrontMatterLines: 0,
		},
		{
			name:                 "OpeningDelimiterOnly",
			content:              []byte("---\n"),
			wantConfig:           map[string]any{},
			wantPrompt:           "",
			wantFrontMatterLines: 1,
		},
		{
			name:                 "DashesInQuotedYAMLValue",
			content:              []byte("---\nkey: \"---\"\n---\nprompt\n"),
			wantConfig:           map[string]any{"key": "---"},
			wantPrompt:           "prompt",
			wantFrontMatterLines: 3,
		},
		{
			name:                 "DashesInBlockScalar",
			content:              []byte("---\nkey: |\n  ---\n  more\n---\nprompt\n"),
			wantConfig:           map[string]any{"key": "---\nmore\n"},
			wantPrompt:           "prompt",
			wantFrontMatterLines: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var path string
			if tt.skipFileCreate {
				path = filepath.Join(t.TempDir(), "nonexistent", "WORKFLOW.md")
			} else {
				dir := t.TempDir()
				path = filepath.Join(dir, "WORKFLOW.md")
				if err := os.WriteFile(path, tt.content, 0o644); err != nil {
					t.Fatalf("failed to write test file: %v", err)
				}
			}

			wf, err := Load(path)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load(%q) error = nil, want *WorkflowError", path)
				}
				var wErr *WorkflowError
				if !errors.As(err, &wErr) {
					t.Fatalf("Load(%q) error type = %T, want *WorkflowError", path, err)
				}
				if wErr.Kind != tt.wantKind {
					t.Errorf("Load(%q) WorkflowError.Kind = %d, want %d", path, wErr.Kind, tt.wantKind)
				}
				return
			}

			if err != nil {
				t.Fatalf("Load(%q) unexpected error: %v", path, err)
			}

			if wf.Config == nil {
				t.Fatalf("Load(%q) Config = nil, want non-nil", path)
			}

			if tt.wantConfig != nil {
				assertMapsEqual(t, tt.wantConfig, wf.Config)
			}

			if wf.PromptTemplate != tt.wantPrompt {
				t.Errorf("Load(%q) PromptTemplate = %q, want %q", path, wf.PromptTemplate, tt.wantPrompt)
			}

			if wf.FrontMatterLines != tt.wantFrontMatterLines {
				t.Errorf("Load(%q) FrontMatterLines = %d, want %d", path, wf.FrontMatterLines, tt.wantFrontMatterLines)
			}
		})
	}
}

func TestWorkflowError_Error(t *testing.T) {
	t.Parallel()

	t.Run("ErrParseError_hint", func(t *testing.T) {
		t.Parallel()

		e := &WorkflowError{Kind: ErrParseError, Path: "test.md", Err: errors.New("yaml: bad")}
		msg := e.Error()
		if want := "did you forget the closing '---' delimiter?"; !strings.Contains(msg, want) {
			t.Errorf("WorkflowError{Kind: ErrParseError}.Error() = %q, want substring %q", msg, want)
		}
	})

	t.Run("ErrMissingFile_path", func(t *testing.T) {
		t.Parallel()

		e := &WorkflowError{Kind: ErrMissingFile, Path: "/some/path.md", Err: errors.New("no such file")}
		msg := e.Error()
		if want := "/some/path.md"; !strings.Contains(msg, want) {
			t.Errorf("WorkflowError{Kind: ErrMissingFile}.Error() = %q, want substring %q", msg, want)
		}
	})

	t.Run("ErrFrontMatterNotMap_type", func(t *testing.T) {
		t.Parallel()

		e := &WorkflowError{Kind: ErrFrontMatterNotMap, Path: "t.md", Err: errors.New("got []interface {}")}
		msg := e.Error()
		if want := "YAML map"; !strings.Contains(msg, want) {
			t.Errorf("WorkflowError{Kind: ErrFrontMatterNotMap}.Error() = %q, want substring %q", msg, want)
		}
	})
}

func TestWorkflowError_Unwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	e := &WorkflowError{Kind: ErrParseError, Path: "x.md", Err: sentinel}
	if !errors.Is(e, sentinel) {
		t.Error("Unwrap did not expose the underlying sentinel error")
	}
}

// assertMapsEqual performs a recursive value comparison of two
// map[string]any values, reporting mismatches via t.Errorf. Leaf values
// are compared with reflect.DeepEqual to avoid panics on uncomparable
// types such as []any that YAML sequences decode into.
func assertMapsEqual(t *testing.T, want, got map[string]any) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("map length = %d, want %d\n  got:  %v\n  want: %v", len(got), len(want), got, want)
		return
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing key %q in got map", k)
			continue
		}
		wm, wIsMap := wv.(map[string]any)
		gm, gIsMap := gv.(map[string]any)
		if wIsMap && gIsMap {
			assertMapsEqual(t, wm, gm)
		} else if !reflect.DeepEqual(wv, gv) {
			t.Errorf("key %q: want %v (%T), got %v (%T)", k, wv, wv, gv, gv)
		}
	}
}
