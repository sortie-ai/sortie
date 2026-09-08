//go:build unix

package probe

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// TestSetProcessGroup mirrors internal/agent/procutil's own coverage of
// the function this one inlines: a nil SysProcAttr is allocated, and a
// pre-existing SysProcAttr field survives alongside Setpgid.
func TestSetProcessGroup(t *testing.T) {
	t.Parallel()

	t.Run("nil SysProcAttr is allocated", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{}
		setProcessGroup(cmd)

		if cmd.SysProcAttr == nil {
			t.Fatal("setProcessGroup() SysProcAttr = nil, want non-nil")
		}
		if !cmd.SysProcAttr.Setpgid {
			t.Error("setProcessGroup() Setpgid = false, want true")
		}
	})

	t.Run("an existing SysProcAttr field is preserved", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{SysProcAttr: &syscall.SysProcAttr{Noctty: true}}
		setProcessGroup(cmd)

		if !cmd.SysProcAttr.Setpgid {
			t.Error("setProcessGroup() Setpgid = false, want true")
		}
		if !cmd.SysProcAttr.Noctty {
			t.Error("setProcessGroup() Noctty = false, want true: a pre-existing field must be preserved")
		}
	})
}

// waitStatusSignaled reports whether waitErr, an *exec.ExitError from a
// process this test started, terminated by signal.
func waitStatusSignaled(t *testing.T, waitErr error) bool {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}

// TestSignalProcessGroup covers the ESRCH-suppression contract that
// distinguishes this function from a bare syscall.Kill, and proves the
// signal is actually delivered to a live process group rather than
// merely returning nil unconditionally.
func TestSignalProcessGroup(t *testing.T) {
	t.Parallel()

	t.Run("ESRCH against an implausible pid is suppressed", func(t *testing.T) {
		t.Parallel()

		if err := signalProcessGroup(math.MaxInt32, syscall.SIGTERM); err != nil {
			t.Errorf("signalProcessGroup(MaxInt32, SIGTERM) = %v, want nil (ESRCH must be suppressed)", err)
		}
	})

	t.Run("a live process group is actually terminated", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("sleep", "3600") //nolint:gosec // bounded fake local process killed below
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() error = %v, want nil", err)
		}
		pid := cmd.Process.Pid
		t.Cleanup(func() {
			_ = signalProcessGroup(pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		})

		if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
			t.Fatalf("signalProcessGroup(pid, SIGTERM) = %v, want nil", err)
		}

		waitErr := cmd.Wait()
		if !waitStatusSignaled(t, waitErr) {
			t.Errorf("cmd.Wait() = %v, want the process to have been terminated by SIGTERM", waitErr)
		}
	})
}

// TestLaunchNativeProbe drives launchNativeProbe against a real stub
// executable per case, so every assertion below is provable against a
// live subprocess rather than a canned return value.
func TestLaunchNativeProbe(t *testing.T) {
	t.Parallel()

	t.Run("captures combined stdout then stderr", func(t *testing.T) {
		t.Parallel()

		script := agenttest.WriteScript(t, t.TempDir(), "echo-both.sh", `printf 'stdout line\n'
printf 'stderr line\n' 1>&2
`)

		output, err := launchNativeProbe(t, script, nil)
		if err != nil {
			t.Fatalf("launchNativeProbe() error = %v, want nil", err)
		}
		want := "stdout line\nstderr line\n"
		if output != want {
			t.Errorf("launchNativeProbe() output = %q, want %q", output, want)
		}
	})

	t.Run("a start failure returns errNativeLaunchFailed with no captured output", func(t *testing.T) {
		t.Parallel()

		missing := filepath.Join(t.TempDir(), "does-not-exist")

		output, err := launchNativeProbe(t, missing, nil)
		if !errors.Is(err, errNativeLaunchFailed) {
			t.Fatalf("launchNativeProbe() error = %v, want errNativeLaunchFailed", err)
		}
		if output != "" {
			t.Errorf("launchNativeProbe() output = %q, want empty: nothing ever ran", output)
		}
	})

	t.Run("a non-zero exit surfaces the raw wait error, unwrapped from either sentinel", func(t *testing.T) {
		t.Parallel()

		script := agenttest.WriteScript(t, t.TempDir(), "exit-seven.sh", "exit 7\n")

		_, err := launchNativeProbe(t, script, nil)
		if err == nil {
			t.Fatal("launchNativeProbe() error = nil, want the process's own exit error")
		}
		if errors.Is(err, errNativeLaunchFailed) || errors.Is(err, errNativeBoundExceeded) {
			t.Errorf("launchNativeProbe() error = %v, want neither sentinel for an ordinary non-zero exit", err)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("launchNativeProbe() error = %v (%T), want an *exec.ExitError", err, err)
		}
		if exitErr.ExitCode() != 7 {
			t.Errorf("launchNativeProbe() exit code = %d, want 7", exitErr.ExitCode())
		}
	})

	t.Run("kills the whole process group, not just its direct child", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		pidFile := filepath.Join(dir, "descendant.pid")
		// The grandchild's stdout/stderr are redirected away from the
		// leader's own, inherited pipes: os/exec's Wait blocks until
		// every holder of the write end of a piped Stdout/Stderr closes
		// it, and an unredirected background grandchild would hold that
		// pipe open for the whole 60s sleep, stalling cmd.Wait() long
		// after the leader itself has exited.
		script := agenttest.WriteScript(t, dir, "leader.sh", `sleep 60 >/dev/null 2>&1 &
echo $! > "$1"
exit 0
`)

		if _, err := launchNativeProbe(t, script, []string{pidFile}); err != nil {
			t.Fatalf("launchNativeProbe() error = %v, want nil", err)
		}

		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatalf("read the descendant pid file: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("parse descendant pid %q: %v", raw, err)
		}

		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("syscall.Kill(%d, 0) = %v, want ESRCH: launchNativeProbe must have killed the whole process group its leader forked into, not just the leader itself", pid, err)
		}
	})
}

// corroborateAbsentSurfaceCoordinates builds a Coordinates whose Profile
// decodes from the package's own sampleProfileJSON fixture and whose
// CommandPath is a stub script carrying scriptContent, so
// corroborateAbsentSurface's native launch runs a real subprocess
// rather than a canned return value.
func corroborateAbsentSurfaceCoordinates(t *testing.T, scriptContent string) Coordinates {
	t.Helper()
	profilePath := writeValidProfileFixture(t)
	profile, err := qualification.ReadRuntimeProfileFile(profilePath)
	if err != nil {
		t.Fatalf("qualification.ReadRuntimeProfileFile(%q) error = %v, want nil", profilePath, err)
	}
	script := agenttest.WriteScript(t, t.TempDir(), "native.sh", scriptContent)
	return Coordinates{CommandPath: script, Model: "fixture-model", Profile: profile}
}

// TestCorroborateAbsentSurface confirms the ordinary path: a native
// launch whose output the profile's recognizer does not recognize
// corroborates the declared absence and passes, driven through the
// real recognizer against a real subprocess rather than a canned
// return value.
func TestCorroborateAbsentSurface(t *testing.T) {
	t.Parallel()

	coords := corroborateAbsentSurfaceCoordinates(t, `printf 'plain unrecognized output\n'
`)
	corroborateAbsentSurface(t, coords, qualification.SurfaceNativeJSON)
}

// TestCorroborateAbsentSurfaceFailsOnARecognizedTerminal is the
// function's mandatory negative control: a native launch whose output
// the same recognizer DOES recognize as a terminal outcome - the
// function's own regression target, a runtime that starts responding
// on a surface its profile still declares absent - must fail the
// corroboration rather than pass it silently. Tuning the stub toward
// this expectation would make the test fail, not pass, which is why
// this direction cannot be gamed the way a positive-only assertion
// could be.
//
// The failing call runs in a subprocess, matching the
// TestGatedFatalsOnInvalidCoordinate idiom already established in
// probe_test.go: calling t.Fatalf directly against this test's own
// *testing.T would mark this whole package's run failed, which is not
// the behavior under test here.
func TestCorroborateAbsentSurfaceFailsOnARecognizedTerminal(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_CORROBORATE_ABSENT_SURFACE_HELPER_PROCESS") == "1" {
		coords := corroborateAbsentSurfaceCoordinates(t, `printf '{"response":{}}\n'
`)
		corroborateAbsentSurface(t, coords, qualification.SurfaceNativeJSON)
		t.Fatal("corroborateAbsentSurface() returned instead of calling t.Fatalf for a recognized terminal outcome")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCorroborateAbsentSurfaceFailsOnARecognizedTerminal$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(), "PROBE_CORROBORATE_ABSENT_SURFACE_HELPER_PROCESS=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from corroborateAbsentSurface() on a recognized terminal outcome; output:\n%s", output)
	}
	if !strings.Contains(string(output), "contradicting the declaration") {
		t.Errorf("helper process output = %s, want it to name the contradiction", output)
	}
}

// TestDefaultOutputDir confirms the ordinary path: a fresh,
// run-scoped, sortie-qualification-prefixed directory outside the
// repository tree, removed by this test once observed.
func TestDefaultOutputDir(t *testing.T) {
	t.Parallel()

	dir := defaultOutputDir(t)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("defaultOutputDir() = %q, want an existing directory (stat error = %v)", dir, err)
	}
	if !strings.HasPrefix(filepath.Base(dir), "sortie-qualification-") {
		t.Errorf("defaultOutputDir() base name = %q, want the sortie-qualification- prefix", filepath.Base(dir))
	}

	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if pathWithin(dir, root) {
		t.Errorf("defaultOutputDir() = %q, want a path outside the repository tree %q", dir, root)
	}
}

// TestDefaultOutputDirRejectsRepositoryTreeTempDir drives the guard
// through a helper subprocess, matching the TestGatedFatalsOnInvalidCoordinate
// idiom already established in probe_test.go: calling t.Fatalf directly
// against this test's own *testing.T would mark this whole package's
// run failed, which is not the behavior under test. TMPDIR is
// redirected into a scratch directory under the repository tree so
// os.MkdirTemp resolves there, giving the guard a real overlap to
// reject instead of a canned one.
func TestDefaultOutputDirRejectsRepositoryTreeTempDir(t *testing.T) {
	t.Parallel()

	root, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	if os.Getenv("PROBE_DEFAULT_OUTPUT_DIR_HELPER_PROCESS") == "1" {
		defaultOutputDir(t)
		t.Fatal("defaultOutputDir() returned instead of calling t.Fatalf for a TMPDIR inside the repository tree")
		return
	}

	scratchTMPDIR := filepath.Join(root, ".sortie-probe-unix-test-tmp")
	if err := os.MkdirAll(scratchTMPDIR, 0o755); err != nil {
		t.Fatalf("create a scratch TMPDIR under the repository tree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratchTMPDIR) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestDefaultOutputDirRejectsRepositoryTreeTempDir$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(),
		"PROBE_DEFAULT_OUTPUT_DIR_HELPER_PROCESS=1",
		"TMPDIR="+scratchTMPDIR,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from defaultOutputDir()'s guard against a TMPDIR inside the repository tree; output:\n%s", output)
	}
	if !strings.Contains(string(output), "resolved inside the repository tree") {
		t.Errorf("helper process output = %s, want it to name the repository-tree rejection", output)
	}
}

// TestMustRepositoryRoot confirms mustRepositoryRoot forwards to
// qualification.RepositoryRootFromWD rather than returning some other
// directory, and that what it returns actually carries the repository
// root marker.
func TestMustRepositoryRoot(t *testing.T) {
	t.Parallel()

	want, err := qualification.RepositoryRootFromWD()
	if err != nil {
		t.Fatalf("qualification.RepositoryRootFromWD() error = %v, want nil", err)
	}

	got := mustRepositoryRoot(t)
	if got != want {
		t.Errorf("mustRepositoryRoot() = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(got, "go.mod")); err != nil {
		t.Errorf("mustRepositoryRoot() = %q, want a directory carrying go.mod: %v", got, err)
	}
}

// canaryStub wraps body in a stub that first asserts it received the
// sample profile's own version_args. Without that assertion a stub
// ignoring its arguments would keep the test green even if the canary
// stopped passing VersionArgs at all.
func canaryStub(body string) string {
	return `[ "$1" = "--version" ] || { printf 'unexpected args: %s\n' "$*" 1>&2; exit 9; }
` + body + "\n"
}

// TestRunAuthenticationCanary confirms the canary's two outcomes: a
// runtime that serves version_args lets the run continue, and one that
// fails them stops it before any graded surface spends a turn.
//
// The failing call runs in a subprocess, matching the idiom the tests
// above already use: the canary reports its failure through t.Fatalf,
// which against this test's own *testing.T would fail the package run
// rather than exercise the behavior under test.
func TestRunAuthenticationCanary(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_AUTHENTICATION_CANARY_HELPER_PROCESS") == "1" {
		runAuthenticationCanary(t, corroborateAbsentSurfaceCoordinates(t, canaryStub("exit 3")))
		t.Fatal("runAuthenticationCanary() returned instead of calling t.Fatalf for a runtime that failed version_args")
		return
	}

	t.Run("a runtime that serves version_args passes", func(t *testing.T) {
		t.Parallel()

		runAuthenticationCanary(t, corroborateAbsentSurfaceCoordinates(t, canaryStub("printf 'stub 1.0\\n'")))
	})

	t.Run("a runtime that fails version_args stops the run", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command(os.Args[0], "-test.run=^TestRunAuthenticationCanary$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
		cmd.Env = append(os.Environ(), "PROBE_AUTHENTICATION_CANARY_HELPER_PROCESS=1")
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("the helper process exited 0, want a failure; output:\n%s", output)
		}
		if !strings.Contains(string(output), "authentication canary") {
			t.Errorf("helper output does not name the canary; output:\n%s", output)
		}
	})
}
