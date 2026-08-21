---
status: accepted
date: 2026-08-21
decision-makers: Serghei Iakovlev
---

# Refuse Agent Requests That Only a Human Could Answer

## Context and Problem Statement

Sortie drives coding-agent runtimes unattended. Several of those runtimes can interrupt their own
work to ask for a decision a person would normally make: permission to run a command, to change a
file, to use a tool, to widen a sandbox, or a genuine question addressed to a human. Some of those
requests are protocol-level requests that expect a reply, and a runtime that has sent one may wait
for that reply with no deadline of its own.

No posture governed that class of request. One adapter observed such requests and neither answered
them nor ended the turn: the request fell through to a generic branch that logged the message and
wrote nothing back. The turn stayed open until an orchestrator-side timeout expired, the attempt was
reported as a generic turn failure, and that classification scheduled a retry which could only
re-enter the same wait. Three costs followed from one omission. A full turn budget was spent waiting
for an answer that was never coming. The reported reason named a timeout instead of the situation.
The bounded retry budget drained into a state no retry could change.

The agent adapter contract already forbade leaving such a request stalled, and already required a
request for human input to fail the run attempt immediately. It left the posture for permission
requests unstated, which is what this decision settles. Two normalized outcome kinds for this
situation were declared in the domain vocabulary and produced by no adapter, only by the test
double.

## Decision Drivers

1. **Consent on a person's behalf is outside the orchestrator's authority.** An unattended run
   contains no one who can grant permission. A reply that grants it asserts a decision no
   participant was authorized to make, and it widens what the agent may do at precisely the moment
   nobody is watching.
2. **An ending is a signal, and everything downstream reads it.** A run that stopped because it
   needs a person is a different fact from a run that failed and from a run that produced nothing.
   Where all three arrive as one generic failure, the operator is told that something timed out, the
   run record says the same, and the retry classification that follows sends the attempt back into
   the wait it just left. The retry budget is bounded on purpose, so an attempt that cannot change
   the outcome spends remediation where it cannot work and leaves it unavailable where it could.
3. **Prevention at launch is not a guarantee.** Configuring a runtime non-interactively removes most
   of these questions before they are asked. It does not remove all of them: at least one runtime's
   human-question path is not governed by its approval configuration at all, so an approval setting
   is not in general a statement about what a runtime will ask. A layer known to leak cannot be the
   only layer.
4. **Uniformity belongs to the outcome, not to the wire.** Whether a refusal can be transmitted is a
   property of a runtime's protocol. Whether the attempt ends, how it is classified, and what the
   operator is told are properties of this system. Letting the first decide the second gives an
   operator behaviour that varies by adapter for reasons that have nothing to do with policy.
5. **A behaviour every adapter must have cannot live in each adapter.** A rule replicated per
   adapter is a rule a new adapter can omit, and omission here is silent: the adapter builds, the
   run starts, and the gap appears only when a runtime asks a question. A rule held in the shared
   layer makes inheritance the default and divergence something review has to see.
6. **A configuration surface must carry the same meaning on every runtime.** A setting whose values
   are unenforceable on most adapters is not a control but a claim the system cannot honour, and its
   default would still have to be whatever the system does anyway.
7. **A verdict about configuration must be the same wherever configuration is read.** Configuration
   is read at startup, again on reload, and offline by a validation command that constructs nothing.
   A refusal confined to one construction path is fatal in one deployment shape, advisory in
   another, and invisible to the offline check, so one contradictory setting yields three answers.
8. **The vocabulary describes what the system does, or it misleads.** An outcome kind no producer
   emits teaches a reader something untrue, and one only a single runtime could ever emit is a
   private detail wearing a shared name.

## Considered Options

- Refuse centrally: the shared layer decides the posture, a permission request is refused in a form
  that lets the agent continue by another route, a request for human input ends the attempt at once
  with a dedicated non-retryable outcome, and every runtime is configured non-interactively at
  launch
- Auto-resolve the request by replying with a session-scoped acceptance
- Surface the request to an operator and hold the turn open until an answer arrives
- Refuse the contradictory configuration at adapter construction and change nothing else
- Make the posture operator-configurable
- Rely on the existing turn and stall timeouts

## Decision Outcome

Chosen option: **refuse centrally**, because it is the only option that gives every request of this
class a defined ending, keeps that ending identical across runtimes whose protocols differ, and puts
the decision in the one place a new adapter cannot fail to inherit.

### The posture

When an agent runtime asks for a decision that only a human could give, Sortie refuses. It does not
synthesize consent on a person's behalf, does not fabricate an answer to a question, and does not
leave such a request unanswered where a reply channel exists.

Two classes carry two consequences, and the class is determined by what the runtime asked for, not
by which runtime asked.

A permission request, may I run this, may I change this, may I use this, is refused in the form that
permits the agent to continue the turn by another route, and the refusal carries a reason addressed
to the agent rather than to the log alone.

A request for human input, a genuine question, is never answered. The attempt ends immediately with
the dedicated human-input-required outcome.

The posture is not an operator setting. It is the same on every deployment and every runtime.

### Prevention first, refusal always

Sortie configures each agent runtime non-interactively at launch, so that most of these questions
are not asked at all. That layer is not airtight: at least one runtime's human-question path is not
governed by its approval configuration, and a configuration that governs approvals is not in general
a statement about a runtime's question path. The refusal path is therefore mandatory and always
present. It is not an optional fallback, and not a defence an adapter may omit on the grounds that
its launch configuration should have prevented the question.

### One place decides

The shared layer that every agent adapter sits behind decides the posture and produces the
normalized outcome. An adapter contributes exactly two things: recognizing that a given runtime
message belongs to one of the two classes, and, where its protocol offers a reply channel, emitting
the refusal in that protocol's own vocabulary. An adapter never decides the posture, never chooses
the consequence, and never constructs the normalized outcome itself. An adapter whose runtime
reports a situation the shared rule cannot express extends the shared rule, under review, rather
than branching privately. This is the arrangement the project already uses for the turn-disposition
decision.

### The ending is a declared signal

The human-input-required outcome is not folded into generic turn failure. It carries the
non-retryable classification already defined for it: the claim is released rather than a retry
scheduled, because a retry re-enters the same wait. The reported reason names the situation rather
than describing a timeout. A run that stopped because it needs a person stays distinguishable from a
run that failed and from a run that produced nothing.

### Runtimes with no reply channel

Where a runtime offers no way to answer, the posture is still uniform in its outcome: prevention at
launch, and, where such a request is nonetheless observed, the same human-input-required ending.
That ending covers both classes. A permission request can be refused in a continuable form only if
the refusal reaches the agent, so where it cannot reach the agent there is no route left to
continue, and the attempt ends rather than proceeding blind. What differs between adapters is only
whether a refusal can be transmitted, and that is a property of the runtime's protocol rather than
a policy an adapter chooses. Uniformity is required of the outcome and of the decision, not of the
wire mechanics.

### Configuration that re-opens the interactive path

Where a runtime-specific pass-through value can defeat the non-interactive launch posture, it is
refused through the shared configuration-diagnostic channel, so that the same verdict is reported at
startup, on reload, and in offline validation. It is not refused only inside an adapter constructor:
a constructor refusal is fatal when the adapter is the configured default and a mere warning
otherwise, and offline validation cannot observe it at all. Adapters remain the owners of their own
pass-through vocabulary; what is shared is the channel the verdict is reported through. Existing
constructor-level refusals of contradictory permission configuration move behind that same channel.

### Vocabulary

The auto-approval outcome kind is removed from the vocabulary. No posture in this decision produces
it, and an outcome kind only one runtime could ever produce is not a uniform part of a shared
vocabulary. The human-input-required outcome kind is live and load-bearing: the shared layer
produces it, and the classification already attached to it takes effect. An outcome kind is retained
without a producer only where the contract states the reservation explicitly, as it does for one
transport-class kind.

### What this decision does not settle

A request a program can satisfy without a person is not of this class. A credential refresh the
orchestrator can answer from its own configuration asks for a value rather than for consent, and it
belongs to the path that owns the credential; where such a request carries a deadline of its own,
the stall this decision addresses cannot arise from it. The refusal rule does not reach requests of
that kind.

This decision governs the class of requests that only a human could answer. It does not govern, and
must not foreclose, the wider question of which positive terminal outcomes an agent may declare
about its own run. Whether that vocabulary later gains an entry for, say, an agent declaring that
the requested result was already true and that nothing needed changing is left open here, and such
an entry must remain distinguishable both from an uncertain run and from a run that produced
nothing. Where this decision touches that ground, it does so only by insisting that an ending is a
declared signal rather than something the orchestrator infers from residue or suppresses after the
fact.

### Considered Options in Detail

**Auto-resolve by replying with a session-scoped acceptance.** Rejected on two independent grounds.
It is expressible on exactly one of the project's agent runtimes, the only one with a reply channel,
so it cannot be a uniform posture: it would be real behaviour on one adapter and absent on every
other. And it grants consent on a person's behalf silently, with no operator declaration behind it,
while the surface that could carry such a declaration is rejected below as unenforceable. It has the
highest throughput of the options considered, and in the unattended case it buys no behavioural gain
over refusal while widening what the agent may do unsupervised.

**Surface the request to an operator and wait.** Out of reach without new cross-process plumbing.
The project's operator-notification delivery deliberately lives outside the orchestrator process,
and an adapter runs inside it. The failure model also offers no parked or awaiting-input state for
this case. Holding the runtime open while waiting is the behaviour this decision exists to remove,
and suspending durably and returning control is a different and much larger design.

**Refuse the configuration at adapter construction, and nothing else.** Insufficient and misplaced.
Insufficient because at least one runtime's human-question path is not governed by the approval
configuration at all, so refusing a configuration value cannot close the case. Misplaced because a
constructor refusal carries two different severities depending on whether the adapter is the
configured default, and offline validation never reaches it. The useful part of this option survives
as the shared configuration verdict above.

**Make the posture operator-configurable.** Rejected under the project's standing rule against
configuration surfaces that are not uniformly meaningful. Most values would be no-ops on most
runtimes, so the setting's behaviour would depend on which adapter is in use, and the default would
still have to be this same decision under another name.

**Rely on the existing timeouts.** The turn and stall timeouts do prevent an unbounded hang, so the
defect was never an infinite wait. The defect is a full timeout burned for nothing, a reported
reason that names a timeout instead of the situation, and a retry classification that sends the run
back into the same wait. Timeouts remain the backstop; they are not the answer.

## Consequences

### Positive

- A request of this class always has a defined ending, and the ending names the real situation.
- The claim is released instead of retried, so a run cannot consume its retry budget re-entering the
  same wait.
- The posture is decided in one place, so a new adapter inherits it rather than re-implementing it,
  and cannot get it wrong by omission.
- One vocabulary entry with no producer is removed and another becomes load-bearing, so the
  vocabulary describes what the system does.
- A configuration value that would re-open the interactive path draws the same verdict at startup,
  on reload, and offline, so an operator learns of the contradiction before a run rather than from
  one.

### Negative

- **A refused permission can cost a completion.** An agent denied consent may abandon a route it
  would have taken with consent, so some runs that could have completed under supervision end
  without completing.
- **A refusal the agent answers by asking again can loop.** The existing timeouts remain the only
  bound on that, and this decision adds no dedicated one.
- **Adapters whose runtimes cannot be answered gain a recognition path with no reply behind it.**
  Its only effect is to end the turn sooner and with a better reason, so the cost is paid in each
  such adapter for a benefit that is entirely in the reporting.
- **A deployment that deliberately ran a runtime interactively loses that ability.** That follows
  from the decision rather than from an oversight.
- **One runtime's behaviour is not established.** What it does when it meets an untrusted tool with
  nothing trusted is unknown, so a recognition path for it rests on a reading of the runtime rather
  than on observed behaviour.

## Confirmation

The decision is validated when all of the following hold:

1. A permission request from a runtime with a reply channel draws a refusal in that runtime's own
   protocol, carrying a reason addressed to the agent, and the turn continues rather than waiting.
2. A request for human input ends the attempt at once, with the human-input-required outcome rather
   than a generic turn failure.
3. That ending releases the claim and schedules no retry.
4. The operator-facing reason for that ending names the situation rather than a timeout, and the run
   record distinguishes it from a run that failed and from a run that produced nothing.
5. Every agent adapter reaches that ending through the shared layer, and none constructs the
   normalized outcome itself.
6. A runtime with no reply channel, observed making a request of either class, ends the attempt with
   the human-input-required outcome.
7. A configuration value that would re-open the interactive path is refused at startup, on reload,
   and by offline validation, with the same verdict in all three.
8. No auto-approval outcome is produced anywhere, and the vocabulary declares none.
9. Every agent runtime is launched in a mode that does not permit it to ask interactively.
