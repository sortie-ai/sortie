#!/bin/sh
# Decide and carry out NITE's action for one nightly
# integration shard.
# Usage: nightly-issue.sh decide
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

# NITE's own default thresholds (tools/nite).
# No shard overrides them yet, so the shell uses the same values to
# bound how many prior samples it fetches.
readonly MONITOR_FAILURE_THRESHOLD=2
readonly MONITOR_PASS_THRESHOLD=2
readonly MONITOR_LOOKBACK=10

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

# list_nightly_incidents lists every issue labeled ci-nightly, newest
# created first, paginated to exhaustion, appending
# "<number>\t<state>\t<title>" per issue to out_file. It uses the
# issues REST route rather than the search route: the search index lags
# issue creation by an unbounded interval, and a durable incident that a
# search misses would file as a duplicate. It returns non-zero on a
# non-zero gh exit, leaving out_file holding whatever earlier pages
# already wrote.
list_nightly_incidents() {
	_lni_out=$1
	: >"$_lni_out"
	_lni_page=1
	while :; do
		_lni_page_json=$(gh api "repos/${GITHUB_REPOSITORY}/issues?state=all&labels=${CI_LABEL_NAME}&sort=created&direction=desc&per_page=100&page=${_lni_page}") || return 1
		printf '%s' "$_lni_page_json" | jq -r \
			'.[] | select(.pull_request == null) | "\(.number)\t\(.state)\t\(.title)"' >>"$_lni_out"
		_lni_count=$(printf '%s' "$_lni_page_json" | jq -r 'length')
		if [ "$_lni_count" -lt 100 ]; then
			return 0
		fi
		_lni_page=$((_lni_page + 1))
	done
}

# fetch_run_history lists the last MONITOR_LOOKBACK completed runs of
# workflow_id on DEFAULT_BRANCH, excluding the current run, then looks
# up the job named JOB_NAME on each one until it holds MAX_SAMPLES_NEEDED
# samples for it. It appends "<run_id>\t<run_url>\t<conclusion>" per
# held sample to out_file, newest first. It returns non-zero when a gh
# call failed, or when at least one run was examined and none of them
# carried a job named JOB_NAME.
fetch_run_history() {
	_frh_workflow_id=$1
	_frh_out=$2

	_frh_runs_json=$(GH_TOKEN="$RUN_HISTORY_TOKEN" gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/${_frh_workflow_id}/runs?status=completed&branch=${DEFAULT_BRANCH}&per_page=${MONITOR_LOOKBACK}&exclude_pull_requests=true") || return 1

	_frh_runs_file=$(mktemp "${TMPDIR:-/tmp}/sortie-nightly-runs.XXXXXX")
	printf '%s' "$_frh_runs_json" | jq -r --argjson cur "$GITHUB_RUN_ID" \
		'.workflow_runs[] | select(.id != $cur) | "\(.id)\t\(.html_url)"' >"$_frh_runs_file"

	_frh_held=0
	_frh_examined=0
	_frh_matched=0
	_frh_failed=0

	while IFS="$(printf '\t')" read -r _frh_run_id _frh_run_url; do
		[ -z "$_frh_run_id" ] && continue
		if [ "$_frh_held" -ge "$MAX_SAMPLES_NEEDED" ]; then
			break
		fi
		_frh_examined=$((_frh_examined + 1))

		if ! _frh_jobs_json=$(GH_TOKEN="$RUN_HISTORY_TOKEN" gh api --paginate "repos/${GITHUB_REPOSITORY}/actions/runs/${_frh_run_id}/jobs?per_page=100" --jq '.jobs[]'); then
			_frh_failed=1
			break
		fi
		_frh_conclusion=$(printf '%s' "$_frh_jobs_json" | jq -r -s --arg name "$JOB_NAME" \
			'[.[] | select(.name == $name) | .conclusion] | (first // empty)')
		if [ -z "$_frh_conclusion" ]; then
			continue
		fi
		_frh_matched=$((_frh_matched + 1))
		_frh_held=$((_frh_held + 1))
		printf '%s\t%s\t%s\n' "$_frh_run_id" "$_frh_run_url" "$_frh_conclusion" >>"$_frh_out"
	done <"$_frh_runs_file"

	rm -f "$_frh_runs_file"

	if [ "$_frh_failed" -eq 1 ]; then
		return 1
	fi
	if [ "$_frh_examined" -gt 0 ] && [ "$_frh_matched" -eq 0 ]; then
		return 1
	fi
	return 0
}

# history_json_from_file renders history_file's tab-separated
# "<run_id>\t<run_url>\t<conclusion>" lines as the historySample JSON
# array the monitor expects.
history_json_from_file() {
	jq -R -s '
		split("\n") | map(select(length > 0)) | map(split("\t")) |
		map({run_id: (.[0] | tonumber), run_url: .[1], conclusion: .[2]})
	' <"$1"
}

# decide_exit_trap is installed before decide takes any other action.
# It fires on EXIT, HUP, INT, and TERM; when DECIDE_DONE was never set,
# no decision was carried out, so it announces the fault on the run
# page and in the job summary itself, calling neither the monitor nor
# gh, and forces a clean exit so a monitor or GitHub-API fault never
# reddens a shard whose own tests passed.
decide_exit_trap() {
	if [ "${DECIDE_TRAP_RUNNING:-0}" -eq 1 ] || [ "$DECIDE_DONE" -eq 1 ]; then
		return
	fi
	DECIDE_TRAP_RUNNING=1

	printf '::error::nightly-issue.sh decide for %s terminated before a NITE decision was carried out\n' "${ADAPTER_NAME:-unknown}"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		printf '%s: no NITE decision was executed; see the job log for the failure\n' "${ADAPTER_NAME:-unknown}" >>"$GITHUB_STEP_SUMMARY"
	fi
	exit 0
}

decide() {
	DECIDE_DONE=0
	trap decide_exit_trap EXIT HUP INT TERM

	require_tools gh jq date mktemp sort head cut
	require_env GH_TOKEN RUN_HISTORY_TOKEN GH_REPO NITE_BIN ADAPTER ADAPTER_NAME KIND SOURCE \
		ADAPTER_VERSION TEST_ISSUE_TYPE_ID JOB_NAME OUTCOME DEFAULT_BRANCH \
		GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_SERVER_URL GITHUB_SHA GITHUB_STEP_SUMMARY

	TITLE="Nightly integration failure: ${ADAPTER_NAME}"
	RUN_URL="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
	NOW=$(date -u '+%Y-%m-%d %H:%M UTC')

	if [ "$MONITOR_FAILURE_THRESHOLD" -ge "$MONITOR_PASS_THRESHOLD" ]; then
		MAX_SAMPLES_NEEDED=$((MONITOR_FAILURE_THRESHOLD - 1))
	else
		MAX_SAMPLES_NEEDED=$((MONITOR_PASS_THRESHOLD - 1))
	fi

	HISTORY_READ=1
	HISTORY_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-nightly-history.XXXXXX")
	: >"$HISTORY_FILE"
	if _workflow_id=$(GH_TOKEN="$RUN_HISTORY_TOKEN" gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}" --jq .workflow_id); then
		if ! fetch_run_history "$_workflow_id" "$HISTORY_FILE"; then
			HISTORY_READ=0
		fi
	else
		HISTORY_READ=0
	fi

	INCIDENT_READ=1
	INCIDENT_STATE=absent
	INCIDENT_NUMBER=0
	INCIDENTS_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-nightly-incidents.XXXXXX")
	if ! list_nightly_incidents "$INCIDENTS_FILE"; then
		INCIDENT_READ=0
	fi
	_match_line=$(awk -F'\t' -v t="$TITLE" '$3 == t { print $1"\t"$2 }' "$INCIDENTS_FILE" | sort -t "$(printf '\t')" -k1,1rn | head -n1)
	if [ -n "$_match_line" ]; then
		INCIDENT_NUMBER=$(printf '%s' "$_match_line" | cut -f1)
		INCIDENT_STATE=$(printf '%s' "$_match_line" | cut -f2)
	fi
	rm -f "$INCIDENTS_FILE"

	_history_json=$(history_json_from_file "$HISTORY_FILE")
	rm -f "$HISTORY_FILE"

	_input_json=$(jq -n \
		--arg outcome "$OUTCOME" \
		--arg job_name "$JOB_NAME" \
		--arg adapter "$ADAPTER" \
		--arg adapter_name "$ADAPTER_NAME" \
		--arg kind "$KIND" \
		--arg source "$SOURCE" \
		--arg version "$ADAPTER_VERSION" \
		--arg run_url "$RUN_URL" \
		--arg commit "$GITHUB_SHA" \
		--arg now "$NOW" \
		--arg test_report_path "${TEST_REPORT_PATH:-}" \
		--arg incident_state "$INCIDENT_STATE" \
		--argjson incident_number "$INCIDENT_NUMBER" \
		--argjson incident_read "$([ "$INCIDENT_READ" -eq 1 ] && printf true || printf false)" \
		--argjson history_read "$([ "$HISTORY_READ" -eq 1 ] && printf true || printf false)" \
		--argjson history "$_history_json" \
		'{
			outcome: $outcome, job_name: $job_name, adapter: $adapter,
			adapter_name: $adapter_name, kind: $kind, source: $source,
			version: $version, run_url: $run_url, commit: $commit, now: $now,
			test_report_path: $test_report_path, incident_state: $incident_state,
			incident_number: $incident_number, incident_read: $incident_read,
			history_read: $history_read, history: $history
		}')

	_decision_json=$(printf '%s' "$_input_json" | "$NITE_BIN" \
		-failure-threshold "$MONITOR_FAILURE_THRESHOLD" \
		-pass-threshold "$MONITOR_PASS_THRESHOLD" \
		-lookback "$MONITOR_LOOKBACK")

	_action=$(printf '%s' "$_decision_json" | jq -r '.action')
	_body=$(printf '%s' "$_decision_json" | jq -r '.body')
	_summary=$(printf '%s' "$_decision_json" | jq -r '.summary')
	_annotation=$(printf '%s' "$_decision_json" | jq -r '.annotation')

	if [ -n "$_annotation" ]; then
		printf '%s\n' "$_annotation"
	fi

	case "$_action" in
	open)
		resolve_area_label
		provision_labels
		BODY_FILE=$(mktemp "${TMPDIR:-/tmp}/sortie-nightly-issue.XXXXXX")
		printf '%s' "$_body" >"$BODY_FILE"
		_number=$(create_issue)
		rm -f "$BODY_FILE"
		printf 'Created issue #%s\n' "$_number"
		;;
	comment)
		gh issue comment "$INCIDENT_NUMBER" --body "$_body" >/dev/null
		;;
	close)
		gh issue comment "$INCIDENT_NUMBER" --body "$_body" >/dev/null
		gh issue close "$INCIDENT_NUMBER" >/dev/null
		;;
	reopen)
		gh issue reopen "$INCIDENT_NUMBER" >/dev/null
		gh issue comment "$INCIDENT_NUMBER" --body "$_body" >/dev/null
		resolve_area_label
		if [ -n "$AREA_LABEL_NAME" ]; then
			gh issue edit "$INCIDENT_NUMBER" --add-label "$AREA_LABEL_NAME" >/dev/null
		fi
		set_test_type "$INCIDENT_NUMBER"
		;;
	none) ;;
	*)
		log "unrecognized NITE action: ${_action}"
		return 1
		;;
	esac

	printf '%s\n' "$_summary" >>"$GITHUB_STEP_SUMMARY"

	DECIDE_DONE=1
}

case "${1:-}" in
decide) decide ;;
*)
	printf 'Usage: %s decide\n' "$0" >&2
	exit 2
	;;
esac
