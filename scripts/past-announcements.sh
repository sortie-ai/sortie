#!/bin/sh
# Print the announcements of the latest published Sortie releases, newest first.
# A release body is the announcement followed by the footer GoReleaser appends;
# releases whose body is still a commit list are skipped.
#
# Usage: past-announcements.sh [version-to-skip]

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

limit=5
skip=${1:-}

require_tools gh jq

repo=${GITHUB_REPOSITORY:-}
if [ -z "$repo" ]; then
	repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
fi

releases=$(gh api "repos/${repo}/releases?per_page=100")

announcements=$(printf '%s\n' "$releases" | jq -r --arg skip "v${skip}" --argjson limit "$limit" '
	def note:
		((.body // "") | gsub("\r"; "") | split("\n")) as $lines
		| ([$lines | index("---"), index("## New Contributors")] | map(select(. != null)) | min // ($lines | length)) as $end
		| $lines[:$end] | join("\n") | sub("\\s+$"; "");
	[.[]
		| select((.draft or .prerelease) | not)
		| select(.tag_name != $skip)
		| select((.body // "") | startswith("## Changelog") | not)
		| select(note != "")
	][:$limit][]
	| "<release version=\"\(.tag_name | ltrimstr("v"))\" date=\"\(.published_at[:10])\">\n\(note)\n</release>"
')

if [ -z "$announcements" ]; then
	log "no release announcement found in ${repo}"
	exit 1
fi

printf '%s\n' "$announcements"
