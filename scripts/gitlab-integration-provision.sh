#!/usr/bin/env bash
# Boot and seed a throwaway containerized GitLab Community Edition instance,
# then publish the coordinates the gitlab adapter integration suite reads.
#
# Usage: gitlab-integration-provision.sh [IMAGE]
#   IMAGE defaults to the pinned tag below, the authoritative version for the
#   release path. Rolling it is a deliberate edit here.
#
# Both tokens are minted in-job against a loopback container, so no repository
# secret is needed. A local run leaves a multi-gigabyte container resident:
#   docker rm -f sortie-gitlab-integration
#
# Coordinates go to $GITHUB_ENV when writable and to stdout as exports, so
#   eval "$(scripts/gitlab-integration-provision.sh)"
# works locally. Everything else goes to stderr, keeping stdout eval-parseable.
#
# READINESS_TIMEOUT_SECONDS is 600 against a measured 111s baseline (two boots,
# 4 CPUs and 16 GB, the documented ubuntu-latest shape); the headroom covers a
# hosted runner's disk throughput, which that constraint does not reproduce.

set -euo pipefail

IMAGE="${1:-gitlab/gitlab-ce:19.2.1-ce.0}"

CONTAINER_NAME="sortie-gitlab-integration"
HOST_PORT=8929
ENDPOINT="http://localhost:${HOST_PORT}"

READINESS_TIMEOUT_SECONDS=600
POLL_INTERVAL_SECONDS=10
PULL_RETRY_DELAY_SECONDS=10
AUTHORIZATION_TIMEOUT_SECONDS=120
AUTHORIZATION_POLL_INTERVAL_SECONDS=2

GROUP_NAME="sortie-gitlab-integration"
PRIMARY_PROJECT_NAME="primary"
SIBLING_PROJECT_NAME="sibling"

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

# Method, path, and token are positional; the JSON body comes from stdin. A
# non-2xx status logs the response and fails, which set -e turns into an exit.
gitlab_call() {
	local method="$1" path="$2" token="$3" body response status
	body=$(cat)
	response=$(curl -sS --max-time 30 -w "${NEWLINE}%{http_code}" \
		-H "PRIVATE-TOKEN: ${token}" \
		-H 'Content-Type: application/json' \
		-X "$method" "${ENDPOINT}/api/v4${path}" \
		-d "$body")
	status=${response##*"$NEWLINE"}
	response=${response%"$NEWLINE"*}
	case "$status" in
	2*)
		printf '%s' "$response"
		;;
	*)
		log "gitlab API ${method} ${path} returned HTTP ${status}: ${response}"
		return 1
		;;
	esac
}

# Remove a prior container of the fixed name so a repeat local run reclaims
# the host port instead of failing to bind it.
docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# Released Community Edition images live only on Docker Hub, pulled anonymously.
# Keeping the pull its own step separates a registry failure from a boot one.
log "pulling ${IMAGE} from Docker Hub"
if ! docker pull "$IMAGE" >/dev/null; then
	log "initial pull of ${IMAGE} from Docker Hub failed; retrying in ${PULL_RETRY_DELAY_SECONDS}s"
	sleep "$PULL_RETRY_DELAY_SECONDS"
	if ! docker pull "$IMAGE" >/dev/null; then
		log "failed to pull ${IMAGE} from Docker Hub after two attempts"
		exit 1
	fi
fi

log "starting ${IMAGE} as ${CONTAINER_NAME} on host port ${HOST_PORT}"
# Limited to the verified-minimal key set: any other key, in particular
# grafana['enable'], fails gitlab-ctl reconfigure and exits the container
# within seconds.
docker run -d --name "$CONTAINER_NAME" \
	-p "${HOST_PORT}:8929" \
	--shm-size=256m \
	-e GITLAB_OMNIBUS_CONFIG="external_url 'http://localhost:${HOST_PORT}'; puma['worker_processes'] = 0; sidekiq['concurrency'] = 1; prometheus_monitoring['enable'] = false; registry['enable'] = false; gitlab_kas['enable'] = false;" \
	"$IMAGE" >/dev/null

log "waiting up to ${READINESS_TIMEOUT_SECONDS}s for ${ENDPOINT}/api/v4/version"
SECONDS=0
ready=false
while [ "$SECONDS" -lt "$READINESS_TIMEOUT_SECONDS" ]; do
	if [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" != "true" ]; then
		log "container exited before readiness; a boot-configuration failure is the usual cause"
		exit 1
	fi

	status=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "${ENDPOINT}/api/v4/version" 2>/dev/null || true)
	case "$status" in
	200 | 401)
		ready=true
		break
		;;
	esac

	sleep "$POLL_INTERVAL_SECONDS"
done
if [ "$ready" != "true" ]; then
	log "gitlab did not become ready within ${READINESS_TIMEOUT_SECONDS}s"
	exit 1
fi
log "gitlab ready after ${SECONDS}s"

# No resource-owner password grant at this version, so the first token comes
# from inside the container. The expiry is read off the instance clock: a date
# literal here would expire as the calendar moves.
log "minting bootstrap administrator token"
bootstrap_runner_script='
user = User.find_by(username: "root")
expiry = Date.today + 1
token = user.personal_access_tokens.create!(name: "sortie-integration-bootstrap", scopes: ["api"], expires_at: expiry)
puts "SORTIE_EXPIRY=#{expiry.iso8601}"
puts "SORTIE_BOOTSTRAP_TOKEN=#{token.token}"
'
runner_output=$(docker exec "$CONTAINER_NAME" gitlab-rails runner "$bootstrap_runner_script")

EXPIRY=$(printf '%s\n' "$runner_output" | sed -n 's/^SORTIE_EXPIRY=//p')
BOOTSTRAP_TOKEN=$(printf '%s\n' "$runner_output" | sed -n 's/^SORTIE_BOOTSTRAP_TOKEN=//p')
if [ -z "$EXPIRY" ] || [ -z "$BOOTSTRAP_TOKEN" ]; then
	log "bootstrap token minting produced unparseable output"
	exit 1
fi

# A Developer-level project access token cannot create a project, so the group,
# both projects, and the published token are the bootstrap identity's only calls.
log "creating group ${GROUP_NAME}"
group_response=$(jq -nc --arg name "$GROUP_NAME" --arg path "$GROUP_NAME" \
	'{name: $name, path: $path, visibility: "private"}' |
	gitlab_call POST "/groups" "$BOOTSTRAP_TOKEN")
GROUP_ID=$(printf '%s' "$group_response" | jq -er '.id')

log "creating sibling project ${SIBLING_PROJECT_NAME}"
sibling_response=$(jq -nc --arg name "$SIBLING_PROJECT_NAME" --argjson namespace_id "$GROUP_ID" \
	'{name: $name, namespace_id: $namespace_id, visibility: "private"}' |
	gitlab_call POST "/projects" "$BOOTSTRAP_TOKEN")
SIBLING_ID=$(printf '%s' "$sibling_response" | jq -er '.id')
SIBLING_PATH=$(printf '%s' "$sibling_response" | jq -er '.path_with_namespace')

log "creating primary project ${PRIMARY_PROJECT_NAME}"
primary_response=$(jq -nc --arg name "$PRIMARY_PROJECT_NAME" --argjson namespace_id "$GROUP_ID" \
	'{name: $name, namespace_id: $namespace_id, visibility: "private"}' |
	gitlab_call POST "/projects" "$BOOTSTRAP_TOKEN")
PRIMARY_ID=$(printf '%s' "$primary_response" | jq -er '.id')
PRIMARY_PATH=$(printf '%s' "$primary_response" | jq -er '.path_with_namespace')

# Seeding the sibling first is what makes the primary project's iids diverge
# from their instance-global ids.
log "seeding sibling project issues"
jq -nc '{title: "Sibling seed issue 1"}' |
	gitlab_call POST "/projects/${SIBLING_ID}/issues" "$BOOTSTRAP_TOKEN" >/dev/null
jq -nc '{title: "Sibling seed issue 2"}' |
	gitlab_call POST "/projects/${SIBLING_ID}/issues" "$BOOTSTRAP_TOKEN" >/dev/null

log "creating published project access token"
published_response=$(jq -nc --arg name "sortie-integration-published" --arg expires_at "$EXPIRY" \
	'{name: $name, scopes: ["api"], access_level: 30, expires_at: $expires_at}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/access_tokens" "$BOOTSTRAP_TOKEN")
PUBLISHED_TOKEN=$(printf '%s' "$published_response" | jq -er '.token')

# The bot's project authorization lands after the token does: the token
# authenticates immediately, while a project-scoped call still answers 404
# until the authorization refresh commits. Gate on the project being visible
# to the token itself, or every fixture below races that refresh.
log "waiting up to ${AUTHORIZATION_TIMEOUT_SECONDS}s for the published token's project authorization"
SECONDS=0
authorized=false
while [ "$SECONDS" -lt "$AUTHORIZATION_TIMEOUT_SECONDS" ]; do
	status=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' \
		-H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}" \
		"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}" 2>/dev/null || true)
	if [ "$status" = "200" ]; then
		authorized=true
		break
	fi
	sleep "$AUTHORIZATION_POLL_INTERVAL_SECONDS"
done
if [ "$authorized" != "true" ]; then
	log "published token never saw project ${PRIMARY_ID} within ${AUTHORIZATION_TIMEOUT_SECONDS}s; last status ${status}"
	exit 1
fi
log "published token authorized after ${SECONDS}s"

PUBLISHED_USER_ID=$(curl -sS --max-time 30 -H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}" "${ENDPOINT}/api/v4/user" | jq -er '.id')

# Every fixture below is created under the published Developer-level token, so
# the suite runs against objects its own identity could have made.
log "creating project labels"
primary_labels=(backlog in-progress review "done" needs-human)
for label in "${primary_labels[@]}"; do
	jq -nc --arg name "$label" '{name: $name, color: "#cccccc"}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/labels" "$PUBLISHED_TOKEN" >/dev/null
done

# Echoes the new issue's iid. The sleeps between calls below keep creation
# timestamps strictly ordered, which is what makes "first candidate" and
# "earliest-created" deterministic.
create_primary_issue() {
	local title="$1" labels_json="$2" extra_json="${3:-{}}"
	jq -nc --arg title "$title" --argjson labels "$labels_json" --argjson extra "$extra_json" \
		'{title: $title, labels: $labels} + $extra' |
		gitlab_call POST "/projects/${PRIMARY_ID}/issues" "$PUBLISHED_TOKEN" | jq -er '.iid'
}

add_comment() {
	local iid="$1" body="$2"
	jq -nc --arg body "$body" '{body: $body}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/issues/${iid}/notes" "$PUBLISHED_TOKEN" >/dev/null
}

log "creating seed issues in ascending time order"
issue_backlog_1=$(create_primary_issue "Backlog seed issue" '["backlog"]')
sleep 1
issue_inprogress=$(create_primary_issue "In-progress seed issue" '["in-progress"]')
sleep 1
issue_review=$(create_primary_issue "Review seed issue" '["review"]')
sleep 1
issue_backlog_2=$(create_primary_issue "Second backlog seed issue" '["backlog"]')
sleep 1
issue_bulk=$(create_primary_issue "Bulk comment seed issue" '["backlog"]')
sleep 1
issue_linktarget=$(create_primary_issue "Link target seed issue" '["in-progress"]')
sleep 1
issue_done=$(create_primary_issue "Done seed issue" '["done"]')
log "seeded open candidates ${issue_backlog_1}, ${issue_inprogress}, ${issue_review}, ${issue_backlog_2}, ${issue_bulk}, ${issue_linktarget} and terminal candidate ${issue_done}"

log "assigning issue ${issue_inprogress} to the published token's own identity"
jq -nc --argjson id "$PUBLISHED_USER_ID" '{assignee_ids: [$id]}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/issues/${issue_inprogress}" "$PUBLISHED_TOKEN" >/dev/null

log "adding two ordered comments to the earliest-created issue ${issue_backlog_1}"
add_comment "$issue_backlog_1" "First seed comment."
add_comment "$issue_backlog_1" "Second seed comment."

log "seeding at least 101 comments on issue ${issue_bulk}"
for i in $(seq 1 101); do
	add_comment "$issue_bulk" "Bulk seed comment ${i}."
done

log "linking issue ${issue_bulk} to ${issue_linktarget}, which carries no human comment"
jq -nc --argjson target_project_id "$PRIMARY_ID" --argjson target_issue_iid "$issue_linktarget" \
	'{target_project_id: $target_project_id, target_issue_iid: $target_issue_iid}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/issues/${issue_bulk}/links" "$PUBLISHED_TOKEN" >/dev/null

log "creating one non-issue work item"
issue_task=$(jq -nc --arg title "Task work item" '{title: $title, issue_type: "task"}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/issues" "$PUBLISHED_TOKEN" | jq -er '.iid')

log "closing issue ${issue_done} into its terminal state"
jq -nc '{state_event: "close"}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/issues/${issue_done}" "$PUBLISHED_TOKEN" >/dev/null

# $GITHUB_ENV takes a bare KEY=VALUE; stdout takes an export statement, so an
# eval'd coordinate is inherited by the test process rather than merely set.
emit() {
	if [ -n "${GITHUB_ENV:-}" ] && [ -w "${GITHUB_ENV}" ]; then
		printf '%s=%s\n' "$1" "$2" >>"$GITHUB_ENV"
	fi
	printf "export %s='%s'\n" "$1" "$2"
}

# Mask before the token is echoed anywhere. The bootstrap token is never
# emitted at all. Only under Actions: locally this would pollute eval's stdout.
if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
	printf '::add-mask::%s\n' "$PUBLISHED_TOKEN"
fi

log "provisioning complete; publishing coordinates"
emit SORTIE_GITLAB_ENDPOINT "$ENDPOINT"
emit SORTIE_GITLAB_TOKEN "$PUBLISHED_TOKEN"
emit SORTIE_GITLAB_PROJECT "$PRIMARY_PATH"
emit SORTIE_GITLAB_MANY_NOTES_IID "$issue_bulk"
emit SORTIE_GITLAB_SYSTEM_NOTES_IID "$issue_linktarget"
emit SORTIE_GITLAB_TASK_IID "$issue_task"
emit SORTIE_GITLAB_FOREIGN_PROJECT "$SIBLING_PATH"
emit GITLAB_IMAGE "$IMAGE"
