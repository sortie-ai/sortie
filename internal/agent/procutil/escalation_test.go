package procutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func init() {
	fakeScenarios["procutil.escalation-leader"] = agenttest.Typed(runEscalationLeader)
	fakeScenarios["procutil.escalation-descendant"] = agenttest.Typed(runEscalationDescendant)
}

type escalationLeaderParams struct {
	DescendantPath string
}

func runEscalationLeader(_ []string, p escalationLeaderParams) int {
	signal.Notify(make(chan os.Signal, 1), os.Interrupt, syscall.SIGTERM)

	cmd := exec.Command(p.DescendantPath) //nolint:gosec // fake runtime path under t.TempDir()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "escalation leader: start descendant: %v\n", err)
		return 1
	}
	_ = cmd.Wait() //nolint:errcheck // a stubborn descendant is only ever reaped by a force kill, whose error carries nothing this fixture needs
	return 0
}

type escalationDescendantParams struct {
	PIDFile      string
	Marker       string
	ExitOnSignal bool
}

func runEscalationDescendant(_ []string, p escalationDescendantParams) int {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	if err := os.WriteFile(p.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "escalation descendant: write pid: %v\n", err)
		return 1
	}

	if !p.ExitOnSignal {
		agenttest.Hang()
		return 0
	}

	<-ch
	if err := os.WriteFile(p.Marker, []byte("terminated"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "escalation descendant: write marker: %v\n", err)
		return 1
	}
	return 0
}

func pollEscalationPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(string(data)); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollEscalationPID(%q) = no PID after %v, want a PID", path, timeout)
	return 0
}

func pollEscalationFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func processAliveForTest(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	defer func() { _ = proc.Release() }()
	if runtime.GOOS == "windows" {
		return true
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func pollProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAliveForTest(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const escalationGrace = 5 * time.Second

func TestArmGroupEscalation_ForceTerminatesStubbornGroupMember(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")

	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", "procutil.escalation-descendant", escalationDescendantParams{
		PIDFile: pidFile,
	})
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.escalation-leader", escalationLeaderParams{
		DescendantPath: descendantPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, escalationGrace)

	pipes, err := StartWithOwnedPipes(cmd, nil)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes() = %v, want nil", err)
	}
	defer func() { _ = pipes.Close() }()

	leaderPID := cmd.Process.Pid
	t.Cleanup(func() { _ = KillProcessGroup(leaderPID) })

	descendantPID := pollEscalationPID(t, pidFile, 5*time.Second)

	cancel()
	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	wait := escalationGrace + groupDrainBound + 2*time.Second
	if !pollProcessGone(descendantPID, wait) {
		t.Errorf("descendant %d still alive %v after cancellation, want gone (grace %v + drain bound %v)", descendantPID, wait, escalationGrace, groupDrainBound)
	}
}

func TestArmGroupEscalation_GroupDrainedInsideGraceSendsNoForceSignal(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "descendant.terminated")
	pidFile := filepath.Join(dir, "descendant.pid")

	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", "procutil.escalation-descendant", escalationDescendantParams{
		PIDFile:      pidFile,
		Marker:       marker,
		ExitOnSignal: true,
	})
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.escalation-leader", escalationLeaderParams{
		DescendantPath: descendantPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, escalationGrace)

	pipes, err := StartWithOwnedPipes(cmd, nil)
	if err != nil {
		t.Fatalf("StartWithOwnedPipes() = %v, want nil", err)
	}
	defer func() { _ = pipes.Close() }()

	leaderPID := cmd.Process.Pid
	t.Cleanup(func() { _ = KillProcessGroup(leaderPID) })

	descendantPID := pollEscalationPID(t, pidFile, 5*time.Second)

	forceSends, waitForQuiet := armEscalationForceSendProbe(t, leaderPID)

	cancelledAt := time.Now()
	cancel()
	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	if !pollEscalationFile(marker, 5*time.Second) {
		t.Fatalf("descendant did not write %q, want it to have caught and exited on the graceful signal well inside the %v grace", marker, escalationGrace)
	}
	if !pollProcessGone(descendantPID, 5*time.Second) {
		t.Fatalf("descendant %d still alive after exiting on the graceful signal, want gone", descendantPID)
	}

	floor := cancelledAt.Add(escalationGrace + groupDrainBound)
	waitForQuiet(500*time.Millisecond, floor, escalationGrace+groupDrainBound+10*time.Second)
	if calls := forceSends.Load(); calls != 0 {
		t.Errorf("force-termination seam called %d times after a group that drained inside the grace, want 0", calls)
	}
}
