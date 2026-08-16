# Gitea REST API: Adapter research notes

> **Product.** Gitea, self-hosted, REST API v1 under `/api/v1`. Gitea serves no GraphQL API, so the REST surface is the whole contract. There is no default host: the instance base URL is part of every configuration, and instance settings rather than an edition tier gate behavior (for example `MAX_RESPONSE_ITEMS`).
>
> **Versions pinned.** The tracker surface is anchored to **Gitea 1.27.0**, observed 2026-07-14. The SCM read surface, the SCM write surface, and the CI status provider are anchored to **Gitea 1.26.4**, observed 2026-08-10, 2026-08-11, and 2026-08-16. The lab instance runs 1.26.4 and self-reports that release in its own OpenAPI description. The tracker route surface, pagination and clamps, list parameters, label filter, error statuses, dependency direction, and merge rejection each carry a 1.26.4 re-observation; a 1.27.0 claim without one rests on the 2026-07-14 observation of that release. Where the two releases answer differently, both answers are stated at the claim.
>
> **Instruments.** Live `curl` probes against the lab instance carrying the token the adapter carries; the instance's own OpenAPI description (`GET /swagger.v1.json`); docs.gitea.com through Context7; Gitea source read at the `v1.26.4` and `v1.27.0` tags; Codeberg's published swagger and `GET /api/v1/version`, read 2026-07-14, for the Forgejo comparison.
>
> **Evidence markers.** `(verified)` marks a claim observed on the 1.27.0 lab on 2026-07-14. "verified live" marks a claim observed on the 1.26.4 lab on the date its sentence names. "swagger" marks the instance OpenAPI description, and an upstream file is cited at the tag it was read at. [Open questions](#open-questions) collects what no probe has settled and names the probe that would settle each.
>
> **Interfaces served.** `domain.TrackerAdapter`, `domain.SCMAdapter`, and `domain.CIStatusProvider`, all implemented by `internal/scm/gitea` and resolved through `internal/registry` under kind `gitea`. The [SCM read surface](#scm-read-surface) covers review decisions, mergeability, CI status, review comments, and label events; the [SCM write surface](#scm-write-surface) covers merge, branch delete, label removal, and the auto-merge preflight; the [CI status provider](#ci-status-provider) reads combined-status CI feedback.

---

## Authentication

Gitea authenticates API requests with personal access tokens. The canonical header format is:

```
Authorization: token <access_token>
```

The word `token` (lowercase) before the key is required "for historical reasons" per the official API usage guide. Alternatives verified on 1.27.0, all returning 200 for the same token and all five re-confirmed live on 1.26.4 (2026-08-11):

| Scheme | Format | Notes |
| --- | --- | --- |
| Token header (canonical) | `Authorization: token <key>` | Use this |
| Bearer header | `Authorization: Bearer <key>` | Documented for OAuth2 access tokens; also accepts personal tokens (verified) |
| Basic, token as password | `Authorization: Basic base64(user:<key>)` | Works (verified) |
| Basic, token as username | `Authorization: Basic base64(<key>:)` | Works (verified) |
| Query parameter | `?access_token=<key>` | Works on 1.27.0 defaults (verified) but leaks the secret into URLs and server logs; the adapter sends the token only in the `Authorization` header (`newGiteaClient`) |

A `Sudo` header (or `sudo` query parameter) lets a site administrator impersonate another user. The adapter does not need it.

### Token creation and scopes

Tokens are created in the UI (Settings, Applications, Generate New Token) or via the API with basic auth:

```
POST /api/v1/users/{username}/tokens
{"name": "sortie", "scopes": ["write:issue", "read:user", "read:repository"]}
```

The response carries the token in `sha1`: a 40-character hex string with **no identifying prefix** (unlike `ghp_` or `lin_api_`), so secret scanners cannot recognize a leaked Gitea token by shape. Scopes follow a `read:{category}` / `write:{category}` taxonomy where write implies read: requesting `read:issue` alongside `write:issue` collapses to `write:issue` in the stored scope set (verified). The special scope `all` grants everything.

Verified minimal scope set for this adapter on 1.27.0:

| Operation | Required scope |
| --- | --- |
| All nine tracker operations (issue reads, comments, label reads and writes, label creation, state transitions) | `write:issue` (verified: a token with only this scope performed every operation) |
| `GET /user` (credential check, automation identity) | `read:user` (403 without it, verified) |
| `GET /repos/{owner}/{repo}` (project preflight) | `read:repository` (403 without it, verified) |

Recommended token: `write:issue, read:user, read:repository`.

### Cheap token validation

`GET /api/v1/user` validates the token and returns the automation identity in one call. The status distinguishes the two failure classes (verified):

- Invalid or missing token: 401 `{"message": "invalid username, password or token"}` (or `"token is required"`).
- Valid token, missing scope: 403 `{"message": "token does not have at least one of required scope(s), required=[read:user], token scope=..."}`.

`runPreflight` runs this check before the first poll, so a bad key fails construction rather than the first fetch. The returned `login` is the identity that `assigned_by` and `mentioned_by` filters reference (see Server-side filtering); the read path does not consume it. A second preflight call in the same function, `GET /repos/{owner}/{repo}`, fails fast on a mistyped `tracker.project` and reports the token's effective `permissions` (`admin`, `push`, `pull`); without it a bad project surfaces as `tracker_not_found` on every subsequent call, because Gitea's 404 body does not distinguish a missing repository from a missing issue (verified). A transient 5xx or transport failure is retried with a bounded backoff before construction fails; a 401, 403, or 404 fails construction immediately.

Sub-path deployments work because Gitea always serves the API at `{ROOT_URL}/api/v1`, so appending is deterministic. Plain-HTTP endpoints send the token in cleartext; operators SHOULD front self-hosted instances with TLS. There is no API version header; behavior is pinned by the instance version, which `GET /api/v1/version` reports (`{"version": "1.27.0"}`). Config field values are listed under [Config mapping](#config-mapping).

---

## Identifiers and project scoping

Gitea issues carry two numbers:

- `number` (the **index**): repo-scoped, human-visible, and the value every per-issue route consumes (`/repos/{owner}/{repo}/issues/{index}`).
- `id`: an instance-global int64 that no issue route accepts as input.

Verified on a two-repository instance: the first issue of the second repository has `number: 1` and global `id: 6`. Pull requests share the issue index sequence within a repository, and the identity split runs deeper: a PR opened after issues 1 through 5 takes `number: 6`, its PR object reports the pull-table `id: 1`, and the same entity fetched through the issues route reports the issue-table `id: 7`. The global `id` is therefore a trap for cross-referencing, and the index is the only identifier worth storing.

The adapter maps `domain.Issue.ID` and `domain.Issue.Identifier` both to the index as a string, exactly as the GitHub adapter does (its `FetchIssueStatesByIdentifiers` documents "ID and Identifier are both the issue number" and delegates to one shared fetch path). `tracker.project` is `owner/repo`; the adapter splits it once at construction and builds all paths from the parts.

---

## State model

Gitea issues natively carry only `open` and `closed` (query enum adds `all`). There is no workflow engine, no transition graph, and no `state_reason`.

The adapter derives workflow state from labels, the same convention the GitHub adapter uses, and couples that state to the native open/closed flag by closing the issue on a terminal transition and reopening it on an active one. One model across the two open/closed trackers keeps operator configuration uniform.

- `tracker.active_states`, `tracker.terminal_states`, and `tracker.handoff_state` name repository labels, normalized to lowercase per [architecture Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules).
- State derivation scans the issue's labels against the configured lists: first match in `active_states` order, then `terminal_states` order; multiple matches log a warning and keep the first.
- Native `open` with no state label maps to the first `active_states` entry.
- Native `closed` with no terminal label maps to the first `terminal_states` entry.
- A terminal-state transition also closes the issue; an active-state transition on a closed issue reopens it.

### Where the mechanics diverge from GitHub

Three verified Gitea behaviors force different label plumbing:

1. **Label removal is by numeric id.** `DELETE /repos/{owner}/{repo}/issues/{index}/labels/{id}` accepts only a label id; a delete by id returns 204 (verified). A name in that position returns 404 with `label does not exist [label_id: 0, ...]` on 1.27.0 and 422 with the same message on 1.26.4 (verified live, 2026-08-11). The adapter resolves names to ids up front, so neither status is reached.
2. **Unknown label names are silently ignored on attach.** `POST .../issues/{index}/labels {"labels": ["no-such-label"]}` returns HTTP 200 and attaches nothing (verified). Gitea never auto-creates labels from this route and never errors. Any flow that trusts the server to reject a bad label name no-ops invisibly, so the adapter resolves every label name itself before attaching and attaches by id (`ensureLabelID`, `AddLabel`, `TransitionIssue`).
3. **Server-side label filtering is hostile to configuration typos.** See Server-side filtering: an unresolvable name in the `labels` query parameter silently disables the filter instead of returning an empty result.

The adapter therefore loads the repository label list once per operation that needs it (`resolveLabelIndex` over `GET /repos/{owner}/{repo}/labels`, paginated to exhaustion) and resolves names case-insensitively: Gitea stores label names with their original casing and matches them case-sensitively, while Sortie's configured state names are lowercase. Paging the catalog to exhaustion is required, because a first-page-only resolver would miss a label defined on a later page and then either fail to resolve a real label or create a duplicate.

### Label hygiene for state labels

A state label must exist in the repository before it can be attached, and the adapter creates it when it does not: `ensureLabelID` resolves the name against the label catalog and, when absent, creates it with `POST /repos/{owner}/{repo}/labels` and attaches the new id. The GitHub adapter instead requires pre-created labels and lets a missing one fail loudly with a 422, which Gitea's attach route cannot do (behavior 2 above): there the operator would see nothing at all, and a state transition that silently does not happen is the worst outcome available. Label creation requires `name` and `color` (422 `[Color]: Required` without it, verified); the adapter sends the fixed color `cccccc`. Creation is covered by the `write:issue` scope (verified), so no extra permissions are needed.

---

## Operations

Route map for the nine `TrackerAdapter` methods (`internal/domain/tracker.go`):

| # | Method | Gitea route(s) |
| --- | --- | --- |
| 1 | `FetchCandidateIssues` | `GET /repos/{owner}/{repo}/issues?state=open&type=issues&limit=50` |
| 2 | `FetchIssueByID` | `GET .../issues/{index}` + `.../issues/{index}/comments` + `.../issues/{index}/dependencies` |
| 3 | `FetchIssuesByStates` | `GET .../issues?state=open&type=issues` and `GET .../issues?state=closed&type=issues` |
| 4 | `FetchIssueStatesByIDs` | `GET .../issues/{index}` per id |
| 5 | `FetchIssueStatesByIdentifiers` | Same as 4 (ID equals Identifier equals index) |
| 6 | `FetchIssueComments` | `GET .../issues/{index}/comments` |
| 7 | `TransitionIssue` | `GET .../issues/{index}` + `GET .../labels` + `DELETE .../issues/{index}/labels/{id}` + `POST .../issues/{index}/labels` + `PATCH .../issues/{index}` |
| 8 | `CommentIssue` | `POST .../issues/{index}/comments` |
| 9 | `AddLabel` | `GET .../labels` + optional `POST .../labels` + `POST .../issues/{index}/labels` |

### 1. `FetchCandidateIssues`

```
GET /repos/{owner}/{repo}/issues?state=open&type=issues&limit=50
```

- `type=issues` excludes pull requests server-side (verified; re-confirmed live on 1.26.4, where it excluded all ten PRs from a 16-entry listing). The adapter keeps the `pull_request`-key guard as a second line of defense, matching the GitHub adapter's `isPullRequest` check, because single-issue fetches still need it (see operation 2).
- State filtering is client-side against the configured `active_states`, exactly like the GitHub adapter's no-filter path (`listAndFilter`). The adapter never pushes the configured state labels into the `labels` query parameter: the filter is AND-semantics across multiple labels (an issue must carry all of them, verified), and an unresolvable name silently drops the whole filter and returns everything (verified). Either property alone disqualifies it for state scoping.
- Gitea offers no `sort` or `direction` parameters on this route; pages arrive newest-first by `created_at`, and issues sharing a timestamp arrive in index order (verified live, 2026-08-11). The GitHub adapter requests oldest-first server-side, so `FetchCandidateIssues` re-sorts the accumulated result by `CreatedAt` ascending after pagination completes, handing the orchestrator identically ordered candidates.
- Pagination via the `Link` header with `httpkit.NewLinkPaginator`, capped at the package's `maxPages` guard of 200 pages. Comments are set to nil on all returned issues, and `DisplayID` is qualified to `owner/repo#N` by `setDisplayID`.
- `tracker.query_filter`, when configured, merges into this same request (see Server-side filtering); there is no separate search endpoint in the flow.

### 2. `FetchIssueByID`

```
GET /repos/{owner}/{repo}/issues/{index}
```

- 404 maps to `tracker_not_found`.
- A PR index resolves on this route and returns the PR in issue shape with `pull_request` set (verified: `GET .../issues/6` returned the PR). The adapter returns `tracker_not_found` for it, like the GitHub adapter.
- Comments come from operation 6's route; blockers from `GET .../issues/{index}/dependencies`, which returns full issue objects for every issue **blocking** the queried one (verified: after `POST .../issues/2/dependencies {"index": 1, "owner": ..., "repo": ...}` created "1 blocks 2", the dependencies list of #2 contains #1, and `.../issues/1/blocks` contains #2; re-established live on 2026-08-11 by creating and deleting one dependency). The create route takes the blocker from the body and the blocked issue from the path, and it requires the full issue-meta body: a body carrying only `{"index": 1}` returns 404 `IsErrRepoNotExist` (verified live, 2026-08-11). Only the integration harness writes dependencies; the adapter reads them. Each blocker entry carries `number`, `state`, and `labels`, which is everything `normalizeBlockers` needs to produce a `domain.BlockerRef` with `ID` and `Identifier` set to the blocker's index and `State` derived from its labels and native state, satisfying the `blocked_by` rule in [architecture Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules).
- Parent is always nil and no parent request is issued: Gitea has no parent or sub-issue concept. GitHub's `.../issues/{index}/dependencies/blocked_by` and `.../issues/{index}/parent` are both absent (404, verified; re-confirmed live on 1.26.4 against a 200 on `.../issues/{index}/dependencies` as the control).

### 3. `FetchIssuesByStates`

Two paginated list queries at most, both with client-side state filtering. The requested set is partitioned by whether each state is terminal, so the two listings are disjoint and need no cross-fetch deduplication:

- Any requested state that is not terminal: `GET .../issues?state=open&type=issues&limit=50`, with the operator `query_filter` merged in.
- Any requested terminal state: `GET .../issues?state=closed&type=issues&limit=50`, without the `query_filter`.

The GitHub adapter serves the terminal half through the search endpoint with a server-side label qualifier. Gitea gets the plain closed listing instead, for three reasons: the search route cannot scope to one repository (see Server-side filtering), a server-side `labels` filter inherits the unresolvable-name foot-gun, and a label filter misses closed issues with no terminal label, which the state model still counts as terminal (first `terminal_states` entry). A full closed-issue scan is more traffic, but this method has no orchestrator caller. Startup terminal cleanup runs through `FetchIssueStatesByIdentifiers` instead.

### 4. `FetchIssueStatesByIDs` and 5. `FetchIssueStatesByIdentifiers`

Gitea has no bulk state-by-ids route. The adapter loops `GET .../issues/{index}` per requested id, skipping 404s (issues not found are omitted from the map, per the interface contract) and skipping PR entries. Both methods share one implementation because ID and Identifier are the same value. Conditional requests are not worth wiring: Gitea sends no `ETag` on API responses (verified), so the GitHub adapter's ETag cache pattern has nothing to key on. Reconciliation requests the running issues plus every issue holding a pending reaction entry per tick, and the instance applies no rate limits, so the per-issue loop is acceptable as-is.

### 6. `FetchIssueComments`

```
GET /repos/{owner}/{repo}/issues/{index}/comments
```

- This route is **unpaginated**: it accepts no `page` or `limit` parameters (per the swagger description) and returns the complete comment list in one response. Verified with 60 comments: all 60 returned in one body, `X-Total-Count: 60`, no `Link` header, and no truncation at the `MAX_RESPONSE_ITEMS` cap that governs paginated routes. A 61-comment issue behaves identically on 1.26.4, including under an explicit `limit=5`, which the route ignores (verified live, 2026-08-11).
- Comments arrive oldest-first (ascending id, verified), so no client-side re-sort is needed, unlike Linear.
- Bodies are Markdown and pass through unflattened, like GitHub and unlike Jira's ADF.
- System events (close, reopen, label changes, dependency changes) do not appear on this route (verified: a closed issue with no human comments returns an empty array); they live on the separate timeline route.
- `since` and `before` (RFC 3339) filter by update time (verified). The route declares no other parameters (swagger, re-read on 1.26.4), and `fetchComments` sends none: it takes the whole list in one request.
- 404 maps to `tracker_not_found`. Returns an empty non-nil slice when the issue has no comments.

### 7. `TransitionIssue`

No workflow transition API exists; the adapter composes the transition:

1. A target that is not a configured active, terminal, or handoff state fails with `tracker_payload_error` before any request, mirroring the GitHub adapter's guard.
2. `GET .../issues/{index}`: current labels and native state. A `pull_request` entry fails with `tracker_not_found`.
3. When the current state label already equals the target, no label work happens at all. Otherwise `resolveLabelIndex` reads the repository's labels into a case-insensitive name-to-id map, and a target that resolves to no label is created (see State model).
4. Remove the current state label by id: `DELETE .../issues/{index}/labels/{label_id}`, tolerating 404 as already-removed. That tolerance is what keeps a label an operator removed by hand from failing the transition; on 1.26.4 the route answers 204 or 422 rather than 404, so the arm does not fire there.
5. Attach the target state label by id: `POST .../issues/{index}/labels` with `{"labels": [<label_id>]}`. Attaching by id, never by name, is what makes a resolution failure loud instead of the silent no-op the name path produces.
6. Reconcile native state: terminal target with native `open` sends `PATCH .../issues/{index}` `{"state": "closed"}`; active target with native `closed` sends `{"state": "open"}`. There is no `state_reason` equivalent; the field does not exist in `EditIssueOption`, and a GitHub-shaped request carrying one is tolerated and ignored (verified).

An invalid `state` value in the PATCH body returns HTTP 412 `unknown state: ...` (verified, re-confirmed live on 1.26.4), a status GitHub does not use; see Error model. Partial failures converge on retry because each step is idempotent, the same argument the GitHub adapter makes.

### 8. `CommentIssue`

```
POST /repos/{owner}/{repo}/issues/{index}/comments
{"body": "<markdown text>"}
```

Returns 201 with the created comment (verified). The text posts verbatim as Markdown with no format conversion, and `CommentIssue` ignores the response body.

### 9. `AddLabel`

Used for CI failure escalation, so a silent no-op violates the contract: the escalation label must actually appear. The flow:

1. Resolve the label name, lowercased, against the repository labels case-insensitively (`resolveLabelIndex`).
2. When absent, create it: `POST /repos/{owner}/{repo}/labels` with `{"name": "<label>", "color": "cccccc"}` (`color` is required; 422 `[Color]: Required` without it, verified). Covered by `write:issue` (verified).
3. Attach by id: `POST .../issues/{index}/labels` `{"labels": [<id>]}`.

The attach endpoint is **additive**: it appends to the issue's label set and never drops existing labels (verified: attaching a second label preserved the first), so no read-modify-write is needed. `PUT .../issues/{index}/labels` replaces the whole set instead (verified); no adapter method uses it. The attach response body is the issue's full label list. Label errors remain non-fatal to the orchestrator.

---

## Server-side filtering and `query_filter`

### Repo issue list parameters

`GET /repos/{owner}/{repo}/issues` accepts (swagger, 1.27.0):

| Parameter | Type | Verified semantics |
| --- | --- | --- |
| `state` | `open`, `closed`, `all` | Defaults to open-only listing |
| `type` | `issues`, `pulls` | Server-side PR exclusion works |
| `labels` | comma-separated names | **AND across labels** (an issue must carry all; verified `labels=bug,in-progress` matches only the issue holding both and `labels=backlog,bug` returns nothing when no issue has both), despite the swagger description claiming "any of". Name matching is case-sensitive, and a name that resolves to no label **silently disables the entire filter** (verified: `labels=BACKLOG` returned every open issue). Both behaviors are re-confirmed live on 1.26.4. Treat as unusable for correctness-critical filtering |
| `q` | string | Free-text match on title and body (verified) |
| `milestones` | comma-separated names or ids | Names with id fallback (swagger) |
| `since`, `before` | RFC 3339 | Update-time window |
| `created_by` | username | Author filter |
| `assigned_by` | username | Assignee filter (verified) |
| `mentioned_by` | username | Mention filter (verified). Self-mentions do not register: an author mentioning themselves is not counted, so the automation identity must differ from the issue author |
| `page`, `limit` | integers | See Pagination |

The `assigned_by`, `mentioned_by`, and `created_by` filters take arbitrary usernames, so scoping candidate polling to issues assigned to or mentioning the automation identity stays on the repo-scoped route.

### Cross-repo search route

`GET /repos/issues/search` is Gitea's counterpart to GitHub's `/search/issues`, and it is not a drop-in: GitHub's path is absent (404, verified; re-confirmed live on 1.26.4 with a 200 on `/repos/issues/search` as the control), `q` is plain text with **no qualifier syntax** (`label:x repo:o/r` does not parse), and the identity filters (`assigned`, `created`, `mentioned`, `review_requested`) are booleans relative to the authenticated user. Scoping is by `owner` or `team`, not by repository, so the adapter has no use for it: every candidate query stays on the repo issue list route.

### `query_filter` mapping

`tracker.query_filter` is adapter-defined (Jira: JQL fragment; Linear: `IssueFilter` JSON object). For Gitea it is a **URL query fragment** merged into the repo issue list request:

```yaml
tracker:
  kind: gitea
  query_filter: "assigned_by=hermes-bot&labels=agent-ready"
```

`parseGiteaQueryFilter` parses the fragment with `url.ParseQuery` and rejects the keys the adapter owns (`state`, `type`, `page`, `limit`) with a configuration error; `mergeQueryParams` merges the rest into the candidate query and the open half of `FetchIssuesByStates`. The offline `sortie validate` hook runs the same parser (`validateQueryFilter`), so its verdict cannot drift from the construction verdict. Operators who put `labels=` in the fragment inherit the server's AND semantics, case sensitivity, and the dropped-filter behavior for unresolvable names, so construction resolves the fragment's label names against the repository label list and warns about each one that does not resolve (`reportUnresolvedLabels`), turning the silent foot-gun into a visible diagnostic. A key outside the recognized set (`knownGiteaFilterKeys`) is passed through with a warning.

---

## Field mapping

`domain.Issue` field to Gitea issue response path (fields per [architecture Section 4.1.1](architecture/04-core-domain-model.md#411-issue)):

| `domain.Issue` field | Gitea field | Notes |
| --- | --- | --- |
| `ID` | `number` | Index as string; the global `id` is never used (see Identifiers) |
| `Identifier` | `number` | Same value as `ID` |
| `DisplayID` | derived | `owner/repo#N`, applied by `setDisplayID` after normalization, so a bare index is never ambiguous across repositories |
| `Title` | `title` | |
| `Description` | `body` | Markdown, empty string when null |
| `Priority` | none | Gitea has no priority field (verified absent from the 1.27.0 issue object); always nil |
| `State` | label-derived | See State model |
| `BranchName` | `ref` | Gitea issues carry an optional branch reference; empty string maps to null. Stored opaque, never parsed |
| `URL` | `html_url` | Directly available |
| `Labels` | `labels[].name` | Lowercased per [Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules) |
| `Assignee` | `assignees[0].login` | Gitea populates both the deprecated singular `assignee` and the `assignees` array (verified); use the array like the GitHub adapter, empty when absent |
| `IssueType` | none | No native issue-type concept; always empty, like Linear |
| `Parent` | none | No parent concept; always nil |
| `Comments` | separate route | Markdown, no flattening |
| `BlockedBy` | `GET .../issues/{index}/dependencies` | Full issue objects; see operation 2 |
| `CreatedAt` | `created_at` | RFC 3339 with zone offset, parses as ISO-8601 |
| `UpdatedAt` | `updated_at` | Same |

Comment mapping: `id`, `user.login`, `body` (Markdown), `created_at`, `updated_at`, all verified present. Imported comments may carry `original_author` when a migration created them; the adapter ignores it. List responses also embed a `repository` object (`{id, name, owner, full_name}`), unneeded because the project is fixed per workflow.

---

## Pagination

- List routes take `page` (1-based) and `limit`. The page-size parameter is `limit`, not `per_page`.
- Responses carry an RFC 8288 `Link` header (`rel="next"`, `rel="last"`) with absolute URLs, in the same format the GitHub adapter already consumes (verified, re-confirmed live on 1.26.4), plus an `X-Total-Count` header that the `Link`-driven pagination flow does not need. `httpkit.NewLinkPaginator` consumes the header unchanged and follows the full `rel="next"` URL, which is correct here because Gitea's next-page URLs preserve the query parameters (verified).
- The server clamps `limit` to the instance's `MAX_RESPONSE_ITEMS` (default 50): a 56-issue repository returns 50 items at `limit=100` (verified, re-confirmed live on 1.26.4), while the `Link` header keeps the requested value and pagination stays consistent. An omitted `limit` falls back to `DEFAULT_PAGING_NUM` (default 30, verified, re-confirmed live on 1.26.4).
- The adapter sends `limit=50`, which equals both the architecture's page-size default (per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)) and the stock cap. Operators can lower the cap in `app.ini` `[api]`, so `paginateIssues` iterates by the `Link` header rather than assume 50 arrived; `GET /api/v1/settings/api` exposes the effective values (`max_response_items: 50`, `default_paging_num: 30` on a stock instance, verified, re-confirmed live on 1.26.4) for diagnostics.
- The per-issue comments route is the exception: unpaginated, complete in one response (see operation 6).
- Cursor integrity (`tracker_missing_end_cursor`) does not apply: there are no cursors, and an absent `Link` header is the normal end-of-results signal.

---

## Rate limiting

Gitea ships **no built-in API rate limiting**. There is no `/rate_limit` endpoint (404, verified; re-confirmed live on 1.26.4 against a `GET /version` 200 control), no `x-ratelimit-*` response headers (verified absent), and the upstream feature request (go-gitea/gitea#9559) remains open. The cost model is inverted from the SaaS trackers: the budget is the self-hosted instance's capacity, and the operator sizes it.

Consequences for the adapter:

- No preemptive throttling and no rate-budget accounting. Poll cadence is the only pressure control, configured by the operator.
- A reverse proxy in front of the instance MAY inject 429 responses. `classifyHTTPError` maps 429 to `tracker_api_error` and logs any `Retry-After` value at WARN, leaving retry backoff to the orchestrator; Gitea itself sends no 429.
- No conditional-request machinery: without `ETag` support there is no 304 path and nothing to cache against, so the GitHub adapter's ETag cache has no counterpart here.

---

## Error model

Every API error carries one uniform JSON body (verified):

```json
{"message": "<diagnostic>", "url": "https://<instance>/api/swagger"}
```

Verified message texts worth matching on: `token is required` and `invalid username, password or token` (401), `token does not have at least one of required scope(s), required=[...], token scope=...` (403, names the missing scopes verbatim), `not found` (404, identical for a missing repository and a missing issue), `[Title]: Required` and `[Color]: Required` (422), `unknown state: <value>` (412), and `repo is archived` (423). The 401, 404, 412, and 422 texts are re-confirmed live on 1.26.4 (2026-08-11). `classifyHTTPError` extracts the envelope's `message` into a bounded snippet on every one of these statuses; the 403 body is what `enrichScopeError` parses for the required scope on the write path.

HTTP status to error category, per [architecture Section 11.4](architecture/11-issue-tracker-integration-contract.md#114-error-handling-contract) and the `domain.TrackerErrorKind` values:

| HTTP status | Condition | Category |
| --- | --- | --- |
| 200, 201, 204 | Success | none |
| 400 | Malformed request | `tracker_payload_error` |
| 401 | Missing or invalid token | `tracker_auth_error` |
| 403 | Valid token, missing scope or permission | `tracker_auth_error` |
| 404 | Missing issue, repository, or label | `tracker_not_found` on issue-scoped reads; the adapter maps by route context because the body cannot distinguish the cases |
| 412 | Precondition failed (verified: unknown `state` value on issue edit; Gitea also uses 412 for other rejected preconditions) | `tracker_payload_error` |
| 422 | Validation failed (missing required field) | `tracker_payload_error` |
| 423 | Repository archived or issue dependencies locked (verified: comment write on an archived repository) | `tracker_api_error`; an operator-configuration condition, not retryable |
| 429 | Rate limited (reverse proxy only; Gitea itself never sends it) | `tracker_api_error`; any `Retry-After` value is logged at WARN |
| 5xx | Server error | `tracker_transport_error` |
| network | DNS, TCP, TLS failure | `tracker_transport_error` |
| decode | JSON decode failure on a 2xx | `tracker_payload_error` |

Two Gitea-specific traps the classification must respect:

- **Silent 200s are the dangerous "errors".** The unknown-label attach and the dropped `labels` filter both return 200 with wrong-shaped success. No status mapping catches them; the adapter's own resolution steps (attach by id, resolve before filter) are the mitigation.
- **404 ambiguity.** A mistyped `tracker.project` produces the same `not found` as a deleted issue. The construction-time repository preflight (see Authentication) pins the blame at startup.

---

## Forgejo and Codeberg

Forgejo is a hard fork of Gitea; Codeberg is the flagship hosted Forgejo instance. Facts below come from Codeberg's live swagger and version endpoint, read 2026-07-14:

- Forgejo serves the same `/api/v1` base path and the same `Authorization: token <key>` header (its docs keep Gitea's "for historical reasons" phrasing), the same `page`/`limit` pagination with `Link` and `x-total-count`, and the same `/settings/api` endpoint.
- `GET /api/v1/version` on Codeberg returns `15.0.0-209-2308e484+gitea-1.22.0`: Forgejo pins its Gitea compatibility marker at **1.22**, the fork point, while this document pins 1.27.0 and 1.26.4. The shared surface narrows with each release on either side.
- Verified route divergence in the exact surface this adapter uses: the label-remove route is `.../labels/{identifier}` on Forgejo, accepting "name or id of the label" (swagger), versus id-only `.../labels/{id}` on Gitea. Forgejo's `IssueLabelsOption` also adds an `updated_at` field Gitea lacks. The adapter's id-based label flow sits inside the portable subset: numeric ids are valid identifiers on both.

Forgejo compatibility is claimed by design and not tested. The adapter targets the portable subset (issue and comment routes, id-based label operations, `Link` pagination), so a Forgejo or Codeberg instance works behind the same `kind: gitea` configuration; no probe has confirmed it (see Open questions). Operators pointing Sortie at codeberg.org must respect Codeberg's terms of service for automation; self-hosted instances are the primary target.

---

## Integration test harness

The env-gated integration suite in `internal/scm/gitea` runs against a throwaway containerized instance the job boots and seeds itself, so no repository secret carries a Gitea credential and no state survives between runs.

- Image: `docker.gitea.com/gitea:1.27.0-rootless`, pinned in `scripts/gitea-integration-provision.sh` and in the nightly matrix entry. The release workflow's `test-integration-gitea` job and the nightly adapter matrix both run the script and then `go test -run 'Integration' ./internal/scm/gitea/...`.
- Provisioning, all in `scripts/gitea-integration-provision.sh`: `gitea admin user create --must-change-password=false ...` (the flag matters, since a fresh user is otherwise forced into a password-change state that blocks token creation with a 401-class error), then `POST /api/v1/users/{user}/tokens` with basic auth for a token scoped `write:issue`, `write:repository`, and `read:user`, then the repository, the labels `backlog`, `in-progress`, `review`, `done`, and `bug`, seed issues covering each state plus a comment thread and a blocker link, a reviewer user and an allowlisted bot user (Gitea forbids a `REQUEST_CHANGES` self-review, so both differ from the PR author), a feature-branch pull request carrying human and bot reviews with new-side and old-side inline comments, a label add-and-remove pair on that PR's timeline, and a probe commit carrying more statuses than one page.
- Gate: `SORTIE_GITEA_TEST=1` (`skipUnlessIntegration`), with `SORTIE_GITEA_ENDPOINT`, `SORTIE_GITEA_TOKEN`, and `SORTIE_GITEA_PROJECT` required by `integrationConfig`. The single gate follows the sibling adapters' convention, and the variable names follow `SORTIE_JIRA_ENDPOINT` for the endpoint and `SORTIE_GITHUB_TOKEN` plus `SORTIE_GITHUB_PROJECT` for the token and project. Without the gate every integration test skips cleanly.
- The script also publishes `SORTIE_GITEA_PR_NUMBER`, `SORTIE_GITEA_BOT_USERNAME`, and `SORTIE_GITEA_MANY_STATUS_SHA`. Each of the three tests that needs one skips with a message naming the fixture when its variable is absent, so a partially seeded lab reports skips rather than failures.

---

## SCM read surface

The tracker adapter package also implements the SCM read methods `GetReviewDecision`, `GetMergeability`, `GetCIStatus`, `FetchPendingReviews`, `FetchBotReviewComments`, and `ListLabelEvents` (`domain.SCMAdapter`, `internal/domain/scm.go`). Gitea exposes no GraphQL API and no aggregate review-decision or check-runs endpoint, so each read is composed from REST routes under `/api/v1`. Every route below returns HTTP 200 against the lab instance, and the review, review-comment, and combined-status shapes recorded here are the populated ones observed on 1.26.4 against a pull request carrying two reviews, inline review comments on both diff sides, and a 51-status commit.

| Method | Gitea route(s) |
| --- | --- |
| `GetReviewDecision` | `GET .../pulls/{index}/reviews` + `GET .../pulls/{index}` |
| `GetMergeability` | `GET .../pulls/{index}` |
| `GetCIStatus` | `GET .../pulls/{index}` + `GET .../commits/{sha}/status` |
| `FetchPendingReviews` | `GET .../pulls/{index}/reviews` + `GET .../pulls/{index}/reviews/{id}/comments` |
| `FetchBotReviewComments` | Same routes as `FetchPendingReviews`, filtered by a bot-username allowlist |
| `ListLabelEvents` | `GET .../issues/{index}/timeline` |

### Pull request reviews

```
GET /repos/{owner}/{repo}/pulls/{index}/reviews
```

- Returns a JSON array of review objects. The `state` field is an enum: `APPROVED`, `PENDING`, `COMMENT`, `REQUEST_CHANGES`, `REQUEST_REVIEW`. Gitea spells the changes-requested state `REQUEST_CHANGES`, not GitHub's `CHANGES_REQUESTED`; a state filter copied verbatim from the GitHub adapter matches nothing.
- Each review carries `dismissed` (an operator nullified the review), `official`, `stale`, `body`, `submitted_at`, `user.login`, and `id`. Dismissed reviews are skipped by every read.
- `FetchPendingReviews` returns the comments of non-bot `REQUEST_CHANGES` reviews; `FetchBotReviewComments` returns the comments of reviews whose author matches a bot-username allowlist, with no review-state filter. Gitea users carry no `type: Bot` marker, so bot classification is the allowlist alone (see [Bot classification](#bot-classification)).
- Both reads share `collectReviewComments`, which contributes a retained review's own trimmed `body` as a PR-level `domain.ReviewComment` with id `review-<id>` before its inline comments, admits each inline comment only when that comment's author passes the same predicate, and deduplicates by id across reviews.
- Paginated by page number, not the `Link` header (see [SCM read pagination](#scm-read-pagination)).
- Verified live against a provisioned PR carrying a `REQUEST_CHANGES` review and a `COMMENT` review: the populated object returned `id`, `state`, `body`, `user.login`, `dismissed`, `official`, `stale`, and `submitted_at`, which is every field these reads consume, plus `comments_count`, `commit_id`, `team`, `updated_at`, and the two HTML URLs.

### Review comments

```
GET /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments
```

- Each comment carries `path`, `body`, `position`, `original_position`, `created_at`, `id`, and `user.login`, alongside `commit_id`, `original_commit_id`, `diff_hunk`, `resolver`, `updated_at`, and the two HTML URLs (verified live). There is no `line`, `start_line`, or `end_line` field (verified live), so review comments are single-line and `EndLine` normalizes to 0.
- `normalizeReviewComment` maps the anchor to one `StartLine`: `position` when it is positive, else `original_position` when that is positive, else 0. The new side wins when both are positive, which keeps the mapping total over every integer pair the route can return.
- `position` and `original_position` are **file line numbers, not diff offsets**, and they select the side of the diff rather than the freshness of the anchor. `position` is the new-side line, `original_position` the old-side line; each is 0 when the comment is not anchored to that side. Verified live by anchoring two comments on the same modified line of a PR whose hunk header is `@@ -9,7 +9,7 @@`: the comment created with `new_position: 12` returned `position: 12, original_position: 0`, and the one created with `old_position: 12` returned `position: 0, original_position: 12`. A diff-offset reading would have returned 5 for both. The upstream model names the fields `LineNum` and `OldLineNum` (`modules/structs/pull_review.go`, identical at the `v1.26.4` and `v1.27.0` tags), and the write side documents `new_position` as "if comment to new file line or 0".
- **A zero `position` therefore does not mean outdated.** It means the comment is anchored to the old side of the diff, which is true the moment the comment is created. Verified live: after pushing a commit that deleted the anchored line outright, both comments came back with their `position` and `original_position` unchanged and their `commit_id` still naming the same value as before the push. The route exposes no invalidation field at all: `PullReviewComment` carries no `invalidated` key in the instance OpenAPI or in the upstream model at either tag.
- **`commit_id` is not a single staleness signal**, and comparing each comment's `commit_id` against the PR head is not the route's available signal that it looks like: the value means one thing on each side, and neither half moves on a later push. On the new side it is the blame commit of the anchored line, sourced from `lineBlame(ctx, pr.BaseRepo, gitRepo, head, treePath, line)` in `createCodeComment` (`services/pull/review.go`, identical at the `v1.26.4` and `v1.27.0` tags), and it can name a commit that was never the pull request head: a comment anchored to a line the head commit did not itself modify reports whichever earlier commit last touched that line, not the head. On the old side it falls through to `headCommitID`, the head ref commit at creation. Verified live: two new-side comments created at one head, anchored to lines that head commit did not modify, both reported the base-branch commit that created the file rather than that head; an old-side comment created at the same head reported the head itself. A further push that touched none of the three anchors changed none of their `commit_id` values.
- The adapter never reports a Gitea review comment outdated. The route exposes no invalidation field, and `commit_id` is not a substitute: on the new side it is a line blame rather than a generation marker, so it disagrees with the head the moment a comment is created on a line the head commit did not touch, and on both sides it is fixed at write time, so it disagrees with the head again after any later push regardless of whether the anchor survived. Deriving `Outdated` from it would drop live feedback on arrival and drop the rest of a review's comments the moment any push lands, which is a stall rather than a fix.
- A comment anchored to neither side of the diff reads back `position: 0, original_position: 0` with an empty `diff_hunk`; a review comment created with `new_position` and `old_position` both `0` is accepted rather than rejected.
- A comment anchored outside the diff entirely is also accepted, on either side, and reads back at the requested line: two new-side comments and one old-side comment created against lines the pull request's single hunk did not cover all returned HTTP 201 and read back the requested line in the requested field. Their `diff_hunk` is not empty in this case; it holds synthesized surrounding context that upstream generates through `GeneratePatchForUnchangedLine` when the real diff cut around the line is empty.
- The review object's `stale` flag is a generation marker, not an anchor signal: it flips `true` on any push to the pull request regardless of what the push touched, observed on two reviews with disjoint anchor sets.
- `updated_at` moves for an invalidating push and for an ordinary body edit, but not for a push that leaves every one of a comment's anchors intact, so it is a superset of invalidation rather than a synonym for it.
- Upstream's internal comment model carries an `Invalidated bool` field (`models/issues/comment.go`, identical at the `v1.26.4` and `v1.27.0` tags) that its API converter (`ToPullReviewComment`, `services/convert/pull_review.go`) does not copy onto the wire shape at either tag, so no route this adapter reads exposes it.

### Review decision

Gitea has no aggregate review-decision field; the GitHub adapter reads one from GraphQL, which Gitea does not offer. `GetReviewDecision` folds the review list together with the PR object's `requested_reviewers` signal in `foldReviewDecision`:

- Reviews are ordered by `submitted_at` then `id`, so the latest `APPROVED` or `REQUEST_CHANGES` per reviewer supersedes that reviewer's earlier reviews. `COMMENT`, `PENDING`, and `REQUEST_REVIEW` are not decisions, and dismissed reviews do not contribute.
- The ordering depends on `submitted_at` parsing as RFC 3339, so a value that does not parse fails the decision read with `domain.ErrSCMPayload` instead of sorting the review to the epoch, and only reviews that can change the verdict are parsed.
- Any standing `REQUEST_CHANGES` yields changes-requested; otherwise any `APPROVED` yields approved; otherwise a non-empty `requested_reviewers` yields review-required; otherwise not-required.

### Mergeability

```
GET /repos/{owner}/{repo}/pulls/{index}
```

- The PR object carries `mergeable` (a plain bool, verified `true` on the lab PR, re-confirmed live on 1.26.4), `merged`, `draft`, `head.sha`, `head.ref`, `base.ref`, and a `requested_reviewers` array. There is no `mergeable_state` string and no tri-state "computing" field (verified absent, re-confirmed live on 1.26.4).
- The boolean is the only mergeability signal, so `mapMergeability` is lossy: a draft maps to `blocked`, a mergeable non-draft to `clean`, and any other state to `unknown`. Gitea never yields `dirty` or `unstable`; a merge conflict and an in-progress recheck both collapse to `unknown`, which the auto-merge state machine re-enqueues rather than treating as a hard conflict.
- The same read supplies `head.sha` (the CI ref for `GetCIStatus`), `head.ref` (the branch), and `base.ref` (the base branch the merge-conflict reaction needs).
- The PR object also carries `merge_commit_sha`, serialized from the `MergedCommitID *string` field in `modules/structs/pull.go` at the Gitea `v1.27.0` tag. This is upstream-source provenance, not a live-verification claim: no merged pull request was read live, so the field's presence and JSON key name come from the upstream model rather than the wire, and the `pr_merged.json` fixture carries the same marker. `GetMergeability` gates the value on `merged`, so a null or stale commit id on an open pull request is never reported as a merge.

### Combined commit status

```
GET /repos/{owner}/{repo}/commits/{sha}/status
```

- Returns `{state, sha, statuses, total_count}`. The top-level `state` is the aggregate; each entry in the `statuses` array carries its own `status`. The two field names differ and MUST NOT be conflated.
- A commit with no CI returns `total_count: 0`, `statuses: null`, and a spurious top-level `state: "pending"` (verified, re-verified live on 1.26.4). Trusting the top-level `state` would report `pending` for a no-CI commit and wrongly hold auto-merge, so the aggregate is read from the per-status entries instead.
- **`total_count` is the current page's length, not the grand total** (verified live: a 51-status commit returned `total_count: 30` on the default page, `50` at `limit=50`, `1` on page two, and `20` at `limit=20`). It can therefore detect neither emptiness nor truncation, so both readers key the empty case on the accumulated status count across every page. The grand total is available, but only in the `X-Total-Count` header, which reported 51 on every one of those requests.
- `GetCIStatus` reads the PR for `head.sha`, walks that commit's statuses, and answers through `scmcore.MergeGate` over the same normalized check-run set the CI provider computes, so one forge cannot answer "is CI green" two ways. A PR response without a head SHA fails with `domain.ErrSCMPayload`, and a commit with no statuses yields the empty string, the "no required checks" signal.
- Per-status `status` values declared by the schema are `success`, `failure`, `error`, `warning`, and `pending`. Only `success` and `failure` are observed live; the other three are declared but unobserved, because the lab runs no CI that produces them. `failure` and `error` are failing; `warning` and `success` are non-failing; `pending` is pending. `mapRunStatus` and `mapConclusion` lowercase the value first and fold anything unrecognized, including the empty string, into pending rather than into a passing verdict. Each entry also carries `context` (the check name), `target_url`, and `description`.
- The per-status `target_url` field name is verified live: every seeded status returned the URL it was created with under that key, and the env-gated integration suite asserts the round-trip from a seeded failing status into the failing check run's `DetailsURL`.
- **The route paginates.** Verified live: the default page size is 30, `limit` is honored up to the instance clamp (`limit=100` returns 50), and the response carries an RFC 8288 `Link` header with `rel="next"` and `rel="last"` (unlike the reviews, review-comments, and timeline routes, which emit none). The instance OpenAPI declares `page` and `limit` on the route, corroborating the observation. A single-request read would take only the first page, so a failing status ordered onto a later page would read as passing to both the CI-failure reaction and the auto-merge CI gate; both readers instead walk every page through `httpkit.NewPagePaginator` up to the package's shared `scmMaxPages` ceiling of 50 pages, logging a WARN when they reach it.

### Label event timeline

```
GET /repos/{owner}/{repo}/issues/{index}/timeline
```

- Pull requests share the issue timeline route (a PR at index 6 is served at `/issues/6/timeline`). The route returns a JSON array of typed entries; verified types include `label`, `comment`, `add_dependency`, and `pull_push`.
- A `label` entry carries `type`, a numeric `id`, `body`, `label.name`, `user.login`, and `created_at`. `body` is `"1"` for a label add and `""` for a label remove (verified live on lab issue #4, which round-trips both). The label name keeps Gitea's original casing (for example `Bug`, `REVIEW`) and is lowercased on normalization.
- Entries arrive oldest-first (verified), so the accumulated slice needs no re-sort. `scmcore.SortableEventID` zero-pads the numeric timeline id to 19 digits so `(timestamp, id)` string ordering matches journal order for entries sharing a timestamp.
- `ListLabelEvents` keeps only `label` entries carrying a label name; a `created_at` that does not parse as RFC 3339 fails the read with `domain.ErrSCMPayload`, and an entry skipped for its kind is never parsed.

### Bot classification

Gitea users carry no `type: Bot` marker (verified: the review `user` object has no bot-type field), so the platform half of the bot-author predicate is always false. `FetchBotReviewComments` classifies solely by a case-insensitive match against the configured bot-username allowlist; a nil or empty allowlist selects nothing. `FetchPendingReviews` passes no allowlist, so its non-bot filter cannot exclude a bot-authored `REQUEST_CHANGES` review, unlike the GitHub adapter's platform-marker exclusion.

### SCM read pagination

The reviews, review-comments, and timeline routes paginate by page number, not by the `Link` header the tracker issue routes use. The timeline route emits no `Link` header and an unreliable `X-Total-Count`: it reported 2, 5, and 30 against `limit=2`, `limit=5`, and the default page size on an issue whose timeline exceeds one page, so the header carries the page length rather than the grand total (verified live, 2026-08-11). `httpkit.NewLinkPaginator` therefore cannot drive these reads: it stops at the first response with no `Link` header, which would truncate a multi-page timeline to page one. The shared page-number paginator (`httpkit.NewPagePaginator`) drives all three reads, reached through the package's own `paginateSCM` wrapper, which pins the `page` and `limit` parameter names and the page ceiling every SCM-boundary reader shares and converts a walk failure to the SCM boundary. The package owns no paginator of its own. Because the timeline is oldest-first and read forward to the cap, an unusually long timeline truncates its newest entries, where a label command lives; the adapter logs a WARN on reaching the cap.

---

## CI status provider

The package registers a CI status provider under kind `gitea` (`domain.CIStatusProvider`, `internal/domain/ci.go`) alongside the tracker and SCM adapters. It drives the CI-failure reaction, the role the GitHub provider fills for GitHub-backed deployments.

```
GET /repos/{owner}/{repo}/commits/{ref}/status
```

- `FetchCIStatus(ref)` reads the combined commit status directly by ref, with no PR fetch or SHA resolution, and normalizes it to a `CIResult`.
- Each per-status entry becomes a `CheckRun`: `context` is the check name, `status` maps to the run status and conclusion (`success` to success, `failure` and `error` to failure, `warning` to neutral, everything else to pending), and `target_url` is the details URL.
- The aggregate is computed from the per-status entries, never the top-level `state` (the same spurious-`pending`-on-empty trap as the SCM read). An empty status set yields a pending result with an empty, non-nil check-run slice.
- The failing-status log excerpt is assembled from the first failing entry's `description` and `target_url`, both already in the authenticated combined-status response. The provider never fetches `target_url`, so an operator-configured or third-party run URL cannot expand the request beyond the Gitea API. ANSI escape sequences are stripped and the excerpt is truncated to `max_log_lines`; a zero budget, or a failing entry with neither field, omits the excerpt.
- The `target_url` field name and the route's pagination are verified live; see Combined commit status. This reader walks every page for the same reason the SCM read does, so a many-status commit cannot hide a failing entry behind the first page.

---

## SCM write surface

The tracker adapter package also implements the SCM write methods `MergePR`, `DeleteBranch`, and `RemoveLabel` (`domain.SCMAdapter`, `internal/domain/scm.go`), plus the auto-merge scope preflight `VerifyAutoMergeScopes`. The route facts below were verified live against the lab instance, and each claim that differs between the two pinned releases says so.

### MergePR

```
POST /repos/{owner}/{repo}/pulls/{index}/merge
{"Do": "merge", "head_commit_id": "<expected head sha>"}
```

- `giteaMergeOption` sends the merge style in the `Do` field, the stale-precondition SHA in `head_commit_id`, and the caller's commit title and message in `MergeTitleField` and `MergeMessageField`, omitting the latter two when empty so Gitea applies its strategy-specific defaults. `mapMergeStrategy` maps the domain strategies to `merge`, `squash`, and `rebase`, and rejects anything else with `domain.ErrSCMPayload` before a request goes out. Gitea's own `delete_branch_after_merge` and `merge_when_checks_succeed` options stay unused: Sortie keeps the two-step merge-then-delete flow and gates CI itself before calling this route.
- A successful merge returns HTTP 200 with an **empty body** (`Content-Length: 0`, verified live): no merge-commit SHA comes back on this route, so a successful `domain.MergeResult` carries `Merged: true` and an empty SHA.
- An already-merged PR returns HTTP 405 `{"message":"The PR is already merged"}` (verified live, 2026-08-11, with the PR unchanged afterward). The adapter does not match that wording: `resolveMergeConflict` re-reads the pull request after the rejection and, on an observed `merged`, returns `scmcore.AlreadyMergedConflict`, which carries the marker text the caller's success dispatch looks for. Gating on the observed state rather than the phrasing survives a reworded rejection and never fires on a stale-head 409.
- A stale precondition (a `head_commit_id` behind the branch's current head) returns HTTP 409 `{"message":"head out of date"}`.
- `classifyHTTPError` records the HTTP status on `domain.TrackerError.Status`, and `scmcore.AsMergeConflict` promotes exactly status 405 and 409 to `domain.ErrSCMConflict`. That promotion is applied on the merge write path only, so a 405 or 409 on any other route keeps its own class. A 403 naming a required scope goes through `enrichScopeError` instead.

### DeleteBranch

```
DELETE /repos/{owner}/{repo}/branches/{branch}
```

- Success is HTTP 204, which `DeleteBranch` returns as nil.
- An already-deleted branch returns HTTP 404 `{"message":"not found","errors":["branch does not exist [...]"]}`. `DeleteBranch` returns that as a `domain.ErrSCMNotFound` rather than mapping it to nil; the caller treats it as a successful no-op.
- A slash-bearing branch name (for example `feature/x`) percent-encoded as `%2F` returns 204, verified live: the route accepts the encoded slash, and the adapter percent-encodes every branch name.

### RemoveLabel

Used by the label-command and label-review reactions to remove the acknowledged command label. Gitea's label-remove route takes a numeric label id, never a name:

```
GET /repos/{owner}/{repo}/issues/{index}/labels          (resolve name to id)
DELETE /repos/{owner}/{repo}/issues/{index}/labels/{id}
```

- A name placed directly in the id position is rejected: HTTP 404 `{"message":"label does not exist [label_id: 0]"}` on 1.27.0, and 422 with the same message on 1.26.4 (verified live, 2026-08-11). `RemoveLabel` resolves the name against the PR's own labels first (`fetchIssueLabels`, `resolveLabelID`) and never places a name in the id slot, so neither status is reachable in practice.
- An unresolved label name is a no-op: no `DELETE` is issued and the call returns nil, the already-absent behavior the `domain.SCMAdapter` contract requires.
- Removing a label the issue does not carry is **not** an error. On 1.26.4 a `DELETE` naming an existing repository label that the issue does not have returns 204, repeatably (verified live, 2026-08-11). A numeric id matching no repository label returns 422 with the same `[label_id: 0]` body as the name case, not 404. On that release the route therefore has no 404 path, and `RemoveLabel`'s mapping of 404 to nil, which covers a delete racing an external removal, does not fire there.

### Auto-merge scope preflight

Gitea exposes no scope-introspection surface: there is no `/rate_limit` endpoint, no `X-OAuth-Scopes` response header, and a token's own scopes appear only inside the body of a 403 rejection on a write call. `permissions.push` from `GET /repos/{owner}/{repo}` reflects the token owner's **repository role**, not the token's own scope: a read-only token owned by a repository admin still reports `push: true`. Gitea also has one coarse `write:repository` scope covering both `MergePR` and `DeleteBranch`, with no separate contents/pull-request split. So the preflight cannot verify the write scope the way the GitHub adapter verifies fine-grained PAT permissions.

`VerifyAutoMergeScopes` substitutes a layered check instead of scope verification:

1. **Fail-open scope sentinel.** With no scope surface to probe, the method returns the `(nil, nil, nil)` "unable to verify" sentinel and auto-merge proceeds, the same path the GitHub adapter takes for a fine-grained PAT. An absent or malformed `project` config value leaves the preflight hints empty and takes this arm too. The `requireContents` argument is accepted and inert, because Gitea's one coarse scope covers both writes.
2. **User-role push gate.** When the tracker project is configured, the method reads `permissions.push` from the repository and, when it is false, returns `write:repository` in `missing`, which `orchestrator.RunAutoMergePreflight` reports as a failed preflight. This is a role check, not a scope check: it catches a wrong-owner or read-only-collaborator token, but not a read-scoped token whose user otherwise has write access.
3. **Runtime scope enrichment.** A 403 on `MergePR` or `DeleteBranch` naming a required scope is rewritten by `enrichScopeError` to name the scope from the `required=[...]` list, falling back to `write:repository` when a bounded body snippet truncated the list, so the operator learns which scope to grant even though the startup check could not confirm it.

---

## Webhooks

Webhooks are the one PR-reaction surface with no adapter implementation: nothing in `internal/scm/gitea` reads or registers them, and every read above is poll-based. Gitea serves standard webhooks at `POST /repos/{owner}/{repo}/hooks` (swagger, not exercised).

---

## Config mapping

- **`tracker.endpoint`:** required, no default host. Instance base URL, for example `https://gitea.example.com`; every constructor right-trims trailing slashes and appends `/api/v1` unless the value already ends in it. TLS strongly recommended; the token travels in a header.
- **`tracker.api_key`:** access token string (single value, no `email:token` composition), sent as `Authorization: token <key>`. No prefix to validate, so the GitHub adapter's key-shape hints do not transfer. `validateAPIKeyHint` points an operator with an empty value at `SORTIE_GITEA_TOKEN`.
- **`tracker.project`:** `owner/repo`; parsed at construction into owner and repo and rejected unless it holds exactly one `/` with non-empty halves.
- **`tracker.active_states` / `tracker.terminal_states` / `tracker.handoff_state`:** repository label names, lowercased at construction, same contract as the GitHub adapter. Omitted lists fall back to `issuekit.DefaultActiveLabelStates` and `issuekit.DefaultTerminalLabelStates`. Missing labels are created on demand (see State model).
- **`tracker.query_filter`:** URL query fragment merged into the repo issue list request; see Server-side filtering.
- **Page size:** `limit=50` per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics); the instance cap defaults to the same 50.
- **Network timeout:** 30 s, set on the shared client by `newGiteaClient`, matching the same section.
- **Headers:** `Authorization`, `Accept: application/json`, and `User-Agent`, which `cmd/sortie` sets to `sortie/<version>` and the constructors default to `sortie/dev`. `Content-Type: application/json` is set on requests carrying a body. No API version header exists.
- **Construction preflight:** `GET /user` (credential and identity check), `GET /repos/{owner}/{repo}` (project check). Both failures are configuration errors surfaced at startup, not at the first poll.

---

## Package layout and shared helpers

One package, `internal/scm/gitea`, registers all three surfaces under kind `gitea`: the tracker through `registry.Trackers.RegisterWithMeta` (which also carries the required-project and required-api-key flags, the default state lists, and the offline `validateConfig` hook), the SCM adapter through `registry.SCMAdapters`, and the CI provider through `registry.CIProviders`. The orchestrator and `cmd/sortie` reach all three through `internal/registry`, never by importing this package. This is the combined tracker-plus-SCM layout `internal/scm/github` uses; the `internal/scm/` boundary rules apply unchanged, with no cross-adapter imports, no orchestrator imports, and external responses normalized to domain types at the boundary.

The adapter builds on the shared utility packages rather than a Gitea client library, which keeps the zero-runtime-dependency model intact:

- `internal/httpkit` for the HTTP client, the `Link`-header paginator (`NewLinkPaginator`), the page-number paginator (`NewPagePaginator`), and the preflight retry backoff.
- `internal/issuekit` for label-state derivation, label and comment normalization, and the default active and terminal state lists.
- `internal/trackermetrics` for per-operation instrumentation, wired through `SetMetrics`.
- `internal/typeutil` for config decoding.
- `internal/scm/scmcore` for the forge decisions every SCM adapter shares: tracker-to-SCM error conversion, merge-conflict promotion and the already-merged marker, bot-author classification, sortable event ids, timestamp parsing, and the CI aggregate and merge gate.

It shares no base structs with the GitHub adapter: the overlap between the two is transport-shaped rather than domain-shaped, and it already lives in `httpkit`.

---

## Key differences from the GitHub adapter

| Aspect | GitHub | Gitea |
| --- | --- | --- |
| Default host | `https://api.github.com` | None; endpoint is required configuration |
| Auth header | `Authorization: Bearer <token>` | `Authorization: token <key>` canonical (Bearer also accepted) |
| Token shape | Prefixed (`ghp_`, `github_pat_`) | 40 hex chars, no prefix |
| Permissions | Fine-grained PAT permissions | `read:`/`write:` scopes; `write:issue` covers the whole tracker surface |
| Search endpoint | `GET /search/issues`, qualifier syntax | `/repos/issues/search`, plain text, no qualifiers, no repo scoping; unused by the adapter |
| `labels` list filter | AND semantics, case-insensitive | AND semantics, case-sensitive, and an unresolvable name drops the filter entirely |
| Label removal | By name | By numeric id only |
| Unknown label on attach | 422, visible | HTTP 200, silently ignored |
| Label auto-creation policy | Pre-created labels required | Create-on-missing (silent-ignore removes the fail-loudly option) |
| `blocked_by` | `.../dependencies/blocked_by` | `.../dependencies` |
| Parent / sub-issues | `.../parent` endpoint | No concept; always nil |
| Issue types | `type.name` (org feature) | No concept; always empty |
| Sort control on lists | `sort` + `direction` | None; fixed newest-first, client-side re-sort |
| Page-size parameter | `per_page` (max 100) | `limit` (clamped to `MAX_RESPONSE_ITEMS`, default 50) |
| Comments route | Paginated | Unpaginated, complete in one response |
| Conditional requests | ETag / 304, free of rate-limit cost | No ETag support |
| Rate limits | 5,000/hr core plus 30/min search plus secondary | None built-in; instance capacity is the budget |
| Close reason | `state_reason` field | No equivalent |
| Validation statuses | 400, 422 | 400, 412, 422, 423 |
| Error body | Varied shapes | Uniform `{"message", "url"}` |
| GraphQL | Available (used for review decisions) | Not available |

---

## Open questions

Each entry names the probe that would settle it.

- **Token scopes on 1.26.4.** Every scope claim in [Authentication](#authentication) rests on the 1.27.0 observation: `write:issue` covering the nine tracker operations, the 403s without `read:user` and `read:repository`, and the read-plus-write collapse. Settling them on 1.26.4 means minting scoped tokens on that lab and repeating the operation matrix. The 403 scope-rejection body that `enrichScopeError` parses is part of the same probe.
- **423 `repo is archived` on 1.26.4.** The lab holds no archived repository, so the status has no reproduction there. Probe: archive a throwaway repository and issue a comment write against it.
- **`assigned_by` and `mentioned_by` on 1.26.4.** Both returned zero for every instance user on that lab: no issue carries an assignee, so `assigned_by` had no positive control, and the two issue bodies that mention another user matched nothing. Because the fixture was partly built by import and Gitea indexes mentions at write time, a stale mention index and a behavior change are indistinguishable there. Probe: create an issue through the API with an assignee and a mention of a second user, then re-query both filters.
- **`MergePR` success and stale-head shapes on 1.26.4.** The empty 200 body and the 409 `head out of date` each need a live pull request to merge or to race, which no probe has done on that lab. The same probe would observe `merge_commit_sha` on the wire, which rests on upstream-source provenance alone (see [Mergeability](#mergeability)).
- **Merge-option field-name casing.** Whether Gitea's binder accepts `"do"` as well as the `"Do"` the adapter sends is undetermined: both spellings reached the already-merged 405, which is returned before the binding discriminates. Probe: send each spelling against a mergeable pull request.
- **Label-remove status on 1.27.0 for an unmatched numeric id.** 1.26.4 answers 422 where 1.27.0 answers 404 for a name in the id slot; the 1.27.0 answer for a numeric id matching no repository label has no observation. Probe: `DELETE .../issues/{index}/labels/{unused-id}` on a 1.27.0 instance.
- **Forgejo and Codeberg.** Compatibility is claimed from the portable-subset argument in [Forgejo and Codeberg](#forgejo-and-codeberg), not observed. Probe: run the env-gated suite against a pinned Forgejo image through the existing container harness.

---

## Source attribution

| Topic | Primary source | Evidence |
| --- | --- | --- |
| Route surface, parameters, request and response schemas | Instance `GET /swagger.v1.json`, which self-reports its release | Live `curl` against every tracker-relevant route, 1.27.0 on 2026-07-14; OpenAPI re-read on 1.26.4 on 2026-08-11 |
| Canonical `token` header, OAuth2 Bearer, query-parameter auth, sudo | docs.gitea.com: API usage (via Context7 `/websites/gitea`) | Live matrix of all five schemes, 1.27.0 on 2026-07-14 and 1.26.4 on 2026-08-11 |
| Token creation route, scope taxonomy, and scope sufficiency | docs.gitea.com: API usage | Live token creation, scope collapse, and 403 scope probes, 1.27.0 on 2026-07-14 |
| Pagination (`page`/`limit`, `Link`, `x-total-count`), clamps and defaults | docs.gitea.com: API usage | Live header inspection with clamp and default measured, 1.27.0 on 2026-07-14 and 1.26.4 on 2026-08-11 |
| `MAX_RESPONSE_ITEMS`, `DEFAULT_PAGING_NUM` | docs.gitea.com: config cheat sheet, `[api]` section | Live `/settings/api` readout on both releases, 2026-07-14 and 2026-08-11 |
| List parameters and ordering (`sort` and `direction` ignored, `per_page` ignored, `type=issues`) | **Live API** | Direct probes, 1.27.0 on 2026-07-14 and 1.26.4 on 2026-08-11 |
| Label filter AND semantics and dropped-filter behavior | **Live API** (the swagger description says "any of", which the instance contradicts) | Two-label and unresolvable-name probes, 1.27.0 on 2026-07-14 and 1.26.4 on 2026-08-11 |
| Label id-only removal, silent unknown-name attach, `color` requirement | **Live API** | Direct probes with statuses and bodies captured, 1.27.0 on 2026-07-14 |
| Label-remove statuses for a name and for an unmatched numeric id | **Live API** (lab instance, Gitea 1.26.4) | 422 for both, and 204 for an existing label the issue does not carry, 2026-08-11 |
| Absent routes (`/search/issues`, `.../dependencies/blocked_by`, `.../parent`) | **Live API** | 404 on each, paired with a 200 control on a sibling route, 1.27.0 on 2026-07-14 and 1.26.4 on 2026-08-11 |
| Dependencies semantics (`dependencies` = blockers, `blocks` = dependents) and create-body shape | **Live API** | Created a dependency and read both directions, 1.27.0 on 2026-07-14; repeated and reverted on 1.26.4 on 2026-08-11 |
| Comments route unpaginated completeness | **Live API**, corroborated by the instance OpenAPI, which declares only `since` and `before` | 60-comment issue returned in full on 1.27.0 on 2026-07-14; 61-comment issue, including under `limit=5`, on 1.26.4 on 2026-08-11 |
| 412, 422, 423, 401, 403, 404 shapes | **Live API** | Provoked each status on 1.27.0 on 2026-07-14; 401, 404, 412, and 422 re-provoked on 1.26.4 on 2026-08-11 |
| No built-in rate limiting | go-gitea/gitea#9559 (open feature request); go-gitea/gitea#24102 (`/rate_limit` 404) | Live 404 on `/rate_limit` against a `/version` 200 control, plus absent `x-ratelimit-*` and `ETag` headers, on both releases |
| Docker image registry and tags | docs.gitea.com: installation with Docker (via Context7) | Documentation only |
| Forgejo API base, token header, pagination | forgejo.org/docs: API usage | Codeberg live swagger and version endpoint, 2026-07-14 |
| Codeberg version marker and label-route divergence | **Live Codeberg** `GET /api/v1/version` and `/swagger.v1.json` | Fetched 2026-07-14 |
| Populated `PullReview` and `PullReviewComment` shapes | **Live API** (lab instance, Gitea 1.26.4) | Provisioned a PR with two reviews and inline comments, then read both routes, 2026-08-10; the comments re-read under a non-admin identity with identical results |
| Review-comment `position` / `original_position` semantics | **Live API** (lab instance, Gitea 1.26.4), corroborated by `modules/structs/pull_review.go` at the `v1.26.4` and `v1.27.0` tags | Anchored one comment new-side and one old-side on the same modified line, then pushed a commit deleting that line and re-read both, 2026-08-10; the side selectors re-read on 2026-08-11 |
| Absence of any invalidation signal: the file-scoped both-zero shape, out-of-diff anchor acceptance, review `stale`, and comment `updated_at` | **Live API** (probe lab instance, Gitea 1.26.4, identified by `GET /api/v1/version` and its own `GET /swagger.v1.json`) | Two runs against one pull request on 2026-08-16: the first anchored comments new-side and old-side on the same modified line and pushed two commits, one deleting the anchored line, without changing either comment; the second added three comments anchored away from the head commit's own change (two new-side, one old-side) and pushed a commit touching none of their anchors. `review_comments_mixed.json`, `review_comments_bot_mixed.json`, and `review_comments_file_scoped.json` were captured from those runs |
| `commit_id`'s blame-versus-head mechanism | `services/pull/review.go` (`createCodeComment`) at the `v1.26.4` and `v1.27.0` tags | Upstream-source provenance, not a live-verification claim: the wire response names only a commit identifier, not the mechanism that produced it. The resulting values were observed live on 1.26.4 on 2026-08-16 |
| Upstream `Invalidated` field absent from the wire shape | `models/issues/comment.go` and `services/convert/pull_review.go` (`ToPullReviewComment`) at the `v1.26.4` and `v1.27.0` tags | Upstream source, cross-checked against the instance OpenAPI and the live response, 1.26.4 on 2026-08-16 |
| Combined-status pagination, `total_count` per-page semantics, and `target_url` | **Live API** (lab instance, Gitea 1.26.4), corroborated by the instance OpenAPI, which declares `page` and `limit` | Seeded 51 statuses on one commit and read the route at the default page size, `limit=50` pages one and two, `limit=20`, and `limit=100`, with response headers captured, 2026-08-10 and 2026-08-11 |
| PR object shape (`mergeable` a plain bool, `mergeable_state` absent) | **Live API** | Lab PR on 1.27.0 on 2026-07-14; re-confirmed on 1.26.4 on 2026-08-11 |
| Already-merged merge rejection (405) | **Live API** (lab instance, Gitea 1.26.4) | `POST .../pulls/{index}/merge` against a merged PR, which stayed unchanged afterward, 2026-08-11 |
| Timeline headers (no `Link`, page-length `X-Total-Count`) | **Live API** (lab instance, Gitea 1.26.4) | `limit=2`, `limit=5`, and the default page size on an issue whose timeline exceeds one page, 2026-08-11 |
| GitHub adapter behavior in the comparison table | `internal/scm/github/tracker.go` (`fetchCandidatesViaIssues`, `fetchCandidatesViaSearch`, `fetchStatesByNumbers`, `fetchBlockers`, `fetchParent`, `TransitionIssue`, `AddLabel`) | Code reading |

### Context7 verification report

Library resolved: `/websites/gitea` (1,874 snippets, High reputation). Queries confirm the canonical `Authorization: token` header and its "historical reasons" phrasing, OAuth2 Bearer and query-parameter alternatives, the scope taxonomy with the `all` special scope, `page`/`limit` pagination with `Link` and `x-total-count` headers, `MAX_RESPONSE_ITEMS` defaulting to 50, and the `docker.gitea.com/gitea:<version>` image reference with rootless variants. Context7 covers none of the label-filter semantics, the id-only label removal, the unpaginated comments route, the 412 and 423 statuses, or the absence of rate limiting; those facts are live-verified, and rate limiting is additionally sourced from the upstream issue tracker. Where the swagger description and the instance disagree (the `labels` filter "any of" claim), this document records the live behavior and names the discrepancy.
