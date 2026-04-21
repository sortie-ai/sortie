#!/usr/bin/env python3
"""
Validate a technical specification against the writing-specs skill structure.

Usage:
    validate_spec.py <path-to-spec.md>

Checks:
    - All six required sections are present and non-empty
    - Spec Compliance Check table has five populated rows
    - At least one Mermaid code block exists
    - Implementation steps have Verify conditions
    - Risk assessment table has at least one data row
    - File structure summary is present
    - No output style violations (function bodies, Symphony references)

Exit codes: 0 = pass, 1 = errors found
"""

import re
import sys
from pathlib import Path

REQUIRED_SECTIONS = [
    (r"##\s+1\.\s+Business Goal", "Section 1: Business Goal & Value"),
    (r"##\s+2\.\s+System Diagram", "Section 2: System Diagram (Mermaid)"),
    (r"##\s+3\.\s+Technical Architecture", "Section 3: Technical Architecture"),
    (r"##\s+4\.\s+Implementation Steps", "Section 4: Implementation Steps"),
    (r"##\s+5\.\s+Risk Assessment", "Section 5: Risk Assessment"),
    (r"##\s+6\.\s+File Structure Summary", "Section 6: File Structure Summary"),
]

COMPLIANCE_ROWS = [
    "architecture doc conformance",
    "adr compatibility",
    "milestone sequencing",
    "single-binary constraint",
    "adapter boundary",
]

SYMPHONY_PATTERNS = [
    re.compile(r"\bsymphony\b", re.IGNORECASE),
    re.compile(r"\belixir\b", re.IGNORECASE),
    re.compile(r"\bBEAM\b"),
    re.compile(r"\bGenServer\b"),
]

NAMING_VIOLATIONS = re.compile(
    r"\bjira_[a-z]|\bclaude_[a-z]|\bcodex_[a-z]|\bcopilot_[a-z]"
)


def extract_section(content: str, pattern: str, next_pattern: str | None) -> str:
    """Extract text between two section headers."""
    match = re.search(pattern, content, re.MULTILINE)
    if not match:
        return ""
    start = match.end()
    if next_pattern:
        next_match = re.search(next_pattern, content[start:], re.MULTILINE)
        if next_match:
            return content[start : start + next_match.start()].strip()
    return content[start:].strip()


def validate(spec_path: str) -> list[dict]:
    """Validate spec file. Returns list of {level, message} dicts."""
    issues = []
    path = Path(spec_path).resolve()

    def error(msg):
        issues.append({"level": "ERROR", "message": msg})

    def warn(msg):
        issues.append({"level": "WARN", "message": msg})

    def info(msg):
        issues.append({"level": "INFO", "message": msg})

    if not path.exists():
        error(f"File not found: {path}")
        return issues

    if not path.suffix == ".md":
        warn("Expected .md extension")

    content = path.read_text(encoding="utf-8")
    lines = content.splitlines()
    info(f"Spec: {path.name} ({len(lines)} lines)")

    # Check filename pattern
    if not re.match(r"Spec-[\w.-]+\.md$", path.name):
        warn(f"Filename '{path.name}' does not match Spec-{{TASK_NAME}}.md pattern")

    # Check required sections
    section_patterns = [p for p, _ in REQUIRED_SECTIONS]
    for i, (pattern, label) in enumerate(REQUIRED_SECTIONS):
        match = re.search(pattern, content, re.MULTILINE)
        if not match:
            error(f"Missing: {label}")
        else:
            next_pat = section_patterns[i + 1] if i + 1 < len(section_patterns) else None
            body = extract_section(content, pattern, next_pat)
            # Exclude HTML comments from content check
            body_clean = re.sub(r"<!--.*?-->", "", body, flags=re.DOTALL).strip()
            if len(body_clean) < 20:
                error(f"Empty or minimal: {label}")

    # Check Spec Compliance Check table
    table_match = re.search(
        r"###?\s+Spec Compliance Check", content, re.MULTILINE
    )
    if not table_match:
        error("Missing: Spec Compliance Check table")
    else:
        table_region = content[table_match.end() : table_match.end() + 2000]
        found_rows = 0
        blank_cells = 0
        for row_name in COMPLIANCE_ROWS:
            if re.search(re.escape(row_name), table_region, re.IGNORECASE):
                found_rows += 1
                # Check for empty cells (pipes with only whitespace)
                row_match = re.search(
                    rf"\|[^|]*{re.escape(row_name)}[^|]*\|([^|]*)\|([^|]*)\|",
                    table_region,
                    re.IGNORECASE,
                )
                if row_match:
                    aligned = row_match.group(1).strip()
                    notes = row_match.group(2).strip()
                    if not aligned or aligned in ("_/_", ""):
                        blank_cells += 1
                    if not notes or notes in ("_/_", ""):
                        blank_cells += 1
        if found_rows < len(COMPLIANCE_ROWS):
            error(
                f"Compliance table: {found_rows}/{len(COMPLIANCE_ROWS)} rows found"
            )
        if blank_cells > 0:
            warn(f"Compliance table: {blank_cells} cell(s) appear unfilled")

    # Check Mermaid diagram
    mermaid_blocks = re.findall(r"```mermaid\s*\n(.+?)```", content, re.DOTALL)
    if not mermaid_blocks:
        error("Missing: Mermaid diagram in Section 2")
    else:
        for i, block in enumerate(mermaid_blocks):
            if len(block.strip()) < 20:
                warn(f"Mermaid block {i + 1} appears too short to be meaningful")

    # Check implementation steps have Verify conditions
    section4 = extract_section(
        content,
        r"##\s+4\.\s+Implementation Steps",
        r"##\s+5\.\s+Risk Assessment",
    )
    if section4:
        # Count numbered steps and verify conditions
        steps = re.findall(r"^\d+\.\s+\*\*", section4, re.MULTILINE)
        verifies = re.findall(r"\*\*Verify[:\*]", section4, re.IGNORECASE)
        if steps and not verifies:
            error("Implementation steps have no **Verify:** conditions")
        elif steps and len(verifies) < len(steps):
            warn(
                f"Only {len(verifies)}/{len(steps)} implementation steps "
                f"have **Verify:** conditions"
            )

    # Check risk assessment table
    section5 = extract_section(
        content,
        r"##\s+5\.\s+Risk Assessment",
        r"##\s+6\.\s+File Structure Summary",
    )
    if section5:
        table_rows = re.findall(r"^\|[^|]+\|[^|]+\|[^|]+\|", section5, re.MULTILINE)
        # Subtract header and separator rows
        data_rows = [
            r for r in table_rows if not re.match(r"^\|\s*[-:]+\s*\|", r)
            and not re.search(r"Risk\s*\|.*Severity\s*\|.*Mitigation", r, re.IGNORECASE)
        ]
        if not data_rows:
            error("Risk assessment table has no data rows")

    # Output style violations
    for pat in SYMPHONY_PATTERNS:
        if pat.search(content):
            error(f"Output style violation: Symphony/Elixir/BEAM reference found ({pat.pattern})")
            break

    # Check for naming violations outside adapter context
    # Simple heuristic: flag if jira_/claude_ appear outside "adapter" context
    for match in NAMING_VIOLATIONS.finditer(content):
        # Get surrounding context (100 chars each side)
        start = max(0, match.start() - 100)
        end = min(len(content), match.end() + 100)
        context = content[start:end].lower()
        if "adapter" not in context and "integration" not in context:
            warn(
                f"Possible naming violation: '{match.group()}' outside adapter context "
                f"(near position {match.start()})"
            )

    # Summary
    errors_count = sum(1 for i in issues if i["level"] == "ERROR")
    warns_count = sum(1 for i in issues if i["level"] == "WARN")

    if errors_count == 0:
        info(f"Validation passed ({warns_count} warning{'s' if warns_count != 1 else ''})")
    else:
        info(
            f"Validation failed: {errors_count} error{'s' if errors_count != 1 else ''}, "
            f"{warns_count} warning{'s' if warns_count != 1 else ''}"
        )

    return issues


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: validate_spec.py <path-to-spec.md>")
        sys.exit(1)

    issues = validate(sys.argv[1])

    for issue in issues:
        level = issue["level"]
        msg = issue["message"]
        prefix = {"ERROR": "x", "WARN": "!", "INFO": "-"}[level]
        print(f"  [{prefix}] {msg}")

    has_errors = any(i["level"] == "ERROR" for i in issues)
    sys.exit(1 if has_errors else 0)
