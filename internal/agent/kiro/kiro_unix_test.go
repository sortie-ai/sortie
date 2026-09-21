//go:build unix

package kiro

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

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// whoamiHeldDescendantScenario names the fake kiro-cli this file's
// fixture uses: on a "whoami" invocation it starts an already-built
// descendant inheriting its own standard output and standard error,
// records the descendant's pid, then prints the success marker and
// exits 0.
const whoamiHeldDescendantScenario = "kiro.whoami-held-descendant"

type whoamiHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
}

func runWhoamiHeldDescendant(args []string, p whoamiHeldDescendantParams) int {
	if len(args) == 0 || args[0] != "whoami" {
		return 0
	}
	cmd := exec.Command(p.ChildPath) //nolint:gosec // fake runtime path this scenario was handed
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "whoami held descendant: start child: %v\n", err)
		return 2
	}
	if err := os.WriteFile(p.ChildPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "whoami held descendant: write child pid: %v\n", err)
		return 2
	}
	fmt.Print("Authenticated with API key\n")
	return 0
}

func init() {
	fakeScenarios[whoamiHeldDescendantScenario] = agenttest.Typed(runWhoamiHeldDescendant)
}

func TestCheckCredential_HeldDescendantHoldingOutput(t *testing.T) {
	setValidAPIKey(t)

	dir := t.TempDir()
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	binPath := agenttest.FakeRuntime(t, dir, "kiro-cli", whoamiHeldDescendantScenario, whoamiHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
	})

	target := agentcore.LaunchTarget{Command: binPath, WorkspacePath: dir}

	start := time.Now()
	agentErr := checkCredential(context.Background(), target, int(procutil.DefaultStopGrace.Milliseconds()))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("checkCredential() took %v, want within 3s", elapsed)
	}
	if agentErr != nil {
		t.Fatalf("checkCredential() = %v, want nil", agentErr)
	}

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(pidPath); readErr == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && v > 0 {
				pid = v
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("descendant pid file %q never populated", pidPath)
	}

	goneDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(goneDeadline) {
		if killErr := syscall.Kill(pid, 0); killErr != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}
