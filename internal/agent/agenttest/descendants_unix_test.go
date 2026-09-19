//go:build unix

package agenttest_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

type detachParams struct {
	Program string
	Receipt string
}

const detachOnExitScenario = "detach-on-exit"

// runDetachOnExit starts a program in its own process group, reports its pid,
// and returns at once, so nothing watching from outside sees the two linked.
func runDetachOnExit(_ []string, params detachParams) int {
	child := exec.Command(params.Program, detachedLifetime) //nolint:gosec // the program is one the calling test resolved and passed in
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "detach-on-exit: start %s: %v\n", params.Program, err)
		return 1
	}
	if err := os.WriteFile(params.Receipt, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "detach-on-exit: report the child: %v\n", err)
		_ = child.Process.Kill()
		return 1
	}
	return 0
}

func init() {
	scenarios[detachOnExitScenario] = agenttest.Typed(runDetachOnExit)
}

// detachedLifetime bounds every detached process so a control that fails before
// anything kills it leaves nothing running for longer.
const detachedLifetime = "60"

func TestMainRecordsWhatAScenarioLeftRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reported := filepath.Join(dir, "child.pid")
	exe := agenttest.FakeRuntime(t, dir, "runtime", detachOnExitScenario, detachParams{Program: requireSleeper(t), Receipt: reported})
	// Registered before the runtime exists, so a later failure still frees
	// whatever it detached.
	t.Cleanup(func() { killReported(reported) })

	if err := exec.Command(exe).Run(); err != nil { //nolint:gosec // exe is a fake runtime under t.TempDir()
		t.Fatalf("Run() error = %v", err)
	}

	child := awaitReported(t, reported)
	recorded := readRecord(t, agenttest.DescendantReceipt(exe))
	if !slices.Contains(recorded, child) {
		t.Errorf("descendant receipt = %v, want the detached child %d the scenario left running", recorded, child)
	}
}

func TestWriteRecordingScriptRecordsItsBackgroundJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reported := filepath.Join(dir, "child.pid")
	script := agenttest.WriteRecordingScript(t, dir, "leaky", requireSleeper(t)+" "+detachedLifetime+" &\necho $! > '"+reported+"'\n")
	t.Cleanup(func() { killReported(reported) })

	if err := exec.Command(script).Run(); err != nil { //nolint:gosec // script is written under t.TempDir()
		t.Fatalf("Run() error = %v", err)
	}

	child := awaitReported(t, reported)
	recorded := readRecord(t, agenttest.DescendantReceipt(script))
	if !slices.Contains(recorded, child) {
		t.Errorf("descendant receipt = %v, want the background job %d the script left running", recorded, child)
	}
}

func requireSleeper(t *testing.T) string {
	t.Helper()
	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("skipping: no sleeper to detach: %v", err)
	}
	return sleeper
}

// awaitReported reads the pid the launched program wrote to path. The trailing
// newline distinguishes a short read from a complete one.
func awaitReported(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		reported, err := os.ReadFile(path) //nolint:gosec // path is under t.TempDir()
		if err == nil && strings.HasSuffix(string(reported), "\n") {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(reported))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no process id was reported to %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readRecord(t *testing.T, receipt string) []int {
	t.Helper()

	raw, err := os.ReadFile(receipt) //nolint:gosec // receipt is under t.TempDir()
	if err != nil {
		t.Fatalf("read the descendant receipt: %v", err)
	}
	var pids []int
	for line := range strings.SplitSeq(string(raw), "\n") {
		if pid, convErr := strconv.Atoi(strings.TrimSpace(line)); convErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killReported frees the reported pid, addressed by id alone so nothing the
// control did not start is reached.
func killReported(path string) {
	reported, err := os.ReadFile(path) //nolint:gosec // path is under the caller's t.TempDir()
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(reported)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
