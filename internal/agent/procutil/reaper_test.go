//go:build unix

package procutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// TestReaper_DoneAndErr asserts that Done closes only once the
// subprocess has exited and that Err, read after that close, reports
// the same outcome cmd.Wait itself would have returned.
func TestReaper_DoneAndErr(t *testing.T) {
	t.Parallel()

	t.Run("non-zero exit", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("/bin/sh", "-c", "exit 7")
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd)
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close within 3s")
		}

		var exitErr *exec.ExitError
		if !errors.As(r.Err(), &exitErr) {
			t.Fatalf("Err() = %v (%T), want *exec.ExitError", r.Err(), r.Err())
		}
		if got := exitErr.ExitCode(); got != 7 {
			t.Errorf("Err().(*exec.ExitError).ExitCode() = %d, want 7", got)
		}
	})

	t.Run("clean exit", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("/bin/true")
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd)
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close within 3s")
		}
		if err := r.Err(); err != nil {
			t.Errorf("Err() = %v, want nil", err)
		}
	})

	t.Run("Done stays open while the subprocess is still running", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("/bin/sh", "-c", "sleep 5")
		SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		r := StartReaper(cmd)
		select {
		case <-r.Done():
			t.Fatal("Done() closed before the subprocess exited")
		case <-time.After(200 * time.Millisecond):
		}

		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("cmd.Process.Kill() = %v", err)
		}
		select {
		case <-r.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Done() did not close after the subprocess was killed")
		}
	})
}

// isReaperTestZombie reports whether pid is a zombie by reading
// /proc/<pid>/stat. Returns false if the file cannot be read.
func isReaperTestZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// assertReaperTestProcessDead polls until pid is gone or a zombie, or
// fails t after timeout.
func assertReaperTestProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if isReaperTestZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d still alive after %v, want gone", pid, timeout)
}

// TestReaper_KillsProcessGroupBeforeDoneCloses asserts the ordering
// StartReaper's doc comment states: by the time Done closes, the
// subprocess's process group has already been killed, including a
// descendant that stayed in the group.
func TestReaper_KillsProcessGroupBeforeDoneCloses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := agenttest.WriteScript(t, dir, "reaper-leader", fmt.Sprintf(
		"sleep 3600 &\nprintf '%%s\\n' \"$!\" > %q\n", pidFile))

	cmd := exec.Command(script)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	childPID := pollReaperTestPIDFile(t, pidFile, 5*time.Second)

	r := StartReaper(cmd)
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close within 5s (the leader exits on its own once it finishes writing the pid file)")
	}

	assertReaperTestProcessDead(t, childPID, 3*time.Second)
}

// pollReaperTestPIDFile polls pidFile until it contains a valid positive
// PID, or fails t after timeout.
func pollReaperTestPIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollReaperTestPIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}
