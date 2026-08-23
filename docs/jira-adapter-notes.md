# Jira adapter notes

Notes for a developer changing sortie's Jira tracker adapter: the decisions it encodes, the places Jira's model does not line up with ours, and the failure modes worth recognizing before you spend a day rediscovering one.

Last updated: 2026-08-23

## One adapter, two products

`tracker.api_version` picks between Jira Cloud and Jira Server / Data Center, and the choice reaches almost every layer: the REST base path, the search resource (the one route whose path is not simply the version number swapped), the type of the description and comment body, and the search pagination model. Treat any change to a read path as two changes until you have checked both arms.

The version doubles as a claim about the deployment, so the constructor cross-checks it against the endpoint host before any network call. A Cloud host asking for the Server version is rejected outright; a non-Cloud host asking for the Cloud version is warned about and allowed, because we cannot enumerate every legitimate hosted domain. Loopback and localhost are exempt from the warning: they are test targets, never real deployments.

## Authentication rides in one config field

`tracker.api_key` carries both schemes. A value containing a colon is `user:secret` and becomes Basic; a colon-free value is a personal access token and becomes Bearer. Encoding the credential pair in a single field follows the curl convention and keeps Jira-specific keys out of the core config schema. The cost is that a malformed key is detectable only by shape, so the shape rules live in one place and both the constructor and the offline validator read them from there.

The rule keys on the version, not the host. A self-hosted endpoint left at the default version rejects a personal access token exactly the way a Cloud endpoint does, and the remedy is either an `email:token` key or an explicit version. That combination surprises operators often enough that the error message has to name both halves of the remedy.

OAuth is not implemented and should not be. Sortie is a headless background service with no interactive authorization flow and no user-facing callback.

## The offline validator re-decides, it does not re-implement

`sortie validate` must reach the same verdict as construction without a network call or an adapter instance. It does that by calling the constructor's own helpers and reusing its message text, so a fix on one side cannot drift from the other. Two rules are easy to break by accident: an invalid version suppresses the host/version conflict diagnostic, because the constructor never reaches that guard for a version it has already rejected, and the api_key value never appears in a diagnostic message.

## Identifiers

Jira hands you a numeric internal ID and a project-prefixed key, and JQL matches them through different fields. Passing one where the other belongs does not error. The by-ID reconciliation query drops every element that is not all digits before building its clause, so a caller bug surfaces as absent entries in the result map rather than as a query matching the wrong issues, and a batch with nothing numeric left issues no request at all. When reconciliation quietly stops noticing state changes, suspect the identifier form before you suspect the query.

Both batch lookups chunk their `IN` clauses on every call, not only for large sets, because they run as GET requests and a long clause pushes the URL past the deployment's URI limit. That limit belongs to the deployment, not to us, so the chunk size is a safety margin rather than a tuned value.

Every interpolated project key, state name, and issue key passes through a quote stripper first. JQL delimits string literals with double quotes and offers no backslash escape for them, so there is no escaping to get right here, only deletion.

## The reconciliation queries ignore query_filter on purpose

`tracker.query_filter` is a raw JQL fragment the adapter neither parses nor validates. It is ANDed into the candidate and by-states queries and deliberately left out of the two batch state lookups: those issues already passed filtering at dispatch time, and re-applying the filter would make a running session invisible the moment an operator narrowed the fragment.

The fragment is wrapped in parentheses when it is appended, and that is load-bearing rather than tidy. Without them a fragment containing a top-level `OR` binds loosely enough to widen the query past the project and state constraints the adapter just built, and an operator would be selecting issues sortie was never configured to touch. Parenthesization is the only containment there is, since nothing parses the fragment.

## Field mapping traps

Priority reads the priority object's id, never its name. Names are workflow text an admin can edit; the id is the numeric scheme the domain's integer priority expects. A non-integer id yields no priority rather than a guess.

Timestamps are stored verbatim as strings and the adapter parses nothing. Anything downstream that does parse them must accept Jira's millisecond-and-offset form as well as RFC 3339. Do not assume RFC 3339 alone.

`BranchName` is always empty. The core REST issue payload does not carry it, and the only source is the development-information surface at `/rest/dev-status/`, which answers only when a source control tool is connected to the site. Note the path: it sits outside the versioned `/rest/api/` tree, which is why searching the REST API reference for a branch field turns up nothing and why people conclude the data does not exist. Nothing in the adapter calls it, and anything that starts to should treat it as a surface with its own rules rather than another issue route.

Comments never arrive with a search result. The candidate fetch leaves `Comments` nil by contract, and the by-id fetch pays at least one more request to fill them.

## Blockers hang on a name an admin can change

The adapter keeps issue links whose type name equals "Blocks" and whose inward side is present, and builds blocker refs from that inward side. A link carrying only the outward side is skipped, so a dependent issue is never mistaken for a blocker.

The comparison is exact and against a compile-time constant. Rename that link type in Jira and every blocker silently vanishes from the adapter's view, after which blocked issues dispatch as though nothing were blocking them. There is no error and no warning. When an operator reports that sortie started work on a blocked issue, check the configured name of their link type first, and check which side of the link their instance puts the blocking issue on before changing the extraction.

## Bodies differ by version, and one of them can lose text

On Cloud, descriptions and comment bodies are Atlassian Document Format node trees, and the adapter flattens them to text. Skip the flattening and raw JSON travels straight into an agent prompt.

The flattener emits only `text` nodes and appends a newline after block-level ones. A node that carries its content somewhere other than a `text` child (mentions, inline cards, emoji, and whatever Atlassian adds next) contributes nothing and reports nothing. Content loss here is silent and it lands in a prompt, so if an agent seems to be missing part of a description, compare the flattened output against the rendered issue before looking anywhere else.

On Server the same fields are plain strings carrying wiki markup, passed through verbatim with no translation, so agents see the raw markup tokens rather than clean prose. That is deliberate: translating a markup dialect is not the adapter's job, and the rendered-HTML expansion is not requested.

Comment creation mirrors the split. On Cloud the text is split on newlines into one paragraph node per line, because a single node with embedded newlines does not render as line breaks.

## Pagination

Cloud search is cursor based, Server search is offset based, and comments are offset based on both. A read-path change usually touches three loops.

One known weakness: the cursor walk treats an absent cursor as the end of the walk, and the decoder never reports a missing one, so a result set truncated by a dropped cursor is indistinguishable from a complete one. The Linear adapter raises a missing-cursor error in the same situation; this one does not.

## Rate limits, retries, and errors

Jira's quotas are not ours to encode. The adapter retries nothing on its own: a rate-limited response classifies as a retryable API error and the orchestrator owns the delay. `Retry-After` survives as message text only, because the domain error type has no field for it. When you need an actual quota, a bucket, or a header name, read Atlassian's rate-limiting documentation for the deployment you target rather than any number written down here.

Auth failures can turn sticky. After repeated failed logins Jira starts demanding a browser login and announces it in the `X-Seraph-LoginReason` response header; that header name is the thing to grep for in a log when a credential that should work does not. The adapter names the challenge in the error message but keeps the kind at auth, since the header changes the remedy rather than the classification. An operator who "already replaced the token" and still gets 401s needs the browser step.

Both 401 and 403 classify as auth. A 404 is re-wrapped at the issue and comment call sites so the message names the issue key instead of the route. A JSON decode failure on a 200 is a payload error naming the response that failed to parse. Response bodies are copied into error messages for diagnosis, bounded to a small prefix; keep that bound, and keep credentials out of anything that reaches a message.

## An interface method with no caller

`FetchIssuesByStates` exists to satisfy the tracker interface. Startup terminal cleanup uses the identifier-batch lookup instead, and nothing else calls it. Do not infer behavior requirements from it that no caller actually relies on.

## What to verify when you change this adapter

The integration suite is gated on `SORTIE_JIRA_TEST=1` and skips cleanly without it. It needs `SORTIE_JIRA_ENDPOINT`, `SORTIE_JIRA_API_KEY`, and `SORTIE_JIRA_PROJECT`, and optionally an active-states list and a query filter; the credential must be able to transition, comment on, and label an issue in that project.

Nothing in CI sets an API version, so every live run exercises the Cloud arm at the default version and the Server / Data Center arm has no live coverage anywhere. If you touch that arm, point the suite at a Data Center instance yourself or you are shipping untested code.

The unit fixtures are hand-authored, not captured payloads. They pin our decoding of a shape someone believed Jira returns, which makes them useless as evidence about Jira: a fixture and the real API can agree with each other and both be wrong.

For anything about routes, JQL grammar, ADF node types, OAuth scopes, or quotas, go to Atlassian's REST API reference for the version you target, or Context7, rather than to this file. The same discipline applies at runtime: read the accepted transitions off the issue's own transitions resource before relying on any list of state names, because the workflow that governs them belongs to the project, not to us.
