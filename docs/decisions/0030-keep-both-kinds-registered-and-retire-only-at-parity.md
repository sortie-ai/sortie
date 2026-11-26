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

4. **Deprecation is a promise; direction is not.** A deprecation notice commits the project to a removal. Issuing one before every difference between the routes is known and stated teaches operators that notices need not be acted on, which is expensive to unlearn.

5. **A migration costs the operator a mechanical edit, nothing more.** The project's transition is not the operator's to pay for: the same workflow, the same behavior, a configuration edit made mechanically. A route that withdraws a capability an operator holds fails that test whatever it gains elsewhere.

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

The protocol is the transport this project builds on. New runtimes arrive on it, and a runtime that qualifies moves onto it. That is a direction rather than a deprecation until the difference between the routes is fully known: a hand-written kind is deprecated once every difference has been graded, by measurement or, where none could be made, as unmeasured, and each one still standing is stated to the operator, with the advice to stay on the hand-written kind while it is needed. Deprecation announces the direction with the remaining cost named; it does not wait for parity, and removal does.

Where two routes reach one runtime, what each costs is documented for that runtime, where `agent.kind` is chosen rather than in an adapter's working notes. It is per runtime because the difference is per runtime, and one sentence covering every pair would be wrong somewhere.

### The condition that gates removal

A hand-written route is removed only once the protocol route reaches full functional parity with it and no capability an operator holds is withdrawn. Parity comes from the protocol where the protocol carries the capability and from this project's own code above the transport where it does not, and a helper written to close such a gap retires once the protocol carries what it compensates for.

Three classes of difference were open when this record was written, and none belongs to a single runtime:

- **Verifying a credential before the first session.** Parity means a workflow whose runtime cannot authenticate fails before the first turn, with the credential named as the cause rather than the transport.
- **Carrying configured credentials into a remote launch.** Parity means a runtime launched over SSH receives what the operator configured for it, with the local launch unchanged.
- **What `sortie validate` refuses offline.** Parity is judged by the capability an operator would lose, not by the surviving route repeating every offline check of the removed one: a refusal that guards against a failure the surviving route cannot reach does not block removal. A protocol runtime that rejects its own arguments by exiting before it answers its handshake is reported before the first turn with its exit status and its own error output, never as a credential failure.

Each is stated as the capability required rather than the deficiency observed, so closing one changes what is true of the code and nothing here. They are a floor, not a ceiling: the condition compares two routes for one pair, and a difference found later blocks removal exactly as these do.

### The invocation of a protocol-driven route

On a generic protocol-driven kind the runtime invocation stays one opaque command, written as a string or as a list whose every element is one argument. The kind gains no field naming a runtime's switch and no offline check beyond those every kind performs. Judging a switch, a model or a trust posture offline would take a table of every runtime's switches, which a kind that launches any runtime does not hold. Only the runtime can judge its own arguments, so what the operator is owed is the runtime's own account of what it rejected.

**Typed runtime-neutral fields**, such as a model applied through the protocol's session configuration, were rejected, the reasoning level excepted. No such value is verifiable offline: whether it is valid is known only once the runtime reports what it accepts, and the runtime's own switches already work inside the command.

**The reasoning level is the one such field the kind applies.** The kind reads the shared `effort` key and, each time a session opens, created, continued or verifying a credential, before its first turn, sets it on the first select option the runtime lists under the protocol's `thought_level` category. The option is found by that category, never by the runtime's option id, so the kind still knows no runtime. The level is checked against the option's offered values before it is sent. A runtime that lists no such option, does not offer the level, refuses or leaves the request unanswered, or reports another value draws a warning, and the session runs at the runtime's own level: the level never fails a session. Like the model, the level cannot be judged offline, and the warning when the session opens takes the place of an offline check rather than meeting that ground. It differs from the model on the second ground: a runtime whose protocol entry point takes no reasoning switch, OpenCode among them, has no carrier for the level inside the command.

The offline checks carry one exception of the same origin. A `variant` key in the kind's block, the `opencode` kind's second name for the level, draws a `sortie validate` warning naming `effort`, so a block carried over from that kind does not lose its level without a word. The exception lasts while the `opencode` kind is registered, and that kind's removal deletes it.

**The runtime's own session options, named by the operator, are the other values the kind applies through the session configuration.** A `config_options` mapping in the kind's block pairs an option id the runtime lists with one of that option's value ids, and each time a session opens, created, continued or verifying a credential, before its first turn, the kind sets every pair on the select option the runtime lists under that id. It is not a typed field: it names no setting and holds no table of runtimes, the operator writes the runtime's own vocabulary as the command does, and only the runtime judges it. Like the level, an entry cannot be judged offline, and the warning when the session opens takes the place of an offline check: each value is checked against the option's offered values before it is sent, and an entry the session does not apply draws a warning while the session runs with the runtime's current value. An entry never fails a session. It never reaches an option the runtime lists under the protocol's `model` or `thought_level` category: the model stays out of the session configuration, and the level has the one key `effort`. The mapping carries what the command cannot: a runtime can accept a switch on its protocol entry point and apply nothing, as Copilot CLI does with a named agent.

The list form of the command is not such a field. It names no switch and is checked offline only for its shape: a list, non-empty, with a non-empty string in every element. It exists because a local launch splits a string on whitespace, so only a list carries a switch value that holds a space, such as an agent name. The string form keeps working unchanged.

**Workflow-declared properties** of the runtime, with a configuration contradicting one refused offline, were rejected. The failure they would guard against, a runtime waiting on an approval nobody can give, does not occur on this route, because this project answers every permission request inside the protocol. How this project answers is the one permission property the workflow declares, because it is this project's own reply rather than a property of the runtime: refusal by default, or the once-only approval of a request that offers exactly one way to grant it, one value for every runtime the kind drives. It names no runtime's switch, so the invocation stays the one opaque command above, and offline it is checked only for holding one of its values. Without the approval, a missing trust switch costs a refused tool call, reported at run time.

### The staged path out

Deprecation begins once every difference is graded and stated, as above. Removal waits for the condition above. The path proceeds in stages.

1. The kind is marked deprecated and stays registered. A configuration naming it runs, and draws a deprecation warning naming the replacement kind.
2. It holds for one release at minimum and two by preference, so an operator who upgrades rarely still meets the warning, and in any case until the removal condition is met.
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
- **A mistake in a protocol runtime's command surfaces at launch, not offline.** `sortie validate` accepts a misspelled switch or an unknown model on a generic kind; the runtime reports it when it starts.
- **A reasoning level the runtime does not offer surfaces at run time.** It draws a warning when the session opens, never an offline error and never a refused turn, and the session runs at the runtime's own level.
- **A continued session with no level keeps the level its runtime holds.** The session configuration has no way to send an absent level, so the kind logs the level the session runs at rather than restoring the runtime's default.
- **A session option the runtime does not offer surfaces at run time.** A `config_options` entry is checked offline only for its type; an id or value the session does not list draws a warning when the session opens, never an offline error and never a refused turn.
- **Removing a `config_options` entry does not reset its option.** An option no entry names keeps the value its runtime holds, which on a continued session can be one an earlier session set.
- **A deprecation can precede the route an operator needs.** An operator who relies on a capability the protocol route still lacks meets the warning with nowhere to move yet, and is told to stay on the hand-written kind until the difference closes.
- **A standing policy is applied by whoever moves a runtime.** Nothing checks that a move was documented or that a removal met the condition, so the policy holds only while it is read.

## Confirmation

1. A workflow file naming either kind for a runtime reachable both ways runs unchanged across the release that publishes the second route.
2. The description of `agent.kind` an operator reads states the direction and, for a runtime reachable both ways, what each route costs.
3. No kind is deprecated while a difference between its routes is ungraded or unstated to the operator, and no kind is removed while any difference in the removal condition stands. A deprecated kind stays registered, runs, and warns naming the replacement kind through at least one release before removal.
4. After removal, a configuration naming the removed kind is converted on the fly, and `sortie validate` and the run both warn that the kind is gone, that it was converted, and that the conversion is temporary.
5. A runtime whose first integration is the protocol is documented and operated like every other kind, with no route-specific procedure and no capability an operator is told to do without.
6. A move onto the protocol produces a measurement and a documentation change, not a second decision record. A record is opened only for a pair whose configuration cannot be converted, or to remove a route at a capability cost.
7. The reasoning level is the only typed setting the generic kind applies through the protocol's session configuration; no other typed setting, the model included, is applied that way. Beside it only an operator's `config_options` entry is applied that way, never to an option the runtime lists under the `model` or `thought_level` category. The `variant` warning is the kind's only offline check keyed on one runtime's vocabulary.
