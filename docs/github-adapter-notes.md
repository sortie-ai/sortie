# GitHub adapter notes

What to know before you change `internal/scm/github`: the design decisions behind it, the places GitHub's model does not line up with ours, and the traps that cost someone a day.

Last updated: 2026-08-23

## Shape of the package

One package registers three kinds under the name `github`: the tracker, the SCM adapter, and the CI status provider. The `internal/scm/` boundary rules apply unchanged: no cross-adapter imports, no importing the orchestrator, and every external response normalized to a domain type before it leaves the package. The orchestrator and `cmd/sortie` reach all three through `internal/registry` and never import this package directly.

There is no GitHub client library in the dependency set and there should not be one; the zero-runtime-dependency model is the reason. Transport, the link-header paginator, and the page-number paginator come from `internal/httpkit`; label-state derivation and normalization from `internal/issuekit`; and the decisions every SCM adapter shares (tracker-to-SCM error conversion, merge-conflict promotion, the already-merged marker, bot classification, sortable event ids, the CI aggregate and the merge gate) from `internal/scm/scmcore`.

Two HTTP clients live side by side: one for REST, one for GraphQL. The GraphQL base path is derived from the configured endpoint rather than hardcoded, because a GitHub Enterprise Server endpoint puts GraphQL at a sibling path of the REST root rather than under it.

## The pinned REST API version is a payload contract

The adapter sets a REST API version header on every request, in one place in the client constructor, and no configuration key overrides it. That pin is deliberate: it makes the payload shapes the normalizers depend on stable, and it makes a payload change an explicit decision rather than something that arrives one morning.

The corollary is that **bumping the pin is a payload-shape change, not a version bump**. A version has already removed a field the adapter used to read. Do not assume a removal announced for one payload is confined to it.

The method for finding out is worth keeping even though its results are not. Because the pin lives in one place, the probe is a single header change: issue the same request twice against the same live resource, varying only the version header between the outgoing value and the incoming one, and **diff the JSON key sets rather than the values**. A removal surfaces as a missing key and never as an error, so a comparison that looks at values will not see it. Run that across every route the adapter reads, not only the one a release note mentions, and against a resource in each state the adapter cares about: a field can be present-but-null in one state and absent in another, so a merged pull request and an open one are two separate probes, as are an issue with a parent and one without.

One consequence is load-bearing today: the pull request payload under the current pin carries no merge commit identifier, so `GetMergeability` sources it from a second, gated GraphQL call when the REST read reports the pull request merged. Two details of that call matter. It asks only for the real merge commit and never for the potential (test) merge commit, so GitHub's speculative merge commit is structurally unreachable through this path. And the merged gate bounds the cost: an open, draft, or closed-unmerged pull request costs no GraphQL call at all.

## Where GitHub's model does not match ours

**No workflow states.** A GitHub issue is open or closed. The adapter derives workflow state from repository labels and couples that to the native flag: a terminal target closes the issue, an active target on a closed issue reopens it. Native open with no state label reads as the first configured active state, native closed with no terminal label as the first terminal state. This is the same convention the Gitea adapter uses, deliberately, so operator configuration is uniform across the open/closed trackers.

**The adapter creates no labels.** The repository is expected to carry the state labels before Sortie starts. Creating them would need broader permissions and would write into a namespace the repository's own conventions own. This is the opposite of the Gitea adapter's choice, and the difference is not arbitrary: GitHub's label-add route fails loudly on a name it will not accept, so an operator sees the failure. Gitea's does not, which is why that adapter creates on demand instead.

**No priority field.** Priority labels reach the domain through the label list like any other label; the numeric priority field stays unset, which the domain reads as "no numeric priority".

**Issue types exist but only for organizations.** Personal repositories, and organizations that have not configured them, return nothing, and the field normalizes to empty. Do not build anything that assumes it is populated.

**Issues are addressed by repository-scoped number, not by the global id.** Both the domain ID and the domain identifier are that number; the global id is never read. `DisplayID` is qualified to `owner/repo#N` so a bare number is never ambiguous across repositories, and the qualifier does not overwrite a value already present.

**Mergeability is computed asynchronously.** GitHub recomputes it after a push, so a read taken too soon returns a value that maps to unknown. Callers treat unknown as a deferral condition and re-read on the next tick; it is not a failure and must not be turned into one.

## Blocker resolution, and the pre-filter that pays for it

`FetchCandidateIssues` never reads the dependencies route. Every candidate comes back with its blockers unresolved, and a shared resolution layer between the registry and the orchestrator calls `FetchIssueBlockers` per candidate afterwards, after the cheaper eligibility and capacity gates have already discarded most of them, bounded by a per-pass read budget. `FetchIssueByID` reads the route directly and clears the flag itself.

What makes that affordable on GitHub is a per-issue dependency summary that both candidate routes already carry on the payload. A candidate whose summary proves it has no dependencies at all skips the extra read entirely, so an operator who has created no dependencies pays nothing. The Gitea adapter has no equivalent because its candidate payload carries no dependency field, and that asymmetry is a difference in wire format rather than in mechanism.

Two traps in that summary:

- It carries both a total dependency count and a count of the ones GitHub still considers open, and **the two diverge**. The pre-filter reads the total. The dispatch gate's question is "does this issue have any dependency at all", not "does it have an open one": a closed dependency still needs a read to confirm its state maps to a configured terminal label rather than being assumed terminal from its native closed status.
- It is null on a pull request, which the issues route co-mingles with issues. The pull-request filter runs before normalization reaches the field, and a null summary is read the same as an absent one regardless, so the flag stays set and the issue gets its read.

**A not-found from the blockers route is a failure, not an empty list.** That route addresses a list resource, and a list resource spells "none" as a 200 with an empty array. A 404 there means the issue is gone or the route is absent, neither of which the adapter is entitled to read as "no blockers". Contrast the parent route, a single-resource route where a 404 legitimately means there is no parent and is mapped to a nil parent. Getting these two backwards is easy and the failure is silent in one direction.

## Two candidate paths, and why the default is the boring one

With no operator query filter set, candidates come from the plain issues list route with client-side state filtering. With one set, they come from the search route, which accepts the full qualifier syntax.

The reason the plain route is the default is the rate-limit budget, not the expressiveness: search carries its own, far stricter per-minute budget separate from the primary hourly one. Routing candidate polling through search when nothing requires it spends the wrong budget.

The search route also caps total results and reports whether a result set is incomplete. The adapter keeps the partial page and warns; it does not treat an incomplete result as a failure.

One deliberate asymmetry: when `FetchIssuesByStates` looks up terminal states through search, the operator's query filter is **not** appended. An operator filter must not be able to hide a terminal issue from that lookup.

## Two CI vocabularies, one gate

GitHub reports CI through two different surfaces with two different value vocabularies for the same underlying gate: the older combined commit status and the newer check runs. The adapter reads both, normalizes each with its own mapping into the domain check-run type, and then reduces the union through the shared merge gate. Neither the top-level aggregate state on the combined-status payload nor the total count on the check-runs payload is read; both are computed from the arrays instead.

Two mapping decisions worth keeping:

- A check run asking for a manual UI action is a **failure**, because the agent cannot perform that action.
- A superseded check run is **pending**, because the run that superseded it carries the authoritative conclusion.

Anything unrecognized, including the empty string, folds to pending rather than to a passing verdict. Keep that direction if you extend the mapping.

The empty string is the gate's "no required checks exist" answer, and the auto-merge loop treats it as a satisfied CI precondition when CI is not explicitly required. It is not the same as "everything passed".

The CI status provider reads the same check-runs route through the same normalization helpers but reduces it with the plain aggregate rather than the merge gate, and additionally pulls a log tail for the first failing GitHub Actions check. That log read works because GitHub Actions creates check runs one-to-one with workflow jobs, so the check run id doubles as the job id. That coupling is a GitHub Actions property, not a check-runs property: a third-party check has no job log behind it.

Neither reader sends the filter parameter on the check-runs route, so GitHub's default decides whether a superseded run still contributes. If you touch that reader, settle what the default is against a commit whose checks were re-run before assuming.

## The merge write path

**A repeated merge is not an error.** Merging a pull request that the first call already merged returns success with the existing merge commit. GitHub short-circuits once merged and stops evaluating the head-SHA precondition, so even a stale expected head still succeeds. The reconcile loop records the duplicate as success through its normal path. Do not add an "already merged" guard in front of the call; it would be redundant and would introduce its own race.

**The already-merged race is different and needs a re-read.** When a different actor merges first, GitHub rejects the call, and the rejection body carries no wording saying the pull request was already merged. The adapter therefore re-reads the pull request after a rejection and reports already-merged only when the re-read confirms it. Gating on observed state rather than on message text is what makes this survive a reworded rejection, and it is the same choice the Gitea adapter makes for the same reason.

**Status-based promotion to a merge conflict applies to the merge write call only.** It keys on the recorded HTTP status, never on message text, so an error whose message merely mentions "conflict" passes through unchanged, and the same statuses arriving from the branch-delete route or from a GraphQL read keep their own class.

**The expected head SHA closes the only race the merge call exposes.** Reading mergeability and merging are two separate calls, and the head can move between them. Passing the head SHA observed at read time as a merge precondition makes GitHub reject the merge instead of merging a head nobody approved; the reconcile loop then re-enqueues, re-reads, and either retries against the new head or defers. Review decision and CI conclusion have no equivalent precondition mechanism, so a change in either is caught by the next tick's re-read rather than by the merge call.

**Deleting an already-absent branch is a not-found that the caller treats as a successful no-op**, which is what lets a retry after a partial-failure tail complete the issue rather than block it. A refused delete against a protected or default branch arrives as one of two different statuses depending on the case, so handle both; the auto-merge loop never targets a default branch, making either an operator-configuration error rather than a runtime condition.

## The scope preflight can only verify one token kind

The auto-merge scope preflight reads the scopes header GitHub returns on a rate-limit read. Classic personal access tokens populate that header. Fine-grained tokens and GitHub App installation tokens do not, and there is no substitute surface for them.

So the preflight fails open: an absent or empty header returns the "unable to verify" sentinel, the caller logs and proceeds, and a genuine gap surfaces at runtime as an auth failure on the first write. The legacy broad scope is treated as a superset that satisfies every requirement and short-circuits the check.

Keep the fail-open arm. Turning an unverifiable check into a hard failure would block every fine-grained-token deployment, which is the least-privilege option and the one we recommend.

## Rate limits shape three decisions

The primary hourly budget, the search per-minute budget, and the GraphQL budget are independent, and the adapter's design responds to all three:

- Candidate polling stays off the search route unless an operator filter requires it, because search draws on the smaller budget.
- The two GraphQL reads are each gated: the review decision is one call per read, and the merge commit is one call only for a pull request already known to be merged.
- The per-issue state reads used by reconciliation go through a bounded, per-path ETag cache. A not-modified response reuses the previously derived state without re-deriving it, and does not charge the primary budget. This matters precisely because reconciliation polls the same issues over and over. The cache size is an operator-tunable extension key, and zero disables conditional requests entirely.

Secondary limits are separate again and are not a quota you can count down; they arrive as a rejection carrying a retry hint. The adapter never sleeps or retries on its own: it classifies, appends the retry hint to the message, and leaves backoff to the orchestrator.

One classification subtlety that is easy to get wrong: the same status code means both "you lack permission" and "you hit a rate limit", and the two map to different error kinds because the orchestrator treats them differently. The adapter disambiguates on the remaining-requests header first and the response body second. Preserve that order.

## Configuration and offline validate parity

The endpoint defaults to the public API host when unset. A non-empty value must parse as an absolute http(s) URL carrying a host and carrying neither query nor fragment, or construction fails. Three things about the parse are load-bearing:

- All three constructors share the one resolver, so an unbracketed IPv6 literal (exactly the form a self-hosted address takes in `ip addr` output) is a configuration fault at startup rather than a transport error on the first request.
- The parser's own error text quotes the whole raw URL, which for an endpoint carrying embedded credentials would republish them. The message carries the redacted form instead.
- The offline `sortie validate` hook calls the same resolver, so the offline verdict cannot drift from the startup verdict. But the CI provider and the SCM adapter read the endpoint from the `github` extensions block first and fall back to the tracker endpoint only when the reaction provider matches the tracker kind. Validate inspects the tracker endpoint alone. That is why the guard lives in each constructor and not in the validate hook.

The project value is `owner/repo` and is rejected if either half is empty or the second half carries another slash. The user agent is not an operator key; `cmd/sortie` sets it and the constructor has a fallback.

**The constructor runs no credential or repository preflight.** A bad token or an inaccessible repository surfaces on the first read, not at startup. This is the opposite of the Gitea adapter's choice and worth knowing when you are debugging a deployment that starts cleanly and then fails on its first poll.

## What to verify, and what is unobserved

**GitHub Enterprise Server is the standing blind spot.** Nothing about the route surface, the version-header behavior, or the dependency routes has been observed against a GHES instance. The endpoint handling for GHES is a fact about our adapter, not about a server. This matters most for the blockers route: the adapter fixes its blocker source at registration and cannot vary it per endpoint, so if GHES answered "no dependencies" with a not-found on that list route, it would have to be escalated rather than absorbed. Treat any GHES claim as unverified until someone runs the probes.

The GraphQL query text is pinned by request-shape unit tests rather than by a live probe. Those tests are what stop a well-meaning edit from asking for the speculative merge commit; do not delete them when refactoring.

When you need to settle a behavior, the shape of the check matters more than the answer: pair every absence claim with a positive control on a sibling route or a sibling issue, or you cannot tell "the feature is absent" from "the fixture was wrong".

## Where to get the facts

This document deliberately carries no route tables, parameter lists, enum values, status-code tables, or rate-limit numbers. Those belong to GitHub, and GitHub publishes them better than we can. In order of authority:

1. **GitHub's REST and GraphQL reference**, reachable through Context7. For a question about what an endpoint requires, the response header GitHub returns naming the accepted permissions for that endpoint is more precise than the documentation page.
2. **A live call with the token the adapter carries**, against a scratch repository, with a positive control alongside.
3. **GraphQL introspection** for anything about the GraphQL schema, including which enum values a field can return. Introspect before relying on any list of values, and fold anything unrecognized into the safe verdict rather than enumerating.
