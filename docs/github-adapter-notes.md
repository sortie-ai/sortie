# GitHub API: Adapter research notes

> GitHub REST API and GraphQL API on `github.com`, pinned to REST API version `2026-03-10`
> through the `X-GitHub-Api-Version` header that `newGitHubClient`
> (`internal/scm/github/client.go`) sets on every request. REST facts were observed live with
> `curl` and a personal access token in March 2026 and cross-checked against GitHub's
> documentation through the Context7 mirror `/websites/github_en_rest`. The pull request
> payload was re-observed live on 2026-08-10 against the public pull request
> `sortie-ai/sortie#772`, using API version `2022-11-28` as the comparison control. GraphQL
> facts come from GitHub's GraphQL documentation; the query text is pinned by the adapter's
> request-shape unit tests rather than by a live probe.
>
> Coverage: every `TrackerAdapter` operation, and the `SCMAdapter` operations the auto-merge
> and merge-completion reactions depend on (`GetReviewDecision`, `GetCIStatus`,
> `GetMergeability`, `MergePR`, `DeleteBranch`). The review-comment and label-event operations
> are listed with their routes, but their payload shapes are not characterized here. The
> check-runs route is shared with `GitHubCIProvider`, the package's `domain.CIStatusProvider`
> implementation.
>
> Every claim describes `github.com`. No GitHub Enterprise Server instance was observed: the
> GHES base-URL handling below is a fact about the adapter, not about a server.

Reference for implementing the GitHub `TrackerAdapter` and `SCMAdapter`, both in
`internal/scm/github`.

---

## Pinned API version

The adapter sends `X-GitHub-Api-Version: 2026-03-10` on every REST request. Under this version
the following properties are absent from the payloads the adapter reads:

| Removed property | Payload | Consequence for the adapter |
| ---------------- | ------- | --------------------------- |
| `assignee` (singular) | Issue and pull request | `normalizeIssue` reads `assignees[0].login`; the plural array survives this version |
| `merge_commit_sha` | Pull request, on every endpoint that returns a pull request object | `GetMergeability` sources the merge commit from GraphQL instead (see [GetMergeability](#3-getmergeability-pull-request-preview)) |
| `head.repo.has_downloads`, `base.repo.has_downloads` | Pull request | Not read |
| `use_squash_pr_title_as_default` | Repository | Not read |

The `merge_commit_sha` removal is reproducible:

```bash
for V in 2022-11-28 2026-03-10; do
  curl -sS -H "Authorization: Bearer $TOKEN" -H "X-GitHub-Api-Version: $V" \
    https://api.github.com/repos/sortie-ai/sortie/pulls/772 \
    | jq -c '{merged, has: has("merge_commit_sha")}'
done
# 2022-11-28: {"merged":true,"has":true}
# 2026-03-10: {"merged":true,"has":false}
```

GitHub supports version `2022-11-28` until March 10, 2028. The adapter pins one version in one
place, `newGitHubClient`, and no configuration key overrides it.

---

## Authentication

Every method sends the token in the `Authorization` header. `newGitHubClient` also sets
`Accept: application/vnd.github+json`, the pinned API-version header, and a `User-Agent`.

### Personal access token, fine-grained

Fine-grained PATs grant per-repository, per-permission access, which is the least-privilege
option for Sortie.

- Generate at `https://github.com/settings/personal-access-tokens/new`.
- Header: `Authorization: Bearer <token>`
- Token prefix: `github_pat_` (machine-identifiable).
- Required repository permission for the tracker surface: **Issues: Read and write**, which
  covers issue read, comment read and write, label read and write, and dependencies read.
- Repository scope: restrict the token to the repository Sortie manages.
- Expiration is mandatory and GitHub enforces a maximum lifetime. `classifyHTTPError` maps
  `401` to `tracker_auth_error` with the message `bad credentials`.

The `X-Accepted-GitHub-Permissions` response header reports the permissions each endpoint
requires, which is how an access failure is diagnosed.

### Personal access token, classic

Classic PATs grant broad scope-based access.

- Header: `Authorization: Bearer <token>`
- Token prefix: `ghp_`
- Required scope: `repo`, which covers full control of private repositories, including issues
  and labels.

Coarser than a fine-grained PAT, and the practical choice when fine-grained PATs are
unavailable, as on some GitHub Enterprise Server instances. A classic PAT is also the only
token kind whose scopes the auto-merge preflight can read (see
[Token scopes](#token-scopes)).

### GitHub App installation token

GitHub Apps authenticate per-installation with short-lived tokens.

- Generate with `POST /app/installations/{installation_id}/access_tokens`, using a JWT signed
  by the App's private key.
- Header: `Authorization: Bearer <installation_token>`
- Token prefix: `gho_`
- Expiration: 1 hour, non-renewable; the caller re-creates it.
- Required permission for the tracker surface: **Issues: Read and write**.

An installation token carries no `X-OAuth-Scopes` header, so the auto-merge preflight cannot
verify it.

### `GITHUB_TOKEN` in GitHub Actions

Available automatically inside a GitHub Actions workflow.

- Header: `Authorization: Bearer $GITHUB_TOKEN`
- Rate limit: 1,000 requests/hour, below a PAT's 5,000/hour.
- Scope: the repository the workflow runs in.

`validateConfig` (`internal/scm/github/validate.go`) emits a warning when `tracker.api_key` is
empty, and names `GITHUB_TOKEN` in that warning when the environment variable is set.

---

## Configuration

| Config field       | Value                                              |
| ------------------ | -------------------------------------------------- |
| `tracker.endpoint` | `https://api.github.com` by default; set to the instance URL for GHES, for example `https://github.example.com/api/v3`. Surrounding whitespace and trailing slashes are trimmed. A value that is not an absolute http(s) URL with a host is rejected at construction |
| `tracker.api_key`  | A single PAT string, not `email:token` as Jira uses. Passed straight through as a Bearer token |
| `tracker.project`  | `owner/repo`, for example `sortie-ai/sortie` |
| `tracker.active_states` | Label names for active states, for example `["backlog", "in-progress", "review"]`. Lowercased at construction. Defaults to `issuekit.DefaultActiveLabelStates()` |
| `tracker.terminal_states` | Label names for terminal states, for example `["done", "wontfix"]`. Lowercased at construction. Defaults to `issuekit.DefaultTerminalLabelStates()` |
| `tracker.handoff_state` | Label name for the post-run handoff state, for example `"review"`. Trimmed and lowercased. Per ADR-0007 |
| `tracker.query_filter` | Optional GitHub search-qualifier fragment, for example `label:sortie-managed milestone:"Sprint 1"`. When set, candidate fetches route through `GET /search/issues` |
| `extensions.github.etag_cache_size` | Entry cap for the per-path ETag cache, merged into the adapter config by `mergeExtensions` (`cmd/sortie/resolve.go`). Defaults to 1000; `0` disables conditional requests |

`user_agent` is not an operator key: `cmd/sortie/boot.go` sets it to `sortie/<version>` before
constructing the adapter, and `sortie/dev` is the constructor's fallback when the key is
absent.

`NewGitHubAdapter` splits `tracker.project` with `strings.Cut` on `/` and rejects a value whose
halves are empty or whose second half contains another slash, returning `tracker_payload_error`
with the message `project must be in owner/repo format`. The offline `sortie validate` pipeline
reports the same shape through `registry.DiagOwnerRepoProject`.

`resolveEndpoint` parses `tracker.endpoint` before any client is built. An empty or
whitespace-only value falls back to `https://api.github.com`; anything else must parse as an
absolute http or https URL carrying a host, or the constructor returns a payload error
(`tracker_payload_error` for the tracker, `ci_payload_error` for the CI provider,
`scm_payload_error` for the SCM adapter). All three constructors share the one helper, so an
IPv6 literal written without brackets — `http://fd00::1:3000`, which `url.Parse` rejects for
carrying more than one colon in an unbracketed host — is a configuration fault at startup
rather than a transport error on the first request. The message carries the endpoint only
after `httpkit.RedactURLUserinfo`, because `url.Parse` quotes the whole raw URL in its own
error text and that text would otherwise republish any embedded credential.

The offline `sortie validate` pipeline decides the same fault through `validateEndpoint`,
which calls the same `resolveEndpoint`, so the offline verdict cannot drift from the startup
verdict. It emits one GitHub-specific endpoint diagnostic:

| Check | Severity | Condition |
| --- | --- | --- |
| `tracker.endpoint.invalid` | `error` | a non-empty `tracker.endpoint` is not an absolute http(s) URL with a host |

The diagnostic message never echoes the configured value. Note that the CI provider and the
SCM adapter read `endpoint` from `extensions.github` first and fall back to `tracker.endpoint`
only when the reaction provider matches the tracker kind, so their endpoint is not always the
one the tracker validate hook inspects — which is why the guard lives in each constructor and
not in the validate hook alone.

The constructor runs no credential or repository preflight. A bad token or an inaccessible
repository surfaces on the first read, not at startup.

Network timeout is 30,000 ms, per
[architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics).

---

## Tracker operations

Every `TrackerAdapter` method is implemented in `internal/scm/github/tracker.go`.

| Operation | Route | Go symbol |
| --------- | ----- | --------- |
| `FetchCandidateIssues` | `GET /repos/{owner}/{repo}/issues`, or `GET /search/issues` when `query_filter` is set | `fetchCandidatesViaIssues`, `fetchCandidatesViaSearch` |
| `FetchIssueByID` | `GET /repos/{owner}/{repo}/issues/{number}` plus the blockers, parent, and comments routes | `FetchIssueByID` |
| `FetchIssuesByStates` | `GET /repos/{owner}/{repo}/issues?state=open` and `GET /search/issues` per terminal label | `fetchOpenIssuesByStates`, `fetchClosedIssuesByLabel` |
| `FetchIssueStatesByIDs` | `GET /repos/{owner}/{repo}/issues/{number}`, one per issue | `fetchStatesByNumbers` |
| `FetchIssueStatesByIdentifiers` | Same route and same helper | `fetchStatesByNumbers` |
| `FetchIssueComments` | `GET /repos/{owner}/{repo}/issues/{number}/comments` | `fetchAllComments` |
| `TransitionIssue` | Label delete, label add, and issue patch | `TransitionIssue` |
| `CommentIssue` | `POST /repos/{owner}/{repo}/issues/{number}/comments` | `CommentIssue` |
| `AddLabel` | `POST /repos/{owner}/{repo}/issues/{number}/labels` | `AddLabel` |

### 1. `FetchCandidateIssues`

Query parameters on the issues route:

| Parameter   | Value       | Notes                            |
| ----------- | ----------- | -------------------------------- |
| `state`     | `open`      | Only open issues are candidates  |
| `sort`      | `created`   | Stable ordering                  |
| `direction` | `asc`       | Oldest first                     |
| `per_page`  | `50`        | Per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics) |

The adapter sends no `labels` parameter. It normalizes every entry and then drops the ones
whose derived state is not in `active_states`, so state filtering is client-side.

**Pull request filtering.** The issues route returns both issues and pull requests. A pull
request entry carries a non-null `pull_request` key, which `isPullRequest`
(`internal/scm/github/normalize.go`) uses to drop it before normalization.

**Search variant.** When `tracker.query_filter` is set, the adapter builds the query
`repo:{owner}/{repo} type:issue state:open {query_filter}` and sends it to
`GET /search/issues` with `sort=created`, `order=asc`, and `per_page=50`. The search endpoint
accepts the full GitHub qualifier syntax (label, assignee, milestone, date ranges) and carries
a separate, much smaller rate-limit budget (see [Rate limiting](#rate-limiting)), which is why
an unset `query_filter` keeps candidate fetches on the plain issues route.

### 2. `FetchIssueByID`

GitHub addresses issues by `number` within a repository. `FetchIssueByID` takes that number as
a string, reads the issue, rejects the resource as not found when it is a pull request, and
then fills `BlockedBy`, `Parent`, and `Comments` from their own routes.

### 3. `FetchIssuesByStates`

GitHub's native state is only `open` or `closed`, while Sortie's states are label-based, so the
adapter splits the requested set:

1. Every requested state that is not a configured terminal state routes through
   `GET /repos/{owner}/{repo}/issues?state=open`, with client-side filtering against the
   requested set.
2. Every requested terminal state gets its own search call:
   `GET /search/issues?q=repo:{owner}/{repo} type:issue state:closed label:"{state}"`. The
   operator's `query_filter` is deliberately not appended here, so an operator filter cannot
   hide a terminal issue from this lookup.
3. Results are deduplicated by issue identifier across both halves.

An unknown state label on a closed issue is not found. That is intentional: only configured
terminal states warrant the closed-issue search path.

### 4. `FetchIssueStatesByIDs` and `FetchIssueStatesByIdentifiers`

GitHub offers no bulk state read, and both methods delegate to `fetchStatesByNumbers`, because
the domain ID and the domain identifier are both the issue number. The helper issues one
`GET /repos/{owner}/{repo}/issues/{number}` per number, sequentially, and stops on the first
error other than a 404 (a 404 skips that issue).

Each request goes through `etagCache` (`internal/scm/github/etag_cache.go`): the cached ETag is
sent as `If-None-Match`, and a `304` reuses the state derived on the last full read without
re-deriving it. `304` responses do not count against the primary rate limit, which matters because
reconciliation polls the same issues repeatedly.

### 5. `FetchIssueComments`

Query parameters:

| Parameter  | Value          | Notes                                            |
| ---------- | -------------- | ------------------------------------------------ |
| `per_page` | `50`           | The only parameter the adapter sends             |
| `page`     | 1, 2, 3, ...   | Supplied by the `Link` header, not constructed   |
| `since`    | ISO-8601 timestamp | Supported by GitHub; the adapter does not send it |

The per-issue comments route supports neither `sort` nor `direction`; those exist only on the
repository-wide comments listing (`GET /repos/{owner}/{repo}/issues/comments`). Comments come
back in ascending ID order, oldest first, so the adapter re-sorts nothing.

Each comment carries `id`, `user.login`, `body`, `created_at`, and `updated_at`. The body is
Markdown, not Jira's ADF, and `normalizeComments` passes it through unchanged.

### 6. `TransitionIssue`

GitHub has no workflow transition concept, so the adapter composes a transition from the label
and issue-edit routes:

1. Reject a `targetState` that is not a configured active state, terminal state, or the
   handoff state, with `tracker_payload_error`, before any write.
2. Read the issue to find its current state label and native `open`/`closed` status.
3. When a different state label is present, remove it:
   `DELETE /repos/{owner}/{repo}/issues/{number}/labels/{name}`. A 404 here is tolerated.
4. When the target label is not already present, add it:
   `POST /repos/{owner}/{repo}/issues/{number}/labels` with body
   `{ "labels": ["target-state-label"] }`.
5. When the target is terminal and the issue is open, close it:
   `PATCH /repos/{owner}/{repo}/issues/{number}` with body
   `{ "state": "closed", "state_reason": "completed" }`.
6. When the target is active and the issue is closed, reopen it:
   `PATCH /repos/{owner}/{repo}/issues/{number}` with body `{ "state": "open" }`.

GitHub's `state_reason` accepts `"completed"`, `"not_planned"`, `"duplicate"`, and
`"reopened"`. The adapter always sends `"completed"`.

Every step is idempotent, so a partial failure converges on the next retry.

Fine-grained PAT permission: **Issues: Read and write**, which covers both the issue update and
the label manipulation.

### 7. `CommentIssue` and `AddLabel`

`CommentIssue` posts `{ "body": text }` verbatim to
`POST /repos/{owner}/{repo}/issues/{number}/comments`. GitHub accepts Markdown natively, so no
format conversion happens.

`AddLabel` posts `{ "labels": [label] }` to
`POST /repos/{owner}/{repo}/issues/{number}/labels`. The route is additive, so existing labels
survive.

---

## State mapping

GitHub issues have two native states, `open` and `closed`. Sortie needs richer state semantics
(backlog, in progress, review, done), so the adapter uses labels as state indicators, following
the shared label-state derivation rule in
[architecture Section 11.6](architecture/11-issue-tracker-integration-contract.md#116-implemented-tracker-adapters).

### Convention

Each Sortie-managed state is a GitHub label name. Example configuration:

```yaml
tracker:
  active_states: ["backlog", "in-progress", "review"]
  terminal_states: ["done", "wontfix"]
  handoff_state: "review"
```

State names are normalized to lowercase per
[Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules).
`issuekit.DeriveLabelState`, called from `normalizeIssue`, derives an issue's state:

- It scans the issue's labels against the configured state lists and returns the first match.
- An issue whose native state is `open` and which carries no matching label takes the first
  `active_states` entry, for example `"backlog"`.
- An issue whose native state is `closed` and which carries no matching terminal label takes
  the first `terminal_states` entry, for example `"done"`.

### Label hygiene

The adapter creates no labels. `TransitionIssue` and `AddLabel` only attach names through the
labels-add route, so the repository is expected to carry the state labels before Sortie starts.
Automatic creation would need broader permissions and would write into a namespace the
repository's own conventions own. Whether GitHub rejects a label name that does not exist in
the repository is an [open question](#open-questions); a rejection arrives as HTTP 422, which
`classifyHTTPError` maps to `tracker_payload_error`.

---

## Field mapping

`domain.Issue` field to GitHub REST response path, as implemented by `normalizeIssue` and
`qualifyDisplayID`:

| `domain.Issue` field | GitHub field | Notes |
| -------------------- | ------------ | ----- |
| `ID`                 | `number` (integer) | Converted to string. GitHub's global `id` is never read |
| `Identifier`         | `number` (integer) | The same value as `ID`, for example `"299"` |
| `DisplayID`          | Constructed        | `owner/repo#N`, set by `qualifyDisplayID`, which does not overwrite a value already present |
| `Title`              | `title`            | |
| `Description`        | `body`             | Markdown. Empty string when null or missing. No flattening |
| `Priority`           | Not populated      | GitHub issues carry no priority field. See [Priority](#priority) |
| `State`              | Label-derived      | See [State mapping](#state-mapping) |
| `BranchName`         | Not populated      | The issue payload carries no branch reference |
| `URL`                | `html_url`         | Used directly, no construction |
| `Labels`             | `labels[].name`    | Lowercased by `issuekit.NormalizeLabels` per [Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules) |
| `Assignee`           | `assignees[0].login` | Empty array yields the empty string. Uses `login`, not a display name. The singular `assignee` field is absent under the pinned version (see [Pinned API version](#pinned-api-version)) |
| `IssueType`          | `type.name`        | See [IssueType](#issuetype-via-the-type-field) |
| `Parent`             | `GET .../issues/{n}/parent` | See [Parent](#parent-via-the-sub-issues-route) |
| `Comments`           | `GET .../issues/{n}/comments` | Markdown body, no flattening. Populated only by `FetchIssueByID` and `FetchIssueComments` |
| `BlockedBy`          | `GET .../issues/{n}/dependencies/blocked_by` | See [BlockedBy](#blockedby-via-the-dependencies-route) |
| `CreatedAt`          | `created_at` (ISO-8601) | |
| `UpdatedAt`          | `updated_at` (ISO-8601) | |

### Priority

GitHub issues have no built-in priority field, and `normalizeIssue` leaves
`domain.Issue.Priority` at nil, which the domain reads as "no numeric priority". Priority
labels such as `priority:1` reach the domain through `Labels` like any other label.

### `IssueType` via the `type` field

GitHub's issue types feature, available in organizations, returns a `type` object on an issue:

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

The adapter maps `type.name` to `domain.Issue.IssueType`. When `type` is null, which is the
case for personal repositories and for organizations that have not configured issue types, the
field is the empty string.

### `BlockedBy` via the dependencies route

```
GET /repos/{owner}/{repo}/issues/{issue_number}/dependencies/blocked_by
```

The route returns a JSON array of full issue objects that block the queried issue. A live call
against `sortie-ai/sortie` issue #218 returns issue #299 as a blocker.

`normalizeBlockers` sets both `domain.BlockerRef.ID` and `domain.BlockerRef.Identifier` to the
blocking issue's `number` as a string, and derives `domain.BlockerRef.State` from the blocker's
own labels with the same rule the issue read uses. A 404 on this route normalizes to an empty
blocker list, not an error.

This is simpler than Jira's `issuelinks` parsing: no link-type matching and no directional
filtering.

### `Parent` via the sub-issues route

```
GET /repos/{owner}/{repo}/issues/{issue_number}/parent
```

The route returns the parent issue object, or `404` when the issue has no parent, verified live
against an issue with no parent assignment. `fetchParent` maps a 404 to a nil `Parent` and sets
both `domain.ParentRef.ID` and `domain.ParentRef.Identifier` to the parent's `number` as a
string.

---

## Pagination

### `Link` header, list routes

GitHub paginates list routes with the `Link` response header, and every list read in the
adapter goes through `httpkit.NewLinkPaginator`.

- Page size is `50` on the tracker routes
  ([architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)
  default, GitHub's maximum is `100`) and `100` on the SCM and CI routes.
- The paginator parses the `rel="next"` URL and follows it verbatim rather than reconstructing
  the query, because GitHub may change the URL structure or add parameters.
- Iteration stops when no `rel="next"` link is present. The `rel="last"` link reports the page
  count and is only needed by `ListLabelEvents`, which walks the journal backward from the last
  page through `rel="prev"`.

Example `Link` header:

```
<https://api.github.com/repos/owner/repo/issues?page=2&per_page=50>; rel="next",
<https://api.github.com/repos/owner/repo/issues?page=5&per_page=50>; rel="last"
```

Every walk is bounded, and reaching the bound logs a warning and returns what was collected:

| Reader | Page cap | Constant |
| ------ | -------- | -------- |
| Tracker list routes | 200 (10,000 issues at 50 per page) | `maxPages` |
| Reviews and review comments | 20 | `maxReviewPages` |
| Label events | 20 | `maxLabelEventPages` |
| Combined status and check runs | 10 | `maxCIPages` |

### Search route

- `GET /search/issues` takes `page` and `per_page`, and emits the same `Link` header.
- GitHub caps search results at 1,000 total.
- The response carries `total_count` and `incomplete_results`. The adapter logs a warning when
  `incomplete_results` is true and keeps the partial page.

---

## Rate limiting

GitHub enforces several independent rate-limit systems.

### Primary limit, per hour

| Authentication method     | Limit                    |
| ------------------------- | ------------------------ |
| Fine-grained PAT          | 5,000 req/hour           |
| Classic PAT               | 5,000 req/hour           |
| `GITHUB_TOKEN`            | 1,000 req/hour           |
| GitHub App installation   | 5,000 to 12,500 req/hour |
| Unauthenticated           | 60 req/hour              |

The window rolls over one hour. Every response carries:

| Header                  | Value                                       |
| ----------------------- | ------------------------------------------- |
| `x-ratelimit-limit`     | Maximum requests in the window              |
| `x-ratelimit-remaining` | Remaining requests                          |
| `x-ratelimit-used`      | Requests consumed                           |
| `x-ratelimit-reset`     | Unix epoch timestamp of the window reset    |
| `x-ratelimit-resource`  | Resource category, for example `core` or `search` |

### Search limit, per minute

The search route has its own, stricter budget: 30 requests/minute authenticated, 10
unauthenticated. Verified live: `GET /search/issues` with PAT authentication returns
`x-ratelimit-limit: 30` and `x-ratelimit-resource: search`.

Search and core budgets are separate, which is why `FetchCandidateIssues` stays on the issues
route whenever `query_filter` is unset.

### GraphQL limit

GraphQL calls draw on a budget separate from the REST limit. `GetReviewDecision` costs one
GraphQL request per call, and `GetMergeability` costs one more per merged pull request.

### Secondary limits

Secondary limits are independent of the per-hour quota:

- Concurrency: no more than 100 concurrent requests.
- Points: no more than 900 points/minute for REST calls. A GET against a non-mutating endpoint
  costs 1 point; a mutating request costs more.
- Content creation: no more than 80 content-generating requests/minute and 500/hour, for
  example creating issues or comments.

A secondary limit returns `403` or `429` with a `Retry-After` header. On a `403` the response
body contains `"You have exceeded a secondary rate limit"`.

### Conditional requests

GitHub returns `ETag` and `Last-Modified` on responses, and honors `If-None-Match` and
`If-Modified-Since`. A `304 Not Modified` does not count against the primary rate limit,
verified live: consecutive requests with `If-None-Match` return `304` and `x-ratelimit-used`
does not increment. `fetchStatesByNumbers` is the only reader that uses this.

### Detection and handling

| Status | Condition           | Detection                                                     |
| ------ | ------------------- | ------------------------------------------------------------- |
| `403`  | Primary limit hit   | `x-ratelimit-remaining` is `0`                                |
| `403`  | Secondary limit hit | Body contains `rate limit`; `Retry-After` present             |
| `429`  | Either limit hit    | `Retry-After` present                                         |

`classifyHTTPError` checks the `X-Ratelimit-Remaining` header first, then the lower-cased body
for `rate limit`, and maps both to `tracker_api_error`. The `Retry-After` value is appended to
the error message and is not carried in a structured field. The adapter itself does not sleep
or retry: retry backoff belongs to the orchestrator, per
[architecture Section 11.4](architecture/11-issue-tracker-integration-contract.md#114-error-categories).

---

## Tracker error mapping

`classifyHTTPError` (`internal/scm/github/client.go`) reads at most 512 bytes of the response
body as a diagnostic snippet, never echoing the token, and maps status to
`domain.TrackerErrorKind`:

| HTTP status | Condition                           | Error kind                |
| ----------- | ----------------------------------- | ------------------------- |
| 200 to 204  | Success                             | none                      |
| 304         | Not modified (conditional request)  | none; cached state is reused |
| 400         | Bad request, malformed query        | `tracker_payload_error`   |
| 401         | Bad credentials or expired token    | `tracker_auth_error`      |
| 403         | Insufficient permissions            | `tracker_auth_error`      |
| 403         | Primary or secondary rate limit     | `tracker_api_error`       |
| 404         | Issue or resource not found         | `tracker_not_found`       |
| 405         | Method not allowed                  | `tracker_api_error`       |
| 409         | Conflict                            | `tracker_api_error`       |
| 410         | Resource permanently gone           | `tracker_api_error`       |
| 422         | Validation failed, for example an unusable label | `tracker_payload_error` |
| 429         | Rate limit exceeded                 | `tracker_api_error`       |
| 5xx         | Server error                        | `tracker_transport_error` |
| network     | DNS, TCP, or TLS failure            | `tracker_transport_error` |
| payload     | JSON decode failure on a 2xx        | `tracker_payload_error`   |
| other       | Any unlisted status                 | `tracker_api_error`       |

**403 disambiguation.** A 403 is a rate limit when `X-Ratelimit-Remaining` is `0` or the body
mentions `rate limit`, and a permission failure otherwise. The two map to different kinds, so
the distinction changes how the orchestrator treats the failure.

---

## SCM adapter surface

`GitHubSCMAdapter` (`internal/scm/github/review.go`) implements the nine methods of
`domain.SCMAdapter`. `FetchPendingReviews` is specified in
[architecture §11B.1](architecture/13-pr-review-comment-feedback-contract.md#11b1-scmadapter-interface),
the five auto-merge methods in
[§11C.1](architecture/14-auto-merge-reaction-contract.md#11c1-scmadapter-write-surface), and
the remaining three by the reaction kinds that use them.

| Method | Route | Go symbol |
| ------ | ----- | --------- |
| `FetchPendingReviews` | `GET /repos/{o}/{r}/pulls/{n}/reviews` and `.../pulls/{n}/comments` | `FetchPendingReviews` |
| `FetchBotReviewComments` | The same two routes | `FetchBotReviewComments` |
| `GetReviewDecision` | `POST /graphql` | `GetReviewDecision` |
| `GetCIStatus` | `GET .../pulls/{n}`, `.../commits/{sha}/status`, `.../commits/{sha}/check-runs` | `GetCIStatus` |
| `GetMergeability` | `GET .../pulls/{n}` plus one gated `POST /graphql` | `GetMergeability` |
| `MergePR` | `PUT .../pulls/{n}/merge` | `MergePR` |
| `DeleteBranch` | `DELETE .../git/refs/heads/{branch}` | `DeleteBranch` |
| `ListLabelEvents` | `GET .../issues/{n}/events` | `ListLabelEvents` |
| `RemoveLabel` | `DELETE .../issues/{n}/labels/{name}` | `RemoveLabel` |

All implementations are safe for concurrent use. SCM errors are normalized to `*domain.SCMError`
with a `SCMErrorKind` drawn from the six values in `internal/domain/scm.go`. Network and HTTP
classification is shared with the tracker adapter through `internal/httpkit` and
`classifyHTTPError`; `scmcore.ToSCMError` remaps the result into the SCM error namespace and
performs no conflict promotion of its own. The 405 and 409 promotion to `ErrSCMConflict` is a
separate step that `scmcore.AsMergeConflict` applies only to the error `MergePR` gets back from
the merge write call.

GraphQL requests go to a second `httpkit.Client` whose base URL comes from `graphqlBasePath`.
For `github.com` that resolves to `https://api.github.com/graphql`. For a GHES endpoint ending
in `/api/v3`, the `/v3` suffix is stripped so the request lands on `/api/graphql`.

### 1. `GetReviewDecision` (GraphQL `pullRequest.reviewDecision`)

Returns the platform's authoritative review decision for a pull request. This is a GraphQL POST
rather than a REST call because the four-valued enum is exposed only on the GraphQL
`PullRequest` type.

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

Response shape, subset:

```json
{ "data": { "repository": { "pullRequest": { "reviewDecision": "APPROVED" } } } }
```

`mapReviewDecision` (`internal/scm/github/graphql.go`) maps the value:

| GraphQL value       | Domain value                            |
| ------------------- | --------------------------------------- |
| `APPROVED`          | `ReviewDecisionApproved`                |
| `CHANGES_REQUESTED` | `ReviewDecisionChangesRequested`        |
| `REVIEW_REQUIRED`   | `ReviewDecisionReviewRequired`          |
| `null` (no policy)  | `ReviewDecisionNotRequired`             |
| Unknown enum value  | `ReviewDecisionNotRequired`, logged at warn |

A 200 response carrying a non-empty `errors` array becomes `ErrSCMAPI` with the joined error
messages. A missing `pullRequest` payload becomes `ErrSCMPayload`.

Idempotent and read-only: repeated calls return the current decision and mutate nothing.
Errors route through `scmcore.ToSCMError`, which never promotes a status to `ErrSCMConflict`.

### 2. `GetCIStatus` (combined status plus check runs)

Returns an aggregate CI conclusion string for the pull request head commit. The adapter reads
the pull request object for the head SHA, then walks two commit-scoped page sequences:

```
GET /repos/{owner}/{repo}/pulls/{prNumber}
GET /repos/{owner}/{repo}/commits/{headSHA}/status
GET /repos/{owner}/{repo}/commits/{headSHA}/check-runs
```

That is three requests when neither list paginates, and more when either does: both walks use
`httpkit.NewPagePaginator` with `per_page=100` and a 10-page cap. A pull request payload with
no `head.sha` fails the read with `ErrSCMPayload`.

The combined-status payload is `{ "state": "...", "statuses": [...] }` and the check-runs
payload is `{ "total_count": N, "check_runs": [...] }`. The adapter reads only the arrays; it
does not key on the top-level `state` or on `total_count`.

The two routes use different vocabularies for the same underlying gate, so each has its own
mapping into `domain.CheckRun`:

| Source | Wire value | Normalized conclusion |
| ------ | ---------- | --------------------- |
| Combined status | `success` | success (completed) |
| Combined status | `failure`, `error` | failure (completed) |
| Combined status | anything else, including `pending` and the empty string | pending (in progress) |
| Check run | `success` | success |
| Check run | `failure`, `action_required` | failure |
| Check run | `cancelled` | cancelled |
| Check run | `timed_out` | timed out |
| Check run | `neutral`, `skipped` | neutral, skipped |
| Check run | `stale`, null, anything else | pending |

`action_required` is a failure because it asks for a manual UI action the agent cannot
perform. `stale` is pending because the check run that superseded it carries the authoritative
conclusion.

`scmcore.MergeGate` then reduces the combined set to one string:

| Observation | Returned conclusion |
| ----------- | ------------------- |
| No statuses and no check runs | `""` (no signal) |
| Any run whose conclusion is failure or timed out | `"failing"` |
| A completed run's conclusion is cancelled, and no run is failing | `"pending"` |
| Every run reports completed status and none failed or is cancelled | `"success"` |
| Anything else | `"pending"` |

Idempotent and read-only. The empty string means "no required checks exist on this pull
request", and the auto-merge loop treats it as a satisfied CI precondition when `require_ci` is
not set.

`GitHubCIProvider.FetchCIStatus` (`internal/scm/github/ci.go`) reads the same check-runs route
through the same normalization helpers (`mapCheckRunStatus`, `mapCheckConclusion`), reduces it
with `scmcore.AggregateCIStatus` instead of `scmcore.MergeGate`, and additionally pulls a log
tail from `GET /repos/{owner}/{repo}/actions/jobs/{check_run_id}/logs` for the first failing
GitHub Actions check. GitHub Actions creates check runs 1:1 with workflow jobs, so the check
run id doubles as the job id on that route. Neither reader sends the `filter` parameter, so
GitHub's default applies; see [Open questions](#open-questions).

### 3. `GetMergeability` (pull request preview)

Returns the merge precondition state. It reuses `fetchPullRequest`, the same read
`GetCIStatus` performs:

```
GET /repos/{owner}/{repo}/pulls/{prNumber}
```

Response subset:

```json
{ "draft": false, "mergeable_state": "clean", "merged": false,
  "head": { "sha": "<headSHA>", "ref": "<branch>" },
  "base": { "ref": "<baseBranch>" } }
```

`mapMergeableState` maps the state onto `domain.MergeabilityState`:

| GitHub `mergeable_state` | Domain `MergeabilityState` |
| ------------------------ | -------------------------- |
| `clean`                  | `MergeabilityClean`        |
| `unstable`               | `MergeabilityUnstable`     |
| `blocked`, `behind`, `draft` | `MergeabilityBlocked`  |
| `dirty`                  | `MergeabilityDirty`        |
| anything else, including the empty string | `MergeabilityUnknown` |

The returned `PRMergeStatus` takes `Draft`, `Mergeability`, `HeadSHA`, `BranchName`,
`BaseBranch` (from `base.ref`), and `Merged` from this REST read unchanged. `ReviewDecision`
and `CIConclusion` are left unset: callers obtain those from the dedicated reads above. GitHub
computes `mergeable_state` asynchronously after a push, so callers treat
`MergeabilityUnknown` as a deferral condition per
[§11C.5](architecture/14-auto-merge-reaction-contract.md#11c5-merge-precondition-state-machine).

**The merge commit identifier comes from GraphQL.** The pinned API version returns no
`merge_commit_sha` on any pull request payload (see
[Pinned API version](#pinned-api-version)), so `MergeCommitSHA` is sourced from a second,
gated call. When the REST read reports `merged: true`, `fetchMergeCommitOID` issues one
`POST /graphql` carrying a query pinned by a request-shape unit test:

```graphql
query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      mergeCommit {
        oid
      }
    }
  }
}
```

The query requests only `mergeCommit { oid }`, never `potentialMergeCommit`, so GitHub's
test-merge commit is structurally unreachable through this path. The `merged: true` gate bounds
the extra request: an open, draft, or closed-unmerged pull request costs no GraphQL call and
leaves `MergeCommitSHA` empty. A merged pull request whose `mergeCommit` field is null also
yields the empty string, with no error. A GraphQL failure on this call fails the whole
`GetMergeability` read.

The cost is one GraphQL POST per merged pull request per call, measured at 1 point against the
GraphQL rate limit. A deployment that configures `reactions.merge_completion` without
`auto_merge` therefore needs a token that can read the GitHub GraphQL API, the same requirement
`GetReviewDecision` imposes.

Idempotent and read-only.

### 4. `MergePR` (PUT merge)

Performs the merge:

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

- `merge_method` is the only required field, mapped 1:1 from the `domain.MergeStrategy`
  constant values.
- `sha`, `commit_title`, and `commit_message` are omitted when the caller passes the empty
  string (`omitempty`), and GitHub then applies its strategy-specific defaults for the commit
  title and message.
- When `sha` is present GitHub treats it as a precondition: a head that moved since the caller
  read it returns HTTP 409. `expectedHeadSHA` comes from the latest `PRMergeStatus.HeadSHA` the
  caller observed.

Response on 200:

```json
{ "sha": "<merge commit sha>", "merged": true, "message": "Pull Request successfully merged" }
```

This `sha` is the merge endpoint's own response field and is unaffected by the pull request
payload's `merge_commit_sha` removal. `MergePR` populates `domain.MergeResult` from it.

A success response with `"merged": false`, the platform's documented but rare refusal path,
becomes `*SCMError` with kind `ErrSCMConflict` and message
`merge endpoint returned merged=false`.

**Idempotency.** A second `PUT` against a pull request the first call already merged returns
HTTP 200 with `"merged": true` and the existing merge commit SHA, not an error. GitHub
short-circuits once a pull request is merged: it stops evaluating the `sha` precondition, so a
stale or even invalid `expectedHeadSHA` still yields 200. The adapter returns a successful
`domain.MergeResult` with a nil error, and the reconcile loop
(`internal/orchestrator/auto_merge_reconcile.go`) records the duplicate merge as success
through its normal path.

**The already-merged race.** When a different actor merges the pull request first, GitHub
rejects the call with a status the promotion covers, and the rejection body carries no wording
saying the pull request was already merged. `resolveMergeConflict` therefore re-reads the pull
request after the rejection and, when the re-read confirms `merged: true`, returns the marker
itself through `scmcore.AlreadyMergedConflict`. The `expectedHeadSHA` precondition, in
contrast, produces HTTP 409 only while the pull request is still open and its head has moved.

### 5. `DeleteBranch` (DELETE git ref)

Deletes the source branch after a successful merge:

```
DELETE /repos/{owner}/{repo}/git/refs/heads/{branch}
```

No request body. GitHub returns HTTP 204 on success.

**Idempotency.** HTTP 404, meaning the branch is already gone, maps to `*SCMError` with kind
`ErrSCMNotFound`. The auto-merge reconcile loop treats that as a successful no-op, so a retried
delete after a partial-failure tail does not block the issue's completion path.

GitHub refuses a delete that targets a protected or default branch. The documented refusal
statuses for this route are HTTP 409 and HTTP 422, and GitHub's reference attributes the
default-branch case to both, so the adapter handles either: 409 keeps `ErrSCMAPI` and 422 maps
to `ErrSCMPayload`. The auto-merge loop never targets a default branch, so either status is an
operator-configuration error rather than a recoverable runtime condition.

### HTTP status mapping

Each row reports the HTTP status the adapter receives and the `SCMErrorKind` it returns. The
base classification happens in `classifyHTTPError`, and `scmcore.ToSCMError` remaps the result
into the SCM namespace. The 405 and 409 rows reflect the merge write path only; every other
call keeps the kind `scmcore.ToSCMError` assigns.

| HTTP status | Condition                                       | SCM error kind      |
| ----------- | ----------------------------------------------- | ------------------- |
| 200, 204    | Success                                         | none                |
| 400         | Malformed request                               | `ErrSCMPayload`     |
| 401         | Bad credentials or expired token                | `ErrSCMAuth`        |
| 403         | Insufficient permissions, non-rate-limit body   | `ErrSCMAuth`        |
| 403, 429    | Rate limit, primary or secondary                | `ErrSCMAPI`         |
| 404         | Resource not found (pull request, branch, ref)  | `ErrSCMNotFound`    |
| 405         | Method not allowed                              | `ErrSCMConflict` on `MergePR`, `ErrSCMAPI` elsewhere |
| 409         | Conflict: head-SHA drift or branch-protection refusal on `MergePR`, a ref conflict on `DeleteBranch` | `ErrSCMConflict` on `MergePR`, `ErrSCMAPI` elsewhere |
| 410         | Resource permanently gone                       | `ErrSCMAPI`         |
| 422         | Validation failed: unsupported `merge_method`, default-branch delete, spam check | `ErrSCMPayload` |
| 5xx         | Server error                                    | `ErrSCMTransport`   |
| network     | DNS, TCP, or TLS failure                        | `ErrSCMTransport`   |
| payload     | JSON decode failure on a 2xx response           | `ErrSCMPayload`     |

`scmcore.AsMergeConflict` keys the promotion on `domain.TrackerError.Status`, not on message
text: it compares that field against 405 and 409 and promotes only on a match, so an error
whose message merely mentions "conflict" passes through unchanged. `MergePR` is its only
caller, which is why a 409 from `DeleteBranch` keeps `ErrSCMAPI` like the rest of the REST
surface, and why the GraphQL reads need no exemption from it.

`RemoveLabel` is the one method that swallows its `ErrSCMNotFound`: an already-absent label
returns nil rather than an error.

### Token scopes

The auto-merge reaction needs write scopes the read-only tracker surface does not. The names
below are stable constants in `internal/scm/github/merge.go`.

| Operation      | Fine-grained PAT permission | Classic PAT scope   |
| -------------- | --------------------------- | ------------------- |
| `MergePR`      | `pull_requests:write`       | `repo`              |
| `DeleteBranch` | `contents:write`            | `repo`              |
| Both           | both of the above           | `repo` covers both  |

A classic PAT with the `repo` scope is a superset that satisfies both fine-grained permissions
for this adapter, and `VerifyAutoMergeScopes` short-circuits on it. Fine-grained permission
names are accepted by the startup token-scope preflight, per
[§11C.9](architecture/14-auto-merge-reaction-contract.md#11c9-token-scope-and-preflight).

The preflight reads the `X-OAuth-Scopes` header on `GET /rate_limit`. Classic PATs populate
that header; fine-grained PATs and GitHub App installation tokens do not. When the header is
absent or empty, `VerifyAutoMergeScopes` returns nil scopes and nil missing scopes, which the
caller reads as "unable to verify": it logs a warning and proceeds, and any genuine scope gap
surfaces at runtime as `ErrSCMAuth` on the first `MergePR` call.

### `expectedHeadSHA` and the TOCTOU window

`GetMergeability` and `MergePR` are two separate HTTP calls, and between them the pull request
head can move: a new push, a force-push, a base-branch update, or a rebase by another actor.
Without a precondition the second call would merge whatever the head is at merge time, not the
head the precondition check approved.

`expectedHeadSHA` closes that window. The caller passes the `PRMergeStatus.HeadSHA` it read
from `GetMergeability`, or from any later read, straight to `MergePR`, which places it in the
merge body's `sha` field. When the current head does not match, GitHub returns HTTP 409, the
adapter maps it to `ErrSCMConflict` carrying the verbatim GitHub body, and the reconcile loop
re-enqueues the merge for the next tick. That tick re-reads the merge state, computes a fresh
fingerprint over the new SHA (per
[§11C.7](architecture/14-auto-merge-reaction-contract.md#11c7-fingerprint)), and either retries
with the new head SHA or defers further when other preconditions still fail.

The SHA closes the only race the merge call exposes. Review decision and CI conclusion are read
separately and have no equivalent precondition mechanism, so a change in either is caught by
the next tick's re-read rather than by the merge call.

---

## Open questions

| Question | Probe that would settle it |
| -------- | -------------------------- |
| Does `POST /repos/{o}/{r}/issues/{n}/labels` create a label name that does not exist in the repository, or reject it? The label-hygiene rule above assumes a rejection, and `TransitionIssue` reports one as `tracker_payload_error` | Against a scratch repository, POST a name absent from `GET /repos/{o}/{r}/labels`, record the status, then re-read the label list to see whether the name now exists |
| Does the check-runs route default to `filter=latest`, keeping only the most recent run per check name? Neither `GetCIStatus` nor `GitHubCIProvider.FetchCIStatus` sends `filter`, so the default decides whether a superseded run still contributes to the verdict | On a commit whose checks were re-run, compare `GET .../commits/{sha}/check-runs` with `GET .../commits/{sha}/check-runs?filter=all` and diff the returned run sets |
| Does version `2026-03-10` remove properties from routes other than `/pulls/{n}`? Only the pull request object was diffed against `2022-11-28` | Fetch `/pulls/{n}/reviews`, `/pulls/{n}/comments`, `/issues/{n}/comments`, `/issues/{n}/events`, `/commits/{sha}/status`, and `/commits/{sha}/check-runs` under both versions and diff the key sets |
| Does GitHub Enterprise Server serve the same route surface and the same version header behavior? Nothing in this document was observed against a GHES instance | Repeat the tracker and SCM read probes against a GHES deployment with `tracker.endpoint` set to `https://<host>/api/v3` |

---

## Evidence

| Topic | Source | Verification |
| ----- | ------ | ------------ |
| Authentication methods | GitHub Docs: Authenticating to the REST API; Context7 `/websites/github_en_rest` | `curl` with a PAT |
| Fine-grained permissions | GitHub Docs: Permissions required for fine-grained PATs | Cross-checked against Context7 |
| Issues routes | GitHub Docs: REST API / Issues | `curl` against the live API |
| Comments routes | GitHub Docs: REST API / Issue Comments | Field structure verified live |
| Labels routes | GitHub Docs: REST API / Labels | Verified live |
| Rate limits, primary | GitHub Docs: Rate limits for the REST API | `curl` header inspection |
| Rate limits, search | Live observation | `x-ratelimit-limit: 30`, `x-ratelimit-resource: search` |
| Rate limits, secondary | GitHub Docs: Best practices for using the REST API | Documentation only |
| Conditional requests | GitHub Docs: Best practices | `304` with unchanged `x-ratelimit-used`, verified live |
| Pagination | GitHub Docs: Using pagination in the REST API | `Link` header confirmed live |
| Search qualifiers | GitHub Docs: Searching issues and pull requests | Live search query |
| `dependencies/blocked_by` | Fine-grained PAT permissions page (route discovered there) | Live: issue #218 returns #299 as a blocker |
| Sub-issues `parent` | Fine-grained PAT permissions page (route discovered there) | Live: 404 for an issue with no parent |
| Issue types (`type`) | Live response | `curl`: `type.name` is `"Research"` on issue #299 |
| `state_reason` | Live response | `curl`: null on an open issue |
| Version `2026-03-10` removals | GitHub Docs: breaking changes for the REST API | Live diff of `/pulls/772` under `2022-11-28` and `2026-03-10` on 2026-08-10 |
| Merge route | GitHub Docs: REST API / Pulls / Merge a pull request | Context7 `/websites/github_en_rest`: 200, 403, 404, 405, 409, 422 |
| Branch delete route | GitHub Docs: REST API / Git / Delete a reference | Context7 `/websites/github_en_rest`: 204 on success, 409 or 422 on a refused delete including a default-branch delete |
| Review decision (GraphQL) | GitHub Docs: GraphQL / `PullRequest.reviewDecision` | Adapter unit test pins the query text |
| Merge commit (GraphQL) | GitHub Docs: GraphQL / `PullRequest.mergeCommit` | Adapter unit test pins the query text |
| Token scopes for merge | GitHub Docs: Permissions required for GitHub Apps and fine-grained PATs | Constants in `internal/scm/github/merge.go` |

Claims about Sortie's own code carry a Go symbol or package path instead of an evidence row:
reading this repository is not an observation of GitHub.
