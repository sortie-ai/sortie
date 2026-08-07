---
status: accepted
date: 2026-08-07
decision-makers: Serghei Iakovlev
---

# Bound Workspace Retention by Age Independently of Tracker State

## Context and Problem Statement

Every workspace removal in the system is gated on the issue reaching a terminal tracker state.
Three paths perform that removal and all three apply the same gate. Startup cleanup lists the
workspace keys on disk, asks the tracker for their states, and removes those reported terminal.
Active-run reconciliation refreshes the tracker state of each running issue and marks the entry for
deferred cleanup only when the refreshed state is a member of the configured terminal states. The
periodic sweep, which runs once every sixty poll ticks, lists the keys on disk, drops the ones
belonging to in-flight work, asks the tracker for the state of the rest, and removes only those
reported terminal.

No fourth path existed. The workspace configuration carried exactly one field, the root directory,
so there was no age bound and no capacity bound to fall back on. A workspace whose issue never
reached a terminal state was retained for the life of the disk.

That is not an edge case. It is the ordinary outcome in a deployment that configures a handoff
state and has no external automation advancing issues, because the handoff state is deliberately
not terminal: one directory per processed issue, retained forever. Each directory holds a source
checkout, so the growth is measured in hundreds of megabytes per issue rather than kilobytes. The
growth was also silent. A sweep that found nothing terminal returned without logging, so the
operator's only signal was the disk filling.

Several populations could never be reached by the terminal gate at all, not merely late. An issue
deleted from the tracker is omitted from the state response, so the sweep skips its key on every
pass forever. An issue a human moved to a state that is neither active nor terminal is skipped the
same way. An issue left in an active state after a permanently failed run keeps its workspace with
no scheduled event that would ever remove it.

Closing the tracker loop on a merged pull request narrows this problem for one population and was
decided separately. It does not close it. That decision recorded an age or capacity bound as worth
having as a backstop for the cases it leaves open, declined to settle it as a side effect, and
required that retention policy be argued on its own terms. This is that argument.

## Decision Drivers

1. **A bound that depends on the tracker cannot cover the cases that need it most.** The workspaces
   with no path to removal are exactly the ones whose issues the tracker will never report as
   terminal: deleted issues, issues parked in an unconfigured state, issues abandoned in an active
   state. Any remedy expressed in tracker vocabulary reproduces the gap it is meant to close.
2. **Deleting a workspace is destructive and irreversible.** A workspace holds a source checkout, a
   possibly dirty working tree, and the `.sortie/scm.json` metadata that is the only durable record
   of a pull request's coordinates. Nothing restores it. A bound that can fire on work that is
   still live is worse than no bound.
3. **The bound must not silently disable a working feature.** Pending reaction entries are runtime
   only. After a restart they are rebuilt by reading `.sortie/scm.json` out of the workspace
   directory, subject to a thirty-day activity lookback. Removing a directory inside that window
   destroys the input that recovery needs and stops the reactions for that issue without reporting
   anything.
4. **A retention window is a policy horizon, not an operational timing.** Poll intervals, timeouts,
   and backoffs are configured in milliseconds because they run on the order of seconds and
   millisecond precision is meaningful. A retention window runs on the order of weeks, where
   millisecond precision is meaningless and a millisecond value is unreadable.
5. **The operator must be able to tell why the bound did not fire.** A bound that removes nothing is
   indistinguishable from a bound that is switched off unless the reason each workspace survived is
   visible. This is the same defect as the silent sweep, reappearing one level up.
6. **Exclusion from cleanup must be justified by a real dependency.** A workspace is excluded
   because removing it would break something. Where nothing depends on the directory, the exclusion
   is incidental, and an incidental exclusion that outlives every other bound in the system defeats
   the bound entirely.

## Considered Options

- Bound retention with an opt-in maximum age, applied to every workspace the sweep already treats as
  a candidate
- Bound retention with a capacity limit on the number of retained workspaces or their total size
- Bound retention with an opt-in maximum age gated on the configured handoff state alone
- Add no bound and rely on terminal-state coverage improving over time

## Decision Outcome

Chosen option: **an opt-in maximum age, applied to every workspace the sweep already treats as a
candidate**, because it is the only option whose reach does not depend on the tracker answering,
and because anchoring the window on the orchestrator's own record of work makes the bound provably
consistent with the recovery window it must not violate.

The bound is configured by a single field, `workspace.retention_days`. It defaults to zero, which
disables it, so a deployment that does not set it behaves exactly as it did before in every
observable respect.

### The unit and the floor

The window is expressed in days. Every other duration in the configuration is expressed in
milliseconds, and this field deliberately departs from that convention. The millisecond fields are
poll intervals, timeouts, debounces, and backoff caps, all sub-hour timings where the unit is
proportionate to the value. A retention window of thirty days is `2592000000` milliseconds, a
figure no operator can read back or check, and an operator who intends thirty days and drops three
digits gets forty-three minutes and loses every workspace on the next sweep. The unit is chosen so
that a misconfiguration is visible on the line where it is written.

A configured value below thirty days is rejected. The floor is not a matter of taste. Pending
reaction recovery reconstructs runtime reaction entries after a restart by reading
`.sortie/scm.json` from the workspace directory, and it considers a candidate only when its latest
activity falls inside a thirty-day lookback. Setting the retention window equal to that lookback
makes a single invariant true by construction: any workspace the bound is permitted to remove is
one that recovery would already have skipped as stale. A shorter window would silently break
reaction recovery for issues that recovery still regards as live.

The two windows are therefore coupled. Changing either without the other reintroduces the defect,
and that coupling is a constraint on both, not a note about one.

Validation is a configuration-shape check requiring no network access, so it is reported before a
run begins. A negative value is an error. A positive value below the floor is an error. Zero is
valid and means the bound is off.

### The population the bound applies to

The bound applies to every workspace key the sweep already treats as a candidate, which is every
key on disk not excluded as in-flight. It is not gated on the handoff state.

Gating on the handoff state would have been convenient, since that field already exists and names
the state where a completed issue parks. It fails on evidence. A parked workspace reaches four
distinct resting conditions, and the handoff state is only the first. An issue parks in an active
state when no handoff state is configured, and again when a run fails permanently and releases the
claim. An issue parks in a state that is neither active nor terminal when a human moves it
somewhere the configuration does not name, at which point reconciliation stops the worker without
cleanup. An issue parks in no state at all when it is deleted from the tracker, because the state
response omits the key and every terminal-gated path skips it.

The last of those is the decisive one. A deleted issue's workspace has no path to removal under any
tracker-driven rule, present or future, because there is no issue left to report a state. A
handoff-only gate would leave the single population that can never be reached any other way exactly
as unbounded as before.

Because the age decision needs no answer from the tracker, the age pass is evaluated from the
workspace listing and the persistence layer alone. It runs whether or not the tracker state read
succeeds. A bound that stops working during a tracker outage would be absent in precisely the
conditions where retention grows fastest, since no issue is being observed into a terminal state
either. The terminal check remains the fast path and runs first; the age pass is the fallback for
everything the terminal check does not remove.

### What the age is measured from

Age is measured from the later of two timestamps: the most recent `completed_at` recorded in
`run_history` for the workspace's identifier, and the `pushed_at` value recorded in the workspace's
`.sortie/scm.json`. A workspace is removable when that anchor is older than the configured window.

Both values already exist. The `run_history` table carries `identifier` and `completed_at` on every
row, and the sweep already treats a workspace key as an identifier when it asks the tracker for that
key's state, so the same equivalence carries the run-history read. The pair is not chosen for
convenience: it is exactly the pair that pending reaction recovery uses to decide whether a
candidate is fresh. Anchoring on the same pair is what makes the floor argument hold. Anchoring on
run completion alone would remove a workspace whose recorded push is recent, which recovery would
still have accepted.

An identifier with no completion record and no recorded push is retained, never removed. The
absence of a record is not evidence of age; it is absence of evidence. It describes a workspace
created by a run that never completed, a workspace produced by an operator or a hook, or a
directory the system did not create at all. None of those may be deleted on an assumption.

Directory modification time is explicitly rejected as the anchor. It is mutated by lifecycle hooks,
by agent processes, and by background tooling inside the checkout, so it reports filesystem
activity rather than work. It would also read a recently touched abandoned workspace as live and an
untouched live one as stale, which inverts the property the bound is trying to measure.

### Safety invariants

The bound removes directories and does nothing else.

1. A workspace holding an entry in the running map or the retry map is never removed, whatever its
   age. These are absolute exclusions and the bound does not weaken them.
2. The bound performs no tracker write, no source-control write, no `reaction_fingerprints` write,
   and no creation or deletion of pending reaction entries. Reaction state is read-only to this
   pass. A workspace removed by age leaves every reaction latch and high-water mark exactly as it
   found them.
3. Removal goes through the existing workspace removal path, so key sanitization, containment of
   the resolved path under the workspace root, and the `before_remove` hook apply unchanged. The
   bound introduces no new way to reach the filesystem.
4. A missing activity anchor retains the workspace, as stated above.
5. The window may not be configured below the pending-reaction recovery lookback, as stated above.

### The pinned population, and why in-flight is narrowed

An age bound placed in front of the existing exclusion rule would have removed nothing at all in an
entire class of deployment, and stating the bound without confronting that would misrepresent what
it buys.

On a normal worker exit, when a source-control adapter is configured, a label command is enabled,
and the workspace metadata names a pull request with an owner and a repository, the orchestrator
seeds a pending reaction entry for that label command. Two reaction kinds are seeded this way,
`label-review` and `label-fix`. Neither carries an expiry, deliberately: a human label gesture stays
actionable regardless of age, so unlike the five kinds that do expire at thirty minutes, `ci`,
`review`, `bot-review`, `merge`, and `merge-conflict`, the label-command kinds have no drop-on-age
branch. Their reconcile passes re-enqueue the entry on every outcome, so once seeded the entry
persists for the life of the process, and after a restart recovery rebuilds it for any issue still
parked in the handoff state inside the lookback.

The sweep excluded every key holding an entry in the running map, the retry map, or the pending
reaction map. Composing those facts: in a deployment that configures label commands, every completed
issue with a managed pull request held a permanent pending entry, so its workspace was permanently
excluded from candidacy, and an age bound would have removed nothing whatsoever.

The exclusion is therefore narrowed. A pending reaction entry pins its workspace only when the
entry's kind carries an expiry. An entry of a kind with no expiry does not pin.

The narrowing is justified by what the label-command reconcile pass actually reads. It polls the
forge's label-event journal using the pull request number, owner, and repository it holds in the
entry, and it compares the result against a high-water mark it holds in SQLite. It opens nothing in
the workspace directory. The detection loop is already decoupled from the directory by design,
because the session a command dispatches runs in a scratch checkout rather than in the directory
that seeded the entry. The workspace is not an input to detection, so pinning it was incidental
rather than required, and an incidental pin that outlives every bound in the system is the one thing
the bound cannot tolerate.

One genuine dependency remains, and the floor is what disarms it. Across a restart the runtime
entry is gone and recovery rebuilds it from `.sortie/scm.json` in the workspace, so a removed
directory ends label-command detection for that pull request. Because the window may not be set
below the recovery lookback, a workspace the bound may remove is one whose latest activity already
falls outside the window recovery honors, and recovery would have skipped it regardless. Nothing
reachable is lost.

The alternative, redefining the pin as a property of the reaction so that the entry outlives the
directory it was seeded from, was considered and rejected. It requires promoting pending reaction
entries from runtime state to persisted state so that the pull request coordinates survive a restart
without the directory. That is a persistence change with its own migration, its own restart
semantics, and its own duplicate-dispatch questions, and it is a larger decision than the one being
made here. Nothing in this decision forecloses it, and if it is taken later the retention floor and
the recovery lookback can be decoupled at that point.

### What the bound protects, and what it does not

It protects every deployment that configures it, for any workspace whose latest recorded activity is
older than the window: an issue parked in the handoff state, an issue left in an active state after a
permanent failure, an issue moved to a state the configuration does not name, and an issue deleted
from the tracker. It reaches those regardless of whether a pull request was produced, whether a
label command is enabled, and whether the tracker is reachable on the pass.

It does not protect a deployment that leaves the field at its default, which is every deployment
until an operator sets it. It does not remove a workspace whose issue is running or retry-scheduled,
however large the directory, because those exclusions are absolute. It does not remove a workspace
whose latest activity is inside the window, so a deployment that processes many issues quickly still
holds every workspace produced in the last window and must size its disk for that, not for the
steady state. It does not remove a workspace pinned by an unexpired reaction, though that pin
resolves itself within thirty minutes. And it removes nothing at all on any pass where the run
history offers no activity anchor for the candidates on disk.

### Observability

The distinction between the reasons a workspace survived is required, not optional.

The bound is opt-in and its most likely first experience is an operator setting it and observing
that nothing is removed. A single fused in-flight count over the running map, the retry map, and the
pending reaction map cannot tell that operator whether the workspaces survived because work is in
progress, because a reaction that will expire is still polling, or because their last activity is
inside the window. Those three have different remedies and one of them is not a fault at all. After
the narrowing above there is a fourth reason, a candidate retained for want of any activity record,
which is the one an operator is least likely to guess and most likely to need.

The sweep therefore emits one summary per pass, and it emits it whether or not anything was removed.
The summary reports separately: the candidates listed on disk, those excluded as running, those
excluded as retry-scheduled, those excluded by an unexpired pending reaction, those removed as
terminal, those removed by age, those retained as inside the window, and those retained for want of
an activity record. Emitting the summary on an empty pass is the specific correction to the silent
growth described above, where a pass that removed nothing said nothing.

### The cleanup model is now two mechanisms

The decision to close the tracker issue when a managed pull request merges remains in force in full.
Its reaction is unchanged in trigger, in target, in idempotency, and in failure posture. Its
scope boundary is unchanged: a pull request closed without merging and an issue with no managed pull
request still leave the issue in the handoff state. Nothing about that reaction is revised here.

What that decision left open, and what this one settles, is the cleanup model. It recorded the
workspace consequence as only partly bounded, listed a retention bound as a backstop worth having,
and directed that the workspace specification state that terminal-gated cleanup remains the only
mechanism. That direction is superseded. Terminal-gated cleanup is now one of two mechanisms, and
the specification must describe both rather than assert a single one.

The two are ordered and do not overlap in intent. The terminal gate is the primary mechanism and
expresses the normal case: work finished, the tracker says so, the workspace goes. The age bound is
a backstop and expresses only the abnormal case: no terminal state arrived and none is coming. The
age bound never fires on a workspace the terminal gate would have removed, because the terminal
check runs first on the same pass. The terminal gate remains unconditional and always on; the age
bound is opt-in and off by default.

The exclusion rule that both mechanisms share is also amended. Where a pending reaction entry of any
kind previously excluded a workspace from sweep candidacy, only an entry of a kind carrying an
expiry does so now. That amendment applies to the terminal path as well, since both read the same
exclusion set. The effect there is small and benign: a terminal issue holding a label-command entry
is now cleaned up on the next sweep rather than never.

### Considered Options in Detail

**A capacity bound on count or total size.** Capping the number of retained workspaces, or their
aggregate size, targets the resource the operator actually cares about and gives a hard ceiling the
age bound cannot promise. It was rejected on its eviction rule. A capacity bound must choose a
victim, and the only defensible ordering is by age, so it is an age bound with an extra parameter
and a worse failure mode. Under an age bound the question asked of each workspace is whether that
workspace is stale, and the answer does not depend on any other workspace. Under a capacity bound a
burst of new issues evicts the oldest survivor even when it is days old and live, so an unrelated
change in arrival rate silently changes what is deleted. Size-based capacity is worse still: a
workspace's size reflects the repository it checked out, so the bound would preferentially delete
work on the largest repositories, which is unrelated to whether that work is finished. A hard
ceiling is a real need and remains available as a later addition on top of the age anchor
established here.

**An age bound gated on the configured handoff state alone.** This is the narrowest possible version
and the safest to reason about: the handoff state is where completed work parks, so a workspace
found there is one whose agent has finished. It is rejected on coverage, for reasons the code makes
concrete. It reaches nothing when no handoff state is configured, since then completed issues park
in an active state instead. It reaches nothing when a human moves an issue to a state the
configuration does not name. And it cannot in principle reach a workspace whose issue was deleted
from the tracker, because there is no state to compare against, which is the one population no
tracker-driven rule will ever remove. Gating on a single existing field would have made the bound
easy to specify and left the hardest case exactly where it was.

**No bound, relying on terminal-state coverage to improve.** Every workspace removed by a terminal
transition is removed for a better reason than age, since the tracker has confirmed the work is
finished, and each addition to terminal coverage shrinks the residue. This is a real argument and it
is why the terminal gate stays primary. It fails as a complete answer because part of the residue is
not shrinking. A deleted issue reports no state at any future time. An issue parked in a state the
configuration does not name will not move on its own. An issue whose terminal transition fails
permanently, for a revoked credential or a workflow that forbids the transition, stays where it is.
For those, waiting is not a strategy, and the disk is consumed by work that finished weeks ago.

## Consequences

### Positive

- Retention has an upper bound that does not depend on the tracker answering, on the issue still
  existing, or on any reaction being configured.
- The populations that no terminal-gated path could ever reach, deleted issues and issues parked in
  unnamed states, are covered for the first time.
- The bound cannot break pending reaction recovery, because its floor is the recovery lookback and
  its anchor is the same pair of timestamps recovery uses.
- The sweep reports on every pass instead of only when it removes something, so an operator can see
  what the sweep considered and why each candidate survived.
- A pending reaction of a kind with no expiry no longer pins a workspace indefinitely, which also
  fixes the smaller pre-existing case where such an entry blocked terminal cleanup outright.
- A deployment that leaves the field unset is unaffected: no new read, no new removal, and the same
  behavior it had before.

### Negative

- **The bound is off by default, so the growth continues until an operator acts.** Making it opt-in
  is deliberate, because a default that deletes checkouts on upgrade is not acceptable, but it means
  the deployments most exposed are the ones that have not read the configuration reference.
- **A live workspace can still be removed.** An operator inspecting a checkout, or holding one aside
  for a post-mortem, leaves no trace in the run history and no push timestamp. If its last recorded
  activity is outside the window, it is removed. The floor makes this unlikely rather than
  impossible, and no signal short of an explicit marker file would make it impossible.
- **Label-command detection ends at a restart for any workspace the bound removed.** Detection
  continues in the running process, since it needs nothing from disk, but recovery cannot rebuild
  the entry without `.sortie/scm.json`. The floor guarantees recovery would have skipped that
  candidate anyway, so nothing reachable is lost, but the two windows are now coupled and a future
  change to either silently changes the other.
- **A label-fix session dispatched after removal pays a fresh checkout.** The workspace is recreated
  and the `after_create` hook runs again, which is where the clone happens. This applies only to
  issues idle beyond the window, and it costs time and bandwidth rather than correctness.
- **The sweep reads the persistence layer where it previously read only the filesystem and the
  tracker.** The `run_history` table is indexed on `issue_id` and not on `identifier`, so the
  activity lookup scans until an index is added. The sweep runs once per sixty poll ticks, which
  makes the cost tolerable, but it is a new dependency in a pass that previously had none.
- **Two mechanisms are harder to reason about than one.** An operator diagnosing a missing workspace
  must now consider that either mechanism could have removed it, and the summary must be read to
  tell which. The alternative was a mechanism that did not cover the cases that needed covering.
- **The narrowed exclusion is a behavior change for deployments that never configure the bound.** A
  label-command entry no longer protects a workspace from the terminal sweep. This only matters for
  an issue that is both terminal and holding such an entry, where the previous behavior was to
  retain the workspace forever, so the change is a correction. It is still a change that ships
  without an opt-in.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The workspace management and safety material: the two-mechanism cleanup model, replacing the
   pending direction to assert that terminal-gated cleanup is the only mechanism; the age anchor and
   the retain-on-missing-record rule; and the safety invariants restated alongside the existing
   containment and sanitization invariants.
2. The configuration specification: the `workspace.retention_days` field, its unit, its default, its
   floor, and its validation, together with the reason the field departs from the millisecond
   convention used by every other duration.
3. The polling, scheduling, and reconciliation material: the order of the terminal check and the age
   pass within a single sweep, the fact that the age pass is evaluated without a tracker read and
   runs when that read fails, and the corrected statement of which paths perform terminal cleanup,
   which currently omits the periodic sweep.
4. The label command and read-only review material: that a label-command pending entry no longer
   excludes a workspace from sweep candidacy, and that detection is unaffected because it reads
   nothing from the workspace directory.
5. The failure model and recovery material: the coupling between the retention floor and the pending
   reaction recovery lookback, stated as a constraint on changing either.
6. The logging, status, and observability material: the per-pass sweep summary, its categories, and
   the requirement that it be emitted when nothing was removed.
7. The workflow file syntax reference: the new field in the workspace block.

## Confirmation

The decision is validated when all of the following hold:

1. A deployment that leaves `workspace.retention_days` unset removes exactly the workspaces it
   removed before, performing no run-history read and no age comparison.
2. A configured window below the pending-reaction recovery lookback, and a negative window, are both
   rejected offline before a run begins.
3. A workspace whose latest run completion and recorded push are both older than the window is
   removed, whether its issue is in the handoff state, in an active state, in a state the
   configuration does not name, or absent from the tracker entirely.
4. A workspace with no run-history completion and no recorded push is retained regardless of how
   long it has been on disk.
5. A workspace whose issue holds an entry in the running map or the retry map is retained regardless
   of its age.
6. In a deployment configuring label commands, a workspace older than the window is removed even
   though its issue holds a pending label-command entry, and the label-command detection loop
   continues to observe and dispatch commands for that pull request in the running process
   afterwards.
7. A workspace pinned by an unexpired reaction of a kind that carries an expiry is retained, and
   becomes removable once that entry expires.
8. The age pass removes eligible workspaces on a pass where the tracker state read fails.
9. The sweep emits its summary on a pass that removes nothing, and the summary distinguishes
   candidates excluded as running, excluded as retry-scheduled, excluded by an unexpired reaction,
   retained as inside the window, and retained for want of an activity record.
10. No sweep pass writes a tracker state, a source-control resource, a reaction fingerprint, or a
    pending reaction entry.
