package prompt

import (
	"fmt"
	"slices"
	"strings"
)

// continuationKey is a template variable that carries reaction data, with
// the prompt a reaction run receives when the operator's template has no
// branch for it.
type continuationKey struct {
	name    string // template variable carrying the reaction data
	builtin string // text/template source of the built-in prompt
}

// continuationKeys lists all template variable names reserved for reaction
// continuation context. Each key receives a nil default in
// [Template.Render] so templates using missingkey=error do not reject
// references to absent reaction types.
var continuationKeys = []continuationKey{
	{name: "ci_failure", builtin: builtinPrompt("fixing the failing CI checks", ciFailureFirstTurn)},
	{name: "review_comments", builtin: builtinPrompt("addressing the review comments", reviewCommentsFirstTurn)},
	{name: "bot_review_comments", builtin: builtinPrompt("addressing the review comments", botReviewCommentsFirstTurn)},
	{name: "merge_conflict", builtin: builtinPrompt("resolving the merge conflicts", mergeConflictFirstTurn)},
	{name: "label_review", builtin: builtinPrompt("reviewing the pull request", labelReviewFirstTurn)},
	{name: "label_fix", builtin: builtinPrompt("fixing the pull request", labelFixFirstTurn)},
}

// builtinTemplates holds the parsed built-in prompt of every continuation
// key. A blank or unparseable source panics at package initialization, so
// no binary runs with a key that lacks its prompt.
var builtinTemplates = func() map[string]*Template {
	templates := make(map[string]*Template, len(continuationKeys))
	for _, k := range continuationKeys {
		if strings.TrimSpace(k.builtin) == "" {
			panic(fmt.Sprintf("prompt: continuation key %q has a blank built-in prompt", k.name))
		}
		tmpl, err := Parse(k.builtin, "built-in "+k.name+" prompt", 0)
		if err != nil {
			panic(fmt.Sprintf("prompt: built-in prompt of continuation key %q does not parse: %v", k.name, err))
		}
		templates[k.name] = tmpl
	}
	return templates
}()

// builtinPrompt wraps the first-turn text of a reaction in the switch on
// .run.is_continuation. The continuation text sends the agent back to the
// first-turn instructions it already holds and adds no duty of its own, so
// it fits whether the first turn came from the operator's branch or from
// the built-in.
func builtinPrompt(task, firstTurn string) string {
	return "{{ if .run.is_continuation -}}\n" +
		"Continue with the instructions you received at the start of this session for " + task + ", and start no work they do not ask for.\n" +
		"{{- else -}}\n" +
		firstTurn + "\n" +
		"{{- end }}"
}

// First-turn texts that say "as described below" rely on the outcome
// instructions the worker appends to every first turn.
const (
	ciFailureFirstTurn = `## Fix the failing CI checks

The CI checks for {{ .issue.identifier }} ({{ .issue.title }}) fail on {{ .ci_failure.ref }}, with {{ .ci_failure.failing_count }} failing.
{{ range .ci_failure.check_runs }}
- {{ .name }}: {{ .conclusion }}{{ if .details_url }}, {{ .details_url }}{{ end }}
{{- end }}
{{ if .ci_failure.log_excerpt }}
Log of the first failing check:

{{ .ci_failure.log_excerpt }}
{{ end }}
Find the cause of each failure and fix it in this workspace. Run the failing checks locally where you can. Commit the fix and push it to the branch the checks ran on. Do not start other work. When you are done, signal the outcome as described below.`

	reviewCommentsFirstTurn = `## Address the review comments

Reviewers left these comments on the pull request for {{ .issue.identifier }} ({{ .issue.title }}). Address each one.
{{ range .review_comments }}
### {{ .reviewer }}{{ if .file }} on {{ .file }}{{ if .start_line }} (line {{ .start_line }}{{ if .end_line }}-{{ .end_line }}{{ end }}){{ end }}{{ end }}

{{ .body }}
{{ end }}
Make the change each comment asks for in this workspace. Commit the changes and push them to the pull request's branch. Do not start other work. When you are done, signal the outcome as described below.`

	botReviewCommentsFirstTurn = `## Address the review comments

Automated review tools left these comments on the pull request for {{ .issue.identifier }} ({{ .issue.title }}). Address each one.
{{ range .bot_review_comments }}
### {{ .reviewer }}{{ if .file }} on {{ .file }}{{ if .start_line }} (line {{ .start_line }}{{ if .end_line }}-{{ .end_line }}{{ end }}){{ end }}{{ end }}

{{ .body }}
{{ end }}
Make the change each comment asks for in this workspace. Commit the changes and push them to the pull request's branch. Do not start other work. When you are done, signal the outcome as described below.`

	mergeConflictFirstTurn = `## Resolve the merge conflicts

Pull request {{ .merge_conflict.pr_number }} for {{ .issue.identifier }} ({{ .merge_conflict.branch }}, head {{ .merge_conflict.head_sha }}) has merge conflicts with its base branch {{ .merge_conflict.base }}.

1. Fetch the latest {{ .merge_conflict.base }}.
2. Rebase {{ .merge_conflict.branch }} onto {{ .merge_conflict.base }}.
3. Resolve every conflict, keeping both the intent of the pull request and the changes on {{ .merge_conflict.base }}.
4. Push the rebased branch.
5. Signal the outcome as described below.

Do not start other work.`

	labelReviewFirstTurn = `## Review this pull request

{{ .label_review.actor }} asked for a code review of pull request {{ .label_review.pr_number }} in {{ .label_review.owner }}/{{ .label_review.repo }}.

1. Fetch the diff of the pull request with the tools available to you.
2. Review the changes for correctness, clarity, and regressions.
3. Post your review comments on the pull request.
4. Signal the outcome as described below.

This workspace holds no checkout of the repository. Do not modify the branch and do not push.`

	labelFixFirstTurn = `## Fix this pull request

{{ .label_fix.actor }} asked for fixes to pull request {{ .label_fix.pr_number }} in {{ .label_fix.owner }}/{{ .label_fix.repo }}.

1. Check out {{ .label_fix.branch }}.
2. Fetch the outstanding review comments on the pull request with the tools available to you.
3. Address the feedback and push the fixes to {{ .label_fix.branch }}.
4. Post a summary comment on the pull request.
5. Signal the outcome as described below.

Push to no other branch and do not start other work.`
)

// BuildReactionTurnPrompt returns the prompt for one turn of a reaction run.
// The operator's template wins when its render with the run's continuation
// context is not blank and differs from its render without it; otherwise the
// built-in prompt of the continuation key is rendered with the same data. A
// render that fails without the context counts as differing, because the
// template then reads the key unguarded.
//
// A continuation holding no registered key, or a turnNumber below 1, behaves
// as [BuildTurnPrompt]. opts apply to every render. Safe for concurrent use.
func BuildReactionTurnPrompt(tmpl *Template, issue map[string]any, attempt any, turnNumber, maxTurns int, continuation map[string]any, opts ...RenderOption) (string, error) {
	key, ok := activeContinuationKey(continuation)
	if !ok || turnNumber < 1 {
		return BuildTurnPrompt(tmpl, issue, attempt, turnNumber, maxTurns, opts...)
	}

	run := RunContext{TurnNumber: turnNumber, MaxTurns: maxTurns, IsContinuation: turnNumber > 1}
	seededOpts := append(slices.Clone(opts), WithContinuationContext(continuation))

	seeded, err := tmpl.Render(issue, attempt, run, seededOpts...)
	if err != nil {
		return "", err
	}
	unseeded, unseededErr := tmpl.Render(issue, attempt, run, opts...)
	if strings.TrimSpace(seeded) != "" && (unseededErr != nil || seeded != unseeded) {
		return seeded, nil
	}
	return builtinTemplates[key].Render(issue, attempt, run, seededOpts...)
}

// activeContinuationKey returns the first registered key that continuation
// maps to a non-nil value.
func activeContinuationKey(continuation map[string]any) (string, bool) {
	for _, k := range continuationKeys {
		if continuation[k.name] != nil {
			return k.name, true
		}
	}
	return "", false
}
