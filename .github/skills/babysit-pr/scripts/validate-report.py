#!/usr/bin/env python3
"""
Validate a babysit-pr Step 6 summary against the required template.

This script is the feedback loop for Step 6: write -> validate -> fix ->
validate again. It catches the structural violations most likely to slip
past manual review and that most corrupt the audit trail downstream.

Checks:
  - Source header is present.
  - Context7 Evidence Log table is present with at least a header row.
  - All seven category sections are present (populated or `_(none)_`).
  - No `[C7-REQUIRED]` tags leaked from internal reasoning.
  - Every populated entry has `**[@reviewer, file:line]**` or
    `general feedback` at the start of the bullet.
  - Every `Deferred to Backlog` entry names a GitHub issue outcome
    (`#N`, `existing`, or `not added`).
  - Every `Rejected` entry cites Context7 or architectural evidence.

Usage:
  python3 validate-report.py <summary-file>

Exit codes:
  0  all checks pass
  1  violations found
  2  usage or file-not-found error
"""

import re
import sys
from pathlib import Path

REQUIRED_SECTIONS = [
    "### Source",
    "### Context7 Evidence Log",
    "### Applied",
    "### Deferred to Backlog",
    "### Skipped — Already Addressed",
    "### Skipped — Subjective",
    "### Rejected",
    "### Needs Discussion",
    "### Stale / Outdated",
]

INTERNAL_TAGS = ["[C7-REQUIRED]"]

BULLET_PATTERN = re.compile(r"^- \*\*\[", re.MULTILINE)
REVIEWER_CITATION = re.compile(
    r"^\- \*\*\[@[^,\]]+,\s*[^\]]+\]\*\*|^\- \*\*\[general feedback\]\*\*",
    re.MULTILINE,
)
GITHUB_OUTCOME = re.compile(r"#\d+|existing|not added", re.IGNORECASE)
EVIDENCE_KEYWORDS = re.compile(
    r"Context7|architecture|docs/architecture|AGENTS\.md|\bC7\b|"
    r"FALLBACK:\s*web|single-writer|layer boundary|CGo|adapter boundary|"
    r"workspace containment|spec",
    re.IGNORECASE,
)


def _section_body(text: str, header: str) -> str:
    """Return the text between `header` and the next `###` header or EOF."""
    start = text.find(header)
    if start == -1:
        return ""
    after = text[start + len(header):]
    next_header = after.find("\n### ")
    if next_header == -1:
        return after
    return after[:next_header]


def validate(path: Path) -> list[str]:
    text = path.read_text(encoding="utf-8")
    errors: list[str] = []

    for section in REQUIRED_SECTIONS:
        if section not in text:
            errors.append(f"Missing required section: {section}")

    for tag in INTERNAL_TAGS:
        if tag in text:
            count = text.count(tag)
            errors.append(
                f"Internal tag leaked into summary: {tag} "
                f"({count} occurrence(s)) — strip before finalizing."
            )

    deferred_body = _section_body(text, "### Deferred to Backlog")
    if deferred_body and "_(none)_" not in deferred_body:
        for match in BULLET_PATTERN.finditer(deferred_body):
            line_start = match.start()
            line_end = deferred_body.find("\n", line_start)
            line = deferred_body[line_start:line_end if line_end != -1 else None]
            if not GITHUB_OUTCOME.search(line):
                snippet = line[:100]
                errors.append(
                    f"Deferred entry missing GitHub outcome (#N, "
                    f"'existing', or 'not added'): {snippet!r}"
                )

    rejected_body = _section_body(text, "### Rejected")
    if rejected_body and "_(none)_" not in rejected_body:
        for match in BULLET_PATTERN.finditer(rejected_body):
            line_start = match.start()
            line_end = rejected_body.find("\n", line_start)
            line = rejected_body[line_start:line_end if line_end != -1 else None]
            if not EVIDENCE_KEYWORDS.search(line):
                snippet = line[:100]
                errors.append(
                    f"Rejected entry missing evidence citation (Context7 "
                    f"or architecture): {snippet!r}"
                )

    for section in REQUIRED_SECTIONS:
        if section in ("### Source", "### Context7 Evidence Log"):
            continue
        body = _section_body(text, section)
        if not body:
            continue
        if "_(none)_" in body:
            continue
        bullets = list(BULLET_PATTERN.finditer(body))
        if not bullets:
            continue
        for match in bullets:
            line_start = match.start()
            line_end = body.find("\n", line_start)
            line = body[line_start:line_end if line_end != -1 else None]
            if not REVIEWER_CITATION.match(line):
                snippet = line[:100]
                errors.append(
                    f"{section.strip()} entry missing "
                    f"`**[@reviewer, file:line]**` prefix: {snippet!r}"
                )

    return errors


def main() -> int:
    if len(sys.argv) != 2:
        print("Usage: python3 validate-report.py <summary-file>", file=sys.stderr)
        return 2

    path = Path(sys.argv[1])
    if not path.exists():
        print(f"File not found: {path}", file=sys.stderr)
        return 2

    errors = validate(path)
    if errors:
        print(f"FAIL: {len(errors)} violation(s):")
        for err in errors:
            print(f"  - {err}")
        print()
        print("Fix the summary and re-run.")
        return 1

    print("PASS: summary meets the required structure.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
