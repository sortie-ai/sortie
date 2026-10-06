//go:build darwin

package procutil

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// darwinExitProbeInterval bounds each wait for the exit notification.
// A notification posted before the registration took effect is never
// delivered, so each timeout reads the direct child's state instead.
const darwinExitProbeInterval = time.Second

// procStatZombie is SZOMB from <sys/proc.h>; golang.org/x/sys defines
// no constant for it.
const procStatZombie = 5

// observeLeaderExit blocks until pid, a child of this process, has
// exited, and leaves it unreaped so its identifier stays assigned to it.
func observeLeaderExit(pid int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return fmt.Errorf("creating kqueue: %w", err)
	}
	defer func() { _ = unix.Close(kq) }()
	unix.CloseOnExec(kq)

	changes := []unix.Kevent_t{{
		Ident:  uint64(pid), //nolint:gosec // G115: a process identifier is never negative
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	events := make([]unix.Kevent_t, 1)
	timeout := unix.NsecToTimespec(darwinExitProbeInterval.Nanoseconds())
	for {
		n, waitErr := unix.Kevent(kq, changes, events, &timeout)
		if errors.Is(waitErr, unix.EINTR) {
			continue
		}
		if errors.Is(waitErr, unix.ESRCH) {
			return nil
		}
		if waitErr != nil {
			return fmt.Errorf("waiting for exit notification: %w", waitErr)
		}
		changes = nil

		if n > 0 {
			if events[0].Flags&unix.EV_ERROR == 0 {
				return nil
			}
			regErr := unix.Errno(events[0].Data) //nolint:gosec // G115: EV_ERROR carries a non-negative errno in Data
			if errors.Is(regErr, unix.ESRCH) {
				return nil
			}
			return fmt.Errorf("registering for exit notification: %w", regErr)
		}

		exited, probeErr := isZombie(pid)
		if probeErr != nil {
			return probeErr
		}
		if exited {
			return nil
		}
	}
}

func isZombie(pid int) (bool, error) {
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, fmt.Errorf("reading state of process %d: %w", pid, err)
	}
	return kinfo.Proc.P_stat == procStatZombie, nil
}

// hasLiveMember reports whether a process other than leader that is not
// a zombie belongs to process group pgid.
func hasLiveMember(pgid, leader int) (bool, error) {
	members, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("reading members of process group %d: %w", pgid, err)
	}
	for i := range members {
		proc := &members[i].Proc
		if int(proc.P_pid) != leader && proc.P_stat != procStatZombie {
			return true, nil
		}
	}
	return false, nil
}
