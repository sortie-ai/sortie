# Jira REST API: Adapter research notes

> Jira Cloud REST API v3 and Jira Server / Data Center REST API v2. No Jira deployment
> version, revision, or edition is pinned: Jira Cloud is continuously deployed, the adapter
> reads no version route, and no Server or Data Center instance has been probed. The Cloud v3
> surface is documentation-derived from a reading of Atlassian's published REST API
> documentation in March 2026; the Server / Data Center v2 surface is documentation-derived
> from a second reading in June 2026, when `tracker.api_version` support shipped. Neither
> reading recorded a per-claim citation, so this document carries no per-claim evidence tags
> for its Jira statements and no probe record. Reference for implementing the Jira
> `TrackerAdapter`. The adapter serves no `SCMAdapter` and no `CIStatusProvider` surface.

## Provenance of every claim

This is the weakest-evidenced document in the `docs/*-adapter-notes.md` family, and stating
that plainly is more useful than implying a rigor it does not have. One tag marks the Jira
claims with stronger backing:

| Tag | Meaning |
| --- | --- |
| **[live-exercised]** | Exercised against a live Jira Cloud site by the env-gated integration suite `internal/tracker/jira/integration_test.go`, which the release and nightly workflows run with `SORTIE_JIRA_TEST=1` against site credentials held as repository secrets. The suite asserts that a route answers and that the normalized `domain.Issue` fields are populated; it captures no payload and inspects no header, so it is weaker evidence than a recorded probe, and it backs only the claims it is attached to. |

Every other Jira statement in this document is untagged: it comes from the documentation
readings named in the provenance block, it carries no per-claim citation, and no running
instance has confirmed it. Treat an untagged Jira sentence as a documentation summary, not as
observed behavior. Unsettled points are collected in [Open questions](#open-questions) rather
than hedged inline.

Claims about sortie's own code carry a Go symbol or package path instead of a tag and were
each checked against `internal/tracker/jira/` and `internal/domain/`. Reading this repository
is not an observation of Jira.

The unit-test fixtures under `internal/tracker/jira/testdata/` are hand-authored, not captured
payloads. They pin the adapter's decoding of a shape someone believed Jira returns; they are
not evidence about Jira and are never cited as such below.

---

## Authentication

Jira accepts several authentication schemes. The adapter uses Basic with an API token on v3,
and Basic or Bearer on v2.

### Basic auth with an API token (Cloud, v3)

The standard scheme for scripts and service integrations. It uses the user's email and an API
token generated from their Atlassian account profile.

- Generate a token at `https://id.atlassian.com/manage/api-tokens`.
- Header: `Authorization: Basic <base64(email:api_token)>`

Sortie uses this scheme on v3 because it runs as a background service, not an interactive
user-facing application. **[live-exercised]** Every Jira integration job authenticates this
way against a live Cloud site.

### Personal access tokens (Server and Data Center, v2)

Personal access tokens replace Basic Auth passwords on Jira Data Center and Server and behave
like bearer tokens.

- Header: `Authorization: Bearer <your_pat>`

`resolveAuth` selects the scheme from `tracker.api_key` on a presence-of-colon rule:

- `api_key` contains a colon (`user:password`): Basic auth, `Authorization: Basic
  base64(user:password)`. Both sides of the colon must be non-empty; a colon at the first or
  last character is rejected at construction with `domain.ErrTrackerAuth`.
- `api_key` has no colon (a colon-free token string): Bearer auth, `Authorization: Bearer
  <token>`. This is the personal-access-token path for Server and Data Center.

Under an effective `api_version` of `"3"`, the default when the field is unset, a colon-free
`api_key` is always rejected with `domain.ErrTrackerAuth`, because v3 requires `email:token`
format. The rule keys on the version, not the host: a self-hosted endpoint left at the default
version rejects a personal access token the same way a Cloud endpoint does, and the remedy is
either an `email:token` key or `tracker.api_version: "2"`. `authFormatError` owns both message
texts.

### OAuth 2.0

The scheme Atlassian recommends for external apps acting on Jira on behalf of users. It uses
the authorization code grant (3LO, three-legged OAuth), restricts scope, and avoids sharing
user credentials.

The adapter does not implement it. Sortie runs as a headless background service and has
neither the interactive 3LO authorization flow nor the user-facing callback the pattern
requires.

### Config mapping

| Config field            | Cloud (v3)                                           | Server / DC (v2)                                    |
| ----------------------- | ---------------------------------------------------- | --------------------------------------------------- |
| `tracker.endpoint`      | `https://<site>.atlassian.net` (no trailing /)       | `https://jira.internal.example.com` (no trailing /) |
| `tracker.api_key`       | `email:api_token`; splits on first `:`               | `user:password` (Basic) or a token (Bearer)         |
| `tracker.project`       | Jira project key, e.g. `SORT`                        | Same                                                |
| `tracker.api_version`   | `"3"` (default, may be omitted)                      | `"2"` (required for Server / DC)                    |

Encoding `email:token` in a single field follows curl convention (`-u email:token`) and avoids
adding Jira-specific config keys to the core schema. `internal/config` resolves
`tracker.api_key` through `os.ExpandEnv`, so a value such as `$JIRA_API_KEY` expands from the
environment.

`normalizeAPIVersion` accepts a string, an int, or a whole-number float, trims surrounding
whitespace, and resolves an absent or empty value to `"3"`. Any other value returns
`domain.ErrTrackerPayload`.

### Construction-time guards

`NewJiraAdapter` rejects a bad configuration before any network call, in this order:

1. An empty `endpoint` returns `domain.ErrTrackerPayload`.
2. An `endpoint` containing `/rest/api/` returns `domain.ErrTrackerPayload`: the field is the
   base URL, and the adapter appends the REST path itself.
3. An `api_version` outside `"2"` and `"3"` returns `domain.ErrTrackerPayload`
   (`normalizeAPIVersion`).
4. An `endpoint` that is not a URL with both a scheme and a host returns
   `domain.ErrTrackerPayload` (`checkHostVersion`, which redacts any userinfo in the message
   through `httpkit.RedactURLUserinfo`).
5. A `.atlassian.net` endpoint combined with `api_version: "2"` returns
   `domain.ErrTrackerPayload` (`cloudVersionConflict`).
6. An empty `api_key` returns `domain.ErrMissingTrackerAPIKey`; a malformed one returns
   `domain.ErrTrackerAuth` (`resolveAuth`).
7. An empty `project` returns `domain.ErrMissingTrackerProject`.

A non-`.atlassian.net` endpoint combined with `api_version: "3"` logs a warning and proceeds,
except for a loopback IP or `localhost` endpoint, which `isLocalEndpoint` treats as a test or
local-dev target and does not warn about.

### Offline validate parity

`validateConfig` re-decides the same faults for `sortie validate` without constructing an
adapter or touching the network, and reuses the constructor's own message text where one
exists, so the offline verdict cannot drift from the startup verdict. It emits eight
Jira-specific diagnostics, all at severity `error`:

| Check | Condition |
| --- | --- |
| `tracker.endpoint.missing` | `tracker.endpoint` is empty |
| `tracker.endpoint.api_suffix` | the trimmed endpoint contains `/rest/api/` |
| `tracker.endpoint.invalid` | the trimmed endpoint is not a URL with a scheme and host |
| `tracker.api_version.invalid` | `tracker.api_version` is outside `"2"` and `"3"` |
| `tracker.api_version.cloud_conflict` | `"2"` against a `.atlassian.net` endpoint |
| `tracker.api_key.jira_format` | the colon sits at the first or last character of the key |
| `tracker.api_key.jira_cloud_format` | a colon-free key against a Cloud endpoint |
| `tracker.api_key.jira_v3_format` | a colon-free key at an effective version of `"3"` |

It also runs the shared state diagnostics: `registry.DiagStateLabelElements` on
`tracker.active_states` and `tracker.terminal_states` at severity `warning`, and
`registry.DiagStateOverlap`. An invalid `api_version` suppresses the Cloud-conflict
diagnostic, mirroring the constructor, which never reaches the host/version guard for a
version it rejects. `validateAPIKeyFormat` skips its version arm when the endpoint host cannot
be classified, since `validateEndpoint` already reports that fault. The `api_key` value never
appears in a diagnostic message.

### CAPTCHA challenge

After several failed logins Jira triggers CAPTCHA and returns `X-Seraph-LoginReason:
AUTHENTICATION_DENIED`. `classifyHTTPError` maps 401 and 403 to `domain.ErrTrackerAuth`
whether or not the header is present; when it carries that value the error message names the
CAPTCHA challenge and points the operator at a browser login. The header changes the message,
not the error kind.

---

## Endpoints and operations

Each `domain.TrackerAdapter` operation maps to a Jira REST endpoint. `basePath` returns
`/rest/api/3` when `api_version` is `"3"` (the default) and `/rest/api/2` when it is `"2"`.
The routes below show the v3 paths; substitute `/rest/api/2` for v2. The one exception is the
search resource: v3 uses `/rest/api/3/search/jql` whereas v2 uses `/rest/api/2/search`, with
no `/jql` segment.

`newJiraClient` builds the shared `httpkit.Client` with a 30 second timeout and sets
`Authorization`, `Accept: application/json`, and `User-Agent` on every request. The user agent
is `sortie/<version>`, injected by `cmd/sortie` as the `user_agent` adapter config key, and
falls back to `sortie/dev` when absent.

### Body format by version

On v3, `description` and comment `body` are ADF (Atlassian Document Format) JSON trees, which
`extractBody` flattens to text through `flattenADF` before storing in
`domain.Issue.Description` and `domain.Comment.Body`.

On v2, the same fields are JSON strings carrying Jira wiki markup (for example `h2. Heading`,
`*bold*`, `{code}...{code}`). `extractBody` unquotes the JSON string and returns it verbatim,
translating no markup token. Downstream consumers (prompt templates, agents) therefore see
wiki markup rather than clean prose when `api_version: "2"` is set. Rendered HTML
(`expand=renderedBody`) is not requested and not in scope. Empty or undecodable input yields
an empty string on both versions.

Comment creation mirrors the split, through `commentPayload`. On v2 the request body is a raw
string:

```json
{"body": "<text>"}
```

On v3 it is an ADF document (see [ADF flattening](#adf-flattening)).

### 1. `FetchCandidateIssues`

`GET /rest/api/3/search/jql`, built by `buildCandidateJQL`:

```
project = "<KEY>" AND status IN ("<state1>", "<state2>", ...) ORDER BY priority ASC, created ASC
```

Query params: `jql`, `fields`, `maxResults`, `nextPageToken`.

The `fields` value is the `searchFields` constant, which requests only `summary`, `status`,
`priority`, `labels`, `assignee`, `issuetype`, `parent`, `issuelinks`, `created`, `updated`,
and `description`. It does not request `comment`; comments have their own endpoint, and
`FetchCandidateIssues` sets `Comments` to nil on every issue it returns.

When `tracker.query_filter` is set, its raw JQL fragment is appended as ` AND (<fragment>)`
before the `ORDER BY` clause.

**[live-exercised]** The route answers with this JQL and the requested fields populate `ID`,
`Identifier`, `Title`, `State`, `Labels`, `URL`, `CreatedAt`, and `UpdatedAt` on every issue
returned.

### 2. `FetchIssueByID`

`GET /rest/api/3/issue/{issueIdOrKey}` with the `fields` param set to `searchFields`. The
`issueID` argument is the Jira issue key, for example `PROJ-123`. The operation then calls
`fetchComments` for the same issue, so a fully populated issue costs two or more requests. A
404 on either request is re-wrapped as `domain.ErrTrackerNotFound` with the message
`issue not found: <key>`.

**[live-exercised]** The route answers for a key drawn from the candidate set, returns a
non-nil `Comments` slice, and returns `domain.ErrTrackerNotFound` for a key that does not
exist.

### 3. `FetchIssuesByStates`

`GET /rest/api/3/search/jql`, built by `buildStatesFetchJQL`:

```
project = "<KEY>" AND status IN ("<state1>", ...) ORDER BY created ASC
```

Same endpoint as the candidate fetch with different JQL, and `tracker.query_filter` merges the
same way. No orchestrator caller and no agent tool invokes this operation; startup terminal
cleanup is served by `FetchIssueStatesByIdentifiers` instead. Adapters implement it to satisfy
`domain.TrackerAdapter`. An empty `states` argument returns an empty slice without a request.

**[live-exercised]** The route answers, and every issue returned carries a state from the
requested set.

### 4. `FetchIssueStatesByIDs`

`GET /rest/api/3/search/jql`, built by `buildIDINJQL`:

```
id IN (10001, 10002, ...) ORDER BY key ASC
```

`id IN (...)` takes Jira's numeric internal IDs, which is what this operation receives.
`buildIDINJQL` drops any non-numeric element, so a caller bug such as passing a key surfaces
as a missing map entry rather than a query against the wrong issue; a batch with no numeric ID
left produces no request at all. Only the `status` field is requested, keeping the payload
small. Used for active-run reconciliation. `tracker.query_filter` is deliberately not applied:
these issues already passed filtering at dispatch time.

Keys and IDs are batched at `batchSize`, 40 per request, on every call rather than only on
large sets, because a long `IN` clause in a GET URL exceeds URI length limits. The loop checks
`ctx.Err()` between batches.

**[live-exercised]** The route answers for a batch of candidate IDs and returns a non-empty
state for each.

### 5. `FetchIssueStatesByIdentifiers`

`GET /rest/api/3/search/jql`, built by `buildKeyINJQL`:

```
key IN ("PROJ-1", "PROJ-2", ...) ORDER BY key ASC
```

`key IN (...)` takes project-prefixed keys. Only the `status` field is requested, the same
`batchSize` of 40 applies, and `tracker.query_filter` is likewise not applied. Used for
startup terminal workspace cleanup. Issues not found are omitted from the returned map rather
than reported as an error.

### 6. `FetchIssueComments`

`GET /rest/api/3/issue/{issueIdOrKey}/comment`.

Query params: `startAt`, `maxResults`, `orderBy`. `fetchComments` sends `orderBy=created` and
a `maxResults` of `maxCommentResults`, 50.

Response: `{ startAt, maxResults, total, comments: [...] }`. Each comment carries `id`,
`author.displayName`, `body`, and `created`; `normalizeComments` maps them through
`issuekit.NormalizeComments`, and a nil author yields an empty author string. A 404 becomes
`domain.ErrTrackerNotFound`. No comments yields an empty non-nil slice.

**[live-exercised]** The route answers and every comment returned carries a non-empty `ID` and
`CreatedAt`.

### 7. `TransitionIssue`

The orchestrator calls this after a successful worker run to move the issue to the configured
`tracker.handoff_state`, for example "Human Review" (per ADR-0007).

- `GET /rest/api/3/issue/{issueIdOrKey}/transitions` lists the transitions available for the
  issue given the current user's permissions and the workflow rules. The adapter matches the
  configured target state against `transition.to.name` case-insensitively and takes the first
  match, not the transition label (`transition.name`), which avoids workflow-specific naming
  fragility.
- `POST /rest/api/3/issue/{issueIdOrKey}/transitions` executes the transition. Request body:
  `{ "transition": { "id": "<transition_id>" } }`. Success is `204 No Content` with an empty
  body.

When no available transition targets the requested state, the adapter returns
`domain.ErrTrackerPayload` with the message `no transition to state %q available for issue
%s`, without issuing the POST.

OAuth scopes required: `write:jira-work` (classic) or `write:issue:jira` (granular). This is
an escalation from the read-only scopes (`read:jira-work`) the fetch operations use.

### 8. `CommentIssue`

`POST /rest/api/3/issue/{issueIdOrKey}/comment` with the body shape described in
[Body format by version](#body-format-by-version). On v3 `buildADFComment` splits the text on
newlines and emits one ADF paragraph node per line, so line breaks render correctly in Jira's
UI; an empty line becomes a paragraph with empty content.

### 9. `AddLabel`

`PUT /rest/api/3/issue/{issueIdOrKey}` with an update-verb body:

```json
{"update": {"labels": [{"add": "<label>"}]}}
```

Used for CI failure escalation. The orchestrator treats `AddLabel` errors as non-fatal.

---

## Field mapping

`normalizeSearchIssue` maps a Jira response to `domain.Issue`:

| `domain.Issue` field | Jira response path                | Notes                                   |
| -------------------- | --------------------------------- | --------------------------------------- |
| `ID`                 | `id` (string)                     | Numeric ID as a string                  |
| `Identifier`         | `key` (string)                    | e.g. `"PROJ-123"`                       |
| `Title`              | `fields.summary`                  |                                         |
| `Description`        | `fields.description`              | v3: ADF flattened to text. v2: raw wiki-markup string. |
| `Priority`           | `fields.priority.id` (string)     | `issuekit.ParsePriorityIntFromString`; a non-integer yields nil. Read `id`, never `name`. |
| `State`              | `fields.status.name`              | Original casing preserved              |
| `BranchName`         | not set                           | See [branch name](#branch-name)         |
| `URL`                | `{endpoint}/browse/{key}`         | Constructed by the adapter              |
| `Labels`             | `fields.labels` (string array)    | `issuekit.NormalizeLabels` lowercases each |
| `Assignee`           | `fields.assignee.displayName`     | Nil yields an empty string              |
| `IssueType`          | `fields.issuetype.name`           |                                         |
| `Parent`             | `fields.parent.id`, `.parent.key` | Nil when absent                         |
| `Comments`           | separate endpoint                 | See operation 6                         |
| `BlockedBy`          | `fields.issuelinks[]` (filtered)  | See [blocker extraction](#blocker-extraction-from-issuelinks) |
| `CreatedAt`          | `fields.created`                  | Stored verbatim as a string             |
| `UpdatedAt`          | `fields.updated`                  | Stored verbatim as a string             |

**[live-exercised]** `CreatedAt` and `UpdatedAt` parse as either RFC 3339 or the Jira
millisecond-and-offset form `2006-01-02T15:04:05.000-0700`; the adapter itself performs no
timestamp parsing.

### Branch name

`domain.Issue.BranchName` is left empty. The core REST API does not carry it. Jira Cloud
exposes development information at
`GET /rest/dev-status/latest/issue/detail?issueId={id}&applicationType=GitHub`, which returns
the branches, commits, and pull requests linked to an issue when a source control tool (GitHub,
Bitbucket) is connected. The adapter does not call that route.

### Blocker extraction from `issuelinks`

The "Blocks" link type has `type.inward = "is blocked by"` and `type.outward = "blocks"`. Read
from the blocked issue, the blocking issue appears in `inwardIssue`. If issue A blocks issue
B, then on issue B the link looks like:

```json
{
  "type": { "name": "Blocks", "inward": "is blocked by" },
  "inwardIssue": {
    "id": "10020",
    "key": "PROJ-20",
    "fields": { "status": { "name": "To Do" } }
  }
}
```

`extractBlockers` keeps links whose `type.name` equals the `blockerLinkTypeName` constant,
`"Blocks"`, and whose `inwardIssue` is present, and builds a `domain.BlockerRef` from
`inwardIssue.id` and `inwardIssue.key`, adding `inwardIssue.fields.status.name` as the blocker
state when the response carries it. Links with only an `outwardIssue` are skipped, so a
dependent issue is never mistaken for a blocker.

The comparison is exact and the link type name is a compile-time constant, so a Jira admin who
renames the "Blocks" link type silently removes every blocker from the adapter's view, and
blocked issues dispatch anyway.

### ADF flattening

An ADF document is a typed node tree:

```json
{
  "type": "doc",
  "version": 1,
  "content": [
    {
      "type": "paragraph",
      "content": [{ "type": "text", "text": "Hello world" }]
    }
  ]
}
```

`flattenADF` walks the tree recursively, concatenating the `text` value of every `text` node
and appending a newline after each node whose type is in `blockLevelTypes` (paragraph,
heading, the list and table families, blockquote, codeBlock, rule, media, panel, decision, and
task nodes). Trailing newlines and spaces are trimmed from the result. Nil or non-map input
yields an empty string. Without this step, `Description` and comment `Body` would carry raw
JSON.

---

## Pagination

### v3 search endpoint, cursor-based

`paginatedSearch` drives `httpkit.NewTokenPaginator` against `/rest/api/3/search/jql` with the
`nextPageToken` parameter and a `maxResults` of `maxSearchResults`, 50.

- First request: omit `nextPageToken`.
- Subsequent requests: pass the `nextPageToken` from the previous response.
- The walk ends when the response carries no `nextPageToken`.

`httpkit.Paginator.All` treats an absent token as the end of the walk unconditionally. The
Jira decoder never returns `domain.ErrTrackerMissingCursor`, so a truncated result set caused
by a dropped cursor is indistinguishable from a complete one.

`POST /rest/api/3/search/jql` also accepts JQL in the request body and so avoids URI length
limits on very long queries, but it paginates by offset (`startAt`/`total`) and Atlassian
recommends the cursor-based GET for new integrations. The adapter uses GET only; its JQL is
short enough, and long `IN` clauses are handled by batching instead.

### v2 search endpoint, offset-based

`paginatedSearchV2` maintains a `startAt` counter against `/rest/api/2/search` with the same
page size of 50.

- First request: `startAt=0`, `maxResults=50`.
- Subsequent requests: `startAt += len(page.issues)`.
- Stop when `len(page.issues) == 0` or `startAt + len(page.issues) >= total`.

The loop checks `ctx.Err()` before each request. It mirrors the comment pagination loop, which
both versions share.

### Comment endpoint, offset-based

`fetchComments` sends `startAt` (0-indexed) and `maxResults` of 50, and continues while
`startAt + len(comments) < total`, on both versions.

---

## Rate limiting

Jira Cloud enforces three independent rate limiting systems.

### Points-based quota, per hour

- Each call consumes points: base 1 plus object costs, so a single-issue GET costs 2 points.
- Default quota: 65,000 points per hour, reset at the top of the UTC hour.

### Burst rate limits, per second and per endpoint

- A token bucket per endpoint per tenant.
- Defaults: GET 100 req/s, POST 100 req/s, PUT 50 req/s, DELETE 50 req/s.
- `GET /rest/api/3/issue/{id}`: a 150 req/s bucket.
- `GET /rest/api/3/search/jql`: 100 req/s.

### Per-issue write limits

- 20 writes per 2 seconds and 100 writes per 30 seconds, per issue.
- The adapter writes on three operations: `TransitionIssue`, `CommentIssue`, and `AddLabel`.
  All three target a single issue and fire on session lifecycle events, so the write path is
  in scope for this limit even though the orchestrator's normal cadence stays well under it.

### 429 handling

All limits return `429 Too Many Requests` with:

| Header                  | Value                                               |
| ----------------------- | --------------------------------------------------- |
| `Retry-After`           | Seconds to wait (integer)                           |
| `X-RateLimit-Remaining` | Remaining capacity                                  |
| `X-RateLimit-Reset`     | ISO-8601 reset timestamp                            |
| `RateLimit-Reason`      | `jira-quota-global-based`, `jira-burst-based`, etc. |

`classifyHTTPError` maps 429 to `domain.ErrTrackerAPI` and interpolates `Retry-After` into the
error message when the header is present. `domain.TrackerError` has no field for the value, so
it survives as message text only, and the status reaches the caller through
`TrackerError.Status`. The adapter performs no retry and no backoff of its own:
`domain.TrackerErrorKind.RetryClassification` marks `ErrTrackerAPI` retryable with
`domain.BackoffExponential`, and the orchestrator owns the delay.

---

## Error mapping

`classifyHTTPError` maps a non-success HTTP response to a `domain.TrackerError`, reading up to
`maxErrorBody` (512) bytes of the body into the message for diagnostics:

| HTTP status | Condition                  | Error kind                |
| ----------- | -------------------------- | ------------------------- |
| 400         | Bad JQL, invalid request   | `tracker_payload_error`   |
| 401         | Invalid or expired token   | `tracker_auth_error`      |
| 403         | Insufficient permissions   | `tracker_auth_error`      |
| 404         | Issue or resource not found | `tracker_not_found`      |
| 429         | Rate limited               | `tracker_api_error`       |
| 5xx         | Server error               | `tracker_transport_error` |
| other       | Any other non-success status | `tracker_api_error`     |

`httpkit.ClassifyTransport` maps a TCP or DNS failure to `tracker_transport_error`. A JSON
decode failure on a 200 produces `tracker_payload_error` at each call site, with a message
naming the response that failed to parse. `FetchIssueByID` and `fetchComments` re-wrap a 404
as `tracker_not_found` with an `issue not found: <key>` message, replacing the generic route
message.

---

## Config notes

- **`tracker.api_key`:** `email:api_token` on v3, split on the first `:`. See
  [authentication](#authentication) for the v2 forms.
- **`tracker.endpoint`:** site URL without a trailing slash or path. The adapter appends
  `/rest/api/<version>/...`.
- **`tracker.project`:** Jira project key, used in the candidate and states JQL.
- **`tracker.active_states`:** defaults to `["Backlog", "Selected for Development", "In
  Progress"]` (`defaultActiveStates`, published to the registry as
  `registry.TrackerMeta.DefaultActiveStates`).
- **`tracker.terminal_states`:** common defaults: `["Done", "Cancelled"]`.
- **`tracker.query_filter`:** a raw JQL fragment appended as ` AND (<fragment>)` to the
  candidate and states queries. The adapter does not parse or validate it, and it is not
  applied to the reconciliation queries.
- **`tracker.api_version`:** `"2"` or `"3"`, defaulting to `"3"`.
- **JQL quoting:** `escapeJQLString` strips double-quote characters from every interpolated
  project key, state name, and issue key. JQL delimits string literals with double quotes and
  offers no backslash escape for them, so removal is the only safe treatment.
- **Network timeout:** 30 seconds, set on the shared `httpkit.Client`.

---

## Open questions

Carried forward explicitly rather than resolved by inference. Nothing in this section is
asserted anywhere else in the document.

| Question | Why it matters | Evidence that would settle it |
| --- | --- | --- |
| Does a live instance really place the blocking issue in `inwardIssue` when the link is read from the blocked issue? | `extractBlockers` reads `inwardIssue` only; a reversed convention silently drops every blocker and blocked issues dispatch anyway | Create a "Blocks" link between two issues and read `fields.issuelinks` from both sides |
| Under what `type.name` does a renamed "Blocks" link type appear? | `blockerLinkTypeName` compares exactly, so a rename is a silent blocker loss | Rename the link type in a test project and re-read `fields.issuelinks` |
| Do `CommentIssue` and `AddLabel` need scopes beyond `write:jira-work` / `write:issue:jira`? | The scope claim was read for transitions only; a missing scope surfaces as a 403 at runtime | Run each write with a token restricted to the classic write scope and record which return 403 |
| Can the v3 search response omit `nextPageToken` while results remain? | The paginator treats an absent token as the end of the walk, so a dropped cursor is a silent truncation | Page a result set larger than one page and compare the accumulated count against a JQL count of the same query |
| Is API-token traffic exempt from the points-based quota? | Decides whether the 65,000-point budget constrains the poll interval at all | Exceed 65,000 points in one UTC hour with an API token and record whether a 429 with `RateLimit-Reason: jira-quota-global-based` arrives |
| What values do the four rate-limit headers actually carry on a 429? | The header table is documentation-derived; the adapter reads only `Retry-After` | Provoke a burst 429 and capture `Retry-After`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, and `RateLimit-Reason` |
| Do the Server / Data Center v2 routes behave as documented? | The entire v2 path, auth, search, pagination, and wiki-markup bodies, has never met a running instance | Run the integration suite against a Data Center instance with `api_version: "2"` |
| What JSON envelope does each rejection class return? | `classifyHTTPError` reads only the status and copies raw body bytes, so a structured envelope is never parsed | Provoke 400 (bad JQL), 401, 403, 404, and 409 and record each response shape |
| At what key or ID count does the `IN (...)` GET URL exceed the deployment's URI limit? | `batchSize` is 40, chosen without measurement; too low costs round trips and too high returns 414 | Raise the batch size against a representative deployment until a request returns 414 |
| Does `GET /rest/dev-status/latest/issue/detail` return usable branch names? | It is the only known source for `domain.Issue.BranchName` on Jira | Connect a source control tool to a test site and read the route for an issue with a linked branch |
| Does ADF carry text in node types `flattenADF` does not handle, such as `inlineCard`, `mention`, or `emoji`? | Unhandled inline nodes drop content from the prompt without any error | Post a description containing each and compare the flattened output against the rendered text |
| Is `X-Seraph-LoginReason` present on Jira Cloud, or only on Server and Data Center? | The CAPTCHA message may be unreachable on the deployment sortie actually targets | Fail authentication repeatedly against both and capture the response headers |
| Which comment ordering does `orderBy=created` produce, and does the parameter exist on v2? | The workpad pattern assumes oldest-first comments | Read a multi-comment issue with and without the parameter on both versions |

---

## Source attribution

| Topic | Primary source | Verification method |
| --- | --- | --- |
| Authentication schemes, token creation URL, OAuth 3LO, personal access tokens | Atlassian REST API documentation, read March 2026 (Cloud) and June 2026 (Server / Data Center) | None recorded; no URL captured per claim |
| Route surface, query parameters, request and response shapes | Atlassian REST API documentation, same readings | Read-path routes exercised by `internal/tracker/jira/integration_test.go` against a live Cloud site; no payload captured |
| ADF document shape and the `search/jql` cursor contract | Atlassian REST API documentation, March 2026 | None recorded |
| v2 wiki-markup bodies and the offset-paginated `search` route | Atlassian REST API documentation, June 2026 | None recorded |
| Rate-limit systems, quotas, burst buckets, and 429 headers | Atlassian REST API documentation, March 2026 | None recorded |
| `X-Seraph-LoginReason` CAPTCHA behavior | Atlassian REST API documentation, March 2026 | None recorded |
| Every claim about the adapter's own behavior | `internal/tracker/jira` (`jira.go`, `client.go`, `jql.go`, `normalize.go`, `adf.go`, `validate.go`) and `internal/domain` (`tracker.go`, `errors.go`) | Repository code reading. No Jira observation is involved, so these claims carry a symbol rather than an evidence tag |
| Which operations reach a live instance in CI | `.github/workflows/release.yml` and `.github/workflows/nightly.yml`, the `SORTIE_JIRA_TEST` jobs | Workflow reading |
