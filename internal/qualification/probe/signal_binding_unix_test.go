//go:build unix

package probe

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/qualification"
)

// probePathFromArgs returns the probe executable named inside args, or "" when
// none does, so a fake runtime can run the program the prompt names.
func probePathFromArgs(args []string) string {
	for _, arg := range args {
		for candidate := range strings.SplitSeq(arg, `"`) {
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && info.Mode()&0o100 != 0 {
				return candidate
			}
		}
	}
	return ""
}

// spawnNamedProbe starts the probe named in args in the caller's process group
// and reports whether it started. That process is what an induction's signal
// binds to, so a fake skipping it never ran the probe.
func spawnNamedProbe(args []string) bool {
	path := probePathFromArgs(args)
	if path == "" {
		return false
	}
	cmd := exec.Command(path) //nolint:gosec // path is a probe executable this package wrote under a test temporary directory
	return cmd.Start() == nil
}

// forgedMarkerScenario writes the probe's started marker and hangs without
// running the probe, standing in for a model that read the probe script and
// wrote the file itself.
const forgedMarkerScenario = "forged-marker"

func runForgedMarker(_ []string, _ struct{}) int {
	if err := os.WriteFile(probeStartedMarker, nil, 0o600); err != nil {
		return 2
	}
	agenttest.Hang()
	return 0
}

const genuineProbeScenario = "genuine-probe"

func runGenuineProbe(args []string, _ struct{}) int {
	if !spawnNamedProbe(args) {
		return 2
	}
	agenttest.Hang()
	return 0
}

// signalIgnoringScenario runs the probe, ignores SIGINT outright, and never
// ends on its own.
const signalIgnoringScenario = "signal-ignoring"

func runSignalIgnoring(args []string, _ struct{}) int {
	signal.Ignore(syscall.SIGINT)
	if !spawnNamedProbe(args) {
		return 2
	}
	agenttest.Hang()
	return 0
}

func init() {
	probeScenarios[forgedMarkerScenario] = agenttest.Typed(runForgedMarker)
	probeScenarios[genuineProbeScenario] = agenttest.Typed(runGenuineProbe)
	probeScenarios[signalIgnoringScenario] = agenttest.Typed(runSignalIgnoring)
}

// signalBindingFixture carries this package's three probe executables and short
// bounds, so a control over a runtime that never answers reports in seconds.
func signalBindingFixture(t *testing.T) *sharedFixture {
	t.Helper()
	scriptDir := t.TempDir()
	return &sharedFixture{
		workspaceRoot:         t.TempDir(),
		tracker:               &groupTracker{},
		usage:                 &usageTracker{},
		nonce:                 "TESTNONCE",
		failingProbe:          agenttest.WriteScript(t, scriptDir, "failing-probe", failingProbeScript),
		cancellationProbe:     agenttest.WriteScript(t, scriptDir, "cancellation-probe", cancellationProbeScript),
		transportProbe:        agenttest.WriteScript(t, scriptDir, "transport-probe", transportProbeScript),
		probeObservationBound: 5 * time.Second,
		signalWaitBound:       3 * time.Second,
	}
}

func TestAsyncSignalBindsToObservedProbeExecution(t *testing.T) {
	t.Parallel()

	t.Run("a runtime that forges the marker without running the probe induces nothing", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", forgedMarkerScenario, struct{}{})

		obs := induceRetryableTransport(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)

		if obs.Grade != qualification.GradeNotObserved || obs.Outcome != qualification.OutcomeFixtureInductionFailed {
			t.Errorf("induceRetryableTransport(...) = %+v, want not_observed/fixture_induction_failed: no probe ever ran, so no signal moment was earned", obs)
		}
		if obs.EvidencePath != "" {
			t.Errorf("induceRetryableTransport(...) evidence path = %q, want empty", obs.EvidencePath)
		}
	})

	t.Run("a runtime that really runs the probe earns the signal", func(t *testing.T) {
		t.Parallel()

		script := agenttest.FakeRuntime(t, t.TempDir(), "native", genuineProbeScenario, struct{}{})

		obs := induceRetryableTransport(t, semanticCoordinates(script), signalBindingFixture(t), semanticTestSurface)

		if obs.Grade != qualification.GradeUsable || obs.Outcome != qualification.OutcomePass {
			t.Errorf("induceRetryableTransport(...) = %+v, want usable/pass: the probe really ran under this launch", obs)
		}
	})
}

func TestNativeAsyncSignalBoundsTheWaitAfterTheSignal(t *testing.T) {
	t.Parallel()

	script := agenttest.FakeRuntime(t, t.TempDir(), "native", signalIgnoringScenario, struct{}{})
	fixture := signalBindingFixture(t)

	started := time.Now()
	obs := induceCancellation(t, semanticCoordinates(script), fixture, semanticTestSurface)
	elapsed := time.Since(started)

	if bound := fixture.observationBound() + fixture.signalBound() + nativeDrainBound; elapsed > bound {
		t.Errorf("induceCancellation(...) took %s, want at most %s: a runtime that ignores the signal held the wait open", elapsed, bound)
	}
	if obs.Grade == qualification.GradeUsable {
		t.Errorf("induceCancellation(...) = %+v, want no usable grade: the runtime never honored the signal", obs)
	}
}

func TestBoundedLaunchEndsWhileADescendantHoldsTheStreams(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := agenttest.WriteScript(t, dir, "leaves-a-holder",
		"sh -c 'sleep 120' &\necho parent-done\nexit 0\n")

	type launchResult struct {
		output string
		err    error
	}
	done := make(chan launchResult, 1)
	go func() {
		output, err := launchNativeProbe(t, script, nil, dir, nil, newOwnedDescendants(&groupTracker{}))
		done <- launchResult{output: output, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Errorf("launchNativeProbe(...) error = %v, want nil: the direct child exited cleanly", got.err)
		}
		if !strings.Contains(got.output, "parent-done") {
			t.Errorf("launchNativeProbe(...) output = %q, want it to carry the direct child's own line", got.output)
		}
	case <-time.After(2 * nativeDrainBound):
		t.Fatal("launchNativeProbe(...) did not return while a descendant held the inherited output streams")
	}
}

func TestPSSnapshotIsBounded(t *testing.T) {
	t.Parallel()

	if psSnapshotBound <= 0 {
		t.Fatalf("psSnapshotBound = %s, want a positive bound", psSnapshotBound)
	}

	members, err := psSnapshot()
	if err != nil {
		t.Fatalf("psSnapshot() error = %v, want nil", err)
	}
	if len(members) == 0 {
		t.Error("psSnapshot() returned no member, want at least this process")
	}
}

func TestLaunchRunsProgram(t *testing.T) {
	t.Parallel()

	members := []processMember{
		{pid: 100, pgid: 100, ppid: 1, basename: "runtime"},
		{pid: 101, pgid: 100, ppid: 100, basename: "cancellation-probe"},
		{pid: 200, pgid: 200, ppid: 1, basename: "runtime"},
		{pid: 201, pgid: 201, ppid: 200, basename: "transport-probe"},
		{pid: 300, pgid: 300, ppid: 1, basename: "cancellation-probe"},
	}

	tests := []struct {
		name     string
		pgid     int
		basename string
		want     bool
	}{
		{name: "a group member running the named probe", pgid: 100, basename: "cancellation-probe", want: true},
		{name: "a descendant that left the group behind", pgid: 200, basename: "transport-probe", want: true},
		{name: "another launch's probe is not this launch's", pgid: 100, basename: "transport-probe", want: false},
		{name: "an unrelated process running the same program", pgid: 999, basename: "cancellation-probe", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := launchRunsProgram(members, tt.pgid, tt.basename); got != tt.want {
				t.Errorf("launchRunsProgram(members, %d, %q) = %t, want %t", tt.pgid, tt.basename, got, tt.want)
			}
		})
	}
}

func TestAwaitProbeExecution(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	probe := agenttest.WriteScript(t, dir, "transport-probe", transportProbeScript)

	t.Run("a running probe under the launch is observed", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command(probe) //nolint:gosec // probe is this test's own fixture
		cmd.Dir = t.TempDir()
		procutil.SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() error = %v, want nil", err)
		}
		pgid := cmd.Process.Pid
		t.Cleanup(func() {
			_ = signalProcessGroup(pgid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		})

		if !awaitProbeExecution(newOwnedDescendants(&groupTracker{}), probe, pgid, time.Now().Add(30*time.Second)) {
			t.Error("awaitProbeExecution(...) = false, want true for a probe this launch is running")
		}
	})

	t.Run("a marker written with no running probe is not observed", func(t *testing.T) {
		t.Parallel()

		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, probeStartedMarker), nil, 0o600); err != nil {
			t.Fatalf("write the forged marker: %v", err)
		}
		if !probeStarted(workspace) {
			t.Fatal("the forged marker did not land, so this control proves nothing")
		}

		// A launch whose leader has already been reaped owns no process
		// at all, so anything this reports would come from the file.
		ended := exec.Command("/bin/sh", "-c", "exit 0") //nolint:gosec // fixed command
		if err := ended.Run(); err != nil {
			t.Fatalf("run the short-lived launch: %v", err)
		}

		if awaitProbeExecution(newOwnedDescendants(&groupTracker{}), probe, ended.Process.Pid, time.Now().Add(500*time.Millisecond)) {
			t.Error("awaitProbeExecution(...) = true, want false: a file is not an execution")
		}
	})
}
