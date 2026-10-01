---
status: accepted
date: 2026-09-30
decision-makers: Serghei Iakovlev
---

# Let a Dispatch Rule Carry the Settings of the Agent Kind It Selects

## Context and Problem Statement

Real workloads mix routine issues with hard ones. Operators want routine work on a cheaper model at a lower reasoning level and hard work on a stronger model at a higher one, chosen from one `WORKFLOW.md` by the same dispatch rules that already route issues. The model and the reasoning level are settings of an agent kind: both live in the settings block named for the kind (`claude-code:`, `codex:`, `copilot-cli:`, `opencode:`), where `effort` sits beside `model` and means the same thing in every block that reads it.

Before this decision a rule could choose only the agent kind and the prompt template. Every issue a kind ran used that kind's one top-level block, so sending two groups of issues to two models of the same kind meant running two workflows, or editing the workflow each time. The same limit blocked a second pattern: separate work profiles in one workflow, such as writing a specification, implementing it, and handling review, each on its own kind or model. Changing that is a change to the design of dispatch rules, so this record restates that design in full and replaces `0011-dispatch-rule-configuration.md`.

Two facts about the pre-decision system shape the answer:

1. Rules were strict about keys. An unrecognized key inside a rule map, inside `dispatch.default`, or inside a `match` block failed the load; only an unknown key directly under `dispatch` was a warning. No workflow that loaded before this decision can carry a settings block inside a rule, so adding one is strictly additive.
2. An agent kind's settings were bound at process start. Each adapter was constructed once, from its top-level block, and kept that block for the life of the process, so a change to a block needed a restart while rules, templates and `agent.*` timeouts took effect on reload. Per-rule settings bound the same way would make a reloaded rule route issues with the new rule and the old settings.

The decision settles five questions: which settings a rule may override, how a rule's settings combine with the workflow's, how they are validated, how they are bound to a claim and to a reload, and where the settings a run used are recorded.

## Decision Drivers

1. **Serve the stated need directly.** Routing cheap and hard work to different models and reasoning levels through dispatch rules is the need. Matching on an issue's title is part of this decision; matching on its description or its comments is a separate question and is not part of it.
2. **Grow toward work profiles without rework.** One workflow should hold several profiles, each a kind, a template and that kind's settings, and the shape chosen now must extend toward automatic stage transitions and per-profile reactions without being replaced.
3. **Strict backward compatibility.** Every workflow that loads today loads unchanged and behaves the same. Every new key is optional.
4. **Least surprise for an operator.** One merge rule, one binding rule and one validation path. A setting must never appear to be applied while it is not, and a correction to the workflow must reach the next attempt.
5. **Session continuity.** A retry or a reaction continuation resumes the agent's prior session. Only something that cannot survive inside one conversation may force a new session.
6. **Budgets are per issue.** `agent.max_tokens`, `agent.max_sessions` and `agent.max_consecutive_absences` sum an issue's whole history. An issue may pass through several rules over its life, as when one profile writes a specification and another implements it.
7. **The core does not interpret adapter settings.** A kind's block is passed through to its adapter. The orchestrator may read only keys that carry one meaning in every kind's block.
8. **Validation stays cheap and offline.** Every check runs the same way at startup, on reload, at every tick and in `sortie validate`, with no network call and no model catalog.
9. **Attribution.** Run history, `sortie stats` and the dashboard must attribute a run to the model it was configured with, and show what the runtime reported running.

## Considered Options

Which settings a rule may override:

- **Adapter block only.** A rule may carry one settings block for the kind it runs; every `agent.*` field stays workflow-wide.
- **Adapter block plus a closed list of `agent.*` fields.** Additionally allow per-attempt limits such as `turn_timeout_ms`, `stall_timeout_ms` and `max_turns` on a rule.
- **No per-rule settings.** Keep rules as they are and run one workflow per settings profile.
- **Named profiles.** A new top-level section of named profiles that rules reference by name.

How a rule's block combines with the top-level block:

- **Key-by-key overlay.** A key written in the rule replaces the inherited value, `null` removes it, every other key is inherited.
- **Whole-block replacement.** The rule's block replaces the top-level block.
- **Model and effort as a pair.** Key-by-key overlay, except that a rule setting `model` also drops the inherited `effort`.
- **Deep merge.** Maps merge recursively and lists append.

How settings are bound to a claim:

- **Freeze settings per claim.** Resolve the block at the first dispatch and reuse the stored values for every retry and continuation of the claim.
- **Resolve settings per attempt.** Freeze the selection identity for the claim; resolve the settings from the configuration in force at the start of each attempt.

## Decision Outcome

Chosen option: **adapter block only, key-by-key overlay, resolved per attempt**, because it answers the stated need with no new concept, keeps every existing workflow valid, gives an operator one rule for what a claim keeps and what each attempt reads, and lets a correction reach the next attempt instead of the next claim.

### Rule shape

Rules live in the `WORKFLOW.md` front matter, in the single file the workflow already is, under `dispatch`:

```yaml
dispatch:
  rules:
    - name: <rule-name>              # [a-z][a-z0-9_-]*; required when the rule carries a settings block
      match:                         # optional; an absent or empty match block matches every issue
        labels: ["bug", "p0-*"]      # glob list, any-of
        issue_type: ["Bug", "Story"] # case-insensitive exact list, any-of
        priority: { lte: 2 }         # exactly one of eq, in, lt, lte, gt, gte
        identifier: ["FE-*"]         # glob list, any-of
        assignee: ["alice", "bob"]   # case-insensitive exact list, any-of
        title: ["[docs]", "wip:"]    # case-insensitive whole-word phrase list, any-of
      agent: <kind>                  # optional; falls back as described below
      template: ./prompts/bug.md     # optional; falls back as described below
      <kind>:                        # optional; settings block for the kind this rule runs
        model: <model>
        effort: <level>

  default:                           # optional
    agent: <kind>                    # optional; defaults to agent.kind
    template: <path>                 # optional; defaults to the WORKFLOW.md body
```

Both `rules` and `default` are optional; a workflow with no `dispatch` section behaves as a single-kind, single-template workflow. A rule must carry at least one of `match`, `agent`, `template` or a settings block. `dispatch.default` carries no settings block: the top-level blocks are the defaults.

### Matching

A `match` block is evaluated against the normalized issue at dispatch time. All keys present must match (AND across keys); a list matches when any element matches (OR within a key), and a scalar is a one-element list. `labels` and `identifier` use glob matching (`*`, `?`, `[set]`): `labels` against the adapter-normalized lowercase label set, `identifier` against the identifier as the adapter produced it. `issue_type` and `assignee` compare case-insensitively, without globs. `priority` takes exactly one of `eq`, `in`, `lt`, `lte`, `gt`, `gte`, and an issue with no priority never matches it. `title` matches when one of its phrases appears in the issue's title as whole words, ignoring letter case, and an issue with an empty title never matches it. There are no regular expressions and no expression language: the key set is closed and small, so a typo cannot silently disable a rule and a reviewer can predict a match by reading it.

A title is written for people rather than for Sortie, so a phrase that could match anywhere in it would match words nobody meant. A phrase therefore matches only where it neither starts nor ends inside a word, a word being a run of letters, digits, and combining marks: `fix` matches `Fix login redirect` but neither `Add prefix to logs` nor `Fixes typo`. A phrase that starts or ends with any other character, as `[docs]` does, is held to no word boundary on that side. Each character of the Han, Hiragana, Katakana, Thai, Lao, Khmer, and Myanmar scripts, which are written without spaces between words, counts as a word of its own. A combining mark, such as a Thai vowel or tone mark, belongs to the character before it in every script, so a match never separates the two. Only letter case, white space, and the invisible emoji variation selectors U+FE0E and U+FE0F are normalized: white space at either end is ignored, each run of it compares as one space, and a variation selector is dropped on both sides, while `*`, `?`, brackets, accents, and every other character match only themselves. A `title` key must list at least one phrase, so that a title-only rule cannot become a catch-all, and every phrase must hold a character other than white space.

### Resolution and fallback

1. Rules are evaluated in YAML order and the first rule whose `match` succeeds is selected. List position is evaluation order, so the file reads top to bottom as the runtime applies it.
2. A rule with no `match`, or an empty one, is a catch-all. Only the last rule may be one; an earlier catch-all makes the rules after it unreachable and is an error.
3. The selected rule's `agent` and `template` each override the default independently. When no rule matches, `dispatch.default` applies. A missing `agent` falls back to `dispatch.default.agent`, then to `agent.kind`; a missing `template` falls back to `dispatch.default.template`, then to the Markdown body of `WORKFLOW.md`.
4. The kind produced by the fallback, `dispatch.default.agent` when set and otherwise `agent.kind`, is the default kind. It alone launches `agent.command`; any other kind launches its own default command.

A rule's kind is therefore its `agent`, else `dispatch.default.agent`, else `agent.kind`. That is the kind its settings block must be named for.

### Settings a rule may carry

A rule may carry exactly one settings block, named for the kind the rule runs, holding the same keys that kind's top-level block accepts. A block for any other kind is an error. Everything the kind reads from its block is overridable this way, `model` and `effort` included, and so are the per-invocation limits some kinds keep in their block. No closed list of "overridable" keys sits on top of a block the core does not interpret.

Two groups of keys stay out of a rule's block:

- **`agent.*` fields stay workflow-wide.** The per-issue budgets, `agent.max_tokens`, `agent.max_sessions` and `agent.max_consecutive_absences`, are permanently workflow-wide: they sum an issue's whole history across every rule that issue passes through, so a per-rule budget would be a budget inside a budget with no single answer to which one applies. Concurrency and scheduling limits (`max_concurrent_agents`, `max_concurrent_agents_by_state`, `max_retry_backoff_ms`) are shared resources of the process and stay workflow-wide for the same reason. Per-attempt limits (`turn_timeout_ms`, `stall_timeout_ms`, `read_timeout_ms`, `stop_grace_ms`, `max_turns`) stay workflow-wide until a need appears that raising the workflow-wide ceiling does not meet. The `agent` key of a rule already names a kind, so such limits would arrive as a new rule key, additively.
- **Keys the orchestrator derives from `agent.*` are rejected in a rule's block.** The orchestrator hands each adapter `kind`, `command` and the four timeouts from the `agent` section, and a block cannot override them. Writing one of them in a rule's block is an error rather than a silently ignored key, because in a rule it reads as an override that would not happen. `agent.command` keeps belonging to the default kind alone.

A rule that carries a settings block must have a `name`. The name is how an attempt finds its rule's settings again after a reload, where an index into the list does not survive an inserted rule, and it is the label `sortie stats` groups by. Rule names stay unique.

### Combining a rule's block with the top-level block

The block an attempt runs with is the top-level block of the rule's kind with the rule's block laid over it, one level deep:

1. A key written in the rule replaces the inherited value whole, whatever its type. Maps are not merged and lists are not appended.
2. A key written as `null` in the rule removes the inherited key, so the adapter sees the key as never written and applies its own default. For `model` and `effort` that means the runtime's own default.
3. Every key the rule does not write is inherited.
4. A kind with no top-level block inherits nothing; the rule's block is the whole block. A rule that introduces a kind other than `agent.kind` and carries that kind's block therefore needs no top-level block for it. A selection that reaches a non-default kind with neither, through `dispatch.default.agent` or a rule without a block, still fails the missing-block check as before.

The rule's block is layered only over the top-level block of the same kind, never over another kind's.

`model` joins `effort` as a key with a single meaning in every kind's block that reads it: a string handed to the runtime as written, where absent, `null` and empty all mean the runtime's own default. The orchestrator reads these two keys for validation advisories and for recording, and never interprets their values.

### Binding: freeze the selection, resolve the settings per attempt

1. **The selection identity is frozen per claim.** Rules are evaluated once, at the claim's first dispatch. The resolved kind, template and rule name are recorded with the running entry and carried by every retry and reaction continuation of the claim.
2. **The retry exception.** A waiting retry checks its frozen selection against the configuration in force when its timer fires. The selection stands, with its resume session identifier, while `agent.kind`, `dispatch.default.agent` or a rule still names its kind and the workflow still holds its template, whether or not the rule still matches the issue. Otherwise the retry is routed afresh by the rules and starts a new session, because a session identifier belongs to one adapter. A kind removed from the product converts onto its replacement kind and keeps the frozen template. A retry whose kind's adapter is unavailable is rescheduled with backoff and keeps its claim. One `Info` record is logged when a retry dispatches on a different selection.
3. **The settings are resolved at the start of every attempt.** At the first dispatch, at each retry and at each reaction continuation, the attempt's block is built from the configuration in force at that moment: the top-level block of the frozen kind with the frozen rule's block for that kind laid over it. A running session never changes settings.
4. **A vanished rule block.** When no rule carries the frozen name any more, or the rule with that name no longer carries a block for the frozen kind, the attempt runs on the top-level block of the frozen kind and logs one `Info` record naming the rule. For a rule that never carried a block this is the behavior it always had.
5. **Settings reach the adapter as a session input.** The resolved block is part of what the orchestrator hands an adapter when it starts a session, not something the adapter keeps from its construction. One resolution point serves launch, preflight and recording, so they cannot disagree about which block applies.

The operator learns one rule: an issue keeps its agent kind, template and rule until its claim is released; what they contain, meaning model, effort, the rest of the kind's settings, the template text and the `agent.*` timeouts, is read from `WORKFLOW.md` at the start of every attempt. Moving a label on an issue whose claim is still held does not re-route it; the next claim does.

### Session continuity across a settings change

A change of settings between attempts does not by itself end a session: an attempt that resumes resumes with the settings it resolved. Every kind that reads `model` and `effort` continues the same conversation when a session is resumed under a different model or level, carrying the earlier history to the new model as context: the runtimes behind `copilot-cli` and `opencode` apply the model and level given at resume to the resumed session, `codex` sends the model and level on every turn of a thread, and `claude-code` already switches models inside one session. Continuity is owed to the conversation, not to the model, so the reason that freezes the kind does not extend to its settings.

What this requires of an adapter is that it deliver the resolved settings on a resumed session as on a new one, on every turn, rather than rely on the runtime to remember them; a runtime may keep the model of a session but forget its level. Keys other than `model` and `effort` follow the same rule, as `mcp_config` always has.

### Reload

A reload of `WORKFLOW.md` applies changed rules to future claims only. In-flight sessions keep running unchanged, and a waiting retry keeps its frozen selection as far as the retry exception allows. Changed settings, in a rule's block or in a top-level block, apply from the next attempt of any claim. The top-level kind blocks therefore become reload-applicable like the rest of the configuration, where they previously required a restart. A reload that fails to parse or validate leaves the last good configuration in force.

### Templates

The Markdown body of `WORKFLOW.md` is the default template. Rule and default templates are separate `.md` files referenced by relative path, resolved against the directory holding `WORKFLOW.md`; absolute paths, `~` expansion and paths that escape the workflow tree, symlinked or not, are rejected. Each referenced file is read and parsed at load with the same template engine and strict options as the body, so a parse error blocks dispatch at load rather than when an issue first matches. A referenced file must not begin with its own front matter. Templates are keyed by their resolved absolute path. The watcher watches `WORKFLOW.md`; a template-only edit is picked up by the defensive reload before a later dispatch, and touching `WORKFLOW.md` reloads it at once.

### Validation

Every check runs offline and through one code path at startup, on reload, before every tick's dispatch and in `sortie validate`.

Errors, which fail the load or block dispatch:

- The structural checks: `dispatch` is a map, `rules` a sequence, `default` a map; each rule is a map with at least one of `match`, `agent`, `template` or a settings block; names match `^[a-z][a-z0-9_-]*$` and are unique; at most one catch-all, and last; `match` keys come from the closed set; a `priority` predicate has exactly one operator; every glob is well-formed; a `title` key lists at least one phrase, and no phrase is white space alone.
- The cross-reference checks: every referenced kind is registered; every referenced template resolves inside the workflow tree, is readable and parses.
- Unknown keys: any key inside a rule, inside `dispatch.default` or inside `match` that is not recognized is an error. A key inside a rule that names a registered agent kind is a settings block; any other unrecognized key stays an error.
- A rule's settings block is named for a kind other than the one the rule runs. This also catches a block left behind after the rule's `agent`, `dispatch.default.agent` or `agent.kind` changed.
- A rule's settings block is not a map, or writes a key the orchestrator derives from `agent.*`.
- A rule carries a settings block and no `name`.
- Every check an adapter applies to its top-level block is applied to each rule's resolved block, with the rule named in the message: key types, the adapter's own configuration checks, the session-resume and usage-reporting checks, and conflicts between keys that only appear once the layers are combined.

Warnings, which never block:

- Every advisory the top-level block of a kind draws is also drawn by a rule's resolved block, such as `effort` set on a kind that does not forward it.
- A rule writes `model`, does not write `effort`, and inherits a non-empty `effort`. Level names depend on the model, so the rule's model would be asked for a level chosen for another one; the message tells the operator to write `effort` in the rule to set it or `null` to clear it.
- An unknown key directly under `dispatch` stays a warning, as before.

Model and level names are not checked against any catalog. The runtime judges them, as it does for the top-level block.

### Recording

Each run-history row records, beside the kind, rule name and template it already carried, the attempt's configured `model` and `effort` as resolved (empty when unset) and the model the runtime reported running (empty when it reported none). The configured values say what the operator asked for; the reported one says what ran, which differs under routing aliases and runtime-side fallbacks. `sortie stats` gains a grouping by configured model beside its groupings by kind, rule and template. The runtime snapshot API and the dashboard show a running session's rule name and configured `model` and `effort` beside the reported model, and the dispatch log line carries the same three fields. Model is not added as a metric label, because its cardinality is unbounded.

### Interaction with other configuration

| Concern | Interaction |
| --- | --- |
| `agent.kind` | Unchanged. The final fallback kind, and the kind whose top-level block the default selection uses. |
| `agent.command` | Belongs to the default kind alone. A rule's block cannot set a command. |
| `agent.*` limits and budgets | Workflow-wide. Budgets sum over every rule an issue passed through. |
| `tracker.query_filter`, `active_states`, `terminal_states` | Independent. They decide which issues are candidates; rules decide how a candidate is dispatched. |
| `tracker.handoff_state` | Independent. Handoff is part of worker exit, not of rule evaluation. |
| `reactions.*` | A continuation reuses the claim's frozen selection through the retry path and resolves its settings at its own start, like any attempt. |
| `self_review.*` | Runs in the same session and template as the coding turns, with the attempt's settings. |
| `max_concurrent_agents_by_state` | Independent. Slots are keyed by tracker state, not by rule. |
| Removed agent kinds | A rule's block for a removed kind converts with its kind, exactly as the top-level block does. |

### Examples

Routing routine and hard work to different models of one kind:

```yaml
agent:
  kind: opencode

opencode:
  model: provider/strong-model
  effort: high

dispatch:
  rules:
    - name: low-cost
      match:
        labels: ["model:low-cost"]
      opencode:
        model: provider/cheap-model
        effort: null          # clear the inherited level; the cheap model may not offer it
    - name: hard
      match:
        labels: ["model:strong"]
      opencode:
        effort: max           # model inherited from the top-level block
---
Resolve {{ .issue.identifier }}: {{ .issue.title }}
```

Issues with neither label run on the top-level block. `sortie stats` groups the runs by rule and by configured model.

Two work profiles on two kinds in one workflow, with the stage expressed as a label the rules match:

```yaml
agent:
  kind: claude-code

claude-code:
  permission_mode: bypassPermissions
  model: strong-model
  effort: high

dispatch:
  rules:
    - name: specify
      match:
        labels: ["specify"]
      template: ./prompts/specify.md
      claude-code:
        effort: max
    - name: implement
      match:
        labels: ["implement"]
      agent: opencode
      template: ./prompts/implement.md
      opencode:               # the whole opencode block; no top-level opencode block is needed
        model: provider/coding-model
        effort: high
---
Resolve {{ .issue.identifier }}: {{ .issue.title }}
```

Review feedback on the implementation continues the `implement` claim through a reaction, on that rule's kind and template, with its settings read fresh at the continuation's start.

### Deferred

These extend the shape chosen here additively and are not part of it:

- Automatic transition from one stage's rule to the next without a human changing the issue, which needs a per-rule handoff and is a decision about tracker writes.
- Review handling on a kind other than the claim's, which needs a reaction to name a rule and a defined point where a new session begins.
- Re-matching the rules on a retry when the new match keeps the same kind and template, so that moving a label escalates a held claim to a stronger model without losing its session.
- Per-rule attempt limits, as a new rule key.
- Named profiles that several rules or reactions share. YAML anchors already remove repetition between rules, and a rule's `name` already gives a stable label.
- Cost estimates per model. Token rates stay keyed by agent kind, so until rates can be set per model, two models of one kind are priced alike.

### Considered Options in Detail

**Adapter block plus a closed list of `agent.*` fields.** Allowing `turn_timeout_ms`, `stall_timeout_ms` or `max_turns` per rule has a real motive: a strong model at a high level thinks longer and is silent longer. But nothing asked for it, a timeout is a ceiling that can simply be raised workflow-wide, and the `agent` key of a rule is already taken by the kind, so the list would need a new key and a second place where limits live. Budgets could not join the list under any shape, because they are per issue and an issue crosses rules. The option adds surface with no validated need; it stays open as an additive rule key.

**No per-rule settings; one workflow per profile.** It costs no code. It also means two processes, two databases and two concurrency limits that know nothing of each other, with statistics split across them and the routing moved into tracker query filters. It declines the need instead of meeting it.

**Named profiles.** A top-level section of named profiles that rules reference would remove repetition and give profiles a name reactions could point to later. It adds a concept, a second way to choose a kind (the rule's `agent` against the profile's), and a class of dangling-reference errors, for a benefit anchors and rule names already provide. The internal model chosen here, a selection that is a kind, a template and a resolved block, is exactly what a profile would resolve to, so profiles can be added on top later without breaking anything.

**Whole-block replacement.** A rule writing only `model` would lose every inherited key, including `permission_mode`, `mcp_config` and `effort`; losing a permission mode silently changes what the agent may do. It is the most surprising of the merge options and contradicts what an operator writing one key expects.

**Model and effort as a pair.** Dropping the inherited `effort` whenever a rule sets `model` addresses a real trap, since level names depend on the model. It does so with a special case for two keys inside a block the core otherwise leaves alone, and it breaks "what you don't write is inherited". The warning for an inherited `effort` under a changed `model`, plus `null` to clear it, closes the same trap without an exception to the merge rule.

**Deep merge.** Recursive map merging and list appending need escape hatches to replace or reset a value, each of which must be learned. Kind blocks are almost entirely scalars and short lists, so the machinery would buy nothing.

**Freeze settings per claim.** It matches the intuition that a claim runs on one configuration, and it gives one issue a fixed cost profile. Its failure modes are ones an operator hits in healthy use. A typo in a model name fails every attempt of every claim that took it, and fixing the workflow does not help those claims until they are released, which with no session limit is never. A reaction days later continues on a model the operator has since replaced, or a provider has since withdrawn. A restart, which applies a new block to every retry, would stop doing so. And it contradicts how Sortie already treats the template text and the `agent.*` timeouts, which are read fresh at every attempt. Freezing the selection identity keeps what continuity needs; freezing the values keeps what it does not.

**Settings bound at process start, as before.** Leaving adapter construction as the binding point while rules reload would make a reloaded rule route with its new match and its old model, a combination of two configurations that neither the old nor the new file describes. Building one adapter instance per distinct resolved block instead would keep the adapter contract unchanged, but it would keep several generations of configuration alive at once and complicate adapters that hold state across sessions. Handing the resolved block to the session is the smaller change.

## Consequences

### Positive

- The need is met in the shape an operator would write unprompted: a block named for the kind, holding only what differs.
- Existing workflows are untouched. Nothing that loaded before can carry a block in a rule, and a rule without one behaves exactly as before.
- One rule governs binding: identity per claim, content per attempt. It is the rule template text and timeouts already followed, and it now covers kind settings, top-level ones included.
- A correction to a model name or level reaches the next attempt of every claim, and top-level kind blocks no longer need a restart.
- Every adapter check applies to per-rule settings without new per-adapter code, because the checks already take a block as input.
- Budgets keep a single, per-issue meaning across rules.
- Run history separates what was configured from what ran, and `sortie stats` shows where the tokens went per model.
- Work profiles on different kinds fit in one workflow today, and the selection model extends toward named profiles, per-rule handoff and reaction routing without a rewrite.

### Negative

- The adapter contract changes: every adapter reads its settings from the session it starts rather than from its construction, which touches every agent adapter once.
- One claim can run consecutive attempts, and one conversation, on different settings if the operator edits the workflow between them. Per-attempt recording keeps this visible.
- Moving a label on an issue whose claim is held does not change its rule until the claim is released. This follows from freezing the rule and must be said plainly in the operator documentation.
- The effective value of `model` or `effort` now has more sources: the rule, the top-level block, the runtime's own host configuration, and the runtime's default, with the runtime's own caps on top. The documentation must publish that order.
- Cost estimates still price every model of a kind alike, so the savings from routing show in token counts before they show in money.
- Run history gains columns and the stats and API surfaces gain fields, a one-time migration.

## Confirmation

The decision is implemented when all of the following hold:

1. A workflow without per-rule blocks loads, validates, dispatches and records exactly as before, apart from the new columns.
2. A rule's block overlays the top-level block of its kind key by key; `null` removes an inherited key; a rule block for a kind with no top-level block stands alone.
3. `sortie validate` reports, offline and through the same path as preflight: a block named for another kind, a block without a rule `name`, a derived key in a rule block, a non-map block, every adapter check against each rule's resolved block with the rule named, and the inherited-`effort` warning.
4. A retry and a reaction continuation keep the frozen kind, template and rule, and run with the settings of the configuration in force at their start; the retry exception and the vanished-rule fallback behave as stated.
5. A retry whose resolved `model` or `effort` changed since the previous attempt resumes the same session and runs every turn with the new values, level included.
6. An edit to a top-level kind block applies to the next attempt without a restart.
7. Each run-history row carries the configured model and effort and the reported model, `sortie stats` groups by configured model, and the snapshot API and dashboard show the rule and configured settings of a running session.
8. The workflow reference, the architecture specification and the operator guide describe the rule block, the merge rule, the binding rule, the source order for `model` and `effort`, and the new reload behavior of kind blocks.
