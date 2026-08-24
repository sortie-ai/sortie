---
status: accepted
date: 2026-08-24
decision-makers: Serghei Iakovlev
---

# Re-Read the Issue State Before Recording an Absence Failure

## Context and Problem Statement

When a worker exits, the orchestrator computes a disposition for the run. If the issue is observed
to be in a terminal state, the run takes the terminal disposition: the handoff write is suppressed,
the claim is released, and nothing is scheduled. If instead the handoff-evidence policy withholds
the handoff because no work was observed in the workspace, the run takes the absence-failure
disposition: a failed run record, a session-failure comment written back to the issue, and a
scheduled retry.

Both branches are chosen from an observation of the issue's tracker state, and that observation was
the freshest one captured before the worker exited, which could be a full turn old. Only one branch
guarded against that. The path that writes the handoff state re-read the tracker immediately before
the write, because overwriting a terminal state with a handoff state would undo a person's own
action. The path that withheld the handoff had no such guard, because withholding writes nothing and
so looked harmless.

It was not harmless. An issue that reached a terminal state late in the run, written by the agent's
own tracker tool, by an `after_run` hook, or by a person, still took the absence-failure path. The
system reported a failure and promised a retry on an issue that was already finished, and the retry
was later discarded unread when reconciliation observed the terminal state and released the claim.
Nothing was corrupted. What was wrong is that a disposition decided on a stale observation
contradicted a decision the tracker already held, and the orchestrator had the means to know it.

## Decision Drivers

1. **A guard present on one branch and absent on its sibling is an accident of construction.** Both
   branches answer the same question from the same observation. The re-read was added to the write
   path because writing over a terminal state destroys information. That reasoning explains why the
   guard appeared there first; it does not explain why the other branch may act on a stale reading.
2. **A report is a claim about the world, and a wrong one is not free.** A session-failure comment
   on a finished issue is read by whoever is watching that issue. An operator who learns that these
   reports are sometimes about issues that are done stops reading them, which costs the reports that
   were right.
3. **A retry that cannot change anything still consumes.** It holds a retry slot, keeps the issue
   claimed until reconciliation catches up, and is then thrown away. The work it displaces is real
   even though the work it does is not.
4. **A finished issue must not accumulate toward being parked.** Consecutive observed absences on
   one issue reach a ceiling, at which point the issue is parked and an escalation is applied. An
   issue that finished its work by a route other than the evidence policy's own signal should not
   move toward that ceiling at all.
5. **The status column of a run record has more than one consumer.** The restart recovery pass
   filters on it, the operator's aggregate surfaces group by it, and the absence-parking count
   filters on it together with the text of the error. Changing what that column says for one
   population of runs is a change that has to be checked against all three.
6. **Freshness at exit is cheap.** Exit is not a hot path. One tracker read per withheld run, on a
   run that has already spent an entire agent turn, is affordable in a way that a read per dispatch
   candidate would not be.

## Considered Options

- Re-read the issue state before recording an absence failure, route a terminal state into the
  ordinary terminal disposition, and let that disposition record `succeeded` as it already does
- Re-read and route as above, but record `failed` with an error that omits the absence marker
- Re-read, record `succeeded`, and exclude suppressed records from the restart recovery query

## Decision Outcome

Chosen option: **re-read and record `succeeded`**, because the property the change rests on is that
a late-observed terminal state produces the same exit as an early-observed one, and the
early-observed terminal exit already records `succeeded`, including on a run that produced nothing.

### The re-read

Before recording an absence failure, the orchestrator reads the issue's state from the tracker. A
terminal state routes the run into the ordinary terminal disposition: the handoff write is
suppressed, the claim is released, no retry is scheduled, and no failure is reported. Any other
state, and a read that fails, leaves the absence-failure disposition exactly as it was.

This is the guard the handoff write path already applies, placed on the arm that had none, and it
inherits that guard's conditions. It is attempted only where terminal states are configured, since
with none configured no value can classify as terminal and the read would spend a tracker call and
up to one request timeout on the event loop for an outcome it cannot reach. A read that errors is
not evidence of anything and does not redirect the run.

The status the terminal disposition writes is unchanged and is not conditional on how that
disposition was reached. On the handoff path `succeeded` has always meant that the run exited
normally and the policy did not withhold the transition, not that work was positively observed; a
run suppressed on an early-observed terminal state has always recorded it. A run suppressed on a
late-observed one records the same thing for the same reason.

### What of the earlier decision remains in force

The decision to withhold the handoff transition only when absence of work is observed remains in
force. What counts as evidence is unchanged. Where the baseline is taken is unchanged. The three
policy values and their meanings are unchanged. The failure and backoff lane an absence is scheduled
on is unchanged, as is the ceiling on consecutive absences, the escalation applied at it, and the
reset a producing run performs. No stored run record gains a field for the verdict. Nothing about
the evidence rule itself is revised here; this decision changes only which runs reach it.

One of that decision's acceptance criteria is superseded, and only for the population this decision
creates. It asserted that after a restart the pass rebuilding pending reaction entries selects the
most recent run that produced something in preference to a later run observed to have produced
nothing. That preference was never a rule the recovery pass expressed. It fell out of the status
column: the query selects the most recent `succeeded` record per issue, and a run observed to have
produced nothing recorded `failed`, so it was passed over. A suppressed run is a run observed to
have produced nothing, and it now records `succeeded`, so the query can select it. For that
population the criterion no longer holds, and it is not reinstated by other means.

The new model is that the preference was incidental rather than load-bearing, and four facts bound
what its loss can change.

A suppressed run's workspace held no source-control metadata naming a pushed commit or a pull
request. Such metadata is itself positive evidence and never withholds, so it cannot be present on a
suppressed record.

The recovery pass reads workspace metadata live, from a path it computes from the candidate's
identifier, rather than from the stored run record, and every reaction kind it can rebuild requires
a pull request number from that metadata. Which record wins therefore cannot change which pull
request is recovered, nor whether one is recovered at all. It can change only the attempt number,
agent, rule and template attributed to the rebuilt entry.

A candidate is used only where the tracker reports its issue in the configured handoff state. A
terminal issue is skipped, and terminal is the state a suppressed run's issue is in at the moment of
suppression.

A producing record and a suppressed record can therefore appear in that order on one issue only
where the workspace carried no such metadata at the later run's exit.

### What this decision does not settle

Whether an agent may declare that the requested result was already true and that nothing needed
changing, so that a run producing nothing can be a positive outcome rather than an absence, is a
separate question and is not settled here. This decision adds no agent signal, no run outcome, no
status-file value and no run-history status value. It routes a run into one of the two dispositions
the orchestrator already had.

### Considered Options in Detail

**Record `failed` with an error that omits the absence marker.** This would preserve the superseded
criterion, and the parking count would still not advance, since that count requires both a failed
status and the marker. Rejected on three grounds. The operator's aggregate surfaces group run
history by that column and would report a failure for a run that ended correctly. The persisted
status would become conditional on how the terminal disposition was reached, so two runs with
identical endings would record different statuses for a reason no consumer can see. And the failed
status would gain a third sense, neither a run that errored nor a run withheld for absence, that
nothing reading the column can distinguish.

**Keep `succeeded` and exclude suppressed records from the recovery query.** Rejected because the
record carries no discriminator to exclude on, and no schema change is wanted for a case this
narrow. Steering the query by writing recognizable text into the error column of a succeeded record
would be a covert schema in a column reserved for naming the cause of a failure.

## Consequences

### Positive

- A finished issue is no longer reported as a failure and no longer draws a retry, so the
  operator-facing claim and the scheduling both follow what the tracker holds.
- The consecutive-absence count that parks an issue no longer advances for a finished issue. That
  count requires both a failed status and the absence marker in the error, and a suppressed run
  carries neither, so an issue finished by an `after_run` hook or by a person cannot be parked for
  producing nothing.
- The terminal guard is now the same on both branches, so a reader does not have to know which arm a
  run is on to know whether the state was verified.

### Negative

- **One tracker read per withheld run.** It runs on the event loop and is bounded by the adapter's
  request timeout. It is spent only where the policy already withheld, which is the rarer path.
- **A read that fails leaves the old behaviour intact.** The fallback is conservative, but a tracker
  unreachable at exit still yields a failure report and a retry on a finished issue. This decision
  does not close that case.
- **An attribution substitution is possible.** Where an issue leaves its terminal state for the
  handoff state before the next restart, and its suppressed record is more recent than a producing
  one, the rebuilt reaction entry carries the suppressed run's attempt number, agent, rule and
  template rather than the producing run's. The pull request recovered is the same either way.
- **A suppressed record can occupy a slot at the recovery pass's candidate bound.** That bound is
  applied before the tracker read, so a record the read would have skipped can still displace
  another issue's candidate on a restart carrying more candidates than the bound allows.

## Confirmation

The decision is validated when all of the following hold:

1. A run whose issue reached a terminal state after the last observation, and whose workspace shows
   no work, suppresses the handoff, releases the claim, schedules no retry, and writes no
   session-failure comment.
2. That run records `succeeded`, and its run record is indistinguishable from that of a run
   suppressed on an early-observed terminal state.
3. A run whose issue is not terminal at the re-read, and whose workspace shows no work, takes the
   absence-failure disposition unchanged: a failed record carrying the absence marker, a
   session-failure comment, and a retry on the backoff lane.
4. A re-read that fails leaves the absence-failure disposition in place.
5. No re-read is attempted where no terminal states are configured.
6. Consecutive absences on an issue that reaches a terminal state stop accumulating, and the issue
   is not parked by that mechanism.
7. The restart recovery pass rebuilds the same pull request from a suppressed record as it would
   from a producing record for the same issue.
