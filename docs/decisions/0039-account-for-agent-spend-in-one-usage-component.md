---
status: accepted
date: 2026-10-27
decision-makers: Serghei Iakovlev
---

# Account for Agent Spend in One Usage Component Fed by Each Runtime's Own Reports

## Context and Problem Statement

The token ceiling, the money ceilings, the per-model cost shares, and the `cost_budget` tool all act on one figure: what a run spent. Before this decision, shared helpers held the accumulator and the completeness rule, but each agent adapter chose the channel its figure came from, mapped that channel's fields, excluded a resumed session's history by a mechanism of its own, and marked for itself when a turn was settled. Remote sessions fared unevenly. The `copilot-cli` kind and the generic `agent-client-protocol` kind declared a remote session unmeasured, because their figures came from files on the runtime's host that nothing read over SSH, while the `opencode` kind read its figure on the remote host through the runtime's own export command.

The generic protocol kind carried the widest gap. The protocol's stable line carries no token counters, so a run on that kind was measured only where a reader outside the protocol applied to its runtime. The one such reader, for Gemini CLI, read the runtime's telemetry, which Sortie switched on through the runtime's environment, with the runtime's chat journal as a backstop. Every other runtime on that kind ran unmeasured: `agent.max_tokens` did not fire for it, and the money ceilings left it out. The larger the share of the roster that arrives through that kind, the larger the share that runs under no spend ceiling.

The runtimes do report spend, through channels the stable line does not describe, and one field name means different things across them. In the releases current when this was decided:

- Gemini CLI and OpenCode return a per-turn count on the protocol's prompt response, while Copilot CLI returns a count cumulative over its process that starts again at zero in a new process.
- Claude Code's stream reports totals cumulative over the session, restored on resume from a record the runtime writes only when its process exits, so a killed process's spend drops out of the next total. Codex's app server repeats the previous turn's snapshot when a thread resumes, and Kiro CLI replays earlier turn reports when a session loads.
- Gemini CLI, Copilot CLI, and Codex count cached tokens inside input, OpenCode counts them outside it, and Claude Code reports cache reads and cache writes as classes apart from input. Gemini CLI's protocol total leaves reasoning tokens out, and its journal's total includes them.
- Gemini CLI returns no usage for a cancelled turn, and a killed process reports nothing on any channel. Codex's rollout file and Gemini CLI's chat journal keep the requests that completed before a kill, and Copilot CLI's session journal keeps the records it had already written. Telemetry from a locally killed Gemini CLI process lost every record it had not flushed.
- Kiro CLI reports spend only in credits, and Copilot CLI reports AI credits and premium requests beside its token counts.
- Claude Code and Codex also report provider usage windows, the quotas a subscription or plan meters over a span such as five hours or a week, on the channels that carry their spend. Claude Code names a window by an identifier, states its used share as a fraction, and says in words of its own how near the limit the window is; Codex gives a window's length in minutes and its share in percent. A bridge that carries Claude Code over the protocol forwards the same report in a vendor member of the protocol's usage notification. The adapter contract held such reports only as an opaque payload, which no adapter produced.
- Copilot CLI, OpenCode, Gemini CLI, and Kiro CLI carry no usage window on their live channels, and each answers for its windows on request. Copilot CLI calls a GitHub endpoint, with the token it runs on, that reports the share of each quota left and when the quotas reset, and in its headless mode answers a quota request of its own from a cached copy of that report. OpenCode, signed in to its console, answers a command of its own with the share used of each window of its subscription and when each resets, and reports a free usage limit in its stream with the delay until it resets. Gemini CLI calls an endpoint, with its Google sign-in, that reports the share of each model's quota left and when it resets. Kiro CLI answers an extension request on the session with the credits used against its plan's limit and the date its billing cycle resets.

This record settles where every kind's spend figure comes from, what normalizes it, how money is derived from it, how a reader and a budget tell figures of different origin and completeness apart, which ceilings act on a run that carries no figure, and how all of it is verified locally and over SSH.

## Decision Drivers

1. **Accounting rules have one owner.** Converting cumulative values, normalizing cache and reasoning, discarding repeats, rebasing after a reset, and judging completeness are the same work on every kind. Spread across adapters, they drifted: resumed history was excluded by different mechanisms, journal readers kept their own size limits, and the remote exemptions were repeated in several places and kept in step only by comments and tests.
2. **Every session of every runtime is measured, local or remote.** Whatever a runtime writes to its standard streams reaches Sortie unchanged over SSH, and a record on the runtime's host needs a read on that host. A kind admitted without measurement, or measured only where it runs locally, leaves its runs under no token or money ceiling for a reason no operator chose.
3. **A field's name does not carry its meaning.** One formula over shared field names prices cache twice on some runtimes and drops it on others. Only semantics declared per source and checked against exact figures are safe to apply.
4. **Interrupted work is billed.** The completed requests of a cancelled or killed turn cost money whether or not a live report covers them, and the runtime's records on disk are the channel that keeps them.
5. **Sortie does not switch on agent telemetry.** ADR-0019 states that Sortie sets no variable that enables a launched agent's telemetry. Telemetry also travels outside the session's standard streams, to a file or a network collector, and a killed process loses whatever it had not flushed.
6. **One money figure compares across agents.** Operator rates and negotiated discounts apply only to a figure Sortie computes from its own price tables, and only such a figure means the same on every kind. Yet a model those tables cannot price left a run with no money figure even when the runtime had stated a dollar amount.
7. **Pricing needs the served model.** A price table rates a model, so a turn's spend is priced only where the model that served each request is known. Copilot CLI's `auto` selection picks a model per request and bills each request at the served model's rate, yet in its protocol mode nothing on the live channel names the served model: the session's model selector reports `auto`, and only its per-request journal records name the model. The `copilot-cli` kind already attributed spend per served model from that journal at every turn end.
8. **A credit is neither a dollar nor a token.** A unit the runtime's vendor defines has no rate Sortie can vouch for, and adding it to dollars or tokens corrupts both.
9. **A figure states its origin.** A reader and a budget must tell a live figure from one read off disk, a price-table amount from a reported one, and a complete figure from a lower bound.
10. **Accounting is proven on exact numbers.** The fake-model bench scripts the usage of every response, and the SSH lab runs the same runtime on a remote host. Before this decision the remote variant of the conformance run asserted nothing about usage.

## Considered Options

- Keep the generic kind's declared gap and bound its runs only with ceilings that do not count spend
- Derive a spend figure from the context occupancy the protocol's stable line carries, published as derived
- Keep a reader per runtime inside each adapter, telemetry included, and add remote reads adapter by adapter
- Use agent telemetry as the source for every kind
- Use the amount the runtime reports as the primary money figure
- One usage component fed by each runtime's live reports and, for turns they leave unsettled, by its records on disk, with money priced from Sortie's tables

## Decision Outcome

Chosen option: **One usage component fed by each runtime's live reports and, for turns they leave unsettled, by its records on disk, with money priced from Sortie's tables**, because it measures every kind by one set of rules locally and over SSH, keeps those rules in one place that one contract proves, and needs no telemetry.

### The usage component

A package of its own, named for usage accounting and placed in the agent adapter layer, is the only code that converts a cumulative figure to a delta, applies a source's scope, normalizes cache and reasoning tokens, discards replayed and repeated figures, detects a counter reset, decides whether a figure is complete or a lower bound, and records provenance. An adapter MUST NOT do any of these itself. The component knows no runtime's format: each source, with its declaration and its parser, lives with the adapter whose runtime it describes. An adapter hands the component what the runtime said, tagged with its source; the component hands the orchestrator the run-level and per-model figures the orchestrator already consumes, each carrying provenance. The orchestrator, the domain model, and persistence name no runtime and no source format.

The component produces one form for every kind. Input counts every input token, cache reads and cache writes included. Output counts every output token, reasoning included. The total is input plus output, computed by the component and never taken from a runtime, because runtimes disagree about what their own totals contain. This is the form the token budget of ADR-0013 already assumes.

### Sources declare their semantics

A source is one channel through which a runtime reports spend. Every source declares:

- whether it reports deltas or cumulative values;
- its scope: per request, turn, process, or session;
- whether cache tokens sit inside its input count;
- whether reasoning tokens sit inside its output count;
- which frames a session load or resume replays;
- how a repeated figure is recognized;
- which message settles a turn;
- for records on disk, whether a request's record is written when the request completes or only when the turn ends;
- the unit of any amount it reports.

The component applies the declaration and MUST NOT read a field the declaration does not map. Each message and each record is checked against its source's mapping on its own: one that lacks a field the mapping needs yields an unknown amount, never zero. A runtime's version never decides how its spend is counted: Sortie MUST NOT trust or discard a build's figures, or choose which of its sources to read, because of its version. Whether Sortie supports a runtime at all is a separate decision this rule does not govern.

A cumulative value becomes a delta against the previous value from the same source within its scope. A value below the previous one is a reset and MUST NOT be recorded as negative spend: what was counted before it stays counted, and the new value counts only from the point the source's scope says the counter restarted. Where the component cannot establish that point, the turn's figure is a lower bound.

The context occupancy the protocol's usage notification carries (`used` and `size`) measures how full the context window is, not what was spent, and enters no figure.

### Every kind and every runtime declaration registers a live source

A live source is the runtime's own output or its protocol messages. Both arrive on the session's standard streams, so a live source works the same locally and over SSH. A live source that reports spend only in a vendor unit, such as credits, is a live source. Every kind, and every runtime declaration under the generic protocol kind, MUST register with a live source that works for a local and a remote session alike, and registration MUST refuse a kind or a declaration without one. No reason, declared or otherwise, takes the place of a live source.

Every source a kind attaches works remotely: the live source on the session's standard streams, and records on disk through the one shared remote-read operation. Whether a figure arrives is therefore a property of the kind, known at dispatch without the runtime's identity, and the same for a local and a remote session. Per-kind remote exemptions do not exist. The usage arrival and attribution the orchestrator reads for a kind follow from the sources it attached rather than being declared beside them. This follows established practice for plugin systems: the container storage interface holds that a plugin's capabilities are the same wherever it is deployed, and a registry refuses an incomplete entry rather than offering an opt-out.

The generic protocol kind attaches the protocol messages as its live source. What those messages mean differs by runtime, so declarations exist per runtime, at the seam ADR-0029 admitted for measurement readers. The message pump, the domain model, and the orchestrator MUST NOT branch on the runtime. A field read this way, whether on the protocol's stable line, on its unstable line, or in a vendor extension, is read as that runtime's output under its declaration, independent of the schema pin.

A declaration is selected from the name the runtime gives in its handshake. Where something must know the runtime before the handshake, its identity is resolved before launch from the configured agent command, never from the SSH executable that launches that command on a remote host, or from the name an earlier handshake of the same launch setup reported, and the handshake confirms it. When the handshake names a different runtime, the declaration the handshake names is selected and the mismatch is reported for that session.

A runtime declaration MAY carry a launch addition for working sessions that turns off a request the runtime makes on its own and that no source records. OpenCode is the case: it generates a session title through a model request whose tokens appear in no per-request record, so a turn that issues that request can only be a lower bound. The addition is selected with the declaration, at the same seam and before launch, from the identity resolved from the configured agent command, and the handshake confirms it. It applies identically to a local launch and to a launch over SSH, where it travels with the session's environment preamble. It MUST merge into any value the operator set for the same setting, for OpenCode `OPENCODE_CONFIG_CONTENT`, and MUST NOT replace or drop the operator's value. With the addition in effect, the turn that would have issued the unrecorded request is measured as complete. The addition MUST NOT change the credential, model, provider, or endpoint the runtime resolves.

A runtime on the generic protocol kind that has no declaration of its own is measured under a default declaration, which reads the protocol's standard usage fields. The protocol has not settled whether those counts are per turn or cumulative, so the default declaration reads them as cumulative over the runtime's process under the reset rule above, where a falling reading counts in full. That reading is exact when the runtime reports cumulative counts and never more than the true spend when it reports per-turn counts, so a figure read under the default declaration is always a lower bound and never complete. A runtime that reports no usage fields leaves its turns with spend that occurred and an unknown amount, as any source that reports nothing does.

### Records on disk settle what the live source did not

The fallback source is the runtime's own records on disk. The component reads them to settle a turn the live source left unsettled: a cancelled turn, a killed process, a missing final report. For such a turn the records' figure replaces whatever the live source reported for it, and the two MUST NOT be added together. When the records hold no figure for the turn, whatever the live source reported for it stands as a lower bound. Records written only when the turn ends cannot settle a turn whose process was killed or ended by its cancellation: that turn's spend occurred and its amount is unknown, never zero.

A source may declare that its live reports name no model, or name only the selector the session requested, such as `auto`, while its records name the model that served each request. For such a source the component also reads the records at the end of a turn the live source settled, solely to attribute that turn's amount to the models that served it. That read never changes an amount: the live amount stands, the records only split it by model, the figure's strategy stays live, and the two figures MUST NOT be added together. A read that fails, and records that hold nothing for the turn, leave the turn's amount settled and unsplit, never unknown. Without this read a run on such a selector carries no model a table can price.

The same read also returns a provider usage-limit refusal the runtime recorded for the turn, with the reset time the record carries. It supplies that reset only to a refusal the live channel already reported under the same name, and never changes an amount. A read that fails, or records that hold no refusal for the turn, leave the refusal as the live channel reported it.

A local session reads its records directly. A remote session MUST read them through one shared remote-read operation built on the SSH launch the session itself used: the same host, options, and environment preamble, running a read-only command with every argument quoted for the remote shell. The operation resolves the runtime's home on the remote host, never from the local host's environment, and applies to the bytes it receives the same size limit a local read applies to a file. A kind MUST NOT build its own remote read.

In a turn the records settle, a record that does not parse yields an unknown amount, never zero. A turn that issued a model request no figure covers stays a lower bound, as before this decision. The fallback does not depend on what the protocol carries: a process that dies leaves its last turn unsettled however complete the protocol's counters become, so this source does not retire when the protocol gains counters.

### Telemetry is not a source

No source reads telemetry, and Sortie MUST NOT set a variable that enables a launched agent's telemetry. The Gemini CLI telemetry reader is removed: Gemini CLI's completed turns are settled by the counts on its prompt response, and its cancelled and killed turns by its chat journal. This restores the statement in ADR-0019 that the reader contradicted.

### Provider usage windows

A provider usage window is a quota a provider meters over a span of time, such as five hours or a week, and enforces by refusing the account's requests until the window resets. The usage component is the only code that normalizes a window report, builds a window's display name, and merges window reports. An adapter MUST NOT do any of these itself.

A source whose reports carry windows also declares the unit of the used share, its status vocabulary, and its window vocabulary: each window identifier it uses, with the window's length and the model or usage pool the window covers. The component reads no window field the declaration does not map. It produces one form for every kind: the window's identifier as the source reported it, a display name built from the window's length and scope, the used share in percent, a status of allowed, warning, or exhausted, the reset time, and the time the report was observed. A share above the whole window is kept as reported. A share the source declares no unit for, a status word outside its vocabulary, and a share, status, or reset the report does not carry are unknown: never zero, and never under the limit.

Window reports merge per agent kind. A report replaces the held report of each window it carries unless the held one was observed later, and leaves every window it does not carry in place, so a source that reports one window at a time and a source that reports only changed windows merge alike. A window whose reset time has passed is no longer current. A window is not spend and enters no token or money figure. The generic protocol kind reads a runtime's windows under the same per-runtime declaration that maps its spend.

A window source is live or pull, and declares which. A live source is the runtime's own output or its protocol messages. A pull source is a request the adapter makes when a turn ends: to the runtime itself, through a command of its own or an extension request on the session's own connection, or to the endpoint the runtime itself calls, with the credential the runtime itself uses. The usage component runs every pull under one policy. A pull runs when the runtime ends a turn that reached it, whether the turn succeeded or failed, and never for a turn Sortie cancelled, a turn stopped because the runtime gave no first event in time or output that could not be read or broke the protocol, or a credential-verification session. It is bounded like an auxiliary launch of the runtime, and its windows are reported before the turn ends. A pull that fails reports no window and leaves every held window in place, and once the runtime cannot answer a pull in a session, the session pulls no more. A pull asks about the account and sets nothing that enables the runtime's telemetry, so it is not telemetry.

Sortie reads a credential for a pull only where the runtime reads it, in the environment Sortie launches the runtime with or in the runtime's own files on its host, and sends it only to the endpoint the runtime sends it to. It MUST NOT store, log, refresh, or rewrite that credential. A remote session pulls on the remote host: an extension request travels on the session's own connection, a command of the runtime runs through the session's own SSH launch, and a credential file is read through the shared remote-read operation, after which an endpoint request leaves from Sortie's host.

A runtime's declaration also names how the runtime reports a provider usage-limit refusal, from members the runtime itself sets and never from message wording, and whether the turn-end pull's windows may supply its reset. When they may and the refusal carries no reset of its own, the earliest reset among the windows the pull reports exhausted is the refusal's reset; where a window names a model and a model is configured, only that model's windows count. A reset the refusal or its records carry outranks every window.

A source may report the share of a window left rather than used, and declares which. A window may measure its use in a vendor unit, such as credits: it then carries the amount used and the limit in that unit, which the source declares for each window it reports, and the amount is never converted. Its used share is the ratio of the two when the source reports none.

### Money

The money figure comes from Sortie's own price tables, the layered rates of ADR-0038, the same for every kind, so operator rates and negotiated discounts apply and ceilings compare across agents.

When no table layer can price a model, the amount the runtime reported for that model's spend is used instead and marked agent-reported, and only when the amount is in US dollars. A reported amount belongs to a model when its source attributes it to that model, or when the run used that model alone; an amount that cannot be attributed leaves the share unpriced. A reported amount of zero for spend that carried tokens is no amount. A cumulative reported amount becomes a delta per source under the reset rule above. A child session's amount MUST NOT be added to its parent's, because a parent's cumulative amount may already include its children's work. A share the tables price only in part stays `partial`: the fallback covers only a model the tables cannot price at all.

The run record keeps both figures side by side: Sortie's figure and, whenever the runtime reported one, the reported amount in its unit.

Under a subscription login every dollar figure, Sortie's or the runtime's, is a list-price equivalent, or an operator-rate equivalent where `token_rates` sets the rate, and never a charge; a money ceiling bounds that equivalent. Sortie does not detect the login type, so the meaning holds for every figure.

### Vendor units

An amount in any unit other than US dollars, such as Kiro CLI's credits or Copilot CLI's AI credits and premium requests, is recorded and shown in its own unit. It MUST NOT be converted to dollars or tokens or added to a token or money figure, and no ceiling reads it. A token field such a runtime leaves at zero is no figure: Kiro CLI's records carry token fields that hold zero whatever it spent.

### Ceilings for a run without a token or money figure

A run whose spend is known only in vendor units, and a run with no figure at all, add nothing to the totals the token and money ceilings compare. Those totals then become lower bounds, the run counts among the unmeasured runs, and dispatch proceeds.

| Ceiling | Run with a token figure | Run with spend only in vendor units | Run with no figure |
| --- | --- | --- | --- |
| `agent.max_sessions` | Applies, except to a run that a provider usage limit refused with a future reset time, when its figure records no output token | Applies | Applies |
| `agent.max_turns` | Applies | Applies | Applies |
| `agent.turn_timeout_ms` and `agent.stall_timeout_ms` | Apply | Apply | Apply |
| `agent.max_consecutive_absences` | Applies | Applies | Applies |
| `agent.max_tokens` | Counts the figure | Counts nothing | Counts nothing |
| `agent.max_cost_usd`, `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, and `agent.max_monthly_cost_usd` | Count the money figure | Count nothing | Count nothing |

### Pricing classes

Pricing keeps its classes: fresh input (input less cache reads and cache writes), cache read, cache write, and output, reasoning included. Each source's cache declaration places every cache token in exactly one class, so no cache token is priced twice.

The embedded price table carries one cache-write rate per model and no separate rate for cache writes with a longer lifetime, and Sortie reads one cache-write rate per model from every table and from `token_rates`. A long-lived cache write, which Claude Code reports apart from short-lived ones, counts in the cache-write class and is priced at that class's rate. At list prices a long-lived write costs more than a short-lived one, so the money figure understates that part of a run that writes long-lived cache; ADR-0038 already lists it among the limits of the estimate. No class is added, and the share is not marked `partial` for it, because a class no table prices would turn every such healthy run into a lower bound.

### Provenance

The run record MUST persist, for its token figure, every source that contributed, the strategy each used (live, or records on disk), and the figure's completeness: complete, lower bound, or unmeasured. Each per-model cost share MUST persist whether the price table or a reported amount priced it, beside the price source and table version ADR-0038 already records. Source names come from a closed vocabulary of vendor-neutral categories that the usage component owns, so persistence and the orchestrator stay free of runtime names. The JSON API, the dashboard, and `sortie stats` show a run's provenance wherever they show that run's spend, and mark an aggregate that includes a lower bound or an unmeasured run as a lower bound.

The token and money ceilings sum complete figures, lower bounds, and agent-reported amounts alike, and report a total as a lower bound whenever any part of it is one or any run in it is unmeasured.

### Verification

One conformance contract runs for every kind that launches a runtime, on the fake-model bench and in the SSH lab, locally and over SSH. For each scripted scenario it asserts that the figure equals, per class and exactly, the scripted usage of the requests the bench served, and that the provenance names the expected source and strategy. The scenarios cover a completed turn, a turn with a tool call, a resumed session, a cancelled turn, and a killed turn, so the fallback is proven wherever it applies. Where a source's records are written only when the turn ends, the killed turn's expected figure is an unknown amount, never zero. The generic protocol kind runs the contract for each runtime that has a declaration and that the bench can serve, and for the default declaration, where the expected figure is the scripted usage marked a lower bound.

### What this supersedes

- From ADR-0029's account of where token counts come from:
  - the rule that the protocol's usage notification and its unstable per-turn counters are corroboration that influences no decision;
  - the conditions on a measurement source outside the protocol, of which per-record validation, selection at one seam, failing closed, and unmeasured spend as a lower bound survive in this record, while the conformance contract takes the place of the one-off recorded experiment;
  - Gemini CLI's telemetry as a confirmed source;
  - the retirement of such a source once the protocol carries counters;
  - the discarding of a reported cache-write count;
  - the refusal to consume the protocol's cost figure;
  - for fields a declared source reads, the requirement that treating a later protocol addition as a source of truth moves the schema pin.

  Where ADR-0029 says the adapter computes the total, the usage component now does. ADR-0029 left open whether the adapter contract keeps an unproduced rate-limit field; it does not, and provider usage windows take its place. A pull declaration and a launch addition for working sessions at the per-runtime seam, each selected like any runtime declaration, supersede the limit ADR-0029 puts on per-runtime exceptions at that seam: the measurement reader and the credential-verification addition are no longer the only exceptions admitted there, and a launch addition no longer applies only to the verification session. The rest of ADR-0029 stands.
- From ADR-0038, the rejection of the runtime's own cost figure, and the rule that a share no layer prices contributes nothing, both of which now give way to an attributable reported amount in US dollars for a model the tables cannot price. The layered tables, the price fixed when a run is written, and the window rules stand.
- ADR-0013's budget unit and ADR-0036's attribution of spend to models are unchanged.

### Considered Options in Detail

**Keeping the declared gap.** It costs nothing to build, and the session, turn, time, and absence ceilings still bound every run. But the runtimes on the generic kind report spend that this option leaves unread, and the share of the roster without a token or money ceiling grows with every runtime that arrives on the protocol.

**Deriving a figure from context occupancy.** Occupancy is the size of the context, not what was billed: it falls after compaction, says nothing about how many requests a turn made, and does not separate cache from fresh input. A figure derived from it is a guess that a budget would act on, however clearly it is labeled.

**A reader per runtime inside each adapter.** This was the state before this decision, and its cost is the drift the first driver describes. Every new reader decides remote handling, resets, and completeness again, and the telemetry one reader relied on contradicts ADR-0019.

**Telemetry for every kind.** It needs Sortie to switch on a launched agent's telemetry, which ADR-0019 rules out. Over SSH it needs a collector the remote runtime can reach outside the session's standard streams, and a killed process loses its unflushed records. Copilot CLI writes no telemetry in its protocol mode, and Codex reports a dollar cost only through its telemetry exporter, after a poll that runs every 150 seconds.

**The reported amount as the primary money figure.** It follows each runtime's own price list rather than the operator's rates, Gemini CLI and Codex report no amount on their live channels, and Copilot CLI and Kiro CLI report units that are not dollars. It survives as the fallback for a model the tables cannot price.

## Consequences

### Positive

- Every session of every kind is measured by one set of rules, locally and over SSH, and a run on the generic protocol kind carries token and money figures whenever its runtime reports usage, under its own declaration or the default one.
- The completed requests of a cancelled or killed turn are counted wherever the runtime recorded them.
- The accounting rules exist once, and one contract proves them on exact figures on both sides of an SSH connection.
- Sortie switches on no agent telemetry.
- A run whose runtime picks its model per request is priced at the models that served it, even where the live channel names only the requested selector.
- A model the tables cannot price no longer erases a run's money figure when the runtime reported one in dollars.
- A reader and a budget see where every figure came from and whether it is complete.

### Negative

- **Records on disk are undocumented formats that change between releases.** Copilot CLI's session journal gained a per-request usage record between releases. A change that breaks parsing yields unknown amounts and lower bounds, or an unsplit amount where the records only attribute a settled turn, but a change that keeps a record parseable while changing what a field means undercounts silently until the gated contract or an operator notices.
- **A turn killed before the runtime wrote its record stays a lower bound.** A Claude Code process killed immediately after a request can leave no journal entry for it, and Copilot CLI can lose the record of a request in flight at a kill or completed just before it.
- **Declarations for the generic kind are manual work.** Each runtime's semantics are measured and declared by hand. A runtime without a declaration of its own is measured under the default declaration, whose figure is only a lower bound, and undercounts a runtime that reports per-turn counts.
- **A kind without a live source cannot be added.** A runtime whose spend reaches Sortie only through records on disk, or only on its local host, is refused at registration until it has a live source.
- **A reported amount follows the runtime's price list.** A money total that includes one mixes Sortie's rates with that runtime's prices for the models Sortie cannot price, and provenance names every such share.
- **Vendor units stay outside every token and money ceiling.** Only the session, turn, time, and absence ceilings bound a run that spends only in credits.
- **Long-lived cache writes are understated.** They are priced at the cache-write rate for as long as no table carries a rate for them.
- **A remote read of records costs an SSH connection.** Each unsettled turn of a remote session and every turn end of a remote session whose source names the served model only in its records opens a connection with its own handshake. Sortie never reuses a connection the operator's SSH configuration keeps open.
- **Persistence gains provenance.** Run records and per-model shares gain provenance and the reported amount; rows written earlier carry none and show their provenance as unknown.
- **The bench cannot script an amount a vendor's service computes.** Kiro CLI's credits come from its vendor's service, so the contract cannot verify them.
- **A pulled window costs a request when every turn ends, and can trail the work.** The turn ends after the pull, within its bound. A remote pull that runs a command or reads a file opens an SSH connection. A vendor's figure can lag the work it counts by minutes, and a runtime that answers from a cached copy reports the window as of that copy.

## Confirmation

The decision is satisfied when all of the following hold:

1. No adapter package converts a cumulative figure to a delta, normalizes cache or reasoning tokens, discards a replayed or repeated figure, detects a counter reset, or decides completeness; a search finds these operations only in the usage component.
2. Registering a kind, or a runtime declaration under the generic protocol kind, that attaches no live source fails, and no registration accepts a reason in its place. A test that enumerates every registered kind and every declaration, the default declaration included, finds each with a live source and finds the same sources attached under a local and a remote launch.
3. Every source carries a declaration of each property listed under "Sources declare their semantics", and a message or record that lacks a field its mapping needs contributes no amount and leaves its turn unsettled. No code path decides by a runtime's version whether to read its output or its records.
4. A remote session of every kind reads its records through the shared remote-read operation over its own SSH launch, and no code path exempts a kind or a runtime from measurement because its session is remote.
5. No code path sets a variable that enables a launched agent's telemetry, and no source reads telemetry.
6. A cumulative series that falls is never recorded as negative spend, and the spend counted before the fall stays counted.
7. A run on a model no table layer prices records the runtime's attributable US-dollar amount as that share's cost, marked agent-reported; an amount in any other unit leaves the share unpriced and is recorded in its own unit; a child session's amount is never added to its parent's.
8. A run whose spend is only in vendor units, and a run with no figure, add nothing to the totals `agent.max_tokens` and the money ceilings compare, make those totals lower bounds, and remain bounded by `agent.max_sessions`, `agent.max_turns`, `agent.turn_timeout_ms`, `agent.stall_timeout_ms`, and `agent.max_consecutive_absences`.
9. A long-lived cache write is priced at the cache-write rate and does not mark its share `partial`.
10. Every run record carries its token figure's sources, strategies, and completeness, and every priced share carries whether the price table or a reported amount priced it.
11. Every kind that launches a runtime passes the conformance contract locally and in the SSH lab, with the exact scripted figure and the expected source and strategy, for completed, tool-calling, resumed, cancelled, and killed turns. A source whose records are written only when the turn ends passes the killed turn with an unknown amount, never zero. The default declaration passes it with the scripted figure marked a lower bound.
12. A run that a provider usage limit refused with a future reset time, and whose token figure records no output token, does not count toward `agent.max_sessions`. Every other run a provider usage limit refused counts, including one whose spend is known only in vendor units and one with no figure.
13. No adapter package normalizes a window report, builds a window's display name, or merges window reports; a search finds these operations only in the usage component. A share, status, or reset that a report does not carry is unknown wherever the window is shown, never zero.
14. Every window source declares whether it is live or pull. A pull runs only when a turn that reached the runtime ends, never for a cancelled turn or a credential-verification session; it reads a credential only from the environment Sortie launches the runtime with or from the runtime's own files on its host, sends it only to the endpoint the runtime itself calls, and never stores, logs, refreshes, or rewrites it. A remote session's pull reaches the remote host only through the session's own connection, its own SSH launch, or the shared remote-read operation.
15. A turn the live source settled reads records only for a source that declares its live reports name no served model, or only the requested selector, and whose records name the served model. That read splits the live amount by served model and changes no amount, the records' figure is never added to the live one, and a failed or missing read leaves the turn's amount settled and unsplit, never unknown.
16. A runtime on the generic protocol kind with no declaration of its own reads the protocol's standard usage fields under the default declaration as cumulative values under the reset rule, and its figure is marked a lower bound, never complete. A runtime that reports no usage fields leaves its turns with an unknown amount, never zero.
17. A declaration is selected from the runtime's handshake name. An identity resolved before the handshake comes from the configured agent command, never from the SSH executable, or from an earlier handshake of the same launch setup; when the handshake names a different runtime, the handshake's declaration is selected and the mismatch is reported for that session.
