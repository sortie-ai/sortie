package workflow

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name           string
		content        []byte // nil means do not create the file
		wantErr        bool
		wantKind       ErrorKind
		wantConfig     map[string]any
		wantPrompt     string
		skipFileCreate bool
	}{
		{
			name:    "HappyPath",
			content: []byte("---\ntracker:\n  kind: jira\n  project: SORT\nagent:\n  kind: claude-code\n---\n# Task\n\nFix the bug in {{ .issue.title }}.\n"),
			wantConfig: map[string]any{
				"tracker": map[string]any{"kind": "jira", "project": "SORT"},
				"agent":   map[string]any{"kind": "claude-code"},
			},
			wantPrompt: "# Task\n\nFix the bug in {{ .issue.title }}.",
		},
		{
			name:       "NoFrontMatter",
			content:    []byte("# Just a prompt\n\nDo the thing.\n"),
			wantConfig: map[string]any{},
			wantPrompt: "# Just a prompt\n\nDo the thing.",
		},
		{
			name:       "EmptyFrontMatter",
			content:    []byte("---\n---\nprompt body\n"),
			wantConfig: map[string]any{},
			wantPrompt: "prompt body",
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
			wantPrompt: "the prompt",
		},
		{
			name:    "TrailingWhitespaceOnDelimiters",
			content: []byte("---   \nkey: value\n---\t  \nthe prompt\n"),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt: "the prompt",
		},
		{
			name:    "NoClosingDelimiter",
			content: []byte("---\nkey: value\n"),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt: "",
		},
		{
			name:    "YAML12StringPreservation",
			content: []byte("---\na: \"NO\"\nb: \"ON\"\nc: \"YES\"\nd: \"null\"\n---\nprompt\n"),
			wantConfig: map[string]any{
				"a": "NO",
				"b": "ON",
				"c": "YES",
				"d": "null",
			},
			wantPrompt: "prompt",
		},
		{
			name:       "PromptBodyTrimming",
			content:    []byte("---\nk: v\n---\n\n\n  prompt text  \n\n\n"),
			wantConfig: map[string]any{"k": "v"},
			wantPrompt: "prompt text",
		},
		{
			name:       "GoTemplateDirectivesPreserved",
			content:    []byte("---\nk: v\n---\n{{ .issue.title }} — {{ if .attempt }}retry{{ end }}\n"),
			wantConfig: map[string]any{"k": "v"},
			wantPrompt: "{{ .issue.title }} — {{ if .attempt }}retry{{ end }}",
		},
		{
			name:    "UTF8BOMStripping",
			content: append([]byte("\xef\xbb\xbf"), []byte("---\nkey: value\n---\nprompt\n")...),
			wantConfig: map[string]any{
				"key": "value",
			},
			wantPrompt: "prompt",
		},
		{
			name:       "EmptyFile",
			content:    []byte(""),
			wantConfig: map[string]any{},
			wantPrompt: "",
		},
		{
			name:       "DelimiterOnlyNoTrailingNewline",
			content:    []byte("---"),
			wantConfig: map[string]any{},
			wantPrompt: "---",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
					t.Fatal("expected error, got nil")
				}
				var wErr *WorkflowError
				if !errors.As(err, &wErr) {
					t.Fatalf("expected *WorkflowError, got %T: %v", err, err)
				}
				if wErr.Kind != tt.wantKind {
					t.Errorf("expected Kind %d, got %d", tt.wantKind, wErr.Kind)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if wf.Config == nil {
				t.Fatal("Config must never be nil")
			}

			if tt.wantConfig != nil {
				assertMapsEqual(t, tt.wantConfig, wf.Config)
			}

			if wf.PromptTemplate != tt.wantPrompt {
				t.Errorf("PromptTemplate mismatch\n  want: %q\n  got:  %q", tt.wantPrompt, wf.PromptTemplate)
			}
		})
	}
}

func TestWorkflowError_Error(t *testing.T) {
	t.Run("ErrParseError_hint", func(t *testing.T) {
		e := &WorkflowError{Kind: ErrParseError, Path: "test.md", Err: errors.New("yaml: bad")}
		msg := e.Error()
		if want := "did you forget the closing '---' delimiter?"; !containsSubstring(msg, want) {
			t.Errorf("ErrParseError message missing hint\n  want substring: %q\n  got: %q", want, msg)
		}
	})

	t.Run("ErrMissingFile_path", func(t *testing.T) {
		e := &WorkflowError{Kind: ErrMissingFile, Path: "/some/path.md", Err: errors.New("no such file")}
		msg := e.Error()
		if !containsSubstring(msg, "/some/path.md") {
			t.Errorf("ErrMissingFile message missing path\n  got: %q", msg)
		}
	})

	t.Run("ErrFrontMatterNotMap_type", func(t *testing.T) {
		e := &WorkflowError{Kind: ErrFrontMatterNotMap, Path: "t.md", Err: errors.New("got []interface {}")}
		msg := e.Error()
		if !containsSubstring(msg, "YAML map") {
			t.Errorf("ErrFrontMatterNotMap message missing 'YAML map'\n  got: %q", msg)
		}
	})
}

func TestWorkflowError_Unwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	e := &WorkflowError{Kind: ErrParseError, Path: "x.md", Err: sentinel}
	if !errors.Is(e, sentinel) {
		t.Error("Unwrap did not expose the underlying sentinel error")
	}
}

// assertMapsEqual performs a recursive value comparison of two
// map[string]any values, reporting mismatches via t.Errorf.
func assertMapsEqual(t *testing.T, want, got map[string]any) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("map length mismatch: want %d, got %d\n  want: %v\n  got:  %v", len(want), len(got), want, got)
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
		} else if wv != gv {
			t.Errorf("key %q: want %v (%T), got %v (%T)", k, wv, wv, gv, gv)
		}
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
