## 11A. CI Feedback Contract

This section defines the CI status provider interface and the orchestrator's CI feedback loop.
The CI feedback system is a read-only integration: it queries CI pipeline status for git refs
and injects failure context into agent continuation prompts. It does not trigger builds, manage
pipelines, or write CI configuration.

### Naming convention

CI status providers use the `*Provider` suffix rather than `*Adapter`. The distinction is
intentional: a provider is a read-only, single-method contract (`FetchCIStatus`), while an adapter
(tracker, agent) manages a full lifecycle with multiple operations and bidirectional state. The
naming signals to implementers that the contract surface is minimal.

### 11A.1 CIStatusProvider interface

```go
type CIStatusProvider interface {
    FetchCIStatus(ctx context.Context, ref string) (CIResult, error)
}
```

- `ref` is a git ref string (branch name or commit SHA). Adapters that require a full commit SHA
  MUST resolve branch names to SHAs internally.
- Returns a `CIResult` on success or a `*CIError` on failure. All error categories are non-fatal
  from the orchestrator's perspective.
- Implementations MUST be safe for concurrent use. The orchestrator's reconcile loop may call
  `FetchCIStatus` for multiple workspaces concurrently.

### 11A.2 CIResult structure

```text
CIResult:
  status:        CIStatus         # aggregate pipeline status
  check_runs:    []CheckRun       # individual check runs
  log_excerpt:   string           # truncated log from first failing check
  failing_count: int              # precomputed count of failing check runs
  ref:           string           # echoed git ref for observability
```

`CIStatus` is an enum with three values:

| Value | Meaning |
|-------|---------|
| `pending` | CI checks are still running, no checks have been reported, or a completed check concluded `cancelled` while no check is failing. |
| `passing` | All checks completed successfully. |
| `failing` | At least one check completed with a failure or timed-out conclusion. |

Two completed conclusions are failing: `failure` and `timed_out`. A completed check whose
conclusion is `cancelled` reached no result, so it is neither a passing nor a failing statement
about the commit: it withholds a passing verdict without asserting a failing one. A `neutral` or
`skipped` conclusion is non-failing, and an unmappable platform conclusion maps to `pending`, which
is also non-failing. The aggregate answers in that order: failing when any run's conclusion is
`failure` or `timed_out`; otherwise pending when a run has not completed or a completed run
concluded `cancelled`; otherwise passing. A completed run carrying a `pending` conclusion still
counts toward `passing`.

Every forge that exposes both a CI provider and a source-control merge-gate read applies this one
aggregation rule, so neither reader can invent its own definition of failing. Only the GitHub and
GitLab adapters can produce a `cancelled` conclusion; Gitea's commit-status vocabulary has no
equivalent, so the shared rule governs a case two of the three adapters can reach. The two readers
can still reach different verdicts when they read different signals: on GitHub the CI provider
reads check runs only, while the merge gate reads the combined commit status as well, so a commit
whose sole failing signal is a legacy commit status is passing to one and failing to the other. On
GitLab the CI provider derives its verdict from the normalized run set. The merge gate answers from
the platform's own pipeline aggregate for every status except a pipeline blocked on a manual
action, which is the one status resolved from that pipeline's own job set; a `canceled` pipeline
answers pending, withholding green without asserting failure, in step with what a cancelled
conclusion does to the aggregate. What remains is the shape where the platform reports a
settled, non-failing pipeline that carries no run at all: the merge gate answers from the
aggregate, and the CI provider answers `pending` from an empty run set.

The two readers are also addressed differently, which is a second reason their answers can differ.
`SCMAdapter.GetCIStatus` takes a pull-request identity (`prNumber`, `owner`, `repo`). On GitLab the
merge gate answers from the head pipeline only when that pipeline describes the pull request's
current head, and defers otherwise; `CIStatusProvider.FetchCIStatus` takes a caller-supplied ref
string and answers about that ref, which may no longer be the pull request's head.

Each `CheckRun` contains:

- `name`: check name as defined by the CI platform (e.g. `test`, `lint`, `build`).
- `status`: execution status (`queued`, `in_progress`, `completed`).
- `conclusion`: normalized outcome (`success`, `failure`, `cancelled`, `timed_out`, `neutral`,
  `skipped`, `pending`). Meaningful only when status is `completed`. Unknown or unmappable
  platform conclusions map to `pending`.
- `details_url`: web URL to the check run's detail page. Empty string when unavailable.

`log_excerpt` handling:

- CI logs may contain secrets accidentally printed by build scripts. Adapters MUST truncate to
  the configured `max_log_lines` and SHOULD strip ANSI escape sequences.
- Consumers MUST NOT persist log excerpts to the database or expose them via unauthenticated API
  endpoints.
- The orchestrator omits the log section from the continuation prompt when `log_excerpt` is empty.

`CIResult.ToTemplateMap()` converts the result to a `map[string]any` with snake_case keys
(`status`, `check_runs`, `log_excerpt`, `failing_count`, `ref`) for use as the `ci_failure`
prompt template variable (Section 12.1).

### 11A.3 CIError type

```text
CIError:
  kind:    CIErrorKind    # normalized error category
  message: string         # operator-friendly description
  err:     error          # underlying error (may be nil)
```

Error categories:

| Kind | Meaning |
|------|---------|
| `ci_transport_error` | Network or transport failure (connection, DNS, TLS). |
| `ci_auth_error` | Authentication or authorization failure (expired token, insufficient scopes). |
| `ci_api_error` | Non-success HTTP status or API-level error (rate limiting, server error). |
| `ci_not_found` | Requested ref or repository does not exist (HTTP 404). |
| `ci_payload_error` | Malformed or unexpected response structure. |

`CIError` implements `Error()` and `Unwrap()` for use with `errors.Is`/`errors.As`.

Orchestrator behavior on CI errors:

- Log a warning with the ref and error category.
- Re-enqueue the pending CI check entry for the next reconciliation tick.
- Increment `sortie_ci_status_checks_total{result="error"}`.

### 11A.4 Reconcile loop integration

CI status reconciliation runs as Part C of active run reconciliation (Section 8.5), after tracker
state refresh. The flow is:

1. Skip entirely when `ci_feedback.kind`/`reactions.ci_failure.provider` is not configured (no
   `CIStatusProvider` constructed), or when no SCM adapter is configured (no `SCMAdapter`
   constructed).
2. For each entry in `pending_reactions` with kind `ci`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Check the watch window (Section 11A.9): past `watch_window_ms` from the last recorded head,
      drop the entry and its attempt counter.
   b1. If the entry holds a triage run that has not finished (Section 5.3.9), re-enqueue it ready for the next tick and continue. No provider call is made and the pending attempt count is untouched.
   c. Resolve the pull request's current head via `SCMAdapter.GetMergeability`, passing the pull
      request number, owner, and repo carried in the entry's data. This runs on every due pass; the
      ref is never a value captured once at worker exit.
   d. On an `ErrSCMNotFound` failure: drop the entry and its attempt counter; the watch ends.
   e. On any other failure: re-enqueue the entry with an exponential backoff delay derived from the
      poll interval and the pending attempt count; continue.
   f. If the pull request is merged: drop the entry and its attempt counter; the watch ends. If the
      pull request is closed without merging: same outcome. The merged check runs before the closed
      check, so a provider whose closed state subsumes merging still ends the watch through the
      merged branch.
   g. If the resolved head is empty: re-enqueue with backoff, recording no head and spending no
      attempt.
   h. Apply fingerprint deduplication against the resolved head (Section 11A.5). A differing stored
      head opens a new epoch (Section 11A.9) before the status read. Entries already dispatched for
      the resolved head are dropped for this tick with backoff, skipping the status read.
   i. Call `CIStatusProvider.FetchCIStatus` with the resolved head.
   j. On fetch error: re-enqueue the entry with an exponential backoff delay derived from the poll
      interval and the pending attempt count; continue.
   k. On `passing`: clear `reaction_attempts` for the issue and kind and keep watching, re-enqueuing
      the entry rather than retiring it, so a later commit on the same pull request is still
      observed.
   l. On `pending`: re-enqueue the entry with the same exponential backoff as the fetch-error path.
   m. On `failing`: handle CI failure (see Section 11A.6).

Every branch that does not end the watch re-enqueues the entry.

### 11A.5 Fingerprint and deduplication

The stored fingerprint value is the pull request head this process last evaluated, read live from
`PRMergeStatus.HeadSHA` on the same pass, not a ref captured once at worker exit. Unlike the
merge-conflict (Section 11E.4) and review (Section 11B.7) fingerprints, which each hash their input
with SHA-256, the CI fingerprint is stored as the raw head string with no hashing.

The pass reads the `reaction_fingerprints` row (kind `ci`) before writing it, because the pass
needs the previous head to detect a change: an upsert-then-read ordering would always read back the
value just written, and no head change would ever be detected. A stored value that differs from the
resolved head opens a new epoch (Section 11A.9): the pass applies the epoch transition, then
upserts the resolved head, which resets `dispatched` to false and re-arms the entry for a fresh
CI-fix dispatch. When the stored head equals the resolved head and `dispatched` is already true, the
entry is dropped for this tick with backoff rather than proceeding to a status read: CI status is
not polled again for that head until a later worker exit, startup recovery, or a further head change
creates or re-arms a pending entry.

Fingerprint errors are logged, and the two directions differ. A failed upsert defers the epoch
transition: the entry is re-enqueued with backoff and the pass ends without applying it, because the
durable head must advance before the runtime boundary does. The transition restarts the watch clock
and re-arms the once-per-epoch escalation, so applying it against a record that did not advance
repeats both on every later pass, and the age basis never grows old enough for the watch window to
elapse. A failed read suppresses epoch detection for that pass rather than fabricate a change:
treating a read failure as a head change would reset the retry budget on a database error.

The `dispatched` flag is not set anywhere in this reconcile pass. `handleCIFailure` (Section 11A.6)
schedules the CI-fix continuation through the shared retry machinery with `ReactionKind` set to
`ci`; the flag is marked only once that continuation retry actually fires and dispatches, in the
shared retry-dispatch path, not in the CI reconcile pass itself.

The fingerprint row is never deleted for kind `ci`. It is the epoch record: neither a `passing`
result (Section 11A.4) nor escalation (Section 11A.7) removes it, because doing so would erase the
head this process last evaluated and make the next pass read a head change that did not happen.

### 11A.6 CI failure handling

The failing verdict describes the head resolved on this same pass (Section 11A.4), never a value
captured at worker exit. When CI status is `failing`, the pass first consults the retry slot
(Section 7.5). A non-nil incumbent means the pass defers: it re-enqueues the pending entry with
backoff, and none of the steps below run for this tick: no run-history row is appended, no counter
increments, and escalation is not evaluated. The retry-slot deferral and the fingerprint dedup
(Section 11A.5) both run before any attempt-counter increment: the fingerprint dedup on the current
head's prior pass, and the retry-slot deferral on this pass, so a continuation already queued for
the issue cannot spend a second attempt.

On a free slot the pass runs the triage gate before anything below, because step 1 writes a durable row that a resuming pass would write twice. The gate is inert when `reactions.ci_failure.triage` is absent, and otherwise resolves as follows.

- The first pass to reach it with a new head starts the triage run and re-enqueues the entry. No run-history row is appended, no counter increments, and no continuation is scheduled.
- A later pass reading `dispatch-agent`, which is also the fallback for every failure mode, falls through to the steps below unchanged.
- A later pass reading `handled` marks the fingerprint dispatched and re-enqueues the entry with the delay the already-dispatched branch of Section 11A.5 applies. `reaction_attempts` is untouched, no run-history row is appended, and no continuation is scheduled, so a handled head and a dispatched one are indistinguishable from the next pass onward.
- A later pass reading `escalate` marks the fingerprint dispatched and escalates (Section 11A.7) with the un-incremented value of `reaction_attempts[issue_id:ci]`.
- The outcome is retained on the entry, so repeated passes over the same head re-apply the stored answer rather than starting a second run. A memoized `escalate` re-applies as `handled`, so a stored answer cannot post a second escalation. A new head discards the retained handle, cancelling the run when it is still in flight, and starts a fresh one.

Nothing re-checks a `handled` answer, and on this kind nothing ages one out either once the script pushes. The watch window is measured from the last recorded head, and a push records a new one, so a script that answers `handled` on each of a succession of heads it pushed itself restarts the window its own answer would otherwise expire against. Such an entry spends no attempt, appends no run-history row, fires no escalation, and never expires, so a failing build stays neither fixed nor reported. The bound is the operator's to supply in the script; the orchestrator imposes none.

On a free slot with the gate proceeding:

1. Persist a CI-failure run history entry (`status: ci_failed`).
2. Increment `reaction_attempts[issue_id:ci]`.
3. If `reaction_attempts[issue_id:ci]` exceeds `ci_feedback.max_retries`: escalate (Section 11A.7).
4. Otherwise:
   a. Convert `CIResult` to a template map via `ToTemplateMap()`.
   b. Schedule a CI-fix dispatch carrying the CI failure context. The retry entry's
      `ContinuationContext` field carries the map through the timer to the worker goroutine.
   c. The worker injects the context into the prompt on turn 1 via `prompt.WithContinuationContext`.

CI-fix dispatches count toward the regular retry machinery but use a fixed delay rather than
exponential backoff.

### 11A.7 Escalation behavior

Two conditions reach the escalation: `reaction_attempts[issue_id:ci]` exceeding `ci_feedback.max_retries`, and a triage command answering `escalate` (Section 11A.6). The action, the metric label, the claim release, and the entry, counter, and fingerprint post-conditions are the same for both. Only the log message and, on the `comment` action, the posted text differ: a triage escalation states that the command asked for a person and does not claim a budget was exhausted.

In either case:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`. The label call runs in a detached goroutine with a 30-second
  timeout.
- `escalation: comment`: post a plain-text comment listing the ref, attempt count, and the names,
  conclusions, and details URLs of exactly the checks the verdict counted as failing. The reaction
  layer applies the same shared classification rather than restating the conclusion set; it MUST
  NOT import an adapter package. The comment call runs in a detached goroutine with a 30-second
  timeout.

Escalation is a per-epoch soft stop, not a terminal action. The configured action applies at most
once per recorded head: a further pass over an already-escalated head neither re-applies the action
nor dispatches. After escalation:

- Cancel any pending retry for the issue.
- Delete the persisted retry entry from SQLite.
- Release the claim (`delete claimed[issue_id]`).
- The pending entry, its `reaction_attempts` counter, and its reaction fingerprint row for kind `ci`
  all survive. The counter stays over budget so no further continuation dispatches until a new
  epoch resets it (Section 11A.9), and the fingerprint row is the epoch record. The entry
  re-enqueues with backoff rather than being dropped, so the watch continues past exhaustion.

Escalation failures are logged and counted (`sortie_ci_escalations_total{action="error"}`) but do
not block claim release.

### 11A.8 Adapter registration

CI status providers register via the CI provider registry using `init()` functions, following the
same pattern as tracker and agent adapters:

```go
func init() {
    registry.CIProviders.Register("github", NewGitHubCIProvider)
}
```

The `CIProviderConstructor` signature is:

```go
type CIProviderConstructor func(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error)
```

The `maxLogLines` parameter comes from `ci_feedback.max_log_lines`. The `adapterConfig` parameter
comes from the `extensions` sub-object keyed by `ci_feedback.kind`. Startup merges tracker
credentials (API key, project, endpoint) into that config only when `tracker.kind` and
`ci_feedback.kind` match.

**Gitea provider.** A Gitea CI status provider registers under kind `gitea`, at parity with the
GitHub provider. Gitea exposes no check-runs API, so the provider reads the combined commit status
as the single source and maps each status entry to a `CheckRun`. It computes the aggregate from
those entries, not from the combined status's top-level state, which Gitea reports as `pending` for
a commit that carries no CI at all. The empty case is detected by a zero status count, so a no-CI
ref reports `pending` from an empty check set rather than a false `passing` or `failing`. A
`warning` status is treated as non-failing. The failing-check log excerpt is drawn only from the
status description and target URL already present in the authenticated response; the provider never
fetches the target URL, so the trust boundary stays at the Gitea API.

**GitLab provider.** A GitLab CI status provider registers under kind `gitlab`. Every ref is
resolved to a full commit SHA first, through the commit-retrieval route, which accepts a branch or
tag name where the commit-status route documents its path segment as a commit hash. The same commit
response names the pipeline the read is scoped to, so a superseded pipeline's entries cannot hold a
green commit at failing; the scope is applied twice, once as a query parameter and again after
decoding, so a deployment that ignores the parameter cannot widen it. Each entry of that pipeline
maps to one check run, with `allow_failure` folded into the conclusion. The failing-check log
excerpt is a real job trace, fetched under a byte cap and sanitized, which is the capability Gitea
has no equivalent for. The trace route ignores a range request, so a trace exceeding the cap yields
the tail of the fetched prefix rather than the true tail, and an entry that is an externally
reported status rather than a pipeline job has no trace at all, leaving the excerpt empty.

### 11A.9 The head watch and the feedback epoch

The pull request's current head is the reaction's subject. Every due pass resolves the head live
through `SCMAdapter.GetMergeability` (Section 11A.4); there is no ref captured once and frozen for
the entry's lifetime.

**Age and the watch window.** The entry's age is measured from `HeadRecordedAt`, the UTC time this
process last recorded a head, falling back to the entry's creation time before any head has been
recorded. `reactions.ci_failure.watch_window_ms` bounds that age (default `86400000`, twenty-four
hours; `0` removes the clock bound; a value above `9223372036854` is rejected when the typed
configuration is built). Past the window, the entry and its attempt counter are dropped
and a warning is logged; the fingerprint row is left intact. The watch also ends, whatever the clock
says, on merge, on close, on a missing pull request (`ErrSCMNotFound`), and when the tracker issue
reaches a `tracker.terminal_states` state.

**The epoch.** An epoch is the span over which the pull request's head is one commit. The
`reaction_fingerprints` row for kind `ci` is the durable epoch record: it holds the head this
process last evaluated, read before it is written on every pass (Section 11A.5). A stored value that
differs from the freshly resolved head closes the current epoch and opens a new one. The reaction
re-arms on the transition: the fingerprint upsert clears `dispatched`, so a failing verdict on the
new head is not deduplicated against the old one.

**Attribution.** On an epoch transition the orchestrator classifies the change as positively not its
own work, or as unknown; there is no third answer, and in particular no answer that names a person.
The change is `not_orchestrator` only when no worker session for the issue ran between the recorded
head and the read: no live entry in the in-memory running, retry, or claimed state, and no
`run_history` row completed inside the interval (excluding `ci_failed` rows, which record a verdict
this reaction observed rather than a worker session). Every failure path, including the first
observation and any observation immediately after a restart, answers unknown. The backoff counter
(`PendingAttempts`) and the per-head escalation flag (`EscalatedForCurrentHead`) reset
unconditionally on every transition; the `reaction_attempts` retry budget resets only when the
answer is `not_orchestrator`. A transition never dispatches by itself: a continuation follows only
if the pass's subsequent status read then finds a failing verdict.

**Identity in seeding and recovery.** The reaction's data (`CIReactionData`) carries the pull request
number, owner, and repository alongside the branch and the head at worker exit, because resolving a
head requires knowing which pull request to ask about. Worker-exit seeding and startup recovery both
require this identity in full; a workspace whose SCM metadata names a branch but no pull request
gets no CI watch. A recovered entry leaves `HeadRecordedAt` zero, so the first pass after a restart
records a baseline and classifies it unknown rather than resetting a counter against a boundary this
process never observed.

