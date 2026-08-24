---
status: accepted
date: 2026-08-24
decision-makers: Serghei Iakovlev
---

# Give the Consecutive-Absence Ceiling Its Own Setting

## Context and Problem Statement

Two ceilings bound how much work the orchestrator will spend on one issue, and a single configured
integer set both.

The per-issue effort budget, `agent.max_sessions`, counts every completed run recorded for an issue
and blocks further dispatch once that count reaches the configured value. The count carries no
predicate on outcome. A run that finished a phase of the work, a run that crashed, and a run that
produced nothing are one row each and are indistinguishable to it.

The consecutive-absence ceiling bounds how many runs in a row may be observed to have produced
nothing before the issue is parked under the escalation label. It had no setting of its own. It took
the value of `agent.max_sessions` where the deployment had set that field, and fell back to a
built-in value of three where the deployment had not.

The two ceilings express different judgements and want opposite values. A cost ceiling states how
much an issue may be allowed to cost across its whole life, and it wants to be generous, because
legitimate work can be long. A runaway guard states how many fruitless attempts are worth making
before a human should look, and it wants to be tight, because every attempt past the useful ones is
waste. One integer cannot be both, and an operator choosing it for either purpose silently set it
for the other.

The cost of the collision fell on the operator reasoning about the runaway guard. A workflow that
advances an issue through several phases, one run per phase, spends the effort budget by working
correctly. An operator who chose a low value to bound a loop thereby imposed that value as a
lifetime cost ceiling, and dispatch stopped partway through a sequence of successful phases with
every ceiling behaving exactly as specified.

## Decision Drivers

1. **The two ceilings do not measure the same quantity.** One counts runs over a lifetime without
   regard to what they produced. The other counts runs in a row that produced nothing, and any run
   that produces something resets it to zero. A value that is correct for a lifetime total is
   arbitrary as a tolerance for repetition, and the reverse.
2. **A cost ceiling that forgives spend is not a cost ceiling.** The unit of account is cumulative
   consumption per issue, settled in ADR-0013. A run that advanced the work consumed tokens and
   wall-clock exactly as a run that failed did. Exempting it would leave total spend unbounded,
   which is the gap the effort budget exists to close.
3. **Progress buys a cheaper attempt in comparable practice, never a larger budget.** Durable
   execution engines, batch job controllers and scheduling systems all carry exemptions from a retry
   budget, and every one of them is cause-based, asking why an attempt failed. None is
   progress-based. Where progress is recorded, it is delivered to the next attempt so that attempt
   resumes rather than restarts. The budget itself stays monotonic.
4. **The two-counter split is the settled shape for this problem.** The same systems, and comparable
   agent orchestrators, pair a total ceiling that never resets and whose outcome is terminal, with a
   separate consecutive-failure counter that any success clears and whose outcome is a pause. One
   container job controller carries the distinction to its conclusion: its consecutive-failure
   counter is zeroed on success and is wired to retry pacing alone, never compared against any
   limit. Progress modulates pacing; it does not move a ceiling.
5. **Repeated absence must still terminate.** ADR-0020 established that an issue producing nothing
   is dispatched a bounded number of times and then parked. That bound is not optional and its
   removal is not an outcome any new setting may offer.
6. **No deployment may lose a cost cap it configured.** An operator who set `agent.max_sessions` has
   a spend cap. Whatever this decision changes, that operator must still have one afterwards, and
   must not have to discover its removal.

## Considered Options

- Leave one setting governing both ceilings, and document the coupling
- Give the consecutive-absence ceiling its own setting, defaulted to the value it already used
- Derive the consecutive-absence ceiling from the effort budget by a stated rule other than identity
- Redefine the effort budget to count only runs that did not advance the work
- Change the effort budget's default value

## Decision Outcome

Chosen option: **give the consecutive-absence ceiling its own setting, defaulted to the value it
already used**, because the two ceilings answer different questions and the shared value was the
only thing forcing one answer onto both, and because the separated setting is invisible to every
deployment that does not touch it.

### The effort budget does not change

`agent.max_sessions` keeps counting every completed run for an issue, unconditionally, and keeps
blocking dispatch at the ceiling. It is a cost ceiling and it stays absolute. Nothing an issue
produces buys it more budget.

This also settles the proposal to count only failed and cancelled runs toward it. Beyond driver 2,
the terminal status recorded for a run is not a progress signal: the successful value is recorded
for a blocked soft stop, for a run that does not drive issue state, for a run whose evidence was not
determinable, and for every run under the `off` evidence policy. Exempting that value would let an
issue consume unbounded effort while recording almost nothing the budget could count.

### The new setting

A new integer field, `agent.max_consecutive_absences`, bounds the count of consecutive observed
absences that ADR-0020 defined. It sits beside `agent.max_sessions` and `agent.max_tokens`, is
overridable through `SORTIE_AGENT_MAX_CONSECUTIVE_ABSENCES`, and is re-read on live reload like its
siblings. Its default is three, which is the value the ceiling already took wherever no effort
budget was configured, and the effort budget is unlimited by default.

The field departs from the shape its two siblings share in one respect, deliberately. `0` does not
mean unlimited; it is rejected as a configuration error naming the field, as any negative value is.
Driver 5 forbids an unbounded absence sequence, so a value expressing one is not an option an
operator may select. The complete escape already exists and is better: setting the evidence policy
to `off` computes no verdict at all, so no absence is ever observed and no count ever advances. That
escape costs no inspection, where an unlimited ceiling would inspect every run and act on nothing.

Everything else about the mechanism is unchanged. The count is still consecutive, any run that
produces evidence still resets it, reaching the ceiling still parks the issue under the escalation
label the recovery paths already use, and no new state, field or tracker transition is introduced.

### Why the earlier argument against a second setting does not survive

ADR-0020 rejected a second numeric setting on two grounds, and the second one carried the weight:

> A new setting would be a knob for a failure mode the operator has never heard of, put in front of
> every operator to serve the few who will ever tune it, and each of them would have to choose a
> value for it before having any experience of the fault it governs. The ceiling that already exists
> expresses the deployment's tolerance for repeating an attempt that is not working, and that is the
> same judgement being asked for here.

The final sentence is the premise, and the system does not satisfy it. `agent.max_sessions`
expresses tolerance for total effort, not for repetition that is not working, because it counts
every run at the same rate whatever the run produced. ADR-0020 recorded that property accurately two
paragraphs earlier, observing that the effort budget "cannot tell a sequence of absences apart from
productive iteration, because both spend it at the same rate", and treated it as a reason the
ceiling was only a partial bound. The consequence not drawn there is the one field evidence has
since forced: a value that cannot tell the two apart cannot express both judgements, so borrowing it
imposes whichever judgement the operator was not making.

The first ground, the cost of a knob, is answered rather than dismissed. It applies to a setting
that demands a choice. This one does not. The ceiling already had a value for the case where no
effort budget is configured, that case is the default, and the new field ships with that same value.
An operator who never reads it gets the behavior they have today. The setting does not put a new
judgement in front of every operator; it withdraws an accidental one from the operators who set an
effort budget. What ADR-0020 was right to refuse, and what this decision also refuses, is a ceiling
the operator must configure before the guard works at all.

### What an operator running today sees

The effort budget is untouched for every deployment. Nobody is uncapped on cost, silently or
otherwise. The absence ceiling moves in three of four cases:

- Effort budget unset, which is the default: no change. The absence ceiling was three and stays
  three.
- Effort budget set above three: the absence ceiling tightens to three. Issues that produce nothing
  are parked sooner, which is announced when it happens and is reversed by setting the new field.
- Effort budget set below three: the absence ceiling loosens to three. Issues that produce nothing
  are dispatched at most two more times than before.
- Effort budget set to exactly three: no change.

The absence ceiling therefore changes for a deployment that set an effort budget other than three,
in a mechanism the operator did not know they were configuring. That is the point of the decision
and it is also a behavior change on upgrade, so the release carrying it states the decoupling and
names the new field. It also names the deployments that tighten: an effort budget well above
three, paired with a workflow whose runs legitimately leave the workspace untouched, is parked
considerably sooner than before. That park is announced and is lifted by a person or by the new
field, but it is the one case where the decision costs a deployment something on upgrade.

The decoupling also changes what a sensible effort budget looks like. An operator who chose a low
budget to bound a loop was choosing against the absence ceiling, not against cost, and after the
split that value is no longer doing the work it was picked for. A workflow that advances one issue
through several phases, each phase a separate run, needs an effort budget sized to the phases it
expects rather than to the loop it feared. The release states this alongside the new field, because
a deployment that upgrades without revisiting the value keeps the ceiling that starved it.

### Considered Options in Detail

**Leave one setting governing both ceilings, and document the coupling.** The cheapest option, and
it has a real argument: the operator learns the relationship and picks a value that serves both.
Rejected because no such value exists in the general case. The two ceilings are optimized in
opposite directions, so documenting the coupling asks the operator to resolve a conflict the system
created and then to accept the worse of the two ceilings whichever way they resolve it. It also
leaves the field-observed failure in place, since the operator who reasons about loops is exactly
the one the documentation would be asking to reason about cost instead.

**Derive the consecutive-absence ceiling from the effort budget by a stated rule other than
identity.** A capped derivation, so that a generous effort budget cannot loosen the guard, is the
strongest form and it fixes one direction of the collision. Rejected because it fixes only that
direction. The reported failure runs the other way: an operator setting a low value to bound a loop
still imposes it as a cost ceiling, and no rule computed from that value removes the ceiling the
value itself is. A derivation also has no defensible arithmetic behind it, because the two
quantities have different units, runs over a lifetime against runs in a row, and it leaves the
operator setting one number and receiving a second, differently-valued ceiling they cannot see.

**Redefine the effort budget to count only runs that did not advance the work.** This is the
proposal the failure most naturally suggests, and it is wrong in the direction that matters.
Drivers 2 and 3 rule it out on principle, and the recorded terminal status makes it unworkable in
practice for the reason given above. It also repairs the wrong instrument: the runaway guard the
proposal is reaching for already exists, is already progress-aware, and already parks loudly.

**Change the effort budget's default value.** Recorded because it is the smallest possible change.
Rejected because the default is already unlimited, so no default can decouple two ceilings that
share a value once the operator sets one.

## Consequences

### Positive

- The two ceilings can be tuned against each other. A deployment may hold a generous lifetime cost
  cap and a tight runaway guard at the same time, which no configuration expressed before.
- The multi-phase workflow shape stops being incompatible with a runaway guard. Bounding a loop no
  longer requires accepting a lifetime cost ceiling of the same size.
- The value the guard uses becomes visible. It was a constant reachable only by reading the source
  and only in the case where no effort budget was set.
- The system now carries the two-counter split in the form driver 4 describes: a monotonic total
  whose outcome is a stop, and a consecutive counter that success clears and whose outcome is a
  park.
- Nothing about the absence mechanism itself changes, so ADR-0020's verdict, reset rule, park and
  escape hatch all keep their meaning.

### Negative

- **The configuration surface grows by one field.** Every operator reading the reference now meets
  two ceilings and has to understand that one counts runs over a lifetime and the other counts runs
  in a row. That cost is real, it falls on all operators, and it is the cost ADR-0020 declined to
  pay. It is accepted here because the alternative is a ceiling that misreports one of the two
  judgements in every deployment that sets it.
- **An upgrade changes absence behavior for deployments that set an effort budget other than
  three.** The direction depends on the configured value, as set out above. The change is bounded
  and reversible by setting the new field, but it is not a no-op and it arrives without the operator
  acting.
- **The new field breaks the shape its siblings share.** `agent.max_sessions` and
  `agent.max_tokens` both read `0` as unlimited. This one rejects `0`. The divergence is justified
  and documented, but an operator who has learned the convention will meet an exception to it.
- **The effort budget stays a lifetime ceiling on a workflow whose natural cost grows with its
  phases.** Separating the guard removes the accidental cause of the reported failure; it does not
  make a per-issue lifetime cost cap easy to size. An operator who wants a cap and does not know how
  many phases an issue will need still has to guess, and the guess is now only about cost.

## Confirmation

The decision is satisfied when all of the following hold:

1. `agent.max_consecutive_absences` parses as an integer, defaults to three, rejects zero and
   negative values with a configuration error naming the field, honors its environment override, and
   re-applies on live reload.
2. The consecutive-absence ceiling reads only the new field. Its value does not change when
   `agent.max_sessions` changes.
3. `agent.max_sessions` blocks dispatch on the same unconditional count of completed runs it used
   before, with no exemption for any outcome.
4. A deployment that sets neither field observes the absence ceiling at three and no effort budget,
   which is what it observed before.
5. A deployment that sets a low `agent.max_sessions` and a high `agent.max_consecutive_absences`
   parks an issue only after that many consecutive absences, and stops dispatch at the effort budget
   independently.
6. Setting the evidence policy to `off` leaves the absence count unreachable, so no value of the new
   field can park an issue in that deployment.
7. The configuration reference states, in the entry for each of the two fields, that the other
   exists and what it separately governs.
