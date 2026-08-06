#!/usr/bin/env bash
# Boot and seed a throwaway containerized Gitea, then publish the coordinates
# the gitea adapter integration suite reads.
#
# Usage: gitea-integration-provision.sh [IMAGE]
#   IMAGE defaults to the pinned tag below, the authoritative version for the
#   release path. Rolling it is a deliberate edit here.
#
# The token is minted in-job against a loopback container, so no repository
# secret is needed.
#
# Coordinates go to $GITHUB_ENV when writable and to stdout as exports, so
#   eval "$(scripts/gitea-integration-provision.sh)"
# works locally. Everything else goes to stderr, keeping stdout eval-parseable.

set -euo pipefail

IMAGE="${1:-docker.gitea.com/gitea:1.27.0-rootless}"

CONTAINER_NAME="sortie-gitea-integration"
HOST_PORT=3000
ENDPOINT="http://localhost:${HOST_PORT}"

GITEA_USER="sortie"
GITEA_PASSWORD="Sortie-Integration-Pw1"
GITEA_EMAIL="sortie@example.com"
GITEA_REPO="adapter-lab"
PROJECT="${GITEA_USER}/${GITEA_REPO}"

# The rootless image boots in seconds, so poll tightly and give up well inside
# the nightly job budget rather than hang on a stuck boot.
READINESS_TIMEOUT=90
POLL_INTERVAL=2

NEWLINE=$'\n'

# Diagnostics go to stderr so stdout carries only the coordinate assignments.
log() {
	printf '%s\n' "$*" >&2
}

# Surface container logs on a non-zero exit. The container is left running: CI
# discards the runner, and the next local run clears it.
dump_logs_on_error() {
	local code=$?
	if [ "$code" -ne 0 ] && docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
		log "provisioning failed (exit ${code}); recent container logs follow:"
		docker logs --tail 200 "$CONTAINER_NAME" >&2 || true
	fi
}
trap dump_logs_on_error EXIT

for tool in docker curl jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		log "required command not found: ${tool}"
		exit 1
	fi
done

# Method, path, and auth mode are positional; the JSON body comes from stdin.
# Auth mode is "token" or "basic" (the admin user and password). A non-2xx status
# logs the response and fails, which set -e turns into an exit.
#
# Repository creation needs admin basic auth; everything else runs under the
# token, so the run also proves the published scopes cover the whole fixture.
# Reviews are the exception, see user_call.
gitea_call() {
	local method="$1" path="$2" auth="$3" body response status
	body=$(cat)
	local auth_args=()
	case "$auth" in
	token) auth_args=(-H "Authorization: token ${TOKEN}") ;;
	basic) auth_args=(-u "${GITEA_USER}:${GITEA_PASSWORD}") ;;
	*)
		log "unknown auth mode: ${auth}"
		return 1
		;;
	esac
	response=$(curl -sS --max-time 30 -w "${NEWLINE}%{http_code}" \
		"${auth_args[@]}" \
		-H 'Content-Type: application/json' \
		-X "$method" "${ENDPOINT}/api/v1${path}" \
		-d "$body")
	status=${response##*"$NEWLINE"}
	response=${response%"$NEWLINE"*}
	case "$status" in
	2*)
		printf '%s' "$response"
		;;
	*)
		log "gitea API ${method} ${path} returned HTTP ${status}: ${response}"
		return 1
		;;
	esac
}

# Review fixtures only: a reviewer cannot review their own pull request, so each
# review needs a distinct author. Passwords travel over loopback and are never
# echoed.
user_call() {
	local user="$1" password="$2" method="$3" path="$4" body response status
	body=$(cat)
	response=$(curl -sS --max-time 30 -w "${NEWLINE}%{http_code}" \
		-u "${user}:${password}" \
		-H 'Content-Type: application/json' \
		-X "$method" "${ENDPOINT}/api/v1${path}" \
		-d "$body")
	status=${response##*"$NEWLINE"}
	response=${response%"$NEWLINE"*}
	case "$status" in
	2*)
		printf '%s' "$response"
		;;
	*)
		log "gitea API ${method} ${path} (as ${user}) returned HTTP ${status}: ${response}"
		return 1
		;;
	esac
}

# Remove a prior container of the fixed name so a repeat local run reclaims the
# host port instead of failing to bind it.
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

log "starting ${IMAGE} as ${CONTAINER_NAME} on host port ${HOST_PORT}"
# INSTALL_LOCK skips the first-run wizard, without which the version route
# answers before a user or token can be created. DISABLE_REGISTRATION shuts off
# open signup.
docker run -d --name "$CONTAINER_NAME" \
	-p "${HOST_PORT}:3000" \
	-e GITEA__security__INSTALL_LOCK=true \
	-e GITEA__service__DISABLE_REGISTRATION=true \
	"$IMAGE" >/dev/null

log "waiting up to ${READINESS_TIMEOUT}s for ${ENDPOINT}/api/v1/version"
deadline=$((SECONDS + READINESS_TIMEOUT))
ready=false
while [ "$SECONDS" -lt "$deadline" ]; do
	status=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' \
		"${ENDPOINT}/api/v1/version" 2>/dev/null || true)
	if [ "$status" = "200" ]; then
		ready=true
		break
	fi
	sleep "$POLL_INTERVAL"
done
if [ "$ready" != "true" ]; then
	log "gitea did not become ready within ${READINESS_TIMEOUT}s"
	exit 1
fi
log "gitea is ready"

# The --must-change-password=false flag is load-bearing: a fresh admin is
# otherwise forced into a password-change state that blocks token creation.
log "creating admin user ${GITEA_USER}"
docker exec "$CONTAINER_NAME" gitea admin user create \
	--username "$GITEA_USER" \
	--password "$GITEA_PASSWORD" \
	--email "$GITEA_EMAIL" \
	--admin \
	--must-change-password=false >/dev/null

# Minted under basic auth. The scopes are the minimum covering every operation
# the suite exercises plus the two construction preflights.
log "creating access token"
token_response=$(curl -sS --max-time 30 \
	-u "${GITEA_USER}:${GITEA_PASSWORD}" \
	-H 'Content-Type: application/json' \
	-X POST "${ENDPOINT}/api/v1/users/${GITEA_USER}/tokens" \
	-d '{"name":"sortie-integration","scopes":["write:issue","write:repository","read:user"]}')
TOKEN=$(printf '%s' "$token_response" | jq -er '.sha1') || {
	log "token creation failed: $(printf '%s' "$token_response" | jq -r '.message // .')"
	exit 1
}

log "creating repository ${PROJECT}"
jq -nc --arg name "$GITEA_REPO" '{name: $name, private: false, auto_init: true}' |
	gitea_call POST "/user/repos" basic >/dev/null

# The five workflow-state and category labels the fixture assigns to its issues.
create_label() {
	local name="$1"
	jq -nc --arg name "$name" '{name: $name, color: "#cccccc"}' |
		gitea_call POST "/repos/${PROJECT}/labels" token | jq -er '.id'
}
log "creating labels"
label_backlog=$(create_label "backlog")
label_in_progress=$(create_label "in-progress")
label_review=$(create_label "review")
label_done=$(create_label "done")
label_bug=$(create_label "bug")

# Echoes the new issue's repo index.
create_issue() {
	local title="$1" labels="$2"
	jq -nc --arg title "$title" --argjson labels "$labels" \
		'{title: $title, body: "Seed issue for the gitea adapter integration fixture.", labels: $labels}' |
		gitea_call POST "/repos/${PROJECT}/issues" token | jq -er '.number'
}

# The earliest-created issue owns the comments and the blocker relationship, so
# candidate ordering puts it first. The sleeps keep the second-precision
# timestamps strictly ordered, which is what makes that first pick deterministic.
log "creating seed issues"
index_backlog=$(create_issue "Backlog seed issue" "[${label_backlog}]")
sleep 1
index_in_progress=$(create_issue "In-progress seed issue" "[${label_in_progress}, ${label_bug}]")
sleep 1
# Only needs to exist as a third open candidate; its index is never used again.
create_issue "Review seed issue" "[${label_review}]" >/dev/null
sleep 1
index_done=$(create_issue "Done seed issue" "[${label_done}]")

log "adding comments to issue ${index_backlog}"
jq -nc '{body: "First seed comment."}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_backlog}/comments" token >/dev/null
jq -nc '{body: "Second seed comment."}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_backlog}/comments" token >/dev/null

# Gitea takes the blocker from the body and the blocked issue from the path, and
# the body needs the full owner/repo/index triple, not the index alone.
log "linking issue ${index_backlog} as a blocker of ${index_in_progress}"
jq -nc --argjson index "$index_backlog" --arg owner "$GITEA_USER" --arg repo "$GITEA_REPO" \
	'{index: $index, owner: $owner, repo: $repo}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_in_progress}/dependencies" token >/dev/null

log "closing issue ${index_done} into its terminal state"
jq -nc '{state: "closed"}' |
	gitea_call PATCH "/repos/${PROJECT}/issues/${index_done}" token >/dev/null

# Gitea forbids a REQUEST_CHANGES self-review, so the reviewer and the
# allowlisted bot are distinct users from the admin PR author.
REVIEWER_USER="sortie-reviewer"
REVIEWER_PASSWORD="Sortie-Reviewer-Pw1"
BOT_USER="sortie-review-bot"
BOT_PASSWORD="Sortie-Review-Bot-Pw1"
FEATURE_BRANCH="feature-x"
PROBE_BRANCH="status-probe"
SENTINEL_PATH="review-sentinel.txt"
# Proves the per-status target_url field name round-trips to CheckRun.DetailsURL.
# The integration test hard-codes the same value.
FAILING_TARGET_URL="https://ci.example.com/build/12345"
# Above both DEFAULT_PAGING_NUM (30) and MAX_RESPONSE_ITEMS (50), so a truncating
# combined-status read is detectable. The integration test hard-codes the count.
PROBE_STATUS_COUNT=51

log "creating reviewer and bot users"
docker exec "$CONTAINER_NAME" gitea admin user create \
	--username "$REVIEWER_USER" \
	--password "$REVIEWER_PASSWORD" \
	--email "${REVIEWER_USER}@example.com" \
	--must-change-password=false >/dev/null
docker exec "$CONTAINER_NAME" gitea admin user create \
	--username "$BOT_USER" \
	--password "$BOT_PASSWORD" \
	--email "${BOT_USER}@example.com" \
	--must-change-password=false >/dev/null

log "adding reviewer and bot as repository collaborators"
jq -nc '{permission: "write"}' |
	gitea_call PUT "/repos/${PROJECT}/collaborators/${REVIEWER_USER}" basic >/dev/null
jq -nc '{permission: "write"}' |
	gitea_call PUT "/repos/${PROJECT}/collaborators/${BOT_USER}" basic >/dev/null

# Resolved rather than assumed to be "main", so a changed instance default does
# not break the PR base.
default_branch=$(curl -sS --max-time 30 \
	-H "Authorization: token ${TOKEN}" \
	"${ENDPOINT}/api/v1/repos/${PROJECT}" | jq -er '.default_branch')

log "creating branch ${FEATURE_BRANCH} and committing the sentinel file"
jq -nc --arg new "$FEATURE_BRANCH" --arg old "$default_branch" \
	'{new_branch_name: $new, old_branch_name: $old}' |
	gitea_call POST "/repos/${PROJECT}/branches" token >/dev/null
# A NEW multi-line file: every line is an added diff line, so an inline comment
# anchored to line 1 reads back with position > 0 (Outdated == false).
sentinel_content=$(printf 'sentinel line 1\nsentinel line 2\nsentinel line 3\nsentinel line 4\nsentinel line 5\n' | base64 | tr -d '\n')
head_sha=$(jq -nc --arg content "$sentinel_content" --arg branch "$FEATURE_BRANCH" \
	'{content: $content, branch: $branch, message: "Add review sentinel file"}' |
	gitea_call POST "/repos/${PROJECT}/contents/${SENTINEL_PATH}" token | jq -er '.commit.sha')

log "opening the review pull request"
pr_index=$(jq -nc --arg head "$FEATURE_BRANCH" --arg base "$default_branch" \
	'{head: $head, base: $base, title: "Review fixture pull request", body: "Seed PR for the gitea SCM adapter integration fixture."}' |
	gitea_call POST "/repos/${PROJECT}/pulls" token | jq -er '.number')

log "submitting the human REQUEST_CHANGES review as ${REVIEWER_USER}"
jq -nc --arg path "$SENTINEL_PATH" \
	'{event: "REQUEST_CHANGES", body: "Please address the inline comment.", comments: [{path: $path, body: "Reviewer inline comment on the sentinel file.", new_position: 1}]}' |
	user_call "$REVIEWER_USER" "$REVIEWER_PASSWORD" POST "/repos/${PROJECT}/pulls/${pr_index}/reviews" >/dev/null

log "submitting the bot COMMENT review as ${BOT_USER}"
jq -nc --arg path "$SENTINEL_PATH" \
	'{event: "COMMENT", body: "Automated review note.", comments: [{path: $path, body: "Bot inline comment on the sentinel file.", new_position: 1}]}' |
	user_call "$BOT_USER" "$BOT_PASSWORD" POST "/repos/${PROJECT}/pulls/${pr_index}/reviews" >/dev/null

# One success and one failure with a target_url, well under any page size, so the
# failing entry is always on page one for every combined-status read.
log "seeding commit A statuses on the pull request head"
jq -nc '{state: "success", context: "ci/build"}' |
	gitea_call POST "/repos/${PROJECT}/statuses/${head_sha}" token >/dev/null
jq -nc --arg url "$FAILING_TARGET_URL" \
	'{state: "failure", context: "ci/test", target_url: $url, description: "The test job failed."}' |
	gitea_call POST "/repos/${PROJECT}/statuses/${head_sha}" token >/dev/null

# A dedicated commit for the pagination probe, kept off the PR head so a
# paginating route cannot push the head's failing entry off page one.
log "creating the many-status probe commit on branch ${PROBE_BRANCH}"
jq -nc --arg new "$PROBE_BRANCH" --arg old "$default_branch" \
	'{new_branch_name: $new, old_branch_name: $old}' |
	gitea_call POST "/repos/${PROJECT}/branches" token >/dev/null
probe_content=$(printf 'status pagination probe commit\n' | base64 | tr -d '\n')
probe_sha=$(jq -nc --arg content "$probe_content" --arg branch "$PROBE_BRANCH" \
	'{content: $content, branch: $branch, message: "Add status probe file"}' |
	gitea_call POST "/repos/${PROJECT}/contents/status-probe.txt" token | jq -er '.commit.sha')

log "seeding ${PROBE_STATUS_COUNT} distinct-context statuses on the probe commit"
for ((i = 1; i <= PROBE_STATUS_COUNT; i++)); do
	jq -nc --arg ctx "ci/probe-${i}" '{state: "success", context: $ctx}' |
		gitea_call POST "/repos/${PROJECT}/statuses/${probe_sha}" token >/dev/null
done

# One add-and-remove pair on the PR timeline (PRs share the issue index space)
# seeds a labeled and an unlabeled event for the label-event read.
log "seeding a label add-and-remove pair on the pull request timeline"
jq -nc --argjson id "$label_review" '{labels: [$id]}' |
	gitea_call POST "/repos/${PROJECT}/issues/${pr_index}/labels" token >/dev/null
gitea_call DELETE "/repos/${PROJECT}/issues/${pr_index}/labels/${label_review}" token </dev/null >/dev/null

# $GITHUB_ENV takes a bare KEY=VALUE; stdout takes an export statement, so an
# eval'd coordinate is inherited by the test process rather than merely set.
emit() {
	if [ -n "${GITHUB_ENV:-}" ] && [ -w "${GITHUB_ENV}" ]; then
		printf '%s=%s\n' "$1" "$2" >>"$GITHUB_ENV"
	fi
	printf "export %s='%s'\n" "$1" "$2"
}

# Mask before the token is echoed anywhere. Only under Actions: locally this
# would pollute eval's stdout.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
	printf '::add-mask::%s\n' "$TOKEN"
fi

log "provisioning complete; publishing coordinates"
emit SORTIE_GITEA_ENDPOINT "$ENDPOINT"
emit SORTIE_GITEA_TOKEN "$TOKEN"
emit SORTIE_GITEA_PROJECT "$PROJECT"
emit SORTIE_GITEA_PR_NUMBER "$pr_index"
emit SORTIE_GITEA_BOT_USERNAME "$BOT_USER"
emit SORTIE_GITEA_MANY_STATUS_SHA "$probe_sha"
emit GITEA_IMAGE "$IMAGE"
