// Integration tests for the Codex CLI agent adapter.
//
// Required environment variables:
//
//	SORTIE_CODEX_TEST=1     enable this suite
//	CODEX_API_KEY           Codex API key for authentication
//
// Optional environment variables:
//
//	SORTIE_CODEX_COMMAND    override the default "codex app-server" binary command
//	SORTIE_CODEX_MODEL      override the default "gpt-5.4-nano" model
//
// Run:
//
//	SORTIE_CODEX_TEST=1 CODEX_API_KEY=... make test PKG=./internal/agent/codex/... RUN=Integration
package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- Integration test helpers ---

// skipUnlessCodexIntegration skips the current test when SORTIE_CODEX_TEST
// is not set to "1", so disabled integration tests are reported as skipped
// rather than silently passing.
func skipUnlessCodexIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CODEX_TEST") != "1" {
		t.Skip("skipping Codex integration test: set SORTIE_CODEX_TEST=1 to enable")
	}
	if os.Getenv("CODEX_API_KEY") == "" {
		t.Skip("skipping Codex integration test: CODEX_API_KEY must be set")
	}
}

// integrationConfig builds the adapter config map for integration tests.
func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.4-nano"
	}
	return map[string]any{
		"approval_policy": "never",
		"thread_sandbox":  "workspaceWrite",
		"model":           model,
	}
}

// integrationCommand returns the Codex CLI binary command from the
// SORTIE_CODEX_COMMAND environment variable, defaulting to "codex app-server".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_CODEX_COMMAND"); cmd != "" {
		return cmd
	}
	return "codex app-server"
}

// integrationAgentConfig returns the [domain.AgentConfig] used by
// integration tests. Timeouts are deliberately generous to accommodate
// API latency variance.
func integrationAgentConfig() domain.AgentConfig {
	return domain.AgentConfig{
		Command:       integrationCommand(),
		TurnTimeoutMS: 90000,
		ReadTimeoutMS: 30000,
	}
}

// gitInitWorkspace creates a temp directory and runs git init inside it.
// The absolute path is returned. The test fails immediately if git init
// fails, because the Codex app-server requires a git repository by default.
func gitInitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitInitWorkspace: git init: %v\n%s", err, out)
	}
	return dir
}

// assertContainsEventType asserts that at least one event in the slice
// has the given type.
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

// assertNoEventType asserts that no event in the slice has the given type.
func assertNoEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			t.Errorf("unexpected event type %q found with message: %q", eventType, e.Message)
			return
		}
	}
}

// requireAgentErrorKind asserts err is a *domain.AgentError with the given Kind.
func requireAgentErrorKind(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

// makeEventCollector returns an OnEvent callback and a snapshot function.
// The snapshot function returns a copy of all events collected so far
// and is safe to call from any goroutine.
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

// mustNewAdapter creates a CodexAdapter from integrationConfig or fails the
// test immediately.
func mustNewAdapter(t *testing.T) *CodexAdapter {
	t.Helper()
	a, err := NewCodexAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	return a.(*CodexAdapter)
}

// mustStartSession calls StartSession with the standard integration config and
// registers a StopSession cleanup. It fails the test immediately on error.
func mustStartSession(t *testing.T, ctx context.Context, adapter *CodexAdapter, workspace string) domain.Session {
	t.Helper()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })
	return session
}

// --- Integration test functions ---

// TestIntegration_StartSession verifies that StartSession returns a populated
// Session with a non-empty thread ID and process PID.
func TestIntegration_StartSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if session.ID == "" {
		t.Error("session.ID is empty; expected non-empty thread ID from app-server")
	}
	if session.AgentPID == "" {
		t.Error("session.AgentPID is empty; expected PID of the persistent subprocess")
	}
	if session.Internal == nil {
		t.Error("session.Internal is nil")
	}
}

// TestIntegration_StopSession verifies that StopSession terminates the
// persistent subprocess cleanly when called after a successful StartSession
// but before any RunTurn. This validates that the subprocess lifecycle is
// correctly managed at both ends.
func TestIntegration_StopSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// StopSession before any RunTurn must terminate the subprocess.
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}

	// A second StopSession call must be idempotent and not panic.
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession (second call): %v", err)
	}
}

// TestIntegration_StartSession_InvalidCommand verifies that StartSession
// returns a properly typed ErrAgentNotFound error when the agent binary
// does not exist on PATH.
func TestIntegration_StartSession_InvalidCommand(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)

	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}

	requireAgentErrorKind(t, err, domain.ErrAgentNotFound)
}

// TestIntegration_RunTurn executes a single turn and verifies the mandatory
// event sequence, token usage, and tool result correlation. A file-read
// prompt is used so the adapter emits at least one EventToolResult with a
// populated ToolName.
func TestIntegration_RunTurn(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "hello.txt"), []byte("Hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	session := mustStartSession(t, ctx, adapter, workspace)
	onEvent, collected := makeEventCollector(t)

	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Read the file hello.txt. Output EXACTLY the file content and absolutely nothing else. No preamble, no explanation.",
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	events := collected()
	t.Logf("received %d events, exit reason: %q", len(events), result.ExitReason)

	// Session ID must equal the thread ID established by StartSession.
	if result.SessionID != session.ID {
		t.Errorf("TurnResult.SessionID = %q, want %q", result.SessionID, session.ID)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if len(events) == 0 {
		t.Fatal("no events received via OnEvent")
	}

	assertContainsEventType(t, events, domain.EventSessionStarted)
	assertContainsEventType(t, events, domain.EventTurnCompleted)
	assertContainsEventType(t, events, domain.EventTokenUsage)
	assertNoEventType(t, events, domain.EventTurnFailed)
	assertNoEventType(t, events, domain.EventStartupFailed)

	// Token totals must be internally consistent if the app-server provides
	// usage data. Some app-server versions omit the usage field; log rather
	// than fail so the test remains useful across versions.
	if result.Usage.TotalTokens > 0 {
		if result.Usage.TotalTokens != result.Usage.InputTokens+result.Usage.OutputTokens {
			t.Errorf("TurnResult.Usage.TotalTokens = %d, want %d (input + output)",
				result.Usage.TotalTokens, result.Usage.InputTokens+result.Usage.OutputTokens)
		}
	} else {
		t.Log("token usage not provided by this app-server version (TotalTokens = 0)")
	}

	// The file-read prompt typically causes at least one commandExecution
	// or fileChange item, producing a correlated EventToolResult with a
	// populated ToolName. Some models may complete without tool use, so
	// log rather than fail.
	var foundToolResult bool
	for _, e := range events {
		if e.Type == domain.EventToolResult && e.ToolName != "" {
			foundToolResult = true
			if e.ToolDurationMS < 0 {
				t.Errorf("EventToolResult.ToolDurationMS = %d, want >= 0", e.ToolDurationMS)
			}
			break
		}
	}
	if !foundToolResult {
		t.Log("no EventToolResult with non-empty ToolName observed (model may have completed without tool use)")
	}
}

// TestIntegration_RunTurn_StopDuringTurn verifies that calling StopSession
// while RunTurn is blocked on the event stream causes RunTurn to unblock
// and return an error without deadlocking. This is the persistent subprocess
// equivalent of context cancellation: it tests the critical lifecycle
// invariant that StopSession always terminates an in-flight turn.
func TestIntegration_RunTurn_StopDuringTurn(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer outerCancel()

	session, err := adapter.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	type turnOutcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan turnOutcome, 1)

	go func() {
		r, e := adapter.RunTurn(outerCtx, session, domain.RunTurnParams{
			// A prompt that causes the model to start a long tool execution,
			// ensuring the turn is genuinely in-flight when we stop it.
			Prompt:  "Execute the shell command: sleep 30",
			OnEvent: func(_ domain.AgentEvent) {},
		})
		outcomeCh <- turnOutcome{result: r, err: e}
	}()

	// Give the turn time to start (turn/start sent, model begins processing).
	// 400ms is well above the turn/start round-trip latency.
	time.Sleep(400 * time.Millisecond)

	// StopSession must terminate the subprocess, unblocking RunTurn.
	if stopErr := adapter.StopSession(context.Background(), session); stopErr != nil {
		t.Errorf("StopSession during turn: %v", stopErr)
	}

	// RunTurn must return within a reasonable bound after the subprocess exits.
	select {
	case outcome := <-outcomeCh:
		if outcome.err == nil {
			t.Error("RunTurn returned nil after StopSession was called mid-turn; expected an error")
		}
		t.Logf("RunTurn returned error after StopSession: %v", outcome.err)
	case <-time.After(10 * time.Second):
		t.Error("RunTurn did not unblock within 10s after StopSession; possible deadlock in reader goroutine")
	}
}

// TestIntegration_MultiTurn validates the core architectural invariant of the
// Codex adapter: the subprocess and thread persist across turns within a
// session. Turn 1 emits EventSessionStarted; turn 2 emits only
// EventNotification for the turn/started notification. Both turns share the
// same SessionID (the thread ID), and the subprocess PID does not change.
func TestIntegration_MultiTurn(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session := mustStartSession(t, ctx, adapter, workspace)
	state := session.Internal.(*sessionState)
	state.mu.Lock()
	pidAfterStart := state.proc.Pid
	state.mu.Unlock()

	// Turn 1: must emit EventSessionStarted and complete successfully.
	onEvent1, collected1 := makeEventCollector(t)
	result1, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: onEvent1,
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 1): %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn (turn 1): ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if result1.SessionID != session.ID {
		t.Errorf("turn 1: TurnResult.SessionID = %q, want %q", result1.SessionID, session.ID)
	}
	assertContainsEventType(t, collected1(), domain.EventSessionStarted)

	// Verify internal turn counter after turn 1.
	if state.turnCount != 1 {
		t.Errorf("state.turnCount after turn 1 = %d, want 1", state.turnCount)
	}

	// Verify the subprocess PID has not changed after turn 1.
	state.mu.Lock()
	pidAfterTurn1 := state.proc.Pid
	state.mu.Unlock()
	if pidAfterTurn1 != pidAfterStart {
		t.Errorf("subprocess PID changed after turn 1: before=%d after=%d (persistent subprocess must survive turns)", pidAfterStart, pidAfterTurn1)
	}

	// Turn 2: must NOT emit EventSessionStarted (only turn 1 does that).
	// The turn/started notification for subsequent turns maps to EventNotification.
	onEvent2, collected2 := makeEventCollector(t)
	result2, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: onEvent2,
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 2): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn (turn 2): ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != session.ID {
		t.Errorf("turn 2: TurnResult.SessionID = %q, want %q (must remain same thread)", result2.SessionID, session.ID)
	}

	events2 := collected2()
	assertNoEventType(t, events2, domain.EventSessionStarted)
	assertContainsEventType(t, events2, domain.EventTurnCompleted)

	// Verify internal turn counter after turn 2.
	if state.turnCount != 2 {
		t.Errorf("state.turnCount after turn 2 = %d, want 2", state.turnCount)
	}

	// The subprocess PID must still be the same original PID.
	state.mu.Lock()
	pidAfterTurn2 := state.proc.Pid
	state.mu.Unlock()
	if pidAfterTurn2 != pidAfterStart {
		t.Errorf("subprocess PID changed after turn 2: original=%d current=%d (persistent subprocess must survive all turns)", pidAfterStart, pidAfterTurn2)
	}

	// Thread ID must be identical across both turns.
	if result1.SessionID != result2.SessionID {
		t.Errorf("SessionID changed between turns: turn1=%q turn2=%q (same thread must be reused)", result1.SessionID, result2.SessionID)
	}
}

// TestIntegration_ResumeSession verifies that StartSession with a
// ResumeSessionID sends thread/resume and returns a session whose ID
// matches the provided thread ID. A turn on the resumed session must
// complete successfully.
func TestIntegration_ResumeSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Establish an original session and capture its thread ID.
	adapter1 := mustNewAdapter(t)
	session1, err := adapter1.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession (original): %v", err)
	}

	result1, err := adapter1.RunTurn(ctx, session1, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (original): %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("original turn: ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	originalThreadID := result1.SessionID
	if originalThreadID == "" {
		t.Fatal("original turn: TurnResult.SessionID is empty")
	}

	// Stop the original session so the thread is persisted.
	if err := adapter1.StopSession(context.Background(), session1); err != nil {
		t.Fatalf("StopSession (original): %v", err)
	}

	// Resume via a fresh adapter and StartSession with the captured thread ID.
	adapter2 := mustNewAdapter(t)
	session2, err := adapter2.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     integrationAgentConfig(),
		ResumeSessionID: originalThreadID,
	})
	if err != nil {
		t.Fatalf("StartSession (resume): %v", err)
	}
	t.Cleanup(func() { _ = adapter2.StopSession(context.Background(), session2) })

	// The resumed session must carry the same thread ID.
	if session2.ID != originalThreadID {
		t.Errorf("resumed session.ID = %q, want %q (provided ResumeSessionID)", session2.ID, originalThreadID)
	}

	// A turn on the resumed session must complete successfully.
	result2, err := adapter2.RunTurn(ctx, session2, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (resumed): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed turn: ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != originalThreadID {
		t.Errorf("resumed turn: TurnResult.SessionID = %q, want %q", result2.SessionID, originalThreadID)
	}
}
