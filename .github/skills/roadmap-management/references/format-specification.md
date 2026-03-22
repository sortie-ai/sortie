# TODO.md Format Specification

Exact structural rules for the Sortie project roadmap file. This specification
is derived from the established conventions in the existing TODO.md and the
project's AGENTS.md requirements. All agents writing to TODO.md must conform.

## Contents

- File structure
- Milestone format
- Task format
- Numbering scheme
- Line width and wrapping
- Indentation rules
- Verify section
- Ordering invariants
- Complete grammar (pseudo-BNF)

## File Structure

```
<title-line>
<blank-line>
<intro-paragraph>
<blank-line>
<milestone-1>
<blank-line>
<milestone-2>
...
```

**Title line:** `# Sortie Roadmap` — single H1, never changed.

**Intro paragraph:** 1-2 lines wrapped at 90 characters describing the overall
project. Ends with a blank line.

## Milestone Format

```
## Milestone N: Name
<blank-line>
<description-paragraph>
<blank-line>
<task-1>
<blank-line>
<task-2>
...
```

| Element | Rule |
|---|---|
| Heading | `## Milestone N: Name` — H2, number matches position (0-indexed) |
| Number | Integer starting at 0, sequential, no gaps |
| Name | Short noun phrase (2-5 words), title case |
| Description | 1-3 lines wrapped at 90 characters, describes scope and purpose |
| Blank lines | One blank line between the heading and description, one between description and first task, one between each task |

**Example:**

```markdown
## Milestone 6: Orchestrator Core

The polling loop, dispatch, reconciliation, retry, and state machine. This is the central
component. Uses mock adapters for tracker and agent - no real external calls.
```

## Task Format

A task is a multi-line block starting with a checkbox line:

```
- [x] N.M Description text that starts on the same line and may wrap to
      additional lines with exactly 6-space indentation aligned to the
      start of the description text.
      **Verify:** description of how to confirm the task is complete.
```

| Element | Rule |
|---|---|
| Checkbox | `- [x] ` (complete) or `- [ ] ` (incomplete) — exactly as shown, with trailing space |
| Task ID | `N.M` where N = milestone number, M = sequential within milestone (1-indexed) |
| Space after ID | Exactly one space between task ID and description start |
| Description | Imperative form ("Implement X", "Add Y", "Define Z"), self-contained |
| Continuation | Lines wrap at 90 characters, continuation indented exactly 6 spaces |
| Verify | `**Verify:**` on its own continuation line (6-space indent), followed by space and verification text |
| Blank line | One blank line after each task (before the next task or next milestone) |

### Checkbox States

| State | Syntax | Meaning |
|---|---|---|
| Incomplete | `- [ ] ` | Task not yet done |
| Complete | `- [x] ` | Task verified as done |

No other states exist. There is no "in progress" checkbox state.

## Numbering Scheme

**Milestone numbers:** Sequential integers starting at 0.
`Milestone 0`, `Milestone 1`, ..., `Milestone 10`.

**Task numbers:** `{milestone_number}.{sequence}` where sequence starts at 1
within each milestone and increments by 1.

Examples: `0.1`, `0.2`, ..., `6.1`, `6.2`, ..., `6.13`, `10.1`, `10.13`.

Rules:
- Task number prefix MUST match the milestone it appears under
- Sequence numbers MUST be contiguous (no gaps: 6.1, 6.2, 6.3 — not 6.1, 6.3)
- New tasks always get the next sequential number (append-only)
- Never renumber existing tasks

## Line Width and Wrapping

| Content | Target | Hard limit |
|---|---|---|
| Description text | 90 characters | 96 characters |
| Milestone headings | No hard limit | Keep concise |
| Code blocks within verify | No wrapping | May exceed limits |
| Lines with inline code | 90 characters | No hard limit |

Target 90 characters per line. The hard limit of 96 accommodates continuation
lines where breaking mid-word would reduce readability. Lines containing inline
code (backtick-wrapped identifiers, commands, paths) may exceed the hard limit
because breaking them harms copy-paste usability.

The limit applies to the visual line including leading whitespace. For the first
line of a task, this includes `- [ ] N.M ` prefix (~12-16 chars) plus
description text. For continuation lines, this includes the 6-space indent plus
text.

## Indentation Rules

| Line type | Indentation |
|---|---|
| Milestone heading | 0 spaces (column 1) |
| Milestone description | 0 spaces (column 1) |
| Task first line | 0 spaces (`- ` at column 1) |
| Task continuation | 6 spaces (aligns with description start after `- [x] `) |
| Verify line | 6 spaces + `**Verify:**` |

The 6-space indent aligns continuation text with the start of the description
on the first task line:

```
- [x] 0.1 Description starts here and may continue onto
      the next line, indented to align with "Description".
      **Verify:** confirmation criteria here.
```

Counting: `- [x] ` = 6 characters, so continuation starts at column 7 (6 spaces).

## Verify Section

Every task MUST have a `**Verify:**` section. This is a non-negotiable
requirement — tasks without verification criteria cannot be accepted.

The verify section describes HOW to confirm the task is complete. Valid
verification methods:

| Method | Example |
|---|---|
| Unit tests | "unit tests cover happy path, missing file, bad YAML" |
| Integration tests | "integration test with mock tracker confirms dispatch" |
| Command execution | "`make lint` and `make fmt` exit 0 with no warnings" |
| Build verification | "`go run ./cmd/sortie` prints version and exits 0" |
| Observable outcome | "document exists with endpoint references and auth requirements" |

The verify text follows the same wrapping and indentation rules as the task
description.

## Ordering Invariants

1. **Milestones are dependency-ordered.** Milestone N depends on Milestone N-1.
   Never reorder milestones.
2. **Tasks within a milestone are dependency-ordered.** Earlier tasks are
   foundational; later tasks build on them.
3. **Completed before incomplete.** Within a milestone, all `[x]` tasks precede
   all `[ ]` tasks. This is a natural consequence of sequential execution —
   not an artificial sorting rule.
4. **Fundamental to specific.** Within a milestone, tasks progress from
   infrastructure/types/interfaces to concrete implementations to integration
   to verification.

## Complete Grammar (Pseudo-BNF)

```
roadmap      = title NL NL intro NL NL milestone+
title        = "# " text
intro        = wrapped-line+
milestone    = heading NL NL description NL NL task (NL NL task)*
heading      = "## Milestone " INT ": " text
description  = wrapped-line+
task         = checkbox taskid SP description-body NL
               (INDENT continuation-line NL)*
               INDENT verify-line
checkbox     = "- [x] " | "- [ ] "
taskid       = INT "." INT
description-body = wrapped-text
continuation-line = wrapped-text
verify-line  = "**Verify:** " wrapped-text (NL INDENT wrapped-text)*
wrapped-line = text{1,96}    (target 90, hard limit 96)
wrapped-text = text{1,~80}   (96 minus 6-space indent minus prefix overhead)
INDENT       = "      "      (6 spaces)
NL           = "\n"
SP           = " "
INT          = [0-9]+
```
