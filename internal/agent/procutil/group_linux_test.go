//go:build linux

package procutil

import (
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func awaitUnreapedExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if isReaperTestZombie(pid) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d did not become an unreaped zombie within 5s", pid)
}

func TestObserveLeaderExitBlocksWhileRunning(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{Hang: true})
	done := make(chan error, 1)
	go func() { done <- observeLeaderExit(cmd.Process.Pid) }()

	select {
	case err := <-done:
		t.Fatalf("observeLeaderExit(%d) = %v while the direct child still runs, want it blocked", cmd.Process.Pid, err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("cmd.Process.Kill() = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("observeLeaderExit(%d) after the exit = %v, want nil", cmd.Process.Pid, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("observeLeaderExit(%d) did not return within 5s of the exit", cmd.Process.Pid)
	}
	if err := cmd.Wait(); !WasSignaled(err) {
		t.Errorf("cmd.Wait() = %v, want the exit status of a child that stayed unreaped", err)
	}
}

func TestObserveLeaderExitReturnsForExitAfterAndBeforeCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		exitBefore   bool
		wantExitCode int
	}{
		{name: "exit after the call began", exitBefore: false, wantExitCode: -1},
		{name: "exit before the call", exitBefore: true, wantExitCode: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := agenttest.Output{Hang: !tt.exitBefore}
			if tt.exitBefore {
				out.ExitCode = tt.wantExitCode
			}
			cmd := startLeader(t, out)
			pid := cmd.Process.Pid
			done := make(chan error, 1)
			if tt.exitBefore {
				awaitUnreapedExit(t, pid)
				go func() { done <- observeLeaderExit(pid) }()
			} else {
				go func() { done <- observeLeaderExit(pid) }()
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("cmd.Process.Kill() = %v", err)
				}
			}

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("observeLeaderExit(%d) = %v, want nil", pid, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("observeLeaderExit(%d) did not return within 5s", pid)
			}

			err := cmd.Wait()
			if tt.wantExitCode < 0 {
				if !WasSignaled(err) {
					t.Errorf("cmd.Wait() = %v, want the signal status of a child that stayed unreaped", err)
				}
				return
			}
			if got := ExtractExitCode(err); got != tt.wantExitCode {
				t.Errorf("exit code from cmd.Wait() = %d, want %d (the child must stay unreaped for Wait)", got, tt.wantExitCode)
			}
		})
	}
}

func TestHasLiveMemberZombieOnly(t *testing.T) {
	t.Parallel()

	cmd := startLeader(t, agenttest.Output{})
	pid := cmd.Process.Pid
	awaitUnreapedExit(t, pid)

	for _, leader := range []int{pid, 0} {
		live, err := hasLiveMember(pid, leader)
		if err != nil {
			t.Fatalf("hasLiveMember(%d, %d) = %v", pid, leader, err)
		}
		if live {
			t.Errorf("hasLiveMember(%d, %d) = true for a group holding only the exited, unreaped direct child, want false", pid, leader)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("cmd.Wait() = %v, want nil", err)
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

	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill(-%d, SIGKILL) = %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for live && time.Now().Before(deadline) {
		if live, err = hasLiveMember(pid, pid); err != nil {
			t.Fatalf("hasLiveMember(%d, %d) = %v", pid, pid, err)
		}
		if live {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if live {
		t.Errorf("hasLiveMember(%d, %d) = true 5s after the group was killed, want false", pid, pid)
	}
}

func TestParseProcStat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stat      string
		wantState byte
		wantPgrp  int
		wantErr   bool
	}{
		{
			name:      "running process",
			stat:      "1234 (sleep) S 1 1234 1234 0 -1 4194304 100 0 0 0 0 0",
			wantState: 'S',
			wantPgrp:  1234,
		},
		{
			name:      "zombie",
			stat:      "77 (sh) Z 1 99 99 0 -1 4194572 0 0 0 0 0 0",
			wantState: 'Z',
			wantPgrp:  99,
		},
		{
			name:      "command name with spaces",
			stat:      "42 (my cmd) S 1 55 55 0 -1 0",
			wantState: 'S',
			wantPgrp:  55,
		},
		{
			name:      "command name containing a closing parenthesis",
			stat:      "42 (a) b (c)) R 1 77 77 0 -1 0",
			wantState: 'R',
			wantPgrp:  77,
		},
		{name: "empty line", stat: "", wantErr: true},
		{name: "no command name terminator", stat: "42 sleep S 1 2 3", wantErr: true},
		{name: "truncated after the state", stat: "42 (a) S 1", wantErr: true},
		{name: "non-numeric process group", stat: "42 (a) S 1 x 3", wantErr: true},
		{name: "state longer than one character", stat: "42 (a) SS 1 2 3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, pgrp, err := parseProcStat([]byte(tt.stat))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseProcStat(%q) = %q, %d, nil, want an error", tt.stat, state, pgrp)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcStat(%q) error = %v, want nil", tt.stat, err)
			}
			if state != tt.wantState || pgrp != tt.wantPgrp {
				t.Errorf("parseProcStat(%q) = %q, %d, want %q, %d", tt.stat, state, pgrp, tt.wantState, tt.wantPgrp)
			}
		})
	}
}
