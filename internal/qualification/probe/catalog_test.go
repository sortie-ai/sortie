//go:build unix

package probe

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// ".git/config" is a file git init always creates, so naming it triggers the
// guard deterministically without any external state.
func TestResolveSharedFixtureFailsOnStrayProjectConfigPath(t *testing.T) {
	t.Parallel()

	if os.Getenv("PROBE_RESOLVE_SHARED_FIXTURE_HELPER_PROCESS") == "1" {
		coords := Coordinates{Profile: qualification.RuntimeProfile{ProjectConfigPaths: []string{".git/config"}}}
		resolveSharedFixture(t, coords)
		t.Fatal("resolveSharedFixture() returned instead of calling t.Fatalf on a stray project_config_paths entry")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestResolveSharedFixtureFailsOnStrayProjectConfigPath$", "-test.v") //nolint:gosec // re-invokes this package's own compiled test binary
	cmd.Env = append(os.Environ(), "PROBE_RESOLVE_SHARED_FIXTURE_HELPER_PROCESS=1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the helper process exited 0, want a non-zero exit from resolveSharedFixture() on a stray project_config_paths entry; output:\n%s", output)
	}
	if !strings.Contains(string(output), "unexpectedly carries project_config_paths entry") {
		t.Errorf("helper process output = %s, want it to name the stray entry", output)
	}
}

func TestProbeScriptsCarryNoNetworkCommand(t *testing.T) {
	t.Parallel()

	networkTools := []string{"curl", "wget", "nc ", "ssh", "scp", "ftp"}
	scripts := map[string]string{
		"failing":      failingProbeScript,
		"cancellation": cancellationProbeScript,
		"transport":    transportProbeScript,
	}
	for name, body := range scripts {
		for _, tool := range networkTools {
			if strings.Contains(body, tool) {
				t.Errorf("probe script %q contains %q, want no network command", name, tool)
			}
		}
	}
}

// awaitProbeStarted blocks until workspace carries the probe started marker or
// deadline passes. It serves the probe scripts' own contract tests, where the
// script writes the file; no induction binds a signal to it, since a marker
// attests only to a file the measured runtime can write itself.
func awaitProbeStarted(workspace string, deadline time.Time) bool {
	for {
		if probeStarted(workspace) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFailingProbeScriptTouchesMarkerAndExitsNonZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.WriteScript(t, dir, "failing-probe", failingProbeScript)

	cmd := exec.Command(script) //nolint:gosec // script is this test's own fixture
	cmd.Dir = dir
	err := cmd.Run()

	var exitErr *exec.ExitError
	if err == nil {
		t.Fatal("failing-probe script exited 0, want a non-zero exit")
	}
	if !isExitError(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Errorf("failing-probe script error = %v, want exit code 1", err)
	}
	if !probeStarted(dir) {
		t.Error("failing-probe script did not create its own started marker before exiting")
	}
}

func isExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return false
	}
	*target = ee
	return true
}

func TestCancellationProbeScriptExitsGracefullyOnSIGINT(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.WriteScript(t, dir, "cancellation-probe", cancellationProbeScript)

	cmd := exec.Command(script) //nolint:gosec // script is this test's own fixture
	cmd.Dir = dir
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v, want nil", err)
	}
	pgid := cmd.Process.Pid

	deadline := time.Now().Add(30 * time.Second)
	if !awaitProbeStarted(dir, deadline) {
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		t.Fatal("cancellation-probe script never created its own started marker")
	}

	if err := signalProcessGroup(pgid, syscall.SIGINT); err != nil {
		t.Fatalf("signalProcessGroup(pgid, SIGINT) error = %v, want nil", err)
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Errorf("cmd.Wait() = %v, want a clean exit(0) after a graceful SIGINT trap", waitErr)
	}
}

func TestTransportProbeScriptRequiresForcefulTermination(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.WriteScript(t, dir, "transport-probe", transportProbeScript)

	cmd := exec.Command(script) //nolint:gosec // script is this test's own fixture
	cmd.Dir = dir
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v, want nil", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(30 * time.Second)
	if !awaitProbeStarted(dir, deadline) {
		t.Fatal("transport-probe script never created its own started marker")
	}

	if err := signalProcessGroup(pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("signalProcessGroup(pgid, SIGKILL) error = %v, want nil", err)
	}
	waitErr := cmd.Wait()
	if !waitStatusSignaled(t, waitErr) {
		t.Errorf("cmd.Wait() = %v, want the process to have been terminated by SIGKILL rather than exiting on its own", waitErr)
	}
}

func TestLaunchEnvironmentAllowlist(t *testing.T) {
	// Not parallel: uses t.Setenv.
	t.Setenv("PROBE_TEST_AUTH_VAR", "auth-value")
	t.Setenv("PROBE_TEST_AMBIENT_LEAK", "should-not-leak")

	coords := Coordinates{AuthEnvNames: []string{"PROBE_TEST_AUTH_VAR"}}
	env := launchEnvironment(coords)

	wantPresent := map[string]bool{"PATH": false, "HOME": false, "PROBE_TEST_AUTH_VAR": false}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, tracked := wantPresent[name]; tracked {
			wantPresent[name] = true
		}
		if name == "PROBE_TEST_AMBIENT_LEAK" {
			t.Errorf("launchEnvironment(...) = %v, want no entry for the ambient variable outside the allowlist", env)
		}
	}
	for name, present := range wantPresent {
		if !present {
			t.Errorf("launchEnvironment(...) = %v, want an entry for %s", env, name)
		}
	}
}

func TestNewRunNonceIsHexAndVaries(t *testing.T) {
	t.Parallel()

	a := newRunNonce(t)
	b := newRunNonce(t)
	if a == b {
		t.Errorf("newRunNonce() produced %q twice, want two distinct nonces", a)
	}
	if len(a) != 32 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("newRunNonce() = %q, want a 32-character lowercase hex string", a)
	}
}
