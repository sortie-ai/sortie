---
status: proposed
date: 2026-05-18
decision-makers: Serghei Iakovlev
---

# Use First-Match-Wins Dispatch Rules in WORKFLOW.md Front Matter

## Context and Problem Statement

Sortie's orchestrator currently dispatches every active candidate issue with the single
agent identified by `agent.kind` and the single prompt template stored in the Markdown body
of `WORKFLOW.md`. Issue [#434](https://github.com/sortie-ai/sortie/issues/434) asks for an
ADR that resolves the configuration format and matching semantics for issue-aware routing,
unblocking the downstream implementation in [#435](https://github.com/sortie-ai/sortie/issues/435)
and the architecture documentation update in
[#471](https://github.com/sortie-ai/sortie/issues/471) within milestone
*M21: Rule-based Dispatch & Auto-merge*.

The decision must address four coupled questions:

1. **Where rules live** — embedded in `WORKFLOW.md` YAML front matter, in a separate
   configuration file, or in CLI flags and environment variables.
2. **How rules match** — which issue attributes are matchable, whether matching uses
   exact, glob, or regular-expression semantics, and how multiple match keys combine.
3. **How rules resolve** — first-match-wins on YAML list order, or weighted priority
   numbers independent of position.
4. **How fallback works** — what runs when no rule matches, and how this preserves
   backward compatibility for the existing single-agent workflows that ship today.

The decision sits between [ADR-0004](0004-workflow-file-format.md) (YAML front matter +
Markdown body single-file format) and [ADR-0005](0005-prompt-template-engine.md)
(Go `text/template` with strict failure semantics) and must remain consistent with both.

## Decision Drivers

1. **Backward compatibility.** Every existing `WORKFLOW.md` in the wild assumes a single
   `agent.kind` and a single Markdown body acting as the prompt template. The new format
   must let those workflows keep running without edits when rule-based routing is not
   configured.
2. **Single-file UX (ADR-0004 driver).** Workflow authors maintain one file per workflow.
   Splitting routing rules into a second file fragments versioning, breaks the unified
   live-reload signal provided by `fsnotify`, and forces operators to keep two files in
   sync.
3. **Strict failure semantics (ADR-0005 driver).** Per-rule template selection must
   integrate with the existing parse-once / execute-per-issue lifecycle. Parse errors in
   any reachable template must block dispatch at workflow load time, not surface
   stochastically when an issue happens to match a misconfigured rule.
4. **Deterministic invariants.** The orchestration state machine (Section 7), candidate
   sorting (Section 8.2 — priority asc, `created_at` oldest first, identifier
   lexicographic), capacity gates (Section 8.3 — global and per-state slots), blocker
   rule, claim, and reconciliation logic must remain unchanged. Rule evaluation is a
   *post-eligibility* selection step, not a replacement for any existing gate.
5. **Session continuity (architecture §7.1, §10.2).** Continuation retries propagate
   `ResumeSessionID` to the agent adapter so the agent resumes its prior thread.
   Switching agent kind or template across attempts on the same claim would corrupt
   that thread. Rule resolution must therefore freeze the selected agent and template
   for the lifetime of a single claim, not re-evaluate on every tick.
6. **Operator readability.** The configuration is reviewed in pull requests by humans.
   Reading a rule list top-to-bottom should produce the same mental model that the
   runtime applies. Decoupling list order from evaluation order (via weight numbers)
   forces reviewers to scan the entire file before knowing what each rule does.
7. **Schema validation precedent.** The config layer already supports both fixed
   sections (`tracker`, `agent`) and dynamic-keys sections (`reactions`,
   `max_concurrent_agents_by_state`). Whichever pattern is chosen, validation must use
   the existing `internal/config/schema.go` mechanisms so unknown keys, type
   mismatches, and semantic violations produce the same operator-visible diagnostics
   as the rest of the schema.
8. **Validation surface.** Per the Dispatch Preflight (Section 6.3), rule mistakes
   should be caught at startup and at every tick before any agent is launched. The
   format must produce structured errors compatible with `ValidateDispatchConfig`.
9. **Bounded matching power.** Regular expressions in operator-edited configs are a
   recurring source of catastrophic backtracking, accidental case-sensitivity bugs,
   and reviewer confusion. The matching DSL should cover labels, issue type, priority,
   identifier prefixes, and assignee with no more power than is needed.
10. **Layer hygiene.** Rule parsing belongs to `internal/config/`; rule evaluation
    belongs to `internal/orchestrator/`. No new package or import edge may break the
    documented downward-only model: low-level `domain`, `config`, and `prompt` packages
    do not import orchestration, workflow, adapter, or workspace packages; adapters do
    not import orchestrator or each other; orchestrator remains the coordination layer
    that consumes config, prompt, registry, workspace, persistence, and adapter
    interfaces.

## Considered Options

- **Option A.** Rules as a `dispatch.rules` section inside `WORKFLOW.md` YAML front
  matter, as an ordered list with first-match-wins semantics and a `dispatch.default`
  fallback block.
- **Option B.** Rules in a separate file (`dispatch.yaml` or similar), referenced from
  `WORKFLOW.md` or auto-discovered next to it.
- **Option C.** Rules as CLI flags or `SORTIE_*` environment variables.
- **Option D.** Rules embedded as a free-form map (dynamic keys, like `reactions`)
  inside `WORKFLOW.md`, with weighted-priority numeric ordering.

## Decision Outcome

Chosen option: **Option A (`dispatch.rules` in `WORKFLOW.md` front matter, ordered list,
first-match-wins, `dispatch.default` fallback)**, because it is the only option that
satisfies all ten decision drivers simultaneously — preserving single-file UX,
backward compatibility, deterministic semantics, and the existing parse-once template
lifecycle — without introducing a new authoring surface, a new file watcher, or a new
schema-evaluation mechanism.

### Wire format

```yaml
dispatch:
  rules:
    - name: <rule-name>            # optional, [a-z][a-z0-9_-]*, advisory; used only in logs and metrics
      match:                        # optional; an absent or empty match block matches every issue
        labels: ["bug", "p0-*"]     # glob list, ANY-of
        issue_type: ["Bug", "Story"] # case-insensitive exact list, ANY-of
        priority: { lte: 2 }        # numeric predicate; one of eq, in, lt, lte, gt, gte
        identifier: ["FE-*"]        # glob list, ANY-of, against issue.Identifier
        assignee: ["alice", "bob"]  # case-insensitive exact list, ANY-of
      agent: <kind>                 # optional; falls through to default agent kind when omitted
      template: ./prompts/bug.md    # optional; falls through to default template when omitted

  default:                          # optional
    agent: <kind>                   # optional; defaults to top-level agent.kind
    template: <path>                # optional; defaults to WORKFLOW.md body
```

Both `rules` and `default` are optional. The minimal valid workflow has no `dispatch`
section at all and continues to behave exactly as it does today.

The `dispatch` key is added to `knownTopLevelKeys` in `internal/config/config.go` and
gains a `SectionSchema` entry in `internal/config/schema.go` with `AllowAdapterPassthrough:
false` and `AllowDynamicKeys: false` for the outer object, plus a dedicated builder
(`buildDispatchConfig`) that validates the inner list shape and per-rule fields. Unknown
keys under `dispatch`, `dispatch.rules[*]`, and `dispatch.default` produce
`FrontMatterWarning` diagnostics through the existing advisory path. Unknown keys inside
`dispatch.rules[*].match` are configuration errors because they change matching behavior.

### Matching semantics

A `match` block is evaluated against the normalized `domain.Issue` (`internal/domain/
issue.go`) at dispatch time:

- **AND across keys.** All match keys present in the block must produce true. A key
  that is absent is not evaluated.
- **OR within a key.** A list value matches if any element matches. Single-value scalars
  are equivalent to a one-element list.
- **Strings — globs.** `labels`, `identifier`, and any other string list use Go's
  `path.Match` semantics (`*`, `?`, `[set]`). `labels` is matched against the
  adapter-normalized lowercase label set; `identifier` is matched against
  `issue.Identifier` with the case the adapter produced.
- **Strings — case-insensitive exact.** `issue_type` and `assignee` use case-insensitive
  equality with no glob expansion. This matches tracker conventions: issue types are a
  closed vocabulary, and assignee identifiers are display-stable.
- **Numbers — predicates.** `priority` accepts an inline predicate object with exactly
  one of: `eq` (integer), `in` (list of integers), `lt`, `lte`, `gt`, `gte` (integer).
  An issue with `nil` priority never matches a numeric predicate.
- **No regex.** v1 deliberately excludes regular expressions to avoid catastrophic
  backtracking and case-sensitivity surprises. If a future need is demonstrated, a
  parallel `labels_re:` (or similar) key may be added as a non-breaking extension.
- **No free-form expression language.** v1 does not introduce CEL, JEXL, Starlark, or
  Go template predicates. The decision drivers prefer narrow, readable matching.

The match key set is intentionally a closed list. Unknown match keys are rejected at
preflight as configuration errors (not warnings) so that typos like `lables:` cannot
silently disable a rule.

### Resolution semantics

1. **Order is meaningful.** Rules are evaluated in the order they appear in YAML. The
   first rule whose `match` block evaluates to true is selected. Subsequent rules are
   not consulted.
2. **Catch-all rules.** A rule with no `match` block (or an empty one) matches every
   issue. A non-final catch-all rule produces a configuration error at preflight,
   because later rules would be unreachable.
3. **Selection result.** The selected rule's `agent` (if present) overrides the default
   agent kind. The selected rule's `template` (if present) overrides the default
   template. Either may be omitted independently — partial overrides are explicitly
   supported.
4. **Fallback.** When no rule matches, `dispatch.default` is used. When `dispatch.default`
   is absent or partial, the missing fields fall back further: `agent` to the existing
   top-level `agent.kind`, `template` to the `WORKFLOW.md` Markdown body.
5. **Freeze on dispatch.** The orchestrator evaluates rules exactly once per issue
   claim, on the *initial* dispatch from `Orchestrator.handleTick`. The resolved
   `(agent_kind, template_id, rule_name)` is recorded on `RunningEntry` and propagated
   through `RetryEntry` so all retries and reaction-driven continuations for the same
   claim reuse the same agent and template. Re-evaluation occurs only after the claim
   is released. This preserves session continuity (driver 5) and gives operators a
   deterministic relationship between an issue's first dispatch and every subsequent
   retry.
6. **Dynamic reload.** When `WORKFLOW.md` is reloaded via the existing `fsnotify`
   watcher (Section 6.2), changed rules apply only to *future* claims. In-flight
   issues continue with their frozen selection.

### Template lifecycle integration with ADR-0005

The default Markdown body of `WORKFLOW.md` continues to be the default prompt template
parsed by the workflow loader. Per-rule and per-`default` templates are independent
`.md` files referenced by relative path (absolute paths and `~/` expansion are not
permitted, matching the conservative posture of the rest of the schema).

At workflow load:

1. The loader resolves each unique `template` path relative to `filepath.Dir(workflow_
   path)`.
2. Each resolved path is read and parsed using the same `prompt.Template` constructor
   that already governs the body template: Go `text/template` with
   `Option("missingkey=error")` and the same FuncMap (`toJSON`, `join`, `lower`) per
   ADR-0005.
3. Parse errors surface as `template_parse_error` for the specific path and block
   dispatch. The existing `last-known-good` behavior on reload failure is preserved.
4. The loaded set of named templates is exposed through a new
   `WorkflowManager.PromptTemplateByID(id string)` method (or equivalent), keyed by
   the absolute resolved path. The existing `PromptTemplateFunc` in `WorkerDeps`
   continues to return the *resolved-at-dispatch* template, so existing worker code
   (`internal/orchestrator/worker.go`) requires only one change: at dispatch time the
   orchestrator computes the template ID once per claim and the worker uses it for
   every turn of that claim.

The v1 filesystem watcher remains scoped to the workflow file. Editing `WORKFLOW.md`
reloads the rules and all referenced templates. Editing a referenced template file by
itself does not trigger the watcher path; the existing defensive reload before dispatch
re-reads referenced templates on the next dispatch cycle. Operators who need an immediate
reload after a template-only edit can touch `WORKFLOW.md`. Expanding the watcher to track
referenced template paths is a future additive improvement, not a requirement of this ADR.

Per-rule template files do **not** carry their own YAML front matter in v1. A
`template_parse_error` is raised if a referenced file begins with `---`. Per-template
configuration overrides (timeouts, max_turns, adapter-specific blocks) are explicitly
out of scope for this ADR; they are a candidate for a future, additive extension.

### Agent reference resolution

`agent: <kind>` in a rule or in `dispatch.default` references one of the kinds
recognized by the agent registry (`internal/registry/`). The corresponding adapter-
specific pass-through block (e.g., `claude-code:`, `codex:`, `copilot:`) must be present
in the front matter when the rule's kind differs from the top-level `agent.kind`,
otherwise the adapter will not have configuration to read.

Other `agent.*` fields (`max_turns`, `turn_timeout_ms`, `stall_timeout_ms`,
`max_concurrent_agents`, `max_retry_backoff_ms`, `max_sessions`,
`max_concurrent_agents_by_state`) remain workflow-wide. They are deliberately not
overridable per-rule in v1 because they are global resource budgets, not per-issue
parameters. Per-rule concurrency caps and per-rule timeouts are tractable additive
extensions if a concrete need emerges; doing them now would broaden the surface area
without a validated use case.

### Validation rules

The dispatch preflight (`internal/orchestrator/preflight.go`, `ValidateDispatchConfig`)
gains the following checks, all of which run at startup and at every tick before
dispatch:

- **Schema-level (warnings):** unknown sub-keys under `dispatch`, `dispatch.rules[*]`,
  and `dispatch.default` produce `unknown_sub_key` diagnostics through the existing
  `ValidateFrontMatter` path.
- **Structural (errors, dispatch blocked):**
  - `dispatch.rules` must be a YAML sequence; `dispatch.default` must be a map.
  - Each rule must be a map with at least one of `match`, `agent`, or `template`.
  - Rule `name`, when present, must match `^[a-z][a-z0-9_-]*$`.
  - Duplicate rule names are rejected with the offending index pair.
  - At most one catch-all rule (no `match` block) is permitted, and it must be the
    last element. A non-terminal catch-all is rejected with `unreachable_rules`.
  - `match` keys must be one of `labels`, `issue_type`, `priority`, `identifier`, or
    `assignee`. Unknown match keys are rejected as configuration errors, not warnings.
  - `match.priority` predicate must contain exactly one of
    `{eq, in, lt, lte, gt, gte}`.
  - Glob patterns are validated by calling `path.Match(pattern, "")` for each configured
    pattern and checking the returned error, so malformed user-provided patterns are caught
    at load time instead of dispatch time.
- **Cross-reference (errors):**
  - Every referenced `agent: <kind>` must resolve to a registered agent adapter.
    Unknown kinds are rejected.
  - Every referenced `template: <path>` must resolve to a regular file inside the
    workflow directory tree, must be readable, and must parse cleanly under ADR-0005
    rules. Symlinked targets outside the workflow directory tree are rejected to
    mirror the workspace safety posture (Section 9.6).

### Interaction with existing front-matter fields

| Existing concern                          | Interaction                                                                                                                                                                                                                                                |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `agent.kind` (default)                    | Unchanged. Becomes the implicit fallback when `dispatch.default.agent` is omitted.                                                                                                                                                                         |
| `agent.command`, timeouts, concurrency    | Workflow-wide. Not overridable per rule in v1.                                                                                                                                                                                                             |
| `tracker.query_filter`                    | Independent. Tracker-side filtering decides which issues *enter* the candidate set; rules decide *how* candidates are dispatched.                                                                                                                          |
| `tracker.active_states`, `terminal_states`| Independent. Eligibility (Section 8.2) is evaluated before rules.                                                                                                                                                                                          |
| `tracker.handoff_state` (ADR-0007)        | Independent. Handoff transitions are part of worker exit handling, not rule evaluation.                                                                                                                                                                    |
| `reactions.*`                             | Reactions trigger continuation dispatches on an existing claim; they reuse the frozen `(agent_kind, template_id, rule_name)` from `RunningEntry`. No re-evaluation.                                                                                        |
| `self_review.*`                           | Self-review runs after the coding turn loop using the same session and template. Rules do not change this.                                                                                                                                                 |
| Dynamic reload (Section 6.2)              | New rules apply to future claims only. Last-known-good config remains in force if reload parses but fails preflight.                                                                                                                                       |
| `max_concurrent_agents_by_state`          | Independent. Per-state slot accounting (Section 8.3) is keyed on tracker state, not on rule. A rule can still be denied dispatch when its issue's state has no slot.                                                                                       |

### Two concrete examples

The first example shows label-and-priority routing with explicit fallback:

```yaml
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project: acme/billing-api
  active_states: [todo, in-progress]
  terminal_states: [done, rejected]

polling:
  interval_ms: 60000

workspace:
  root: ~/workspace/acme-billing

agent:
  kind: claude-code           # default agent kind
  command: claude
  max_turns: 5
  max_concurrent_agents: 4

# Adapter-specific pass-through blocks for the agent kinds referenced by rules
claude-code:
  model: claude-sonnet-4-20250514
  permission_mode: acceptEdits

codex:
  approval_policy: never

dispatch:
  rules:
    - name: docs
      match:
        labels: ["docs", "documentation"]
      agent: codex
      template: ./prompts/docs.md

    - name: critical-bug
      match:
        labels: ["bug"]
        priority: { lte: 2 }
      template: ./prompts/bug-critical.md
      # agent omitted -> falls back to default (claude-code)

    - name: feature-work
      match:
        issue_type: ["Feature", "Story"]
      template: ./prompts/feature.md

  default:
    template: ./prompts/default.md
    # agent omitted -> falls back to top-level agent.kind (claude-code)

---
# Markdown body remains the prompt template used when no rule matches AND
# dispatch.default.template is absent. It is unchanged from the workflow's
# pre-rules state, which is what makes existing workflows keep working.

You are a coding assistant. Resolve {{ .issue.identifier }}: {{ .issue.title }}.
```

The second example shows identifier-prefix routing for a monorepo where issue keys
are themselves the routing signal, plus a terminal catch-all:

```yaml
tracker:
  kind: jira
  api_key: $JIRA_TOKEN
  project: ACME
  active_states: [to do, in progress]
  terminal_states: [done]

agent:
  kind: claude-code           # default kind for the catch-all rule
  command: claude
  max_turns: 5

claude-code:
  model: claude-sonnet-4-20250514

codex:
  approval_policy: never

dispatch:
  rules:
    - name: frontend
      match:
        identifier: ["FE-*"]      # ACME-FE-123 style keys
      agent: codex
      template: ./prompts/frontend.md

    - name: backend-critical
      match:
        identifier: ["BE-*"]
        priority: { lte: 1 }
      template: ./prompts/backend-critical.md

    - name: backend-default
      match:
        identifier: ["BE-*"]
      template: ./prompts/backend.md

    - name: ops
      match:
        assignee: ["ops-bot"]
      template: ./prompts/ops.md

    - name: catch-all           # no match block; must be last
      template: ./prompts/default.md

# dispatch.default omitted entirely -> the catch-all rule is the only fallback
# Per first-match-wins semantics, this is unambiguous.
---
You are the default Sortie agent. Resolve {{ .issue.identifier }}: {{ .issue.title }}.
```

### Considered Options in Detail

**Option B — Separate `dispatch.yaml` file (or auto-discovered sibling file).** A
dedicated file would cleanly separate routing from the rest of the workflow
configuration and would benefit from independent JSON Schema bindings in editors. It
was rejected for four reasons. First, it duplicates the failure mode that ADR-0004
explicitly avoided when it chose front matter over a separate prompt file: operators
now manage two artifacts that must remain consistent. Second, the live-reload
infrastructure currently watches one file (`internal/workflow/manager.go` plus
`fsnotify` per [ADR-0006](0006-use-fsnotify-for-file-watching.md)); a second file
requires a second watcher, a second debounce policy, and a synchronization story for
the case where one file is reloaded successfully while the other fails. Third, file
discovery becomes ambiguous: does Sortie look at `dispatch.yaml` next to `WORKFLOW.md`,
under `.sortie/dispatch.yaml`, or both? Each answer is a small extra rule operators
must remember. Fourth, the dispatch rules reference agent kinds whose configuration
(`claude-code:`, `codex:`) lives in `WORKFLOW.md`; cross-file references make
validation errors harder to read and harder to fix. The clean-separation benefit does
not outweigh these costs at Sortie's current scale.

**Option C — CLI flags and environment variables.** Rules expressed as
`--rule="labels=bug:agent=claude-code:template=./prompts/bug.md"` are infeasible for
non-trivial routing. Shell quoting, multi-line composition, glob escaping, and
predicate syntax do not survive the command line. The flag would either degenerate to
"one rule" — which does not solve the problem — or duplicate a config file inside an
argv string. Environment variables share the same problems and additionally violate
the source-precedence model (`SORTIE_*` env vars override individual config fields,
not whole structures). This option was rejected without further analysis once the
structural complexity became clear.

**Option D — Free-form dynamic-keys map with weighted-priority ordering.** The
`reactions` precedent suggests a syntactic shape like:

```yaml
dispatch:
  rules:
    docs:
      weight: 100
      match: { labels: [docs] }
      agent: codex
    bug:
      weight: 50
      match: { labels: [bug] }
      template: ./prompts/bug.md
```

YAML maps do not preserve declaration order across implementations, so first-match-wins
is not portable when rules are stored as a map. Weighted priority numbers fill that
gap but introduce two costs. First, list position no longer matches evaluation order,
so a reviewer cannot read top-to-bottom and predict behavior — they must compute the
sort over the entire rule set. Second, removing a rule requires checking weights on
all others to avoid creating ties or misaligned precedence. Numeric weights are
appropriate when rule sets are large enough that reordering them in source becomes
impractical (firewall ACLs with hundreds of rules, Cloud Load Balancer URL maps with
thousands of paths); Sortie's expected rule cardinality is small (single digits per
workflow in practice), where readable position-based order is unambiguously better.
The `reactions` analogy is also not exact: reactions are *named events* that fire
independently and never compete for selection, so they need no ordering. Dispatch
rules *do* compete, so ordering is intrinsic — and the simplest encoding of ordering
is a sequence.

## Consequences

### Positive

- **Backward compatibility is the default.** Workflows without a `dispatch` section
  behave exactly as they do today. Existing tests, examples, and operator habits keep
  working.
- **Single-file authoring is preserved.** Operators continue to version and review one
  artifact per workflow.
- **Live reload requires no new long-running watcher.** `fsnotify` already watches
  `WORKFLOW.md`; the loader gains a few extra fields to populate, nothing more.
  Per-rule template files are read at workflow load and reload by the same code that
  already reads the Markdown body. Template edits made alongside a `WORKFLOW.md` change
  are loaded immediately by that workflow-file event; template-only edits are picked up
  by the existing defensive reload before a future dispatch.
- **Parse errors are bounded at load time.** ADR-0005's contract — parse once, block
  dispatch on parse error — extends uniformly to per-rule templates without a new
  policy.
- **Rule evaluation is a pure function.** A new helper in `internal/orchestrator/` —
  exposed as something like `ResolveRule(issue, rules, default) → (agentKind,
  templateID, ruleName)` — has no I/O, no time dependence, and is trivially
  table-test-able. The dispatch loop in `Orchestrator.handleTick` gains a single call
  before `DispatchIssue`; nothing else moves.
- **Diagnostics flow through existing pipes.** Unknown keys raise
  `FrontMatterWarning`s; structural errors raise `*ConfigError`; preflight failure
  triggers the existing skip-dispatch-but-reconcile path documented in Section 6.3.
- **The orchestration state machine is untouched.** Eligibility, sorting, capacity,
  retry, and reconciliation logic stay byte-for-byte identical. Rule resolution slots
  in as a post-eligibility, pre-`DispatchIssue` step.

### Negative

- **Session continuity demands freezing the resolution.** `RunningEntry` and
  `RetryEntry` gain three new persisted fields (`rule_name`, `template_id`, plus
  retention of the already-recorded `AgentKind`) so retries and reaction continuations
  reuse the original selection. The persistence schema (Section 19) must be amended,
  and the SQLite migration logic must handle pre-existing in-flight rows by defaulting
  the new fields to the workflow's default agent and template. This is a one-time
  migration cost.
- **A new agent kind block must exist for every kind referenced by a rule.**
  Operators who use, for example, `agent: codex` in a rule but do not provide a
  `codex:` extension block will receive a preflight error. The error message must
  point precisely to the missing block — generic "unknown adapter" messaging would
  frustrate operators.
- **Catch-all-position rule is a footgun.** Authors instinctively place "default"
  rules first. The validator must reject non-terminal catch-alls with a clear
  diagnostic (`unreachable_rules: rule at index N follows a catch-all at index M`).
- **No regex in v1 may chafe.** Some operators will want substring-on-title or
  case-insensitive label matching beyond globs. The chosen scope is deliberate; if a
  follow-up issue documents a concrete use case the glob set cannot express, an
  additive `labels_re:`-style key is non-breaking.
- **Per-template Markdown files live outside `WORKFLOW.md`.** A single-file workflow
  with one inline template was the optimum point of ADR-0004; rules that override
  the template move some content out of the canonical file. This is inherent to the
  problem — multiple templates cannot share one Markdown body — but it does mean
  operators using rule-based dispatch maintain a small directory of prompt files. The
  filesystem layout convention (`./prompts/*.md`) is recommended in documentation but
  not enforced by the schema; absolute paths and parent-directory traversal are
  forbidden for safety reasons.
- **Validation cost grows with rule count.** Each rule with a `template:` triggers
  one `os.Stat` and one template parse at every reload. For workflows with dozens of
  rules this is still negligible (parses are sub-millisecond on typical hardware),
  but it scales linearly. If a workflow ever exceeds hundreds of rules this should be
  revisited; that scale has no current use case.

## Confirmation

The decision is validated when all of the following are true after implementation
(tracked under [#435](https://github.com/sortie-ai/sortie/issues/435)):

1. **Backward compatibility.** Every existing example workflow under `examples/` and
   every fixture under `internal/workflow/testdata/` continues to pass without
   modification, with empty `ServiceConfig.Dispatch` and identical observable
   behavior.
2. **Rule resolution.** Table-driven tests in `internal/orchestrator/route_test.go`
   exercise: AND across keys, OR within a key, glob matches and non-matches, numeric
   predicates, nil-priority handling, case-insensitivity for `issue_type` and
   `assignee`, catch-all rule, fallback to `dispatch.default`, fallback to
   top-level `agent.kind` and body template, and frozen selection across retry and
   reaction continuation.
3. **Schema validation.** Unit tests in `internal/config/schema_test.go` cover the
  warning cases (unknown sub-keys under `dispatch`, `dispatch.rules[*]`, and
  `dispatch.default`) and the error cases (malformed list, missing fields, duplicate
  rule names, unreachable rules, unknown match keys, unknown agent kinds, missing or
  unreadable template files, malformed globs, malformed priority predicates).
4. **Two-rules acceptance test.** A new integration test in
   `internal/orchestrator/dispatch_test.go` confirms the milestone verification
   criterion from issue #435: an issue with label `bug` dispatches to a different
   agent and/or template than one with label `docs`, and an issue with no matching
   label uses the default.
5. **Diagnostics.** Operator-facing error messages for each failure mode are reviewed
   for clarity. The CLI `sortie validate` subcommand (proposed under ADR-0004's
   negative-consequence mitigation) exercises the same code path as preflight.
6. **Documentation.** Architecture (Section 5.3 and Section 7-8) and the WORKFLOW.md
   syntax reference are updated under issue
   [#471](https://github.com/sortie-ai/sortie/issues/471) and the operator
   how-to guide under [#469](https://github.com/sortie-ai/sortie/issues/469).
