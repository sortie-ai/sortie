#!/bin/sh
# Build the "New Contributors" section of the release notes and export it for
# the release.footer template in .goreleaser.yaml.
#
# Usage:
#   scripts/release-contributors.sh <tag> [previous-tag]

set -eu

tag=${1:?usage: release-contributors.sh <tag> [previous-tag]}
prev=${2:-}

# Machine accounts whose logins lack the "[bot]" suffix; that suffix is filtered
# on its own.
BOT_LOGINS='
Copilot
goreleaserbot
'

# Alternate accounts folded into one credit; "<alias> <canonical>" per line.
LOGIN_ALIASES='
serghei-dev sergeyklay
'

NEWLINE='
'

log() {
	printf 'release-contributors: %s\n' "$*" >&2
}

# Rewrites the login in field 1, so it serves both the "<login>" and the
# "<login> <pr>" streams.
canonicalize() {
	awk -v aliases="$LOGIN_ALIASES" '
		BEGIN {
			n = split(aliases, lines, "\n")
			for (i = 1; i <= n; i++)
				if (split(lines[i], f, " ") == 2)
					alias[f[1]] = f[2]
		}
		NF { if ($1 in alias) $1 = alias[$1]; print }
	'
}

if ! command -v gh >/dev/null 2>&1; then
	log "the GitHub CLI (gh) is required"
	exit 1
fi

repo=${GITHUB_REPOSITORY:-}
if [ -z "$repo" ]; then
	repo=$(gh repo view --json nameWithOwner -q .nameWithOwner)
fi

if [ -z "$prev" ]; then
	if git rev-parse -q --verify "refs/tags/${tag}" >/dev/null; then
		prev=$(git describe --tags --abbrev=0 "${tag}^" 2>/dev/null || echo '')
	else
		prev=$(git describe --tags --abbrev=0 HEAD 2>/dev/null || echo '')
	fi
fi

# Both endpoints accept a tag that does not exist yet, given a commitish, so this
# runs before the tag is pushed and a failure leaves no orphaned tag behind.
target=$(git rev-parse HEAD)

log "collecting contributors for ${tag} (previous: ${prev:-none}) in ${repo}"

if [ -n "$prev" ]; then
	notes=$(gh api -X POST "repos/${repo}/releases/generate-notes" \
		-f tag_name="$tag" \
		-f previous_tag_name="$prev" \
		-f target_commitish="$target" \
		-q .body)
else
	notes=$(gh api -X POST "repos/${repo}/releases/generate-notes" \
		-f tag_name="$tag" \
		-f target_commitish="$target" \
		-q .body)
fi

# "* <title> by @<login> in <url>/pull/<n>". The leading .* is greedy, so a title
# containing " by @" resolves to the last occurrence.
pairs=$(printf '%s\n' "$notes" |
	sed -n 's#^\* .* by @\([^ ]*\) in .*/pull/\([0-9][0-9]*\)$#\1 \2#p' |
	canonicalize)

# GitHub names one pull request per first-timer; the rest come from $pairs.
first_timers=$(printf '%s\n' "$notes" |
	sed -n 's#^\* @\([^ ]*\) made their first contribution in .*#\1#p' |
	canonicalize | awk '!seen[$0]++')

is_bot() {
	case "$1" in
	*'[bot]') return 0 ;;
	esac
	printf '%s\n' "$BOT_LOGINS" | grep -qxF -- "$1"
}

# "#12" alone, "#12, #13, and #14" for several.
join_pull_requests() {
	_jpr_total=$(printf '%s\n' "$1" | wc -l | tr -d ' ')
	_jpr_index=0
	_jpr_out=''
	for _jpr_n in $1; do
		_jpr_index=$((_jpr_index + 1))
		if [ "$_jpr_index" -eq 1 ]; then
			_jpr_out="#${_jpr_n}"
		elif [ "$_jpr_index" -eq "$_jpr_total" ]; then
			_jpr_out="${_jpr_out}, and #${_jpr_n}"
		else
			_jpr_out="${_jpr_out}, #${_jpr_n}"
		fi
	done
	printf '%s' "$_jpr_out"
}

entries=''
new_count=0
for login in $first_timers; do
	if is_bot "$login"; then
		log "skipping bot ${login}"
		continue
	fi
	prs=$(printf '%s\n' "$pairs" | awk -v l="$login" '$1 == l { print $2 }' | sort -nu)
	[ -n "$prs" ] || continue
	entry="@${login} made their first contribution in $(join_pull_requests "$prs")"
	if [ -z "$entries" ]; then
		entries="$entry"
	else
		entries="${entries}${NEWLINE}${NEWLINE}${entry}"
	fi
	new_count=$((new_count + 1))
done

# The block is not newline-padded: the footer template owns the blank lines that
# separate it from the horizontal rule.
new_contributors=''
if [ -n "$entries" ]; then
	new_contributors="## New Contributors${NEWLINE}${NEWLINE}${entries}"
fi

emit() {
	if [ -n "${GITHUB_ENV:-}" ]; then
		{
			printf '%s<<__RELEASE_NOTES_EOF__\n' "$1"
			printf '%s\n' "$2"
			printf '__RELEASE_NOTES_EOF__\n'
		} >>"$GITHUB_ENV"
	else
		printf '===== %s =====\n%s\n' "$1" "$2"
	fi
}

emit RELEASE_NEW_CONTRIBUTORS "$new_contributors"

log "${new_count} new contributor(s) after bot filtering"
