//go:build unix

package procutil

import (
	"math"
	"os/exec"
	"syscall"
	"testing"
)

func TestSetProcessGroup(t *testing.T) {
	t.Parallel()

	t.Run("nil SysProcAttr", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{}
		SetProcessGroup(cmd)

		if cmd.SysProcAttr == nil {
			t.Fatal("SetProcessGroup() SysProcAttr = nil, want non-nil")
		}
		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
	})

	t.Run("existing SysProcAttr fields preserved", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{
			SysProcAttr: &syscall.SysProcAttr{Noctty: true},
		}
		SetProcessGroup(cmd)

		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
		if !cmd.SysProcAttr.Noctty {
			t.Error("SetProcessGroup() Noctty = false, want true (pre-existing field must be preserved)")
		}
	})
}

func TestSignalProcessGroup_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; no such process group can exist.
	err := SignalProcessGroup(math.MaxInt32, syscall.SIGTERM)
	if err != nil {
		t.Errorf("SignalProcessGroup(MaxInt32, SIGTERM) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalGraceful_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; ESRCH is silently swallowed.
	err := SignalGraceful(math.MaxInt32)
	if err != nil {
		t.Errorf("SignalGraceful(MaxInt32) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalProcessGroup_LiveProcess(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "3600")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	if err := SignalProcessGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("SignalProcessGroup(pid, SIGTERM) = %v, want nil", err)
	}

	err := cmd.Wait()
	if !WasSignaled(err) {
		t.Errorf("WasSignaled(cmd.Wait()) = false, want true (process should have been terminated by SIGTERM)")
	}
}
