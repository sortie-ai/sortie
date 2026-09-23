#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT_DIR=$(CDPATH='' cd -- "${SCRIPT_DIR}/../.." && pwd)
SCRIPT="${REPO_ROOT_DIR}/scripts/nightly-issue.sh"
# An intentional change to a recorded gh call updates these files.
EXPECTED_DIR="${SCRIPT_DIR}/testdata/nightly-issue"

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/sortie-nightly-issue-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/bin/sh
{
	printf '=== call ===\n'
	for _a in "$@"; do
		case "$_a" in
		body=@*)
			_path=${_a#body=@}
			cat "$_path" >>"$GH_BODIES_LOG"
			printf '\n---body---\n' >>"$GH_BODIES_LOG"
			printf '%s\n' 'body=@<BODYFILE>'
			;;
		*)
			printf '%s\n' "$_a"
			;;
		esac
	done
} >>"$GH_ARGV_LOG"
case "$*" in
*'/actions/runs/42 --jq .workflow_id') printf '%s\n' 7 ;;
*'/actions/workflows/7/runs?'*) printf '%s\n' '{"workflow_runs":[]}' ;;
*'/issues?state=all&labels=ci-nightly'*) printf '%s\n' "$GH_ISSUES_JSON" ;;
'label create'*) exit 0 ;;
*' --jq .node_id')
	case "$*" in
	*'/labels/'*) printf 'LABEL_NODE_ID\n' ;;
	*'/issues/'*) printf 'ISSUE_NODE_ID\n' ;;
	*) printf 'REPO_NODE_ID\n' ;;
	esac
	;;
*'createIssue'*) printf '%s\n' 555 ;;
*'updateIssue'*) exit 0 ;;
'issue comment'* | 'issue close'* | 'issue reopen'* | 'issue edit'*) exit 0 ;;
*)
	printf 'unexpected gh call: %s\n' "$*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$tmp/bin/gh"

cat >"$tmp/bin/nite" <<'EOF'
#!/bin/sh
cat >/dev/null
printf '{"action":"%s","body":"%s","summary":"summary","annotation":""}\n' "$NITE_FORCE_ACTION" "$NITE_FORCE_BODY"
EOF
chmod +x "$tmp/bin/nite"

run_revision() {
	_script=$1
	_gh_log=$2
	_bodies_log=$3
	: >"$_gh_log"
	: >"$_bodies_log"
	: >"$tmp/summary"
	PATH="$tmp/bin:$PATH" \
		GH_TOKEN=test RUN_HISTORY_TOKEN=test GH_REPO=sortie-ai/sortie \
		NITE_BIN="$tmp/bin/nite" \
		ADAPTER=example ADAPTER_NAME=Example KIND=agent SOURCE=test ADAPTER_VERSION=1.0 \
		TEST_ISSUE_TYPE_ID=IT_test JOB_NAME='Integration: Example' OUTCOME=success DEFAULT_BRANCH=main \
		GITHUB_REPOSITORY=sortie-ai/sortie GITHUB_RUN_ID=42 GITHUB_SERVER_URL=https://github.com \
		GITHUB_SHA=deadbeef GITHUB_STEP_SUMMARY="$tmp/summary" \
		GH_ARGV_LOG="$_gh_log" GH_BODIES_LOG="$_bodies_log" \
		GH_ISSUES_JSON="$ISSUES_JSON" \
		NITE_FORCE_ACTION="$NITE_FORCE_ACTION" NITE_FORCE_BODY="canned incident body" \
		"$_script" decide

	[ "$(cat "$tmp/summary")" = summary ] || fail "${_script} decide (action ${NITE_FORCE_ACTION}) did not carry out the NITE decision: $(cat "$tmp/summary")"
}

check_action() {
	_name=$1
	ISSUES_JSON=$2
	NITE_FORCE_ACTION=$3

	run_revision "$SCRIPT" "$tmp/gh.log" "$tmp/bodies.log"

	diff "${EXPECTED_DIR}/${_name}.gh.txt" "$tmp/gh.log" >"$tmp/diff.out" 2>&1 ||
		fail "gh argument vectors differ for NITE action ${_name}: $(cat "$tmp/diff.out")"
	diff "${EXPECTED_DIR}/${_name}.bodies.txt" "$tmp/bodies.log" >"$tmp/diff.out" 2>&1 ||
		fail "gh -F body=@<path> file contents differ for NITE action ${_name}: $(cat "$tmp/diff.out")"
}

title_row='Nightly integration failure: Example'
open_row=$(printf '[{"number":99,"state":"open","title":"%s","pull_request":null}]' "$title_row")
closed_row=$(printf '[{"number":99,"state":"closed","title":"%s","pull_request":null}]' "$title_row")

check_action open '[]' open
check_action comment "$open_row" comment
check_action close "$open_row" close
check_action reopen "$closed_row" reopen
check_action none '[]' none

grep -q '=== call ===' "$tmp/gh.log" || fail "no gh calls were recorded for the none action"
