package prompt

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// branchingTemplate emits the full issue title on first turns and a
// short continuation line on subsequent turns.
const branchingTemplate = `{{ if .run.is_continuation }}Continue turn {{ .run.turn_number }} cont=true{{ else }}Task: {{ .issue.title }} cont=false{{ end }}`

func TestBuildTurnPrompt(t *testing.T) {
	issue := map[string]any{
		"title": "Fix login bug",
		"state": "In Progress",
	}

	t.Run("FirstTurnFullPrompt", func(t *testing.T) {
		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "Fix login bug") {
			t.Errorf("expected issue title in output, got %q", got)
		}
		if !strings.Contains(got, "cont=false") {
			t.Errorf("expected cont=false in output, got %q", got)
		}
		if got == DefaultContinuationPrompt {
			t.Error("first turn must not return DefaultContinuationPrompt")
		}
	})

	t.Run("ContinuationTurnRendersTemplate", func(t *testing.T) {
		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 2, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == DefaultContinuationPrompt {
			t.Error("expected author-defined continuation, got DefaultContinuationPrompt")
		}
		if !strings.Contains(got, "cont=true") {
			t.Errorf("expected cont=true in output, got %q", got)
		}
	})

	t.Run("ContinuationTurnShorter", func(t *testing.T) {
		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		first, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("first turn: %v", err)
		}
		cont, err := BuildTurnPrompt(tmpl, issue, nil, 2, 20)
		if err != nil {
			t.Fatalf("continuation turn: %v", err)
		}
		if len(cont) >= len(first) {
			t.Errorf("continuation (%d bytes) should be shorter than first turn (%d bytes)", len(cont), len(first))
		}
		if strings.Contains(cont, "Fix login bug") {
			t.Error("continuation should not contain the issue title")
		}
	})

	t.Run("ContinuationFallbackOnEmptyOutput", func(t *testing.T) {
		// Template emits nothing when is_continuation is true (no else branch).
		body := `{{ if not .run.is_continuation }}Full task: {{ .issue.title }}{{ end }}`
		tmpl, err := Parse(body, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 2, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != DefaultContinuationPrompt {
			t.Errorf("expected DefaultContinuationPrompt, got %q", got)
		}
	})

	t.Run("ContinuationFallbackOnWhitespaceOnly", func(t *testing.T) {
		body := "{{ if .run.is_continuation }}  \n  {{ else }}Full task{{ end }}"
		tmpl, err := Parse(body, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 2, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != DefaultContinuationPrompt {
			t.Errorf("expected DefaultContinuationPrompt, got %q", got)
		}
	})

	t.Run("FirstTurnRenderError", func(t *testing.T) {
		body := "{{ .issue.nonexistent_field }}"
		tmpl, err := Parse(body, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = BuildTurnPrompt(tmpl, map[string]any{}, nil, 1, 20)
		if err == nil {
			t.Fatal("expected render error, got nil")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
		if te.Kind != ErrTemplateRender {
			t.Errorf("Kind = %d, want ErrTemplateRender (%d)", te.Kind, ErrTemplateRender)
		}
	})

	t.Run("ContinuationTurnRenderError", func(t *testing.T) {
		// References a missing field unconditionally — errors on all turns.
		body := "{{ .issue.missing_field }}"
		tmpl, err := Parse(body, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = BuildTurnPrompt(tmpl, map[string]any{}, nil, 2, 20)
		if err == nil {
			t.Fatal("expected render error on continuation turn, got nil")
		}
		var te *TemplateError
		if !errors.As(err, &te) {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
	})

	t.Run("InvalidTurnNumber", func(t *testing.T) {
		tmpl, err := Parse("hello", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = BuildTurnPrompt(tmpl, issue, nil, 0, 20)
		if err == nil {
			t.Fatal("expected error for turnNumber=0, got nil")
		}
		// Must NOT be a *TemplateError — this is a caller bug, not a template issue.
		var te *TemplateError
		if errors.As(err, &te) {
			t.Errorf("expected plain error, got *TemplateError: %v", err)
		}
	})

	t.Run("NilAttemptFirstRun", func(t *testing.T) {
		tmpl, err := Parse("{{ .issue.title }} attempt={{ .attempt }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "Fix login bug") {
			t.Errorf("expected issue title in output, got %q", got)
		}
	})

	t.Run("RetryAttemptFirstTurn", func(t *testing.T) {
		tmpl, err := Parse("{{ .issue.title }} attempt={{ .attempt }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, 2, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "attempt=2") {
			t.Errorf("expected attempt=2 in output, got %q", got)
		}
	})

	t.Run("ContinuationConsistentAcrossTurns", func(t *testing.T) {
		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		for _, turn := range []int{3, 4, 5} {
			got, err := BuildTurnPrompt(tmpl, issue, nil, turn, 20)
			if err != nil {
				t.Fatalf("turn %d: unexpected error: %v", turn, err)
			}
			want := fmt.Sprintf("Continue turn %d cont=true", turn)
			if got != want {
				t.Errorf("turn %d: got %q, want %q", turn, got, want)
			}
		}
	})
}
