//go:build unix

package procutil

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// writeShellScript writes content to a file under dir and returns its
// path. The file is never executed directly - callers pass it to
// /bin/sh as an argument - so it needs no executable bit and cannot hit
// the ETXTBSY race that direct execution of a just-written file has.
func writeShellScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v, want nil", path, err)
	}
	return path
}

// pollForPID polls path until it holds a positive integer, returning it.
func pollForPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollForPID(%q) = no PID after %v, want a PID", path, timeout)
	return 0
}

// pollForFile reports whether path appears before the timeout expires.
func pollForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestSetGroupCancel_CancelReachesDescendant verifies that cancelling the
// context of a command prepared by SetGroupCancel delivers a catchable
// termination signal to the whole process group, not just to the direct
// child.
//
// The evidence is a marker written by a grandchild from inside its own
// signal handler. A grandchild is reachable only through the group, and
// it can only run a handler if the signal was catchable, so the marker
// distinguishes a group-wide graceful signal from os/exec's default of
// force-killing the direct child alone. The direct child waits for the
// grandchild before exiting, so cmd.Wait cannot return until the marker
// is on disk.
func TestSetGroupCancel_CancelReachesDescendant(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "descendant.terminated")
	pidFile := filepath.Join(dir, "descendant.pid")

	descendant := writeShellScript(t, dir, "descendant.sh", fmt.Sprintf(
		"MARKER='%s'\n"+
			"PID_FILE='%s'\n"+
			"trap 'printf terminated > \"$MARKER\"; exit 0' TERM\n"+
			"printf '%%s\\n' \"$$\" > \"$PID_FILE\"\n"+
			"while :; do sleep 1; done\n",
		marker, pidFile,
	))
	leader := writeShellScript(t, dir, "leader.sh", fmt.Sprintf(
		"DESCENDANT='%s'\n"+
			"/bin/sh \"$DESCENDANT\" &\n"+
			"DESCENDANT_PID=$!\n"+
			"trap 'wait \"$DESCENDANT_PID\"; exit 0' TERM\n"+
			"wait \"$DESCENDANT_PID\"\n",
		descendant,
	))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", leader)
	SetGroupCancel(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v, want nil", err)
	}
	leaderPID := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) })

	descendantPID := pollForPID(t, pidFile, 5*time.Second)

	cancel()
	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	if !pollForFile(marker, 5*time.Second) {
		t.Errorf("SetGroupCancel(): cancelling left %q absent, want the descendant to have caught a graceful signal", marker)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil {
		t.Errorf("SetGroupCancel(): descendant %d still alive after cancellation, want gone", descendantPID)
	}
}
