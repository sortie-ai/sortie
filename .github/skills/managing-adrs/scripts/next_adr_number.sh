#!/usr/bin/env bash
# Outputs the next available ADR number(s) in docs/decisions/.
# Works regardless of current working directory — resolves docs/decisions/
# relative to this script's own location.
#
# Usage:
#   next_adr_number.sh              # prints one number, e.g. 0004
#   next_adr_number.sh --count 3    # prints three: 0004, 0005, 0006 (one per line)
#
# Exit codes:
#   0  success
#   1  docs/decisions/ not found

set -euo pipefail

# Resolve docs/decisions/ relative to this script:
# scripts/ → managing-adrs/ → skills/ → .github/ → <project root>/docs/decisions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DECISIONS_DIR="$SCRIPT_DIR/../../../../docs/decisions"
COUNT=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --count) COUNT="$2"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [[ ! -d "$DECISIONS_DIR" ]]; then
  echo "Error: docs/decisions/ not found at $DECISIONS_DIR" >&2
  exit 1
fi

# Find the highest existing ADR number
max=0
for f in "$DECISIONS_DIR"/[0-9][0-9][0-9][0-9]-*.md; do
  [[ -e "$f" ]] || continue
  num=$(basename "$f" | grep -oE '^[0-9]+' | sed 's/^0*//' )
  [[ -z "$num" ]] && num=0
  (( num > max )) && max=$num
done

# Output sequential numbers
for (( i = 1; i <= COUNT; i++ )); do
  printf "%04d\n" $(( max + i ))
done
