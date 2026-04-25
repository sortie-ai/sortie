//go:build unix

package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// nopWriteCloser is an io.WriteCloser that discards all writes.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// loadFixture reads testdata/<name> and returns its bytes.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	return data
}

// makeTestState builds a sessionState backed by in-memory pipes, safe
// for use in RunTurn and handleToolCall unit tests that do not launch a
// real subprocess.
func makeTestState(fixtureData []byte) *sessionState {
	state := &sessionState{
		threadID:   "thread-001",
		target:     agentcore.LaunchTarget{WorkspacePath: "/tmp"},
		waitCh:     make(chan struct{}),
		stdin:      nopWriteCloser{},
		stdout:     io.NopCloser(bytes.NewReader(nil)),
		msgCh:      make(chan parsedMessage, 16),
		readerDone: make(chan struct{}),
		stopCh:     make(chan struct{}),
	}

	go func() {
		defer close(state.readerDone)
		defer close(state.msgCh)
		scanner := bufio.NewScanner(bytes.NewReader(fixtureData))
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for scanner.Scan() {
			msg := parseMessage(scanner.Bytes())
			select {
			case state.msgCh <- msg:
			case <-state.stopCh:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case state.msgCh <- parsedMessage{Err: err}:
			case <-state.stopCh:
			}
		}
	}()

	return state
}

// fakeSession wraps state in a domain.Session suitable for RunTurn.
func fakeSession(state *sessionState) domain.Session {
	return domain.Session{
		ID:       state.threadID,
		AgentPID: "12345",
		Internal: state,
	}
}

// collectEvents is an OnEvent callback that appends to a slice.
func collectEvents(events *[]domain.AgentEvent) func(domain.AgentEvent) {
	var mu sync.Mutex
	return func(e domain.AgentEvent) {
		mu.Lock()
		*events = append(*events, e)
		mu.Unlock()
	}
}

// firstEventOfType returns the first event with the given type, or the
// zero value if none was found.
func firstEventOfType(events []domain.AgentEvent, t domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == t {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

func TestRunTurn_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	session := domain.Session{Internal: "not-a-session-state"}
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_SuccessfulTurn(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}
	if result.SessionID != "thread-001" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "thread-001")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event, none found")
	}
}

func TestRunTurn_FirstTurnEmitsSessionStarted(t *testing.T) {
	t.Parallel()

	// turnCount=0 → incremented to 1 inside RunTurn → EventSessionStarted.
	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "hello",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventSessionStarted)
	if !ok {
		t.Fatal("expected EventSessionStarted on first turn, not found")
	}
	if e.SessionID != "thread-001" {
		t.Errorf("EventSessionStarted.SessionID = %q, want %q", e.SessionID, "thread-001")
	}
}

func TestRunTurn_SubsequentTurnEmitsNotification(t *testing.T) {
	t.Parallel()

	// Pre-set turnCount=1 so the adapter sees this as the second turn.
	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	state.turnCount = 1
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "continue",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if _, ok := firstEventOfType(events, domain.EventSessionStarted); ok {
		t.Error("did not expect EventSessionStarted on subsequent turn")
	}
}

func TestRunTurn_FailedTurnContextWindowExceeded(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_failed.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "do something",
		OnEvent: collectEvents(&events),
	})

	if err == nil {
		t.Fatal("RunTurn() expected error, got nil")
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %v, want %v", result.ExitReason, domain.EventTurnFailed)
	}
	if _, ok := firstEventOfType(events, domain.EventTurnFailed); !ok {
		t.Error("expected EventTurnFailed event, none found")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage event on failed turn, none found")
	}
}

func TestRunTurn_StdoutClosedBeforeTurnCompleted(t *testing.T) {
	t.Parallel()

	// Only the turn/start response — no turn/completed — so the
	// background goroutine closes msgCh before turn/completed arrives.
	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{}}\n"
	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_StdoutEOFBeforeTurnStartResponse(t *testing.T) {
	t.Parallel()

	// Empty fixture: msgCh closes before any turn/start response arrives.
	// Tests the !ok path in the session-scoped response-wait loop.
	state := makeTestState(nil)
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_TurnStartErrorResponse(t *testing.T) {
	t.Parallel()

	// turn/start response carries an error — RunTurn should return ErrTurnFailed.
	fixture := "{\"id\":1,\"error\":{\"code\":-32000,\"message\":\"thread not found\"}}\n"
	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrTurnFailed)
}

func TestRunTurn_CancelledContextReturnsError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	state := makeTestState(loadFixture(t, "runturn_success.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	_, err := adapter.RunTurn(ctx, fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(domain.AgentEvent) {},
	})
	// readResponse returns context.Canceled → wrapped in ErrPortExit.
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

func TestRunTurn_ItemStartedAndCompletedEmitsToolResult(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_items.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "run command",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from item tracking, not found")
	}
	if e.ToolName != "ls -la" {
		t.Errorf("ToolResult.ToolName = %q, want %q", e.ToolName, "ls -la")
	}
}

func TestRunTurn_AgentMessageTextEmitsNotification(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_agent_message.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "explain",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// item/completed with agentMessage type and non-empty text emits EventNotification.
	notifs := 0
	for _, e := range events {
		if e.Type == domain.EventNotification {
			notifs++
		}
	}
	if notifs == 0 {
		t.Error("expected at least one EventNotification for agentMessage text, found none")
	}
}

func TestRunTurn_MiscNotifications(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_misc_notifications.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// some/unknown/method → EventOtherMessage
	if _, ok := firstEventOfType(events, domain.EventOtherMessage); !ok {
		t.Error("expected EventOtherMessage for unknown notification method, not found")
	}
	// turn/plan/updated → EventNotification
	found := false
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == "plan updated" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'plan updated' EventNotification, not found")
	}
}

func TestRunTurn_ToolCallWithNilRegistry(t *testing.T) {
	t.Parallel()

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{}) // no tool_registry

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventUnsupportedToolCall)
	if !ok {
		t.Fatal("expected EventUnsupportedToolCall with nil registry, not found")
	}
	if e.ToolName != "create_issue" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "create_issue")
	}
}

func TestRunTurn_ToolCallWithRegisteredTool(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:   "create_issue",
		result: json.RawMessage(`{"id":"123"}`),
	})

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from registered tool, not found")
	}
	if e.ToolError {
		t.Error("EventToolResult.ToolError = true, want false")
	}
	if e.ToolName != "create_issue" {
		t.Errorf("EventToolResult.ToolName = %q, want %q", e.ToolName, "create_issue")
	}
}

func TestRunTurn_ToolCallWithToolError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:    "create_issue",
		execErr: errors.New("tracker unavailable"),
	})

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult from failed tool, not found")
	}
	if !e.ToolError {
		t.Error("EventToolResult.ToolError = false, want true")
	}
}

func TestRunTurn_ToolCallToolNotInRegistry(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	// Registry has no "create_issue" tool.

	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	if _, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if _, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall for unregistered tool, not found")
	}
}

// --- handleToolCall direct tests ---

func TestHandleToolCall_InvalidParams(t *testing.T) {
	t.Parallel()

	a, _ := NewCodexAdapter(map[string]any{})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 42},
		Notification: rpcNotification{
			Method: "item/tool/call",
			Params: json.RawMessage(`not-valid-json`),
		},
	}

	// Should not panic; writes an error response to stdin (discarded).
	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	// Invalid params: no event emitted (returns early after sendResponse).
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func TestHandleToolCall_NilRegistryEmitsUnsupported(t *testing.T) {
	t.Parallel()

	a, _ := NewCodexAdapter(map[string]any{})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{}}`),
		},
	}

	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	if e, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall with nil registry")
	} else if e.ToolName != "my_tool" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "my_tool")
	}
}

func TestHandleToolCall_ToolNotFound(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"unknown_tool","arguments":{}}`),
		},
	}

	evt := adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	if evt != nil {
		events = append(events, *evt)
	}
	for e := range toolEventCh {
		events = append(events, e)
	}

	if _, ok := firstEventOfType(events, domain.EventUnsupportedToolCall); !ok {
		t.Fatal("expected EventUnsupportedToolCall for unregistered tool")
	}
}

func TestHandleToolCall_ToolSuccess(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "my_tool", result: json.RawMessage(`"ok"`)})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 7},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{"x":1}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult on success")
	}
	if e.ToolError {
		t.Error("EventToolResult.ToolError = true, want false")
	}
	if e.ToolName != "my_tool" {
		t.Errorf("ToolName = %q, want %q", e.ToolName, "my_tool")
	}
}

func TestHandleToolCall_ToolError(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "my_tool", execErr: errors.New("service down")})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 7},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"my_tool","arguments":{}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	e, ok := firstEventOfType(events, domain.EventToolResult)
	if !ok {
		t.Fatal("expected EventToolResult on tool error")
	}
	if !e.ToolError {
		t.Error("EventToolResult.ToolError = false, want true")
	}
	if e.Message != "service down" {
		t.Errorf("Message = %q, want %q", e.Message, "service down")
	}
}

// --- StopSession ---

func TestStopSession_InvalidInternalType(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: "wrong"})
	if err == nil {
		t.Fatal("StopSession() expected error for wrong internal type, got nil")
	}
}

func TestRunTurn_MultiTurnNoRace(t *testing.T) {
	t.Parallel()

	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-001\"}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"completed\"},\"usage\":{\"input_tokens\":100,\"output_tokens\":50,\"cached_input_tokens\":10}}}\n" +
		"{\"id\":2,\"result\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-002\"}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-002\",\"status\":\"completed\"},\"usage\":{\"input_tokens\":200,\"output_tokens\":100,\"cached_input_tokens\":20}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(1) ExitReason = %v, want %v", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(2) error = %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(2) ExitReason = %v, want %v", result2.ExitReason, domain.EventTurnCompleted)
	}
}

func TestRunTurn_StdoutEOFBetweenTurns(t *testing.T) {
	t.Parallel()

	// One complete turn in the fixture. After the first RunTurn drains the
	// channel, the reader goroutine closes msgCh on EOF. A second RunTurn
	// call receives !ok immediately and returns ErrPortExit — the
	// "stdout EOF between turns" behavior introduced by the session-scoped
	// reader refactoring (previously undetected).
	fixture := "{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"starting\"}}}\n" +
		"{\"method\":\"turn/started\",\"params\":{\"turnId\":\"turn-001\"}}\n" +
		"{\"method\":\"turn/completed\",\"params\":{\"turn\":{\"id\":\"turn-001\",\"status\":\"completed\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"cached_input_tokens\":0}}}\n"

	state := makeTestState([]byte(fixture))
	adapter, _ := NewCodexAdapter(map[string]any{})
	session := fakeSession(state)

	if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first turn",
		OnEvent: func(domain.AgentEvent) {},
	}); err != nil {
		t.Fatalf("RunTurn(1) unexpected error: %v", err)
	}

	// After the first turn, the fixture is exhausted and msgCh is closed.
	// The second call must return ErrPortExit immediately.
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "second turn",
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestStopSession_NilState(t *testing.T) {
	t.Parallel()

	// State with nil proc and nil waitCh — StopSession should return nil.
	state := &sessionState{
		stdin:  nopWriteCloser{},
		waitCh: nil,
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}
}

func TestStopSession_WithActiveReaderGoroutine(t *testing.T) {
	t.Parallel()

	// Provide more messages than the channel buffer (16) so the reader
	// goroutine is blocked on a channel send when StopSession closes stopCh.
	line := []byte("{\"method\":\"turn/started\",\"params\":{}}\n")
	state := makeTestState(bytes.Repeat(line, 20))
	// Simulate the subprocess having already exited so waitCh does not block.
	close(state.waitCh)

	adapter, _ := NewCodexAdapter(map[string]any{})
	err := adapter.StopSession(context.Background(), domain.Session{Internal: state})
	if err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	// StopSession waits on readerDone internally; it must be closed on return.
	select {
	case <-state.readerDone:
		// OK
	default:
		t.Error("readerDone should be closed after StopSession")
	}
}

// --- fakeTool for tool registry tests ---

type fakeTool struct {
	name    string
	result  json.RawMessage
	execErr error
	delay   time.Duration
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return "fake tool for testing" }
func (f *fakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *fakeTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.result, f.execErr
}

// --- Race-detection tests ---

func TestHandleToolCall_EventsSerialized(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{name: "slow_tool", result: json.RawMessage(`"ok"`), delay: time.Millisecond})
	a, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})
	adapter := a.(*CodexAdapter)
	state := makeTestState(nil)
	var wg sync.WaitGroup
	toolEventCh := make(chan domain.AgentEvent, 8)

	msg := parsedMessage{
		IsNotification: true,
		Response:       rpcResponse{ID: 1},
		Notification: rpcNotification{
			Params: json.RawMessage(`{"tool":"slow_tool","arguments":{}}`),
		},
	}

	adapter.handleToolCall(context.Background(), state, &wg, msg, toolEventCh, slog.Default())
	wg.Wait()
	close(toolEventCh)

	var events []domain.AgentEvent
	for evt := range toolEventCh {
		events = append(events, evt)
	}

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != domain.EventToolResult {
		t.Errorf("event type = %v, want EventToolResult", events[0].Type)
	}
}

func TestRunTurn_ToolCallEventSerialization(t *testing.T) {
	t.Parallel()

	reg := domain.NewToolRegistry()
	reg.Register(&fakeTool{
		name:   "create_issue",
		result: json.RawMessage(`{"id":"42"}`),
		delay:  time.Millisecond,
	})
	state := makeTestState(loadFixture(t, "runturn_tool_call.jsonl"))
	adapter, _ := NewCodexAdapter(map[string]any{"tool_registry": reg})

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), fakeSession(state), domain.RunTurnParams{
		Prompt:  "use tool",
		OnEvent: collectEvents(&events),
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %v, want EventTurnCompleted", result.ExitReason)
	}
	if _, ok := firstEventOfType(events, domain.EventToolResult); !ok {
		t.Error("expected EventToolResult, not found")
	}
	if _, ok := firstEventOfType(events, domain.EventTokenUsage); !ok {
		t.Error("expected EventTokenUsage, not found")
	}
}
