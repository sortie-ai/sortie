//go:build darwin

package procutil

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// The exit is observed through the test's own kqueue, registered before the
// kill so the notification cannot be missed, and leaves the child unreaped.
func awaitExitUnreaped(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	kq, err := unix.Kqueue()
	if err != nil {
		t.Fatalf("unix.Kqueue() = %v", err)
	}
	defer func() { _ = unix.Close(kq) }()

	register := []unix.Kevent_t{{
		Ident:  uint64(cmd.Process.Pid), //nolint:gosec // G115: a process identifier is never negative
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}}
	if _, err := unix.Kevent(kq, register, nil, nil); err != nil {
		t.Fatalf("registering for the exit of process %d = %v", cmd.Process.Pid, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("cmd.Process.Kill() = %v", err)
	}

	events := make([]unix.Kevent_t, 1)
	timeout := unix.NsecToTimespec((10 * time.Second).Nanoseconds())
	for {
		n, err := unix.Kevent(kq, nil, events, &timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || n != 1 {
			t.Fatalf("waiting for the exit of process %d = %d events, %v, want 1", cmd.Process.Pid, n, err)
		}
		return
	}
}

func awaitObservation(t *testing.T, done <-chan error, pid int, within time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("observeLeaderExit(%d) = %v, want nil", pid, err)
		}
	case <-time.After(within):
		t.Fatalf("observeLeaderExit(%d) did not return within %v", pid, within)
	}
}

func TestObserveLeaderExitDoesNotReturnWhileRunning(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{Hang: true})
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- observeLeaderExit(pid) }()

	select {
	case err := <-done:
		t.Fatalf("observeLeaderExit(%d) = %v while the direct child still runs, want it blocked past one probe interval", pid, err)
	case <-time.After(darwinExitProbeInterval + 500*time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("cmd.Process.Kill() = %v", err)
	}
	awaitObservation(t, done, pid, 10*time.Second)
	if err := cmd.Wait(); !WasSignaled(err) {
		t.Errorf("cmd.Wait() = %v, want the signal status of a child that stayed unreaped", err)
	}
}

func TestObserveLeaderExitReturnsForExitAfterAndBeforeCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		exitBefore bool
	}{
		{name: "exit after the call began", exitBefore: false},
		{name: "exit before the call", exitBefore: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := startLeader(t, agenttest.Output{Hang: true})
			pid := cmd.Process.Pid
			done := make(chan error, 1)
			if tt.exitBefore {
				awaitExitUnreaped(t, cmd)
				go func() { done <- observeLeaderExit(pid) }()
			} else {
				go func() { done <- observeLeaderExit(pid) }()
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("cmd.Process.Kill() = %v", err)
				}
			}

			awaitObservation(t, done, pid, 10*time.Second)

			if err := cmd.Wait(); !WasSignaled(err) {
				t.Errorf("cmd.Wait() = %v, want the signal status of a child that stayed unreaped", err)
			}
		})
	}
}

func TestZombieProbeReadsExitedAndRunning(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{Hang: true})
	pid := cmd.Process.Pid

	running, err := isZombie(pid)
	if err != nil {
		t.Fatalf("isZombie(%d) while running = %v", pid, err)
	}
	if running {
		t.Errorf("isZombie(%d) = true for a running direct child, want false", pid)
	}

	awaitExitUnreaped(t, cmd)

	exited, err := isZombie(pid)
	if err != nil {
		t.Fatalf("isZombie(%d) after the exit = %v", pid, err)
	}
	if !exited {
		t.Errorf("isZombie(%d) = false for an exited, unreaped direct child, want true", pid)
	}
}

func TestHasLiveMemberZombieOnly(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{Hang: true})
	pid := cmd.Process.Pid
	awaitExitUnreaped(t, cmd)

	live, err := hasLiveMember(pid, pid)
	if err != nil {
		t.Fatalf("hasLiveMember(%d, %d) = %v", pid, pid, err)
	}
	if live {
		t.Errorf("hasLiveMember(%d, %d) = true for a group holding only the exited, unreaped direct child, want false", pid, pid)
	}
}

func TestHasLiveMemberOneOtherMember(t *testing.T) {
	t.Parallel()

	cmd := startLeaderWithLiveMember(t)
	pid := cmd.Process.Pid

	live, err := hasLiveMember(pid, pid)
	if err != nil {
		t.Fatalf("hasLiveMember(%d, %d) = %v", pid, pid, err)
	}
	if !live {
		t.Errorf("hasLiveMember(%d, %d) = false while a descendant runs in the group, want true", pid, pid)
	}
}

func TestKillGroupZeroSignalEPERMForZombieOnlyGroup(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{Hang: true})
	pid := cmd.Process.Pid
	awaitExitUnreaped(t, cmd)

	err := syscall.Kill(-pid, 0)

	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("syscall.Kill(-%d, 0) = %v for a group holding only the exited, unreaped direct child, want EPERM", pid, err)
	}
}
