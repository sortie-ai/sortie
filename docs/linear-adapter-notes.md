# Linear GraphQL API: Adapter research notes

> Linear public GraphQL API, researched June 2026 against the official developer
> documentation, the published SDK schema (`linear/linear@df20561`, 2026-06-11),
> and the official TypeScript SDK error classifier, then **verified live** against
> a real workspace (team `SOR`) on 2026-06-13.
> Reference for implementing the Linear `TrackerAdapter`.
>
> Claims tagged **[live-verified]** were confirmed by direct GraphQL calls against
> the live API, the strongest available evidence. Where the live API contradicted
> the published documentation, the live behavior wins and the conflict is noted.

Linear exposes a single GraphQL endpoint rather than the REST surfaces of the
existing Jira and GitHub adapters. Client construction, query shapes, pagination,
and error handling all differ. The adapter is built on `internal/httpkit` with no
third-party GraphQL library: a GraphQL call is an HTTP POST with a JSON body, and
the response is a JSON envelope. Nothing more is required.

---

## Authentication

Linear supports two authentication methods. Sortie uses personal API keys.

### Personal API keys (recommended for Sortie)

The standard method for scripts and service integrations.

- Created in Linear under **Settings > Account > Security & Access**. Admins can
  always create keys; members can create their own only when admins permit it
  via **Settings > Administration > API > Member API keys**.
- At creation time a key is either given full access to everything the user can
  access, or restricted to a permission subset: **Read, Write, Admin, Create
  issues, Create comments**. A key can additionally be limited to specific teams.
- Header: `Authorization: <API_KEY>`. **No `Bearer` prefix.** This is the most
  common integration bug; the official curl example passes the key verbatim.
  **[live-verified]** The bare key returns HTTP 200. Sending
  `Authorization: Bearer <API_KEY>` returns HTTP 400 with the explicit body
  message "It looks like you're trying to use an API key as a Bearer token.
  Remove the Bearer prefix from the Authorization header."
  (`extensions.type: "invalid input"`, `code: "INPUT_ERROR"`).
- Key format: prefixed `lin_api_`. Linear participates in GitHub's secret
  scanning program: a key committed to a public GitHub repository is detected
  and automatically revoked. Treat an accidentally published key as already dead.
- Keys are user-scoped and act with that user's workspace permissions (as
  narrowed by the selected scopes and teams). They do not expire on a schedule.

Recommended key shape for Sortie: restricted to the configured team, with
**Read + Write + Create comments**. Write covers `issueUpdate` (state
transitions and label changes); Create comments covers `commentCreate`.
"Create issues" and "Admin" are not needed.

Per-operation minimum permission (key scope plus, where relevant, a team
setting):

| Operation                                                                                                                                       | Minimum permission                                                                       |
| ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `FetchCandidateIssues`, `FetchIssueByID`, `FetchIssuesByStates`, `FetchIssueStatesByIDs`, `FetchIssueStatesByIdentifiers`, `FetchIssueComments` | **Read** (member access to the team)                                                     |
| `TransitionIssue` (`issueUpdate` state)                                                                                                         | **Write**                                                                                |
| `AddLabel`, attach existing label (`issueUpdate`)                                                                                               | **Write**                                                                                |
| `CommentIssue` (`commentCreate`)                                                                                                                | **Create comments** (or Write)                                                           |
| `AddLabel`, create missing label (`issueLabelCreate`)                                                                                           | **Write**, plus the team's label-management setting must allow members (see operation 9) |

The only operation that can fail on a correctly-scoped key is the
create-missing-label path, because label creation is additionally gated by a
team setting; every other operation succeeds with Read or Write as listed.

### OAuth 2.0

For applications acting on behalf of multiple users.

- Header: `Authorization: Bearer <ACCESS_TOKEN>` (with `Bearer`).
- Scopes: `read` (always present), `write`, `issues:create`, `comments:create`,
  `timeSchedule:write`, `admin`, plus app-actor scopes (`app:assignable`,
  `app:mentionable`) for Linear's agent features.
- Access tokens expire after **24 hours** and must be refreshed with a refresh
  token; client-credentials tokens last 30 days.

Not used by Sortie. The orchestrator runs as a single headless principal, and
the 24-hour expiry would force a token-refresh subsystem that personal API keys
make unnecessary.

### Key validation

The `viewer` query is the documented way to test credentials. It costs almost
nothing and identifies the acting user:

```graphql
query Me {
  viewer {
    id
    name
    email
  }
}
```

The adapter should run this at construction time: a failure classifies the key
before the first poll cycle, and the returned `viewer.id` is useful when an
assignee filter is configured (see `FetchCandidateIssues`).

**[live-verified]** A valid key returns
`{ "data": { "viewer": { "id": "<uuid>", "name": "...", "email": "..." } } }`
on HTTP 200 (`X-Complexity: 2`). An invalid key returns **HTTP 401** with a body
error: `message: "Authentication required, not authenticated"`,
`extensions.type: "authentication error"`, `code: "AUTHENTICATION_ERROR"`,
`statusCode: 401`, `userPresentableMessage: "You need to authenticate to access
this operation."` So an invalid key fails at both the HTTP layer (401) and the
body layer; the adapter's body-first classifier handles it either way.

### Config mapping

| Config field       | Value                                                     |
| ------------------ | --------------------------------------------------------- |
| `tracker.endpoint` | `https://api.linear.app/graphql` (default, omit normally) |
| `tracker.api_key`  | Personal API key (`lin_api_...`), sent verbatim           |
| `tracker.project`  | Linear **team key**, e.g. `ENG` (see Identifiers below)   |

---

## Endpoint and transport

- Single endpoint: `POST https://api.linear.app/graphql`.
- Request: `Content-Type: application/json`, body
  `{ "query": "...", "variables": { ... } }`.
- Response: always JSON. Success data lives under `data`; errors live in a
  top-level `errors` array, **potentially alongside partial `data`**.
- The endpoint supports introspection; the full schema is also published at
  `linear/linear` (`packages/sdk/src/schema.graphql`).
- Network timeout: 30,000 ms (architecture Section 11.2 default).

HTTP status semantics differ from REST APIs and from each other. The table marks
what was observed live:

| Status | Meaning                                                                                                                     | Observed                                |
| ------ | --------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| 200    | Request executed. **The body may still contain `errors`.** Covers not-found and argument-validation errors. Always inspect. | [live-verified]                         |
| 400    | Malformed `Authorization` format (e.g. a `Bearer`-prefixed API key); also documented for rate limiting (`RATELIMITED`)      | 400 for bad auth format [live-verified] |
| 401    | Missing, invalid, or revoked credentials                                                                                    | [live-verified]                         |
| 403    | Authenticated but forbidden                                                                                                 | documented                              |
| 429    | Also documented for rate limiting; the official SDK handles both 400 and 429                                                | documented                              |
| 5xx    | Server-side failure                                                                                                         | documented                              |

The decisive rule, now confirmed by the spread above: **HTTP status alone never
classifies a Linear response.** An entity-not-found and an argument-validation
failure both arrive on HTTP 200 with the real error in the body; an invalid key
arrives on 401 with the same body shape. Error classification inspects the
response body first and falls back to the status code only when the body carries
no `errors` array (see Error model).

### httpkit fit

GraphQL inverts two assumptions baked into the current `httpkit` surface:

- All calls are `POST` to one path. `httpkit.Client.Send` provides POST, but it
  returns only the body, not the response headers. Reading the rate-limit and
  `X-Complexity` headers on success requires a Send variant that exposes
  headers (small additive change).
- Cursor state travels in the JSON `variables` object (`after`), not in a URL
  query parameter, so `httpkit.NewTokenPaginator` does not apply as-is. The
  adapter implements the same loop shape over POST: issue request, decode
  `nodes` + `pageInfo`, stop when `hasNextPage` is false, honor a `MaxPages`
  bound, and report a missing cursor (see Pagination). Either generalize the
  paginator with a request-builder hook or keep the loop local to the adapter;
  the loop is ~30 lines either way.

Error classification stays in the `httpkit` mold: the GraphQL transport wraps
`ClassifyError`/`ClassifyTransport` for HTTP-level failures and adds a
body-level classification pass that REST adapters do not need.

---

## Identifiers and team scoping

Linear has three identifier-like values per issue, and the distinction drives
several adapter decisions:

| Value              | Example                                | Properties                                  |
| ------------------ | -------------------------------------- | ------------------------------------------- |
| `issue.id`         | `a7c4f8e2-1b9d-4e3a-8f2c-6d5e4a3b2c1f` | UUID, stable, globally unique               |
| `issue.identifier` | `ENG-123`                              | Human-readable, team key + issue number     |
| `issue.number`     | `123`                                  | Numeric part, **unique only within a team** |

Per the schema, `Team.key` is "used as a prefix in issue identifiers (e.g.,
'ENG' in 'ENG-123')", and `Issue.number` is "scoped to the issue's team".

Domain mapping: `issue.id` is the domain `ID`, `issue.identifier` is the domain
`Identifier`. The lookup queries accept both forms: the official documentation
queries `issue(id: "BLA-123")` directly, and documents `issueUpdate` as
accepting "a UUID or a shorthand ID". This is convenient but silent: nothing in
a response says which form was used, so the adapter should always pass the form
it means and never construct one from the other.

**`tracker.project` selects the team key** (`ENG`), not a Linear project. Three
reasons:

1. Workflow states are team-scoped ("Each team has its own set of workflow
   states", per the schema). The state model, the central mapping of this
   adapter, is only well-defined relative to one team.
2. The team key is the identifier prefix, so `tracker.project` matching the
   visible `ENG-123` prefix is the least surprising operator experience, and it
   mirrors the Jira adapter where `tracker.project` is the issue-key prefix.
3. Linear "projects" are cross-team containers with their own lifecycle; they
   do not own states or identifiers. Filtering by project would still require a
   team for state resolution.

The filter is `team: { key: { eq: "ENG" } }` (`TeamFilter.key` is a
`StringComparator`); no team UUID resolution is needed for reads.

---

## State model

This is the least mechanical part of the port. Jira has rich named workflow
states; GitHub has only `open`/`closed` plus labels. Linear sits in between: it
has real named states, but every state also carries a workspace-immutable
**category** that tells you what the state means.

### Workflow state shape

```graphql
type WorkflowState {
  id: ID! # UUID, required for transitions
  name: String! # e.g. "In Progress", "Done", "Backlog"; operator-visible
  type: String! # category, see below
  position: Float! # display order within the type group
  team: Team! # owning team
}
```

`WorkflowState.type` is one of **seven** values (schema, 2026-06):

| `type`      | Meaning                                             | Suggested bucket                  |
| ----------- | --------------------------------------------------- | --------------------------------- |
| `triage`    | Intake queue awaiting acceptance (optional feature) | Neither (exclude by default)      |
| `backlog`   | Accepted, not planned                               | `active_states` candidate         |
| `unstarted` | Planned, not begun (e.g. "Todo")                    | `active_states` candidate         |
| `started`   | Work in progress (e.g. "In Progress", "In Review")  | `active_states` / `handoff_state` |
| `completed` | Done                                                | `terminal_states`                 |
| `canceled`  | Abandoned                                           | `terminal_states`                 |
| `duplicate` | Closed as duplicate of another issue                | `terminal_states`                 |

**[live-verified]** A freshly created team (`SOR`) shipped with exactly six
default states and no `triage`: Backlog (`backlog`), Todo (`unstarted`),
In Progress (`started`), Done (`completed`), Canceled (`canceled`),
Duplicate (`duplicate`). `triage` is opt-in per team, confirming it must be
excluded by default. A sensible default mapping for this stock layout is active
`["Backlog", "Todo", "In Progress"]`, terminal `["Done", "Canceled",
"Duplicate"]`, and an operator-added `"In Review"` (`started`) for
`handoff_state`.

Two properties make this trickier than it looks:

- **States are team-scoped.** Two teams can each have an "In Progress" with
  different UUIDs. All state lookups must be scoped to the configured team.
- **A team can have several states of the same type.** "In Review" and "QA"
  can both be `started`. The category alone cannot distinguish them.

### Mapping to the normalized model

The adapter follows the Jira/GitHub convention: **operators configure state
names**, and the adapter treats them as opaque strings. `domain.Issue.State`
is `issue.state.name` with original casing preserved. Example:

```yaml
tracker:
  kind: linear
  project: "ENG"
  active_states: ["Backlog", "Todo"]
  terminal_states: ["Done", "Canceled", "Duplicate"]
  handoff_state: "In Review"
```

The `type` field is not used for candidate selection (names are more precise
and match the other adapters), but it is the right tool for a **startup
preflight**: fetch the team's states once and verify every configured name
exists, with a case-insensitive comparison in Go:

```graphql
query TeamStates($teamKey: String!) {
  teams(filter: { key: { eq: $teamKey } }, first: 1) {
    nodes {
      id
      key
      states(first: 50) {
        nodes {
          id
          name
          type
          position
        }
      }
    }
  }
}
```

This preflight serves three purposes:

1. Misconfigured state names fail at startup, not silently as an empty
   candidate list.
2. The adapter caches the canonical casing of each configured name. The `in`
   state filter used by the fetch queries is case-sensitive
   (`StringComparator` distinguishes `eq`/`in` from `eqIgnoreCase`, which has
   no `in` analogue), so queries must send the canonical names.
   **[live-verified]** `state: { name: { in: ["todo"] } }` returned zero issues
   while `["Todo"]` returned all six; case-sensitivity of the `in` filter is
   real, and the preflight casing cache is mandatory, not a nicety.
3. Warn when a configured `active_states` entry resolves to a state whose
   `type` is `completed`, `canceled`, or `duplicate` (and vice versa for
   `terminal_states`). The categories are wrong-config tripwires even though
   they do not drive selection.

### Transitions need a UUID

`issueUpdate` takes a `stateId` UUID, never a state name. `TransitionIssue`
therefore resolves the name first. A single query walks from the issue to its
team's states, which also keeps multi-team workspaces safe without caching:

```graphql
query ResolveStateID($issueId: String!, $stateName: String!) {
  issue(id: $issueId) {
    id
    team {
      states(filter: { name: { eqIgnoreCase: $stateName } }, first: 1) {
        nodes {
          id
          name
        }
      }
    }
  }
}
```

`eqIgnoreCase` tolerates casing drift between operator config and Linear.
An empty `nodes` array means no state with that name exists in the issue's
team: return `ErrTrackerPayload` (no available transition leads to the target
state). Unlike Jira there is no transition graph: any state can move to any
state, so resolve-then-update is the entire flow.

**[live-verified]** The resolve query with `stateName: "in review"` (lowercase)
against `issue("SOR-5")` returned the team's `In Review` state and its UUID, then
`issueUpdate(id: "SOR-5", input: { stateId: <uuid> })` returned
`success: true` with `issue.state.name == "In Review"`. The full handoff path,
case-insensitive resolve plus update, works end to end.

---

## Operations

The `TrackerAdapter` Go interface has nine methods. Seven of them are the
required tracker operations in the architecture spec (Section 11.1),
`FetchCandidateIssues` through `TransitionIssue`; the interface adds
`CommentIssue` (lifecycle comments) and `AddLabel` (CI-failure label
escalation) beyond that required set. All nine map onto the single GraphQL
endpoint and are covered below as operations 1 through 9 (`CommentIssue` and
`AddLabel` are operations 8 and 9).

### 1. `FetchCandidateIssues`: `issues` query, team + active states

```graphql
query CandidateIssues(
  $teamKey: String!
  $states: [String!]!
  $first: Int!
  $after: String
) {
  issues(
    first: $first
    after: $after
    filter: {
      team: { key: { eq: $teamKey } }
      state: { name: { in: $states } }
    }
    sort: [
      { priority: { order: Ascending, noPriorityFirst: false } }
      { createdAt: { order: Ascending } }
    ]
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state {
        name
      }
      assignee {
        displayName
        name
        email
      }
      parent {
        id
        identifier
      }
      labels(first: 25) {
        nodes {
          name
        }
      }
      inverseRelations(first: 25) {
        nodes {
          type
          issue {
            id
            identifier
            state {
              name
            }
          }
        }
      }
    }
    pageInfo {
      hasNextPage
      endCursor
    }
  }
}
```

Variables: `{ "teamKey": "ENG", "states": ["Backlog", "Todo"], "first": 50, "after": null }`.

Notes:

- The `sort` argument exists on `issues` (`IssueSortInput`), but
  **[live-verified]** its priority ordering is non-obvious and should not be
  relied on. With issues of priority 0, 2, and 3 present,
  `sort: [{ priority: { order: Ascending, noPriorityFirst: false } }, { createdAt: { order: Ascending } }]`
  returned the no-priority (0) group first, then priority 3, then priority 2,
  which is neither plain ascending nor descending by the numeric value. Rather
  than depend on server-side priority semantics, the adapter should compute the
  normalized domain priority (0 to nil, 1..4 to int) and **sort candidates
  client-side** for deterministic, cross-adapter-consistent dispatch order. The
  `sort` argument can be dropped or kept as a coarse hint; the createdAt
  secondary is still a reasonable stable order for the fetch itself.
- Comments are deliberately not fetched; `Issue.Comments` stays nil per the
  interface contract, and the pre-dispatch `FetchIssueByID` supplies them.
- Nested connections are capped at `first: 25` as cheap insurance against a
  pathological issue (hundreds of labels/blockers); the candidate query
  measured only 95 complexity points, so this is not cap-avoidance (see Rate
  limiting).
- Archived issues are excluded by default (`includeArchived` defaults to
  false); do not pass the argument.
- Optional assignee scoping, if ever configured, is a filter fragment rather
  than a post-filter: `assignee: { isMe: { eq: true } }` selects issues
  assigned to the key's user without resolving the viewer id, and
  `assignee: { id: { eq: $uuid } }` pins a specific user. **[live-verified]**
  `isMe: { eq: true }` returned only the one issue assigned to the key's user.

### 2. `FetchIssueByID`: `issue` query, UUID or identifier

```graphql
query IssueByID($id: String!) {
  issue(id: $id) {
    id
    identifier
    title
    description
    priority
    branchName
    url
    createdAt
    updatedAt
    state {
      name
    }
    assignee {
      displayName
      name
      email
    }
    parent {
      id
      identifier
    }
    labels(first: 25) {
      nodes {
        name
      }
    }
    inverseRelations(first: 25) {
      nodes {
        type
        issue {
          id
          identifier
          state {
            name
          }
        }
      }
    }
    comments(first: 50, orderBy: createdAt) {
      nodes {
        id
        body
        createdAt
        user {
          displayName
          name
          email
        }
        botActor {
          name
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}
```

`Query.issue(id: String!)` accepts either the UUID or the human identifier;
the official docs query `issue(id: "BLA-123")` directly. When
`comments.pageInfo.hasNextPage` is true, continue with the dedicated comment
pagination from operation 6 and append.

Not-found surfaces as a body-level error with `data.issue: null`; map it to
`ErrTrackerNotFound` (see Error model).

### 3. `FetchIssuesByStates`: same query, caller-supplied states

The `FetchCandidateIssues` query verbatim, with the orchestrator-supplied
state list (terminal cleanup passes terminal states) and without the `sort`
argument (order is irrelevant to cleanup). State names pass through the same
canonical-casing cache built by the startup preflight, because `in` matching
is case-sensitive while the interface contract promises case-insensitive
comparison.

### 4. `FetchIssueStatesByIDs`: `issues` filtered by id batch

```graphql
query IssueStatesByIDs($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first) {
    nodes {
      id
      state {
        name
      }
    }
  }
}
```

`IssueIDComparator.in` takes UUIDs. Chunk at 50 ids per request and set
`first` to the chunk length. Build the result map from the returned nodes;
ids absent from the response are simply omitted from the map (the interface
treats missing as not-an-error). No `pageInfo` is needed because the page size
equals the requested id count.

### 5. `FetchIssueStatesByIdentifiers`: team-key + number filter

`IssueFilter` has **no `identifier` field** (schema-verified; it offers `id`,
`number`, `team`, and others). The adapter splits each identifier into its team
key and numeric part (`"SOR-7"` to `("SOR", 7)`) and filters by the number set,
scoped to the configured team:

```graphql
query IssueStatesByNumbers(
  $teamKey: String!
  $numbers: [Float!]!
  $first: Int!
) {
  issues(
    filter: { team: { key: { eq: $teamKey } }, number: { in: $numbers } }
    first: $first
  ) {
    nodes {
      identifier
      number
      state {
        name
      }
    }
  }
}
```

`Issue.number` is a `Float` in the schema, and `NumberComparator.in` is the
matching filter. Build the result map keyed by `identifier` from the returned
nodes; numbers absent from the response are omitted (the interface treats
missing as not-an-error). All identifiers share the configured `tracker.project`
team key, so one filter covers the batch; chunk the number list at 50.

> **Do not use an aliased `issue(id:)` batch here.** It is the obvious design and
> it is broken. `Query.issue` returns the non-null type `Issue!`, so when **any**
> alias resolves to a missing issue, the GraphQL executor nulls the **entire**
> `data` object and abandons the sibling aliases. **[live-verified]** A batch of
> `i0: issue("SOR-5")`, `i1: issue("SOR-99999")`, `i2: issue("SOR-7")` returned
> `data: null` with a single error for `i1`; the two valid lookups returned
> nothing. A deleted or renamed issue, the exact reconciliation case this method
> serves, would wipe the states of every other issue in the batch. The
> connection-filter form above does not have this failure: **[live-verified]**
> `number: { in: [5, 7, 99999] }` returned SOR-5 and SOR-7 and silently dropped 99999. The same reasoning is why `FetchIssueStatesByIDs` (operation 4) uses
> `id: { in: [...] }` rather than aliased lookups.

### 6. `FetchIssueComments`: `issue.comments` connection

```graphql
query IssueComments($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    comments(first: $first, after: $after, orderBy: createdAt) {
      nodes {
        id
        body
        createdAt
        user {
          displayName
          name
          email
        }
        botActor {
          name
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}
```

- `orderBy: createdAt` is the documented default field. **[live-verified]** Its
  direction is **descending, newest first**: two comments created 28 ms apart
  came back newest-then-oldest. Agents want chronological context, so the
  adapter MUST sort the accumulated slice by `createdAt` ascending in Go before
  returning. The client-side sort is mandatory, not defensive.
- `body` is markdown ("derived representation of the canonical bodyData
  ProseMirror content" per the schema); no flattening, pass through.
- `user` is null for comments created by integrations or bots; `botActor`
  covers agent/bot comments. Author resolution: `user.displayName`, then
  `user.name`, then `botActor.name`, else empty string. (`externalUser`
  exists for Slack/Intercom-originated comments; not selected initially.)
  **[live-verified]** A human-authored comment returned
  `user: { displayName, name, email }` with `botActor: null`; the comment
  `body` round-tripped verbatim, including a raw issue URL (mention rendering
  happens in the UI, not in the stored markdown).
- Empty connection returns an empty non-nil slice. Not-found maps to
  `ErrTrackerNotFound`.

### 7. `TransitionIssue`: resolve query + `issueUpdate`

Resolution query in the State model section. The mutation:

```graphql
mutation IssueUpdateState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
    issue {
      id
      state {
        name
      }
    }
  }
}
```

`issueUpdate` is documented to accept a UUID or shorthand id. The payload is
`IssuePayload { success: Boolean!, issue: Issue, lastSyncId: Float! }`. Treat
a non-empty `errors` array as failure regardless of `success`; treat
`success: false` without errors as `ErrTrackerAPI` (defensive; Linear
normally signals failure via `errors`).

### 8. `CommentIssue`: `commentCreate`

```graphql
mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment {
      id
    }
  }
}
```

The body is plain markdown; no ADF-style wrapping (contrast with Jira).
**[live-verified]** `commentCreate.input.issueId` accepts **both** the UUID and
the shorthand identifier: `commentCreate(input: { issueId: "SOR-5", body: ... })`
succeeded and returned the created comment. The orchestrator always has the
UUID, so prefer it for clarity, but a shorthand-id call is not an error.

**On mentions via URL.** Linear issue URLs have the stable form
`https://linear.app/<urlKey>/issue/<IDENTIFIER>/<slug>` (**[live-verified]**:
`organization.urlKey` was `sortie-ai`, so SOR-5 is
`https://linear.app/sortie-ai/issue/SOR-5/...`; a user profile is
`https://linear.app/<urlKey>/profiles/<handle>`). An agent may embed such URLs
in a comment body to reference issues for human readers. Do **not** rely on a
plain URL rendering as an interactive mention "pill", though: **[live-verified]**
the comment body round-trips verbatim through the API (the URL is stored as
plain text), and mention-pill rendering is a paste-time editor behavior that the
editor itself lets users decline ("Keep as link"). Whether a markdown-body URL
becomes a mention on display is not documented for the API path and was not
verified, so the adapter treats URLs as plain text and makes no mention
guarantee.

### 9. `AddLabel`: resolve + optional create + append

Linear attaches labels by UUID, so "add label by name" is a compound
operation. The schema offers two write shapes on `IssueUpdateInput`, and the
choice matters (see Collection writes below for the hazard):

- `labelIds: [String!]`: "the identifiers of the issue labels associated with
  this ticket". **Replaces the full set.**
- `addedLabelIds: [String!]` / `removedLabelIds: [String!]`: "labels to be
  added to" / "removed from" the issue. **Append/remove semantics.**

The adapter uses `addedLabelIds`, which reduces `AddLabel` to:

**Step 1.** Resolve the label by name, covering both team-scoped and
workspace labels (the root query returns both, per its schema doc):

```graphql
query ResolveLabel($name: String!) {
  issueLabels(filter: { name: { eqIgnoreCase: $name } }, first: 50) {
    nodes {
      id
      name
      team {
        id
      }
    }
  }
}
```

Prefer a label whose `team.id` matches the configured team; fall back to a
workspace label (`team` null).

**Step 2.** If no label matched, create it scoped to the team:

```graphql
mutation LabelCreate($teamId: String!, $name: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name }) {
    success
    issueLabel {
      id
    }
  }
}
```

Omitting `teamId` would create a workspace-level label (schema-documented);
the adapter always passes the team id, because workspace-label management is
more likely to require elevated permissions. On a name-uniqueness conflict
from a concurrent create (`invalid input` class error), re-run step 1 once
and use the now-existing id. If creation is forbidden for the key, return
`ErrTrackerAuth`; the orchestrator treats label errors as non-fatal, and the
operator remedy is to pre-create the escalation label in Linear.

Label-creation is gated by a specific, named team setting, not just admin
status: under **Team settings > Access and permissions**, a team owner chooses
whether **all members** or **only team owners** may create and edit issue
labels (per Linear's members-and-roles docs and the team-owners changelog). So
a Read+Write member key gets a `FORBIDDEN`/`feature not accessible` error on
`issueLabelCreate` precisely when that team has restricted label management to
owners. The operator has two clean remedies: flip that team setting to allow
members, or pre-create the escalation label (default `needs-human`) once, after
which the adapter's lookup in step 1 finds it and never calls
`issueLabelCreate`. Document both in the operator README.

**Step 3.** Append by id; no read of the existing label set is needed:

```graphql
mutation IssueAddLabel($id: String!, $labelIds: [String!]!) {
  issueUpdate(id: $id, input: { addedLabelIds: $labelIds }) {
    success
  }
}
```

**[live-verified]** The whole flow was exercised end to end:
`issueLabelCreate(input: { teamId, name: "needs-human" })` returned
`success: true` with a team-scoped label id, and
`issueUpdate(id: "SOR-5", input: { addedLabelIds: [<id>] })` left the issue with
**both** its pre-existing `Feature` label and the new `needs-human` label. The
append semantics are real and the read-before-write step is genuinely
unnecessary. (The test key had full access, so the `FORBIDDEN` degradation path
was not exercised; that branch remains documentation-sourced.)

---

## Field mapping

`domain.Issue` field to Linear source (all field names schema-verified):

| `domain.Issue` field | Linear source                                         | Notes                                                                                                                                                                                                                                                                  |
| -------------------- | ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ID`                 | `issue.id`                                            | UUID string                                                                                                                                                                                                                                                            |
| `Identifier`         | `issue.identifier`                                    | e.g. `"ENG-123"`                                                                                                                                                                                                                                                       |
| `DisplayID`          | empty                                                 | Identifiers are already display-ready                                                                                                                                                                                                                                  |
| `Title`              | `issue.title`                                         |                                                                                                                                                                                                                                                                        |
| `Description`        | `issue.description`                                   | Markdown per schema; nullable, null becomes empty string                                                                                                                                                                                                               |
| `Priority`           | `issue.priority`                                      | Float in schema: 0 = No priority, 1 = Urgent, 2 = High, 3 = Medium, 4 = Low. Map 1..4 to `*int`; map 0 to nil                                                                                                                                                          |
| `State`              | `issue.state.name`                                    | Original casing preserved                                                                                                                                                                                                                                              |
| `BranchName`         | `issue.branchName`                                    | Non-null; auto-generated. Format is workspace-configurable (see below); treat the whole string as opaque, never parse the prefix                                                                                                                                       |
| `URL`                | `issue.url`                                           | Provided directly, no construction                                                                                                                                                                                                                                     |
| `Labels`             | `issue.labels.nodes[].name`                           | Lowercase each (Section 11.3); non-nil empty slice when none                                                                                                                                                                                                           |
| `Assignee`           | `assignee.displayName`, fallback `name`, then `email` | `assignee` is strictly the `User` type, nullable; null becomes empty string. Agents/apps are not `User`s (they surface under the separate `delegate`/`botActor` fields the adapter does not read), so an agent-driven issue with no human assignee normalizes to empty |
| `IssueType`          | empty                                                 | Linear has no native issue-type field                                                                                                                                                                                                                                  |
| `Parent`             | `issue.parent` `{id, identifier}`                     | Nil when absent                                                                                                                                                                                                                                                        |
| `Comments`           | separate connection (op 6)                            | Nil when not fetched; empty non-nil when fetched and empty                                                                                                                                                                                                             |
| `BlockedBy`          | `inverseRelations.nodes` where `type == "blocks"`     | See below                                                                                                                                                                                                                                                              |
| `CreatedAt`          | `issue.createdAt`                                     | ISO-8601 `DateTime`, passed through                                                                                                                                                                                                                                    |
| `UpdatedAt`          | `issue.updatedAt`                                     | ISO-8601 `DateTime`, passed through                                                                                                                                                                                                                                    |

### Blocker extraction from `inverseRelations`

`IssueRelation` is directional. Per the schema: `issue` is "the source issue
whose relationship is being described", `relatedIssue` is "the target issue",
and `type` "describes how the source issue relates to this issue". The enum
`IssueRelationType` is `blocks | duplicate | related | similar`.

So when issue A blocks issue B, the relation record is
`{ issue: A, type: "blocks", relatedIssue: B }`, and it appears in
**B's `inverseRelations`**. Reading B, each `inverseRelations` node with
`type == "blocks"` contributes `node.issue` as a blocker:

- `node.issue.id` to `BlockerRef.ID`
- `node.issue.identifier` to `BlockerRef.Identifier`
- `node.issue.state.name` to `BlockerRef.State`

This matches the architecture's normalization rule ("blocked_by derived from
inverse relations where relation type is `blocks`") almost verbatim; Linear is
the tracker whose native model the rule was phrased for. Compare `type`
case-insensitively after trimming, defensively.

**[live-verified]** With SOR-5 set to block SOR-7
(`issueRelationCreate(issueId: SOR-5, relatedIssueId: SOR-7, type: blocks)`),
reading **SOR-7** returned
`inverseRelations.nodes: [{ type: "blocks", issue: { identifier: "SOR-5",
state: { name: "Todo" } } }]`. The blocked issue carries the relation, the
blocker is `node.issue`, and `type` came back as the lowercase string
`"blocks"`. The direction in the mapping above is correct.

Other field mappings confirmed against live issues: `priority` 2 and 3 returned
as expected and onboarding issues returned `priority: 0` (map to nil);
`branchName` was always present and auto-generated (`tasks/sor-5-implement-...`,
where the `tasks/` prefix is this workspace user's handle, not a fixed string);
`description` was markdown and came back `null` for an issue created without one
(map null to empty string); `assignee` returned
`{ displayName, name, email }` for an assigned issue and `null` for unassigned;
`parent` returned `{ id, identifier }` on the sub-issue; `url` was supplied
directly.

### `branchName` is not a fixed format

The branch-name prefix is **not** stable across workspaces. This run observed
`tasks/sor-5-implement-...`, where `tasks/` is the acting user's handle rather
than a fixed token. The schema explains why: `Organization.gitBranchFormat` is a
workspace-level template ("Supports template variables like `{issueIdentifier}`
and `{issueTitle}`. If null, the default formatting will be used"), so the
default prefix is derived from the acting user's handle and the whole format is
operator-configurable. The adapter MUST treat `branchName` as an opaque string:
store it, never parse the prefix or assume a `<handle>/<identifier>-<slug>`
shape.

### Comment mapping

| `domain.Comment` field | Linear source                                                              |
| ---------------------- | -------------------------------------------------------------------------- |
| `ID`                   | `comment.id`                                                               |
| `Author`               | `user.displayName`, fallback `user.name`, then `botActor.name`, else empty |
| `Body`                 | `comment.body` (markdown, pass through)                                    |
| `CreatedAt`            | `comment.createdAt`                                                        |

---

## Pagination

Linear uses Relay-style cursor connections everywhere:

- Arguments: `first` (forward page size) + `after` (cursor); `last`/`before`
  exist for backward paging and are not used.
- Every connection exposes `pageInfo { hasNextPage endCursor }`. Loop: request
  with `after: null`, then `after: endCursor`, until `hasNextPage` is false.
- Default page size is 50 when `first` is omitted (documented and repeated in
  every connection's schema doc). Sortie always passes `first` explicitly:
  50 for top-level collections (Section 11.2), smaller for nested connections
  (complexity, below).
- `first` must be in the range **1 to 250**, both bounds enforced and
  **[live-verified]**: `first: 251` fails with
  `constraints.max: "first must not be greater than 250"` and `first: 0` fails
  with `constraints.min: "first must not be less than 1"`, both as an
  `Argument Validation Error` on HTTP 200. With Sortie's page size of 50, neither
  bound is reachable; keep `first` between 1 and 250.
- If `hasNextPage` is true but `endCursor` is empty or null, return
  `ErrTrackerMissingCursor` (`tracker_missing_end_cursor`, Section 11.4)
  instead of treating pagination as complete. Silent truncation here is a
  data-loss bug; this mirrors the Jira adapter's guard.
- Default ordering is by `createdAt`; `orderBy: updatedAt` is the only
  alternative (`PaginationOrderBy` has exactly those two values).
  **[live-verified]** The direction is **descending (newest first)**, so any
  path that needs chronological order (comments for prompt context) must
  re-sort client-side (see operation 6).
- **[live-verified]** The cursor format is not uniform across connections: the
  `issues` connection returned a base64-encoded `endCursor`
  (`eyJrZXkiOi..."`), while the `comments` connection returned a bare comment
  UUID. Treat `endCursor` as an opaque token and pass it back verbatim; never
  parse or construct it.
- Cap the pagination loop with the existing `MaxPages`-style bound and surface
  the same `OnLimitReached` warning the REST paginator emits today.

---

## Rate limiting

### Request budget

The official rate-limiting page documents 5,000 requests/hour for an API key,
5,000 for an OAuth app, and 600 unauthenticated.

**[live-verified] conflict:** the live test workspace returned
`x-ratelimit-requests-limit: 2500`, **half** the documented figure. A
follow-up confirmed the request limit is **dynamic**: Linear scales it by the
number of paid seats in the workspace, so a free or single-seat workspace gets
less than the documented headline. **Do not hardcode 5,000.** Read
`x-ratelimit-requests-limit` and `-remaining` from each response and treat them
as the source of truth.

### Complexity budget

| Auth mode       | Points per hour | Live check                                              |
| --------------- | --------------- | ------------------------------------------------------- |
| API key         | 3,000,000       | `x-ratelimit-complexity-limit: 3000000` [live-verified] |
| OAuth app       | 2,000,000       | documented                                              |
| Unauthenticated | 100,000         | documented                                              |

Plus a **single-query cap of 10,000 points** for all auth modes.

Documented scoring: each property 0.1 points, each object 1 point, each
connection multiplies its children's points by the pagination argument (or the
default 50), result rounded up.

### Measured complexity, and why the documented formula over-estimates

The documented "multiply by page size" rule is a worst-case upper bound, not
the figure the API charges. Measured `X-Complexity` values, all
**[live-verified]**:

| Query                                                                 | X-Complexity |
| --------------------------------------------------------------------- | ------------ |
| `viewer { id name email displayName }`                                | 2            |
| `issues(first: 250) { nodes { id } }`                                 | 3            |
| Candidate query, `first: 50`, nested `labels`/`relations` `first: 25` | **95**       |
| Candidate query, `first: 250`, nested `first: 50`                     | 180          |
| `team { states(first: 50) labels(first: 50) }`                        | 132          |
| `issues(filter:{number:{in:[...]}})` state batch                      | 4            |

Two lessons for the implementation:

- **The top-level list `first` is not multiplied.** `issues(first: 250)`
  selecting only `id` cost 3, and raising the candidate query's outer `first`
  from 50 to 250 barely moved the cost (95 to 180, and that rise came from the
  nested `first` going 25 to 50). The cost tracks the rows actually returned,
  not the requested page size.
- **The production candidate query costs ~95 points, under 1% of the 10,000
  single-query cap.** The "trap" of a candidate fetch blowing the cap does not
  occur at any realistic issue shape. The `first: 25` on nested `labels` and
  `inverseRelations` is cheap insurance against a pathological issue with
  hundreds of labels or blockers, not a cap-avoidance necessity.

The authoritative cost is always the `X-Complexity` response header. Log it
(the adapter can expose `sortie_tracker_complexity` later) rather than
predicting from the formula.

At Sortie's expected scale (poll every 60 s, one or two pages per poll, a
handful of reconcile lookups) neither the hourly budgets nor the single-query
cap is a practical constraint.

### Response headers [live-verified]

All header names below were observed on live responses.

| Header                               | Meaning                                                          |
| ------------------------------------ | ---------------------------------------------------------------- |
| `x-ratelimit-requests-limit`         | Request quota (dynamic; 2500 in the test workspace)              |
| `x-ratelimit-requests-remaining`     | Requests left in the window                                      |
| `x-ratelimit-requests-reset`         | Window reset time, **epoch milliseconds** (13-digit)             |
| `x-complexity`                       | Complexity score of this query                                   |
| `x-ratelimit-complexity-limit`       | Complexity quota                                                 |
| `x-ratelimit-complexity-remaining`   | Complexity left in the window                                    |
| `x-ratelimit-complexity-reset`       | Window reset time, epoch milliseconds                            |
| `x-ratelimit-endpoint-*`, `...-name` | Per-endpoint sub-budgets where applied (documented)              |
| `Retry-After`                        | Seconds to wait (the official SDK reads it on rate-limit errors) |

The `*-reset` values are epoch **milliseconds** (e.g. `1781361577787`), not
seconds; divide by 1000 before comparing to a Go `time.Unix` value.

### Detection and backoff

A rate-limited response is documented to arrive as **HTTP 400 with
`errors[].extensions.code == "RATELIMITED"`**. The official SDK additionally
treats HTTP 429 as rate-limited and reads `Retry-After`; the two sources
disagree on the status code, so the adapter accepts **either** status and keys
on the body code. (The limit could not be exhausted on the live workspace
without abusing it, so the rate-limit body was not captured first-hand; the
classifier keys on the body regardless.) Classification: `ErrTrackerAPI`
(retryable, exponential backoff per the orchestrator's existing semantics).
Honor `Retry-After` as a minimum delay when present; otherwise back off with
the standard base 2 s, max 30 s, jitter plus or minus 30%, and log the relevant
`*-reset` value at WARN.

---

## Error model

The defining transport property: **Linear returns application errors inside
HTTP 200 bodies.** A response is an error if and only if the body contains a
non-empty top-level `errors` array (or the HTTP layer itself failed).
Status-code-only classification misses every body-level error. The live spread
proved this directly: entity-not-found and argument-validation both came on
HTTP 200, while an invalid key came on 401 and a malformed `Authorization`
format on 400. The body is the reliable signal in every case.

### Error envelope [live-verified]

The real envelope carries more structure than the SDK types alone reveal. This
is the verbatim live response for `issue(id: "<nonexistent-uuid>")`:

```json
{
  "errors": [
    {
      "message": "Entity not found: Issue",
      "path": ["byUuid"],
      "locations": [{ "line": 1, "column": 68 }],
      "extensions": {
        "type": "invalid input",
        "code": "INPUT_ERROR",
        "statusCode": 400,
        "userError": true,
        "userPresentableMessage": "Could not find referenced Issue."
      }
    }
  ],
  "data": null
}
```

Observed `extensions` fields and their reliability:

- `extensions.type`: lowercase phrase. **This is the stable classification key**
  and the field the official SDK keys on. Live values seen: `invalid input`
  (not-found, argument validation, bad auth format), `authentication error`
  (invalid key). Full SDK map (`error.ts`): `feature not accessible`,
  `invalid input`, `ratelimited`, `network error`, `authentication error`,
  `forbidden`, `bootstrap error`, `unknown`, `internal error`, `other`,
  `user error`, `graphql error`, `lock timeout`, `usage limit exceeded`.
- `extensions.code`: UPPER_SNAKE constant, **and not stable enough to classify
  on.** Live, three different codes mapped to the same `type: "invalid input"`:
  `INPUT_ERROR` (not-found and bad Bearer format) and `INVALID_INPUT`
  (a `first > 250` argument-validation error). `AUTHENTICATION_ERROR`
  accompanied `type: "authentication error"`. The only code pinned by the docs
  is `RATELIMITED`. **Classify on `type`, treat `code` as diagnostic only.**
- `extensions.statusCode` and `extensions.http.status`: a numeric status echoed
  inside the body (e.g. `400` even on an HTTP-200 response, `401` on the auth
  error). Diagnostic, not the HTTP status.
- `extensions.userError` (boolean) and `extensions.userPresentableMessage`
  (string): present on user-facing failures; the latter is the cleanest text
  for `TrackerError.Message`.
- `extensions.validationErrors` (array): present on argument-validation
  failures, with `constraints` such as `{ "max": "first must not be greater
than 250" }`.

### The not-found special case [live-verified]

This is the most important error finding, and it overturns the pre-key
assumption. There is **no dedicated not-found code or type**. A nonexistent
issue returns, on **HTTP 200**:

- `message`: `"Entity not found: Issue"`
- `extensions.type`: `"invalid input"` (the generic input-error bucket)
- `extensions.code`: `"INPUT_ERROR"`
- `extensions.userPresentableMessage`: `"Could not find referenced Issue."`
- `data`: `null`

Because `type` is the generic `"invalid input"`, the only signal that
distinguishes not-found from any other input error is the **message**.
Detection: an error whose `message` begins with `Entity not found`
(case-insensitive) maps to `ErrTrackerNotFound`. The `userPresentableMessage`
"Could not find referenced Issue." is a secondary confirmation.

**The non-null-abort gotcha.** `Query.issue` returns `Issue!`. When the
resolver fails, the executor propagates null to the root and **nulls the entire
`data` object**, and it abandons sibling root fields in the same operation
(verified: a 3-alias batch with one bad id returned `data: null` and only the
bad alias's error). This is why operation 5 must not use an aliased `issue(id:)`
batch. For a single `FetchIssueByID`, a not-found simply yields `data: null`
plus the not-found error, which classifies cleanly.

### Classification algorithm

```
classify(httpStatus, body):
  if transport failure (DNS, TCP, TLS, timeout):    ErrTrackerTransport
  if body parses and body.errors is non-empty:
    if any error.message starts with "entity not found"
       (case-insensitive):                           ErrTrackerNotFound
    if any extensions.code == "RATELIMITED"
       or extensions.type == "ratelimited":          ErrTrackerAPI (retryable)
    if type == "authentication error":               ErrTrackerAuth
    if type == "forbidden"
       or type == "feature not accessible":          ErrTrackerAuth
    if type in {"invalid input", "user error",
                "graphql error"} or userError:       ErrTrackerPayload
    if type in {"internal error", "network error",
                "lock timeout", "bootstrap error"}:  ErrTrackerTransport
    otherwise:                                       ErrTrackerAPI
  if httpStatus is not 200 (no errors array):
    400:                                             ErrTrackerPayload
    401:                                             ErrTrackerAuth
    403:                                             ErrTrackerAuth
    429:                                             ErrTrackerAPI (retryable)
    5xx:                                             ErrTrackerTransport
  if body does not parse as JSON on 200:             ErrTrackerPayload
```

The not-found message check runs **first**, before the `type`-based rules,
precisely because not-found arrives under the generic `type: "invalid input"`
that would otherwise classify as `ErrTrackerPayload`. Include the first error's
`userPresentableMessage` (falling back to `message`) in `TrackerError.Message`
so operators see Linear's own wording.

`feature not accessible` is grouped with `ErrTrackerAuth` deliberately. It means
the workspace plan does not include a requested capability, which is an
operator-actionable, **non-retryable** condition. The alternative mapping
(`ErrTrackerAPI`) is retryable and would make the orchestrator retry a plan
limitation forever, so the non-retryable bucket is the safer fit even though the
condition is not strictly an auth failure. The domain error enum has no
dedicated "configuration" kind; if one is added later, this is the case to move.

### Partial success

GraphQL permits `data` populated alongside non-empty `errors` (officially
documented: "queries can partially succeed with a 200"). Policy:

- **Mutations:** any non-empty `errors` array is a failure, regardless of
  `data` or `success`.
- **State batches (operations 4 and 5):** these use connection filters
  (`id: { in }`, `number: { in }`), so a missing issue is simply an absent node,
  never an error. Do not rely on per-alias error handling; the non-null-abort
  behavior above makes aliased batches unusable for this.
- **Other reads:** if the requested root field is non-null and usable, log the
  errors at DEBUG and return the data; if the root field is null, classify the
  first error.

---

## Collection writes: the read-before-write hazard

The issue-update mutation exposes one field with replace semantics and a pair
with delta semantics for the same collection:

| `IssueUpdateInput` field | Semantics (schema doc)                                     |
| ------------------------ | ---------------------------------------------------------- |
| `labelIds`               | "labels associated with this ticket": **replaces the set** |
| `addedLabelIds`          | "labels to be added": appends                              |
| `removedLabelIds`        | "labels to be removed": removes                            |

Writing through `labelIds` requires first reading the complete current label
set. Because `issue.labels` is itself a paginated connection, a naive
implementation that reads one page (default 50) and writes back
`existing + new` **silently deletes every label beyond the first page**. This
is the read-before-write pagination hazard this adapter must not have:

- `AddLabel` uses `addedLabelIds` exclusively. No read of the current set, no
  pagination, no race window between read and write. **[live-verified]** Adding
  `needs-human` via `addedLabelIds` to an issue that already had `Feature` left
  the issue with both labels, so the append semantics hold and the read is
  genuinely unnecessary.
- If any future write path genuinely needs `labelIds` (full replacement), it
  MUST paginate `issue.labels` to exhaustion first, and apply the
  missing-cursor guard, before composing the write.

The same audit applies to any other collection-valued update field added
later; check for an `added*`/`removed*` pair in the schema before reaching
for the replace-style field. (Subscriber lists, for example, follow the same
pattern in `IssueUpdateInput`.)

---

## Config notes

- **`tracker.api_key`:** single `lin_api_...` string, sent verbatim in
  `Authorization` (no Bearer, no `email:token` composition).
- **`tracker.endpoint`:** defaults to `https://api.linear.app/graphql`; there
  is no self-hosted Linear, so overriding only serves tests and mocks.
- **`tracker.project`:** the team key (`ENG`). Validated at startup by the
  team-states preflight; an unknown key is a configuration error
  (`ErrTrackerPayload` at construction).
- **`tracker.active_states` / `tracker.terminal_states` /
  `tracker.handoff_state`:** team-scoped state **names**, resolved to
  canonical casing at startup. Example: active `["Backlog", "Todo"]`,
  terminal `["Done", "Canceled", "Duplicate"]`, handoff `"In Review"`.
- **Page size:** 50 top-level, 25 for nested connections inside issue nodes;
  always explicit `first`.
- **Network timeout:** 30,000 ms.
- **No API-version header exists**; the GraphQL schema evolves in place.
  Breaking-change exposure is limited by requesting only needed fields.
- **Write side effects:** Linear emits webhooks on data changes, including
  issue, comment, and label events. The adapter's writes (`TransitionIssue`,
  `CommentIssue`, `AddLabel`) are ordinary data changes, so they can trigger any
  webhook automations the operator has configured in the workspace. Whether a
  change made through the API also notifies the key's own user is not documented
  and was not verified. Sortie polls rather than consuming webhooks, so this is
  an operator-awareness note, not an adapter dependency.

---

## Key differences from the Jira and GitHub adapters

| Aspect               | Jira                               | GitHub                             | Linear                                                        |
| -------------------- | ---------------------------------- | ---------------------------------- | ------------------------------------------------------------- |
| Protocol             | REST, multiple endpoints           | REST, multiple endpoints           | GraphQL, single POST endpoint                                 |
| Auth header          | `Basic base64(email:token)`        | `Bearer <token>`                   | `<api_key>` verbatim, no scheme prefix                        |
| Error transport      | HTTP status codes                  | HTTP status codes                  | `errors[]` inside HTTP 200 bodies                             |
| Rate-limit signal    | 429 + `Retry-After`                | 403/429 + headers                  | 400 (or 429) + `RATELIMITED` body code                        |
| Rate-limit model     | Points quota (65K/hr)              | Requests (5K/hr) + search (30/min) | Requests (dynamic, 2.5K live) + complexity (3M/hr, 10K/query) |
| State model          | Workflow states + transition graph | open/closed + labels-as-states     | Team-scoped named states + 7 type categories                  |
| Transitions          | Transition API (graph-restricted)  | Label add/remove + close/reopen    | `issueUpdate(stateId)`, any state to any state                |
| Issue identifier     | `PROJ-123` (project key)           | `299` (repo-scoped number)         | `ENG-123` (team key + number), plus UUID                      |
| Lookup by both forms | id or key in path                  | number in path                     | `issue(id:)` accepts UUID or identifier                       |
| Description/comments | ADF tree (flatten) or wiki markup  | Markdown                           | Markdown                                                      |
| Priority             | `priority.id` numeric              | none native                        | 0..4 numeric, 0 = none (map to nil)                           |
| Blockers             | `issuelinks` type parsing          | `dependencies/blocked_by` endpoint | `inverseRelations` where `type == "blocks"`                   |
| Branch name          | dev-status API (extra call)        | not in API response                | `branchName` field, always present                            |
| Pagination           | `nextPageToken` / offset           | `Link` header                      | Relay cursors (`pageInfo`, `endCursor`)                       |
| Label writes         | n/a for adapter                    | additive label endpoints           | `addedLabelIds` delta (avoid `labelIds` replace)              |

---

## Live verification results

The six items the pre-key draft flagged as unverified were all resolved against
the live API on 2026-06-13 (team `SOR`):

1. **Entity-not-found shape.** HTTP 200, `extensions.type: "invalid input"`,
   `code: "INPUT_ERROR"`, `message: "Entity not found: Issue"`. No dedicated
   not-found code; classify by message. Also nulls the whole `data` object. See
   Error model.
2. **Maximum `first` = 250.** Confirmed: 251 is rejected with "first must not be
   greater than 250".
3. **`orderBy: createdAt` direction = descending (newest first).** Confirmed;
   the adapter must re-sort comments ascending client-side.
4. **`X-Complexity` measurements.** Candidate query 95 (not the ~5,900 the
   documented formula predicts); the top-level `first` is not multiplied. See
   the measured-complexity table.
5. **`commentCreate.input.issueId` accepts shorthand identifiers.** Confirmed
   (`"SOR-5"` worked); UUID still preferred for clarity.
6. **`issueLabelCreate` permission.** Worked with the full-access test key and
   created a team-scoped label. The `FORBIDDEN` degradation path for a
   restricted key was not exercised and remains documentation-sourced.

One additional finding the live run surfaced that the draft did not anticipate:
the aliased-`issue(id:)` batch for `FetchIssueStatesByIdentifiers` is broken by
the non-null `Issue!` return type (one miss nulls the whole response). Operation
5 was rewritten to a `number: { in: [...] }` connection filter.

A later verification pass confirmed several secondary facts now folded into the
sections above: the `branchName` format is workspace-configurable
(`Organization.gitBranchFormat` template), so the prefix is opaque; label
creation is gated by the **Team settings > Access and permissions** label
toggle, sharpening the `FORBIDDEN` guidance; `first` has an enforced minimum of 1
(`first: 0` rejected); and the issue URL format is
`https://linear.app/<urlKey>/issue/<IDENTIFIER>/<slug>` (live `urlKey` =
`sortie-ai`).

### Integration test setup

The live test workspace is configured and reusable: team key `SOR`, with states
Backlog/Todo/In Progress/In Review/Done/Canceled/Duplicate and labels
Feature/Bug/Improvement/needs-human. Issues SOR-5 (Todo, assigned, labeled,
2 comments, blocks SOR-7, parent of SOR-8), SOR-6 (Done), SOR-7 (Backlog,
blocked by SOR-5), SOR-8 (Todo sub-issue) exercise every read path; SOR-5 was
moved to In Review by the transition test.

Integration tests gate on `SORTIE_LINEAR_TEST=1` (plus `SORTIE_LINEAR_API_KEY`
and `SORTIE_LINEAR_TEAM_KEY`) and must skip cleanly when unset, per the
project's existing convention. Keep them read-only by default; gate the
transition, comment, and label-write paths behind an additional explicit opt-in
so a default run never mutates the workspace.

---

## Source attribution

| Topic                                                                           | Primary source                                                                     | Cross-check                                                                              |
| ------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| Endpoint, auth headers, viewer query                                            | Linear Developers: GraphQL getting started (`linear.app/developers/graphql`)       | Context7 `/websites/linear_app_developers` quotes                                        |
| API key creation, scopes, team limits                                           | Linear Docs: API and webhooks (`linear.app/docs/api-and-webhooks`)                 | First-party only                                                                         |
| Key prefixes, leak revocation                                                   | Linear changelog 2021-08-19 (GitHub secret scanning)                               | GitHub secret-scanning program docs                                                      |
| OAuth scopes and token lifetime                                                 | Linear Developers: OAuth 2.0 (`linear.app/developers/oauth-2-0-authentication`)    | First-party only                                                                         |
| Rate limits, complexity scoring, headers                                        | Linear Developers: Rate limiting (`linear.app/developers/rate-limiting`)           | SDK `RatelimitedLinearError` header reads                                                |
| Pagination defaults and ordering                                                | Linear Developers: Pagination (`linear.app/developers/pagination`)                 | Connection arg docs in the published schema                                              |
| Schema shapes (all types, inputs, enums)                                        | `linear/linear` `packages/sdk/src/schema.graphql` @ `df20561` (2026-06-11)         | Official docs examples where available                                                   |
| `issue(id:)` / `issueUpdate` accept identifiers                                 | **Live API** (`issue(id: "SOR-5")` and `commentCreate(issueId: "SOR-5")` accepted) | Official docs examples (`issue(id: "BLA-123")`, "UUID or a shorthand ID")                |
| Error envelope and type classification                                          | `linear/linear` `packages/sdk/src/error.ts` (official client parser)               | **Live API** error responses (auth, not-found, validation)                               |
| Entity-not-found message shape                                                  | **Live API** (`issue(id:)` on a nonexistent id), 2026-06-13                        | Schema: `Query.issue` returns non-null `Issue!`, explaining the `data: null` propagation |
| Relation direction semantics                                                    | **Live API** (created SOR-5 blocks SOR-7, read SOR-7 inverseRelations)             | Schema doc strings on `IssueRelation`                                                    |
| Max `first`, orderBy direction, complexity, identifier acceptance, label append | **Live API**, team `SOR`, 2026-06-13                                               | Schema constraints and SDK where applicable                                              |
| Dynamic request rate limit (2,500 live vs 5,000 documented)                     | **Live API** response headers                                                      | Web search: Linear scales request limit by paid seats                                    |

### Context7 verification report

Library resolved: `/websites/linear_app_developers` (347 snippets, High
reputation). Queries confirmed: the auth header formats (API key verbatim vs
OAuth Bearer), the `issue(id: "BLA-123")` lookup-by-identifier example, the
`issueUpdate` "UUID or shorthand ID" statement, and the `RATELIMITED` error
envelope. Context7 did not cover API key scopes (taken from first-party user
docs). The items Context7 and the docs left open (not-found shape, maximum page
size, sort direction, real complexity) are now closed by live verification
rather than left single-sourced.
