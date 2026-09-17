package main

import (
	"fmt"
	"strings"
	"testing"
)

// baseRenderContext returns a renderContext with realistic, non-zero
// values in every field a render function reads, so a test that
// overrides only the field it cares about does not accidentally rely
// on a zero value.
func baseRenderContext() renderContext {
	return renderContext{
		input: monitorInput{
			Adapter:     "acp-example",
			Kind:        "agent",
			Source:      "npm",
			Version:     "1.2.3",
			RunURL:      "https://example.invalid/runs/1002",
			Commit:      "deadbeefcafe",
			Now:         "2026-09-08 02:14 UTC",
			AdapterName: "Example",
		},
		classification: sampleClassification{
			Classification: "contract",
			failedTests:    []string{"TestFoo"},
			excerpt:        "FAIL\n",
		},
		streaks:          streakResult{failStreak: 2},
		failureThreshold: 2,
		passThreshold:    2,
	}
}

func TestRenderBody(t *testing.T) {
	t.Parallel()

	t.Run("none_action_renders_empty", func(t *testing.T) {
		t.Parallel()

		got := renderBody("none", verdictFailing, baseRenderContext())

		if got != "" {
			t.Errorf("renderBody(%q, ...) = %q, want empty", "none", got)
		}
	})

	t.Run("open_and_reopen_render_the_failing_body", func(t *testing.T) {
		t.Parallel()

		for _, action := range []string{"open", "reopen"} {
			ctx := baseRenderContext()

			got := renderBody(action, verdictFailing, ctx)

			if !strings.Contains(got, ctx.classification.Classification) {
				t.Errorf("renderBody(%q, ...) = %q, want it to name the classification %q", action, got, ctx.classification.Classification)
			}
			if !strings.Contains(got, "TestFoo") {
				t.Errorf("renderBody(%q, ...) = %q, want the failing test list", action, got)
			}
		}
	})

	t.Run("failing_body_names_consecutive_pass_policy", func(t *testing.T) {
		t.Parallel()

		got := renderBody("open", verdictFailing, baseRenderContext())

		if !strings.Contains(got, "required consecutive passing samples") {
			t.Errorf("renderBody(%q, ...) = %q, want consecutive-pass closure wording", "open", got)
		}
	})

	t.Run("comment_on_failing_sample_matches_open_body", func(t *testing.T) {
		t.Parallel()

		ctx := baseRenderContext()

		openBody := renderBody("open", verdictFailing, ctx)
		commentBody := renderBody("comment", verdictFailing, ctx)

		if openBody != commentBody {
			t.Errorf("renderBody(%q, failing, ...) = %q, want it to equal renderBody(%q, failing, ...) = %q", "comment", commentBody, "open", openBody)
		}
	})

	t.Run("passing_comment_below_threshold_names_streak_and_omits_recovery_language", func(t *testing.T) {
		t.Parallel()

		ctx := baseRenderContext()
		ctx.streaks = streakResult{passStreak: 1}
		ctx.passThreshold = 2

		got := renderBody("comment", verdictPassing, ctx)

		if strings.Contains(got, "Recovered") || strings.Contains(got, "Closing") {
			t.Errorf("renderBody(%q, passing, ...) = %q, want no %q or %q", "comment", got, "Recovered", "Closing")
		}
		if !strings.Contains(got, "1 consecutive passing sample of 2 required") {
			t.Errorf("renderBody(%q, passing, ...) = %q, want it to name the passing streak and threshold", "comment", got)
		}
	})

	t.Run("close_states_recovery_streak_and_run_url", func(t *testing.T) {
		t.Parallel()

		ctx := baseRenderContext()
		ctx.streaks = streakResult{passStreak: 2}
		ctx.passThreshold = 2

		got := renderBody("close", verdictPassing, ctx)

		if !strings.Contains(got, "Recovered") || !strings.Contains(got, "Closing") {
			t.Errorf("renderBody(%q, ...) = %q, want it to state the recovery and the closing", "close", got)
		}
		if !strings.Contains(got, ctx.input.RunURL) {
			t.Errorf("renderBody(%q, ...) = %q, want the run URL %q", "close", got, ctx.input.RunURL)
		}
	})

	t.Run("close_body_unreachable_without_history_read", func(t *testing.T) {
		t.Parallel()

		streaks := streakResult{passStreak: 5}
		decision := decideAction("open", 42, verdictPassing, streaks, 2, 2, false, true)
		if decision.Action == "close" {
			t.Fatalf("decideAction(..., historyRead=false) chose %q, want it never to choose %q", decision.Action, "close")
		}

		ctx := baseRenderContext()
		ctx.streaks = streaks

		got := renderBody(decision.Action, verdictPassing, ctx)

		if strings.Contains(got, "Recovered") || strings.Contains(got, "Closing") {
			t.Errorf("renderBody(%q, passing, ...) = %q, want no %q or %q when history could not be read", decision.Action, got, "Recovered", "Closing")
		}
	})

	t.Run("evidence_lists_every_counted_run_url_and_states_no_shared_cause", func(t *testing.T) {
		t.Parallel()

		ctx := baseRenderContext()
		ctx.streaks = streakResult{
			failStreak: 3,
			counted: []historySample{
				{RunURL: "https://example.invalid/runs/1001"},
				{RunURL: "https://example.invalid/runs/1000"},
			},
		}

		got := renderBody("open", verdictFailing, ctx)

		for _, url := range []string{
			ctx.input.RunURL,
			"https://example.invalid/runs/1001",
			"https://example.invalid/runs/1000",
		} {
			if !strings.Contains(got, url) {
				t.Errorf("renderBody(%q, ...) = %q, want it to list the counted run URL %q", "open", got, url)
			}
		}
		if !strings.Contains(got, "carries no classification") {
			t.Errorf("renderBody(%q, ...) = %q, want it to state that a prior sample's verdict carries no classification", "open", got)
		}
	})
}

func TestRenderSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		action         string
		incidentNumber int
	}{
		{"none_action", "none", 0},
		{"open_action", "open", 501},
		{"reopen_action", "reopen", 501},
		{"comment_action", "comment", 501},
		{"close_action", "close", 501},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := baseRenderContext()
			ctx.streaks = streakResult{failStreak: 2, passStreak: 0}

			got := renderSummary(ctx, tt.action, tt.incidentNumber)

			wantIncident := "none"
			if tt.incidentNumber != 0 {
				wantIncident = fmt.Sprintf("#%d", tt.incidentNumber)
			}
			for _, want := range []string{
				ctx.input.AdapterName,
				ctx.classification.Classification,
				tt.action,
				fmt.Sprintf("%d of %d required", ctx.streaks.failStreak, ctx.failureThreshold),
				fmt.Sprintf("%d of %d required", ctx.streaks.passStreak, ctx.passThreshold),
				wantIncident,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("renderSummary(..., %q, %d) = %q, want it to contain %q", tt.action, tt.incidentNumber, got, want)
				}
			}
		})
	}
}
