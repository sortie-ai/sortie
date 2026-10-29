#!/bin/sh
# Write the release announcement for <version> with a Claude Code agent that
# reads the version's CHANGELOG.md section and the repository, and print it.
#
# Usage: release-announcement.sh <version>
#
# Authenticates with ANTHROPIC_API_KEY when it is set, and otherwise through
# Workload Identity Federation from GitHub Actions (see anthropic-token.sh).
# RELEASE_ANNOUNCEMENT_MODEL overrides the model.

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

version=${1:?usage: release-announcement.sh <version>}
model=${RELEASE_ANNOUNCEMENT_MODEL:-claude-opus-5-5}

require_tools claude jq git awk curl

# Bare mode reads no federation variables, so Claude Code reaches federation
# only through an apiKeyHelper.
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
	settings='{}'
else
	require_env ANTHROPIC_FEDERATION_RULE_ID ANTHROPIC_ORGANIZATION_ID \
		ANTHROPIC_SERVICE_ACCOUNT_ID ANTHROPIC_WORKSPACE_ID
	settings=$(jq -cn --arg helper "'${SCRIPT_DIR}/anthropic-token.sh'" '{apiKeyHelper: $helper}')
fi

root=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)

section=$(awk -v heading="## [${version}]" '
	index($0, heading) == 1 { found = 1; next }
	found && /^## / { exit }
	found { print }
' "${root}/CHANGELOG.md")
if [ -z "$(printf '%s' "$section" | tr -d '[:space:]')" ]; then
	log "CHANGELOG.md has no section for ${version}"
	exit 1
fi

examples=$("${SCRIPT_DIR}/past-announcements.sh" "$version")

previous=$(git -C "$root" describe --tags --abbrev=0 --exclude "v${version}" HEAD)

prompt=$(cat <<EOF
<changelog_section version="${version}">
${section}
</changelog_section>

<examples>
${examples}
</examples>

Write the announcement for Sortie ${version}. Its changes are the commits in ${previous}..HEAD.
EOF
)

schema='{"type":"object","properties":{"announcement":{"type":"string","minLength":1}},"required":["announcement"],"additionalProperties":false}'

log "drafting the ${version} announcement with ${model} (previous release: ${previous})"

status=0
response=$(cd "$root" && printf '%s\n' "$prompt" | claude --bare -p \
	--model "$model" \
	--settings "$settings" \
	--system-prompt-file "${root}/.github/prompts/release-announcement.md" \
	--tools 'Read,Grep,Glob,Bash' \
	--allowedTools 'Read' 'Grep' 'Glob' 'Bash(git log *)' 'Bash(git show *)' 'Bash(git diff *)' \
	--permission-mode dontAsk \
	--max-turns 60 \
	--no-session-persistence \
	--output-format json \
	--json-schema "$schema") || status=$?

announcement=$(printf '%s\n' "$response" | jq -r '.structured_output.announcement // empty' 2>/dev/null) || :
if [ "$status" -ne 0 ] || [ -z "$announcement" ]; then
	reason=$(printf '%s\n' "$response" | jq -r '"\(.subtype // "error"): \(.result // "no result")"' 2>/dev/null) || reason=$response
	log "claude exited with status ${status} and no announcement: ${reason}"
	exit 1
fi

log "estimated cost: \$$(printf '%s\n' "$response" | jq -r '.total_cost_usd')"
printf '%s\n' "$announcement"
