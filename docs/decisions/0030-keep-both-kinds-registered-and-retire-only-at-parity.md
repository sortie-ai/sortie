---
status: accepted
date: 2026-09-09
decision-makers: Serghei Iakovlev
---

# Keep Both Kinds Registered and Retire a Hand-Written One Only at Parity

## Context and Problem Statement

Adopting one generic transport does not consolidate the roster; for part of it, it doubles the names. A runtime that moves onto the protocol keeps the hand-written kind that has always driven it and gains a second route through `agent-client-protocol`, with the runtime named in `agent.command`. Both are registered, both work, and they are not equivalent. That is not a transitional accident but the shape the roster takes for every runtime the eligibility rule admits, once per move, while any hand-written kind remains.

The inequality is measured rather than anticipated, which makes this a decision and not a forecast: on the first pair to exist each route carried something the other did not, and the capability an operator would expect to settle the choice was absent from both. No single axis orders two routes, an advantage on one can be conditional on configuration, and the axes separating one pair are not those separating the next.

A runtime moves onto the protocol when its protocol surface carries every load-bearing capability at least as well as its own structured native surface, or when it has no such surface at all, measured on both sides. That rule is settled, and the roster does not consolidate at adoption. What it leaves open is what becomes of the hand-written integration a moved runtime leaves behind. The question is identical for every pair, and the answer lived only in a maintainer's working notes.

## Decision Drivers

1. **A released `agent.kind` value is a compatibility surface.** Operators write the kind into workflow files an upgrade must not break. A kind that disappears in the release publishing its replacement turns a working deployment into a failing one, with no warning and no window.

2. **The inequality between two routes is per pair.** Tool-server delivery, session continuation, credential verification, and what the offline verdict refuses divide differently behind each runtime. No general claim that one route is better survives the second pair.

3. **What separates the routes is transport-wide, not runtime-specific.** The differences that block removal are properties of a generic kind that knows no runtime and may not learn one. Closing one for a single runtime closes it for none.

4. **Deprecation is a promise; direction is not.** A deprecation notice commits the project to a removal. Issuing one before the removal is achievable teaches operators that notices need not be acted on, which is expensive to unlearn.

5. **A migration costs the operator one line.** The project's transition is not the operator's to pay for: the same workflow, the same behavior, one changed value. A route that withdraws a capability an operator holds fails that test whatever it gains elsewhere.

6. **The question recurs unchanged.** One move creates one pair, and the roster carries runtimes still to move. Answering per runtime would re-argue identical reasoning and let the outcomes diverge for no reason an operator could see.

## Considered Options

- One standing policy: both kinds registered, the protocol named as the direction, removal gated on measured parity, retirement staged
- Deprecate the hand-written kind in the release that publishes the protocol route
- Remove the hand-written kind once the protocol route works
- Publish nothing: keep the protocol route an undocumented experiment until parity is reached
- Keep both routes indefinitely, with no stated direction and no retirement path
- Decide each pair on its own, when the pair appears

## Decision Outcome

Chosen option: **one standing policy: keep both kinds registered, name the protocol as the direction, and gate removal on measured parity**, because the only statement about the future that costs an operator nothing is a direction, and the only honest condition for removing a route is that removal withdraws nothing an operator holds.

### Both kinds stay registered

A workflow file naming either kind keeps working across the release that publishes the second route, and the hand-written route keeps its documentation rather than losing it quietly. Publishing a route is additive: it registers a kind, it does not retire one.

### What an operator is told

The protocol is the transport this project builds on. New runtimes arrive on it, and a runtime that qualifies moves onto it. That is a direction rather than a deprecation: no deprecation is in force, and none is issued before its condition is met.

Where two routes reach one runtime, what each costs is documented for that runtime, where `agent.kind` is chosen rather than in an adapter's working notes. It is per runtime because the difference is per runtime, and one sentence covering every pair would be wrong somewhere.

### The condition that gates removal

A hand-written route is removed only once the protocol route reaches full functional parity with it and no capability an operator holds is withdrawn. Parity comes from the protocol where the protocol carries the capability and from this project's own code above the transport where it does not, and a helper written to close such a gap retires once the protocol carries what it compensates for.

Three classes of difference were open when this record was written, and none belongs to a single runtime:

- **Verifying a credential before the first session.** Parity means a workflow whose runtime cannot authenticate fails before the first turn, with the credential named as the cause rather than the transport.
- **Carrying configured credentials into a remote launch.** Parity means a runtime launched over SSH receives what the operator configured for it, with the local launch unchanged.
- **What `sortie validate` refuses offline.** Parity means the offline verdict refuses the configuration mistakes a hand-written route refuses, without the generic kind holding a table of any runtime's flags.

Each is stated as the capability required rather than the deficiency observed, so closing one changes what is true of the code and nothing here. They are a floor, not a ceiling: the condition compares two routes for one pair, and a difference found later blocks removal exactly as these do.

### The staged path out

Deprecation begins only after the condition above is met, and proceeds in stages.

1. The kind is marked deprecated and stays registered. A configuration naming it runs, and draws a deprecation warning naming a migration guide.
2. It holds for one release at minimum and two by preference, so an operator who upgrades rarely still meets the warning.
3. On removal, a configuration naming the kind is converted on the fly onto the surviving route, and the operator is warned, from `sortie validate` as well as at run time, that the kind is gone, that it was converted, and that the conversion is temporary.

No release turns a working workflow file into a failing one without that path having been walked first.

### A runtime that arrives with no predecessor

A runtime whose first integration is the protocol falls outside all of the above: nothing to migrate from, no route to deprecate, no conversion to write. It is still held to the roster's consistency, whether a capability comes from the protocol or from this project's code above the transport. A runtime that supports less than its neighbors is not an outcome this project ships, and "the protocol does not carry it" is not an answer an operator is given.

### What this record settles once

This is the standing answer for every runtime reachable through both a hand-written kind and the generic one: the pair that exists, and every pair a later move creates. A move onto the protocol needs a measurement and a documentation change, not a decision; what it produces is that runtime's own evidence, which belongs where the runtime is documented.

Two things reopen the policy rather than apply it. The first is a pair whose configuration has no behavior-preserving conversion onto the surviving route: the last stage of the path is then unavailable, removal breaks a workflow file however it is staged, and the choice between keeping a route forever and breaking a configuration deliberately is not made here. The second is a proposal to remove a route at the cost of something an operator holds, which reverses the fifth driver and cannot be granted by applying the policy resting on it.

### Considered Options in Detail

**Deprecate the hand-written kind at once, or remove it once the protocol route works.** Both rejected on the first driver and the fifth. Working is not parity. Removing in the release that publishes the replacement breaks configurations already released, and a deprecation announces a removal the project could not then deliver, which spends the credibility of every later notice.

**Publish nothing until parity.** Rejected as a steady state, and recorded as the fallback it was. It affects no operator, and pays by leaving the transport everything is meant to arrive on unexercised and undocumented, so the differences that gate parity surface last rather than first. Keeping both kinds registered buys the same guarantee for free.

**Keep both routes indefinitely.** Rejected because it makes the duplication permanent. Shared behavior lands twice, an operator never learns which name to write, and the transport's justification erodes: the next runtime is meant to cost a measurement rather than a parser, which is untrue while the parsers are maintained beside it.

**Decide each pair on its own.** Rejected on the sixth driver. Pairs differ in their measurements, not in what should happen to them, so deciding per pair re-argues a settled question, invites a different answer each time, and puts a decision in front of a move that needs only a measurement.

## Consequences

### Positive

- No upgrade breaks a workflow file over this. Adding a route registers a kind; removing one is staged, warned at every step, and converted automatically at the end.
- Retirement has a stated condition instead of a judgment call, so a route is neither removed for looking obsolete nor kept for looking familiar, and every later move inherits the answer rather than reopening it.

### Negative

- **The duplication is paid every release until parity.** Two routes to one runtime means two documented surfaces, two test suites, and a change to shared behavior landing in both.
- **The condition carries no date, and nothing forces the last difference closed.** A hand-written route can outlive its usefulness while each difference stays open on its own merits.
- **Compensating above the transport moves cost rather than removing it.** A helper written to reach parity is code this project owns, and the obligation to retire it once the protocol carries the capability is unenforced.
- **A standing policy is applied by whoever moves a runtime.** Nothing checks that a move was documented or that a removal met the condition, so the policy holds only while it is read.

## Confirmation

1. A workflow file naming either kind for a runtime reachable both ways runs unchanged across the release that publishes the second route.
2. The description of `agent.kind` an operator reads states the direction and, for a runtime reachable both ways, what each route costs.
3. No kind is deprecated while any difference in the removal condition stands, and a deprecated kind stays registered, runs, and warns naming a migration guide through at least one release before removal.
4. After removal, a configuration naming the removed kind is converted on the fly, and `sortie validate` and the run both warn that the kind is gone, that it was converted, and that the conversion is temporary.
5. A runtime whose first integration is the protocol is documented and operated like every other kind, with no route-specific procedure and no capability an operator is told to do without.
6. A move onto the protocol produces a measurement and a documentation change, not a second decision record. A record is opened only for a pair whose configuration cannot be converted, or to remove a route at a capability cost.
