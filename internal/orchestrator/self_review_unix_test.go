//go:build unix

package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// mustSelfReviewGit runs git args in dir, failing t on error.
func mustSelfReviewGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed git subcommands in a test fixture
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGenerateWorkspaceDiff_HeldDescendantHoldingOutput pins that, with
// a held descendant holding the output of the git diff invocations
// generateWorkspaceDiff makes, the call returns within its timer with
// the diff text.
func TestGenerateWorkspaceDiff_HeldDescendantHoldingOutput(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found on PATH")
	}

	dir := t.TempDir()
	mustSelfReviewGit(t, dir, "init")
	mustSelfReviewGit(t, dir, "config", "user.email", "t@t.test")
	mustSelfReviewGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	mustSelfReviewGit(t, dir, "add", "f.txt")
	mustSelfReviewGit(t, dir, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatalf("modify file: %v", err)
	}

	binDir := t.TempDir()
	fakeGitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\n\"" + realGit + "\" \"$@\"\nsleep 30 &\n"
	if err := os.WriteFile(fakeGitPath, []byte(script), 0o755); err != nil { //nolint:gosec // fixture script under t.TempDir()
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	type outcome struct {
		diff string
		err  error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		diff, _, _, diffErr := generateWorkspaceDiff(context.Background(), dir, 0)
		done <- outcome{diff, diffErr}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("generateWorkspaceDiff() did not return within 20s, want a bounded return despite the held descendant")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("generateWorkspaceDiff() took %v, want within 5s", elapsed)
	}
	if oc.err != nil {
		t.Fatalf("generateWorkspaceDiff() error = %v", oc.err)
	}
	if !strings.Contains(oc.diff, "world") {
		t.Errorf("diff = %q, want it to contain the new line", oc.diff)
	}
}

// TestGenerateWorkspaceDiff_IntentToAddReapsHeldDescendant covers
// generateWorkspaceDiff's git add --intent-to-add call, which runs
// through RunCapture with an empty CaptureParams, so it captures no
// output and TestGenerateWorkspaceDiff_HeldDescendantHoldingOutput
// cannot observe it. What it can assert is that the reap still
// terminates that launch's process group: a held descendant the
// intent-to-add invocation starts is gone once generateWorkspaceDiff
// returns, which the git launch's prior route (a plain cmd.Run with no
// process group) would never guarantee.
func TestGenerateWorkspaceDiff_IntentToAddReapsHeldDescendant(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found on PATH")
	}

	dir := t.TempDir()
	mustSelfReviewGit(t, dir, "init")
	mustSelfReviewGit(t, dir, "config", "user.email", "t@t.test")
	mustSelfReviewGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	mustSelfReviewGit(t, dir, "add", "f.txt")
	mustSelfReviewGit(t, dir, "commit", "-m", "init")

	binDir := t.TempDir()
	pidPath := filepath.Join(binDir, "descendant.pid")
	fakeGitPath := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$2" = "--intent-to-add" ]; then
	sleep 30 &
	echo $! > %q
fi
exec "%s" "$@"
`, pidPath, realGit)
	if err := os.WriteFile(fakeGitPath, []byte(script), 0o755); err != nil { //nolint:gosec // fixture script under t.TempDir()
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	done := make(chan error, 1)
	go func() {
		_, _, _, diffErr := generateWorkspaceDiff(context.Background(), dir, 0)
		done <- diffErr
	}()

	select {
	case diffErr := <-done:
		if diffErr != nil {
			t.Fatalf("generateWorkspaceDiff() error = %v", diffErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("generateWorkspaceDiff() did not return within 20s")
	}

	pid := pollVerificationPID(t, pidPath, 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if killErr := syscall.Kill(pid, 0); killErr != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("intent-to-add descendant %d still answers signal 0, want it gone", pid)
}

// TestRunSingleVerification_HeldDescendantSucceedsAndLogsLeftoverRecord
// pins that a verification command that prints a line, backgrounds a
// held descendant holding standard output, and exits 0 returns within
// 2s with ExitCode 0, an empty ExecutionError, TimedOut false, and the
// printed line in Stdout, the descendant is gone, and exactly one
// LeftoversTerminatedMessage record carrying command is logged.
func TestRunSingleVerification_HeldDescendantSucceedsAndLogsLeftoverRecord(t *testing.T) {
	wsPath := t.TempDir()
	pidFile := filepath.Join(wsPath, "descendant.pid")
	command := fmt.Sprintf(`echo line1; sleep 30 & echo $! > %q; exit 0`, pidFile)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	start := time.Now()
	result := runSingleVerification(context.Background(), command, wsPath, 5000, logger, &domain.NoopMetrics{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("runSingleVerification() took %v, want within 2s", elapsed)
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.ExecutionError != "" {
		t.Errorf("ExecutionError = %q, want empty", result.ExecutionError)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if !strings.Contains(result.Stdout, "line1") {
		t.Errorf("Stdout = %q, want it to contain %q", result.Stdout, "line1")
	}

	out := logBuf.String()
	if count := strings.Count(out, procutil.LeftoversTerminatedMessage); count != 1 {
		t.Errorf("log contains %d occurrences of %q, want exactly 1; log = %q", count, procutil.LeftoversTerminatedMessage, out)
	}
	if !strings.Contains(out, "command=") {
		t.Errorf("log missing a command attribute; log = %q", out)
	}

	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("parse descendant pid from %q: %v", pidData, convErr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if killErr := syscall.Kill(pid, 0); killErr != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}

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
		"MARKER='%s'\n"+
			"PID_FILE='%s'\n"+
			"trap 'printf terminated > \"$MARKER\"; exit 0' TERM\n"+
			"printf '%%s\\n' \"$$\" > \"$PID_FILE\"\n"+
			"while :; do sleep 1; done\n",
		marker, pidFile,
	)
	if err := os.WriteFile(descendant, []byte(script), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v, want nil", descendant, err)
	}

	command := fmt.Sprintf(`DESCENDANT='%s'; /bin/sh "$DESCENDANT" & D=$!; trap 'wait "$D"; exit 0' TERM; wait "$D"`, descendant)

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
