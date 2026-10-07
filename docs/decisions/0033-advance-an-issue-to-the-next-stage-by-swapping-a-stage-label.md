---
status: accepted
date: 2026-10-07
decision-makers: Serghei Iakovlev
---

# Advance an Issue to the Next Stage by Swapping a Stage Label

## Context and Problem Statement

Operators split work on one issue into stages, for example specify, then plan, then implement, then test. Each stage is a dispatch rule that selects its own agent kind, prompt template and that kind's settings, and an issue reaches a stage because it carries the label the rule matches. Every stage after the first consumes what an earlier stage left behind: a specification, then a plan derived from it, then the code that implements the plan.

Before this decision a stage could not hand the issue to the next stage. After a successful run the orchestrator moved the issue to the single workflow-wide `tracker.handoff_state`, and that state had to lie outside `tracker.active_states`, because an active handoff target would have dispatched the issue again on the next poll tick and looped. Every stage therefore ended with the issue parked, and a person had to move it back into the active set, and usually relabel it, before the next stage could start. A pipeline of four stages needed three manual steps that carried no judgment.

Four facts about the pre-decision system shape the answer:

1. The orchestrator wrote tracker state at exactly three points in an issue's life: the in-progress state at dispatch, the handoff target on a successful exit, and the merge-completion target when a managed pull request merged. It also applied labels, an escalation label and a parking label, and the architecture stated that no label it applied carried state semantics.
2. The tracker adapter contract could add a label to an issue and could not remove one. An adapter that did not support labels was allowed to accept the add and record nothing.
3. Dispatch rules matched an issue's labels, type, priority, identifier, assignee and title. No rule could match the issue's tracker state.
4. An issue's workspace was reused by every run of that issue and removed only when the tracker reported the issue terminal or when the opt-in age bound reclaimed it. Files one run left in the workspace were already visible to the next run of the same issue.

`0031-let-a-dispatch-rule-carry-its-agent-kinds-settings.md` fixed the shape of a rule, froze a rule's selection for the lifetime of a claim, and deferred automatic transition from one stage's rule to the next as a decision about tracker writes. This record makes that decision. It settles where the next stage is declared and how it falls back to `handoff_state` and `no_change_state`; whether a hop may keep the issue in the active set and how loops are prevented; how the next stage is selected on every tracker; how a hop combines with the claim freeze, continuation retries, reactions, the handoff evidence policy and an issue that is already terminal; how it is validated, including collisions with `terminal_states` and `in_progress_state`; what happens when the write fails; and where a hop is recorded.

## Decision Drivers

1. **N stages, defined by the operator.** One `WORKFLOW.md` must express a chain of any length, each stage a rule with its own kind, template and settings, without a fixed vocabulary of stage names.
2. **One mechanism on every tracker.** Some trackers have native workflow states with a transition graph that may refuse a move; others derive state from labels. Stage handoff must behave the same on all of them and must not depend on a tracker's own workflow configuration.
3. **The orchestrator writes only what it observed.** A hop reports an event the orchestrator saw, a successful run of a rule that names a next rule, and never expresses a judgment about the work. The judgment lives in configuration the operator wrote.
4. **No loop can run unbounded.** Keeping the issue active between stages removes the protection that made `handoff_state` non-active. Whatever replaces it must hold against a misconfigured chain and against tracker automation the orchestrator cannot see.
5. **Strict backward compatibility.** Every workflow that loads today loads unchanged and behaves the same. A rule without the new keys ends on `handoff_state` exactly as before.
6. **One claim, one stage.** A claim keeps its rule for its lifetime. A stage change must be a claim boundary, with a fresh selection and a fresh session, so that no running session, retry or reaction continues one stage's work under another stage's rule.
7. **The orchestrator never reads the work.** Files pass between stages through the workspace. The orchestrator does not open, parse or judge them.
8. **Validation stays cheap and offline.** Every check runs the same way at startup, on reload, at every tick and in `sortie validate`, with no network call.
9. **Every hop is attributable.** Run history must show which stage handed the issue to which, whether the tracker accepted the write, and which runs belong to one pass through the chain.

## Considered Options

Where the stage lives and who moves it:

- **A per-rule `next` naming another rule, carried by a Sortie-owned stage label, with the issue kept active and guarded statically and at runtime.**
- **A per-rule handoff target restricted to non-active states, with the next stage started by a separate trigger.**
- **A per-rule handoff target that names a native tracker state, which may be active.**
- **The stage stored only in Sortie's database.**
- **Transitions written by the agent.**
- **No change: document the manual step between stages.**

Whether the next stage is gated on files the previous stage declares:

- **Declare expected output files per rule and check their existence before advancing.**
- **Leave output checking to the stage's own agent and the existing status file.**

Whether a hop also moves native tracker state:

- **Write a per-rule native state together with the stage label.**
- **Write the stage label only.**

## Decision Outcome

Chosen option: **a per-rule `next` naming another rule, carried by a Sortie-owned stage label, with the issue kept active and guarded statically and at runtime; output checking left to the stage; the stage label written alone**, because it expresses a chain of any length in the rule shape operators already write, behaves identically on every tracker, makes each stage its own claim, and bounds every loop by two independent guards while leaving every existing workflow untouched.

### Rule shape

Two optional keys join a dispatch rule:

```yaml
dispatch:
  max_consecutive_hops: 10           # optional; per-issue ceiling on automatic hops
  rules:
    - name: specify
      stage: stage-specify           # Sortie-owned stage label; selects this rule
      next: plan                     # rule the issue moves to after a successful run
      template: ./prompts/specify.md
    - name: plan
      stage: stage-plan
      next: implement
      template: ./prompts/plan.md
    - name: implement
      stage: stage-implement         # no next: a successful run ends on handoff_state
      template: ./prompts/implement.md
```

- **`stage`** is a label. A rule that carries it is selected by that label instead of by a `match` block, so `stage` and `match` are mutually exclusive on one rule. The label is the stage's position in the tracker, and it is Sortie-owned: the orchestrator adds and removes it, and a person places an issue on a stage by applying it.
- **`next`** names another rule by its `name`. Absent `next` means the behavior every rule has today: a successful run ends on the workflow-wide handoff write point, which selects `no_change_state` for a declaration that stood and `handoff_state` otherwise, behind the human gate.

A rule that carries `stage` or `next` must have a `name`, and the name is the stage's name in logs, run history and templates. A rule with `stage` and no `match` is not a catch-all: it is selected only by its label, so the rule that only the last position may hold stays the one rule with neither. `next` may sit on a rule without `stage`, so an entry rule that matches an ordinary label can start a chain; the target of a `next` must carry `stage`. `dispatch.default` carries neither key.

### Selecting a stage

Stage labels are evaluated before the ordered rules. An issue that carries exactly one configured stage label is selected by the rule that owns that label, whatever its list position. An issue that carries none is routed by the ordered rules as before, and a rule with `stage` never matches it. An issue that carries more than one is selected by the label furthest along the chain of `next` links among them, and by list order where the labels do not lie on one chain, and the dispatch logs one warning naming every stage label it found. Stage labels compare case-insensitively against the adapter-normalized label set, as the `labels` key already does.

Giving stage labels precedence is what keeps a chain from being shadowed: an earlier rule, a catch-all included, cannot capture an issue that carries a stage label. Resolving several labels toward the downstream stage is what makes a hop that added the next label and failed to remove the old one route forward, never back.

Rules still cannot match native tracker state, and this decision does not add that key. The stage label works the same on a tracker whose state is native as on one whose state is itself a label, and the native state of a staged issue stays what the active-state gate and `tracker.in_progress_state` make it.

### The hop

A worker exit advances the issue only on success: where the run would otherwise take the handoff write point, meaning the issue is still active, the exit is not a blocked soft stop, the dispatch drives issue state, and the frozen `tracker.handoff_evidence` policy permits the write, and the run's rule carries `next` in the configuration in force at the exit. A declaration that the requested outcome already held, once it stands, is a success and advances like any other. A stood declaration on a rule with `next` does not select `no_change_state`, because the hop replaces the handoff write point for that rule; the declaration still resets the consecutive-absence count, as it does today, and the next stage learns of it from its template. A blocked exit parks the issue as before. A failed, timed-out or stalled run retries as before and never advances.

The hop is the same pre-write discipline as the handoff write, then one tracker write in two calls:

1. The terminal test against the freshest observation and, when `tracker.terminal_states` is non-empty, the verification read run first. A terminal result suppresses the hop and the exit takes the terminal disposition.
2. The orchestrator adds the next rule's stage label to the issue.
3. It then removes every other configured stage label the issue carries. A stage label is Sortie-owned, so removing a stale one is cleanup, not a judgment.
4. It cancels the issue's queued retry and every pending reaction entry, releases the claim, and seeds no reaction entries from this exit.

The issue stays in its active state. The next poll tick selects the next rule by its label, takes a fresh claim, freezes a fresh selection and starts a fresh session, never a resume of the previous stage's session, because a session belongs to one rule's kind and template. An issue in the chain therefore spends one poll interval between stages.

When the hop is not made, the exit takes the disposition it would have taken had the rule carried no `next`: the handoff write point, with its own target selection and its own failure semantics. This is the fallback in three cases: the add fails or is refused, whatever the error kind; the consecutive-hop ceiling is reached; and the rule named by the frozen selection no longer carries `next` after a reload. A failed hop is not retried by re-running the stage, because that spends a full agent run to repeat one label write, while the handoff state places the issue where a person sees it with its stage unchanged. When the add succeeds and a removal fails, the hop stands: the issue routes forward by the downstream rule above, the result is recorded as partial, one warning is logged, and the leftover label is removed by the next hop or by a person.

Until a later read of the issue observes the label a hop added, the issue is not dispatched on any rule other than the hop's target. A candidate listing may lag a write, and without this hold a stale read would run the previous stage again. The hold lifts on the first observation of the target label, or when the issue is observed outside the active states.

### Two loop guards

**Static, at load and in `sortie validate`.** `next` names an existing rule; it does not name the rule that carries it; its target carries `stage`; and the graph of `next` links is acyclic. Each rule has at most one `next`, so an acyclic graph means every chain ends, and its length is bounded by the number of staged rules. A cycle is an error that names every rule on it.

**Runtime, per issue.** The orchestrator counts consecutive automatic hops for each issue, durably. The count resets when the issue is observed outside the active states, by a handoff, a park, a terminal state or any non-active state a person moves it to, and when a park on it is released. It does not reset when a stage label changes while the issue stays active, because a label move cannot be told apart from tracker automation, and automation that moves labels can rebuild a cycle the static guard cannot see. A hop that would exceed `dispatch.max_consecutive_hops` is not made, and the exit falls back to the handoff write point. The ceiling is per issue rather than per chain for the same reason: a loop that leaves the chain through an external move and re-enters it is still one issue spinning.

`dispatch.max_consecutive_hops` defaults to the larger of `10` and the number of hops in the longest configured chain, so a healthy chain never reaches it. An explicit value below the longest chain is an error, because every issue would fall back mid-chain on a healthy run. `0` and negative values are errors. The per-issue budgets stay in force underneath both guards: `agent.max_sessions` and `agent.max_tokens` sum every stage an issue passes through, which also bounds automation that cycles an issue through a non-active state and back.

### Validation

Every check runs offline through one code path at startup, on reload, before every tick's dispatch and in `sortie validate`.

Errors:

- The `next` checks above, and a `next` on a rule without `name`.
- A rule that carries both `stage` and `match`, or a `stage` that is not a non-empty string.
- Two rules with the same stage label, compared case-insensitively.
- A stage label equal, case-insensitively, to any configured state name: every entry of `active_states` and `terminal_states`, using the adapter's fallback lists where a list is empty, and `handoff_state`, `in_progress_state` and `no_change_state`. On a tracker that derives state from labels a state name is a label, so a stage label that equals one would turn the hop into a state transition, and the collision is rejected on every tracker so a workflow does not change meaning when its tracker does.
- A stage label equal to a label the orchestrator applies for another purpose: a reaction's `escalation_label` or the parking label.
- Any rule carries `next` while `tracker.handoff_state` is unset. Every fallback path lands on the handoff write point, and without a handoff state it would degrade to the continuation retry, which re-runs the same stage.
- Any rule carries `next` while the configured tracker kind does not declare the stage-label capability below.
- `dispatch.max_consecutive_hops` outside its range.

Warnings:

- `agent.max_sessions` is positive and smaller than the number of runs the longest chain needs, so a healthy issue would exhaust its session budget before the chain ends.

`in_progress_state` needs no new rule. A hop writes no state, so it cannot collide with a terminal or in-progress state, and each stage's dispatch performs the in-progress transition exactly as any dispatch does; an issue already in that state converges without a change.

### The tracker capability

Removing a label is a new tracker operation. It is an optional capability an adapter declares at registration, like the other optional tracker capabilities, not a method every adapter must implement. An adapter that declares it implements label removal and guarantees that both the labels it adds and the labels it removes are visible to subsequent reads of the issue, the candidate listing included. The existing permission of the add operation to accept a write and record nothing does not extend to an adapter that declares the capability. A workflow whose rules carry `next` on a tracker kind without the capability fails validation, because a hop the tracker silently drops would re-run the previous stage until the ceiling.

### The orchestrator's tracker writes

This decision changes a stated boundary and owns the change. The stage label is the first label the orchestrator applies that carries meaning for dispatch: its presence selects a rule. It is still not a tracker state. It does not decide whether an issue is active or terminal, it never moves an issue in or out of the active set, and it has no effect on reconciliation, workspace cleanup or concurrency limits keyed by state. The orchestrator now writes tracker state at the same three points as before, and in addition writes stage labels at one point, a successful exit of a rule that names a next rule, which is an event it observed and not a judgment about the work. The architecture's account of tracker writes must say so.

### Passing files between stages

A stage hands its files to the next stage through the issue's workspace, which every run of the issue reuses. The orchestrator never reads a stage's files. The convention for where a file lives belongs to the templates, which name paths with the template data they already have, such as the issue identifier. A file that must outlive the workspace, which is removed when the issue turns terminal or ages out, belongs in the repository.

Every prompt render receives a `stage` object with three fields, present on every render so that a template evaluated with `missingkey=error` never fails on a stage it was not reached through:

| Field | Value |
| --- | --- |
| `stage.current` | The selected rule's name when the rule carries `stage`; empty otherwise. |
| `stage.previous` | The name of the rule whose hop placed the issue on this stage; empty when a person placed it or no hop preceded the dispatch. |
| `stage.previous_outcome` | `succeeded` or `no_change` for the run that hopped; empty when `stage.previous` is empty. |

`stage.previous` and `stage.previous_outcome` are filled when the dispatch selects the target of the issue's latest hop and the hop count has not reset since. They are frozen with the selection, so a retry or a reaction continuation of the same claim renders them as the first attempt did.

### Interaction with existing mechanisms

| Concern | Interaction |
| --- | --- |
| Claim freeze | A hop releases the claim; the next stage is a new claim with a new frozen selection and a new session. Moving a stage label on an issue whose claim is held changes nothing until the claim is released, as before. |
| Continuation retry | Replaced by the hop on the success path, as the handoff write replaced it. A hop that is not made falls back to the handoff write point, whose own failure path schedules the continuation retry. |
| Reactions | A hop cancels the issue's pending reaction entries and queued retry, which belong to the claim being released and would otherwise continue the previous stage on its rule while the next stage holds the issue. A hop exit seeds no reaction entries. The last stage, which ends on the handoff write point, seeds them exactly as today, so review handling, CI feedback, auto-merge and merge completion attach to the last stage's rule, and the CI entry it seeds carries the ref of the latest push. |
| `tracker.handoff_evidence` | The verdict gates the hop as it gates the handoff write. A withheld verdict does not advance; it takes the failure path and counts toward `agent.max_consecutive_absences` as today. |
| Terminal issue | The terminal disposition precedes the hop. An issue observed terminal, or found terminal by the verification read, never advances. |
| Blocked exit | Parks the issue as today, and the park resets the hop count. |
| Label-command dispatches | A dispatch that does not drive issue state never hops. |
| `agent.max_sessions`, `agent.max_tokens` | Unchanged and per issue; they sum every stage. |
| Self-review | Runs before the exit disposition as today; the hop follows it. |
| Reload | A rule's `next` is read from the configuration in force at the exit; its selection identity stays frozen. A changed chain applies from the next exit. |
| Workspace retention | Unchanged. The interval between a hop and the next dispatch is one poll tick, and the age bound measures from the latest completed run. |

### Recording

Each run-history row carries the run's chain identifier, assigned at the first dispatch on a stage that no hop reached and inherited by every run the chain's hops lead to. A run that reached a hop decision also records the target rule and the write result: `advanced`, `partial`, `failed`, or `ceiling`. The orchestrator emits `stage.advanced` for a hop that was made and `stage.not_advanced` for a hop that was due and not made, each carrying the issue, the source rule, the target rule, the chain identifier, the hop count and, for the second, the reason. Both are ordinary events any notification destination may subscribe to; no destination receives them implicitly. `sortie stats` and the dashboard show a run's chain and stage beside its rule.

### Examples

A chain whose entry is an ordinary label, so a person only labels the issue once:

```yaml
tracker:
  active_states: ["To Do", "In Progress"]
  handoff_state: Human Review

dispatch:
  rules:
    - name: specify
      match:
        labels: ["feature"]
      next: implement
      template: ./prompts/specify.md
    - name: implement
      stage: stage-implement
      agent: <kind>
      template: ./prompts/implement.md
---
Resolve {{ .issue.identifier }}: {{ .issue.title }}
```

`specify` writes `.specs/{{ .issue.identifier }}.md` because its template says so. On success the orchestrator adds `stage-implement`; the next tick selects `implement` by that label ahead of `specify`, and its template reads the same path, with `{{ .stage.previous }}` rendering `specify`. On success `implement` moves the issue to `Human Review` and its reactions take over. Moving the issue back to an active state reruns `implement`, because its label is still there; removing `stage-implement` instead sends the issue back to `specify`.

### Deferred

These extend the shape chosen here additively and are not part of it:

- Backward edges, such as a review stage sending an issue back to implementation. They make cycles intentional, so they need a per-edge bound and a defined meaning for the hop count, and the static guard would have to distinguish an intended cycle from a mistaken one.
- Conditional next stages chosen by the run's outcome. Only success advances in this decision, so a stage has one successor.
- A native tracker state written with the hop, for boards that show stages as columns. It would be a second write per hop whose failure must combine with the label write, a tracker's transition graph may refuse it, and the in-progress transition at the next dispatch would overwrite it unless that also became per rule.
- Expected output files declared per rule and checked before advancing.
- A rule key that matches native tracker state.

`0031-let-a-dispatch-rule-carry-its-agent-kinds-settings.md` listed six deferrals. This decision settles the first, automatic transition between stages. Review handling on a kind other than the claim's stays deferred, but a chain now gives it a natural owner, the last stage. Re-matching on a retry, per-rule attempt limits, named profiles and per-model cost estimates are unchanged: `next` refers to a rule by its `name`, which is the stable reference that ADR already required for a rule with a settings block, so named profiles can still be added on top without changing how a chain is written.

### Considered Options in Detail

**A per-rule handoff target restricted to non-active states, with a separate trigger.** This keeps the rule that no orchestrator write lands an issue in the active set, which is the strongest argument for it: the original loop cannot return by construction. But the separate trigger has to be something that moves the issue back into the active set, and the only parties that can are a person, which is the manual step this decision removes, or a second orchestrator write on a timer or an event, which is the active-state write by another name with an extra state between stages and an extra poll interval of latency. It also needs one non-active state per stage boundary, created in the tracker, and on a tracker with a transition graph each of those states must be reachable from the previous one.

**A per-rule handoff target that names a native tracker state, which may be active.** It reuses the transition the orchestrator already performs and shows the stage on a board. It fails the one-mechanism driver. Rules cannot match native state, so the next stage would need a new match key; the in-progress transition at each dispatch writes one state over every stage state, so the two settings would conflict; a tracker's transition graph may refuse the move between two stage states the operator never connected; and on a tracker that derives state from labels a stage would be one more state label, which makes stages and the active set the same list. A label is uniform on every tracker and touches none of that.

**The stage stored only in Sortie's database.** No tracker write, no new tracker capability, and no read lag: the orchestrator would always know the stage. But the stage would be invisible where people work. A person could not see which stage an issue is in, could not place an issue on a stage or send it back, except through a new operator surface that most people who triage tickets cannot reach, and two Sortie instances sharing a tracker would disagree. It would also make a database restore or a new deployment lose every issue's position. The tracker is the source of truth for where an issue is, and a stage is part of where it is.

**Transitions written by the agent.** The agent can already write to the tracker through its tool, and it knows best whether its stage's work is done. It is rejected as the mechanism for the same reasons handoff is the orchestrator's: a tool call is probabilistic, a crash or a timeout skips it, correctness would depend on every template instructing it correctly, and the agent would be writing a routing decision the orchestrator then trusts without having observed the success it implies. An agent that moves the issue itself remains possible and compatible: the orchestrator's pre-write observation sees the change and the issue routes by what is there.

**No change.** Documenting the manual step costs nothing and keeps every invariant as it is. It declines a need operators already meet with one manual step per stage boundary, which is exactly the toil an orchestrator exists to remove, and the multi-profile shape of dispatch rules was chosen to grow into this.

**Declare expected output files per rule.** A per-rule list of paths checked for existence before advancing would stop a chain whose stage claimed success without producing its file, and checking existence does not read content. It is left out of this decision for three reasons. A stage that cannot produce its output already has a precise signal, the `blocked` status, and a stage that writes nothing is already caught by the handoff evidence policy, which gates the hop. Useful paths depend on the issue, so the list would need templated paths in configuration, a second template surface with its own validation. And a missing file would need its own disposition, neither failure nor block, for one more case. It can be added later as a rule key without changing anything here.

**Write a per-rule native state with the stage label.** Rejected for this decision and listed as deferred, for the reasons given there.

## Consequences

### Positive

- A chain of any length fits in one `WORKFLOW.md`, written with two keys on the rules operators already have.
- Handoff between stages behaves identically on every tracker and never depends on a tracker's transition graph.
- Each stage runs as its own claim and session, so a stage's selection, settings and reactions never leak into the next.
- A person sees the stage on the issue and changes it by moving one label, which is the same gesture that starts a chain.
- Loops are bounded twice: a misconfigured chain fails to load, and anything the configuration cannot see stops at a per-issue ceiling and lands where a person sees it.
- Existing workflows are untouched; a rule without `next` ends on `handoff_state` exactly as before.
- Run history and events show every hop with its source, target, result and chain.

### Negative

- The orchestrator now writes a label that carries dispatch meaning, a widening of the boundary that said no label it writes carries meaning. The architecture, the workflow reference and the operator guide must state it.
- The tracker contract gains an optional capability, label removal with read-back visibility, which each adapter implements once; a workflow using `next` fails validation on a tracker kind that lacks it.
- The handoff write's guarantee that a successful run leaves the active set no longer holds for a rule with `next`. The loop it prevented is now prevented by the static and runtime guards instead.
- A stage that produced nothing new after an earlier stage pushed still counts as work observed, because pushed-commit metadata in the reused workspace is positive evidence whichever run wrote it. A chain's later stages are therefore gated mostly by their own status and by success.
- Pending reactions of an intermediate stage are dropped at the hop; a CI failure during an intermediate stage is seen only when a later stage's exit, or the last stage's, seeds the CI entry again.
- An issue carrying several stage labels is routed by a precedence rule a person has to learn, and a lagging read can delay the next stage by a poll interval while the hop's read-back hold is in force.
- Run history gains columns, and the template data gains a `stage` object.

## Confirmation

The decision is implemented when all of the following hold:

1. A workflow without `stage` or `next` loads, validates, dispatches and records exactly as before, apart from the new columns and the always-present `stage` template object.
2. `sortie validate` reports offline, through the same path as preflight: a missing or self-named target, a target without `stage`, a cycle naming its rules, `stage` beside `match`, duplicate stage labels, a stage label colliding with any configured state name or applied label, `next` without `handoff_state`, `next` on a tracker kind without the capability, an out-of-range or too-small `dispatch.max_consecutive_hops`, and the `agent.max_sessions` warning.
3. A successful run of a rule with `next` adds the target label, removes the other stage labels, cancels the issue's pending reactions and queued retry, releases the claim, leaves the issue active, and the next tick dispatches the target rule on a new claim and a new session with `stage.previous` set.
4. A blocked, failed or evidence-withheld run, or a terminal issue, does not advance. A failed add, a reached ceiling and a vanished `next` each fall back to the handoff write point; a failed removal records `partial` and still advances.
5. The hop count persists across restarts, resets only on an observation outside the active states or a released park, and stops a cycle created by moving labels externally at the ceiling.
6. A stale read after a hop does not dispatch the previous stage.
7. Each hop writes its run-history fields and emits `stage.advanced` or `stage.not_advanced`.
8. The architecture's account of tracker writes, the workflow reference, the prompt reference and the operator guide describe stage labels, `next`, the guards, the fallback and the template fields.
