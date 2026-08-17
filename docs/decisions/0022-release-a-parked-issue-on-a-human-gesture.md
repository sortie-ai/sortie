---
status: accepted
date: 2026-08-17
decision-makers: Serghei Iakovlev
---

# Release a Parked Issue on a Human Gesture in the Tracker

## Context and Problem Statement

The orchestrator selects issues for dispatch from the tracker states the operator declared active.
Two paths declare that the selection should stop because a person is needed. On the first, the agent
itself reports that it cannot proceed, through the status file whose vocabulary Sortie appends to
every run's first prompt. On the second, the orchestrator observes that consecutive runs produced no
work; [ADR-0020](0020-withhold-handoff-on-observed-absence-of-work.md) bounded that sequence with a
ceiling and, on reaching it, withheld further dispatch and applied the operator's escalation label
to the issue. Both paths mean the same thing to the person who reads the tracker: this issue is not
moving and a human must look at it.

Nothing released such an issue. The eligibility gate that held it was derived from durable state
that only a successful run could clear, and a held issue cannot run, so it could not produce the
clearing evidence. The circularity was complete rather than approximate: the count was re-derived on
every poll tick from run history, run history had no delete, prune, or retention path, and the one
routine that ended the sequence ran on the exit of a run that produced work. An issue that reached
the ceiling stayed there for the lifetime of the deployment's database.

The escalation label was write-only. The orchestrator applied it on the recovery paths and on the
absence ceiling, and read it nowhere that bore on whether an issue was dispatched. Dispatch
eligibility was decided by required fields, active-state membership, non-terminal state, whether a
run was already in flight, whether the issue was claimed, whether an effort budget was exhausted,
and whether a non-terminal blocker existed. Labels were not consulted on any of those. They were
consulted elsewhere in the same layer, to select which dispatch rule matched an issue, which is what
makes their absence from the eligibility decision a real absence rather than an oversight in the
reading. So the system announced that a human was needed and offered the human no gesture that had
any effect.

The label-shaped exclusion that did work belonged to the operator, not to the orchestrator.
`tracker.query_filter` is label-capable on every shipped adapter, but it is a query string the
adapter interpolates into the forge's own query language and sends to the forge. The orchestrator
receives only the issues that survived, and never learns that any were withheld or why. That
distinction is load-bearing, because it is why "labels already affect what is dispatched" was never
a counter-argument: the filter changes what the orchestrator is shown, not what the orchestrator
decides. It also sets the bar that had to be cleared. An operator who excluded labelled issues
through the filter could undo the exclusion by editing the filter. An operator whose issue was
parked by the orchestrator could not undo that by any means aimed at that issue.

The only releases were deployment-wide. Raising the per-issue effort ceiling above the recorded
count lifted the gate, because the absence ceiling derives from that same setting. Disabling the
evidence policy lifted it too, for every issue at once. Neither is a gesture an operator can make
about one issue, and both ask a deployment to change its global posture to unstick a single ticket.

That is the asymmetry worth recording: a state an operator can be placed into, can observe in the
tracker, and cannot leave. It was strictly worse than the operator-side workaround it stood beside,
and worse in the one respect that matters, which is reversibility.

The precedent cuts both ways and is stated honestly rather than hidden. The sibling gate that
suppresses dispatch on an exhausted effort budget is releasable only by a configuration change, for
exactly the same reason: its count comes from run history, and run history is monotone because
nothing removes rows from it. A design that gives one derived gate a per-issue release and leaves
the other without one is asserting that the two gates mean different things. That assertion has to
be made, not assumed.

## Decision Drivers

1. **A state the system can enter and the operator cannot leave is worse than a state the system
   never enters.** Every other durable disposition in the system is reversible by someone: a claim
   is released, a retry is cancelled, a workspace is reclaimed, an issue in a terminal state can be
   moved back. A park was the only disposition with no exit. Its cost is not that it fires wrongly;
   it is that firing correctly is also permanent, so the remedy for a correctly parked issue and the
   remedy for a wrongly parked one are the same remedy, and there was none.
2. **The announcement and the release must live on the same surface.** The park announces itself in
   the tracker, to a person who is reading the tracker. A release that required orchestrator access
   would summon a person to one system and answer them in another, and would exclude every operator
   who can triage a ticket but cannot reach the host. The gesture must be available to whoever the
   label was addressed to.
3. **Parking must mean one thing across every trigger.** Two triggers already park, a third class of
   trigger is conceivable, and each one arrives at the same tracker-visible outcome. If the release
   rule were attached to the trigger rather than to the park, an operator would have to know why an
   issue was parked before knowing how to release it, and would learn that from a log they may not
   have. One meaning system-wide is what makes the label legible without the record behind it.
4. **A release the system cannot distinguish from a write that never happened is not a release.**
   The label reaches the tracker through a network call that can fail, and one shipped adapter
   accepted the write while recording nothing. If absence of the label released the park, then a
   park whose label never landed would release itself on the next tick, silently, and the issue
   would resume dispatch having never been seen by anyone. The release rule and the write it depends
   on cannot be evaluated independently.
5. **A release must actually release.** The gate is not a stored flag; it is re-derived on each poll
   tick from durable history. Removing the runtime record without ending the sequence that produced
   it leaves the derivation intact, so the same tick that honors the operator's gesture re-computes
   the exhausted count and parks the issue again. The operator would observe their action being
   undone within one poll interval, which is worse than no release at all because it looks like the
   system rejecting them.
6. **Making labels an input to dispatch is a real cost, and it should be paid once.** Until this
   decision the dispatch path could be read without reference to labels at all. Afterwards it
   cannot. That is a permanent widening of what a reader of that path must hold in mind, and it
   argues for one label-reading rule with one meaning rather than a family of them.
7. **The effort budget and the park express different things, so their releases need not match.** An
   exhausted effort budget says the deployment has spent what it allotted; the allotment is a
   configuration value, so changing the configuration is the coherent release and the gate is
   correctly monotone. A park says a person is needed; the coherent release is that person acting.
   Deriving both from run history is an implementation resemblance, not a shared meaning.

## Considered Options

- Release the park only when the parking label is removed from the issue
- Release the park only when the issue is moved to a different tracker state
- Release on either gesture, guarded by confirmation that the parking label reached the tracker, and
  resetting the counter that produced the park
- Release through an explicit operator command issued to the orchestrator
- Provide no per-issue release, leaving only the deployment-wide levers

## Decision Outcome

Chosen option: **release on either gesture, guarded by confirmation that the parking label reached
the tracker, and resetting the counter that produced the park**, because it puts the release on the
surface the park addressed, accepts both of the natural things an operator does to a ticket they
have just triaged, and closes the two ways a release can be hollow.

### Two gestures, one meaning

A parked issue is released when the orchestrator observes, on a poll tick, either that the parking
label is no longer on the issue or that the issue's tracker state differs from the state it held
when it was parked. Both are gestures a person makes in the tracker with no knowledge of the
orchestrator, and both are things an operator does anyway on finishing a triage: they clear the
marker, or they move the ticket.

Accepting both is not indecision. They are the two ends of one triage and an operator may perform
either, in either order, and frequently performs only one. An operator who decides the issue is not
ready moves it out of the active queue; an operator who fixes whatever blocked the agent clears the
label and leaves the state alone. A rule that recognized one and not the other would leave half of
all correct operator behavior with no effect, which is the fault this decision exists to remove,
reproduced in a smaller form.

Where the gesture is a state change, the effect on dispatch still runs through the pre-existing
active-state gate. Moving a parked issue to a state the deployment does not call active releases the
park and leaves the issue undispatchable for the ordinary reason. That is the correct outcome and it
is worth stating: the release lifts the park, it does not assert that the issue should now run.

Both releases apply to every trigger that parks an issue. The rule is a property of the park, not of
the reason recorded on it. A park taken because the agent reported itself unable to proceed and a
park taken because the absence ceiling was reached are released by the same two gestures, on the
same terms. This is the substance of the decision rather than a tidiness preference: the label on
the issue is the whole of what most operators will ever see, and it has to carry one instruction.

### The release is guarded by confirmation that the label reached the tracker

The orchestrator records, alongside the park, whether it has confirmed that the parking label is on
the issue. Confirmation comes from observing the label on a later fetch of that issue, not from the
write returning without error. Until the label has been confirmed, the absence of the label does not
release the park.

Without the guard the release rule would be evaluated against a fact the orchestrator does not have.
A label write that failed, or that was accepted by an adapter which stores nothing, leaves the issue
looking exactly like an issue whose label a human just removed. Release-on-absent-label would then
un-park the issue on the very next tick, silently, and the loop the park existed to stop would
resume with no human having seen anything. The guard is what makes the two cases distinguishable: an
issue with no confirmed label stays parked, because the system has no evidence its request for
attention was ever delivered.

The state-change gesture carries the same shape of problem and the same shape of answer. A park
recorded without an observed tracker state, which happens where the park is taken on a path that
deliberately runs before any tracker fetch, cannot be compared against a later observation. Such a
park is not released by the state gesture until an observation supplies the missing baseline, and
the first observation supplies it rather than releasing on it. The alternative, treating a missing
baseline as "release on any observation", would release every such park on the next tick and defeat
the gate entirely.

### A release resets the counter that produced the park

Releasing a park whose trigger maintains a counter also ends that counter's sequence, whatever the
release gesture was.

Without this the release is inert. The runtime record is removed, the same poll tick re-derives the
exhausted count from run history, finds it still at or above the ceiling, and parks the issue again
before the dispatch loop runs. The operator's gesture would be honored and reversed inside one tick,
and the log would show a release followed immediately by a park, which reads as the system arguing
with the person it summoned.

The reset is also the correct meaning independently of that mechanical necessity. The counter exists
to bound how many times the system will retry something that is not working before asking for help.
A person acting on the issue is precisely the event that licenses the bound to start over, on
exactly the terms a run that produced work already does. Treating the human gesture as weaker
evidence than a successful run would say that the system trusts its own observations more than the
judgement of the person it asked for.

Ordering follows from this. The release rule is evaluated before the trigger that would re-park, on
the same tick and over the same candidates, so an issue released this tick is not re-parked by a
derivation that has not yet been reset.

### Why the release is defined on the park and not on the trigger

The park is one record with one gate and one tracker-visible marker. Defining the release on the
park means a new parking trigger inherits a release rather than choosing one, and cannot ship a park
with no way out. That is the property this decision most wants to be durable: the fault being closed
here was not that one trigger forgot to define a release, it was that a park could exist without
anyone having had to answer the question.

### Considered Options in Detail

**Release only when the parking label is removed.** This is the most direct reading of the label:
the marker says a human is needed, so removing it says a human came. It needs no baseline, no
comparison against an earlier observation, and no per-trigger knowledge. It is rejected as the sole
rule because it makes the release conditional on a gesture many operators will not make. An operator
who triages the ticket and moves it, leaving the label, has acted decisively and would see no
effect; worse, the label they left behind would keep the issue parked in a state they consider
resolved. It would also make the release rule wholly dependent on a tracker write having previously
succeeded, with no second path when it did not.

**Release only when the issue is moved to a different tracker state.** A state change is the
strongest available evidence that a person made a decision, and it is the gesture the system already
treats as authoritative elsewhere: the handoff transition is a state change, and a terminal state
already suppresses dispatch. Basing the release on it alone would need no label read at all, which
would preserve the property that the orchestrator does not consult labels for eligibility. It is
rejected because it forces the operator to move a ticket in order to say something about a ticket.
An operator who has fixed the blocking condition and wants the agent to try again wants the issue to
stay exactly where it is; the only way to release it would be to move it out of the active state and
back, which is an awkward gesture that pollutes the issue's history and, in a tracker with a
constrained workflow, may not be permitted at all. It also cannot be recognized where no baseline
state was recorded.

**Release through an explicit operator command issued to the orchestrator.** A command is
unambiguous, needs no inference from tracker observations, requires no label read, and could report
its own outcome directly to the person who issued it. Nothing about the design would be guessed. It
is rejected because it answers the wrong audience. The park announces itself in the tracker, to
whoever triages tickets, and that person may have no access to the orchestrator's host, its
configuration, or any command surface it exposes. A release that only a deployment operator can
perform turns every parked issue into an escalation to a second person, and the number of people who
can unstick an issue becomes smaller than the number who can diagnose it. It also introduces a
control surface with its own authentication and audit questions, to duplicate a signal the tracker
already carries. The option is not foreclosed by this decision: a command could later be added as a
further release trigger without disturbing the two defined here, since the release is defined on the
park.

**Provide no per-issue release.** This is the status quo and it is the serious alternative. It
matches the sibling effort-budget gate exactly, which is monotone for the same reason and releasable
only by configuration; consistency between two gates of identical construction is a genuine
architectural good. It adds no capability, so it cannot add a defect. It keeps the orchestrator
ignorant of labels, preserving a property of the dispatch path that a reader can currently rely on
absolutely. And it has a coherent story for the operator: a park means the deployment's tolerance
for this issue is spent, and spending more means saying so in configuration.

It is rejected on the seventh driver. The consistency is a resemblance between implementations
rather than between meanings. An effort budget is a resource allotment, and the party who set the
allotment is the party who changes it, so a configuration release is the right shape. A park is a
request addressed to a person, and a request whose only answer is a global configuration change is
not a request. The asymmetry with the budget gate is also not symmetric in cost: an over-budget
issue has consumed something measurable and the operator can decide whether to spend more, whereas a
parked issue may have consumed almost nothing and be parked on a verdict about residue. Leaving it
unreleasable makes a bounded retry policy into a permanent verdict on a ticket. The option's best
argument, that it keeps labels out of the dispatch path, is real and is recorded below as the price
of the decision rather than being disputed.

## Consequences

### Positive

- A park becomes a reversible disposition, so the remedy for a correctly parked issue and for a
  wrongly parked one is the same gesture, performed by the same person, in the system where the park
  announced itself.
- The gesture requires no orchestrator access. Whoever the label was addressed to can answer it.
- Parking has one meaning system-wide. A new parking trigger inherits the release rule rather than
  choosing one, and cannot ship a hold with no exit.
- The escalation label stops being write-only on this path. What the system writes to the tracker to
  request attention is now also what it reads to learn that attention was given, so the marker is a
  channel rather than an annotation.
- The release cannot be hollow in either of the two ways available to it. It is not undone by a poll
  tick re-deriving the count, and it is not granted by a label that never arrived.
- The deployment-wide levers remain, and remain correct for what they express. Raising the effort
  ceiling or disabling the evidence policy still changes the deployment's posture; neither is now
  the only way to unstick one ticket.

### Negative

- **Issue labels become an input to dispatch eligibility for the first time.** Until this decision
  the orchestrator wrote labels and never read them to decide whether an issue was dispatched; the
  eligibility rules could be read and reasoned about without labels entering the account at all.
  Afterwards they cannot. Anyone reading the dispatch path must know that a label can change whether
  an issue is eligible, and that the label in question is not named in the dispatch rules but
  derived from a reaction's escalation setting. This is a permanent widening of that path's surface
  and it is the price of the decision, not an incidental detail of it.
- **A label a human removes for an unrelated reason releases a park.** The gesture is not
  distinguishable from a deliberate release, because nothing about a label removal records intent.
  An operator tidying labels, a bulk edit, an automation that normalizes label sets, or a person who
  simply disagrees with the label's wording all release every park they touch. The issue then
  resumes dispatch with its counter reset, and the only trace is the release record.
- **The system's willingness to be released depends on the tracker having been reachable earlier.**
  The confirmation guard is what makes the release safe, and it means a release now rests on a prior
  write having succeeded. Where the label write failed, the label-removal gesture has nothing to act
  on, and the issue stays parked until the operator makes the state-change gesture instead. The park
  is still escapable, but by only one of the two routes, and the operator has no indication from the
  tracker which route is available to them, because the missing label is exactly what they would
  have looked at.
- **A park recorded with no observed state is releasable by only one gesture until the next
  observation.** Where the park is taken on a path that runs before any tracker fetch, no baseline
  state exists to compare against, and the first observation records the baseline rather than
  releasing on it. An operator who moved the issue between the park and that first observation has
  their move recorded as the parked state instead of honoring it, and must act a second time.
- **The label the release rule compares against is fixed when the orchestrator starts.** It is taken
  from the review reaction's escalation setting, falling back to `needs-human`, and is captured once
  at construction rather than re-read when configuration reloads. An operator who changes that
  setting while the orchestrator is running finds that parks continue to be written and compared
  against the previous value until a restart, so the label on the issue and the label the rule
  watches can disagree.
- **The two derived gates now differ in kind, and the difference must be learned.** The effort
  budget remains monotone and releasable only by configuration; the park is releasable per issue.
  Both are computed from the same durable history and both suppress dispatch, so an operator seeing
  an issue that will not dispatch has to establish which of the two is holding it before knowing
  whether a tracker gesture will help.
- **A release resets a bound that was reached for a reason.** An operator who removes the label
  without addressing the underlying condition grants the system a full fresh sequence of attempts.
  The park will be re-reached, so the loop is still bounded, but each unconsidered release buys
  another full run of it, and the effort those runs consume is real.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The failure model and recovery strategy material: the park as one record with one gate shared by
   every trigger, the two release gestures, the confirmation guard on the label gesture and the
   baseline requirement on the state gesture, the reset of a trigger's counter on release, and the
   ordering of the release evaluation ahead of any trigger that would re-park on the same tick.
2. The orchestration state machine material: dispatch eligibility restated to include the park, and
   the statement that a label is now read on that path.
3. The issue tracker integration contract material: the parking label as a non-state write whose
   later presence or absence on the issue is read back, and the requirement that an adapter
   accepting a label write must make that label visible to subsequent reads of the issue.
4. The agent-to-orchestrator protocol material: the consequence of the agent's inability-to-proceed
   report now parking its issue, and what releases that park.
5. The logging, status and observability material: the records emitted on park and on release, the
   release trigger named on the release record, and the parked set surfaced in the runtime snapshot.
6. The persistence material: the durability of the park across restart, and the statement that the
   park is current state rather than run history.

## Confirmation

The decision is validated when all of the following hold:

1. A parked issue whose parking label a person removes is released on the next poll tick and becomes
   an ordinary dispatch candidate on that same tick.
2. A parked issue that a person moves to a different tracker state is released on the next poll
   tick, and where the new state is not active the issue remains undispatchable by the pre-existing
   active-state gate rather than by the park.
3. Both releases behave identically for a park taken because the agent reported it could not proceed
   and for a park taken because the absence ceiling was reached.
4. A park whose label write failed, or whose adapter recorded nothing, is not released by the
   absence of that label. The issue stays parked.
5. A park whose label is later observed on the issue becomes releasable by removal of that label,
   and the confirmation survives a restart.
6. Releasing a park whose trigger maintains a counter ends that counter's sequence, so the same
   tick's trigger does not re-park the issue, and a subsequent run begins a fresh sequence.
7. Releasing a park whose trigger maintains no counter touches no counter.
8. An issue released on a tick is not re-parked on that tick by a derivation computed before the
   reset.
9. A release is recorded, naming which of the two gestures triggered it, so an operator can tell a
   released issue from one that was never parked.
10. An operator with no access to the orchestrator host, its configuration, or its command surface
    can release a parked issue using only the tracker.
