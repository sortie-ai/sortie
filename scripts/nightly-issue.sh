#!/bin/sh
# Create, update, or resolve the issue for one nightly integration shard.
# Usage: nightly-issue.sh report|resolve
# shellcheck disable=SC2016

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

issue_number() {
	gh issue list --state open --limit 100 --search "in:title \"${TITLE}\"" --json number,title |
		jq -r --arg title "$TITLE" '[.[] | select(.title == $title) | .number] | (first // empty)'
}

create_issue() {
	gh label create ci-nightly --color D73A4A \
		--description "Nightly integration test failures" --force >/dev/null 2>&1 || true

	_repository_id=$(gh api "repos/${GITHUB_REPOSITORY}" --jq .node_id)
	_ci_label_id=$(gh api "repos/${GITHUB_REPOSITORY}/labels/ci-nightly" --jq .node_id)

	if [ "$KIND" = "agent" ]; then
		_agent_label_id=$(gh api "repos/${GITHUB_REPOSITORY}/labels/area%3Aagent-adapter" --jq .node_id)
		gh api graphql \
			-f query='mutation($repository: ID!, $title: String!, $body: String!, $type: ID!, $ciLabel: ID!, $areaLabel: ID!) { createIssue(input: {repositoryId: $repository, title: $title, body: $body, issueTypeId: $type, labelIds: [$ciLabel, $areaLabel]}) { issue { number } } }' \
			-f repository="$_repository_id" \
			-f title="$TITLE" \
			-F body="@${BODY_FILE}" \
			-f type="$TEST_ISSUE_TYPE_ID" \
			-f ciLabel="$_ci_label_id" \
			-f areaLabel="$_agent_label_id" \
			--jq .data.createIssue.issue.number
	else
		gh api graphql \
			-f query='mutation($repository: ID!, $title: String!, $body: String!, $type: ID!, $ciLabel: ID!) { createIssue(input: {repositoryId: $repository, title: $title, body: $body, issueTypeId: $type, labelIds: [$ciLabel]}) { issue { number } } }' \
			-f repository="$_repository_id" \
			-f title="$TITLE" \
			-F body="@${BODY_FILE}" \
			-f type="$TEST_ISSUE_TYPE_ID" \
			-f ciLabel="$_ci_label_id" \
			--jq .data.createIssue.issue.number
	fi
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

	_number=$(issue_number)
	if [ -n "$_number" ]; then
		gh issue comment "$_number" --body-file "$BODY_FILE"
		if [ "$KIND" = "agent" ]; then
			gh issue edit "$_number" --add-label area:agent-adapter
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
