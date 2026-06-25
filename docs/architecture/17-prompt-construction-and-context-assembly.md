## 12. Prompt Construction and Context Assembly

### 12.1 Inputs

Inputs to prompt rendering:

- `workflow.prompt_template`
- normalized `issue` object
- optional `attempt` integer (retry/continuation metadata)
- `run` object: `turn_number`, `max_turns`, `is_continuation`
- `ci_failure` (map or nil): CI failure context injected into CI-fix continuation prompts via
  `CIResult.ToTemplateMap()`. Nil on initial dispatch and non-CI retries. When non-nil, contains:
  - `status`: aggregate CI pipeline status string (`failing`)
  - `check_runs`: list of individual check run maps (each with `name`, `status`, `conclusion`,
    `details_url`)
  - `log_excerpt`: truncated log from the first failing check (empty string when unavailable)
  - `failing_count`: number of check runs with a failure conclusion
  - `ref`: the git ref that was queried

CI failure context is injected only on turn 1 of a CI-fix dispatch. The worker reads the context
from the dispatch site (carried via `context.WithValue` or the retry entry's `ContinuationContext`
field) and passes it to `prompt.WithContinuationContext`. Templates SHOULD use a conditional guard:
`{{ if .ci_failure }}...{{ end }}`. When `ci_failure` is nil, the template variable is still
present in the data map (set to nil) so strict `missingkey=error` evaluation does not reject
templates that reference the field.

- `review_comments` (list of maps or nil): review comment context injected into review-fix
  continuation prompts. Nil on initial dispatch and non-review retries. When non-nil, each
  element contains:
  - `id`: SCM-platform comment identifier
  - `file`: file path the comment is attached to (empty for PR-level comments)
  - `start_line`: first line of commented range (0 for non-inline)
  - `end_line`: last line of commented range (0 for single-line or non-inline)
  - `reviewer`: username of the comment author
  - `body`: comment text

Review comment context is injected only on turn 1 of a review-fix dispatch, following the same
`ContinuationContext` pathway as CI failure context. Templates SHOULD use a conditional guard:
`{{ if .review_comments }}...{{ end }}`. When `review_comments` is nil, the template variable is
still present in the data map (set to nil) so strict `missingkey=error` evaluation does not
reject templates that reference the field.

### 12.2 Rendering Rules

- Render with strict variable checking.
- Render with strict filter checking.
- Convert issue object keys to strings for template compatibility.
- Preserve nested arrays/maps (labels, blockers) so templates can iterate.

### 12.3 Retry/Continuation Semantics

`attempt` and `run` should be passed to the template because the workflow prompt may provide
different instructions for:

- first run (`attempt` null or absent)
- continuation turn within an active multi-turn session (`run.is_continuation == true`)
- retry after error/timeout/stall (`attempt >= 1`, `run.is_continuation == false`)

### 12.4 Failure Semantics

If prompt rendering fails:

- Fail the run attempt immediately.
- Let the orchestrator treat it like any other worker failure and decide retry behavior.

