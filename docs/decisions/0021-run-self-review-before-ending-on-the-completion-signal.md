---
status: accepted
date: 2026-08-17
decision-makers: Serghei Iakovlev
---

# Run Self-Review Before Ending the Run on the Completion Signal

## Context and Problem Statement

Sortie appends a fixed instruction to the agent prompt on the first turn of every worker run. It
names two values the agent may write to `.sortie/status`: `blocked` when it cannot proceed, and
`needs-human-review` when its work is complete and awaiting review. The worker reads that file
after every turn. On either value it stopped the agent session, ran the workspace teardown hook,
and returned from the worker with a soft stop.

The self-review phase sits after the coding turn loop. It runs the operator's verification
commands, gives the agent a review turn, and where the resulting verdict calls for one gives it a
fix turn, repeating up to `self_review.max_iterations`. It is gated on `self_review.enabled`, on
the issue still being in an active tracker state, on a live context, and on a dispatch posture
that drives issue state.

The two arrangements were built independently and the ordering between them was never settled. The
status read returned from the worker rather than leaving the turn loop, so it exited past the phase
that follows the loop. Of the two ways the loop was otherwise left, the per-turn tracker refresh
that finds the issue no longer active fails the phase's own active-state gate, which left
turn-budget exhaustion as the only exit that reached the phase. An agent that followed the
instruction Sortie itself injects therefore ended the run before the verification its operator had
enabled could run, and the feature took effect only when the agent failed to finish.

Neither the internal protocol material nor the specification was wrong about its own half. The
protocol material described the status exit, including the teardown hook on that path, and never
mentioned the self-review phase; the specification's worker algorithm described the phase and never
mentioned the status read. There was nothing to bring back into line, only a position to take.

The failure was quiet in every channel. The run recorded the same normal exit a verified run
records, no review metadata was written, so nothing stated that verification had not happened, and
the teardown hook received the `disabled` self-review outcome even where the feature was switched
on. The only way to learn of it was to read the orchestrator.

The question reaches past the exit itself. The phase runs agent turns of its own and reads the same
file after each of them, so any ordering must also settle what the file means once the phase has
started, and whether the two values still mean different things there.

## Decision Drivers

1. **A signal the system injects must not disable a feature the operator enabled.** The instruction
   is appended on turn one of every run; no configuration field, prompt template, or dispatch
   posture suppresses it. An agent that writes the value is doing exactly what it was told. A
   design in which correct agent behavior silently switches off a configured gate has located the
   fault in the wrong component.
2. **The two values mean different things, and a solution that merges them is wrong.** `blocked`
   reports that the agent cannot proceed; `needs-human-review` reports that the work is complete.
   The exit path already acts on the difference: a blocked soft stop suppresses the handoff
   transition, and the completion signal takes it. Running a verification loop over work the agent
   has called stuck spends turns and tokens re-asking a question already answered.
3. **"Work complete" is the precondition self-review needs, not a competitor to it.** Verification
   is meaningful over a finished change, and the completion signal is the agent's own statement
   that the change is finished. Reading it as the trigger for the phase takes the statement for
   what its instruction text says; reading it as the end of the run reads a scheduling instruction
   into a statement about the work.
4. **The status file carries no memory of having been read.** Every read is stateless and
   last-state-wins: the contents at the moment of the read determine the signal. A phase that runs
   agent turns and reads the same path after each of them will read the value that admitted it
   unless something removes it. Ordering alone does not settle the question; the lifetime of the
   signal is part of the decision.
5. **The phase needs a live session and an intact workspace.** Its turns run against the session the
   coding loop used, and its verification commands run in the workspace as the agent left it. Both
   were disposed of before the worker returned on this path, so the placement is constrained on
   both sides rather than free.
6. **A deployment that has not enabled self-review must be untouched.** The feature is off by
   default. A change to the handling of a signal that every run's prompt advertises must not be
   observable where the feature is not configured.

## Considered Options

- Run the self-review phase before the run ends on the completion signal, leaving `blocked` an
  immediate exit
- Suppress the injected instruction when self-review is enabled
- Keep the exit unchanged and document the phase as reachable only on turn-budget exhaustion

## Decision Outcome

Chosen option: **run the self-review phase before the run ends on the completion signal**, because
it is the only option that keeps both the agent's report of completion and the operator's
verification gate, and because it reads the signal as the statement about the work that Sortie's
own instruction text makes it.

### What the completion signal means

Where `self_review.enabled` is set and the phase's other gate conditions hold, `needs-human-review`
means that the work is complete, not that the run is over. The worker leaves the coding turn loop,
runs the self-review phase, and then ends the run with the soft stop it previously took at once:
the same normal exit kind, the same soft-stop reason, and the same exit disposition. The handoff
transition, the release of the claim, and the suppression of the continuation retry are all
unchanged. No issue takes a different disposition under this decision than it took before it; what
changes is the work performed before the disposition is computed.

Where self-review is disabled, or where any of the phase's gate conditions fails, the signal ends
the run exactly as it did before. The gate itself is not widened, relaxed, or re-evaluated: the
phase admits the completion signal on precisely the conditions it already admits an exhausted turn
budget.

### `blocked` is not a trigger

`blocked` remains an immediate exit and never enters the phase. Its disposition is unchanged: no
handoff transition, no continuation retry, claim released. An agent that reports it cannot proceed
has said that the work is unfinished, which is the one state in which a verification loop over that
work has nothing to verify and no one to hand a fix to.

### The signal is consumed on entry

The worker removes the status file at the moment it admits the run to the phase, before the first
review turn. Without that removal the phase reads, after its own first turn, the value that
admitted it, and aborts on its first iteration having produced nothing. The removal is the same
best-effort deletion the worker already performs before each dispatch, applied at a second point,
with the same treatment of symlinks and the same tolerance of failure.

This narrows a rule the agent-to-orchestrator contract stated absolutely. The orchestrator's only
operations on that path were the pre-dispatch deletion and the post-turn read. It now also deletes
the file at the moment it acts on a signal, so the file states what the agent has said since the
orchestrator last responded rather than carrying forward a value the orchestrator has already
honored. The agent remains the file's only writer, and nothing else in the system reads it.

### What the file means inside the phase

Inside the phase the two values diverge, and the divergence follows from what each one adds.

`blocked` aborts the phase. It is new information: the agent reports that it cannot carry the work
further, which is as true of a fix the review asked for as it is of the original task. The
iteration is recorded as aborted, naming the signal, and the run proceeds to its exit.

`needs-human-review` does not end the phase. Written during a fix turn it means that the fix is
applied and the work is complete again, which is the same statement that admitted the run to the
phase in the first place. It is consumed on the same terms as on entry, and the loop proceeds to
re-verification, where the verification commands and the review turn decide whether the fix holds.
Written during a review turn it carries the same meaning and is consumed on the same terms; there
the verdict file, not the status file, decides what the iteration does next.

An agent may restate completion on every fix turn without ever satisfying the verification. That
costs iterations and nothing else: the loop is bounded by `self_review.max_iterations` and
terminates with the cap-reached outcome it already has.

### Placement relative to teardown

The phase runs while the agent session is live and before the workspace teardown hook, which is
where it already sits on the turn-budget path. On the completion-signal path the session stop and
the `after_run` hook both ran before the worker returned, so both move behind the phase. The hook
then receives the self-review outcome and the summary path it already receives on the other path,
in place of the `disabled` value it received before.

### Considered Options in Detail

**Suppress the injected instruction when self-review is enabled.** The exit path is untouched and
the conflict disappears at its source, since an agent never told to write the file does not write
it. It is rejected on three grounds. It removes the agent's only means of reporting completion on
that deployment, so a finished run becomes indistinguishable from one that ran out of turns and
every completed run ends by exhausting its turn budget. It lets a configuration field the agent
cannot see rewrite the agent-to-orchestrator contract, so the same agent, prompt template, and task
speak different protocols on two deployments. And it does not reach `blocked`, which the same
instruction carries: either that signal is lost with it, or the instruction is split and the agent
is told it may report being stuck but not being finished.

**Keep the exit unchanged and document it.** This is the cheapest option and it has an argument:
the phase stays reachable on turn-budget exhaustion, so the feature is not inert in principle, and
a documented property is one an operator can design around. It is rejected because what would be
documented is that enabling the verification gate has no effect on the runs it exists for. The
operator's only remedy would be to write a prompt instructing the agent to ignore the instruction
Sortie itself appends, on every deployment that wants the feature. A gate whose trigger is the
agent failing to finish cannot verify finished work.

## Consequences

### Positive

- The verification the operator configured runs on the path that completed runs actually take, and
  it runs over a change the agent has declared finished, which is the input it was designed for.
- No issue takes a different exit disposition than it took before. The handoff transition, the
  claim release, and the suppression of the continuation retry are identical, so the change is
  confined to the work performed ahead of them.
- A deployment with self-review disabled is unchanged by construction rather than by exemption. The
  gate is evaluated before anything on this path differs, and where it fails the worker takes the
  exit it always took.
- Review metadata is recorded for runs that end on the completion signal, so the run record states
  whether verification ran and what it concluded, for the population where it previously stated
  nothing.
- The teardown hook receives the real self-review outcome on this path instead of `disabled`, so a
  hook that branches on it behaves the same however the run reached its exit.
- The status file gains a defined lifetime. It states what the agent has said since the
  orchestrator last acted on it, which is what makes the file readable by a phase that runs turns
  of its own.

### Negative

- **One status value now means two things depending on configuration.** With self-review off it
  ends the run; with it on it starts a phase. The injected instruction is identical on both
  deployments and the agent cannot tell which will happen. A third value, or a per-deployment
  instruction, would buy a precision the agent has no use for, since its statement about the work
  is the same in both cases, and would add a protocol term describing the orchestrator's own
  scheduling.
- **A run that signals completion takes longer and costs more.** Verification commands, a review
  turn, and possibly fix turns now run before an exit that was previously immediate. That is the
  price of the feature the operator enabled, but it lands on a path that never carried it.
- **The turn count reported for this path grows.** Review and fix turns are counted alongside coding
  turns, and that count reaches the run record and the soft-stop comment the operator reads, so
  identical coding work reports a larger number after this change than before it.
- **The orchestrator writes to the status path during a run.** The contract previously guaranteed
  that the pre-dispatch deletion was its only write-side operation there. An agent or a hook that
  read the file back expecting its own last write to survive the run finds it removed. The
  guarantee is narrower than it was, even though the agent remains the sole writer.
- **The agent loses a way to halt the phase without misreporting.** Inside the phase, an agent that
  means "stop now, a human must look at this" has only `blocked` to say it with, and that value
  suppresses the handoff transition, which is wrong for finished work. The exposure is bounded by
  the iteration cap, but the expressiveness available inside the phase is smaller than outside it.
- **The active-state gate is evaluated against a staler observation on this path.** The status file
  is read before the per-turn tracker refresh, so a run admitted to the phase by the completion
  signal carries the state observed one turn earlier, or the dispatch-time snapshot on turn one. An
  agent that moves the issue to the handoff state itself and then signals completion is therefore
  admitted to the phase on a state that no longer holds. Adding a refresh would spend a tracker
  call on every such exit to change the outcome for a population that has already finished its
  work, so the gate is left as it is and the staleness is accepted.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The agent-to-orchestrator protocol material: the ordering between a recognized status value and
   the self-review phase, the divergence of the two values on that path, the meaning of each value
   written during a review turn and during a fix turn, the consumption of the file on entry to the
   phase, and the resulting narrowing of the rule that the orchestrator performs no write-side
   operation on the file during a run.
2. The orchestration state machine material: the worker-exit sequence on a recognized status value,
   restated so the self-review phase precedes session teardown, the teardown hook, and the exit
   disposition, with the disposition itself unchanged for both values.
3. The reference algorithm for the worker attempt: the status read inside the turn loop, which the
   algorithm omits, and the admission of the completion signal to the phase that follows the loop.
4. The workspace management and safety material: the second point at which the status file is
   removed, and the statement that the removal carries the same symlink rejection and best-effort
   failure handling as the pre-dispatch one.
5. The failure model and recovery material: the unchanged dispositions of both values, stated
   alongside the new fact that one of them now runs the phase first.
6. The logging, status, and observability material: the review metadata now recorded for runs that
   end on the completion signal, and the self-review outcome now exported to the teardown hook on
   that path.

## Confirmation

The decision is validated when all of the following hold:

1. A run on a deployment with self-review enabled, in which the agent writes `needs-human-review`
   after a coding turn, executes the configured verification commands and at least one review turn
   before the run ends, reaching the phase through the signal rather than through turn-budget
   exhaustion.
2. That run ends with the normal exit kind, soft-stop reason, and exit disposition it took before
   this decision: the handoff transition fires where it is configured and the issue is active, the
   continuation retry stays suppressed, and the claim is released.
3. A run in which the agent writes `blocked` ends immediately, executes no verification command and
   no review turn, and takes the blocked disposition.
4. A deployment with self-review disabled is byte-for-byte identical in behavior: no review turn, no
   removal of the status file beyond the pre-dispatch one, and the same exit.
5. A phase entered from the completion signal does not abort on its first iteration, which
   demonstrates that the value admitting the run is not visible to the phase's own first read.
6. `needs-human-review` written during a fix turn does not end the phase: the loop re-runs its
   verification commands and its review turn, and the iteration cap still bounds it.
7. `blocked` written during a review turn or during a fix turn ends the phase, the iteration is
   recorded as aborted naming the signal, and the run proceeds to its exit.
8. A run that ends on the completion signal records review metadata, and its teardown hook receives
   the self-review outcome rather than the disabled value.
9. The phase runs while the agent session is live and before the teardown hook on the
   completion-signal path, as it does on the turn-budget path.
