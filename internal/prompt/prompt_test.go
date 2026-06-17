package prompt

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()

	t.Run("ValidTemplate", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("Hello {{ .issue.title }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tmpl == nil {
			t.Fatal("expected non-nil *Template")
		}
	})

	t.Run("EmptyBody", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tmpl == nil {
			t.Fatal("expected non-nil *Template")
		}
	})

	t.Run("InvalidSyntax", func(t *testing.T) {
		t.Parallel()

		_, err := Parse("{{ .issue.title", "WORKFLOW.md", 0)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		if te.Kind != ErrTemplateParse {
			t.Errorf("Kind = %d, want ErrTemplateParse (%d)", te.Kind, ErrTemplateParse)
		}
	})

	t.Run("ParseErrorLineOffset", func(t *testing.T) {
		t.Parallel()

		// Put the error on line 3 of the template body.
		body := "line1\nline2\n{{ .issue.title"
		_, err := Parse(body, "WORKFLOW.md", 10)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		// Template line 3 + front matter 10 = WORKFLOW.md line 13.
		if te.Line != 13 {
			t.Errorf("Line = %d, want 13", te.Line)
		}
		if te.Source != "WORKFLOW.md" {
			t.Errorf("Source = %q, want %q", te.Source, "WORKFLOW.md")
		}
	})

	t.Run("UnknownFunction", func(t *testing.T) {
		t.Parallel()

		_, err := Parse("{{ .issue.title | nonexistent }}", "WORKFLOW.md", 0)
		if err == nil {
			t.Fatal("expected error for unknown function")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		if te.Kind != ErrTemplateParse {
			t.Errorf("Kind = %d, want ErrTemplateParse", te.Kind)
		}
	})
}

func TestRender(t *testing.T) {
	t.Parallel()

	sampleIssue := map[string]any{
		"id":         "12345",
		"identifier": "PROJ-42",
		"title":      "Fix the login bug",
		"state":      "In Progress",
		"labels":     []string{"bug", "urgent"},
		"blocked_by": []map[string]any{
			{"identifier": "PROJ-40", "state": "Done"},
			{"identifier": "PROJ-41", "state": "In Progress"},
		},
		"parent":   map[string]any{"identifier": "PROJ-10"},
		"assignee": "alice",
	}

	t.Run("AllVariables", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"Issue: {{ .issue.title }} Attempt: {{ .attempt }} Turn: {{ .run.turn_number }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, 2, RunContext{TurnNumber: 3, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		want := "Issue: Fix the login bug Attempt: 2 Turn: 3"
		if got != want {
			t.Errorf("Render() = %q, want %q", got, want)
		}
	})

	t.Run("AttemptNil_FirstRun", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"{{ if .attempt }}retry #{{ .attempt }}{{ else }}first run{{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, nil, RunContext{TurnNumber: 1, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "first run" {
			t.Errorf("Render() = %q, want %q", got, "first run")
		}
	})

	t.Run("AttemptInteger_Retry", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"{{ if .attempt }}retry #{{ .attempt }}{{ else }}first run{{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, 3, RunContext{TurnNumber: 1, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "retry #3" {
			t.Errorf("Render() = %q, want %q", got, "retry #3")
		}
	})

	t.Run("RunFields", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"turn={{ .run.turn_number }} max={{ .run.max_turns }} cont={{ .run.is_continuation }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, nil, RunContext{
			TurnNumber:     5,
			MaxTurns:       20,
			IsContinuation: true,
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		want := "turn=5 max=20 cont=true"
		if got != want {
			t.Errorf("Render() = %q, want %q", got, want)
		}
	})

	t.Run("NestedLabels", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"{{ range .issue.labels }}[{{ . }}]{{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, nil, RunContext{TurnNumber: 1, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "[bug][urgent]" {
			t.Errorf("Render() = %q, want %q", got, "[bug][urgent]")
		}
	})

	t.Run("NestedBlockedBy", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"{{ range .issue.blocked_by }}{{ .identifier }} {{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, nil, RunContext{TurnNumber: 1, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "PROJ-40 PROJ-41 " {
			t.Errorf("Render() = %q, want %q", got, "PROJ-40 PROJ-41 ")
		}
	})

	t.Run("ParentAccess", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			"{{ if .issue.parent }}parent={{ .issue.parent.identifier }}{{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(sampleIssue, nil, RunContext{TurnNumber: 1, MaxTurns: 20})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "parent=PROJ-10" {
			t.Errorf("Render() = %q, want %q", got, "parent=PROJ-10")
		}
	})
}

func TestRender_FuncMap(t *testing.T) {
	t.Parallel()

	issue := map[string]any{
		"title":  "Test Issue",
		"state":  "In Progress",
		"labels": []string{"bug", "urgent"},
	}
	run := RunContext{TurnNumber: 1, MaxTurns: 20}

	t.Run("toJSON", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .issue.labels | toJSON }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(issue, nil, run)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != `["bug","urgent"]` {
			t.Errorf("Render() = %q, want %q", got, `["bug","urgent"]`)
		}
	})

	t.Run("join", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(`{{ join ", " .issue.labels }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(issue, nil, run)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "bug, urgent" {
			t.Errorf("Render() = %q, want %q", got, "bug, urgent")
		}
	})

	t.Run("join_AnySlice", func(t *testing.T) {
		t.Parallel()

		// YAML decoder produces []any, not []string. Verify join handles it.
		issueAny := map[string]any{
			"labels": []any{"bug", "urgent"},
		}
		tmpl, err := Parse(`{{ join ", " .issue.labels }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(issueAny, nil, run)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "bug, urgent" {
			t.Errorf("Render() = %q, want %q", got, "bug, urgent")
		}
	})

	t.Run("lower", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .issue.state | lower }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(issue, nil, run)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != "in progress" {
			t.Errorf("Render() = %q, want %q", got, "in progress")
		}
	})
}

func TestRender_Errors(t *testing.T) {
	t.Parallel()

	issue := map[string]any{
		"title": "Test Issue",
	}
	run := RunContext{TurnNumber: 1, MaxTurns: 20}

	t.Run("DollarRootAccess", func(t *testing.T) {
		t.Parallel()

		// Inside {{ range }}, $ accesses the root data map.
		issueWithLabels := map[string]any{
			"title":  "Test Issue",
			"labels": []any{"bug", "urgent"},
		}
		tmpl, err := Parse(
			"{{ range .issue.labels }}{{ . }}: {{ $.issue.title }}; {{ end }}",
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(issueWithLabels, nil, run)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		want := "bug: Test Issue; urgent: Test Issue; "
		if got != want {
			t.Errorf("Render() = %q, want %q", got, want)
		}
	})

	t.Run("EmptyIssueMap", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .issue.title }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, err = tmpl.Render(map[string]any{}, nil, run)
		if err == nil {
			t.Fatal("expected error for missing key in empty issue map")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		if te.Kind != ErrTemplateRender {
			t.Errorf("Kind = %d, want ErrTemplateRender (%d)", te.Kind, ErrTemplateRender)
		}
	})

	t.Run("UnknownTopLevelKey", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .config }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, err = tmpl.Render(issue, nil, run)
		if err == nil {
			t.Fatal("expected error for unknown key .config")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		if te.Kind != ErrTemplateRender {
			t.Errorf("Kind = %d, want ErrTemplateRender (%d)", te.Kind, ErrTemplateRender)
		}
	})

	t.Run("RenderErrorLineOffset", func(t *testing.T) {
		t.Parallel()

		// Error on template line 2, with 7 front matter lines.
		body := "line 1\n{{ .nonexistent }}"
		tmpl, err := Parse(body, "WORKFLOW.md", 7)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, err = tmpl.Render(issue, nil, run)
		if err == nil {
			t.Fatal("expected error for .nonexistent")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		// Template line 2 + front matter 7 = WORKFLOW.md line 9.
		if te.Line != 9 {
			t.Errorf("Line = %d, want 9", te.Line)
		}
	})

	t.Run("ErrorMessageContainsSource", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .config }}", "my/WORKFLOW.md", 5)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		_, err = tmpl.Render(issue, nil, run)
		if err == nil {
			t.Fatal("expected render error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "my/WORKFLOW.md") {
			t.Errorf("error message missing source path\n  got: %q", msg)
		}
		if !strings.Contains(msg, "render") {
			t.Errorf("error message missing error kind\n  got: %q", msg)
		}
	})
}

func TestTemplateError_Unwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel")
	te := &TemplateError{Kind: ErrTemplateParse, Source: "x.md", Err: sentinel}
	if !errors.Is(te, sentinel) {
		t.Errorf("errors.Is(TemplateError, sentinel) = false, want true")
	}
}

func TestRender_Concurrent(t *testing.T) {
	t.Parallel()

	tmpl, err := Parse(
		"{{ .issue.title }} turn={{ .run.turn_number }}",
		"WORKFLOW.md", 0,
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			issue := map[string]any{"title": "concurrent test"}
			_, renderErr := tmpl.Render(issue, nil, RunContext{
				TurnNumber: i + 1,
				MaxTurns:   50,
			})
			if renderErr != nil {
				t.Errorf("concurrent render %d failed: %v", i, renderErr)
			}
		})
	}
	wg.Wait()
}

func TestRender_BotReviewComments(t *testing.T) {
	t.Parallel()

	issue := map[string]any{"title": "Fix linter findings"}
	run := RunContext{TurnNumber: 2, MaxTurns: 5, IsContinuation: true}

	botComments := []map[string]any{
		{
			"id":         "501",
			"file":       "internal/handler.go",
			"start_line": 20,
			"end_line":   22,
			"reviewer":   "golangci-lint[bot]",
			"body":       "Error return value of `doWork` is not checked.",
		},
	}

	t.Run("BotReviewComments_RendersWithoutMissingKeyError", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(`{{ if .bot_review_comments }}has-bot-comments{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run,
			WithContinuationContext(map[string]any{"bot_review_comments": botComments}),
		)
		if err != nil {
			t.Fatalf("Render with bot_review_comments: %v", err)
		}
		if got != "has-bot-comments" {
			t.Errorf("Render() = %q, want %q", got, "has-bot-comments")
		}
	})

	t.Run("BotReviewComments_DefaultNilNoError", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(`{{ if .bot_review_comments }}FAIL{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run)
		if err != nil {
			t.Fatalf("Render with default bot_review_comments=nil: %v", err)
		}
		if got != "" {
			t.Errorf("Render() = %q, want empty string when bot_review_comments is nil", got)
		}
	})

	t.Run("BotReviewComments_PerCommentShapeMatchesReviewComments", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			`{{ range .bot_review_comments }}{{ index . "id" }}:{{ index . "file" }}:{{ index . "start_line" }}:{{ index . "end_line" }}:{{ index . "reviewer" }}:{{ index . "body" }}{{ end }}`,
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run,
			WithContinuationContext(map[string]any{"bot_review_comments": botComments}),
		)
		if err != nil {
			t.Fatalf("Render with bot_review_comments shape: %v", err)
		}
		want := "501:internal/handler.go:20:22:golangci-lint[bot]:Error return value of `doWork` is not checked."
		if got != want {
			t.Errorf("Render() = %q, want %q", got, want)
		}
	})

	t.Run("WithContinuationContext_AcceptsBotReviewCommentsKey", func(t *testing.T) {
		t.Parallel()

		// WithContinuationContext must not panic when passed bot_review_comments.
		opt := WithContinuationContext(map[string]any{"bot_review_comments": botComments})
		if opt == nil {
			t.Error("WithContinuationContext returned nil for registered key bot_review_comments")
		}
	})
}

func TestWithContinuationContext_UnregisteredKeyPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("WithContinuationContext with unregistered key did not panic; want panic")
		}
	}()

	WithContinuationContext(map[string]any{"unregistered_key": "value"})
}

func TestRender_WithContinuationContext(t *testing.T) {
	t.Parallel()

	t.Run("DefaultNil_NoError", func(t *testing.T) {
		t.Parallel()
		// Template uses ci_failure in a conditional; default nil must not trigger
		// missingkey=error and must evaluate the block as falsy.
		tmpl, err := Parse(`{{ if .ci_failure }}FAIL{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(map[string]any{"title": "t"}, nil, RunContext{TurnNumber: 1, MaxTurns: 5})
		if err != nil {
			t.Fatalf("render with default ci_failure=nil: %v", err)
		}
		if got != "" {
			t.Errorf("rendered %q, want empty string when ci_failure is nil", got)
		}
	})

	t.Run("WithContinuationContext_nil_Falsy", func(t *testing.T) {
		t.Parallel()
		tmpl, err := Parse(`{{ if .ci_failure }}FAIL{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(
			map[string]any{"title": "t"}, nil,
			RunContext{TurnNumber: 1, MaxTurns: 5},
			WithContinuationContext(map[string]any{"ci_failure": nil}),
		)
		if err != nil {
			t.Fatalf("render with WithContinuationContext(ci_failure=nil): %v", err)
		}
		if got != "" {
			t.Errorf("rendered %q, want empty string when WithContinuationContext(ci_failure=nil)", got)
		}
	})

	t.Run("WithContinuationContext_map_Truthy", func(t *testing.T) {
		t.Parallel()
		tmpl, err := Parse(
			`{{ if .ci_failure }}{{ index .ci_failure "status" }}{{ end }}`,
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := tmpl.Render(
			map[string]any{"title": "t"}, nil,
			RunContext{TurnNumber: 1, MaxTurns: 5},
			WithContinuationContext(map[string]any{"ci_failure": map[string]any{"status": "failing"}}),
		)
		if err != nil {
			t.Fatalf("render with ci_failure map: %v", err)
		}
		if got != "failing" {
			t.Errorf("rendered %q, want %q", got, "failing")
		}
	})

	t.Run("BuildTurnPrompt_CIFailureForwarded", func(t *testing.T) {
		t.Parallel()
		tmpl, err := Parse(
			`{{ if .ci_failure }}ci={{ index .ci_failure "count" }}{{ end }}`,
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := BuildTurnPrompt(
			tmpl,
			map[string]any{"title": "t"}, nil, 1, 5,
			WithContinuationContext(map[string]any{"ci_failure": map[string]any{"count": 3}}),
		)
		if err != nil {
			t.Fatalf("BuildTurnPrompt: %v", err)
		}
		if !strings.Contains(got, "ci=3") {
			t.Errorf("BuildTurnPrompt output %q missing %q", got, "ci=3")
		}
	})
}

func TestRender_MergeConflict(t *testing.T) {
	t.Parallel()

	issue := map[string]any{"title": "Resolve conflicts"}
	run := RunContext{TurnNumber: 2, MaxTurns: 5, IsContinuation: true}

	mergeConflict := map[string]any{
		"pr_number": 142,
		"branch":    "feature/x",
		"head_sha":  "abc123",
		"base":      "release/2.0",
	}

	t.Run("MergeConflict_RendersWithoutMissingKeyError", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(`{{ if .merge_conflict }}has-conflict{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run,
			WithContinuationContext(map[string]any{"merge_conflict": mergeConflict}),
		)
		if err != nil {
			t.Fatalf("Render with merge_conflict: %v", err)
		}
		if got != "has-conflict" {
			t.Errorf("Render() = %q, want %q", got, "has-conflict")
		}
	})

	t.Run("MergeConflict_DefaultNilNoError", func(t *testing.T) {
		t.Parallel()

		// The key is seeded to nil by Render, so a template referencing it
		// under missingkey=error must not error and must evaluate as falsy.
		tmpl, err := Parse(`{{ if .merge_conflict }}FAIL{{ end }}`, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run)
		if err != nil {
			t.Fatalf("Render with default merge_conflict=nil: %v", err)
		}
		if got != "" {
			t.Errorf("Render() = %q, want empty string when merge_conflict is nil", got)
		}
	})

	t.Run("MergeConflict_FieldsRenderIncludingRealBase", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(
			`{{ with .merge_conflict }}{{ .pr_number }}:{{ .branch }}:{{ .head_sha }}:{{ .base }}{{ end }}`,
			"WORKFLOW.md", 0,
		)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := tmpl.Render(issue, nil, run,
			WithContinuationContext(map[string]any{"merge_conflict": mergeConflict}),
		)
		if err != nil {
			t.Fatalf("Render with merge_conflict fields: %v", err)
		}
		want := "142:feature/x:abc123:release/2.0"
		if got != want {
			t.Errorf("Render() = %q, want %q (real base, not a hardcoded default)", got, want)
		}
	})

	t.Run("WithContinuationContext_AcceptsMergeConflictKey", func(t *testing.T) {
		t.Parallel()

		// WithContinuationContext must not panic for the registered key.
		opt := WithContinuationContext(map[string]any{"merge_conflict": mergeConflict})
		if opt == nil {
			t.Error("WithContinuationContext returned nil for registered key merge_conflict")
		}
	})
}
