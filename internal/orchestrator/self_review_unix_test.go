//go:build unix

package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TestRunVerification_TimeoutSignalsDescendantGroup verifies that a
// verification command that overruns its timeout is torn down through
// its process group.
//
// A verification command always runs as "sh -c", so the work an operator
// configures - a build, a test suite - is a grandchild of the shell the
// orchestrator starts. Killing the shell alone leaves that work running
// against the workspace after the run has moved on.
//
// The evidence is a marker the grandchild writes from its own signal
// handler: it is reachable only through the group, and it can only run a
// handler if the signal was catchable. The shell waits for the
// grandchild inside its own handler, so cmd.Wait cannot return, and the
// post-wait group reap cannot run, until the marker is on disk.
func TestRunVerification_TimeoutSignalsDescendantGroup(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	marker := filepath.Join(wsPath, "descendant.terminated")
	pidFile := filepath.Join(wsPath, "descendant.pid")
	descendant := filepath.Join(wsPath, "descendant.sh")

	script := fmt.Sprintf(
		"trap 'printf terminated > %s; exit 0' TERM\n"+
			"printf '%%s\\n' \"$$\" > %s\n"+
			"while :; do sleep 1; done\n",
		marker, pidFile,
	)
	if err := os.WriteFile(descendant, []byte(script), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v, want nil", descendant, err)
	}

	command := fmt.Sprintf(`/bin/sh %s & D=$!; trap 'wait "$D"; exit 0' TERM; wait "$D"`, descendant)

	done := make(chan domain.VerificationResult, 1)
	go func() {
		done <- runSingleVerification(context.Background(), command, wsPath, 1000, discardLogger(), &domain.NoopMetrics{})
	}()

	descendantPID := pollVerificationPID(t, pidFile, 5*time.Second)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })

	var result domain.VerificationResult
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runSingleVerification did not return within 30s, want a timed-out result")
	}

	if !result.TimedOut {
		t.Errorf("runSingleVerification().TimedOut = false, want true")
	}
	if !pollVerificationFile(marker, 5*time.Second) {
		t.Errorf("runSingleVerification() left %q absent, want the descendant to have caught a graceful signal", marker)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil {
		t.Errorf("runSingleVerification() left descendant %d alive, want gone", descendantPID)
	}
}

// pollVerificationPID polls path until it holds a positive integer.
func pollVerificationPID(t *testing.T, path string, timeout time.Duration) int {
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
	t.Fatalf("pollVerificationPID(%q) = no PID after %v, want a PID", path, timeout)
	return 0
}

// pollVerificationFile reports whether path appears before the timeout.
func pollVerificationFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
