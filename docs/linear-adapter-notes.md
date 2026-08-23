# Linear adapter notes

Notes for a developer changing sortie's Linear tracker adapter: what GraphQL costs us that REST does not, where Linear's model and ours disagree, and the traps that are invisible until they bite.

Last updated: 2026-08-23

## GraphQL through a REST-shaped toolkit

Every call is a POST of `{ query, variables }` to one URL, and the response is a JSON envelope. There is no GraphQL client library and none is needed.

Two assumptions in the shared HTTP toolkit do not survive the switch, and the adapter resolves each rather than bending the toolkit. Rate-limit and complexity headers must be readable on success, so the client sends through the variant that returns headers alongside the body. And the pagination cursor travels inside the `variables` object rather than a URL parameter, so the REST token paginator does not apply and the adapter keeps its own loop of the same shape.

Error classification stays in the toolkit's mold: the adapter registers its own error and transport classifiers, and adds a body-level pass that REST adapters have no need for.

## Errors live in the body, not the status

This is the property that shapes the whole adapter. Linear returns application errors inside HTTP 200 bodies. A response is an error if and only if the body carries a non-empty top-level `errors` array, or the HTTP layer itself failed. Entity-not-found and argument-validation both arrive on 200; an invalid credential fails at the HTTP layer and carries the same body shape anyway.

So classification inspects the body first and falls back to the status only when there is no errors array. Any new decode path must run the classifier before it looks at `data`, or it will read a null field and report the wrong failure.

Classify on `extensions.type`, the lowercase phrase, and treat `extensions.code` as diagnostic. Several unrelated codes map onto one type, and the type is what Linear's own client keys on. The rate-limit signal is the single exception, accepted from either field, because Linear reports it under a generic input-error type.

`feature not accessible` is grouped with the auth errors on purpose. It means the workspace plan does not include a capability, which is operator-actionable and never resolves by waiting. Classifying it as an API error would make it retryable and the orchestrator would retry a billing limitation forever.

## Not-found is a message, not a code

There is no dedicated not-found type or code. A missing entity arrives under the same generic input-error type as a bad argument, and the only signal separating them is the message prefix. The message check therefore runs before every type-based rule; move it later and every not-found becomes a payload error, and the orchestrator stops distinguishing a deleted issue from a malformed query.

## The non-null abort, and why batches use filters

The root issue field is non-null in the schema. When its resolver fails, the executor propagates null to the root, nulls the entire `data` object, and abandons the sibling root fields in the same operation. An aliased batch of issue lookups is therefore all-or-nothing: one missing issue wipes the results for every other issue in the batch.

That is fatal for exactly the operations that need batching, since state reconciliation is the place where a deleted or renamed issue is the normal case, not the exception. Both state-batch operations use connection filters (`id: { in: ... }` and a team plus number filter) instead, where a missing issue is simply an absent node. Never rewrite them as alias batches, however much cheaper it looks.

## Team key, not project

`tracker.project` holds a Linear team key, not a Linear project. Three reasons, and they all still hold: workflow states are team-scoped, so the state model this adapter is built around is only well defined relative to one team; the team key is the visible prefix of every issue identifier, which makes it the least surprising thing for an operator to configure; and Linear projects are cross-team containers that own neither states nor identifiers, so filtering by one would still need a team for state resolution.

Reads filter by team key directly, so no team UUID lookup is needed. Only the label-create path needs the UUID, and it resolves one per call.

## States are team-scoped names with a category attached

Operators configure state names and the adapter treats them as opaque strings, matching the other adapters. The category that Linear attaches to each state is not used for selection, because a team can hold several states in the same category and the category alone cannot tell them apart. It is used as a tripwire: an active state whose category closes an issue, or a terminal state whose category does not, gets a warning and no failure.

The startup preflight fetches the team's states once, fails construction when a configured name or the team key does not exist, and caches each name's canonical casing. That cache is mandatory, not a nicety: the state filter's `in` comparison is case-sensitive and has no case-insensitive counterpart, so a name whose casing drifts from Linear's returns zero issues, silently, forever. Every fetch sends canonical-cased names for that reason.

Transitions take a state UUID, never a name, so the transition path resolves the name against the issue's own team in the same query that fetches the issue. Resolving through the issue rather than a cached team map is what keeps a multi-team workspace safe. There is no transition graph: any state can move to any state, so resolve-then-update is the entire flow, and an empty resolution means the name does not exist in that team rather than that the move is disallowed.

## Identifiers: three values, and the filter accepts two

An issue carries a UUID, a human identifier such as `ENG-123`, and a number that is unique only within its team. The UUID is the domain ID and the human identifier is the domain identifier. Single-issue queries and the update mutation accept either form, and nothing in the response says which one was used, so the adapter passes verbatim whichever form it was given and never derives one from the other.

The issue filter has no identifier field. Identifier batches therefore split the trailing integer off each identifier and filter by the configured team plus that number set, skipping anything whose trailing part is not an integer. The team half of the identifier is not read, because every identifier the orchestrator passes belongs to the configured team. Before assuming an identifier filter has appeared, check the published schema or the endpoint's own introspection rather than this paragraph.

## Writes: append fields, never replace fields

The issue-update input exposes both a replace-style collection field and an append/remove pair for the same collection. Writing through the replace field means reading the current set first, and because that set is itself a paginated connection, the obvious implementation reads one page, writes back what it read plus the addition, and silently deletes every member beyond the first page.

The adapter never uses the replace form. Labels are attached through the append field, which needs no read, has no pagination, and leaves no race window. Any future write to a collection-valued field must audit the schema for an `added*`/`removed*` pair before reaching for the replace-style name, and any code that genuinely must replace a set has to paginate that connection to exhaustion first, missing-cursor guard included.

Mutations carry one shared result policy: a non-empty errors array is a failure whatever the payload's success flag says, and a success flag of false with no errors maps to an API error. The adapter never consumes partial data; a populated `data` alongside errors still fails the call.

## Labels

Attaching a label by name is a compound operation, because Linear attaches labels by UUID. Resolve the name, create the label when nothing matched, then append the id.

Resolution must compare the configured project against the label's team **key**. The label's team object also carries a UUID, and matching a team key against that UUID field never matches, which silently demotes every team-scoped label to the workspace-scoped fallback. That bug is invisible in tests that use only workspace labels.

Label creation always passes a team id, so the label is team-scoped: workspace-level label management is likelier to need elevated access. A concurrent create is expected rather than exceptional, so any payload-class create failure triggers one re-resolve, and only a re-resolve that still finds nothing surfaces the original error.

Label creation is also gated by a team-level permission setting that decides whether all members or only owners may create labels, so a correctly scoped key can still be refused. That is the one operation a well-configured key can fail. The remedies are to flip that team setting or to pre-create the escalation label once, after which resolution finds it and the create path never runs. Label failures are non-fatal to the orchestrator either way. Linear's own permissions documentation is the place to check what the setting is currently called.

## Pagination

Every connection is a Relay cursor connection, and one loop walks all of them: send `first`, read `pageInfo`, stop when there is no next page.

Cursors are opaque. Their encoding is not even uniform across connections in one response, so pass an end cursor back verbatim and never parse or construct one.

A page that claims another page while omitting the end cursor raises a missing-cursor error rather than ending the walk. Silent truncation here is data loss, and the by-id read applies the same guard to its inline first page of comments before handing the cursor onward.

A caller may seed the cursor to resume a connection past a page it already holds; the loop preserves that seed instead of restarting from the beginning. That is what keeps the inline comment page from being fetched twice and duplicated, so a refactor that resets the cursor breaks comment collection in a way no single-page test notices.

Nested connections inside an issue node are capped at their first page and are not paginated. Truncation is logged with the issue and the connection named, which keeps a dropped label or blocker observable instead of imaginary. The cap is cheap insurance against a pathological issue, not a complexity-budget necessity.

Never depend on server-side ordering. Connection order and the priority sort argument are both treated as hints: dispatch order comes from a client-side sort by normalized priority then creation time, and comments are re-sorted into chronological order after collection because agents read them as a narrative.

## Field mapping

Priority is a small integer where zero means no priority, so zero and null both normalize to nil rather than to a top-priority value.

`branchName` is always present and always opaque. Its format is a workspace-level template, and the default prefix derives from the acting user's handle, so nothing may parse it or assume a shape.

The assignee is strictly a human user. Agents and applications surface through separate fields the adapter does not read, so an agent-driven issue with no human assignee normalizes to an empty assignee rather than to the agent's name. Comment authors have the same split, which is why author resolution falls back through the bot actor before giving up; comments originating in a chat integration expose their author in a field the adapter does not select and resolve to the bot name or to empty.

Blockers come from the issue's inverse relations. The relation record lives on the blocked issue, and the blocker is the relation's source issue, so reading a blocked issue gives you the blockers directly. The relation type is compared case-insensitively after trimming, and the blocker slice is always non-nil.

Descriptions and comment bodies are markdown and pass through untouched. There is nothing to flatten and no wrapper to build on the write side either.

What the adapter writes is stored verbatim, and that includes text it might be tempting to treat as rich. An interactive mention is something Linear's editor produces while a human is typing or pasting, not something the stored body encodes, so an issue or user URL an agent puts in a comment reaches human readers as a plain link. Never compose comment text whose meaning depends on a mention rendering.

## Rate limits

No quota may be hardcoded. Linear derives the request limit from the size of the workspace, scaling it with the number of paid seats, so the published headline figure describes no particular workspace and the limit your test workspace reports will not be the limit an operator's workspace reports. The response headers are the only source of truth for the workspace you are actually calling.

There is also a complexity budget, and the response reports the cost of the query just executed. That header, not any published scoring formula, is the authoritative cost. The formula is a worst-case upper bound because it multiplies each connection's cost by the page size you asked for, while the charge tracks the rows actually returned, so a query that requests a large page and matches few issues costs a fraction of its estimate. Sizing a query against the formula therefore leads you to shrink pages that were never expensive.

The adapter's rate-limit behavior is purely observational. It logs once when the request window is exhausted and otherwise does nothing: classification marks the condition retryable and the orchestrator owns the delay. The one delay the adapter does apply is at construction, where a transient preflight failure is retried on a bounded backoff before construction fails.

One trap in the headers: the reset values are epoch milliseconds, not seconds. Divide before comparing against a Go time value, or you will report a reset date thousands of years out.

## Config and validation

The API key is sent verbatim in the `Authorization` header with no scheme prefix. A `Bearer` prefix is rejected, and it is the most common integration bug with this API. Surrounding whitespace fails authentication for the same reason, which is why the offline validator warns about it and about a key missing the expected prefix. No diagnostic ever contains the key.

The endpoint defaults to Linear's single hosted GraphQL URL; there is no self-hosted Linear, so an override serves tests and mocks only. A present value must parse as an absolute http(s) URL with a host, and the failure message carries only the userinfo-stripped form: the standard URL parse error quotes the whole raw URL, which would print a password into the log.

`tracker.query_filter` is a JSON object merged into the fetch filter as extra sibling fields, which Linear ANDs with the adapter's own constraints. The top-level team and state keys are rejected so an operator cannot widen the team or state scope out from under the state model, the fragment travels in the variables object rather than the query text, and the filter map is rebuilt per call so concurrent fetches never share it.

Configured state names are validated online, by the preflight, not offline. The offline validator only checks shapes it can decide without a network call: endpoint form, a team key that looks like a team key rather than an owner/repo path, and state list elements. Empty or padded state names are errors here rather than warnings, because construction rejects them.

Writes are ordinary data changes, so they emit whatever webhooks and automations the operator has configured in their workspace. Sortie polls and consumes no webhooks itself, so this is an operator-awareness note rather than a dependency.

## What to verify when you change this adapter

The integration suite is gated on `SORTIE_LINEAR_TEST=1` and skips cleanly without it. It needs `SORTIE_LINEAR_API_KEY` and `SORTIE_LINEAR_TEAM_KEY`, and optionally an endpoint override and an active-states list. A key restricted to the configured team, with permission to read, to write, and to create comments, covers every operation except creating a label.

The workspace behind it needs a team whose states cover the configured active, terminal, and handoff names, and at least one issue carrying a label, a comment, a blocking relation to another issue, and a sub-issue, since those are what exercise the normalization paths. Write tests are shaped to leave the workspace as they found it, apart from one comment test that deliberately leaves a timestamped trace.

For schema shapes, filter fields, enum values, scopes, and quota figures, use the endpoint's own introspection, the published schema, Linear's developer documentation, or Context7. Any of those beats a value copied into this file, and the introspection response beats all of them, because it describes the deployment you are actually calling.
