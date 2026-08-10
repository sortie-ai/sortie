---
status: accepted
date: 2026-08-10
decision-makers: Serghei Iakovlev
---

# Withhold the Handoff Transition Only When Absence of Work Is Observed

## Context and Problem Statement

The orchestrator advances a tracker issue to its configured handoff state when a worker exits
normally. That transition is a claim about the run. It moves the issue out of the active queue, so
the orchestrator will not pick it up again, and it tells the operator's system of record that
something is ready for a human to look at.

The conditions governing the transition described the run's exit and not its output. A handoff state
had to be configured, the issue had to still be in an active tracker state, the exit had to be
something other than a soft stop reporting the agent blocked, and the dispatch posture had to be one
that drives issue state. None of the four inspected the workspace, a diff, a turn count, a tool call,
or any token counter. A worker that exited cleanly having produced nothing took exactly the same
path as one that did the work, and the two were indistinguishable in every artifact the run left
behind.

The specification stated the same four conditions for the same disposition. Code and specification
agreed, which is what made this a design question rather than a conformance defect: there was
nothing to bring back into line, only a position to take.

The resulting failure was silent in every channel an operator watches. No warning was logged, the
run recorded the same terminal status a productive run records, the same metrics were emitted, and
the issue's arrival in the handoff state looked exactly like every other completion. The only
observer positioned to notice was the human who opened the issue expecting to review something and
found nothing there.

The post-run workspace hook could not substitute for the missing check. It runs inside the worker,
before the exit disposition is computed, and its failure is logged and ignored by contract. A hook
that observed an empty workspace could say so and change nothing. Ordering was not the obstacle,
since the hook's exit status is available before the disposition is decided, so a design that gave
the hook a vote was on the table. It would have amended a contract that had been in the
specification since its first commit and whose rationale was recorded nowhere.

A partial mitigation was already in place and is instructive about what it does not reach. Two of
the five agent adapters reclassify an exit-zero turn that produced no output tokens as a failed
turn, and only in the branch where no terminal result event arrived; where a successful result event
is present, the turn completes regardless of counts. Two further adapters do not report usage in a
form that makes a token comparison meaningful at all. The class of failure was therefore confirmed
by more than one runtime, and the guard against it covered a fraction of them.

The reach of the question extends past the transition itself. The terminal status recorded for a run
is a pure function of the worker's exit kind, so an empty run recorded the successful value correctly
by that value's own definition, and the difficulty was that every consumer read a stronger meaning
into it than the definition supported. One of those consumers is the startup pass that rebuilds
pending reaction entries, which selects the most recent run recorded as successful for each issue. An
empty run in a reused per-issue workspace, holding source-control metadata an earlier run wrote,
therefore became the row that seeded reaction state after a restart.

## Decision Drivers

1. **The transition asserts something about a run's output, and the test applied to it described only
   the run's exit.** Every one of the four conditions was a property of how the process ended or of
   how the deployment was configured. None was a property of what the run produced. A test whose
   subject differs from the claim it licenses will eventually license a false claim, and here it did
   so in the ordinary case rather than an exotic one.
2. **The two possible errors are not symmetric.** Advancing an issue that has nothing behind it moves
   work in a system the orchestrator does not own, removes it from the queue the orchestrator does
   own, and puts it in front of a person. Withholding the advance from an issue that did have
   something behind it leaves that issue exactly where it already was, in a queue the orchestrator
   continues to watch, with the withholding recorded. The first error is silent and leaves the
   system; the second is visible and stays inside it. A policy calibrated on that asymmetry is
   allowed to be stricter than one calibrated on the cost of the checks alone.
3. **Any remedy must hold for every agent runtime, including ones not yet written.** Agents leave
   their results in different places and describe their work in different vocabularies. A predicate
   that holds for one runtime is a property of that adapter, not of the system, and it decays the
   moment a further runtime is added. The orchestrator's own vocabulary is the only one every run
   shares.
4. **Absence of evidence is not evidence of absence.** Not every deployment can be measured. A
   workspace need not be a version-controlled tree, a run may produce its result somewhere the
   orchestrator cannot see, and an inspection can fail outright. A two-valued predicate has to assign
   each of those cases to one side or the other, and assigning them to emptiness makes the remedy
   misfire on exactly the deployments that never had the problem.
5. **Token counts are a weak and non-uniform proxy for work performed.** The reclassification already
   in place reaches two of the five agent adapters and only one branch within them, two further
   adapters supply nothing comparable, and a durable distinction between a count measured as zero and
   a count never measured is a recent addition rather than an old assumption. A system-wide rule
   cannot rest on a signal that the runtimes do not report on comparable terms.
6. **A successful run routinely leaves a clean working tree.** A post-run hook that stages, commits
   and pushes the agent's work is a common and entirely correct configuration, and it leaves nothing
   behind in the working tree for a later inspection to find. Any test evaluated only against the
   workspace as it stands at exit reads those runs as empty, which inverts the outcome on the
   best-configured deployments.
7. **Coverage must not depend on the operator already knowing about the failure, and what is offered
   must be a single choice.** A remedy that protects only deployments which configure it protects
   the population that has already diagnosed the problem, which is not the population that has it. A
   remedy that arrives as a configurable framework asks every operator to define correctness for
   themselves, which is the same abdication in a longer form.

## Considered Options

- Impose no requirement, and document the current behavior as the intended one
- An operator-declared readiness predicate in the workflow configuration, naming paths that must
  exist in the workspace when the worker exits
- A three-valued evidence predicate evaluated by the orchestrator from what the system already
  observes, governed by one operator-facing policy setting
- Extend the agent adapter contract so that each adapter declares whether its turn produced work
- Perform no orchestrator-side tracker writes at all, leaving the agent to transition the issue
  through provider-native tools

## Decision Outcome

Chosen option: **a three-valued evidence predicate evaluated by the orchestrator**, because it is
the only option that reaches every deployment without asking the operator to define correctness
first, and because its third value is what confines the behavior change to deployments the system
can actually measure.

### Three values, and why the third one carries the design

The predicate returns one of three verdicts, and only one of them changes anything.

1. **Work observed.** The transition proceeds exactly as it does today.
2. **Absence of work observed.** The transition is withheld.
3. **Evidence not determinable.** The transition proceeds exactly as it does today, and the verdict
   is recorded.

The third verdict is the load-bearing part and the reason the change is safe to ship on by default.
An operator whose workspace is not a version-controlled tree, whose agent leaves its results
somewhere outside the filesystem, or whose runtime reports nothing the orchestrator can measure,
sees no behavioral change and configures nothing. Unknown is never equated with empty. That is
what keeps the decision from taxing the population that does not have the problem, and it is the
difference between a predicate that is agent-agnostic in principle and one that is agent-agnostic in
practice.

The verdict is a fifth condition and not a replacement for the four. It is consulted only where the
four already select the handoff path, so it can withhold a transition and can never cause one. No
issue advances under this decision that would not have advanced before it.

The predicate is monotone in its positive signals. Adding a further signal can only reclassify a
case from absence to work observed, never in the other direction. Anything that later contributes
evidence, including a contribution from an adapter, therefore narrows the set of withheld
transitions and can be added without re-opening this decision.

### What counts as evidence

Work is observed when any of the following holds.

1. The workspace's committed position has moved since the baseline captured at the start of the run.
2. The workspace's working tree differs from that baseline.
3. The workspace holds source-control metadata naming a pushed commit or a pull request for this
   issue.

Absence of work is observed when the workspace was inspected against a baseline and none of the
three holds. Evidence is not determinable when no baseline could be captured, the workspace is not
a version-controlled tree, the inspection failed, or no workspace was recorded, and no positive
signal holds.

The third signal is not decoration, and it is where a naive version of this predicate breaks a
guarantee the system already makes. A workspace is keyed per issue and reused across that issue's
runs, so the source-control metadata in it is a statement about the issue's work rather than about
the current run alone. A run that legitimately finds nothing left to do, in a workspace whose
earlier run already pushed a branch or opened a pull request, is a run whose issue does have
something behind it. Withholding that run's transition would strand the issue in an active state
with its work already finished, which is the dispatch cycle that orchestrator-driven handoff was
introduced to end. The predicate must not reintroduce it, and this signal is what prevents it.

The completed-turn count already carried on the worker result is deliberately not decisive. A
nonzero count is not evidence that anything was produced, because a turn can complete having changed
nothing, and that is precisely the failure being addressed. A zero count is already acted on by the
runtimes that measure anything comparable, and only within one branch of them, so it is neither
uniformly available nor uniformly meaningful. The count is recorded alongside the verdict as
diagnostic context and grants neither of the two decisive verdicts.

### The baseline, and where it is taken

Evidence is a difference against the workspace state captured when the run begins. It is not a
property of the workspace as it stands at exit. Getting this wrong inverts the outcome, so it is
stated as a constraint on any implementation rather than left to one.

The baseline is captured at the boundary between workspace preparation and agent launch, after
preparation has finished and before the agent starts. Both edges of that boundary matter. Capturing
earlier would count preparation as output, since preparing a workspace can populate an entire source
checkout. Capturing later would miss the beginning of the agent's own work. The baseline records the
workspace's committed position together with enough of its working-tree state to establish, later,
whether anything changed.

Two failure modes are closed by measuring a difference rather than a state. A post-run hook that
commits and pushes leaves a clean working tree at exit, so a test asking only whether the tree is
dirty would classify the most completely successful runs as empty. And a per-issue workspace carries
forward whatever an earlier run left uncommitted, so a test asking only whether the tree differs
from its committed position would read that residue as this run's work. Committed movement and
uncommitted modification both count as work, and both are measured against the baseline. No
particular command is prescribed and none should be inferred.

### What happens when absence is observed

The transition is not performed and the issue stays in its active tracker state. The attempt is
recorded as unsuccessful, naming the evidence verdict as the cause, and the ordinary failure and
retry path applies with the backoff it already has and the ceiling set out below. No new lifecycle,
no new issue state, and no new retry policy are introduced.

The failure path is chosen over the continuation path deliberately. A continuation expresses that
work is under way and should carry on, and it fires at a short fixed delay on that assumption. A run
that produced nothing is not work under way. Routing it to a continuation would spin a tight cycle
on an issue that is not moving, which is the behavior the handoff transition was introduced to
stop. The failure path already carries an increasing delay, a bound on attempts, and a record an
operator can find.

### How often an absence may repeat before the issue is parked

A withheld transition leaves the issue in an active tracker state, which is the whole point of
withholding it, and which also means a later poll cycle will pick the issue up again. Before this
decision a run that produced nothing still advanced its issue and therefore stopped. Without a bound
it would not stop at all. The ceiling nearest to hand, the per-issue effort budget, is only a
partial one: it counts every recorded run, so repeated absences do spend it, but it is unlimited
unless the deployment sets it, and it cannot tell a sequence of absences apart from productive
iteration, because both spend it at the same rate. Exchanging a silent false advance for an
unbounded quiet loop is not an improvement. It also contradicts what any operator brings to this
system from continuous integration, where a thing that fails is retried a bounded number of times,
then reported and left alone.

The bound counts consecutive observed absences on one issue. Any run that produces evidence resets
the count. Counting consecutive absences rather than absences in total is the substantive part: an
issue that alternates between producing work and producing nothing is making progress, and a
lifetime total would eventually park it for having made that progress unevenly, which is a
punishment for a pattern that is not a fault.

The ceiling is the value the deployment has already chosen for its per-issue effort budget, applied
to the count of consecutive absences rather than to the count of runs. An observed absence is routed
to the failure and retry path, and its consecutive count is carried alongside the attempt sequence
that path already maintains. No second numeric setting is
introduced. A new setting would be a knob for a failure mode the operator has never heard of, put in
front of every operator to serve the few who will ever tune it, and each of them would have to
choose a value for it before having any experience of the fault it governs. The ceiling that already
exists expresses the deployment's tolerance for repeating an attempt that is not working, and that
is the same judgement being asked for here.

On reaching the ceiling the issue is parked, through the escalation mechanism the system already
owns. The operator-configurable escalation label that the review and auto-merge recovery paths
already apply on exhaustion is applied here on the same terms, and the claim is released. No new
issue state, no new field, and no new tracker transition is introduced. Reusing the label is the
substance of the choice and not an economy: an operator who already filters on that label, alerts on
it, or triages by it keeps every one of those behaviors working unchanged, and a parked issue turns
up in the place that operator already looks. A parallel mechanism would be a second thing to
discover, a second thing to wire into alerting, and a second way for a parked issue to go unnoticed.

The parked outcome is announced and not merely reached. The operator learns that the system
dispatched the issue, observed nothing each time, stopped trying, and marked the issue for a human.
An issue that goes quiet with no account of why is the failure this decision exists to remove, and a
bound that ended the loop silently would reintroduce it in a new place.

### One setting, three named policies

The behavior is governed by a single operator-facing field, `tracker.handoff_evidence`, placed
beside the handoff state it qualifies. It takes one of three named policies.

- `observed`, the default, applies the three-valued rule above. Only a positively observed absence
  withholds the transition.
- `strict` treats an undeterminable verdict as an observed absence. It suits a deployment whose
  operator knows every workspace is a version-controlled tree and wants the guarantee to hold
  without exception. In a deployment where that is not true, it withholds every transition, which is
  the operator's choice to make and is stated plainly in the operator-facing reference.
- `off` restores the previous behavior exactly. Under this policy no baseline is captured, no
  evidence is read, and the transition is decided by the four conditions alone.

The `off` policy is not the verdict being computed and then ignored. Nothing is computed. That
distinction is what makes the escape hatch complete: an operator who selects it pays no inspection
cost, sees no new log line, and cannot be affected by a defect in the predicate.

One field with three named values is the whole of the new configuration surface. The alternative,
letting the operator compose the predicate from parts, would relocate the definition of a finished
run from the system to the operator, and the seventh driver rules that out.

### What the operator sees

A withheld transition is reported at warning level, naming the issue and the verdict, and is
counted, so it is distinguishable from an ordinary run failure both in the log and in the metric
surface.

An undeterminable verdict is also recorded, at a lower level. This matters more than it appears to.
The likely first experience of the default policy is an operator observing that nothing ever
changes, and that observation has two very different explanations: every run genuinely produced
something, or nothing could ever be measured. Those have different remedies, and one of them is not
a fault. A policy that changes nothing has to be able to say why it changed nothing.

### The verdict is reported but not stored

The verdict is logged and counted. It is deliberately not given a durable field of its own on the
run record. A stored verdict would make historical reporting possible, and the case for it is
coherent, but it is also hypothetical: no one has yet needed to know what fraction of a deployment's
runs were undeterminable over a quarter. A stored field, in exchange, is permanent. It is carried by
every deployment whether or not that deployment ever reads it, it is migrated by every upgrade, and
once rows are written under it, it cannot be withdrawn without leaving a gap in the history it was
added to provide. Its cost falls due immediately and its value stays conditional.

The diagnostic case that does occur is already served by what this decision mandates elsewhere. A
withheld transition records the verdict as the run's failure reason, so the affected run states on
its own record why it did not advance. The counter that distinguishes a withheld transition from an
ordinary run failure gives the aggregate view for as long as the deployment's metric retention
holds. Between them they answer the question an operator actually asks first, which is why this
issue did not move, and the one asked next, which is how often this is happening.

The deferral is a decision rather than an omission, and it has a condition that reverses it. Should
an operator need the distribution of verdicts over a period longer than metric retention covers, or
need to attribute historical runs to a verdict after the fact, the field earns its permanence and
should be added then, when the shape of the report is known and can inform the shape of the field.

### Why the default withholds rather than warns

Established practice in agentic automation tooling gates the artifact-producing step on a non-empty
patch and exposes the choice as a single three-valued option, warn or fail or ignore, alongside an
explicit opt-in for deliberately empty results. Three lessons generalize from it: the artifact is
the evidence, in preference to any account a runtime gives of itself; the policy is one setting and
not a language; and the default is safe without being fatal.

The first two are adopted unchanged. The third is adopted in a modified form, and the modification
is worth stating rather than glossing. A warning is a defensible default where a human reads the
output of every invocation. This orchestrator runs unattended, so a warning is a line in a log
nobody is watching, and defaulting to one would preserve the silent failure under a new name. The
second driver supplies the rest: the outcome the default prevents leaves the system and cannot be
retracted from inside it, while the outcome the default risks stays in the queue the orchestrator
already owns and announces itself. Where the two costs are that unequal, the safe default is the one
that declines to act.

The default is likewise not `off`. A remedy shipped inert protects the operators who already
diagnosed the problem and went looking for the setting, and the defining property of this failure is
that it produces no signal that would send anyone looking. Shipping it on, with a verdict that
abstains wherever it cannot see, is what makes the coverage match the exposure.

### Considered Options in Detail

**Impose no requirement, and document the current behavior as intended.** This is the cheapest
option and it has a real argument behind it: an orchestrator cannot know what a given agent
considers a result, and writing the behavior down converts a surprise into a documented property
that operators can design around. It is rejected because documenting a silent failure does not stop
it. The behavior advances work in a system the orchestrator does not own, on the strength of
nothing, and removes the issue from the only queue that would have brought it back. An operator
reading the documentation learns that they must audit handoffs by hand, which is a cost paid on
every run to avoid a fault that occurs on few.

**An operator-declared readiness predicate in the workflow configuration.** The operator names paths
that must exist in the workspace when the worker exits, and the transition is withheld unless they
do. This is explicit, entirely runtime-agnostic, and expresses something the orchestrator genuinely
cannot infer, which is what that particular deployment considers a finished piece of work. It is
rejected on two grounds. It relocates the definition of a finished run onto every operator, and most
tasks have no single artifact whose existence settles the question, so the predicate would either be
trivially satisfiable or wrong. And its coverage is exactly inverted with respect to need: it
protects only deployments that configure it, and the operators who configure it are the ones who
already know the failure exists. Nothing about this option is foreclosed. An operator-declared
predicate could later be added as a further positive signal, where the monotonicity property above
guarantees it can only narrow the set of withheld transitions.

**Extend the agent adapter contract so each adapter declares whether its turn produced work.** This
is the most general option, because the adapter is the component that understands its runtime, and
it is the only one that could see work an agent performed outside the workspace entirely. It is
rejected for now on cost and on coherence. It touches every adapter and the contract itself, so each
runtime that arrives later inherits a new obligation. Each adapter would have to invent its own
notion of work produced, and the notions would not be comparable, which reproduces at the contract
level exactly the non-uniformity that makes token counts unusable. And it still cannot see work that
was produced and then reverted, so it does not subsume the workspace signal it would be replacing.
The refinement worth keeping is narrower than the option: an adapter may later contribute a positive
signal into the predicate, while the verdict itself stays with the orchestrator. That preserves one
definition of the verdict across every runtime and inherits the monotonicity guarantee.

**Perform no orchestrator-side tracker writes at all.** In this topology, observed in comparable
systems, the orchestrator never transitions an issue and the agent moves it through provider-native
tools as part of its own work. The failure mode disappears by construction, because an agent that
does nothing writes nothing, and there is no separate component left to make a claim on its behalf.
The option is recorded because it is a coherent alternative and not a strawman: it is a smaller
system with fewer moving parts and one fewer contract to keep true. It is rejected because
deterministic orchestrator-side write-back is a load-bearing property here rather than an
implementation choice. It is what allows an agent with no tracker capability at all to participate
on equal terms, and it is what makes the transition a guarantee of the system rather than something
contingent on the agent remembering to perform it, choosing the right target, and holding a
credential for it. Adopting the topology would trade a fault that misreports a small number of runs
for a design in which no run's tracker outcome is guaranteed.

## Consequences

### Positive

- The failure is closed for every deployment the system can measure, and closed without the operator
  configuring anything, which is the only shape of coverage that matches a failure producing no
  signal.
- A deployment the system cannot measure is unaffected by construction rather than by exemption.
  There is no list of supported runtimes to maintain and no new way for an unfamiliar setup to be
  handled wrongly, because the abstention is a verdict rather than a special case.
- Backward compatibility is bounded by a single stated rule. Behavior changes for exactly one input:
  a run in a measurable workspace that the system positively observed to have produced nothing.
  Every other run, in every deployment, takes the decision it took before, and the one changed case
  is escapable with one field.
- No new machinery is introduced. The workspace inspection is the kind the self-review path already
  performs, the source-control metadata is already read on the exit path, the turn count is already
  carried on the worker result, the disposition is the failure and retry path that already exists,
  and the parking of an exhausted issue is the escalation the recovery paths already perform.
- The terminal status recorded for a run moves closer to the meaning its consumers already read into
  it, which narrows a long-standing gap between a value's definition and its use rather than
  widening it.
- The startup pass that rebuilds pending reaction entries stops selecting a run that produced
  nothing, so recovered reaction state is attributed to the run that actually produced the artifact.
- The predicate's monotonicity fixes the shape of every later extension. A new positive signal, from
  an adapter or from operator configuration, can only shrink the set of withheld transitions, so it
  is addable without re-opening the verdict or the policy.
- Repeated absence terminates and reports. An issue the system cannot make progress on is dispatched
  a bounded number of times, parked through the escalation path the operator already watches, and
  then left alone. That is the shape an operator already expects from unattended automation, and it
  is reached without adding a setting, a state, or a second escalation path to learn.

### Negative

- **A primary dispatch whose entire product is a tracker write is classified as empty.** The exposed
  population is narrower than it first appears. A dispatch posture that does not drive issue state
  is already excluded by one of the four pre-existing conditions, so the reaction-style runs whose
  natural product is a comment or a review never reach the evidence predicate at all. What remains
  exposed is a primary dispatch whose whole result is a write to the tracker, or a change written
  outside the workspace root, in a workspace that is a version-controlled tree. Such a run presents
  a measurable workspace and no movement, so the verdict is absence rather than undeterminable and
  the transition is withheld. Hosting the tracker-access tool the agent calls does not rescue the
  case. That tool executes in a sidecar process the agent runtime owns, and the only medium it
  shares with the orchestrator is a read-only handle to the orchestrator's own store, so nothing it
  does is reported back and a write made through it is, at the moment the disposition is computed,
  indistinguishable from no write at all. The third verdict cannot rescue the case either, because
  from everything the orchestrator observes it is identical to a genuinely empty run. This is the
  population for which the default is wrong, and it is why the field exists: such a deployment sets
  the policy to `off`.
- **A silent wrong outcome is exchanged for a bounded repeated attempt.** An issue whose runs keep
  producing nothing is dispatched again on the consecutive-absence sequence until the ceiling is
  reached, and is parked only then. The exchange is deliberate, since the repetition happens inside
  the queue the orchestrator owns and announces itself each time, but it is an exchange and not a
  free improvement. A deployment that hits this sees repeated dispatch, and pays the effort those
  dispatches consume, where it previously saw one wrong advance and then silence. The bound makes
  the repetition finite; it does not make it free.
- **An issue can now be parked by the orchestrator on evidence alone.** Reaching the ceiling applies
  the escalation label to an issue no human has yet looked at, on the strength of a predicate that
  measures the residue of a run rather than its intent. A deployment whose agents legitimately
  produce nothing the system can see therefore accumulates labeled issues until its operator selects
  a different policy. The label those issues arrive under is the one the recovery paths already use,
  so escalations of different origin land in one place and are told apart only by the record that
  accompanies each. Sharing the label is the deliberate choice made above, and this is its price.
- **The terminal status recorded for a run changes meaning, and a database will span both
  meanings.** It was a pure function of the worker's exit kind. It now also asserts that the system
  did not positively observe the run producing nothing. Rows written before the change, and rows
  written under the `off` policy, carry the older meaning, and nothing rewrites them. An aggregate
  computed across that boundary compares two definitions, and the success rate of an affected
  deployment falls on the day the change ships without anything about its agents having changed.
- **One case loses startup recovery of its pending reactions.** The recovery pass selects, per issue,
  the most recent run recorded as successful whose completion falls inside the recovery lookback.
  Where a producing run remains inside the lookback, the selection now falls through to it and the
  recovered entry is unchanged in substance. Where the last producing run has aged out of the
  lookback and only empty runs remain inside it, the issue previously recovered, because the empty
  run refreshed its recency, and now does not. A long-lived pull request whose only recent runs were
  empty therefore stops being observed after a restart. The loss is small and it is the lookback
  behaving as specified rather than being renewed by a run that did nothing, but it is a loss.
- **The dispatch path gains an inspection it did not have.** A baseline is captured before every
  agent launch, on a path that previously touched the workspace only to prepare it. The cost is
  small and the failure is handled, since a baseline that cannot be captured yields the
  undeterminable verdict, but it is a new dependency on a path that had none. Under the `off` policy
  it does not occur.
- **The `strict` policy has no partial form.** In a deployment whose workspaces are not
  version-controlled trees it withholds every transition and stops the pipeline. That is the
  operator's choice and it is documented, but the setting fails whole rather than degrading.
- **The predicate cannot see work that was produced and then reverted.** An agent that writes a file
  and removes it, or commits and then resets, leaves a workspace indistinguishable from its
  baseline. Classifying that run as empty is defensible, since nothing survived it, but the verdict
  describes the residue and not the effort, and an operator who reads it as a statement about
  whether the agent did anything will misread it.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The orchestration state machine material: the worker-exit disposition restated as the four
   existing conditions together with the evidence verdict, the three verdicts and the disposition
   each produces, and the routing of an observed absence to the failure and retry path rather than
   to the continuation path.
2. The configuration specification: the `tracker.handoff_evidence` field, its three named values,
   its default, and its validation as a closed set of names checkable offline.
3. The workspace management and safety material: the baseline captured at the boundary between
   workspace preparation and agent launch, what it records, and the rule that evidence is a
   difference against it rather than a property of the tree at exit. The post-run hook's contract is
   unchanged and is explicitly not given a vote in the disposition.
4. The agent adapter contract material: the statement that the verdict belongs to the orchestrator,
   that no obligation is added to any adapter, and that an adapter may later contribute a positive
   signal without owning the verdict.
5. The tracker integration contract material: the handoff write is now conditional on the verdict,
   and the conditions under which it is withheld.
6. The failure model and recovery strategy material: the disposition of an observed absence, the
   count of consecutive observed absences and its reset by any run that produces evidence, the
   ceiling that count is measured against and its derivation from the configured per-issue effort
   budget rather than from a new setting, the parking of an issue through the existing escalation
   label on reaching that ceiling, and the change in which run the startup reaction-recovery pass
   selects.
7. The persistence material: the redefinition of a run's terminal status, the statement that
   historical rows are not rewritten and carry the previous definition, and the statement that no
   new stored field records the evidence verdict.
8. The logging, status and observability material: the warning emitted for a withheld transition,
   the record emitted for an undeterminable verdict, the record emitted when an issue is parked on
   reaching the consecutive-absence ceiling, and the counter that distinguishes a withheld
   transition from an ordinary run failure.
9. The workflow file syntax reference: the new field in the tracker block.

## Confirmation

The decision is validated when all of the following hold:

1. A deployment that sets the policy to `off` captures no baseline, inspects no workspace at exit,
   emits no new log record, and takes exactly the transition decision it took before.
2. A run that exits normally in a version-controlled workspace, changing no file and holding no
   source-control metadata, does not advance its issue. The attempt is recorded as unsuccessful with
   the evidence verdict named as its cause, and the issue remains in its active state.
3. A run whose post-run hook stages, commits and pushes every change, leaving a clean working tree at
   exit, advances its issue.
4. A run in a workspace that is not a version-controlled tree advances its issue under the default
   policy, does not advance it under `strict`, and records the undeterminable verdict in both cases.
5. A run that produces nothing in a workspace whose earlier run already pushed a branch or opened a
   pull request advances its issue, so an issue whose work is finished is never stranded in an active
   state.
6. A run whose workspace already carried uncommitted changes when the run began, and which changed
   nothing further, is classified as an observed absence rather than as work.
7. A primary dispatch whose only product is a write to the tracker is classified as an observed
   absence under the default policy and advances its issue under `off`, so the case the field exists
   for behaves as documented rather than by accident.
8. A withheld transition is distinguishable from an ordinary run failure both in the log and in the
   metric surface.
9. An observed absence is scheduled on the failure and retry path with its increasing delay, not on
   the continuation path with its fixed short delay.
10. Consecutive observed absences on one issue reach the configured ceiling, at which
    point the escalation label is applied to the issue, the claim is released, and the absence
    sequence produces no further dispatch.
11. A run that produces evidence resets the consecutive-absence count, so an issue alternating
    between producing work and producing nothing is not parked by this mechanism.
12. The parked outcome is reported, naming the issue, the number of consecutive absences observed,
    and the escalation applied, so an operator can tell a parked issue from an abandoned one.
13. No stored run record gains a field for the evidence verdict; a withheld run names the verdict in
    its failure reason instead.
14. After a restart, the pass that rebuilds pending reaction entries selects the most recent run that
    produced something in preference to a later run observed to have produced nothing.
