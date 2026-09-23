#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT_DIR=$(CDPATH='' cd -- "${SCRIPT_DIR}/../.." && pwd)
SCRIPT="${REPO_ROOT_DIR}/scripts/protocol-pin.sh"

fail() {
	printf '%s\n' "$1" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/sortie-protocol-pin-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

mkdir "$tmp/bin"
GO_BIN=$(command -v go) || fail "go not found on PATH"
"$GO_BIN" build -o "$tmp/bin/protocolpin" "${REPO_ROOT_DIR}/tools/protocolpin" || fail "building protocolpin failed"

build_fixture_repo() {
	_dir=$1
	_tag=$2
	_commit=$3
	_schema_digest=$4
	mkdir -p "${_dir}/internal/agent/clientprotocol/schemagen" "${_dir}/internal/agent/clientprotocol/testdata/${_tag}"
	cat >"${_dir}/internal/agent/clientprotocol/generate.go" <<EOF
package clientprotocol

//go:generate go run github.com/sortie-ai/sortie/internal/agent/clientprotocol/schemagen testdata/${_tag} wire_gen.go
EOF
	cat >"${_dir}/internal/agent/clientprotocol/testdata/${_tag}/PROVENANCE.txt" <<EOF
Upstream repository: https://github.com/agentclientprotocol/agent-client-protocol
Tag:    ${_tag}
Commit: ${_commit}
schema.json 247168 sha256:${_schema_digest}
meta.json 1159 sha256:061edb6efa8fb2aa2792459a86ec7268de5fe665bba48b2ffe7939df01481f88
EOF
	cat >"${_dir}/internal/agent/clientprotocol/schemagen/generate.go" <<EOF
package main

const (
	upstreamTag    = "${_tag}"
	upstreamCommit = "${_commit}"
)
EOF
}

ROOT_121="$tmp/repo-1.21.0"
ROOT_123="$tmp/repo-1.23.0"
build_fixture_repo "$ROOT_121" "schema-v1.21.0" "272bf799f35a258c6a4107a0410ed361e83683d3" \
	"caf62ff962ada396878372ced11efb2c6764e59d90919a38583c319948931a42"
build_fixture_repo "$ROOT_123" "schema-v1.23.0" "6d08f412a7a1370d3cc9a124e3be3d6acf92641e" \
	"3c17bd6385d90cf672d8a661fddc359d73422cf8b8ce6865213d25cfd4c0eca7"

REPORT_TITLE=$("$tmp/bin/protocolpin" locate -repo-root "$ROOT_121" | jq -r .title)

FIXTURE_RELEASES="${REPO_ROOT_DIR}/tools/protocolpin/testdata/releases.json"
[ -f "$FIXTURE_RELEASES" ] || fail "fixture not found: ${FIXTURE_RELEASES}"
# Real release notes push one page past the per-argument limit; the
# padding reproduces that size.
RELEASES_FILE="$tmp/releases.json"
jq '.[0].body = ("x" * 200000)' "$FIXTURE_RELEASES" >"$RELEASES_FILE"

mkdir "$tmp/bin-fault"
cat >"$tmp/bin-fault/protocolpin" <<'EOF'
#!/bin/sh
printf 'protocolpin: forced fault for the test\n' >&2
exit 1
EOF
chmod +x "$tmp/bin-fault/protocolpin"

cat >"$tmp/bin/gh" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"${tmp}/gh.log"
case "\$*" in
*'/releases?per_page=100&page=1')
	cat "${RELEASES_FILE}"
	;;
*'/git/matching-refs/tags/'*)
	printf '[{"ref":"refs/tags/%s","object":{"type":"commit","sha":"%s"}}]\n' "\${FIXTURE_TAG}" "\${FIXTURE_COMMIT}"
	;;
*'/issues?state=all&labels=area:agent-adapter'*)
	printf '%s\n' "\${FIXTURE_ISSUES_JSON}"
	;;
*'/issues/'*' --jq .body')
	printf '%s\n' "\${FIXTURE_ISSUE_BODY}"
	;;
'label create'*)
	exit 0
	;;
*' --jq .node_id')
	case "\$*" in
	*'/labels/'*) printf 'LABEL_NODE_ID\n' ;;
	*) printf 'REPO_NODE_ID\n' ;;
	esac
	;;
*'createIssue'*)
	printf '%s\n' "\${FIXTURE_CREATED_NUMBER}"
	;;
'issue comment'* | 'issue edit'* | 'issue close'* | 'issue reopen'*)
	exit 0
	;;
*)
	printf 'unexpected gh call: %s\n' "\$*" >&2
	exit 1
	;;
esac
EOF
chmod +x "$tmp/bin/gh"

cat >"$tmp/bin/curl" <<EOF
#!/bin/sh
cat >>"${tmp}/curl-bodies.log"
printf '\n---\n' >>"${tmp}/curl-bodies.log"
printf '204'
EOF
chmod +x "$tmp/bin/curl"

run_report() {
	_expect_exit=$1
	_protocolpin_bin=$2
	_repo_root=$3
	shift 3

	: >"$tmp/gh.log"
	: >"$tmp/curl-bodies.log"
	summary="$tmp/summary"
	: >"$summary"

	_status=0
	PATH="$tmp/bin:$PATH" \
		GH_TOKEN=test \
		GH_REPO=sortie-ai/sortie \
		REPO_ROOT="$_repo_root" \
		PROTOCOLPIN_BIN="$_protocolpin_bin" \
		TASK_ISSUE_TYPE_ID=IT_test \
		GITHUB_REPOSITORY=sortie-ai/sortie \
		GITHUB_RUN_ID=42 \
		GITHUB_SERVER_URL=https://github.com \
		GITHUB_STEP_SUMMARY="$summary" \
		DISCORD_WEBHOOK=https://discord.example/webhook \
		"$@" \
		"$SCRIPT" report || _status=$?

	[ "$_status" -eq "$_expect_exit" ] || fail "scripts/protocol-pin.sh report exited ${_status}, want ${_expect_exit}"
}

FIXTURE_TAG="schema-v1.21.0"
FIXTURE_COMMIT="272bf799f35a258c6a4107a0410ed361e83683d3"
FIXTURE_ISSUES_JSON='[]'
FIXTURE_ISSUE_BODY=''
FIXTURE_CREATED_NUMBER='4242'
export FIXTURE_TAG FIXTURE_COMMIT FIXTURE_ISSUES_JSON FIXTURE_ISSUE_BODY FIXTURE_CREATED_NUMBER

run_report 0 "$tmp/bin/protocolpin" "$ROOT_121"

grep -q 'createIssue' "$tmp/gh.log" || fail "open: gh.log does not record a createIssue call: $(cat "$tmp/gh.log")"
grep -q "^Created issue #${FIXTURE_CREATED_NUMBER}\$" "$tmp/summary" || fail "open: summary does not record the created issue number: $(cat "$tmp/summary")"

[ "$(grep -c '^{' "$tmp/curl-bodies.log")" -eq 1 ] || fail "open: curl-bodies.log does not record exactly one Discord post: $(cat "$tmp/curl-bodies.log")"
embed=$(grep '^{' "$tmp/curl-bodies.log")
echo "$embed" | jq -e '.embeds | length == 1' >/dev/null || fail "open: Discord body carries no embed: $embed"
echo "$embed" | jq -e --argjson want 3447003 '.embeds[0].color == $want' >/dev/null || fail "open: Discord embed color = $(echo "$embed" | jq '.embeds[0].color'), want 3447003"
echo "$embed" | jq -e --arg title "$REPORT_TITLE" '(.embeds[0].title | length) > 0' >/dev/null || fail "open: Discord embed title is empty: $embed"
echo "$embed" | jq -e --arg number "$FIXTURE_CREATED_NUMBER" '.embeds[0].url == ("https://github.com/sortie-ai/sortie/issues/" + $number)' >/dev/null ||
	fail "open: Discord embed url = $(echo "$embed" | jq '.embeds[0].url'), want the created issue's URL"
echo "$embed" | jq -e '.embeds[0].timestamp | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")' >/dev/null ||
	fail "open: Discord embed timestamp = $(echo "$embed" | jq '.embeds[0].timestamp'), want an RFC 3339 UTC timestamp"

FIXTURE_TAG="schema-v1.23.0"
FIXTURE_COMMIT="6d08f412a7a1370d3cc9a124e3be3d6acf92641e"
FIXTURE_ISSUES_JSON=$(jq -nc --arg title "$REPORT_TITLE" '[{"number":7,"state":"open","title":$title,"pull_request":null}]')
FIXTURE_ISSUE_BODY='an existing report body'
export FIXTURE_TAG FIXTURE_COMMIT FIXTURE_ISSUES_JSON FIXTURE_ISSUE_BODY

run_report 0 "$tmp/bin/protocolpin" "$ROOT_123"

grep -q '^issue close 7 --reason completed' "$tmp/gh.log" || fail "close: gh.log does not record closing issue #7: $(cat "$tmp/gh.log")"
grep -q 'createIssue' "$tmp/gh.log" && fail "close: gh.log unexpectedly records a createIssue call: $(cat "$tmp/gh.log")"
[ -s "$tmp/curl-bodies.log" ] && fail "close: a Discord post was attempted, want none: $(cat "$tmp/curl-bodies.log")"

run_report 1 "$tmp/bin-fault/protocolpin" "$ROOT_121"

[ "$(grep -c '^{' "$tmp/curl-bodies.log")" -eq 1 ] || fail "fault: curl-bodies.log does not record exactly one Discord post: $(cat "$tmp/curl-bodies.log")"
fault_embed=$(grep '^{' "$tmp/curl-bodies.log")
echo "$fault_embed" | jq -e --argjson want 15548997 '.embeds[0].color == $want' >/dev/null ||
	fail "fault: Discord embed color = $(echo "$fault_embed" | jq '.embeds[0].color'), want 15548997"
echo "$fault_embed" | jq -e '.embeds[0].title | startswith("protocol-pin:")' >/dev/null ||
	fail "fault: Discord embed title = $(echo "$fault_embed" | jq '.embeds[0].title'), want it to start with \"protocol-pin:\""
echo "$fault_embed" | jq -e '.embeds[0].url == "https://github.com/sortie-ai/sortie/actions/runs/42"' >/dev/null ||
	fail "fault: Discord embed url = $(echo "$fault_embed" | jq '.embeds[0].url'), want the run URL"
grep -q '^protocol-pin: protocolpin locate failed$' "$tmp/summary" || fail "fault: summary does not record the fault reason: $(cat "$tmp/summary")"
