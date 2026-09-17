#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SCRIPT="${SCRIPT_DIR}/../../scripts/nightly-issue.sh"

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/sortie-nightly-issue-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

mkdir "$tmp/bin"
cat >"$tmp/bin/gh" <<'EOF'
#!/bin/sh
case "$*" in
*'/actions/runs/42 --jq .workflow_id') printf '%s\n' 7 ;;
*'/actions/workflows/7/runs?'*) printf '%s\n' '{"workflow_runs":[]}' ;;
*'/issues?'*) printf '%s\n' '[{"number":12,"state":"closed","title":"Nightly integration failure: Example","pull_request":{"url":"https://example.invalid/pr/12"}}]' ;;
*) printf 'unexpected gh call: %s\n' "$*" >&2; exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/gh"

cat >"$tmp/nite" <<'EOF'
#!/bin/sh
input=$(cat)
state=$(printf '%s' "$input" | jq -r .incident_state)
number=$(printf '%s' "$input" | jq -r .incident_number)
[ "$state" = absent ] || exit 1
[ "$number" -eq 0 ] || exit 1
printf '%s\n' '{"action":"none","body":"","summary":"summary","annotation":""}'
EOF
chmod +x "$tmp/nite"

summary="$tmp/summary"
PATH="$tmp/bin:$PATH" \
	GH_TOKEN=test \
	RUN_HISTORY_TOKEN=test \
	GH_REPO=sortie-ai/sortie \
	NITE_BIN="$tmp/nite" \
	ADAPTER=example \
	ADAPTER_NAME=Example \
	KIND=agent \
	SOURCE=test \
	ADAPTER_VERSION=1.0 \
	TEST_ISSUE_TYPE_ID=test \
	JOB_NAME='Integration: Example' \
	OUTCOME=success \
	DEFAULT_BRANCH=main \
	GITHUB_REPOSITORY=sortie-ai/sortie \
	GITHUB_RUN_ID=42 \
	GITHUB_SERVER_URL=https://github.com \
	GITHUB_SHA=deadbeef \
	GITHUB_STEP_SUMMARY="$summary" \
	"$SCRIPT" decide

[ "$(cat "$summary")" = summary ] || fail "nightly-issue.sh did not write the NITE summary"
