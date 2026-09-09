# Add or refresh an agent adapter

This guide is the ordered procedure for two situations: adding a new agent adapter kind, and refreshing an existing adapter after its upstream runtime changed. It assumes the mental model in [Agent adapter architecture: concepts](agent-adapter-concepts.md); read that first if a step below does not make sense on its own.

## Add a new agent adapter kind

1. **Create the package.** Add `internal/agent/<name>`, one package per kind. Give it a package comment naming the external system and the `domain.AgentAdapter` interface it implements, per the project's Go documentation conventions.

2. **Implement `domain.AgentAdapter`.** Implement `StartSession`, `RunTurn`, and `StopSession` against `domain.Session`, `domain.StartSessionParams`, `domain.RunTurnParams`, and `domain.TurnResult`. Parse the runtime's actual wire format inside this package only, and normalize every result to `domain.AgentEvent`, `domain.TurnResult`, and `domain.AgentError` before returning it. Add a compile-time assertion, `var _ domain.AgentAdapter = (*YourAdapter)(nil)`, next to the type definition.

3. **Reuse shared helpers instead of writing your own.** Check `internal/agent/agentcore` (session, event, and disposition helpers, including binary resolution via `agentcore.ResolveBinary`), `internal/agent/procutil` (subprocess group handling and graceful shutdown), `internal/agent/mcpconfig` (MCP configuration parsing), `internal/agent/sshutil` (SSH invocation), `internal/agent/jsonrpc` (newline-delimited JSON-RPC framing, for a persistent-session protocol), and `internal/agent/agenttest` (shared conformance assertions every adapter's own tests call, including `agenttest.AssertMCPInjection` and `agenttest.AssertUsageReporting`) before writing an equivalent. A helper this task needs that does not exist yet belongs in a new package named for the concern it serves, not folded into `internal/domain` or duplicated per adapter.

4. **Register the kind in `init()`.** Call `registry.Agents.RegisterWithMeta` (or the bare `Register` when the adapter needs no declared metadata) with the adapter's kind string and constructor:

   ```go
   func init() {
       registry.Agents.RegisterWithMeta("your-kind", NewYourAdapter, registry.AgentMeta{
           RequiresCommand:     true,
           ValidateAgentConfig: validateConfig,
           MCPInjection:        registry.MCPInjectionSupported, // or Translated, or Unsupported
           UsageArrival:        registry.UsageArrivalIncremental, // or TurnEnd, or None
           UsageAttribution:    registry.UsageAttributionPerModel, // or SessionTotal, or None
       })
   }
   ```

   Set `MCPInjection`, `UsageArrival`, and `UsageAttribution` to what the adapter actually does today, not what the underlying CLI could in principle support. Derive `UsageArrival` and `UsageAttribution` from the adapter's own emission code, not from the CLI's documentation: re-read the symbol that decides when a `token_usage` event fires and whether it carries a model before writing the literal. Add a `UsageSessionRules` entry only when some passthrough setting or launch mode narrows the pair for part of this kind's configuration space, and add `SessionResumeBlockedBy` only if some config key of this adapter's own can block session resume under a given passthrough.

5. **Blank-import the package from `cmd/sortie`.** Add `_ "github.com/sortie-ai/sortie/internal/agent/<name>"` to the import block in `cmd/sortie/main.go`, alongside the existing kind packages. This is the only place a kind package is imported outside its own tests; nothing else needs to change to make the kind resolvable through `registry.Agents.Get`.

6. **Add an env-gated integration test.** Gate the adapter's live-runtime test behind `SORTIE_<ADAPTER>_TEST=1`, one gate for the whole package. Skip cleanly, with `t.Skip` and a message naming the variable, when it is unset or not `1`. Do not add a second gate for a specific operation inside the same package. Run the shared conformance helpers against the adapter's own event stream: `agenttest.AssertMCPInjection`, `agenttest.AssertUsageContract`, `agenttest.AssertMeasurementAbsent` where the adapter reports no token usage, and `agenttest.AssertUsageReporting` for the adapter's own usage disposition.

7. **Run the contract tests.** `make test` runs `go test -race ./...`, which includes `internal/adaptertest`. Its checks catch a cross-adapter import, an orchestrator import of the new package, a vendor name leaking outside the new package's own directory (rule IDENTITY), and a duplicated helper the ban table already names an owner for. Fix a violation there before moving on; it is enforced independently of whether the code otherwise compiles and runs.

8. **Write the adapter notes file.** Add `docs/<kind>-adapter-notes.md`: the runtime fact everything else follows from, how to recover a volatile detail from the installed binary, and any exit-code or output behavior the adapter works around. Keep it prose the next maintainer reads before touching the code, not a log of what changed and when.

## Refresh an existing adapter after the upstream runtime changed

1. **Re-run the adapter's own integration suite against the new binary.** Set the adapter's `SORTIE_<ADAPTER>_TEST=1` gate and whatever command or credential variables its suite reads, and run it against the updated runtime. A test asserting the shape of what the runtime sends (handshake fields, a terminal event's own member names) is the one class of failure that distinguishes a real behavior change from a fixture that merely still parses; do not treat a pass as clean without that assertion actually exercising the new binary's output.

2. **If the runtime is reached through the generic `agent-client-protocol` kind, re-measure before changing anything.** Update the runtime's tracked profile document under `internal/qualification/profiles/<runtime-id>.json` to reflect the change: an entry point's argument vector, a recognizer's terminal-locating fields, a newly declared gap, or a newly declared absent surface. Drive a fresh live run against the updated runtime through `TestQualificationProfile`, the single gated entry point in `internal/qualification/probe`, to collect a fresh set of `qualification.Record` evidence rows, one row per surface-capability-case observation. It gates on `SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST=1` the same way an adapter's integration test gates, and takes the runtime binary, the model identifier, the authentication variable names, and the profile document to measure against from the four `SORTIE_CLIENTPROTOCOL_QUALIFICATION_*` coordinates beside it. A misspelled gate name skips the run and reports success, so read the skip line rather than the exit status when a run produces nothing. Feed those records and the updated profile to `qualification.ExplainEligibility` and read the resulting `Verdict`: `qualified` only when every load-bearing capability's protocol grade still meets or beats its richest native reference, `not_qualified` when one now falls short, and `unmeasured` when a row this run needed went unobserved. A verdict change from `qualified` is the signal that a runtime change moved a capability the orchestrator's decisions depend on, not merely a cosmetic output difference.

3. **Update the adapter notes file to match the fresh measurement.** Rewrite it in the present tense to describe current behavior; do not append a dated changelog entry describing what changed. Where the runtime is qualification-profiled, the notes document's required sections, its one `Eligibility:` line, and its per-surface-capability status rows must match the new `EligibilityReport` exactly. Validate the rewritten document against the fresh expectation with `qualification.ValidateNotes` before treating the refresh as done; a mismatched grade row or a missing section is a defect in the notes, not an acceptable rounding of the measurement.

4. **Record the fresh measurement artifact.** Where the runtime carries a tracked measurement document, replace it with one built from this run's `Verdict` and `NotesExpectation`, bound to the profile document's own digest (`RuntimeProfile.Digest`) so a later edit to the profile without a fresh run is detectable rather than silently treated as still current.
