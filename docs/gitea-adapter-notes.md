# Gitea adapter notes

What to know before you change `internal/scm/gitea`: the design decisions behind it, the places Gitea's model does not line up with ours, and the traps that cost someone a day.

Last updated: 2026-08-23

## Shape of the package

One package registers three kinds under the name `gitea`: the tracker, the SCM adapter, and the CI status provider. That is the same combined layout the GitHub adapter uses, and the `internal/scm/` boundary rules apply unchanged: no cross-adapter imports, no importing the orchestrator, and every external response normalized to a domain type before it leaves the package. The orchestrator and `cmd/sortie` reach all three through `internal/registry` and never call into this package directly.

There is no Gitea client library in the dependency set and there should not be one; the zero-runtime-dependency model is the reason. Transport, pagination, and preflight backoff come from `internal/httpkit`, label-state derivation and normalization from `internal/issuekit`, and the forge decisions every SCM adapter shares (error conversion, merge-conflict promotion, bot classification, sortable event ids, the CI aggregate and merge gate) come from `internal/scm/scmcore`. The overlap with the GitHub adapter is transport-shaped rather than domain-shaped, which is why the two share `httpkit` and no base structs.

Gitea serves no GraphQL. The REST surface is the whole contract, so anything the GitHub adapter answers with one GraphQL field has to be composed here from several REST reads.

## Where Gitea's model does not match ours

**No workflow states.** A Gitea issue is open or closed; there is no transition graph and no close-reason field. The adapter derives workflow state from repository labels and couples that to the native flag: a terminal transition also closes the issue, an active transition on a closed issue reopens it. Native open with no state label reads as the first configured active state, native closed with no terminal label as the first terminal state. This is deliberately the same convention the GitHub adapter uses, so operator configuration is uniform across the open/closed trackers.

**No review decision.** Gitea has no aggregate review verdict, so `GetReviewDecision` folds one out of the review list plus the pull request's requested-reviewers signal: latest decision per reviewer wins, any standing changes-requested loses, otherwise any approval wins, otherwise a non-empty requested-reviewer list means review-required. Ordering depends on the review timestamp parsing; a value that will not parse fails the read rather than silently sorting to the epoch.

**No priority, no issue type, no parent.** None of these concepts exist. The corresponding domain fields are always empty. Do not add a synthetic mapping for them.

**Two issue numbers, one usable.** Every issue carries a repo-scoped index and an instance-global id. Only the index is accepted by any per-issue route, and the same entity read through the pull route and the issue route reports two different global ids. Store the index; treat the global id as a trap. The adapter maps both `ID` and `Identifier` to the index and qualifies `DisplayID` to `owner/repo#N` so a bare index is never ambiguous across repositories.

**Mergeability is one boolean.** There is no state string and no tri-state "still computing". The mapping is therefore lossy on purpose: a draft is blocked, a mergeable non-draft is clean, everything else is unknown. A merge conflict and an in-progress recheck are indistinguishable and both land on unknown, which the auto-merge state machine re-enqueues instead of treating as a hard conflict. Do not try to recover a richer signal from this field.

## Label traps

These are the reason the label plumbing looks more elaborate than it should.

**Attaching an unknown label name returns success and does nothing.** Not a 422, not a 404, but a 2xx with an unchanged issue. Any flow that trusts the server to reject a bad label name no-ops invisibly, and a state transition that silently does not happen is the worst outcome available. The adapter therefore resolves every name to a numeric label id itself and attaches by id, never by name. Attaching by id is what makes a resolution failure loud.

**Removal is by numeric id only.** A name in the id slot is rejected, and the rejection status is not stable across releases, so do not key on it. Resolve first; the adapter never puts a name in that slot.

**The server-side `labels` list filter is unusable for correctness.** It is AND across the listed names (an issue must carry all of them) despite documentation to the contrary, it matches case-sensitively while our configured state names are lowercased, and a name that resolves to no label silently disables the whole filter and returns everything. Any one of those disqualifies it for state scoping; the adapter filters states client-side instead. When an operator puts `labels=` in `query_filter`, construction resolves those names against the repository catalog and warns about each one that does not resolve, turning the silent foot-gun into a visible diagnostic.

**The label catalog must be paged to exhaustion.** A first-page-only resolver will miss a label defined later in the catalog and then either fail to resolve a real label or create a duplicate of it.

**Missing state labels are created, not required.** The GitHub adapter can require pre-created labels because a bad name fails loudly there. Here it cannot, so the adapter creates the label on demand instead. Creation needs a color as well as a name.

## Review comments: anchors, and the absence of a staleness signal

This is the most expensive thing in the file to have got wrong.

The two positional fields on a review comment are **file line numbers, not diff offsets**, and they select the *side* of the diff rather than the freshness of the anchor: one is the new-side line, the other the old-side line, and each is zero when the comment is not anchored to that side. A reader that treats them as diff offsets silently attaches the comment to the wrong line. The adapter takes the new side when it is positive, else the old side, else zero, which keeps the mapping total over every integer pair the route can return.

A zero new-side position therefore **does not mean outdated**. It means the comment is anchored to the old side, which is true the moment the comment is created.

The route exposes no invalidation field at all. Upstream's internal comment model carries one; the API converter does not put it on the wire. Nothing else is a substitute:

- The commit id on a comment is a *line blame* on the new side and the head-at-creation on the old side. Neither half moves on a later push, and the new-side value routinely names a commit that was never the pull request head. Comparing it against the current head looks like a staleness check and is not one.
- The review-level stale flag is a generation marker: it flips on any push regardless of what the push touched.
- The comment's update timestamp moves for an invalidating push, for an ordinary body edit, and not at all for a push that leaves the anchors intact, so it is a superset of invalidation rather than a synonym.

So the adapter never marks a Gitea review comment outdated. Deriving it from any of the above would drop live feedback on arrival and drop the rest of a review the moment any push lands, which is a stall rather than a fix.

Comments anchored to neither side, and comments anchored outside the diff entirely, are both accepted by the write side and read back intact. Handle them; do not assume the forge rejects them.

## Combined commit status: two traps in one response

**The obvious total field is the current page's length, not the grand total.** It cannot detect emptiness and it cannot detect truncation. The grand total is only in a response header. Both readers key the empty case on the accumulated status count across every page.

**The route paginates, and a single-request read is a correctness bug.** A failing status ordered onto a later page reads as passing to both the CI-failure reaction and the auto-merge CI gate. Both readers walk every page through the shared page-number paginator up to the package's page ceiling and log a warning on reaching it.

**The top-level aggregate state lies about a commit with no CI**, reporting pending where nothing ran. Trusting it would hold auto-merge forever on a repository without CI. Both readers compute the aggregate from the per-status entries instead, and both go through the same shared merge gate so one forge cannot answer "is CI green" two different ways.

The per-status field name for a single entry differs from the top-level aggregate field name. Conflating them is easy and silent.

## Pagination is not uniform across routes

Three different shapes coexist, and picking the wrong helper truncates results without erroring:

- The tracker issue routes emit a standard `Link` header; the link-following paginator is correct there.
- The reviews, review-comments, and timeline routes emit **no** `Link` header. The link-following paginator stops at the first response without one, which would silently truncate a multi-page read to page one. These go through the page-number paginator, wrapped by the package's own helper so the parameter names, the page ceiling, and the conversion to the SCM error boundary are pinned in one place.
- The per-issue comments route is unpaginated and returns the whole list in one response, ignoring any page-size parameter.

The instance clamps the requested page size to its own configured maximum, which an operator can lower, so never assume the size you asked for is the size you got; iterate on the pagination signal.

Because the timeline is oldest-first and read forward to the cap, an unusually long timeline truncates its *newest* entries, which is exactly where a label command lives. The adapter warns on reaching the cap.

## Writes

The merge route returns success with an empty body: there is no merge commit SHA to report back, so a successful merge result carries a true flag and an empty SHA.

An already-merged pull request and a stale expected-head SHA are both rejections, distinguished by status. The adapter deliberately does **not** match on the rejection's wording. It re-reads the pull request after a rejection and reports already-merged only when the re-read says merged. Gating on observed state rather than phrasing survives a reworded message and never misfires on a stale-head race. Status-based promotion to a merge conflict is applied on the merge write path only, so the same statuses on any other route keep their own class.

Gitea's own delete-branch-after-merge and merge-when-checks-succeed options stay unused: Sortie keeps the two-step merge-then-delete flow and gates CI itself.

Branch names are percent-encoded, slashes included. Deleting an already-absent branch is a not-found error that the caller treats as a successful no-op.

## The auto-merge scope preflight cannot verify a scope

Gitea exposes no scope introspection: no rate-limit endpoint, no scopes response header, and a token's own scopes appear only inside the body of a rejection on a write call. The repository permissions block reports the token *owner's role*, not the token's scope: a read-only token owned by a repository admin still reports push access. And there is one coarse write scope covering both merge and branch delete, with no contents/pull-request split, so the "requires contents" argument is accepted and inert.

The preflight is therefore layered rather than authoritative: fail open with the "unable to verify" sentinel so auto-merge proceeds, add a user-role push gate that catches a wrong-owner or read-only-collaborator token, and rewrite the runtime rejection on the first write so it names the scope the operator has to grant. If you extend this, keep the fail-open arm. Turning an unverifiable check into a hard failure blocks every correctly configured deployment.

## No rate limiting, no conditional requests

Gitea ships no built-in API rate limiting and sends no rate-limit headers, so there is no budget accounting to do; the self-hosted instance's capacity is the budget and poll cadence is the only pressure control. A reverse proxy in front of the instance may still inject a rate-limit rejection, which the adapter classifies as an API error and logs the retry hint from, leaving backoff to the orchestrator.

It also sends no `ETag`, so the GitHub adapter's conditional-request cache has nothing to key on here. Do not port it.

## Configuration and offline validate parity

The endpoint is required (there is no default host), and every constructor parses it through the shared endpoint parser before building a client. Three things about that are load-bearing:

- The parser's own error text would quote the whole raw URL, which for a `scheme://user:secret@host` endpoint names the credential in the failure message. Every constructor's message carries the redacted form instead.
- An unbracketed IPv6 host, exactly the form a self-hosted address takes in `ip addr` output, fails to parse. Surfacing that as a configuration error at startup rather than as a transport failure on the first request is intentional.
- The three constructors do not always see the same value. The resolver copies the `gitea` extensions block into the CI provider's and SCM adapter's config before falling back to the tracker endpoint, so an extensions-level endpoint override wins for those two surfaces. Offline `sortie validate` inspects only the tracker endpoint and never sees the override. That is why the guard lives in each constructor and not in the validate hook alone: a tracker endpoint that validates offline says nothing about what the other two surfaces construct against.

The validate hook decides its verdict from the same parser call the constructors make, so the offline verdict cannot drift from the construction verdict. The same holds for `query_filter`: construction and validate run the same parser. `query_filter` here is a URL query fragment merged into the issue list request; keys the adapter owns are rejected outright, unrecognized keys pass through with a warning.

Construction runs two preflights, a credential and identity check and a repository check, because Gitea's not-found body cannot distinguish a missing repository from a missing issue. Without the repository preflight, a mistyped project surfaces as a not-found on every subsequent call with no hint where the blame belongs. Transient failures retry with a bounded backoff; auth and not-found failures fail construction immediately.

## What to verify, and what a lab cannot show you

The integration suite is env-gated behind a single `SORTIE_GITEA_TEST=1` and runs against a throwaway container the job boots and seeds itself, so no repository secret carries a Gitea credential and no state survives a run. Without the gate every test skips cleanly. Fixture-dependent tests skip with a message naming the missing fixture, so a partially seeded lab reports skips rather than failures.

**Test prerequisite: the token needs three scopes.** The provisioning script mints issue write, repository write, and user read, and a developer pointing the suite at a hand-made token needs the same set. The mapping is not obvious from the operation list: issue write covers the entire tracker surface including creating a label, repository write is the single coarse scope covering both the merge and the branch delete (there is no contents and pull-request split to grant separately), and user read exists only for the credential-and-identity preflight. Scopes come in read and write variants per category with write implying read, so granting the read variants alongside is redundant rather than safer. A token short of any of the three fails at construction rather than at an assertion, because the preflights run before the first call the test makes: the symptom is every test in the package erroring identically, not one test failing.

Provisioning notes worth keeping:

- A freshly created admin-made user is forced into a password-change state that blocks token creation unless the create command explicitly disables it.
- Gitea forbids a changes-requested self-review, so the reviewer and the allowlisted bot user must both differ from the pull request author.
- The seed needs a commit carrying more statuses than fit on one page, or the combined-status pagination bug reappears untested.

What the lab cannot show you: a lab that runs no CI only ever produces two of the declared per-status values, so the remaining ones are exercised by fixtures and by the fold-unknown-to-pending rule rather than observed. Bot classification is allowlist-only here because Gitea users carry no platform bot marker at all, so the changes-requested read cannot exclude a bot-authored review the way the GitHub adapter can.

Forgejo and Codeberg are Gitea forks that keep the same API base path, auth header, and pagination model, and the adapter deliberately targets the portable subset (issue and comment routes, id-based label operations, link pagination) so they work behind the same `gitea` kind. That compatibility is a design argument, not a tested one. The forks have diverged on at least the label routes, and the shared surface narrows with every release on either side. Treat a Forgejo deployment as unverified until the gated suite runs against one.

Webhooks are the one reaction surface with no implementation here: every read above is poll-based.

## Where to get the facts

Do not take route shapes, parameter lists, enum values, or status codes from this document; it deliberately does not carry them. In order of authority:

1. **The instance's own OpenAPI description**, served by every Gitea deployment. It self-reports the release it belongs to and is the only source that matches the box you are actually talking to. Read it before trusting anything else.
2. **A live probe against the lab, carrying the token the adapter carries.** Several documented behaviors are contradicted by the instance (the `labels` filter's semantics are the standing example), so the instance wins over the description when they disagree.
3. **docs.gitea.com**, reachable through Context7, for auth schemes, the scope taxonomy, pagination, and configuration keys.
4. **Upstream source at the tag the instance reports**, for anything about a mechanism rather than a wire shape. The blame-versus-head behavior of a review comment's commit id, and the existence of an invalidation field that never reaches the wire, are both only visible there.

When a question is "what values can this field take", the answer is to introspect the instance and to fold anything unrecognized into the safe verdict, not to enumerate.
