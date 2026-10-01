#!/bin/sh
# Report every gopls diagnostic, hints included, over the tracked Go files and
# fail on any the project has not already chosen to suppress. gopls reads
# neither .golangci.yml nor //nolint, so those decisions are re-applied here.
# Usage: lint-gopls.sh   (GOPLS overrides the gopls binary)

set -eu

GOPLS=${GOPLS:-gopls}

out=$(mktemp "${TMPDIR:-/tmp}/lint_gopls.XXXXXX")
trap 'rm -f "$out"' EXIT

# golangci-lint skips generated files, so they are not handed to gopls either.
git ls-files -z '*.go' |
	xargs -0 grep -L --null -E '^// Code generated .* DO NOT EDIT\.$' |
	xargs -0 "$GOPLS" check -severity=hint >"$out"

# nolint_for FINDING LINTER: true when the finding's line, or the line above
# it, carries a //nolint directive naming LINTER, as golangci-lint reads it.
nolint_for() {
	file=${1%%:*}
	rest=${1#*:}
	n=${rest%%:*}
	start=$((n > 1 ? n - 1 : 1))
	sed -n "${start},${n}p" "$file" |
		grep -qE "//nolint:([a-z0-9]+,)*$2([,[:space:]]|\$)"
}

suppressed() {
	case $1 in
	# .golangci.yml disables modernize's omitzero: it changes JSON marshaling.
	*': Omitempty has no effect on nested struct fields'*) return 0 ;;
	*': unused parameter: '*) nolint_for "$1" unparam ;;
	*) return 1 ;;
	esac
}

found=0
while IFS= read -r line; do
	[ -n "$line" ] || continue
	suppressed "$line" && continue
	printf '%s\n' "$line"
	found=1
done <"$out"

[ "$found" -eq 0 ]
