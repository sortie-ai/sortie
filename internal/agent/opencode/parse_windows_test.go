//go:build windows

package opencode

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
)

// exportHeldDescendantScenario names the fake opencode binary the
// export query test uses: on an "export" invocation it starts an already-built
// descendant inheriting its own standard output and standard error,
// records the descendant's pid, then prints the configured fixture
// body and exits 0.
const exportHeldDescendantScenario = "opencode.export-held-descendant"

type exportHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
	Body         string
}

func runExportHeldDescendant(args []string, p exportHeldDescendantParams) int {
	if len(args) == 0 || args[0] != "export" {
		return 0
	}
	if err := startOpencodeWinHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "export held descendant: %v\n", err)
		return 2
	}
	fmt.Print(p.Body)
	return 0
}

// modelsHeldDescendantScenario names the fake opencode binary the
// models query test uses: on a "models" invocation it starts an already-built
// descendant the same way, then prints a two-line catalog and exits 0.
const modelsHeldDescendantScenario = "opencode.models-held-descendant"

type modelsHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
}

func runModelsHeldDescendant(args []string, p modelsHeldDescendantParams) int {
	if len(args) == 0 || args[0] != "models" {
		return 0
	}
	if err := startOpencodeWinHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "models held descendant: %v\n", err)
		return 2
	}
	fmt.Print("anthropic/claude-sonnet-4-5\nopenai/gpt-5\n")
	return 0
}

func startOpencodeWinHeldDescendant(childPath, childPIDPath string) error {
	cmd := exec.Command(childPath) //nolint:gosec // fake runtime path this scenario was handed
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(childPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}

func init() {
	fakeScenarios[exportHeldDescendantScenario] = agenttest.Typed(runExportHeldDescendant)
	fakeScenarios[modelsHeldDescendantScenario] = agenttest.Typed(runModelsHeldDescendant)
}

// pollOpencodeWinPIDAndAssertGone polls path for a positive PID, then
// polls until the process can no longer be opened or reports exited,
// failing t if either bound is exceeded.
func pollOpencodeWinPIDAndAssertGone(t *testing.T, path string) {
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

// TestQueryExportUsage_HeldDescendantHoldingOutput covers the export
// query on the Windows job: with a held descendant holding the export query's
// output, queryExportUsage returns within its timer with the export's
// usage, and the descendant is gone.
func TestQueryExportUsage_HeldDescendantHoldingOutput(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	binPath := agenttest.FakeRuntime(t, dir, "opencode", exportHeldDescendantScenario, exportHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
		Body:         string(loadFixture(t, "export_usage.json")),
	})
	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       binPath,
			WorkspacePath: dir,
		},
		sessionID:  "ses_abc123",
		baseLogger: slog.Default(),
	}

	type outcome struct{ usage exportUsage }
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		done <- outcome{queryExportUsage(context.Background(), state, 0)}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queryExportUsage() did not return within 5s, want a bounded return despite the held descendant")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryExportUsage() took %v, want within 3s", elapsed)
	}
	if oc.usage.InputTokens != 1750 {
		t.Errorf("InputTokens = %d, want 1750", oc.usage.InputTokens)
	}
	if oc.usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", oc.usage.OutputTokens)
	}

	pollOpencodeWinPIDAndAssertGone(t, pidPath)
}

// TestQueryModelNotFound_HeldDescendantHoldingOutput covers the models
// query on the Windows job: with a held descendant holding the models query's
// output, queryModelNotFound returns within its timer reporting the
// configured model present in the catalog (ok false), and the
// descendant is gone.
func TestQueryModelNotFound_HeldDescendantHoldingOutput(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	binPath := agenttest.FakeRuntime(t, dir, "opencode", modelsHeldDescendantScenario, modelsHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
	})

	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       binPath,
			WorkspacePath: dir,
		},
		baseLogger: slog.Default(),
	}
	state.passthrough.Model = "anthropic/claude-sonnet-4-5"

	type outcome struct {
		message string
		ok      bool
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		message, ok := queryModelNotFound(context.Background(), state)
		done <- outcome{message, ok}
	}()

	var oc outcome
	select {
	case oc = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queryModelNotFound() did not return within 5s, want a bounded return despite the held descendant")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryModelNotFound() took %v, want within 3s", elapsed)
	}
	if oc.ok {
		t.Errorf("ok = true (message %q), want false: the configured model is present in the catalog", oc.message)
	}

	pollOpencodeWinPIDAndAssertGone(t, pidPath)
}
