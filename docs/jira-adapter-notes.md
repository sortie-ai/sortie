# Jira REST API: Adapter Research Notes

> Jira Cloud REST API v3, researched March 2026.
> Reference for implementing the Jira `TrackerAdapter`.

---

## Authentication

Jira supports several authentication methods depending on hosting environment and use case.

### Basic auth with API token (Cloud, recommended for Sortie)

The standard method for scripts and service integrations. Uses the user's email and an
API token generated from their Atlassian account profile.

- Generate a token at `https://id.atlassian.com/manage/api-tokens`.
- Header: `Authorization: Basic <base64(email:api_token)>`

This is the recommended method for Sortie because it runs as a background service, not
an interactive user-facing application.

### OAuth 2.0 (Cloud)

The recommended method for external apps accessing Jira on behalf of users. Uses the
authorization code grant type (3LO, Three-Legged OAuth). More secure as it restricts
scope and doesn't require sharing user credentials.

Not used by Sortie. Sortie runs as a headless background service and does not implement
the interactive 3LO authorization flow or user-facing callback required by this pattern.

### Personal Access Tokens (Data Center / Server)

PATs act as a secure alternative to Basic Auth passwords, behaving like bearer tokens.

- Header: `Authorization: Bearer <your_pat>`
- Available in Jira Data Center and Server.

Sortie supports PAT authentication when `tracker.api_version: "2"` is set. The adapter
selects the auth mode from the `tracker.api_key` value using a presence-of-colon rule:

- `api_key` contains a colon (`user:password`): Basic auth, `Authorization: Basic
  base64(user:password)`. Both sides of the colon must be non-empty; an empty user
  (`":password"`) or empty secret (`"user:"`) is rejected at construction time with
  `ErrTrackerAuth`.
- `api_key` has no colon (a colon-free token string): Bearer auth, `Authorization:
  Bearer <token>`. This is the PAT path for Server and Data Center.

Under an effective `api_version` of `"3"` - the default when the field is unset - a
colon-free `api_key` is always rejected with `ErrTrackerAuth`, because v3 requires
`email:token` format. The rule keys on the version, not the host: a self-hosted endpoint
left at the default version rejects a PAT the same way a Cloud endpoint does, and the
remedy is either an `email:token` key or `tracker.api_version: "2"`.

### Config mapping

| Config field            | Cloud (v3)                                           | Server / DC (v2)                                   |
| ----------------------- | ---------------------------------------------------- | -------------------------------------------------- |
| `tracker.endpoint`      | `https://<site>.atlassian.net` (no trailing /)       | `https://jira.internal.example.com` (no trailing /) |
| `tracker.api_key`       | `email:api_token`; splits on first `:`               | `user:password` (Basic) or PAT token (Bearer)      |
| `tracker.project`       | Jira project key, e.g. `SORT`                        | Same                                               |
| `tracker.api_version`   | `"3"` (default, may be omitted)                      | `"2"` (required for Server / DC)                   |

Encoding `email:token` in a single field follows curl convention (`-u email:token`) and avoids
adding Jira-specific config keys to the core schema. The value may be provided via environment
variable indirection (e.g. `$JIRA_API_KEY`) if the config layer supports it.

**Construction-time host/version guard:** An `endpoint` that is not a URL with a scheme
and host is rejected at startup (`ErrTrackerPayload`). A `.atlassian.net` endpoint combined
with `api_version: "2"` is rejected at startup (`ErrTrackerPayload`). A non-`.atlassian.net`
endpoint combined with `api_version: "3"` emits a warning and proceeds, except for a
loopback IP or `localhost` endpoint, which is a test or local-dev target and does not
warn.

**Offline parity:** the adapter's `sortie validate` hook re-decides the three
version-dependent faults above without constructing an adapter or touching the network -
an `api_version` outside `"2"` and `"3"`, `"2"` against an `.atlassian.net` endpoint, and
a colon-free `api_key` at an effective version of `"3"` - and reuses the constructor's own
message text, so the offline verdict cannot drift from the startup verdict. An invalid
`api_version` suppresses the Cloud-conflict diagnostic, mirroring the constructor, which
never reaches the host/version guard for a version it rejects.

**CAPTCHA caveat:** After several failed logins Jira triggers CAPTCHA and returns
`X-Seraph-LoginReason: AUTHENTICATION_DENIED`. The adapter should detect this header and
return `tracker_auth_error`.

---

## Endpoints

Each `TrackerAdapter` operation maps to a Jira REST endpoint. The base path is
`/rest/api/3` when `api_version` is `"3"` (default) and `/rest/api/2` when
`api_version` is `"2"`. The table below shows the v3 paths; substitute `/rest/api/2`
for v2. The one exception is the search resource: v3 uses `/rest/api/3/search/jql`
whereas v2 uses `/rest/api/2/search` (no `/jql` segment).

### v2 body format

On v2, `description` and comment `body` fields are returned as a **raw string in Jira
wiki markup** (for example `h2. Heading`, `*bold*`, `{code}...{code}`). The adapter
reads this string verbatim; it does not flatten or translate markup tokens. Downstream
consumers (prompt templates, agents) therefore see wiki markup rather than clean prose
when `api_version: "2"` is set. The v3 path returns ADF, which the adapter flattens to
text before storing in `domain.Issue.Description` and `domain.Comment.Body`.

Comment creation on v2 sends a raw-string body:

```json
{"body": "<text>"}
```

Comment creation on v3 sends an ADF document (see ADF flattening section below).

### v2 search pagination

The v2 search endpoint (`GET /rest/api/2/search`) uses offset-based pagination
(`startAt` / `total`). The adapter loops until `startAt + page_count >= total` or the
page is empty. Page size is 50 for both versions.

### 1. `FetchCandidateIssues` → `GET /rest/api/3/search/jql`

JQL:

```
project = "<KEY>" AND status IN ("<state1>", "<state2>", ...) ORDER BY priority ASC, created ASC
```

Query params: `jql`, `fields`, `maxResults`, `nextPageToken`

Request only needed fields:
`summary`, `status`, `priority`, `labels`, `assignee`, `issuetype`, `parent`,
`issuelinks`, `created`, `updated`, `description`

Does **not** request `comment` (separate call; comments use a dedicated endpoint).

Note: `POST /rest/api/3/search/jql` also accepts JQL in the request body and avoids URI
length limits for very long queries. However, POST uses offset-based pagination and
Atlassian recommends the GET endpoint with cursor-based pagination. Sortie's JQL queries
are short enough for GET.

### 2. `FetchIssueByID` → `GET /rest/api/3/issue/{issueIdOrKey}`

Query param `fields` to select specific fields. Returns a single issue with full detail.

The `description` field uses **ADF** (Atlassian Document Format), a JSON tree, not plain text.
Must be flattened (see ADF section below).

### 3. `FetchIssuesByStates` → `GET /rest/api/3/search/jql`

JQL:

```
project = "<KEY>" AND status IN ("<state1>", ...) ORDER BY created ASC
```

Same endpoint as candidate fetch, different JQL. No orchestrator caller invokes this
operation; startup terminal cleanup is served by `FetchIssueStatesByIdentifiers` instead.
Paginate to fetch all matching issues.

### 4. `FetchIssueStatesByIDs` → `GET /rest/api/3/search/jql`

JQL:

```
key IN ("PROJ-1", "PROJ-2", ...) ORDER BY key ASC
```

Request only `status` field to minimize payload. Used for active-run reconciliation.

Note: `id IN (...)` uses numeric internal IDs; `key IN (...)` uses project-prefixed keys.

With many running issues (50+), the `key IN (...)` JQL in a GET URL may exceed URI length
limits. When this happens, split the keys into smaller batches and issue multiple
`GET /rest/api/3/search/jql` requests so each URL stays within safe limits.

### 5. `FetchIssueComments` → `GET /rest/api/3/issue/{issueIdOrKey}/comment`

Query params: `startAt`, `maxResults`, `orderBy`

Response: `{ startAt, maxResults, total, comments: [...] }`

Comment body uses ADF and must be flattened. Each comment has `id`, `author.displayName`,
`body` (ADF), `created`, `updated`.

### Transitions

Sortie uses these endpoints for orchestrator-initiated handoff transitions when
`tracker.handoff_state` is configured (per ADR-0007). After a successful worker run, the
orchestrator calls `TransitionIssue` to move the issue to the configured handoff state
(e.g., "Human Review").

- `GET /rest/api/3/issue/{issueIdOrKey}/transitions`: lists available transitions for an
  issue based on the current user's permissions and workflow rules. The adapter matches the
  configured target state against `transition.to.name` (case-insensitive), not the
  transition label (`transition.name`), to avoid workflow-specific naming fragility.
- `POST /rest/api/3/issue/{issueIdOrKey}/transitions`: executes a transition, moving the
  issue to a new status. Request body: `{ "transition": { "id": "<transition_id>" } }`.
  Returns `204 No Content` on success (empty body).

OAuth scopes required: `write:jira-work` (classic) or `write:issue:jira` (granular). This
is an escalation from the read-only scopes (`read:jira-work`) used by other adapter
operations.

---

## Field Mapping

`domain.Issue` field → Jira REST response path:

| `domain.Issue` field | Jira field                        | Notes                                   |
| -------------------- | --------------------------------- | --------------------------------------- |
| `ID`                 | `id` (string)                     | Numeric ID as string                    |
| `Identifier`         | `key` (string)                    | e.g. `"PROJ-123"`                       |
| `Title`              | `fields.summary`                  |                                         |
| `Description`        | `fields.description`              | v3: ADF flattened to text. v2: raw string (wiki markup). |
| `Priority`           | `fields.priority.id` (string)     | e.g. `"3"` → int 3; use `id` not `name` |
| `State`              | `fields.status.name`              | Preserve original casing                |
| `BranchName`         | —                                 | See dev-status note below               |
| `URL`                | `{endpoint}/browse/{key}`         | Constructed                             |
| `Labels`             | `fields.labels` (string array)    | Lowercase each                          |
| `Assignee`           | `fields.assignee.displayName`     | Nil → empty string                      |
| `IssueType`          | `fields.issuetype.name`           |                                         |
| `Parent`             | `fields.parent.id`, `.parent.key` | Nil when absent                         |
| `Comments`           | Separate endpoint                 | v3: ADF flattened to text. v2: raw string (wiki markup). |
| `BlockedBy`          | `fields.issuelinks[]` (filtered)  | See blocker extraction below            |
| `CreatedAt`          | `fields.created` (ISO-8601)       |                                         |
| `UpdatedAt`          | `fields.updated` (ISO-8601)       |                                         |

### BranchName via dev-status API

Not available through the core REST API v3. However, Jira Cloud exposes development
information via `GET /rest/dev-status/latest/issue/detail?issueId={id}&applicationType=GitHub`
when a source control tool (GitHub, Bitbucket) is connected. This returns branches, commits,
and PRs linked to the issue. Not required for initial implementation but noted as a potential
future source.

### Blocker extraction from `issuelinks`

The "Blocks" link type has `type.inward = "is blocked by"` and `type.outward = "blocks"`.

When reading links from the **blocked** issue, the blocking issue appears in `inwardIssue`.
Filter for links where:

- `type.name == "Blocks"` AND `inwardIssue` is present
- Extract `inwardIssue.key` as the blocker identifier.

Example: if issue A blocks issue B, then on issue B the link looks like:

```json
{
  "type": { "name": "Blocks", "inward": "is blocked by" },
  "inwardIssue": { "key": "A-1" }
}
```

Caveats:

- The link type name "Blocks" may be renamed by Jira admins. The adapter should make
  the expected link type name configurable (or at minimum, a named constant) so it can
  be adjusted without code changes.
- Verify link direction against live Jira responses during adapter implementation;
  the inward/outward semantics depend on which issue the link is read from.

### ADF (Atlassian Document Format) flattening

Jira v3 returns `description` and comment `body` as ADF JSON:

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

The adapter must recursively walk the tree and extract all `text` node values, joining
paragraphs with newlines. Without this, `Description` and comment `Body` would be raw JSON.

**v2 bodies are not ADF:** When `api_version: "2"`, `description` and comment `body` are
JSON strings containing Jira wiki markup, not ADF objects. The adapter reads them verbatim
with a JSON string unquote, not ADF flattening. Rendered HTML (`expand=renderedBody`) is
not requested and not in scope.

---

## Pagination

### v3 search endpoint (`GET /rest/api/3/search/jql`), cursor-based

- First request: omit `nextPageToken`, set `maxResults` (recommend `50`).
- Subsequent requests: pass the `nextPageToken` from the previous response.
- Stop when `nextPageToken` is absent from the response.
- If the response returns fewer results than `maxResults` but `nextPageToken` is missing
  and there are signals of remaining data, raise `tracker_missing_end_cursor` rather than
  silently treating pagination as complete.

`POST /rest/api/3/search/jql` uses offset-based (`startAt`/`total`) pagination but is
deprecated for new integrations. Prefer `GET` with cursor-based pagination.

### v2 search endpoint (`GET /rest/api/2/search`), offset-based

The v2 endpoint uses offset-based pagination. The adapter maintains a `startAt` counter
and advances it by the number of results returned per page.

- First request: `startAt=0`, `maxResults=50`.
- Subsequent requests: `startAt += len(page.issues)`.
- Stop when `len(page.issues) == 0` or `startAt + len(page.issues) >= total`.

This mirrors the comment pagination loop already used by both v2 and v3.

### Comment endpoint, offset-based

- `startAt` (0-indexed), `maxResults` (default 50)
- Response includes `total`. Continue while `startAt + len(comments) < total`.

---

## Rate Limiting

Jira Cloud enforces three independent rate limiting systems:

### 1. Points-based quota (per-hour)

- Each call consumes points: base 1 + object costs (e.g., single issue GET = 2 points).
- Default quota: **65,000 points/hour** (resets at top of UTC hour).
- API-token traffic may be exempt from points-based limits (as of March 2026).

### 2. Burst rate limits (per-second, per-endpoint)

- Token bucket algorithm per endpoint per tenant.
- Defaults: GET 100 req/s, POST 100 req/s, PUT 50 req/s, DELETE 50 req/s.
- `GET /rest/api/3/issue/{id}`: 150 req/s burst bucket.
- `GET /rest/api/3/search/jql`: 100 req/s.

### 3. Per-issue write limits

- 20 writes/2s, 100 writes/30s per issue.
- **Not relevant.** Sortie is read-only from the tracker.

### 429 handling

All limits return `429 Too Many Requests` with:

| Header                  | Value                                               |
| ----------------------- | --------------------------------------------------- |
| `Retry-After`           | Seconds to wait (integer)                           |
| `X-RateLimit-Remaining` | Remaining capacity                                  |
| `X-RateLimit-Reset`     | ISO-8601 reset timestamp                            |
| `RateLimit-Reason`      | `jira-quota-global-based`, `jira-burst-based`, etc. |

**Adapter guidance:** Respect `Retry-After` as minimum delay. Exponential backoff with jitter
(base 2s, max 30s, jitter ±30%). Map 429 → `tracker_api_error` with `Retry-After` preserved.

---

## Error Mapping

HTTP status → error category:

| HTTP Status | Condition                  | Error Category            |
| ----------- | -------------------------- | ------------------------- |
| 200         | Success                    | —                         |
| 400         | Bad JQL, invalid request   | `tracker_payload_error`   |
| 401         | Invalid/expired token      | `tracker_auth_error`      |
| 403         | Insufficient permissions   | `tracker_auth_error`      |
| 404         | Issue/resource not found   | `tracker_api_error`       |
| 429         | Rate limited               | `tracker_api_error`       |
| 5xx         | Server error               | `tracker_transport_error` |
| TCP/DNS     | Network failure            | `tracker_transport_error` |
| —           | JSON decode failure on 200 | `tracker_payload_error`   |
| —           | CAPTCHA (X-Seraph header)  | `tracker_auth_error`      |

---

## Config Notes

- **`tracker.api_key` format:** `email:api_token`, split on first `:`.
- **`tracker.endpoint`:** Site URL without trailing slash or path. Adapter appends `/rest/api/3/...`.
- **`tracker.project`:** Jira project key used in all JQL queries.
- **`tracker.active_states`:** Common defaults: `["Backlog", "Selected for Development", "In Progress"]`.
- **`tracker.terminal_states`:** Common defaults: `["Done", "Cancelled"]`.
- **JQL quoting:** Always quote string values in JQL to handle special characters in state names.
- **Network timeout:** 30000 ms.
