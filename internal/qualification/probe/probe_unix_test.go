//go:build unix

package probe

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
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

const spawnDetachedChildScenario = "spawn-detached-child"

func spawnDetachedChild(args []string, hangPath string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "spawn-detached-child: missing pid file argument")
		return 1
	}
	cmd := exec.Command(hangPath) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawn-detached-child: start %s: %v\n", hangPath, err)
		return 1
	}
	if err := os.WriteFile(args[0], []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "spawn-detached-child: write pid file: %v\n", err)
		return 1
	}
	return 0
}

const versionCanaryScenario = "version-canary"

func runVersionScenario(args []string, out agenttest.Output) int {
	if len(args) == 0 || args[0] != "--version" {
		fmt.Fprintf(os.Stderr, "unexpected args: %s\n", strings.Join(args, " "))
		return 9
	}
	return out.Run()
}

func init() {
	probeScenarios[mcpToolServerScenario] = agenttest.Typed(runMCPToolServer)
	probeScenarios[spawnDetachedChildScenario] = agenttest.Typed(spawnDetachedChild)
	probeScenarios[versionCanaryScenario] = agenttest.Typed(runVersionScenario)
	probeScenarios[semanticOrchestrationScenario] = agenttest.Typed(runSemanticOrchestration)
}

type semanticOrchestrationParams struct {
	// ReceiptDir, when non-empty, receives a "cancel-launched" marker the moment
	// the cancellation branch runs, so a test can prove that branch never ran
	// rather than only inspecting the Observation, which a declared not-inducible
	// pair would produce either way.
	ReceiptDir string
}

const semanticOrchestrationScenario = "semantic-orchestration"

func runSemanticOrchestration(args []string, params semanticOrchestrationParams) int {
	joined := strings.Join(args, "\x00")
	switch {
	case strings.Contains(joined, "transport-probe"):
		if !spawnNamedProbe(args) {
			return 2
		}
		agenttest.Hang()
		return 0
	case strings.Contains(joined, "cancellation-probe"):
		if params.ReceiptDir != "" {
			if err := os.WriteFile(filepath.Join(params.ReceiptDir, "cancel-launched"), nil, 0o600); err != nil {
				return 2
			}
		}
		if !spawnNamedProbe(args) {
			return 2
		}
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT)
		<-sigCh
		if _, err := fmt.Fprint(os.Stdout, `{"type":"result","status":"cancelled"}`); err != nil {
			return 2
		}
		return 0
	default:
		if _, err := fmt.Fprint(os.Stdout, `{"type":"result","status":"end_turn"}`); err != nil {
			return 2
		}
		return 0
	}
}

func semanticOrchestrationCoordinates(runtimePath string, notInducible bool) Coordinates {
	profile := qualification.RuntimeProfile{
		ProbePrompts: map[string]string{
			promptKeySuccess:        "please succeed",
			promptKeyRuntimeRefusal: "please refuse",
		},
		EntryPoints: map[qualification.Surface]qualification.EntryPoint{
			qualification.SurfaceNativeJSON: {Args: []string{"--prompt", "{prompt}"}},
		},
		Recognizers: map[qualification.Surface]qualification.Recognizer{
			qualification.SurfaceNativeJSON: {
				Locator:       qualification.TerminalLocator{Mode: "discriminated", DiscriminatorKey: "type", DiscriminatorValue: "result"},
				ErrorMembers:  []string{"error"},
				StatusMember:  "status",
				StatusCases:   map[string]qualification.Case{"cancelled": qualification.CaseCancellation, "refusal": qualification.CaseRuntimeRefusal},
				StatusEndTurn: []string{"end_turn"},
			},
		},
	}
	if notInducible {
		profile.NotInducibleCases = []qualification.SurfaceNotInducible{
			{Surface: qualification.SurfaceNativeJSON, Case: qualification.CaseCancellation, Reason: qualification.NotInducibleTerminalAtExitOnly},
		}
	}
	return Coordinates{CommandPath: runtimePath, Profile: profile}
}

func emptyCollectedObservations() *collectedObservations {
	return &collectedObservations{
		semantic: map[qualification.Surface]map[qualification.Case]qualification.Observation{},
	}
}

func TestInduceNativeSemanticsSkipsDeclaredNotInducibleCancellation(t *testing.T) {
	t.Parallel()

	newFixture := signalBindingFixture

	t.Run("a declared not-inducible pair produces zero cancellation launches", func(t *testing.T) {
		t.Parallel()

		receiptDir := t.TempDir()
		script := agenttest.FakeRuntime(t, t.TempDir(), "native", semanticOrchestrationScenario, semanticOrchestrationParams{ReceiptDir: receiptDir})
		coords := semanticOrchestrationCoordinates(script, true)
		collected := emptyCollectedObservations()

		induceNativeSemantics(t, coords, newFixture(t), collected, qualification.SurfaceNativeJSON)

		if _, err := os.Stat(filepath.Join(receiptDir, "cancel-launched")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("induceNativeSemantics(...) launched the cancellation probe despite the profile declaring (surface, cancellation) not inducible (stat err = %v)", err)
		}
		if obs, ok := collected.semantic[qualification.SurfaceNativeJSON][qualification.CaseCancellation]; ok {
			t.Errorf("collected.semantic[...][CaseCancellation] = %+v, want no collected observation at all: a declared not-inducible pair is graded from the declaration alone", obs)
		}
	})

	t.Run("no declaration launches the cancellation probe", func(t *testing.T) {
		t.Parallel()

		receiptDir := t.TempDir()
		script := agenttest.FakeRuntime(t, t.TempDir(), "native", semanticOrchestrationScenario, semanticOrchestrationParams{ReceiptDir: receiptDir})
		coords := semanticOrchestrationCoordinates(script, false)
		collected := emptyCollectedObservations()

		induceNativeSemantics(t, coords, newFixture(t), collected, qualification.SurfaceNativeJSON)

		if _, err := os.Stat(filepath.Join(receiptDir, "cancel-launched")); err != nil {
			t.Errorf("induceNativeSemantics(...) did not launch the cancellation probe though the profile declares nothing (stat err = %v), want the fixture's own guard to be the only thing that can suppress it", err)
		}
		if _, ok := collected.semantic[qualification.SurfaceNativeJSON][qualification.CaseCancellation]; !ok {
			t.Errorf("collected.semantic[...] carries no CaseCancellation entry, want one collected from the real launch")
		}
	})
}

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

		hangPath := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Hang: true})
		cmd := exec.Command(hangPath) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
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

func TestLaunchNativeProbe(t *testing.T) {
	t.Parallel()

	t.Run("captures combined stdout then stderr", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Stdout: "stdout line\n", Stderr: "stderr line\n"})

		output, err := launchNativeProbe(t, script, nil, t.TempDir(), nil, nil)
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

		output, err := launchNativeProbe(t, missing, nil, t.TempDir(), nil, nil)
		if !errors.Is(err, errNativeLaunchFailed) {
			t.Fatalf("launchNativeProbe() error = %v, want errNativeLaunchFailed", err)
		}
		if output != "" {
			t.Errorf("launchNativeProbe() output = %q, want empty: nothing ever ran", output)
		}
	})

	t.Run("a non-zero exit surfaces the raw wait error, unwrapped from either sentinel", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{ExitCode: 7})

		_, err := launchNativeProbe(t, script, nil, t.TempDir(), nil, nil)
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
		// The grandchild is started with nil Stdout/Stderr, which os/exec routes
		// to the null device rather than inheriting the leader's piped streams:
		// cmd.Wait blocks until every holder of a piped stream's write end closes
		// it, so an inherited pipe would stall Wait for as long as the grandchild
		// hangs, long after the leader exited.
		hangPath := agenttest.FakeRuntime(t, dir, "grandchild", agenttest.OutputScenario, agenttest.Output{Hang: true})
		script := agenttest.FakeRuntime(t, dir, "leader", spawnDetachedChildScenario, hangPath)

		if _, err := launchNativeProbe(t, script, []string{pidFile}, t.TempDir(), nil, nil); err != nil {
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

// testSharedFixture is the smallest fixture a launch needs: a
// run-scoped root to launch in and a tracker to register its group
// with, with no wrapper, so the launch runs the command directly.
func testSharedFixture(t *testing.T) *sharedFixture {
	t.Helper()
	return &sharedFixture{workspaceRoot: t.TempDir(), tracker: &groupTracker{}}
}

// corroborateAbsentSurfaceCoordinates builds a Coordinates whose Profile
// decodes from the package's own sampleProfileJSON fixture and whose
// CommandPath is runtimePath, so corroborateAbsentSurface's native
// launch runs a real subprocess rather than a canned return value.
func corroborateAbsentSurfaceCoordinates(t *testing.T, runtimePath string) Coordinates {
	t.Helper()
	profilePath := writeValidProfileFixture(t)
	profile, err := qualification.ReadRuntimeProfileFile(profilePath)
	if err != nil {
		t.Fatalf("qualification.ReadRuntimeProfileFile(%q) error = %v, want nil", profilePath, err)
	}
	return Coordinates{CommandPath: runtimePath, Model: "fixture-model", Profile: profile}
}

func TestCorroborateAbsentSurface(t *testing.T) {
	t.Parallel()

	runtimePath := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: "plain unrecognized output\n"})
	coords := corroborateAbsentSurfaceCoordinates(t, runtimePath)
	corroborateAbsentSurface(t, coords, testSharedFixture(t), qualification.SurfaceNativeJSON)
}

func TestCorroborateAbsentSurfaceFailsOnARecognizedTerminal(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_CORROBORATE_ABSENT_SURFACE_HELPER_PROCESS") == "1" {
		runtimePath := agenttest.FakeRuntime(t, t.TempDir(), "native", agenttest.OutputScenario, agenttest.Output{Stdout: `{"response":{}}` + "\n"})
		coords := corroborateAbsentSurfaceCoordinates(t, runtimePath)
		corroborateAbsentSurface(t, coords, testSharedFixture(t), qualification.SurfaceNativeJSON)
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

// TestRunVersionCanary confirms the canary's two outcomes: a runtime
// that serves version_args lets the run continue, and one that fails
// them stops it before any graded surface spends a turn. Both outcomes
// launch versionCanaryScenario, which fails the run on its own if it
// stopped receiving the sample profile's version_args at all: a stub
// ignoring its arguments would keep this test green even if the canary
// stopped passing them.
//
// The failing call runs in a subprocess, matching the idiom the tests
// above already use: the canary reports its failure through t.Fatalf,
// which against this test's own *testing.T would fail the package run
// rather than exercise the behavior under test.
func TestRunVersionCanary(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_VERSION_CANARY_HELPER_PROCESS") == "1" {
		runtimePath := agenttest.FakeRuntime(t, t.TempDir(), "canary", versionCanaryScenario, agenttest.Output{ExitCode: 3})
		runVersionCanary(t, corroborateAbsentSurfaceCoordinates(t, runtimePath), testSharedFixture(t))
		t.Fatal("runVersionCanary() returned instead of calling t.Fatalf for a runtime that failed version_args")
		return
	}

	t.Run("a runtime that serves version_args passes", func(t *testing.T) {
		t.Parallel()

		runtimePath := agenttest.FakeRuntime(t, t.TempDir(), "canary", versionCanaryScenario, agenttest.Output{Stdout: "stub 1.0\n"})
		runVersionCanary(t, corroborateAbsentSurfaceCoordinates(t, runtimePath), testSharedFixture(t))
	})

	t.Run("a runtime that fails version_args stops the run", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command(os.Args[0], "-test.run=^TestRunVersionCanary$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
		cmd.Env = append(os.Environ(), "PROBE_VERSION_CANARY_HELPER_PROCESS=1")
		output, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("the helper process exited 0, want a failure; output:\n%s", output)
		}
		if !strings.Contains(string(output), "version canary") {
			t.Errorf("helper output does not name the canary; output:\n%s", output)
		}
	})
}

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

func TestAssertSessionGroupAbsent(t *testing.T) {
	t.Parallel()

	pid := reapedProcessGroupLeaderPID(t)
	assertSessionGroupAbsent(t, domain.Session{AgentPID: strconv.Itoa(pid)})
}

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

func TestAwaitMinuteBoundaryReturnsImmediatelyOutsideTheCurrentMinute(t *testing.T) {
	t.Parallel()

	start := time.Now()
	awaitMinuteBoundary(time.Now().Add(-time.Hour))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("awaitMinuteBoundary(createdAt outside the current minute) took %v, want an immediate return", elapsed)
	}
}
