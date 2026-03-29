# GitHub REST API: Adapter research notes

> GitHub REST API, API version `2026-03-10`, researched March 2026.
> Reference for implementing the GitHub `TrackerAdapter`.

---

## Authentication

GitHub supports several authentication methods. All methods send the token in the
`Authorization` header and MUST include the `X-GitHub-Api-Version: 2026-03-10` header for
stable behavior.

### Personal access token, fine-grained (recommended for Sortie)

Fine-grained PATs grant per-repository, per-permission access. This is the recommended
method for Sortie because it follows the principle of least privilege.

- Generate at `https://github.com/settings/personal-access-tokens/new`.
- Header: `Authorization: Bearer <token>`
- Token prefix: `github_pat_` (machine-identifiable).
- Required repository permission: **Issues: Read and write** (covers issue read, comment
  read/write, label read/write, dependencies read).
- Repository scope: restrict the token to the specific repository Sortie will manage.
- Expiration: mandatory. GitHub enforces a maximum lifetime. The adapter SHOULD detect
  `401` responses and return `tracker_auth_error` with a message suggesting token renewal.

The `X-Accepted-GitHub-Permissions` response header reports the permissions required by
each endpoint, useful for diagnosing access errors.

### Personal access token, classic

Classic PATs grant broad scope-based access.

- Header: `Authorization: Bearer <token>`
- Token prefix: `ghp_`
- Required scope: `repo` (covers full control of private repositories, including issues
  and labels).

Less preferred than fine-grained PATs due to coarse scoping. Acceptable for quick setup
or when fine-grained PATs are unavailable (e.g., some GitHub Enterprise Server instances).

### GitHub App installation token

GitHub Apps authenticate per-installation using short-lived tokens.

- Generate by `POST /app/installations/{installation_id}/access_tokens` with a JWT signed
  by the App's private key.
- Header: `Authorization: Bearer <installation_token>`
- Token prefix: `gho_`
- Token expiration: 1 hour (non-renewable; must re-create).
- Required permission: **Issues: Read and write**.

Not used by Sortie initially. Relevant if Sortie is distributed as a GitHub App in the
future.

### `GITHUB_TOKEN` (GitHub Actions)

Automatically available in GitHub Actions workflows.

- Header: `Authorization: Bearer $GITHUB_TOKEN`
- Rate limit: 1,000 requests/hour (lower than PAT's 5,000/hour).
- Scope: limited to the repository where the workflow runs.
- Useful for CI-driven Sortie runs.

### Config mapping

| Config field       | Value                                             |
| ------------------ | ------------------------------------------------- |
| `tracker.endpoint` | `https://api.github.com` (default, omit for GHES) |
| `tracker.api_key`  | PAT value (fine-grained or classic)               |
| `tracker.project`  | `owner/repo`, e.g. `sortie-ai/sortie`            |

The adapter splits `tracker.project` on `/` to extract `owner` and `repo` for URL
construction. If the value does not contain exactly one `/`, the adapter returns a
configuration validation error at startup.

For GitHub Enterprise Server, `tracker.endpoint` is set to the instance URL
(e.g., `https://github.example.com/api/v3`).

---

## Endpoints

Each `TrackerAdapter` operation maps to one or more GitHub REST API endpoints.

### 1. `FetchCandidateIssues` → `GET /repos/{owner}/{repo}/issues`

Lists open issues for the repository. Filters issues to those matching the configured
active states (via label-based state mapping; see State mapping below).

Query parameters:

| Parameter   | Value                      | Notes                               |
| ----------- | -------------------------- | ----------------------------------- |
| `state`     | `open`                     | Only open issues are candidates     |
| `labels`    | comma-separated label list | Filter by active state labels       |
| `sort`      | `created`                  | Stable ordering                     |
| `direction` | `asc`                      | Oldest first                        |
| `per_page`  | `50`                       | Per architecture Section 11.2       |

**Pull request filtering:** The issues endpoint returns both issues and pull requests.
Pull requests have a non-null `pull_request` key. The adapter MUST skip entries where
`pull_request` is present.

**Alternative: `GET /search/issues`** for `query_filter` support. When `tracker.query_filter`
is configured, the adapter uses the search endpoint instead:

```
GET /search/issues?q=repo:{owner}/{repo}+type:issue+state:open+{query_filter}&sort=created&order=asc&per_page=50
```

The search endpoint supports the full GitHub search qualifier syntax (label, assignee,
milestone, date ranges). The search API has a **separate rate limit** of 30 requests/minute
(see Rate limiting below), so the adapter SHOULD prefer the issues endpoint when
`query_filter` is not set.

### 2. `FetchIssueByID` → `GET /repos/{owner}/{repo}/issues/{issue_number}`

Returns a single issue by number. GitHub identifies issues by `number` within a repository
(not a global numeric ID).

The `id` field in the response is a global GitHub-internal integer. The adapter uses
`number` as the domain `Identifier` (as a string, e.g. `"299"`) and `id` as the domain
`ID` (as a string, e.g. `"4162016052"`).

### 3. `FetchIssuesByStates` → `GET /repos/{owner}/{repo}/issues` (repeated)

Fetches issues matching specific states. Since GitHub's native state is only `open` or
`closed`, and Sortie's states are label-based, the adapter:

1. Determines which native GitHub state(s) to query (`open`, `closed`, or both) based
   on whether the requested states overlap with `active_states` or `terminal_states`.
2. For each page of results, filters to issues whose labels match the requested states.
3. Paginates until all results are collected.

For terminal state queries that need closed issues: `GET /repos/{owner}/{repo}/issues?state=closed&labels={terminal_label}&per_page=50`.

### 4. `FetchIssueStatesByIDs` → `GET /repos/{owner}/{repo}/issues/{issue_number}` (batched)

GitHub does not provide a bulk "get issue states by IDs" endpoint. The adapter issues
individual `GET /repos/{owner}/{repo}/issues/{issue_number}` requests for each issue.

To minimize rate limit consumption:

- Use conditional requests (`If-None-Match` with cached ETags) for issues that were
  recently fetched. Verified: 304 responses do NOT count against the primary rate limit.
- Parallelize requests within a bounded concurrency pool (e.g., 10 concurrent requests).
- Return the label-derived state for each issue by scanning labels against the
  `active_states` and `terminal_states` configuration.

For small numbers of issues (<20), the overhead is acceptable. For larger sets, consider
using the search endpoint: `GET /search/issues?q=repo:{owner}/{repo}+type:issue+{numbers}`
but note the 30 req/min search rate limit.

### 5. `FetchIssueStatesByIdentifiers` → same as `FetchIssueStatesByIDs`

GitHub issue identifiers (numbers as strings) map directly to the issues endpoint.
The adapter converts identifiers to issue numbers and follows the same batched-fetch
strategy as `FetchIssueStatesByIDs`.

### 6. `FetchIssueComments` → `GET /repos/{owner}/{repo}/issues/{issue_number}/comments`

Query parameters:

| Parameter  | Value          | Notes                                           |
| ---------- | -------------- | ----------------------------------------------- |
| `per_page` | `50`           | Per architecture Section 11.2                   |
| `page`     | 1, 2, 3, ...   | Offset pagination                               |
| `since`    | ISO-8601 timestamp | Optional. Only comments updated after this time |

The per-issue comments endpoint does not support `sort` or `direction` parameters.
Comments are returned in ascending ID order (oldest first) by default. The `sort` and
`direction` parameters are only available on the repo-wide comments listing
(`GET /repos/{owner}/{repo}/issues/comments`).

Response is a JSON array of comment objects. Each comment has `id`, `user.login`, `body`
(Markdown, **not** ADF like Jira), `created_at`, `updated_at`.

Comment `body` is Markdown and requires no flattening (unlike Jira's ADF). The adapter
passes it through directly as the domain `Comment.Body`.

### 7. `TransitionIssue` → label add/remove + optional state change

GitHub has no workflow transition concept. The adapter implements "state transitions" by
manipulating labels. Per the project's design (issue #218), `TransitionIssue`:

1. Identifies the current state label(s) on the issue (labels matching `active_states`
   or `terminal_states`).
2. Removes the current state label:
   `DELETE /repos/{owner}/{repo}/issues/{issue_number}/labels/{name}`
3. Adds the target state label:
   `POST /repos/{owner}/{repo}/issues/{issue_number}/labels`
   Body: `{ "labels": ["target-state-label"] }`
4. If transitioning to a terminal state, also closes the issue:
   `PATCH /repos/{owner}/{repo}/issues/{issue_number}`
   Body: `{ "state": "closed", "state_reason": "completed" }`
5. If transitioning from a terminal state to an active state, reopens the issue:
   `PATCH /repos/{owner}/{repo}/issues/{issue_number}`
   Body: `{ "state": "open" }`

The `state_reason` field accepts `"completed"`, `"not_planned"`, `"duplicate"`, or
`"reopened"`. Default to `"completed"` for terminal transitions. Use `"not_planned"`
for wontfix-style closures.

Fine-grained PAT permission required: **Issues: Read and write** (covers both issue
update and label manipulation).

---

## State mapping

GitHub issues have only two native states: `open` and `closed`. Sortie requires richer
state semantics (backlog, in-progress, review, done). The adapter bridges this gap using
**labels as state indicators**.

### Convention

Each Sortie-managed state maps to a GitHub label. Example configuration:

```yaml
tracker:
  active_states: ["backlog", "in-progress", "review"]
  terminal_states: ["done", "wontfix"]
  handoff_state: "review"
```

These values correspond to GitHub label names (normalized to lowercase per architecture
Section 11.3). The adapter:

- Derives an issue's state by scanning its labels against the configured state lists.
- Returns the **first matching** label as the state. If multiple state labels are present,
  logs a warning and returns the first match in `active_states` order, then
  `terminal_states` order.
- Issues with no matching state label and native state `open` are treated as having the
  first `active_states` entry (e.g., `"backlog"`).
- Issues with native state `closed` and no matching terminal label are treated as having
  the first `terminal_states` entry (e.g., `"done"`).

### Label hygiene

The adapter does NOT create labels automatically. The repository MUST have the state labels
pre-created before Sortie starts. If a configured state label does not exist in the
repository, `TransitionIssue` fails with `tracker_payload_error`.

Rationale: automatic label creation requires administrator permissions and could conflict
with existing repository conventions.

---

## Field mapping

`domain.Issue` field → GitHub REST response path:

| `domain.Issue` field | GitHub field                      | Notes                                               |
| -------------------- | --------------------------------- | --------------------------------------------------- |
| `ID`                 | `id` (integer)                    | Global numeric ID, convert to string                 |
| `Identifier`         | `number` (integer)                | Repo-scoped, convert to string, e.g. `"299"`        |
| `Title`              | `title`                           |                                                      |
| `Description`        | `body`                            | Markdown. Nil when empty. No flattening needed       |
| `Priority`           | —                                 | GitHub has no native priority. See note below        |
| `State`              | Label-derived                     | See State mapping section                            |
| `BranchName`         | —                                 | Not available in issue response. See note below      |
| `URL`                | `html_url`                        | Directly available, no construction needed           |
| `Labels`             | `labels[].name`                   | Lowercase each per Section 11.3                      |
| `Assignee`           | `assignees[0].login`              | Empty array → empty string. Uses `login`, not `displayName`. The singular `assignee` field was removed in API version `2026-03-10`; use the `assignees` array |
| `IssueType`          | `type.name`                       | GitHub issue types (if configured). See note below   |
| `Parent`             | `GET .../issues/{n}/parent`       | Separate endpoint. 404 → nil                        |
| `Comments`           | Separate endpoint                 | Markdown body, no flattening                         |
| `BlockedBy`          | `GET .../issues/{n}/dependencies/blocked_by` | Separate endpoint. See note below    |
| `CreatedAt`          | `created_at` (ISO-8601)           |                                                      |
| `UpdatedAt`          | `updated_at` (ISO-8601)           |                                                      |

### Priority

GitHub issues have no built-in priority field. Options:

- **Labels:** Use priority labels (e.g., `priority:1`, `priority:critical`). The adapter
  could parse a numeric suffix from labels matching a configured prefix. Not in initial
  implementation.
- **Return nil.** The simplest approach. `domain.Issue.Priority` is `*int` and nil
  indicates non-numeric priority.

Initial implementation: return nil. Priority-from-labels is a future enhancement.

### BranchName

Not available in the issues REST API response. A linked branch could be discovered via
the timeline events endpoint (`GET /repos/{owner}/{repo}/issues/{issue_number}/timeline`)
by looking for `cross-referenced` events from pull requests, or via the Git references
API. Not required for initial implementation.

### IssueType via `type` field

GitHub's issue types feature (available in organizations) returns a `type` object on
issues:

```json
{
  "type": {
    "id": 32278178,
    "name": "Research",
    "description": "ADR, spike, or investigation before implementation",
    "color": "purple"
  }
}
```

The adapter maps `type.name` to `domain.Issue.IssueType`. When `type` is null (individual
user repos or organizations without issue types configured), the adapter returns an empty
string.

### BlockedBy via dependencies API

GitHub provides a first-class dependencies endpoint:

```
GET /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocked_by
```

Returns a JSON array of issue objects that block the queried issue. The adapter extracts
`number` (as string) from each blocking issue to populate `domain.BlockerRef.Identifier`,
and `id` (as string) to populate `domain.BlockerRef.ID`.

Verified: `GET /repos/sortie-ai/sortie/issues/218/dependencies/blocked_by` returns
issue #299 as a blocker (JSON array of full issue objects).

This is significantly simpler than Jira's `issuelinks` parsing. No link-type matching or
directional filtering required.

### Parent via sub-issues API

GitHub provides a parent endpoint:

```
GET /repos/{owner}/{repo}/issues/{issue_number}/parent
```

Returns the parent issue object or `404` if no parent is set. The adapter maps:
- `parent.id` → `domain.ParentRef.ID` (as string)
- `parent.number` → `domain.ParentRef.Identifier` (as string)

Verified: returns `404` for issues without a parent assignment.

---

## Pagination

### Issues endpoint (`GET /repos/{owner}/{repo}/issues`), Link header

GitHub uses `Link` header-based pagination for list endpoints.

- Set `per_page=50` (architecture Section 11.2 default, max `100`).
- Parse the `Link` response header for the URL with `rel="next"`.
- Stop when no `rel="next"` link is present.
- The `rel="last"` link indicates total pages but is not needed for iteration.

Example `Link` header:

```
<https://api.github.com/repos/owner/repo/issues?page=2&per_page=50>; rel="next",
<https://api.github.com/repos/owner/repo/issues?page=5&per_page=50>; rel="last"
```

The adapter MUST use the full URL from the `Link` header for subsequent requests, not
construct URLs manually. GitHub may change URL structure or add query parameters.

### Search endpoint (`GET /search/issues`), page-based

- Uses `page` and `per_page` query parameters.
- Maximum 1,000 results total (GitHub hard limit on search results).
- Response includes `total_count` and `incomplete_results` flag.
- When `incomplete_results` is `true`, results may be missing. Log a warning.
- Parse `Link` header the same way as the issues endpoint.

### Comments endpoint, page-based

Same `Link` header pattern. `per_page=50`, follow `rel="next"`.

---

## Rate limiting

GitHub enforces multiple independent rate limiting systems.

### Primary rate limit (per-hour)

| Authentication method     | Limit            |
| ------------------------- | ---------------- |
| Fine-grained PAT          | 5,000 req/hour   |
| Classic PAT               | 5,000 req/hour   |
| `GITHUB_TOKEN`            | 1,000 req/hour   |
| GitHub App installation   | 5,000–12,500 req/hour |
| Unauthenticated           | 60 req/hour      |

Resets on a rolling 1-hour window. Response headers on every request:

| Header                  | Value                                |
| ----------------------- | ------------------------------------ |
| `x-ratelimit-limit`    | Maximum requests in the window       |
| `x-ratelimit-remaining`| Remaining requests                   |
| `x-ratelimit-used`     | Requests consumed                    |
| `x-ratelimit-reset`    | Unix epoch timestamp of window reset |
| `x-ratelimit-resource` | Resource category (e.g., `core`, `search`) |

### Search rate limit (per-minute)

The search endpoint has a separate, stricter rate limit:

- **Authenticated:** 30 requests/minute
- **Unauthenticated:** 10 requests/minute

Verified: `x-ratelimit-limit: 30` and `x-ratelimit-resource: search` on
`GET /search/issues` with PAT authentication.

The adapter MUST track search rate limits separately from core rate limits. When
`query_filter` is not configured, prefer the issues endpoint to avoid consuming the
scarce search budget.

### Secondary rate limits (concurrency and points)

GitHub enforces secondary limits that are not tied to the per-hour quota:

- **Concurrency:** No more than 100 concurrent requests.
- **Points:** No more than 900 points/minute for REST API calls. GET requests to
  non-mutating endpoints cost 1 point; mutating requests cost more.
- **Content creation:** No more than 80 content-generating requests/minute and 500/hour
  (e.g., creating issues, comments).

Secondary limits return `403 Forbidden` or `429 Too Many Requests` with a `Retry-After`
header. When the status is `403`, the response body contains
`"You have exceeded a secondary rate limit"`.

### Conditional requests (ETag / Last-Modified)

GitHub returns `ETag` and `Last-Modified` headers on responses. The adapter SHOULD cache
these and send `If-None-Match` (or `If-Modified-Since`) on subsequent requests for the
same resource.

- `304 Not Modified` responses do **not** count against the primary rate limit.
- Verified: consecutive requests with `If-None-Match` return `304` and
  `x-ratelimit-used` does not increment.

This is valuable for `FetchIssueStatesByIDs` where the same issues are polled
repeatedly during reconciliation.

### 429 and 403 handling

| Status | Condition           | Detection                                                       |
| ------ | ------------------- | --------------------------------------------------------------- |
| `403`  | Primary limit hit   | `x-ratelimit-remaining` is `0`                                  |
| `429`  | Primary limit hit   | `Retry-After` header present                                    |
| `403`  | Secondary limit hit | Body contains `"secondary rate limit"`; `Retry-After` present   |
| `429`  | Secondary limit hit | `Retry-After` header present; no `x-ratelimit-remaining: 0`     |

**Adapter guidance:** Respect `Retry-After` as minimum delay. Apply exponential backoff
with jitter (base 2s, max 60s, jitter ±30%). Map both to `tracker_api_error` with the
`Retry-After` value preserved for the orchestrator's retry logic.

---

## Error mapping

HTTP status → error category:

| HTTP Status | Condition                        | Error Category            |
| ----------- | -------------------------------- | ------------------------- |
| 200–204     | Success                          | —                         |
| 304         | Not Modified (conditional)       | — (use cached data)       |
| 400         | Bad request (malformed query)    | `tracker_payload_error`   |
| 401         | Bad credentials / expired token  | `tracker_auth_error`      |
| 403         | Insufficient permissions         | `tracker_auth_error`      |
| 403         | Secondary rate limit             | `tracker_api_error`       |
| 404         | Issue/resource not found         | `tracker_api_error`       |
| 410         | Resource permanently gone        | `tracker_api_error`       |
| 422         | Validation failed (e.g., bad label) | `tracker_payload_error` |
| 429         | Primary rate limit exceeded      | `tracker_api_error`       |
| 5xx         | Server error                     | `tracker_transport_error` |
| TCP/DNS     | Network failure                  | `tracker_transport_error` |
| —           | JSON decode failure on 200       | `tracker_payload_error`   |

**403 disambiguation:** The adapter distinguishes secondary rate limits (body contains
`"rate limit"`) from permission errors (body contains `"Resource not accessible"` or
other messages) by inspecting the response body. Rate limit 403s map to
`tracker_api_error`; permission 403s map to `tracker_auth_error`.

---

## Config notes

- **`tracker.api_key` format:** Single PAT token string (not `email:token` like Jira).
  The adapter passes it directly as a Bearer token.
- **`tracker.endpoint`:** Defaults to `https://api.github.com`. Override for GitHub
  Enterprise Server (e.g., `https://github.example.com/api/v3`). No trailing slash.
- **`tracker.project`:** `owner/repo` format. The adapter validates the format (exactly
  one `/`) at startup and rejects invalid values.
- **`tracker.active_states`:** Label names representing active states.
  Example: `["backlog", "in-progress", "review"]`.
- **`tracker.terminal_states`:** Label names representing terminal states.
  Example: `["done", "wontfix"]`.
- **`tracker.handoff_state`:** Label name for the post-run handoff state.
  Example: `"review"`. Per ADR-0007.
- **`tracker.query_filter`:** Optional GitHub search qualifier string appended to
  the search query. Example: `label:sortie-managed milestone:"Sprint 1"`.
  When set, the adapter uses `GET /search/issues` instead of `GET /repos/.../issues`.
- **Network timeout:** 30,000 ms per architecture Section 11.2.
- **API version header:** The adapter MUST send `X-GitHub-Api-Version: 2026-03-10` on
  every request. This pins behavior to the latest supported version and prevents
  breakage from future API evolution.
- **User-Agent header:** GitHub requires (SHOULD) a `User-Agent` header identifying the
  application. Use `Sortie/<version>` or similar.

---

## Key differences from Jira adapter

| Aspect             | Jira                                  | GitHub                                       |
| ------------------ | ------------------------------------- | -------------------------------------------- |
| State model        | Rich workflow states                  | `open`/`closed` only; labels for states      |
| Issue identifier   | Project key (`PROJ-123`)              | Number within repo (`299`)                   |
| Description format | ADF (JSON tree, must flatten)         | Markdown (pass through)                      |
| Priority           | `priority.id` (numeric)              | None natively; labels possible               |
| Blocker detection  | `issuelinks` parsing                  | `dependencies/blocked_by` endpoint           |
| Parent reference   | `fields.parent` in issue response     | Separate `parent` endpoint                   |
| Issue types        | `issuetype.name` (always present)     | `type.name` (org feature, may be null)       |
| Auth format        | `email:api_token` (Basic)             | Bearer token (single string)                 |
| Pagination         | Cursor-based (`nextPageToken`)        | `Link` header with `rel="next"`              |
| Rate limiting      | Points-based (65K/hr)                 | Request-count (5K/hr) + search (30/min)      |
| Transitions        | Workflow transition API               | Label add/remove + state change              |
| Comment format     | ADF (must flatten)                    | Markdown (pass through)                      |
| Search syntax      | JQL                                   | GitHub search qualifiers                     |

---

## Source attribution

| Topic                    | Primary source                                                              | Verification method          |
| ------------------------ | --------------------------------------------------------------------------- | ---------------------------- |
| Authentication methods   | GitHub Docs: Authenticating to the REST API; Context7 `/websites/github_en_rest` | `curl` with PAT confirmed    |
| Fine-grained permissions | GitHub Docs: Permissions required for fine-grained PATs                     | Cross-referenced Context7    |
| Issues endpoints         | GitHub Docs: REST API / Issues                                              | `curl` live API              |
| Comments endpoints       | GitHub Docs: REST API / Issue Comments                                      | Verified field structure     |
| Labels endpoints         | GitHub Docs: REST API / Labels                                              | Verified via live API        |
| Rate limits (primary)    | GitHub Docs: Rate limits for the REST API                                   | `curl` header inspection     |
| Rate limits (search)     | Live API verification                                                       | `x-ratelimit-limit: 30` confirmed |
| Rate limits (secondary)  | GitHub Docs: Best practices for using the REST API                          | Documentation only           |
| Conditional requests     | GitHub Docs: Best practices; live verification                              | `304` + unchanged `x-ratelimit-used` confirmed |
| Pagination               | GitHub Docs: Using pagination in the REST API                               | `Link` header confirmed live |
| Search qualifiers        | GitHub Docs: Searching issues and pull requests                             | Live search query confirmed  |
| Dependencies/blocked_by  | Fine-grained PAT permissions page (endpoint discovered)                     | `curl` live: issue #218 returns #299 as blocker |
| Sub-issues/parent        | Fine-grained PAT permissions page (endpoint discovered)                     | `curl` live: 404 for issue without parent     |
| Issue types (`type`)     | Live API response                                                           | `curl`: `type.name` = `"Research"` on #299    |
| `state_reason` field     | Live API response                                                           | `curl`: `null` on open issue |

## Context7 verification report

Library resolved: `/websites/github_en_rest` (7,164 code snippets, High reputation,
benchmark score 73.57).

Queries executed:

1. **Authentication** — `topic: authentication`, 5,000 tokens. Confirmed: fine-grained
   PATs use Bearer auth, GitHub Apps use JWT → installation token flow, classic PATs use
   `repo` scope. Consistent with official docs.

2. **Search endpoint** — `topic: search`, 5,000 tokens. Confirmed: `GET /search/issues`
   with `q` parameter, `per_page` max 100, response includes `total_count` and
   `incomplete_results`. Search rate limit (30/min) confirmed independently via live
   `curl` (not in Context7 snippets).

Context7 did not cover: dependencies/blocked_by endpoint, sub-issues/parent endpoint,
issue types (`type` field), or secondary rate limit details. These were verified
exclusively through live API calls and the official GitHub documentation pages.
