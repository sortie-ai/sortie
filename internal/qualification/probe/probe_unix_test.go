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
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
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

// TestPathWithin covers the containment check the profile-supplied
// paths rely on, including the case a textual comparison cannot see: a
// symlink inside the tree pointing outside it. This repository itself
// carries such a link, so the case is not hypothetical.
func TestPathWithin(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("os.MkdirAll(%q): %v", dir, err)
		}
	}

	inside := filepath.Join(root, "inside.txt")
	mustWriteFile(t, inside, "inside\n")
	target := filepath.Join(outside, "target.txt")
	mustWriteFile(t, target, "outside\n")

	escaping := filepath.Join(root, "escaping.txt")
	if err := os.Symlink(target, escaping); err != nil {
		t.Fatalf("os.Symlink(%q, %q): %v", target, escaping, err)
	}
	sibling := root + "-copy"
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", sibling, err)
	}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "the root itself is contained", path: root, want: true},
		{name: "a file under the root is contained", path: inside, want: true},
		{name: "a symlink under the root pointing outside is not", path: escaping},
		{name: "a sibling whose name begins with the root's is not", path: sibling},
		{name: "a path outside the root is not", path: target},
		{name: "a path that does not resolve is not", path: filepath.Join(root, "absent.txt")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pathWithin(tt.path, root); got != tt.want {
				t.Errorf("pathWithin(%q, %q) = %v, want %v", tt.path, root, got, tt.want)
			}
		})
	}
}

// gradingWiringProfile is the protocol-only profile Run's grading
// wiring runs under: both native surfaces declared absent, so a
// protocol-only profile passes SurfaceProtocol to
// SetSessionContinuation. Every measurable surface still carries its
// own entry point, matching what a real profile carries: an absent
// surface is corroborated by launching it, so its entry point is never
// omitted, only declared absent.
func gradingWiringProfile() qualification.RuntimeProfile {
	return qualification.RuntimeProfile{
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceProtocol:         {Args: []string{"--acp"}},
			qualification.SurfaceNativeJSON:       {Args: []string{"--output-format", "json", "--prompt", "{prompt}"}},
			qualification.SurfaceNativeStreamJSON: {Args: []string{"--output-format", "stream-json", "--prompt", "{prompt}"}},
		},
		AbsentSurfaces: []qualification.AbsentSurface{
			{Surface: qualification.SurfaceNativeJSON, Reason: qualification.SurfaceNotOffered},
			{Surface: qualification.SurfaceNativeStreamJSON, Reason: qualification.SurfaceNotOffered},
		},
	}
}

// buildWiredFixture reproduces Run's own grading wiring against three
// independent inducer outcomes, one per wired row,
// without launching any live process: an unmeasured-variant fixture
// carries every non-wired row at its scaffolded default, and the
// three wired rows are set exactly as Run sets them from its own
// inducers' results.
func buildWiredFixture(toolGrade, permissionGrade, continuationGrade qualification.Grade) (*qualification.Fixture, qualification.RuntimeProfile) {
	profile := gradingWiringProfile()
	fixture := qualification.NewFixture(qualification.FixtureUnmeasured, profile.AbsentSurfaces...)
	fixture.SetToolServerDelivery(toolGrade, "tool server induction: "+string(toolGrade))
	fixture.SetPermissionHandling(permissionGrade, "permission induction: "+string(permissionGrade))
	fixture.SetSessionContinuation(qualification.SurfaceProtocol, continuationGrade, "continuation induction: "+string(continuationGrade))
	fixture.Finalize()
	return fixture, profile
}

// findNotesGrade returns the entry in grades matching surface and
// capability, or nil.
func findNotesGrade(grades []qualification.NotesGrade, surface qualification.Surface, capability qualification.Capability) *qualification.NotesGrade {
	for i := range grades {
		if grades[i].Surface == surface && grades[i].Capability == capability {
			return &grades[i]
		}
	}
	return nil
}

// TestRunGradingWiringAppliesEachInducerOutcomeToItsOwnRow drives
// Run's grading wiring, the three fixture setters Run calls with each
// inducer's own outcome, with each of the three grades an inducer can
// report, for each of the three rows Run wires, and reads each row's
// grade back through ExpectationFrom rather than off Fixture.Records
// directly. A setter that leaves an owning record behind, the way
// SetSessionContinuation's baseline rewrite and SetPermissionHandling's
// derivation exist to prevent, either leaves the row's derived grade
// stale or unbalances ConclusionsFromRecords' own grade-count
// invariant; either failure reddens this control.
func TestRunGradingWiringAppliesEachInducerOutcomeToItsOwnRow(t *testing.T) {
	t.Parallel()

	grades := []qualification.Grade{qualification.GradeUsable, qualification.GradeGap, qualification.GradeNotObserved}

	rows := []struct {
		name       string
		capability qualification.Capability
	}{
		{"tool server delivery", qualification.CapabilityToolServerDelivery},
		{"permission handling", qualification.CapabilityPermissionHandling},
		{"session continuation", qualification.CapabilitySessionContinuation},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			for _, grade := range grades {
				t.Run(string(grade), func(t *testing.T) {
					t.Parallel()

					toolGrade := qualification.GradeUsable
					permissionGrade := qualification.GradeUsable
					continuationGrade := qualification.GradeUsable
					switch row.capability {
					case qualification.CapabilityToolServerDelivery:
						toolGrade = grade
					case qualification.CapabilityPermissionHandling:
						permissionGrade = grade
					case qualification.CapabilitySessionContinuation:
						continuationGrade = grade
					}

					fixture, profile := buildWiredFixture(toolGrade, permissionGrade, continuationGrade)

					verdict := qualification.ComputeEligibility(fixture.Records, profile)
					conclusions, err := ConclusionsFromRecords(fixture.Records, verdict, profile)
					if err != nil {
						t.Fatalf("ConclusionsFromRecords(...) = _, %v, want nil", err)
					}

					expectation := ExpectationFrom(conclusions)
					got := findNotesGrade(expectation.Grades, qualification.SurfaceProtocol, row.capability)
					if got == nil {
						t.Fatalf("ExpectationFrom(...).Grades carries no protocol %s entry", row.capability)
					}
					if got.Grade != grade {
						t.Errorf("ExpectationFrom(...) protocol %s grade = %s, want %s", row.capability, got.Grade, grade)
					}
					if want := qualification.StatusLabel(grade); got.Label != want {
						t.Errorf("ExpectationFrom(...) protocol %s label = %q, want %q", row.capability, got.Label, want)
					}
				})
			}
		})
	}
}

// reapedProcessGroupLeaderPID starts and waits out a trivial process
// group leader, returning its pid once the kernel has reaped it, so a
// test can assert against a process group that provably no longer
// exists rather than one merely assumed absent.
func reapedProcessGroupLeaderPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v, want nil", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() error = %v, want nil", err)
	}
	return pid
}

// TestAssertSessionGroupAbsent confirms the ordinary path: a session
// whose AgentPID names a process group the kernel has already reaped
// is confirmed absent without failing t, driven against a real
// subprocess's own pid rather than a canned one.
func TestAssertSessionGroupAbsent(t *testing.T) {
	t.Parallel()

	pid := reapedProcessGroupLeaderPID(t)
	assertSessionGroupAbsent(t, domain.Session{AgentPID: strconv.Itoa(pid)})
}

// TestAssertSessionGroupAbsentFailsOnUnparsablePID is the function's
// negative control: an AgentPID that does not parse as a process id
// must fail t rather than silently skip the liveness check. The
// failing call runs in a subprocess, matching the idiom already
// established above: calling t.Fatalf directly against this test's
// own *testing.T would fail the whole package run rather than exercise
// the behavior under test.
func TestAssertSessionGroupAbsentFailsOnUnparsablePID(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_ASSERT_SESSION_GROUP_ABSENT_HELPER_PROCESS") == "1" {
		assertSessionGroupAbsent(t, domain.Session{AgentPID: "not-a-pid"})
		t.Fatal("assertSessionGroupAbsent() returned instead of calling t.Fatalf for an unparsable agent pid")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestAssertSessionGroupAbsentFailsOnUnparsablePID$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(), "PROBE_ASSERT_SESSION_GROUP_ABSENT_HELPER_PROCESS=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from assertSessionGroupAbsent() on an unparsable agent pid; output:\n%s", output)
	}
	if !strings.Contains(string(output), "did not parse as a process id") {
		t.Errorf("helper process output = %s, want it to name the parse failure", output)
	}
}

// TestRepositoryPath covers the two non-fatal outcomes: a rel that
// does not exist yet, returned unchecked for the caller's own error to
// report, and one that exists inside root, confirmed contained and
// returned.
func TestRepositoryPath(t *testing.T) {
	t.Parallel()

	t.Run("a path that does not exist is returned unchecked", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		got := repositoryPath(t, root, "does/not/exist.txt")
		want := filepath.Join(root, "does/not/exist.txt")
		if got != want {
			t.Errorf("repositoryPath(root, %q) = %q, want %q", "does/not/exist.txt", got, want)
		}
	})

	t.Run("a file inside the root is confirmed contained and returned", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		mustWriteFile(t, filepath.Join(root, "notes.md"), "hi\n")
		got := repositoryPath(t, root, "notes.md")
		want := filepath.Join(root, "notes.md")
		if got != want {
			t.Errorf("repositoryPath(root, %q) = %q, want %q", "notes.md", got, want)
		}
	})
}

// TestRepositoryPathFailsOnPathEscapingRoot is the function's negative
// control: a rel that exists but resolves outside root, through a
// symlink, must fail t rather than return the escaping path. The
// failing call runs in a subprocess for the same reason the controls
// above do.
func TestRepositoryPathFailsOnPathEscapingRoot(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_REPOSITORY_PATH_HELPER_PROCESS") == "1" {
		root := t.TempDir()
		outside := t.TempDir()
		target := filepath.Join(outside, "target.txt")
		mustWriteFile(t, target, "outside\n")
		escaping := filepath.Join(root, "escaping.txt")
		if err := os.Symlink(target, escaping); err != nil {
			t.Fatalf("os.Symlink(%q, %q): %v", target, escaping, err)
		}
		repositoryPath(t, root, "escaping.txt")
		t.Fatal("repositoryPath() returned instead of calling t.Fatalf for a path resolving outside the root")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRepositoryPathFailsOnPathEscapingRoot$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(), "PROBE_REPOSITORY_PATH_HELPER_PROCESS=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from repositoryPath() on a path escaping root; output:\n%s", output)
	}
	if !strings.Contains(string(output), "resolves outside the repository") {
		t.Errorf("helper process output = %s, want it to name the escape", output)
	}
}

// TestAwaitMinuteBoundaryReturnsImmediatelyOutsideTheCurrentMinute
// covers the loop-not-entered path: a createdAt whose own UTC minute
// already differs from the current one needs no wait at all. The
// waiting path itself is not exercised here - it would need the real
// wall clock to cross a minute boundary mid-test, which is not a bound
// a unit test should depend on.
func TestAwaitMinuteBoundaryReturnsImmediatelyOutsideTheCurrentMinute(t *testing.T) {
	t.Parallel()

	start := time.Now()
	awaitMinuteBoundary(time.Now().Add(-time.Hour))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("awaitMinuteBoundary(createdAt outside the current minute) took %v, want an immediate return", elapsed)
	}
}
