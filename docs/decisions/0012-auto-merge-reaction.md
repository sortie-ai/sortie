---
status: accepted
date: 2026-05-25
decision-makers: Serghei Iakovlev
---

# Extend SCMAdapter with Write Methods for Auto-Merge Reactions

## Context and Problem Statement

The `SCMAdapter` interface in `internal/domain/scm.go` is currently
read-only. It exposes a single method (`FetchPendingReviews`) and five
error kinds (`ErrSCMTransport`, `ErrSCMAuth`, `ErrSCMAPI`,
`ErrSCMNotFound`, `ErrSCMPayload`). The architecture digest describes it
as a "Read-only multi-method (`FetchPendingReviews`) adapter."

Adding an orchestrator-driven auto-merge reaction requires the SCM
adapter to perform writes against the provider's API: invoke the merge
endpoint, optionally delete the source branch after merge, and read
three predicates the existing interface does not cover (review decision,
CI conclusion, mergeability). The merge endpoint also returns HTTP 405
and HTTP 409 responses with semantics distinct from the five existing
error kinds: a mid-flight precondition invalidation rather than a
transport or server failure.

The decision sits between [ADR-0003](0003-adapter-based-integration.md)
(adapter interface pattern, additive only) and
[ADR-0007](0007-handoff-state-and-tracker-writes.md) (precedent for
adding a write method to a previously read-only tracker adapter).
Recording the contract widening here, before implementation, prevents
future contributors from treating the SCM read-only label as still
authoritative.

## Decision Drivers

1. **Adapter contract clarity.** The architecture digest's "read-only"
   label for `SCMAdapter` becomes incorrect the moment a write method
   ships. Contributors who reference the digest to reason about token
   scopes, credential resolution, or failure semantics need the current
   statement of what the adapter does.
2. **Symmetry with TrackerAdapter.**
   [ADR-0007](0007-handoff-state-and-tracker-writes.md) added
   `TransitionIssue` (write) to a `TrackerAdapter` that previously held
   only reads, and the architecture and digest were updated accordingly.
   The SCM widening follows the same shape and should follow the same
   recording discipline.
3. **Single adapter handle per integration dimension.** The orchestrator
   already holds one `SCMAdapter` per workflow via `internal/registry`.
   A second handle would force the wiring layer to resolve two
   credentials, register two adapters per provider, and synchronize
   their lifecycles without delivering capability the single handle
   does not.
4. **Idempotency contract for the merge endpoint.** A second attempt on
   an already-merged PR returns HTTP 409 Conflict, and the orchestrator
   MUST treat that as success rather than as an error. The existing
   five error kinds do not carry this distinction; a dedicated kind
   keeps the recovery policy declarative.
5. **Trust boundary visibility.** Read scopes and write scopes on the
   provider token are operationally distinct. The widening MUST be
   documented so operators upgrading from a previously read-only
   deployment understand the scope escalation before the orchestrator
   attempts an irreversible action.
6. **Future SCM adapters.** GitLab and Bitbucket auto-merge
   implementations will satisfy the same interface. Locking in the
   contract before adding one provider's methods prevents shape drift
   between providers.

## Considered Options

- **Option A.** Extend `SCMAdapter` in place with the new read methods,
  the new write methods, and the sixth error kind. The interface
  becomes read-write.
- **Option B.** Split into `SCMReader` (the existing surface) and
  `SCMWriter` (the new write surface) as two separate interfaces. The
  GitHub adapter implements both.
- **Option C.** Keep `SCMAdapter` read-only. Introduce a separate
  `MergeAdapter` interface registered alongside it, with its own
  credential resolution and registry entry.
- **Option D.** Do not add write methods. Implement auto-merge by
  dispatching a coding agent with a merge prompt that uses the agent's
  existing tool surface.

## Decision Outcome

Chosen option: **Option A (extend `SCMAdapter` in place with reads,
writes, and `ErrSCMConflict`)**, because it preserves the
single-handle-per-dimension adapter model, mirrors the precedent set by
[ADR-0007](0007-handoff-state-and-tracker-writes.md) for
`TrackerAdapter`, and keeps provider credential resolution unchanged.
Options B and C add interface or registry surface that no consumer
uses; Option D defeats the determinism the orchestrator-driven merge
provides.

### Interface surface

The widened `SCMAdapter` adds five methods:

```go
type SCMAdapter interface {
    FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]ReviewComment, error)

    GetReviewDecision(ctx context.Context, prNumber int, owner, repo string) (ReviewDecision, error)
    GetCIStatus(ctx context.Context, prNumber int, owner, repo string) (string, error)
    GetMergeability(ctx context.Context, prNumber int, owner, repo string) (PRMergeStatus, error)

    MergePR(ctx context.Context, prNumber int, owner, repo string, strategy MergeStrategy, commitTitle, commitMessage, expectedHeadSHA string) (MergeResult, error)
    DeleteBranch(ctx context.Context, owner, repo, branch string) error
}
```

Method ordering puts reads first, writes second, matching the visual
posture operators expect when reasoning about side effects.

The `MergePR` signature carries an `expectedHeadSHA` parameter so the
provider rejects with HTTP 409 if the PR head moved between the
precondition check and the merge request. This closes a TOCTOU window
that an unconditional merge would leave open. Empty `commitTitle` and
`commitMessage` defer to the provider's strategy-specific defaults.

`DeleteBranch` returns `ErrSCMNotFound` when the branch is already
gone. The caller treats this as a successful no-op; the merge is the
irreversible action and is not rolled back when the branch delete
fails.

### Sixth error kind

```go
const ErrSCMConflict SCMErrorKind = "scm_conflict_error"
```

`ErrSCMConflict` covers HTTP 405 Method Not Allowed and HTTP 409
Conflict from the merge endpoint. The two responses share semantics
distinct from the existing five kinds:

- A 5xx response is `ErrSCMAPI`: transient, server-side, retry
  naturally.
- A malformed body response (422 on an unsupported `merge_method`, for
  example) is `ErrSCMPayload`: configuration error, escalate
  immediately.
- A 405 or 409 response is `ErrSCMConflict`: precondition raced (head
  SHA moved, branch protection refused) or already satisfied (PR
  already merged), re-evaluate on the next tick.

Collapsing 405 and 409 into `ErrSCMAPI` would force every consumer to
inspect error message text to recover the intent. A dedicated kind
keeps the consumer's switch statement declarative: each kind names one
recovery policy.

A 409 "already merged" response is treated as success by the
orchestrator, preserving merge idempotency.

### Token scope and operational stakes

Auto-merge requires write scopes on the provider token:
`pull_requests:write` for the merge call and, when branch deletion is
enabled, `contents:write` for the delete call. The token reuses the
existing `tracker.api_key` credential; no second secret is introduced.

The orchestrator MUST validate token scopes at startup. Discovering a
missing scope only on the first merge attempt would mean exhausting
the retry budget on a configuration error and surfacing the gap
per-issue rather than per-deployment. Validating once at process start
makes the gap observable before any merge is attempted.

Operators who decline the scope escalation retain previous behavior
unchanged; auto-merge activates only when its `provider` key is
configured in the workflow.

### Considered Options in Detail

**Option B (split into `SCMReader` and `SCMWriter`).** Interface
segregation is sound where readers and writers have distinct
lifetimes, distinct implementations, or distinct test surfaces. The
GitHub adapter satisfies none of these: one implementation serves
both, one credential authenticates both, one HTTP client transports
both. Splitting introduces two registry entries and two registration
calls per provider, doubles the number of mocks test code must
construct, and forces the orchestrator wiring layer to resolve two
handles where one suffices. The pattern would pay off only if a
future SCM provider implemented one surface without the other; no
such provider is in scope.

**Option C (separate `MergeAdapter` interface).** A dedicated merge
interface preserves the read-only `SCMAdapter` label at the cost of a
second registry entry, a second `init()` registration per provider,
and a second credential resolution path in `cmd/sortie/main.go`. The
merge call uses the same provider token, the same `httpkit.Client`,
and the same error mapping as `FetchPendingReviews`. Splitting them
by operation type rather than by integration dimension is artificial.
The adapter model
([ADR-0003](0003-adapter-based-integration.md)) is organized by
integration dimension (tracker, agent, SCM, CI), not by read/write
polarity. Following the same axis here keeps the registry layout
legible.

**Option D (agent-dispatched merge).** Dispatching a coding agent with
a merge prompt would use the existing tool surface and the existing
dispatch loop, avoiding the contract widening entirely. It introduces
three failure modes the orchestrator-driven approach avoids: the
agent may forget to call the tool, the agent may call the wrong tool,
and the agent may call the tool with hallucinated parameters. A merge
is a single API call with a fixed parameter shape; routing it through
agent inference is unnecessary indirection. Probabilistic execution
is appropriate for code generation, not for irreversible single-step
API calls. The same reasoning drove
[ADR-0007](0007-handoff-state-and-tracker-writes.md) to choose
orchestrator-driven handoff transitions over agent-driven
transitions; this ADR extends that reasoning to merges.

## Consequences

### Positive

- **Architecture documentation reflects reality.** The digest entry
  for SCM Adapter changes from "Read-only multi-method" to "Read-write
  multi-method" and lists the six methods. The label no longer drifts
  from the code.
- **Future SCM providers satisfy the same contract.** GitLab's
  `accept_merge_request` and Bitbucket's `pullrequests/{id}/merge`
  endpoints fit the `MergePR` shape behind their own adapters.
- **One credential per provider.** The existing token resolution
  covers both read and write paths.
- **Sixth error kind keeps the failure matrix declarative.** Each
  kind names one recovery policy; no consumer needs to inspect error
  message text to choose retry, escalate, or treat-as-success.
- **Mirrors the precedent of
  [ADR-0007](0007-handoff-state-and-tracker-writes.md).** Contributors
  who learned the tracker-write pattern there apply the same model
  to SCM.

### Negative

- **Mock surface grows.** Test doubles that satisfy `SCMAdapter` must
  implement five additional methods. Existing fakes in
  `internal/scm/github/` and any mocks in `internal/orchestrator/`
  gain stub implementations.
- **Scope escalation is a real operational change.** Operators who
  previously enabled only read-driven reactions retain read-only
  tokens; enabling auto-merge requires upgrading the token. Startup
  validation surfaces this clearly, but the upgrade itself is the
  operator's action and cannot be hidden.
- **`ErrSCMConflict` requires consumer awareness.** Code paths that
  handle SCM errors must add a switch arm for the new kind. Falling
  through to the `ErrSCMAPI` arm degrades merge idempotency silently:
  the "already merged" case would be treated as a retryable error.
- **Architecture digest revision required.** Section 1 (Primary
  Components) and the Section 11B label must be updated when this ADR
  is accepted. The change is mechanical but must not be forgotten;
  otherwise the digest contradicts the code.

## Confirmation

The decision is validated when all of the following hold:

1. `internal/domain/scm.go` exposes six methods (`FetchPendingReviews`
   plus the five new) and six error kinds (`ErrSCMTransport`,
   `ErrSCMAuth`, `ErrSCMAPI`, `ErrSCMNotFound`, `ErrSCMPayload`,
   `ErrSCMConflict`).
2. The architecture digest Section 1 entry for SCM Adapter reads
   "Read-write multi-method adapter" and lists the six methods.
3. `internal/scm/github/merge.go` implements the five new methods and
   maps HTTP 405 and HTTP 409 to `ErrSCMConflict`.
4. The orchestrator validates write-scope availability at startup and
   refuses to attempt merges when the validation fails. Validation
   failure is observable in logs at process start, not only on the
   first merge attempt.
5. Consumers of `SCMAdapter` in `internal/orchestrator/` handle
   `ErrSCMConflict` distinct from `ErrSCMAPI`. The 409 "already
   merged" case is treated as success; 405 method-not-allowed and 409
   head-SHA-mismatch cases re-enqueue.
6. No consumer relies on the previous read-only assumption.
