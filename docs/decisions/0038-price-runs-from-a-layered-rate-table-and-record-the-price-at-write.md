---
status: accepted
date: 2026-10-18
decision-makers: Serghei Iakovlev
---

# Price Runs from a Layered Rate Table and Record the Price When the Run Is Written

## Context and Problem Statement

Every spend ceiling Sortie enforces counts sessions or tokens. Operators budget in money, and before this decision they converted a dollar figure into tokens by hand. Sortie already showed a money figure: it priced every measured run in USD from the operator's `token_rates` for the dashboard, the JSON API, and `sortie stats`. Nothing could enforce that figure, and the way it was produced could not carry a ceiling.

The pricing code lived in `internal/server`, which imports the orchestrator, so the orchestrator could not reach it without an import cycle. The daemon parsed `token_rates` only when the HTTP server was enabled, so with the server off nothing was priced. The orchestrator kept its own partial reading of `token_rates` for a preflight warning, which counted an incomplete entry as priced while the server counted it as unpriced. Rates were keyed by agent kind, although models of one kind differ in price several times over, and no run recorded its tokens per model. History was priced afresh on every read with the rates the process started with, so a restart with new rates re-priced past runs, and `token_rates` took a restart to change while every other ceiling reloaded live. An unset `cache_read_per_mtok` priced cache reads at `input_per_mtok`, which for a provider that discounts cache reads overstated a cache-heavy run many times over.

A per-issue money ceiling and an instance-wide daily and monthly money ceiling both need one answer to the same questions: where pricing lives, where rates come from, at what granularity, what an unpriceable run contributes, when a run's price is fixed, how rates reload, what the `cost_budget` sidecar reads, where an instance-wide window starts, how cache reads are priced, and what the figure means. ADR-0013 deferred a money budget to this decision because a per-model price table either goes stale between releases or needs an outbound call. This decision settles that tradeoff.

## Decision Drivers

1. **Nobody should hand-maintain a price list.** Model prices change on the provider's schedule, and an orchestrator that requires the operator to type in every rate for every model leaves most deployments unpriced. Price data that tools in this space share is published openly, updated daily, and licensed for reuse.
2. **Sortie ships as one statically-linked binary that runs offline.** Pricing must work with no network access and must not add a Go dependency or a C toolchain. A failed or refused download never blocks startup or leaves a deployment unpriced.
3. **One figure for every agent kind.** A ceiling that sums one runtime's own dollar estimate with Sortie's estimate for another kind adds numbers produced under different price lists. Every kind Sortie drives reports tokens; only some report money.
4. **Money tracks the model, not the kind.** One kind runs models whose prices differ several times over, and a dispatch rule that routes cheap work to a cheap model saves money only if the price follows the model.
5. **A recorded spend must not move by itself.** An instance-wide hold that starts or ends because a restart, an upgrade, or a refreshed table re-priced past runs is a defect, and so is a dashboard whose history totals change when nothing ran.
6. **The advisory and the enforcing surface report the same figure.** The agent's `cost_budget` reading and the orchestrator's refusal must not disagree after an edit made without a restart.
7. **No upgrade changes an operator's figures.** A deployment that already prices with per-kind `token_rates` must see the same costs after this decision ships.
8. **Not knowing a price never becomes zero silently.** A run Sortie cannot price is visible as such wherever its cost is shown or summed.

## Considered Options

- Operator-supplied `token_rates` as the only rate source, keyed by agent kind
- A price table embedded in each release, with no operator override
- A price table fetched at runtime as the only source
- The cost figure some agent runtimes report on their own stream
- Layered rates with the fetched table off by default
- Layered rates: the operator's rates over a fetched table over a table embedded in each release, priced per model and recorded when the run is written

## Decision Outcome

Chosen option: **Layered rates: the operator's rates over a fetched table over a table embedded in each release, priced per model and recorded when the run is written**, because it prices every deployment without manual rate entry, keeps prices current between releases, and still prices offline from the embedded table, gives every kind one figure from one rule, and fixes each run's price once so no later change of rates moves a recorded spend.

### Where pricing lives

Pricing moves out of `internal/server` into its own package, named for pricing, which the HTTP server, the orchestrator, `cmd/sortie`, and `sortie mcp-server` all import. The package holds rate parsing, the table layers, model-name matching, and the estimate. The orchestrator's partial reading of `token_rates` is removed in favor of it, so one rule decides what is priced. Rates are parsed at every startup and every workflow load, whether or not the HTTP server is enabled. The move is a refactor done before either money ceiling is built, so both ceilings are built on pricing they can already reach.

### Where rates come from

Rates come from these layers, highest precedence first:

1. **The operator's rates.** Entries in `token_rates` take precedence over every table. An operator can also name a table of their own, by file (`price_table.replace_file`) or by address (`price_table.replace_url`), that replaces the fetched and embedded tables entirely, and `token_rates` entries then lie over that table as they lie over the shared one. A table named by file replaces them from its first load, because a file that cannot be read fails the load. A table named by address replaces them only once a download of it has passed validation; until then the fetched and embedded tables keep pricing runs. An operator table uses the same format as the shared table, so one parser reads both.
2. **A table fetched at runtime.** Fetching is on by default (`price_table.fetch` defaults to `true`). The address, `price_table.fetch_url`, defaults to the published LiteLLM `model_prices_and_context_window.json`.
3. **A table embedded in the binary** (`price_table.embedded`, default `true`). The release build embeds a snapshot of the LiteLLM `model_prices_and_context_window.json` at a pinned upstream revision through `go:embed`, after validating it. The file is MIT-licensed outside the upstream `enterprise/` directory, and its copyright and license notice ships with the release archives.

The operator can independently disable the embedded table, disable fetching with `price_table.fetch: false` or `SORTIE_PRICE_TABLE_FETCH`, and substitute a table of their own.

A fetched table is accepted only when it parses against the expected schema and is no smaller than a minimum size, so a truncated or empty response never replaces good data. An entry whose price falls to zero against the copy it would replace is discarded and the previous entry kept. The last table that passed validation is stored on disk beside the database. A failed fetch keeps that copy in force, or, when no copy exists yet and the embedded table is enabled, the embedded table, and never blocks startup. The fetch is a plain download of a public file: it transmits no run data, no configuration, and no operator-authored string. It is controlled only by `price_table.fetch` and its environment override `SORTIE_PRICE_TABLE_FETCH`, like every other setting. The constraints ADR-0019 sets on outbound features concern transmitting usage data and do not apply to it.

Sortie maps each model name an agent runtime reports or a run is configured with onto a table key. Runtimes and tables spell the same model differently, so the pricing package owns that mapping. A name that maps to no key has no rate.

The LiteLLM file wins as the shared source because it has the widest circle of consumers among tools that price agent runs, it is updated daily, it carries separate cache-read and cache-write rates for most chat models, and reading it needs no new dependency.

### Rate granularity

Runs are priced per model. The adapters carry token counts per model through to the run record, which needs a schema change. An agent whose stream reports a per-model breakdown has each model's counts attributed to that model. Counts no model claims go to the model current when they arrive: the one the run's events named last (`reported_model`), else `configured_model`. Counts that arrive before a run's events name any model stay under the configured model and do not move when a model is named later.

An existing per-kind entry such as `token_rates.claude-code` keeps precedence over every table and prices every model of that kind alike, so an upgrade changes no figure. It keeps its old rule that an unset cache-read or cache-write rate prices that class at `input_per_mtok`. `sortie validate` warns that such an entry disables per-model pricing for its kind. A per-model operator entry for a model that appears in some table may set any subset of rates and inherits the rest from the table; one for a model that appears in no table should set all of `input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`, and `cache_write_per_mtok`. `sortie validate` warns about such an entry that leaves a rate unset rather than refusing it, because a key that named no agent kind was accepted and ignored before per-model entries existed, and refusing it would stop a deployment on upgrade. The entry prices the classes it sets, and a run that spends an unset class is recorded `partial`.

### Price at write

When a run row is written, the orchestrator records its estimated cost, and for each model's share of it the price source (operator, fetched, or embedded) and the version of the rates that priced it: the pinned upstream revision of the embedded table, a digest of a fetched or operator table's content, or a digest of `token_rates` for a share an operator entry priced. A share whose operator entry takes some of its rates from a table records both versions, the `token_rates` digest joined to the table's version by `+`, so both inputs of its price stay auditable. History is no longer priced at read. The dashboard, the JSON API, `GET /api/v1/stats`, and `sortie stats` all read the recorded figures, so for finished runs the command and the route can no longer disagree after an edit. A running session's figure is the orchestrator's live estimate under the rates in force, and the row written at the session's end fixes it.

Rows written before this change carry no recorded cost. The first start that applies the schema change prices them once, under the rates in force at that start, on the run's reported model, else its configured model, and records the result with its source and version. They never change after that.

### Reloading rates

Because a recorded figure never changes, `token_rates` reloads live like every other ceiling. New rates apply to the live estimate of running sessions and to every run written after the reload. A fetched table is fetched at startup and refreshed once every 24 hours; a new table likewise affects only runs written after it lands.

### Runs that cannot be priced

A run that spent tokens on a model no layer prices, or whose tokens include a class its rate entry leaves unset, is recorded with the `unpriced` or `partial` cost status. A money ceiling sums the priced spend as a lower bound, lets dispatch proceed, and warns. A configuration under which the ceiling can never see spend is warned about, never refused: `sortie validate` and the running service warn when an agent kind a money ceiling applies to reports no token usage under that configuration, and when no rate source is available at all (the embedded table disabled, fetching off, and no operator rates or table). Dispatch continues under either configuration, because a pricing gap must not stop an operator's work, and the operator who reads the warning decides whether to fix the configuration.

### Rates in the `cost_budget` sidecar

The `cost_budget` tool reads the cost the orchestrator recorded: the recorded cost of the issue's `run_history` rows plus the running session's estimate, which the orchestrator writes to `session_metadata` on the same throttled cadence as its token figures. The sidecar never reads rates from WORKFLOW.md and never prices anything, so its reading and the orchestrator's refusal share one source.

### Window boundaries

The instance-wide daily and monthly ceilings use UTC calendar days and months. A configured time zone would need time zone data on every platform Sortie ships for, Windows included. The dashboard's "today" figures start at midnight in the browser's time zone, so for any viewer not on UTC the dashboard's today total and the daily ceiling's window cover different spans, offset by the viewer's UTC offset.

### Cache reads

A money ceiling prices prompt-cache reads at their own rate. Cache reads cost money even though they no longer count toward `agent.max_tokens`. The shared table carries separate cache-read and cache-write rates, and new pricing code never substitutes the input rate for a missing one: a cache class with no rate leaves the run `partial`. The input-rate fallback survives only for the per-kind entries kept for compatibility.

### Estimate, not bill

A money ceiling bounds Sortie's estimate, computed from the input, output, cache-read, and cache-write rates per model, not the provider's bill. The rates cannot express long-context price tiers (they apply per request above a token threshold, and Sortie records totals per run, so a tier cannot be applied even when a table lists it), the separate price of one-hour cache writes, batch, flex, and priority service tiers including `service_tier`, data-residency surcharges, negotiated discounts, subscription plans under which spend is not tokens times a list price, and fees for tools such as web search. A ceiling can therefore be reached before or after the bill reaches the same amount.

### Considered Options in Detail

**Operator-supplied per-kind `token_rates` only.** This keeps Sortie free of any price data, but it leaves every operator who does not type in rates unpriced and prices every model of a kind alike, so a money ceiling would be wrong by the price ratio between a kind's models. It survives as the top layer, not as the only one.

**An embedded table with no override.** It works offline and needs no configuration, but prices go stale between releases, and an operator with a new model or a negotiated rate could not correct a figure that a ceiling enforces. It survives as the bottom layer.

**A fetched table as the only source.** It is the freshest, but it makes pricing depend on the network, which an offline or audited deployment refuses. It survives as the middle layer, with the embedded table beneath it so a failed download still prices.

**The layered option with fetching off by default.** It keeps a default install from contacting any address it was not configured with, but no studied tool that can fetch a price table ships that default, and it leaves prices as old as the release for every operator who never touches the setting. An operator who needs no outbound contact turns fetching off with one setting.

**The runtime's own cost figure.** Claude Code reports `total_cost_usd`, OpenCode reports `cost` on each step, and the Agent Client Protocol defines a cost on `usage_update` that a given agent may or may not send. Each figure follows that runtime's price list rather than the operator's rates, and the other kinds report none. Using it where present would mix two price lists in one ceiling and make the same work cost differently by kind. Sortie computes every figure by one rule and does not read these fields for pricing. The Claude Code `max_budget_usd` setting that Sortie passes to the CLI remains that runtime's own per-process cap and is unrelated to Sortie's ceilings.

**models.dev as the shared source.** It covers more models, but it is roughly twice the size of the LiteLLM file and its consumers are mostly one runtime. It is a valid operator table only if converted to the shared format.

**The genai-prices package.** Its schema is the richest (price tiers, effective dates, matching rules in the data), and it ships a Go package with embedded data. It is rejected because it adds a Go dependency for what is one JSON file, and it was still at version 0.1.x.

**Pricing inside the orchestrator, read by the server.** It removes the import cycle but puts table loading, fetching, and the release snapshot into the orchestrator, and `sortie stats`, the sidecar, and the server's configuration report would still need the same parser outside it.

**Pricing kept in the server, injected into the orchestrator as a function.** It keeps parsing tied to the HTTP server's lifetime, leaves `sortie stats` and the sidecar without a shared home for the rules, and keeps a second partial reading alive in the orchestrator.

**Per-kind rates with an override keyed by configured model.** `configured_model` is often empty or an alias, and a run's real model is the reported one, so an override keyed on configuration prices the wrong model whenever the runtime picks one.

**Price at read.** It needs no schema change but lets a restart, an upgrade, or a table refresh move recorded spend and start or end an instance-wide hold.

**Holding an unpriceable issue, or refusing a money ceiling the configuration cannot enforce.** A new model released after the binary would then stop work or fail validation until the operator adds a line, and a kind that reports no token usage, or a deployment with every rate source off, would stop all dispatch. A lower bound with a warning keeps work moving; no pricing gap stops work.

**Rates passed to the sidecar.** The sidecar would then price the running session itself and could still disagree with the orchestrator whenever a reload lands mid-session.

**A configured time zone for the windows.** It matches a team's working day but needs time zone data on every platform and still differs from a dashboard viewer in another zone.

## Consequences

### Positive

- Every deployment prices runs with no rate entered by hand, with current prices by default and with embedded prices when offline, and a money ceiling becomes buildable on pricing the orchestrator can reach.
- A recorded cost never moves, so restarts, upgrades, and table refreshes cannot start or end a hold, and `token_rates` reloads live.
- Every kind is priced by one rule, per model, so routing work to a cheaper model shows in money as well as in tokens.
- `sortie stats` and `GET /api/v1/stats` read the same recorded figures of finished runs and no longer disagree after an edit.
- The agent's `cost_budget` reading and the orchestrator's refusal share one recorded source.

### Negative

- **Sortie can make an outbound call for its own bookkeeping.** ADR-0013 ruled such calls out, and ADR-0019 stated that the only compiled-in endpoint literals are the public defaults of configured integrations. This decision revisits both on that point: the default address of the price file becomes a compiled-in endpoint literal that is not an integration default, and it changes ADR-0019's statement that Sortie reaches only the endpoints its configuration names: a default install contacts the price-file address. An operator in an audited network must set `price_table.fetch: false` or `SORTIE_PRICE_TABLE_FETCH`.
- **Schema changes.** Run rows gain per-model token counts and a recorded cost with source and table version, `session_metadata` gains the running estimate, and pre-change rows are priced once at the first start after the change.
- **A downloaded table is trusted on its host, not on its content.** Validation rejects a malformed, truncated, or zeroed table, but a table with false yet plausible rates is accepted, and a money ceiling enforces on it. The default address follows the upstream branch rather than a pinned revision, so freshness is bought with that trust; an operator who needs reviewed prices pins `price_table.fetch_url` to a revision or a mirror, uses `price_table.replace_file`, or turns fetching off.
- **Each release carries price-data upkeep.** A scheduled pull request refreshes and validates the snapshot, each release embeds the snapshot its source holds, and the embedded table is as old as the last merged refresh. Between releases the fetched table keeps prices current; an operator who turned fetching off fixes a new model with one line of `token_rates`.
- **Model-name matching is ongoing work.** Each runtime spells models its own way, and a new spelling prices nothing until the mapping learns it.
- **The figure is an estimate.** The factors listed above make a ceiling diverge from the bill, and an operator on a subscription plan sees a money figure that does not describe what they pay.
- **Per-kind entries freeze pricing for their kind.** An operator who keeps one gets the old behavior, including the input-rate fallback for cache reads, until they remove it; `sortie validate` says so.

## Confirmation

The decision is satisfied when all of the following hold:

1. One package, imported by the server, the orchestrator, `cmd/sortie`, and `sortie mcp-server`, holds every pricing rule, no other package reads `token_rates` rate values, and rates are parsed with the HTTP server disabled.
2. With no network access and no `token_rates`, a run of a model present in the embedded table records a cost with source `embedded` and the snapshot's revision.
3. With no `price_table` settings, Sortie downloads the table from `price_table.fetch_url`'s default address; a response that fails the schema, falls below the minimum size, or drops an entry's price to zero leaves the last good copy, else the embedded table, in force; an unreachable address does not delay startup; and `price_table.fetch: false` or `SORTIE_PRICE_TABLE_FETCH` set to false makes no request.
4. A per-kind `token_rates` entry yields the same cost for the same usage as before the change, and `sortie validate` warns that it disables per-model pricing for its kind.
5. Editing `token_rates` without a restart changes the cost of runs written afterward and leaves every recorded cost unchanged.
6. `cost_budget` reports a cost equal to the recorded run costs plus the running estimate in `session_metadata`, and reads no rates.
7. A run with cache reads on a model whose entry has no cache-read rate is recorded `partial`, never priced at the input rate, unless a per-kind entry prices it.
8. `sortie validate` and the service warn, and dispatch continues, when a money ceiling applies to a kind that reports no token usage, or when no rate source is available.
