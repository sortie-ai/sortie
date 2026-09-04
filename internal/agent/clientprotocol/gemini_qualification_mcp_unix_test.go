//go:build unix

package clientprotocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
	"github.com/sortie-ai/sortie/internal/registry"
)

// geminiMCPToolName is the one tool the qualification MCP server
// exposes.
const geminiMCPToolName = "sortie_qualification_probe"

// geminiMCPServerName is the server name the generated MCP
// configuration declares.
const geminiMCPServerName = "sortie-qualification-probe"

// geminiNonce returns one generated public nonce. It is a random token
// the harness owns: nothing secret travels in it, and only its
// presence is ever recorded.
func geminiNonce(t *testing.T) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	return "sortie-nonce-" + hex.EncodeToString(raw[:])
}

// geminiMCPFixture is the positive tool-server fixture: a test-owned
// local stdio MCP server exposing one named probe tool, a receipt file
// where the server records every nonce it actually received, and a
// generated MCP configuration compatible with parseMCPServers and
// parsedMCPServers.wireServers.
type geminiMCPFixture struct {
	ConfigPath  string
	ServerPath  string
	ReceiptPath string
	Nonce       string
}

// geminiNewMCPFixture writes the server executable, the receipt file's
// parent, and the generated MCP configuration under dir, and generates
// the run's public nonce. The server records nothing but received
// nonces: no header, environment value, or arbitrary payload is ever
// written.
func geminiNewMCPFixture(t *testing.T, dir string) geminiMCPFixture {
	t.Helper()

	receiptPath := filepath.Join(dir, "mcp-receipt.txt")
	nonce := geminiNonce(t)

	// The server speaks minimal JSON-RPC over stdio: initialize,
	// tools/list advertising the probe tool, and tools/call recording
	// the received nonce before returning it as text content.
	server := fmt.Sprintf(`receipt='%s'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      id=$(printf '%%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
      printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"%s\",\"version\":\"fixture\"}}}"
      ;;
    *'"method":"tools/list"'*)
      id=$(printf '%%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
      printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"tools\":[{\"name\":\"%s\",\"description\":\"qualification probe\",\"inputSchema\":{\"type\":\"object\"}}]}}"
      ;;
    *'"method":"tools/call"'*)
      id=$(printf '%%s' "$line" | sed 's/.*"id":\([0-9]*\).*/\1/')
      value=$(printf '%%s' "$line" | sed 's/.*"nonce":"\([^"]*\)".*/\1/')
      printf '%%s\n' "$value" >> "$receipt"
      printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"$value\"}]}}"
      ;;
  esac
done
`, receiptPath, geminiMCPServerName, geminiMCPToolName)
	serverPath := agenttest.WriteScript(t, dir, "mcp-fixture-server.sh", server)

	configPath := filepath.Join(dir, "mcp-config.json")
	config := fmt.Sprintf(`{"mcpServers":{"%s":{"type":"stdio","command":%q,"args":[],"env":{}}}}`,
		geminiMCPServerName, serverPath)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write qualification MCP config: %v", err)
	}

	return geminiMCPFixture{
		ConfigPath:  configPath,
		ServerPath:  serverPath,
		ReceiptPath: receiptPath,
		Nonce:       nonce,
	}
}

// geminiReadMCPReceipt returns every nonce the fixture server recorded,
// in receipt order. The receipt carries nonce strings only.
func geminiReadMCPReceipt(t *testing.T, fixture geminiMCPFixture) []string {
	t.Helper()
	raw, err := os.ReadFile(fixture.ReceiptPath) //nolint:gosec // the receipt path is written by this test under its own temp directory
	if err != nil {
		return nil
	}
	var received []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line != "" {
			received = append(received, line)
		}
	}
	return received
}

// geminiGradeMCPDelivery grades tool-server delivery. A usable verdict
// requires both that the server actually received the generated nonce
// and that the turn consumed the returned nonce; an advertisement or a
// tool_call output alone is never positive.
func geminiGradeMCPDelivery(received []string, nonce string, turnConsumed bool) qualification.Grade {
	serverReceived := false
	for _, value := range received {
		if value == nonce {
			serverReceived = true
		}
	}
	if serverReceived && turnConsumed {
		return qualification.GradeUsable
	}
	return qualification.GradeNotObserved
}

// geminiDriveMCPFixture drives the fixture server over stdio and
// reports what it observed: whether the probe tool was advertised,
// whether the server received the nonce, and what the call returned.
type geminiMCPDriveResult struct {
	advertised    bool
	serverReceipt []string
	returnedNonce string
}

// geminiDriveMCPFixture starts the fixture server, completes
// initialize, lists tools, and calls the probe tool with the fixture's
// nonce.
func geminiDriveMCPFixture(t *testing.T, fixture geminiMCPFixture) geminiMCPDriveResult {
	t.Helper()

	cmd := exec.Command(fixture.ServerPath) //nolint:gosec // the server path is written by this test under its own temp directory
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start MCP fixture server: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	reader := bufio.NewScanner(stdout)
	reader.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// The scan runs off the caller's goroutine so a server that never
	// answers cannot park the caller inside a blocking read until the
	// whole package times out with no diagnostic. readerDone gives the
	// scan a cancellation path: every send selects against it, so the
	// goroutine cannot block on a channel the caller has abandoned.
	lines := make(chan []byte)
	readerDone := make(chan struct{})
	t.Cleanup(func() { close(readerDone) })
	go func() {
		defer close(lines)
		for reader.Scan() {
			line := append([]byte(nil), reader.Bytes()...)
			select {
			case lines <- line:
			case <-readerDone:
				return
			}
		}
	}()

	call := func(method string, params string) map[string]json.RawMessage {
		t.Helper()
		if _, err := fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":7,"method":%q,"params":%s}`+"\n", method, params); err != nil {
			t.Fatalf("write %s request: %v", method, err)
		}
		deadline := time.After(awaitTimeout)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("MCP fixture server stream ended before the %s response", method)
					return nil
				}
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(line, &envelope); err != nil {
					continue
				}
				if id, ok := envelope["id"]; ok && strings.TrimSpace(string(id)) == "7" {
					return envelope
				}
			case <-deadline:
				t.Fatalf("timed out waiting for the %s response", method)
				return nil
			}
		}
	}

	call("initialize", `{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"sortie","version":"fixture"}}`)

	listResult := call("tools/list", `{}`)
	var toolList struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if raw, ok := listResult["result"]; ok {
		if err := json.Unmarshal(raw, &toolList); err != nil {
			t.Fatalf("decode tools/list result: %v", err)
		}
	}
	advertised := false
	for _, tool := range toolList.Tools {
		if tool.Name == geminiMCPToolName {
			advertised = true
		}
	}

	callResult := call("tools/call", fmt.Sprintf(`{"name":%q,"arguments":{"nonce":%q}}`, geminiMCPToolName, fixture.Nonce))
	var callPayload struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if raw, ok := callResult["result"]; ok {
		if err := json.Unmarshal(raw, &callPayload); err != nil {
			t.Fatalf("decode tools/call result: %v", err)
		}
	}
	returned := ""
	for _, block := range callPayload.Content {
		if block.Text != "" {
			returned = block.Text
		}
	}

	return geminiMCPDriveResult{
		advertised:    advertised,
		serverReceipt: geminiReadMCPReceipt(t, fixture),
		returnedNonce: returned,
	}
}

// TestGeminiQualificationMCPFixtureReceipt confirms the positive
// tool-server fixture end to end: the generated configuration parses
// through the existing parseMCPServers path, the session-creation wire
// carries the declared stdio server, and delivery is graded usable only
// when the server received the generated nonce and the turn consumed
// the returned nonce. Advertisement alone, or a tool_call output alone,
// is never a positive verdict.
func TestGeminiQualificationMCPFixtureReceipt(t *testing.T) {
	t.Parallel()

	t.Run("config parses and renders through the existing path", func(t *testing.T) {
		t.Parallel()

		fixture := geminiNewMCPFixture(t, t.TempDir())
		parsed, agentErr := parseMCPServers(fixture.ConfigPath, false)
		if agentErr != nil {
			t.Fatalf("parseMCPServers(%q) error = %v", fixture.ConfigPath, agentErr)
		}
		servers, withheld := parsed.wireServers(true)
		if withheld {
			t.Error("wireServers(true) withheld = true, want false for a stdio server")
		}
		if len(servers) != 1 || servers[0].Stdio == nil {
			t.Fatalf("wireServers(true) = %+v, want one stdio server", servers)
		}
		if servers[0].Stdio.Name != geminiMCPServerName {
			t.Errorf("server name = %q, want %q", servers[0].Stdio.Name, geminiMCPServerName)
		}
		if servers[0].Stdio.Command != fixture.ServerPath {
			t.Errorf("server command = %q, want %q", servers[0].Stdio.Command, fixture.ServerPath)
		}
	})

	t.Run("session creation carries the declared stdio server on the wire", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		fixture := geminiNewMCPFixture(t, dir)
		capturePath := filepath.Join(dir, "session_new.jsonl")
		scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))

		session, err := startSession(context.Background(), domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: scriptPath},
			MCPConfigPath: fixture.ConfigPath,
		})
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
		wire := strings.TrimSpace(string(captured))

		declared, ok := registry.Agents.Meta("agent-client-protocol")
		if !ok {
			t.Fatal(`registry.Agents.Meta("agent-client-protocol") ok = false, want true`)
		}
		agenttest.AssertMCPInjection(t, declared.MCPInjection, fixture.ConfigPath, agenttest.MCPLaunchSurface{
			Wire: []string{wire},
		})
		if !strings.Contains(wire, geminiMCPServerName) || !strings.Contains(wire, fixture.ServerPath) {
			t.Errorf("session/new wire %q does not name the declared server and its command", wire)
		}
	})

	t.Run("receipt plus consumed returned nonce grades usable", func(t *testing.T) {
		t.Parallel()

		fixture := geminiNewMCPFixture(t, t.TempDir())
		drive := geminiDriveMCPFixture(t, fixture)

		if !drive.advertised {
			t.Error("tools/list did not advertise the probe tool")
		}
		if len(drive.serverReceipt) != 1 || drive.serverReceipt[0] != fixture.Nonce {
			t.Errorf("server receipt = %v, want exactly the generated nonce", drive.serverReceipt)
		}
		if drive.returnedNonce != fixture.Nonce {
			t.Errorf("returned nonce = %q, want the generated nonce echoed back", drive.returnedNonce)
		}

		turnConsumed := strings.Contains("turn consumed "+drive.returnedNonce, fixture.Nonce)
		if got := geminiGradeMCPDelivery(drive.serverReceipt, fixture.Nonce, turnConsumed); got != qualification.GradeUsable {
			t.Errorf("geminiGradeMCPDelivery() = %s, want usable for receipt plus consumed nonce", got)
		}
	})

	t.Run("advertisement alone never grades usable", func(t *testing.T) {
		t.Parallel()

		if got := geminiGradeMCPDelivery(nil, "sortie-nonce-unsigned", false); got == qualification.GradeUsable {
			t.Errorf("geminiGradeMCPDelivery(nil, ...) = %s, want a non-usable verdict for advertisement only", got)
		}
	})

	t.Run("a tool_call output without server receipt never grades usable", func(t *testing.T) {
		t.Parallel()

		nonce := "sortie-nonce-output-only"
		if got := geminiGradeMCPDelivery(nil, nonce, true); got == qualification.GradeUsable {
			t.Errorf("geminiGradeMCPDelivery(nil, %q, true) = %s, want a non-usable verdict without server receipt", nonce, got)
		}
	})

	t.Run("a receipt without a consumed returned nonce never grades usable", func(t *testing.T) {
		t.Parallel()

		received := []string{"sortie-nonce-unconsumed"}
		if got := geminiGradeMCPDelivery(received, "sortie-nonce-unconsumed", false); got == qualification.GradeUsable {
			t.Errorf("geminiGradeMCPDelivery(receipt, nonce, false) = %s, want a non-usable verdict while the turn never consumed the nonce", got)
		}
	})
}
