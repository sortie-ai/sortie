---
status: accepted
date: 2026-08-27
decision-makers: Serghei Iakovlev
---

# Let the Agent Declare That Nothing Needed Changing

## Context and Problem Statement

An agent had two things it could say about its own run through `.sortie/status`. `blocked` reported that further work was futile, which parks the issue. `needs-human-review` reported finished work awaiting a person, which takes the handoff path. Neither expressed a third conclusion an agent reaches regularly: the requested outcome already held and nothing needed changing.

Such a run leaves no commit, no diff, no pushed branch. The handoff-evidence policy reads a run by that residue, so it classified the run as an absence of work: handoff withheld, failure recorded, session-failure comment written where those comments are enabled, retry scheduled, consecutive-absence count advanced toward the escalation label. Right by definition and wrong about the run, the verdict recurred on every later dispatch, so the sequence ran to the ceiling.

The only remedy was `tracker.handoff_evidence: off`, which computes no verdict for any issue, trading one issue's false failure for the guard on all. Deployments used the `after_run` hook instead, leaving something findable. That manufactures evidence rather than stating a conclusion. The hook also runs before the disposition, with no vote in it.

The disposition reads the status file. The agent's tracker tool also executes any transition the agent asks for, terminal states included, and an observed terminal state suppresses the handoff and releases the claim. Neither was the documented way to state this conclusion, and the two produced different outcomes: a failure and a retry, or a quiet release.

## Decision Drivers

1. **The conclusion is a finding, not a residue.** Only the party that investigated knows whether the outcome already held; no inspection separates looking and finding nothing from doing nothing.
2. **The condition is per-issue and per-run.** A control scoped to the deployment cannot express it, and one set before the run cannot know it.
3. **The absence guard may narrow and must not widen.** ADR-0020 withholds a handoff only on observed absence and lets later evidence reclassify only toward work observed. A declaration inherits that direction.
4. **An observed tracker state names no author.** A terminal state read at exit may come from the agent, a hook, an automation, or a person, and is indistinguishable from an unrelated write.
5. **Where a no-change run leaves the issue is a local judgment.** With no pull request and no diff, a handoff asks some boards for a review of nothing and gives others the wanted outcome.
6. **Vocabulary is a cost paid in the injected instruction.** Sortie appends a fixed instruction on the first turn of every run, naming the recognized values and explaining each. It is compiled in, not adjustable by an operator.

## Considered Options

- A third recognized status-file value meaning the requested result already holds, counting as positive evidence and driving a configurable target state
- Treat a terminal state observed at exit as the declaration
- A per-issue operator gesture in the tracker, a label or comment declaring a no-change conclusion acceptable there
- Document `tracker.handoff_evidence: off` as the supported answer

## Decision Outcome

Chosen option: **a third recognized status-file value**, because the declaration has to come from the party that investigated, and the status file is how an agent already speaks to the disposition about its own run.

### The signal

An agent writes `no-change-needed` to `.sortie/status` to state that the requested outcome already held and nothing needed changing. It is consumed like the two recognized values.

At the end of the turn loop the two values diverge: `blocked` ends the run, `needs-human-review` admits the run to the self-review phase. A declaration admits the phase too: the operator's verification commands are what can falsify a claim that nothing needed changing, so running them makes it checked evidence, not an assertion on trust. Where they fail, change was needed after all and the run continues on the ordinary path.

A declared run yields no absence verdict, records a success, does not advance the consecutive-absence count, and releases a park held for consecutive absences.

The declaration is monotone: it may move an absence verdict or an undeterminable one to work observed, and never withholds a transition that would otherwise proceed. That is the whole of its effect on the guard. Under `off` nothing is computed, so its only effect there is the target state.

### Where the issue goes

A new field, `tracker.no_change_state`, names the state a declared run moves the issue to. Unset, it is `tracker.handoff_state`, so existing configurations see no change in where issues land. An operator may name a value from `tracker.terminal_states` instead, the case that earns the field: a handoff with no pull request and no diff puts an issue before a person with nothing to look at. It is therefore the one target-state field allowed to name a terminal state, the case it serves, where the sibling fields are rejected on such a collision. Where no handoff path applies, the declaration changes no issue state.

A terminal state is not the default. The orchestrator's own terminal writes are confined to the opt-in `merge_completion` reaction, on ADR-0017's reasoning that a write closing an issue on the operator's board must be asked for. A default would extend self-closing to deployments that never asked for it.

### What the declaration does not do

A run that produces nothing and declares nothing is an absence of work and keeps every outcome it had. An undeterminable run that declares nothing keeps its policy-dependent outcome: the transition proceeds under `observed`, is withheld under `strict`. Under `off` neither run has a verdict computed.

A terminal state observed at exit keeps its meaning: a decision already made, which the disposition respects by suppressing the handoff, releasing the claim, and scheduling nothing. It is not a declaration, and the terminal test runs first, so a declaration on an already terminal issue changes nothing.

### Considered Options in Detail

**Treat a terminal state observed at exit as the declaration.** Rejected on driver 4: an agent's conclusion, a hook's side effect and a person's action would license the same outcome, and the case the decision exists to serve cannot be told from the other two.

**A per-issue operator gesture in the tracker.** Rejected because it needs a human on every occurrence, arrives after the false failure is recorded, and answers a different question: an operator can call the conclusion acceptable; only the agent can say this run reached one.

**Document `tracker.handoff_evidence: off` as the supported answer.** Rejected because it protects only the deployments that have already diagnosed the problem, by withdrawing the guard from every issue they own.

## Consequences

### Positive

- A run concluding that the requested outcome already held is recorded as the success it is, not as a failure with a retry behind it, and no issue is parked for repeatedly concluding so.
- Coverage does not depend on an operator acting: the instruction naming the status values is injected on every run of every deployment, so the third value reaches every agent the two existing ones do, as ADR-0020 demands.

### Negative

- **The declaration is unverified by construction.** Self-review can falsify it, but a deployment with self-review off has no check, and a passing check shows only that the outcome holds now. An agent that writes the value on a run that needed change defeats the guard there, undetectably.
- **The injected instruction grows.** Appended on the first turn of every run in every deployment, a third value costs tokens everywhere. It must be worded so an agent reaches for it only where the outcome genuinely already held, and that wording is compiled in, beyond an operator's reach.
- **A terminal `tracker.no_change_state` closes issues on that assertion.** It moves the orchestrator's terminal writes past the merge it watched, and a wrongly declared run closes an undone issue.
- **A fifth state field.** `tracker.no_change_state` joins the active, in-progress, handoff and terminal fields, and is the one whose value may legitimately equal another's, so a value entered in the wrong field is not caught the way sibling collisions are.

## Confirmation

The decision is satisfied when all of the following hold:

1. `no-change-needed` ends the turn loop and is consumed where the two recognized values are, with nothing surviving into the next run.
2. A declared run is admitted to the self-review phase on the same terms as the completion signal, and skipped on the same terms.
3. A declared run showing no work records a success, schedules no retry, writes no session-failure comment, and does not advance the absence count.
4. A run showing no work that declares nothing keeps every outcome it had: handoff withheld, failure recorded, absence count advanced.
5. A declared run releases a park held for consecutive absences, under a policy that computes a verdict.
6. A declared run reaches `tracker.no_change_state`, or `tracker.handoff_state` where that field is unset, under every policy value including `off`.
7. An undeterminable declared run proceeds under `strict` rather than being withheld.
8. A terminal state observed at exit, with no declaration, suppresses the handoff and releases the claim as before, reaching no new state.
