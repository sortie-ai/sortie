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

const originalStatusSuffix = `If you determine that you cannot make further progress on this task without human
intervention, or if your work is complete and requires human review, or if you
determine that the requested outcome already held and you changed nothing, signal
the orchestrator by running the following, replacing STATUS with exactly one of
the three values below:

    mkdir -p .sortie && echo "STATUS" > .sortie/status

Use "blocked" when you cannot proceed. Use "needs-human-review" when your work is
complete and awaiting review. Use "no-change-needed" when the requested outcome
already held before you started and you made no change to reach it. Do not write
"no-change-needed" if you performed any work. Do not write this file during normal
productive work.`

func TestRuntimeStatusSuffixContent(t *testing.T) {
	t.Parallel()

	for _, want := range []string{"blocked", "needs-human-review", "no-change-needed"} {
		if !strings.Contains(RuntimeStatusSuffix, want) {
			t.Errorf("RuntimeStatusSuffix missing substring %q:\n%s", want, RuntimeStatusSuffix)
		}
	}

	if !strings.HasPrefix(RuntimeStatusSuffix, originalStatusSuffix+"\n\n") {
		t.Errorf("RuntimeStatusSuffix does not start with the original two paragraphs followed by a blank line:\n%s", RuntimeStatusSuffix)
	}

	appended := strings.TrimPrefix(RuntimeStatusSuffix, originalStatusSuffix)
	for _, want := range []string{
		"Give your reason on the lines after the value",
		"path in the repository",
		"never include credentials or other secrets",
		"under 1024 bytes",
	} {
		if !strings.Contains(appended, want) {
			t.Errorf("appended paragraphs missing %q:\n%s", want, appended)
		}
	}

	var writeExamples []string
	for line := range strings.SplitSeq(RuntimeStatusSuffix, "\n") {
		if strings.Contains(line, ".sortie/status") {
			writeExamples = append(writeExamples, strings.TrimSpace(line))
		}
	}
	if len(writeExamples) != 2 {
		t.Fatalf("RuntimeStatusSuffix has %d write examples %q, want 2", len(writeExamples), writeExamples)
	}

	// An agent that copies the placeholder command verbatim would signal a
	// value its run did not reach, and a hardcoded "blocked" parks the issue.
	for _, v := range []string{"blocked", "needs-human-review", "no-change-needed"} {
		if strings.Contains(writeExamples[0], v) {
			t.Errorf("placeholder write example %q hardcodes recognized status %q", writeExamples[0], v)
		}
	}

	reasonExample := writeExamples[1]
	if !strings.Contains(reasonExample, `printf '%s\n' "blocked" "`) {
		t.Errorf("reason write example %q, want printf with the value and the reason as separate lines", reasonExample)
	}
}

func TestBuildTurnPrompt(t *testing.T) {
	t.Parallel()

	issue := map[string]any{
		"title": "Fix login bug",
		"state": "In Progress",
	}

	t.Run("FirstTurnFullPrompt", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "Fix login bug") {
			t.Errorf("BuildTurnPrompt() = %q, want substring %q", got, "Fix login bug")
		}
		if !strings.Contains(got, "cont=false") {
			t.Errorf("BuildTurnPrompt() = %q, want substring %q", got, "cont=false")
		}
		if got == DefaultContinuationPrompt {
			t.Errorf("BuildTurnPrompt() = DefaultContinuationPrompt, want author-defined prompt")
		}
	})

	t.Run("ContinuationTurnRendersTemplate", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse(branchingTemplate, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 2, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == DefaultContinuationPrompt {
			t.Errorf("BuildTurnPrompt() = DefaultContinuationPrompt, want author-defined continuation")
		}
		if !strings.Contains(got, "cont=true") {
			t.Errorf("BuildTurnPrompt() = %q, want substring %q", got, "cont=true")
		}
	})

	t.Run("ContinuationTurnShorter", func(t *testing.T) {
		t.Parallel()

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
			t.Errorf("BuildTurnPrompt() continuation len = %d, want < first turn len %d", len(cont), len(first))
		}
		if strings.Contains(cont, "Fix login bug") {
			t.Errorf("BuildTurnPrompt() continuation = %q, want no issue title", cont)
		}
	})

	t.Run("ContinuationFallbackOnEmptyOutput", func(t *testing.T) {
		t.Parallel()

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
			t.Errorf("BuildTurnPrompt() = %q, want DefaultContinuationPrompt", got)
		}
	})

	t.Run("ContinuationFallbackOnWhitespaceOnly", func(t *testing.T) {
		t.Parallel()

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
			t.Errorf("BuildTurnPrompt() = %q, want DefaultContinuationPrompt", got)
		}
	})

	t.Run("FirstTurnEmptyNoFallback", func(t *testing.T) {
		t.Parallel()

		// First-turn empty output must pass through as-is, NOT substitute
		// DefaultContinuationPrompt. The fallback is for continuation turns only.
		tmpl, err := Parse("", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == DefaultContinuationPrompt {
			t.Errorf("BuildTurnPrompt(turnNumber=1) = DefaultContinuationPrompt, want empty passthrough")
		}
	})

	t.Run("FirstTurnRenderError", func(t *testing.T) {
		t.Parallel()

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
		t.Parallel()

		// References a missing field unconditionally; errors on all turns.
		body := "{{ .issue.missing_field }}"
		tmpl, err := Parse(body, "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = BuildTurnPrompt(tmpl, map[string]any{}, nil, 2, 20)
		if err == nil {
			t.Fatal("expected render error on continuation turn, got nil")
		}
		if _, ok := errors.AsType[*TemplateError](err); !ok {
			t.Fatalf("expected *TemplateError, got %T: %v", err, err)
		}
	})

	t.Run("InvalidTurnNumber", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("hello", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		_, err = BuildTurnPrompt(tmpl, issue, nil, 0, 20)
		if err == nil {
			t.Fatal("expected error for turnNumber=0, got nil")
		}
		// Must NOT be a *TemplateError; this is a caller bug, not a template issue.
		if _, ok := errors.AsType[*TemplateError](err); ok {
			t.Errorf("BuildTurnPrompt(turnNumber=0) error type = %T, want plain error", err)
		}
	})

	t.Run("NilAttemptFirstRun", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .issue.title }} attempt={{ .attempt }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, nil, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "Fix login bug") {
			t.Errorf("BuildTurnPrompt() = %q, want substring %q", got, "Fix login bug")
		}
	})

	t.Run("RetryAttemptFirstTurn", func(t *testing.T) {
		t.Parallel()

		tmpl, err := Parse("{{ .issue.title }} attempt={{ .attempt }}", "WORKFLOW.md", 0)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		got, err := BuildTurnPrompt(tmpl, issue, 2, 1, 20)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "attempt=2") {
			t.Errorf("BuildTurnPrompt() = %q, want substring %q", got, "attempt=2")
		}
	})

	t.Run("ContinuationConsistentAcrossTurns", func(t *testing.T) {
		t.Parallel()

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
