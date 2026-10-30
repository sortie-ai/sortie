//go:build unix

package clientprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/launchtest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func startTestSession(ctx context.Context, a *ClientProtocolAdapter, params domain.StartSessionParams) (domain.Session, error) {
	return startSession(ctx, a, params, agentcore.NewTurnEndUsage())
}

// syncBuffer is a bytes.Buffer guarded by a mutex, for a logger a
// background release goroutine writes to concurrently with the test
// goroutine reading it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

func TestStartSessionMCPInjectionWire(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	workspace := t.TempDir()
	mcpConfigPath := agenttest.WriteSeededMCPConfig(t, workspace)
	replaceInMCPConfig(t, mcpConfigPath, `"sortie-tools":{"command":"/usr/bin/sortie","args":["mcp-server"]},`, "")
	capturePath := filepath.Join(dir, "session_new.jsonl")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))

	params := domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
		MCPConfigPath: mcpConfigPath,
	}

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, params)
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

	agenttest.AssertMCPInjection(t, declared.MCPDialect, mcpConfigPath, agenttest.MCPLaunchSurface{
		Wire: []string{strings.TrimSpace(string(captured))},
	})
}

const (
	toolServerChildScenario          = "clientprotocol.tool-server-child"
	connectedToolServerChildScenario = "clientprotocol.tool-server-child-connected"
)

func init() {
	fakeRuntimeScenarios[toolServerChildScenario] = func([]string, json.RawMessage) int {
		fmt.Println(`{"listening":"127.0.0.1:45678"}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	fakeRuntimeScenarios[connectedToolServerChildScenario] = func([]string, json.RawMessage) int {
		fmt.Println(`{"listening":"127.0.0.1:45678"}`)
		fmt.Println(`{"connected":true}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
}

func replaceInMCPConfig(t *testing.T, path, old, replacement string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	patched := strings.Replace(string(raw), old, replacement, 1)
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func seedRunnableToolServer(t *testing.T, workspace, childScenario string) string {
	t.Helper()

	mcpConfigPath := agenttest.WriteSeededMCPConfig(t, workspace)
	child := agenttest.FakeRuntime(t, t.TempDir(), "toolserver", childScenario, nil)
	replaceInMCPConfig(t, mcpConfigPath, "/usr/bin/sortie", child)
	return mcpConfigPath
}

func sessionNewServers(t *testing.T, capturePath string) []mcpServer {
	t.Helper()

	lines := readCapturedLines(t, capturePath)
	if len(lines) != 1 {
		t.Fatalf("captured %d session/new requests, want 1", len(lines))
	}
	var request struct {
		Params newSessionRequest `json:"params"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &request); err != nil {
		t.Fatalf("decode session/new request: %v", err)
	}
	return request.Params.MCPServers
}

func sortieToolsEntries(t *testing.T, capturePath string) []mcpServer {
	t.Helper()

	var entries []mcpServer
	for _, server := range sessionNewServers(t, capturePath) {
		switch {
		case server.HTTP != nil && server.HTTP.Name == "sortie-tools":
			entries = append(entries, server)
		case server.Stdio != nil && server.Stdio.Name == "sortie-tools":
			entries = append(entries, server)
		}
	}
	return entries
}

func TestStartSessionLocalRelayDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		advertiseHTTP bool
		wantHTTP      bool
	}{
		{name: "runtime advertising http gets the loopback relay", advertiseHTTP: true, wantHTTP: true},
		{name: "runtime without http gets the stdio entry", advertiseHTTP: false, wantHTTP: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			workspace := t.TempDir()
			capturePath := filepath.Join(dir, "session_new.jsonl")
			agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{
				Handshake:              true,
				IncludeMCPCapabilities: tt.advertiseHTTP,
				CaptureSessionNewPath:  capturePath,
			})

			session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
				WorkspacePath: workspace,
				AgentConfig:   domain.AgentConfig{Command: agentPath},
				MCPConfigPath: seedRunnableToolServer(t, workspace, connectedToolServerChildScenario),
			})
			if err != nil {
				t.Fatalf("startSession() error = %v", err)
			}
			t.Cleanup(func() {
				if err := stopSession(context.Background(), session); err != nil {
					t.Errorf("stopSession() error = %v", err)
				}
			})

			entries := sortieToolsEntries(t, capturePath)

			if len(entries) != 1 {
				t.Fatalf("session/new carries %d sortie-tools entries, want 1", len(entries))
			}
			entry := entries[0]
			if !tt.wantHTTP {
				if entry.Stdio == nil {
					t.Errorf("sortie-tools entry = %+v, want the stdio entry", entry)
				}
				return
			}
			if entry.Stdio != nil {
				t.Fatalf("session/new carries the stdio sortie-tools entry %+v, want only the http entry", entry.Stdio)
			}
			if !strings.HasPrefix(entry.HTTP.URL, "http://127.0.0.1:") || !strings.HasSuffix(entry.HTTP.URL, "/mcp") {
				t.Errorf("sortie-tools url = %q, want http://127.0.0.1:<port>/mcp", entry.HTTP.URL)
			}
			var authorization string
			for _, header := range entry.HTTP.Headers {
				if header.Name == "Authorization" {
					authorization = header.Value
				}
			}
			if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) == len("Bearer ") {
				t.Errorf("sortie-tools Authorization header is not a non-empty bearer value")
			}
		})
	}
}

func TestStartSessionRemoteRuntimeWithoutHTTPDeclaresToolServersGap(t *testing.T) {
	// Not parallel: sets PATH via t.Setenv.
	dir := t.TempDir()
	sshDir := t.TempDir()
	workspace := t.TempDir()

	agenttest.NewSSHStandIn(t, sshDir, agenttest.SSHStandIn{
		PATH:    sshStandInEnvPath(t, true),
		Forward: agenttest.SSHForwardAllocated,
	})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	capturePath := filepath.Join(dir, "session_new.jsonl")
	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{
		Handshake:             true,
		CaptureSessionNewPath: capturePath,
	})

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		MCPConfigPath: seedRunnableToolServer(t, workspace, toolServerChildScenario),
		SSHHost:       "user@stand-in-host",
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	servers := sessionNewServers(t, capturePath)

	if len(servers) != 0 {
		t.Errorf("session/new carries %d servers, want none: a remote runtime without HTTP gets no entry", len(servers))
	}
	if !session.NoToolChannel {
		t.Errorf("session.NoToolChannel = false, want true: the withheld tool server is a declared gap")
	}
}

func recordSSHLaunch(t *testing.T, standIn string) (command string, wire func() (argv []string, stdin string)) {
	t.Helper()

	dir := t.TempDir()
	argvLog, stdinLog := filepath.Join(dir, "argv"), filepath.Join(dir, "stdin")
	command = agenttest.WriteScript(t, t.TempDir(), "ssh", fmt.Sprintf(
		"printf '%%s\\0' \"$@\" >> '%s'\ntee -a '%s' | '%s' \"$@\"\n", argvLog, stdinLog, standIn))
	return command, func() ([]string, string) {
		argv, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", argvLog, err)
		}
		stdin, err := os.ReadFile(stdinLog)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", stdinLog, err)
		}
		return strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00"), string(stdin)
	}
}

func TestStartSessionRemoteHTTPRuntimeKeepsOrchestratorSecretsOffArgvAndStdin(t *testing.T) {
	// Not parallel: sets PATH via t.Setenv.
	const envBlockSecret = "seeded-env-block-secret"

	dir := t.TempDir()
	sshDir := t.TempDir()
	workspace := t.TempDir()

	standIn := agenttest.NewSSHStandIn(t, t.TempDir(), agenttest.SSHStandIn{
		PATH:    sshStandInEnvPath(t, true),
		Forward: agenttest.SSHForwardAllocated,
	})
	command, wire := recordSSHLaunch(t, standIn)
	if err := os.Symlink(command, filepath.Join(sshDir, "ssh")); err != nil {
		t.Fatalf("Symlink(ssh): %v", err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	mcpConfigPath := seedRunnableToolServer(t, workspace, connectedToolServerChildScenario)
	replaceInMCPConfig(t, mcpConfigPath, `"args":["mcp-server"]`, `"args":["mcp-server"],"env":{"SORTIE_TEST_CREDENTIAL":"`+envBlockSecret+`"}`)
	capturePath := filepath.Join(dir, "session_new.jsonl")
	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{
		Handshake:              true,
		IncludeMCPCapabilities: true,
		CaptureSessionNewPath:  capturePath,
	})

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		MCPConfigPath: mcpConfigPath,
		SSHHost:       "user@stand-in-host",
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	entries := sortieToolsEntries(t, capturePath)
	if len(entries) != 1 || entries[0].HTTP == nil {
		t.Fatalf("session/new sortie-tools entries = %+v, want the one http entry", entries)
	}
	var bearer string
	for _, header := range entries[0].HTTP.Headers {
		if header.Name == "Authorization" {
			bearer = strings.TrimPrefix(header.Value, "Bearer ")
		}
	}
	argv, stdin := wire()

	agenttest.AssertRemoteMCPSurface(t, mcpConfigPath, bearer, agenttest.RemoteLaunchSurface{
		Argv:       argv,
		Stdin:      stdin,
		SessionNew: readCapturedLines(t, capturePath),
	})
	if slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, envBlockSecret) }) {
		t.Errorf("ssh argv = %q, want no env-block credential", argv)
	}
	if strings.Contains(stdin, envBlockSecret) {
		t.Errorf("ssh stdin = %q, want no env-block credential", stdin)
	}
}

func TestStartSessionRejectsMalformedMCPConfig(t *testing.T) {
	t.Parallel()

	scriptPath := agenttest.WriteScript(t, t.TempDir(), "agent.sh", mcpHandshakeScript(filepath.Join(t.TempDir(), "unused")))

	agenttest.AssertMalformedMCPConfigRejected(t, func(mcpConfigPath string) error {
		session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: scriptPath},
			MCPConfigPath: mcpConfigPath,
		})
		if err == nil {
			_ = stopSession(context.Background(), session)
		}
		return err
	})
}

// mcpHandshakeThenGracefulExitScript installs a TERM handler that waits
// delaySeconds and writes evidencePath, then answers the handshake. The handler
// is installed before the handshake so there is no window in which the default
// disposition applies: a caller signals only after startSession has returned,
// which cannot happen until the handshake is answered.
func mcpHandshakeThenGracefulExitScript(evidencePath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
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

// Cancelling the launch context signals the agent gracefully rather than with
// an uncatchable kill. Without startSession's cmd.Cancel and cmd.WaitDelay,
// exec.CommandContext SIGKILLs the child, the handler never runs, and the
// evidence file never appears.
func TestStartSessionCancelledLaunchContextSignalsGracefully(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evidencePath := filepath.Join(dir, "evidence")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeThenGracefulExitScript(evidencePath, "0.4"))

	ctx, cancel := context.WithCancel(context.Background())
	session, err := startTestSession(ctx, &ClientProtocolAdapter{}, domain.StartSessionParams{
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
	waittest.File(t, evidencePath)
}

func TestStartSessionLocalWorkspaceIsSymlink(t *testing.T) {
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: link,
		AgentConfig:   domain.AgentConfig{Command: "/nonexistent/sortie-clientprotocol-fixture"},
	})
	if err == nil {
		t.Fatal("startSession(linked workspace) error = nil, want non-nil")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrInvalidWorkspaceCwd {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrInvalidWorkspaceCwd)
	}

	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatalf("ReadDir(target): %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("link target gained entries %v, want none (no subprocess started)", entries)
	}
}

// stderrThenExitScript writes marker to stderr and exits without answering
// stdin, so initialize fails against a closed connection rather than a timeout.
func stderrThenExitScript(marker string) string {
	return `printf '%s\n' '` + marker + `' 1>&2
exit 1
`
}

func TestStartSessionEmitsCollectedStderrAtWarnOnFailedInitialize(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-init-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed initialize did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// mcpHandshakeWithDetachedChildScript backgrounds a helper under setsid, a new
// session and group leader that escapes the group SetGroupCancel placed this
// script in.
func mcpHandshakeWithDetachedChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("setsid ", pidFile)
}

// mcpHandshakeWithGroupChildScript leaves one idle child inside the script's
// process group, so teardown's group-directed termination reaches it.
func mcpHandshakeWithGroupChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("", pidFile)
}

// mcpHandshakeWithChildScript builds both of the above; launcher decides whether
// the child keeps the script's process group or leaves it.
func mcpHandshakeWithChildScript(launcher, pidFile string) string {
	return launcher + `sh -c 'echo $$ >"` + pidFile + `"; while :; do sleep 0.05; done' </dev/null >/dev/null 2>&1 &
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
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

// initThenStderrExitScript answers initialize, writes marker to stderr, and
// exits before session/new, so the failure arrives from resolveSession rather
// than doInitialize.
func initThenStderrExitScript(marker string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      printf '%s\n' '` + marker + `' 1>&2
      exit 1
      ;;
  esac
done
`
}

func TestStartSessionEmitsCollectedStderrAtWarnOnFailedResolveSession(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-resolve-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", initThenStderrExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed resolveSession did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// Teardown reaches a descendant that stays in the group it was launched into,
// so a survivor outside that group means the group was escaped rather than that
// teardown reaches nothing. Removing SetGroupCancel fails this test, not the
// escaped-member one.
func TestStopSessionReachesGroupChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "group-child.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithGroupChildScript(pidPath))

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	childPID := waittest.PIDFile(t, pidPath)
	launchtest.KillOnCleanup(t, session.AgentPID)

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	waittest.ProcessGone(t, childPID)
}

// A descendant that detaches into its own process group survives the
// group-directed termination. This is not a defect: kill_process_group can only
// reach the group it targeted.
func TestStopSessionDoesNotReachEscapedProcessGroupMember(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithDetachedChildScript(pidPath))

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	escapedPID := waittest.PIDFile(t, pidPath)
	t.Cleanup(func() { agenttest.KillEscaped(pidPath) })

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Errorf("escaped process group member (pid %d) liveness probe after stopSession() = %v, want still running", escapedPID, err)
	}
}

// stderrThenExitWithDetachedHolderScript backgrounds a setsid descendant that
// inherits this script's stdout handle, then exits before answering initialize.
// The runtime is gone, but the escaped descendant keeps the output pipe from
// reaching EOF, so a handshake call is still in flight when the release gives up.
func stderrThenExitWithDetachedHolderScript(marker, pidFile string) string {
	return `setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
printf '%s\n' '` + marker + `' 1>&2
exit 7
`
}

// No t.Parallel(): installs a process-wide slog default.
func TestStartSessionHandshakeAbandonedByEscapedDescendantFailsWithPortExit(t *testing.T) {
	agenttest.RequireSetsid(t)

	const marker = "distinguishing-stderr-line-handshake-abandoned"
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { agenttest.KillEscaped(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitWithDetachedHolderScript(marker, pidPath))

	var buf syncBuffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}

	_, err := startTestSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("startSession() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	var earlyExitErr *agentcore.EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Fatalf("startSession() error = %v, chain does not hold an *agentcore.EarlyExitError", agentErr)
	}
	if earlyExitErr.Status() != "exit status 7" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 7")
	}
	if !strings.Contains(earlyExitErr.Output(), marker) {
		t.Errorf("EarlyExitError.Output() = %q, want it to contain %q", earlyExitErr.Output(), marker)
	}
	output := buf.String()
	if !hasWarnStderrLineRecord(output, marker) {
		t.Errorf("startSession() output = %s, want one record carrying level=WARN, msg=\"agent stderr\", and line=%s together (the collector also logs this same line at Debug, which must not satisfy this check on its own)", output, marker)
	}
}

// hasWarnStderrLineRecord reports whether one log line carries level=WARN,
// msg="agent stderr", and line=marker together. The collector logs the same
// line at Debug, so the three checks must hold on one record, not anywhere.
func hasWarnStderrLineRecord(output, marker string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "level=WARN") &&
			strings.Contains(line, `msg="agent stderr"`) &&
			strings.Contains(line, "line="+marker) {
			return true
		}
	}
	return false
}

// handshakeThenExitWithDetachedHolderScript answers the two startup calls, then
// spawns a setsid descendant that inherits this script's stdout handle and
// exits, so the runtime is gone while its descendant still holds the write end.
func handshakeThenExitWithDetachedHolderScript(pidFile string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
exit 0
`
}

// On Linux the release's CloseStdout already unparks the reader by the time
// this runs, so this does not exercise the post-abandonment stop arm runPump
// falls back to when a reader stays genuinely parked; that arm is proven
// separately by an untagged test whose pipes are never wired to the connection.
func TestStopSessionReturnsBoundedAfterReleaseAbandonsOnARealSubprocess(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { agenttest.KillEscaped(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", handshakeThenExitWithDetachedHolderScript(pidPath))

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}
	session, err := startTestSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}

	waittest.Receive(t, state.release.Abandoned(), "the release to abandon the parked reader")

	start := time.Now()
	if err := stopSession(context.Background(), session); err != nil {
		t.Errorf("stopSession() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	if elapsed >= ceiling {
		t.Errorf("stopSession() took %v, want under %v (the pinned teardown ceiling)", elapsed, ceiling)
	}
}

// promptThenExitWithDetachedHolderScript answers the two startup calls, then on
// the first session/prompt spawns a setsid descendant holding this script's
// stdout handle and exits, so a turn is in flight when the runtime is gone.
func promptThenExitWithDetachedHolderScript(pidFile string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
    *'"method":"session/prompt"'*)
      break
      ;;
  esac
done
setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
exit 0
`
}

func TestRunTurnEndsBoundedWhenRuntimeExitsWithEscapedDescendantHoldingOutput(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { agenttest.KillEscaped(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", promptThenExitWithDetachedHolderScript(pidPath))

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	start := time.Now()
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})
	elapsed := time.Since(start)

	if elapsed > procutil.DefaultDrainGrace {
		t.Errorf("RunTurn() took %v, want under the default drain grace %v (bounded by the injected drain grace %v)", elapsed, procutil.DefaultDrainGrace, grace)
	}
	agentErr, ok := errors.AsType[*domain.AgentError](runErr)
	if !ok {
		t.Fatalf("RunTurn() error = %v (%T), want a non-nil *domain.AgentError", runErr, runErr)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("RunTurn() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != procutil.OutputAbandonedMessage {
		t.Errorf("RunTurn() error message = %q, want %q", agentErr.Message, procutil.OutputAbandonedMessage)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	stopStart := time.Now()
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession() error = %v, want nil", err)
	}
	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	if stopElapsed := time.Since(stopStart); stopElapsed >= ceiling {
		t.Errorf("StopSession() took %v, want under %v (the pinned teardown ceiling)", stopElapsed, ceiling)
	}
}

// sshStandInEnvPath returns a PATH directory holding a symlink to sh, and to dd
// when includeDD is set. An isolated directory matters because a real sh and dd
// usually share one directory, so reusing it for the no-dd case would resolve
// dd anyway.
func sshStandInEnvPath(t *testing.T, includeDD bool) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH: %v", err)
	}

	dir := t.TempDir()
	if err := os.Symlink(shPath, filepath.Join(dir, "sh")); err != nil {
		t.Fatalf("Symlink(sh): %v", err)
	}

	if includeDD {
		ddPath, err := exec.LookPath("dd")
		if err != nil {
			t.Skipf("dd not found on PATH: %v", err)
		}
		if err := os.Symlink(ddPath, filepath.Join(dir, "dd")); err != nil {
			t.Fatalf("Symlink(dd): %v", err)
		}
	}
	return dir
}

func TestStartSessionSSH_CarriesEnvironmentVariable(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	dir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "CLIENTPROTOCOL_TEST_CARRY"
	const carriedValue = "carried-value-clientprotocol"
	t.Setenv(carriedName, carriedValue)

	capturePath := filepath.Join(dir, "captured.jsonl")
	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{
		Handshake:       true,
		EnvCaptureNames: []string{carriedName},
		EnvCapturePath:  capturePath,
	})

	session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	captures := readEnvCaptures(t, capturePath)
	if len(captures) != 1 {
		t.Fatalf("env captures = %d, want 1", len(captures))
	}
	if got := captures[0][carriedName]; got != carriedValue {
		t.Errorf("remote agent observed %q = %q, want %q", carriedName, got, carriedValue)
	}
}

func TestStartSessionSSH_NoDDEndsAsPortExitNotAgentNotFound(t *testing.T) {
	// Not parallel: installs a process-wide slog default and sets PATH
	// and the carried variable via t.Setenv.
	const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"

	dir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, false)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "CLIENTPROTOCOL_TEST_CARRY_NODD"
	t.Setenv(carriedName, "some-value")

	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{Handshake: true})

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("startSession() error kind = %q, want %q (never the agent-not-found category)", agentErr.Kind, domain.ErrPortExit)
	}
	var earlyExitErr *agentcore.EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Fatalf("startSession() error = %v, chain does not hold an *agentcore.EarlyExitError", agentErr)
	}
	if earlyExitErr.Status() != "exit status 1" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 1")
	}
	if !strings.Contains(earlyExitErr.Output(), ddMissingMessage) {
		t.Errorf("EarlyExitError.Output() = %q, want it to contain %q", earlyExitErr.Output(), ddMissingMessage)
	}

	output := buf.String()
	wantLineAttr := fmt.Sprintf("line=%q", ddMissingMessage)
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, `msg="agent stderr"`) && strings.Contains(line, wantLineAttr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("startSession() output = %s, want a WARN agent stderr record carrying the guard's message %q", output, ddMissingMessage)
	}
}

// captureCwdAndFirstLineThenHandshakeScript captures, ahead of the handshake,
// the working directory and the raw bytes of the first stdin line. A remote
// launch would land its preamble ahead of initialize on that first line and
// would never set cmd.Dir, so both are observable signs the wrong branch ran.
func captureCwdAndFirstLineThenHandshakeScript(cwdPath, firstLinePath string) string {
	return `pwd > '` + cwdPath + `'
first='` + firstLinePath + `'
capture_first=1
while IFS= read -r line; do
  if [ "$capture_first" = "1" ]; then
    printf '%s' "$line" > "$first"
    capture_first=0
  fi
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
  esac
done
`
}

func TestStartSessionLocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	// Not parallel: sets the carried variable via t.Setenv.
	const varName = "CLIENTPROTOCOL_TEST_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		dir := t.TempDir()
		cwdPath := filepath.Join(dir, "cwd.txt")
		firstLinePath := filepath.Join(dir, "first_line.txt")
		scriptPath := agenttest.WriteScript(t, dir, "agent.sh", captureCwdAndFirstLineThenHandshakeScript(cwdPath, firstLinePath))

		workspacePath := t.TempDir()
		session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
			WorkspacePath: workspacePath,
			AgentConfig:   domain.AgentConfig{Command: scriptPath},
			SSHEnvNames:   tc.sshEnvNames,
		})
		if err != nil {
			t.Fatalf("%s: startSession() error = %v, want nil", tc.name, err)
		}
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("%s: stopSession() error = %v", tc.name, err)
		}

		firstLine, err := os.ReadFile(firstLinePath)
		if err != nil {
			t.Fatalf("%s: ReadFile(first_line.txt): %v", tc.name, err)
		}
		if !strings.HasPrefix(string(firstLine), "{") {
			t.Errorf("%s: first stdin line = %q, want it to start with the initialize request's own %q", tc.name, firstLine, "{")
		}

		wantCwd, err := filepath.EvalSymlinks(workspacePath)
		if err != nil {
			t.Fatalf("%s: EvalSymlinks(workspacePath): %v", tc.name, err)
		}
		gotCwdRaw, err := os.ReadFile(cwdPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(cwd.txt): %v", tc.name, err)
		}
		gotCwd, err := filepath.EvalSymlinks(strings.TrimSpace(string(gotCwdRaw)))
		if err != nil {
			t.Fatalf("%s: EvalSymlinks(captured cwd): %v", tc.name, err)
		}
		if gotCwd != wantCwd {
			t.Errorf("%s: runtime cwd = %q, want the configured workspace %q", tc.name, gotCwd, wantCwd)
		}
	}
}

func readEnvCaptures(t *testing.T, path string) []map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	var captures []map[string]string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var capture map[string]string
		if err := json.Unmarshal([]byte(line), &capture); err != nil {
			t.Fatalf("decode env capture line %q: %v", line, err)
		}
		captures = append(captures, capture)
	}
	return captures
}

func assertDirGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %q = %v, want it removed", path, err)
	}
}

const probeExecutable = "verification-probe-agent"

type probeVerification struct{ dir string }

func init() {
	usagesource.VerificationAdditions.Register("verification-probe", func() usagesource.VerificationAddition {
		return &probeVerification{}
	})
}

func (v *probeVerification) Claim(target agentcore.LaunchTarget) (usagesource.VerificationSwitches, bool) {
	if target.RemoteCommand != "" || filepath.Base(target.Command) != probeExecutable {
		return usagesource.VerificationSwitches{}, false
	}
	dir, err := os.MkdirTemp("", "sortie-probe-")
	if err != nil {
		return usagesource.VerificationSwitches{}, false
	}
	path := filepath.Join(dir, "marker")
	if err := os.WriteFile(path, []byte("probe"), 0o600); err != nil {
		return usagesource.VerificationSwitches{}, false
	}
	v.dir = dir
	return usagesource.VerificationSwitches{Args: []string{"--probe-flag"}, Env: []string{"PROBE_FILE=" + path}}, true
}

func (v *probeVerification) Context() registry.VerificationContext {
	return registry.DeclareVerificationContext(
		registry.VerificationKeepsAllTools,
		registry.VerificationKeepsProjectInstructions,
		registry.VerificationKeepsUserInstructions,
		registry.VerificationKeepsSystemPrompt,
	)
}

func (v *probeVerification) Close() {
	if v.dir != "" {
		_ = os.RemoveAll(v.dir) //nolint:errcheck // best-effort test cleanup
	}
}

func TestStartSessionVerificationLaunch(t *testing.T) {
	tests := []struct {
		name        string
		executable  string
		working     bool
		fullContext bool
		wantClaimed bool
	}{
		{name: "a claimed launch gets its switches after the operator's, and teardown removes its file", executable: probeExecutable, wantClaimed: true},
		{name: "an unclaimed command line is unchanged", executable: "plain-agent"},
		{name: "a full-context verification is unchanged", executable: probeExecutable, fullContext: true},
		{name: "a working session is unchanged", executable: probeExecutable, working: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			argsPath, envPath := filepath.Join(dir, "args"), filepath.Join(dir, "env")
			script := `printf '%s\n' "$@" > '` + argsPath + `'
printf '%s\n' "$PROBE_FILE" > '` + envPath + `'
` + captureCwdAndFirstLineThenHandshakeScript(filepath.Join(dir, "cwd"), filepath.Join(dir, "first"))
			agentPath := agenttest.WriteScript(t, dir, tt.executable, script)

			session, err := startTestSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
				WorkspacePath:           t.TempDir(),
				AgentConfig:             domain.AgentConfig{Command: agentPath + " --acp"},
				CredentialVerification:  !tt.working,
				VerificationFullContext: tt.fullContext,
			})
			if err != nil {
				t.Fatalf("startSession() error = %v, want nil", err)
			}
			if err := stopSession(context.Background(), session); err != nil {
				t.Errorf("stopSession() error = %v", err)
			}

			wantArgs, wantFile := []string{"--acp"}, false
			if tt.wantClaimed {
				wantArgs, wantFile = []string{"--acp", "--probe-flag"}, true
			}
			if gotArgs := readCapturedLines(t, argsPath); !slices.Equal(gotArgs, wantArgs) {
				t.Errorf("runtime args = %q, want %q", gotArgs, wantArgs)
			}
			file := readCapturedLines(t, envPath)[0]
			if (file != "") != wantFile {
				t.Fatalf("PROBE_FILE = %q, want set = %v", file, wantFile)
			}
			if wantFile {
				assertDirGone(t, filepath.Dir(file))
			}
		})
	}
}

const additionVariable = "PROBE_FILE"

func additionAdapter() *ClientProtocolAdapter {
	decl := scriptedSpendDeclaration(nil, nil, nil)
	decl.Launches = func(agentcore.LaunchTarget) bool { return true }
	decl.LaunchAddition = usagesource.LaunchAddition{Name: additionVariable, Merge: func(current string, _ bool) (string, bool) {
		if current == "kept-on" {
			return current, false
		}
		return strings.TrimSuffix(current, "+off") + "+off", true
	}}
	declarations := registry.NewRegistry[usagesource.Declaration, struct{}]("usage declaration")
	declarations.Register("addition", decl)
	return &ClientProtocolAdapter{declarations: declarations}
}

func TestStartSessionLaunchAddition(t *testing.T) {
	tests := []struct {
		name         string
		remote       bool
		verification bool
		wantBase     string
	}{
		{name: "a local working launch merges into the operator's value", wantBase: "operator"},
		{name: "a local verification launch merges into the claimed switch's value", verification: true, wantBase: "marker"},
		{name: "a remote launch carries the merged value in its SSH options", remote: true, wantBase: "operator"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.verification {
				t.Setenv(additionVariable, "operator")
			}
			dir := t.TempDir()
			capturePath := filepath.Join(dir, "captured.jsonl")
			agent := agenttest.FakeRuntime(t, dir, probeExecutable, scenarioProtocolAgent, protocolAgentParams{
				Handshake: true, EnvCaptureNames: []string{additionVariable}, EnvCapturePath: capturePath,
			})
			params := domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: domain.AgentConfig{Command: agent}, CredentialVerification: tt.verification}
			if tt.remote {
				sshDir := t.TempDir()
				agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				params.SSHHost, params.SSHEnvNames = "user@stand-in-host", []string{additionVariable}
			}

			session, err := startTestSession(context.Background(), additionAdapter(), params)
			if err != nil {
				t.Fatalf("startSession() error = %v, want nil", err)
			}
			if err := stopSession(context.Background(), session); err != nil {
				t.Errorf("stopSession() error = %v", err)
			}

			captures := readEnvCaptures(t, capturePath)
			if len(captures) != 1 {
				t.Fatalf("runtime starts = %d, want 1", len(captures))
			}
			got := captures[0][additionVariable]
			if base, merged := strings.CutSuffix(got, "+off"); !merged || filepath.Base(base) != tt.wantBase {
				t.Errorf("runtime observed %s = %q, want %q merged with the addition", additionVariable, got, tt.wantBase)
			}
			if tt.remote {
				return
			}
			entries := slices.DeleteFunc(slices.Clone(session.Internal.(*sessionState).launchEnv), func(entry string) bool {
				return !strings.HasPrefix(entry, additionVariable+"=")
			})
			if want := []string{additionVariable + "=" + got}; !slices.Equal(entries, want) {
				t.Errorf("launch environment entries for %s = %q, want %q", additionVariable, entries, want)
			}
		})
	}
}

func TestNewSessionLedgerIsALowerBoundWhenARequestGoesUnrecorded(t *testing.T) {
	t.Parallel()

	const unrecordedResult = `{"models":{"currentModelId":"auto"}}`
	tests := []struct {
		name         string
		carried      string
		session      string
		wantComplete bool
	}{
		{name: "the launch carries the merged value", carried: additionVariable + "=operator+off", session: `{}`, wantComplete: true},
		{name: "the merge reports the request still on", carried: additionVariable + "=kept-on", session: `{}`},
		{name: "the launch carried nothing", carried: "UNRELATED=1", session: `{}`},
		{name: "the opening result reports a request nothing records", carried: additionVariable + "=operator+off", session: unrecordedResult},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := &sessionState{logger: discardLogger(), target: agentcore.LaunchTarget{Command: "agent"}, launchEnv: []string{tt.carried}}
			live := spendReading(10, 2, 0, "", "")
			decl, _ := additionAdapter().declarations.Get("addition")
			decl.UnrecordedRequests = func(session json.RawMessage) bool { return string(session) == unrecordedResult }

			ledger := newSessionLedger(t.Context(), state, decl, true, json.RawMessage(tt.session))

			settlement := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, &live, usage.RecordsRead{})
			if settlement.Complete != tt.wantComplete {
				t.Errorf("Settle().Complete with launch carrying %q and opening result %s = %v, want %v", tt.carried, tt.session, settlement.Complete, tt.wantComplete)
			}
		})
	}
}

func readCapturedLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}
