---
status: accepted
date: 2026-08-19
decision-makers: Serghei Iakovlev
---

# Scope the CI Verdict to the Pull Request's Current Head

## Context and Problem Statement

The aggregate CI verdict and the source-control merge gate derive from one shared rule over a check
run's normalized conclusion. One rule serves both readers so that a forge cannot answer "is CI
green" two ways. That rule sorted every completed conclusion into failing or not failing, and it
counted cancelled as failing.

The CI-failure reaction paired that rule with a commit it could not change. The reaction captured a
git ref once, from the workspace's source-control metadata at worker exit, and polled that same ref
for the life of its pending entry. The ref never advanced.

The two properties combined badly. A required workflow configured to cancel its own in-flight runs
when a newer commit lands is a standard concurrency setting on hosted CI, and an actively worked
pull request triggers it on every push. Each push left a completed check whose conclusion was
cancelled, on a commit nobody intended to fix. The verdict read that as a failing pipeline,
dispatched a fix continuation, and spent one of the retries bounded by
`reactions.ci_failure.max_retries`. In one observed deployment both configured retries went exactly
this way: a genuine failure on a later commit got no remediation at all, the issue stayed in its
review state with nothing acting on it, and run history recorded failures on refs where nothing had
failed. The escalation that fires on budget exhaustion then named those cancelled checks as the
reason manual intervention was required.

The frozen ref cost a second thing, reported independently. After the first passing result the
reaction retired its pending entry, so a commit pushed afterwards that genuinely failed CI was never
observed. The remediation an operator pays for was unavailable in both directions: spent on commits
that were not broken, absent on the ones that were.

## Decision Drivers

1. **A verdict describes a commit.** "Is CI failing" has no answer for a pull request in general; it
   has an answer for a specific commit. A completed check on a commit the branch has moved past is a
   true statement about work nobody intends to change, and it was being read as a statement about
   the pull request.
2. **Head currency was already accepted, on one of the two readers.** One forge's merge gate maps a
   head pipeline to a gate verdict only when that pipeline describes the merge request's current
   head, and defers otherwise. The principle was settled; it had not reached the second reader.
3. **Every other pull-request reaction already follows the head.** The review, auto-merge, and
   merge-conflict reactions carry the pull request identity, re-resolve its current head on each
   pass, and fingerprint on it. CI was the only reaction holding a commit captured once, which makes
   it a lagging participant rather than a deliberate asymmetry.
4. **The reaction fingerprint was designed for a moving ref.** It clears the dispatched mark
   whenever the ref changes, which re-arms the reaction on the next push. It never fired because the
   ref could not change.
5. **One cancelled conclusion covers several distinct events.** A run a person stopped deliberately,
   a run killed because a sibling job in the same workflow failed, and a run superseded by a newer
   commit all complete with the same conclusion, and none of the three states that the code under
   test is broken. The fail-fast case does carry that statement, on the sibling check, which
   concluded as a failure in its own right. Cancellation is also partial: one head can carry
   cancelled checks alongside checks that completed successfully, so a cancelled check neither
   proves nor disproves that its commit was superseded.
6. **The retry budget is the scarce resource.** It is bounded on purpose, and every
   misclassification that spends one removes remediation from the moment it is needed.

## Considered Options

- Scope the verdict by head currency: a completed check on a commit that is no longer the pull
  request head yields no verdict for that pull request, the CI-failure reaction evaluates the
  current head, and a cancelled check on that head is inconclusive rather than failing
- Classify by conclusion alone: move cancelled out of the failing set for every forge
- Discriminate at the forge adapter: read what the platform exposes about how a run ended and
  normalize the distinction into the check-run domain type
- Make the treatment configurable, so a deployment whose pipeline cancels routinely declares it

## Decision Outcome

Chosen option: **scope the verdict by head currency**, because it removes both defects with one
rule, leaves a single aggregate shared by both readers and every forge, and asks about the right
commit instead of asking a conclusion to carry information it does not hold.

### The rule

A completed check on a commit that is no longer the pull request's head describes superseded work
and produces no CI verdict for that pull request. The CI-failure reaction evaluates the pull
request's current head on each pass rather than a commit fixed at worker exit.

Two conclusions are failing, on every forge: failure and timed out. Cancelled is inconclusive. A
cancelled check states that its run reached no conclusion, which is neither a passing nor a failing
statement about the commit, so it contributes to neither.

The aggregate over a head answers in this order. Any check concluding failure or timed out makes the
head failing. Otherwise, any check still queued or running, and any check that concluded cancelled,
makes the head pending. Otherwise the head is passing. A head carrying three cancelled checks and
one successful one is pending: not green, because checks that never ran cannot grant green, and not
failing, because nothing failed.

Pending and superseded are different conditions with different handling. A superseded head yields no
verdict at all, including no passing verdict from a successful check on it, and the reaction
re-targets the current head on the same pass. A pending head is still the subject, and the reaction
keeps polling that same commit under the existing backoff.

In one line: a cancelled run withholds green rather than asserting failure, and a completed check on
a commit the branch has moved past is not this pull request's verdict at all.

### Currency is not a property of the conclusion

The CI verdict and the merge gate keep answering from one shared conclusion rule, so neither reader
gains a private definition of failing. Currency is a property of which commit the question is asked
about, and the caller that knows the pull request identity decides it. The read-only CI status
contract continues to answer about a caller-supplied ref and gains no knowledge of pull requests.
The layer that already holds pull request identity is the layer that resolves the head, which is why
this rule needs no new vocabulary in the classifier and no new field on the check-run type.

### The mechanism already exists

Nothing here is a new concept for this system. One forge's merge gate already refuses a gate verdict
from a pipeline that does not describe the current head. The sibling reactions already re-resolve
the head on every pass. The fingerprint that prevents a second dispatch for the same ref already
clears itself when the ref changes, and was inert only because the ref could not move. The decision
extends an accepted rule to the one reader that had not adopted it, and by doing so it closes two
independently reported defects with a single rule rather than two unrelated patches.

### Considered Options in Detail

**Classify by conclusion alone.** Uniform, cheap, and confined to one predicate. It is rejected for
where it puts cancelled, not for moving it out of the failing set. Deferral is decided by a run's
status and not its conclusion, so a completed run whose conclusion merely leaves the failing set
counts toward passing: a head whose checks were all cancelled would read green, the merge gate would
merge a pull request whose required checks never ran, and a passing verdict would retire the episode
outright. Withholding green keeps the protection the failing classification provided while dropping
the false claim it made. This option also leaves the frozen ref untouched, so the reaction would
still stop watching after the first passing result and still never see a later failing commit.

**Discriminate at the forge adapter.** Reading what actually happened to a run is the most faithful
option. It costs a new field on the check-run domain type that every provider must implement, plus
an extra read per poll against a rate-limited API. The decisive objection is that not every platform
can answer how a run ended, so identical facts would produce different verdicts on different forges,
which is the property the shared rule exists to prevent. It is rejected as the primary mechanism and
not foreclosed. It remains available later as an additive refinement that would let the watch tell a
cancellation that will never re-run from one a re-run is coming for, and so stop polling a dead head
instead of waiting out the window.

**Make the treatment configurable.** The default it would ship is this same decision under another
name, so the configuration surface buys nothing at the setting's default and pushes a correctness
question onto the operator at every other value. It asks a deployment to declare its forge's
cancellation semantics, which the system is better placed to determine than the person writing the
configuration file. A configuration surface is also permanent once shipped, while the wrong default
is the thing being corrected here.

## Consequences

### Positive

- Routine pushes stop draining the retry budget. What the operator buys with a bounded budget is
  spent on the commit they can act on.
- A commit pushed after a passing result is still watched, so a later genuine failure produces a fix
  continuation instead of silence.
- Run history stops asserting failures on refs where nothing failed, and the escalation stops naming
  checks that were merely superseded.
- One definition of failing survives, across both readers and every forge.
- A run stopped by hand no longer wakes an agent to fix a run that reported no failure.
- The CI reaction joins the reaction kinds keyed on pull request identity, so all of them share one
  model of what a reaction watches and when it re-arms.

### Negative

- **The reaction reads the pull request's head on every pass.** That is an additional request per
  poll against a rate-limited API, on top of the check read it already performs.
- **The CI reaction's durable state must carry pull request identity, which it did not before.** The
  reaction previously needed only a ref. The identity must survive process restart and recovery, or
  the reaction resumes unable to resolve a head.
- **The pending entry must outlive a passing result.** Retiring it on the first pass was what made
  the watch cheap and self-limiting. The watch window becomes an explicit, bounded design parameter
  that has to be chosen and defended (per ADR-0024), rather than an accident of the entry expiring.
- **A narrow race remains.** A commit landing between resolving the head and reading that head's
  checks yields a verdict about a head that has just been superseded. The next pass corrects it, and
  the fingerprint re-arms on the new ref, so the error is transient rather than sticky.
- **Operator-facing escalation text must follow the same rule.** Text that attributes budget
  exhaustion to cancelled checks will otherwise name checks the verdict no longer counts, and an
  escalation that disagrees with the verdict that produced it is worse than a terse one.
- **A head whose checks all ended cancelled reaches no verdict on its own.** It is not failing, so
  no agent is woken, and it is not green, so the merge gate holds. It resolves only when someone
  re-runs the checks or a newer commit lands, and until then the watch keeps polling it under
  backoff. A pull request can therefore sit un-merged with nothing visibly wrong with it, which is
  the price of refusing to grant green to checks that never ran.

## Specification Material Requiring Update

Named by document and topic rather than by section or filename, since neither is stable.

1. The CI feedback contract material: failure and timed out are the failing conclusions, cancelled
   is inconclusive and withholds green without asserting failure, one aggregate still serves both
   readers, and currency is added as a separate property decided by the caller that knows the pull
   request, not by the conclusion.
2. The reaction material: the CI reaction's subject is the pull request rather than a ref captured
   at worker exit, the head is re-resolved on each pass, the fingerprint re-arms when the head
   moves, and the watch window is bounded.
3. The persistence material: pull request identity in the CI reaction's durable state, and its
   survival across restart and recovery.
4. The operator-facing reference material for the CI-failure reaction: what spends a retry and what
   does not, and the escalation text that names failing checks.

## Confirmation

The decision is validated when all of the following hold:

1. A pull request whose workflow cancels superseded runs spends no CI-failure retry on those
   cancellations and records no failure in run history for them.
2. A check that completes as a failure or a timeout on the pull request's current head still
   produces a failing verdict and a fix continuation.
3. A current head carrying cancelled checks and no failing check produces no fix continuation, no
   run history failure row, and no green verdict for the merge gate.
4. A commit that genuinely fails CI after an earlier commit passed is observed, within the watch
   window, and produces a fix continuation.
5. The verdicts for failure and timed out conclusions are unchanged on every forge that can produce
   them.
6. The escalation raised on budget exhaustion names exactly the checks the verdict counted as
   failing.
7. The CI reaction resumes after a restart with enough state to resolve the pull request's head.
