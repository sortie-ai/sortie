# GitLab REST API: Adapter research notes

> GitLab REST API v4, researched August 2026 and pinned to **self-managed GitLab Community
> Edition 19.2.1** (revision `f4d029d2da8`, `enterprise: false`), **verified live** on
> 2026-08-04. Community Edition is the compatibility floor: the adapter depends only on what
> it provides. A second live instance, **GitLab.com** (version `19.3.0-pre`, revision
> `7725732be39`, `enterprise: true`, namespace subscription plan `free`), was used solely for
> the self-managed-versus-SaaS comparison. A second live pass on 2026-08-05 re-verified the
> pinned container image `gitlab/gitlab-ce:19.2.1-ce.0` (version `19.2.1`, revision
> `f4d029d2da8`, matching the self-managed lab) and GitLab.com, whose revision had moved to
> `6f59ad3485c` as a continuously deployed instance does. A third live pass on 2026-08-10
> characterized the merge-request, approval, review, pipeline, and job surface against the same
> self-managed Community Edition 19.2.1 instance (revision `f4d029d2da8`), using a project at
> namespace depth three and a registered runner executing real jobs. A fourth live pass on
> 2026-08-16 characterized the `manual` pipeline shape against the same instance and revision,
> using a purpose-built scratch project and its own registered runner. Reference for
> implementing the GitLab `TrackerAdapter` and `SCMAdapter`.
>
> Self-managed GitLab has no fixed host, so the instance base URL is part of every
> self-managed configuration, and instances differ in version and settings. Facts hold for
> 19.2.1 Community Edition defaults unless marked otherwise. GitLab also exposes a GraphQL
> API; the tracker surface needs none of it (see [GraphQL](#graphql)).

## Provenance of every claim

Every statement about GitLab below carries one of five tags. Nothing about the platform is
asserted without one. Claims about Sortie's own code carry a Go symbol or package path
instead of a tag: the tags record what was observed of GitLab, and reading this repository
is not an observation of GitLab.

| Tag | Meaning |
| --- | --- |
| **[live-CE]** | Observed directly against the self-managed Community Edition 19.2.1 instance on 2026-08-04, again on 2026-08-05 against the pinned container image `gitlab/gitlab-ce:19.2.1-ce.0` (version `19.2.1`, revision `f4d029d2da8`, matching the lab), again on 2026-08-10 for the merge-request, approval, pipeline, and job surface, and again on 2026-08-16 for the `manual` pipeline shape. The strongest evidence for Community Edition behavior. On 2026-08-10 and 2026-08-16 the tag also covers the instance's **own GraphQL introspection response**, which is version-exact for the deployment under test and is named as such wherever it is the source. |
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

The forge surface does not need GraphQL either: the merge-request, approval, review-state,
pipeline, and job routes the `SCMAdapter` consumes are all REST, and each was exercised there
(see [Merge requests, approvals, and pipelines](#merge-requests-approvals-and-pipelines)).
GraphQL is used in these notes only as a research instrument, to read an instance's own
version-exact enumerations.

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

Helper sharing follows the Linear and Gitea precedent, and the shared homes cover decisions as
well as transport: `internal/httpkit` for the HTTP client, transport-error classification
(`httpkit.ClassifyTransport`), the `Link`-header paginator (`httpkit.NewLinkPaginator`,
`httpkit.ParseLinkRel`), the page-number paginator (`httpkit.NewPagePaginator`), and preflight
retry (`httpkit.RetryWithBackoff`); `internal/issuekit` for shared normalization and
label-state derivation (`issuekit.DeriveLabelState`, `issuekit.CurrentLabelState`) plus the
default state lists; `internal/registry` for the offline validate primitives
(`registry.DiagStateLabelElements`, `registry.DiagStateOverlap`); `internal/scm/scmcore` for
the decisions every forge half shares; `internal/adaptertest` for the conformance assertions
an adapter suite calls; `internal/trackermetrics` for instrumentation; and `internal/typeutil`
for config decoding.

No shared base structs with the GitHub or Gitea adapters: GitLab's REST v4 is not
GitHub-compatible, so the wire overlap is transport-shaped and lives in `httpkit`. The overlap
that is not transport-shaped is the set of decisions taken over normalized domain types, and
that lives in `scmcore`; see
[decisions the adapter does not make](#decisions-the-adapter-does-not-make).

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

The derivation itself is not GitLab code. `issuekit.DeriveLabelState` owns the scan order, the
first-match rule, the fallback to the first configured entry, and the WARN it logs under the
`issue_identifier` attribute when an issue carries more than one configured state label;
`issuekit.CurrentLabelState` answers the same question with no native fallback, which is what
the transition needs. GitLab supplies only its own spelling of the two native statuses, which
`normalizeIssue` passes as `opened` and `closed` alongside the configured lists, and the
defaults come from `issuekit.DefaultActiveLabelStates` and
`issuekit.DefaultTerminalLabelStates`. The adapter writes the wire decode, not the rule.

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

Every row also records the originating HTTP status in `domain.TrackerError.Status`, which
`classifyHTTPError` (`internal/scm/gitlab/client.go`) sets on each classified response. A
status with no dedicated arm, 405 among them, reaches the default arm and still carries its
own value there. That field is what the forge half's merge-conflict promotion keys on, so the
forge half inherits it without a second classification path (see
[the merge call](#6-merge-call)).

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

## Merge requests, approvals, and pipelines

This section characterizes the surface the GitLab `SCMAdapter` half implements. Each
capability records the response shape, the mapping onto the `internal/domain` SCM type, and the
decision taken. Nothing here is part of the tracker adapter.

The fixture is a project at namespace depth three (`scmlab/team/squad/mrlab`), five merge
requests covering the clean, conflicting, draft, and merged cases, a registered runner
executing real jobs, and a project access token identity for the bot probes. Every probe
below was run against it. Reads use the project Developer token unless a probe is explicitly
about the administrator view.

Three findings dominate the design and are stated once here because several capabilities
depend on them:

- **`detailed_merge_status` is a single value that recomputes.** Approving a merge request
  flipped it from `mergeable` to `checking` and back **[live-CE]**. Any read can land on a
  transient computing value, so no single read is authoritative.
- **The merge endpoint's rejection body does not name its reason.** Already-merged, draft, and
  conflicting merge requests all return the byte-identical `405 {"message":"405 Method Not
  Allowed"}` **[live-CE]**. See [the merge call](#6-merge-call) for what that forces.
- **Embedded user objects carry no bot marker.** The `bot` field exists on `GET /users/:id` but
  not on the author of a note **[live-CE]** **[source]**, which reopens a claim the earlier
  gap map got wrong.

### Decisions the adapter does not make

Several decisions this section once left to the GitLab author arrive from
`internal/scm/scmcore`, which every forge half depends on and which imports only
`internal/domain`. The adapter supplies the wire read and the normalization; the verdict is
computed once, shared:

| Decision | Shared owner | What GitLab supplies |
| --- | --- | --- |
| Which check-run conclusions count as failing | `scmcore.IsFailingConclusion` | The GitLab job or status value to normalize |
| The aggregate CI verdict, and the failing count | `scmcore.AggregateCIStatus`, `scmcore.FailingCount` | The normalized `domain.CheckRun` set |
| The merge-gate CI answer | `scmcore.MergeGate` | The same normalized set |
| Tracker error to `domain.SCMError` conversion | `scmcore.ToSCMError` | Nothing; the tracker client's classification feeds it |
| Promotion of a merge rejection to `ErrSCMConflict` | `scmcore.AsMergeConflict` | The status on the originating `domain.TrackerError` |
| The already-merged marker phrase | `scmcore.AlreadyMergedConflict` | The re-read that confirms the merge landed |
| Bot-author classification | `scmcore.IsBotAuthor` | The platform bot flag and the configured allowlist |
| Label-event id ordering | `scmcore.SortableEventID` | The numeric journal id |

Two consequences change behavior this section previously described as the adapter's own:

- **`ErrSCMConflict` is reachable only from the merge write path.** `scmcore.ToSCMError`
  performs no promotion, and `scmcore.AsMergeConflict` is called only by `MergePR`. A 405 or
  409 from `RemoveLabel`, `DeleteBranch`, or any read keeps the kind `scmcore.ToSCMError`
  assigned. The promotion reads `domain.TrackerError.Status` directly rather than matching
  message text, so no route-shaped or prefix-shaped exemption is needed.
- **The already-merged marker is constructed, not matched.** It is emitted only after a
  re-read of the merge request confirms the merge landed, never from the provider's response
  body. See [the merge call](#6-merge-call), where this is the contract rather than a
  workaround for it.

`internal/adaptertest` holds the conformance assertions an adapter suite calls rather than
restates: `AssertSCMErrorKind`, `AssertAlreadyMergedMarker`, `AssertBranchAbsentDisposition`,
`AssertLabelAbsentDisposition`, `AssertCIAggregateMatchesCore`, and
`AssertLabelEventsOrdered`. Each pins one clause of the contract, and
`AssertCIAggregateMatchesCore` recomputes the aggregate and the failing count from the
returned `CheckRuns`, so a provider that sources either from anywhere else fails the
assertion.

### The merge-request object

`GET /projects/:id/merge_requests/:iid` returns 58 keys on Community Edition **[live-CE]**. The
fields the SCM contract consumes:

| Field | Observed values | Feeds |
| --- | --- | --- |
| `iid` | Project-scoped integer | The `prNumber` argument on every method |
| `state` | `opened`, `merged`, `closed`, `locked` | `PRMergeStatus.Merged` (`state == "merged"`) |
| `draft` | Boolean, duplicated by the deprecated `work_in_progress` | `PRMergeStatus.Draft` |
| `sha` | Head commit of the source branch | `PRMergeStatus.HeadSHA`, and the merge precondition |
| `source_branch` / `target_branch` | Branch names | `PRMergeStatus.BranchName` / `.BaseBranch` |
| `merge_commit_sha` | Null until merged, then the merge commit | `PRMergeStatus.MergeCommitSHA` |
| `merge_status` | `checking`, `can_be_merged`, `cannot_be_merged` | Nothing: superseded by `detailed_merge_status` |
| `detailed_merge_status` | 24-value enum, see [mergeability](#2-mergeability) | `PRMergeStatus.Mergeability` |
| `has_conflicts` | Boolean | Corroborates `conflict`, not used alone |
| `head_pipeline` | Full pipeline object, or null | [CI status](#4-ci-status-source) |
| `reviewers` | Array of bare user objects, **no review state** | Nothing, see [changes-requested reviews](#3-changes-requested-reviews) |

Community Edition carries **no** `approvals_before_merge` key on the merge-request object
**[live-CE]**.

A merged merge request reports `state: "merged"`, a non-null `merged_at` and `merge_commit_sha`,
and `detailed_merge_status: "not_open"` **[live-CE]**. That combination is the only reliable
already-merged signal the adapter has.

### 1. Review decision

**The Community Edition approvals payload is smaller than the documentation implies.** The
route is available on all tiers, but what it returns on Community Edition is four keys, and
two of them are viewer-relative:

```json
{"user_has_approved": false, "user_can_approve": true, "approved": false, "approved_by": []}
```

After an approval **[live-CE]**:

```json
{"user_has_approved": true, "user_can_approve": false, "approved": true,
 "approved_by": [{"user": {"id": 1, "username": "root", ...},
                  "approved_at": "2026-08-10T15:33:59.184+02:00"}]}
```

There is **no `approvals_required` and no `approvals_left`** on Community Edition **[live-CE]**.
Those are Enterprise fields, and their absence is the decisive constraint: the approvals payload
alone cannot say whether review is *required*, only whether one happened. Every richer route is
Enterprise Edition and returns `404 {"error":"404 Not Found"}`, the unmatched-route envelope:
`approval_state`, `approval_rules`, and the project-level `approvals` configuration
**[live-CE]** **[source]**.

`user_has_approved` and `user_can_approve` are computed for the **calling token**, not for the
merge request. The same merge request reported `user_has_approved: true` to the approver and
`false` to a different token in the same second **[live-CE]**. The adapter MUST ignore both
fields; only `approved` and `approved_by[]` are identity-independent.

**Decision: fold `ReviewDecision` from the approvals payload plus the per-reviewer states**, in
the same spirit as the Gitea adapter folding a decision from its review list plus the
requested-reviewers signal. The reviewer states come from the dedicated route characterized in
[changes-requested reviews](#3-changes-requested-reviews). Evaluated in order:

| Condition | `ReviewDecision` |
| --- | --- |
| Any reviewer state is `requested_changes` | `CHANGES_REQUESTED` |
| `approved == true` | `APPROVED` |
| The reviewer list is non-empty | `REVIEW_REQUIRED` |
| No reviewers and no approvals | `NOT_REQUIRED` |

The changes-requested arm is tested first so a later approval by a second reviewer cannot
silently clear an outstanding block, matching the contract's rule that `APPROVED` holds only
when no `CHANGES_REQUESTED` supersedes it.

The last row is the load-bearing one, and it is a deliberate widening of the merge gate.
Community Edition has no approval rules, so it can never *require* an approval; a merge request
with no reviewer assigned is genuinely unreviewed rather than pending. `NOT_REQUIRED` lets the
auto-merge loop proceed on such a merge request. An operator who wants review enforced on
Community Edition MUST assign a reviewer, because the platform offers no server-side
alternative.

Corroborating evidence that Community Edition cannot require approval: after `unapprove` left
the merge request with zero approvals, `detailed_merge_status` was still `mergeable`
**[live-CE]**. The `not_approved` value exists in the enum but has no rule that can produce it
on Community Edition.

**`ReviewDecision` MUST NOT be derived from `detailed_merge_status`.** That field reports one
blocker at a time with undocumented precedence (see [mergeability](#2-mergeability)), so its
`not_approved` value is a sufficient signal of a missing approval and never a necessary one.

### 2. Mergeability

`detailed_merge_status` is a single string naming **one** blocking condition. The version-exact
enumeration was read from the live instance's own GraphQL schema, which is the authority for
this deployment rather than the upstream default branch:

```
approvals_syncing  blocked_status  checking  ci_must_pass  ci_still_running
commits_status  conflict  discussions_not_resolved  draft_status
external_status_checks  jira_association  locked_lfs_files  locked_paths
mergeable  merge_time  need_rebase  not_approved  not_open  preparing
requested_changes  security_policies_violations  security_policy_pipeline_check
title_not_matching  unchecked
```

24 values **[live-CE]**. REST renders them lowercase snake_case; the GraphQL enum renders the
same symbols upper-case. Six were observed directly in REST bodies: `preparing`, `checking`,
`mergeable`, `conflict`, `draft_status`, and `not_open` **[live-CE]**. The rest are
**[live-CE]** at schema level only.

**The source file and the running instance disagree, and the gap was not fully explained.**
`app/graphql/types/merge_requests/detailed_merge_status_enum.rb` declares **22** values, and the
file is byte-identical at the `v19.2.1-ee` tag and on the upstream default branch (its last
change was 2025-04-15, confirmed through the repository commit history) **[source]**. The
running Community Edition 19.2.1 instance serves **24**, adding `requested_changes` and
`security_policy_pipeline_check` **[live-CE]**. The file ends with
`DetailedMergeStatusEnum.prepend_mod_with(...)`, the hook an Enterprise module uses to extend it,
but the expected `ee/app/graphql/types/merge_requests/detailed_merge_status_enum.rb` path returns
404 **[source]**, so **the file that contributes the two extra values was not located**. This is
carried into the [open questions](#open-questions) rather than resolved by inference.

Two consequences hold regardless of where the extra values come from:

- **A declared value is not a reachable value.** Enum membership describes the schema surface,
  not what a licensed check can actually emit. `not_approved` is declared and, as shown above,
  cannot occur on Community Edition because no approval rule exists to produce it.
- **The mapping MUST have a default arm.** The list is demonstrably not closed against the source
  file, so an adapter that switches exhaustively over 22 or even 24 values will meet a string it
  does not know.

**Mapping to `domain.MergeabilityState`:**

| `detailed_merge_status` | `MergeabilityState` | Why |
| --- | --- | --- |
| `mergeable` | `clean` | The only affirmative value |
| `conflict` | `dirty` | The only conflict value; corroborated by `has_conflicts: true` **[live-CE]** |
| `unchecked`, `checking`, `preparing`, `approvals_syncing` | `unknown` | The platform is still computing |
| everything else | `blocked` | A precondition is unmet |

The four computing values are the load-bearing group, because the contract makes `unknown` a
deferral condition and the reconcile loop re-enqueues on it. A newly created merge request
reported `preparing` on the list route within a second of creation and `mergeable` moments later
**[live-CE]**, and approving one moved it back to `checking` **[live-CE]**. Classifying either as
`blocked` would still re-enqueue, but classifying either as `clean` would let the loop merge on a
stale computation.

**`unstable` is unreachable, by design rather than omission.** A pipeline whose only failing job
carries `allow_failure: true` reports top-level `status: "success"` **[live-CE]**, so a merge
request in that state reports `mergeable` and maps to `clean`. The warning is visible only in
`detailed_status.group == "success-with-warnings"` on the pipeline detail route **[live-CE]**.
The merge state machine treats `clean` and `unstable` identically, so nothing is lost; the
adapter simply never emits `unstable`, matching the Gitea adapter, which never emits it either.

**Unrecognized values map to `blocked`, not `unknown`.** Every value in the live enum other than
`mergeable` and the four computing states is a blocking reason, so an unfamiliar value from a
newer instance is far more likely to be a new blocker than a new computing state. Both arms
re-enqueue, so this choice is about not misreporting a permanent blocker as transient. The
adapter SHOULD log the unrecognized value at WARN.

**Robustness to masking, which the mapping MUST assume.** GitLab returns one value even when
several conditions block at once, and the precedence between them is neither documented nor
guaranteed. Upstream issue
[gitlab-org/gitlab#570458](https://gitlab.com/gitlab-org/gitlab/-/issues/570458) reports
`not_approved` masking `need_rebase` **[docs]**. Three rules follow, and they are what make the
mapping safe:

1. Treat the value as a **sufficient** signal that merging is blocked, never as a **necessary**
   one. The adapter MUST NOT conclude "no conflict" from a value of `not_approved`.
2. Derive nothing else from it. `ReviewDecision` comes from the approvals fold and
   `CIConclusion` from the pipeline read, so a masked value cannot corrupt either.
3. Rely on re-reading rather than on completeness. Masking changes only *which* blocker is
   reported, never *whether* one is, so the mapping above yields a non-`clean` state whenever
   any blocker exists. Each blocker surfaces in turn as the previous one clears, which costs
   extra poll ticks and cannot produce a wrong merge.

The merge endpoint is the backstop: it re-evaluates every precondition server-side and rejects
with 405 if any still holds, so a stale `clean` read cannot merge a blocked merge request.

### 3. Changes-requested reviews

**GitLab does have a per-reviewer review state on the REST surface, and the reference
documentation does not mention it.** This was the single most contested question in this
research, because two sources appeared to disagree. Both turned out to be describing different
objects.

The **merge-request object's** `reviewers[]` array holds bare user objects whose `state` is the
user's *account* state **[live-CE]**:

```json
"reviewers": [{"id": 1, "username": "root", "state": "active", "locked": false, ...}]
```

The reference documentation is correct about this array: "Current state of the reviewer's user
account. Possible values: `active`, `blocked`, or `deactivated`" **[docs]**.

The **dedicated reviewers route**, `GET /projects/:id/merge_requests/:iid/reviewers`, returns a
different entity: the user is nested under `user`, and the top-level `state` is the *review*
state **[live-CE]**:

```json
[{"user": {"id": 1, "username": "root", "state": "active", ...},
  "state": "unreviewed",
  "created_at": "2026-08-10T15:33:58.714+02:00"}]
```

Both `state` fields appear in one response and mean different things. Reading the account state
where the review state was intended is the trap, and it is easy to fall into because the outer
object nests the very field name it shadows.

Three independent confirmations that the review state is Community Edition and real:

- `lib/api/merge_requests.rb` defines the route at CE level, outside the `ee/` tree, and renders
  it with `Entities::MergeRequestReviewer`, which exposes `user`, `state`, and `created_at`
  **[source]**.
- The state enum lives in `app/models/concerns/merge_request_reviewer_state.rb` as
  `unreviewed: 0, reviewed: 1, requested_changes: 2, approved: 3, unapproved: 4,
  review_started: 5`, with no `ee/` override (that path returns 404) **[source]**. The live
  instance's own GraphQL schema exposes the same six values as `MergeRequestReviewState`
  **[live-CE]**.
- The field tracks real review actions: it read `unreviewed` on assignment, `approved` after an
  approval, and `unapproved` after an unapprove, all on Community Edition **[live-CE]**.

**Decision: `FetchPendingReviews` selects notes on merge requests whose reviewer state is
`requested_changes`.** GitLab has no review object bundling a state with a comment set the way
GitHub does, so selection is two reads: fetch the reviewer states, then fetch the notes and keep
those authored by a reviewer in `requested_changes`. Notes carry no review-state field of their
own, so the author login is the only join key.

**What this cannot do, and the degradation that follows.** GitLab does not attach comments to a
review verdict, so a reviewer who requests changes and comments separately produces a comment
set that is *everything that reviewer ever wrote on the merge request*, not the comments
belonging to one review round. The adapter MUST NOT claim finer granularity. When no reviewer is
in `requested_changes`, `FetchPendingReviews` returns an empty non-nil slice.

**No REST or GraphQL route sets the state on this version.** `PUT .../reviewers/:user_id`,
`PUT .../request_changes`, and a bogus-state variant all returned
`404 {"error":"404 Not Found"}` **[live-CE]**, and the live GraphQL schema exposes only
`mergeRequestReviewerRereview` and `mergeRequestSetReviewers` **[live-CE]**. This does not
affect the adapter, which only reads, but it does mean an integration test cannot drive a
merge request into `requested_changes` through the API. The state must be set through the web
UI, or the test must assert on the two states the API can produce (`approved` via `approve`,
`unapproved` via `unapprove`).

The note shape itself matches the issue notes already characterized, plus the review-specific
fields. A diff note carries `type: "DiffNote"`, a `position` object with
`base_sha`/`start_sha`/`head_sha`, `old_path`/`new_path`, `position_type`, and
`old_line`/`new_line`, and the resolution triple `resolvable`, `resolved`, `resolved_by`
**[live-CE]**. `POST .../discussions` returns a discussion wrapper with an `id`, `individual_note`,
`resolvable`, `resolved`, and a `notes[]` array **[live-CE]**. GitLab exposes no
platform "outdated" flag on a note, so `ReviewComment.Outdated` has no direct source; the
adapter derives it by comparing the note's `position.head_sha` against the merge request's
current `sha`.

### 4. CI status source

The issue frames pipelines and commit statuses as two competing models. They are not: **GitLab
unifies them, and the unification was verified in both directions.**

Posting an external commit status to a SHA that **already has a pipeline** attaches it to that
pipeline and changes the pipeline's own status. A pipeline reporting `success` (its only failing
job carried `allow_failure: true`) became `failed` after one external `failed` status was posted
to its SHA, and the new entry came back carrying `pipeline_id: 4`, the existing pipeline
**[live-CE]**.

Posting an external commit status to a SHA with **no pipeline** creates one. A SHA that reported
zero pipelines gained pipeline 5 with `source: "external"`, and that pipeline immediately became
the merge request's `head_pipeline` **[live-CE]**.

**Decision: `head_pipeline` on the merge-request object is the single CI source, and no manual
fold is needed for any status whose aggregate is a function of the failing set.** The
externally-reported status is already folded in by the platform. This saves a request compared
with the Gitea adapter, which must aggregate a status list itself. One status is not such a
function and is the sole exception: `manual`, which fetches that pipeline's commit statuses and
folds them through `scmcore.MergeGate`, as the `GetCIStatus` mapping below specifies.

`head_pipeline` is a full pipeline object embedded in the merge-request response, carrying
`id`, `status`, `source`, `sha`, `ref`, `web_url`, timing fields, and a `detailed_status`
sub-object **[live-CE]**. Reading it costs nothing beyond the merge-request read that
`GetMergeability` already performs.

**`head_pipeline` is an association, not a derived field.** The REST entity exposes the
`head_pipeline` association verbatim
([`lib/api/entities/merge_request.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/entities/merge_request.rb))
**[source]**, and nothing re-validates it against the merge request's current head. A push that
creates no pipeline leaves the field pointing at the superseded commit: a merge request whose
`sha` had moved to `86c6dc4b` after a commit removed `.gitlab-ci.yml` still reported
`head_pipeline` at `5487092b` with status `success`, and its pipeline list carried that one
pipeline only **[live-CE]**. The platform's own mergeability checks read `diff_head_pipeline`
instead, which is the same association narrowed to `nil` when it does not match the current
head
([`app/models/merge_request.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/merge_request.rb))
**[source]**. Any read that folds the head pipeline's job set therefore addresses
`head_pipeline.sha` and `head_pipeline.id`, never the merge request's own `sha`, so the
pipeline and the commit it describes cannot disagree.

**`GetCIStatus` mapping.** The orchestrator compares the returned string against the literal
`"success"` and treats the empty string as "no checks exist"
(`internal/orchestrator/auto_merge_reconcile.go`). Returning GitLab's pipeline status verbatim
would satisfy that comparison, but it is not the right answer: `scmcore.MergeGate` owns the
merge-gate vocabulary so that one forge cannot answer "is CI green" two ways, and the GitHub
and Gitea adapters both return its four values (`success`, `pending`, `failing`, and the empty
string). GitLab maps into the same four through `mapPipelineStatus`
(`internal/scm/gitlab/scm_merge.go`), which partitions every value of the platform's 13-value
`head_pipeline.status` enum:

| `head_pipeline.status` | `GetCIStatus` returns |
| --- | --- |
| `null` (no pipeline for the head SHA) | `""` |
| `success`, `skipped` | `"success"` |
| `failed`, `canceled` | `"failing"` |
| `created`, `waiting_for_resource`, `preparing`, `waiting_for_callback`, `pending`, `running`, `canceling`, `scheduled` | `"pending"` (still settling) |
| `manual` | the verdict `scmcore.MergeGate` computes over the pipeline's own job set, at the cost of one extra request (see below) |
| any other value, including the empty string | `"pending"`, and logs one WARN naming the observed value |

`skipped` is a terminal, non-failing status, and it folds to a merge-eligible verdict rather
than to a deferral. The platform's composite-status computation reports `skipped` for a pipeline
only when every job in it is either `skipped` or an ignored `manual` job, and the weaker branch
that would also call a pipeline `skipped` when only some of its jobs are skipped is guarded by a
`needs`-DAG flag that the pipeline's own status computation never sets. A `skipped` pipeline's
job set therefore cannot carry a failing conclusion, and no fetch of the statuses list can
reveal one it does not already rule out.

Expressing the shared rule over the platform's own aggregate rather than over a fetched run set
preserves the saving recorded above for twelve of the thirteen values: those answers cost no
request beyond the merge-request read. Fetching the statuses list for every status and calling
`scmcore.MergeGate` on the normalized runs would pay one extra request per tick per pending
merge, against a rate-limited API, to resolve differences the platform already reports for
free. `manual` is the one value the platform aggregate cannot resolve, so it is the one value
that pays for a job-set read.

`manual` is not a settled-and-clean signal. The platform reports it for a pipeline with no job
left running or queued, but its composite-status computation tests for a blocking manual job
before it falls through to reporting `failed`: the `any_of?(:manual)` branch precedes the final
`else 'failed'`
([`lib/gitlab/ci/status/composite.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/gitlab/ci/status/composite.rb))
**[source]**. A pipeline holding both a blocking manual job and a failed job therefore still
reports `manual`. Two arrangements reach that shape, and both were reproduced **[live-CE]**:

- A blocking manual job sharing a stage with the job that failed. Jobs `manual_gate` (`manual`)
  and `failing_job` (`failed`, `allow_failure: false`), both in stage `test`, produced
  `head_pipeline.status: "manual"`.
- Any pipeline that uses `needs`, where a job starts as soon as its own dependencies finish and
  can fail while an earlier-stage manual job is still untriggered. Jobs `build_job` (`success`,
  stage `build`), `manual_gate` (`manual`, stage `gate`), and `failing_job` (`failed`, stage
  `test`, `needs: [build_job]`) likewise produced `head_pipeline.status: "manual"`.

Only the pipeline's own job set separates those shapes from the benign one; no other field of
the merge-request response distinguishes them. Reading
`GET /projects/:id/repository/commits/:sha/statuses` for `head_pipeline.sha`, scoped to
`head_pipeline.id`, and folding the normalized entries through `scmcore.MergeGate` classifies
each observed shape **[live-CE]**:

| Job set on a `manual` head pipeline | Normalized conclusions | `scmcore.MergeGate` |
| --- | --- | --- |
| `manual` plus `failed`, same stage | `neutral`, `failure` | `failing` |
| `success` plus `manual` plus `failed`, `needs` DAG | `success`, `neutral`, `failure` | `failing` |
| `manual` plus `manual`, nothing else | `neutral`, `neutral` | `success` |
| `manual` plus a later-stage `created` job | `neutral`, `pending` (still `queued`) | `pending` |

The last row is the ordinary manual-gate pipeline, where the gate blocks a later stage whose
jobs have been created but not made runnable. The fold answers it `pending`, which is correct:
work remains that has not run. Mapping `manual` to a merge-eligible verdict without reading the
job set would answer the first, second, and fourth rows wrongly, letting an auto-merge reaction
merge a commit whose CI provider reports failing, or merge before the remaining work has run,
on an operation that cannot be undone.

The job-set read is one request in every observed case: the four fixtures returned `X-Total` of
2, 3, 2, and 2, each with `X-Total-Pages: 1` **[live-CE]**. The `pipeline_id` scope is
load-bearing rather than defensive. A commit carrying two pipelines returned both pipelines'
entries unfiltered (`X-Total: 2`) and exactly one entry under each `pipeline_id` value
(`X-Total: 1`), and `head_pipeline` moved to the newer pipeline **[live-CE]**.

A project that enables the "Pipelines must succeed" setting never reaches the CI read at all.
There a merge request whose head pipeline is `manual` reports
`detailed_merge_status: "ci_still_running"` **[live-CE]**, which is outside `mapMergeability`'s
recognized set and lands in its `blocked` arm, so the auto-merge precondition table stops on
mergeability before it calls `GetCIStatus`. The value is `ci_still_running` rather than
`ci_must_pass` because the reason is chosen by `diff_head_pipeline_considered_in_progress?`,
which under that setting reduces to `!pipeline.complete?`, and `manual` is not one of the four
completed statuses
([`app/services/merge_requests/mergeability/detailed_merge_status_service.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/services/merge_requests/mergeability/detailed_merge_status_service.rb),
[`app/models/merge_request.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/merge_request.rb),
[`app/models/concerns/ci/has_status.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/ci/has_status.rb))
**[source]**. `ci_still_running` is one of the 24 values the instance's own `DetailedMergeStatus`
enum declares **[live-CE]**, and it reaches `mapMergeability`'s unrecognized arm, so the adapter
logs one WARN naming it on each such read.

A `skipped` pipeline created by a `[ci skip]` commit carries no commit-status entry at all,
rather than one entry per skipped job: the pipeline-creation chain halts before it seeds any
job. `GetCIStatus` still returns `"success"` for it, because the mapping reads the pipeline's
own status field rather than the entry list. `FetchCIStatus` reads the entry list instead, so
it sees an empty run set and reports `"pending"`, and the merge gate that
`scmcore.MergeGate` would compute over that same empty set is `CIGateAbsent`, the empty string.
Both `""` and `"success"` are merge-eligible to the auto-merge reaction, and a `"pending"`
`FetchCIStatus` verdict is non-failing, so neither reader's answer for this shape lets a
failing commit merge or lets a passing one go unmerged.

The empty run set is reachable only for `skipped`. A `manual` pipeline always carries at least
the blocking manual job's entry, because the composite computation reports `manual` only for a
status set holding one, and every observed `manual` fixture returned at least two entries
**[live-CE]**. The `manual` arm therefore never folds an empty set, and it holds at `"pending"`
rather than reporting `CIGateAbsent` if it ever observes one.

**`FetchCIStatus` mapping to `domain.CIResult`.** The check-run list comes from
`GET /projects/:id/repository/commits/:sha/statuses`, which returns one entry per job **and**
per external status, each with `name`, `status`, `allow_failure`, `pipeline_id`, and
`target_url` **[live-CE]**:

| GitLab job or status | `CheckConclusion` | `CheckRunStatus` |
| --- | --- | --- |
| `success` | `success` | `completed` |
| `failed`, `allow_failure: false` | `failure` | `completed` |
| `failed`, `allow_failure: true` | `neutral` | `completed` |
| `canceled` | `cancelled` | `completed` |
| `skipped` | `skipped` | `completed` |
| `manual` | `neutral` | `completed` |
| `created`, `pending`, `waiting_for_resource`, `waiting_for_callback`, `preparing`, `scheduled` | `pending` | `queued` |
| `running`, `canceling` | `pending` | `in_progress` |

**The statuses route paginates.** `GET /projects/:id/repository/commits/:sha/statuses` returns
`X-Per-Page: 20` by default and honors `per_page` up to 100, advertising `X-Total`,
`X-Total-Pages`, `X-Next-Page`, and a `Link` header carrying `rel="next"` **[live-CE]**. A commit
carrying 103 seeded statuses returned 100 entries on page one with `X-Total-Pages: 2` and a
`rel="next"` link at `per_page=100`, confirming `FetchCIStatus`'s walk through
`httpkit.NewLinkPaginator` must cross a page boundary to see the full set on a pipeline this
size.

**`allow_failure` belongs in the conclusion, not in the aggregate.** Neither the aggregate nor
the failing count is the adapter's to choose: `scmcore.AggregateCIStatus` and
`scmcore.FailingCount` compute both from the normalized `CheckRun` set, and
`adaptertest.AssertCIAggregateMatchesCore` recomputes them from the returned `CheckRuns` and
fails the suite when either disagrees. Sourcing the aggregate from the pipeline's own status,
as this section previously specified, would fail that assertion on exactly the case that
motivated it.

Mapping a soft-failing job to `neutral` reconciles the two: the platform's `allow_failure`
rule is applied once, where the entry is normalized, and the shared fold then agrees with the
platform for free. The warnings-only case observed live is the test. `soft-fail` was `failed`
with `allow_failure: true` while the pipeline was `success` **[live-CE]**, and under this
mapping the run is non-failing, `AggregateCIStatus` returns `passing`, and `FailingCount`
returns zero. A separate "count only `allow_failure: false` entries" rule is therefore not
needed and MUST NOT be written: it would restate in the count what the conclusion already
encodes, and the two would drift.

The warnings-only case is also the one place where the pipeline's `detailed_status` sub-object
carries information the top-level status does not: `{"label": "passed with warnings", "group":
"success-with-warnings", "icon": "status_warning"}` **[live-CE]**. It is available on the
pipeline detail route and on the embedded `head_pipeline`, but **not** on the pipeline list
route **[live-CE]**.

### 5. Job log retrieval

`GET /projects/:id/jobs/:job_id/trace` returns the job log as `Content-Type: text/plain` with an
explicit `Content-Length` and no `Transfer-Encoding: chunked` **[live-CE]**. A job that does not
exist returns `404 {"message":"404 Not found"}` **[live-CE]**.

**The trace is not a plain log, and treating it as one would put terminal control bytes into an
agent prompt.** A real failing job's trace, 3,926 bytes over 44 lines, has three layers of markup
on every line **[live-CE]**:

```
2026-08-10T13:32:58.212878Z 01O ESC[32;1m$ ls /definitely-missing-pathESC[0;m
2026-08-10T13:32:58.213217Z 01E ls: /definitely-missing-path: No such file or directory
2026-08-10T13:32:58.264859Z 00O section_end:1786368778:step_scriptCR ESC[0K
2026-08-10T13:32:58.512771Z 00O ESC[31;1mERROR: Job failed: exit code 1ESC[0;m
```

`ESC` is byte `0x1b` and `CR` is byte `0x0d`; both are literal in the response. Each line carries:

1. An RFC 3339 timestamp with nanosecond precision, then a space.
2. A four-character stream token: `00O`, `01O`, `01E`, or `00O+`. The trailing `E` marks stderr
   and `O` stdout; the `+` marks a continuation of the previous logical line.
3. ANSI SGR escape sequences for color, plus `ESC[0K` erase-to-end-of-line, plus
   `section_start:<epoch>:<name>` and `section_end:<epoch>:<name>` collapsible-section markers
   each terminated by a carriage return.

The `domain.CIResult.LogExcerpt` godoc already tells adapters they "must truncate to a
configurable line count" and "should strip ANSI escape sequences", so truncation is a
requirement and stripping is a recommendation the GitLab trace makes unavoidable in practice.
For GitLab that means stripping all three layers, not only the ANSI one.

**Decision: fetch the whole trace, then truncate client-side.** The route does not support
partial fetches. A request carrying `Range: bytes=0-99` returned `200` with the full
`Content-Length: 3926` and no `Accept-Ranges` or `Content-Range` header **[live-CE]**, so the
server ignored it. `ci_feedback.max_log_lines` is therefore applied after the body is read: take
the last N lines of the sanitized text, matching the reaction's intent of showing the end of a
failing job.

**Which job to fetch, and the trap that a commit-status entry is not always a job.** Jobs and
external statuses share one identifier space, because GitLab stores both in the same table. The
commit-statuses list therefore mixes entries whose `id` works on the trace route with entries
whose `id` does not, and nothing in the entry's own fields separates them.

Confirmed with a positive control on one pipeline **[live-CE]**:

```
id 11  soft-fail        (job)              GET .../jobs/11/trace -> 200
id 12  external-scanner (external status)  GET .../jobs/12/trace -> 404 {"message":"404 Not found"}
                                           GET .../jobs/12       -> 404 {"message":"404 Not found"}
```

`FetchCIStatus` populates `LogExcerpt` from the first failing check run. "Failing" here is not
a GitLab predicate the adapter restates: `scmcore.IsFailingConclusion` is the selector, and its
godoc names log selection as the reason it is exported separately from the verdict. Under the
conclusion mapping above that resolves to the first entry with `status: "failed"` and
`allow_failure: false`, because a soft-failing job normalizes to `neutral`, which is not a
failing conclusion. **Decision: resolve the job set
from `GET /projects/:id/pipelines/:pipeline_id/jobs` and select only from entries appearing
there**, rather than calling the trace route speculatively and treating a 404 as a negative.
That costs one request instead of one per candidate, and it avoids logging a 404 that is not an
error. When the first failing entry is an external status, the adapter leaves `LogExcerpt` empty
rather than fabricating one, which is the behavior the godoc already prescribes for a provider
that cannot retrieve a log.

The job object itself (`GET /projects/:id/jobs/:job_id`) supplies the rest of the check-run
mapping: `name`, `stage`, `status`, `allow_failure`, `web_url` for `CheckRun.DetailsURL`, and
`failure_reason`, which read `script_failure` for the deliberately failing job **[live-CE]**.

### 6. Merge call

The verb is `PUT /projects/:id/merge_requests/:iid/merge`. Success returns `200` with the full
merge-request object, from which `MergeResult.SHA` is taken from `merge_commit_sha` and
`MergeResult.Merged` from `state == "merged"` **[live-CE]**.

**The precondition parameter is `sha`, and it is the head of the *source* branch, not the target.**
On this instance it is **required, not optional**: omitting it returned
`400 {"message":"SHA must be provided when merging"}` for both a Developer and an administrator
token, on both a `checking` and a `mergeable` merge request **[live-CE]**. The source shows the
requirement is conditional on `user_project.namespace.require_sha_for_merge?` **[source]**, so an
instance may not enforce it. The adapter MUST always send `sha` regardless, which satisfies both
configurations and gives the precondition semantics the contract wants anyway.

**Parameter mapping for `domain.MergeStrategy`:**

| Strategy | Parameters | Note |
| --- | --- | --- |
| `StrategyMerge` | `sha`, optional `merge_commit_message` | The project's `merge_method` was `merge` **[live-CE]** |
| `StrategySquash` | `sha`, `squash: true`, optional `squash_commit_message` | Verified: returned both a `squash_commit_sha` and a `merge_commit_sha` **[live-CE]** |
| `StrategyRebase` | Not expressible on this endpoint | See below |

**`StrategyRebase` degrades and the degradation MUST be explicit.** GitLab has no per-call rebase
strategy. Rebase-on-merge is a project-level setting (`merge_method` of `rebase_merge` or `ff`),
and the standalone `PUT .../rebase` route is asynchronous: it returned
`202 {"rebase_in_progress":true}` and does not merge **[live-CE]**. The adapter MUST NOT silently
substitute a merge commit for a requested rebase. It either rejects a configured `rebase`
strategy at construction time, or documents that the strategy is governed by the project's
`merge_method` and that the configured value is inert.

`commitTitle` and `commitMessage` map to `merge_commit_message` for a merge and
`squash_commit_message` for a squash. GitLab takes a single message field per strategy rather
than a separate title, so the adapter joins the two with a blank line, matching how GitLab's own
UI composes them.

**Rejection bodies, and the disambiguation problem this creates.** The checks fire in a fixed
order, confirmed both live and in source (`build_merge_params` calls `check_sha_param!` before
`execute_merge` reaches the mergeability gate) **[source]**:

| Order | Condition | Response |
| --- | --- | --- |
| 1 | Merge request does not exist | `404 {"message":"404 Not found"}` |
| 2 | `sha` omitted where required | `400 {"message":"SHA must be provided when merging"}` |
| 3 | Caller may not merge the target branch | `401 {"message":"401 Unauthorized"}` |
| 4 | `sha` does not match the source head | `409 {"message":"SHA does not match HEAD of source branch: <actual sha>"}` |
| 5 | Not mergeable, for any reason at all | `405 {"message":"405 Method Not Allowed"}` |

All five were observed live **[live-CE]**.

**The 405 body carries no reason, and this is the finding that shapes the implementation.**
Already-merged, draft, and conflicting merge requests produced byte-identical 405 responses
**[live-CE]**:

```
already merged (iid 4, correct sha) -> 405 {"message":"405 Method Not Allowed"}
draft          (iid 3, correct sha) -> 405 {"message":"405 Method Not Allowed"}
conflicting    (iid 2, correct sha) -> 405 {"message":"405 Method Not Allowed"}
```

The source explains why: both arms of `execute_merge` call the same bare `not_allowed!` helper,
which renders a constant **[source]**. When these notes were written the `SCMAdapter` contract
keyed the already-merged marker on the provider's response body, and GitLab's constant body was
a problem to be worked around. It is no longer one. The contract requires implementations to
surface the substring "already merged" in `SCMError.Message` "when they have confirmed, by
re-reading the pull request, that the pull request is merged, rather than by matching the
provider's response wording" (`MergePR`, `internal/domain/scm.go`). GitLab's opaque 405 is
therefore no longer exceptional: GitHub and Gitea re-read for the same reason, and Gitea does
so even though its own body does say "already merged".

**Decision: on 405, re-read the merge request and classify from its state.** This is the
contract's own procedure rather than a substitute for it.

```
PUT .../merge -> 405
  scmcore.AsMergeConflict promotes on TrackerError.Status, giving ErrSCMConflict
  GET /projects/:id/merge_requests/:iid
    state == "merged"  -> scmcore.AlreadyMergedConflict, Message carries "already merged"
    otherwise          -> the promoted ErrSCMConflict, Message names detailed_merge_status
  read fails           -> the promoted ErrSCMConflict unchanged (retry is the safe default)
```

The re-read is authoritative for this purpose because a merged merge request reports
`state: "merged"` together with a non-null `merged_at` and `merge_commit_sha`, and
`detailed_merge_status: "not_open"` **[live-CE]**. It costs one extra request only on the error
path. Two steps belong to the shared owners rather than to the adapter: the promotion to
`ErrSCMConflict` is `scmcore.AsMergeConflict`, which reads `domain.TrackerError.Status`, matches
405 or 409, and never inspects body text; and the marker phrase is
`scmcore.AlreadyMergedConflict`, which the adapter calls only once the re-read confirms the
merge. The tracker client already records `Status` on every classified response, so the forge
half needs no second classification path (see
[status-to-category mapping](#status-to-category-mapping)).

The 409 arm is promoted by the same `scmcore.AsMergeConflict` call and arrives as
`ErrSCMConflict` too, but it needs no re-read: it is unambiguously head-SHA drift, and its
message even echoes the actual source head, which the state machine's "head SHA mismatch"
transition re-enqueues. Carrying no marker is exactly right there, since an unmarked
`ErrSCMConflict` is the retry disposition. `MergeResult.SHA` on that path is unset.

**A 401 on this route is an authorization failure, not a bad token.** The Developer token
received `401 {"message":"401 Unauthorized"}` on every merge attempt because `main` is protected
with `merge_access_levels` of Maintainers **[live-CE]**, and the source guards the route with
`unauthorized! unless merge_request.can_be_merged_by?(current_user)`, commented "the user doesn't
have permissions to push into target branch" **[source]**. This deviates from the rest of the
GitLab API, which uses 403 for insufficient permission and 401 for a bad credential. Mapping 401
to `ErrSCMAuth` is still the right behavior, because both causes need a human, and `ErrSCMAuth`
escalates immediately rather than retrying. But the adapter cannot tell the operator *which* of
the two it is, so the error message MUST name both possibilities.

**Branch deletion after merge** is `DELETE /projects/:id/repository/branches/:branch`, returning
`204` with an empty body on success and `404 {"message":"404 Branch Not Found"}` when the branch
is already gone **[live-CE]**. The 404 is not mapped to nil: `DeleteBranch` returns a
`domain.SCMError` of kind `ErrSCMNotFound` and the caller treats that as a successful no-op,
which is what `adaptertest.AssertBranchAbsentDisposition` pins. `RemoveLabel` disposes of its
already-absent case the other way, returning nil
(`adaptertest.AssertLabelAbsentDisposition`), so the two must not be written from one template.
Branch names containing a slash MUST be percent-encoded: `feat%2Fnested-name` deleted
successfully **[live-CE]**. Note that `should_remove_source_branch: true` on the merge call is
asynchronous; the branch was still readable immediately after a 200 merge response
**[live-CE]**, so `DeleteBranch` must tolerate both the branch being present and it having
already been reaped. Neither route may produce `ErrSCMConflict`, whatever status it returns:
only the merge write path promotes.

### 7. Bot classification

**The platform bot marker does not reach note authors, which corrects a claim in the earlier gap
map.** That claim was true of *token identities* and does not carry to *note authors*.

Every embedded user object in the API, including a note's `author`, is rendered with
`API::Entities::UserBasic`, which exposes `id`, `username`, `name`, `state`, `locked`,
`public_email`, `avatar_url`, and `web_url`, and **no `bot` field** **[source]**. Live
confirmation on notes written by a project access token identity and by a human, on the same
merge request **[live-CE]**:

```
note 97 system=False author=project_3_bot_e24d338cfed686e5ed9e28650363bc72
        author keys: [avatar_url, id, locked, name, public_email, state, username, web_url]
note 98 system=False author=sortie-bot
        author keys: [avatar_url, id, locked, name, public_email, state, username, web_url]
```

The two are indistinguishable by shape. The list route `GET /users?username=<name>` also renders
`UserBasic` and likewise omits `bot` **[live-CE]**.

`bot` is available, on one route only: `GET /users/:id` returns it, and **a non-administrator
token can read it** **[live-CE]**. That last point is what makes the platform predicate usable at
all; had it been administrator-only, the allowlist would have been the only option.

**Decision: supply the contract's union with the allowlist as the primary signal and a cached
`GET /users/:id` lookup as the platform signal.** The union itself is `scmcore.IsBotAuthor`,
which every forge calls with the same two inputs: GitHub passes its `user.type == "Bot"` flag,
Gitea passes a constant false because it has no platform marker, and GitLab passes the `bot`
field the per-user route returns. `FetchBotReviewComments` collects the distinct author IDs
from the notes it already fetched, resolves each once through `GET /users/:id`, and calls
`scmcore.IsBotAuthor(login, bot, botUsernames)` per comment. The lookup is cached for the
adapter's lifetime, and the cost is bounded by the number of distinct authors on one merge
request, not by the number of comments. GitLab is the only forge of the three that spends a
request to answer the platform half.

An adapter that wants to avoid the extra requests entirely may pass an empty `botUsernames` and
skip the lookup, but then bot selection returns nothing, so the allowlist stops being optional.
That is the degradation to state in the configuration documentation.

**The username pattern is a hint, not a contract.** GitLab names its managed token identities
`project_<id>_bot_<hex>` and `group_<id>_bot_<hex>` **[live-CE]**, so a prefix match would
classify GitLab's own token identities for free. It would not classify a third-party review bot
driven by a personal access token, which is the common case for the review-comments reaction.
The adapter MUST NOT use the pattern as its only platform signal.

### 8. Project addressing

**A subgroup path round-trips through the contract's `(owner, repo)` pair at any nesting depth.**
The fixture project sits three namespaces deep, which is one level deeper than a subgroup needs
to be to break a single-slash assumption:

```
GET /api/v4/projects/scmlab%2Fteam%2Fsquad%2Fmrlab
-> 200 {"id": 3, "path_with_namespace": "scmlab/team/squad/mrlab", ...}
```

**[live-CE]**. Sub-routes resolve identically under the same encoded prefix:
`.../merge_requests` returned `200 []` **[live-CE]**.

**Decision: the adapter joins `owner` and `repo` with `/` and percent-encodes the result once.**
`owner` carries the whole namespace path (`scmlab/team/squad`) and `repo` the project path
(`mrlab`). Two prohibitions follow, and both are the kind of assumption a GitHub or Gitea
implementation carries in by habit:

- The adapter MUST NOT validate `owner` as a single path segment, and MUST NOT split it on `/`.
- The adapter MUST NOT encode `owner` and `repo` separately and then join them, because that
  encodes the separator between them while leaving the separators inside `owner` unencoded.

Both failure modes were probed, and they fail differently, which is useful for diagnosis:

| Form | Response | Meaning |
| --- | --- | --- |
| `scmlab%2Fteam%2Fsquad%2Fmrlab` | `200` | Correct |
| `scmlab/team/squad/mrlab` (unencoded) | `404 {"error":"404 Not Found"}` | Route never matched |
| `scmlab%252Fteam%252Fsquad%252Fmrlab` (double-encoded) | `404 {"message":"404 Project Not Found"}` | Route matched, no such project |

**[live-CE]**. The two envelopes are the ones already distinguished in
[Error model](#four-envelope-shapes): the `error` key means the router found nothing, and the
`message` key means the router found the route and the resource lookup failed. An adapter that
logs the raw body can tell an encoding bug from a configuration mistake without further probing.

This matches the tracker half's existing rule that `tracker.project` MUST NOT be validated as
exactly one slash, so the two halves share one addressing convention. The one-slash grammar has
a shared home, `registry.DiagOwnerRepoProject`, which the GitHub and Gitea validators call and
which the GitLab validator deliberately does not (`validateProject`,
`internal/scm/gitlab/validate.go`). Reaching for the shared helper by reflex would reintroduce
the rule this section rules out.

### Route inventory

Every route below was exercised on the fixture unless the status column says otherwise.

| Contract method | Route | Status |
| --- | --- | --- |
| `GetMergeability` | `GET /projects/:id/merge_requests/:iid` | Verified **[live-CE]** |
| `GetReviewDecision` | `GET .../merge_requests/:iid/approvals` plus `.../reviewers` | Verified, two requests **[live-CE]** |
| `GetCIStatus` | `head_pipeline` on the merge-request object, plus `GET /projects/:id/repository/commits/:sha/statuses` on a `manual` head pipeline | Verified, no extra request for twelve of the thirteen pipeline statuses and one extra request for `manual` **[live-CE]** |
| `FetchCIStatus` | `GET /projects/:id/repository/commits/:sha/statuses` | Verified **[live-CE]** |
| Job log for `max_log_lines` | `GET /projects/:id/jobs/:job_id/trace` | Verified, `text/plain`, no range support **[live-CE]** |
| `FetchPendingReviews` | `.../merge_requests/:iid/reviewers` plus `.../notes` | Verified **[live-CE]** |
| `FetchBotReviewComments` | `.../merge_requests/:iid/notes` plus `GET /users/:id` | Verified **[live-CE]** |
| `MergePR` | `PUT .../merge_requests/:iid/merge` | Verified, all five response classes **[live-CE]** |
| `DeleteBranch` | `DELETE /projects/:id/repository/branches/:branch` | Verified, 204 and 404 **[live-CE]** |
| `ListLabelEvents` | `GET .../merge_requests/:iid/resource_label_events` | Route verified, `200 []`; shape verified on the issue variant **[live-CE]**. Entry ids normalize through `scmcore.SortableEventID`, so ordering by `(At, ID)` as strings matches journal order |
| `RemoveLabel` | `PUT .../merge_requests/:iid` with `remove_labels` | Not exercised on merge requests; identical parameter to the issue route. A name that does not exist returns `200` and changes nothing **[live-CE]**, which is already the contract's nil-return no-op |

Two findings from the tracker research that the forge half inherits unchanged:

- **State-change and label-change events are not notes.** GitLab keeps them in separate journals,
  `resource_label_events` and `resource_state_events`, both available on Community Edition and
  both verified to return typed entries **[live-CE]**. The label-command reaction reads the label
  journal, not the note stream.
- **Notes are the same entity as on issues**, so the comment normalization is shared. System notes
  must be filtered the same way, and the merge-request notes route accepts the same
  `activity_filter` and `sort` parameters.

### What Community Edition cannot do

Stated plainly, with the degradation rather than a workaround:

| Capability | Community Edition | Degradation |
| --- | --- | --- |
| Required approvals | No approval rules exist; `approvals_required` and `approvals_left` are absent from the payload **[live-CE]** | `ReviewDecision` returns `NOT_REQUIRED` when no reviewer is assigned, so the merge gate is open unless the operator assigns reviewers |
| `approval_state`, approval rules, project approval settings | All routes return the unmatched-route `404 {"error":"404 Not Found"}` **[live-CE]** **[source]** | The decision is folded from `approvals` plus `reviewers`; per-rule detail is unavailable |
| Setting a reviewer's review state through the API | No REST route and no GraphQL mutation on 19.2.1 **[live-CE]** | Reading works; integration tests can only produce `approved` and `unapproved` |
| Per-review comment grouping | GitLab has no review object bundling a verdict with comments | `FetchPendingReviews` returns all comments by reviewers in `requested_changes`, not one review round |
| Bot marker on a note author | `UserBasic` has no `bot` field **[source]** **[live-CE]** | One `GET /users/:id` per distinct author, cached, or the username allowlist alone |
| Per-call rebase merge strategy | The merge endpoint has no rebase parameter; `.../rebase` is a separate async route **[live-CE]** | `StrategyRebase` is governed by the project's `merge_method`, not by the call |
| A merge rejection reason | Every non-mergeable cause returns the same 405 constant **[live-CE]** **[source]** | One extra merge-request read on the error path to classify already-merged |
| `unstable` mergeability | A warnings-only pipeline reports `success` and the merge request reports `mergeable` **[live-CE]** | The adapter never emits `unstable`; the state machine treats it as `clean` anyway |

---

## Config notes

- **`tracker.endpoint`:** optional, defaulting to `https://gitlab.com`, so only a
  self-managed deployment sets it. The adapter appends `/api/v4`. TLS strongly recommended:
  the token travels in a header.
- **`tracker.api_key`:** access token string, sent as `PRIVATE-TOKEN`. The adapter and the
  validator run no prefix or length check: the access-token prefix is an
  administrator-writable application setting, so a shape check would reject valid tokens on
  a customized instance. Scope `api` for the full surface, `read_api` for a read-only
  deployment.
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
- **Headers:** `PRIVATE-TOKEN` on every request; `User-Agent: sortie/<version>` on tracker
  requests and the configured `user_agent` on the SCM and CI roles;
  `Content-Type: application/json` on writes. No API version header exists.
- **Construction preflight:** `GET /personal_access_tokens/self` (credential, scope, and
  expiry check), `GET /projects/:id` (project check, and the only way to separate a wrong
  project from an unauthorized token), and `GET /projects/:id/labels` (canonical casing of the
  configured state labels). The token introspection is advisory and never blocks
  construction; its result only reports whether the token authenticated in the project-check
  failure message. A failure of either of the other two is a configuration error surfaced at
  startup.

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
21. **Forge route existence.** Merge-request list, merge-request notes and discussions,
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

The 2026-08-10 pass added the following against a purpose-built project at namespace depth three
(`scmlab/team/squad/mrlab`), with a registered Docker-executor runner:

23. **Project addressing at depth.** `GET /projects/scmlab%2Fteam%2Fsquad%2Fmrlab` returned 200
    and echoed `path_with_namespace: "scmlab/team/squad/mrlab"`; the sub-route
    `.../merge_requests` returned `200 []`. Unencoded slashes returned
    `404 {"error":"404 Not Found"}` and double-encoded `%252F` returned
    `404 {"message":"404 Project Not Found"}`, the two envelopes distinguishing an unmatched
    route from a failed lookup.
24. **`detailed_merge_status` values.** Six values were observed in REST bodies: `preparing` and
    `checking` on freshly created or freshly approved merge requests, `mergeable`, `conflict`
    (with `has_conflicts: true`), `draft_status`, and `not_open` after merge. The instance's own
    GraphQL schema enumerated 24 values, two more than the upstream default branch carries.
    Approving a merge request moved the field from `mergeable` back to `checking`, proving it
    recomputes.
25. **Approvals payload.** Community Edition returned exactly four keys: `user_has_approved`,
    `user_can_approve`, `approved`, `approved_by[]`. No `approvals_required` and no
    `approvals_left`. The first two keys were viewer-relative: the same merge request reported
    `user_has_approved: true` to the approver and `false` to another token. `approval_state`,
    `approval_rules`, and the project-level `approvals` route each returned
    `404 {"error":"404 Not Found"}`. With zero approvals the merge request was still `mergeable`.
26. **Per-reviewer review state.** `GET .../merge_requests/:iid/reviewers` returned objects
    nesting the user under `user` and exposing a top-level `state`, which read `unreviewed` on
    assignment, `approved` after `approve`, and `unapproved` after `unapprove`. The
    merge-request object's own `reviewers[]` array carried only bare user objects whose `state`
    was the account state `active`. No REST route and no GraphQL mutation set the review state.
27. **CI unification.** An external commit status posted to a SHA that already had a pipeline
    attached to that pipeline (`pipeline_id` echoed the existing id) and flipped the pipeline
    from `success` to `failed`. An external status posted to a SHA with no pipeline created one
    with `source: "external"`, which became the merge request's `head_pipeline`. A pipeline whose
    only failing job carried `allow_failure: true` reported top-level `success` with
    `detailed_status.group: "success-with-warnings"`, and `detailed_status` was present on the
    pipeline detail route and on `head_pipeline` but absent from the pipeline list route.
28. **Job trace.** `GET /projects/:id/jobs/:job_id/trace` returned `text/plain` with
    `Content-Length: 3926` over 44 lines for a real failing job. Every line carried an RFC 3339
    nanosecond timestamp, a stream token (`00O`, `01O`, `01E`, `00O+`), and ANSI escape
    sequences, plus `section_start`/`section_end` markers terminated by carriage returns. A
    `Range: bytes=0-99` request returned 200 with the full body and no `Accept-Ranges` or
    `Content-Range` header. A nonexistent job returned `404 {"message":"404 Not found"}`.
29. **Merge endpoint.** Omitting `sha` returned `400 {"message":"SHA must be provided when
    merging"}` for both token identities. A Developer token returned
    `401 {"message":"401 Unauthorized"}` against the default branch, which is protected with
    `merge_access_levels` of Maintainers. A wrong `sha` returned `409 {"message":"SHA does not
    match HEAD of source branch: <actual sha>"}`. Already-merged, draft, and conflicting merge
    requests each returned the byte-identical `405 {"message":"405 Method Not Allowed"}`. A
    squash merge returned both a `squash_commit_sha` and a `merge_commit_sha`. The standalone
    rebase route returned `202 {"rebase_in_progress":true}`.
30. **Bot marker.** A note authored by a project access token identity and a note authored by a
    human carried identical author key sets, neither containing `bot`. `GET /users/:id` returned
    `bot: true` for the token identity, and returned it to a non-administrator token.
    `GET /users?username=` omitted the field.
31. **Branch deletion.** `DELETE .../repository/branches/:branch` returned 204 with an empty body
    on success and `404 {"message":"404 Branch Not Found"}` for an absent branch. A branch name
    containing a slash deleted successfully when percent-encoded. `should_remove_source_branch:
    true` on the merge call did not delete the branch synchronously: it was still readable
    immediately after the 200 merge response.

The 2026-08-16 pass added the following against a purpose-built scratch project in the
administrator's own namespace, with its own registered Docker-executor runner. Every result
below was read with the project Developer token; the administrator token created the project,
the runner, and the branches only.

32. **A `manual` head pipeline can carry a failed job, in two arrangements.** A pipeline whose
    stage `test` held `manual_gate` (`when: manual`, `allow_failure: false`) and `failing_job`
    (`exit 1`) settled at `head_pipeline.status: "manual"` with jobs `failing_job: failed` and
    `manual_gate: manual`. A second pipeline with `build_job` in stage `build`, `manual_gate` in
    stage `gate`, and `failing_job` in stage `test` declaring `needs: [build_job]` likewise
    settled at `manual`, with `build_job: success`, `manual_gate: manual`, and
    `failing_job: failed`. Neither merge request's `detailed_merge_status` was affected: both
    read `mergeable`, so the CI read is reached on a project that does not require passing
    pipelines.
33. **The commit-statuses view of those pipelines.** Scoped to `head_pipeline.sha` and
    `head_pipeline.id`, the same-stage pipeline returned two entries (`manual_gate: manual`,
    `failing_job: failed`, `allow_failure: false` on both) and the `needs` pipeline returned
    three. Normalizing each through the job-outcome mapping yields `neutral` plus `failure`, and
    `success` plus `neutral` plus `failure`, so `scmcore.MergeGate` returns `failing` for both.
34. **A wholly manual pipeline folds to a merge-eligible verdict.** Two blocking manual jobs and
    nothing else settled at `head_pipeline.status: "manual"` and returned two `manual` entries,
    which normalize to two completed `neutral` runs, so `scmcore.MergeGate` returns `success`.
35. **A manual gate blocking a later stage folds to a deferral.** `manual_gate` in stage `gate`
    with `deploy_job` in stage `deploy` settled at `head_pipeline.status: "manual"` with
    `deploy_job: created`, which normalizes to a `queued` run, so `scmcore.MergeGate` returns
    `pending`. Folding the job set therefore distinguishes this shape from the wholly manual one,
    which no single pipeline status can.
36. **`pipeline_id` scoping is load-bearing on the statuses route.** Triggering a second pipeline
    for an unchanged SHA moved `head_pipeline` to the new pipeline and left the commit carrying
    two. The unfiltered statuses read returned `X-Total: 2` with one entry per pipeline; each
    `pipeline_id` value returned `X-Total: 1`. Every fixture in this pass fit one page:
    `X-Total` of 2, 3, 2, and 2 with `X-Total-Pages: 1`.
37. **`head_pipeline` can describe a superseded commit.** After a commit that removed
    `.gitlab-ci.yml`, and therefore created no pipeline, a merge request reported
    `sha: 86c6dc4b` while `head_pipeline` still reported `id: 18`, `sha: 5487092b`, and
    `status: "success"`. Its merge-request pipeline list carried that one pipeline only.
38. **"Pipelines must succeed" reports `ci_still_running` for a `manual` head pipeline.** With
    `only_allow_merge_if_pipeline_succeeds: true` on the project, all three `manual` merge
    requests moved from `detailed_merge_status: "mergeable"` to `"ci_still_running"`, while
    `merge_status` stayed `can_be_merged`. Setting the flag back to false restored `mergeable`.
39. **The pipeline-status enum still has 13 values.** The instance's own GraphQL
    `PipelineStatusEnum` and `CiJobStatus` each enumerate `created`, `waiting_for_resource`,
    `preparing`, `waiting_for_callback`, `pending`, `running`, `success`, `failed`, `canceling`,
    `canceled`, `skipped`, `manual`, and `scheduled`, matching `AVAILABLE_STATUSES` at
    `v19.2.1-ee`. `DetailedMergeStatus` still enumerates 24 values, `ci_still_running` among
    them.

All fixture mutations made during the 2026-08-04 and 2026-08-05 passes were reverted:
probe-created issues, notes, issue links, and labels were deleted, altered label sets and issue
states were restored, and every token created for scope probing was revoked. The 2026-08-10
fixtures were **left in place** as the SCM fixture blueprint: the `scmlab` group tree, the
`scmlab/team/squad/mrlab` project, its merge requests, and the registered runner. The 2026-08-16
pass created its own project and runner rather than mutating that blueprint, and deleted both
afterwards.

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
| Per-reviewer review state route and entity | [`lib/api/merge_requests.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/merge_requests.rb) and [`lib/api/entities/merge_request_reviewer.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/entities/merge_request_reviewer.rb) | Live: state read as `unreviewed`, `approved`, and `unapproved` in turn. Route is defined outside `ee/` and is undocumented in the reference docs |
| Review-state enum | [`app/models/concerns/merge_request_reviewer_state.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/merge_request_reviewer_state.rb) | Six values; the `ee/` override path returns 404. Corroborated by the live instance's `MergeRequestReviewState` GraphQL enum |
| `detailed_merge_status` enumeration | The live instance's own GraphQL introspection, and [`app/graphql/types/merge_requests/detailed_merge_status_enum.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/graphql/types/merge_requests/detailed_merge_status_enum.rb) | 24 values on 19.2.1 against 22 on the upstream default branch; six confirmed in REST bodies |
| Merge-endpoint check ordering, the 405 constant, and the conditional `sha` requirement | [`lib/api/merge_requests.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/merge_requests.rb) (`build_merge_params`, `check_sha_param!`, `execute_merge`, `not_allowed!`) | Live: all five response classes provoked in order |
| Absence of a bot marker on embedded users | [`lib/api/entities/note.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/entities/note.rb) and [`lib/api/entities/user_basic.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/entities/user_basic.rb) | Live: identical author key sets for a token-bot note and a human note; `bot` present only on `GET /users/:id` |
| Single-value `detailed_merge_status` masking | [gitlab-org/gitlab#570458](https://gitlab.com/gitlab-org/gitlab/-/issues/570458) | Upstream issue report; the precedence itself was not reproduced here |
| Composite-status branch order, and why a `manual` pipeline can hold a failed job | [`lib/gitlab/ci/status/composite.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/gitlab/ci/status/composite.rb) and [`app/models/concerns/ci/has_status.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/ci/has_status.rb) | Live: four pipeline shapes built and settled, each read back as jobs and as commit statuses |
| `head_pipeline` as an unvalidated association, versus `diff_head_pipeline` | [`lib/api/entities/merge_request.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/lib/api/entities/merge_request.rb) and [`app/models/merge_request.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/merge_request.rb) | Live: a push creating no pipeline left the field on the superseded commit |
| `ci_still_running` for a `manual` head pipeline under "Pipelines must succeed" | [`app/services/merge_requests/mergeability/detailed_merge_status_service.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/services/merge_requests/mergeability/detailed_merge_status_service.rb) and [`app/services/merge_requests/mergeability/check_ci_status_service.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/services/merge_requests/mergeability/check_ci_status_service.rb) | Live: the setting toggled on and off against three merge requests |
| Pipeline-status enum completeness | The live instance's own GraphQL introspection, and [`app/models/concerns/ci/has_status.rb`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/app/models/concerns/ci/has_status.rb) | 13 values on both, against a control query whose known-good types resolved |
| Declared authentication mechanisms, and the incompleteness of the machine-readable route surface | [`doc/api/openapi/openapi_v2.yaml`](https://gitlab.com/gitlab-org/gitlab/-/blob/v19.2.1-ee/doc/api/openapi/openapi_v2.yaml) and the [interactive rendering](https://docs.gitlab.com/api/openapi/openapi_interactive/) | Inspected for declared paths and parameters; live 404 on every instance-served description path |
| Container image size and boot behavior | Docker Hub registry API; local image inspection | Measured pull, boot, healthcheck, and readiness polling |
| Gitea and GitHub adapter behavior used in the comparison tables | The Gitea and GitHub adapter research notes in this directory, and `internal/domain/tracker.go` | Document and code reading at research time |
| Decisions attributed to a shared package rather than to this adapter | `internal/scm/scmcore`, `internal/adaptertest`, `internal/httpkit`, `internal/issuekit`, `internal/registry`, and the `domain.SCMAdapter` and `domain.CIResult` godoc in `internal/domain` | Repository code reading after the adapter-family consolidation. No GitLab observation is involved, so these claims carry a symbol rather than an evidence tag |

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
| Which file contributes `requested_changes` and `security_policy_pipeline_check` to a Community Edition instance's `DetailedMergeStatus` enum? | The Community Edition source file declares 22 values and the running instance serves 24, so the enum cannot be enumerated from the source path alone, and it is unclear whether either value is reachable without a license | Locate the module reached by `DetailedMergeStatusEnum.prepend_mod_with`, and check whether its mergeability checks are license-gated at runtime |
| What is the precedence order among simultaneously blocking `detailed_merge_status` values? | Decides how many poll ticks a merge request needs to clear several blockers, and whether any blocker can be starved indefinitely | Construct a merge request that is simultaneously conflicted, draft, and unapproved, and record which value is reported as each condition is cleared in turn |
| Does `require_sha_for_merge?` vary by namespace or instance, and what governs it? | The adapter always sends `sha`, so behavior is safe either way, but a wrong error message would mislead an operator whose instance does not require it | Read `require_sha_for_merge?` on a second namespace, and on an instance where the merge call succeeds without `sha` |
| Can a reviewer reach `requested_changes` on Community Edition through the web UI, and does the REST `reviewers` route then report it? | The `FetchPendingReviews` selection rests on that state being reachable; only `approved` and `unapproved` were produced through the API | Drive the "request changes" action in the Community Edition web UI, then re-read `GET .../merge_requests/:iid/reviewers` |
| Is the timestamp-and-stream prefix on the job trace a property of the API, the runner version, or a runner feature flag? | Decides whether the trace sanitizer can rely on the prefix being present, or must handle both shapes | Fetch a trace produced by an older runner, and one produced with the runner timestamp feature flag disabled |
| Does the notes route on a merge request paginate and filter identically to the issue variant? | The comment normalization is assumed shared; a divergence would surface as dropped review comments | Seed a merge request with more than 100 notes and repeat the issue-side pagination and `activity_filter` probes |
| Does `GET /users/:id` draw on a distinct rate-limit budget from the merge-request reads? | Bot classification adds one request per distinct author; a stricter budget would change the caching strategy | Compare `RateLimit-Name` on a throttled `GET /users/:id` against a throttled merge-request read on GitLab.com |
| Does self-managed Enterprise Edition behave as its source implies? | Every Enterprise Edition claim here is source plus documentation, never observed | Run the same probe set against a licensed self-managed Enterprise Edition instance |
