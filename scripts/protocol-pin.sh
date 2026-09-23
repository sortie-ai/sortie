#!/bin/sh
# Compare the pinned Agent Client Protocol schema against the publisher's
# releases and keep the drift report issue current.
# Usage: protocol-pin.sh report
# shellcheck disable=SC2016

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
# shellcheck source=scripts/lib/github-issue.sh
. "${SCRIPT_DIR}/lib/github-issue.sh"

readonly REPORT_LABEL_NAME="area:agent-adapter"
readonly REPORT_LABEL_COLOR="0E8A16"
readonly REPORT_LABEL_DESCRIPTION="Agent interface, Claude Code adapter, Copilot adapter, mock"

readonly FAULT_COLOR=15548997
readonly NOTIFY_COLOR=3447003

fail() {
	printf '::error::protocol-pin: %s\n' "$1" >&2
	printf 'protocol-pin: %s\n' "$1" >>"$GITHUB_STEP_SUMMARY"
	if [ -n "${DISCORD_WEBHOOK:-}" ]; then
		jq -n --arg title "protocol-pin: $1" --arg url "$RUN_URL" --argjson color "$FAULT_COLOR" \
			'{title: $title, url: $url, color: $color, timestamp: (now | todate)}' |
			"${SCRIPT_DIR}/discord-notify.sh" --embed || printf '::warning::protocol-pin: discord notification failed\n' >&2
	fi
	exit 1
}

fetch_releases() {
	_fr_out=$1
	printf '[]' >"$_fr_out"
	_fr_page=1
	while :; do
		_fr_page_json=$(gh api "repos/${PUBLISHER_REPO}/releases?per_page=100&page=${_fr_page}") || return 1
		jq -n --argjson acc "$(cat "$_fr_out")" --argjson page "$_fr_page_json" '$acc + $page' >"${_fr_out}.next"
		mv "${_fr_out}.next" "$_fr_out"
		_fr_count=$(printf '%s' "$_fr_page_json" | jq 'length')
		if [ "$_fr_count" -lt 100 ]; then
			return 0
		fi
		_fr_page=$((_fr_page + 1))
	done
}

fetch_pinned_ref_data() {
	_frd_refs_out=$1
	_frd_tagobj_out=$2

	_frd_refs_json=$(gh api "repos/${PUBLISHER_REPO}/git/matching-refs/tags/${TAG}") || return 1
	printf '%s' "$_frd_refs_json" >"$_frd_refs_out"

	_frd_exact_type=$(printf '%s' "$_frd_refs_json" | jq -r --arg ref "refs/tags/${TAG}" '[.[] | select(.ref == $ref)][0].object.type // empty')
	if [ "$_frd_exact_type" != "tag" ]; then
		printf 'null' >"$_frd_tagobj_out"
		return 0
	fi

	_frd_exact_sha=$(printf '%s' "$_frd_refs_json" | jq -r --arg ref "refs/tags/${TAG}" '[.[] | select(.ref == $ref)][0].object.sha')
	_frd_tagobj_json=$(gh api "repos/${PUBLISHER_REPO}/git/tags/${_frd_exact_sha}") || return 1
	printf '%s' "$_frd_tagobj_json" | jq '{object: .object}' >"$_frd_tagobj_out"
}

report() {
	require_tools gh jq date mktemp awk sort head cut
	require_env GH_TOKEN GH_REPO REPO_ROOT PROTOCOLPIN_BIN TASK_ISSUE_TYPE_ID \
		GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_SERVER_URL GITHUB_STEP_SUMMARY

	RUN_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
	NOW=$(date -u '+%Y-%m-%d %H:%M UTC')

	_locate_json=$("$PROTOCOLPIN_BIN" locate -repo-root "$REPO_ROOT") || fail "protocolpin locate failed"
	PUBLISHER_REPO=$(printf '%s' "$_locate_json" | jq -r .repository)
	TAG=$(printf '%s' "$_locate_json" | jq -r .tag)
	TITLE=$(printf '%s' "$_locate_json" | jq -r .title)

	RELEASES_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-releases.XXXXXX")
	fetch_releases "$RELEASES_FILE" || fail "reading the publisher's release listing failed"

	REFS_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-refs.XXXXXX")
	TAGOBJ_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-tagobj.XXXXXX")
	fetch_pinned_ref_data "$REFS_FILE" "$TAGOBJ_FILE" || fail "reading the pinned tag's ref or tag object failed"

	INCIDENTS_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-issues.XXXXXX")
	list_labeled_issues "$REPORT_LABEL_NAME" "$INCIDENTS_FILE" || fail "listing ${REPORT_LABEL_NAME} issues failed"
	_match_line=$(awk -F'\t' -v t="$TITLE" '$3 == t { print $1"\t"$2 }' "$INCIDENTS_FILE" | sort -t "$(printf '\t')" -k1,1rn | head -n1)
	rm -f "$INCIDENTS_FILE"

	REPORT_STATE=absent
	REPORT_NUMBER=0
	REPORT_BODY=""
	if [ -n "$_match_line" ]; then
		REPORT_NUMBER=$(printf '%s' "$_match_line" | cut -f1)
		REPORT_STATE=$(printf '%s' "$_match_line" | cut -f2)
		REPORT_BODY=$(gh api "repos/${GITHUB_REPOSITORY}/issues/${REPORT_NUMBER}" --jq .body) || fail "reading issue #${REPORT_NUMBER} failed"
	fi

	_bundle=$(jq -n \
		--slurpfile releases "$RELEASES_FILE" \
		--slurpfile refs "$REFS_FILE" \
		--slurpfile tagobj "$TAGOBJ_FILE" \
		--arg state "$REPORT_STATE" \
		--argjson number "$REPORT_NUMBER" \
		--arg body "$REPORT_BODY" \
		--arg run_url "$RUN_URL" \
		--arg now "$NOW" \
		'{
			releases: $releases[0],
			pinned_tag_refs: $refs[0],
			pinned_tag_object: $tagobj[0],
			report: {state: $state, number: $number, body: $body},
			run_url: $run_url,
			now: $now
		}')
	rm -f "$RELEASES_FILE" "$REFS_FILE" "$TAGOBJ_FILE"

	DECIDE_STDERR=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-decide-stderr.XXXXXX")
	if ! _decision_json=$(printf '%s' "$_bundle" | "$PROTOCOLPIN_BIN" decide -repo-root "$REPO_ROOT" 2>"$DECIDE_STDERR"); then
		_reason=$(cat "$DECIDE_STDERR")
		rm -f "$DECIDE_STDERR"
		fail "$_reason"
	fi
	rm -f "$DECIDE_STDERR"

	_action=$(printf '%s' "$_decision_json" | jq -r .action)
	_body=$(printf '%s' "$_decision_json" | jq -r .body)
	_comment=$(printf '%s' "$_decision_json" | jq -r .comment)
	_notify=$(printf '%s' "$_decision_json" | jq -r .notify)
	_summary=$(printf '%s' "$_decision_json" | jq -r .summary)

	_issue_number=$REPORT_NUMBER
	case "$_action" in
	open)
		ensure_label "$REPORT_LABEL_NAME" "$REPORT_LABEL_COLOR" "$REPORT_LABEL_DESCRIPTION"
		BODY_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-protocol-pin-body.XXXXXX")
		printf '%s' "$_body" >"$BODY_FILE"
		_issue_number=$(create_issue "$TITLE" "$BODY_FILE" "$TASK_ISSUE_TYPE_ID" "$REPORT_LABEL_NAME")
		rm -f "$BODY_FILE"
		_summary="${_summary}
Created issue #${_issue_number}"
		;;
	update)
		gh issue comment "$REPORT_NUMBER" --body "$_comment" >/dev/null
		gh issue edit "$REPORT_NUMBER" --body "$_body" >/dev/null
		;;
	reopen)
		set_issue_type "$REPORT_NUMBER" "$TASK_ISSUE_TYPE_ID"
		gh issue reopen "$REPORT_NUMBER" >/dev/null
		gh issue comment "$REPORT_NUMBER" --body "$_comment" >/dev/null
		gh issue edit "$REPORT_NUMBER" --body "$_body" >/dev/null
		;;
	close)
		gh issue comment "$REPORT_NUMBER" --body "$_comment" >/dev/null
		gh issue edit "$REPORT_NUMBER" --body "$_body" >/dev/null
		gh issue close "$REPORT_NUMBER" --reason completed >/dev/null
		;;
	none) ;;
	*)
		fail "unrecognized protocolpin action: ${_action}"
		;;
	esac

	if [ -n "$_notify" ] && [ -n "${DISCORD_WEBHOOK:-}" ]; then
		_issue_url="https://github.com/${GITHUB_REPOSITORY}/issues/${_issue_number}"
		jq -n --arg title "$_notify" --arg url "$_issue_url" --argjson color "$NOTIFY_COLOR" \
			'{title: $title, url: $url, color: $color, timestamp: (now | todate)}' |
			"${SCRIPT_DIR}/discord-notify.sh" --embed || printf '::warning::protocol-pin: discord notification failed\n' >&2
	fi

	printf '%s\n' "$_summary" >>"$GITHUB_STEP_SUMMARY"
}

case "${1:-}" in
report) report ;;
*)
	printf 'Usage: %s report\n' "$0" >&2
	exit 2
	;;
esac
