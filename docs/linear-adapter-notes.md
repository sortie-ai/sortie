# Linear GraphQL API: Adapter research notes

> Linear public GraphQL API, hosted cloud only, pinned to the published SDK
> schema `linear/linear@df20561` (2026-06-11). Instruments: live GraphQL calls
> against the Linear workspace `sortie-ai`, team `SOR`, on 2026-06-13; the
> official developer and user documentation; and the official TypeScript SDK
> error classifier (`packages/sdk/src/error.ts`). Reference for the Linear
> `domain.TrackerAdapter` implementation in `internal/tracker/linear`.
>
> Claims tagged **[live-verified]** rest on the live pass, and where the live API
> contradicts the published documentation the live behavior wins and the conflict
> is recorded. Every other claim is schema- or documentation-sourced and names its
> source in Source attribution.
>
> Coverage: authentication, identifiers, the team state model, all nine
> `domain.TrackerAdapter` operations, field mapping, pagination, rate limiting,
> and the error model are anchored to the pinned schema and the 2026-06-13 live
> pass. The forbidden label-create path and the rate-limit response body are not
> live-observed; see Open questions.

Linear exposes a single GraphQL endpoint rather than the REST surfaces of the
Jira and GitHub adapters. Client construction, query shapes, pagination, and
error handling all differ. `linear.newLinearClient` builds on
`httpkit.NewClient` with no third-party GraphQL library: a GraphQL call is an
HTTP POST with a JSON body, and the response is a JSON envelope. Nothing more is
required.

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

`linear.checkViewer` runs it at construction, inside `linear.runPreflight`, so a
failure classifies the key before the first poll cycle. The adapter discards the
returned payload and keeps only the classification.

**[live-verified]** A valid key returns
`{ "data": { "viewer": { "id": "<uuid>", "name": "...", "email": "..." } } }`
on HTTP 200 (`X-Complexity: 2`). An invalid key returns **HTTP 401** with a body
error: `message: "Authentication required, not authenticated"`,
`extensions.type: "authentication error"`, `code: "AUTHENTICATION_ERROR"`,
`statusCode: 401`, `userPresentableMessage: "You need to authenticate to access
this operation."` So an invalid key fails at both the HTTP layer (401) and the
body layer; the adapter's body-first classifier handles it either way.

---

## Endpoint and transport

- Single endpoint: `POST https://api.linear.app/graphql`.
- Request: `Content-Type: application/json`, body
  `{ "query": "...", "variables": { ... } }`.
- Response: always JSON. Success data lives under `data`; errors live in a
  top-level `errors` array, **potentially alongside partial `data`**.
- The endpoint supports introspection; the full schema is also published at
  `linear/linear` (`packages/sdk/src/schema.graphql`).
- Network timeout: 30,000 ms, the [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)
  default, set by `linear.defaultTimeout` and passed to `httpkit.ClientOptions.Timeout`.

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

The decisive rule, which the spread above confirms: **HTTP status alone never
classifies a Linear response.** An entity-not-found and an argument-validation
failure both arrive on HTTP 200 with the real error in the body; an invalid key
arrives on 401 with the same body shape. Error classification inspects the
response body first and falls back to the status code only when the body carries
no `errors` array (see Error model).

### httpkit fit

GraphQL inverts two assumptions the REST side of `httpkit` is built on, and the
adapter resolves each one:

- All calls are `POST` to one path, and the rate-limit and `X-Complexity`
  headers must be readable on success. `linearClient.Execute` posts through
  `httpkit.Client.SendWithHeaders`, which returns the body together with a clone
  of the response headers; the endpoint carries the full URL, so the path
  argument is empty.
- Cursor state travels in the JSON `variables` object (`after`), not in a URL
  query parameter, so `httpkit.NewTokenPaginator` does not apply. The adapter
  keeps its own loop of the same shape, `linear.paginate`: issue the request,
  decode `nodes` plus `pageInfo`, stop when `hasNextPage` is false, bound the
  walk at `linear.maxPages`, and report a missing cursor (see Pagination).

Error classification stays in the `httpkit` mold: `linear.classifyResponseBody`
and `linear.classifyTransport` are wired into `httpkit.ClientOptions` as the
error and transport classifiers, and `classifyResponseBody` adds the body-level
pass that REST adapters do not need.

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
a response says which form was used, so the adapter passes the form it means
verbatim and never constructs one from the other.

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
`StringComparator`), built by `linear.buildFetchFilter`; no team UUID resolution
is needed for reads. Only the label-create write path needs the team UUID, and
it resolves one per call through `LinearAdapter.resolveTeamID` (see operation 9).

---

## State model

Linear has real named states, and every state also carries a workspace-immutable
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
Duplicate (`duplicate`). `triage` is opt-in per team, so it is excluded by
default. The adapter's defaults for this stock layout are
`linear.defaultActiveStates` (`["Backlog", "Todo", "In Progress"]`) and
`linear.defaultTerminalStates` (`["Done", "Canceled", "Duplicate"]`), applied
when the config omits the list and registered as `DefaultActiveStates` and
`DefaultTerminalStates` in the adapter's `registry.TrackerMeta`. There is no
default `handoff_state`; an operator-added `"In Review"` (`started`) fills it.

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

The `type` field does not drive candidate selection (names are more precise and
match the other adapters). It serves the **startup preflight** instead:
`linear.runPreflight` fetches the team's states once through
`linear.fetchTeamStates` and verifies every configured name exists, comparing
case-insensitively in Go:

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
   candidate list. A name absent from the team, and an unknown team key, both
   return `domain.ErrTrackerPayload` and block construction.
2. The adapter caches the canonical casing of each configured name in the map
   `runPreflight` returns, and `linear.canonicalize` applies it to every state
   list the fetch queries send. The `in` state filter is case-sensitive
   (`StringComparator` distinguishes `eq`/`in` from `eqIgnoreCase`, which has
   no `in` analogue), so queries must send the canonical names.
   **[live-verified]** `state: { name: { in: ["todo"] } }` returned zero issues
   while `["Todo"]` returned all six; case-sensitivity of the `in` filter is
   real, and the preflight casing cache is mandatory, not a nicety.
3. `linear.warnWrongCategory` emits a WARN when a configured `active_states`
   entry resolves to a state whose `type` is `completed`, `canceled`, or
   `duplicate`, and the reverse for `terminal_states`. The categories are
   wrong-config tripwires even though they do not drive selection, so the
   mismatch never fails construction.

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
team, and `LinearAdapter.TransitionIssue` returns `domain.ErrTrackerPayload`
for it; a null `issue` means the issue itself is missing and returns
`domain.ErrTrackerNotFound`. Unlike Jira there is no transition graph: any state
can move to any state, so resolve-then-update is the entire flow.

**[live-verified]** The resolve query with `stateName: "in review"` (lowercase)
against `issue("SOR-5")` returned the team's `In Review` state and its UUID, then
`issueUpdate(id: "SOR-5", input: { stateId: <uuid> })` returned
`success: true` with `issue.state.name == "In Review"`. The full handoff path,
case-insensitive resolve plus update, works end to end.

---

## Operations

The `domain.TrackerAdapter` Go interface has nine methods. Seven of them are the
required tracker operations in the architecture spec ([Section 11.1](architecture/11-issue-tracker-integration-contract.md#111-required-operations)),
`FetchCandidateIssues` through `TransitionIssue`; the interface adds
`CommentIssue` (lifecycle comments) and `AddLabel` (CI-failure label
escalation) beyond that required set. All nine map onto the single GraphQL
endpoint and are covered below as operations 1 through 9 (`CommentIssue` and
`AddLabel` are operations 8 and 9).

### 1. `FetchCandidateIssues`: `issues` query, team + active states

```graphql
query CandidateIssues($filter: IssueFilter!, $first: Int!, $after: String) {
  issues(
    first: $first
    after: $after
    filter: $filter
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
        pageInfo {
          hasNextPage
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
        pageInfo {
          hasNextPage
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

Variables: `{ "filter": { ... }, "first": 50, "after": null }`. The filter
travels in the variables object, never in the query text.
`linear.buildFetchFilter` allocates it per call from the configured team key,
the state list, and the operator's `tracker.query_filter` fragment (see Config
notes), so concurrent fetches never share a map.

Notes:

- The `sort` argument exists on `issues` (`IssueSortInput`), but
  **[live-verified]** its priority ordering is non-obvious and the adapter does
  not rely on it. With issues of priority 0, 2, and 3 present,
  `sort: [{ priority: { order: Ascending, noPriorityFirst: false } }, { createdAt: { order: Ascending } }]`
  returned the no-priority (0) group first, then priority 3, then priority 2,
  which is neither plain ascending nor descending by the numeric value. Instead
  of depending on server-side priority semantics,
  `linear.sortByPriorityThenCreated` sorts candidates client-side by normalized
  priority (nil last) then `createdAt`, giving deterministic dispatch order
  consistent with the other adapters. The `sort` argument stays on the candidate
  query as a coarse hint and is dropped from the by-states query
  (`linear.queryIssuesByStates`), where dispatch order is irrelevant.
- Comments are deliberately not fetched; `LinearAdapter.fetchIssues` sets
  `Issue.Comments` to nil per the interface contract, and the pre-dispatch
  `FetchIssueByID` supplies them.
- Nested connections are capped at `first: 25` as cheap insurance against a
  pathological issue (hundreds of labels or blockers); the candidate query
  measured only 95 complexity points, so this is not cap-avoidance (see Rate
  limiting). The adapter does not paginate nested connections. It selects their
  `pageInfo.hasNextPage` and `linear.warnNestedOverflow` emits a WARN naming the
  issue and the connection when the cap truncates, so a dropped label or blocker
  stays observable.
- Archived issues are excluded by default (`includeArchived` defaults to
  false); the adapter does not pass the argument.
- Assignee scoping belongs in the operator's `tracker.query_filter` fragment
  rather than a post-filter: `assignee: { isMe: { eq: true } }` selects issues
  assigned to the key's user without resolving the viewer id, and
  `assignee: { id: { eq: "<uuid>" } }` pins a specific user. **[live-verified]**
  `isMe: { eq: true }` returned only the one issue assigned to the key's user.

### 2. `FetchIssueByID`: `issue` query, UUID or identifier

The issue selection is the one shown for operation 1, plus an inline first page
of comments:

```graphql
query IssueByID($id: String!) {
  issue(id: $id) {
    # ...the operation 1 node selection...
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
`comments.pageInfo.hasNextPage` is true, `LinearAdapter.collectComments`
resumes the operation 6 connection from the inline `endCursor` and appends the
continuation pages, so the inline page is never re-fetched; an inline
`hasNextPage` with an empty `endCursor` returns
`domain.ErrTrackerMissingCursor`.

Not-found surfaces as a body-level error with `data.issue: null`, which
`FetchIssueByID` maps to `domain.ErrTrackerNotFound` (see Error model).

### 3. `FetchIssuesByStates`: same query, caller-supplied states

`linear.queryIssuesByStates` is the `FetchCandidateIssues` query without the
`sort` argument (order is irrelevant to cleanup), run with the
orchestrator-supplied state list; terminal cleanup passes terminal states.
`FetchIssuesByStates` returns an empty slice with no API call when the state
list is empty, and otherwise passes the names through the same canonical-casing
cache the preflight built, because `in` matching is case-sensitive while the
interface contract promises case-insensitive comparison.

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

`IssueIDComparator.in` takes UUIDs. `FetchIssueStatesByIDs` chunks at
`linear.stateBatchChunkSize` (50) ids per request and sets `first` to the chunk
length, then builds the result map from the returned nodes; ids absent from the
response are omitted from the map (the interface treats missing as
not-an-error). No `pageInfo` is needed because the page size equals the
requested id count. A connection filter is what makes that omission safe: an
aliased `issue(id:)` batch fails the whole response on one miss (see Error
model).

### 5. `FetchIssueStatesByIdentifiers`: team-key + number filter

`IssueFilter` has **no `identifier` field** (schema-verified; it offers `id`,
`number`, `team`, and others). `linear.extractNumbers` splits each identifier on
its last hyphen and keeps the trailing integer (`"SOR-7"` to `7`), skipping any
identifier whose trailing part is not an integer, and the query filters by the
number set scoped to the configured team key. The team half of the identifier is
not read: every identifier the orchestrator passes belongs to the configured
team, and `tracker.project` supplies the team key.

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
matching filter. `FetchIssueStatesByIdentifiers` builds the result map keyed by
`identifier` from the returned nodes; numbers absent from the response are
omitted (the interface treats missing as not-an-error), and the number list is
chunked at `linear.stateBatchChunkSize` (50).

The connection filter, not an aliased `issue(id:)` batch, is what makes a
missing issue harmless here. An alias batch nulls the entire `data` object when
any one alias misses, which is the exact reconciliation case this method serves
(see Error model). **[live-verified]** The connection-filter form has no such
failure: `number: { in: [5, 7, 99999] }` returned SOR-5 and SOR-7 and silently
dropped 99999.

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
  came back newest-then-oldest. Agents want chronological context, so
  `linear.sortCommentsByCreatedAt` sorts the accumulated slice by `createdAt`
  ascending before returning. The client-side sort is mandatory, not defensive.
- `body` is markdown ("derived representation of the canonical bodyData
  ProseMirror content" per the schema); the adapter passes it through without
  flattening.
- `user` is null for comments created by integrations or bots; `botActor`
  covers agent and bot comments. `linear.resolveCommentAuthor` resolves the
  author as `user.displayName`, then `user.name`, then `botActor.name`, else the
  empty string. `externalUser` exists for Slack- and Intercom-originated
  comments and the adapter does not select it, so such comments resolve to
  `botActor.name` or the empty string. **[live-verified]** A human-authored
  comment returned `user: { displayName, name, email }` with `botActor: null`;
  the comment `body` round-tripped verbatim, including a raw issue URL (mention
  rendering happens in the UI, not in the stored markdown).
- An empty connection returns an empty non-nil slice. The query re-selects the
  parent issue on every continuation page, so a not-found mid-pagination
  surfaces as `domain.ErrTrackerNotFound` from `linear.decodeCommentsPage`.

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
`IssuePayload { success: Boolean!, issue: Issue, lastSyncId: Float! }`.
`linear.runMutation` applies the result policy for every mutation: a non-empty
`errors` array is a failure regardless of `success`, and `success: false` with
no errors maps to `domain.ErrTrackerAPI` (defensive; Linear normally signals
failure through `errors`).

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
in a comment body to reference issues for human readers. A plain URL does not
render as an interactive mention "pill": **[live-verified]** the comment body
round-trips verbatim through the API (the URL is stored as plain text), and
mention-pill rendering is a paste-time editor behavior that the editor itself
lets users decline ("Keep as link"). The adapter treats URLs as plain text and
makes no mention guarantee.

### 9. `AddLabel`: resolve + optional create + append

Linear attaches labels by UUID, so "add label by name" is a compound operation.
`IssueUpdateInput` offers a replace-style field and an append/remove pair for
the same collection, and `LinearAdapter.AddLabel` uses the append form
`addedLabelIds` (see Collection writes for the hazard the replace form carries).
That reduces `AddLabel` to three steps.

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
        key
      }
    }
  }
}
```

The selection carries `team.key`, not `team.id` alone, because
`tracker.project` is a team **key** and `IssueLabel.team.id` is a UUID: matching
the configured project against the UUID field never matches and silently
demotes every team-scoped label. `LinearAdapter.resolveLabelID` compares
`node.team.key` with the configured project, returns the first team-scoped
match, and falls back to the first workspace label (`team` null) when no
team-scoped label matches.

**Step 2.** If no label matched, resolve the owning team's UUID with
`LinearAdapter.resolveTeamID` (the label create needs a UUID, and the transition
path that would otherwise resolve one does not run here), then create the label
scoped to that team:

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
`LinearAdapter.createLabel` always passes the team id, because workspace-label
management is more likely to require elevated permissions. On any payload-class
create error, which covers the name-uniqueness conflict a concurrent create
produces, `AddLabel` re-runs step 1 once and uses the now-existing id; only a
re-resolve that still finds nothing surfaces the original create error. A
create forbidden for the key classifies as `domain.ErrTrackerAuth`. The
orchestrator treats label errors as non-fatal (`internal/orchestrator` logs the
failure at WARN and counts a CI-escalation error), and the operator remedy is to pre-create the escalation label in Linear.

Label-creation is gated by a specific, named team setting, not just admin
status: under **Team settings > Access and permissions**, a team owner chooses
whether **all members** or **only team owners** may create and edit issue
labels (per Linear's members-and-roles docs and the team-owners changelog). So
a Read+Write member key gets a `FORBIDDEN`/`feature not accessible` error on
`issueLabelCreate` precisely when that team has restricted label management to
owners. The operator has two clean remedies: flip that team setting to allow
members, or pre-create the escalation label (default `needs-human`) once, after
which the adapter's lookup in step 1 finds it and never calls
`issueLabelCreate`.

**Step 3.** Append by id; no read of the existing label set is needed:

```graphql
mutation IssueAddLabel($id: String!, $labelIds: [String!]!) {
  issueUpdate(id: $id, input: { addedLabelIds: $labelIds }) {
    success
  }
}
```

**[live-verified]** The whole flow works end to end:
`issueLabelCreate(input: { teamId, name: "needs-human" })` returns
`success: true` with a team-scoped label id, and
`issueUpdate(id: "SOR-5", input: { addedLabelIds: [<id>] })` left the issue with
**both** its pre-existing `Feature` label and the new `needs-human` label. The
append semantics are real and the read-before-write step is genuinely
unnecessary.

---

## Field mapping

`linear.normalizeIssue` maps a Linear issue node to `domain.Issue` as follows
(all Linear field names schema-verified):

| `domain.Issue` field | Linear source                                         | Notes                                                                                                                                                                                                                                                                  |
| -------------------- | ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ID`                 | `issue.id`                                            | UUID string                                                                                                                                                                                                                                                            |
| `Identifier`         | `issue.identifier`                                    | e.g. `"ENG-123"`                                                                                                                                                                                                                                                       |
| `DisplayID`          | empty                                                 | Identifiers are already display-ready                                                                                                                                                                                                                                  |
| `Title`              | `issue.title`                                         |                                                                                                                                                                                                                                                                        |
| `Description`        | `issue.description`                                   | Markdown per schema; nullable, null becomes empty string                                                                                                                                                                                                               |
| `Priority`           | `issue.priority`                                      | Float in schema: 0 = No priority, 1 = Urgent, 2 = High, 3 = Medium, 4 = Low. `linear.normalizePriority` maps 1..4 to `*int` and 0 or null to nil                                                                                                                                                          |
| `State`              | `issue.state.name`                                    | Original casing preserved                                                                                                                                                                                                                                              |
| `BranchName`         | `issue.branchName`                                    | Non-null; auto-generated. Format is workspace-configurable (see below); treat the whole string as opaque, never parse the prefix                                                                                                                                       |
| `URL`                | `issue.url`                                           | Provided directly, no construction                                                                                                                                                                                                                                     |
| `Labels`             | `issue.labels.nodes[].name`                           | Lowercased by `issuekit.NormalizeLabels` ([Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules)); non-nil empty slice when none                                                                                                                                                                                                           |
| `Assignee`           | `assignee.displayName`, fallback `name`, then `email` | Resolved by `linear.resolveAssignee`; `assignee` is strictly the `User` type, nullable; null becomes empty string. Agents/apps are not `User`s (they surface under the separate `delegate`/`botActor` fields the adapter does not read), so an agent-driven issue with no human assignee normalizes to empty |
| `IssueType`          | empty                                                 | Linear has no native issue-type field                                                                                                                                                                                                                                  |
| `Parent`             | `issue.parent` `{id, identifier}`                     | Nil when absent                                                                                                                                                                                                                                                        |
| `Comments`           | separate connection (operation 6)                     | Nil when not fetched; empty non-nil when fetched and empty                                                                                                                                                                                                             |
| `BlockedBy`          | `inverseRelations.nodes` where `type == "blocks"`     | Extracted by `linear.extractBlockers`; see below                                                                                                                                                                                                                                                              |
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
the tracker whose native model the rule was phrased for. `linear.extractBlockers`
compares `type` case-insensitively after trimming, defensively, and always
returns a non-nil slice.

**[live-verified]** With SOR-5 set to block SOR-7
(`issueRelationCreate(issueId: SOR-5, relatedIssueId: SOR-7, type: blocks)`),
reading **SOR-7** returned
`inverseRelations.nodes: [{ type: "blocks", issue: { identifier: "SOR-5",
state: { name: "Todo" } } }]`. The blocked issue carries the relation, the
blocker is `node.issue`, and `type` came back as the lowercase string
`"blocks"`. The direction in the mapping above is correct.

**[live-verified]** The nullability behavior in the table above holds on live
issues: `priority` came back 2 and 3 on prioritized issues and 0 on onboarding
issues; `description` was markdown and `null` on an issue created without one;
`assignee` was `{ displayName, name, email }` when assigned and `null` when
unassigned; `parent` was `{ id, identifier }` on a sub-issue; and `url` was
supplied directly with no construction needed.

### `branchName` is not a fixed format

`branchName` is non-null and auto-generated, but its prefix is **not** stable
across workspaces. **[live-verified]** SOR-5 returned
`tasks/sor-5-implement-...`, where `tasks/` is the acting user's handle rather
than a fixed token. The schema explains why: `Organization.gitBranchFormat` is a
workspace-level template ("Supports template variables like `{issueIdentifier}`
and `{issueTitle}`. If null, the default formatting will be used"), so the
default prefix derives from the acting user's handle and the whole format is
operator-configurable. The adapter MUST treat `branchName` as an opaque string:
`linear.normalizeIssue` copies it into `domain.Issue.BranchName` unparsed, and
no code may assume a `<handle>/<identifier>-<slug>` shape.

### Comment mapping

| `domain.Comment` field | Linear source                                                              |
| ---------------------- | -------------------------------------------------------------------------- |
| `ID`                   | `comment.id`                                                               |
| `Author`               | `user.displayName`, fallback `user.name`, then `botActor.name`, else empty |
| `Body`                 | `comment.body` (markdown, pass through)                                    |
| `CreatedAt`            | `comment.createdAt`                                                        |

---

## Pagination

Linear uses Relay-style cursor connections everywhere, and `linear.paginate`
walks them:

- Arguments: `first` (forward page size) + `after` (cursor); `last`/`before`
  exist for backward paging and the adapter does not use them.
- Every connection exposes `pageInfo { hasNextPage endCursor }`. The loop sends
  `after: null` on the first request, then `after: endCursor`, until
  `hasNextPage` is false. The cursor travels in the `variables` object, so a
  caller may seed `variables["after"]` to resume a connection past a page it
  already holds; `paginate` preserves that seed instead of restarting at the
  first page, which is what keeps the inline comment page from being re-fetched
  and duplicated (see operation 2).
- Default page size is 50 when `first` is omitted (documented and repeated in
  every connection's schema doc). Sortie always passes `first` explicitly:
  `linear.topLevelPageSize` (50) for top-level collections
  ([Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)),
  and a literal 25 for the nested `labels` and `inverseRelations` connections
  inside issue nodes.
- `first` must be in the range **1 to 250**, both bounds enforced and
  **[live-verified]**: `first: 251` fails with
  `constraints.max: "first must not be greater than 250"` and `first: 0` fails
  with `constraints.min: "first must not be less than 1"`, both as an
  `Argument Validation Error` on HTTP 200. With Sortie's page size of 50, neither
  bound is reachable; keep `first` between 1 and 250.
- When `hasNextPage` is true but `endCursor` is empty or null, the loop returns
  `domain.ErrTrackerMissingCursor` (`tracker_missing_end_cursor`, [Section 11.4](architecture/11-issue-tracker-integration-contract.md#114-error-handling-contract))
  instead of treating pagination as complete. Silent truncation here is a
  data-loss bug; this mirrors the Jira adapter's guard.
  `LinearAdapter.collectComments` applies the same guard to the inline comment
  page before it hands the cursor to `paginate`.
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
- The loop is bounded by `linear.maxPages` (200, so 10,000 items at the
  top-level page size). On reaching it with more pages available, `paginate`
  logs a WARN carrying `max_pages` and returns the items accumulated so far.
  This is the adapter's own bound; `httpkit.PaginatorOptions.OnLimitReached`
  belongs to the REST paginator, which the GraphQL path does not use.

---

## Rate limiting

### Request budget

The official rate-limiting page documents 5,000 requests/hour for an API key,
5,000 for an OAuth app, and 600 unauthenticated.

**[live-verified] conflict:** the live test workspace returned
`x-ratelimit-requests-limit: 2500`, **half** the documented figure. The request
limit is **dynamic**: Linear scales it by the number of paid seats in the
workspace, so a free or single-seat workspace gets less than the documented
headline. No quota figure may be hardcoded; the response headers are the source
of truth. `linear.recordRateLimit` reads
`x-ratelimit-requests-remaining` after every call and treats exhaustion as an
observable event rather than tracking the quota itself.

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

The authoritative cost is always the `X-Complexity` response header, never the
documented formula.

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
seconds. `linear.parseResetSeconds` divides by `linear.rateLimitResetDivisor`
(1000) before the value is comparable to a Go `time.Unix` value.

### Detection and backoff

A rate-limited response is documented to arrive as **HTTP 400 with
`errors[].extensions.code == "RATELIMITED"`**. The official SDK additionally
treats HTTP 429 as rate-limited and reads `Retry-After`; the two sources
disagree on the status code, so `linear.classifyGraphQLErrors` keys on the body,
accepting either `extensions.code == "RATELIMITED"` or
`extensions.type == "ratelimited"` on any status, and `linear.classifyHTTPStatus`
maps a bare 429 with no errors array to the same kind. Classification is
`domain.ErrTrackerAPI`, which `domain.TrackerErrorKind.RetryClassification`
reports as retryable with `domain.BackoffExponential`, so the orchestrator owns
the delay.

The adapter does not sleep on `Retry-After` and does not track the quota. Its
only rate-limit behavior is observational: `linear.recordRateLimit` logs one
WARN, "rate limit exhausted", carrying `requests_remaining`, the reset time in
epoch seconds, and `Retry-After` when present, and only when
`x-ratelimit-requests-remaining` reaches zero. The one place a delay is applied
in the adapter is construction: `linear.runPreflight` retries a transient
preflight failure through `httpkit.RetryWithBackoff` on the shared
`httpkit.DefaultPreflightBackoff` schedule (1 s, 2 s, 4 s) before failing.

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

There is **no dedicated not-found code or type**. The envelope above is the
whole of it: a nonexistent issue arrives on HTTP 200 under the generic
`type: "invalid input"`, so the only signal that separates not-found from any
other input error is the **message**. An error whose `message` begins with
`Entity not found` (case-insensitive) maps to `domain.ErrTrackerNotFound`;
`linear.notFoundPrefix` holds that prefix and the check is a lowercase,
trimmed `strings.HasPrefix`. The `userPresentableMessage`
"Could not find referenced Issue." is a secondary confirmation.

**The non-null-abort gotcha.** `Query.issue` returns `Issue!`. When the
resolver fails, the executor propagates null to the root, **nulls the entire
`data` object**, and abandons sibling root fields in the same operation.
**[live-verified]** A batch of `i0: issue("SOR-5")`, `i1: issue("SOR-99999")`,
`i2: issue("SOR-7")` returned `data: null` with a single error for `i1`, and the
two valid lookups returned nothing. An aliased `issue(id:)` batch is therefore
unusable for the state-batch operations 4 and 5, where a deleted or renamed
issue is the normal case: one miss would wipe the states of every other issue in
the batch. For a single `FetchIssueByID`, a not-found simply yields `data: null`
plus the not-found error, which classifies cleanly.

### Classification algorithm

`linear.classifyGraphQLErrors` classifies a decoded errors array, and
`linear.classifyHTTPStatus` is the fallback for a non-2xx response with no
errors array. `linear.classifyResponseBody`, wired in as the `httpkit` error
classifier, runs the body test first and falls back to the status:

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
                "graphql error"}:                    ErrTrackerPayload
    if type in {"internal error", "network error",
                "lock timeout", "bootstrap error"}:  ErrTrackerTransport
    if userError:                                    ErrTrackerPayload
    otherwise:                                       ErrTrackerAPI
  if the response is non-2xx with no errors array:
    400:                                             ErrTrackerPayload
    401, 403:                                        ErrTrackerAuth
    429:                                             ErrTrackerAPI (retryable)
    5xx:                                             ErrTrackerTransport
    any other:                                       ErrTrackerAPI
  if the body does not parse as JSON:                ErrTrackerPayload
```

The not-found message check runs **first**, before the `type`-based rules,
precisely because not-found arrives under the generic `type: "invalid input"`
that would otherwise classify as `ErrTrackerPayload`.
`linear.classifiedMessage` puts the first error's `userPresentableMessage`
(falling back to its `message`) in `domain.TrackerError.Message`, so operators
see Linear's own wording.

`feature not accessible` is grouped with `domain.ErrTrackerAuth` deliberately.
It means the workspace plan does not include a requested capability, which is an
operator-actionable, **non-retryable** condition. The alternative mapping,
`domain.ErrTrackerAPI`, is retryable and would make the orchestrator retry a
plan limitation forever, so the non-retryable bucket is the safer fit even
though the condition is not strictly an auth failure.

### Partial success

GraphQL permits `data` populated alongside non-empty `errors` (officially
documented: "queries can partially succeed with a 200"). The adapter does not
consume partial data: every decode path runs `classifyGraphQLErrors` on the
errors array before it looks at `data`, so a non-empty array fails the call on
reads and writes alike.

- **Mutations:** a non-empty `errors` array is a failure regardless of `data` or
  `success`, enforced once in `linear.runMutation`.
- **State batches (operations 4 and 5):** these use connection filters
  (`id: { in }`, `number: { in }`), so a missing issue is an absent node rather
  than an error, and no per-alias error handling is involved. The non-null-abort
  behavior above is what makes aliased batches unusable here.
- **Other reads:** a non-empty `errors` array is classified and returned as the
  call's error even when the root field is populated. A null root field on a
  clean errors array is the separate not-found case each read maps explicitly
  (`FetchIssueByID`, `decodeCommentsPage`, `TransitionIssue`,
  `resolveTeamID`).

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

- `LinearAdapter.AddLabel` uses `addedLabelIds` exclusively (query
  `linear.queryIssueAddLabel`). No read of the current set, no pagination, no
  race window between read and write; operation 9 carries the live evidence that
  the append preserves existing labels.
- A write through `labelIds` (full replacement) MUST paginate `issue.labels` to
  exhaustion first, and apply the missing-cursor guard, before composing the
  write. No adapter path uses `labelIds`.

Every collection-valued field in `IssueUpdateInput` needs the same audit: check
for an `added*`/`removed*` pair in the schema before reaching for the
replace-style field. Subscriber lists follow the same pattern.

---

## Config notes

- **`tracker.api_key`:** single `lin_api_...` string, sent verbatim in
  `Authorization` (no Bearer, no `email:token` composition). Required:
  `NewLinearAdapter` returns `domain.ErrMissingTrackerAPIKey` when it is empty.
  The offline `sortie validate` hook `linear.validateAPIKeyHint` warns on a
  missing key, on surrounding whitespace, and on a key without the `lin_api_`
  prefix, and never puts the key value in a diagnostic.
- **`tracker.endpoint`:** defaults to `linear.defaultEndpoint`
  (`https://api.linear.app/graphql`) when empty or whitespace-only; there is no
  self-hosted Linear, so overriding only serves tests and mocks. A present
  value is parsed by `linear.resolveEndpoint` and rejected at construction
  (`domain.ErrTrackerPayload`) unless it is an absolute http(s) URL carrying a
  host; see [Endpoint parsing and offline validate
  parity](#endpoint-parsing-and-offline-validate-parity).
- **`tracker.project`:** the team key (`ENG`). Required:
  `NewLinearAdapter` returns `domain.ErrMissingTrackerProject` when it is empty.
  Validated at construction by the team-states preflight; an unknown key returns
  `domain.ErrTrackerPayload`. The offline hook `linear.validateProject` flags
  whitespace as an error and a `/` as a warning, and leaves existence and casing
  to the preflight.
- **`tracker.active_states` / `tracker.terminal_states` /
  `tracker.handoff_state`:** team-scoped state **names**, resolved to canonical
  casing at construction. Example: active `["Backlog", "Todo"]`, terminal
  `["Done", "Canceled", "Duplicate"]`, handoff `"In Review"`. The two lists fall
  back to the adapter defaults when omitted (see State model); `handoff_state`
  has no default.
- **`tracker.query_filter`:** an optional JSON object merged into the fetch
  filter by `linear.buildFetchFilter` as extra `IssueFilter` fields, which
  Linear ANDs with the adapter's own constraints. `linear.parseQueryFilter`
  rejects a value that is not a JSON object, and rejects the top-level reserved
  keys `team` and `state` so the operator cannot widen the team or state scope.
  The fragment travels in the GraphQL variables object, never in the query text.
- **Page size:** see Pagination.
- **Network timeout:** see Endpoint and transport.
- **No API-version header exists**; the GraphQL schema evolves in place.
  Breaking-change exposure is limited by requesting only needed fields.
- **Write side effects:** Linear emits webhooks on data changes, including
  issue, comment, and label events. The adapter's writes (`TransitionIssue`,
  `CommentIssue`, `AddLabel`) are ordinary data changes, so they can trigger any
  webhook automations the operator has configured in the workspace. Sortie polls
  rather than consuming webhooks, so this is an operator-awareness note, not an
  adapter dependency.

### Endpoint parsing and offline validate parity

`NewLinearAdapter` resolves `endpoint` through `linear.resolveEndpoint`, a
thin wrapper over `httpkit.ResolveEndpoint`. An empty or whitespace-only
value substitutes `linear.defaultEndpoint`; either way the result is parsed
by `httpkit.ParseEndpoint`, so the default host and an operator-supplied host
are validated identically before either reaches the GraphQL client. A
supplied value that does not parse as an absolute http(s) URL carrying a
host fails construction with `domain.ErrTrackerPayload`. `url.Parse`'s own
error text quotes the whole raw URL, so an endpoint written as
`scheme://user:secret@host` would otherwise name both the username and the
password in the failure message; the constructor's message instead carries
only `httpkit.Endpoint.Redacted`, the userinfo-stripped form. This closes the
same exposure Gitea's endpoint guard closes, though Linear's single hosted
host makes the case a test or mock endpoint rather than a self-hosted
deployment.

Linear registers no `SCMAdapter` and no `CIStatusProvider`, so `tracker.endpoint`
is the only endpoint any Linear-backed surface reads. Unlike Gitea, there is no
`extensions.linear` block that could hand a different endpoint to a second
constructor, so the offline validate verdict always matches the construction
verdict for every surface Linear serves.

`validateConfig`'s `validateEndpoint` decides its verdict from the same
`resolveEndpoint` call the constructor makes:

| Check | Severity | Condition |
| --- | --- | --- |
| `tracker.endpoint.invalid` | `error` | a present, non-empty `tracker.endpoint` does not parse as an absolute http(s) URL with a host |

An empty or whitespace-only endpoint produces no diagnostic, since it resolves
to the default host. There is deliberately no plain-http warning here, unlike
Gitea's `tracker.endpoint.insecure`, because Linear is a single hosted host
with no self-hosted deployment mode. The diagnostic message never echoes the
configured value.

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

## Integration test setup

The live test workspace is configured and reusable: team key `SOR`, with states
Backlog, Todo, In Progress, In Review, Done, Canceled, and Duplicate, and labels
Feature, Bug, Improvement, and needs-human. Issues SOR-5 (assigned, labeled,
commented, blocks SOR-7, parent of SOR-8), SOR-6, SOR-7 (blocked by SOR-5), and
SOR-8 (sub-issue) exercise every read path.

`internal/tracker/linear/integration_test.go` gates every test, read and write
alike, on the single variable `SORTIE_LINEAR_TEST=1` and skips cleanly when it
is unset, matching the sibling adapters. `skipUnlessIntegration` enforces the
gate; `newIntegrationAdapter` requires `SORTIE_LINEAR_API_KEY` and
`SORTIE_LINEAR_TEAM_KEY`, and reads the optional `SORTIE_LINEAR_ENDPOINT` and
`SORTIE_LINEAR_ACTIVE_STATES` overrides. The write tests are shaped to leave the
workspace where they found it: `TestIntegration_TransitionIssue` re-applies the
issue's current state, and `TestIntegration_AddLabel` adds the idempotent
`needs-human` label. `TestIntegration_CommentIssue` is the one test that leaves a
trace, a timestamped comment on the first candidate issue.

---

## Open questions

Each entry names the probe that would settle it.

- **The `FORBIDDEN` label-create path.** `issueLabelCreate` is documented to
  fail for a member key when the team restricts label management to owners, and
  the adapter classifies that as `domain.ErrTrackerAuth`, but the branch is
  documentation-sourced: the live pass covers only a full-access key. Probe: set
  **Team settings > Access and permissions** to owners-only, call
  `issueLabelCreate` with a member key restricted to that team, and record the
  status, `extensions.type`, and `extensions.code`.
- **The rate-limit error body.** The 400-versus-429 status and the
  `RATELIMITED` envelope are documentation- and SDK-sourced; the live workspace
  cannot be exhausted without abusing it. Probe: on a disposable workspace,
  drive requests past `x-ratelimit-requests-limit` and capture the status,
  headers, and full errors array of the first rejected call.
- **Mention rendering of a URL posted through the API.** A comment body
  round-trips verbatim, but whether the Linear UI renders a bare issue URL in an
  API-created comment as an interactive mention is not documented. Probe: post a
  comment whose body is a bare issue URL through `commentCreate`, then read the
  same comment's `bodyData` and compare it with a comment where the URL was
  pasted in the editor.
- **Self-notification on API writes.** Linear emits webhooks for the adapter's
  writes, but whether a change made with a personal API key also notifies that
  key's own user is not documented. Probe: with notifications enabled for the
  key's user, run `issueUpdate` and `commentCreate` on an issue that user
  subscribes to, then read the `notifications` connection for entries whose
  actor is that user.

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
