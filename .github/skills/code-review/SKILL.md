---
name: code-review
description: "Reviews pull requests in this repository for the defect classes a mechanical checklist misses: documentation that outlived the code it describes, reaction and retry state that leaks or clobbers a sibling, contract widenings that leave test doubles silently asserting nothing, kind-keyed maps missing a new entry, configuration literals that mean opposite things in adjacent blocks, error-taxonomy routing gaps, and wire assumptions that break on pagination. Use on every pull request review in addition to the coding standards in copilot-instructions.md. Covers Go orchestrator code, adapter packages, the architecture specification under docs/architecture/, accepted decision records, and operator-facing strings."
license: MIT
---

# Reviewing sortie pull requests

`.github/copilot-instructions.md` covers the mechanical checks: layering, concurrency, path containment, SQLite rules, error wrapping, resource lifecycle, adapter boundaries, style. Do not repeat them. This skill covers what those checks cannot see: defects that are locally correct and globally wrong.

Two rules govern every finding.

**Verify before flagging.** Open the file and read the line. A finding that cites a line that says something else costs the maintainer more than silence. State the evidence inline: file, symbol, and what it actually does.

**One grounded finding beats five speculative ones.** Uncertain findings are noise. If a check below needs a fact you cannot establish from the diff plus the files it touches, say what you could not verify instead of guessing.

## Start from the pull request description

`.github/pull_request_template.md` makes the author declare Intent, Sensitive Areas, Breaking Changes, and Migrations. Treat each as a claim to audit, not as context to absorb.

- **Intent says opt-in or default-off.** Find the branch that makes it inert when unconfigured, and confirm nothing outside it changed behavior. An opt-in feature that alters a default path is the failure this claim hides.
- **Sensitive Areas names a file.** Review it first and hardest. An empty Sensitive Areas on a diff that touches orchestrator state, an adapter boundary, or workspace removal is itself a finding.
- **Breaking Changes says none.** Check exported signatures, domain struct fields consumed by adapters, config field names and defaults, and log or metric names an operator may depend on.
- **Migrations says none.** Check `internal/persistence/sql/` and any new column read.

## Documentation is part of the change

The specification under `docs/architecture/` is the contract implementation follows, so drift is a defect, not a nicety. For any behavior change, find the section that describes that behavior and confirm the PR updates it.

- **A documented guarantee with no enforcing line.** When a document claims a safety property ("marked dispatched synchronously, which prevents duplicate dispatch"), locate the code that provides it. If the code does it later, elsewhere, or not at all, the document is wrong and the PR that touched the area should fix it.
- **Absolute claims.** "No path releases X", "the only mechanism", "always", "never". One counterexample falsifies these permanently, and they rot silently. Ask whether the diff introduces a second path that makes an existing absolute false.
- **Counts in prose.** "has nine parts", "three tables", "two packages implement". A new pass, table, or implementation makes them false and nobody notices. Flag the count, and propose a formulation that states the ordering or the property instead.
- **Enumerations that must grow.** Adapter lists, reaction kind lists, supported-forge tables. If the PR adds a member, every enumeration of that set is a review target.
- **Operator-visible surface, separate repository.** The docs site lives outside this repository. A PR adding a config field, environment variable, CLI flag, or reaction kind cannot update it here. Flag the gap so it is tracked; do not ask for a file this repository does not hold.
- **Accepted decision records are immutable.** `docs/decisions/` records what was decided and why. Do not propose adding issue numbers, section numbers, or line numbers to one; do not re-litigate a decision recorded there. If the code contradicts an accepted record, that is the finding.

## Orchestrator state: the defects that survive unit tests

State lives in maps keyed by issue and reaction kind, mutated across passes that run in a fixed order each tick. Locally correct edits break neighbors.

- **The retry slot is one per issue.** `ScheduleRetry` cancels any queued retry for that issue first. Any pass that schedules a retry discards whatever another kind had queued, along with its continuation context. When a diff adds or moves a `ScheduleRetry` call, ask what it displaces and whether the victim can re-detect its own work.
- **Cleanup must be scoped to the escalating kind.** An issue-wide clear takes sibling reactions' entries, counters, and fingerprint rows with it. A kind parked in a handoff state has no worker exit left to reseed it, so the loss is permanent until restart.
- **Every pending entry needs a release path.** For a new or modified reaction kind, establish who seeds the entry, what drops it (age, terminal state, or its own completion), and what happens across a restart. A kind with no expiry and no terminal check leaks the entry and polls the forge forever.
- **Claim and counter are not the entry.** `state.Claimed`, `state.ReactionAttempts`, and `state.PendingReactions` have independent lifetimes. A change that releases one is not evidence the others are released.
- **Kind-keyed maps must gain the new member.** A new reaction kind needs an entry wherever kinds are enumerated: pinning behavior, TTL wiring, recovery seeding, validation, metrics labels. Grep the kind constant across the package; a missing entry usually means a silent default, not a compile error.
- **The fingerprint protocol has an order.** Upsert the fingerprint, read it back, act, and mark dispatched only after the action succeeded. Marking before the action loses the retry on a transient failure; deleting the row on success re-arms the same observation on the next tick; retaining it blocks a legitimate second episode. Judge which of the three the diff wants, then check it does that.

## Adapter contracts

- **Widening a domain type reaches every implementation and every double.** A new field on a shared struct must be populated by each adapter and by each fake in tests. A double that leaves it at the zero value asserts the negative case forever: a bool stays false, a string stays empty, and the test passes while covering nothing.
- **Fixtures must match the live wire shape.** Response fixtures built from a hand-written guess hide real bugs: a dropped cursor field, a team key placed in a UUID field, a status list that arrives paginated. Prefer a fixture derived from a recorded response.
- **Pagination is not optional.** A single `GET` against a route that paginates silently truncates at the page size. Any new list read needs the pagination walk, and the review question is what the route's default page size is.
- **Text-sniffing a message is a wire dependency.** Matching on a substring of an error body works until the forge rewrites the sentence. Flag it when introduced; when it already exists, flag only if the diff extends it.
- **Adapters normalize at the boundary.** A forge-specific field name, status string, or flag reaching orchestrator code is a leak even when it compiles.

## Configuration and validation

- **The same literal can mean opposite things.** A zero budget disables count-based escalation in one reaction and escalates on first detection in its sibling. When a diff adds a numeric field, compare its semantics against the adjacent block that shares the name.
- **Two environment mechanisms, never conflated.** The override registry maps one `SORTIE_*` variable to one config field and replaces the YAML value. `$VAR` indirection is a workflow-authored reference expanded at load. A change to one is not a change to the other, and documentation that credits the wrong one is a finding.
- **Offline validation versus construction.** `sortie validate` performs config-shape checks without network access. Credential, project, and state resolution happen when the adapter is constructed. A check placed in the wrong layer either fails offline or never runs.
- **An earlier error can shadow a later arm.** If the config layer rejects a combination before the adapter validator sees it, that validator arm is unreachable end to end. Flag a test that claims to cover it through the full path.
- **Durations end in `_ms` here.** A field measured in another unit needs a stated reason, because the reader will assume milliseconds.

## Error handling beyond wrapping

- **Route every class.** Transport and API errors back off and retry; auth and payload errors are deterministic and escalate without consuming the retry budget; not-found stops. A new call site that collapses these into one branch spends the budget proving a permission error is permanent.
- **A silent early return blinds the operator.** A pass that returns without a log leaves no way to tell "working, nothing to do" from "misconfigured, doing nothing". Periodic passes that remove or dispatch nothing should still report why.
- **Degrade-to-continue needs somewhere to continue to.** A failure posture copied from a path that has a retry loop is wrong on a path that has none.

## Tests

- **A regression test must fail without the fix.** For a bug fix, look for the assertion that the old code would violate. A test that passes both ways documents behavior; it does not lock the fix.
- **Zero-value doubles.** See the contract-widening note above: this is the most common way a sortie test passes while asserting nothing.
- **Env-gated integration tests skip cleanly.** Without `SORTIE_<ADAPTER>_TEST=1` they must skip, never fail.

## Operator-facing strings

Help text, warnings, and report output are read by operators who never see the schema. A message naming a table, a column, or a code path is a finding. Configuration keys the operator writes are fine.

## Do not raise these

- **Go version idioms.** The module targets a modern Go release. Range-over-int, per-iteration loop variables, and the standard library added in recent releases compile. Do not claim otherwise; CI is the arbiter.
- **Deliberate decisions recorded in `docs/decisions/`.** Disagreeing with an accepted record is not a review comment.
- **Established patterns repeated correctly.** A new pass that mirrors its five siblings is not a finding because you would have written it differently.
- **Equivalent rewrites.** Style preferences with no behavioral difference.

## Severity

Use the ladder at the end of `.github/copilot-instructions.md`. Two additions specific to this skill: documentation that states a guarantee the code does not provide is Major, not Minor, because the next implementer builds on it; a leaked or clobbered state entry is Major even when no test fails, because its symptom is a workflow that never completes.
