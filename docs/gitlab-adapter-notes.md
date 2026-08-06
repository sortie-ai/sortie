# GitLab REST API: Adapter research notes

> GitLab REST API v4, researched August 2026 and pinned to **self-managed GitLab Community
> Edition 19.2.1** (revision `f4d029d2da8`, `enterprise: false`), **verified live** on
> 2026-08-04. Community Edition is the compatibility floor: the adapter depends only on what
> it provides. A second live instance, **GitLab.com** (version `19.3.0-pre`, revision
> `7725732be39`, `enterprise: true`, namespace subscription plan `free`), was used solely for
> the self-managed-versus-SaaS comparison. A second live pass on 2026-08-05 re-verified the
> pinned container image `gitlab/gitlab-ce:19.2.1-ce.0` (version `19.2.1`, revision
> `f4d029d2da8`, matching the self-managed lab) and GitLab.com, whose revision had moved to
> `6f59ad3485c` as a continuously deployed instance does. Reference for implementing the
> GitLab `TrackerAdapter`.
>
> Self-managed GitLab has no fixed host, so the instance base URL is part of every
> self-managed configuration, and instances differ in version and settings. Facts hold for
> 19.2.1 Community Edition defaults unless marked otherwise. GitLab also exposes a GraphQL
> API; the tracker surface needs none of it (see [GraphQL](#graphql)).

## Provenance of every claim

Every statement below carries one of five tags. Nothing is asserted without one.

| Tag | Meaning |
| --- | --- |
| **[live-CE]** | Observed directly against the self-managed Community Edition 19.2.1 instance on 2026-08-04, and again on 2026-08-05 against the pinned container image `gitlab/gitlab-ce:19.2.1-ce.0` (version `19.2.1`, revision `f4d029d2da8`, matching the lab). The strongest evidence for Community Edition behavior. |
| **[live-SaaS]** | Observed directly against GitLab.com (`19.3.0-pre`, `enterprise: true`, plan `free`) on 2026-08-04, and again on 2026-08-05 at revision `6f59ad3485c`, moved from the previously recorded `7725732be39` as a continuously deployed instance does. Evidence about GitLab.com only, never about self-managed. |
| **[docs]** | First-party GitLab documentation, cited by URL. |
| **[source]** | Upstream source in `gitlab-org/gitlab`, cited by path at ref `v19.2.1-ee` (the tag matching the researched Community Edition version; the `-ee` tag carries both the Community Edition code and the `ee/` tree, which is how a route's edition gating is proved). |
| **[unknown]** | Not settled by this research. Listed with the evidence that would settle it. |

Where documentation and observed behavior disagree, both sides are recorded and the
behavior the adapter must trust is named. No self-managed Enterprise Edition instance was
available, so **every Enterprise Edition claim is [source] plus [docs] and was never
observed live**; GitLab.com is an Enterprise Edition codebase but not a self-managed
Enterprise Edition deployment, and the two are not interchangeable.

### Why no machine-readable route surface backs these notes

GitLab publishes a Swagger 2.0 description of its REST API in its documentation tree
(`doc/api/openapi/openapi_v2.yaml` and `openapi_v3.yaml`, rendered as an interactive page on
the documentation site) **[source]** **[docs]**. It is not a substitute for the Gitea
research method, for two reasons established by inspection.

First, **a running instance does not serve it.** `GET /api/v4/openapi.json`,
`/api/v4/openapi.yaml`, and `/api/v4/swagger.json` all return
`404 {"error":"404 Not Found"}` **[live-CE]**, so there is no per-instance description to pin
against the version under test the way Gitea's `/swagger.v1.json` allows.

Second, **it omits the routes this adapter depends on.** The 80,000-line description declares
`/api/v4/groups/{id}/issues`, `/api/v4/projects/{id}/issues/{issue_iid}/links`, and several
award-emoji routes, but declares **no** `/api/v4/projects/{id}/issues`,
`/api/v4/projects/{id}/issues/{issue_iid}`, or
`/api/v4/projects/{id}/issues/{issue_iid}/notes` path, and the string `activity_filter` does
not appear in it at all **[source]**. The core list, single-fetch, update, and note routes,
which are seven of the nine operations, are absent.

The description is therefore used here only where it is authoritative about itself: its
`securityDefinitions` declare exactly two mechanisms, a `PRIVATE-TOKEN` header and a
`private_token` query parameter **[source]**, which independently corroborates the
[header schemes](#header-schemes) below. Everything else rests on live observation, the
reference documentation, and upstream source.

---

## The three products

GitLab ships as three distinct products, and conflating them is the primary correctness
hazard in this integration.

| Product | Codebase | Feature gating | Role here |
| --- | --- | --- | --- |
| Self-managed Community Edition | Community Edition only; no `ee/` tree loaded | Nothing to gate: Enterprise routes and parameters are absent from the running API | **The compatibility floor.** The adapter depends only on this surface |
| Self-managed Enterprise Edition | Community Edition plus `ee/` | License tier (Premium, Ultimate) gates features at the service layer | Superset. The adapter must not require anything unique to it |
| GitLab.com | Enterprise Edition | Namespace subscription plan gates features; Free plan is *not* Community Edition | Superset with a different failure mode |

### Detecting the edition

`GET /api/v4/version` and `GET /api/v4/metadata` return an `enterprise` boolean documented
as "Indicates whether the GitLab instance is Enterprise Edition" [[docs]](https://docs.gitlab.com/api/version/).
Observed: `{"version":"19.2.1","revision":"f4d029d2da8","kas":{...},"enterprise":false}`
**[live-CE]** and `{"version":"19.3.0-pre","revision":"7725732be39","kas":{...},"enterprise":true}`
**[live-SaaS]**. Both endpoints require authentication: without a token they return
`401 {"message":"401 Unauthorized"}` **[live-CE]**. A token carrying only `read_api`
suffices **[live-CE]**.

The `enterprise` flag distinguishes the codebase, not the license tier. It cannot tell
Premium from Free on an Enterprise Edition instance. Subscription tier is separate:
`GET /api/v4/namespaces` reports a `plan` field, observed `"plan": "free"` for the
GitLab.com namespace used here **[live-SaaS]**.

### Why the distinction has teeth: the same gap, three different errors

Blocking issue relationships are the clearest case. Creating one requires
`link_type=blocks` on the issue-links route.

| Product | Request | Result |
| --- | --- | --- |
| Self-managed Community Edition | `POST /projects/:id/issues/:iid/links?...&link_type=blocks` | `400 {"error":"link_type does not have a valid value"}` **[live-CE]** |
| GitLab.com, Free plan | Same request | `403 {"message":"Blocked issues not available for current license"}` **[live-SaaS]** |
| Self-managed Enterprise Edition, Premium or above | Same request | Accepted **[source]** **[docs]**, never observed |

The two failures are semantically identical and syntactically unrelated: one is a parameter
validation error, the other an authorization-shaped error. An adapter that classified `403`
as an authentication failure would report a credential problem for a licensing gap. The
mechanism is visible in source: the route declares
`optional :link_type, type: String, values: IssueLink.available_link_types`
([`lib/api/issue_links.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issue_links.rb))
**[source]**, and `available_link_types` is defined in Community Edition as
`[TYPE_RELATES_TO]` with the comment "overridden in EE"
([`app/models/concerns/issuable_link.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/issuable_link.rb))
and extended in
[`ee/app/models/concerns/ee/issuable_link.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/ee/app/models/concerns/ee/issuable_link.rb)
to `super + [TYPE_BLOCKS, TYPE_IS_BLOCKED_BY]` **[source]**. On Community Edition the
enumeration genuinely lacks the value; on Enterprise Edition it has it and the license check
runs later.

The consequence for the normalized issue model is recorded under
[`blocked_by`](#blocked_by-is-unavailable-on-community-edition).

---

## GraphQL

GitLab exposes a GraphQL endpoint at `/api/graphql` in addition to REST v4. This research
found **no tracker operation that only GraphQL serves**: all nine `TrackerAdapter` methods
map to REST v4 routes (see [Operations](#operations)), and two operations that cost extra
round trips on other trackers are cheaper on REST here than they would be anywhere else
(a batch state lookup by `iids[]`, and a whole state transition in one request). The adapter
therefore stays on REST v4, consistent with the transport constraint of building on
`internal/httpkit` with no third-party GitLab client library.

GraphQL becomes relevant only for surfaces outside the tracker contract; see the
[gap map](#out-of-scope-gap-map-for-merge-requests-and-pipelines).

---

## Package placement

**Recommendation: `internal/scm/gitlab`,** the combined forge layout, seeded with the tracker.
The decision and its alternatives are recorded in
[ADR-0016](decisions/0016-place-forge-integrations-in-one-package-per-forge.md).

GitLab is a full forge: issues, merge requests, and pipelines. The evidence that its tracker
and forge halves share machinery rather than merely sitting in one product:

| Shared mechanism | Issue surface | Merge-request surface |
| --- | --- | --- |
| Authentication and token model | `PRIVATE-TOKEN` header, `api` scope | Identical **[live-CE]** |
| Project addressing | Numeric ID or URL-encoded `group/project`, plus a resource-scoped `iid` | Identical **[live-CE]** |
| Pagination | `Link` header, offset and keyset | Identical **[docs]** |
| Error envelopes | Four shapes (see [Error model](#error-model)) | Identical **[live-CE]** |
| Rate-limit headers | `RateLimit-*` | Identical **[live-SaaS]** |
| Comment model | `GET/POST /projects/:id/issues/:iid/notes` | `GET/POST /projects/:id/merge_requests/:iid/notes`, same entity **[live-CE]** |
| Label-event journal | `GET /projects/:id/issues/:iid/resource_label_events` | `GET /projects/:id/merge_requests/:iid/resource_label_events`, same entity **[live-CE]** |

Every row is machinery a merge-request and pipeline half would re-declare from scratch in a
separate package. Starting in `internal/tracker/gitlab` would force a package move at that
point. The `internal/scm/` boundary rules apply unchanged: no cross-adapter imports, no
orchestrator imports, and external responses are normalized to domain types at the boundary.
The orchestrator resolves the adapter through `internal/registry` under tracker kind
`gitlab`.

Helper sharing follows the Linear and Gitea precedent: `internal/httpkit` for the HTTP client
and the `Link`-header paginator (`httpkit.NewLinkPaginator`, `httpkit.ParseLinkRel`),
`internal/issuekit` for shared normalization, `internal/trackermetrics` for instrumentation,
and `internal/typeutil` for config decoding. No shared base structs with the GitHub or Gitea
adapters: GitLab's REST v4 is not GitHub-compatible, so the overlap is transport-shaped and
already lives in `httpkit`.

---

## Authentication

### Header schemes

All schemes were exercised against the same token **[live-CE]**:

| Scheme | Format | Verdict |
| --- | --- | --- |
| Private-token header (canonical) | `PRIVATE-TOKEN: <token>` | **Use this.** 200 **[live-CE]** |
| Bearer header | `Authorization: Bearer <token>` | 200 **[live-CE]**. Documented for OAuth 2.0 access tokens and also accepts personal, project, and group access tokens |
| Query parameter | `?private_token=<token>` | 200 **[live-CE]**, but leaks the secret into URLs, proxy logs, and server logs. The adapter MUST NOT use it |
| Job-token header | `JOB-TOKEN: <token>` | Rejects a personal access token: `401 {"message":"401 Unauthorized"}` **[live-CE]**. Accepts only CI job tokens, which the adapter does not use |

`PRIVATE-TOKEN` is the scheme the REST documentation leads with
[[docs]](https://docs.gitlab.com/api/rest/). It is preferred here over `Authorization: Bearer`
because it is unambiguous: it accepts only GitLab access tokens, whereas the `Authorization`
header is shared with OAuth flows.

### Token types

Four token types authenticate against the issue surface. All four are sent in the same
header, and all carry the same fixed GitLab access-token prefix and were 51 characters
long at the researched version
**[live-CE]**. The prefix makes a token machine-identifiable, so secret scanners can
recognize a leak by shape.

| Token type | Identity returned by `GET /user` | Access scope | Suitability |
| --- | --- | --- | --- |
| Personal access token | The owning human user, `bot: false` **[live-CE]** | Everything that user can reach | Works. Couples automation to a person's account and their whole project set |
| **Project access token** | A generated bot user, `bot: true` **[live-CE]** | Exactly one project. A sibling project in the same group returns `404 {"message":"404 Project Not Found"}` **[live-CE]** | **Recommended for a single-project workflow** on self-managed Community Edition, which has this token type at any license. On GitLab.com it requires a Premium or Ultimate subscription [[docs]](https://docs.gitlab.com/user/project/settings/project_access_tokens/), so a Free namespace can only use a personal access token. Least privilege, and the containment is enforced by the server |
| Group access token | A generated bot user, `bot: true` **[live-CE]** | Every project in the group, plus `GET /groups/:id/issues` **[live-CE]** | Appropriate when one workflow spans a group, on self-managed Community Edition at any license. On GitLab.com it requires the same Premium or Ultimate subscription as the project access token [[docs]](https://docs.gitlab.com/user/group/settings/group_access_tokens/) |
| OAuth 2.0 access token | The authorizing user | The granted scopes | Not used. Sortie runs as a headless service and implements no interactive authorization-code flow or callback, the same reasoning the Jira integration applies |

Project and group access tokens are created with an `access_level`; the level must permit
issue writes for the write operations. A `Developer`-level token performed every write in
this research **[live-CE]**.

`POST /projects/:id/access_tokens` also requires `expires_at`, and the accepted window is
bounded at both ends by the run date: a date more than one year ahead returns
`400 {"message":"400 Bad request - Expiration date must be before <one year from the run
date>"}`, and a date in the past returns **201** carrying `"active": false`, whose token then
fails every call with `401 {"error":"invalid_token"}` **[live-CE]**. The ceiling moves daily,
so automation MUST derive the value at run time rather than carry a literal.

The generated bot usernames take the form `project_<id>_bot_<hex>` and
`group_<id>_bot_<hex>` **[live-CE]**. That matters for identity-based filtering: the
automation identity that `assignee_username` and mention searches must name is that
generated username, not the token's display name (see
[Server-side filtering](#server-side-filtering-and-query_filter)).

### Scopes

Classic access tokens carry coarse scopes. The full nine-operation surface was probed with
single-scope tokens **[live-CE]**:

| Scope | Reads (`/user`, project, issue list, notes list, token introspection) | Writes (state transition, note create, label attach) |
| --- | --- | --- |
| `read_api` | All 200 | All `403 {"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token"}` |
| `api` | All 200 | All 200 or 201 |

**Required scope: `api`.** There is no finer-grained classic scope for issue writes; GitLab
offers no equivalent of a per-resource "Issues: read and write" permission at this token
model. `read_api` is enough for a read-only deployment.

GitLab also offers *granular* personal access tokens. The GitLab.com token used here was
granular, and `GET /personal_access_tokens/self` reported `"scopes": ["granular"]` with
`"granular": true` **[live-SaaS]**: the scopes array is opaque for granular tokens
and carries no usable permission detail. A granular GitLab.com token that reads successfully
fails a note create with a bare `403 {"message":"403 Forbidden"}` carrying no
`insufficient_scope` marker **[live-SaaS]**. Whether self-managed Community Edition 19.2.1
supports granular tokens, and what its introspection reports, is **[unknown]**: both
Community Edition tokens observed here reported `"granular": false`. Creating a granular
token on a self-managed Community Edition instance and reading `/personal_access_tokens/self`
would settle it.

### Cheap token validation

```
GET /api/v4/personal_access_tokens/self
```

One request validates the credential and returns its lifecycle state. Observed shape
**[live-CE]**:

```json
{"id":3,"name":"...","revoked":false,"created_at":"...","description":"","scopes":["api"],
 "user_id":3,"last_used_at":"...","active":true,"granular":false,"expires_at":"2027-08-04"}
```

This is strictly better than a bare identity call: it reports `scopes`, `active`, `revoked`,
and `expires_at`, so the constructor can fail with an actionable diagnostic (wrong scope,
revoked, expired) instead of a generic 401. It works at `read_api` **[live-CE]** and works
for project and group access tokens, returning the token record **[live-CE]**. The
actionable-diagnostic guarantee holds for classic tokens; for a granular token the scopes
array is opaque (`["granular"]`), so no scope-based diagnostic is possible and the write
failure arrives as a bare 403 **[live-SaaS]**.

The three credential failure classes are distinguishable **[live-CE]**:

- Invalid token: `401 {"message":"401 Unauthorized"}`.
- Revoked token: `401 {"error":"invalid_token","error_description":"Token was revoked. You have to re-authorize from the user."}`.
- Valid token, insufficient scope: `403 {"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token"}`.

The constructor SHOULD run this check plus a project preflight
(`GET /projects/:id`) before the first poll, mirroring the Linear and Gitea adapters, so a
misconfiguration fails construction rather than the first fetch. The project preflight is
not optional here: a 404 on a project-scoped route cannot distinguish a mistyped project
from a revoked authorization (see [Error model](#error-model)).

### Config mapping

| Config field | Value |
| --- | --- |
| `tracker.endpoint` | Instance base URL, for example `https://gitlab.example.com`. Required for self-managed; `https://gitlab.com` for SaaS. The adapter appends `/api/v4`; it SHOULD tolerate a value already ending in `/api/v4` without double-appending |
| `tracker.api_key` | Access token, sent verbatim as `PRIVATE-TOKEN` (single string, no `email:token` composition) |
| `tracker.project` | Numeric project ID or `group/project` path; see [Identifiers](#identifiers-and-project-scoping) |

Plain-HTTP endpoints send the token in cleartext; operators SHOULD front self-managed
instances with TLS. There is no API version header: behavior is pinned by the instance
version, which `GET /api/v4/version` reports.

---

## Identifiers and project scoping

### Two issue numbers, only one usable

GitLab issues carry two integers, and the distinction is not cosmetic:

- `iid` (internal ID): **project-scoped**, human-visible, the value every per-issue route
  consumes (`/projects/:id/issues/:issue_iid`).
- `id`: an **instance-global** integer that the project-scoped routes do not accept.

Divergence measured live. Within one project, two issues created after the first six had
`(iid, id)` pairs `(7, 9)` and `(8, 10)` **[live-CE]**; in a second project on the same
instance, the first two issues were `(1, 7)` and `(2, 8)` **[live-CE]**. On GitLab.com the
gap is dramatic: a fresh project's first two issues were `(1, 196614283)` and
`(2, 196614285)` **[live-SaaS]**. Any code that treats the two as interchangeable is
correct only on the very first project of a brand-new instance, which is exactly the
configuration a naive test fixture produces.

The global `id` is not merely redundant, it is inaccessible: `GET /issues/:id` returned
`403 {"message":"403 Forbidden"}` for a project Developer and `200` only for an instance
administrator **[live-CE]**. The `iid` is the only identifier worth storing.

The adapter maps `domain.Issue.ID` and `domain.Issue.Identifier` both to the `iid` as a
string, as the GitHub and Gitea adapters map theirs to the issue number and index
respectively. `FetchIssueStatesByIDs` and `FetchIssueStatesByIdentifiers` therefore share
one implementation.

A qualified display form is available without string assembly: every issue carries
`references: {"short":"#2","relative":"#2","full":"group/project#2"}` **[live-CE]**.

### Project addressing

`tracker.project` accepts either form **[live-CE]**:

- Numeric project ID: `/projects/1/issues`.
- URL-encoded path: `/projects/group%2Fproject/issues`. The slash MUST be percent-encoded;
  an unencoded slash produces `404 {"error":"404 Not Found"}` **[live-CE]** because the
  route no longer matches.

The path form is recommended for configuration because it is stable under the operator's
mental model and readable in a workflow file, while a numeric ID is opaque. The adapter MUST
percent-encode the whole path segment exactly once. A project can be moved or
renamed, which changes the path but not the ID; a deployment that expects to survive a
rename SHOULD configure the numeric ID.

Nested subgroups are supported and produce more than one slash
(`group/subgroup/project` becomes `group%2Fsubgroup%2Fproject`), so unlike the GitHub and
Gitea adapters the value MUST NOT be validated as "exactly one slash".

---

## State model

GitLab issues natively carry only `opened` and `closed` (the list query enum adds `all`)
**[live-CE]**. There is no workflow engine and no transition graph.

**Decision: label-driven workflow states, the same convention the GitHub and Gitea adapters
use.** `tracker.active_states`, `tracker.terminal_states`, and `tracker.handoff_state` name
project or group labels; state derivation scans the issue's labels against the configured
lists and takes the first match; native `opened` with no state label maps to the first
`active_states` entry and native `closed` with no terminal label to the first
`terminal_states` entry; a terminal transition also closes the issue and an active
transition reopens it.

Three GitLab behaviors make this materially better than the equivalent on Gitea, and one
makes it materially more dangerous.

### A whole transition is one request

`PUT /projects/:id/issues/:iid` accepts `state_event`, `add_labels`, and `remove_labels`
together, and applies all three atomically. Verified: an issue in state `opened` with labels
`[REVIEW, review, workflow::other, workflow::testing]` received one request with
`state_event=close&add_labels=done&remove_labels=review,REVIEW,workflow::testing,workflow::other`
and came back `200` as `closed` with labels exactly `[done]`; the reverse request restored it
**[live-CE]**. The Gitea adapter needs up to five requests for the same outcome.

Precedence when both lists name the same label is deterministic: `add_labels=x` combined
with `remove_labels=x` leaves `x` absent, so **remove wins** **[live-CE]**.

`state_event` accepts exactly `close` and `reopen`
([`lib/api/issues.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issues.rb):
`optional :state_event, type: String, values: %w[reopen close]`) **[source]**,
corroborated by documentation [[docs]](https://docs.gitlab.com/api/issues/) and live: the
past-tense and GitHub-style spellings `closed` and `open` both return
`400 {"error":"state_event does not have a valid value"}` **[live-CE]**. Re-sending
`state_event=close` to an already-closed issue returns `200` and is a no-op, so every step
is idempotent and a partial failure converges on retry **[live-CE]**.

### Labels are created on demand

`add_labels` naming a label that does not exist returns `200` and **creates** it as a project
label: after `add_labels=zzz-does-not-exist`, the project label list contained
`zzz-does-not-exist` with `is_project_label: true` **[live-CE]**. This is documented
behavior: "If a label does not already exist, this creates a new project label and assigns it
to the issue" [[docs]](https://docs.gitlab.com/api/issues/). Two independent sources agree.

This removes the label-hygiene problem that forced the Gitea adapter into an explicit
create-on-missing policy: no name-to-id resolution, no separate create call, no fixed default
color, and no silent no-op on an unknown name.

`remove_labels` naming a label that does not exist returns `200` and changes nothing
**[live-CE]**, so removal is safely idempotent.

### The case-collision trap

Label names are **case-sensitive**, and auto-creation turns that into silent data corruption
rather than an error. Attaching `REVIEW` to an issue that already carried `review` produced
an issue with **both** labels and created a second project label `REVIEW` **[live-CE]**. The
filter side agrees: `labels=BACKLOG` returned an empty set where `labels=backlog` returned
three issues **[live-CE]**.

The damage is specific to Sortie's model. Labels normalize to lowercase in the domain issue,
so `REVIEW` and `review` both arrive as `review`: the derived state looks correct while the
project has grown a phantom label, and a `remove_labels` for one casing leaves the other
attached, so state labels accumulate. Nothing in the API reports this.

Mitigation, and the reason it is normative: **the adapter MUST resolve the canonical casing
of every configured state label against the project label list at construction, and MUST
send the canonical casing on every write.** `GET /projects/:id/labels` returns project and
group labels together, paginated **[live-CE]**. A configured name that matches no existing
label case-insensitively is the operator's intended new label and may be created by the first
`add_labels` write; a name that matches case-insensitively but not exactly MUST be rewritten
to the stored casing rather than attached as-is.

### Scoped labels do not enforce exclusivity on Community Edition

GitLab's `key::value` scoped labels are the obvious mechanism for mutually exclusive workflow
states: on Premium and above, attaching `workflow::b` removes `workflow::a` automatically.
That mechanism is **not available on Community Edition**, and it fails open rather than
loudly.

Verified: `workflow::testing` and `workflow::other` were both created as ordinary project
labels (`201`), and a single `add_labels=workflow::testing,workflow::other` request left the
issue carrying **both** **[live-CE]**. On Community Edition the `::` is just two characters
in a label name.

The adapter therefore MUST NOT rely on scoped-label exclusivity for state, and MUST remove
the previous state label explicitly via `remove_labels`. Operators MAY name their state
labels with a `::` prefix for readability, gaining automatic exclusivity as a side effect
only on Premium and above; the adapter's behavior must not change either way.

### Label scope: project and group

Labels exist at project and group level, distinguished by `is_project_label` **[live-CE]**.
A group label attaches to a project issue and is filterable exactly like a project label:
`add_labels=<group label>` succeeded and `labels=<group label>` matched the issue
**[live-CE]**. Group labels are the right home for state labels shared across several
projects in one group. `add_labels` auto-creation always creates a *project* label
[[docs]](https://docs.gitlab.com/api/issues/), so a group-level state label must be
pre-created by the operator.

### `blocked_by` is unavailable on Community Edition

The normalized issue model derives `blocked_by` from inverse relations of type `blocks`. On
Community Edition that relation type does not exist: `link_type` accepts only `relates_to`,
and `blocks` and `is_blocked_by` both return
`400 {"error":"link_type does not have a valid value"}` **[live-CE]** (see
[the three products](#why-the-distinction-has-teeth-the-same-gap-three-different-errors) for
the source proof and the GitLab.com contrast).

Positive controls confirm the route works and the negative result is real, not an empty
query: `link_type=relates_to` returned `201`, omitting `link_type` defaulted to
`relates_to`, and `GET /projects/:id/issues/:iid/links` returned the created link from both
directions **[live-CE]**.

**Consequence: `domain.Issue.BlockedBy` MUST be an empty slice on Community Edition.** The
adapter MUST NOT populate it from `relates_to` links: a "relates to" edge carries no
direction and no blocking semantics, so treating it as a blocker would suppress dispatch for
merely cross-referenced issues. Because a blocker whose state is unknown is treated as
non-terminal (conservative), inventing blockers is the more harmful error.

On an Enterprise Edition instance licensed at Premium or above, `GET .../links` entries carry
`link_type: "blocks"` or `"is_blocked_by"` **[source]** and could populate `blocked_by`.
Whether to read them conditionally is a design question this research does not settle; the
Community Edition floor requires the empty-slice behavior regardless.

---

## Operations

Route map for the nine `TrackerAdapter` methods (`internal/domain/tracker.go`). All routes
verified **[live-CE]**.

| # | Method | GitLab route(s) |
| --- | --- | --- |
| 1 | `FetchCandidateIssues` | `GET /projects/:id/issues?state=opened&issue_type=issue&scope=all&per_page=100&order_by=created_at&sort=asc` |
| 2 | `FetchIssueByID` | `GET /projects/:id/issues/:iid` + `GET .../notes?activity_filter=only_comments` |
| 3 | `FetchIssuesByStates` | `GET /projects/:id/issues?state=opened&...` and `GET /projects/:id/issues?state=closed&...` |
| 4 | `FetchIssueStatesByIDs` | `GET /projects/:id/issues?iids[]=N&iids[]=M&state=all&scope=all` (one request per batch) |
| 5 | `FetchIssueStatesByIdentifiers` | Same as 4 (ID equals Identifier equals `iid`) |
| 6 | `FetchIssueComments` | `GET /projects/:id/issues/:iid/notes?activity_filter=only_comments&sort=asc` |
| 7 | `TransitionIssue` | `GET /projects/:id/issues/:iid` + one `PUT /projects/:id/issues/:iid` |
| 8 | `CommentIssue` | `POST /projects/:id/issues/:iid/notes` |
| 9 | `AddLabel` | `PUT /projects/:id/issues/:iid` with `add_labels` |

### 1. `FetchCandidateIssues`

```
GET /projects/:id/issues?state=opened&issue_type=issue&scope=all&per_page=100&order_by=created_at&sort=asc
```

Each parameter earns its place:

- **`state=opened` is mandatory, not a default.** The list route's default state is `all`:
  with no `state` parameter the response included the closed issue, and the keyset `Link`
  header echoed the effective query as `...&state=all&...` **[live-CE]**. The documentation
  does not state the default [[docs]](https://docs.gitlab.com/api/issues/). Omitting it
  would put terminal issues in the candidate set. Note also that the value is `opened`, not
  `open`: `state=open` returns `400 {"error":"state does not have a valid value"}`
  **[live-CE]**.
- **`issue_type=issue` excludes non-issue work items server-side.** GitLab's issue list
  returns tasks, incidents, and test cases alongside issues. Verified: after creating one
  `task` and one `incident`, the unfiltered `state=opened` listing returned all seven items
  including both, while `issue_type=issue` returned exactly the five issues **[live-CE]**.
  This is the direct analogue of Gitea's `type=issues` pull-request exclusion, and omitting
  it would dispatch agents against checklist items. Accepted values are `issue`, `incident`,
  `test_case`, `task` [[docs]](https://docs.gitlab.com/api/issues/).
- **`scope=all`** avoids any dependence on the route's default scope, which the
  documentation gives as `all` [[docs]](https://docs.gitlab.com/api/issues/) but which the
  instance-wide `GET /issues` route does not appear to share (that route returned an empty
  set for a user who authored nothing **[live-CE]**). Stating it explicitly costs nothing.
- **`order_by=created_at&sort=asc`** hands the orchestrator oldest-first candidates, matching
  the GitHub adapter's server-side ordering, so no client-side re-sort is needed. `order_by`
  defaults to `created_at` and `sort` to `desc` [[docs]](https://docs.gitlab.com/api/issues/).
- **`per_page=100`** is the maximum (see [Pagination](#pagination)).

State filtering is client-side against the configured `active_states`. Pushing the configured
state labels into `labels` is not viable: the filter is AND across comma-separated names
(verified: `labels=bug,in-progress` matched the one issue carrying both, `labels=backlog,bug`
matched nothing **[live-CE]**), and Sortie needs OR across several active states. GitLab
offers no OR label filter on this route; see
[the `or[]` trap](#the-silent-parameter-trap).

Comments are set to nil on all returned issues, per the interface contract.
`tracker.query_filter`, when configured, merges into this same request.

### 2. `FetchIssueByID`

```
GET /projects/:id/issues/:iid
```

- A missing `iid` returns `404 {"message":"404 Not found"}`, mapped to `tracker_not_found`
  **[live-CE]**. `iid=0` behaves the same **[live-CE]**. A non-numeric `iid` returns
  `400 {"error":"issue_iid is invalid"}` **[live-CE]**, which the adapter should never
  provoke because it controls the value.
- The route resolves tasks, incidents, and test cases as well as issues; the response carries
  `issue_type` **[live-CE]**. The adapter SHOULD return `tracker_not_found` for a non-`issue`
  type, mirroring the GitHub and Gitea adapters' treatment of a pull request reached through
  the issue route, so a candidate that changed type cannot slip through revalidation.
- Comments come from operation 6's route. `BlockedBy` is empty (see
  [`blocked_by`](#blocked_by-is-unavailable-on-community-edition)); no links request is
  issued, saving a round trip.
- `Parent` is always nil: GitLab's parent relationship is a work-item concept not exposed as
  a parent reference on this REST entity **[live-CE]**, and no field in the observed issue
  object carries one.

### 3. `FetchIssuesByStates`

Two paginated list queries at most, both with client-side state filtering:

- Requested states intersecting `active_states`: `state=opened`.
- Requested states intersecting `terminal_states`: `state=closed`.

Verified partition: `state=opened` returned the five open issues, `state=closed` the one
closed issue, `state=all` all six **[live-CE]**.

A server-side label filter is rejected for the same two reasons as in operation 1, plus a
third: a label filter would miss closed issues carrying no terminal label, which the state
model still counts as terminal (first `terminal_states` entry).

### 4. `FetchIssueStatesByIDs` and 5. `FetchIssueStatesByIdentifiers`

GitLab provides a true batch lookup, which neither the GitHub nor the Gitea adapter has:

```
GET /projects/:id/issues?iids[]=1&iids[]=3&state=all&scope=all
```

Verified: `iids[]=1&iids[]=3` returned exactly those two issues **[live-CE]**. Reconciliation
for N running issues costs one request instead of N. `state=all` is required so a
just-closed issue is still reported.

Issues absent from the response are omitted from the returned map, satisfying the interface
contract, with no per-issue 404 to swallow.

The adapter MUST chunk large batches: the request is a query string, and the documentation
records `414 Request-URI Too Large` as a possible response
[[docs]](https://docs.gitlab.com/api/rest/). The exact `iids[]` count at which a given
deployment returns 414 is **[unknown]** and depends on the front-end web server
configuration, not on GitLab; a conservative chunk size well below any plausible URI limit
avoids the question. Both methods share one implementation because ID equals Identifier
equals `iid`.

Conditional requests are available and worth considering here, unlike on Gitea: GitLab sends
a weak `ETag` on list and notes responses, and `If-None-Match` returns `304` **[live-CE]**
(verified on both the issue list and the notes list). Whether a `304` is cheaper than a `200`
against a rate-limit budget is **[unknown]**; GitLab's rate-limit documentation does not
address conditional requests, and no `RateLimit-*` headers were observable on the
Community Edition instance because throttling is off by default there. Measuring
`RateLimit-Observed` across a `304` on GitLab.com would settle it.

### 6. `FetchIssueComments`

```
GET /projects/:id/issues/:iid/notes?activity_filter=only_comments&sort=asc&per_page=100
```

- **The notes route returns system notes mixed in with human comments.** A note carries a
  `system` boolean; an issue with two seed comments returned three notes, the third being a
  system note with body `marked as related to #2` **[live-CE]**. The documentation shows
  system notes in the example response [[docs]](https://docs.gitlab.com/api/notes/).
  Passing them to an agent as human feedback would pollute every continuation prompt.
- **`activity_filter=only_comments` excludes them server-side.** Measured on the same issue:
  no filter and `activity_filter=all_notes` both returned 5 notes (1 system, 4 user),
  `only_comments` returned 4 (0 system), `only_activity` returned 1 (1 system)
  **[live-CE]**. The parameter is declared in source as
  `optional :activity_filter, type: String, values: UserPreference::NOTES_FILTERS...,
  default: 'all_notes'`
  ([`lib/api/notes.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/notes.rb))
  **[source]** and documented with values `all_notes`, `only_comments`, `only_activity`
  [[docs]](https://docs.gitlab.com/api/notes/). Three independent sources agree. An invalid
  value returns `400 {"error":"activity_filter does not have a valid value"}` **[live-CE]**,
  so the parameter cannot be silently dropped by a version that lacks it: it either works
  or fails loudly. The adapter SHOULD nonetheless keep a client-side `system` guard, since
  the field is authoritative and the check is free.
- **`sort=asc` is required for chronological order.** The default is `sort=desc`,
  newest-first, in source
  ([`lib/api/notes.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/notes.rb):
  `optional :sort, type: String, values: %w[asc desc], default: 'desc'`) **[source]**, in
  documentation [[docs]](https://docs.gitlab.com/api/notes/), and live **[live-CE]**.
  Requesting `sort=asc` server-side avoids the client-side re-sort the Linear adapter needs.
- The route **is paginated**, unlike Gitea's comments route. An issue with 55 notes returned
  20 with `X-Total: 55`, `X-Total-Pages: 3`, `X-Next-Page: 2` and a `rel="next"` link at the
  default page size, and all 55 in one page at `per_page=100` **[live-CE]**. The adapter MUST
  paginate.
- Bodies are Markdown and pass through unflattened, like GitHub and Gitea and unlike Jira's
  ADF **[live-CE]**.
- Notes carry an `internal` boolean (renamed from `confidential` in an earlier release)
  **[live-CE]** **[source]**. Internal notes are visible only to project members at
  Reporter level and above; they are returned to a token that can see them. The adapter
  passes them through: they are genuine human comments, and an operator who does not want
  them in prompts controls that by not writing them.
- `404` maps to `tracker_not_found` **[live-CE]**. Returns an empty non-nil slice when the
  issue has no comments.
- The issue object's `user_notes_count` counts non-system notes only (55 on the bulk issue,
  and 2 on an issue whose notes route returned 3) **[live-CE]**, so it is a reliable
  "are there comments" hint without a second request.

### 7. `TransitionIssue`

Two requests, because one is a read for the current state:

1. `GET /projects/:id/issues/:iid`: current labels and native state. A non-`issue`
   `issue_type` fails with `tracker_not_found`.
2. One `PUT /projects/:id/issues/:iid` carrying, as needed, `state_event` (`close` for a
   terminal target on an open issue, `reopen` for an active target on a closed issue),
   `add_labels` with the canonical casing of the target state label, and `remove_labels` with
   the canonical casing of the current state label.

A target that is not a configured active, terminal, or handoff state fails with
`tracker_payload_error` before any write, mirroring the GitHub and Gitea adapters.

The step count is the operational headline: the whole transition is atomic in the sense that
the server applies label changes and the state change in one transaction, so there is no
window in which the issue carries a terminal label while still open. Verified in both
directions **[live-CE]**; see [State model](#a-whole-transition-is-one-request).

The adapter MUST NOT use the `labels` parameter, which **replaces** the entire label set:
`labels=bug` on an issue carrying three labels left it with one **[live-CE]**. Only
`add_labels` and `remove_labels` are additive and subtractive respectively.

### 8. `CommentIssue`

```
POST /projects/:id/issues/:iid/notes
body=<markdown text>
```

Returns `201` with the created note; the text posts verbatim as Markdown with newlines
preserved, and `system` is `false` **[live-CE]**. No format conversion.

Two validation failures worth naming **[live-CE]**:

- Omitted `body`: `400 {"error":"body is missing"}`.
- Empty `body`: `400 {"message":"400 Bad request - Note {:note=>[\"can't be blank\"]}"}`.
  The message embeds a Ruby hash rendering; it is a string, and the adapter should treat it
  as an opaque diagnostic snippet rather than parse it.

Note creation is separately rate-limited on both products; see
[Rate limiting](#rate-limiting).

### 9. `AddLabel`

```
PUT /projects/:id/issues/:iid
add_labels=<label>
```

Used for CI failure escalation, so the label must actually appear. One request suffices, and
the parameter is **additive**: `add_labels=bug` on an issue already carrying three labels
returned all four **[live-CE]**. A label that does not exist is created (see
[State model](#labels-are-created-on-demand)), so the escalation label needs no
pre-provisioning and there is no silent no-op to guard against.

The case-collision rule applies here too: the adapter MUST send the canonical casing when the
label already exists in a different case, or the project grows a duplicate.

GitLab performs no concurrency control on issue updates: two simultaneous `PUT`s carrying
opposing `add_labels` and `remove_labels` both returned 200, with no 409 and no conflict
signal, and their label deltas interleaved nondeterministically **[live-CE]**. Two concurrent
writers against one project silently corrupt each other's state.

Label errors remain non-fatal to the orchestrator.

---

## Server-side filtering and `query_filter`

### The silent parameter trap

This is the single most important safety finding for operator-supplied filters.

**GitLab silently ignores unrecognized query parameters.** Its API layer declares each
parameter explicitly and discards anything undeclared, so a misspelled filter key does not
error: it disables the filter. Measured against a positive control **[live-CE]**:

| Query | Result |
| --- | --- |
| `labels=backlog` (control) | 3 of 6 issues |
| `labelz=backlog` | **all 6** |
| `label=backlog` | **all 6** |
| `assignee=<user>` | **all 6** |
| `totally_bogus_param=xyz` | **all 6** |

Two consequences differ in severity. A typo in a *narrowing* filter widens the candidate set,
dispatching agents against issues the operator meant to exclude. A typo in an *excluding*
filter likewise stops excluding. Neither is visible in any response.

Invalid *values* on *recognized* keys behave in the opposite, safe way: they return `400`.
`state=open`, `state_event=closed`, and `activity_filter=bogus` each returned
`400 {"error":"<param> does not have a valid value"}` **[live-CE]**. So the danger is
confined to key names.

The trap also catches parameters that exist in GitLab's *interface* but not in its REST
surface. `or[label_name][]` is a plausible-looking OR label filter and is inert:
`or[label_name][]=backlog` returned all 6 issues where `labels=backlog` returned 3, and
`or[label_name][]=zzz-none` also returned all 6 **[live-CE]**. It behaves identically on
GitLab.com, where `or[label_name][]=<existing label>` returned both issues in a project where
only one carried the label **[live-SaaS]**. This is not tier gating: the parameter is not
declared on the REST route at all
([`lib/api/issues.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issues.rb))
**[source]** and is absent from the documented parameter list
[[docs]](https://docs.gitlab.com/api/issues/). Three sources agree it does not exist; the
live behavior shows how it fails.

**Therefore the adapter MUST validate every `query_filter` key against an allowlist of
parameters it knows the route declares, and MUST reject an unknown key as a configuration
error at construction.** Passing operator input through unvalidated converts a typo into a
silent scope change. This is stricter than the Gitea adapter's rule, which reserves only the
four keys it owns, and the stricter rule is required because GitLab's failure is silent in
both directions.

### Verified filter parameters

`GET /projects/:id/issues`, all rows exercised **[live-CE]**:

| Parameter | Verified semantics |
| --- | --- |
| `state` | `opened`, `closed`, `all`. **Default `all`** |
| `issue_type` | `issue`, `incident`, `test_case`, `task`. Filters non-issue work items out server-side |
| `labels` | Comma-separated names, **AND** across names. Case-**sensitive**. An unresolvable name returns an **empty set**, not a dropped filter: `labels=BACKLOG` and `labels=no-such-label` both returned nothing while `labels=backlog` returned 3. Honest, unlike Gitea's equivalent |
| `labels=None` / `labels=Any` | Wildcards: `None` returned nothing (every issue was labelled), `Any` returned all 6 |
| `not[labels]` | Negation works: `not[labels]=backlog` returned the 3 issues without it. Also accepts the array form `not[labels][]` |
| `not[author_username]` | Works: returned nothing when every issue shared one author |
| `assignee_username` | Single value works. **Two values return `400 {"error":"assignee_username allows one value, but found 2: ...}`** on Community Edition; see [the compatibility verdict](#surface-the-adapter-must-avoid-with-the-negative-control-for-each) |
| `assignee_id` | Numeric user ID; also accepts `None` and `Any` wildcards, both verified |
| `author_username` | Works |
| `scope` | `all`, `assigned_to_me`, `created_by_me`. `assigned_to_me` correctly returned only the issue assigned to the calling identity |
| `search` | Free-text match; `in=title` and `in=title,description` scope it |
| `iids[]` | Repeatable, returns exactly the named `iid`s. Backs the batch state lookup |
| `updated_after` | ISO-8601 window; a future timestamp returned nothing (negative control), the current day returned all 6 (positive control) |
| `milestone` | Accepts a title, plus `None` and `Any` |
| `confidential` | Boolean |
| `my_reaction_emoji` | Accepts `Any` |
| `order_by`, `sort` | `order_by` values `created_at`, `due_date`, `label_priority`, `milestone_due`, `popularity`, `priority`, `relative_position`, `title`, `updated_at` **[source]**; `sort` is `asc` or `desc` |
| `with_labels_details` | `true` replaces the label name array with label objects (`id`, `name`, `description`, `color`, `text_color`, `archived`) |
| `page`, `per_page`, `pagination` | See [Pagination](#pagination) |

### The motivating scenario: scoping to the automation identity

On a shared instance, candidates should be limited to issues assigned to or mentioning the
automation identity. The two halves have very different quality.

**Assigned: a first-class filter.** `assignee_username=<automation identity>` and
`assignee_id=<id>` each returned exactly the assigned issue, and `scope=assigned_to_me`
returned the same issue without naming the identity **[live-CE]**. `scope=assigned_to_me` is
preferable in configuration because it needs no username: it resolves against the token's own
identity, which for a project or group access token is a generated bot username the operator
would otherwise have to look up.

**Mentioned: text search only.** GitLab's issue list route has **no mention filter**: no
`mentioned_by` equivalent exists in the documented parameter set
[[docs]](https://docs.gitlab.com/api/issues/) or in the route's declared parameters
([`lib/api/issues.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issues.rb))
**[source]**. The available approximation is a text search:
`search=@<automation identity>` returned the issue whose description read
`cc @<automation identity> please pick this up.`, and so did
`search=<automation identity>&in=title,description` **[live-CE]**.

The approximation's limits must be stated, because it looks like a mention filter and is not:

- It is a substring match over title and description, not a mention index. It matches a
  username appearing in prose, inside a code block, or as part of a longer name.
- It does not see mentions in **comments**, only in the issue body.
- Search is rate-limited separately and much more tightly than the general API: a documented
  10 requests per minute per IP on GitLab.com
  [[docs]](https://docs.gitlab.com/user/gitlab_com/#rate-limits-on-gitlabcom), and
  `search_rate_limit = 30` per minute on the Community Edition instance's own settings
  **[live-CE]**. This applies to the dedicated search API; whether the `search` *parameter*
  on the issue-list route draws on the same budget is **[unknown]** and would be settled by
  observing `RateLimit-Name` on a throttled response.

Recommendation: treat "assigned to the automation identity" as the supported scoping
mechanism and "mentions" as a best-effort text filter the operator opts into, with the
substring caveat documented in the operator-facing reference.

### `query_filter` mapping

`tracker.query_filter` is adapter-defined (Jira: a JQL fragment; Linear: an `IssueFilter`
JSON object; Gitea: a URL query fragment). For GitLab the recommendation is a **URL query
fragment**, matching Gitea:

```yaml
tracker:
  kind: gitlab
  query_filter: "scope=assigned_to_me&not[labels]=needs-triage"
```

The adapter parses it with `url.ParseQuery`, then:

- **Rejects unknown keys** as a configuration error, against the allowlist above. This is the
  mitigation for [the silent parameter trap](#the-silent-parameter-trap) and is the one place
  this adapter must be stricter than its siblings.
- **Rejects the keys it owns** (`state`, `issue_type`, `page`, `per_page`, `pagination`,
  `order_by`, `sort`), because an operator override of these changes correctness rather than
  scope.
- Merges the rest into the candidate query and into the `state=opened` half of
  `FetchIssuesByStates`, never into the `state=closed` half, so an operator filter cannot
  hide a terminal issue from reconciliation.
- SHOULD warn when a `query_filter` `labels` value does not resolve case-sensitively against
  the project label list, since GitLab's honest-but-empty result looks the same as a
  correctly-empty one.

### Cross-project routes

Two other routes return issues and are **not** used by the adapter:

- `GET /groups/:id/issues` returns issues across every project in a group, by numeric ID or
  by path, filtered to what the token may see **[live-CE]**. Relevant only if a workflow ever
  spans a group; `tracker.project` names one project.
- `GET /issues` (instance-wide) and the search API `GET /search?scope=issues` and
  `GET /projects/:id/search?scope=issues` all work **[live-CE]**. The search API adds
  nothing the issue-list route lacks and carries the tighter search rate limit.

---

## Field mapping

`domain.Issue` field to GitLab issue response path. The observed Community Edition issue
object carried exactly these keys **[live-CE]**: `_links`, `assignee`, `assignees`, `author`,
`closed_at`, `closed_by`, `confidential`, `created_at`, `description`, `discussion_locked`,
`downvotes`, `due_date`, `has_tasks`, `id`, `iid`, `imported`, `imported_from`, `issue_type`,
`labels`, `merge_requests_count`, `milestone`, `moved_to_id`, `project_id`, `references`,
`service_desk_reply_to`, `severity`, `start_date`, `state`, `subscribed`,
`task_completion_status`, `task_status`, `time_stats`, `title`, `type`, `updated_at`,
`upvotes`, `user_notes_count`, `web_url`.

| `domain.Issue` field | GitLab field | Notes |
| --- | --- | --- |
| `ID` | `iid` | As a string. The global `id` is never used |
| `Identifier` | `iid` | Same value as `ID`. `references.full` supplies a qualified `group/project#N` form when a display string is wanted |
| `Title` | `title` | |
| `Description` | `description` | Markdown; null on an issue created without one **[live-CE]** |
| `Priority` | none | GitLab issues carry **no priority field** (absent from the key list above) **[live-CE]**. Always nil. GitLab expresses priority through *label* priority, which the list route can order by (`order_by=priority`, `order_by=label_priority`) but which is a project-level label ordering, not a per-issue integer |
| `State` | label-derived | See [State model](#state-model). Native `state` is `opened` or `closed` |
| `BranchName` | none | GitLab issues carry no branch reference field **[live-CE]**. Always nil |
| `URL` | `web_url` | Observed pointing at the work-item path (`.../-/work_items/1`) at this version rather than `.../-/issues/1` **[live-CE]**. Stored opaque; both forms resolve |
| `Labels` | `labels[]` | A plain string array by default; lowercased on normalization. See the [case-collision trap](#the-case-collision-trap) for why lowercasing is lossy here |
| `Assignee` | `assignees[0].username` | GitLab populates both a deprecated singular `assignee` and an `assignees` array **[live-CE]**; use the array, empty when absent. Multiple assignees are a tier-gated feature, so the array holds at most one entry on Community Edition |
| `IssueType` | `issue_type` | Lowercase (`issue`, `incident`, `task`, `test_case`) **[live-CE]**. A parallel `type` field carries the uppercase form (`ISSUE`, `TASK`); use `issue_type` |
| `Parent` | none | Always nil (see operation 2) |
| `Comments` | separate route | Markdown, no flattening, system notes excluded |
| `BlockedBy` | none on Community Edition | Always an empty slice; see [`blocked_by`](#blocked_by-is-unavailable-on-community-edition) |
| `CreatedAt` | `created_at` | ISO-8601 with zone offset, for example `2026-08-04T11:51:58.172+02:00` **[live-CE]**. GitLab.com returned UTC `Z` form **[live-SaaS]**; both parse |
| `UpdatedAt` | `updated_at` | Same |

Comment mapping: `id`, `author.username`, `body` (Markdown), `created_at`, `updated_at`,
`system`, `internal`, all verified present **[live-CE]**.

The Community Edition and GitLab.com key lists differ by exactly one field: GitLab.com adds
`blocking_issues_count` **[live-SaaS]**, an Enterprise Edition entity field. Neither product
returned a `weight` field, GitLab.com included, because the namespace is on the Free plan
**[live-SaaS]**. The adapter reads no field outside the Community Edition list.

---

## Pagination

GitLab offers two modes and the adapter needs no special handling to support either, because
both advertise the next page in an RFC 8288 `Link` header with an absolute URL that preserves
every query parameter **[live-CE]**. `httpkit.NewLinkPaginator` follows `rel="next"` via
`httpkit.ParseLinkRel` and requests the full URL, which is exactly the required behavior.

### Offset pagination (default)

| Property | Value |
| --- | --- |
| Parameters | `page` (1-based), `per_page` |
| `per_page` default | **20**, measured as `X-Per-Page: 20` with no parameter **[live-CE]**; documented as 20 [[docs]](https://docs.gitlab.com/api/rest/) |
| `per_page` maximum | **100**. `per_page=200` returned `X-Per-Page: 100` **[live-CE]**; documented maximum 100 [[docs]](https://docs.gitlab.com/api/rest/) |
| Headers | `X-Total`, `X-Total-Pages`, `X-Page`, `X-Per-Page`, `X-Next-Page`, `X-Prev-Page`, plus `Link` with `rel="first"`, `"prev"`, `"next"`, `"last"` **[live-CE]** **[docs]** |

The adapter sends `per_page=100`, double the architecture's page-size default of 50, because
100 is the server maximum and halves the round trips for the same result. `per_page=0` is a
degenerate case that returned a single item and no pagination headers **[live-CE]**; the
adapter never sends it.

**Above 10,000 records GitLab omits `X-Total`, `X-Total-Pages`, and the `rel="last"` link**
[[docs]](https://docs.gitlab.com/api/rest/). This is **[docs]** only and was not verified
live: the fixture had six issues, and manufacturing 10,001 was out of proportion to the
finding. It is nonetheless the reason the adapter MUST drive pagination from `rel="next"`
rather than counting up to `X-Total-Pages`: the counting approach silently truncates on
exactly the large projects where it matters. `rel="next"` is unaffected by the omission.

### Keyset pagination

`pagination=keyset` with mandatory `order_by` and `sort`
[[docs]](https://docs.gitlab.com/api/rest/) is supported on project issues. Verified: the
response carried a `Link` with `rel="next"` whose URL embedded an opaque `cursor=` parameter
and preserved every other parameter, and **no** `X-Total`, `X-Total-Pages`, or `X-Per-Page`
headers **[live-CE]**. All of `order_by=created_at`, `updated_at`, `title`,
`relative_position`, and `priority` were accepted in keyset mode **[live-CE]**.

Keyset mode is the correct choice for a large, mutating collection because offset pagination
can skip or repeat rows when the underlying set changes between pages. This research does not
recommend switching to it as the default: the candidate query is filtered to open issues in a
single project and ordered by creation time, and offset pagination's `X-Total` is useful
diagnostically. It is recorded as the available remedy if a deployment shows pagination drift.

`tracker_missing_end_cursor` does not apply. GitLab exposes no cursor the adapter must carry
itself: an absent `rel="next"` is the normal end-of-results signal in both modes, and in
keyset mode the cursor is embedded in a URL the paginator follows opaquely.

One caveat worth stating because it affects proxied self-managed deployments: the `Link`
URLs observed were absolute and matched the requested host **[live-CE]**, but whether GitLab
builds them from the incoming request or from the instance's configured external URL is
**[unknown]**. On a deployment where the two differ, following an absolute `Link` URL could
address the wrong host. Issuing one paginated request through a proxy whose forwarded host
differs from the instance's external URL and inspecting the `Link` header would settle it.

---

## Rate limiting

The cost model differs sharply between the two products, and the self-managed default is the
opposite of what the SaaS behavior would suggest.

### Self-managed Community Edition

**General API throttling is disabled by default.** The instance reported
`throttle_authenticated_api_enabled: false` **[live-CE]**, and the documentation states that
authenticated API request limits are among those "disabled by default"
[[docs]](https://docs.gitlab.com/administration/settings/user_and_ip_rate_limits/). When an
operator enables it, the default budget is 7,200 requests per 3,600 seconds per user
(`throttle_authenticated_api_requests_per_period: 7200`,
`throttle_authenticated_api_period_in_seconds: 3600` **[live-CE]**, matching the documented
defaults **[docs]**). Consistent with throttling being off, **no `RateLimit-*` headers
appeared on any Community Edition response** **[live-CE]**.

Several *granular* limits are enabled by default regardless, and two touch this adapter
directly **[live-CE]**:

| Setting | Default | Effect on the adapter |
| --- | --- | --- |
| `notes_create_limit` | 300 per minute | Throttles `CommentIssue` |
| `issues_create_limit` | 300 per minute | Not used by the adapter |
| `search_rate_limit` | 30 per minute | Throttles the search API |
| `project_api_limit` | 400 | Project reads, including the construction preflight |

`rate_limiting_response_text` was `null` **[live-CE]**, meaning the stock response body
applies; an operator may set arbitrary text there, so **a 429 body from a self-managed
instance is not guaranteed to be JSON.** The adapter MUST NOT require a parseable body to
classify a 429.

### GitLab.com

Throttling is on and observable. Response headers **[live-SaaS]**:

```
ratelimit-limit: 2000
ratelimit-name: throttle_authenticated_api
ratelimit-observed: 1
ratelimit-remaining: 1999
ratelimit-reset: <unix timestamp>
```

Documented limits [[docs]](https://docs.gitlab.com/user/gitlab_com/#rate-limits-on-gitlabcom):
2,000 authenticated API requests per minute, 500 unauthenticated per minute per IP, and two
per-endpoint limits that matter here (**60 note creations per minute** and 200 issue
creations per minute), plus 10 search requests per minute per IP. The live `ratelimit-limit:
2000` corroborates the general figure.

Note the divergence: GitLab.com allows 60 note creations per minute where the self-managed
default allows 300. **The stricter product is the hosted one**, so a deployment tuned against
a self-managed instance can hit the comment limit on GitLab.com.

### What the adapter must do

- Read the documented header set `RateLimit-Limit`, `RateLimit-Name`, `RateLimit-Observed`,
  `RateLimit-Remaining`, `RateLimit-Reset`, and on a 429 also `RateLimit-ResetTime` and
  `Retry-After` [[docs]](https://docs.gitlab.com/administration/settings/user_and_ip_rate_limits/).
  These are the IETF-style `RateLimit-*` names, not GitHub's `X-RateLimit-*`; the GitHub
  adapter's header names do not transfer.
- Treat all of them as optional. They are absent on a stock self-managed instance
  **[live-CE]** and documented as absent from some GitLab.com responses [[docs]].
- Map 429 to `tracker_api_error` and honor `Retry-After`, without requiring a JSON body.
- Apply no preemptive throttling on self-managed, where poll cadence is the only pressure
  control, exactly as the Gitea notes conclude. `RateLimit-Remaining`, when present, is the
  signal for a SaaS deployment.

---

## Error model

### Four envelope shapes

GitLab does not have one error envelope. Four distinct shapes were observed **[live-CE]**,
and an adapter that reads only `message` will log empty diagnostics for half of them:

| Shape | Origin | Example |
| --- | --- | --- |
| `{"message": "<string>"}` | Application-level errors | `{"message":"404 Project Not Found"}` |
| `{"error": "<string>"}` | Parameter validation and unmatched routes | `{"error":"state does not have a valid value"}`, `{"error":"404 Not Found"}` |
| `{"error": "...", "error_description": "..."}` | Token authorization, OAuth-style | `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token"}` |
| `{"message": "<string with embedded model errors>"}` | Model validation surfaced through a message | `{"message":"400 Bad request - Note {:note=>[\"can't be blank\"]}"}` |

**The adapter MUST read `message` and `error`, preferring whichever is present, and MUST
append `error_description` when present.** `message` is documented as possibly being an
object rather than a string for some validation failures, so a decoder MUST tolerate a
non-string `message` without failing the whole response.

A useful secondary signal: `{"error":"404 Not Found"}` (title case, `error` key) marks a
**route** that did not match, while `{"message":"404 Not found"}` (sentence case, `message`
key) marks an application resource that does not exist. Both were observed repeatedly and the
distinction was corroborated structurally: an unencoded project path produced the former,
a missing issue `iid` the latter, and the merge-request `approvals` route (present in
Community Edition source) produced the latter while `approval_state` (present only under
`ee/`) produced the former **[live-CE]** **[source]**. The adapter MAY use it for a better
diagnostic; it MUST NOT depend on it for control flow, since it is a byproduct of the
framework rather than a documented contract.

### The 404 ambiguity, and why the preflight is mandatory

A 404 on a project-scoped route has three causes that the response cannot distinguish, and
one of them is an authorization failure **[live-CE]**:

| Cause | Response |
| --- | --- |
| Project does not exist | `404 {"message":"404 Project Not Found"}` |
| Project exists, the token's identity is not a member | `404 {"message":"404 Project Not Found"}`, byte-identical |
| No token at all, private project | `404 {"message":"404 Project Not Found"}` |

This is deliberate: GitLab masks the existence of private resources rather than returning
403, so an unauthorized caller cannot enumerate them. The security property is sound and the
adapter cannot defeat it. The mitigations:

- **Distinguish project-level from issue-level 404s by message.** `404 Project Not Found`
  versus `404 Not found` separates "cannot see the project" from "the issue is gone"
  **[live-CE]**. Only the latter is the `tracker_not_found` the interface contract means for
  a missing issue.
- **Run the construction-time project preflight** (see
  [Authentication](#cheap-token-validation)), so a wrong project or an unauthorized token
  fails at startup with a message naming both possibilities, instead of producing a
  permanent stream of not-found results at poll time.

Note the asymmetry with an invalid credential: a bad *token* returns `401`, while a valid
token lacking *access* returns `404` **[live-CE]**. A 404 therefore cannot be ruled out as an
authorization problem.

### Status-to-category mapping

Per the error-handling contract in
[architecture Section 11.4](architecture/11-issue-tracker-integration-contract.md#114-error-handling-contract)
and the `domain.TrackerErrorKind` values:

| HTTP status | Condition | Category |
| --- | --- | --- |
| 200, 201, 202, 204 | Success | none |
| 304 | Conditional request, unchanged | none; treat as "no new data" |
| 400 | Parameter validation, model validation, **or a parameter enum that lacks an Enterprise Edition value** | `tracker_payload_error` |
| 401 | Missing, invalid, revoked, or expired token | `tracker_auth_error` |
| 403 | Insufficient token scope; **or an Enterprise Edition feature gated by license**; or a route requiring administrator | `tracker_auth_error` for the scope case. A license-gated 403 MUST NOT be reported as a credential problem: its body names the feature (`Blocked issues not available for current license` **[live-SaaS]**), and the adapter avoids these routes on the Community Edition floor |
| 404 | Missing issue, missing project, **or unauthorized project** | `tracker_not_found` on issue-scoped reads, keyed by message as above; a project-level 404 is a configuration error at construction |
| 409 | Conflict | `tracker_api_error` **[unknown]** for the issue surface: no 409 was provoked on any of the nine operations' routes |
| 414 | Request URI too large, from an over-long `iids[]` batch | `tracker_payload_error`; the adapter prevents it by chunking. **[docs]**, not observed |
| 422 | Unprocessable entity | `tracker_payload_error` **[unknown]** for the issue surface: validation failures arrived as 400 here, not 422 |
| 429 | Rate limited. Body may be operator-defined text, not JSON | `tracker_api_error`, honor `Retry-After` |
| 5xx | Server error | `tracker_transport_error` |
| network | DNS, TCP, TLS failure | `tracker_transport_error` |
| decode | JSON decode failure on a 2xx | `tracker_payload_error` |

The categories the adapter never needs: `tracker_missing_end_cursor` has no analogue (see
[Pagination](#pagination)).

**The dangerous "errors" are the 200s.** Two failure modes return success with the wrong
result and no status to key on: an unrecognized query parameter silently disabling a filter
(see [the silent parameter trap](#the-silent-parameter-trap)), and a case-variant label
attach silently creating a duplicate (see
[the case-collision trap](#the-case-collision-trap)). Neither is caught by status mapping;
both are prevented by the adapter's own validation, which is why both mitigations are
normative.

---

## Community Edition, Enterprise Edition, and GitLab.com compatibility verdict

**Verdict: every one of the nine `TrackerAdapter` operations is fully available on
self-managed Community Edition 19.2.1, with one degradation in the normalized issue model
and no degradation in behavior.** The single gap is `blocked_by`, which is structurally
unavailable and normalizes to an empty slice.

### Surface the adapter uses

| Capability | Community Edition | GitLab.com Free | Enterprise Edition Premium+ |
| --- | --- | --- | --- |
| Issue list, single issue, `iids[]` batch | Yes **[live-CE]** | Yes **[live-SaaS]** | Yes |
| Notes read and create, `activity_filter` | Yes **[live-CE]** | Yes **[docs]** | Yes |
| `state_event` close and reopen | Yes **[live-CE]** | Yes **[docs]** | Yes |
| `add_labels` / `remove_labels`, with auto-create | Yes **[live-CE]** | Yes **[live-SaaS]** | Yes |
| Project, group, and personal access tokens | Yes **[live-CE]** | Yes **[live-SaaS]** | Yes |
| `scope=assigned_to_me`, `assignee_username` (single) | Yes **[live-CE]** | Yes **[live-SaaS]** | Yes |
| `not[...]` negation filters | Yes **[live-CE]** | Yes **[docs]** | Yes |
| Offset and keyset pagination, `ETag` / 304 | Yes **[live-CE]** | Yes **[docs]** | Yes |

### Surface the adapter must avoid, with the negative control for each

| Feature | Community Edition evidence | GitLab.com Free evidence | Source proof |
| --- | --- | --- | --- |
| `link_type=blocks` / `is_blocked_by` (and therefore `blocked_by`) | `400 {"error":"link_type does not have a valid value"}` **[live-CE]** | `403 {"message":"Blocked issues not available for current license"}` **[live-SaaS]** | `available_link_types` returns `[TYPE_RELATES_TO]` in Community Edition, extended under `ee/` **[source]** |
| Issue `weight` (write) | `400` naming the **complete** accepted parameter set, which excludes `weight` **[live-CE]** | `200`, parameter accepted **[live-SaaS]** | `update_params_at_least_one_of` in Community Edition versus the `ee/` override adding `weight, epic_id, epic_iid` **[source]** |
| Issue `weight` (filter) | Silently ignored: `weight=3` returned all issues **[live-CE]** | n/a | Not a Community Edition parameter **[source]** |
| `epic_id`, `epic_iid` | Absent from the accepted parameter set **[live-CE]** | Accepted **[live-SaaS]** | Same `ee/` override **[source]** |
| Multiple assignees | `400 {"error":"assignee_username allows one value, but found 2: ...}` **[live-CE]** | `200`, two values accepted **[live-SaaS]** | `CheckAssigneesCount` allows `size <= 1` and is `prepend_mod_with`-extended in `ee/` **[source]** |
| Scoped-label mutual exclusion | Both `workflow::a` and `workflow::b` coexist on one issue **[live-CE]** | n/a | Premium feature **[docs]** |
| `iteration_id`, `iteration_title`, `health_status` | Not probed | Not probed | Declared only in the `ee/` parameter blocks **[source]**; Premium and Ultimate respectively **[docs]** |
| `blocking_issues_count` issue field | Absent from the issue object **[live-CE]** | Present **[live-SaaS]** | Enterprise Edition entity field |
| Merge-request `approval_state` | `404 {"error":"404 Not Found"}`, route unmatched **[live-CE]** | Not probed | Defined only in `ee/lib/ee/api/merge_request_approvals.rb` **[source]** |

The `weight` write probe deserves a note because it produced an unusually strong proof. `PUT`
with only `weight=3` returned
`400 {"error":"assignee_id, assignee_ids, confidential, created_at, description,
discussion_locked, due_date, start_date, labels, add_labels, remove_labels, milestone_id,
milestone, severity, state_event, title, issue_type are missing, at least one parameter must
be provided"}` **[live-CE]**, the server enumerating its entire accepted parameter set,
because `weight` was not among them and the request therefore carried zero recognized
parameters. The identical request on GitLab.com produced the same list **plus `weight`,
`epic_id`, `epic_iid`** **[live-SaaS]**. Those two lists match Community Edition
`update_params_at_least_one_of` and its `ee/` override exactly **[source]**. Three sources,
one conclusion, and a precise boundary rather than an inference.

### A documentation conflict to record

The issues API documentation presents `assignee_username` as an array parameter without a
tier annotation, and a summary of that page reports it as "available as string array for all
tiers" [[docs]](https://docs.gitlab.com/api/issues/). Community Edition rejects two values
with `400 {"error":"assignee_username allows one value, but found 2: ...}` **[live-CE]**,
while GitLab.com accepts two **[live-SaaS]**. Source explains it: the parameter is declared
`type: Array[String], check_assignees_count: true`, and the validator's `param_allowed?`
returns `params[attr_name].size <= 1` and is `prepend_mod_with`-extended in the Enterprise
tree
([`lib/api/validations/validators/check_assignees_count.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/validations/validators/check_assignees_count.rb))
**[source]**.

**The adapter must trust the live Community Edition behavior: exactly one
`assignee_username` value.** The documentation is accurate about the parameter's *type* and
silent about the tier restriction on its *cardinality*, which reads as permission it does not
grant.

---

## Integration test strategy

Two options were evaluated, mirroring the Gitea assessment.

1. **Containerized GitLab Community Edition service in CI.** The job starts a pinned image,
   provisions a token, project, labels, and issues, runs the gated tests, and discards the
   container.
2. **A GitLab.com test project with a repository-secret token**, the pattern the Jira and
   Linear nightly jobs use against SaaS APIs.

**Recommendation: a containerized GitLab Community Edition service, but scheduled rather
than on every push, with a GitLab.com job as a thin compatibility supplement.** The
recommendation is weaker than the equivalent Gitea recommendation, and the reason is measured
cost, not preference.

### Measured cost of the container

All figures measured on 2026-08-04 for `gitlab/gitlab-ce:19.2.1-ce.0`, the tag matching the
researched version:

| Metric | Value | Method |
| --- | --- | --- |
| Compressed image size (registry) | 1,382,863,597 bytes (1.29 GiB) | Docker Hub registry API |
| Uncompressed image size (on disk) | 3,555,125,274 bytes (3.31 GiB) | Local image inspection after pull |
| Pull time | 63 s | Measured once, warm network, cold local image cache |
| Container start to first HTTP response from the API | **120 s** | Measured, reproduced on two independent boots |
| Container start to Docker healthcheck reporting `healthy` | **60 s** | Same measurements |

The boot profile is a three-stage sequence, identical across both measured boots: no TCP
connection at all for the first ~90 s (the web front end is not yet listening), then
`502 Bad Gateway` at ~100 s (front end listening, application not yet up), then a real
application response at 120 s. The first boot of the same image and configuration reached
`502` at 104 s and a real response by 124 s, so 120 s is representative rather than a lucky
run. Figures are for a reduced-worker configuration on a single host and will vary with CI
runner size.

That is roughly an order of magnitude heavier than a Gitea container in bytes and several
times slower to become usable. Three consequences shape the recommendation:

- **The weight argues against per-push execution.** A 1.29 GiB pull plus a two-minute boot on
  every push is a poor trade for an adapter whose logic is exercised by unit tests against
  recorded fixtures. A scheduled job (the nightly adapter matrix) pays the cost once per day.
- **The container's own health signal is not a readiness signal.** The Docker healthcheck
  reported `healthy` at 60 s, a full minute before the API returned any HTTP response at all,
  and while the front end was still answering `502`. A CI job that waits on container health
  and then runs tests will fail against a booting instance. **The wait condition MUST be an
  HTTP poll of the API, never `docker` health.**
- **The health endpoints are not usable as an external gate.** `/-/readiness`, `/-/liveness`,
  and `/-/health` are restricted to an IP allowlist. The image ships the default
  `gitlab_rails['monitoring_whitelist'] = ['127.0.0.0/8', '::1/128']`, read from the
  configuration template inside the image **[live, measured]**. Measured on a fully booted
  instance: from inside the container `/-/readiness` returned
  `200 {"status":"ok","master_check":[{"status":"ok"}]}`, while the identical request from the
  host through the published port returned `404 {"error":"Not Found","status":404}`, because
  the source address is the container network gateway, not a loopback address. A CI job
  polling `/-/readiness` from the runner would therefore wait forever on an instance that is
  already serving.

  **The correct external gate is `GET /api/v4/version` returning any status other than a
  gateway error.** It returns `401 {"message":"401 Unauthorized"}` before provisioning
  **[live, measured]**, and a 401 is a positive signal: it proves the application is routing
  and authenticating. Readiness is a positive set: the first HTTP 200 or 401 response from
  this route is the precise condition **[live-CE]**, not the absence of the two transient
  statuses observed during boot, because a negative set misreads any unlisted transient as
  readiness. The poll MUST also abort as soon as the container is no longer running rather
  than wait out its budget, because a boot failure exits the container within 8 s
  **[live-CE]**. A job that must use a health endpoint has to run the poll *inside* the
  container so the request originates from loopback.

### Why the container still wins on correctness

Everything the Gitea notes argue applies here and more forcefully, because GitLab's
Community-Edition-versus-Enterprise-Edition split is the adapter's central compatibility
risk:

- **Only a self-managed Community Edition instance tests the compatibility floor.** GitLab.com
  is an Enterprise Edition codebase; a test suite green against GitLab.com proves nothing
  about Community Edition, and this research found four places where the two differ in
  observable behavior (`link_type`, `weight`, `epic_id`, `assignee_username` cardinality). A
  GitLab.com-only strategy would have silently accepted every one of them. All four are
  unobservable through `domain.TrackerAdapter` as implemented: `normalizeIssue` sets
  `BlockedBy` empty unconditionally and the adapter creates no issue link, so `link_type`
  behavior is unreachable; `gitlabIssue` decodes neither `weight` nor `epic_id` and
  `gitlabIssueUpdate` sends neither; `assignee_username` reaches the wire only through an
  operator-written `query_filter` **[live-CE]**. What a GitLab.com job buys is forward-version
  drift detection against a continuously deployed instance, not product-divergence coverage.
  The compatibility-floor argument for the container is unaffected.
- No repository secrets: the token is generated inside the job.
- No shared mutable state between runs, and no cross-fork secret exposure.
- Exact version pinning to the researched release, and local reproducibility.

### Recommended shape

- Image: `gitlab/gitlab-ce:<version>-ce.0`, pinned. The Community Edition image is the
  correct choice precisely because it is *not* Enterprise Edition.
- Reduce boot cost with a `GITLAB_OMNIBUS_CONFIG` limited to the verified-minimal key set:
  `external_url`, `puma['worker_processes']`, `sidekiq['concurrency']`,
  `prometheus_monitoring['enable']`, `registry['enable']`, and `gitlab_kas['enable']`; the
  instance only serves API calls. `grafana['enable']` is **not** a supported key at 19.2.1:
  `gitlab-ctl reconfigure` fails with `Mixlib::Config::UnknownConfigOptionError: Reading
  unsupported config value grafana` and the container exits within 8 s **[live-CE]**. This
  research booted with exactly the verified-minimal key set.
- Provisioning: a token cannot be minted from credentials over HTTP: `POST /oauth/token` with
  `grant_type=password` returns `400 {"error":"unsupported_grant_type"}` **[live-CE]**. The
  bootstrap token instead comes from one Rails runner invocation inside the container,
  measured at 24 s, and no initial root password is needed; the project, labels, and issues
  are then created through the API. Every provisioning call this research made is a plain
  REST request, so the fixture is fully scriptable.
- Gate: `SORTIE_GITLAB_TEST=1`, with `SORTIE_GITLAB_ENDPOINT`, `SORTIE_GITLAB_TOKEN`, and
  `SORTIE_GITLAB_PROJECT` supplied by the job, following the sibling adapters' single-gate
  convention and their variable naming. Without the gate the tests MUST skip cleanly, never
  fail.
- Supplement: a GitLab.com job behind the same gate pattern, a compatibility canary rather
  than the primary suite. Keeping it alive costs one access token with issue write, an
  endpoint, and a project coordinate held as repository configuration; a hosted project
  seeded with the state labels, the non-state label the label test attaches, at least four
  open candidates, one closed terminal issue, and two ordered comments; a token expiry
  bounded at one year **[live-SaaS]**, so the row fails on expiry; and a shared mutable
  fixture whose open-candidate count is the maintenance obligation.

This recommendation is implemented as `scripts/gitlab-integration-provision.sh` plus the two
GitLab nightly matrix rows.

### Fixture blueprint

The verification lab doubles as the fixture blueprint, and two of its properties are
load-bearing rather than incidental:

- **Two projects, not one.** A single-project instance makes `iid` and global `id` numerically
  identical for every issue, so a test suite cannot catch code that confuses them. The second
  project's first issue had `iid: 1` and global `id: 7` **[live-CE]**, which is the assertion
  that pins the distinction. The divergence requires seeding the sibling project's issues
  before the primary project's: with two issues seeded in the sibling project first, the
  primary project's first issue carried `iid: 1` and global `id: 3` **[live-CE]**. The second
  project also supplies the negative control for authorization masking: a token that is a
  member of the first project only receives `404 {"message":"404 Project Not Found"}` for the
  second **[live-CE]**.
- **A non-administrator identity.** An administrator token cannot observe the authorization
  failures a real deployment hits. Every probe reported here was run with a project
  Developer token; the administrator token was used only for provisioning.
- Labels covering the state model plus one **group-level** label, to assert that a group label
  is readable and attachable on a project issue **[live-CE]**.
- Issues covering: open with a state label, open with two labels, closed with a terminal
  label, one **assigned** to the automation identity, one **mentioning** it in the
  description, one carrying comments and an issue link. A bulk-comment issue (at least 101
  notes, verified to produce `X-Total: 101`, `X-Total-Pages: 2`, `X-Next-Page: 2`, and a
  `rel="next"` link, with exactly 100 entries on page one, because the adapter sends
  `per_page=100` **[live-CE]**) to assert note pagination, and one non-`issue` work item to
  assert the `issue_type=issue` exclusion.

---

## Out of scope: gap map for merge requests and pipelines

Recorded for planning only; nothing below is part of the tracker adapter. Route existence was
confirmed **[live-CE]** by reaching each route (a `404` on a resource that does not exist
proves the route matched; see [Error model](#four-envelope-shapes) for how that is
distinguished from an unmatched route). Response shapes are not characterized here.

| Future capability | GitLab surface | Community Edition status |
| --- | --- | --- |
| Merge-request list and detail | `GET /projects/:id/merge_requests`, `.../merge_requests/:iid` | Available **[live-CE]** |
| Review decision | `GET .../merge_requests/:iid/approvals` | Route available **[live-CE]** **[source]**. The richer `approval_state` and all approval-*rule* routes are Enterprise Edition only: `approval_state` returned `404 {"error":"404 Not Found"}` (route unmatched) and is defined only under `ee/` **[live-CE]** **[source]**. A Community Edition review decision must be folded from `approvals` plus the merge-request object, as the Gitea adapter folds one from its review list |
| Mergeability | The merge-request object's merge-status fields | Available **[live-CE]**; field semantics not characterized |
| CI status | `GET /projects/:id/pipelines`, plus commit status routes | Available **[live-CE]** |
| Review threads and comments | `GET .../merge_requests/:iid/notes`, `.../discussions` | Available **[live-CE]**. Same note entity as issues, so the comment normalization is shared |
| Label-event journal for label commands | `GET .../merge_requests/:iid/resource_label_events` | Available **[live-CE]**. Shape verified on the issue variant: each event carries `id`, `action` (`add` or `remove`), `label.name`, `user.username`, `created_at`, `resource_type` **[live-CE]** |
| Branch delete after merge | `DELETE /projects/:id/repository/branches/:branch` | Route family available **[live-CE]** |
| Protected-branch awareness | `GET /projects/:id/protected_branches` | Available **[live-CE]** |
| Webhooks | `POST /projects/:id/hooks` | Not exercised |

Two findings from this research that a future forge half will need:

- **State-change and label-change events are not notes.** GitLab keeps them in separate
  journals: `resource_label_events` and `resource_state_events`, both available on Community
  Edition and both verified to return typed entries **[live-CE]**. The label-command reaction
  reads the label journal, not the note stream.
- **Bot identity is a platform marker here.** Project and group access token identities report
  `bot: true`, and a human-owned personal access token identity reports `bot: false`
  **[live-CE]**. Unlike Gitea, bot classification need not rest on a username allowlist alone.

---

## Config notes

- **`tracker.endpoint`:** required for self-managed, no default host; `https://gitlab.com`
  for SaaS. The adapter appends `/api/v4`. TLS strongly recommended: the token travels in a
  header.
- **`tracker.api_key`:** access token string, sent as `PRIVATE-TOKEN`. Carries GitLab's
  fixed access-token prefix and was 51 characters long at the researched version, so a
  shape hint is possible in configuration validation. Scope `api` for the
  full surface, `read_api` for a read-only deployment.
- **`tracker.project`:** numeric project ID or `group/project` path, percent-encoded once by
  the adapter. MUST NOT be validated as exactly one slash: subgroups produce more.
- **`tracker.active_states` / `tracker.terminal_states` / `tracker.handoff_state`:** project
  or group label names. The adapter lowercases them for comparison but MUST send the label's
  canonical stored casing on writes (see
  [the case-collision trap](#the-case-collision-trap)).
- **`tracker.query_filter`:** URL query fragment, keys validated against an allowlist and
  rejected when unknown; see
  [`query_filter` mapping](#query_filter-mapping).
- **Page size:** `per_page=100`, the server maximum, above the architecture's default of 50
  (per [architecture Section 11.2](architecture/11-issue-tracker-integration-contract.md#112-query-semantics)).
- **Network timeout:** 30,000 ms per the same section.
- **Headers:** `PRIVATE-TOKEN` on every request; `User-Agent: Sortie/<version>`;
  `Content-Type: application/json` on writes. No API version header exists.
- **Construction preflight:** `GET /personal_access_tokens/self` (credential, scope, and
  expiry check), `GET /projects/:id` (project check, and the only way to separate a wrong
  project from an unauthorized token), and `GET /projects/:id/labels` (canonical casing of the
  configured state labels). All three failures are configuration errors surfaced at startup.

---

## Key differences from the Gitea and GitHub adapters

| Aspect | GitHub | Gitea 1.27 | GitLab 19.2 Community Edition |
| --- | --- | --- | --- |
| Default host | `https://api.github.com` | None | None for self-managed; `https://gitlab.com` for SaaS |
| Auth header | `Authorization: Bearer` | `Authorization: token` | `PRIVATE-TOKEN` (Bearer also accepted) |
| Token shape | Prefixed | 40 hex, no prefix | Prefixed, 51 chars |
| Least-privilege token | Fine-grained PAT, per-repository | Scopes only | **Project access token**, server-enforced single-project |
| Permission model | Per-resource permissions | `read:` / `write:` scopes | Coarse `api` / `read_api` only |
| Token introspection | n/a | Scopes visible only in a 403 body | `GET /personal_access_tokens/self`: scopes, active, revoked, expiry |
| Issue identifier | Repo-scoped number | Repo-scoped index | Project-scoped `iid`; global `id` exists and is inaccessible |
| Project scoping | `owner/repo`, one slash | `owner/repo`, one slash | Numeric ID or percent-encoded path, **any number of slashes** |
| Non-issue items in the list | Pull requests, excluded by a key check | Pull requests, excluded by `type=issues` | Tasks, incidents, test cases, excluded by `issue_type=issue` |
| Default list state | Open | Open | **All**; must be set explicitly |
| `labels` list filter | AND, case-insensitive | AND, case-sensitive, unresolvable name **drops the filter** | AND, case-sensitive, unresolvable name returns **empty** (honest) |
| Unknown query parameter | Error | Ignored | **Silently ignored**, the primary `query_filter` hazard |
| Unknown label on attach | 422, visible | HTTP 200, silently ignored | **HTTP 200, label created** |
| Label case handling | Case-insensitive | Case-sensitive | Case-sensitive **and** auto-creating, so a case variant creates a duplicate |
| Transition cost | Several requests | Up to five requests | **One request** (`state_event` + `add_labels` + `remove_labels`) |
| Batch state lookup | Per-issue or search | Per-issue loop | **One request** via `iids[]` |
| Comments route | Paginated | Unpaginated | Paginated, **and system notes must be filtered** (`activity_filter=only_comments`) |
| Comment default order | n/a | Oldest-first | **Newest-first**; `sort=asc` required |
| Conditional requests | ETag / 304, free of rate cost | No ETag | ETag / 304 present; rate-cost effect unknown |
| Page-size maximum | 100 | 50 (`limit`) | 100 (`per_page`) |
| Pagination modes | Offset via `Link` | Offset via `Link` | Offset **and** keyset, both via `Link` |
| Rate limits | 5,000/hr core | None built-in | **Off by default self-managed**; 2,000/min on GitLab.com, with a stricter 60/min note-creation limit |
| Rate-limit headers | `X-RateLimit-*` | None | `RateLimit-*` (IETF style), absent when throttling is off |
| `blocked_by` | Dependencies endpoint | Dependencies endpoint | **Unavailable on Community Edition**; always empty |
| Priority field | None | None | None (label priority is a project-level ordering, not a per-issue integer) |
| Error envelope | Varied | Uniform `{message, url}` | **Four shapes**; `message` and `error` both required |
| Unauthorized resource | 404 | 404 | 404, byte-identical to a missing project |
| GraphQL | Available, used for review decisions | Not available | Available, **not needed** for the tracker |

---

## Live verification results

Established against self-managed GitLab Community Edition 19.2.1 (revision `f4d029d2da8`,
`enterprise: false`) on 2026-08-04, using a **project Developer** token for every probe whose
result is reported, and an administrator token only for provisioning and cleanup. GitLab.com
(`19.3.0-pre`, revision `7725732be39`, `enterprise: true`, namespace plan `free`) was used
only for the comparisons marked **[live-SaaS]**.

1. **Edition and tier.** `GET /version` reported `enterprise: false` on the self-managed
   instance and `enterprise: true` on GitLab.com; `GET /namespaces` reported `plan: free` for
   the GitLab.com namespace. Both `/version` and `/metadata` returned 401 without a token and
   200 with a `read_api` token.
2. **Auth scheme matrix.** `PRIVATE-TOKEN`, `Authorization: Bearer`, and
   `?private_token=` all returned 200; `JOB-TOKEN` with a personal access token returned 401.
   A bad token returned `401 {"message":"401 Unauthorized"}`; a revoked token returned
   `401 {"error":"invalid_token","error_description":"Token was revoked. ..."}`; no token at
   all against a private project returned `404 {"message":"404 Project Not Found"}`.
3. **Scope sufficiency.** A `read_api`-only token performed all four probed reads and was
   refused all three writes with
   `403 {"error":"insufficient_scope",...}`; an `api` token performed everything.
4. **Token types.** Personal, project, and group access tokens all authenticated. The project
   token's identity was `project_<id>_bot_<hex>` with `bot: true` and could not see a sibling
   project (404); the group token's identity was `group_<id>_bot_<hex>` with `bot: true` and
   could see both projects in the group and `GET /groups/:id/issues`. A human-owned personal
   access token reported `bot: false`. `GET /personal_access_tokens/self` returned the token
   record for all of them.
5. **Identifier divergence.** Two issues created in the first project had `(iid, id)` of
   `(7, 9)` and `(8, 10)`; the second project's first two issues were `(1, 7)` and `(2, 8)`.
   On GitLab.com a new project's first two issues were `(1, 196614283)` and `(2, 196614285)`.
   `GET /issues/:id` returned 403 for the Developer token and 200 for the administrator.
6. **Project addressing.** A percent-encoded `group%2Fproject` path resolved; an unencoded
   slash returned `404 {"error":"404 Not Found"}`.
7. **Blocking links.** `link_type=blocks` and `is_blocked_by` returned
   `400 {"error":"link_type does not have a valid value"}`; `relates_to` returned 201; an
   omitted `link_type` defaulted to `relates_to`; the created link was visible from both
   endpoints of the relation. On GitLab.com the same two values returned
   `403 {"message":"Blocked issues not available for current license"}`.
8. **Tier boundaries.** A `PUT` carrying only `weight=3` returned a 400 enumerating the
   entire accepted parameter set, which excluded `weight`; the same request on GitLab.com
   returned a list additionally containing `weight`, `epic_id`, and `epic_iid`, and the write
   itself returned 200. `weight=3` as a *filter* was silently ignored. Two
   `assignee_username` values returned `400 {"error":"assignee_username allows one value, but
   found 2: ...}` on Community Edition and 200 on GitLab.com. The Community Edition and
   GitLab.com issue objects differed by exactly one key, `blocking_issues_count`.
9. **Scoped labels.** `workflow::testing` and `workflow::other` were both created as ordinary
   project labels and coexisted on one issue after a single `add_labels` carrying both.
10. **Silent parameter ignore.** `labels=backlog` returned 3 of 6 issues, while `labelz=`,
    `label=`, `assignee=`, `totally_bogus_param=`, `or[label_name][]=` (with an existing label
    and with a nonexistent one), and `weight=` each returned all 6. On GitLab.com
    `or[label_name][]=<existing label>` likewise returned every issue. By contrast
    `state=open`, `state_event=closed`, and `activity_filter=bogus` each returned
    `400 {"error":"<param> does not have a valid value"}`.
11. **Filter semantics.** `labels=bug,in-progress` matched only the issue carrying both;
    `labels=backlog,bug` matched nothing; `labels=BACKLOG` and `labels=no-such-label` returned
    empty sets; `labels=Any` returned all, `labels=None` none; `not[labels]=backlog` returned
    the complement; `assignee_username`, `assignee_id`, `assignee_id=None|Any`,
    `author_username`, `scope=assigned_to_me`, `search`, `search&in=`, `iids[]`, and
    `updated_after` (future timestamp empty, current day full) all behaved as documented.
12. **Work item exclusion.** After creating one `task` and one `incident`, the unfiltered
    `state=opened` list returned seven items including both; `issue_type=issue` returned
    exactly the five issues; `issue_type=task` returned only the task.
13. **State and label writes.** `state_event=close` then `reopen` toggled the native state and
    were idempotent on repetition; `closed` and `open` returned 400. `add_labels` was additive
    and created a nonexistent label as a project label; `remove_labels` tolerated a
    nonexistent name; `add_labels=REVIEW` on an issue carrying `review` produced both labels
    and a second project label; `labels=` replaced the entire set. A single `PUT` carrying
    `state_event`, `add_labels`, and `remove_labels` applied all three, and `add` plus
    `remove` of the same name resolved in favor of remove.
14. **Group labels.** A group-level label (`is_project_label: false`) attached to a project
    issue and matched a `labels=` filter.
15. **Notes.** The route returned system notes mixed with user comments;
    `activity_filter=only_comments` and `only_activity` partitioned them exactly;
    `sort=asc` reordered them oldest-first against a `desc` default; a 55-note issue paginated
    at 20 per page with `X-Total: 55` and a `rel="next"` link, and returned all 55 at
    `per_page=100`; `user_notes_count` counted non-system notes only; creating a note returned
    201 with the Markdown body and newlines preserved; an omitted body returned
    `400 {"error":"body is missing"}` and an empty body a 400 embedding a model-validation
    rendering.
16. **Resource event journals.** `resource_label_events` returned typed `add` and `remove`
    entries naming the label, actor, and timestamp; `resource_state_events` returned the close
    event. Label changes appeared in the label journal and **not** in the note stream.
17. **Pagination.** `per_page` defaulted to 20 and clamped 200 to 100; offset responses carried
    `X-Total`, `X-Total-Pages`, `X-Page`, `X-Per-Page`, `X-Next-Page` and a `Link` header;
    `pagination=keyset` returned a `rel="next"` link embedding an opaque `cursor` and omitted
    the `X-Total` family; five `order_by` values were accepted in keyset mode; the keyset link
    echoed the effective query, which is how the `state=all` default was established.
18. **Conditional requests.** A weak `ETag` was present on the issue-list and notes responses,
    and `If-None-Match` returned 304 for both.
19. **Rate limiting.** No `RateLimit-*` headers appeared on any Community Edition response, and
    the instance reported `throttle_authenticated_api_enabled: false` with a 7,200-per-3,600s
    default, alongside enabled granular limits including `notes_create_limit: 300` and
    `search_rate_limit: 30`, and a null `rate_limiting_response_text`. GitLab.com returned
    `ratelimit-limit: 2000`, `ratelimit-name: throttle_authenticated_api`,
    `ratelimit-observed`, `ratelimit-remaining`, and `ratelimit-reset`.
20. **Errors.** Four envelope shapes were observed. A missing issue returned
    `404 {"message":"404 Not found"}`; a missing project, an unauthorized project, and an
    unauthenticated request to a private project all returned the byte-identical
    `404 {"message":"404 Project Not Found"}`; an unmatched route returned
    `404 {"error":"404 Not Found"}`; a non-numeric `iid` returned
    `400 {"error":"issue_iid is invalid"}`.
21. **Gap-map route existence.** Merge-request list, merge-request notes and discussions,
    merge-request `approvals`, pipelines, commits, branches, protected branches,
    `related_merge_requests`, and both resource-event journals all matched their routes;
    merge-request `approval_state` did not.
22. **Container cost and boot profile.** The `gitlab/gitlab-ce:19.2.1-ce.0` image is
    1,382,863,597 bytes compressed and 3,555,125,274 bytes on disk, and pulled in 63 s. Two
    independent boots of the same image and configuration produced the same three-stage
    profile: no TCP connection for roughly the first 90 s, `502` from the web front end at
    100 to 114 s, and a real application response by 120 to 124 s. The Docker healthcheck
    reported `healthy` at 60 s, before the application answered at all. On a fully booted
    instance, `/-/readiness` returned `200` from inside the container and
    `404 {"error":"Not Found","status":404}` from the host through the published port,
    matching the shipped default `gitlab_rails['monitoring_whitelist'] = ['127.0.0.0/8',
    '::1/128']` read from the image's own configuration template, while
    `GET /api/v4/version` returned `401` from both.

All fixture mutations made during this research were reverted: probe-created issues, notes,
issue links, and labels were deleted, altered label sets and issue states were restored, and
every token created for scope probing was revoked.

---

## Source attribution

| Topic | Primary source | Verification method |
| --- | --- | --- |
| Edition detection (`enterprise` flag) | [Version API](https://docs.gitlab.com/api/version/) | Live on both instances |
| Authentication schemes, token header | [REST API](https://docs.gitlab.com/api/rest/) | Live matrix of four schemes |
| Token types, scopes, introspection | Live API | Single-scope tokens created and probed; project and group tokens created and probed |
| Issue list parameters and tier annotations | [Issues API](https://docs.gitlab.com/api/issues/) | Live probe of every parameter reported |
| `state_event` enum | [`lib/api/issues.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issues.rb) | Docs plus live rejection of invalid values |
| Community Edition update-parameter set, and its Enterprise extension | [`lib/api/helpers/issues_helpers.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/helpers/issues_helpers.rb) and [`ee/lib/ee/api/helpers/issues_helpers.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/ee/lib/ee/api/helpers/issues_helpers.rb) | Server-enumerated parameter list in a 400 body on both products |
| `link_type` enum and its Enterprise extension | [`lib/api/issue_links.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issue_links.rb), [`app/models/concerns/issuable_link.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/issuable_link.rb), [`ee/app/models/concerns/ee/issuable_link.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/ee/app/models/concerns/ee/issuable_link.rb) | Live 400 on Community Edition, live 403 on GitLab.com |
| Assignee-count restriction | [`lib/api/validations/validators/check_assignees_count.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/validations/validators/check_assignees_count.rb) | Live 400 on Community Edition, live 200 on GitLab.com |
| Absence of `or[]` on the REST route | [`lib/api/issues.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/issues.rb) and the documented parameter list | Live: filter inert on both products, against a positive control |
| Notes parameters, `activity_filter`, sort default | [Notes API](https://docs.gitlab.com/api/notes/) and [`lib/api/notes.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/notes.rb) | Live partition of system and user notes |
| Label auto-creation on `add_labels` | [Issues API](https://docs.gitlab.com/api/issues/) | Live: label created and returned in the project label list |
| Pagination parameters, headers, keyset, 10,000-record header omission | [REST API](https://docs.gitlab.com/api/rest/) | Live for every item except the 10,000-record omission, which is documentation only |
| Rate-limit headers and self-managed defaults | [User and IP rate limits](https://docs.gitlab.com/administration/settings/user_and_ip_rate_limits/) | Live: instance settings readout, and header presence on GitLab.com versus absence on Community Edition |
| GitLab.com rate limits | [GitLab.com settings](https://docs.gitlab.com/user/gitlab_com/#rate-limits-on-gitlabcom) | Live `ratelimit-limit: 2000` |
| Merge-request approval Community/Enterprise split | [`lib/api/merge_request_approvals.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/merge_request_approvals.rb) and [`ee/lib/ee/api/merge_request_approvals.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/ee/lib/ee/api/merge_request_approvals.rb) | Live: `approvals` route matched, `approval_state` did not |
| Declared authentication mechanisms, and the incompleteness of the machine-readable route surface | [`doc/api/openapi/openapi_v2.yaml`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/doc/api/openapi/openapi_v2.yaml) and the [interactive rendering](https://docs.gitlab.com/api/openapi/openapi_interactive/) | Inspected for declared paths and parameters; live 404 on every instance-served description path |
| Container image size and boot behavior | Docker Hub registry API; local image inspection | Measured pull, boot, healthcheck, and readiness polling |
| Gitea and GitHub adapter behavior used in the comparison tables | The Gitea and GitHub adapter research notes in this directory, and `internal/domain/tracker.go` | Document and code reading at research time |

### Open questions

Carried forward explicitly rather than resolved by inference:

| Question | Why it matters | Evidence that would settle it |
| --- | --- | --- |
| Does a `304` response consume rate-limit budget? | Decides whether ETag caching is worth wiring for reconciliation | Compare `RateLimit-Observed` across a `304` and a `200` on GitLab.com |
| Are `Link` header URLs built from the request or from the instance's configured external URL? | A proxied self-managed deployment could be sent to the wrong host by the paginator | One paginated request through a proxy whose forwarded host differs from the external URL |
| Does the `search` *parameter* on the issue list draw on the search rate-limit budget, or the general API budget? | Decides whether mention-scoping is cheap or tightly capped | `RateLimit-Name` on a throttled response to a `search=`-bearing issue-list request |
| Does self-managed Community Edition 19.2.1 support granular access tokens, and what does introspection report? | Decides whether scope preflight can be trusted on self-managed | Create a granular token on a self-managed Community Edition instance and read `/personal_access_tokens/self` |
| At what `iids[]` count does a deployment return `414`? | Sets the batch chunk size for reconciliation | Increase `iids[]` count against a representative front-end web server configuration |
| Do 409 or 422 statuses occur anywhere on the nine operations' routes? | Two rows of the error mapping are unexercised | Provoke a conflict or unprocessable-entity condition on the issue routes |
| Does self-managed Enterprise Edition behave as its source implies? | Every Enterprise Edition claim here is source plus documentation, never observed | Run the same probe set against a licensed self-managed Enterprise Edition instance |
