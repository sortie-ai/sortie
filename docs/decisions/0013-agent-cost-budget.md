---
status: accepted
date: 2026-06-09
decision-makers: Serghei Iakovlev
---

# Use cumulative per-issue token counts for the agent cost budget

## Context and Problem Statement

Sortie runs autonomous coding agents, and every run drives LLM calls that consume tokens and
bill real money. The orchestrator already bounds how often it will work an issue: the
`agent.max_sessions` limit counts completed sessions and stops re-dispatching once the count
is reached. That governs the number of attempts, not their cost. A single session can consume
an unbounded number of tokens, so a handful of cheap retries are capped while one runaway
session is not.

The data needed to govern cost already flows through the orchestrator. Agent adapters
normalize each provider's reporting into `input_tokens`, `output_tokens`, `total_tokens`, and
`cache_read_tokens`, and the orchestrator accumulates these counts per session. Nothing acts
on them. There is no ceiling on consumption, and an agent has no way to ask how much budget it
has left.

This decision adds a cost budget denominated in tokens, with two surfaces. The advisory
surface is a tool the agent queries mid-session to decide whether to skip an expensive step,
return partial work, or hand off. The enforcing surface is a hard ceiling the orchestrator
applies before re-dispatch, refusing to re-run an issue whose budget is spent. Because this is
the first cost-control plane in the system, it also settles the questions every later cost
feature inherits: the unit of account, the configuration surface, where cumulative spend is
stored, how the new ceiling coexists with the session ceiling, and what the tool and the
refusal report.

## Decision Drivers

1. **A ceiling on cost, not only on attempts.** The session limit bounds retries; one session
   can still spend without limit. Operators need a cap on what an issue is allowed to cost.
2. **Token prices are an external moving target.** Providers set per-model prices and change
   them on their own schedule. A budget denominated in money would tie Sortie's correctness to
   a pricing surface it neither controls nor can predict.
3. **No runtime dependencies.** Sortie ships as a single statically-linked binary that makes no
   outbound calls for its own bookkeeping. Pricing in money would require a maintained
   per-model price table, forcing either a table that rots between releases or a network fetch
   the deployment model forbids.
4. **One mental model for budgets.** The session limit is a bare integer where `0` means
   unlimited. A token budget that copies that shape, enforced on the same dispatch path, asks
   the operator to learn nothing new.
5. **The advisory and the enforcing surface must agree.** If the agent's view of remaining
   budget and the orchestrator's refusal can diverge, the agent self-regulates against one
   number while the orchestrator blocks on another. Both must report the same spend and the
   same reason.
6. **Cumulative spend needs a home.** Deciding the budget without deciding where the per-issue
   total lives would defer a persistence question to implementation, where it surfaces as a
   stalled change rather than a recorded choice.

## Considered Options

- **Option A.** A budget denominated in tokens, accumulated cumulatively per issue from token
  columns on `run_history`, read by a read-only `cost_budget` tool and enforced by a token
  ceiling on the same pre-dispatch path as the session ceiling.
- **Option B.** A budget denominated in money, backed by a per-model price table embedded in
  the binary or fetched at runtime.
- **Option C.** A budget scoped to a single session, with no cumulative per-issue accounting.
- **Option D.** A cumulative per-issue budget backed by a new per-session token table rather
  than columns on `run_history`.

## Decision Outcome

Chosen option: **Option A**, because it caps real consumption, copies the proven session-limit
model end to end, and keeps the reading tool free of any external dependency. Option B is
deferred rather than rejected; Options C and D are rejected.

### Budget unit: tokens

The unit is the cumulative `total_tokens` reported by the adapter. A money unit is deferred to
a separate future decision.

The budget reads `total_tokens` as the adapter reports it. `cache_read_tokens` is retained for
information but is not a second budget axis, and the budget never recomputes `total_tokens`
from its components. Sortie does not define `cache_read_tokens` as either included in
`total_tokens` or disjoint from it; the orchestrator treats the adapter's `total_tokens` as
authoritative and tracks `cache_read_tokens` in a separate accumulator. The budget inherits
that treatment rather than inventing a competing one.

### Configuration: `agent.max_tokens`

A new integer field, `agent.max_tokens`, sits beside `agent.max_sessions`. `0`, the default,
means unlimited. It mirrors the session limit in every mechanical respect: parsed as an
integer, rejected when negative, overridable through the `SORTIE_AGENT_MAX_TOKENS` environment
variable, and re-read on live reload so a change takes effect at the next retry evaluation. A
single scalar does not warrant a dedicated configuration block.

### Budget scope and storage: cumulative per issue, in `run_history`

The budget covers cumulative spend per issue across every session. That is what an operator
means by a cost cap, and it closes the gap a per-session view leaves open, where each retry
starts a fresh allowance and total spend climbs without limit.

This total was not stored in any existing structure. `run_history` held one row per completed
session but no token counts. `session_metadata` kept a single row per issue and overwrote it
at each session's end, remembering only the latest session rather than the sum.
`aggregate_metrics` was cumulative but global, with no per-issue breakdown.

The decision records per-session token totals on `run_history` and sums them per issue:

- A forward-only migration adds token columns to `run_history`, at minimum `total_tokens` and
  the per-component counts for parity with what `session_metadata` already stores. Rows written
  before the migration read as zero.
- The session-exit path, which already writes the run-history row, fills the new columns from
  the totals it holds at that point.
- A per-issue token sum and a batch "which of these issues are over budget" query are added,
  mirroring the session-count query the session limit already uses.

`run_history` is the right home because it is already the per-session, per-issue ledger: the
session limit already counts its rows, an existing per-issue index already serves these
aggregates, and recording token sums there lets both budgets read from one table.

### Coexistence and precedence with the session limit

Both budgets are independent hard ceilings evaluated before re-dispatch. A re-dispatch is
blocked when either ceiling is reached, so whichever fills first across polling cycles is the
one that fires. When a single evaluation finds both exhausted, the reported and logged reason
names the token budget. That precedence affects only which reason is reported; the block
itself is identical whichever ceiling triggered it.

The token check sits beside the session check on the same pre-dispatch path. When
`agent.max_tokens` is set and the issue's summed tokens reach it, the orchestrator marks the
issue exhausted, releases its claim, drops its retry entry, and declines to re-dispatch,
exactly as the session check does, logged with structured attributes for the used and budgeted
token counts. A failed token query fails open, matching the session check, so a transient
database error never silently strands an issue. Both checks feed the single exhausted-issue
set the dispatcher already consults, and the per-tick rebuild of that set accounts for both
ceilings.

### Tool contract and refusal record

`cost_budget` reads local state and makes no outbound calls, the same read-only class as the
existing `workspace_history` tool, and it registers the same way. It reports domain problems,
such as state that is missing or unreadable, inside its JSON result; a returned error is
reserved for internal faults.

The result reports both ceilings so the agent sees its whole budget picture:

```json
{
  "used_tokens": 0,
  "budget_tokens": 0,
  "remaining_tokens": null,
  "used_sessions": 0,
  "budget_sessions": 0
}
```

- `used_tokens`: cumulative `total_tokens` for the issue across completed sessions, plus the
  session currently running (see Consequences).
- `budget_tokens`: the configured `agent.max_tokens`, where `0` means unlimited.
- `remaining_tokens`: `budget_tokens` minus `used_tokens`, floored at zero, or `null` when the
  budget is unlimited, so the agent can distinguish "no limit" from "nothing left".
- `used_sessions`: completed sessions for the issue. The running session is not counted,
  matching how the session limit counts.
- `budget_sessions`: the configured `agent.max_sessions`, where `0` means unlimited.

`used_sessions` excludes the running session while `used_tokens` includes it. The asymmetry is
deliberate: a session is a discrete unit that is either finished or not, whereas tokens accrue
continuously, and an advisory reading that ignored the running session's spend would be
useless exactly when the agent consults it.

A hard refusal must leave the same account the advisory tool would show. When the orchestrator
blocks a re-dispatch, it records a structured, machine-readable reason, naming which ceiling
fired together with the used and budgeted tokens and sessions, rather than only a log line.
The advisory reading and the enforced refusal then explain the same block in the same terms.

### Considered Options in Detail

**Option B, a money unit, deferred.** Money needs a per-model price table kept current against
prices Sortie does not set. Embedding the table stales the binary between releases; fetching it
at runtime is exactly the outbound dependency the single-binary model rejects. Money also needs
per-model token attribution the system does not record: the orchestrator counts requests per
model, not tokens per model, so money is a strictly larger change than tokens rather than a
relabeling of one. Tokens, by contrast, are provider-neutral, already normalized at the adapter
boundary, already accumulated, and already the unit operators know from the session limit.
Money can return as its own decision once a pricing strategy and per-model token attribution
exist to support it.

**Option C, a per-session budget, rejected.** A per-session cap leaves cumulative spend
unbounded: each retry begins a fresh allowance, so an issue can cost arbitrarily much across
enough sessions. Bounding that is the entire purpose of this decision.

**Option D, a separate per-session token table, rejected.** `run_history` is already the
per-session, per-issue ledger, the session limit already counts its rows, and its per-issue
index already serves these aggregates. A second table duplicates that ledger and splits the
two budgets across two stores for nothing in return.

A dedicated `cost` configuration block was also considered and rejected: a single integer does
not justify it, and with money deferred there is no richer pricing structure to hold.

## Consequences

### Positive

- A runaway session is bounded. The cost gap the session limit cannot reach is closed.
- Operators carry one mental model across both budgets: a bare integer, `0` for unlimited,
  enforced on the same dispatch path.
- Both budgets read one table and feed one exhausted-issue set. No second store, no second gate.
- The reading tool stays free of outbound calls, so the single-binary deployment is untouched.
- The agent's advisory view and the orchestrator's enforcement report the same numbers and the
  same reason.

### Negative

- **A schema migration is required.** The migration adds token columns to `run_history`, the
  session-exit path must populate them, and restart recovery must tolerate rows that predate
  the migration. Spend recorded before the migration reads as zero and is invisible to the
  budget.
- **The session-metadata write cadence changes.** This consequence carries the most weight. The
  tool runs in a separate, read-only sidecar process with its own database connection and no
  access to the orchestrator's memory, and `session_metadata` is written only when a session
  ends. A tool reading only the database therefore cannot see the spend of the session
  currently running; on a first session it would always read zero used tokens, which strips the
  advisory surface of its purpose. The running session's spend must be made visible to the
  tool. The intended mechanism writes `session_metadata` incrementally during the session,
  throttled and driven by the usage events already arriving, so the tool can add the running
  session's recorded total to the per-issue sum, keyed to the session identifier the tool
  already receives so a stale earlier session is never added. No double count arises, because
  the running session reaches `run_history` only when it ends. This turns `session_metadata`
  from a written-once-at-exit record into a live one, a change to an existing write path rather
  than an additive one.
- **No exhaustion metric in the first version.** The session limit emits no metric when it
  trips, and tool calls are already counted generically by `sortie_tool_calls_total`. For parity
  and a smaller surface, the token budget emits no exhaustion metric either. If such a counter
  is wanted later, it should be added for both budgets together, across the metric definitions,
  the example dashboard, and its coverage test, so the two budgets stay symmetric.
- **Documentation owes an update on acceptance.** Accepting this decision obliges edits to the
  configuration reference for `agent.max_tokens`, the architecture specification covering the
  effort budget, the persistence schema, and the agent-tool catalog, and the workflow
  reference. The edits are mechanical, but skipping them leaves the specification contradicting
  the code.

## Confirmation

The decision is satisfied when all of the following hold:

1. `agent.max_tokens` parses as an integer, defaults to `0`, rejects negative values with a
   configuration error that names the field, honors the `SORTIE_AGENT_MAX_TOKENS` override, and
   re-applies on live reload.
2. A forward-only migration adds the token column or columns to `run_history`, and the
   session-exit path populates `total_tokens`, and the per-component counts when present, as it
   writes the run-history row.
3. A per-issue token sum and a batch over-budget query exist and mirror the session-count query
   the session limit uses.
4. Before re-dispatch, the orchestrator blocks an issue whose summed tokens have reached a
   non-zero `agent.max_tokens`: it marks the issue exhausted, releases the claim, drops the
   retry entry, and fails open on a query error. The per-tick rebuild of the exhausted-issue set
   accounts for both ceilings.
5. `cost_budget` reads only local state, makes no outbound calls, returns the five-field result
   with `remaining_tokens` reported as `null` under an unlimited budget, and reports domain
   problems inside the result rather than as a returned error.
6. While a session is running and before its `run_history` row exists, the tool reports a
   `used_tokens` value that includes the running session's spend.
7. A server-side refusal records a machine-readable reason that matches the advisory reading,
   and when both ceilings are exhausted in one evaluation the reason names the token budget.
