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

# wait_for_json FIELD_EXPR EXPECTED URL TIMEOUT INTERVAL
#
# Polls URL under the published token until `jq -r FIELD_EXPR` equals
# EXPECTED or TIMEOUT elapses, the same shape as the shared HTTP-status
# poll uses for a JSON field instead of a status code. GitLab is the only
# forge whose fixture fields settle asynchronously, so this stays local
# to this script rather than joining scripts/lib/provision.sh for a
# second caller that does not exist.
wait_for_json() {
	_wj_field=$1 _wj_expected=$2 _wj_url=$3 _wj_timeout=$4 _wj_interval=$5
	_wj_start=$(date +%s)
	while :; do
		_wj_value=$(curl -sS --max-time 30 -H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}" "$_wj_url" | jq -r "$_wj_field" 2>/dev/null || true)
		if [ "$_wj_value" = "$_wj_expected" ]; then
			return 0
		fi
		_wj_elapsed=$(($(date +%s) - _wj_start))
		[ "$_wj_elapsed" -lt "$_wj_timeout" ] || break
		sleep "$_wj_interval"
	done
	log "fixture ${_wj_url} field ${_wj_field} was ${_wj_value} after ${_wj_timeout}s, want ${_wj_expected}"
	return 1
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

# The primary project moves one namespace level deeper than the sibling so
# the forge suite exercises a project addressed through a nested namespace
# path, the rule the adapter is forbidden to split or validate as one
# segment.
log "creating subgroup team"
team_response=$(jq -nc --arg name "team" --arg path "team" --argjson parent_id "$GROUP_ID" \
	'{name: $name, path: $path, parent_id: $parent_id, visibility: "private"}' |
	gitlab_call POST "/groups" "$BOOTSTRAP_TOKEN")
TEAM_GROUP_ID=$(printf '%s' "$team_response" | jq -er '.id')

# initialize_with_readme seeds the one commit every branch-based forge
# fixture below is cut from; a project with no default branch has no ref
# to branch off of.
log "creating primary project ${PRIMARY_PROJECT_NAME}"
primary_response=$(jq -nc --arg name "$PRIMARY_PROJECT_NAME" --argjson namespace_id "$TEAM_GROUP_ID" \
	'{name: $name, namespace_id: $namespace_id, visibility: "private", initialize_with_readme: true}' |
	gitlab_call POST "/projects" "$BOOTSTRAP_TOKEN")
PRIMARY_ID=$(printf '%s' "$primary_response" | jq -er '.id')
PRIMARY_PATH=$(printf '%s' "$primary_response" | jq -er '.path_with_namespace')
DEFAULT_BRANCH=$(printf '%s' "$primary_response" | jq -er '.default_branch')

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

# Fixed values the integration suite hard-codes on its own side; a mismatch
# between the two is a test failure by design.
SEEDED_FAILING_TARGET_URL="https://ci.example.com/build/12345"
SEEDED_JOURNAL_LABEL="forge-journal"

# create_branch BRANCH REF
create_branch() {
	_cb_branch=$1 _cb_ref=$2
	jq -nc --arg branch "$_cb_branch" --arg ref "$_cb_ref" \
		'{branch: $branch, ref: $ref}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/repository/branches" "$PUBLISHED_TOKEN" >/dev/null
}

# commit_file BRANCH PATH CONTENT MESSAGE
#
# Echoes the new commit's id, read from the commits route rather than the
# files route, which returns no commit identifier.
commit_file() {
	_cf_branch=$1 _cf_path=$2 _cf_content=$3 _cf_message=$4
	_cf_out=$(jq -nc --arg branch "$_cf_branch" --arg message "$_cf_message" \
		--arg path "$_cf_path" --arg content "$_cf_content" \
		'{branch: $branch, commit_message: $message,
		  actions: [{action: "create", file_path: $path, content: $content}]}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/repository/commits" "$PUBLISHED_TOKEN")
	printf '%s' "$_cf_out" | jq -er '.id'
}

# post_status SHA STATE NAME [TARGET_URL] [DESCRIPTION]
post_status() {
	_ps_sha=$1 _ps_state=$2 _ps_name=$3 _ps_url=${4:-} _ps_desc=${5:-}
	jq -nc --arg state "$_ps_state" --arg name "$_ps_name" --arg url "$_ps_url" --arg desc "$_ps_desc" \
		'{state: $state, name: $name}
		 + (if $url != "" then {target_url: $url} else {} end)
		 + (if $desc != "" then {description: $desc} else {} end)' |
		gitlab_call POST "/projects/${PRIMARY_ID}/statuses/${_ps_sha}" "$PUBLISHED_TOKEN" >/dev/null
}

# open_mr SOURCE TARGET TITLE TOKEN
#
# Echoes the new merge request's iid.
open_mr() {
	_om_source=$1 _om_target=$2 _om_title=$3 _om_token=$4
	_om_out=$(jq -nc --arg source "$_om_source" --arg target "$_om_target" --arg title "$_om_title" \
		'{source_branch: $source, target_branch: $target, title: $title}' |
		gitlab_call POST "/projects/${PRIMARY_ID}/merge_requests" "$_om_token")
	printf '%s' "$_om_out" | jq -er '.iid'
}

# Two more identities, both used only inside this script and never
# emitted: the published token already covers the fixture the tracker
# suite reads, but every project access token is bot:true, so the forge
# suite needs a platform bot distinct from its own identity and a
# non-bot human to review, approve, and author a comment the bot
# predicate must reject.
log "creating bot project access token"
bot_response=$(jq -nc --arg name "sortie-integration-bot" --arg expires_at "$EXPIRY" \
	'{name: $name, scopes: ["api"], access_level: 30, expires_at: $expires_at}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/access_tokens" "$BOOTSTRAP_TOKEN")
BOT_TOKEN=$(printf '%s' "$bot_response" | jq -er '.token')
BOT_LOGIN=$(curl -sS --max-time 30 -H "PRIVATE-TOKEN: ${BOT_TOKEN}" "${ENDPOINT}/api/v4/user" | jq -er '.username')

log "waiting up to ${AUTHORIZATION_TIMEOUT_SECONDS}s for the bot token's project authorization"
if ! wait_for_http "${ENDPOINT}/api/v4/projects/${PRIMARY_ID}" "200" \
	"$AUTHORIZATION_TIMEOUT_SECONDS" "$AUTHORIZATION_POLL_INTERVAL_SECONDS" \
	-H "PRIVATE-TOKEN: ${BOT_TOKEN}"; then
	log "bot token never saw project ${PRIMARY_ID} within ${AUTHORIZATION_TIMEOUT_SECONDS}s; last status ${WAIT_STATUS}"
	exit 1
fi
log "bot token authorized after ${WAITED_SECONDS}s"

# force_random_password plus skip_confirmation means no password literal
# ever enters this script.
log "creating human user"
HUMAN_USERNAME="sortie-human-$(date +%s)"
human_response=$(jq -nc --arg email "${HUMAN_USERNAME}@example.com" --arg username "$HUMAN_USERNAME" --arg name "Sortie Human" \
	'{email: $email, username: $username, name: $name, force_random_password: true, skip_confirmation: true}' |
	gitlab_call POST "/users" "$BOOTSTRAP_TOKEN")
HUMAN_ID=$(printf '%s' "$human_response" | jq -er '.id')

jq -nc --argjson user_id "$HUMAN_ID" '{user_id: $user_id, access_level: 30}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/members" "$BOOTSTRAP_TOKEN" >/dev/null

human_token_response=$(jq -nc --arg name "sortie-integration-human" --arg expires_at "$EXPIRY" \
	'{name: $name, scopes: ["api"], expires_at: $expires_at}' |
	gitlab_call POST "/users/${HUMAN_ID}/personal_access_tokens" "$BOOTSTRAP_TOKEN")
HUMAN_TOKEN=$(printf '%s' "$human_token_response" | jq -er '.token')

log "waiting up to ${AUTHORIZATION_TIMEOUT_SECONDS}s for the human token's project authorization"
if ! wait_for_http "${ENDPOINT}/api/v4/projects/${PRIMARY_ID}" "200" \
	"$AUTHORIZATION_TIMEOUT_SECONDS" "$AUTHORIZATION_POLL_INTERVAL_SECONDS" \
	-H "PRIVATE-TOKEN: ${HUMAN_TOKEN}"; then
	log "human token never saw project ${PRIMARY_ID} within ${AUTHORIZATION_TIMEOUT_SECONDS}s; last status ${WAIT_STATUS}"
	exit 1
fi
log "human token authorized after ${WAITED_SECONDS}s"

# The protected-target merge refusal needs a branch protected at
# Maintainer level; the published Developer-level token cannot merge into
# it, and cannot delete it either, so no test ever removes it.
log "creating and protecting branch protected-base"
create_branch "protected-base" "$DEFAULT_BRANCH"
jq -nc '{name: "protected-base", push_access_level: 40, merge_access_level: 40}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/protected_branches" "$BOOTSTRAP_TOKEN" >/dev/null

# The forge merge request: mergeable, reviewed, approved, CI on its head,
# a plain note from each identity, one inline discussion note, and a
# two-entry label-event journal.
log "seeding the forge merge request fixture"
create_branch "forge-fixture" "$DEFAULT_BRANCH"
FORGE_SHA=$(commit_file "forge-fixture" "forge-sentinel.txt" \
	"$(printf 'sentinel line 1\nsentinel line 2\nsentinel line 3\nsentinel line 4\nsentinel line 5\n')" \
	"Add forge sentinel file")
post_status "$FORGE_SHA" "success" "ci/build"
post_status "$FORGE_SHA" "failed" "ci/test" "$SEEDED_FAILING_TARGET_URL" "The test job failed."

MR_IID=$(open_mr "forge-fixture" "$DEFAULT_BRANCH" "Forge fixture merge request" "$PUBLISHED_TOKEN")

log "waiting up to 60s for the forge merge request diff version to prepare"
if ! wait_for_json '.[0].head_commit_sha' "$FORGE_SHA" \
	"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/versions" 60 2; then
	exit 1
fi

jq -nc '{body: "Human note on the forge merge request."}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/notes" "$HUMAN_TOKEN" >/dev/null
jq -nc '{body: "Bot note on the forge merge request."}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/notes" "$BOT_TOKEN" >/dev/null

version_response=$(curl -sS --max-time 30 -H "PRIVATE-TOKEN: ${PUBLISHED_TOKEN}" \
	"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/versions")
version_base_sha=$(printf '%s' "$version_response" | jq -er '.[0].base_commit_sha')
version_head_sha=$(printf '%s' "$version_response" | jq -er '.[0].head_commit_sha')
version_start_sha=$(printf '%s' "$version_response" | jq -er '.[0].start_commit_sha')

jq -nc --arg base "$version_base_sha" --arg head "$version_head_sha" --arg start "$version_start_sha" \
	'{body: "Bot inline note on the sentinel file.",
	  position: {base_sha: $base, head_sha: $head, start_sha: $start,
	             new_path: "forge-sentinel.txt", position_type: "text", new_line: 1}}' |
	gitlab_call POST "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/discussions" "$BOT_TOKEN" >/dev/null

jq -nc --argjson id "$HUMAN_ID" '{reviewer_ids: [$id]}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}" "$PUBLISHED_TOKEN" >/dev/null
# The approval MUST precede the mergeability gate in await_fixture_settled
# below: approving moves detailed_merge_status back to "checking", so
# gating first would make "mergeable" the pre-approval value rather than
# the settled one.
gitlab_call POST "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}/approve" "$HUMAN_TOKEN" </dev/null >/dev/null

jq -nc --arg label "$SEEDED_JOURNAL_LABEL" '{add_labels: $label}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}" "$PUBLISHED_TOKEN" >/dev/null
jq -nc --arg label "$SEEDED_JOURNAL_LABEL" '{remove_labels: $label}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/merge_requests/${MR_IID}" "$PUBLISHED_TOKEN" >/dev/null

# The draft merge request: blocked mergeability, review required, no
# pipeline on its head.
log "seeding the draft merge request fixture"
create_branch "forge-draft" "$DEFAULT_BRANCH"
commit_file "forge-draft" "forge-draft.txt" "draft fixture content" "Add forge draft file" >/dev/null
DRAFT_IID=$(open_mr "forge-draft" "$DEFAULT_BRANCH" "Draft: forge draft merge request" "$PUBLISHED_TOKEN")
jq -nc --argjson id "$HUMAN_ID" '{reviewer_ids: [$id]}' |
	gitlab_call PUT "/projects/${PRIMARY_ID}/merge_requests/${DRAFT_IID}" "$PUBLISHED_TOKEN" >/dev/null

# The conflicting merge request: the same file diverges on each side, no
# reviewer, no approval.
log "seeding the conflicting merge request fixture"
create_branch "conflict-base" "$DEFAULT_BRANCH"
create_branch "forge-conflict" "conflict-base"
commit_file "forge-conflict" "conflict-sentinel.txt" "conflict side A content" "Add conflict side A" >/dev/null
commit_file "conflict-base" "conflict-sentinel.txt" "conflict side B content" "Add conflict side B" >/dev/null
CONFLICT_IID=$(open_mr "forge-conflict" "conflict-base" "Forge conflict merge request" "$PUBLISHED_TOKEN")

# The label-probe merge request: the only merge request any test writes
# labels to, so no other fixture's mergeability or review state can be
# perturbed by a label-driven recompute.
log "seeding the label-probe merge request fixture"
create_branch "label-probe" "$DEFAULT_BRANCH"
commit_file "label-probe" "label-probe.txt" "label probe fixture content" "Add label probe file" >/dev/null
LABEL_IID=$(open_mr "label-probe" "$DEFAULT_BRANCH" "Forge label-probe merge request" "$PUBLISHED_TOKEN")

# The pagination-probe commit: 103 statuses on a commit no merge request
# heads, crossing the CI provider's per_page=100 boundary.
log "seeding the pagination-probe commit and its 103 statuses"
create_branch "status-probe" "$DEFAULT_BRANCH"
PROBE_SHA=$(commit_file "status-probe" "status-probe.txt" "status pagination probe commit" "Add status probe file")
i=1
while [ "$i" -le 101 ]; do
	post_status "$PROBE_SHA" "success" "ci/probe-${i}"
	i=$((i + 1))
done
post_status "$PROBE_SHA" "failed" "ci/probe-fail"
post_status "$PROBE_SHA" "running" "ci/probe-running"

# await_fixture_settled gates every asynchronously computed merge-request
# field this script publishes a coordinate for, so no test ever reads an
# unsettled platform value. Each gate fails the script rather than
# warning: publishing an unsettled fixture would turn a provisioning
# fault into a test failure attributed to the adapter.
await_fixture_settled() {
	log "waiting up to 60s for the forge merge request to settle mergeable"
	if ! wait_for_json '.detailed_merge_status' "mergeable" \
		"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${MR_IID}" 60 2; then
		exit 1
	fi
	log "waiting up to 60s for the forge merge request head pipeline to settle failed"
	if ! wait_for_json '.head_pipeline.status' "failed" \
		"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${MR_IID}" 60 2; then
		exit 1
	fi
	log "waiting up to 60s for the draft merge request to settle draft_status"
	if ! wait_for_json '.detailed_merge_status' "draft_status" \
		"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${DRAFT_IID}" 60 2; then
		exit 1
	fi
	log "waiting up to 60s for the conflicting merge request to settle conflict"
	if ! wait_for_json '.detailed_merge_status' "conflict" \
		"${ENDPOINT}/api/v4/projects/${PRIMARY_ID}/merge_requests/${CONFLICT_IID}" 60 2; then
		exit 1
	fi
}
await_fixture_settled

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
emit SORTIE_GITLAB_MR_IID "$MR_IID"
emit SORTIE_GITLAB_LABEL_MR_IID "$LABEL_IID"
emit SORTIE_GITLAB_DRAFT_MR_IID "$DRAFT_IID"
emit SORTIE_GITLAB_CONFLICT_MR_IID "$CONFLICT_IID"
emit SORTIE_GITLAB_BOT_USERNAME "$BOT_LOGIN"
emit SORTIE_GITLAB_HUMAN_USERNAME "$HUMAN_USERNAME"
emit SORTIE_GITLAB_MANY_STATUS_SHA "$PROBE_SHA"
emit SORTIE_GITLAB_PROTECTED_BRANCH "protected-base"
emit GITLAB_IMAGE "$IMAGE"
