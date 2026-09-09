//go:build unix

package probe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/qualification"
)

const (
	// toolServerName is the declared MCP server's own name, used both
	// in the generated configuration and reported in its handshake.
	toolServerName = "sortie-probe-tools"

	// probeToolName is the single tool the induced server declares.
	probeToolName = "sortie_probe_tool"

	// toolInductionTurnBound bounds one tool-server or
	// permission-handling induction turn.
	toolInductionTurnBound = 3 * time.Minute

	// permissionAcceptedNotice and permissionNoOptionNotice mirror the
	// two operator-facing notice strings
	// internal/agent/agentcore.DecideHumanRequest emits for a
	// permission request under this client's own unattended refusal
	// posture. This file's own import list stays confined to
	// internal/qualification, internal/agent/clientprotocol, and
	// internal/domain, so the two strings are duplicated here rather
	// than referenced; a change to either constant on the adapter side
	// is a change this file must follow.
	permissionAcceptedNotice = "refused a permission request because this run is unattended and no one can approve it"
	permissionNoOptionNotice = "the agent needs a permission this unattended run cannot grant"
)

// mcpToolServerScript is a fake MCP stdio server that records every
// tools/call it receives to callRecordPath, one line per call, and
// otherwise answers only what a session/new declaring it needs:
// initialize, tools/list, and tools/call. It follows
// session_unix_test.go's mcpHandshakeScript shape, adapted from the
// Agent Client Protocol's own wire to the Model Context Protocol's.
func mcpToolServerScript(callRecordPath string) string {
	return `record='` + callRecordPath + `'
extract_id() {
  printf '%s' "$1" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p'
}
while IFS= read -r line; do
  id=$(extract_id "$line")
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"` + toolServerName + `","version":"0.0.0"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"` + probeToolName + `","description":"records that the qualification probe induced a call","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      printf '%s\n' "$line" >> "$record"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}\n' "$id"
      ;;
  esac
done
`
}

// writeExecutableScript writes an executable POSIX shell script under
// dir and returns its path.
func writeExecutableScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o700); err != nil { //nolint:gosec // the induction script is a test fixture the probe writes and launches under its own temp directory
		t.Fatalf("write induction script %s: %v", path, err)
	}
	return path
}

// writeToolServerMCPConfig writes a generated MCP configuration
// declaring one stdio server at scriptPath, in the shape
// internal/agent/clientprotocol/mcp_test.go's writeMCPConfig builds,
// and returns its path.
func writeToolServerMCPConfig(t *testing.T, dir, scriptPath string) string {
	t.Helper()
	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{%q:{"type":"stdio","command":%q,"args":[]}}}`, toolServerName, scriptPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write induction MCP configuration %s: %v", path, err)
	}
	return path
}

// fileHasContent reports whether path exists and is non-empty. A
// missing file is not an error: the induction's own server never
// created it when no call reached it.
func fileHasContent(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Size() > 0, nil
}

// startInductionSession launches one protocol session with argv as the
// runtime's own arguments and mcpConfigPath declaring the induced tool
// server, in a fresh isolated workspace. It registers the session's own
// teardown, including the step 3.1 process-group-absence assertion,
// before returning.
func startInductionSession(t *testing.T, coords Coordinates, argv []string, mcpConfigPath string) (domain.AgentAdapter, domain.Session, error) {
	t.Helper()

	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		return nil, domain.Session{}, fmt.Errorf("construct the induction adapter: %w", err)
	}

	fullCommand := append([]string{coords.CommandPath}, argv...)
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        strings.Join(fullCommand, " "),
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  int(toolInductionTurnBound / time.Millisecond),
			StallTimeoutMS: 60000,
		},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		return adapter, domain.Session{}, err
	}
	t.Cleanup(func() {
		if err := adapter.StopSession(context.Background(), session); err != nil {
			t.Errorf("stop induction session: %v", err)
		}
		assertSessionGroupAbsent(t, session)
	})
	return adapter, session, nil
}

// induceToolServerCall drives one turn that can only be answered by
// calling the single tool a declared stdio server offers, and grades
// the row from that server's own call record: never from the wire, and
// never from delivery alone.
func induceToolServerCall(t *testing.T, coords Coordinates) (qualification.Grade, string) {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := filepath.Join(dir, "calls.jsonl")
	scriptPath := writeExecutableScript(t, dir, "mcp-server.sh", mcpToolServerScript(callRecordPath))
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)

	argv, err := coords.Profile.EntryArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("tool-server induction could not resolve the protocol entry point: %v", err)
	}

	adapter, session, err := startInductionSession(t, coords, argv, mcpConfigPath)
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("tool-server induction session failed to start: %v", err)
	}

	var calledAnyTool bool
	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: fmt.Sprintf("Call the tool named %s now, with no arguments, then reply with exactly SORTIE_PROBE_DONE.", probeToolName),
		OnEvent: func(ev domain.AgentEvent) {
			if ev.Type == domain.EventToolResult {
				calledAnyTool = true
			}
		},
	})
	if runErr != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("tool-server induction turn did not complete: %v", runErr)
	}

	recorded, err := fileHasContent(callRecordPath)
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("tool-server induction could not read the server's own call record: %v", err)
	}
	switch {
	case recorded:
		return qualification.GradeUsable, "the declared tool server recorded a call and the turn consumed it"
	case calledAnyTool:
		// The runtime reached a tool but not this one, which separates a
		// delivery that never arrived from one the runtime routed
		// elsewhere. A gap that names only the missing record leaves the
		// two indistinguishable to whoever reads the tracked artifact.
		return qualification.GradeGap, "the turn completed a tool call that never reached the declared server"
	default:
		return qualification.GradeGap, "the turn completed without calling any tool, so the declared server was never reached"
	}
}

// permissionAskingArgs reads the protocol entry point's own stated
// asking posture. It is read rather than derived from the graded
// launch: which element of an argument vector is the posture switch is
// not recoverable from the vector, and a profile whose trailing element
// is the protocol switch would be relaunched out of protocol mode
// entirely by any positional rule. A profile that states no asking
// posture leaves the row unmeasured, which is the honest outcome, and
// nothing here reads which runtime it launches.
func permissionAskingArgs(coords Coordinates) ([]string, error) {
	argv, ok := coords.Profile.AskingArgs(qualification.SurfaceProtocol, coords.Model, "", "")
	if !ok {
		return nil, fmt.Errorf("runtime %s's protocol entry point states no asking posture", coords.Profile.RuntimeID)
	}
	return argv, nil
}

// containsNotification reports whether events carries a notification
// whose message contains substr.
func containsNotification(events []domain.AgentEvent, substr string) bool {
	for _, ev := range events {
		if ev.Type == domain.EventNotification && strings.Contains(ev.Message, substr) {
			return true
		}
	}
	return false
}

// inducePermissionRequest reuses induceToolServerCall's own server and
// forcing prompt under the posture permissionAskingArgs restores, and
// grades the row from whether the runtime raised a permission request
// and whether the client's own refusal answered it, per this
// capability's own mapping: an absent request is unmeasured, never
// usable.
func inducePermissionRequest(t *testing.T, coords Coordinates) (qualification.Grade, string) {
	t.Helper()

	dir := t.TempDir()
	callRecordPath := filepath.Join(dir, "calls.jsonl")
	scriptPath := writeExecutableScript(t, dir, "mcp-server.sh", mcpToolServerScript(callRecordPath))
	mcpConfigPath := writeToolServerMCPConfig(t, dir, scriptPath)

	argv, err := permissionAskingArgs(coords)
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("permission induction could not resolve an asking posture: %v", err)
	}

	adapter, session, err := startInductionSession(t, coords, argv, mcpConfigPath)
	if err != nil {
		return qualification.GradeNotObserved, fmt.Sprintf("permission induction session failed to start: %v", err)
	}

	var events []domain.AgentEvent
	var calledAnyTool bool
	_, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: fmt.Sprintf("Call the tool named %s now, with no arguments.", probeToolName),
		OnEvent: func(ev domain.AgentEvent) {
			events = append(events, ev)
			if ev.Type == domain.EventToolResult {
				calledAnyTool = true
			}
		},
	})

	switch {
	case containsNotification(events, permissionAcceptedNotice):
		return qualification.GradeUsable, "the runtime raised a permission request and the client's refusal answered it with none left pending"
	case containsNotification(events, permissionNoOptionNotice):
		return qualification.GradeGap, "the runtime raised a permission request but offered no refusing option to answer it with"
	case runErr != nil:
		return qualification.GradeNotObserved, fmt.Sprintf("permission induction turn did not complete and raised no visible request: %v", runErr)
	case !calledAnyTool:
		// Nothing asked for consent because nothing was attempted, which
		// is a different unmeasured state from a runtime that ran a tool
		// and asked no one.
		return qualification.GradeNotObserved, "the turn called no tool at all, so no permission request could arise to observe"
	default:
		return qualification.GradeNotObserved, "the runtime raised no permission request under a posture that should provoke one"
	}
}
