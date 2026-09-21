//go:build unix

package copilot

import (
	"context"
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
	"github.com/sortie-ai/sortie/internal/domain"
)

const versionHeldDescendantScenario = "copilot.version-held-descendant"

type versionHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
}

func runVersionHeldDescendant(args []string, p versionHeldDescendantParams) int {
	if len(args) == 0 || args[0] != "--version" {
		return 0
	}
	if err := startHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "version held descendant: %v\n", err)
		return 2
	}
	fmt.Println("copilot version 1.2.3")
	return 0
}

func startHeldDescendant(childPath, childPIDPath string) error {
	cmd := exec.Command(childPath) //nolint:gosec // fake runtime path this scenario was handed
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(childPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}

func init() {
	fakeScenarios[versionHeldDescendantScenario] = agenttest.Typed(runVersionHeldDescendant)
}

// pollCopilotPIDAndAssertGone polls path for a positive PID, then
// polls until kill(pid, 0) reports an error (process gone), failing t
// if either bound is exceeded.
func pollCopilotPIDAndAssertGone(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && v > 0 {
				pid = v
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("pid file %q never populated", path)
	}

	goneDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(goneDeadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}

func TestStartSession_VersionCanaryHeldDescendantHoldingOutput(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	dir := t.TempDir()
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	binPath := agenttest.FakeRuntime(t, dir, "copilot", versionHeldDescendantScenario, versionHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
	})

	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}

	start := time.Now()
	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: binPath},
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartSession() took %v, want within 3s", elapsed)
	}
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	pollCopilotPIDAndAssertGone(t, pidPath)
}
