# NITE

NITE is the Nightly Incident Triage Engine used by Sortie's nightly CI. It
classifies one integration-test sample, combines it with recent run history,
and returns the GitHub issue action selected by the consecutive-sample policy.

NITE reads JSON from standard input and writes its decision as JSON to standard
output. It is built only for CI tooling and is not included in the Sortie
binary.

## Reports

Each integration job writes its decision to the job summary and the job log.
The `nightly-<adapter>-<attempt>` artifact retains `summary.md` and
`decision.json` for 14 days, including when tests fail or the incident action
fails. `action` describes the selected action. If that action fails after the
decision is persisted, the job summary, artifact summary, and error annotation
all report the incomplete action separately from a missing decision.

The summary includes the tested version, passed/failed/skipped counts, coverage,
incident streaks, the action's reason, failure output, and skipped-test output.
Counts identify tests by package and name and exclude parent suites whose
results duplicate their children. A parent's own failure is retained when none
of its children failed. A passing parent with only skipped children is not an
executed test.

| Classification | Meaning |
| --- | --- |
| `pass` | Executed tests passed. Coverage can still be partial. |
| `test_failure` | A test or package failed; the cause is in the failure evidence. This replaces `contract`, which implied an established adapter defect. |
| `environment` | Setup failed without a usable test result. |
| `not_a_sample` | No tests executed; the sample does not affect the incident streak. |

Coverage is `complete` when executed tests have no skips, `partial` when some
were skipped, and `none` when none executed. These values describe the selected
suite, not all possible integration behavior. Partial coverage does not change
the incident policy. Prior samples are still inferred from GitHub job
conclusions, which do not carry test counts or coverage.

Failure excerpts prefer failing-test output over later successful-test output
and retain at most 6,000 bytes. A first failure below the incident threshold
produces a notice; the job still fails. Errors reading history or applying an
incident action remain errors.
