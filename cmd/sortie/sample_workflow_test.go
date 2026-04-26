package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// repoRoot returns the absolute path to the repository root, derived
// from the test file's known location at cmd/sortie/.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../")
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return abs
}

// loadSampleWorkflow loads and parses a workflow file from the examples
// directory, returning the parsed template ready for rendering.
func loadSampleWorkflow(t *testing.T, name string) *prompt.Template {
	t.Helper()
	path := filepath.Join(repoRoot(t), "examples", name)
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("workflow.Load(%s): %v", name, err)
	}
	if wf.PromptTemplate == "" {
		t.Fatalf("workflow.Load(%s): empty prompt template", name)
	}
	tmpl, err := prompt.Parse(wf.PromptTemplate, name, wf.FrontMatterLines)
	if err != nil {
		t.Fatalf("prompt.Parse(%s): %v", name, err)
	}
	return tmpl
}

// fullIssue returns a fully-populated issue map exercising all template
// variables.
func fullIssue() map[string]any {
	return map[string]any{
		"identifier":  "TEST-42",
		"title":       "Implement user authentication",
		"description": "Add OAuth2 login flow with SSO support.",
		"url":         "https://tracker.example.com/TEST-42",
		"labels":      []any{"feature", "auth"},
		"parent": map[string]any{
			"identifier": "TEST-10",
		},
		"blocked_by": []any{
			map[string]any{"identifier": "TEST-40", "state": "In Progress"},
			map[string]any{"identifier": "TEST-41", "state": "Done"},
		},
	}
}

// minimalIssue returns an issue map with only required fields populated
// and all optional fields empty/nil.
func minimalIssue() map[string]any {
	return map[string]any{
		"identifier":  "TEST-1",
		"title":       "Minimal task",
		"description": "",
		"url":         "",
		"labels":      []any{},
		"parent":      nil,
		"blocked_by":  []any{},
	}
}

func TestSampleWorkflowLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		file     string
		wantKeys []string
	}{
		{
			name:     "WORKFLOW.md loads with expected config keys",
			file:     "WORKFLOW.md",
			wantKeys: []string{"tracker", "polling", "workspace", "hooks", "agent", "claude-code", "server"},
		},
		{
			name:     "WORKFLOW.test.md loads with expected config keys",
			file:     "WORKFLOW.test.md",
			wantKeys: []string{"tracker", "polling", "workspace", "agent", "file"},
		},
		{
			name:     "WORKFLOW.opencode.md loads with expected config keys",
			file:     "WORKFLOW.opencode.md",
			wantKeys: []string{"tracker", "polling", "workspace", "hooks", "agent", "opencode", "server"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(repoRoot(t), "examples", tt.file)
			wf, err := workflow.Load(path)
			if err != nil {
				t.Fatalf("workflow.Load(%s): %v", tt.file, err)
			}
			if wf.PromptTemplate == "" {
				t.Errorf("workflow.Load(%s): prompt template is empty", tt.file)
			}
			for _, key := range tt.wantKeys {
				if _, ok := wf.Config[key]; !ok {
					t.Errorf("workflow.Load(%s): config missing key %q", tt.file, key)
				}
			}
		})
	}
}

func TestSampleWorkflowRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		file  string
		issue map[string]any
	}{
		{
			name:  "WORKFLOW.md full issue",
			file:  "WORKFLOW.md",
			issue: fullIssue(),
		},
		{
			name:  "WORKFLOW.md minimal issue",
			file:  "WORKFLOW.md",
			issue: minimalIssue(),
		},
		{
			name:  "WORKFLOW.test.md full issue",
			file:  "WORKFLOW.test.md",
			issue: fullIssue(),
		},
		{
			name:  "WORKFLOW.test.md minimal issue",
			file:  "WORKFLOW.test.md",
			issue: minimalIssue(),
		},
		{
			name:  "WORKFLOW.opencode.md full issue",
			file:  "WORKFLOW.opencode.md",
			issue: fullIssue(),
		},
		{
			name:  "WORKFLOW.opencode.md minimal issue",
			file:  "WORKFLOW.opencode.md",
			issue: minimalIssue(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpl := loadSampleWorkflow(t, tt.file)
			rc := prompt.RunContext{TurnNumber: 1, MaxTurns: 15, IsContinuation: false}
			out, err := tmpl.Render(tt.issue, nil, rc)
			if err != nil {
				t.Fatalf("Render(%s, first turn): %v", tt.file, err)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("Render(%s, first turn): produced empty output", tt.file)
			}
		})
	}
}

func TestSampleWorkflowContinuationShorter(t *testing.T) {
	t.Parallel()

	tmpl := loadSampleWorkflow(t, "WORKFLOW.md")
	issue := fullIssue()

	firstRC := prompt.RunContext{TurnNumber: 1, MaxTurns: 15, IsContinuation: false}
	firstOut, err := tmpl.Render(issue, nil, firstRC)
	if err != nil {
		t.Fatalf("Render(first turn): %v", err)
	}

	contRC := prompt.RunContext{TurnNumber: 3, MaxTurns: 15, IsContinuation: true}
	contOut, err := tmpl.Render(issue, nil, contRC)
	if err != nil {
		t.Fatalf("Render(continuation): %v", err)
	}

	// Section 3.4: continuation turns must produce shorter output than first turns.
	if len(contOut) >= len(firstOut) {
		t.Errorf("continuation output (%d chars) should be shorter than first turn (%d chars)",
			len(contOut), len(firstOut))
	}
}

func TestSampleWorkflowRetryBlock(t *testing.T) {
	t.Parallel()

	tmpl := loadSampleWorkflow(t, "WORKFLOW.md")
	issue := fullIssue()

	rc := prompt.RunContext{TurnNumber: 1, MaxTurns: 15, IsContinuation: false}

	// Without retry attempt — no retry text.
	noRetry, err := tmpl.Render(issue, nil, rc)
	if err != nil {
		t.Fatalf("Render(no retry): %v", err)
	}
	if strings.Contains(strings.ToLower(noRetry), "retry attempt") {
		t.Error("Render(attempt=nil) should not contain retry text")
	}

	// With retry attempt=2 — retry text must appear.
	withRetry, err := tmpl.Render(issue, 2, rc)
	if err != nil {
		t.Fatalf("Render(attempt=2): %v", err)
	}
	if !strings.Contains(withRetry, "retry attempt 2") {
		t.Errorf("Render(attempt=2) should contain 'retry attempt 2', got:\n%s", withRetry)
	}
}

func TestSampleWorkflowTemplateVariables(t *testing.T) {
	t.Parallel()

	tmpl := loadSampleWorkflow(t, "WORKFLOW.md")
	issue := fullIssue()
	rc := prompt.RunContext{TurnNumber: 1, MaxTurns: 15, IsContinuation: false}

	out, err := tmpl.Render(issue, nil, rc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Verify all mandatory template variables produce visible output.
	checks := []struct {
		desc string
		want string
	}{
		{"issue.identifier", "TEST-42"},
		{"issue.title", "Implement user authentication"},
		{"issue.description", "Add OAuth2 login flow with SSO support."},
		{"issue.url", "https://tracker.example.com/TEST-42"},
		{"issue.labels via join", "feature, auth"},
		{"issue.parent.identifier", "TEST-10"},
		{"issue.blocked_by identifier", "TEST-40"},
		{"issue.blocked_by state", "In Progress"},
	}

	for _, c := range checks {
		if !strings.Contains(out, c.want) {
			t.Errorf("rendered output missing %s: want %q in output", c.desc, c.want)
		}
	}
}

func TestSampleWorkflowTestFilePathConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "examples", "WORKFLOW.test.md")
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("workflow.Load(WORKFLOW.test.md): %v", err)
	}

	// Verify file.path is CWD-relative (examples/issues.json), not bare issues.json.
	// Section 3.2: file tracker resolves path relative to CWD.
	fileExt, ok := wf.Config["file"]
	if !ok {
		t.Fatal("WORKFLOW.test.md config missing 'file' extension block")
	}
	fileMap, ok := fileExt.(map[string]any)
	if !ok {
		t.Fatalf("file extension type = %T, want map[string]any", fileExt)
	}
	filePath, ok := fileMap["path"]
	if !ok {
		t.Fatal("file extension missing 'path' key")
	}
	got, ok := filePath.(string)
	if !ok {
		t.Fatalf("file.path type = %T, want string", filePath)
	}
	if got != "examples/issues.json" {
		t.Errorf("file.path = %q, want %q (must be CWD-relative, not bare filename)", got, "examples/issues.json")
	}
}

func TestSampleWorkflowNoHTMLComments(t *testing.T) {
	t.Parallel()

	files := []string{"WORKFLOW.md", "WORKFLOW.test.md", "WORKFLOW.opencode.md"}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(repoRoot(t), "examples", name)
			wf, err := workflow.Load(path)
			if err != nil {
				t.Fatalf("workflow.Load(%s): %v", name, err)
			}
			// Section 3.5: HTML comments must not appear — they are not
			// stripped by Go text/template and would leak into the prompt.
			if strings.Contains(wf.PromptTemplate, "<!--") {
				t.Errorf("%s prompt body contains HTML comment (<!--); use Go template comments {{/* */}} instead", name)
			}
		})
	}
}

func TestSampleWorkflowOpenCodeExtension(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "examples", "WORKFLOW.opencode.md")
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("workflow.Load(WORKFLOW.opencode.md): %v", err)
	}

	raw, ok := wf.Config["opencode"]
	if !ok {
		t.Fatal("WORKFLOW.opencode.md config missing 'opencode' extension block")
	}
	opencodeCfg, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("opencode extension type = %T, want map[string]any", raw)
	}

	if _, ok := opencodeCfg["model"]; !ok {
		t.Error("opencode extension missing 'model' field")
	}
	if v, _ := opencodeCfg["dangerously_skip_permissions"].(bool); !v {
		t.Error("opencode extension 'dangerously_skip_permissions' must be true")
	}
	if v, _ := opencodeCfg["disable_autocompact"].(bool); !v {
		t.Error("opencode extension 'disable_autocompact' must be true")
	}
	allowedTools, ok := opencodeCfg["allowed_tools"].([]any)
	if !ok {
		t.Fatal("opencode extension missing 'allowed_tools' list")
	}
	gotAllowedTools := make([]string, 0, len(allowedTools))
	for _, tool := range allowedTools {
		name, ok := tool.(string)
		if !ok {
			t.Fatalf("allowed tool type = %T, want string", tool)
		}
		gotAllowedTools = append(gotAllowedTools, name)
	}
	wantAllowedTools := []string{"read", "glob", "grep", "edit", "bash"}
	if !slices.Equal(gotAllowedTools, wantAllowedTools) {
		t.Errorf("opencode extension allowed_tools = %v, want %v", gotAllowedTools, wantAllowedTools)
	}

	agent, ok := wf.Config["agent"].(map[string]any)
	if !ok {
		t.Fatal("WORKFLOW.opencode.md config missing 'agent' map")
	}
	if kind, _ := agent["kind"].(string); kind != "opencode" {
		t.Errorf("agent.kind = %q, want %q", kind, "opencode")
	}
	if cmd, _ := agent["command"].(string); cmd != "opencode" {
		t.Errorf("agent.command = %q, want %q", cmd, "opencode")
	}

	tracker, ok := wf.Config["tracker"].(map[string]any)
	if !ok {
		t.Fatal("WORKFLOW.opencode.md config missing 'tracker' map")
	}
	if kind, _ := tracker["kind"].(string); kind != "jira" {
		t.Errorf("tracker.kind = %q, want %q", kind, "jira")
	}
}

func TestSampleWorkflowEnvVarIndirection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "examples", "WORKFLOW.md")
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("workflow.Load(WORKFLOW.md): %v", err)
	}

	tracker, ok := wf.Config["tracker"].(map[string]any)
	if !ok {
		t.Fatal("config missing tracker map")
	}

	// All operator-specific values must use $SORTIE_* indirection.
	checks := []struct {
		key  string
		want string
	}{
		{"endpoint", "$SORTIE_JIRA_ENDPOINT"},
		{"api_key", "$SORTIE_JIRA_API_KEY"},
		{"project", "$SORTIE_JIRA_PROJECT"},
	}
	for _, c := range checks {
		got, _ := tracker[c.key].(string)
		if got != c.want {
			t.Errorf("tracker.%s = %q, want %q", c.key, got, c.want)
		}
	}
}
