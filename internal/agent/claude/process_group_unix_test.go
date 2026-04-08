//go:build unix

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// writePgidScript creates a mock claude script that spawns a long-running
// grandchild (sleep 3600), writes the grandchild PID to pidFile, emits the
// Claude init event, then blocks until signaled. Using agenttest.WriteScript
// avoids the ETXTBSY race on Linux.
func writePgidScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"sleep 3600 &\n"+
			"CHILD_PID=$!\n"+
			"echo \"$CHILD_PID\" > '%s'\n"+
			"echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"test-pgid\"}'\n"+
			"sleep 3600\n",
		pidFile,
	)
	return agenttest.WriteScript(t, dir, "fake-claude-pgid", content)
}

// pollPIDFile polls pidFile until it contains a valid positive PID or the
// timeout expires.
func pollPIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollPIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}

// isZombie reports whether pid is a zombie process by reading
// /proc/<pid>/stat. Returns false if the file cannot be read.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// Format: "<pid> (<comm>) <state> ...". State follows the closing paren.
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// assertProcessDead polls until the process is gone or a zombie (effectively
// dead, awaiting reap) or the timeout expires. Zombies still satisfy
// kill(pid, 0) so we also check /proc/<pid>/stat to avoid flaky failures.
func assertProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if isZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("assertProcessDead: process %d still alive after %v", pid, timeout)
}

// TestRunTurn_ProcessGroupKill_ContextCancel verifies that cancelling the
// RunTurn context sends SIGTERM to the entire process group, killing both
// the agent subprocess and any grandchildren it spawned.
func TestRunTurn_ProcessGroupKill_ContextCancel(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	script := writePgidScript(t, tmpDir, pidFile)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type turnResult struct {
		result domain.TurnResult
		err    error
	}
	resultCh := make(chan turnResult, 1)

	go func() {
		r, e := adapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt:  "test",
			OnEvent: func(domain.AgentEvent) {},
		})
		resultCh <- turnResult{r, e}
	}()

	grandchildPID := pollPIDFile(t, pidFile, 5*time.Second)

	cancel()

	res := <-resultCh

	var agentErr *domain.AgentError
	if !errors.As(res.err, &agentErr) {
		t.Fatalf("RunTurn() error type = %T, want *domain.AgentError", res.err)
	}
	if agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnCancelled)
	}

	assertProcessDead(t, grandchildPID, 2*time.Second)
}

// TestRunTurn_ProcessGroupKill_StopSession verifies that calling StopSession
// sends SIGTERM to the entire process group, killing both the agent subprocess
// and any grandchildren it spawned.
func TestRunTurn_ProcessGroupKill_StopSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	script := writePgidScript(t, tmpDir, pidFile)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() = %v", err)
	}

	type turnResult struct {
		result domain.TurnResult
		err    error
	}
	resultCh := make(chan turnResult, 1)

	go func() {
		r, e := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "test",
			OnEvent: func(domain.AgentEvent) {},
		})
		resultCh <- turnResult{r, e}
	}()

	grandchildPID := pollPIDFile(t, pidFile, 5*time.Second)

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() = %v", err)
	}

	<-resultCh

	assertProcessDead(t, grandchildPID, 2*time.Second)
}
