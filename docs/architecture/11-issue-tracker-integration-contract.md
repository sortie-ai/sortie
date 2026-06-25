## 11. Issue Tracker Integration Contract

### 11.1 Required Operations

A tracker adapter must support these operations:

1. `fetch_candidate_issues()`
   - Return issues in configured active states for the configured project.

2. `fetch_issue_by_id(issue_id)`
   - Return a single fully-populated issue including comments and attachments. Used for
     pre-dispatch revalidation and prompt rendering.

3. `fetch_issues_by_states(state_names)`
   - Used for startup terminal cleanup.

4. `fetch_issue_states_by_ids(issue_ids)`
   - Used for active-run reconciliation.

5. `fetch_issue_states_by_identifiers(identifiers)`
   - Return the current state for each requested issue identifier (human-readable key).
     Issues not found are omitted. Used for startup terminal workspace cleanup.

6. `fetch_issue_comments(issue_id)`
   - Return comments for an issue. Used for continuation runs and the agent workpad pattern.

7. `transition_issue(issue_id, target_state)`
   - Transition an issue to the named target state in the tracker's native workflow system.
   - Used for orchestrator-initiated handoff transitions (ADR-0007).
   - Error semantics: returns normalized error categories per Section 11.4.
   - All errors are non-fatal from the orchestrator's perspective.

### 11.2 Query Semantics

Each tracker adapter defines its own query semantics. The architecture specifies only the
normalized interface and the minimum field set that must be returned (see Section 4.1.1). Adapters
are responsible for translating their native API responses to the normalized issue model.

Pagination is required for candidate issue fetches. Page size default: `50`. Network timeout:
`30000 ms`.

### 11.3 Normalization Rules

Candidate issue normalization should produce fields listed in Section 4.1.1.

Additional normalization details:

- `labels` -> lowercase strings
- `blocked_by` -> derived from inverse relations where relation type is `blocks`
- `priority` -> integer only (non-integers become null)
- `created_at` and `updated_at` -> parse ISO-8601 timestamps

### 11.4 Error Handling Contract

Recommended error categories:

- `unsupported_tracker_kind`
- `missing_tracker_api_key`
- `missing_tracker_project`
- `tracker_transport_error` (transport failures)
- `tracker_auth_error` (authentication/authorization failures)
- `tracker_api_error` (non-200 HTTP or API-level error)
- `tracker_payload_error` (malformed or unexpected response structure)
- `tracker_missing_end_cursor` (pagination integrity error)

Each adapter maps its native errors to these categories.

Orchestrator behavior on tracker errors:

- Candidate fetch failure: log and skip dispatch for this tick.
- Running-state refresh failure: log and keep active workers running.
- Startup terminal cleanup failure: log warning and continue startup.

### 11.5 Tracker Writes (Important Boundary)

Sortie does not require first-class tracker write APIs in the orchestrator.

- Ticket mutations (state transitions, comments, PR metadata) are typically handled by the coding
  agent using tools defined by the workflow prompt.
- Sortie remains a scheduler/runner and tracker reader.
- Workflow-specific success often means "reached the next handoff state" (for example
  `Human Review`) rather than tracker terminal state `Done`.
- The agent tool subsystem (Section 10.4) is part of the agent toolchain rather than
  orchestrator business logic. `tracker_api` executes tracker operations through agent-initiated
  tool calls, not orchestrator-driven writes.

### 11.6 Implemented Tracker Adapters

Three tracker adapters ship today: Jira (`internal/tracker/jira`, Atlassian REST API), GitHub
(`internal/scm/github`, Issues and Labels REST API), and Linear (`internal/tracker/linear`,
GraphQL API). Each normalizes its native responses to the `Issue` model (Section 4.1.1), maps
native errors to the categories in Section 11.4, and registers under its `kind` via `init()`.
The orchestrator core never imports these packages; it resolves them through `internal/registry`.

#### 11.6.1 Linear adapter

The Linear adapter targets Linear's single GraphQL endpoint
(`https://api.linear.app/graphql`). Unlike the Jira and GitHub adapters, which call multiple REST
endpoints, every Linear operation is an HTTP POST of a `{ "query", "variables" }` body to that one
endpoint, built on `internal/httpkit` with no third-party GraphQL library.

**Authentication.** The resolved `tracker.api_key` is sent verbatim in the `Authorization` header
with no `Bearer` prefix and no `email:token` composition. Linear personal API keys carry the
`lin_api_` prefix. The constructor runs a `viewer` credential check before the first poll, so an
invalid key fails construction rather than the first fetch.

**Team scoping.** `tracker.project` is the Linear team key (for example `ENG`), the prefix in the
human identifier `ENG-123`. It is not a Linear project, which is a cross-team container that owns
neither workflow states nor identifiers. Team scoping is required because workflow states are
team-scoped: the read filter is `team: { key: { eq: <project> } }`, and the by-identifier state
batch splits each identifier into its team key and number.

**State model.** Linear sits between Jira's transition-graph workflow and GitHub's
open/closed-plus-labels model: it has team-scoped named states, and every state carries an
immutable category (`triage`, `backlog`, `unstarted`, `started`, `completed`, `canceled`,
`duplicate`). Operators configure state names in `active_states`, `terminal_states`, and
`handoff_state`; the adapter treats the names as opaque strings and preserves their original
casing in `Issue.state`, consistent with the other adapters. The category drives no candidate
selection. At construction the adapter fetches the team's states once, fails when a configured
name is absent from the team, caches the canonical casing of each configured name (the GraphQL
`name: { in: [...] }` filter is case-sensitive), and emits a WARN when a configured `active_states`
entry resolves to a terminal category or a `terminal_states` entry resolves to a non-terminal
category. When `active_states` or `terminal_states` is omitted, the adapter applies the stock
defaults `["Backlog", "Todo", "In Progress"]` and `["Done", "Canceled", "Duplicate"]`.

**Normalization specifics.** Beyond the shared rules in Section 11.3:

- `priority` is a Linear float where `0` means "no priority"; the adapter maps `0` to null and
  `1..4` to a non-null integer.
- `branch_name` comes from Linear's `branchName`, whose prefix is workspace-configurable; the
  adapter stores it as an opaque string and never parses it.
- `assignee` resolves through `displayName`, then `name`, then `email`; only a null assignee maps
  to the empty string, so an assignee carrying just an email resolves to that email.
- `blocked_by` is derived from `inverseRelations` nodes whose `type` equals `blocks`, matching the
  Section 11.3 rule, which was phrased for Linear's native relation model.
- `issue_type` is always empty because Linear has no native issue-type field.
- Comments are fetched only by `fetch_issue_by_id` and `fetch_issue_comments`; Linear returns them
  newest-first, so the adapter re-sorts them ascending by creation time before returning.

**Transport and pagination.** Linear uses Relay-style cursor connections (`first` plus `after`,
with `pageInfo { hasNextPage endCursor }`). The adapter paginates candidate and comment fetches to
exhaustion with a top-level page size of 50 and a 30,000 ms network timeout. A `hasNextPage` of
true with an empty `endCursor` returns `tracker_missing_end_cursor` rather than silently
truncating, mirroring the Jira guard.

**Error classification.** Linear returns application errors inside HTTP 200 bodies, so the adapter
classifies a response by its top-level `errors` array first and falls back to the HTTP status only
when no `errors` array is present. The category mapping keys on `extensions.type`, treats
`extensions.code` as diagnostic only, and special-cases entity-not-found, which arrives under the
generic `invalid input` type and is distinguished by an error message beginning with
`entity not found`. A `RATELIMITED` body code (on HTTP 400 or 429) maps to the retryable
`tracker_api_error`; `feature not accessible` maps to the non-retryable `tracker_auth_error`.

**Write operations.** The adapter implements the three writes the `TrackerAdapter` interface
requires beyond the read set:

- `transition_issue` resolves the configured state name to a team-scoped workflow-state UUID with
  a case-insensitive filter, then applies it through the `issueUpdate` mutation. Linear has no
  transition graph, so any state can move to any state. `handoff_state` and `in_progress_state`
  name Linear workflow states resolved this way at transition time.
- `comment_issue` posts the comment body verbatim as markdown with no ADF-style wrapping, in
  contrast to the Jira adapter.
- `add_label` resolves the label name to its UUID (preferring a team-scoped label over a workspace
  label), creates a team-scoped label when none exists, and attaches it through Linear's additive
  `addedLabelIds` input, so the issue's existing labels are never dropped. A forbidden label
  create maps to `tracker_auth_error`; label errors are non-fatal, and the operator remedy is to
  pre-create the label.

**Operator query filter.** `tracker.query_filter` (Section 5.3.1) is interpreted as a Linear
`IssueFilter` JSON object fragment. The adapter combines the fragment with the team and state
constraints inside the GraphQL `filter` argument rather than appending a string the way the Jira
and GitHub adapters do. See the workflow reference for the operator-facing shape.

