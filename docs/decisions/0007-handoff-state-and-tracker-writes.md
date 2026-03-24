---
status: accepted
date: 2026-03-23
decision-makers: Serghei Iakovlev
---

# Use Handoff State Transitions to Signal Agent Completion

## Context and Problem Statement

The orchestrator's continuation retry mechanism creates an unbounded dispatch loop when an
agent completes work successfully but the issue remains in an active tracker state. The loop
occurs because:

1. The agent finishes its turns and exits normally.
2. The orchestrator schedules a continuation retry (architecture Section 7.3, `Worker Exit (normal)`).
3. On retry, the issue is still active in the tracker (the agent created a PR but cannot
   transition the ticket).
4. The orchestrator dispatches again. The agent finds no remaining work, exits normally.
5. Go to step 2.

The architecture spec (Section 11.5) explicitly states: "Sortie does not require first-class
tracker write APIs in the orchestrator" and "Sortie remains a scheduler/runner and tracker
reader." However, the spec also acknowledges (Section 1): "A successful run may end at a
workflow-defined handoff state (for example `Human Review`), not necessarily `Done`," and
(Section 11.5): "Workflow-specific success often means 'reached the next handoff state'
(for example `Human Review`) rather than tracker terminal state `Done`."

This creates a gap: the orchestrator recognizes the concept of handoff states but has no
mechanism to signal completion back to the tracker, relying entirely on the agent or a human
to transition the issue out of an active state. When neither does so promptly, the
continuation retry produces wasted agent invocations, token spend, and API load.

The system needs a feedback channel from the orchestrator to the tracker — or a bounded
fallback — to break the cycle.

## Decision Drivers

1. **Correctness over cost.** An unbounded loop wastes tokens and API quota, but more
   importantly it produces noise: the agent repeatedly runs against completed work,
   potentially making unwanted changes or creating duplicate PRs.
2. **Adapter boundary preservation.** Any tracker write mechanism must respect the adapter
   interface pattern (ADR-0003). Tracker-specific transition mechanics (Jira workflow
   transitions, Linear GraphQL mutations, GitHub Projects column moves) must be encapsulated
   in adapter packages, not in orchestration logic.
3. **Backward compatibility.** Existing deployments where the agent or a human manages
   transitions must continue to work without configuration changes. The solution must be
   opt-in.
4. **Graceful degradation.** If the orchestrator cannot perform a tracker transition (network
   error, permission denied, misconfigured state name), the system must fall back to existing
   behavior (continuation retry), not fail permanently.
5. **Defense in depth.** No single mechanism should be the sole protection against runaway
   agent execution. A complementary effort budget provides a hard ceiling independent of
   transition success.

## Considered Options

- **Option A: Orchestrator handoff state transition** — Add `tracker.handoff_state` config
  and `TrackerAdapter.TransitionIssue` operation. On normal worker exit with the issue still
  active, the orchestrator transitions it to the handoff state.
- **Option B: Agent-initiated transitions** — Rely on the agent to transition issues using
  the `tracker_api` client-side tool (architecture Section 10.4). No orchestrator writes.
- **Option C: Pure effort budget (circuit breaker)** — Add `agent.max_sessions` to cap the
  number of worker sessions per issue. No tracker writes by the orchestrator.
- **Option D: `.sortie/status` file convention only** — The agent writes a status file
  (architecture Section 21) to signal completion. The orchestrator reads it and suppresses
  continuation retries.

## Decision Outcome

Chosen option: **Option A (orchestrator handoff state transition)**, combined with
**Option C (per-issue effort budget)** as defense-in-depth, because this deterministically
breaks the continuation retry loop for configured deployments while providing a hard ceiling
against failure modes the transition alone cannot handle. Options B and D remain
complementary capabilities but are insufficient as sole solutions.

### Why Option A

The handoff state transition solves the root cause: the orchestrator lacks a feedback channel.
By adding a single optional config field (`tracker.handoff_state`) and one new adapter
operation (`TransitionIssue`), the orchestrator can signal to the tracker that agent work is
complete and the issue is ready for human review.

This aligns with established ITSM workflow patterns where automated systems move issues to
intermediate states (e.g., "Pending Review", "Awaiting CAB Approval") rather than to terminal
states. The handoff state is explicitly _not_ a terminal state — it is a pause point where a
human evaluates the agent's output and either moves the issue back to an active state for
further work or forward to a terminal state.

**Jira implementation.** Jira's workflow engine uses named transitions (not direct status
assignment). The adapter's `TransitionIssue` implementation:

1. `GET /rest/api/3/issue/{issueIdOrKey}/transitions` — fetch available transitions.
2. Find the transition whose target status name (`to.name`) matches the configured
   `handoff_state` (case-insensitive comparison).
3. `POST /rest/api/3/issue/{issueIdOrKey}/transitions` with `{"transition": {"id": "<id>"}}`.

OAuth scopes required: `write:jira-work` (classic) or `write:issue:jira` (granular). This is
an escalation from the current read-only scopes (`read:jira-work`). Operators adopting
`handoff_state` must update their API token permissions.

**Matching by target status, not transition name.** Jira transition names are user-defined
labels (e.g., "Send to Review", "Move to QA") that may differ from the target status name
(e.g., "Human Review"). The adapter matches against `transition.to.name` — the actual
destination status — to avoid workflow-specific naming fragility.

**Other trackers.** Linear: GraphQL `issueUpdate` mutation with `stateId`. GitHub Projects
v2: `updateProjectV2ItemFieldValue` mutation on the Status field. Each adapter encapsulates
its native transition mechanism behind the same `TransitionIssue` signature.

### Why Option C as Defense-in-Depth

The handoff transition handles the happy path (~90% of cases). The effort budget handles
failure modes that the transition cannot:

- `TransitionIssue` fails repeatedly (permissions, workflow conditions, network).
- `handoff_state` is misconfigured (state does not exist in the tracker).
- Human moves the issue back to active without reviewing, creating a human-mediated loop.
- Agent is stuck in a non-productive cycle but does not trigger error-path retry limits.

`agent.max_sessions` provides a hard ceiling: after N completed sessions for the same issue,
the orchestrator releases the claim and logs a warning instead of re-dispatching. This is
analogous to an HTTP request timeout — you do not plan for the server to hang, but the timeout
is non-negotiable.

### Why Not Option B Alone (Agent Transitions)

The agent _can_ transition issues when the `tracker_api` tool is available (Section 10.4),
and some deployments will prefer this model. However:

- Not all agents use the `tracker_api` tool. The prompt may not instruct the agent to
  transition issues.
- Agent tool calls are probabilistic. The agent may forget, fail, or choose not to call the
  tool.
- The `tracker_api` tool is defined in Milestone 8 (8.6) — it does not exist yet. The
  handoff state is needed for Milestone 7 E2E testing.
- Agent-initiated transitions couple workflow correctness to prompt engineering quality. The
  orchestrator should have a deterministic fallback.

Agent-initiated transitions and orchestrator handoff transitions are complementary: if the
agent already transitioned the issue before exiting, the orchestrator observes a non-active
state and skips the handoff transition entirely.

### Why Not Option D Alone (Status File)

The `.sortie/status` file (architecture Section 21) is an advisory signal: "blocked" or
"needs-human-review." It suppresses continuation retries but does not communicate completion
to the tracker. The issue remains in an active state in the tracker, invisible to
non-orchestrator tools (dashboards, reports, team boards). The handoff transition moves the
issue to a visible handoff state in the tracker, maintaining tracker-as-source-of-truth
semantics.

`.sortie/status` remains useful as an agent-to-orchestrator signal that refines handoff
behavior:

| `.sortie/status` value | Worker exit | Orchestrator action              |
| ---------------------- | ----------- | -------------------------------- |
| `needs-human-review`   | Normal      | Transition to `handoff_state`    |
| `blocked`              | Normal      | No transition, no continuation   |
| absent or unknown      | Normal      | Transition to `handoff_state`    |
| (any)                  | Error       | Standard error retry, no handoff |

### Considered Options in Detail

**Option A: Orchestrator handoff state transition.** Adds a `TransitionIssue` operation to
the `TrackerAdapter` interface and a `tracker.handoff_state` config field. On normal worker
exit, if the issue is still in an active state and `handoff_state` is configured, the
orchestrator calls `TransitionIssue`. On success, the continuation retry is skipped. On
failure, the system degrades to existing continuation retry behavior. Pros: deterministic,
works without prompt cooperation, visible in tracker. Cons: requires write permissions on the
tracker API token, adds one method to the adapter interface, and the transition may fail if
the Jira workflow does not permit it from the current state.

**Option B: Agent-initiated transitions.** The agent transitions the issue using the
`tracker_api` tool during its session. The orchestrator remains read-only. Pros: no interface
change, no additional permissions, the agent has richer context for deciding the target state.
Cons: probabilistic (agent may not call the tool), requires the `tracker_api` tool to exist
(Milestone 8), couples correctness to prompt quality, does not work when the agent crashes
before reaching the transition step.

**Option C: Pure effort budget.** Adds `agent.max_sessions` to cap the number of dispatch
cycles per issue. After N sessions, the orchestrator releases the claim regardless of tracker
state. Pros: simple, no tracker writes, bounded by definition. Cons: does not communicate
completion to the tracker (issue stays active), the budget is a blunt instrument (correct N
depends on issue complexity), and N sessions of wasted work still run before the circuit opens.

**Option D: `.sortie/status` file convention.** The agent writes a status file to signal
completion. The orchestrator reads it and suppresses retries. Pros: no tracker API changes,
agent-driven, already specified in Section 21. Cons: depends on agent cooperation (same as
Option B), not visible in the tracker, does not solve the case where the agent exits without
writing the file (crash, timeout, max_turns exhausted).

## `TransitionIssue` Contract

```
TransitionIssue(ctx context.Context, issueID string, targetState string) error
```

**Semantics:**

- Maps `targetState` to the tracker's native transition mechanism.
- For Jira: fetches available transitions, finds the one whose target status name matches
  `targetState` (case-insensitive), and executes it.
- Returns `nil` on success.
- Returns a `*TrackerError` on failure, using the existing error taxonomy (Section 11.4):
  - `tracker_transport_error`: network failure during the API call.
  - `tracker_auth_error`: insufficient permissions (missing `write:jira-work` scope).
  - `tracker_api_error`: Jira returned a non-success HTTP status (400, 409, 422).
  - `tracker_not_found`: the issue does not exist (404).
  - `tracker_payload_error`: no available transition leads to `targetState` from the current
    issue state (the Jira workflow does not permit this transition).
- The caller (orchestrator) treats all errors as non-fatal: log and degrade to continuation
  retry.

**Adapter implementations:**

| Adapter          | Mechanism                                                                        | Notes                                                                                    |
| ---------------- | -------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| Jira             | `GET /issue/{id}/transitions` → match `to.name` → `POST /issue/{id}/transitions` | Two API calls per transition. Transition names are user-defined; match by target status. |
| File             | Update `State` field in memory                                                   | For testing. In-memory only; file on disk is not modified.                               |
| (Future: Linear) | `issueUpdate` GraphQL mutation with `stateId`                                    | Single mutation. State matched by name to ID.                                            |
| (Future: GitHub) | `updateProjectV2ItemFieldValue` mutation                                         | Status is a single-select custom field.                                                  |

## `tracker.handoff_state` Config Field

- **Type:** string, optional.
- **Default:** absent (no handoff transition; continuation retry works as today).
- **Location:** `tracker` config object (Section 5.3.1).
- **Validation rules:**
  - When set, must be a non-empty string after `$VAR` resolution.
  - Must not appear in `tracker.active_states` (would cause immediate re-dispatch after
    handoff, defeating the purpose).
  - Must not appear in `tracker.terminal_states` (handoff is not terminal; the issue may
    return to active).
- **Supports:** `$VAR` environment indirection.
- **Dynamic reload:** changes take effect for future worker exits, not in-flight sessions.

## `agent.max_sessions` Config Field (Defense-in-Depth)

- **Type:** integer, optional.
- **Default:** `0` (unlimited — no effort budget enforced).
- **Location:** `agent` config object (Section 5.3.5).
- **Semantics:** maximum number of completed worker sessions for a single issue before the
  orchestrator stops re-dispatching it. Counted from `run_history` entries for the issue.
- **Enforcement point:** `HandleRetryTimer`, before dispatch eligibility check. If
  `count(run_history where issue_id = X) >= max_sessions`, release the claim and log a
  warning.
- **Dynamic reload:** changes take effect for future retry timer evaluations.

## Failure Semantics

**Transition failure degradation path:**

1. Worker exits normally. Issue is still in an active state. `handoff_state` is configured.
2. Orchestrator calls `TransitionIssue(ctx, issueID, handoffState)`.
3. **Success:** Skip continuation retry. Issue is now in `handoff_state` (not active), so
   future poll ticks will not dispatch it.
4. **Failure:** Log the error at WARN level. Schedule a standard continuation retry
   (attempt 1, 1s delay). The next retry will re-check: if the issue is still active, the
   worker runs again, and on its next normal exit, the orchestrator retries the transition.
   If the issue was manually transitioned in the meantime, the retry finds a non-active state
   and releases the claim.

This degradation is bounded by `agent.max_sessions` when configured, preventing infinite
loops from persistent transition failures.

## Interaction with Existing Mechanisms

**Continuation retry (Section 7.3).** `handoff_state` replaces the continuation retry on
the normal-exit happy path. When `handoff_state` is not configured or `TransitionIssue`
fails, the existing continuation retry behavior is unchanged.

**Reconciliation (Section 8.5).** No change. Reconciliation checks tracker state for running
issues. If `handoff_state` is not in `active_states`, reconciliation will stop workers for
issues that have been transitioned to `handoff_state` — this is correct behavior.

**`.sortie/status` (Section 21).** The status file refines handoff behavior as described in
the table above. "blocked" suppresses both transition and retry; "needs-human-review"
triggers transition; absent/unknown defaults to transition.

**`tracker_api` tool (Section 10.4).** If the agent already transitioned the issue during
its session, the orchestrator's post-exit state check sees a non-active state and skips both
the handoff transition and the continuation retry — no conflict.

## Spec Sections Requiring Update

1. **Section 5.3.1** — Add `handoff_state` to `tracker` object field list.
2. **Section 5.3.5** — Add `max_sessions` to `agent` object field list.
3. **Section 6.4** — Config cheat sheet updates for both fields.
4. **Section 7.1** — Document handoff transition after normal worker exit.
5. **Section 7.3** — New trigger: `Worker Exit (normal)` + active state → handoff transition.
6. **Section 11.1** — `TransitionIssue` as the seventh `TrackerAdapter` operation.
7. **Section 11.5** — Update boundary: "Sortie performs handoff transitions when configured;
   full tracker writes remain agent responsibility via `tracker_api`."
8. **Section 14.2** — Transition failure recovery: degrade to continuation retry.
9. **Section 16.6** — Update Worker Exit pseudocode.

## Consequences

### Positive

- Breaks the unbounded continuation loop deterministically for the configured happy path.
- Visible in the tracker: issues move to a named handoff state, appearing on team boards
  and in reports.
- Opt-in: zero behavior change for existing deployments that omit `handoff_state`.
- Graceful degradation: transition failure falls back to existing behavior.
- Defense-in-depth: `max_sessions` provides a hard ceiling independent of transition success.
- Aligns with established ITSM patterns where automated systems hand off to human review.

### Negative

- **Write permissions required.** Operators using `handoff_state` must grant write scopes
  to the tracker API token. For Jira: `write:jira-work` (classic) or `write:issue:jira`
  (granular). This is a permission escalation from the current read-only posture.
- **Jira workflow dependency.** The handoff transition requires the Jira workflow to permit
  a transition from the current state to `handoff_state`. If the workflow is restrictive
  (e.g., no transition from "In Progress" to "Human Review"), the transition fails silently
  and degrades to continuation retry. Operators must configure their Jira workflows to allow
  the handoff path.
- **Two API calls per transition (Jira).** The `GET transitions` + `POST transition` pattern
  adds two Jira API calls per normal worker exit. At typical orchestrator throughput
  (~10 concurrent agents, ~minutes per session), this is negligible. At extreme scale,
  the additional API calls should be monitored against Jira rate limits.
- **Interface extension.** `TransitionIssue` is a new required method on `TrackerAdapter`.
  All existing adapter implementations must be updated. Since only two adapters exist (Jira
  and File), the migration cost is minimal.
- **`max_sessions` requires `run_history` query.** The effort budget check in
  `HandleRetryTimer` requires counting completed sessions from SQLite. This is a single
  `SELECT COUNT(*)` query, well within SQLite's performance profile.
