//go:build linux

package procutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// observeLeaderExit blocks until pid, a child of this process, has
// exited. WNOWAIT leaves it unreaped, so its identifier stays assigned
// to it.
func observeLeaderExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

// directChildRunning reports whether the direct child of cmd has not
// exited. WNOWAIT leaves an exited child unreaped, and a child that is
// still running leaves the signal number of the returned information
// zero. A probe that fails counts as running.
func directChildRunning(cmd *exec.Cmd) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err != nil || info.Signo == 0
	}
}

// hasLiveMember reports whether a process other than leader that is
// neither a zombie nor dead belongs to process group pgid. A process
// that exits between the listing and its lookup is skipped.
//
// The group number is compared through getpgid, which costs one system
// call per process, and only the few processes that match have their
// stat read for their state: reading every stat file made each reap
// cost far more than the launch it ended.
func hasLiveMember(pgid, leader int) (bool, error) {
	dir, err := os.Open("/proc")
	if err != nil {
		return false, fmt.Errorf("listing /proc: %w", err)
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return false, fmt.Errorf("listing /proc: %w", err)
	}
	for _, name := range names {
		pid, convErr := strconv.Atoi(name)
		if convErr != nil || pid == leader {
			continue
		}
		group, groupErr := unix.Getpgid(pid)
		if errors.Is(groupErr, unix.ESRCH) {
			continue
		}
		if groupErr != nil {
			return false, fmt.Errorf("reading process group of process %d: %w", pid, groupErr)
		}
		if group != pgid {
			continue
		}
		stat, readErr := os.ReadFile("/proc/" + name + "/stat") //nolint:gosec // G304: name is a numeric entry of /proc
		if readErr != nil {
			continue
		}
		state, _, parseErr := parseProcStat(stat)
		if parseErr != nil {
			return false, fmt.Errorf("reading /proc/%d/stat: %w", pid, parseErr)
		}
		if !isDeadState(state) {
			return true, nil
		}
	}
	return false, nil
}

func isDeadState(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}

// parseProcStat reads the state and the process-group identifier from a
// /proc/<pid>/stat line. It starts after the last ')' because the
// command name before it is arbitrary text that may contain one.
func parseProcStat(stat []byte) (state byte, pgrp int, err error) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, errors.New("no command name terminator in stat line")
	}
	// After the command name come the state, the parent identifier and
	// the process-group identifier.
	fields := bytes.Fields(stat[end+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, errors.New("stat line has no state and process group fields")
	}
	pgrp, err = strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, fmt.Errorf("process group field: %w", err)
	}
	return fields[0][0], pgrp, nil
}
