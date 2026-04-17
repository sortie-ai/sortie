# Per-Comment Classification Block — Template

Write this block verbatim for each comment before assigning a category.
Forcing yourself through every field catches comments that seem clear
but turn out to depend on an unverified library claim or an unstated
architectural assumption.

## Template

> **Comment:** (verbatim quote from the reviewer, including any code
> fragments they included)
>
> **References:** (file path, function name, or line number — or
> "general feedback" when no specific location is cited)
>
> **Library evidence:** (Evidence Table row number and verdict from
> Step 2c, e.g., "Row 3 — REVIEWER CORRECT — optional improvement" —
> or "N/A — not a library behavior claim")
>
> **Reasoning:**
> (a) What the reviewer asserts.
> (b) What Context7 confirms, refutes, or leaves ambiguous — or, when
>     library evidence is N/A, what the architecture document,
>     project conventions, or a logical analysis of the code says.
> (c) Net verdict.
>
> **Category:** (exactly one of the seven categories — see the list
> in SKILL.md Step 3 or the full rubric in the
> classification-categories reference loaded at Step 3)

## Filled example

> **Comment:** "This `db.ExecContext(ctx, ...)` call discards the
> returned `sql.Result` but may want `RowsAffected` — otherwise how do
> we know the update hit an existing row?"
>
> **References:** `internal/persistence/running.go:87`,
> `(*Store).markCompleted()`
>
> **Library evidence:** N/A — concern is about result-usage logic,
> not `database/sql` or `modernc.org/sqlite` behavior.
>
> **Reasoning:**
> (a) The reviewer asserts that `markCompleted` should check
>     `RowsAffected` to detect the case where the row is missing.
> (b) `docs/architecture.md` specifies that `markCompleted` is a
>     best-effort terminal write; a missing row means the orchestrator
>     already reconciled the state, which is a legal no-op. The
>     existing code treats a zero-row update as success. The reviewer
>     is reading it as a missed error, but the spec says it is an
>     expected outcome during concurrent reconciliation.
> (c) The reviewer's concern is reasonable at face value but
>     contradicts the specified behavior. No bug.
>
> **Category:** Incorrect or Counterproductive

## Discipline notes

- **Quote the comment verbatim.** Paraphrasing loses the reviewer's
  exact claim and invites the agent to argue with a strawman.
- **Cite file:line in References.** "general feedback" is allowed but
  should be rare — most review comments are line-anchored.
- **Library evidence must be N/A or a specific row number.** "I think
  Context7 would say…" is not evidence. If you need evidence you do
  not have, return to Step 2b and get it.
- **Reasoning runs (a) → (b) → (c) in order.** Do not skip (a) even
  when the assertion seems obvious. Writing it down catches
  misreadings.
- **Category must be exactly one.** If two categories both seem to
  apply, the comment is Needs Discussion — or you have not applied
  the decision rubric in the classification-categories reference
  correctly.
