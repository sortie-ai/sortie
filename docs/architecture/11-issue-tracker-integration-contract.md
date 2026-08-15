## 11. Issue Tracker Integration Contract

### 11.1 Required Operations

A tracker adapter must support these operations:

1. `fetch_candidate_issues()`
   - Return issues in configured active states for the configured project.

2. `fetch_issue_by_id(issue_id)`
   - Return a single fully-populated issue including comments and attachments. Used for
     pre-dispatch revalidation and prompt rendering.

3. `fetch_issues_by_states(state_names)`
   - Return issues in the specified states. The GitHub, Gitea, GitLab, and file adapters compare
     state names case-insensitively in process. The Jira adapter sends each name verbatim into a
     JQL `status IN (...)` clause, leaving matching to the server. The Linear adapter resolves
     each name against the canonical casing of the team's workflow states, read once at
     construction: a name matching one of those states resolves case-insensitively, and a name
     matching none is sent as written and compared case-sensitively (§11.6.1).
   - No orchestrator caller and no agent tool invokes this operation. Adapters implement it to
     satisfy the `TrackerAdapter` interface; removing it would be an interface change, not an
     adapter change.

4. `fetch_issue_states_by_ids(issue_ids)`
   - Return the current state for each requested issue ID. Issues not found are omitted (not an
     error).
   - Callers: active-run reconciliation, the worker's per-turn state refresh, startup pending-
     reaction recovery, merge-completion reconciliation, and the exit-time verification read that
     precedes an orchestrator-initiated handoff transition.

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

The orchestrator writes tracker state at exactly three points in an issue's life, each one a
single named state drawn from configuration, each one corresponding to an event the orchestrator
itself observed:

- The in-progress state (`tracker.in_progress_state`), at dispatch, when that field is
  configured and the dispatch drives issue state (§11F excludes the read-only and read-write
  label-command postures).
- The handoff state (`tracker.handoff_state`), on a normal worker exit when that field is configured,
  the issue is still in an active tracker state, the exit is not a blocked soft stop, and the
  dispatch drives issue state. Only where those four conditions already select the handoff path is
  the run's frozen `tracker.handoff_evidence` policy consulted as a fifth condition. `Work observed`
  permits the write; `absence of work observed` withholds it; `evidence not determinable` permits it
  under `observed` and withholds it under `strict`; and `off` computes no verdict and preserves the
  four-condition decision. This condition can suppress a write and can never cause one. A withheld
  outcome makes no handoff transition or pre-write verification call, leaves the issue in its active
  state, and follows the failure and retry disposition in §14.2.

  A write that the evidence policy permits is still preceded by a terminal test against the freshest
  available observation of the issue's state and, when `tracker.terminal_states` is non-empty and
  that observation is not already terminal, by one `fetch_issue_states_by_ids` verification read
  immediately before the write. A terminal result from either the observation or the verification
  read suppresses the write. With no terminal states configured no value can classify as terminal,
  so the verification read is skipped.
- The merge-completion target state (`reactions.merge_completion.target_state`), when a
  Sortie-managed pull request merges while the linked issue is still parked in
  `tracker.handoff_state`, independently of who or what performed the merge. Active only when
  `reactions.merge_completion.provider` is configured. See §11G for the full contract.

The orchestrator writes no other tracker state. Beyond these three writes, it posts tracker
comments and applies escalation labels, none of which carries state semantics: the dispatch
comment (`tracker.comments.on_dispatch`), the worker-exit completion and failure comments
(`tracker.comments.on_completion`, `tracker.comments.on_failure`), the auto-merge success comment
(§11C), a reaction escalation label or comment when a reaction's retry budget is exhausted or a
non-retryable error occurs, and the primary-dispatch parking label applied when the consecutive
handoff-absence ceiling is reached (§14.2). None of these is a state transition.

Free-form ticket mutation falls outside these three writes and stays with the coding agent, which
mutates tickets through the `tracker_api` tool subsystem (Section 10.4), part of the agent
toolchain rather than orchestrator business logic.

The distinction that governs this boundary is not between reading and writing. It is between a
write that reports an event the orchestrator observed and a write that expresses a judgment
about the work. The orchestrator makes only the first kind: a case that requires judgment, such
as choosing between a completion state and an abandonment state for a pull request closed
unmerged, falls outside all three writes above and remains the operator's or the coding agent's
to make.

Workflow-specific success often means "reached the next handoff state" (for example
`Human Review`) rather than tracker terminal state `Done`.

### 11.6 Implemented Tracker Adapters

The following tracker adapters ship today:

- Jira, over the Atlassian REST API.
- Linear, over its GraphQL API.
- GitHub, Gitea, and GitLab, each over its own REST API. On a forge the tracker role and the
  source-control role share one integration, so the same credential and host serve both. See
  ADR-0016.

Each normalizes its native responses to the `Issue` model (Section 4.1.1), maps native errors to
the categories in Section 11.4, and registers itself under its `kind` at initialization. The
orchestrator core never depends on an adapter directly; it resolves one by `kind` through the
adapter registry, which is what keeps the core free of vendor vocabulary.

**Label-state derivation, shared by GitHub, Gitea, and GitLab.** None of the three forges has a
native Sortie-recognized workflow state: each exposes only an open/closed status plus free-form
labels, so all three derive issue state from configured labels through one rule. Operators
configure label names in `active_states`, `terminal_states`, and `handoff_state`; each adapter
lowercases them and compares issue labels case-insensitively. State derivation scans the
configured active labels, then the terminal labels, then the handoff label, in that order, and
takes the first match. An issue carrying more than one configured state label logs one WARN naming
every matched label and the issue identifier, and keeps the first match. An issue with no
configured state label falls back to the first active label when the platform's native status
reports open (`opened` on GitLab) and to the first terminal label when it reports closed. When
`active_states` or `terminal_states` is omitted or empty, all three adapters apply the same
internal defaults, `["backlog", "in-progress", "review"]` and `["done", "wontfix"]`, for this
label-to-state derivation. These defaults are an adapter-internal derivation fallback, not a
substitute for the workflow configuration: the orchestrator gates dispatch and reconciliation on
the workflow's `tracker.active_states` and `tracker.terminal_states`, which an operator sets to the
labels that should be treated as active or terminal. The fallback list is what
`tracker.handoff_state` and `tracker.in_progress_state` are checked against in the dispatch
preflight when the matching workflow list is empty, so a handoff target that belongs to the
fallback is rejected whether or not the workflow spells the list out. The GitHub, Gitea, and GitLab
subsections below record only where an adapter's native open/closed spelling or label handling
diverges from this shared rule.

#### 11.6.1 Linear adapter

The Linear adapter targets Linear's single GraphQL endpoint
(`https://api.linear.app/graphql`). Unlike the Jira and GitHub adapters, which call multiple REST
endpoints, every Linear operation is an HTTP POST of a `{ "query", "variables" }` body to that one
endpoint, built on the shared HTTP client with no third-party GraphQL library.

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
name is absent from the team, caches the canonical casing of every workflow state the team
defines (the GraphQL `name: { in: [...] }` filter is case-sensitive), and emits a WARN when a
configured `active_states` entry resolves to a terminal category or a `terminal_states` entry
resolves to a non-terminal category. When `active_states` or `terminal_states` is omitted, the
adapter applies the stock defaults `["Backlog", "Todo", "In Progress"]` and
`["Done", "Canceled", "Duplicate"]`.

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

#### 11.6.2 Gitea adapter

The Gitea adapter targets the REST API v1 of a self-hosted Gitea instance, built on
the shared HTTP client with no third-party Gitea client library. Gitea has no hosted tier, so
`tracker.endpoint` is required and carries the instance base URL; the adapter trims a trailing
slash, appends `/api/v1`, and tolerates an endpoint that already ends in `/api/v1`. Its wire model
is close to the GitHub adapter's, issues plus labels plus an open/closed status, and it diverges
where Gitea's API differs.

**Authentication.** The resolved `tracker.api_key` is a Gitea access token, sent verbatim in the
`Authorization` header under the `token` scheme (`Authorization: token <key>`), not a `Bearer`
prefix, so surrounding whitespace fails authentication. The constructor runs a two-call preflight
before the first poll: a `GET /user` credential check followed by a `GET /repos/{owner}/{repo}`
project check. A config error (401, 403, or 404) fails construction immediately, while a transient
error is retried with a bounded backoff, so a misconfiguration surfaces at startup rather than on
the first fetch.

**Repository scoping.** `tracker.project` is the repository in `owner/repo` form (for example
`sortie-ai/sortie`), exactly one slash with non-empty halves. Every read and write route is scoped
to that repository. The adapter uses no global issue id: it addresses issues by their
repository-scoped index (the Gitea `number`) and qualifies each issue's `display_id` as
`owner/repo#N`.

**State model.** Gitea has neither Jira's transition graph nor Linear's named workflow states. It
offers a native open/closed status plus free-form repository labels, so the adapter models Sortie
state with labels, closest to the GitHub adapter, following the shared label-state derivation rule
above. Its native open and closed status values are `open` and `closed`.

**Normalization specifics.** Beyond the shared rules in Section 11.3:

- `id` and `identifier` are both the repository-scoped index as a string; Gitea's global issue id is
  never read.
- `priority` is always null, because Gitea issues carry no priority field.
- `parent` is always null, because Gitea has no sub-issue relationship, and no parent request is
  issued.
- `blocked_by` is read from the issue dependencies route (`/issues/{index}/dependencies`), the Gitea
  form of the inverse `blocks` relation described in Section 11.3.
- `branch_name` comes from the issue `ref` field and is stored as an opaque string.
- `assignee` is the first entry of the issue's assignee list. Pull requests are skipped on every
  list route, so they never enter the candidate set.

**Transport and pagination.** Gitea paginates with `Link` response headers rather than cursors or
page counts, so the adapter follows the header to exhaustion with a page size of 50 and a 30,000 ms
network timeout. An absent `Link` header is the normal end of results, never a missing-cursor error.
Gitea sends no `ETag`, so the adapter issues no conditional requests. Gitea has no built-in rate
limiting; a 429 can arrive only from a fronting proxy, and the adapter surfaces the proxy's
`Retry-After` in a log while leaving retry backoff to the orchestrator.

**Error classification.** The adapter maps HTTP status to the Section 11.4 categories from the
uniform Gitea error envelope (`{ "message", "url" }`), reading only `message` for a bounded
diagnostic snippet and never echoing the token. 401 and 403 map to `tracker_auth_error`, 404 to a
not-found result, 400, 412, and 422 to `tracker_payload_error`, 423 (a locked or archived
repository) and 429 to `tracker_api_error`, and 5xx and transport failures to
`tracker_transport_error`. The 412 and 423 statuses extend the set the GitHub adapter classifies.

**Write operations.** The adapter implements the three writes the `TrackerAdapter` interface
requires beyond the read set, composing each from Gitea's label and issue-edit routes because Gitea
has no transition endpoint:

- `transition_issue` rejects a target that is not a configured active, terminal, or handoff label
  before any write. Otherwise it removes the current state label by id, resolves or creates the
  target label and attaches it by id, and reconciles native status: a terminal target closes an open
  issue, and an active target reopens a closed one. Every step is idempotent, so a partial failure
  converges on retry. `handoff_state` and `in_progress_state` name labels applied through this same
  path, the latter driven by the orchestrator's dispatch-time transition.
- `comment_issue` posts the text verbatim as a Markdown comment.
- `add_label` resolves or creates the label by lowercased name and attaches it by id additively, so
  existing labels are preserved. A label is attached by id, never by name, because Gitea silently
  ignores an unknown name on the attach route. A configured or attached label that does not exist is
  created with a fixed neutral color, since Gitea rejects a label create without one.

**Operator query filter.** `tracker.query_filter` (Section 5.3.1) is a URL query fragment for the
repository issue-list route, parsed with `url.ParseQuery`. It merges into the open-issue listings
that back candidate polling and not into the closed-issue listing used for terminal-state cleanup,
so an operator filter never hides a terminal issue from reconciliation. The adapter reserves the
four keys it owns (`state`, `type`, `page`, and `limit`) and rejects a fragment naming any of them;
any other key merges through. It never pushes the configured state labels into Gitea's `labels`
query parameter, whose AND semantics and silent drop on an unresolvable name make it unsafe for
correctness-critical filtering, so state filtering stays client-side. See the workflow reference for
the operator-facing shape and the diagnostics the adapter emits for unrecognized or unresolved
filter keys.

#### 11.6.3 GitLab adapter

The GitLab adapter targets the REST API v4 of GitLab.com or a self-managed instance, built on
the shared HTTP client with no third-party GitLab client library. `tracker.endpoint` carries the
instance base URL and is optional: it defaults to the GitLab.com host, which a self-managed
deployment overrides. The adapter trims a trailing slash, appends `/api/v4`, and tolerates an
endpoint that already ends in `/api/v4`. Its wire model is closest to the GitHub and Gitea adapters,
issues plus labels plus an opened/closed status, and it diverges where GitLab's API differs. The
package belongs to the forge family, one package per forge per ADR-0016, because GitLab supplies
code review and CI from the same API surface under the same authentication and project addressing.

**Authentication.** The resolved `tracker.api_key` is a GitLab access token, personal, project, or
group, sent verbatim in the `PRIVATE-TOKEN` header rather than through an `Authorization` scheme, so
surrounding whitespace fails authentication. The constructor runs a three-part preflight before the
first poll. A token introspection call reports the token's scopes, active flag, revocation flag, and
expiry; it is advisory and never blocks construction, because its only job is to sharpen the
diagnostic on a later failure. A project read follows as the authoritative gate. When any state
label is configured, a paginated read of the project label catalog closes the preflight. A config
error fails construction immediately, while a transient error is retried with a bounded backoff, so
a misconfiguration surfaces at startup rather than on the first fetch. GitLab answers an
inaccessible project and a missing project with an identical 404, so the not-found message reports
whether introspection authenticated the token, which is what separates a wrong project from an
under-scoped credential.

**Project scoping.** `tracker.project` is either a numeric project ID or the project's full
namespace path, percent-encoded once by the adapter. GitLab nests subgroups to any depth, so the
path carries any number of slashes and the adapter enforces no `owner/repo` grammar, unlike the
GitHub and Gitea adapters. The adapter uses no instance-global issue id: it addresses issues by
their project-scoped `iid` and qualifies each issue's `display_id` from the server-computed
reference, falling back to `<project>#<iid>` when that reference is empty.

**State model.** GitLab has neither Jira's transition graph nor Linear's named workflow states. It
offers a native opened/closed status plus free-form project and group labels, so the adapter models
Sortie state with labels, as the GitHub and Gitea adapters do, following the shared label-state
derivation rule above. Its native open and closed status values are `opened` and `closed`.

Label names are case-sensitive on GitLab, and attaching a name that matches no label creates it
instead of failing, so a configured label differing only in case from a stored one would silently
create a duplicate. The preflight catalog read resolves the stored casing of every configured state
label, across project labels and inherited group labels alike, and every state-label write sends
that stored casing. A configured name absent from the catalog resolves to nothing and is sent as
configured, which creates it on the first write, so a label the project does not hold yet is an
intended new label rather than an error.

**Normalization specifics.** Beyond the shared rules in Section 11.3:

- `id` and `identifier` are both the project-scoped `iid` as a string; the instance-global issue id
  is never read, because the project-scoped routes do not accept it and the route that does is
  closed to a non-administrator.
- `priority` is always null. GitLab issues carry no priority field, and label priority is a
  project-level ordering rather than a per-issue integer.
- `parent` is always null, because the issue route exposes no sub-issue relationship, and no parent
  request is issued.
- `blocked_by` is always a non-nil empty slice. The issue-links route exists on Community Edition
  but accepts only the `relates_to` link type, so no `blocks` relation exists to invert as
  Section 11.3 describes, and no links request is issued.
- `branch_name` is always empty, because GitLab derives branch names in the UI rather than storing
  one on the issue.
- `assignee` is the username of the first entry of the issue's assignee list.
- `issue_type` passes through from the issue. GitLab models tasks, incidents, and test cases as
  issue types on the same routes, so every list route filters to the `issue` type and the
  single-issue routes reject anything else as not found. Non-issue work items never enter the
  candidate set.
- Comments come from the issue notes route, which interleaves human comments with system journal
  entries. The adapter requests comments only and drops any note the server flags as a system note,
  so a label change or a state change never reaches a prompt as a comment. A note marked internal
  passes through, being a genuine human comment rather than a journal entry. GitLab returns notes
  newest-first, so the adapter requests ascending order.

**Transport and pagination.** GitLab paginates with `Link` response headers, so the adapter follows
the header to exhaustion with a page size of 100, the server maximum and above the Section 11.2
default of 50, and a 30,000 ms network timeout. An absent `Link` header, or a final page carrying no
next relation, is the normal end of results, never a missing-cursor error. A bounded page ceiling
stops a runaway walk and logs a WARN rather than looping. Request throttling is off by default on a
self-managed instance and active on GitLab.com; the adapter surfaces the server's `Retry-After` in a
log while leaving retry backoff to the orchestrator.

**Error classification.** GitLab returns four different error envelope shapes, a string message, a
non-string message, a bare error, and an error paired with a description, so the adapter reads all
four to build one bounded diagnostic snippet. The snippet never carries the token, which travels
only in the request header. 401 and 403 map to `tracker_auth_error`, 404 to a not-found result, 400,
414, and 422 to `tracker_payload_error`, 409 and 429 to `tracker_api_error`, and 5xx and transport
failures to `tracker_transport_error`. The 414 status extends the set the GitHub and Gitea adapters
classify: it guards the batched state lookup, whose request line grows with the number of issue ids
it carries.

**Write operations.** The adapter implements the three writes the `TrackerAdapter` interface
requires beyond the read set. GitLab has no transition endpoint, so both label writes compose from
its one issue-update route, which carries the state event and the label additions and removals in a
single body, and the comment write posts to the notes route:

- `transition_issue` rejects a target that is not a configured active, terminal, or handoff label
  before any write. Otherwise one request carries the whole transition: it attaches the target label
  in the project's stored casing, removes every case variant of the label being replaced along with
  any pre-existing variant of the target, and reconciles native status, closing an open issue for a
  terminal target and reopening a closed one for an active target. Clearing the variants in the same
  request is what stops a project already holding a case-duplicate label from accumulating state
  labels on every transition. A transition that is already converged issues no request and reports
  success. `handoff_state` and `in_progress_state` name labels applied through this same path, the
  latter driven by the orchestrator's dispatch-time transition.
- `comment_issue` posts the text verbatim as a note, with no conversion, escaping, or truncation.
  GitLab executes a recognized slash command at the start of a line in a note body as a quick
  action. When the body is consumed entirely as quick actions, GitLab creates no note, and the
  adapter reports `tracker_payload_error` rather than a false success. Executed command keys are
  logged at WARN whenever GitLab reports any; the note text itself is never logged.
- `add_label` resolves the label's stored casing against a freshly read catalog and attaches it
  additively, so existing labels are preserved. The catalog is re-read rather than taken from the
  construction-time casing map, because an escalation label is not a tracker-config value the
  constructor ever saw. A catalog read failure does not fail the attach: the adapter logs a WARN and
  sends the label as configured, because a missed escalation is worse than a cosmetic duplicate. The
  attach creates the label as a server-side side effect when it does not already exist.

**Operator query filter.** `tracker.query_filter` (Section 5.3.1) is a URL query fragment for the
project issue-list route, parsed the way the Gitea adapter parses its own. It merges into the
open-issue listings that back candidate polling and not into the closed-issue listing used for
terminal-state cleanup, nor into the batched state lookups that reconcile active runs, so an
operator filter never hides an issue from reconciliation. The adapter reserves the eight keys it
owns, whose override would change correctness rather than scope, and rejects a fragment naming any
of them. Where the Gitea adapter warns and forwards an unrecognized key, this adapter refuses it:
GitLab silently ignores a parameter it does not honor, so a typo would widen the candidate set to
every open issue with no visible signal. Negation is accepted through GitLab's negation hash for the
subset the server honors there. The adapter never pushes the configured state labels into GitLab's
`labels` query parameter, whose AND semantics offer no way to express the disjunction state
filtering needs, so state filtering stays client-side. The same grammar backs the adapter's offline
configuration diagnostics, so the `sortie validate` verdict cannot drift from the construction
verdict. See the workflow reference for the operator-facing shape and the diagnostics the adapter
emits for filter labels absent from the project catalog.

#### 11.6.4 GitHub adapter

The GitHub adapter targets the REST API (plus the search endpoint) at `tracker.endpoint`, which
defaults to `https://api.github.com`, built on the shared HTTP client with no third-party GitHub
client library. Its wire model is the closest of the three forges to Gitea's, issues plus labels
plus an open/closed status, and the two diverge mainly in how each locates issues by state.

**Authentication.** The resolved `tracker.api_key` is sent as a `Bearer` token in the
`Authorization` header alongside a pinned API-version header. Unlike the Gitea and GitLab adapters,
the constructor runs no credential or project preflight; a misconfigured token or an inaccessible
repository surfaces on the first read rather than at startup.

**Repository scoping.** `tracker.project` is the repository in `owner/repo` form, exactly one slash
with non-empty halves, the same grammar Gitea enforces. Every read and write route is scoped to
that repository. The adapter uses no global issue id: it addresses issues by their
repository-scoped `number` and qualifies each issue's `display_id` as `owner/repo#N`.

**State model.** GitHub has neither Jira's transition graph nor Linear's named workflow states. It
offers a native open/closed status plus free-form repository labels, so the adapter models Sortie
state with labels, following the shared label-state derivation rule above. Its native open and
closed status values are `open` and `closed`, the same spelling Gitea uses.

**Normalization specifics.** Beyond the shared rules in Section 11.3:

- `id` and `identifier` are both the repository-scoped issue number as a string; GitHub's global
  issue id is never read.
- `priority` is always null, because GitHub issues carry no priority field.
- `blocked_by` is read from the issue dependencies route
  (`/issues/{number}/dependencies/blocked_by`), the GitHub form of the inverse `blocks` relation
  described in Section 11.3.
- `parent` comes from the issue's parent route; a missing parent (HTTP 404) normalizes to nil
  rather than an error.
- `assignee` is the first entry of the issue's assignee list.
- `issue_type` passes through the issue's native type field when GitHub reports one, and is empty
  otherwise.
- Comments are fetched only by `fetch_issue_by_id` and `fetch_issue_comments`; GitHub returns them
  in ascending creation order already, so the adapter re-sorts nothing.
- The issues-list and search routes co-mingle pull requests with issues; the adapter drops any
  entry carrying a non-nil pull-request marker before it reaches normalization.

**Transport and pagination.** GitHub paginates with `Link` response headers, so the adapter follows
the header to exhaustion with a page size of 50, a bounded ceiling of 200 pages, and a 30,000 ms
network timeout. `fetch_issue_states_by_ids` and `fetch_issue_states_by_identifiers` read one issue
at a time instead, through a per-path ETag cache: a cached `304 Not Modified` response reuses the
last derived state without re-deriving it, and a fresh response replaces the cache entry.

**Error classification.** The adapter maps HTTP status to the Section 11.4 categories from the
response body's first bytes, read only as a bounded diagnostic snippet and never echoing the
token. 401 maps to `tracker_auth_error`; 403 maps to `tracker_auth_error` unless the response
carries an exhausted primary rate-limit header or a secondary rate-limit message, in which case it
maps to `tracker_api_error`; 404 maps to a not-found result; 400 and 422 map to
`tracker_payload_error`; 410, 405, 409, and 429 map to `tracker_api_error`; 5xx and transport
failures map to `tracker_transport_error`; and any other status maps to `tracker_api_error`.

**Write operations.** The adapter implements the three writes the `TrackerAdapter` interface
requires beyond the read set, composing each from GitHub's label and issue-edit routes because
GitHub has no transition endpoint:

- `transition_issue` rejects a target that is not a configured active, terminal, or handoff label
  before any write. Otherwise it removes the current state label, adds the target label, and
  reconciles native status: a terminal target closes an open issue with a completed state reason,
  and an active target reopens a closed one. Every step is idempotent, so a partial failure
  converges on retry.
- `comment_issue` posts the text verbatim as a Markdown comment; GitHub accepts Markdown natively,
  so no format conversion happens.
- `add_label` attaches a label by name additively through GitHub's labels-add route, so existing
  labels are preserved.

**Operator query filter.** `tracker.query_filter` (Section 5.3.1) is a raw GitHub search-qualifier
fragment appended after the adapter's own `repo:`, `type:issue`, and `state:open` qualifiers. It
merges into the search-endpoint candidate fetch when non-empty (an empty filter keeps candidate
fetches on the plain issues-list route) and never into the closed-issue search
`fetch_issues_by_states` performs for terminal-state matching, so an operator filter never hides a
terminal issue from that lookup.
