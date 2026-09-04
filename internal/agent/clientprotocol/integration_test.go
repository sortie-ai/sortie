// Integration tests for the Agent Client Protocol transport, driven
// against a real, locally installed protocol-speaking binary.
//
// Required environment variables:
//
//	SORTIE_CLIENTPROTOCOL_TEST=1     enable this suite
//	SORTIE_CLIENTPROTOCOL_COMMAND    the protocol-speaking binary's launch
//	                                  command, including whatever flag
//	                                  puts it into Agent Client Protocol
//	                                  mode (for example "copilot --acp" or
//	                                  "opencode acp")
//
// This suite names no default binary: the kind is generic, with no
// runtime of its own, and naming one would make a single vendor the
// implicit reference for it. Without SORTIE_CLIENTPROTOCOL_TEST=1, or
// without SORTIE_CLIENTPROTOCOL_COMMAND, every test in this file skips
// rather than failing.
//
// The runtime SORTIE_CLIENTPROTOCOL_COMMAND names must complete a turn,
// answer it with a stop reason of end_turn, stream at least one text
// message chunk, and report agentInfo in its initialize response. The
// example commands above illustrate command shape only; neither is
// known to satisfy that contract, since both were driven through the
// handshake alone.
//
// Run:
//
//	SORTIE_CLIENTPROTOCOL_TEST=1 SORTIE_CLIENTPROTOCOL_COMMAND="..." \
//	  go test ./internal/agent/clientprotocol/... -run Integration
package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

type liveProtocolCapture struct {
	clientPath string
	agentPath  string
	expect     captureExpectation
	events     func() []domain.AgentEvent // nil for expectHandshakeOnly
}

// --- Integration test helpers ---

// skipUnlessClientProtocolIntegration skips the current test unless
// SORTIE_CLIENTPROTOCOL_TEST=1 is set and SORTIE_CLIENTPROTOCOL_COMMAND
// names a launch command. The command variable is a fixture coordinate
// rather than a second gate: its own absence skips exactly like an
// absent credential does in the sibling agent packages' suites, never
// failing.
func skipUnlessClientProtocolIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CLIENTPROTOCOL_TEST") != "1" {
		t.Skip("skipping Agent Client Protocol integration test: set SORTIE_CLIENTPROTOCOL_TEST=1 to enable")
	}
	if os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND") == "" {
		t.Skip("skipping Agent Client Protocol integration test: SORTIE_CLIENTPROTOCOL_COMMAND must name a protocol-speaking binary and its launch flags")
	}
}

// integrationAgentConfig returns the [domain.AgentConfig] every test in
// this suite launches with. Timeouts are deliberately generous to
// accommodate a real model's latency.
func integrationAgentConfig(t *testing.T, expect captureExpectation, events func() []domain.AgentEvent) domain.AgentConfig {
	t.Helper()
	dir := t.TempDir()
	capture := liveProtocolCapture{
		clientPath: filepath.Join(dir, "client.jsonl"),
		agentPath:  filepath.Join(dir, "agent.jsonl"),
		expect:     expect,
		events:     events,
	}
	wrapperPath := filepath.Join(dir, "capture.sh")
	wrapper := fmt.Sprintf("#!/bin/sh\nset -eu\ntee %q | \"$@\" | tee %q\n", capture.clientPath, capture.agentPath)
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("integrationAgentConfig() write capture wrapper: %v", err)
	}
	t.Cleanup(func() { assertLiveProtocolConformance(t, capture) })

	return domain.AgentConfig{
		Command:       wrapperPath + " " + os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND"),
		TurnTimeoutMS: 300000,
		ReadTimeoutMS: 30000,
	}
}

func capturedJSONLines(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // path is created under this test's temp directory
	if err != nil {
		t.Fatalf("capturedJSONLines(%q) error = %v", path, err)
	}
	var lines [][]byte
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			lines = append(lines, []byte(line))
		}
	}
	return lines
}

func jsonRPCIDKey(raw json.RawMessage) string {
	return string(raw)
}

func assertLiveProtocolConformance(t *testing.T, capture liveProtocolCapture) {
	t.Helper()

	requestMethods := make(map[string]string)
	for _, line := range capturedJSONLines(t, capture.clientPath) {
		var header wireHeader
		if err := json.Unmarshal(line, &header); err != nil {
			t.Errorf("decode captured client message %q: %v", line, err)
			continue
		}
		if header.Method != "" && len(header.ID) != 0 {
			requestMethods[jsonRPCIDKey(header.ID)] = header.Method
		}
	}

	defs := loadSchemaDefs(t, schemaAssetsDir)
	var undeclared []string
	for i, line := range capturedJSONLines(t, capture.agentPath) {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params any             `json:"params"`
			Result any             `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Errorf("decode live agent message %d = %q, want JSON-RPC: %v", i, line, err)
			continue
		}

		var defName string
		var body any
		switch envelope.Method {
		case methodSessionUpdate:
			defName, body = "SessionNotification", envelope.Params
		case methodSessionRequestPermission:
			defName, body = "RequestPermissionRequest", envelope.Params
		case "":
			if envelope.Error != nil {
				t.Logf("live agent response %d carried a JSON-RPC error; no success body to validate", i)
				continue
			}
			switch requestMethods[jsonRPCIDKey(envelope.ID)] {
			case methodInitialize:
				defName = "InitializeResponse"
			case methodSessionNew:
				defName = "NewSessionResponse"
			case methodSessionLoad:
				defName = "LoadSessionResponse"
			case methodSessionResume:
				defName = "ResumeSessionResponse"
			case methodSessionPrompt:
				defName = "PromptResponse"
			default:
				undeclared = append(undeclared, fmt.Sprintf("message[%d].response", i))
				continue
			}
			body = envelope.Result
		default:
			undeclared = append(undeclared, fmt.Sprintf("message[%d].method(%s)", i, envelope.Method))
			continue
		}

		violations, messageUndeclared := checkWeak(defs, defName, body)
		for _, violation := range violations {
			t.Errorf("live agent message %d: %s", i, violation)
		}
		undeclared = append(undeclared, messageUndeclared...)
	}
	t.Logf("live agent undeclared properties: %v", undeclared)

	var events []domain.AgentEvent
	if capture.events != nil {
		events = capture.events()
	}
	shapeViolations, observed := recordedShapeViolations(capturedJSONLines(t, capture.clientPath), capturedJSONLines(t, capture.agentPath), events, capture.expect)
	for _, v := range shapeViolations {
		t.Errorf("live shape violation: %s", v)
	}
	t.Logf("live shape observation: %+v", observed)
}

// gitInitWorkspace creates a temp directory and runs git init inside
// it, matching the sibling agent packages' own integration fixture:
// several protocol-speaking runtimes expect a git repository by
// default.
func gitInitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitInitWorkspace: git init: %v\n%s", err, out)
	}
	return dir
}

// mustNewClientProtocolAdapter constructs a *ClientProtocolAdapter or
// fails the test immediately.
func mustNewClientProtocolAdapter(t *testing.T) *ClientProtocolAdapter {
	t.Helper()
	a, err := NewClientProtocolAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter: %v", err)
	}
	return a.(*ClientProtocolAdapter)
}

// mustStartClientProtocolSession calls StartSession with the standard
// integration config and registers a StopSession cleanup. It fails the
// test immediately on error.
func mustStartClientProtocolSession(t *testing.T, ctx context.Context, adapter *ClientProtocolAdapter, workspace string, expect captureExpectation, events func() []domain.AgentEvent) domain.Session {
	t.Helper()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expect, events),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })
	return session
}

// makeEventCollector returns an OnEvent callback and a snapshot
// function. The snapshot function returns a copy of every event
// collected so far and is safe to call from any goroutine.
func makeEventCollector(t *testing.T) (onEvent func(domain.AgentEvent), collected func() []domain.AgentEvent) {
	t.Helper()
	var mu sync.Mutex
	var events []domain.AgentEvent
	onEvent = func(e domain.AgentEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	collected = func() []domain.AgentEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]domain.AgentEvent, len(events))
		copy(out, events)
		return out
	}
	return onEvent, collected
}

// assertContainsEventType fails t when no event in events has the
// given type.
func assertContainsEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			return
		}
	}
	types := make([]domain.AgentEventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	t.Errorf("expected event type %q not found; got types: %v", eventType, types)
}

// --- Integration test functions ---

// TestIntegration_StartSession verifies that StartSession returns a
// populated Session with a non-empty session id and process PID.
func TestIntegration_StartSession(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expectHandshakeOnly, nil),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if session.ID == "" {
		t.Error("session.ID is empty; expected a non-empty session id from session/new")
	}
	if session.AgentPID == "" {
		t.Error("session.AgentPID is empty; expected the PID of the launched subprocess")
	}
	if session.Internal == nil {
		t.Error("session.Internal is nil")
	}
}

// TestIntegration_StopSession verifies that StopSession terminates the
// subprocess cleanly when called after a successful StartSession but
// before any RunTurn, and that a second call is idempotent.
func TestIntegration_StopSession(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expectHandshakeOnly, nil),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession (second call): %v", err)
	}
}

// TestIntegration_RunTurnWithPermissionContinuation drives one full
// session: start a session, run a turn whose prompt asks the model to
// write and then read back a file (an action a protocol-speaking
// runtime's default posture typically pauses on for approval),
// observe the normalized events, and stop the session. The
// adapter's own posture answers any session/request_permission it
// receives by selecting a refusing option and letting the turn
// continue, per this piece's refusal-reply design; this test observes
// that continuation when the runtime asked, and reports plainly when
// it did not, since a live runtime's default approval posture is
// outside this suite's control.
func TestIntegration_RunTurnWithPermissionContinuation(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)
	targetFile := filepath.Join(workspace, "greeting.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	onEvent, collected := makeEventCollector(t)
	session := mustStartClientProtocolSession(t, ctx, adapter, workspace, expectToolForcingTurn, collected)

	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: fmt.Sprintf(
			"Create a file at %s containing exactly the text: hello from sortie. "+
				"After writing it, read the file back and report exactly what it contains.",
			targetFile,
		),
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	events := collected()
	t.Logf("received %d events, exit reason: %q", len(events), result.ExitReason)

	if result.SessionID != session.ID {
		t.Errorf("TurnResult.SessionID = %q, want %q", result.SessionID, session.ID)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	assertContainsEventType(t, events, domain.EventSessionStarted)

	var sawPermissionContinuation bool
	for _, e := range events {
		if e.Type == domain.EventNotification && strings.Contains(e.Message, "refused a permission request") {
			sawPermissionContinuation = true
			break
		}
	}
	if sawPermissionContinuation {
		t.Log("observed a session/request_permission the adapter refused, and the turn continued past it to completion")
	} else {
		t.Log("no session/request_permission was observed on this run; this runtime's default approval posture may not require one for this prompt")
	}
}

// TestIntegration_SessionContinuation drives one session through a
// full turn, stops it, then starts a second session against the same
// workspace naming the first session's identifier as ResumeSessionID.
// It reads the outcome through the once-per-session capability notice
// the second session's first turn emits: the notice names the session
// continuation label only when the entry was lowered, so its absence
// is the positive confirmation and its presence is the clean,
// non-failing fallback either way. This runtime's own
// support for continuation is not asserted either way; whichever
// branch fires, a confirmed continuation must return the identifier
// the first session held, and an unconfirmed one must still complete
// its turn.
func TestIntegration_SessionContinuation(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	onEvent1, collected1 := makeEventCollector(t)
	firstSession := mustStartClientProtocolSession(t, ctx, adapter, workspace, expectCompletedTurn, collected1)

	if _, err := adapter.RunTurn(ctx, firstSession, domain.RunTurnParams{
		Prompt:  "Reply with exactly one word: acknowledged.",
		OnEvent: onEvent1,
	}); err != nil {
		t.Fatalf("first session RunTurn: %v", err)
	}

	if err := adapter.StopSession(ctx, firstSession); err != nil {
		t.Fatalf("StopSession (first session): %v", err)
	}

	onEvent2, collected2 := makeEventCollector(t)
	secondSession, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     integrationAgentConfig(t, expectCompletedTurn, collected2),
		ResumeSessionID: firstSession.ID,
	})
	if err != nil {
		t.Fatalf("StartSession (continuation): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), secondSession) })

	if _, err := adapter.RunTurn(ctx, secondSession, domain.RunTurnParams{
		Prompt:  "Reply with exactly one word: acknowledged.",
		OnEvent: onEvent2,
	}); err != nil {
		t.Fatalf("second session RunTurn: %v", err)
	}

	notice := firstNotification(t, collected2())
	confirmed := !strings.Contains(notice.Message, capabilityLabelSessionContinuation)
	if confirmed {
		t.Log("session continuation was confirmed by this runtime")
		if secondSession.ID != firstSession.ID {
			t.Errorf("second session id = %q, want %q: a confirmed continuation must return the identifier it actually loaded or resumed", secondSession.ID, firstSession.ID)
		}
	} else {
		t.Log("session continuation was not confirmed by this runtime; the entry was lowered and the run fell back to a fresh session cleanly")
	}
}
