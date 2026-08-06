# shellcheck shell=sh
# Shared helpers for the containerized tracker provisioning scripts. Sourced,
# never executed. The caller sets CONTAINER_NAME before anything that touches
# the container.
#
# POSIX sh, so no pipefail: a pipeline reports only its last command. Callers
# must capture a helper's output into a variable and check that status before
# piping it onward, or a failed call is masked by a jq that exits 0 on empty
# input.

# A literal newline. $'\n' is a bashism.
NEWLINE='
'

# Diagnostics go to stderr so stdout carries only the coordinate assignments.
log() {
	printf '%s\n' "$*" >&2
}

require_tools() {
	for _rt_tool in "$@"; do
		if ! command -v "$_rt_tool" >/dev/null 2>&1; then
			log "required command not found: ${_rt_tool}"
			return 1
		fi
	done
}

# Surface container logs on a non-zero exit. The container is left running: CI
# discards the runner, and the next local run clears it.
dump_logs_on_error() {
	_dl_code=$?
	if [ "$_dl_code" -ne 0 ] && docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
		log "provisioning failed (exit ${_dl_code}); recent container logs follow:"
		docker logs --tail 200 "$CONTAINER_NAME" >&2 || true
	fi
}

# Remove a prior container of the fixed name so a repeat local run reclaims the
# host port instead of failing to bind it.
reclaim_container() {
	docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
}

container_running() {
	[ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null || true)" = "true" ]
}

# wait_for_http URL ACCEPTED_STATUSES TIMEOUT INTERVAL [curl args...]
#
# Polls until the URL answers with one of the space-separated accepted statuses.
# Sets WAITED_SECONDS and WAIT_STATUS. A container that exits mid-wait fails fast
# instead of burning the whole budget.
wait_for_http() {
	_wh_url=$1 _wh_accept=$2 _wh_timeout=$3 _wh_interval=$4
	shift 4
	_wh_start=$(date +%s)
	while :; do
		if ! container_running; then
			log "container exited before readiness; a boot-configuration failure is the usual cause"
			return 1
		fi

		WAIT_STATUS=$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$@" "$_wh_url" 2>/dev/null || true)
		case " ${_wh_accept} " in
		*" ${WAIT_STATUS} "*)
			WAITED_SECONDS=$(($(date +%s) - _wh_start))
			return 0
			;;
		esac

		WAITED_SECONDS=$(($(date +%s) - _wh_start))
		[ "$WAITED_SECONDS" -lt "$_wh_timeout" ] || return 1
		sleep "$_wh_interval"
	done
}

# api_call METHOD URL [curl args...]
#
# The JSON body comes from stdin. A non-2xx status logs the response and fails.
api_call() {
	_ac_method=$1 _ac_url=$2
	shift 2
	_ac_body=$(cat)
	_ac_response=$(curl -sS --max-time 30 -w "${NEWLINE}%{http_code}" \
		-H 'Content-Type: application/json' \
		"$@" \
		-X "$_ac_method" "$_ac_url" \
		-d "$_ac_body")
	_ac_status=${_ac_response##*"$NEWLINE"}
	_ac_response=${_ac_response%"$NEWLINE"*}
	case "$_ac_status" in
	2*)
		printf '%s' "$_ac_response"
		;;
	*)
		log "api ${_ac_method} ${_ac_url} returned HTTP ${_ac_status}: ${_ac_response}"
		return 1
		;;
	esac
}

# $GITHUB_ENV takes a bare KEY=VALUE; stdout takes an export statement, so an
# eval'd coordinate is inherited by the test process rather than merely set.
emit() {
	if [ -n "${GITHUB_ENV:-}" ] && [ -w "${GITHUB_ENV}" ]; then
		printf '%s=%s\n' "$1" "$2" >>"$GITHUB_ENV"
	fi
	printf "export %s='%s'\n" "$1" "$2"
}

# Mask before the secret is echoed anywhere. Only under Actions: locally this
# would pollute eval's stdout.
mask_secret() {
	if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
		printf '::add-mask::%s\n' "$1"
	fi
}
