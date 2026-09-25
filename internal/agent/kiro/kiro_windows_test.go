//go:build windows

package kiro

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
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

// pollKiroWinPIDAndAssertGone polls path for a positive PID, then polls
// until the process can no longer be opened or reports exited, failing
// t if either bound is exceeded.
func pollKiroWinPIDAndAssertGone(t *testing.T, path string) {
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
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: pid was read back from an os.Process.Pid
		if err != nil {
			return
		}
		event, waitErr := windows.WaitForSingleObject(handle, 0)
		_ = windows.CloseHandle(handle)
		if waitErr == nil && event == uint32(windows.WAIT_OBJECT_0) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still running after 3s, want gone", pid)
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

	type outcome struct{ err *domain.AgentError }
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		agentErr := checkCredential(context.Background(), target, int(procutil.DefaultStopGrace.Milliseconds()), slog.Default())
		done <- outcome{agentErr}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("checkCredential() did not return within 5s, want a bounded return despite the held descendant")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("checkCredential() took %v, want within 3s", elapsed)
	}
	if oc.err != nil {
		t.Fatalf("checkCredential() = %v, want nil", oc.err)
	}

	pollKiroWinPIDAndAssertGone(t, pidPath)
}
