---
status: accepted
date: 2026-09-01
decision-makers: Serghei Iakovlev
---

# Adopt the Agent Client Protocol as a Single Generic Agent Transport

## Context and Problem Statement

Every coding-agent runtime Sortie drives is reached through a hand-written integration that parses that runtime's own output format. Adding a runtime means reverse-engineering a vendor format; keeping one means tracking that vendor's releases. The roster is narrow for that reason, and defects arrive by class rather than singly, because each integration solves the same problem again in its own words.

The Agent Client Protocol is a published, versioned standard for exactly this conversation: a client launches a coding agent as a subprocess and speaks newline-delimited JSON-RPC to it over the child's standard input and output. It normalizes most of what each integration reverse-engineers today. One framing. One turn lifecycle in which every turn ends with a declared outcome drawn from a closed set. One closed set of session updates covering messages, reasoning, tool calls, plans, and mode changes. One way to hand an agent its tool servers, where this project today has a different arrangement in every integration that supports them at all.

Sortie adopts it. Continuing without the protocol was weighed against the same evidence and is recorded below as a rejected option, because it leaves a hand-written integration as the only route to a runtime whose protocol mode ships before any structured native output does, and that ordering has already been observed once: one runtime shipped a working protocol mode six months before it shipped any machine-readable output of its own.

What this record settles is how adoption is structured, how far its scope reaches, what has to be true of a runtime before it moves, and the four questions a protocol-driven runtime cannot be operated without answering: where its token counts come from, whether session continuation is a condition of support, who answers a permission request, and which wire version is pinned.

One navigational fact bears on the answer. The abbreviation of this protocol's name is already used in this project's own documentation for an unrelated industry protocol whose name differs from this one by a single word, and the project's file-based agent-to-orchestrator signalling occupies neighbouring subject matter under an abbreviation of its own. Using the abbreviation here would collide outright with the first and invite confusion with the second, so it is not used: this record says "the protocol" after the name is introduced.

## Decision Drivers

1. **The protocol removes the parsing, not the integration.** One measured client carries fifteen agents and branches on agent identity zero times across its whole source, resolving differences by advertised capability instead; two other clients deleted their per-agent layers after finding them near-duplicates of each other. Against that, the reference client written by the protocol's own authors special-cases agents by name today, including recovering a mandatory stop reason by matching English prose in an error string, a workaround that has been live for over ten months. The saving is real, and it is contingent on a discipline the protocol does not enforce.

2. **The protocol's stable line does not carry token spend.** Its one usage notification reports how full the context window is and how large it is. That figure falls after context compaction, so it cannot satisfy a counter this project requires to be non-decreasing across a run. Real per-turn counters exist only on the line the protocol marks unstable, whose governing design note is a draft that says so, and whose own type description contradicts itself: the container says the figures cover one turn, its three required fields say they cover the session.

3. **A capability a runtime advertises can be false, and this was observed.** One runtime advertised session-continuation support and did not replay the conversation when asked to; the defect stayed open for 69 days and two siblings of it remain open. The protocol builds its entire additive-growth mechanism on those advertisements: an omitted capability must be treated as unsupported, and a client must not call a method the agent did not advertise. A client that trusts the advertisement is wrong, and a client that ignores it is forbidden. Only observation resolves this.

4. **Per-runtime divergence is real, differently shaped on each runtime, and not static.** Four runtimes were measured live on the wire on one host. All four send something the current released stable schema does not contain, and they do it four different ways. Three independently reimplemented a field the protocol removed from its unstable line months earlier, so the field is being reinvented rather than inherited from shared code. One implements a method that appears in none of the four published schema artifacts. Divergence of this kind is a property of the build in front of the client, not of the runtime as a category.

5. **The wire has been stable; everything wrapped around it has not.** Across twelve months the negotiated wire version stayed the integer 1 through 59 library releases, ten method additions and one removal, and the removed surface carried an explicit instability marker. An attempt to falsify this by diffing every type definition between adjacent releases produced fourteen candidate breakages, all fourteen of which turned out to be artifacts of the diffing tool. Over the same window, the reference client performed roughly one protocol-dependency migration every three weeks, and every measured migration was a migration of a language binding rather than of the wire.

6. **The successor line rewrites the consuming half.** Of the current stable line's 170 definitions, 40 do not exist in the draft successor and 45 are new; of the 130 in common, 25 are unchanged. The method count falls from 25 to 16. Most expensively for this project, the response that ends a turn no longer ends it: it acknowledges acceptance, and the outcome and its reason arrive later as a notification. No public artifact carries a stabilization date, and the published migration guidance tells implementers to carry both versions side by side until the successor settles.

7. **Most of what the protocol asks of a client exists because the reference client has a person in it.** Reading unsaved editor state, running commands in the client's terminal, and showing a person a form or a URL are all optional and inert unless the client advertises them. One obligation cannot be declined: a permission request can arrive at any moment, and a client that does not answer leaves the agent waiting.

8. **The boundary this transport arrives at is already load-bearing and already shared.** Retry, reconciliation, run recording, and escalation consume a normalized event vocabulary and a normalized turn result, not the shape of an adapter. Turn disposition and the refusal posture are decided by a layer every existing integration already sits behind. A new transport therefore cannot lose a capability the orchestrator produces above it; it can only fail to deliver an input.

## Considered Options

- One registered adapter kind per agent, each pairing the protocol with one named runtime
- A single generic adapter kind, with the agent named by the command that launches it
- A transport selected inside each existing per-runtime kind
- Continuing without the protocol, writing one hand-written integration per runtime

## Decision Outcome

Chosen option: **a single generic adapter kind**, because the per-agent divergence the other two options exist to declare is a property of the build in front of the client that only observation settles, so declaring it per kind buys configuration surface without buying accuracy.

### One kind, and how the agent is identified

One registration, one integration, one set of tests. The agent is identified by the command that launches it, in the same place every other runtime's command is already given. There is no per-agent registration and no per-agent metadata. The name the kind is registered under spells the protocol out rather than using the abbreviation, which already means two other things here.

Nothing in the transport branches on the agent's identity. Where two agents behave differently, the difference is resolved by what the agent advertised and by what it was observed to do, never by which agent it is. A case that appears to need a branch on identity is evidence that the runtime was measured incompletely, not that the runtime is special. A dedicated kind for one agent remains available as a last resort and requires a recorded reason for why the difference could not be expressed as a capability state.

### Which runtimes move

A runtime moves onto the protocol when its protocol surface carries every load-bearing capability at least as well as its own structured native surface does, or when it has no structured native surface at all. Otherwise it stays where it is.

A capability is load-bearing when losing it changes a decision the orchestrator makes: turn disposition, retry classification, the token ceiling, tool-server delivery, and session continuation. A capability whose loss changes only a log line or an operator display is not load-bearing, and its absence does not block a move.

The rule applies to measured surfaces on both sides. An unmeasured surface is neither rich nor poor; it is unmeasured, and the runtime waits. This is not a formality. The rule is what keeps adoption from becoming a blanket, and a blanket contradicts the measurements. Applied today:

- Three of the project's registered runtimes speak the protocol natively and were measured doing so. Each of the three qualifies. On one of them the gain is direct and measured on both sides: its native token counts are recovered from a session journal only after the process exits and are unavailable over a remote worker connection, while the protocol delivers the same counts on the wire inside the session. On another the protocol is not worse on any load-bearing row and better on two, and its counters are absent on both sides, which is a property of the runtime rather than of the transport.
- One runtime that has no integration at all speaks the protocol natively. It qualifies, with token counting recorded as a declared gap, because it emits no usage notification and puts counts in a vendor extension instead.
- Two runtimes reach the protocol only through a separate bridge process maintained by a third party. Neither bridge was ever run, so neither is measured, and both stay on their hand-written integrations. For the more heavily used of the two the rule is not merely unproven but adverse: its native surface reports the four counters cleanly, the protocol's stable line reports none, and moving it would remove the token ceiling from the runtime that carries most of this project's work.

### Capability states are resolved by observation

The transport keeps a per-session record of this project's own capabilities, not of the protocol's. The list is the project's because the project's contract changes more slowly than the protocol does, and it is what the orchestrator was promised.

Each entry resolves in three stages, and each stage can only lower it. First, what the kind structurally cannot do, known before launch and visible to offline validation. Second, what the agent advertised in its handshake response, which is the protocol's only capability advertisement. What a session-creation response reports about available modes and configuration options is not a capability advertisement and never lowers an entry, and neither does an unsolicited notification that merely lists what the session currently offers. Third, what the agent was observed to do the first time the capability was used: a missing field, a refused method, or a continuation that replays nothing.

The second and third stages reach only those entries the protocol is expected to carry. Lowering such an entry withdraws the protocol as its source, because the protocol's own rule is that an omitted capability is unsupported; the entry then settles where it would have settled had the protocol never carried it, which is our own production for most of the list and a declared gap for the rest. An entry the shared layer already produces is never lowered by an advertisement the agent did not make, because the protocol was never its source. There is no separate absent state: absence of an advertisement is a transition, not a destination.

A lowering within a session is final for that session and is re-evaluated on the next one. Decisions read these entries as they stood when the turn began: a lowering observed during a turn takes effect from the following turn, so that retry classification, the token ceiling, tool delivery and continuation cannot change meaning underneath a turn already in flight. The turn that made the observation still ends on the evidence it actually saw.

Each entry ends in one of four states. **Protocol** means the agent advertised it and delivered it, and the value is read from the wire. **Ours** means the protocol does not carry it, or carries it in a form the contract cannot consume, and the shared layer produces it as it already does for every existing integration. **Gap** means nobody carries it and the contract wants it: the degradation is declared, the operator is told once for the session, and the decision that depends on it is switched off. **Refused** means the configuration contradicts the project's posture and the run does not start.

"Ours" is the most common state, and it is not a degradation. Turn disposition, the refusal posture, the turn counter, the wall-clock ceiling, and token accumulation are already produced above the transport for every runtime the project drives. "Gap" is a mode that already ships: one runtime runs in it today, the token ceiling does not fire, the incompleteness is recorded, and dispatch proceeds.

### Turn outcomes and retry classification

Every turn ends with one of five declared reasons, and the response that ends the turn cannot omit it. This is the clearest gain in the whole surface: the shared disposition rule receives the runtime's own verdict instead of reconstructing one from an exit code and residue, and an orchestrator-initiated cancellation returns as a completed response carrying a cancelled reason rather than as an error, which the existing cancelled outcome already consumes.

One of the five reasons carries a consequence the retry classification has to respect. The reason reporting that the agent declined to continue also means, by the protocol's own definition, that the prompt and everything after it are excluded from the next prompt. A retry after it resumes a different conversation than the one it believes it is resuming, so it is mapped to an error kind that says so rather than falling through to the default for an unrecognized kind, which is retryable with exponential backoff.

A sixth reason is possible even on the pinned version, because a later release may add one and a nonconformant agent may invent one, and the field is the one place where leniency would be unsafe: skipping it would leave a turn that never settles. It is therefore exempt from lenient parsing. An unrecognized reason ends the turn as an error rather than as a success, is not retried, because the condition that produced it is by definition unknown and a retry may repeat it without bound, and is recorded with the value the agent sent so that the gap is diagnosable rather than merely survivable. Reading an unknown reason as success is the one outcome this rule exists to prevent: it would report work as finished on evidence the client could not interpret.

### Where token counts come from

They come from this system, not from the protocol. The adapter accumulates the counters from the events it observes, exactly as the project's existing measuring integrations do, and computes the total itself rather than passing a runtime-supplied total through.

The protocol's stable usage notification is read as corroboration and never as a value, because it reports context occupancy rather than spend and decreases after compaction. The per-turn counters on the unstable line are read the same way and influence no decision: not disposition, not retry classification, not the budget. The self-contradiction in that type over whether its figures cover a turn or a session is precisely why it cannot be a value; a reader has to choose a side, and choosing wrong either double-counts or under-counts every turn. Where a runtime's own figures do not reconcile, and one measured runtime's input and output counts sum to neither its own reported total nor its context figure, the existing rule that the adapter computes the total is what prevents the discrepancy from reaching the budget.

Where a runtime reports no counters at all, the run is reported unmeasured. That is a supported, shipped mode and not a failure: the token ceiling does not fire, the incompleteness is recorded, and dispatch continues. A cache-write count that one measured runtime reports has no home in the contract and is discarded; that is already true of that runtime's native surface and this decision does not change it.

This leaves the cost budget of ADR-0013 untouched. Its unit, its storage, and its enforcement point are unchanged, and this record adds no budget surface. The money figure the protocol optionally attaches to its usage notification is not consumed, because a money unit was deliberately deferred by ADR-0013 and the protocol's figure would not close that deferral: it makes no normative claim of monotonicity, and one measured runtime reports its spend in a vendor credit that is not a currency at all.

### Session continuation is not a condition of support

A runtime is supported without it. Continuation is one capability entry among the rest, and its state is resolved by observation, never by the advertisement.

The evidence forbids anything weaker. One runtime advertised continuation and did not deliver it for 69 days; another advertised the identical capability and replayed the conversation correctly when a second process asked it to. The advertisement is therefore evidence in neither direction, while the protocol simultaneously forbids calling the method without it. The only sound reading is that the advertisement is a precondition for trying and observation is the verdict.

A probe carries a negative control. A deliberately invented method name is sent so the shape of "not implemented" is known for that build, because without it an unimplemented method and a broken one are indistinguishable. On one measured runtime this is what separated the two: the invented method and the continuation method both returned method-not-found, while a third call returned a meaningful internal error, which is what a request reaching real code looks like.

The existing preflight gate that names a configuration key blocking continuation is not extended to cover a structural inability to continue. It answers a different question, and the capability record answers this one.

### Permission requests

The policy is already settled by ADR-0025 and is not a property of the transport. When an agent asks for a decision only a person could give, Sortie refuses; the shared layer decides the posture and produces the outcome; an adapter contributes only recognition of the class and, where the runtime's protocol offers a reply channel, the refusal expressed in that protocol's own vocabulary. This record changes none of that.

What it settles is the mechanics on this wire. The protocol offers a real reply channel, so this becomes the second transport in the project with one, and a permission request is refused rather than left to end the attempt.

- The reply selects an offered option by its declared kind, never by its identifier. Identifiers are opaque strings the agent chooses; only the kind is a closed set, and only the kind is portable.
- The option list carries no uniqueness constraint, so a kind may appear on more than one option while the reply must name exactly one. Where several options share the selected kind, the first in the order the agent sent them is chosen, so that the same offer always produces the same reply.
- Two refusing kinds exist, one refusing the request in front of it and one asking the agent to remember the refusal. Where both are offered, the once-only kind is selected. The remembered kind would persist a decision inside the agent beyond the request that prompted it, while this project's posture is produced fresh for every request from a rule that is uniform and not operator-configurable; storing it in the agent would place state this project does not own ahead of the rule that produces it, and would outlive the session that created it.
- Where no option of a refusing kind is offered, the protocol provides no way to say no by selection. The request is answered with the cancelled outcome, the only other answer the pinned line defines, and the attempt then ends with the human-input-required outcome rather than proceeding blind.
- Where the option list is empty, which the pinned version permits and the draft successor forbids, the same answer and the same ending apply.
- The request is always answered. The pinned line admits exactly two outcomes, selecting an offered option or cancelling, and offers no third way to express refusal; leaving the call unanswered would strand the agent on a request that never returns.
- Two prominent implementations do the opposite, and neither is copied. The reference autonomous client selects the first option in the list; the widest measured consumer auto-approves when no handler is registered. Both fail open, and both are what a reader finds first when looking for an example.

The client advertises no capability for showing a person a form or opening a URL, so the agent must not request one and is required to fall back gracefully. Those obligations are unconditionally about a present person: the client must let a person review and modify a response before it is sent, must obtain consent before navigating anywhere, and must not prefetch. None of them can be honestly satisfied by an unattended orchestrator, so the capability is not advertised at all rather than answered badly.

### The pinned version, and what triggers a migration

The negotiated wire version is pinned at the integer 1, the released stable line. The draft successor is not adopted. A version the client cannot accept ends the session rather than continuing, because the protocol defines no error for a version mismatch and leaves the decision to disconnect entirely to the client.

The wire version and the schema release are different numbers, and conflating them is a known trap: the publisher versions its schema artifacts on their own line and states outright that wire compatibility must not be inferred from it. Validation and the hand-written types are therefore pinned to one named stable schema release, recorded with the tag and the commit it resolves to, and that pin moves deliberately rather than by following the latest publication. The release pinned at adoption is the stable schema artifact published under the tag schema-v1.21.0, which is an annotated tag resolving to commit 272bf799f35a258c6a4107a0410ed361e83683d3. It is the stable artifact, not the unstable one published beside it under the same tag, and its own metadata declares wire version 1. Naming the release alone would not identify it, because the tag and the wire version are different numbers and the stable and unstable artifacts ship together.

Later additions published against the same wire version are accepted on the wire and ignored by the client until the pin moves. Parsing stays lenient for everything that does not route a decision, so an unrecognized field or update variant is skipped rather than rejected; a client that refused them would reject agents that are already conformant, because every runtime measured for this decision sends something the pinned release does not describe. Treating such an addition as a source of truth requires moving the pin, which is its own change with its own evidence.

Two conditions together trigger reconsideration, and neither alone is enough:

1. The successor line carries a public stabilization commitment. None exists in any public artifact today.
2. The move does not cost a load-bearing capability. Today it would: the response that ends a turn stops ending it, and the outcome and its reason move to a notification that the client must consume even to learn how its own cancellation resolved.

Neither condition is a date, deliberately. A date would be a prediction, and the published guidance is to carry both versions side by side for an unspecified period rather than to migrate on a schedule.

Two related choices follow from the same evidence. The client is written against the wire rather than against a published language binding, because the wire held still for a year while the bindings around it did not, and every measured migration in that window was a binding migration. And remote transports stay out of scope: the protocol's own normative transport page lists the remote mechanism and leaves its section empty, so there is nothing an independent implementer can build against and expect to interoperate with, whatever first-party code ships alongside.

### Parsing posture

Messages are parsed leniently. An unknown key, an unknown update variant, and an unknown field value do not fail a message; the message is consumed and the unrecognized part is recorded as unparsed.

This is not tolerance for its own sake. The published schema carries a marker requiring default-on-error deserialization 249 times, so the protocol expects a forgiving reader. And a client that rejects unknown keys rejects three of the four runtimes measured live, each of which sends a field the current released stable schema does not contain.

The cost is named in the negatives below: a change that keeps every field and changes what a value means passes through unnoticed.

### What this decision does not settle

**Tool-server delivery over a remote worker connection.** Under the protocol, tool servers are re-expressed inside the session-creation request rather than handed over as a file path, and the protocol describes a server's launch command as an absolute path to an executable, which a remote worker resolves on the far side. One runtime today delivers tool servers over a remote connection under the pass-through model, and the protocol path reclassifies that delivery as translated. Whether the delivery survives is unknown. It is settled by one measurement of a protocol session on a remote worker with at least one server configured, and because tool-server delivery is load-bearing, that measurement gates moving that runtime rather than following it.

**Runtimes reachable only through a bridge.** Two are, and no bridge was ever run. They are out of scope until one is, and the question they raise is about installing and updating a third process rather than about the transport. The condition that settles it is a measured turn through a bridge covering the load-bearing rows, in particular whether the bridge forwards token counts at all.

**Whether the client is written here or taken from a community module.** Both routes exist and both carry real risk: the most-starred module trails the current stable surface and has been forked by its own largest consumer, while the only module pinned to the current stable schema, with a checksum lock, comes from an organization five days older than the module. Taking a dependency is a separate decision under the project's standing rule, and this record does not make it.

**Whether an event-channel operation and an unproduced rate-limit field survive in the adapter contract.** Both are surfaced by this work and neither is caused by it. The first is implemented as a no-op by every runtime and called by nothing; the second is declared, consumed all the way to a public observability field, and produced by no integration at all, so that field is always empty. The protocol cannot fill the second, and its absence there is a recorded design decision rather than an omission. Both deserve an explicit answer, and neither is answered here.

**A model identifier for protocol-driven runtimes.** It is empty for several runtimes today, and the protocol's stable line has no standard field for it: the field that carried it was removed, and the method that set it was withdrawn. One measured runtime supplies both anyway, off-schema. Filling a recorded field from data that exists in no published schema artifact is a decision, and it is not made here.

### Considered Options in Detail

**Continuing without the protocol.** Rejected because it holds the roster at its current width while leaving every future runtime to be reverse-engineered again. The cost it avoids is real but bounded, and the cost it keeps is unbounded: each integration parses a vendor format that changes on the vendor's schedule, and defects arrive by class because each integration solves the same problem in its own words. It is also the only option that forecloses a runtime whose protocol mode ships before any structured native output does, which has already happened once. It remains the correct answer for any runtime the eligibility rule below does not admit, so it is rejected as a general policy rather than as a per-runtime outcome.

**One registered adapter kind per agent.** Rejected because its central justification does not survive the measurements. The option exists so that per-agent differences can be declared where per-kind properties already live: tool-server delivery mode, the continuation gate, and configuration validation. None of the three carries the differences that actually exist. Tool-server delivery is identical for every agent on this protocol, because servers are always re-expressed into the session-creation request and the standard-input mechanism for them is mandatory for every agent on the pinned line, so one value is honest for all of them and splitting kinds gains nothing. The continuation gate names a configuration key that blocks continuation, not an ability to continue, and a generic kind has no such keys. What does differ between agents is capabilities, and capabilities cannot be declared before launch, because the advertisement can be false and because two builds of the same runtime differ. The option's certain cost is a doubled configuration surface for every runtime that already has an integration, plus a registration, a metadata declaration, an environment-gated test suite, and two documentation rows for each agent added.

**A transport selected inside each existing per-runtime kind.** Rejected on reach and on coherence. On reach: it is available only where the runtime has a native protocol mode, which is three of the project's runtimes; for the other two an inner transport means launching a different process, which is a different integration wearing the same name rather than a transport choice. It is also the only option that does not widen the roster at all, and roster width is half the problem this work exists to address. On coherence: two session models under one name leave the kind's operator-facing configuration describing one path and silent about the other. A runtime-specific pass-through value that becomes a launch flag on the native path means nothing on the protocol path, where refusal travels through the permission channel instead, and the validator that today refuses that value for contradicting the unattended posture would be inspecting an inert setting.

## Consequences

### Positive

- One integration reaches every agent that speaks the protocol natively, and the marginal cost of the next such agent is a measurement rather than a parser.
- Turn framing, turn outcomes, the update vocabulary, and tool-server delivery arrive normalized, and a mandatory outcome on every turn feeds the shared disposition rule directly rather than being reconstructed from each vendor's residue.
- The refusal posture gains a second real reply channel, so a permission request from a runtime on this transport is refused in a form the agent can act on rather than ending the attempt.
- Two things the project does not record today arrive for free: the runtime version as the agent itself reports it, and a cost figure the project chooses not to consume yet.
- The most common failure mode of consuming a moving protocol is bounded by construction: an agent that gains a capability and an agent that loses one both cost zero code, because both are a capability entry resolving differently.
- The wire version the project pins has not moved in a year, and the project depends on that rather than on a language binding that migrated roughly every three weeks over the same period.

### Negative

- **A generic kind cannot carry a per-runtime configuration verdict.** Five per-runtime validators today refuse a configuration that would re-open an interactive path; the generic kind has one validator covering every agent behind it. The compensating fact is narrower than the loss: on this transport the client owns the agent's standard input and answers permission requests itself, so the specific stall those validators exist to prevent cannot arise from the protocol's own surface. An argument an operator passes in the launch command still is not reviewed.
- **The roster does not consolidate.** Two runtimes, including the most heavily used, stay on hand-written integrations, so the project maintains two transports rather than one, and a change to shared behavior has to land in both.
- **A runtime with no counters has no token ceiling, and this decision lets that spread.** The mode already ships and one runtime already runs in it, so the risk is one of proportion: the more of the roster arrives through a transport whose stable line carries no spend counter, the more of the roster runs uncapped.
- **Lenient parsing cannot see a lie.** The capability record detects an absent field and a refused method. It does not detect a field that is present and whose meaning changed, which is exactly how one runtime's native surface broke twice inside a release that documented neither change. The only defenses are a gated conformance test that fails on a shape change, because production by design will not, and recording the runtime version so the question "since which build" has an answer after the fact.
- **Per-runtime knowledge does not disappear.** It moves out of a parser and into capability states resolved at runtime, and producing the initial states for a new agent is manual measurement: a turn that forces a tool call and a permission request, a continuation attempt from a second process, and a negative control, all recorded against a version and a date.
- **Half the protocol's client obligations are declined rather than met.** That is sound under the protocol's own rules, which require the agent not to request an unadvertised capability, but it means the project is a deliberately partial client, and an agent that behaves badly when refused is a category this project will meet before most clients do.
- **The successor line is a rewrite that will arrive without a date.** Carrying two versions side by side is the published guidance, and the project has committed to a trigger rather than a schedule, so the migration will be decided under time pressure created by someone else.
- **The client is not small, and code generation does not produce it.** Closing over the messages the transport needs reaches roughly three to five times the declared type surface of the project's existing persistent-session integration, most of it in unions the implementation language does not express directly. A general schema-to-type generator was tried against four releases of the published schema and produced no output at all on any of them, so the shape is written by hand whichever route the client itself takes.
- **Accepting this obliges edits to the agent adapter contract.** The contract's built-in-adapter enumeration, its session-model description, and its statement of how tool servers are delivered all describe a roster this decision widens. The edits are mechanical, and skipping them leaves the specification contradicting the code, which the project treats as a defect in both directions.

## Confirmation

The decision is validated when all of the following hold:

1. A runtime moves onto the protocol only with a recorded measurement of both its surfaces against the load-bearing list, and no runtime moves on an unmeasured surface.
2. No code path branches on the agent's identity. Differences resolve through an advertised capability or an observed message shape.
3. The client answers every permission request with no person involved, selecting by the option's declared kind and never by its identifier, selecting the once-only refusing kind when both refusing kinds are offered, and selecting the earliest option in the order the agent sent them when several share the selected kind. A request offering two options of the same kind is one of the cases confirmed. Where no refusing option is offered or the option list is empty, it answers with the cancelled outcome and then ends the attempt with the human-input-required outcome.
4. The client advertises no filesystem, terminal, or human-prompting capability, and no run waits on an unanswered request.
5. Token counters for a protocol-driven runtime are computed by the adapter from observed events, no runtime-supplied total is passed through, and a runtime that reports no counters is reported unmeasured with the token ceiling inactive and the incompleteness recorded.
6. A capability the agent advertised but did not deliver stops being read from the protocol for the rest of that session, settling into our own production or a declared gap, and is not raised again within it. A capability the shared layer produces is unaffected by anything the agent did or did not advertise.
7. Session continuation is absent from the conditions of support, and its state for a given session comes from a call with a negative control rather than from the advertisement.
8. The negotiated wire version is the integer 1, a version the client cannot accept ends the session, and validation runs against the stable artifact of the tagged schema release the pin records, identified by tag and commit, rather than against whatever is published latest.
9. An unknown key, an unknown update variant, and an unknown value do not fail a message, including additions published against the same wire version after the pinned release. The reason that ends a turn is excluded from this: an unrecognized one settles the turn as a non-retried error carrying the value received, and a parser exercised with a reason outside the pinned set confirms the turn still settles.
10. The runtime version as the agent reports it is recorded for every session, and one notification per session lists the capabilities resolved to a declared gap.
