#!/bin/sh
# Create, update, or resolve the issue for one nightly integration shard.
# Usage: nightly-issue.sh report|resolve
# shellcheck disable=SC2016

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

readonly CI_LABEL_NAME="ci-nightly"
readonly CI_LABEL_COLOR="D73A4A"
readonly CI_LABEL_DESCRIPTION="Nightly integration test failures"

readonly AGENT_LABEL_NAME="area:agent-adapter"
readonly AGENT_LABEL_COLOR="0E8A16"
readonly AGENT_LABEL_DESCRIPTION="Agent interface, Claude Code adapter, Copilot adapter, mock"

readonly TRACKER_LABEL_NAME="area:tracker-adapter"
readonly TRACKER_LABEL_COLOR="006B75"
readonly TRACKER_LABEL_DESCRIPTION="Tracker interface, Jira adapter, GitHub adapter, file adapter"

readonly SCM_LABEL_NAME="area:scm"
readonly SCM_LABEL_COLOR="ad076e"
readonly SCM_LABEL_DESCRIPTION="SCM interface, pull request reviews, merge and branch operations, CI status providers"

readonly ORCHESTRATOR_LABEL_NAME="area:orchestrator"
readonly ORCHESTRATOR_LABEL_COLOR="5319E7"
readonly ORCHESTRATOR_LABEL_DESCRIPTION="Dispatch, retry, reconciliation, state machine, poll loop"

issue_number() {
	_in_issues=$(gh issue list --state open --limit 100 --search "in:title \"${TITLE}\"" --json number,title)
	printf '%s\n' "$_in_issues" |
		jq -r --arg title "$TITLE" '[.[] | select(.title == $title) | .number] | (first // empty)'
}

resolve_area_label() {
	AREA_LABEL_NAME=""
	AREA_LABEL_COLOR=""
	AREA_LABEL_DESCRIPTION=""
	case "$KIND" in
	agent)
		AREA_LABEL_NAME=$AGENT_LABEL_NAME
		AREA_LABEL_COLOR=$AGENT_LABEL_COLOR
		AREA_LABEL_DESCRIPTION=$AGENT_LABEL_DESCRIPTION
		;;
	tracker)
		AREA_LABEL_NAME=$TRACKER_LABEL_NAME
		AREA_LABEL_COLOR=$TRACKER_LABEL_COLOR
		AREA_LABEL_DESCRIPTION=$TRACKER_LABEL_DESCRIPTION
		;;
	SCM)
		AREA_LABEL_NAME=$SCM_LABEL_NAME
		AREA_LABEL_COLOR=$SCM_LABEL_COLOR
		AREA_LABEL_DESCRIPTION=$SCM_LABEL_DESCRIPTION
		;;
	"orchestrator E2E")
		AREA_LABEL_NAME=$ORCHESTRATOR_LABEL_NAME
		AREA_LABEL_COLOR=$ORCHESTRATOR_LABEL_COLOR
		AREA_LABEL_DESCRIPTION=$ORCHESTRATOR_LABEL_DESCRIPTION
		;;
	esac
}

ensure_label() {
	gh label create "$1" --color "$2" --description "$3" --force >/dev/null 2>&1 || true
}

label_node_id() {
	_lni_name=$(printf '%s' "$1" | jq -sRr @uri)
	gh api "repos/${GITHUB_REPOSITORY}/labels/${_lni_name}" --jq .node_id
}

provision_labels() {
	ensure_label "$CI_LABEL_NAME" "$CI_LABEL_COLOR" "$CI_LABEL_DESCRIPTION"
	if [ -n "$AREA_LABEL_NAME" ]; then
		ensure_label "$AREA_LABEL_NAME" "$AREA_LABEL_COLOR" "$AREA_LABEL_DESCRIPTION"
	fi
}

create_issue() {
	_repository_id=$(gh api "repos/${GITHUB_REPOSITORY}" --jq .node_id)
	_ci_label_id=$(label_node_id "$CI_LABEL_NAME")
	set -- -f "labels[]=${_ci_label_id}"
	if [ -n "$AREA_LABEL_NAME" ]; then
		_area_label_id=$(label_node_id "$AREA_LABEL_NAME")
		set -- "$@" -f "labels[]=${_area_label_id}"
	fi

	gh api graphql \
		-f query='mutation($repository: ID!, $title: String!, $body: String!, $type: ID!, $labels: [ID!]!) { createIssue(input: {repositoryId: $repository, title: $title, body: $body, issueTypeId: $type, labelIds: $labels}) { issue { number } } }' \
		-f repository="$_repository_id" \
		-f title="$TITLE" \
		-F body="@${BODY_FILE}" \
		-f type="$TEST_ISSUE_TYPE_ID" \
		"$@" \
		--jq .data.createIssue.issue.number
}

set_test_type() {
	_stt_number=$1
	_stt_issue_id=$(gh api "repos/${GITHUB_REPOSITORY}/issues/${_stt_number}" --jq .node_id)
	gh api graphql \
		-f query='mutation($issue: ID!, $type: ID!) { updateIssue(input: {id: $issue, issueTypeId: $type}) { issue { number } } }' \
		-f issue="$_stt_issue_id" \
		-f type="$TEST_ISSUE_TYPE_ID" >/dev/null
}

write_report_body() {
	_failed=""
	_log="Install step failed before tests ran; no test output captured."
	if [ -f gotest.json ]; then
		_failed=$(jq -rR 'fromjson? | select(.Action == "fail" and (.Test != null)) | .Test' gotest.json 2>/dev/null | sort -u)
		_log=$(jq -jR 'fromjson? | select(.Action == "output") | .Output' gotest.json 2>/dev/null | tail -c 6000)
		if [ -z "$_log" ]; then
			_log=$(tail -c 6000 gotest.json)
		fi
	fi

	if [ -n "$_failed" ]; then
		_failed_md=$(printf '%s\n' "$_failed" | sed 's/^/- `/; s/$/`/')
	else
		_failed_md="_No test-level failures parsed (build error, panic, timeout, or install failure). See the log excerpt._"
	fi

	{
		printf 'Nightly integration tests failed for `%s`.\n\n' "$ADAPTER"
		printf '| Field | Value |\n|---|---|\n'
		printf '| Adapter | `%s` (%s) |\n' "$ADAPTER" "$KIND"
		printf '| Tested version | %s |\n' "$ADAPTER_VERSION"
		printf '| Install source | %s |\n' "$SOURCE"
		printf '| Run | %s |\n' "$RUN_URL"
		printf '| Commit | `%s` |\n' "$GITHUB_SHA"
		printf '| Date | %s |\n\n' "$NOW"
		printf '### Failing tests\n\n%s\n\n' "$_failed_md"
		printf '### Log excerpt\n\n```\n%s\n```\n\n' "$_log"
		printf '%s\n' "_Filed by the nightly integration monitor. It comments on recurrence and closes automatically when this adapter passes again._"
	} >"$BODY_FILE"
}

report_failure() {
	require_tools gh jq date mktemp sort sed tail
	require_env ADAPTER ADAPTER_NAME KIND SOURCE ADAPTER_VERSION \
		GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_SERVER_URL GITHUB_SHA \
		TEST_ISSUE_TYPE_ID

	TITLE="Nightly integration failure: ${ADAPTER_NAME}"
	RUN_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
	NOW=$(date -u '+%Y-%m-%d %H:%M UTC')
	BODY_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-nightly-issue.XXXXXX")
	trap 'rm -f "$BODY_FILE"' EXIT HUP INT TERM
	write_report_body
	resolve_area_label
	provision_labels

	_number=$(issue_number)
	if [ -n "$_number" ]; then
		gh issue comment "$_number" --body-file "$BODY_FILE"
		if [ -n "$AREA_LABEL_NAME" ]; then
			gh issue edit "$_number" --add-label "$AREA_LABEL_NAME"
		fi
		set_test_type "$_number"
		printf 'Updated existing issue #%s\n' "$_number"
	else
		_number=$(create_issue)
		printf 'Created issue #%s\n' "$_number"
	fi

	printf 'Issue #%s has type Test\n' "$_number"
}

resolve_issue() {
	require_tools gh jq date
	require_env ADAPTER ADAPTER_NAME GITHUB_REPOSITORY GITHUB_RUN_ID \
		GITHUB_SERVER_URL

	TITLE="Nightly integration failure: ${ADAPTER_NAME}"
	_number=$(issue_number)
	if [ -z "$_number" ]; then
		return
	fi

	_run_url="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
	_now=$(date -u '+%Y-%m-%d %H:%M UTC')
	gh issue comment "$_number" --body "Recovered: \`${ADAPTER}\` integration tests passed on ${_now}. Closing. Run: ${_run_url}"
	gh issue close "$_number"
	printf 'Closed issue #%s (recovered)\n' "$_number"
}

case "${1:-}" in
report) report_failure ;;
resolve) resolve_issue ;;
*)
	printf 'Usage: %s report|resolve\n' "$0" >&2
	exit 2
	;;
esac
