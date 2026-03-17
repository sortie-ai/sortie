---
status: proposed
date: 2026-03-17
decision-makers: Serghei Iakovlev
---

# Use Go text/template for Prompt Rendering

## Context and Problem Statement

Sortie renders per-issue prompts by combining a workflow-defined template (the Markdown body
of `WORKFLOW.md`) with runtime data: the normalized issue object, the retry/continuation
attempt counter, and the run context (turn number, max turns, continuation flag). The
rendered prompt is delivered to the coding agent on each turn.

The template engine is **user-facing API surface**. Workflow authors — experienced engineers
who understand agent orchestration, not necessarily Go developers — write and maintain these
templates.
Once workflows exist in production repositories, changing the template engine breaks every
existing workflow. This decision is therefore effectively permanent and must be evaluated
with the weight of a public API contract.

The engine must satisfy the rendering requirements defined in architecture Sections 5.4 and
12.2: strict variable checking (unknown variables fail rendering), strict filter/pipeline
checking (unknown functions fail rendering), iteration over nested collections (labels,
blockers), and conditional branching on `attempt` nullability and `run.is_continuation` to
support the three prompt modes described in Section 12.3 (first run, continuation turn,
error retry).

## Decision Drivers

1. **Strict failure semantics.** Unknown variables and unknown functions must fail rendering,
   not silently produce empty strings. This is a hard requirement from architecture
   Section 5.4: silent failures cause agents to receive malformed prompts that are difficult
   to diagnose. The engine must support this mode natively or through reliable configuration.
2. **Conditional branching.** Workflow authors must distinguish first run (`attempt` absent),
   continuation turn (`run.is_continuation == true`), and error retry (`attempt >= 1`,
   `run.is_continuation == false`) within a single template (Section 12.3). The engine must
   support conditionals, not just variable interpolation.
3. **Collection iteration.** Templates must iterate over `issue.labels` (list of strings)
   and `issue.blockers` (list of objects) to include structured context in prompts
   (Section 12.2). The engine must support range/loop constructs over nested data.
4. **Dependency posture.** The orchestrator targets single-binary, zero-runtime-dependency
   deployment (ADR-0001). Build-time dependencies are acceptable but carry maintenance cost.
   A stdlib solution avoids third-party version churn and supply-chain risk.
5. **Agent generation quality.** AI coding agents write and maintain workflow templates.
   The engine syntax must be well-represented in LLM training data to produce correct
   templates with minimal iteration. Obscure or niche syntaxes increase the cost of
   agent-assisted workflow authoring.
6. **Error message clarity.** When a workflow author makes a template mistake — misspelled
   variable, unclosed action, wrong pipeline — the error message must point to the problem
   clearly enough for a non-Go developer to fix it without reading engine source code.

## Considered Options

- Go `text/template` with `missingkey=error`
- Pongo2 (Jinja2-compatible for Go)
- Handlebars via Go library
- Simple string interpolation

## Decision Outcome

Chosen option: **Go `text/template` with `Option("missingkey=error")`**, because it is the
only option that satisfies all six decision drivers — strict failure semantics, conditional
branching, collection iteration, zero external dependencies, strong LLM training coverage,
and long-term API stability — without introducing trade-offs that compromise the
architecture's requirements.

**Configuration:**

```go
tmpl, err := template.New("prompt").
    Option("missingkey=error").
    Parse(workflowPromptBody)
```

The `missingkey=error` option causes `Execute` to return an error when a template references
a key that does not exist in the data map. This directly satisfies the strict variable
checking requirement from Section 5.4. Combined with Go's default behavior of returning an
error for undefined functions in pipelines, both strictness requirements are met without
custom wrappers.

**Template data contract:**

The data map passed to `Execute` contains exactly three top-level keys:

- `issue` — the full normalized issue object (Section 4.1.1), with nested maps for
  structured fields (labels, blockers, parent). Map keys are strings; values preserve their
  Go types.
- `attempt` — `nil` on first run, integer `>= 1` on retry or continuation. Workflow authors
  use `{{if .attempt}}` to test presence, not `{{if eq .attempt nil}}`.
- `run` — a struct or map with three fields: `turn_number` (int), `max_turns` (int),
  `is_continuation` (bool).

No additional keys are injected. Unknown top-level keys referenced in templates (e.g.,
`{{.config}}`) fail rendering due to `missingkey=error`.

**Template lifecycle:**

1. **Parse once** on workflow load (and re-parse on dynamic reload per Section 6.2).
   Parse errors surface as `template_parse_error` and block dispatch.
2. **Execute per issue** on each turn. Execution errors surface as `template_render_error`
   and fail only the affected run attempt (Section 5.5).

**FuncMap policy:**

The initial implementation ships with a **minimal, prompt-essential FuncMap** in addition to
the built-in actions (`if`, `else`, `range`, `with`, `and`, `or`, `not`, `eq`, `ne`, `lt`,
`le`, `gt`, `ge`, `len`, `index`, `print`, `printf`, `println`, `call`):

| Function | Signature | Purpose |
| -------- | --------- | ------- |
| `toJSON`  | `toJSON value -> string` | Serialize any value to a compact JSON string. Agents parse structured data more reliably from JSON than from Go's default `fmt` representation. Without this, workflow authors must manually `{{ range }}` over every nested structure to produce agent-readable output. |
| `join`   | `join sep list -> string` | Join a list of strings with a separator. Common for rendering `issue.labels` as a comma-separated inline list instead of a verbose `{{ range }}` loop. |
| `lower`  | `lower string -> string` | Lowercase a string. Useful for normalizing labels and states in prompt text. |

Each addition to `FuncMap` extends the permanent API surface and must be treated as a
compatibility commitment. Functions beyond this initial set are added only when workflow
authors demonstrate a concrete need that cannot be met with the existing vocabulary.

### Considered Options in Detail

**Pongo2 (Jinja2-compatible for Go).** Implements Django/Jinja2 template syntax:
`{{ issue.title }}` (no dot prefix), `{% if attempt %}...{% endif %}`,
`{{ issue.title|upper }}`, `{% for label in issue.labels %}`. The syntax is arguably more
readable for non-Go developers, and the filter ecosystem (`|default:"N/A"`, `|join:", "`,
`|truncatechars:100`) covers common prompt formatting needs out of the box. Jinja2 is the
dominant template syntax in the DevOps ecosystem (Ansible playbooks, SaltStack states,
Cookiecutter project templates), so engineers in the target audience are likely familiar
with it.

However, pongo2 introduces a significant external dependency (`github.com/flosch/pongo2/v6`)
that must be version-tracked across Go releases. The library's maintenance cadence is
irregular — periods of active development followed by dormancy — which creates risk for a
long-lived project that treats the template engine as permanent API surface. Jinja2's
`{% %}` block syntax can visually conflict with Markdown code fences and Go template
examples embedded in prompts, requiring awareness of escaping rules. The rich filter
ecosystem is a double-edged sword: it expands the API surface that must remain stable,
and filters like `|safe` or `|escape` carry HTML-specific semantics that are meaningless in
a prompt rendering context. Pongo2's strict mode (`nil` variable behavior) differs subtly
from `text/template`'s `missingkey=error`: pongo2 raises on undefined variables by default
but allows `|default` to suppress the error, which can mask genuine template mistakes if
workflow authors cargo-cult `|default` onto every variable.

LLM generation quality for Jinja2 syntax is high — comparable to Go templates — due to
extensive Ansible and Django representation in training data. This advantage is real but
does not overcome the dependency and maintenance concerns, especially since Go template
syntax is also well-represented.

**Handlebars via Go library (`aymerick/raymond`).** Implements Mustache-compatible syntax:
`{{issue.title}}`, `{{#if attempt}}...{{/if}}`, `{{#each issue.labels}}...{{/each}}`.
The syntax is clean in Markdown contexts and the "logic-less" philosophy constrains
template complexity, which could be desirable for prompt templates that should remain
readable.

However, Handlebars' logic-less philosophy is fundamentally incompatible with the
architecture's requirements. Section 12.3 requires templates to branch on three distinct
conditions: first run vs. continuation turn vs. error retry. Handlebars supports `{{#if}}`
for truthiness checks but not comparison operators (`eq`, `ne`, `gt`), which means
`{{#if (eq run.is_continuation true)}}` requires registering custom helpers. The resulting
templates become more verbose and less readable than the equivalent Go template or Jinja2
conditionals. The Go Handlebars ecosystem is also thin: `aymerick/raymond` is the primary
library, and it has seen minimal maintenance since 2020. Handlebars syntax is
well-represented in LLM training data (JavaScript ecosystem), but the Go library's API
differences from the JavaScript reference implementation mean that LLM-generated templates
may use features that `raymond` does not support.

**Simple string interpolation.** Replace `${issue.title}` or `{issue.title}` markers with
values from the data map. Implementation is trivial: a regex or `strings.Replacer` pass,
under 30 lines of code, zero dependencies, zero learning curve.

However, string interpolation cannot satisfy the architecture's requirements. There is no
conditional syntax, so the three prompt modes from Section 12.3 cannot be expressed in a
single template — the implementation would need to maintain separate template strings for
each mode or move branching logic into Go code, violating the principle that workflow
authors control prompt policy. There is no iteration syntax, so `issue.labels` and
`issue.blockers` cannot be enumerated in prompts. There is no composition or nesting, so
prompt sections cannot be conditionally included or excluded. The simplicity that makes
interpolation attractive also makes it categorically insufficient for this use case. A
project that starts with interpolation will inevitably need to migrate to a real template
engine, and that migration breaks every existing workflow — the exact scenario the decision
drivers are designed to prevent.

## Consequences

### Positive

- **Zero external dependencies.** `text/template` is part of the Go standard library. No
  version pinning, no supply-chain audit, no risk of upstream abandonment. The engine will
  be maintained as long as Go itself exists.
- **Native strict mode.** `Option("missingkey=error")` satisfies the strictness requirement
  without custom wrappers, interceptors, or post-processing validation. The semantics are
  documented in the Go standard library and will not change across Go releases.
- **Parse/Execute separation.** Templates are compiled once and executed per issue, which
  allows parse errors to be caught at workflow load time (blocking dispatch) while execution
  errors are scoped to individual run attempts. This maps directly to the error class
  distinction in Section 5.5 (`template_parse_error` vs `template_render_error`).
- **Extensible without breakage.** Custom functions can be added to `FuncMap` incrementally.
  Each addition expands the template vocabulary without changing existing syntax. The
  initial set (`toJSON`, `join`, `lower`) covers the most common prompt authoring needs;
  future functions can be added as backward-compatible extensions without affecting existing
  workflows.
- **Stable LLM generation.** Go template syntax (`{{ .field }}`, `{{ if }}`, `{{ range }}`)
  is extensively represented in LLM training data from Go documentation, Hugo themes,
  Kubernetes manifests (Helm charts), and Prometheus alerting rules. AI agents produce
  correct Go templates at high pass@1 rates.

### Negative

- **Dot-prefixed access is a Go-ism.** Workflow authors must write `{{ .issue.title }}`, not
  `{{ issue.title }}`. The leading dot references the current context value — a concept
  specific to Go's template engine that has no analogue in Jinja2 or Handlebars. Non-Go
  developers will encounter this as an unexpected syntax requirement. Mitigation: the
  `template_render_error` produced by `missingkey=error` when the dot is omitted includes
  the variable name, which makes the mistake diagnosable. The `WORKFLOW.md` documentation
  (or a future `sortie init` scaffold) should include a working example template that
  demonstrates correct syntax for all three prompt modes.
- **Error messages are terse and positional.** `text/template` errors report byte offsets
  and action numbers (e.g., `template: prompt:1:15: executing "prompt" at <.issue.titl>:
  map has no entry for key "titl"`), not line-and-column positions relative to the original
  `WORKFLOW.md` file. When the prompt body starts after YAML front matter, the byte offset
  is relative to the template string, not the source file. If an engineer sees `line 2`
  but the actual error is on line 45 of `WORKFLOW.md` (after 43 lines of YAML front
  matter), the tool becomes actively hostile. Mitigation: the prompt rendering layer **must**
  (not should — this is an MVP requirement) rewrite `text/template` error positions by
  adding the front matter line count to the template-relative line number, and must include
  the issue identifier and turn number in the wrapped error. This line-offset mapping must
  be implemented in the initial prompt renderer (task 1.4), not deferred. The
  implementation is straightforward — count newlines in the front matter block and add the
  offset to the line number parsed from the error string — but it is non-negotiable for
  operator trust.
- **No built-in `default` function.** Go `text/template` has no `default` filter for
  providing fallback values when a field is nil or empty. Workflow authors must use
  `{{ if .issue.assignee }}{{ .issue.assignee }}{{ else }}unassigned{{ end }}` instead of
  a hypothetical `{{ .issue.assignee | default "unassigned" }}`. This is verbose but
  explicit. The initial FuncMap intentionally omits `default` because it would interact
  subtly with `missingkey=error`: authors might cargo-cult `| default` onto every variable
  to suppress errors, masking genuine template mistakes (the same problem noted in the
  pongo2 analysis above). If usage patterns demonstrate that `default` is genuinely needed
  and the masking risk is acceptable, it can be added as a backward-compatible extension.
- **`nil` vs zero-value semantics require care in the data layer.** Go `text/template`
  with `missingkey=error` distinguishes between a key that is absent from the map (error)
  and a key that is present with a nil value (no error, evaluates as falsy in `{{ if }}`).
  The `attempt` field, which is `nil` on first run and an integer on retries (Section 5.4),
  must be explicitly included in the data map with a `nil` value — not omitted. If the
  implementation accidentally omits `attempt` from the map on first run,
  `{{ if .attempt }}` will produce a `missingkey=error` instead of evaluating as false.
  The implementation must include a test case that verifies `{{ if .attempt }}` evaluates
  to false on first run (nil value present) and true on retry (integer value present).
  This is a well-known `text/template` subtlety that is easy to get wrong and hard to
  diagnose from the error message alone.
- **Context shift inside `{{ range }}` changes dot semantics.** Inside
  `{{ range .issue.labels }}`, the dot (`.`) refers to the current list element, not the
  root data map. Accessing the issue title inside a range block requires
  `{{ $.issue.title }}` (dollar-sign prefix to reference the root). This is documented Go
  template behavior but is the single most common mistake for engineers coming from Jinja2
  (Ansible) or Helm. Documentation alone is insufficient — engineers do not read docs until
  after they hit the error. Mitigation: the `sortie validate` subcommand (proposed in
  ADR-0004 for pre-commit and CI integration) must perform a static analysis pass that
  detects references to top-level data keys (`.issue`, `.attempt`, `.run`) inside
  `{{ range }}` blocks and emits a human-readable diagnostic: `"did you mean $.issue.title
  instead of .issue.title? Inside {{ range }}, the dot refers to the current element, not
  the root data."` This requires parsing the template AST via `text/template/parse` — the
  tree is available after `template.Parse()` and can be walked to detect `FieldNode`
  references to known top-level names within `RangeNode` subtrees. The prompt template
  documentation must also include a labeled example demonstrating `$` access inside
  `{{ range }}` blocks.
