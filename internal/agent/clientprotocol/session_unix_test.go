//go:build unix

package clientprotocol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// mcpHandshakeScript is a fake agent that answers exactly the two
// calls startSession makes before returning: initialize (always id 1,
// since it is the connection's first call) and session/new (always id
// 2). It captures the raw session/new request line to captureFile
// before answering it, so a test can inspect the exact bytes the
// adapter wrote for its own tool-server delivery.
func mcpHandshakeScript(captureFile string) string {
	return `capture='` + captureFile + `'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' "$line" >> "$capture"
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
  esac
done
`
}

// TestStartSessionMCPInjectionWire calls agenttest.AssertMCPInjection
// with the wire channel populated from the actual session/new request
// bytes the adapter wrote, and is what gives cmd/sortie's
// TestEveryAgentKindHasMCPInjectionCoverage a call site for this
// kind. It launches a shell-script agent, so it runs only where
// /bin/sh exists.
func TestStartSessionMCPInjectionWire(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mcpConfigPath := writeMCPConfig(t)
	capturePath := filepath.Join(dir, "session_new.jsonl")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))

	params := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
		MCPConfigPath: mcpConfigPath,
	}

	session, err := startSession(context.Background(), params)
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured session/new request: %v", err)
	}

	declared, ok := registry.Agents.Meta("agent-client-protocol")
	if !ok {
		t.Fatal(`registry.Agents.Meta("agent-client-protocol") ok = false, want true`)
	}

	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{
		Wire: []string{strings.TrimSpace(string(captured))},
	})
}

// mcpHandshakeThenGracefulExitScript installs a handler for the
// graceful signal that waits delaySeconds and writes evidencePath
// before exiting, then answers startSession's handshake exactly as
// mcpHandshakeScript does, so a test can tell a catchable signal from
// an uncatchable kill.
//
// The handler is installed before the handshake rather than after it.
// A caller signals only once startSession has returned, which cannot
// happen until the handshake below has been answered, so installing
// first leaves no window in which the default disposition applies and
// the evidence is never written. The idle loop sleeps in short
// intervals because the handler does not run until the command it
// interrupts has finished.
func mcpHandshakeThenGracefulExitScript(evidencePath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
while :; do sleep 0.05; done
`
}

// TestStartSessionCancelledLaunchContextSignalsGracefully:
// cancelling the context a session was started with, without calling
// stopSession at all, delivers the catchable termination signal to the
// agent rather than an uncatchable kill, and the agent's handler
// evidence shows it. The property fails if either startSession's
// cmd.Cancel or its cmd.WaitDelay is removed: without them,
// exec.CommandContext's default cancellation path SIGKILLs the direct
// child instead, the handler this fixture depends on never runs, and
// the evidence file this test waits for never appears.
func TestStartSessionCancelledLaunchContextSignalsGracefully(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evidencePath := filepath.Join(dir, "evidence")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeThenGracefulExitScript(evidencePath, "0.4"))

	ctx, cancel := context.WithCancel(context.Background())
	session, err := startSession(ctx, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	cancel()
	waitForFile(t, evidencePath, awaitTimeout)
}
