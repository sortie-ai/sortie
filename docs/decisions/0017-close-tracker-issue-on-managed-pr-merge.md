---
status: accepted
date: 2026-08-07
decision-makers: Serghei Iakovlev
---

# Close the Tracker Issue When a Managed Pull Request Merges

## Context and Problem Statement

The orchestrator writes tracker state in exactly two places. At dispatch it moves the issue to
`tracker.in_progress_state`, and on a normal worker exit with the issue still active it moves the
issue to `tracker.handoff_state`. A third write exists in the agent tool surface, where
`tracker_api` executes a transition the agent asked for, but that is an agent-initiated call rather
than an orchestrator decision.

No orchestrator code path moved an issue into a value drawn from `tracker.terminal_states`. Every
reaction kind that reports an outcome back to the tracker did so with `AddLabel` or `CommentIssue`,
and none of them transitioned the issue. The result was a workflow that never closed on its own. An
operator who configured a handoff state got issues that stopped there permanently unless a human or
an external automation advanced them, and the merge that actually completed the work left no mark on
the issue's state.

The second consequence is quieter and more damaging. Workspace cleanup is gated entirely on terminal
state. When reconciliation refreshes the tracker state of a running issue, it marks the entry for
deferred cleanup only if the refreshed state is terminal. The periodic sweep that collects abandoned
workspace directories lists the keys on disk, excludes the ones belonging to in-flight work, asks the
tracker for the state of the rest, and removes only those whose state is terminal. Neither path has
an age-based or capacity-based fallback, and the workspace configuration exposes nothing but a root
directory. In a deployment where issues stop at the handoff state, one workspace directory per
processed issue was retained indefinitely and disk usage grew without bound. The growth was silent,
because a sweep that finds no terminal issue reports nothing to clean and logs nothing at all.

Two recorded positions stood against closing the loop, so the question is architectural rather than a
matter of implementation. The handoff-state decision established the handoff state as deliberately
not terminal and required that it never appear in the terminal-state list. The tracker integration
contract stated that Sortie remains a scheduler, a runner, and a tracker reader, with ticket
mutations handled by the coding agent. Adding an orchestrator-driven terminal transition amends both,
and this decision states each amendment in full.

## Decision Drivers

1. **Coverage must not depend on an unrelated opt-in.** The problem is present in every deployment
   that configures a handoff state. A remedy available only to deployments that also enabled
   orchestrator-driven merging would leave the majority of affected deployments unchanged.
2. **A terminal state list is not a set of interchangeable values.** The shipped defaults prove this
   rather than merely suggesting it: the forge adapters default to a completion state and an
   abandonment state together, and the Linear adapter defaults to a completion state plus two
   distinct abandonment states. Any rule that picks a member of that list by position or by
   convenience will eventually mark completed work as abandoned.
3. **A tracker write must happen once per event, and survive a restart.** Poll ticks are frequent and
   the process is expected to restart. A transition that fires twice is visible noise on a team
   board; a transition that never fires because the process restarted at the wrong moment reproduces
   the original defect.
4. **There is no continuation to degrade into.** The handoff transition can afford to fail quietly
   because the continuation retry re-runs the worker and the transition is attempted again. After a
   merge there is no further work to continue, so a failure that is only logged is a failure that is
   never noticed.
5. **A visible write to the operator's system of record must be opted into.** Moving an issue to a
   terminal state closes it on the operator's board and, on the forges, closes the native issue. A
   deployment that does not ask for this must behave exactly as it did before.
6. **The remedy should reuse the mechanisms that already exist.** Reaction lifecycle, cross-restart
   deduplication, escalation actions, and the offline configuration validator are all in place. A
   design that invents parallel machinery for the same jobs adds surface without adding capability.

## Considered Options

- Extend the auto-merge reaction's success path so that a merge performed by the orchestrator also
  transitions the issue
- Introduce a reaction kind that observes the merge state of managed pull requests independently of
  whether orchestrator-driven merging is configured
- Leave tracker state entirely to the operator and address only the workspace lifecycle consequence,
  by bounding workspace retention on age or capacity
- Have the coding agent close the issue through the agent tool surface, and instruct it to do so in
  the workflow prompt

## Decision Outcome

Chosen option: **introduce a reaction kind that observes the merge state of managed pull requests**,
because it is the only option that covers merges the orchestrator did not perform itself, and
because the pending-reaction lifecycle it plugs into already provides the polling, the persistence,
the escalation, and the restart recovery the transition needs.

The reaction is named `merge_completion` in configuration and carries the internal reaction kind
`merge-completion`. It is inert unless configured, and a deployment that omits it is unaffected in
every observable respect.

### Observing the merge

The reaction observes the merge by polling the pull request recorded in the workspace SCM metadata
that the agent writes, which is the same source the review, bot-review, merge-conflict, and
orchestrator-driven merge reactions already use to learn a pull request's number, owner, repository,
and branch. A pending entry is created on worker exit when the metadata names a pull request, and it
is recreated after a restart by the pending-reaction recovery pass, which reads the run history and
the workspace metadata and rebuilds entries for issues sitting in the handoff state. That recovery
gate suits this reaction exactly, because the handoff state is precisely where an issue awaiting its
merge is parked.

The pending entry for this kind is not subject to the thirty-minute expiry the other reaction kinds
apply. Those kinds observe something that either happens shortly after the agent finishes or does not
happen at all, so dropping the entry bounds pointless polling. A merge waits on human review and
routinely takes days. Expiry is instead bounded by the thirty-day recovery lookback already applied
to run-history rows and workspace activity, and by the reaction ceasing to poll once the issue leaves
the handoff state.

### Selecting the terminal target

The target is never inferred. The reaction transitions the issue to the single state named by its own
`target_state` field, and the field has no default. Deriving the target from the terminal-state list,
whether by taking the first entry or by matching a well-known name, would depend on list ordering
that carries no meaning and on state vocabulary that varies per deployment. Because the shipped
defaults already place a completion state and an abandonment state in the same list, an inference
rule would be wrong by construction for a class of correct configurations.

### Idempotency

The idempotency key is the `reaction_fingerprints` row identified by the issue and the reaction kind
`merge-completion`, whose fingerprint value is the merge commit identifier reported by the forge.
The table is keyed on the issue and the kind, its upsert resets the dispatched flag whenever the
fingerprint value changes, and it lives in the same SQLite database as the rest of the orchestrator
state, so the latch holds across poll ticks and across process restarts alike.

Using the merge commit identifier rather than the pull request number is deliberate. It re-arms the
reaction correctly when an issue legitimately produces a second merge, and it refuses to fire when
the forge has not reported a merge commit, since an empty fingerprint is treated as no observation
rather than as an observation of nothing. On a successful transition the row is marked dispatched and
retained by this reaction; it is not deleted here, because deleting it would let the next poll tick
observe the same merge as new.

The latch is this reaction's alone to keep. An escalation that releases an issue after a retry
budget is exhausted clears the escalating kind's own pending entry, attempt counter, and
`reaction_fingerprints` row, and nothing else the issue holds, so a sibling kind escalating on the
same issue leaves this reaction's entry and row intact. That scoping is what makes the
once-per-merge property unconditional. A clear ranging over every pending entry and every
`reaction_fingerprints` row an issue holds would take this reaction's entry and row with it and
leave the same merge observable as new, so per-kind scope is an invariant of the escalation paths
rather than a detail of any one of them. A second guard sits behind the latch: recovery rebuilds an
entry only for an issue still parked in the handoff state, and an issue this reaction has already
closed is no longer parked there.

### Failure posture

There is no continuation to degrade into, so every failure resolves to one of three outcomes:
bounded retry, immediate escalation, or a recorded stop. The routing follows the tracker error
taxonomy, because those categories already distinguish a fault that will pass from a fault that will
not.

| Error kind               | Posture                                                                |
| ------------------------ | ---------------------------------------------------------------------- |
| `tracker_transport_error` | Retry with backoff, bounded by `max_retries`, then escalate            |
| `tracker_api_error`       | Retry with backoff, bounded by `max_retries`, then escalate            |
| `tracker_auth_error`      | Escalate immediately, no retry                                         |
| `tracker_payload_error`   | Escalate immediately, no retry                                         |
| `tracker_not_found`       | Stop, mark dispatched, log at warning level, no escalation             |

Transport and API failures are transient by definition, and the fingerprint row stays undispatched so
a restart resumes the attempt. Authentication failures mean a revoked token or a scope the operator
never granted, and retrying against them burns rate limit without any prospect of success. Payload
failures mean the target state is not reachable, whether because a Jira workflow forbids the
transition from the current status, because no workflow state of that name exists in the issue's
Linear team, or because a label-driven adapter rejects a target that is not among its configured
states. Both are configuration faults, both are deterministic, and both are reported to the operator
at once rather than after a retry budget has been spent proving they are permanent.

A not-found failure is the one case with nothing to escalate onto: the issue is gone, so a label or a
comment has no destination. The reaction records the attempt as dispatched, drops the pending entry,
and logs the outcome.

Escalation uses the same `escalation` and `escalation_label` mechanism as the sibling reactions, so a
failed close surfaces as a label or a comment on the issue rather than only in the process log. This
is the driver that has no analogue in the handoff transition, and it is the reason the failure path
is louder here than there.

### Scope boundary

A pull request closed without merging is out of scope. Closing a pull request unmerged is a human
signal whose meaning is ambiguous between abandonment, supersession, and a change of approach, and
choosing between a completion state and an abandonment state requires intent the orchestrator does
not have. An issue with no managed pull request is likewise out of scope, because there is no merge
to observe. In both cases the issue remains in the handoff state, which is the behavior of a
deployment that does not configure this reaction at all.

### Adapter coverage

No tracker adapter changes. Every shipped tracker adapter already accepts a terminal target and
already performs the native consequence. The GitHub, Gitea, and GitLab adapters validate the target
against the configured active, terminal, and handoff states, swap the state label, and patch the
native issue to closed when the target is terminal. The Jira adapter matches the target against the
destination status of the available workflow transitions and executes the matching one. The Linear
adapter resolves the target to a team-scoped workflow state and applies it. The file adapter records
the override in memory.

The source-control adapters do require a change, and this is the one part of the design that is not
free. The merge state of a pull request is currently unrepresentable in the domain contract. The
merge-status type carries a draft flag, a mergeability classification, a head commit, and the two
branch names, and none of those can express that a pull request has merged. The mergeability
classification has values for clean, unstable, blocked, dirty, and unknown, with no member meaning
merged, so a merged pull request normalizes to unknown and is indistinguishable from one the forge is
still evaluating. The GitHub adapter does not even decode the relevant response fields, though the
endpoint it already calls returns them. Today the only way the system learns of a merge is by
performing it, or by matching the text of a conflict response against the phrase the forge uses when
a pull request is already merged.

The merge-status type therefore gains a merged indicator and a merge commit identifier, populated
from the pull request object the mergeability read already fetches, at no additional request cost.
Discrete fields are added rather than a new member of the mergeability classification, so that
existing consumers switching on that classification keep their current behavior. Two packages
implement the source-control contract, one per forge, and both are updated.

### Configuration surface

The reaction is configured as a block under `reactions`, following the shape every other reaction
kind uses.

| Field              | Type    | Default        | Notes                                              |
| ------------------ | ------- | -------------- | -------------------------------------------------- |
| `provider`         | string  | empty          | Source-control adapter kind. Empty leaves the reaction inert |
| `target_state`     | string  | none           | Required when `provider` is set                    |
| `poll_interval_ms` | integer | `60000`        | Minimum `30000`, matching the sibling reactions    |
| `max_retries`      | integer | `2`            | Applies to the retryable error kinds only          |
| `escalation`       | string  | `label`        | `label` or `comment`                               |
| `escalation_label` | string  | `needs-human`  | Applied when `escalation` is `label`               |

The `provider` field is the opt-in switch, matching how the orchestrator-driven merge reaction
activates. The last four fields are the common reaction fields and are parsed by the shared reaction
decoder; `target_state` and `poll_interval_ms` are kind-specific and are extracted into a typed
configuration value at construction, as the sibling kinds do with their own fields.

Validation happens in the orchestrator's offline reaction validator, which receives the whole service
configuration and can therefore check a reaction field against a tracker field. Every check below is
a configuration-shape check requiring no network access, so `sortie validate` reports all of them
before a run begins. Each failure is an error rather than an advisory, because a reaction configured
to close issues into a state it cannot reach is worse than one that is switched off.

1. `provider` set and `target_state` empty is an error.
2. `target_state` that is not a member of `tracker.terminal_states`, compared case-insensitively, is
   an error.
3. `target_state` equal to `tracker.handoff_state`, compared case-insensitively, is an error.
4. `target_state` present in `tracker.active_states`, compared case-insensitively, is an error.
5. `poll_interval_ms` below the sibling minimum is an error.

Environment indirection through `$VAR` is not supported for any field in this block. No reaction
field supports it: the shared reaction decoder reads string values verbatim, and the indirection
applies to a fixed set of fields elsewhere in the configuration. State names are not secrets and have
no reason to be the exception.

Dynamic reload does not apply. Reaction configuration is captured when the orchestrator is
constructed and is not rebuilt when the workflow file is reloaded, unlike the tracker state lists,
which are read fresh on every tick. A change to this block takes effect on restart. This matches
every existing reaction kind, and the validation above compares the target against the tracker
configuration as it stood at construction.

The reaction posts no comment on a successful close. The state change is itself the record, an
orchestrator-driven merge already comments when it merges, and a second write per issue buys nothing
that the state change does not already show.

### Amendment to the handoff-state decision

The handoff-state decision remains in force in full. The handoff state is still deliberately not
terminal, it must still never appear in the terminal-state list, it must still never appear in the
active-state list, and the transition into it on normal worker exit is unchanged in trigger, in
target, and in its degrade-to-continuation-retry failure posture. Nothing about the handoff
transition is revised.

What that decision described, and what this one changes, is the shape of the pause. The handoff state
was framed as a pause point at which a human evaluates the agent's output and either sends the issue
back to an active state or forward to a terminal state, with the forward move belonging to the human
in every case. This decision reassigns one specific instance of that forward move. When a pull
request managed by Sortie merges, and only then, and only when the operator has configured this
reaction, the orchestrator performs the forward move itself. Every other exit from the handoff state
remains the human's, including a pull request closed unmerged, an issue that never produced a pull
request, and an issue a human decides to abandon.

The two decisions also differ in what a failure means, and the difference is deliberate. The handoff
transition may fail quietly because the continuation retry will bring the worker back and the
transition will be attempted again, with the session budget as the outer bound. This transition has
no such second chance, so its failures escalate.

### Amendment to the tracker-writes boundary

The tracker integration contract stated that Sortie does not require first-class tracker write APIs
in the orchestrator, that ticket mutations are typically handled by the coding agent through tools
defined by the workflow prompt, and that Sortie remains a scheduler, a runner, and a tracker reader.
Read literally, that boundary had already moved before this decision: the orchestrator sets the
in-progress state at dispatch and the handoff state at exit, so it has not been a pure reader for
some time.

The boundary is restated here in the form this decision leaves it in. The orchestrator writes tracker
state at exactly three points in an issue's life, each one a single named state drawn from
configuration, each one corresponding to an event the orchestrator itself observed: the in-progress
state when it dispatches an agent, the handoff state when an agent exits normally with the issue
still active, and the completion state when a pull request it manages merges. It writes no other
tracker state. Free-form ticket mutation remains outside the orchestrator and stays with the coding
agent through the agent tool surface, which is unchanged.

The distinction that survives is not between reading and writing. It is between a write that reports
an event the orchestrator observed and a write that expresses a judgement about the work. The
orchestrator makes the first kind and never the second. Choosing between a completion state and an
abandonment state for a pull request closed unmerged is a judgement, which is why that case is out of
scope here rather than an oversight.

### Considered Options in Detail

**Extend the auto-merge reaction's success path.** This is the smallest possible change. The
orchestrator-driven merge already knows the merge succeeded, already holds the issue identifier, and
already performs after-merge work in one place, so the transition would be a few lines beside the
existing comment and branch deletion. It fails on coverage, for three independent reasons, any one of
which alone would be disqualifying. The reaction is opt-in and off by default, so deployments that
merge by hand never reach the path. A merge performed by a human is not observed there at all: the
next merge attempt returns a conflict response, which is routed to the error handler, recognized by
matching the response text against the already-merged phrase, and returned from early, so the success
path never executes. And the pending entry for that kind is dropped once its thirty-minute expiry
passes, so a pull request reviewed overnight and merged the next morning is never seen. The
deployments this option covers are precisely the ones least affected by the problem, since a
deployment that lets the orchestrator merge has already granted it write authority over the outcome.

**Leave tracker state to the operator and bound workspace retention instead.** Adding an age-based or
capacity-based bound to the workspace sweep would stop the disk growth, preserve the documented
boundary exactly as written, and require no tracker write, no new permission, and no new
configuration in the tracker's vocabulary. It was rejected because it treats the visible symptom and
leaves the cause. The issue still never closes, the operator's board still fills with work that
finished weeks ago, and the tracker stops being a usable record of what is outstanding. It also trades
one silent failure for another: a sweep that deletes a workspace on age alone will eventually delete
one an operator was still inspecting, and it will do so without knowing whether the work it is
discarding was finished or abandoned. The bound is still worth having as a backstop for the cases
this decision leaves open, and that is recorded below as a consequence rather than folded in here,
because retention policy is a separate decision with its own tradeoffs and deserves to be argued on
its own terms rather than settled as a side effect of this one.

**Have the coding agent close the issue.** The agent can already transition issues through the agent
tool surface, the tool exists, and the prompt could instruct the agent to close the issue once its
pull request merges. The timing defeats it. The agent exits when it finishes writing code, and the
merge happens later, after a review the agent is not present for. Closing at exit would close the
issue before the work was accepted, and keeping the agent alive until the merge would hold a worker
slot and a token budget open across a human review cycle. The reliability objection that the earlier
decisions raised against agent-driven transitions applies here too, since a tool call is
probabilistic and a close is a visible write, but it is the timing that rules the option out
regardless.

## Consequences

### Positive

- The workflow closes. An issue whose pull request merged reaches a terminal state without a human
  touching it, and the tracker becomes an accurate record of what is outstanding.
- Workspace cleanup starts working as designed for the covered case. The existing sweep needs no
  change, because the issues it was already looking for now actually arrive in a terminal state.
- Coverage does not depend on who performed the merge. A merge by a human, by a forge automation
  rule, or by the orchestrator itself is observed the same way.
- The failure path is visible. A close that cannot happen surfaces on the issue through the same
  escalation mechanism the other reactions use, rather than only in a log line.
- The mechanisms are the existing ones. Reaction lifecycle, cross-restart deduplication, escalation
  actions, and the offline validator are reused rather than reimplemented.
- Misconfiguration is caught before the first run. Every validation rule is a configuration-shape
  check, so a target state that is not terminal, collides with the handoff state, or is missing
  entirely is reported offline.

### Negative

- **Workspace growth is only partly bounded.** The reaction is opt-in, and it covers only issues
  whose managed pull request merged. A deployment that does not configure it, an issue that never
  produced a pull request, an issue whose pull request was closed unmerged, and an issue whose
  transition fails permanently all still retain a workspace directory forever. Bounding retention
  independently of tracker state remains an open decision, and this one narrows the gap rather than
  closing it.
- **Write permission escalates for deployments that adopt it.** Closing an issue needs a write scope
  on the tracker credential, and on the forges it closes the native issue as well. Operators who ran
  with a read-only credential, or with write scope sufficient only for labels and comments, must
  widen it before this reaction can work. The reaction being off by default keeps this a deliberate
  act rather than a surprise on upgrade.
- **A close is not reversible by the orchestrator.** Nothing in this design reopens an issue closed in
  error. A misconfigured target state that is nonetheless valid, for example an abandonment state
  named where a completion state was meant, closes completed work under the wrong label, and the
  validator cannot catch it because it cannot know which member of the terminal list the operator
  considers success.
- **Tracker request volume rises.** Each covered issue adds one transition, and the Jira adapter
  spends two requests on it because it must read the available transitions before executing one. The
  polling that observes the merge adds load of its own for as long as an issue sits in the handoff
  state, which is bounded by the poll interval and its thirty-second floor but is unbounded in
  duration, since a pull request can wait for review indefinitely. Deployments with many
  simultaneously parked issues should set the interval deliberately rather than accept the default.
- **The source-control contract widens again.** Both forge packages that implement it gain the new
  merge fields, and every test double satisfying the contract must carry them. The change is
  mechanical, but a double that leaves the merged indicator at its zero value will silently model a
  pull request that never merges, which is a test that passes while asserting nothing.
- **The native close reason is not selectable.** The GitHub adapter closes an issue with a completed
  reason for any terminal target, including one the operator intends as abandonment. This predates
  the decision and is not made worse by it, but a reaction that closes issues automatically makes it
  visible on many more issues than before.
- **A restart within the recovery gap loses the observation.** Pending-reaction recovery rebuilds
  entries only for issues in the handoff state with run history and workspace activity inside the
  thirty-day lookback. An issue outside that window, or one whose workspace was removed by other
  means, is never re-observed, and its merge goes unnoticed.

## Specification Material Requiring Update

Named by document and topic rather than by section, since the numbering is not stable.

1. A new reaction-contract document under the architecture specification, sibling to the auto-merge
   reaction contract, describing the trigger, the polling model, the terminal-target rule, the
   idempotency latch, and the failure matrix.
2. `06-configuration-specification.md`: the `reactions.merge_completion` block, its fields, their
   defaults, and their validation rules, including the absence of environment indirection and the
   restart-to-apply reload behavior.
3. `07-orchestration-state-machine.md`: the transition into the completion state as a third
   orchestrator-driven tracker write, and its place relative to the handoff transition.
4. `08-polling-scheduling-and-reconciliation.md`: the new reconciliation pass, its interaction with
   the terminal workspace sweep, and the reason this reaction kind has no pending-entry expiry.
5. `11-issue-tracker-integration-contract.md`: the tracker-writes boundary, restated as the three
   orchestrator-written states and the event-versus-judgement distinction that separates them from
   agent-driven mutation.
6. `14-auto-merge-reaction-contract.md`: the relationship between the two kinds, and a note that the
   already-merged path through the error handler does not itself close the issue.
7. `19-failure-model-and-recovery-strategy.md`: the per-error-kind posture, and the absence of a
   continuation fallback as the reason escalation replaces silent degradation.
8. `24-persistence-schema.md`: the new reaction kind value stored in the fingerprint table, and the
   use of the merge commit identifier as the fingerprint.
9. `09-workspace-management-and-safety.md`: the cleanup consequence, and an explicit statement that
   terminal-gated cleanup remains the only mechanism and is still unbounded for the uncovered cases.
10. `docs/workflow-reference.md`: the configuration block in the workflow file syntax reference.

## Confirmation

The decision is validated when all of the following hold:

1. A deployment configuring the reaction sees an issue reach the configured target state after its
   managed pull request merges, whether the merge was performed by the orchestrator, by a human, or
   by a forge automation rule.
2. A deployment that omits the reaction block behaves identically to one running without this
   change, performing no additional tracker write and no additional forge request.
3. The transition fires exactly once per merge. Repeated poll ticks, a process restart between the
   merge and the transition, and an escalation of another reaction kind on the same issue each
   produce one transition in total.
4. The reaction transitions only to the state named by `target_state`, never to another member of
   the terminal-state list.
5. Offline validation rejects a missing target state, a target that is not terminal, a target equal
   to the handoff state, a target that is also an active state, and a poll interval below the floor,
   with no network access.
6. Authentication and payload failures escalate on the first occurrence without consuming the retry
   budget; transport and API failures retry to the configured bound and then escalate; a not-found
   failure stops without escalating.
7. The terminal workspace sweep removes the workspace of an issue closed by this reaction on a
   subsequent pass.
8. A pull request closed without merging, and an issue with no managed pull request, leave the issue
   in the handoff state.
