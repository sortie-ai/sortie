# GitHub REST API: Adapter research notes

> GitHub REST API, API version `2026-03-10`, researched March 2026.
> Reference for implementing the GitHub `TrackerAdapter` and `SCMAdapter`.

---

## Authentication

GitHub supports several authentication methods. All methods send the token in the
`Authorization` header and must include the `X-GitHub-Api-Version: 2026-03-10` header for
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
- Expiration: mandatory. GitHub enforces a maximum lifetime. The adapter should detect
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
| `per_page`  | `50`                       | Per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)       |

**Pull request filtering:** The issues endpoint returns both issues and pull requests.
Pull requests have a non-null `pull_request` key. The adapter must skip entries where
`pull_request` is present.

**Alternative: `GET /search/issues`** for `query_filter` support. When `tracker.query_filter`
is configured, the adapter uses the search endpoint instead:

```
GET /search/issues?q=repo:{owner}/{repo}+type:issue+state:open+{query_filter}&sort=created&order=asc&per_page=50
```

The search endpoint supports the full GitHub search qualifier syntax (label, assignee,
milestone, date ranges). The search API has a **separate rate limit** of 30 requests/minute
(see Rate limiting below), so the adapter should prefer the issues endpoint when
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
| `per_page` | `50`           | Per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)                   |
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
[Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules)). The adapter:

- Derives an issue's state by scanning its labels against the configured state lists.
- Returns the **first matching** label as the state. If multiple state labels are present,
  logs a warning and returns the first match in `active_states` order, then
  `terminal_states` order.
- Issues with no matching state label and native state `open` are treated as having the
  first `active_states` entry (e.g., `"backlog"`).
- Issues with native state `closed` and no matching terminal label are treated as having
  the first `terminal_states` entry (e.g., `"done"`).

### Label hygiene

The adapter does not create labels automatically. The repository must have the state labels
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
| `Description`        | `body`                            | Markdown. Empty string when missing or null. No flattening needed |
| `Priority`           | —                                 | GitHub has no native priority. See note below        |
| `State`              | Label-derived                     | See State mapping section                            |
| `BranchName`         | —                                 | Not available in issue response. See note below      |
| `URL`                | `html_url`                        | Directly available, no construction needed           |
| `Labels`             | `labels[].name`                   | Lowercase each per [Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules)                      |
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

- Set `per_page=50` ([architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics) default, max `100`).
- Parse the `Link` response header for the URL with `rel="next"`.
- Stop when no `rel="next"` link is present.
- The `rel="last"` link indicates total pages but is not needed for iteration.

Example `Link` header:

```
<https://api.github.com/repos/owner/repo/issues?page=2&per_page=50>; rel="next",
<https://api.github.com/repos/owner/repo/issues?page=5&per_page=50>; rel="last"
```

The adapter must use the full URL from the `Link` header for subsequent requests, not
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

The adapter must track search rate limits separately from core rate limits. When
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

GitHub returns `ETag` and `Last-Modified` headers on responses. The adapter should cache
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
- **Network timeout:** 30,000 ms per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics).
- **API version header:** The adapter must send `X-GitHub-Api-Version: 2026-03-10` on
  every request. This pins behavior to the latest supported version and prevents
  breakage from future API evolution. This version also removes `head.repo.has_downloads`
  and `base.repo.has_downloads` from the pull request read and `use_squash_pr_title_as_default`
  from the repository read; the adapter does not read any of these three fields. The prior
  version, `2022-11-28`, remains supported until March 10, 2028.
- **User-Agent header:** The adapter must send a `User-Agent` header identifying the
  application, for example `Sortie/<version>`.

---

## SCM write surface

The `SCMAdapter` interface (per `internal/domain/scm.go`) is implemented by
`internal/scm/github/`. The package exposes the six methods listed in
[architecture §11C.1](architecture/14-auto-merge-reaction-contract.md#11c1-scmadapter-write-surface): one read-only method documented in [§11B.1](architecture/13-pr-review-comment-feedback-contract.md#11b1-scmadapter-interface)
(`FetchPendingReviews`) and five additional methods used by the auto-merge
reconcile loop. The five methods below extend the surface beyond the seven
tracker operations covered earlier in this document.

All implementations are safe for concurrent use. SCM errors are normalized to
`*domain.SCMError` with a `SCMErrorKind` drawn from the six values in
`internal/domain/scm.go`. Network and HTTP classification is shared with the
tracker adapter via `internal/httpkit` and the `classifyHTTPError` helper in
`internal/scm/github/client.go`; the SCM-specific remapping (including the
405/409 promotion to `ErrSCMConflict`) lives in `toSCMError`
(`internal/scm/github/review.go`).

### 1. `GetReviewDecision` (GraphQL `pullRequest.reviewDecision`)

Returns the platform's authoritative review decision for a PR. Issues a
GraphQL POST rather than a REST call because the four-valued enum is exposed
only on the GraphQL `PullRequest` type.

```
POST /graphql
Content-Type: application/json

{
  "query": "query($owner: String!, $repo: String!, $number: Int!) {
    repository(owner: $owner, name: $repo) {
      pullRequest(number: $number) {
        reviewDecision
      }
    }
  }",
  "variables": { "owner": "...", "repo": "...", "number": <prNumber> }
}
```

Response shape (subset):

```json
{ "data": { "repository": { "pullRequest": { "reviewDecision": "APPROVED" } } } }
```

Mapping from GraphQL value to `domain.ReviewDecision`:

| GraphQL value       | Domain value                          |
| ------------------- | ------------------------------------- |
| `APPROVED`          | `ReviewDecisionApproved`              |
| `CHANGES_REQUESTED` | `ReviewDecisionChangesRequested`      |
| `REVIEW_REQUIRED`   | `ReviewDecisionReviewRequired`        |
| `null` (no policy)  | `ReviewDecisionNotRequired`           |
| Unknown enum value  | `ReviewDecisionNotRequired` (logs warn) |

Idempotent and read-only: repeated calls return the current decision and do
not mutate platform state. The GraphQL transport prefix
`POST /graphql:` exempts errors from the 405/409 promotion that applies to
the REST surface (see `toSCMError`).

### 2. `GetCIStatus` (combined status + check runs)

Returns an aggregate CI conclusion string for the PR head commit. The
adapter first reads the PR object to obtain the head SHA, then queries two
additional commit-scoped endpoints in sequence, three calls in total:

```
GET /repos/{owner}/{repo}/pulls/{prNumber}
GET /repos/{owner}/{repo}/commits/{headSHA}/status
GET /repos/{owner}/{repo}/commits/{headSHA}/check-runs
```

The first call returns the head SHA from `head.sha`. The second returns the
combined commit-status payload (`{ "state": "...", "statuses": [...] }`).
The third returns the check-runs payload (`{ "total_count": N, "check_runs":
[...] }`).

The aggregate is computed from both signals:

| Observation                                                 | Returned conclusion |
| ----------------------------------------------------------- | ------------------- |
| No statuses and no check runs                               | `""` (no signal)    |
| Any failing status (`failure`, `error`) or check conclusion (`failure`, `timed_out`, `cancelled`, `action_required`) | `"failing"`         |
| At least one pending status or check (`pending`, `in_progress`, `queued`) and none failing | `"pending"`         |
| All passing or non-failing (`success`, `neutral`, `skipped`) | `"success"`         |

Idempotent and read-only. The empty-string return signals "no required
checks exist on the PR" and the auto-merge loop treats it as a satisfied
CI precondition when `require_ci` is not set.

### 3. `GetMergeability` (pull request preview)

Returns the merge precondition state for a PR. Reuses the same pull request
read used by `GetCIStatus`:

```
GET /repos/{owner}/{repo}/pulls/{prNumber}
```

Response subset:

```json
{ "draft": false, "mergeable_state": "clean", "merged": false,
  "head": { "sha": "<headSHA>", "ref": "<branch>" },
  "base": { "ref": "<baseBranch>" } }
```

The adapter maps `mergeable_state` to `domain.MergeabilityState`:

| GitHub `mergeable_state` | Domain `MergeabilityState` |
| ------------------------ | -------------------------- |
| `clean`                  | `MergeabilityClean`        |
| `unstable`               | `MergeabilityUnstable`     |
| `blocked`, `behind`, `draft` | `MergeabilityBlocked`  |
| `dirty`                  | `MergeabilityDirty`        |
| anything else (including the empty string) | `MergeabilityUnknown` |

The returned `PRMergeStatus` populates `Draft`, `Mergeability`, `HeadSHA`, `BranchName`,
`BaseBranch` (from `base.ref`), and `Merged` (from `merged`). `MergeCommitSHA` is never
populated under the pinned API version.
`ReviewDecision` and `CIConclusion` are left unset: callers obtain those values from the
dedicated reads (see §1 and §2 above). GitHub computes `mergeable_state` asynchronously after
a push, so callers treat `MergeabilityUnknown` as a deferral condition per
[§11C.5](architecture/14-auto-merge-reaction-contract.md#11c5-merge-precondition-state-machine).

API version `2026-03-10` removes `merge_commit_sha` from pull request payloads across every
endpoint that returns a pull request object, the same version that removed `Assignee`. Because
the adapter pins `2026-03-10` on every request, the response never carries `merge_commit_sha`,
and `PRMergeStatus.MergeCommitSHA` is never populated by this read.

Idempotent and read-only.

### 4. `MergePR` (PUT merge)

Performs the merge. Sends:

```
PUT /repos/{owner}/{repo}/pulls/{prNumber}/merge
Content-Type: application/json

{
  "merge_method": "merge" | "squash" | "rebase",
  "sha": "<expectedHeadSHA>",
  "commit_title": "<optional>",
  "commit_message": "<optional>"
}
```

Field rules:

- `merge_method` is the only required field. Mapped 1:1 from
  `domain.MergeStrategy` constant values.
- `sha`, `commit_title`, and `commit_message` are omitted when the caller
  passes the empty string (`omitempty`). GitHub then uses the
  strategy-specific defaults for the commit title and message.
- When `sha` is present GitHub uses it as a precondition: if the PR head
  has moved since the caller read it, GitHub returns HTTP 409. The
  `expectedHeadSHA` parameter is sourced from the latest
  `PRMergeStatus.HeadSHA` value the caller observed.

Response on 200:

```json
{ "sha": "<merge_commit_sha>", "merged": true, "message": "Pull Request successfully merged" }
```

The adapter returns a `domain.MergeResult` populated from these fields.

Idempotency: a second `PUT` against a PR already merged by the first call
returns HTTP 200 with `"merged": true` and the existing merge commit SHA,
not an error. GitHub short-circuits once a PR is merged: the `sha`
precondition is no longer evaluated, so a stale or even invalid
`expectedHeadSHA` still yields 200. The adapter therefore returns a
successful `domain.MergeResult` (carrying the existing merge commit SHA)
with a nil error, and the orchestrator's reconcile loop
(`internal/orchestrator/auto_merge_reconcile.go`) records the duplicate
merge as success through its normal success path.

The generic promotion of HTTP 405 and 409 to `ErrSCMConflict` in the
`SCMAdapter` contract, and the orchestrator's `already merged` substring
check, remain the fallback for a provider that rejects a re-merge. GitHub
does not exercise that path for an already-merged PR; the `expectedHeadSHA`
precondition produces HTTP 409 only while the PR is still open and its head
has moved.

A success response with `"merged": false` (the platform's documented but
rare refusal path) is mapped to `*SCMError` with kind `ErrSCMConflict` and
message `merge endpoint returned merged=false`.

### 5. `DeleteBranch` (DELETE git ref)

Deletes the source branch after a successful merge. Sends:

```
DELETE /repos/{owner}/{repo}/git/refs/heads/{branch}
```

No request body. GitHub returns HTTP 204 on success.

Idempotency: an HTTP 404 (branch already gone) maps to
`*SCMError` with kind `ErrSCMNotFound`. The auto-merge reconcile loop
treats this as a successful no-op so that a retried delete after a
partial-failure tail does not block the issue's completion path.

GitHub refuses a delete that targets a protected or default branch. The
documented refusal statuses for this endpoint are HTTP 409 (conflict) and
HTTP 422 (validation failed); GitHub's reference attributes the
default-branch case to both, so the adapter must handle either. A 409 maps
to `ErrSCMConflict` and a 422 maps to `ErrSCMPayload` (see the status table
below). The auto-merge loop does not target default branches, so either
status is an operator-configuration error rather than a recoverable runtime
condition.

### HTTP status mapping

The mapping below is the SCM view: each row reports the HTTP status the
adapter receives and the `SCMErrorKind` it returns. The base classification
happens in `classifyHTTPError` (`internal/scm/github/client.go`), and
`toSCMError` (`internal/scm/github/review.go`) remaps the result for the
SCM error namespace, including the 405/409 promotion to `ErrSCMConflict`.

| HTTP Status | Condition                                          | SCM error kind        |
| ----------- | -------------------------------------------------- | --------------------- |
| 200, 204    | Success                                            | (no error)            |
| 400         | Malformed request                                  | `ErrSCMPayload`       |
| 401         | Bad credentials or expired token                   | `ErrSCMAuth`          |
| 403         | Insufficient permissions (non-rate-limit body)     | `ErrSCMAuth`          |
| 403, 429    | Rate limit (primary or secondary, with `Retry-After` or `X-RateLimit-Remaining: 0` or `rate limit` body) | `ErrSCMAPI` |
| 404         | Resource not found (PR, branch, ref)               | `ErrSCMNotFound`      |
| 405         | Method not allowed on any non-GraphQL REST SCM call | `ErrSCMConflict`      |
| 409         | Conflict on any non-GraphQL REST SCM call (merge head SHA drift, branch protection refusal, `already merged` body, or a `DeleteBranch` ref conflict) | `ErrSCMConflict` |
| 410         | Resource permanently gone                          | `ErrSCMAPI`           |
| 422         | Validation failed (unsupported `merge_method`, default-branch delete, spam check) | `ErrSCMPayload` |
| 5xx         | Server error                                       | `ErrSCMTransport`     |
| network     | DNS, TCP, TLS failure                              | `ErrSCMTransport`     |
| payload     | JSON decode failure on a 2xx response              | `ErrSCMPayload`       |

The 405/409-to-`ErrSCMConflict` promotion is keyed on the error message,
not on the endpoint path. `classifyHTTPError` formats a 405 as
`"<method> <path>: method not allowed: <detail>"` and a 409 as
`"<method> <path>: conflict: <detail>"`. `toSCMError` promotes any
`ErrSCMAPI` error whose message contains `method not allowed` or
`: conflict:` to `ErrSCMConflict`, so the promotion applies to every
non-GraphQL REST SCM call, not only the merge endpoint. In practice the
merge endpoint is the main source of these statuses (GitHub returns 405
when the PR is in a state that bars any merge, and 409 on head-SHA drift,
branch-protection refusal, or an already-merged PR), but a 409 from
`DeleteBranch` (a ref conflict) is promoted the same way. Collapsing both
statuses into one kind lets the reconcile loop apply a single disposition
policy on the merge path. The promotion is suppressed for messages prefixed
with `POST /graphql:` so a 405 or 409 against the GraphQL endpoint is not
misread as a conflict.

The `already merged` 409 subcase is signaled by the verbatim GitHub body in
`SCMError.Message`. The orchestrator inspects the message case-insensitively
and dispatches the subcase to the merge-success branch; every other
`ErrSCMConflict` is re-enqueued at the poll interval per [§11C.5](architecture/14-auto-merge-reaction-contract.md#11c5-merge-precondition-state-machine).

### Token scopes

The auto-merge reaction requires write scopes that the read-only tracker
surface does not. The names below are stable constants in
`internal/scm/github/merge.go`.

| Operation       | Fine-grained PAT permission | Classic PAT scope |
| --------------- | --------------------------- | ----------------- |
| `MergePR`       | `pull_requests:write`       | `repo`            |
| `DeleteBranch`  | `contents:write`            | `repo`            |
| Both            | both of the above           | `repo` covers both |

A classic PAT with the `repo` scope is a superset that satisfies the
fine-grained `pull_requests:write` and `contents:write` permissions for
this adapter. Fine-grained PAT permission names are accepted by the
startup token-scope preflight (per [§11C.9](architecture/14-auto-merge-reaction-contract.md#11c9-token-scope-and-preflight)).

The preflight reads the `X-OAuth-Scopes` header on `GET /rate_limit`.
Classic PATs populate that header; fine-grained PATs and GitHub App
installation tokens do not. When the verifier returns no scope information
the preflight fails open: it logs a warning and proceeds, and any genuine
scope gap surfaces at runtime as `ErrSCMAuth` on the first `MergePR` call.

### `expectedHeadSHA` and the TOCTOU window

`GetMergeability` and `MergePR` are two separate HTTP calls. Between them
the PR head can move: a new push, a force-push, a base-branch update, or a
rebase performed by another actor. Without a precondition the second call
would merge whatever the head is at the moment of the merge, not the head
the precondition check approved.

The `expectedHeadSHA` parameter forecloses that window. The caller passes
the `PRMergeStatus.HeadSHA` it read from `GetMergeability` (or any later
read) directly to `MergePR`. The adapter places that value in the merge
request body's `sha` field. GitHub treats the field as a precondition: if
the current PR head does not match, the server returns HTTP 409, the
adapter maps it to `ErrSCMConflict` with the verbatim GitHub body, and the
reconcile loop re-enqueues the merge for the next tick. The next tick
re-reads the merge state, computes a fresh fingerprint over the new SHA
(per [§11C.7](architecture/14-auto-merge-reaction-contract.md#11c7-fingerprint)), and either retries with the new head SHA or defers further
when other preconditions still fail.

The window the SHA closes is the only race the merge call exposes: review
decision and CI conclusion are read separately and have no equivalent
precondition mechanism, so changes there are caught by the next-tick
re-read rather than by the merge call itself.

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
| Merge endpoint           | GitHub Docs: REST API / Pulls / Merge a pull request                        | Context7 `/websites/github_en_rest`: 200, 403, 404, 405, 409, 422 |
| Branch delete endpoint   | GitHub Docs: REST API / Git / Delete a reference                            | Context7 `/websites/github_en_rest`: 204 on success, 409 or 422 on a refused delete (including a default-branch delete) |
| Review decision (GraphQL) | GitHub Docs: GraphQL / PullRequest.reviewDecision                          | Adapter unit test pins the query text         |
| Token scopes (merge)     | GitHub Docs: Permissions required for GitHub Apps and fine-grained PATs     | Constants in `internal/scm/github/merge.go`   |

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

3. **Merge endpoint** (added with SCM write surface). Confirmed via Context7:
   `PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge` returns 200 on success
   and 403, 404, 405, 409, 422 on various failure modes. Body fields are
   `commit_title`, `commit_message`, `sha`, and `merge_method`. The `sha` field
   acts as a precondition; mismatch returns 409.

4. **Branch delete endpoint** (added with SCM write surface). Confirmed via
   Context7: `DELETE /repos/{owner}/{repo}/git/refs/{ref}` returns 204 on
   success. A refused delete returns 409 (conflict) or 422 (validation
   failed); GitHub's reference attributes a default-branch delete to both
   statuses.
