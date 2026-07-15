#!/usr/bin/env bash
# Boot and seed a throwaway containerized Gitea, then publish the coordinates
# the gitea adapter integration suite reads.
#
# The instance is ephemeral: the access token is generated in-job against a
# loopback container, so nothing long-lived is stored and no repository secret
# is needed. In CI the runner is discarded after the job; locally the script
# removes any prior container of its fixed name before booting, so a repeat run
# does not collide on the host port.
#
# Output contract. Four coordinates are published two ways. They are appended as
# bare KEY=VALUE lines to the file named by $GITHUB_ENV when it is set and
# writable, which Actions exports to successor CI steps. They are also printed to
# stdout as export statements, so a developer can run
#   eval "$(scripts/gitea-integration-provision.sh)"
# and get them exported into the current shell. Everything else goes to stderr,
# keeping stdout parseable by eval.
#
# Prerequisites, all present on ubuntu-latest and expected locally: docker (a
# running daemon), curl, and jq.
#
# Usage: gitea-integration-provision.sh [IMAGE]
#   IMAGE defaults to the pinned tag below, which is the authoritative version
#   for the release path. Rolling the pinned version is a deliberate edit here.

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

# The rootless image boots quickly; poll on a short interval and give up well
# inside the nightly job budget rather than hang a stuck boot.
READINESS_TIMEOUT=90
POLL_INTERVAL=2

NEWLINE=$'\n'

# Diagnostics go to stderr so stdout carries only the coordinate assignments.
log() {
	printf '%s\n' "$*" >&2
}

# On a non-zero exit after the container exists, surface its logs so a boot,
# readiness, or provisioning failure is diagnosable. The container is never
# removed here: CI discards the runner, and the next local run clears it.
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

# A Gitea API call. Method, path, and auth mode are positional; the JSON request
# body is read from stdin. Auth mode is "token" (the generated access token) or
# "basic" (the admin user and password). On any non-2xx status the response body
# is logged and the call fails, so set -e stops the script and the trap dumps
# the container logs.
#
# Repository creation needs a scope the published token deliberately omits, so
# that one call runs under basic auth; every issue-scoped call runs under the
# token, exercising that the published scopes cover the fixture's issue writes.
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

# Remove a prior container of the fixed name so a repeat local run reclaims the
# host port instead of failing to bind it.
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

log "starting ${IMAGE} as ${CONTAINER_NAME} on host port ${HOST_PORT}"
# INSTALL_LOCK skips the first-run install wizard and DISABLE_REGISTRATION turns
# off open signup; without the lock the version route answers before the
# instance can create a user or a token.
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

# Mint the token under basic auth. The scope set is the minimum that covers
# every operation the suite exercises plus the two construction preflights.
log "creating access token"
token_response=$(curl -sS --max-time 30 \
	-u "${GITEA_USER}:${GITEA_PASSWORD}" \
	-H 'Content-Type: application/json' \
	-X POST "${ENDPOINT}/api/v1/users/${GITEA_USER}/tokens" \
	-d '{"name":"sortie-integration","scopes":["write:issue","read:user","read:repository"]}')
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

# Create an issue with the given title and label ids and echo its repo index.
create_issue() {
	local title="$1" labels="$2"
	jq -nc --arg title "$title" --argjson labels "$labels" \
		'{title: $title, body: "Seed issue for the gitea adapter integration fixture.", labels: $labels}' |
		gitea_call POST "/repos/${PROJECT}/issues" token | jq -er '.number'
}

# The earliest-created issue owns the comments and the blocker relationship, so
# the candidate ordering (ascending by creation time) puts it first. A short
# pause between creations keeps the second-precision timestamps strictly
# ordered, so the first candidate is deterministic.
log "creating seed issues"
index_backlog=$(create_issue "Backlog seed issue" "[${label_backlog}]")
sleep 1
index_in_progress=$(create_issue "In-progress seed issue" "[${label_in_progress}, ${label_bug}]")
sleep 1
# The review issue only needs to exist as a third open candidate; its index is
# not referenced again, so it is not captured.
create_issue "Review seed issue" "[${label_review}]" >/dev/null
sleep 1
index_done=$(create_issue "Done seed issue" "[${label_done}]")

log "adding comments to issue ${index_backlog}"
jq -nc '{body: "First seed comment."}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_backlog}/comments" token >/dev/null
jq -nc '{body: "Second seed comment."}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_backlog}/comments" token >/dev/null

# Record that the backlog issue blocks the in-progress issue. Gitea reads the
# blocker from the body and the blocked issue from the path, and it needs the
# full owner/repo/index triple, not the index alone.
log "linking issue ${index_backlog} as a blocker of ${index_in_progress}"
jq -nc --argjson index "$index_backlog" --arg owner "$GITEA_USER" --arg repo "$GITEA_REPO" \
	'{index: $index, owner: $owner, repo: $repo}' |
	gitea_call POST "/repos/${PROJECT}/issues/${index_in_progress}/dependencies" token >/dev/null

log "closing issue ${index_done} into its terminal state"
jq -nc '{state: "closed"}' |
	gitea_call PATCH "/repos/${PROJECT}/issues/${index_done}" token >/dev/null

# Publish one coordinate to $GITHUB_ENV (when writable) and to stdout. The
# $GITHUB_ENV file takes a bare KEY=VALUE line, which Actions exports to
# successor steps. Stdout takes an export statement, so a developer running
# eval "$(...)" gets the coordinate exported into the shell and inherited by the
# test process, not merely set as a non-exported shell variable.
emit() {
	if [ -n "${GITHUB_ENV:-}" ] && [ -w "${GITHUB_ENV}" ]; then
		printf '%s=%s\n' "$1" "$2" >>"$GITHUB_ENV"
	fi
	printf "export %s='%s'\n" "$1" "$2"
}

# Register the token as a masked value so it is redacted from the CI log. The
# mask directive is emitted only under Actions; locally it would pollute the
# stdout that eval consumes.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
	printf '::add-mask::%s\n' "$TOKEN"
fi

log "provisioning complete; publishing coordinates"
emit SORTIE_GITEA_ENDPOINT "$ENDPOINT"
emit SORTIE_GITEA_TOKEN "$TOKEN"
emit SORTIE_GITEA_PROJECT "$PROJECT"
emit GITEA_IMAGE "$IMAGE"
