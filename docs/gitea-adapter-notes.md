# Gitea REST API: Adapter research notes

> Gitea REST API v1, researched July 2026 and pinned to **Gitea 1.27.0**. Route surface taken from the instance's own OpenAPI description (`GET /swagger.v1.json`), cross-checked against docs.gitea.com, then **verified live** against a local Gitea 1.27.0 instance on 2026-07-14. Forgejo compatibility checked against Codeberg's published swagger and version endpoint on the same date. Reference for implementing the Gitea `TrackerAdapter` and `SCMAdapter`. The [SCM write surface](#scm-write-surface) covers merge, branch delete, label removal, and the auto-merge preflight.
>
> Gitea is self-hosted: there is no fixed default host, so the instance base URL is part of every configuration, and instances differ in version and settings. Facts below hold for 1.27.0 defaults unless marked otherwise. Gitea exposes no GraphQL API; the REST surface under `/api/v1` is the whole contract.

---

## Native adapter versus GitHub adapter reuse

The central question for this integration: Gitea's API is GitHub-inspired, and the GitHub adapter honors a configurable `tracker.endpoint`, so basic issue polling might work by pointing the existing GitHub adapter at a Gitea instance. The community request that motivated this research proposed exactly that (issue #619).

**Verdict: build a standalone native `kind: gitea` adapter.** Reuse fails in ways that are worse than hard errors: two of the failure modes below corrupt state or lose data silently. A native adapter also matches the one-package-per-tracker precedent set by the Jira, GitHub, and Linear adapters (per ADR-0003), so no ADR is required for this outcome.

### Reuse compatibility matrix

Every route and behavior the GitHub tracker adapter (`internal/scm/github`, `tracker.go`) depends on, checked against Gitea 1.27.0. "Verified" means exercised live against the local instance.

| GitHub adapter dependency | Gitea 1.27.0 behavior | Consequence under reuse |
| --- | --- | --- |
| `Authorization: Bearer <token>` header | Accepted (verified) | Works |
| `GET /repos/{owner}/{repo}/issues?state=open` | Route exists (verified) | Works |
| `sort=created&direction=asc` on the issue list | Parameters silently ignored; order is fixed newest-first (verified) | Candidate ordering diverges silently |
| `per_page=50` | Parameter silently ignored; Gitea reads `limit`, so the default page size 30 applies (verified) | Correct results, more round trips |
| `pull_request` key on PR entries in the issue list | Present (verified) | PR skip logic works |
| `Link` header pagination | Same `rel="next"` / `rel="last"` format (verified) | Works |
| ETag conditional requests (`If-None-Match`) | No `ETag` header on API responses (verified) | Cache never hits; every poll pays full cost |
| `GET /search/issues` (used for `query_filter` candidates and for terminal states in `FetchIssuesByStates`) | Route absent; Gitea's search lives at `/repos/issues/search` with different, non-qualifier parameters (verified 404) | `query_filter` breaks; startup terminal cleanup degrades to a permanent warning |
| `GET .../issues/{n}/dependencies/blocked_by` | Route absent (verified 404); Gitea serves the same data at `.../issues/{index}/dependencies` | Adapter swallows the 404 and returns no blockers: **silent loss of `blocked_by`**, so blocked issues dispatch anyway |
| `GET .../issues/{n}/parent` | Route absent; Gitea has no parent concept | 404 tolerated, parent is nil; harmless |
| `DELETE .../issues/{n}/labels/{name}` in `TransitionIssue` | Route parameter is a numeric label id; a name parses to 0 and returns 404 `label does not exist [label_id: 0, ...]` (422 on Gitea 1.26.x and earlier) | Adapter tolerates the 404, so the old state label is **never removed: state labels accumulate and state reads silently corrupt** |
| `POST .../issues/{n}/labels {"labels": ["name"]}` | Accepted, additive; but an unknown label name is **silently ignored** with HTTP 200 (verified) | A transition to a not-yet-created state label no-ops without any error |
| `PATCH .../issues/{n} {"state": ..., "state_reason": ...}` | `state` honored; unknown `state_reason` field tolerated and ignored (verified) | Close and reopen work |
| `GET .../issues/{n}/comments?per_page=50` | Route exists; ignores paging parameters and returns the full comment list in one response (verified with 60 comments) | Complete results by accident: the paginator sees no `Link` header and stops after one page |
| `POST .../issues/{n}/comments` | Compatible (verified) | Works |
| `X-GitHub-Api-Version` header | Ignored | Harmless |

Reuse survives the read-mostly happy path and breaks exactly where issue polling turns into orchestration: the state machine (`TransitionIssue`), `query_filter`, terminal cleanup, and blocker awareness. The two silent failures (label removal no-op, unknown-label no-op) would surface as mysteriously stuck workflows rather than as errors an operator can act on.

### Helper sharing

The native adapter is built on the existing shared utility packages, the same way the Linear adapter is: `internal/httpkit` for the HTTP client and the `Link`-header paginator (`httpkit.NewLinkPaginator`, `httpkit.ParseLinkRel`), `internal/issuekit` for shared normalization helpers, `internal/trackermetrics` for instrumentation, and `internal/typeutil` for config decoding. No third-party Gitea client library, consistent with the zero-runtime-dependency model, and no shared base structs with the GitHub adapter: the overlap is transport-shaped, not domain-shaped, and lives in `httpkit` already.

### Package placement

Recommendation: **`internal/scm/gitea`**, the combined tracker-plus-SCM layout the GitHub adapter uses, not a standalone `internal/tracker/gitea` package.

The GitHub precedent shows a tracker registration can ship from an SCM-family package: `internal/scm/github` registers tracker kind `github` (per [architecture Section 11.6](architecture/11-issue-tracker-integration-contract.md#116-implemented-tracker-adapters)), and later grew the `SCMAdapter` and CI surfaces in place. The long-term plan for Gitea is full GitHub parity (tracker, PR reactions, CI feedback, SCM at all levels), so the PR-automation epic sketched in the gap map below lands in the same package. Starting in `internal/tracker/gitea` would force a package move when that epic starts. The `internal/scm/` boundary rules apply unchanged: no cross-adapter imports, no orchestrator imports, and external responses are normalized to domain types at the boundary. The orchestrator resolves the adapter through `internal/registry` under tracker kind `gitea`.

---

## Authentication

Gitea authenticates API requests with personal access tokens. The canonical header format is:

```
Authorization: token <access_token>
```

The word `token` (lowercase) before the key is required "for historical reasons" per the official API usage guide. Verified alternatives on 1.27.0, all returning 200 for the same token:

| Scheme | Format | Notes |
| --- | --- | --- |
| Token header (canonical) | `Authorization: token <key>` | Use this |
| Bearer header | `Authorization: Bearer <key>` | Documented for OAuth2 access tokens; also accepts personal tokens (verified) |
| Basic, token as password | `Authorization: Basic base64(user:<key>)` | Works (verified) |
| Basic, token as username | `Authorization: Basic base64(<key>:)` | Works (verified) |
| Query parameter | `?access_token=<key>` | Works on 1.27.0 defaults (verified) but leaks the secret into URLs and server logs; the adapter MUST NOT use it |

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

The constructor SHOULD run this check before the first poll, mirroring the Linear adapter's `viewer` credential check, so a bad key fails construction rather than the first fetch. The returned `login` is the identity that `assigned_by` and `mentioned_by` filters reference (see Server-side filtering). A second preflight call, `GET /repos/{owner}/{repo}`, fails fast on a mistyped `tracker.project` and reports the token's effective `permissions` (`admin`, `push`, `pull`); without it a bad project surfaces as `tracker_not_found` on every subsequent call, because Gitea's 404 body does not distinguish a missing repository from a missing issue (verified).

### Config mapping

| Config field | Value |
| --- | --- |
| `tracker.endpoint` | Instance base URL, for example `https://gitea.example.com`. Required: there is no default host. The adapter appends `/api/v1`; it SHOULD tolerate a value that already ends in `/api/v1` without double-appending |
| `tracker.api_key` | Access token (single string, no `email:token` composition) |
| `tracker.project` | `owner/repo`, for example `myorg/myrepo`; validated as exactly one `/` at startup, like the GitHub adapter |

Sub-path deployments work because Gitea always serves the API at `{ROOT_URL}/api/v1`, so appending is deterministic. Plain-HTTP endpoints send the token in cleartext; operators SHOULD front self-hosted instances with TLS. There is no API version header; behavior is pinned by the instance version, which `GET /api/v1/version` reports (`{"version": "1.27.0"}`).

---

## Identifiers and project scoping

Gitea issues carry two numbers:

- `number` (the **index**): repo-scoped, human-visible, and the value every per-issue route consumes (`/repos/{owner}/{repo}/issues/{index}`).
- `id`: an instance-global int64 that no issue route accepts as input.

Verified on a two-repository instance: the first issue of the second repository has `number: 1` and global `id: 6`. Pull requests share the issue index sequence within a repository, and the identity split runs deeper: a PR created after issues 1 through 5 got `number: 6`, its PR object reports the pull-table `id: 1`, while the same entity fetched through the issues route reports the issue-table `id: 7`. The global `id` is therefore a trap for cross-referencing, and the index is the only identifier worth storing.

The adapter maps `domain.Issue.ID` and `domain.Issue.Identifier` both to the index as a string, exactly as the GitHub adapter does (its `FetchIssueStatesByIdentifiers` documents "ID and Identifier are both the issue number" and delegates to one shared fetch path). `tracker.project` is `owner/repo`; the adapter splits it once at construction and builds all paths from the parts.

---

## State model

Gitea issues natively carry only `open` and `closed` (query enum adds `all`). There is no workflow engine, no transition graph, and no `state_reason`.

**Decision: label-driven workflow states, identical to the GitHub adapter's convention.** The alternative the research brief named, native state combined with labels, is not a distinct option: the GitHub convention already couples the two by closing the issue on a terminal transition and reopening it on an active one. Adopting the same model keeps operator configuration uniform across the two open/closed trackers.

- `tracker.active_states`, `tracker.terminal_states`, and `tracker.handoff_state` name repository labels, normalized to lowercase per [architecture Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules).
- State derivation scans the issue's labels against the configured lists: first match in `active_states` order, then `terminal_states` order; multiple matches log a warning and keep the first.
- Native `open` with no state label maps to the first `active_states` entry.
- Native `closed` with no terminal label maps to the first `terminal_states` entry.
- A terminal-state transition also closes the issue; an active-state transition on a closed issue reopens it.

### Where the mechanics diverge from GitHub

Three verified Gitea behaviors force different label plumbing:

1. **Label removal is by numeric id.** `DELETE /repos/{owner}/{repo}/issues/{index}/labels/{id}` accepts only a label id; a name in that position returns 404 with `label does not exist [label_id: 0, ...]` (422 on Gitea 1.26.x and earlier). The adapter resolves names to ids up front, so neither status is reached.
2. **Unknown label names are silently ignored on attach.** `POST .../issues/{index}/labels {"labels": ["no-such-label"]}` returns HTTP 200 and attaches nothing (verified). Gitea never auto-creates labels from this route and never errors. Any flow that trusts the server to reject a bad label name will no-op invisibly, so the adapter MUST resolve every label name itself before attaching, and MUST attach by id.
3. **Server-side label filtering is hostile to configuration typos.** See Server-side filtering: an unresolvable name in the `labels` query parameter silently disables the filter instead of returning an empty result.

The adapter therefore loads the repository label list once per operation that needs it (`GET /repos/{owner}/{repo}/labels`, paginated) and resolves names case-insensitively: Gitea stores label names with their original casing and matches them case-sensitively, while Sortie's configured state names are lowercase.

### Label hygiene for state labels

The state labels MUST exist in the repository before transitions can work. Two policies were considered:

- **Pre-created labels, fail loudly when missing** (the GitHub adapter's policy). On GitHub a missing label produces a visible 422; on Gitea the attach route cannot fail loudly (behavior 2 above), so the failure the operator would see is nothing at all.
- **Create-on-missing** (the Linear adapter's policy): resolve the name, and when absent, create it with `POST /repos/{owner}/{repo}/labels`, then attach by id.

Recommendation: **create-on-missing**, because Gitea's silent-ignore removes the fail-loudly option, and a state transition that silently does not happen is the worst outcome of the three. Label creation requires `name` and `color` (422 `[Color]: Required` without it, verified); the adapter supplies a fixed default color (for example `#cccccc`). Creation is covered by the `write:issue` scope (verified), so no extra permissions are needed.

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

- `type=issues` excludes pull requests server-side (verified). The adapter keeps the `pull_request`-key guard as a second line of defense, matching the GitHub adapter's `isPullRequest` check, because single-issue fetches still need it (see operation 2).
- State filtering is client-side against the configured `active_states`, exactly like the GitHub adapter's no-filter path. The adapter MUST NOT push the configured state labels into the `labels` query parameter: the filter is AND-semantics across multiple labels (an issue must carry all of them, verified), and an unresolvable name silently drops the whole filter and returns everything (verified). Either property alone disqualifies it for state scoping.
- Gitea offers no `sort` or `direction` parameters on this route; pages arrive newest-first. The GitHub adapter requests oldest-first server-side, so the Gitea adapter SHOULD re-sort the accumulated result by `created_at` ascending after pagination completes, handing the orchestrator identically ordered candidates.
- Pagination via the `Link` header with `httpkit.NewLinkPaginator` and the shared max-page guard. Comments are set to nil on all returned issues.
- `tracker.query_filter`, when configured, merges into this same request (see Server-side filtering); there is no separate search endpoint in the flow.

### 2. `FetchIssueByID`

```
GET /repos/{owner}/{repo}/issues/{index}
```

- 404 maps to `tracker_not_found`.
- A PR index resolves on this route and returns the PR in issue shape with `pull_request` set (verified: `GET .../issues/6` returned the PR). The adapter returns `tracker_not_found` for it, like the GitHub adapter.
- Comments come from operation 6's route; blockers from `GET .../issues/{index}/dependencies`, which returns full issue objects for every issue **blocking** the queried one (verified: after `POST .../issues/2/dependencies {"index": 1, "owner": ..., "repo": ...}` created "1 blocks 2", the dependencies list of #2 contains #1, and `.../issues/1/blocks` contains #2). Each blocker normalizes to `domain.BlockerRef` with `ID` and `Identifier` set to the blocker's index and `State` derived from its labels and native state, satisfying the `blocked_by` rule in [architecture Section 11.3](architecture/11-issue-tracker-integration-contract.md#113-normalization-rules).
- Parent is always nil: Gitea 1.27 has no parent or sub-issue concept.

### 3. `FetchIssuesByStates`

Two paginated list queries at most, both with client-side state filtering:

- Requested states intersecting `active_states`: `GET .../issues?state=open&type=issues&limit=50`.
- Requested states intersecting `terminal_states`: `GET .../issues?state=closed&type=issues&limit=50`.

The GitHub adapter serves the terminal half through the search endpoint with a server-side label qualifier. Gitea gets the plain closed listing instead, for three reasons: the search route cannot scope to one repository (see Server-side filtering), a server-side `labels` filter inherits the unresolvable-name foot-gun, and a label filter misses closed issues with no terminal label, which the state model still counts as terminal (first `terminal_states` entry). A full closed-issue scan is more traffic, but startup terminal cleanup runs once per process start against an unthrottled self-hosted instance.

### 4. `FetchIssueStatesByIDs` and 5. `FetchIssueStatesByIdentifiers`

Gitea has no bulk state-by-ids route. The adapter loops `GET .../issues/{index}` per requested id, skipping 404s (issues not found are omitted from the map, per the interface contract) and skipping PR entries. Both methods share one implementation because ID and Identifier are the same value. Conditional requests are not worth wiring: Gitea sends no `ETag` on API responses (verified), so the GitHub adapter's ETag cache pattern has nothing to key on. Reconciliation touches only the handful of currently running issues per tick, and the instance applies no rate limits, so the per-issue loop is acceptable as-is.

### 6. `FetchIssueComments`

```
GET /repos/{owner}/{repo}/issues/{index}/comments
```

- This route is **unpaginated**: it accepts no `page` or `limit` parameters (per the swagger description) and returns the complete comment list in one response. Verified with 60 comments: all 60 returned in one body, `X-Total-Count: 60`, no `Link` header, and no truncation at the `MAX_RESPONSE_ITEMS` cap that governs paginated routes.
- Comments arrive oldest-first (ascending id, verified), so no client-side re-sort is needed, unlike Linear.
- Bodies are Markdown and pass through unflattened, like GitHub and unlike Jira's ADF.
- System events (close, reopen, label changes, dependency changes) do not appear on this route (verified: a closed issue with no human comments returns an empty array); they live on the separate timeline route.
- `since` and `before` (RFC 3339) filter by update time (verified) and MAY serve future incremental fetches; the initial implementation fetches all.
- 404 maps to `tracker_not_found`. Returns an empty non-nil slice when the issue has no comments.

### 7. `TransitionIssue`

No workflow transition API exists; the adapter composes the transition:

1. `GET .../issues/{index}`: current labels and native state. A `pull_request` entry fails with `tracker_not_found`.
2. Resolve the repository's labels (`GET .../labels`, paginated) into a case-insensitive name-to-id map. A target state name that resolves to no label triggers the create-on-missing policy (see State model); a target that is not a configured active, terminal, or handoff state fails with `tracker_payload_error` before any write, mirroring the GitHub adapter's guard.
3. Remove the current state label by id: `DELETE .../issues/{index}/labels/{label_id}`, tolerating 404 as already-removed.
4. Attach the target state label by id: `POST .../issues/{index}/labels` with `{"labels": [<label_id>]}`. Attaching by id, never by name, is what makes step 2's resolution failure loud instead of the silent no-op the name path produces.
5. Reconcile native state: terminal target with native `open` sends `PATCH .../issues/{index}` `{"state": "closed"}`; active target with native `closed` sends `{"state": "open"}`. There is no `state_reason` equivalent; the field does not exist in `EditIssueOption`.

An invalid `state` value in the PATCH body returns HTTP 412 `unknown state: ...` (verified), a status GitHub does not use; see Error model. Partial failures converge on retry because each step is idempotent, the same argument the GitHub adapter makes.

### 8. `CommentIssue`

```
POST /repos/{owner}/{repo}/issues/{index}/comments
{"body": "<markdown text>"}
```

Returns 201 with the created comment (verified). The text posts verbatim as Markdown, no format conversion.

### 9. `AddLabel`

Used for CI failure escalation, so a silent no-op violates the contract: the escalation label must actually appear. The flow:

1. Resolve the label name against the repository labels, case-insensitively.
2. When absent, create it: `POST /repos/{owner}/{repo}/labels` with `{"name": "<label>", "color": "#cccccc"}` (`color` is required; 422 without it, verified). Covered by `write:issue` (verified).
3. Attach by id: `POST .../issues/{index}/labels` `{"labels": [<id>]}`.

The attach endpoint is **additive**: it appends to the issue's label set and never drops existing labels (verified: attaching a second label preserved the first). The response body is the issue's full label list, so the adapter can confirm the attach landed. Label errors remain non-fatal to the orchestrator.

---

## Server-side filtering and `query_filter`

### Repo issue list parameters

`GET /repos/{owner}/{repo}/issues` accepts (swagger, 1.27.0):

| Parameter | Type | Verified semantics |
| --- | --- | --- |
| `state` | `open`, `closed`, `all` | Defaults to open-only listing |
| `type` | `issues`, `pulls` | Server-side PR exclusion works |
| `labels` | comma-separated names | **AND across labels** (an issue must carry all; verified `labels=backlog,bug` returns nothing when no issue has both), despite the swagger description claiming "any of". Name matching is case-sensitive, and a name that resolves to no label **silently disables the entire filter** (verified: `labels=BACKLOG` returned every open issue). Treat as unusable for correctness-critical filtering |
| `q` | string | Free-text match on title and body (verified) |
| `milestones` | comma-separated names or ids | Names with id fallback (swagger) |
| `since`, `before` | RFC 3339 | Update-time window |
| `created_by` | username | Author filter |
| `assigned_by` | username | Assignee filter (verified) |
| `mentioned_by` | username | Mention filter (verified). Self-mentions do not register: an author mentioning themselves is not counted, so the automation identity must differ from the issue author |
| `page`, `limit` | integers | See Pagination |

The `assigned_by`, `mentioned_by`, and `created_by` filters take arbitrary usernames and cover the motivating scenario from issue #619 (scope candidate polling to issues assigned to or mentioning the automation identity) directly on the repo-scoped route.

### Cross-repo search route

`GET /repos/issues/search` is Gitea's counterpart to GitHub's `/search/issues`, and it is not a drop-in: the path differs, `q` is plain text with **no qualifier syntax** (`label:x repo:o/r` does not parse), and the identity filters (`assigned`, `created`, `mentioned`, `review_requested`) are booleans relative to the authenticated user. Scoping is by `owner` or `team`, not by repository, so the adapter has no use for it: every candidate query stays on the repo issue list route.

### `query_filter` mapping

`tracker.query_filter` is adapter-defined (Jira: JQL fragment; Linear: `IssueFilter` JSON object). For Gitea the recommendation is a **URL query fragment** merged into the repo issue list request:

```yaml
tracker:
  kind: gitea
  query_filter: "assigned_by=hermes-bot&labels=agent-ready"
```

The adapter parses the fragment with `url.ParseQuery`, rejects keys it owns (`state`, `type`, `page`, `limit`) with a configuration error, and merges the rest into the candidate query and the open half of `FetchIssuesByStates`. Operators who put `labels=` in the fragment inherit the server's AND semantics, case sensitivity, and the dropped-filter behavior for unresolvable names; the adapter SHOULD warn when a `query_filter` label does not resolve against the repository's label list, turning the silent foot-gun into a visible diagnostic.

---

## Field mapping

`domain.Issue` field to Gitea issue response path (fields per [architecture Section 4.1.1](architecture/04-core-domain-model.md#411-issue)):

| `domain.Issue` field | Gitea field | Notes |
| --- | --- | --- |
| `ID` | `number` | Index as string; the global `id` is never used (see Identifiers) |
| `Identifier` | `number` | Same value as `ID` |
| `Title` | `title` | |
| `Description` | `body` | Markdown, empty string when null |
| `Priority` | none | Gitea has no priority field (verified absent from the 1.27.0 issue object); always nil. Priority-from-labels stays a future enhancement, as in the GitHub notes |
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
- Responses carry an RFC 8288 `Link` header (`rel="next"`, `rel="last"`) with absolute URLs, in the same format the GitHub adapter already consumes (verified), plus an `X-Total-Count` header that the `Link`-driven pagination flow does not need. `httpkit.NewLinkPaginator` and `httpkit.ParseLinkRel` handle the `Link` header unchanged; the paginator follows the full `Link` URL, which is correct here because Gitea's next-page URLs preserve the query parameters (verified).
- The server clamps `limit` to the instance's `MAX_RESPONSE_ITEMS` (default 50): `limit=100` returned 50 items (verified), while the `Link` header keeps the requested value and pagination stays consistent. An omitted `limit` falls back to `DEFAULT_PAGING_NUM` (default 30, verified).
- The adapter sends `limit=50`, which equals both the architecture's page-size default (per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)) and the stock cap. Operators can lower the cap in `app.ini` `[api]`, so the adapter MUST iterate by the `Link` header rather than assume 50 arrived; `GET /api/v1/settings/api` exposes the effective values (`max_response_items`, `default_paging_num`) for diagnostics.
- The per-issue comments route is the exception: unpaginated, complete in one response (see operation 6).
- Cursor integrity (`tracker_missing_end_cursor`) does not apply: there are no cursors, and an absent `Link` header is the normal end-of-results signal.

---

## Rate limiting

Gitea ships **no built-in API rate limiting**. There is no `/rate_limit` endpoint (404, verified), no `x-ratelimit-*` response headers (verified absent), and the upstream feature request (go-gitea/gitea#9559) remains open. The cost model is inverted from the SaaS trackers: the budget is the self-hosted instance's capacity, and the operator sizes it.

Consequences for the adapter:

- No preemptive throttling and no rate-budget accounting. Poll cadence is the only pressure control, configured by the operator.
- A reverse proxy in front of the instance MAY inject 429 responses. The adapter maps 429 to `tracker_api_error` and honors a `Retry-After` header when present, mirroring the GitHub adapter's handling, but expects never to see one from Gitea itself.
- No conditional-request machinery: without `ETag` support there is no 304 path and nothing to cache against. The GitHub adapter's ETag cache is dead weight here and is not ported.

---

## Error model

Every API error carries one uniform JSON body (verified):

```json
{"message": "<diagnostic>", "url": "https://<instance>/api/swagger"}
```

Verified message texts worth matching on: `token is required` and `invalid username, password or token` (401), `token does not have at least one of required scope(s), required=[...], token scope=...` (403, names the missing scopes verbatim), `not found` (404, identical for a missing repository and a missing issue), `[Title]: Required` and `[Color]: Required` (422), `unknown state: <value>` (412), and `repo is archived` (423).

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
| 429 | Rate limited (reverse proxy only; Gitea itself never sends it) | `tracker_api_error`, honor `Retry-After` |
| 5xx | Server error | `tracker_transport_error` |
| network | DNS, TCP, TLS failure | `tracker_transport_error` |
| decode | JSON decode failure on a 2xx | `tracker_payload_error` |

Two Gitea-specific traps the classification must respect:

- **Silent 200s are the dangerous "errors".** The unknown-label attach and the dropped `labels` filter both return 200 with wrong-shaped success. No status mapping catches them; the adapter's own resolution steps (attach by id, resolve before filter) are the mitigation.
- **404 ambiguity.** A mistyped `tracker.project` produces the same `not found` as a deleted issue. The construction-time repository preflight (see Authentication) pins the blame at startup.

---

## Forgejo and Codeberg

Forgejo is the 2024 hard fork of Gitea; Codeberg is the flagship hosted Forgejo instance. Findings from Codeberg's live swagger and version endpoint (2026-07-14):

- Forgejo serves the same `/api/v1` base path and the same `Authorization: token <key>` header (its docs keep Gitea's "for historical reasons" phrasing), the same `page`/`limit` pagination with `Link` and `x-total-count`, and the same `/settings/api` endpoint.
- `GET /api/v1/version` on Codeberg returns `15.0.0-209-2308e484+gitea-1.22.0`: Forgejo pins its Gitea compatibility marker at **1.22**, the fork point, while this research pins 1.27. The shared surface is drifting apart release by release.
- Verified route divergence in the exact surface this adapter uses: the label-remove route is `.../labels/{identifier}` on Forgejo, accepting "name or id of the label" (swagger), versus id-only `.../labels/{id}` on Gitea. Forgejo's `IssueLabelsOption` also adds an `updated_at` field Gitea lacks. The adapter's id-based label flow sits inside the portable subset: numeric ids are valid identifiers on both.

**Verdict: Forgejo compatibility is claimed by design, not tested.** The adapter targets the portable subset (issue and comment routes, id-based label operations, `Link` pagination), so a Forgejo or Codeberg instance is expected to work behind the same `kind: gitea` configuration, and no Forgejo-specific adapter is planned. Testing against a pinned Forgejo image (or Codeberg) is explicitly out of scope for this milestone; when demand materializes, the container harness below accepts a Forgejo image with no structural change. Operators pointing Sortie at codeberg.org must respect Codeberg's terms of service for automation; self-hosted instances are the primary target.

---

## Integration test strategy

Two options were evaluated:

1. **Containerized Gitea service in CI.** The job starts a pinned image, provisions an admin user, token, repository, labels, and issues at job start, runs the gated tests, and discards the container.
2. **Hosted long-lived instance with repository secrets**, the pattern the Jira and Linear nightly jobs use against SaaS APIs.

**Recommendation: containerized Gitea in CI.** Gitea is the first tracker in the project that can run inside the job, which removes every drawback the SaaS pattern tolerates by necessity: no repository secrets (the token is generated inside the job), no shared mutable state between runs, no cross-fork secret exposure, exact version pinning to the researched release, and local reproducibility with one `docker run`. The hosted-instance option exercises real TLS and proxy behavior, but that coverage belongs to operators' environments, not to adapter correctness.

Sketch, verified end-to-end by this research's provisioning sequence:

- Image: `docker.gitea.com/gitea:1.27.0-rootless` (the registry and tag scheme documented in the official Docker installation guide; the tag pins the researched version).
- Provisioning: `gitea admin user create --must-change-password=false ...` (the flag matters: a fresh user is otherwise forced into a password-change state that blocks token creation with a 401-class error), then `POST /api/v1/users/{user}/tokens` with basic auth for the token, then repository, labels, and issues through the API.
- Gate: `SORTIE_GITEA_TEST=1`, with `SORTIE_GITEA_ENDPOINT`, `SORTIE_GITEA_TOKEN`, and `SORTIE_GITEA_PROJECT` supplied by the job, following the sibling adapters' single-gate convention and their variable naming: `SORTIE_JIRA_ENDPOINT` sets the endpoint-name precedent, and `SORTIE_GITHUB_TOKEN` plus `SORTIE_GITHUB_PROJECT` set the token and project precedents for a token-authenticated tracker. Without the gate the tests MUST skip cleanly, never fail.
- The same harness slots into the nightly adapter matrix as a tracker entry whose "install" step is the container boot instead of a CLI install.

---

## SCM write surface

The tracker adapter package also implements the SCM write methods `MergePR`, `DeleteBranch`, and `RemoveLabel` (`domain.SCMAdapter`, `internal/domain/scm.go`), plus the auto-merge scope preflight `VerifyAutoMergeScopes`. The route facts below were verified live against a local Gitea instance.

### MergePR

```
POST /repos/{owner}/{repo}/pulls/{index}/merge
{"Do": "merge", "head_commit_id": "<expected head sha>"}
```

- Field binding is case-insensitive: Gitea accepts `"Do"` or `"do"`.
- A successful merge returns HTTP 200 with an **empty body** (`Content-Length: 0`, verified live): no merge-commit SHA comes back on this route.
- An already-merged PR returns HTTP 405 `{"message":"The PR is already merged"}`. That message already contains the substring "already merged" case-insensitively, the marker the caller's success dispatch looks for.
- A stale precondition (a `head_commit_id` behind the branch's current head) returns HTTP 409 `{"message":"head out of date"}`.
- Both 405 and 409 map to `ErrSCMConflict`; only the already-merged case carries the marker text.
- The commit-title and commit-message fields and Gitea's own `delete_branch_after_merge` and `merge_when_checks_succeed` options are not exercised: Sortie keeps the two-step merge-then-delete flow and gates CI itself before calling this route.

### DeleteBranch

```
DELETE /repos/{owner}/{repo}/branches/{branch}
```

- Success is HTTP 204.
- An already-deleted branch returns HTTP 404 `{"message":"not found","errors":["branch does not exist [...]"]}`.
- A slash-bearing branch name (for example `feature/x`) percent-encoded as `%2F` returns 204, verified live: the route accepts the encoded slash.

### RemoveLabel

Used by the label-command and label-review reactions to remove the acknowledged command label. Gitea's label-remove route takes a numeric label id, never a name:

```
GET /repos/{owner}/{repo}/issues/{index}/labels          (resolve name to id)
DELETE /repos/{owner}/{repo}/issues/{index}/labels/{id}
```

- A name placed directly in the id position is rejected: HTTP 404 `{"message":"label does not exist [label_id: 0]"}` on Gitea 1.27.0, or 422 with the same message on Gitea 1.26.x and earlier. The adapter resolves the name against the PR's own labels first and never places a name in the id slot, so neither status is reachable in practice.
- An unresolved label name is a no-op: no `DELETE` is issued and the call returns success.
- Deleting an already-removed label (a raced 404 on a valid numeric id) is also treated as success.

### Auto-merge scope preflight

Gitea exposes no scope-introspection surface: there is no `/rate_limit` endpoint, no `X-OAuth-Scopes` response header, and a token's own scopes appear only inside the body of a 403 rejection on a write call. `permissions.push` from `GET /repos/{owner}/{repo}` reflects the token owner's **repository role**, not the token's own scope: a read-only token owned by a repository admin still reports `push: true`. Gitea also has one coarse `write:repository` scope covering both `MergePR` and `DeleteBranch`, with no separate contents/pull-request split. So the preflight cannot verify the write scope the way the GitHub adapter verifies fine-grained PAT permissions.

`VerifyAutoMergeScopes` substitutes a layered check instead of scope verification:

1. **Fail-open scope sentinel.** With no scope surface to probe, the preflight reports "unable to verify" and auto-merge proceeds, the same path the GitHub adapter takes for a fine-grained PAT.
2. **User-role push gate.** When the tracker project is configured, the preflight reads `permissions.push` from the repository and fails startup when it is false: the token's user lacks repository write access. This is a role check, not a scope check: it catches a wrong-owner or read-only-collaborator token, but not a read-scoped token whose user otherwise has write access.
3. **Runtime scope enrichment.** A 403 on `MergePR` or `DeleteBranch` naming a required scope is rewritten to name `write:repository` explicitly, so the operator learns which scope to grant even though the startup check could not confirm it.

---

## Out of scope: remaining pull request reaction surface

The SCM write surface above (merge, branch delete, label removal) and the auto-merge scope preflight are implemented. The remaining PR-reaction surface below, review decisions, mergeability, CI status, bot-review filtering, and label-command event detection, has no adapter implementation; the gap map records what that surface would build on. Route facts come from the 1.27.0 swagger; none were exercised beyond the notes given.

| SCM surface (GitHub adapter today) | Gitea 1.27 equivalent | Gap notes |
| --- | --- | --- |
| `GetReviewDecision` via GraphQL `reviewDecision` | No GraphQL API. `GET /repos/{owner}/{repo}/pulls/{index}/reviews` returns per-review states (`APPROVED`, `PENDING`, `COMMENT`, `REQUEST_CHANGES`, `REQUEST_REVIEW`) | No aggregate decision field anywhere; the adapter must fold the review list (and branch-protection approval requirements) into a decision itself |
| `GetMergeability` via `mergeable_state` string taxonomy | PR object carries boolean `mergeable`, plus `merged`, `draft`, `head.sha` (verified live) | Boolean collapses GitHub's `clean`/`behind`/`blocked`/`dirty`/`unstable` distinctions; mapping to `MergeabilityState` is lossy and needs supplementary signals |
| `GetCIStatus` via combined status plus check runs | `GET /repos/{owner}/{repo}/commits/{ref}/status` (combined) and `.../statuses/{sha}`; Gitea Actions reports through commit statuses | No check-runs API; single-source aggregation |
| `FetchPendingReviews`, `FetchBotReviewComments` | `GET .../pulls/{index}/reviews` and review-comment routes | Gitea users carry no `type: Bot` marker; bot classification needs an allowlist rather than a platform predicate |
| `ListLabelEvents` via issue events API | No events route; `GET .../issues/{index}/timeline` returns typed entries (verified types include `label`, `comment`, `add_dependency`) with `since`/`before` and paging | Label-command detection would re-derive the journal from timeline entries |
| Webhooks (future push-based reactivity) | `POST /repos/{owner}/{repo}/hooks`, standard Gitea webhooks | Aligns with the webhook-ingress future extension |

---

## Config notes

- **`tracker.endpoint`:** required, no default host. Instance base URL; the adapter appends `/api/v1`. TLS strongly recommended; the token travels in a header.
- **`tracker.api_key`:** access token string, sent as `Authorization: token <key>`. No prefix to validate; the GitHub adapter's key-shape hints do not transfer.
- **`tracker.project`:** `owner/repo`; validated at startup (exactly one `/`, non-empty halves).
- **`tracker.active_states` / `tracker.terminal_states` / `tracker.handoff_state`:** repository label names, lowercase, same contract as the GitHub adapter. Missing labels are created on demand (see State model).
- **`tracker.query_filter`:** URL query fragment merged into the repo issue list request; see Server-side filtering.
- **Page size:** `limit=50` per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics); the instance cap defaults to the same 50.
- **Network timeout:** 30,000 ms per the same section.
- **Headers:** `User-Agent: Sortie/<version>`; `Content-Type: application/json` on writes. No API version header exists.
- **Construction preflight:** `GET /user` (credential and identity check), `GET /repos/{owner}/{repo}` (project check). Both failures are configuration errors surfaced at startup, not at the first poll.

---

## Key differences from the GitHub adapter

| Aspect | GitHub | Gitea 1.27 |
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

## Live verification results

All facts marked "verified" were established against a local Gitea 1.27.0 instance on 2026-07-14:

1. **Auth scheme matrix.** `token`, `Bearer`, basic with token as password, basic with token as username, and the `access_token` query parameter all authenticate; missing token yields 401 `token is required`, a bad token 401 `invalid username, password or token`.
2. **Scope collapse and sufficiency.** Requested `read:issue` + `write:issue` + `read:repository` + `write:repository` collapsed to the write pair; a `write:issue`-only token performed every one of the nine operations, including label creation, but got 403 on `GET /user` and `GET /repos/{owner}/{repo}`.
3. **Label filter semantics.** `labels=in-progress,bug` matched only the issue carrying both; `labels=backlog,bug` matched nothing; `labels=BACKLOG` (wrong case, unresolvable) returned every open issue, proving the dropped filter.
4. **Label attach and removal.** Attach by name works and is additive; an unknown name returns 200 and attaches nothing; `DELETE .../labels/review` (a name) returns 404 `label does not exist [label_id: 0, repo_id: 1]`; delete by id returns 204; `PUT .../labels` replaces the whole set; label creation without `color` returns 422 `[Color]: Required`.
5. **Identity split.** A second repository's first issue: `number` 1, global `id` 6. The PR at index 6 reports pull-table `id` 1 as a PR and issue-table `id` 7 when fetched through the issues route, with `pull_request` set in both list and single-fetch shapes.
6. **List behavior.** Default listing is open-only, newest-first, PRs mixed in; `type=issues` excludes them; `state=all` includes closed; `sort=created&direction=asc&per_page=3` changed nothing (5 items, descending order).
7. **Pagination.** `limit=2` produced `Link` with `rel="next"` and `rel="last"` plus `X-Total-Count: 5`; `limit=100` over 56 issues returned 50 (clamp); no `limit` returned 30; `/settings/api` reported `max_response_items: 50`, `default_paging_num: 30`.
8. **Comments.** Ascending order with `updated_at` present; system events excluded (a PATCH-closed issue shows zero comments); `since` filters correctly; an issue with 60 comments returned all 60 in one unpaginated response with `X-Total-Count: 60` and no `Link`.
9. **Transitions.** `PATCH {"state": "closed"}` and `{"state": "open"}` work; the GitHub-shaped extra `state_reason` field is tolerated and ignored; `{"state": "bogus"}` returns 412 `unknown state: bogus`.
10. **Dependencies.** `POST .../issues/2/dependencies` requires the full `IssueMeta` body (`index`, `owner`, `repo`); with only `index` it returns 404. After creating "1 blocks 2", `.../issues/2/dependencies` lists #1 and `.../issues/1/blocks` lists #2 (documented responses include 423 for locked dependencies).
11. **Identity filters.** `assigned_by=sortie` matched the assigned issue; `mentioned_by=sortie` missed the issue whose author self-mentioned; `mentioned_by=hermes` matched after a second user was mentioned by a different author; `q=handle` matched on body text.
12. **Errors.** 404 body is `not found` for both a missing issue and a missing repository; 422 `[Title]: Required` on an empty create; 403 scope message names the required and held scopes; comment write on an archived repository returns 423 `repo is archived`.
13. **No rate limiting.** `/api/v1/rate_limit` returns 404; no `x-ratelimit-*` headers on any response; no `ETag` header on API responses.
14. **Forgejo probe.** Codeberg's swagger title is "Forgejo API" at `/api/v1`; its version string is `15.0.0-209-2308e484+gitea-1.22.0`; the label-remove route is `.../labels/{identifier}` ("name or id"), and `IssueLabelsOption` gains `updated_at`.

### Integration test setup

The verification lab doubles as the fixture blueprint: a primary test repository with labels `backlog`, `in-progress`, `review`, `done`, `bug`; issues #1 (backlog, two comments, blocks #2), #2 (in-progress and bug, assigned), #3 (closed), #4 (label round-trip target), #5 (60-comment bulk thread), #7 (mentions a second user); PR #6 from branch `feature-x`; plus a second, archived repository (56 bulk issues) for clamp and 423 checks. The provisioning sequence (admin user with `--must-change-password=false`, token via `POST /users/{user}/tokens`, everything else through the API) is fully scriptable for the CI container job.

---

## Source attribution

| Topic | Primary source | Verification method |
| --- | --- | --- |
| Route surface, parameters, request and response schemas | Local instance `GET /swagger.v1.json` (authoritative for 1.27.0) | Live `curl` against every tracker-relevant route |
| Canonical `token` header, OAuth2 Bearer, query-parameter auth, sudo | docs.gitea.com: API usage (via Context7 `/websites/gitea`) | Live matrix of all five schemes |
| Token creation route and scope taxonomy | docs.gitea.com: API usage | Live token creation, scope collapse, and 403 scope probes |
| Pagination (`page`/`limit`, `Link`, `x-total-count`) | docs.gitea.com: API usage | Live header inspection; clamp and default measured |
| `MAX_RESPONSE_ITEMS`, `DEFAULT_PAGING_NUM` | docs.gitea.com: config cheat sheet, `[api]` section | Live `/settings/api` readout |
| Label filter AND semantics and dropped-filter behavior | **Live API** (swagger description says "any of", which the live instance contradicts) | Two-label and unresolvable-name probes |
| Label id-only removal, silent unknown-name attach, `color` requirement | **Live API** | Direct probes, statuses and bodies captured |
| Dependencies semantics (`dependencies` = blockers, `blocks` = dependents) | **Live API** | Created a dependency and read both directions |
| Comments route unpaginated completeness | **Live API** | 60-comment issue returned in full |
| 412, 422, 423, 401, 403, 404 shapes | **Live API** | Provoked each status |
| No built-in rate limiting | go-gitea/gitea#9559 (open feature request); go-gitea/gitea#24102 (`/rate_limit` 404) | Live 404 on `/rate_limit`; absent headers |
| Docker image registry and tags | docs.gitea.com: installation with Docker (via Context7) | Documentation only |
| Forgejo API base, token header, pagination | forgejo.org/docs: API usage | Codeberg live swagger and version endpoint |
| Codeberg version marker and label-route divergence | **Live Codeberg** `GET /api/v1/version` and `/swagger.v1.json` | Fetched 2026-07-14 |
| GitHub adapter behavior used in the reuse matrix | `internal/scm/github/tracker.go` (`fetchCandidatesViaIssues`, `fetchCandidatesViaSearch`, `fetchStatesByNumbers`, `fetchBlockers`, `fetchParent`, `TransitionIssue`, `AddLabel`) | Code reading at research time |

### Context7 verification report

Library resolved: `/websites/gitea` (1,874 snippets, High reputation). Queries confirmed: the canonical `Authorization: token` header and its "historical reasons" phrasing, OAuth2 Bearer and query-parameter alternatives, the scope taxonomy with the `all` special scope, `page`/`limit` pagination with `Link` and `x-total-count` headers, `MAX_RESPONSE_ITEMS` defaulting to 50, and the `docker.gitea.com/gitea:<version>` image reference with rootless variants. Context7 did not cover the label-filter semantics, the id-only label removal, the unpaginated comments route, the 412/423 statuses, or the absence of rate limiting; those facts are live-verified (and, for rate limiting, sourced from the upstream issue tracker). Where the swagger description and the live instance disagreed (the `labels` filter "any of" claim), the live behavior is recorded and the discrepancy noted.
