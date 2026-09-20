//go:build unix

package probe

import (
	"os"
	"os/exec"
	"strings"
	"testing"

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
