#!/bin/sh
# enforce-style.sh — run Copilot CLI on Go source files to enforce the listed
# requirement documents, then format with golangci-lint fmt.
#
# Usage:
#   scripts/enforce-style.sh [OPTIONS] [FILE]
#
# Options:
#   --codestyle      apply go-codestyle rules
#   --documentation  apply go-documentation rules
#   --logging        apply go-logging rules
#   --help           show this message and exit
#
# When no guideline flag is given, all three are applied.
# FILE, if provided, limits processing to that single file instead of scanning
# every non-test file under internal/.
set -eu

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
die()   { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS] [FILE]

Run Copilot CLI on Go source files to enforce style guidelines, then format
with golangci-lint fmt.

Options:
  --codestyle      apply go-codestyle rules
  --documentation  apply go-documentation rules
  --logging        apply go-logging rules
  --help           show this message and exit

When no guideline flag is given, all three are applied.
FILE limits processing to a single file instead of all non-test files under
internal/.

Examples:
  $(basename "$0")
  $(basename "$0") --codestyle --logging
  $(basename "$0") internal/orchestrator/dispatch.go
  $(basename "$0") --documentation internal/orchestrator/dispatch.go
EOF
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
use_codestyle=0
use_documentation=0
use_logging=0
target=''

while [ $# -gt 0 ]; do
    case "$1" in
        --codestyle)     use_codestyle=1 ;;
        --documentation) use_documentation=1 ;;
        --logging)       use_logging=1 ;;
        --help)          usage; exit 0 ;;
        --*)             die "unknown option: $1" ;;
        *)
            [ -n "$target" ] && die "unexpected argument: $1"
            target="$1"
            ;;
    esac
    shift
done

# Default: all guidelines when none are explicitly selected.
if [ "$use_codestyle" -eq 0 ] && [ "$use_documentation" -eq 0 ] && [ "$use_logging" -eq 0 ]; then
    use_codestyle=1
    use_documentation=1
    use_logging=1
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# ---------------------------------------------------------------------------
# Build the requirements list from selected flags.
# ---------------------------------------------------------------------------
set --
[ "$use_codestyle" -eq 1 ]     && set -- "$@" "$REPO_ROOT/.github/instructions/go-codestyle.instructions.md"
[ "$use_documentation" -eq 1 ] && set -- "$@" "$REPO_ROOT/.github/instructions/go-documentation.instructions.md"
[ "$use_logging" -eq 1 ]       && set -- "$@" "$REPO_ROOT/.github/instructions/go-logging.instructions.md"

for req in "$@"; do
    [ -f "$req" ] || die "requirements file not found: $req"
done

req_count=$#

req_list=$(
    i=1
    for req in "$@"; do
        printf '  %d. %s\n' "$i" "$req"
        i=$((i + 1))
    done
)

# ---------------------------------------------------------------------------
# Build the file list.
# ---------------------------------------------------------------------------
tmpfile=$(mktemp)
trap 'rm -f "$tmpfile"' EXIT INT TERM

if [ -n "$target" ]; then
    [ -f "$target" ] || die "file not found: $target"
    printf '%s\n' "$target" > "$tmpfile"
else
    find "$REPO_ROOT/internal" -name "*.go" ! -name "*_test.go" | sort > "$tmpfile"
fi

total=$(awk 'END { print NR }' "$tmpfile")

printf 'Requirements : %d\n' "$req_count"
printf 'Files        : %d\n' "$total"

# ---------------------------------------------------------------------------
# Process each file.
# ---------------------------------------------------------------------------
n=0
while IFS= read -r file; do
    n=$((n + 1))
    printf '\n[%d/%d] %s\n' "$n" "$total" "$file"

    prompt=$(cat <<PROMPT
You are a strict Go code quality enforcer. Your only task is to review and fix
the Go source file below so it fully complies with every requirement document
listed. Apply every fix directly to the file on disk. Do not ask for
confirmation. Do not skip any violation. Do not summarize or explain — just
fix the code.

FILE TO PROCESS:
  $file

REQUIREMENT DOCUMENTS — read every file completely before touching the target file:
$req_list
STEPS — execute every step in order, no skipping:
  1. Read every requirement document in full.
  2. Read the target file.
  3. Identify every violation of every rule across all documents.
  4. Edit the file to fix every violation.

CONSTRAINTS:
  - Do not alter business logic, algorithms, or the public API shape.
  - Only style, naming, comments, and doc-strings are in scope.
PROMPT
)

    # Run copilot in a subshell rooted at the repository so it resolves project
    # context correctly. $prompt is passed as a copilot argument, not evaluated
    # by the shell again, so special characters in paths cannot cause injection.
    (
        cd "$REPO_ROOT"
        copilot -p "$prompt" \
            -s \
            --output-format json \
            --allow-all \
            --autopilot \
            --no-ask-user \
            --max-autopilot-continues 30
    )

    printf '  -> golangci-lint fmt %s\n' "$file"
    golangci-lint fmt "$file"
done < "$tmpfile"

printf '\nDone. Processed %d files.\n' "$total"
