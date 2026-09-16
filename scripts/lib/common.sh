# shellcheck shell=sh
# Shared POSIX shell helpers. Sourced, never executed.

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

require_env() {
	for _re_name in "$@"; do
		case "$_re_name" in
		'' | *[!A-Za-z0-9_]*)
			log "invalid environment variable name: ${_re_name}"
			return 1
			;;
		esac
		eval "_re_value=\${${_re_name}:-}"
		if [ -z "$_re_value" ]; then
			log "required environment variable is empty: ${_re_name}"
			return 1
		fi
	done
}
