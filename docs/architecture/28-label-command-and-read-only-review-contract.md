## 11F. PR Label Command Contract

Every reaction kind in §11A through §11E observes a durable condition: a CI conclusion, a
review-comment set, a mergeability state. The reconcile loop re-reads that condition on each tick
and deduplicates it against a persisted fingerprint, so a missed tick loses nothing because the
fact remains re-readable. This section defines three mechanisms that together let an operator
command work on a Sortie-managed pull request by applying a label, a discrete human gesture rather
than a durable condition. The first is a journal-polling detection substrate that turns each
labeling gesture into an at-most-once dispatch. The second is a no-clone, read-only dispatch posture
that reviews a PR without a checkout. The third is a read-write, clone-and-checkout dispatch posture
that pushes review-feedback fixes to the PR branch. ADR-0015 records the decision to detect label
commands by polling the journal; this section specifies the detection substrate and both dispatch
postures it drives.

The command surface is a label because the platform grants labels a permission gate that comments
lack (on GitHub, applying a label requires the triage role), and because Sortie's SCM and tracker
vocabulary is already label-based. The gesture is consumed rather than standing: after a confirmed
command the orchestrator removes the label, so the label reads as a one-shot button. The first
command is a read-only review, whose runtime and persisted reaction-kind discriminator is
`label-review`. A fix command that pushes review-feedback changes to the PR branch is the second
command over the same substrate and configuration surface (ADR-0015); its runtime discriminator is
`label-fix`.

Three names denote three distinct concepts and MUST NOT be conflated: `label_commands` is the
`WORKFLOW.md` configuration block; `label-review` (hyphen) is the runtime and persisted
reaction-kind discriminator; `label_review` (underscore) is the prompt continuation key. The fix
command reuses the same configuration block with a second triple: `label-fix` (hyphen) is its
runtime and persisted reaction-kind discriminator, and `label_fix` (underscore) is its prompt
continuation key. This is the same YAML-versus-runtime asymmetry §11C.4 and §11E document for
auto-merge and merge-conflict.

The label-review and label-fix reactions coexist with every other reaction kind, and with each
other, on the same PR without interference. Each owns a distinct `pending_reactions` entry and
`reaction_fingerprints` row, because every key is composed via `ReactionKey(issue_id, kind)` and the
fingerprint table primary key is `(issue_id, kind)`.

### 11F.1 SCMAdapter interface widening

Detection adds one read and one write to the `SCMAdapter` interface, additive per the adapter
pattern of ADR-0003 and following the read-write widening precedent ADR-0012 set for auto-merge. It
introduces one normalized journal type:

```go
type LabelEvent struct {
    ID    string    // journal entry id, unique per PR; sorts lexically in journal order
    Label string    // normalized (lowercased) label name
    Actor string    // login of the acting user
    Added bool      // true = labeled, false = unlabeled
    At    time.Time // journal timestamp, UTC
}

ListLabelEvents(ctx context.Context, prNumber int, owner, repo string) ([]LabelEvent, error)
RemoveLabel(ctx context.Context, prNumber int, owner, repo, label string) error
```

- `ListLabelEvents` returns the PR's label-event journal oldest-first, a non-nil empty slice when
  the PR has no label events, or a `*SCMError` on failure.
- `RemoveLabel` removes the named label. It returns nil on success and maps an already-absent label
  (a `*SCMError` of kind `scm_not_found`) to a successful no-op, matching the `DeleteBranch`
  precedent (§11C). Any other failure is a `*SCMError`.
- `prNumber`, `owner`, and `repo` are sourced from `SCMMetadata` (§9.5), never from tracker project
  configuration.
- Implementations MUST normalize provider field names at the boundary; platform-specific field
  names MUST NOT leave the adapter package. `Label` is lowercased by the adapter so the orchestrator
  never re-normalizes it.
- `ID` MUST be normalized by the adapter so that lexicographic string order matches the journal's
  chronological order among events sharing a timestamp; the orchestrator uses the id as the
  tiebreaker when it compares positions by `(At, id)`. Providers with numeric journal ids satisfy
  this by zero-padding the decimal id to a fixed width (the GitHub adapter pads to 19 digits, the
  int64 maximum). An adapter whose native ids do not sort lexically in journal order MUST derive a
  sortable surrogate at the boundary; raw UUIDs do not qualify.
- Implementations MUST be safe for concurrent use.

No diff-fetch method and no review-comment-posting method are added. The reviewing agent fetches the
diff and posts comments through its own SCM tooling (§11F.6).

### 11F.2 Label-event journal detection substrate

A snapshot of current labels cannot see a label applied and removed between two ticks, cannot
distinguish removed-and-reapplied from unchanged, and names no actor. The label-event journal
removes all three limits: each labeling action is a durable, uniquely identified, timestamped,
attributed record the orchestrator can re-read at any time, so polling loses nothing, the record
identity is a natural deduplication key, and the actor comes for free (ADR-0015).

On GitHub the journal is the per-issue events endpoint (pull requests are issues for this API
family), whose `labeled` and `unlabeled` entries carry a unique id, the label name, the acting user,
and a timestamp. The endpoint is oldest-first and offers no server-side since-filter, and the command
and its retraction signal live on the newest entries. The adapter therefore pages from the tail: it
follows the response `Link` relation `rel="last"` to the final page and walks backward via
`rel="prev"`, retaining the newest events within a fixed page cap, then re-flattens to oldest-first.
When the journal exceeds the cap the adapter emits a warning naming the PR, so a real command is
never silently dropped on an event-heavy PR. Managed PRs are short-lived and low-traffic, so
journals stay small; a page cache is an available optimization if profiling ever demands one.

The abstraction is portable, and the GitLab adapter fits it without a contract change. On GitLab
the journal is the merge request's resource label-event route, whose entries carry the same id,
user, action, and timestamp, and whose numeric ids satisfy the `ID` ordering rule through the same
zero-padding the GitHub adapter applies. The GitLab adapter walks that journal forward rather than
from the tail, and sorts the normalized events ascending by `(At, ID)` itself, so ordering does not
depend on the server's. An entry whose label was later deleted from the project renders its label
object null and is skipped, because it carries no name to normalize. The forward walk retains the
events GitLab serves first when a journal exceeds the page cap, where the GitHub tail walk retains
the newest, and the adapter warns when the cap is reached.

### 11F.3 Command contract and detection invariants

The unit of command is one `labeled` journal event whose normalized label name matches a configured
command label on a Sortie-managed PR. Detection maintains a per-PR high-water mark, an opaque
position formed from the newest processed event's timestamp and id (`"<timestamp>|<id>"`), compared
lexically by `(At, id)`. Both components are normalized to a fixed-width, lexically-sortable form.
The orchestrator formats the timestamp as RFC 3339 UTC with all nine fractional-second digits
forced (a trailing-zero-trimming layout such as Go's `RFC3339Nano` is unsuitable: it breaks lexical
ordering between timestamps of differing precision); the adapter normalizes the id per the
`LabelEvent.ID` contract (§11F.1). Lexical order therefore matches chronological order even for
events that share a timestamp.

Each due tick reads the journal and considers only events whose position sorts strictly after the
stored mark. The following invariants hold:

- **The mark always advances.** On every tick that reads new events the mark advances to the newest
  examined event, including `unlabeled` entries, foreign labels, and retracted or self-authored
  gestures. The mark tracks journal position, not command history.
- **Burst collapse.** All matching `labeled` events in one batch collapse to at most one command.
  Applying a label twice between ticks is one intent, not two.
- **Retraction window.** A command is confirmed only if its label is still present on the PR at
  detection time. When new matching `labeled` events exist but the label is absent, the gesture was
  retracted: the mark advances and nothing dispatches. This gives the operator a cancellation window
  bounded by the poll interval.
- **Depth one per kind.** A gesture arriving while a command of the same kind is queued or its
  session is running collapses into the outstanding command; no second entry per kind is created.
  This rests on the single pending entry per `ReactionKey(issue_id, "label-review")`, the
  running-or-queued guard, and cancel-before-schedule at dispatch.
- **At-most-once across restart.** The mark is persisted before the dispatch is scheduled. A crash
  in the narrow window between persisting the mark and scheduling loses the command (the operator
  re-applies the label) rather than duplicating it, because the advanced mark is already in SQLite
  and the processed event never again sorts after it. At-most-once rests solely on the advancing
  mark.
- **Self-authored events ignored (best-effort).** A filter for `labeled` events authored by the
  orchestrator's own identity is not run in this slice because no orchestrator identity is wired; the
  structural guarantee that Sortie never applies command labels makes its absence a no-op.

After scheduling a dispatch the orchestrator removes the command label as the operator-visible
acknowledgment: present means "queued, not yet picked up"; disappearance means "accepted" (or
"retracted by you"); re-applying is the single gesture to issue the next command. This is the
deliberate contrast with a standing-instruction label, which stays on the PR and describes a desired
steady state. Correctness never depends on the removal: deduplication rests entirely on the mark, so
a failed removal (missing write scope, transport error) logs a warning and leaves a stale label
behind, degrading only the one-shot-button reading until the operator removes it manually. The
removal itself produces an `unlabeled` entry, which is never a command.

### 11F.4 Reconcile loop integration

Label-command detection runs as a reconcile pass alongside the sibling reaction passes in active run
reconciliation (§8.5). It is skipped entirely when no `SCMAdapter` is constructed or the
label-review feature is not configured. Ordering relative to the other passes does not affect
correctness: the pass is fully cross-kind isolated.

Loop body per `label-review` entry in `pending_reactions`:

1. Remove the entry from the map (prevents reprocessing within the same tick).
2. Type-assert the kind data; on mismatch, log and skip.
3. Respect the `PendingRetryAt` poll throttle: if not yet due, re-enqueue and continue. Unlike every
   sibling kind there is no TTL and no drop-on-age branch, because a human gesture stays actionable
   regardless of age.
4. Read the stored mark from `reaction_fingerprints`. The `dispatched` flag is not consulted for
   this kind (§11F.5).
5. Call `ListLabelEvents`. On error, increment the pending-backoff counter, set `PendingRetryAt`,
   re-enqueue, and continue.
6. Select events past the stored mark. If none, re-enqueue at the poll interval with the mark
   unchanged.
7. Compute the newest examined position. Collapse matching `labeled` events to at most one command
   and evaluate the retraction check.
8. When a command is confirmed, decide in this order:
   a. A foreign-kind incumbent occupies the retry slot (Section 7.5): defer. Re-enqueue the entry
      with the mark unchanged and `CreatedAt` refreshed to the tick's `now`; do not upsert the
      mark, do not advance the high-water mark, and do not remove the label.
   b. A same-kind incumbent occupies the slot, or a `label-review` worker is already running:
      collapse. Persist the newest mark and re-enqueue at the poll interval without dispatching.
   c. Otherwise: dispatch. Persist the newest mark, schedule a fresh read-only dispatch carrying
      the PR coordinates into the slot the check above already confirmed free, remove the label
      best-effort, and re-enqueue the entry at the next poll interval.
9. When a command is not confirmed (retraction, foreign labels, or unlabeled-only): persist the
   newest mark and re-enqueue at the poll interval without dispatching. This arm does not consult
   the retry slot.

The re-enqueue at step 8c is the property the sibling passes lack. Worker-exit seeding (§11F.9)
cannot re-arm detection after a review completes, because a read-only exit's claim, even when
retained solely to protect a foreign retry-slot incumbent, is evaluated as released for the
reaction-enqueue gate (Section 7.5), so a label-command dispatch never seeds any reaction kind.
The pass instead keeps the detection entry alive across its own dispatch, carrying the PR identity
in the entry's kind data, so a re-applied label after a completed review is detected on a later
tick without a process restart.

Cross-kind isolation: the pass MUST scope every `pending_reactions` and `reaction_fingerprints`
mutation to `kind = "label-review"`. It MUST NOT read or write any other kind's entry, fingerprint,
or counter, matching the isolation invariant §11C and §11E state for auto-merge and merge-conflict.

A second reconcile pass, for `label-fix`, runs immediately after the label-review pass on every
tick. It is structurally identical to the loop above: it is skipped when no `SCMAdapter` is
constructed or the label-fix feature is not configured, it matches on `fix_label` in place of
`review_label`, and it scopes every `pending_reactions` and `reaction_fingerprints` mutation to
`kind = "label-fix"`, never reading or writing the label-review entry, fingerprint, or counter. On a
confirmed command it schedules the read-write fix dispatch posture (§11F.13) instead of the
read-only review posture. Ordering between the two passes does not affect correctness because each
is fully cross-kind isolated.

### 11F.5 Deduplication storage

The reaction reuses the `reaction_fingerprints` table (§19.2) with the kind discriminator
`label-review`. The primary key `(issue_id, kind)` gives the PR an independent mark, isolated from
every other reaction kind's row. No migration is required because `kind` is TEXT and the key already
isolates kinds.

For this kind the `fingerprint` column stores the high-water mark (the newest processed event's
position), not a hash of observed state (ADR-0015). The column stays opaque TEXT and the table's
upsert semantics are unchanged, but a reader of the schema must know that a fingerprint value is a
digest for some kinds and a position mark for this one.

The `dispatched` flag carries no durable meaning for this kind. The upsert resets `dispatched` to
zero whenever the stored value changes, and the mark changes on every tick that reads new events, so
the flag is cleared before any later tick could read it. The durable at-most-once guarantee
therefore rests solely on the advancing mark. The pass MUST NOT read `dispatched` as a dedup input
and MUST NOT adopt the sibling review pass's "stored fingerprint matches and dispatched" skip, which
for an always-advancing mark would never fire for the intended reason.

### 11F.6 Read-only, no-clone review dispatch

A confirmed review command dispatches a fresh agent session that reviews the PR without a checkout.
Every other dispatch (initial or reaction-driven) prepares a per-issue workspace through the
operator's clone-and-build hooks and starts the session with that workspace as the working
directory. A review needs a working directory for MCP tools and cost accounting, but no checkout and
no branch. The read-only posture supplies exactly that:

- **Scratch workspace, no hooks.** The session obtains a minimal per-issue scratch directory,
  containment-validated under the workspace root exactly as a normal dispatch (§9.6), but the
  operator `after_create`, `before_run`, and `after_run` hooks do not run. There is no clone, no
  build, no branch, and no checkout. The directory is the per-issue workspace reused from an
  earlier session when one still exists on disk, but its presence is not guaranteed: the periodic
  sweep's age bound may have removed it since the pending entry was seeded (§11F.9). When absent,
  the dispatch recreates the directory rather than fail, the same recreate step every dispatch
  posture takes. Because the directory may carry state from a prior session, a stale
  `.sortie/status` is cleared best-effort after creation so it cannot end the review on turn one.
- **Fresh session.** The session starts with no resume identifier. It is a new session, not a
  continuation of a live one.
- **Single selecting flag.** The posture is selected by one worker flag derived from the dispatch
  reaction kind. A `label-review` dispatch runs the read-only path; every other dispatch is
  unchanged.

The read-only path suppresses every issue-work side effect a normal dispatch performs, because a
review claims no work and changes no issue state:

- the dispatch-time transition of the linked issue to the in-progress tracker state;
- the dispatch comment on the linked issue;
- the per-turn tracker-state refresh, and with it the issue-state termination gate that ends a
  normal turn loop when the issue leaves an active state;
- the self-review loop, which has no local checkout to verify;
- the `after_run` teardown hook, on every exit path including panic recovery, because no operator
  setup hook ran on the scratch workspace;
- the worker-exit handoff transition and the active-issue continuation retry, so a clean review exit
  neither hands off nor re-dispatches.

Because the read-only turn loop is not gated on issue state, its turn budget rests on the existing
`agent.max_turns` ceiling plus the agent's own completion signal (the `.sortie/status` control-plane
file, §21). A review is naturally a single turn (fetch the diff, post the review, stop), and
`agent.max_turns` is the backstop that bounds a session that does not self-signal. No new termination
gate is introduced.

Credentials reach the reviewing agent through the orchestrator's process environment: every agent
adapter launches its subprocess with the orchestrator's environment (§10.7), so SCM credentials
present there (the standard token or CLI-auth deployment) are available to a no-clone session even
with the setup hooks skipped. The one unsupported case is auth provisioned solely by a
workspace-local setup hook (for example a checkout-scoped git credential helper written during
`after_create`): a no-clone review runs no such hook, so an operator relying on hook-local auth MUST
also expose the credential in the orchestrator's process environment for review sessions.

The reviewing agent fetches the diff and posts review comments through its own SCM tooling and
credentials, consistent with Sortie's scheduler-and-reader boundary: the orchestrator adds no
diff-fetch or comment-post method to `SCMAdapter` and posts nothing itself. The orchestrator injects
only the PR coordinates, through the prompt continuation key (§11F.7).

A fix command is the read-write counterpart of this posture: it pushes review-feedback changes to
the PR branch and therefore needs a checkout and the content-write scope (ADR-0015). It is not a
read-only dispatch; §11F.13 specifies its dispatch posture.

### 11F.7 Prompt continuation key

The dispatch injects the PR context on turn one through the continuation-key mechanism (§12),
reusing the key channel without implying a session resume: a continuation key merges values into the
template data map and is independent of whether the session resumes. The key `label_review` MUST be
registered so it is seeded to nil in the template data map, so `Option("missingkey=error")` does not
reject a template that references it when no review is active. This is the same registration rule
§11E.6 states for `merge_conflict`.

The `label_review` value is a map carrying the coordinates the agent needs to fetch the diff and
post its review:

```text
label_review (map or nil):
  pr_number:    int      # PR to review
  owner:        string   # repository owner
  repo:         string   # repository name
  actor:        string   # login that applied the review label
  requested_at: string   # RFC3339 timestamp of the confirmed labeling gesture
```

The orchestrator injects only these coordinates, never the diff text. The agent produces a review
only when the operator's prompt template contains a `{{ if .label_review }}` branch instructing it
to fetch the diff and post comments; without that branch a review dispatch runs the normal work
prompt against a scratch directory and posts nothing. Go templates give no signal of which keys a
template referenced, so the orchestrator cannot detect the missing branch at render time and there
is no orchestrator-side posting fallback. Two measures keep the outcome diagnosable rather than
silent: the reconcile emits an informational log at each dispatch recording the PR number and the
acting user, and the workflow loader SHOULD emit a warning when `label_commands` is active with a
non-empty review label but the resolved template text does not reference the `label_review` key.

The key `label_fix` follows the same registration rule for the fix command. Its value mirrors
`label_review` and adds the PR head branch the fix session checks out and pushes to:

```text
label_fix (map or nil):
  pr_number:    int      # PR to fix
  owner:        string   # repository owner
  repo:         string   # repository name
  branch:       string   # PR head branch to check out and push to
  actor:        string   # login that applied the fix label
  requested_at: string   # RFC3339 timestamp of the confirmed labeling gesture
```

The orchestrator injects only these coordinates, never the diff, the review comments, or the
checked-out tree. The agent checks out `label_fix.branch`, addresses the review comments, pushes
fixes to that branch, and posts a summary comment only when the operator's prompt template contains
a `{{ if .label_fix }}` branch instructing it to do so.

The missing-branch case is not a structural no-op for fix, unlike for review. A misfired review
dispatch runs against a scratch directory with no checkout and structurally posts and pushes nothing
(§11F.6). A misfired fix dispatch runs the normal `workspace.Prepare` clone path (§11F.13) and
carries the content-write scope, so it runs the normal work prompt against a real checkout with push
capability; whether it pushes then depends on the operator's normal prompt and the agent's behavior
against an under-review issue, and cannot be asserted to be a no-op. Because of this stronger
residual risk, the workflow loader MUST (not SHOULD) emit a warning when `label_commands` is active
with a non-empty fix label but the resolved template text does not reference the `label_fix` key.
This scan stays advisory: it is a warning, not a configuration error and not an activation gate,
because a text scan over template source is a heuristic that can false-negative a correctly
configured template. The reconcile's informational dispatch log applies to the fix command exactly
as it does to review.

### 11F.8 Configuration and activation

Activation is a single block in the `reactions` family of `WORKFLOW.md` front matter:

```yaml
reactions:
  label_commands:
    provider: github              # required to activate; absent = feature off
    review_label: "sortie:review" # default; "" disables the review command
    fix_label: "sortie:fix"       # default; "" disables the fix command
    poll_interval_ms: 60000       # default; floor 30000
```

The block diverges from the common per-kind reaction schema (§5.3.9): it carries no `max_retries`,
`escalation`, or `escalation_label`, because detection has no escalation posture (a human is in the
loop). It parses through a dedicated path and never appears as an entry in the generic reactions map.

- Activation is by `provider`, never by an `enabled` flag. The block absent or `provider` empty
  means the feature is off and no journal read ever happens for either command. Activation
  considers `fix_label` exactly as it considers `review_label`: a fix-only configuration
  (`review_label` empty, `fix_label` non-empty) activates the block and constructs the SCM adapter
  exactly as a review-only configuration does; the two commands share one activation gate and one
  adapter.
- `review_label` and `fix_label` default to namespaced names (the `sortie:` prefix) to stay clear of
  team label vocabularies, and each is individually disableable by setting it to the empty string.
  Setting `provider` while both labels are empty is a configuration validation error, a loud
  misconfiguration rather than a silently inert block. Because this is a pure config-shape check it
  surfaces offline through `sortie validate`.
- `poll_interval_ms` defaults to 60000 and is clamped up to a floor of 30000, with a warning logged
  at load time. The detection latency contract is one poll interval; against a session that runs for
  minutes the added latency is immaterial.
- Label-name comparison uses the adapter's normalized (lowercased) label names.

Sortie never creates command labels. The operator creates the two labels (write access on GitHub) or
points the configuration at existing ones, mirroring the GitHub tracker adapter's label-hygiene
stance for state labels.

### 11F.9 Scope, seeding, and recovery

Detection covers Sortie-managed PRs only: those whose workspace SCM metadata (§9.5) reports
`pr_number > 0` with non-empty `owner` and `repo`. A pending `label-review` entry is seeded on normal
worker exit when the SCM adapter is configured, the feature is active, and that metadata is present,
matching the seeding predicate the sibling kinds use, except that it requires no branch (a review has
no checkout). The seeded entry carries the dispatch-frozen agent kind, rule name, and template id
copied from the exiting run, so the later read-only session resolves the same adapter and template; a
"create only if absent" guard preserves in-progress detection state across re-exits.

A pending `label-fix` entry is seeded the same way, on normal worker exit, when the SCM adapter is
configured and the fix feature is active, but its predicate adds the branch guard the review
predicate omits: the workspace SCM metadata must additionally report a non-empty head branch. A fix
session checks out that branch, so a PR record without one has nothing to check out and seeds no fix
entry. The seeded entry carries the branch alongside the PR number, owner, and repo, and the same
frozen agent kind, rule name, and template id the review seed carries.

Neither seeding block fires on the command session's own exit, and not because the metadata is
absent: both sessions reuse the per-issue workspace directory, which may still hold the
`.sortie/scm.json` a prior full session wrote. The operative gate is the reaction-enqueue
predicate shared by every seeding block: the issue must have been claimed at exit and the exit must
either take the handoff path or leave the claim held. A read-only review exit and a fix exit
(§11F.13) are both excluded from the handoff path and always release the claim, so the predicate is
never satisfied and no seeding block reads the workspace metadata at all on either exit.
Repeatability after a completed review or fix is carried instead by the reconcile re-enqueue on
dispatch (§11F.4).

Startup recovery re-seeds a `label-review` entry for each recovered active-issue run with the same PR
metadata, again omitting the branch guard, and re-seeds a `label-fix` entry under the same branch
guard seeding uses, so a label applied while Sortie was down is detected on the first tick after
restart (the journal is durable) for either command. The persisted mark is read back from
`reaction_fingerprints` for each kind, so recovery does not reset deduplication state.

When the linked issue reaches a terminal state, the tracker-reconcile terminal-state path removes
the `label-review` or `label-fix` pending entry, whether or not the entry carries a TTL of its own;
neither kind carries an attempt counter to release alongside it. The label-journal poll stops on the
next reconcile tick.

A pending `label-review` or `label-fix` entry no longer excludes its workspace from periodic-sweep
candidacy, because neither kind carries an expiry. Detection is unaffected by that
narrowing: the label-command reconcile pass polls the forge's label-event journal by PR number,
owner, and repository, and compares the result against a high-water mark held in SQLite, reading
nothing from the workspace directory. A workspace the sweep removes by age still leaves detection
observing and dispatching commands for that pull request in the running process, and a dispatch
after the removal recreates the directory rather than fail (§11F.6).

### 11F.10 Authorization posture

The platform's label permission is the authorization gate: on GitHub, applying a label requires the
triage role or higher, a real gate that comments lack. The residual risk is stated openly: a
triage-level user who cannot push code can command an agent session that pushes to the PR branch
(through the fix command). The risk is bounded because the branch still lands only through the
ordinary review and merge gates, and because the per-issue session and token ceilings (§8.4) cap the
compute a PR can be commanded into. The journal supplies the actor, recorded in the dispatch context
and logs; an actor allowlist is a compatible future extension for which the data already flows.

### 11F.11 Error and failure model

All SCM errors are non-fatal, matching the SCM error posture of §11B. Detection has no escalation
posture and no retry budget: a human is in the loop by construction and re-gestures when nothing
visibly happens, and failures of the dispatched session flow through the normal agent retry and
budget machinery.

| Condition | Visibility | Recovery |
|-----------|------------|----------|
| `ListLabelEvents` returns a `*SCMError` | Warn with PR number and error kind | Increment per-entry backoff, set `PendingRetryAt`, re-enqueue; retried on the next due tick. No escalation. |
| `RemoveLabel` returns a `*SCMError` (including a scope gap) | Warn with PR number and label | None required; deduplication is already committed. The label lingers until removed manually. |
| Fingerprint read fails | Warn | The pass backs off and re-enqueues without dispatching: at-most-once rests solely on the mark, so a command must not dispatch when the stored mark cannot be read. |
| Fingerprint upsert fails | Warn | The pass proceeds; the durable guarantee degrades to best-effort for that tick, matching the siblings. |
| `provider` set with both labels empty | Config error surfaced by config load and `sortie validate` | The operator fixes the config; the workflow loader retains the previous good config until fixed. |
| `poll_interval_ms` below the floor | Warn at load | Clamped to 30000; the run continues. |
| Crash between persisting the mark and scheduling | Startup logs | The command is lost, not duplicated (§11F.3); the operator re-applies the label. |
| Review feature active but the template lacks the `{{ if .label_review }}` branch | Info dispatch log always present; advisory Warn at prompt load | The session posts no review; the operator adds the documented branch. No orchestrator-side posting fallback exists. |
| The fix session cannot check out the PR head branch, apply the review feedback, or push | Agent event stream, normal turn-failure logging | The turn fails through the normal agent machinery; no orchestrator-side push exists to fail. The operator re-applies the label to retry. |
| The content-write scope preflight finds a missing scope at startup | Warn naming the missing scope (not Error; the fix command is default-on) | Advisory only; detection and dispatch proceed. The gap surfaces later as the fix session's push failure (above) and as a `RemoveLabel` warning (above). |
| Fix feature active but the template lacks the `{{ if .label_fix }}` branch | Info dispatch log always present; advisory Warn at prompt load (MUST, stronger than the SHOULD for `label_review`) | The session runs the normal work prompt against a real checkout with push capability, so the outcome cannot be asserted to push nothing. The operator adds the documented template branch. No orchestrator-side fix fallback exists. |

### 11F.12 State machine

Per-issue `label-review` reaction lifecycle (the `issue_id:label-review` slot):

| From | Event | To | Action |
|------|-------|----|--------|
| (none) | Normal worker exit passing the reaction-enqueue gate (§11F.9), SCM adapter and label-review configured, PR metadata present | pending | Seed the entry if absent (frozen agent kind, rule, template). |
| (none) | Startup recovery, recovered active run with PR metadata | pending | Re-seed the entry if absent; read the mark back from SQLite. |
| pending | Reconcile tick, `now < PendingRetryAt` | pending | Re-enqueue, no journal read. |
| pending | Reconcile tick, journal fetch error | pending | Increment backoff, set `PendingRetryAt`, re-enqueue. |
| pending | Reconcile tick, no events past the mark | pending | Re-enqueue at the poll interval; mark unchanged. |
| pending | Reconcile tick, new events but no confirmed command (retraction, foreign or unlabeled only) | pending | Advance the mark; re-enqueue at the poll interval; no dispatch. |
| pending | Reconcile tick, confirmed command, a foreign-kind incumbent occupies the retry slot (Section 7.5) | pending | Defer: re-enqueue with the mark unchanged and `CreatedAt` refreshed; no mark advance, no dispatch, no label removal. |
| pending | Reconcile tick, confirmed command, a `label-review` command already queued or running | pending | Advance the mark; re-enqueue; the gesture collapses into the outstanding command. |
| pending | Reconcile tick, confirmed command, retry slot free | dispatched | Advance the mark; schedule the read-only dispatch; remove the label best-effort; re-enqueue at the poll interval. |
| pending | Issue reaches terminal state (tracker reconcile) | (none) | Drop the pending entry; cancel and delete the issue's retry; release the claim. Detection stops polling on the next tick. |

Per-issue `label-fix` reaction lifecycle (the `issue_id:label-fix` slot) follows the same three
states, differing only in the seeding and recovery guard and in which dispatch the confirmed-command
transition schedules:

| From | Event | To | Action |
|------|-------|----|--------|
| (none) | Normal worker exit passing the reaction-enqueue gate (§11F.9), SCM adapter and label-fix configured, PR metadata present with a non-empty head branch | pending | Seed the entry if absent (frozen agent kind, rule, template, and branch). |
| (none) | Startup recovery, recovered active run with PR metadata carrying a non-empty head branch | pending | Re-seed the entry if absent; read the mark back from SQLite. |
| pending | Reconcile tick, `now < PendingRetryAt` | pending | Re-enqueue, no journal read. |
| pending | Reconcile tick, journal fetch error | pending | Increment backoff, set `PendingRetryAt`, re-enqueue. |
| pending | Reconcile tick, no events past the mark | pending | Re-enqueue at the poll interval; mark unchanged. |
| pending | Reconcile tick, new events but no confirmed command (retraction, foreign or unlabeled only) | pending | Advance the mark; re-enqueue at the poll interval; no dispatch. |
| pending | Reconcile tick, confirmed command, a foreign-kind incumbent occupies the retry slot (Section 7.5) | pending | Defer: re-enqueue with the mark unchanged and `CreatedAt` refreshed; no mark advance, no dispatch, no label removal. |
| pending | Reconcile tick, confirmed command, a `label-fix` command already queued or running | pending | Advance the mark; re-enqueue; the gesture collapses into the outstanding command. |
| pending | Reconcile tick, confirmed command, retry slot free | dispatched | Advance the mark; schedule the fix dispatch (§11F.13); remove the label best-effort; re-enqueue at the poll interval. |
| pending | Issue reaches terminal state (tracker reconcile) | (none) | Drop the pending entry; cancel and delete the issue's retry; release the claim. Detection stops polling on the next tick. |

A PR record whose head branch is empty seeds and recovers no `label-fix` entry at all: the slot
stays absent rather than entering `pending`, because a fix session with nothing to check out is
never scheduled.

### 11F.13 Read-write, clone-and-checkout fix dispatch

A confirmed fix command dispatches a fresh agent session that checks out the PR head branch and
pushes review-feedback fixes to it. Unlike the review posture (§11F.6), the fix session pushes
commits, so it needs the same real per-issue workspace and operator hooks a normal dispatch uses:

- **Full workspace, hooks run.** The session takes the same `workspace.Prepare` path a normal
  dispatch takes: the operator `after_create` and `before_run` setup hooks run, cloning the
  per-issue directory when it is absent and reusing the existing checkout when it is present. The
  `after_run` teardown hook runs on every exit path, including panic recovery, because the setup
  hooks ran. Sortie never checks out a branch itself; the agent checks out the PR head branch with
  its own git tooling, told the branch through the `label_fix` continuation key (§11F.7, the
  `branch` field). A stale `.sortie/status` from a prior session in the reused directory is cleared
  before the first turn, exactly as for a normal dispatch.
- **Fresh session.** The session starts with no resume identifier. It is a new session, not a
  continuation of a live one.
- **Single selecting flag.** The posture is selected by the same worker flag §11F.6 describes,
  derived from the dispatch reaction kind. A `label-fix` dispatch runs this read-write path; every
  other dispatch is unchanged.

The fix path suppresses every issue-work side effect a normal dispatch performs, because the linked
issue of a PR under review is typically not in an active work state and the fix session must not
re-drive it:

- the dispatch-time transition of the linked issue to the in-progress tracker state;
- the dispatch comment on the linked issue;
- the per-turn tracker-state refresh, and with it the issue-state termination gate that would
  otherwise end the turn loop as soon as the linked issue leaves an active state;
- the self-review loop;
- the worker-exit handoff transition and the active-issue continuation retry, so a clean fix exit
  neither hands off nor re-dispatches; repeatability after a completed fix comes instead from the
  reconcile re-enqueue (§11F.4).

Because the fix turn loop is not gated on issue state, its turn budget rests on the existing
`agent.max_turns` ceiling plus the agent's own completion signal (the `.sortie/status` control-plane
file, §21). A fix is naturally multi-turn (fetch the comments, apply changes, push, post the
summary), so the completion signal matters more here than for review: without it a completed fix
session runs to `agent.max_turns` and wastes turns.

Credentials reach the fix agent the same way they reach a normal dispatch: through the
orchestrator's process environment (§10.7) and through any checkout-scoped credential the operator
setup hooks provision during `after_create`. Because the fix session runs those hooks, it has
strictly more credential paths available than the read-only review, not fewer.

The fix agent fetches the review comments, applies changes, pushes commits, and posts the summary
comment through its own SCM tooling and credentials, consistent with Sortie's scheduler-and-reader
boundary: the orchestrator adds no comment-fetch, push, or comment-post method to `SCMAdapter` and
performs none of that work itself. It injects only the PR coordinates and the head branch, through
the prompt continuation key (§11F.7).
