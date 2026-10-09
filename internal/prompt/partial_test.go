package prompt

import (
	"errors"
	"strings"
	"testing"
)

const (
	bodySource = "WORKFLOW.md"
	bodyOffset = 10
)

func partialFile(name, body string) PartialFile {
	return PartialFile{Path: "/wf/" + name, Name: "./" + name, Body: body}
}

func parseSet(files []PartialFile, body string) (*Template, error) {
	partials, err := ParsePartials(files)
	if err != nil {
		return nil, err
	}
	return ParseWithPartials(body, bodySource, bodyOffset, partials)
}

func mustParseSet(t *testing.T, files []PartialFile, body string) *Template {
	t.Helper()
	tmpl, err := parseSet(files, body)
	if err != nil {
		t.Fatalf("parseSet(%q): %v", body, err)
	}
	return tmpl
}

func TestParseWithPartials_LoadFaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		partials   []PartialFile
		body       string
		wantErr    bool
		wantSource string
		wantLine   int
		wantInErr  string
	}{
		{
			name:       "syntax error in partial",
			partials:   []PartialFile{partialFile("p/a.md", "{{ define \"x\" }}ok{{ end }}\n\n\n{{ if }}")},
			body:       `{{ template "x" . }}`,
			wantErr:    true,
			wantSource: "/wf/p/a.md",
			wantLine:   4,
		},
		{
			name:       "text outside define block",
			partials:   []PartialFile{partialFile("p/a.md", "{{ define \"x\" }}ok{{ end }}\n\nstray\n")},
			body:       `{{ template "x" . }}`,
			wantErr:    true,
			wantSource: "/wf/p/a.md",
			wantLine:   3,
		},
		{
			name: "undefined call in untaken else branch of partial",
			partials: []PartialFile{partialFile("p/a.md",
				"{{ define \"x\" }}\n{{ if .issue.never }}ok{{ else }}\n{{ template \"missing\" . }}{{ end }}{{ end }}")},
			body:       `{{ template "x" . }}`,
			wantErr:    true,
			wantSource: "/wf/p/a.md",
			wantLine:   3,
		},
		{
			name: "call cycle loads",
			partials: []PartialFile{partialFile("p/a.md",
				`{{ define "a" }}{{ template "b" . }}{{ end }}{{ define "b" }}{{ if .issue.again }}{{ template "a" . }}{{ end }}{{ end }}`)},
			body: `{{ template "a" . }}`,
		},
		{
			name: "name defined in two partials",
			partials: []PartialFile{
				partialFile("p/a.md", `{{ define "x" }}a{{ end }}`),
				partialFile("p/b.md", "\n{{ define \"x\" }}b{{ end }}"),
			},
			body:       `{{ template "x" . }}`,
			wantErr:    true,
			wantSource: "/wf/p/b.md",
			wantLine:   2,
			wantInErr:  "/wf/p/a.md",
		},
		{
			name:       "prompt template defines partial name",
			partials:   []PartialFile{partialFile("p/a.md", `{{ define "x" }}a{{ end }}`)},
			body:       "line one\n{{ define \"x\" }}b{{ end }}{{ template \"x\" . }}",
			wantErr:    true,
			wantSource: bodySource,
			wantLine:   2 + bodyOffset,
			wantInErr:  "/wf/p/a.md",
		},
		{
			name:       "reserved name in partial",
			partials:   []PartialFile{partialFile("p/a.md", "\n\n{{ define \"prompt\" }}a{{ end }}")},
			body:       "text",
			wantErr:    true,
			wantSource: "/wf/p/a.md",
			wantLine:   3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseSet(tt.partials, tt.body)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("parseSet(%q) = %v, want nil", tt.body, err)
				}
				return
			}
			var te *TemplateError
			if !errors.As(err, &te) {
				t.Fatalf("parseSet(%q) error = %T (%v), want *TemplateError", tt.body, err, err)
			}
			if te.Kind != ErrTemplateParse {
				t.Errorf("parseSet(%q) Kind = %v, want %v", tt.body, te.Kind, ErrTemplateParse)
			}
			if te.Source != tt.wantSource || te.Line != tt.wantLine {
				t.Errorf("parseSet(%q) located at %s:%d, want %s:%d", tt.body, te.Source, te.Line, tt.wantSource, tt.wantLine)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("parseSet(%q) error = %q, want to contain %q", tt.body, err, tt.wantInErr)
			}
		})
	}
}

func TestParseWithPartials_SharedLayoutRendersOwnBlock(t *testing.T) {
	t.Parallel()

	files := []PartialFile{partialFile("shared.md", `{{ define "context" -}}
Resolve {{ .issue.identifier }}: {{ .issue.title }}
{{- end }}
{{ define "layout" -}}
{{ template "context" . }}
{{ template "first_run_steps" . }}
{{- end }}`)}
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "bug",
			body: "{{ define \"first_run_steps\" }}Reproduce the bug first.{{ end }}\n{{- template \"layout\" . }}",
			want: "Resolve A-1: T\nReproduce the bug first.",
		},
		{
			name: "feature",
			body: "{{ define \"first_run_steps\" }}Write the specification first.{{ end }}\n{{- template \"layout\" . }}",
			want: "Resolve A-1: T\nWrite the specification first.",
		},
	}
	partials, err := ParsePartials(files)
	if err != nil {
		t.Fatalf("ParsePartials: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpl, err := ParseWithPartials(tt.body, bodySource, 0, partials)
			if err != nil {
				t.Fatalf("ParseWithPartials(%q): %v", tt.body, err)
			}

			got, err := tmpl.Render(map[string]any{"identifier": "A-1", "title": "T"}, nil, RunContext{})
			if err != nil {
				t.Fatalf("Render() = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTemplate_RenderErrorInPartialIsLocatedInPartial(t *testing.T) {
	t.Parallel()

	tmpl := mustParseSet(t,
		[]PartialFile{partialFile("p/a.md", "{{ define \"x\" }}\nok\n{{ .issue.absent }}{{ end }}")},
		`{{ template "x" . }}`)

	_, err := tmpl.Render(map[string]any{"title": "T"}, nil, RunContext{})

	var te *TemplateError
	if !errors.As(err, &te) {
		t.Fatalf("Render() error = %T (%v), want *TemplateError", err, err)
	}
	if te.Kind != ErrTemplateRender {
		t.Errorf("Render() Kind = %v, want %v", te.Kind, ErrTemplateRender)
	}
	if te.Source != "/wf/p/a.md" || te.Line != 3 {
		t.Errorf("Render() located at %s:%d, want /wf/p/a.md:3", te.Source, te.Line)
	}
}
