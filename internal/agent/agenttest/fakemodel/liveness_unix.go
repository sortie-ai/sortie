//go:build unix

package fakemodel

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// livenessChecked reports that remainingRuntime probes the process table.
const livenessChecked = true

// remainingRuntime names process pid while it still answers signal 0, and is
// empty once it does not. A zombie answers, so it reads as remaining. The
// caller guarantees pid is above 1: signal 0 to process 0 or to every process
// would answer for processes that are not the runtime's.
func remainingRuntime(pid int) string {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return ""
	}
	return fmt.Sprintf("process %d", pid)
}

// heldLaunches returns the Released method of each held launch record whose
// direct child is among pids, in pids order.
func heldLaunches(pids []int) []func() (released bool, cleanupErr error) {
	var launches []func() (released bool, cleanupErr error)
	for _, pid := range pids {
		if group := procutil.HeldGroup(pid); group != nil {
			launches = append(launches, group.Released)
		}
	}
	return launches
}

// killHeldGroups sends SIGKILL to the process group of each held launch record
// whose direct child is among pids. The signal is raw: procutil.Group.Signal
// and Kill record the exit as Sortie's own stop, which would make a crash
// indistinguishable from a requested shutdown.
func killHeldGroups(pids []int) error {
	var errs []error
	for _, pid := range pids {
		group := procutil.HeldGroup(pid)
		if group == nil {
			continue
		}
		if err := syscall.Kill(-group.PID(), syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, fmt.Errorf("kill process group %d: %w", group.PID(), err))
		}
	}
	return errors.Join(errs...)
}
