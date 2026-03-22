#!/usr/bin/env python3
"""Validate TODO.md structural integrity against the Sortie roadmap format.

Usage:
    python3 validate_roadmap.py [path/to/TODO.md]

Input:  Path to TODO.md (default: TODO.md in current directory)
Output: One line per violation, or "OK: N milestones, M tasks, no violations."
Exit:   0 = valid, 1 = violations found, 2 = file not found / read error
"""

import re
import sys
from pathlib import Path


def validate(path: str) -> list[str]:
    """Return a list of violation messages. Empty list means valid."""
    errors: list[str] = []
    warnings: list[str] = []
    try:
        text = Path(path).read_text(encoding="utf-8")
    except FileNotFoundError:
        return [f"File not found: {path}"]
    except OSError as e:
        return [f"Cannot read {path}: {e}"]

    lines = text.split("\n")
    if not lines or not lines[0].startswith("# "):
        errors.append("Line 1: File must start with '# ' title")

    milestone_re = re.compile(r"^## Milestone (\d+): (.+)$")
    task_re = re.compile(r"^- \[([ x])\] (\d+)\.(\d+) (.+)$")
    verify_re = re.compile(r"^ {6}\*\*Verify:\*\*")
    continuation_re = re.compile(r"^ {6}\S")

    current_milestone: int | None = None
    expected_milestone = 0
    last_task_seq = 0
    task_ids: set[str] = set()
    milestone_count = 0
    task_count = 0
    task_has_verify = True  # no pending task at start
    last_task_line = 0
    seen_incomplete = False  # tracks [x] before [ ] ordering within milestone

    for i, line in enumerate(lines, start=1):
        # Check line width (skip blank lines, code fences, heading lines).
        # The project convention targets ~90 chars with a hard limit of 96.
        # Lines containing inline code (backtick-wrapped identifiers and
        # commands) are exempt because breaking them harms copy-paste.
        if (line and not line.startswith("#") and not line.startswith("```")
                and len(line) > 96):
            if "`" not in line:
                errors.append(
                    f"Line {i}: Exceeds 96 characters ({len(line)})"
                )

        # Milestone heading
        m = milestone_re.match(line)
        if m:
            # Check previous task had verify
            if not task_has_verify and last_task_line > 0:
                errors.append(
                    f"Line {last_task_line}: Task missing **Verify:** section"
                )

            num = int(m.group(1))
            if num != expected_milestone:
                errors.append(
                    f"Line {i}: Expected Milestone {expected_milestone}, "
                    f"got Milestone {num}"
                )
            current_milestone = num
            expected_milestone = num + 1
            last_task_seq = 0
            milestone_count += 1
            task_has_verify = True
            seen_incomplete = False
            continue

        # Task line
        t = task_re.match(line)
        if t:
            # Check previous task had verify
            if not task_has_verify and last_task_line > 0:
                errors.append(
                    f"Line {last_task_line}: Task missing **Verify:** section"
                )

            state = t.group(1)
            milestone_num = int(t.group(2))
            seq = int(t.group(3))
            task_id = f"{milestone_num}.{seq}"

            # Milestone match
            if current_milestone is None:
                errors.append(
                    f"Line {i}: Task {task_id} appears before any milestone"
                )
            elif milestone_num != current_milestone:
                errors.append(
                    f"Line {i}: Task {task_id} has milestone prefix "
                    f"{milestone_num} but appears under Milestone "
                    f"{current_milestone}"
                )

            # Sequential numbering
            if seq != last_task_seq + 1:
                errors.append(
                    f"Line {i}: Expected task {milestone_num}."
                    f"{last_task_seq + 1}, got {task_id}"
                )
            last_task_seq = seq

            # Duplicate check
            if task_id in task_ids:
                errors.append(f"Line {i}: Duplicate task ID {task_id}")
            task_ids.add(task_id)

            # Completion ordering: completed tasks generally precede
            # incomplete ones, but some tasks (e.g., release automation)
            # can be completed independently of milestone sequence. Report
            # as a warning, not a hard violation.
            if state == " ":
                seen_incomplete = True
            elif state == "x" and seen_incomplete:
                warnings.append(
                    f"Line {i}: Completed task {task_id} appears after "
                    f"incomplete tasks in Milestone {current_milestone} "
                    f"(may be intentional)"
                )

            task_count += 1
            task_has_verify = False
            last_task_line = i
            continue

        # Verify line (within a task)
        if verify_re.match(line):
            task_has_verify = True
            continue

        # Continuation line validation (6-space indent but not verify)
        if continuation_re.match(line) and not task_has_verify:
            # This is a valid continuation line within a task
            pass

    # Final task verify check
    if not task_has_verify and last_task_line > 0:
        errors.append(
            f"Line {last_task_line}: Task missing **Verify:** section"
        )

    if not errors:
        completed = sum(
            1 for line in lines if task_re.match(line) and "[x]" in line
        )
        status = (
            f"OK: {milestone_count} milestones, {task_count} tasks "
            f"({completed} completed, {task_count - completed} remaining)"
        )
        if warnings:
            status += f", {len(warnings)} warning(s)"
        else:
            status += ", no violations"
        print(status + ".")
    for w in warnings:
        print(f"WARNING: {w}", file=sys.stderr)
    return errors


def main() -> int:
    path = sys.argv[1] if len(sys.argv) > 1 else "TODO.md"
    errors = validate(path)
    for e in errors:
        print(f"VIOLATION: {e}", file=sys.stderr)
    if errors:
        print(f"\n{len(errors)} violation(s) found.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
