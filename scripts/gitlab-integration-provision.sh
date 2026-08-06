#!/bin/sh
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

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/provision.sh
. "${SCRIPT_DIR}/lib/provision.sh"

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

trap dump_logs_on_error EXIT

require_tools docker curl jq

gitlab_call() {
	_gl_method=$1 _gl_path=$2 _gl_token=$3
	api_call "$_gl_method" "${ENDPOINT}/api/v4${_gl_path}" -H "PRIVATE-TOKEN: ${_gl_token}"
}

reclaim_container

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
	-p "${HOST_PORT}:${HOST_PORT}" \
	--shm-size=256m \
	-e GITLAB_OMNIBUS_CONFIG="external_url 'http://localhost:${HOST_PORT}'; puma['worker_processes'] = 0; sidekiq['concurrency'] = 1; prometheus_monitoring['enable'] = false; registry['enable'] = false; gitlab_kas['enable'] = false;" \
	"$IMAGE" >/dev/null

# 401 counts as ready: the route answers before any credential exists.
log "waiting up to ${READINESS_TIMEOUT_SECONDS}s for ${ENDPOINT}/api/v4/version"
if ! wait_for_http "${ENDPOINT}/api/v4/version" "200 401" \
	"$READINESS_TIMEOUT_SECONDS" "$POLL_INTERVAL_SECONDS"; then
	log "gitlab did not become ready within ${READINESS_TIMEOUT_SECONDS}s"
	exit 1
fi
log "gitlab ready after ${WAITED_SECONDS}s"

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
if ! wait_for_http "${ENDPOINT}/api/v4/projects/${PRIMARY_ID}" "200" \
	"$AUTHORIZATION_TIMEOUT_SECONDS" "$AUTHORIZATION_POLL_INTERVAL_SECONDS" \
	-H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}"; then
	log "published token never saw project ${PRIMARY_ID} within ${AUTHORIZATION_TIMEOUT_SECONDS}s; last status ${WAIT_STATUS}"
	exit 1
fi
log "published token authorized after ${WAITED_SECONDS}s"

PUBLISHED_USER_ID=$(curl -sS --max-time 30 -H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}" "${ENDPOINT}/api/v4/user" | jq -er '.id')

# Every fixture below is created under the published Developer-level token, so
# the suite runs against objects its own identity could have made.
log "creating project labels"
for label in backlog in-progress review 'done' needs-human; do
	jq -nc --arg name "$label" '{name: $name, color: "#cccccc"}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/labels" "$PUBLISHED_TOKEN" >/dev/null
done

# Echoes the new issue's iid. The sleeps between calls below keep creation
# timestamps strictly ordered, which is what makes "first candidate" and
# "earliest-created" deterministic.
create_primary_issue() {
	_cpi_extra=${3:-\{\}}
	_cpi_out=$(jq -nc --arg title "$1" --argjson labels "$2" --argjson extra "$_cpi_extra" \
		'{title: $title, labels: $labels} + $extra' |
		gitlab_call POST "/projects/${PRIMARY_ID}/issues" "$PUBLISHED_TOKEN")
	printf '%s' "$_cpi_out" | jq -er '.iid'
}

add_comment() {
	jq -nc --arg body "$2" '{body: $body}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/issues/${1}/notes" "$PUBLISHED_TOKEN" >/dev/null
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
i=1
while [ "$i" -le 101 ]; do
	add_comment "$issue_bulk" "Bulk seed comment ${i}."
	i=$((i + 1))
done

log "linking issue ${issue_bulk} to ${issue_linktarget}, which carries no human comment"
jq -nc --argjson target_project_id "$PRIMARY_ID" --argjson target_issue_iid "$issue_linktarget" \
	'{target_project_id: $target_project_id, target_issue_iid: $target_issue_iid}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/issues/${issue_bulk}/links" "$PUBLISHED_TOKEN" >/dev/null

log "creating one non-issue work item"
task_response=$(jq -nc --arg title "Task work item" '{title: $title, issue_type: "task"}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/issues" "$PUBLISHED_TOKEN")
issue_task=$(printf '%s' "$task_response" | jq -er '.iid')

log "closing issue ${issue_done} into its terminal state"
jq -nc '{state_event: "close"}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/issues/${issue_done}" "$PUBLISHED_TOKEN" >/dev/null

# The bootstrap token is never emitted, so only this one needs masking.
mask_secret "$PUBLISHED_TOKEN"

log "provisioning complete; publishing coordinates"
emit SORTIE_GITLAB_ENDPOINT "$ENDPOINT"
emit SORTIE_GITLAB_TOKEN "$PUBLISHED_TOKEN"
emit SORTIE_GITLAB_PROJECT "$PRIMARY_PATH"
emit SORTIE_GITLAB_MANY_NOTES_IID "$issue_bulk"
emit SORTIE_GITLAB_SYSTEM_NOTES_IID "$issue_linktarget"
emit SORTIE_GITLAB_TASK_IID "$issue_task"
emit SORTIE_GITLAB_FOREIGN_PROJECT "$SIBLING_PATH"
emit GITLAB_IMAGE "$IMAGE"
