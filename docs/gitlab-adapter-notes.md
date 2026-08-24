# GitLab adapter notes

What to know before you change `internal/scm/gitlab`: the design decisions behind it, the places GitLab's model does not line up with ours, and the traps that cost someone a day.

Last updated: 2026-08-23

## Shape of the package

One package registers three kinds under the name `gitlab`: the tracker, the SCM adapter, and the CI status provider. GitLab is a full forge, and its issue half and merge-request half share their authentication, project addressing, pagination, error envelopes, comment entity, and label-event journal. Splitting them across packages would have meant re-declaring all of that twice, which is why they live together.

The `internal/scm/` boundary rules apply unchanged: no cross-adapter imports, no importing the orchestrator, and every external response normalized to a domain type before it leaves the package. The orchestrator and `cmd/sortie` reach all three kinds through `internal/registry` and never call into this package directly.

There is no GitLab client library in the dependency set and there should not be one. Transport, the link-header paginator, the page-number paginator, and preflight retry come from `internal/httpkit`; label-state derivation and normalization from `internal/issuekit`; offline validate primitives from `internal/registry`; conformance assertions from `internal/adaptertest`. GitLab's REST is not GitHub-shaped, so the only overlap with the sibling adapters is transport-shaped and already lives in `httpkit`.

### Decisions this adapter does not make

`internal/scm/scmcore` owns the verdicts every forge half shares, and the adapter supplies only the wire read and the normalization. That split is what stops one forge from answering the same question two ways.

| Decision | Owner |
| --- | --- |
| Which conclusions count as failing | `scmcore.IsFailingConclusion` |
| The aggregate CI verdict and the failing count | `scmcore.AggregateCIStatus`, `scmcore.FailingCount` |
| The merge-gate CI answer | `scmcore.MergeGate` |
| Tracker error to SCM error conversion | `scmcore.ToSCMError` |
| Promotion of a merge rejection to a conflict | `scmcore.AsMergeConflict` |
| The already-merged marker phrase | `scmcore.AlreadyMergedConflict` |
| Bot-author classification | `scmcore.IsBotAuthor` |
| Label-event id ordering | `scmcore.SortableEventID` |

Two consequences shape the merge path. A merge conflict is reachable only from the merge write call, because the generic error conversion promotes nothing and the promotion helper has exactly one caller. And the already-merged marker is *constructed* after a confirming re-read, never matched out of a response body.

`internal/adaptertest` holds the conformance assertions the suite calls rather than restates. One of them recomputes the CI aggregate and the failing count from the returned check runs, so a provider that sources either from anywhere else fails the suite. That assertion exists because sourcing the aggregate from GitLab's own pipeline status is the obvious shortcut and it is wrong.

## The three products, and why the distinction has teeth

GitLab ships as three things, and conflating them is the central correctness hazard here:

- **Self-managed Community Edition** is the compatibility floor. The adapter depends only on what it provides.
- **Self-managed Enterprise Edition** is a superset gated by license tier.
- **GitLab.com** runs the Enterprise codebase gated by namespace subscription plan. A Free namespace on GitLab.com is **not** Community Edition, and a suite green against GitLab.com proves nothing about the floor.

The same missing capability produces structurally unrelated failures across the three. Blocking issue relationships are the clearest case: on the Community floor the relation type is simply absent from the parameter's accepted values, so the request fails **parameter validation**; on a Free hosted namespace the value exists and the license check rejects it, so the same request fails with an **authorization-shaped** error. An adapter that classified the second as a credential problem would tell the operator their token is broken when the truth is a licensing gap.

**A Community Edition lab is blind to a whole class of responses.** License-gated enum values, tier-gated parameters, and Enterprise-only entity fields cannot be produced there at all. Anything you cannot reach on the floor is a claim you have not tested, and the honest thing to do is say so rather than assume the shapes match.

The reverse blindness is real too. The pinned lab has been observed serving Enterprise-only GraphQL types while reporting itself as Community Edition, so the instance's own schema is not a reliable statement about what its *checks* can emit. Schema membership describes the surface; it does not describe what a licensed code path will actually produce.

## GraphQL is a research instrument here, not a transport

Every tracker and forge operation maps to a REST route, and two of them are cheaper on REST than they would be anywhere else. So the adapter stays on REST.

GraphQL is still worth reaching for when you need to know what values a field can take, because a running instance will introspect its own schema for you, version-exact for the box under test. But there is a trap in doing that:

**The GraphQL enum is not the REST wire vocabulary.** For the mergeability status the two vocabularies agree on most values and diverge on several, with independently chosen identifiers rather than one being a lowercase rendering of the other. Building a REST-side value set out of GraphQL introspection quietly produces names the REST surface never sends. The adapter's expected-value set is therefore derived from the check services' own identifiers and cross-checked against the REST reference, and there is a comment in the code saying so, because "correcting" a spelling toward the GraphQL name is exactly the edit that would reintroduce the bug.

**A per-instance OpenAPI description does not exist.** GitLab publishes a Swagger document in its documentation tree, but a running instance does not serve it, and that document omits most of the routes this adapter depends on. Do not plan a verification method around it the way the Gitea work reasonably could.

## Identifiers and project addressing

**Two integers, one usable.** A GitLab issue carries a project-scoped internal id and an instance-global id, and only the project-scoped one is accepted by the per-issue routes. The global id is not merely redundant, it is inaccessible to an ordinary project member. Both the domain ID and the domain identifier map to the project-scoped value, which is why the two state-lookup methods share one implementation.

The fixture consequence is sharp and easy to miss: **on the first project of a fresh instance the two numbers coincide**, so a single-project test fixture cannot catch code that confuses them. The lab seeds a second project *first*, precisely so the primary project's issues have divergent values.

**A project path can carry any number of slashes.** GitLab has nested subgroups, so the one-slash `owner/repo` grammar the GitHub and Gitea validators enforce is wrong here. Two prohibitions follow, and both are the kind of assumption a sibling implementation carries in by habit:

- Never validate the owner half as a single path segment and never split it on a slash.
- Never encode the two halves separately and then join them, which encodes the separator between them and leaves the separators inside the owner unencoded.

Join first, percent-encode the whole thing exactly once. The GitLab validator deliberately does **not** call the shared one-slash project diagnostic that its siblings call; reaching for that helper by reflex reintroduces the rule this rules out.

The two failure modes are usefully distinguishable in the response: an unencoded path fails to match any route, and a double-encoded path matches the route and finds no project. The error envelopes differ accordingly (see the error model below), so a logged body tells an encoding bug from a configuration mistake without further probing.

## State model, and the label traps

GitLab issues are natively open or closed with no workflow engine, so the adapter derives state from labels, the same convention the GitHub and Gitea adapters use. The derivation rule itself lives in `internal/issuekit`; the adapter contributes only GitLab's spelling of the two native statuses and the wire decode.

**A whole transition is one request.** The issue update route takes the state change and the label additions and removals together and applies them atomically, so there is no window in which an issue carries a terminal label while still open. Gitea needs up to five requests for the same outcome. When both lists name the same label, removal wins.

**Labels are created on demand by an attach.** Naming a label that does not exist creates it as a project label and attaches it. That removes the label-hygiene problem that forced the Gitea adapter into an explicit create-on-missing policy: no name-to-id resolution, no separate create call, no silent no-op.

**And that auto-creation turns case sensitivity into silent data corruption.** Label names are case-sensitive. Attaching a case variant of an existing label does not match it and does not error; it creates a second label and leaves the issue carrying both. Our normalization lowercases labels, so the derived state still looks correct while the project has quietly grown a phantom label, and a removal for one casing leaves the other attached, so state labels accumulate. Nothing in the API reports any of this.

The mitigation is normative and lives at construction: **resolve the canonical stored casing of every configured state label against the project's label list, and send that casing on every write.** A configured name matching nothing is the operator's intended new label and may be created by the first write; a name matching case-insensitively but not exactly must be rewritten to the stored casing.

**Do not use the whole-set label parameter.** It *replaces* the label set. Only the additive and subtractive parameters are safe.

**Scoped labels do not enforce exclusivity on the floor.** The `key::value` convention is the obvious mechanism for mutually exclusive workflow states, and its automatic exclusivity is a paid feature. On the Community floor the separator is just two characters in a name, and it fails open rather than loudly: both values coexist on the issue. So the adapter always removes the previous state label explicitly, and operators may use the convention for readability without the adapter's behavior depending on it.

**GitLab performs no concurrency control on issue updates.** Two simultaneous writers with opposing label deltas both succeed, with no conflict signal, and interleave nondeterministically. Two writers against one project silently corrupt each other's state; there is no server-side remedy to reach for.

## Blockers are structurally unavailable on the floor

The blocking relation type does not exist on Community Edition; only an undirected "relates to" edge does. The blocked-by list is therefore always empty, and it must not be populated from the undirected relation: that edge carries no direction and no blocking semantics, and a blocker whose state is unknown is treated as non-terminal, so inventing blockers suppresses dispatch for merely cross-referenced issues. Inventing is the more harmful error here.

This is **declared, not inferred**. The registration states that this tracker has no blocking relation to carry, so the shared dispatch resolver reads the empty list as complete rather than as unread, and no candidate is ever marked blockers-unresolved. If you ever wire the Enterprise relation types, that declaration is the thing to change first, not the normalizer.

## The silent parameter trap

**GitLab ignores unrecognized query parameters.** A misspelled filter key does not error; it disables the filter. A typo in a narrowing filter widens the candidate set and dispatches agents against issues the operator meant to exclude, and nothing in any response shows it.

The trap also catches parameters that exist in GitLab's *interface* but not on the REST route. An OR-label filter is the standing example: it looks plausible, it is undeclared on the route, and it is inert.

Invalid *values* on *recognized* keys behave the opposite, safe way and fail loudly. So the danger is confined to key names, which is what makes the mitigation possible: **the operator's query filter is validated against an allowlist of known keys and rejected at construction on an unknown one.** This is stricter than the Gitea adapter, which reserves only the keys it owns, and the stricter rule is required because GitLab's failure is silent in both directions.

The adapter also rejects the keys it owns outright, since an override of those changes correctness rather than scope, and it merges the filter only into the open half of the state lookup, never the closed half, so an operator filter cannot hide a terminal issue from reconciliation.

## Candidate and comment reads

Three parameters on the issue list route are load-bearing rather than decorative, and omitting any of them is a correctness bug rather than a performance one:

- **The list route's default state is "all", not open.** Omitting the state parameter puts terminal issues in the candidate set. And the value is GitLab's own spelling, not GitHub's.
- **The issue type filter excludes non-issue work items server-side.** GitLab's issue list returns tasks, incidents, and test cases alongside issues. Without it the orchestrator dispatches agents against checklist items. This is the direct analogue of Gitea's pull-request exclusion.
- **Ordering is requested server-side** so the orchestrator gets oldest-first candidates with no client-side re-sort, matching the GitHub adapter.

State filtering stays client-side. The server-side label filter is AND across names, and Sortie needs OR across several active states; GitLab offers no OR label filter on this route.

**The notes route mixes system notes in with human comments.** Passing a "marked as related to" system note to an agent as human feedback would pollute every continuation prompt. There is a server-side filter parameter for this, and it fails loudly on a bad value, so it cannot be silently dropped by a version that lacks it. The adapter still keeps a client-side guard on the per-note system flag, because the field is authoritative and the check is free.

Two more properties of that route: it defaults to **newest-first**, so ascending order is requested explicitly, and it **paginates**, unlike Gitea's equivalent.

Internal notes are passed through. They are genuine human comments, and an operator who does not want them in prompts controls that by not writing them.

**There is a true batch state lookup**, which neither sibling adapter has: one request resolves many issues by their internal ids. Reconciliation for N running issues costs one request instead of N. Two caveats: the batch must request all states so a just-closed issue is still reported, and it must be **chunked**, because the request is a query string and a long enough one meets a URI-length limit imposed by the front-end web server rather than by GitLab.

## Pagination

Both offset and keyset modes advertise the next page in a standard link header carrying an absolute URL that preserves every parameter, so following that link handles both with no mode-specific code.

**Drive pagination from the next link, never from a total-pages count.** Above a size threshold GitLab stops emitting the total headers and the last link entirely. Counting up to a total silently truncates on exactly the large projects where truncation matters; the next link is unaffected.

Keyset mode is the correct choice for a large mutating collection, since offset pagination can skip or repeat rows when the set changes between pages. The adapter does not use it by default because the candidate query is filtered, single-project, and ordered by creation time. It is recorded here as the available remedy if a deployment ever shows pagination drift.

One caveat for proxied self-managed deployments: the link URLs are absolute, and whether GitLab builds them from the incoming request or from the instance's configured external URL is unsettled. On a deployment where the two differ, following the link could address the wrong host.

## Rate limiting

The cost model is the opposite of what the hosted behavior suggests. **General API throttling is off by default on self-managed**, and no rate-limit headers appear at all in that configuration. Several *granular* limits are on regardless, and the one that touches this adapter throttles note creation.

On GitLab.com throttling is on and observable, and **the hosted product is the stricter one for note creation** by a wide margin. A deployment tuned against a self-managed instance can hit the comment limit on GitLab.com.

Three rules for the adapter:

- The header names are the IETF-style ones. GitHub's `X-`-prefixed names do not transfer.
- Treat every rate-limit header as optional. They are absent entirely on a stock self-managed instance.
- **A rate-limited response body is not guaranteed to be JSON.** The response text is an operator-writable instance setting. Classification must not require a parseable body.

Apply no preemptive throttling on self-managed, where poll cadence is the only pressure control. The remaining-requests header, when present, is the signal for a hosted deployment.

## Error model

**There is no single error envelope.** Several distinct shapes coexist: an application-level message key, a parameter-validation error key, an OAuth-style pair of an error and a description, and a message key carrying an embedded rendering of model errors. An adapter that reads only one of them logs empty diagnostics for half of its failures. Read both the message and the error key, prefer whichever is present, append the description when there is one, and tolerate a non-string message without failing the whole response.

A useful secondary signal: the parameter-validation envelope marks a **route** that did not match, while the application envelope marks a **resource** that does not exist. That distinction separates an encoding bug from a configuration mistake, and it separates a Community-Edition-absent route from a permission failure. Use it for diagnostics; do not build control flow on it, since it is a byproduct of the framework rather than a documented contract.

**A not-found on a project-scoped route has three causes and one of them is an authorization failure.** GitLab masks the existence of private resources rather than returning a permission error, so "project does not exist", "your identity is not a member", and "no token at all" are byte-identical. The security property is sound and the adapter cannot defeat it. Two mitigations:

- Distinguish project-level from issue-level not-founds by their message. Only the issue-level one is the not-found the interface contract means.
- **Run the construction-time project preflight.** Without it a wrong project or an unauthorized token produces a permanent stream of not-founds at poll time with no hint where the blame belongs.

Note the asymmetry with a bad credential: an invalid *token* is rejected as unauthenticated, while a valid token lacking *access* is rejected as not-found. A not-found can never be ruled out as an authorization problem.

**The dangerous failures are the successful ones.** An unrecognized query parameter disabling a filter, and a case-variant label attach creating a duplicate, both return success with the wrong result and no status to key on. Neither is caught by status mapping; both are prevented by the adapter's own validation, which is why both mitigations above are normative rather than advisory.

## Mergeability is one value that masks and recomputes

The detailed merge status names **one** blocking condition at a time, the precedence between them is neither documented nor guaranteed, and it recomputes: approving a merge request flips it into a computing state and back. Four rules follow, and together they are what make the mapping safe.

1. **Treat the value as sufficient evidence that merging is blocked, never as necessary evidence about any particular blocker.** Never conclude "no conflict" from a value naming a missing approval.
2. **Derive nothing else from it.** The review decision comes from the approvals fold and the CI conclusion from the pipeline read, so a masked value cannot corrupt either.
3. **The mapping must have a default arm, and that arm is "blocked".** The value set is demonstrably not closed: the instance serves values the Community source file does not declare. Every value outside the affirmative and computing groups is a blocking reason, so an unfamiliar value from a newer instance is far more likely to be a new blocker than a new computing state. Both arms re-enqueue, so this choice is about not misreporting a permanent blocker as transient.
4. **The computing values must map to unknown, not to blocked or clean.** Unknown is a deferral the reconcile loop re-enqueues on. Classifying a computing state as clean would let the loop merge on a stale computation.

The merge endpoint is the backstop: it re-evaluates every precondition server-side, so a stale clean read cannot merge a blocked merge request.

The unstable mergeability state is never emitted, by design rather than omission. A pipeline whose only failing job is allowed to fail reports overall success, so the merge request reads as clean. The state machine treats clean and unstable identically, so nothing is lost.

## Review decision

**The Community Edition approvals payload cannot say whether review is required**, only whether an approval happened. The fields that would express a requirement are Enterprise fields, and every richer route is Enterprise-only and simply does not match.

Two of the four fields it does return are **computed for the calling token**, not for the merge request: the same merge request reports one thing to the approver and the opposite to a different token in the same second. Ignore them. Only the identity-independent approval flag and the approver list are usable.

So the decision is folded from the approvals payload plus the per-reviewer review states, in the same spirit as the Gitea fold. Changes-requested is tested first, so a later approval by a second reviewer cannot silently clear an outstanding block.

The load-bearing arm is the last one: with no reviewer assigned, the decision is **not-required**, which lets auto-merge proceed. That is a deliberate widening of the merge gate, and it is forced: the floor has no approval rules, so it can never *require* an approval, and a merge request with no reviewer is genuinely unreviewed rather than pending. An operator who wants review enforced on the floor must assign a reviewer, because the platform offers no server-side alternative.

**The two `state` fields trap.** The merge-request object's reviewer array holds bare user objects whose state is the user's *account* state. The dedicated reviewers route returns a different entity where the user is nested and the top-level state is the *review* state. Both fields appear in one response and mean different things, and the outer object nests the very field name it shadows. Reading the account state where the review state was intended is easy and silent.

**The review state cannot be set through the API**, on REST or GraphQL. Reading works. That means an integration test cannot drive a merge request into changes-requested at all: it must assert on the two states approving and unapproving can produce, or the state must be set through the web UI.

**GitLab has no review object bundling a verdict with a comment set.** Selection is two reads: fetch the reviewer states, then keep the notes authored by a reviewer in changes-requested. The author login is the only join key. The degradation to state plainly: the result is *everything that reviewer ever wrote on the merge request*, not the comments belonging to one review round. Do not claim finer granularity than that.

GitLab exposes no outdated flag on a note, so that field is derived by comparing the note's anchored head commit against the merge request's current one.

## CI: the head pipeline is an association, not a derived field

This is the most expensive thing in the file to get wrong.

**Nothing re-validates the head pipeline against the merge request's current head.** A push that creates no pipeline of its own leaves the field pointing at a superseded commit, still reporting whatever that older pipeline concluded. The platform's own mergeability checks use a *different* association, the same one narrowed to nothing when it does not match the current head. The divergence also opens transiently on the ordinary path, for around a second after a push that does create a pipeline, and it reproduces on the hosted product too, so it is not a floor-only artifact.

So every read that folds the head pipeline's job set addresses the **pipeline's own** commit and id, never the merge request's, and the CI read compares the two commits before classifying anything. On a mismatch it defers as pending rather than report a verdict computed for a commit nobody is merging. That deferral is recoverable on the next poll, not terminal.

Two shapes are exempt from that comparison: a merged-results pipeline and a merge-train pipeline generated for the merge request being read. Both run on a temporary ref whose commit exists in neither branch, so their commit can never equal the merge request's, and no field on either the embedded object or the pipeline detail route relates the two. **The exemption tests the platform-set source together with the exact ref anchored to this merge request, never the ref alone**: a branch can be literally named to imitate a generated ref, and the platform reports the ref verbatim. Both exempt shapes are paid features, so a Community deployment cannot produce either, which is another thing a floor-only lab cannot show you.

**The head pipeline is populated on the detail route only.** The list route returns it as null for every merge request. A fold sourcing it from a list response would find no pipeline anywhere and answer "no CI gate" for everything, which the auto-merge reconciler treats as merge-eligible.

### Why the platform's own aggregate is used, with one exception

GitLab unifies pipelines and external commit statuses in both directions: posting an external status to a commit that already has a pipeline folds into that pipeline and can change its status, and posting one to a commit with no pipeline creates one. So for almost every pipeline status the platform has already done the fold, and reading it costs nothing beyond the merge-request read the mergeability check already performs. Fetching and folding a job set for every status would pay an extra request per tick per pending merge, against a rate-limited API, to recompute something the platform reports for free.

**The exception is the manual status, and it is not a settled-and-clean signal.** The platform's composite computation tests for a blocking manual job *before* it falls through to reporting failure, so a pipeline holding both a blocking manual job and a failed job still reports manual. Two ordinary arrangements reach that shape: a manual job sharing a stage with the failing one, and any pipeline using a dependency graph, where a job can fail while an earlier-stage manual job is still untriggered. Neither arrangement moves the mergeability status, so on a project that does not require passing pipelines the merge gate rests entirely on the CI read.

Only the pipeline's own job set separates those from the benign manual gate. So the manual arm, and only the manual arm, spends one extra request to fetch that job set and folds it through the shared merge gate. Mapping manual to a merge-eligible verdict without reading the jobs would merge a commit whose CI reports failing, on an operation that cannot be undone.

Two addressing mistakes on the job-set route **fail quietly**: an abbreviated commit id returns an empty list rather than an error, and a pipeline scope matching no pipeline returns an empty list rather than an error. A mis-addressed read is therefore indistinguishable from a pipeline carrying no job, which is why the manual arm holds at pending on an empty set instead of reporting that no CI gate exists.

The pipeline scope on that route is load-bearing rather than defensive: a commit can carry more than one pipeline, and an unscoped read returns both pipelines' entries.

### Two more CI rules worth keeping

**A soft-failing job belongs in the conclusion, not in the aggregate.** Map a job that failed but is allowed to fail to a neutral conclusion, and the shared fold then agrees with the platform for free. A separate "count only hard failures" rule must not be written: it would restate in the count what the conclusion already encodes, and the two would drift. The conformance assertion that recomputes the aggregate from the returned runs exists to catch exactly that drift.

**A commit-status entry is not always a job.** Jobs and external statuses share one identifier space, so the status list mixes entries whose id works on the job-log route with entries whose id does not, and nothing in the entry's own fields separates them. Resolve the real job set from the pipeline's jobs route and select only from entries appearing there, rather than calling the log route speculatively and reading a not-found as a negative. When the first failing entry is an external status, leave the log excerpt empty rather than fabricating one.

**The job log is not plain text.** Every line carries three layers of markup: a high-precision timestamp, a stream token marking stdout or stderr and line continuation, and ANSI escape sequences plus collapsible-section markers with literal control bytes. Handing that to an agent puts terminal control bytes into a prompt. Strip all three layers, not only the ANSI one. The route also supports no partial fetch, so the whole trace is read and truncated client-side to the configured line budget, taking the tail.

## The merge call

**The precondition parameter is the head of the source branch**, and on the pinned deployment it is required rather than optional, though the requirement is conditional on an instance setting. Always send it: that satisfies both configurations and gives the precondition semantics the contract wants anyway.

**Every rejection reason returns the same opaque response.** Already-merged, draft, and conflicting merge requests are byte-identical, because both arms of the server's merge path call the same bare helper rendering a constant. So on a rejection the adapter re-reads the merge request and classifies from its observed state, reporting already-merged only when the re-read confirms it. That is the contract's own procedure rather than a workaround for the opaque body: GitHub and Gitea do the same, and Gitea does it even though its body *does* say already-merged.

The head-drift rejection needs no re-read. It is unambiguous, and carrying no already-merged marker is exactly right there, since an unmarked conflict is the retry disposition.

**An unauthenticated-shaped rejection on this route is an authorization failure, not a bad token.** GitLab guards the merge route on whether the caller may push to the target branch, so a protected branch produces it with a perfectly good credential. This deviates from the rest of the API. Mapping it to an auth error is still right, because both causes need a human, but the adapter cannot tell which, so the message must name both possibilities.

**Rebase is not expressible on this endpoint.** Rebase-on-merge is a project-level setting, and the standalone rebase route is asynchronous and does not merge. Never silently substitute a merge commit for a requested rebase.

GitLab takes a single commit message per strategy rather than a separate title, so the caller's title and message are joined with a blank line, matching how GitLab's own interface composes them.

**Branch reaping after merge is asynchronous and races your client.** It is governed by the merge request's own setting, resolved at creation from a project default that removes the branch, and the window can be shorter than a post-merge round trip. The branch delete must tolerate both the branch still being present and it having already been reaped, and a delete-after-merge *assertion* in a test is a race unless the fixture opens its merge request with source-branch removal explicitly disabled. A branch the forge reaped and a branch the caller deleted are indistinguishable at that route.

An already-absent branch is returned as a not-found error that the caller treats as a successful no-op, while an already-absent label on removal returns nil. **The two dispositions are opposite on purpose** and are each pinned by their own conformance assertion, so they must not be written from one template.

## Bot classification costs a request here

**The platform bot marker does not reach note authors.** Every embedded user object in the API, a note's author included, is rendered with a basic entity that carries no bot field. A bot identity and a human are indistinguishable by shape.

The marker is available on the per-user route, and a non-administrator token can read it, which is the only reason the platform half of the predicate is usable at all. So bot review comments resolve the distinct author ids of the notes already fetched, look each up once, cache for the adapter's lifetime, and pass the result into the shared union with the configured allowlist. Cost is bounded by distinct authors on one merge request, not by comment count. GitLab is the only one of the three forges that spends a request to answer the platform half.

An adapter run with an empty allowlist may skip the lookup, but then bot selection returns nothing, so the allowlist stops being optional. That degradation belongs in the operator-facing documentation.

**The generated-bot username pattern is a hint, not a contract.** GitLab names its own managed token identities predictably, so a prefix match would classify them for free. It would not classify a third-party review bot driven by a personal access token, which is the common case for the review-comments reaction. Never use the pattern as the only platform signal.

## Configuration and construction

The endpoint defaults to the hosted host, so only a self-managed deployment sets it. The token is sent in GitLab's own header rather than as a bearer credential, because that header accepts only GitLab access tokens while the bearer header is shared with OAuth flows.

**No prefix or length check on the token.** The access-token prefix is an administrator-writable instance setting, so a shape check would reject valid tokens on a customized instance. Do not add one.

The coarse scope model has no per-resource equivalent of "issues: read and write": one scope covers the whole write surface and a narrower one covers a read-only deployment. Granular hosted tokens report an opaque scope list, so no scope-based diagnostic is possible for them and a write failure arrives without the marker the classic tokens carry.

Prefer a **project access token** where the deployment allows it. It is a server-enforced single-project credential, which is genuinely least privilege rather than least privilege by convention. Its identity is a generated bot user, which matters for any identity-based filtering: the automation identity the operator has to name is that generated username, not a display name.

Construction runs three preflights:

- Token introspection, which reports scopes, active, revoked, and expiry in one call and lets a misconfiguration fail with an actionable diagnostic instead of a generic rejection. This one is **advisory and never blocks construction**; its result only enriches the project-check failure message.
- The project check, which is the only way to separate a wrong project from an unauthorized token, given the not-found masking above.
- The project label list, which is where the canonical label casing comes from.

## What to verify, and how

The integration suite is env-gated behind a single `SORTIE_GITLAB_TEST=1` and skips cleanly without it. It runs against a containerized Community Edition instance the job boots and provisions itself, scheduled rather than per-push, with a hosted job behind the same gate as a thin compatibility canary.

The container is heavy: an order of magnitude larger than a Gitea image and minutes rather than seconds to become usable. It is still the right call, because **only a self-managed Community Edition instance tests the compatibility floor**, and a hosted-only strategy would have silently accepted every product divergence found so far. What the hosted job buys is forward-version drift detection against a continuously deployed instance, not product-divergence coverage.

Five things about that harness are hard-won and will bite you again if they are lost:

- **The boot passes through phases that look like failures.** There is a window with nothing listening on the port at all, and then a window where the front end is up and answers a gateway error while the application behind it is still starting. A readiness poll must treat both a refused connection and a gateway error as ordinary mid-boot states rather than as a dead container, and its overall budget has to be sized for a boot measured in minutes: a timeout carried over from a lighter container's harness gives up on an instance that was going to come up.
- **The container's own health signal is not a readiness signal.** It flips to healthy well inside the gateway-error phase above, while the API is still answering nothing at all. A job that waits on container health and then runs tests fails against a booting instance.
- **The health endpoints are unusable as an external gate.** They are restricted to a loopback allowlist, so a poll from the CI runner reaches them as the container network gateway and gets a not-found forever, on an instance that is already serving. Poll the API's version route from outside instead, and treat the **first response of any kind other than a gateway error** as ready, including an unauthenticated rejection: that rejection proves the application is routing and authenticating. Readiness must be a positive set; a negative set misreads any unlisted transient as ready.
- **The poll must abort when the container stops**, rather than wait out its budget. A misconfiguration exits the container within seconds, and the most likely misconfiguration is an unsupported configuration key, which the reconfigure step rejects outright.
- **A token cannot be minted from credentials over HTTP.** The bootstrap credential comes from one runner invocation inside the container; everything after that is plain REST.

The fixture's shape carries two non-obvious requirements:

- **Two projects, seeded sibling-first**, so the two issue identifiers diverge and a suite can catch code that confuses them. The second project also supplies the negative control for authorization masking.
- **A non-administrator identity for every probe.** An administrator token cannot observe the authorization failures a real deployment hits; the administrator credential is for provisioning only.

Beyond that the fixture needs a group-level label as well as project labels, an issue with enough comments to cross a page boundary at the adapter's own page size, a non-issue work item to prove the type exclusion, and a merge fixture opened with source-branch removal disabled so a post-merge branch assertion is a sequence rather than a race.

**Pair every absence claim with a positive control.** Most of the durable findings here are negative results, and a negative result taken without a control is indistinguishable from a broken fixture.

## Where to get the facts

This document deliberately carries no route tables, parameter lists, enum values, status codes, or rate-limit numbers. In order of authority:

1. **The running instance's own GraphQL introspection**, which is version-exact for the deployment under test. Use it to learn what a field can contain, remembering that schema membership is not reachability and that the GraphQL vocabulary is not the REST wire vocabulary.
2. **A live call against the lab with the token the adapter carries**, with a positive control alongside. Where documentation and observed behavior disagree, the instance wins, and the disagreement is worth recording as a hazard rather than silently resolving.
3. **First-party GitLab documentation**, reachable through Context7. Watch for the case where it is accurate about a parameter's *type* and silent about a tier restriction on its *cardinality*, which reads as permission it does not grant.
4. **Upstream source at the tag the instance reports**, which is the only way to prove that a route or a parameter value is edition-gated rather than merely absent. The Enterprise tree carries both the Community code and the Enterprise overrides, which is what makes the gating visible.
