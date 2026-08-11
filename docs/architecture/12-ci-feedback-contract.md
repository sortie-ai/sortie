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
| `pending` | CI checks are still running or no checks have been reported. |
| `passing` | All checks completed successfully. |
| `failing` | At least one check completed with a failure, timed-out, or cancelled conclusion. |

The aggregate treats exactly three completed conclusions as failing: `failure`, `timed_out`, and
`cancelled`. A `neutral` or `skipped` conclusion is non-failing, and an unmappable platform
conclusion maps to `pending`, which is also non-failing. Whether the aggregate defers is decided by
the run's `status`, not its conclusion: a run that has not completed holds the aggregate at
`pending`, while a completed run carrying a `pending` conclusion counts toward `passing`.

Every forge that exposes both a CI provider and a source-control merge-gate read applies this one
aggregation rule, so neither reader can invent its own definition of failing. The two can still
reach different verdicts when they read different signals: on GitHub the CI provider reads check
runs only, while the merge gate reads the combined commit status as well, so a commit whose sole
failing signal is a legacy commit status is passing to one and failing to the other.

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

1. Skip entirely when neither `ci_feedback.kind` nor `reactions.ci_failure.provider` is configured
   (no `CIStatusProvider` constructed).
2. For each entry in `pending_reactions` with kind `ci`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Determine the ref (SHA preferred, branch as fallback) and apply fingerprint deduplication
      (Section 11A.5). Entries already dispatched for this exact ref are dropped for this tick.
   c. Call `CIStatusProvider.FetchCIStatus` with the ref.
   d. On fetch error: re-enqueue the entry with an exponential backoff delay derived from the
      poll interval and the pending attempt count; continue.
   e. On `passing`: clear `reaction_attempts` for the issue and kind, and delete the fingerprint
      row.
   f. On `pending`: re-enqueue the entry with the same exponential backoff as the fetch-error
      path.
   g. On `failing`: handle CI failure (see Section 11A.6).

### 11A.5 Fingerprint and deduplication

Before calling `FetchCIStatus`, the reconcile pass deduplicates against the same ref. The
fingerprint value is the ref itself (`CIReactionData.SHA`, falling back to `CIReactionData.Branch`),
the identical string passed to the status fetch. Unlike the merge-conflict (Section 11E.4) and
review (Section 11B.7) fingerprints, which each hash their input with SHA-256, the CI fingerprint
is stored as the raw ref string with no hashing.

The pass upserts the ref into `reaction_fingerprints` (kind `ci`) and reads the row back. The
upsert resets `dispatched` to false whenever the stored fingerprint differs from the ref being
upserted, so a new SHA (or a branch ref that has moved to a new SHA) always re-arms the entry for a
fresh CI-fix dispatch. When the read-back fingerprint matches the current ref and `dispatched` is
already true, the entry is dropped for this tick rather than re-enqueued: CI status is not polled
again for that ref until a later worker exit or startup recovery creates a new pending entry.
Fingerprint errors are logged and treated as non-fatal, but the two directions differ. A failed
upsert does not disable dedup: the read-back still runs, so a stored row matching the current ref
and already marked dispatched still drops the entry. Only a failed read-back skips dedup entirely,
and the pass then proceeds to `FetchCIStatus` rather than dropping the entry.

The `dispatched` flag is not set anywhere in this reconcile pass. `handleCIFailure` (Section 11A.6)
schedules the CI-fix continuation through the shared retry machinery with `ReactionKind` set to
`ci`; the flag is marked only once that continuation retry actually fires and dispatches, in the
shared retry-dispatch path, not in the CI reconcile pass itself.

The fingerprint row is deleted, not merely left stale, when the episode closes: on a `passing`
result (Section 11A.4) and during escalation (Section 11A.7).

### 11A.6 CI failure handling

When CI status is `failing`, the pass first consults the retry slot (Section 7.5). A non-nil
incumbent means the pass defers: it re-enqueues the pending entry unchanged except for a
refreshed `CreatedAt`, and none of the steps below run for this tick — no run-history row is
appended, no counter increments, and escalation is not evaluated.

On a free slot:

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

When `reaction_attempts[issue_id:ci]` exceeds `ci_feedback.max_retries`:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`. The label call runs in a detached goroutine with a 30-second
  timeout.
- `escalation: comment`: post a plain-text comment listing the ref, attempt count, failing check
  names, conclusions, and details URLs. The comment call runs in a detached goroutine with a
  30-second timeout.

After escalation:

- Cancel any pending retry for the issue.
- Delete the persisted retry entry from SQLite.
- Release the claim (`delete claimed[issue_id]`).
- Clear all `reaction_attempts` and `pending_reactions` entries for the issue.
- Delete the reaction fingerprint row for kind `ci`.

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
resolved to a full commit SHA first, because the commit-status route matches its path segment
literally and answers a branch name or an abbreviated SHA with an empty list. The same commit
response names the pipeline the read is scoped to, so a superseded pipeline's entries cannot hold a
green commit at failing. Each entry of that pipeline maps to one check run, with `allow_failure`
folded into the conclusion. The failing-check log excerpt is a real job trace, fetched under a byte
cap and sanitized, which is the capability Gitea has no equivalent for.

