# shellcheck shell=sh
# sshd sends no signal to a command it runs without a terminal when the
# connection ends, so the agent is stopped once the shell's parent, the sshd
# session process, is gone.
sortie_start_agent() {
	exec 3<&0
	"$@" <&3 3<&- &
	agent_pid=$!
	exec 3<&-
}

# The lock is held through descriptor 4, which the agent and everything it
# starts inherit and the watchdog closes, so it is free once the whole agent
# tree has exited. The waiter that takes it afterwards removes the file; the
# supervisor removes it when the agent ends without having been stopped.
sortie_hold_barrier() {
	command -v flock >/dev/null 2>&1 || return 0
	( umask 077; : >"$1" ) 2>/dev/null || return 0
	exec 4>"$1"
	flock -x 4
	sortie_barrier=$1
}

sortie_parent_alive() {
	kill -0 "$PPID" 2>/dev/null || [ -e "/proc/$PPID" ] || ps -p "$PPID" >/dev/null 2>&1
}

sortie_stop_session_group() {
	grace=$1
	trap '' TERM
	kill -TERM 0
	while [ "$grace" -gt 0 ] && kill -0 "$agent_pid" 2>/dev/null; do
		sleep 1
		grace=$((grace - 1))
	done
	kill -KILL 0
}

sortie_watch_parent() {
	while sortie_parent_alive; do
		sleep 1
	done
	sortie_stop_session_group "$1"
}

# An agent that exits by itself can leave children running, such as a
# background fetch it started. sshd makes this shell the leader of the
# session's process group, so the rest of the group is the agent's leftovers;
# a shell that leads no group shares it with processes it must not signal.
sortie_end_leftovers() {
	pgid=$(ps -o pgid= -p "$$" 2>/dev/null) || return 0
	[ "$((pgid))" -eq "$$" ] || return 0
	trap '' TERM
	kill -TERM 0 2>/dev/null
}

sortie_supervise() {
	sortie_watch_parent "$1" </dev/null >/dev/null 2>&1 4>&- &
	watchdog=$!
	wait "$agent_pid"
	status=$?
	kill "$watchdog" 2>/dev/null
	sortie_end_leftovers
	[ -z "${sortie_barrier-}" ] || rm -f "$sortie_barrier"
	return "$status"
}
