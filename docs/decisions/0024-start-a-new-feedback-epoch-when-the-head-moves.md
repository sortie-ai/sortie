---
status: accepted
date: 2026-08-19
decision-makers: Serghei Iakovlev
---

# Start a New Feedback Epoch When the Pull Request Head Moves

## Context and Problem Statement

A managed pull request opens, its checks pass, the tracker issue moves to its configured post-work
state, and review finds nothing. Then a commit lands on the branch. Every conclusion the
orchestrator reached describes a commit that is no longer the head, and nothing re-derives them. The
reaction that watched CI retired itself on the passing result, and each reaction that kept watching
answers only its own question, so nothing in the system carries the statement that everything
concluded about this pull request is void.

Scoping the CI verdict to the pull request's current head (per ADR-0023) settled which commit a
verdict describes. It left the lifecycle open: what becomes of the rest of the pull request's state
when the head moves. Both ways of guessing are wrong. Under-reacting leaves the orchestrator acting
on a commit nobody is looking at. Over-reacting is worse. A re-arm that also clears the retry
counters removes the only bound that is on in a default deployment, because `agent.max_tokens` and
`agent.max_sessions` both default to unlimited, and a fix loop then runs until a person notices.

## Decision Drivers

1. **A conclusion belongs to a commit, not to a pull request.** Head currency settled this for the
   CI verdict. The same force applies to every other conclusion keyed on the head, and the answer
   should not differ by reaction kind for no reason other than which one was written first.
2. **Invalidation is not a dispatch trigger.** That a prior conclusion is void says nothing about
   whether new work is needed. An orchestrator that treats a push as a reason to wake an agent
   produces work nobody asked for and spends a budget nobody released.
3. **Remembered state can invalidate but must not attribute.** A stored "the commit I pushed"
   desynchronizes under concurrent updates, and a system that reads the divergence as evidence of a
   person's edit eventually tells an operator that someone changed a branch nobody touched. A record
   of a prior head is sound as a validity token, where a mismatch yields no answer, and unsound as
   an identity claim.
4. **The retry bound is the only bound enabled by default.** `reactions.<kind>.max_retries` defaults
   to 2, while both per-issue ceilings default to unlimited. A reset that fires on the
   orchestrator's own fix commit makes the retry bound vacuous and leaves the loop unbounded in the
   configuration most deployments run.
5. **A tracker write is public; internal state is not.** Moving an issue backwards changes its board
   position, notifies watchers, re-enters the dispatch matcher, and depends on a transition graph
   that is not reliably symmetric. Re-arming a watch is reversible, invisible, and costs one read.
6. **A spent budget should close the automatic path without locking the manual one.** The budget
   exists to stop an agent from spending without limit, not to make the pull request unserviceable.

## Considered Options

- Start a new epoch on each observed head change: re-arm only the reactions whose subject is the
  head, reset the watch clock unconditionally, and reset the attempt counters only when the
  orchestrator can positively establish that it did not produce the new head
- Keep the present lifecycle and require an explicit human gesture to restart anything
- Re-arm every reaction and reset every counter on any head change
- Attribute each head change by remembering the commit the orchestrator dispatched from and reading
  any other head as a person's work
- Move the tracker issue back to its pre-handoff state on a head change and let the ordinary
  dispatch path re-derive everything

## Decision Outcome

Chosen option: **start a new epoch on each observed head change**, because it re-derives exactly the
conclusions the head invalidates, keeps the retry bound meaningful by resetting it on evidence
rather than on suspicion, and needs no write to the tracker.

### The epoch

An epoch is the span over which a pull request's head is one commit. The orchestrator records the
head it last evaluated; reading a different head closes that epoch and opens a new one. Conclusions
reached in a closed epoch bind nothing in the new one.

Supersession and an unfinished check are separate conditions with separate handling. A check on a
commit the branch has moved past yields no verdict and the watch re-targets on the same pass. An
inconclusive check on the current head withholds green, produces no failing verdict, and the watch
keeps polling that same commit.

### What a new epoch re-arms

| Reaction                                | Re-armed | Reason                                                                                                                    |
| --------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------- |
| `reactions.ci_failure`                  | Yes      | Its subject is whether the head builds.                                                                                    |
| `reactions.merge_conflicts`             | Yes      | Its subject is whether the head merges into the base.                                                                      |
| `reactions.auto_merge`                  | No       | It already resolves every gate against the current head on each pass. A new head makes its own gates read not green and not approved. |
| `reactions.review_comments`             | No       | Armed by the arrival of a person's review, not by a commit.                                                                |
| `reactions.bot_review`                  | No       | Armed by the arrival of a bot's review. A reviewer that re-reviews on push arms it through that path.                      |
| `reactions.label_commands.review_label` | No       | A label is a person's gesture. A commit is not one.                                                                        |
| `reactions.label_commands.fix_label`    | No       | A label is a person's gesture. A commit is not one.                                                                        |
| `reactions.merge_completion`            | No       | Its subject is whether the managed pull request merged, which a commit on an open branch does not change.                  |

Leaving the two review kinds alone is also what stops an epoch from re-delivering findings the new
commits already addressed, which is the characteristic defect of feedback loops in this shape.

A new epoch dispatches nothing by itself. It re-arms a watch, and a continuation follows only where
that watch's own condition then holds: the head's checks fail, or the head conflicts with its base.
A push that builds clean and merges clean produces a new epoch and no work.

### Attribution: the orchestrator's own work, or unknown

The orchestrator classifies a head change as its own work or as unknown. There is no third answer,
and in particular there is no answer that names a person.

The recorded head is a validity token. When the current head matches it, nothing changed. When it
does not, the record is spent and yields no attribution on its own. A head change is positively
**not** the orchestrator's own work when no worker session for that issue ran between the recording
and the read, which run history answers without depending on the record staying synchronized. Every
other head change is unknown.

Unknown takes the conservative branch on each axis, and the two axes differ. It re-arms, because a
changed head genuinely voids prior conclusions and a needless re-arm costs one poll. It does not
reset a counter, because a needless reset costs an unbounded loop. Operator-facing text follows the
same rule: the orchestrator reports that the head changed, never that a person changed it.

### What resets and what persists

| Counter                                                | Resets | Condition                                       |
| ------------------------------------------------------ | ------ | ----------------------------------------------- |
| The watch clock that bounds the pending entry           | Yes    | Any head change.                                |
| Attempt count for `reactions.ci_failure`                | Yes    | Only when the change is positively not the orchestrator's own work. |
| Attempt count for `reactions.merge_conflicts`           | Yes    | Only when the change is positively not the orchestrator's own work. |
| Attempt counts of the kinds a new epoch does not re-arm | No     |                                                 |
| `agent.max_sessions`                                    | No     | Per-issue lifetime ceiling.                     |
| `agent.max_tokens`                                      | No     | Per-issue lifetime ceiling.                     |

The dividing line is not spend against counts. It is per-epoch budget against per-issue lifetime
ceiling. `agent.max_sessions` counts sessions rather than measuring spend and still does not reset,
because a ceiling that restarts is not a ceiling.

The condition on the attempt reset carries the whole safety argument. Without it, the agent's own
fix commit resets the counter that bounds the fix loop, that counter stops bounding anything, and
what remains is a spend ceiling that is unlimited unless an operator set it.

### The watch window

`reactions.ci_failure.watch_window_ms` bounds how long the orchestrator keeps watching a pull
request once every reaction condition has resolved. The age is measured from the last observed head
change rather than from the entry's creation, so an actively worked pull request stays watched and
only silence ages it out. The default is `86400000`, twenty-four hours: the smallest window that
covers the dominant real case, a reviewer who returns the next working day and pushes a fixup.
Setting `0` removes the clock bound.

The watch also ends, whatever the clock says, when the pull request merges or closes, or when the
tracker issue enters a state listed in `tracker.terminal_states`. Past the window the pull request
is not abandoned: applying `reactions.label_commands.fix_label` re-arms it by hand.

The window is configurable where the cancellation semantics settled in ADR-0023 deliberately are
not, and the difference between them is the rule. Configure what the operator knows and the system
cannot infer, such as how long a team takes to come back to a pull request. Decide in code what the
system reads better than the person writing the configuration file, such as how a forge reports a
superseded run.

### Coalescing, and the shape of exhaustion

Every evaluation reads the head at the moment it runs, so a burst of commits landing between two
passes is one epoch and produces one evaluation rather than one per commit. A continuation already
in flight is not cancelled when a newer head lands: the spend is committed either way, and the next
pass evaluates the newer head. One continuation in flight and one queued per issue is the existing
limit and this decision does not raise it.

Exhaustion is a per-epoch soft stop. When a reaction's attempt count passes its `max_retries` the
orchestrator applies the configured escalation once for that epoch and stops dispatching that
reaction automatically. It does not end the watch and it does not block a label command. A head
change that is positively not the orchestrator's own work opens a new epoch, restores the automatic
path, and permits one further escalation, which makes a person's push the gesture that lifts the
stop in the same way a tracker gesture releases a parked issue per ADR-0022. Under
`escalation: comment` this bounds the loop to one comment per epoch rather than one per pass.

### Considered Options in Detail

**Keep the present lifecycle.** Cheapest, and it cannot loop. It also leaves the orchestrator
holding conclusions about a commit that is no longer the head, which is the condition this decision
exists to remove, and it puts the cost of noticing on the operator every time.

**Re-arm everything and reset every counter.** This is the reading of "a new commit voids
everything" that requires no attribution at all, and it is the dangerous one. Resetting the attempt
count on the agent's own fix commit is an unbounded loop in the default configuration. Re-arming
the review kinds re-delivers comments the new commit already addressed. Re-arming the merge
completion watch asks a question whose answer a new commit cannot change.

**Attribute each head change to a person.** It is the obvious reading of "someone else pushed this",
and it fails in a way that is worse than being unavailable, because the failure is a confident false
statement to an operator. The stored head desynchronizes whenever two updates run concurrently, and
the divergence is then indistinguishable from a person's edit. Deriving the answer from the commits
and from run history on every pass, and answering unknown rather than guessing, costs one comparison
and cannot produce that failure.

**Move the tracker issue back to its pre-handoff state.** It re-derives everything with no new
mechanism, since the ordinary dispatch path would then run. It is rejected because it is the most
invasive available action for the least reversible reason. It writes to the system of record other
people and other automation read, thrashes board position on every fixup commit, re-enters the
dispatch matcher and so turns an invalidation into a dispatch, and it assumes a backward transition
that many tracker workflows do not permit. Re-arming is separable from moving the issue, and
separating them is what keeps a head change from dispatching an agent on its own.

## Consequences

### Positive

- A commit that lands after a passing verdict is evaluated rather than ignored, whoever pushed it.
- The retry bound keeps bounding. A fix loop cannot extend itself by pushing.
- No tracker write, so board position, watcher notifications, and dispatch matching are untouched by
  a head change.
- Exhaustion stops the automatic path and leaves the pull request serviceable by hand.
- A burst of commits costs one evaluation.

### Negative

- **The watch is longer and therefore costlier.** A pull request under review holds a pending entry
  for up to a day, and each reconcile pass reads its head.
- **A person's push after exhaustion silently restores the automatic path.** That is intended, and
  it means an operator who pushes a commit to a pull request they had escalated will see the agent
  start again with a fresh attempt budget.
- **Attribution is conservative in one direction.** A person's commit that lands while a worker
  session for that issue happens to be running is unknown, so the counters do not reset. The reset
  is lost, not the re-arm, and the label command remains available.
- **A new configuration field is permanent.** `reactions.ci_failure.watch_window_ms` cannot be
  withdrawn once deployments depend on it.
- **The epoch is observed only where a CI reaction is configured.** A deployment running no CI
  reaction keeps no head watch and gets no epoch, which is consistent, since there is no verdict to
  void, but it means the merge-conflict re-arm arrives only through that reaction's own polling.

## Untested Assumption

The design assumes that a commit the orchestrator pushes under its own credential produces an
observable check run. A forge suppresses workflow runs for events triggered by its own default
automation credential, and exempts the pull request opened, synchronize, and reopened events from
that suppression by creating those runs in a state a person must release. Neither rule is documented
to cover an application installation credential, which is what an orchestrator pushes with when it
does not use a personal one. If runs are suppressed or held for release, the watch on a self-pushed
head never resolves and the merge gate never goes green.

Confirm it before implementation, on a repository configured with `provider: github`:

1. Open a pull request whose branch was pushed with an application installation credential.
2. Push a follow-up commit to that same branch with that same credential.
3. Record which of three outcomes the follow-up produces: the run fires normally, the run is created
   in a state awaiting release by a person, or no run is created.

The first outcome confirms the assumption. The second makes the self-pushed head's verdict depend on
a human action and requires the watch to distinguish a held run from a pending one. The third means
the orchestrator cannot observe its own fix commits at all on that credential, and the credential
choice becomes part of this feature rather than a deployment detail.

## Confirmation

The decision is validated when all of the following hold:

1. A commit pushed to a managed pull request after its checks passed is evaluated, and a failing
   result on it produces a fix continuation.
2. A pull request whose checks pass on the new head after that same commit produces no continuation
   and no tracker write.
3. Repeated failing results across successive self-pushed fix commits escalate after
   `reactions.ci_failure.max_retries` fix continuations in total, not that many per commit.
4. A head change from a commit no worker session could have produced resets the attempt count, and
   one that overlapped a worker session does not.
5. No log line, comment, or label produced by a head change asserts that a person made the commit.
6. Applying the fix label to an escalated pull request dispatches a continuation, and re-escalation
   posts at most one comment per epoch under `escalation: comment`.
7. Three commits pushed within one poll interval produce one evaluation.
