---
status: accepted
date: 2026-08-08
decision-makers: Serghei Iakovlev
---

# Keep Usage Data on the Host and Aggregate Across Instances by Pull

## Context and Problem Statement

Sortie writes one row per agent run to `run_history` and reports aggregates from it locally. The
`sortie stats` subcommand opens the database read-only and emits a single document under
`--format json`, carrying a summary plus breakdowns by status, agent adapter, dispatch rule, and
prompt template, each with duration percentiles, token sums, and derived cost. Every process owns
its own database, so those figures describe one process and nothing beyond it.

A deployment that runs several independent Sortie processes, one per repository or one per team, had
no stated way to see figures across them. Separately, the project carried no contract governing data
leaving the machine at all. Nothing said whether Sortie transmits anything, to whom, under what
consent, or what an operator running it where outbound connections are audited may assume.

The two gaps had to be settled in one decision. Any mechanism that sends aggregates somewhere
answers the first question and creates the second, and a contract written for the second constrains
every available answer to the first. Settling either alone would have decided the other by accident.

## Decision Drivers

1. **The figures already have a defined shape, and a second shape would be a defect.** The
   `sortie stats --format json` envelope is a stable document: it declares the range it covers, the
   schema tier the database supports, the warnings that apply, and a null for every figure the tier
   or the data cannot supply. Any export that emitted a different set of figures would create two
   answers to the same question, differing in population, in rounding, and in what a null means.
2. **Cross-instance aggregation already exists, by pull.** Sortie serves a Prometheus endpoint at
   `GET /metrics` covering thirty-four metric families across dispatch, retries, reconciliation,
   poll cycles, tracker requests, tool calls, worker duration, token consumption, and every reaction
   kind, and a JSON HTTP API at `/api/v1/state`, `/api/v1/refresh`, and `/api/v1/{identifier}`. A
   shipped Grafana dashboard renders that surface across thirty-one panels. Prometheus attaches an
   `instance` and a `job` label to every series it scrapes, so one Prometheus pointed at several
   Sortie processes already holds every figure partitioned by instance and can aggregate across
   them. The problem to be solved was materially narrower than it first appeared.
3. **A destination outside the operator's organization creates an obligation that the same data
   inside it does not.** Dispatch rule names and prompt template identifiers are written by the
   operator in the workflow file and can carry internal project vocabulary. They are persisted in
   `run_history` as `rule_name` and `template_id`, they appear as the `by_rule` and `by_template`
   breakdowns of the stats envelope, and one of them is already a Prometheus label on
   `sortie_dispatch_rule_match_total{layer, rule}`. Inside the operator's own network none of that
   is remarkable, because the names originated there. Crossing an organizational boundary is the
   event that creates the obligation, and it does so without a single field changing.
4. **Participation in opt-in collection is governed by the design of the consent prompt, and an
   unattended process has no prompt to offer.** Sortie runs as a daemon and in CI. It cannot block
   on an interactive question. Any collection scheme it could implement is therefore restricted to
   the weakest end of the range that published measurement covers.
5. **The outbound posture must be establishable without reading the source.** An operator deciding
   whether to run Sortie inside an audited network needs a stated contract, not an inference drawn
   from the absence of an observed connection.
6. **What is settled here should not need re-arguing.** The conditions any outbound feature would
   have to satisfy are cheapest to fix now, while the arguments for and against transmitting are
   both in view, rather than inside a later proposal that has already chosen its direction.

## Considered Options

- No outbound reporting, with operators aggregating the JSON output of the local reporting command
  using their own tooling
- An opt-in export of Sortie's own aggregates to an operator-chosen endpoint, with no default
  destination and no project-operated receiver, so that the operator owns both ends
- An opt-in report of aggregates to a project-operated endpoint, for aggregate insight across
  installations
- External pull-based aggregation, in which a separate consumer polls each instance's existing HTTP
  surfaces and the orchestrator pushes nothing and is unchanged

## Decision Outcome

Chosen option: **no outbound reporting**, because the cross-instance question the outbound options
were reaching for is already answered by pull, and because a collection scheme that cannot prompt
cannot produce a sample that would support the decisions it would be collected to inform.

Two parts of the rejected options are kept rather than discarded. The export contract from the
second option is documented rather than built, so that an operator who wants to move figures off the
host has a defined path and any future built-in export has a shape it must match. The fourth option
is named as the shape that any future cross-instance aggregation takes. The third option is
rejected, on the terms set out below.

### The posture this establishes

Sortie transmits no usage data. The codebase contains no telemetry client, no analytics client, and
no update check. The only network destinations it reaches are the tracker, forge, and agent
endpoints its own configuration names, and the sole endpoint literals compiled into the binary are
the public defaults for those integrations. The HTTP server binds to `127.0.0.1` on port `7678` by
default, so even the local observability surfaces are reachable from elsewhere only when an operator
deliberately changes the bind address.

One boundary needs stating precisely, because it is easy to misread. Sortie launches a coding agent
as a subprocess and passes the ambient environment through to it. That agent is a separate program
with its own vendor relationship and its own telemetry posture, governed by the environment the
operator provides. Sortie sets no variable that enables it and no variable that disables it. The
claim made here is about Sortie, and it does not extend to the programs Sortie runs.

### The documented export path

An operator who wants figures off the host pipes the existing command:

```sh
sortie stats --format json --since 2026-07-01 --until 2026-08-01 \
  | curl -sS -X POST -H 'Content-Type: application/json' --data-binary @- \
      https://metrics.internal.example/sortie
```

No Sortie feature is involved. The command reads the database read-only, the operator chooses the
destination, the schedule, and the retention, and the credential belongs to the operator.

The contract this documents is a constraint on any export, whether built into Sortie later or
assembled by an operator now: an export emits the figures of the `sortie stats --format json`
envelope, not a second and divergent set. The envelope's own fields make the reason concrete. It
carries `schema_tier`, so a consumer can tell whether token and cost figures were available at all
rather than reading a null as a zero. It carries `warnings`, so a consumer can tell a clean
aggregate from a degraded one. A second figures document would either reproduce that structure or
silently drop it, and dropping it turns absent data into apparent data.

The envelope also carries `workflow_path` and `db_path`, which are local filesystem paths. Inside
the operator's own network that is useful provenance and identifies which process produced the
document. It is also the clearest illustration of the point below: the same field is unremarkable in
one destination and a disclosure in another.

### Cross-instance aggregation, and the non-goal it does not violate

The project's stated non-goals exclude Sortie being a multi-tenant control plane or a separately
deployed frontend application. Two readings of that had been conflated, and separating them is the
most consequential clarification recorded here.

The first reading is that the Sortie binary is not a multi-tenant server. It holds. A Sortie process
serves one workflow file, one database, and one tracker configuration, with no notion of a tenant
anywhere in its state. This decision preserves that reading without qualification.

The second reading is that cross-instance aggregation is therefore impossible. It is false, and the
metric surface described above is the proof: the figures are already exposed, already labelled per
instance by the scraper, and already aggregable. An external consumer that polls unmodified
single-tenant instances does not make any of those instances multi-tenant. Each one continues to
answer only for itself, on its own port, from its own database, with no knowledge that another
instance exists.

The direction is the load-bearing part, and it is recorded as a rule rather than a preference. The
orchestrator does not push to any aggregator. Aggregation, if built, pulls. A push design would put
a destination, a credential, a schedule, and a failure mode inside the orchestrator, and would make
every instance aware of something outside itself. A pull design puts all four outside, leaves the
orchestrator unchanged, and keeps the property that an instance exposing nothing is an instance
nothing can collect from.

### Why the destination decides, and not the payload

The second and third options carry identical data. They differ only in who receives it, and that
difference is the whole of the distinction between them.

A dispatch rule named for an internal programme, or a prompt template identifier naming an
unreleased component, is information the operator already holds about itself. Sending it to the
operator's own metrics store moves it from one system the operator runs to another. Sending it
across an organizational boundary discloses it to a party that did not have it, and creates an
obligation to state what is collected, to keep it, and to answer for it. The field is the same in
both cases. The obligation appears only in the second.

This is why an operator-owned export needs a documented contract and nothing more, while a
project-operated endpoint needs a redaction rule, a published inventory, and a consent mechanism
before it can send a single byte.

### Why a project-operated endpoint is rejected

The measured record on opt-in participation is consistent across three independent programmes, two
very different populations, and two very different collection mechanisms.

Mozilla recorded an opt-in telemetry rate of three percent in 2011, and attributed the higher rate
of a parallel programme of its own to that programme's interface: "Currently telemetry opt-in rate
is alarmingly low 3%.
Test pilot is closer to 10%, we suspect this is due to the better UI experience provided by test
pilot's door hanger" (https://bugzilla.mozilla.org/show_bug.cgi?id=675391, comment 0). The same year,
on the channels whose users are the most technical, a reply on the proposal to switch to opt-out
reported "<4% of users opt-in to telemetry on the nightly and aurora builds (we should expect less
on release builds)" (https://bugzilla.mozilla.org/show_bug.cgi?id=679431, comment 4). A later
attempt to track uptake across channels is instructive for a different reason: the analyst supplying
the figures cautioned that the quantity being measured was the proportion with the feature enabled
rather than the proportion who had chosen it, that figures reached by different methods within the
same discussion were not comparable, and that the limitation survives whatever the number turns out
to be, since "whatever be enabled rate, telemetry it is user opted in and hence (in the absence of
any other information) *biased*" (https://bugzilla.mozilla.org/show_bug.cgi?id=843807, comment 7).
After the switch to opt-out, Mozilla estimated that 93 percent of release channel profiles had
telemetry enabled (https://docs.telemetry.mozilla.org/datasets/pings.html).

Fedora reached the same conclusion from the other end, retiring a long-running opt-in hardware
census with the finding that the data did not answer the question it was gathered for: "as long as
it's an opt-in, we can't use the data to generalize about our installed base"
(https://fedoraproject.org/wiki/Smolt_retirement).

A controlled field study over more than eighty thousand unique visitors isolated the variable
directly, holding the data collected constant and changing only the notice
(https://arxiv.org/abs/1909.02638). Under privacy-by-default conditions, where a visitor had to
actively select each category or vendor, fewer than 0.1 percent consented to everything. Under
confirmation conditions the same consent reached 21.1 percent on desktop and 39.2 percent on mobile
when the accept control was a plain text link, and 26.9 percent on desktop and 50.8 percent on
mobile when the same control was highlighted. Nothing but the presentation differed.

The inference is the reason for the rejection. Across every one of these measurements, the single
lever that materially moves participation is the design of the consent prompt, and the largest step
of all is the one from opt-in to opt-out. Sortie runs unattended, as a daemon and in CI, and cannot
block on an interactive prompt. That constraint is deliberate and is retained. It forecloses the
lever, and the constraint recorded below that collection is never opt-out forecloses the other one
by choice rather than by necessity. Both are closed, and what remains is a configuration field in a
file, which is weaker than every condition any of these studies measured: each of those still put a
visible notice in front of every participant, and a configuration field puts nothing in front of
anyone.

What that leaves is a self-selected fraction. The objection is to the shape of that sample and not
to its size, and the distinction matters because it is the shape that cannot be fixed by waiting. A
self-selected fraction does not become representative by growing. Deployments that opt in would
differ systematically from those that do not, in exactly the dimensions such data would be gathered
to measure: which adapters are configured, which reaction kinds are enabled, how the workflow file
is written, how much the operator is willing to disclose. Decisions taken from it would be taken
from a population selected by its willingness to be measured.

This rejection is not a permanent one and is not made on ideological grounds. It rests on two
premises, and a future architect may revisit it when either changes. The first premise is that
Sortie cannot obtain consent in a form that moves participation, which would change if a consent
mechanism appears that is as legible as an interactive prompt without blocking an unattended
process, and if it is measured to move participation rather than assumed to. The second premise is
that the questions worth asking of such data require a representative sample, which would change if
a question is posed whose answer survives self-selection. Until one of those changes, the obligation
described above would be taken on in exchange for data that does not answer the question.

### Constraints on any future outbound feature

These bind any later proposal to transmit anything, and are recorded so they are not re-argued.

1. Collection is opt-in. It is never opt-out, and never on by default. This is the constraint that
   deliberately declines the lever the measurements above identify as the effective one.
2. Both `DO_NOT_TRACK=1` and a Sortie-specific environment variable disable it, the latter following
   the `SORTIE_` prefix every environment variable the binary reads already uses. Either alone is
   enough, and neither depends on the configuration file being reachable or valid.
3. An itemised inventory of what is collected and what is not is published before any release
   capable of transmitting, not alongside it and not after it.
4. Nothing blocks on an interactive prompt. A daemon and a CI job must reach a running state without
   a human answering a question.
5. Operator-authored strings are never transmitted verbatim off the operator's own infrastructure.
   Dispatch rule names, prompt template identifiers, workflow and database paths, workspace keys,
   and issue titles all fall under this. The rule constrains what crosses an organizational
   boundary, and does not constrain what the operator's own metrics store or log pipeline receives.

### Considered Options in Detail

**An opt-in export to an operator-chosen endpoint.** This is the option with the fewest objections,
and its contract is kept even though its mechanism is not. It has no default destination and no
project-operated receiver, so the obligation described above never arises: the operator owns the
sender, the receiver, the credential, and the retention. It was not built because the mechanism adds
nothing the shell already provides. The figures exist, the command that emits them exists, the
format is stable and self-describing, and the destination is one an operator must configure either
way. Building it into the binary would add a destination field, a credential field, a schedule, a
retry posture, a failure-reporting path, and a validation rule, all to replace a pipe. The part
of the option that carries real value is the contract, not the code, and the contract is recorded
here without either.

**An opt-in report to a project-operated endpoint.** This is the only option that answers a question
the others cannot: how Sortie behaves across deployments that have no relationship with each other.
That question is real, and no amount of operator-owned tooling answers it. It is rejected on the
evidence set out above. A scheme that cannot prompt cannot reach a participation rate at which the
responding population resembles the whole, and the resulting sample would be selected precisely on
the axis that any conclusion drawn from it would concern. The obligation created by crossing an
organizational boundary would be taken on in full, in exchange for data that does not support the
inference it exists to support. Both premises are named above, and both are revisitable.

**External pull-based aggregation.** This is not rejected. It is named as the shape any future
cross-instance aggregation takes, and it is recorded here rather than adopted because it requires
nothing of the orchestrator: the metric families, the scrape labels, and the JSON API it would read
are already in place and unchanged by this decision. The reason it belongs in this record is that
its existence is what narrows the first two options. Once it is clear that a pull-based consumer can
already produce a cross-deployment view from unmodified instances, an outbound push mechanism is no
longer the only way to answer the cross-instance question, and it has to justify itself on other
grounds. Nothing is said here about what such a consumer would be or who would build one.

## Consequences

### Positive

- The outbound posture is a single stated fact rather than an inference: Sortie transmits no usage
  data, and an operator can assert that in an audited environment without reading the source.
- The cross-instance question has an answer that requires no new mechanism, no new credential, and
  no change to the orchestrator.
- The multi-tenant non-goal is stated in a form that no longer blocks work it was never meant to
  block, while continuing to exclude what it was written to exclude.
- Any export, whether an operator's pipe today or a built-in feature later, has one figures document
  to match, so a second and divergent set of figures cannot appear by default.
- The constraints binding a future outbound feature are settled, so a later proposal argues its own
  merits rather than re-opening consent, disablement, disclosure, and redaction from the start.
- The direction of any future aggregation is fixed as pull, which keeps destinations, credentials,
  schedules, and their failure modes outside the orchestrator.

### Negative

- **The question a project-operated endpoint would answer stays unanswered.** How Sortie behaves
  across deployments that have no relationship with each other is a real question, and this decision
  declines the only mechanism that addresses it directly. That cost is accepted rather than
  dismissed.
- **The effective lever is deliberately not pulled.** The measurements cited above identify opt-out
  as the change that moves participation by an order of magnitude, and the recorded constraints
  forbid it. The rejection of a project-operated endpoint therefore rests partly on a restriction
  this decision imposes on itself, which a future architect may weigh differently.
- **Cross-instance aggregation is available rather than provided.** An operator gets it by running a
  Prometheus and pointing it at each instance. The shipped dashboard's queries carry no instance
  selector, so a multi-instance scrape renders one series per instance where a query is a bare
  selector and a single figure where it aggregates. Neither is wrong, and neither is a view designed
  for the multi-instance case.
- **The documented export path is an operator's responsibility end to end.** Scheduling, retries,
  authentication, and the consequences of piping a document containing local filesystem paths to a
  destination are all outside Sortie, so a mistake in any of them is invisible to it.
- **A contract with no implementation can drift.** The export contract is stated against the current
  `sortie stats --format json` envelope, and nothing mechanically enforces that a later change to
  that envelope is considered against this contract.
- **The specification wording is now more precise than the document that carries it.** The stated
  non-goals still read in the form that admits the conflated interpretation, and reconciling that
  wording is separate work not performed by this decision.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The goals and non-goals material: the multi-tenant non-goal restated so that it excludes the
   Sortie binary being a multi-tenant server without also appearing to exclude an external consumer
   polling unmodified single-tenant instances.
2. The security and operational safety material: the outbound-data posture as a stated property,
   the set of network destinations a running instance reaches, and the boundary between Sortie's own
   posture and that of the coding agent it launches.
3. The logging, status, and observability material: that a single scraper collecting from several
   instances is the supported cross-instance view, and that the orchestrator pushes to no
   aggregator.
4. The operator-facing reference documentation: the documented export path, the contract that any
   export emits the figures of the stats envelope, and the constraints binding a future outbound
   feature.

## Confirmation

The decision is validated when all of the following hold:

1. An instance run with outbound network access restricted to the tracker, forge, and agent
   endpoints its own configuration names completes a poll cycle, a dispatch, and a `sortie stats`
   invocation with no attempted connection to any other destination.
2. A search of the codebase finds no telemetry client, no analytics client, and no update check, and
   no compiled-in endpoint literal other than the public defaults of the configured integrations.
3. `sortie stats --format json` remains the only document Sortie produces that carries aggregate
   run figures, and no second figures document exists with a different population or a different
   null convention.
4. A single Prometheus scraping several instances holds every `sortie` metric family partitioned by
   the `instance` label and can aggregate across them, with no change to any instance.
5. No orchestrator code path sends run data, aggregate or per-run, to any destination the operator's
   configuration does not name.
6. Setting `DO_NOT_TRACK=1` is a documented, honoured disablement for any outbound feature that is
   ever added, and remains honoured when the configuration file is absent or invalid.
7. The stats envelope's operator-authored fields, including the dispatch rule and prompt template
   breakdowns and the workflow and database paths, are emitted only to destinations the operator
   chose.
