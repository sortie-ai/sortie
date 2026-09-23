#!/bin/sh
# Post one message to a Discord channel webhook.
# Usage: discord-notify.sh < message.txt
#        discord-notify.sh --embed < embed.json
#
# Without a flag stdin is the plain-text message; with --embed it is one
# Discord embed object.
#
# The endpoint comes from DISCORD_WEBHOOK. Nothing about the request is
# echoed: the URL is itself the only credential, and GitHub masks only the
# secrets it injected, not a URL a script assembled at runtime.

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

# Discord answers 400 above these lengths.
readonly CONTENT_LIMIT=2000
readonly EMBED_TITLE_LIMIT=256

# A webhook parses user mentions out of content unless told otherwise, and
# the callers put tracker-supplied text in there.
build_body() {
	case "${1:-}" in
	'')
		jq -Rs --argjson limit "$CONTENT_LIMIT" \
			'{content: .[0:$limit], allowed_mentions: {parse: []}}'
		;;
	--embed)
		jq -c --argjson limit "$EMBED_TITLE_LIMIT" \
			'{embeds: [.title |= .[0:$limit]], allowed_mentions: {parse: []}}'
		;;
	*)
		log "discord-notify: unknown argument: $1"
		return 1
		;;
	esac
}

main() {
	require_tools curl jq
	require_env DISCORD_WEBHOOK

	_body=$(build_body "$@")

	if ! _status=$(printf '%s' "$_body" | curl -s -o /dev/null -w '%{http_code}' \
		--max-time 30 \
		-X POST \
		-H 'Content-Type: application/json' \
		--data-binary @- \
		"$DISCORD_WEBHOOK" 2>/dev/null); then
		log 'discord-notify: the request never completed'
		return 1
	fi

	case "$_status" in
	2*) ;;
	*)
		log "discord-notify: Discord answered ${_status}"
		return 1
		;;
	esac
}

main "$@"
