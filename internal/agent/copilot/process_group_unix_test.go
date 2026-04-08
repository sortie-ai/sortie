//go:build unix

package copilot

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

// writeCopilotPgidScript creates a mock copilot script that spawns a
// long-running grandchild (sleep 3600), writes the grandchild PID to pidFile,
// emits a copilot-style init event (session.mcp_servers_loaded), then blocks
// until signaled. Using agenttest.WriteScript avoids the ETXTBSY race on Linux.
func writeCopilotPgidScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	const initEvent = `{"type":"session.mcp_servers_loaded","data":{"servers":[]},"id":"1","timestamp":"2026-01-01T00:00:00.000Z"}`
	content := fmt.Sprintf(
		"sleep 3600 &\n"+
			"CHILD_PID=$!\n"+
			"echo \"$CHILD_PID\" > '%s'\n"+
			"echo '%s'\n"+
			"sleep 3600\n",
		pidFile, initEvent,
	)
	return agenttest.WriteScript(t, dir, "fake-copilot-pgid", content)
}

// pollCopilotPIDFile polls pidFile until it contains a valid positive PID or
// the timeout expires.
func pollCopilotPIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
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
	t.Fatalf("pollCopilotPIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}

// assertCopilotProcessDead polls until syscall.Kill(pid, 0) returns an error
// (signaling the process is gone) or the timeout expires.
func assertCopilotProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("assertCopilotProcessDead: process %d still alive after %v", pid, timeout)
}

// TestRunTurn_ProcessGroupKill_ContextCancel verifies that cancelling the
// RunTurn context sends SIGTERM to the entire process group, killing both
// the copilot subprocess and any grandchildren it spawned.
func TestRunTurn_ProcessGroupKill_ContextCancel(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	workspace := t.TempDir()
	adapter, session := newTestSession(t, workspace)

	pidFile := filepath.Join(workspace, "child.pid")
	session.Internal.(*sessionState).command = writeCopilotPgidScript(t, workspace, pidFile)

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

	grandchildPID := pollCopilotPIDFile(t, pidFile, 5*time.Second)

	cancel()

	res := <-resultCh

	var agentErr *domain.AgentError
	if !errors.As(res.err, &agentErr) {
		t.Fatalf("RunTurn() error type = %T, want *domain.AgentError", res.err)
	}
	if agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnCancelled)
	}

	assertCopilotProcessDead(t, grandchildPID, 2*time.Second)
}

// TestRunTurn_ProcessGroupKill_StopSession verifies that calling StopSession
// sends SIGTERM to the entire process group, killing both the copilot
// subprocess and any grandchildren it spawned.
func TestRunTurn_ProcessGroupKill_StopSession(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	workspace := t.TempDir()
	adapter, session := newTestSession(t, workspace)

	pidFile := filepath.Join(workspace, "child.pid")
	session.Internal.(*sessionState).command = writeCopilotPgidScript(t, workspace, pidFile)

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

	grandchildPID := pollCopilotPIDFile(t, pidFile, 5*time.Second)

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() = %v", err)
	}

	<-resultCh

	assertCopilotProcessDead(t, grandchildPID, 2*time.Second)
}
