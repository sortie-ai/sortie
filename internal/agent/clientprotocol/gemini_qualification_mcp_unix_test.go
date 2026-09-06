//go:build unix

package clientprotocol

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// geminiQualifiedMCPToolName returns the name the runtime declares the
// fixture's one tool under. It is the only value the run-scoped
// policy's tool-server rule may name.
func geminiQualifiedMCPToolName() string {
	return "mcp_" + geminiMCPServerName + "_" + geminiMCPToolName
}

// geminiRandomToken returns one generated public token with the given
// fixed prefix, followed by 16 lowercase hexadecimal characters. It is
// a random token the harness owns: nothing secret travels in it, and
// only its presence is ever recorded.
func geminiRandomToken(t *testing.T, prefix string) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return prefix + hex.EncodeToString(raw[:])
}

// geminiNonce returns one generated public nonce.
func geminiNonce(t *testing.T) string {
	t.Helper()
	return geminiRandomToken(t, "sortie-nonce-")
}

// geminiMCPToolNameCharset is every character the runtime's own tool
// matcher accepts in a server or tool name component.
const geminiMCPToolNameCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.:"

// TestGeminiRandomTokenAndQualifiedMCPToolName binds geminiRandomToken's
// generated shape and uniqueness, the two tool-naming constants'
// character-set and underscore constraints, and geminiQualifiedMCPToolName's
// exact derivation and length bound.
func TestGeminiRandomTokenAndQualifiedMCPToolName(t *testing.T) {
	t.Parallel()

	t.Run("geminiRandomToken carries the prefix and 16 lowercase hex characters", func(t *testing.T) {
		t.Parallel()

		const prefix = "sortie-fixture-"
		token := geminiRandomToken(t, prefix)
		suffix, ok := strings.CutPrefix(token, prefix)
		if !ok {
			t.Fatalf("geminiRandomToken(%q) = %q, want it to start with the prefix", prefix, token)
		}
		if len(suffix) != 16 {
			t.Fatalf("geminiRandomToken(%q) suffix = %q, want 16 characters", prefix, suffix)
		}
		if strings.Trim(suffix, "0123456789abcdef") != "" {
			t.Errorf("geminiRandomToken(%q) suffix = %q, want lowercase hexadecimal only", prefix, suffix)
		}
	})

	t.Run("two calls with the same prefix produce different values", func(t *testing.T) {
		t.Parallel()

		first := geminiRandomToken(t, "sortie-fixture-")
		second := geminiRandomToken(t, "sortie-fixture-")
		if first == second {
			t.Errorf("geminiRandomToken() = %q twice, want a distinct value per call", first)
		}
	})

	t.Run("neither tool-naming constant carries a character outside the runtime's charset", func(t *testing.T) {
		t.Parallel()

		for _, name := range []string{geminiMCPServerName, geminiMCPToolName} {
			if strings.Trim(name, geminiMCPToolNameCharset) != "" {
				t.Errorf("name %q carries a character outside [a-zA-Z0-9_\\-.:]", name)
			}
		}
	})

	t.Run("the server name carries no underscore", func(t *testing.T) {
		t.Parallel()

		if strings.Contains(geminiMCPServerName, "_") {
			t.Errorf("geminiMCPServerName = %q carries an underscore, want none: the runtime splits the qualified name at the first underscore after the mcp_ prefix", geminiMCPServerName)
		}
	})

	t.Run("the qualified name is derived exactly and stays within the runtime's length bound", func(t *testing.T) {
		t.Parallel()

		got := geminiQualifiedMCPToolName()
		want := "mcp_" + geminiMCPServerName + "_" + geminiMCPToolName
		if got != want {
			t.Errorf("geminiQualifiedMCPToolName() = %q, want %q", got, want)
		}
		if points := utf8.RuneCountInString(got); points > 63 {
			t.Errorf("geminiQualifiedMCPToolName() = %d code points, want at most 63", points)
		}
	})
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

	// Reply is the value the server returns as the tool's result. It
	// never appears in any prompt, so its presence in a turn is
	// evidence that the server's own output re-entered the turn, not
	// that the caller's argument did.
	Reply string
}

// geminiNewMCPFixture writes the server executable, the receipt file's
// parent, and the generated MCP configuration under dir, and generates
// the run's public nonce and reply token. The server records nothing
// but received nonces: no header, environment value, or arbitrary
// payload is ever written.
func geminiNewMCPFixture(t *testing.T, dir string) geminiMCPFixture {
	t.Helper()

	receiptPath := filepath.Join(dir, "mcp-receipt.txt")
	nonce := geminiNonce(t)
	reply := geminiRandomToken(t, "sortie-reply-")

	// The server speaks minimal JSON-RPC over stdio: initialize,
	// tools/list advertising the nonce-carrying probe tool, and
	// tools/call recording a conforming nonce before returning the
	// fixture's own reply token as text content. A request whose id
	// does not read as digits gets no reply at all, and a call
	// carrying no conforming nonce gets an isError result naming no
	// substring of the request.
	server := fmt.Sprintf(`receipt='%s'
while IFS= read -r line; do
  id=$(printf '%%s' "$line" | sed -n 's/.*"id":\([^,}]*\)[,}].*/\1/p')
  case "$id" in
    ''|*[!0-9]*)
      continue
      ;;
  esac
  case "$line" in
    *'"method":"initialize"'*)
      printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{\"tools\":{}},\"serverInfo\":{\"name\":\"%s\",\"version\":\"fixture\"}}}"
      ;;
    *'"method":"tools/list"'*)
      printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"tools\":[{\"name\":\"%s\",\"description\":\"qualification probe\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"nonce\":{\"type\":\"string\",\"description\":\"the nonce to record\"}},\"required\":[\"nonce\"]}}]}}"
      ;;
    *'"method":"tools/call"'*)
      value=$(printf '%%s' "$line" | sed -n 's/.*"nonce":"\(sortie-nonce-[0-9a-f]\{16\}\)".*/\1/p')
      if [ -n "$value" ]; then
        printf '%%s\n' "$value" >> "$receipt"
        printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"%s\"}]}}"
      else
        printf '%%s\n' "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":\"missing or malformed nonce argument\"}]}}"
      fi
      ;;
  esac
done
`, receiptPath, geminiMCPServerName, geminiMCPToolName, reply)
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
		Reply:       reply,
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
// and that turnConsumed reports the turn's notification stream carrying
// the server's own reply token rather than the nonce the caller sent;
// an advertisement or a tool_call output alone is never positive.
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

// geminiGradeMCPRow decides the tool-server row's grade, outcome, and
// detail together from one turn's evidence: the receipt the fixture
// server wrote, that turn's own event window, and the turn's error, if
// any. It delegates the both-halves rule to geminiGradeMCPDelivery,
// reads no file, and calls no runtime. Every detail it returns stays
// within qualification.DetailBound code points and carries no path, no
// token value, and no session identifier.
func geminiGradeMCPRow(fixture geminiMCPFixture, received []string, events []domain.AgentEvent, turnErr error) (qualification.Grade, qualification.Outcome, string) {
	if turnErr != nil {
		return qualification.GradeNotObserved, qualification.OutcomePrerequisiteFailed, "the tool-server turn did not complete"
	}

	receiptHasNonce := slices.Contains(received, fixture.Nonce)
	receiptEmpty := len(received) == 0
	consumed := geminiNotificationsCarry(events, fixture.Reply)
	permissionNoted := geminiPermissionRequested(events)
	usable := geminiGradeMCPDelivery(received, fixture.Nonce, consumed) == qualification.GradeUsable

	switch {
	case usable && !permissionNoted:
		return qualification.GradeUsable, qualification.OutcomePass,
			"the server received the nonce and the turn carried the server's reply; the probe tool is pre-authorized so no permission request is raised for it"
	case usable:
		return qualification.GradeUsable, qualification.OutcomePass,
			"the server received the nonce and the turn carried the server's reply; a permission request in the turn was raised for a call other than the probe tool"
	case receiptHasNonce:
		return qualification.GradeNotObserved, qualification.OutcomeNotObserved, "the server was called and the turn did not carry what it returned"
	case !receiptEmpty:
		return qualification.GradeNotObserved, qualification.OutcomeNotObserved, "the server recorded a value that is not this run's nonce"
	case permissionNoted:
		return qualification.GradeNotObserved, qualification.OutcomeNotObserved, "no tool call reached the server, and a permission request in this turn was refused"
	default:
		return qualification.GradeNotObserved, qualification.OutcomeNotObserved, "no tool call reached the server"
	}
}

// TestGeminiGradeMCPRow drives every arm of the tool-server row's
// grading table from constructed inputs alone, with no receipt file and
// no runtime. It confirms each arm returns exactly the grade and
// outcome its row names, that no two arms share a detail string, and
// that every detail stays within qualification.DetailBound code points
// and carries neither a path separator nor either fixture token.
func TestGeminiGradeMCPRow(t *testing.T) {
	t.Parallel()

	fixture := geminiMCPFixture{Nonce: "sortie-nonce-row-fixture", Reply: "sortie-reply-row-fixture"}
	replyEvent := domain.AgentEvent{Type: domain.EventNotification, Message: fixture.Reply}
	permissionRefused := domain.AgentEvent{Type: domain.EventNotification, Message: geminiPermissionNoticeStem}
	permissionUnanswered := domain.AgentEvent{Type: domain.EventNotification, Message: geminiPermissionUnansweredStem}

	tests := []struct {
		name        string
		received    []string
		events      []domain.AgentEvent
		turnErr     error
		wantGrade   qualification.Grade
		wantOutcome qualification.Outcome
	}{
		{
			name:        "an incomplete turn is not observed regardless of the other evidence",
			received:    []string{fixture.Nonce},
			events:      []domain.AgentEvent{replyEvent},
			turnErr:     fmt.Errorf("turn did not complete"),
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomePrerequisiteFailed,
		},
		{
			name:        "receipt plus reply with no permission noted grades usable",
			received:    []string{fixture.Nonce},
			events:      []domain.AgentEvent{replyEvent},
			wantGrade:   qualification.GradeUsable,
			wantOutcome: qualification.OutcomePass,
		},
		{
			name:        "receipt plus reply with a permission notice for another call still grades usable",
			received:    []string{fixture.Nonce},
			events:      []domain.AgentEvent{permissionRefused, replyEvent},
			wantGrade:   qualification.GradeUsable,
			wantOutcome: qualification.OutcomePass,
		},
		{
			name:        "receipt carries the nonce but the window never carried the reply",
			received:    []string{fixture.Nonce},
			events:      nil,
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeNotObserved,
		},
		{
			name:        "receipt carries a value that is not this run's nonce",
			received:    []string{"sortie-nonce-someone-elses-run"},
			events:      nil,
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeNotObserved,
		},
		{
			name:        "empty receipt with a permission notice means the client refused the call",
			received:    nil,
			events:      []domain.AgentEvent{permissionUnanswered},
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeNotObserved,
		},
		{
			name:        "empty receipt with no permission notice means no call reached the server",
			received:    nil,
			events:      nil,
			wantGrade:   qualification.GradeNotObserved,
			wantOutcome: qualification.OutcomeNotObserved,
		},
	}

	seenDetails := make(map[string]string)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			grade, outcome, detail := geminiGradeMCPRow(fixture, tt.received, tt.events, tt.turnErr)
			if grade != tt.wantGrade {
				t.Errorf("geminiGradeMCPRow() grade = %q, want %q", grade, tt.wantGrade)
			}
			if outcome != tt.wantOutcome {
				t.Errorf("geminiGradeMCPRow() outcome = %q, want %q", outcome, tt.wantOutcome)
			}
			if points := utf8.RuneCountInString(detail); points > qualification.DetailBound {
				t.Errorf("geminiGradeMCPRow() detail = %d code points, want at most %d", points, qualification.DetailBound)
			}
			if strings.ContainsAny(detail, "/\\") {
				t.Errorf("geminiGradeMCPRow() detail %q carries a path separator, want none", detail)
			}
			if strings.Contains(detail, fixture.Nonce) || strings.Contains(detail, fixture.Reply) {
				t.Errorf("geminiGradeMCPRow() detail %q carries a token value, want neither fixture token named", detail)
			}
		})
	}

	for _, tt := range tests {
		_, _, detail := geminiGradeMCPRow(fixture, tt.received, tt.events, tt.turnErr)
		if other, seen := seenDetails[detail]; seen {
			t.Errorf("arms %q and %q share the detail %q, want every arm's detail distinct", other, tt.name, detail)
		}
		seenDetails[detail] = tt.name
	}
}

// geminiDriveMCPFixture drives the fixture server over stdio and
// reports what it observed: whether the probe tool was advertised,
// whether the server received the nonce, and what the call returned.
type geminiMCPDriveResult struct {
	advertised    bool
	serverReceipt []string
	returnedText  string
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
		returnedText:  returned,
	}
}

// geminiMCPFixtureConn is a raw stdio connection to the fixture server,
// for driving a request whose id or arguments geminiDriveMCPFixture's
// fixed sequence cannot express, such as an id that does not read as
// digits.
type geminiMCPFixtureConn struct {
	stdin io.Writer
	lines <-chan []byte
}

// geminiOpenMCPFixtureConn starts the fixture server and returns a raw
// connection over its stdio pipes.
func geminiOpenMCPFixtureConn(t *testing.T, fixture geminiMCPFixture) geminiMCPFixtureConn {
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

	return geminiMCPFixtureConn{stdin: stdin, lines: lines}
}

// send writes one request line with id spliced in verbatim, so a test
// can drive an id that does not read as digits.
func (c geminiMCPFixtureConn) send(t *testing.T, id, method, params string) {
	t.Helper()
	if _, err := fmt.Fprintf(c.stdin, `{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`+"\n", id, method, params); err != nil {
		t.Fatalf("write %s request: %v", method, err)
	}
}

// awaitLine returns the next response line, failing t if none arrives
// within awaitTimeout.
func (c geminiMCPFixtureConn) awaitLine(t *testing.T) []byte {
	t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			t.Fatal("MCP fixture server stream ended with no more lines")
		}
		return line
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for a fixture server response")
		return nil
	}
}

// silentFor reports whether no response line arrives before d elapses.
// A short d keeps this negative assertion bounded rather than costing
// the package's whole awaitTimeout on every no-reply case.
func (c geminiMCPFixtureConn) silentFor(d time.Duration) bool {
	select {
	case <-c.lines:
		return false
	case <-time.After(d):
		return true
	}
}

// TestGeminiQualificationMCPFixtureReceipt confirms the positive
// tool-server fixture end to end: the generated configuration parses
// through the existing parseMCPServers path, the session-creation wire
// carries the declared stdio server, and delivery is graded usable only
// when the server received the generated nonce and the turn carried
// back the server's own reply token. Advertisement alone, or a
// tool_call output alone, is never a positive verdict, and a call
// carrying no conforming nonce or an unreadable request id gets no
// receipt.
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

	t.Run("receipt plus consumed reply token grades usable", func(t *testing.T) {
		t.Parallel()

		fixture := geminiNewMCPFixture(t, t.TempDir())
		drive := geminiDriveMCPFixture(t, fixture)

		if !drive.advertised {
			t.Error("tools/list did not advertise the probe tool")
		}
		if len(drive.serverReceipt) != 1 || drive.serverReceipt[0] != fixture.Nonce {
			t.Errorf("server receipt = %v, want exactly the generated nonce", drive.serverReceipt)
		}
		if drive.returnedText != fixture.Reply {
			t.Errorf("tool result = %q, want the fixture's reply token %q, not the nonce it was called with", drive.returnedText, fixture.Reply)
		}
		if fixture.Reply == fixture.Nonce {
			t.Fatalf("fixture reply %q equals the nonce, want two distinct generated tokens", fixture.Reply)
		}
		if strings.Contains(fixture.Reply, fixture.Nonce) || strings.Contains(fixture.Nonce, fixture.Reply) {
			t.Errorf("fixture reply %q and nonce %q, want neither a substring of the other", fixture.Reply, fixture.Nonce)
		}

		turnConsumed := geminiNotificationsCarry([]domain.AgentEvent{{Type: domain.EventNotification, Message: drive.returnedText}}, fixture.Reply)
		if got := geminiGradeMCPDelivery(drive.serverReceipt, fixture.Nonce, turnConsumed); got != qualification.GradeUsable {
			t.Errorf("geminiGradeMCPDelivery() = %s, want usable for receipt plus a turn that carried the reply token", got)
		}
	})

	t.Run("a missing or malformed nonce argument gets isError and leaves the receipt absent", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name      string
			arguments string
		}{
			{"missing nonce argument", `{}`},
			{"malformed nonce shape", `{"nonce":"not-the-right-shape"}`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				fixture := geminiNewMCPFixture(t, t.TempDir())
				conn := geminiOpenMCPFixtureConn(t, fixture)
				conn.send(t, "3", "tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, geminiMCPToolName, tt.arguments))

				var envelope struct {
					Result struct {
						IsError bool `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				if err := json.Unmarshal(conn.awaitLine(t), &envelope); err != nil {
					t.Fatalf("decode tools/call result: %v", err)
				}
				if !envelope.Result.IsError {
					t.Error("result.isError = false, want true for a call with no conforming nonce")
				}
				if len(envelope.Result.Content) == 0 || envelope.Result.Content[0].Text == "" {
					t.Error("error result carries no content text, want a fixed literal")
				} else if strings.Contains(envelope.Result.Content[0].Text, tt.arguments) {
					t.Errorf("error result text %q copies the request, want a fixed literal naming no request substring", envelope.Result.Content[0].Text)
				}
				if received := geminiReadMCPReceipt(t, fixture); received != nil {
					t.Errorf("receipt after %s = %v, want none", tt.name, received)
				}
			})
		}
	})

	t.Run("a request whose id does not read as digits gets no reply at all", func(t *testing.T) {
		t.Parallel()

		fixture := geminiNewMCPFixture(t, t.TempDir())
		conn := geminiOpenMCPFixtureConn(t, fixture)

		conn.send(t, `"not-a-digit-id"`, "tools/call", fmt.Sprintf(`{"name":%q,"arguments":{"nonce":%q}}`, geminiMCPToolName, fixture.Nonce))
		if !conn.silentFor(200 * time.Millisecond) {
			t.Error("server replied to a request whose id does not read as digits, want silence")
		}
		if received := geminiReadMCPReceipt(t, fixture); received != nil {
			t.Errorf("receipt after a non-digit request id = %v, want none", received)
		}

		conn.send(t, "9", "initialize", `{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"sortie","version":"fixture"}}`)
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(conn.awaitLine(t), &envelope); err != nil {
			t.Fatalf("decode initialize response: %v", err)
		}
		if strings.TrimSpace(string(envelope.ID)) != "9" {
			t.Errorf("initialize response id = %s, want 9 (the server still answers after silently skipping the earlier request)", envelope.ID)
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
