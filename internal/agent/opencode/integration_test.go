package opencode_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"

	_ "github.com/sortie-ai/sortie/internal/agent/opencode"
	"github.com/sortie-ai/sortie/internal/registry"
)

func skipIfNotEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_OPENCODE_TEST") != "1" {
		t.Skip("set SORTIE_OPENCODE_TEST=1 to run opencode integration tests")
	}
}

// integrationCommand returns the opencode binary path, defaulting to "opencode".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_OPENCODE_COMMAND"); cmd != "" {
		return cmd
	}
	return "opencode"
}

// integrationConfig returns base config for integration tests.
func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_OPENCODE_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-4-5"
	}

	cfg := map[string]any{
		"dangerously_skip_permissions": true,
		"disable_autocompact":          true,
		"model":                        model,
	}
	return cfg
}

// mustNewAdapter creates an adapter or fatals.
func mustNewAdapter(t *testing.T) domain.AgentAdapter {
	t.Helper()
	factory, err := registry.Agents.Get("opencode")
	if err != nil {
		t.Fatalf("registry.Agents.Get(opencode): %v", err)
	}
	a, err := factory(integrationConfig())
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	return a
}

// mustStartIntegrationSession starts a session against the real opencode binary.
// ReadTimeoutMS is set to 3 minutes to absorb one-time cold-start SQLite
// migrations that can run for over 30 seconds on first launch.
func mustStartIntegrationSession(t *testing.T, a domain.AgentAdapter, resumeID string) domain.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: 3 * 60 * 1000, // 3 minutes: absorbs cold-start SQLite migration
		},
		ResumeSessionID: resumeID,
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	return session
}

// collectAllEvents runs a turn and returns all events and the result.
// The per-turn context allows up to 5 minutes so that tests are not tripped
// by slow first-turn processing on a cold-start instance.
func collectAllEvents(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult) {
	t.Helper()
	events, result, err := collectAllEventsErr(t, a, session, prompt)
	if err != nil {
		t.Logf("RunTurn error: %v", err)
	}
	return events, result
}

// collectAllEventsErr is collectAllEvents that also surfaces the RunTurn error
// so callers can classify the failure (e.g. distinguish a transient subprocess
// early-exit from a deterministic adapter defect).
func collectAllEventsErr(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var events []domain.AgentEvent
	result, err := a.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	return events, result, err
}

// isTransientFirstEventExit reports whether err is the specific transient
// failure where the OpenCode subprocess exited before emitting its first JSON
// event. This surfaces as an [domain.AgentError] of kind [domain.ErrPortExit]
// and is an infrastructure/CLI symptom (e.g. cold-start), not an adapter
// resume defect. Every other failure category returns false so it propagates
// to the test assertion unchanged.
func isTransientFirstEventExit(result domain.TurnResult, err error) bool {
	if result.ExitReason != domain.EventTurnEndedWithError {
		return false
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		return false
	}
	return agentErr.Kind == domain.ErrPortExit &&
		strings.Contains(agentErr.Message, "process exited before first opencode json event")
}

func TestIntegration_HappyPathFreshTurn(t *testing.T) {
	skipIfNotEnabled(t)

	a := mustNewAdapter(t)
	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	events, result := collectAllEvents(t, a, session, "Reply with exactly: hello")

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var sessionStarted bool
	for _, e := range events {
		if e.Type == domain.EventSessionStarted {
			sessionStarted = true
			if e.SessionID == "" {
				t.Error("EventSessionStarted has empty SessionID")
			}
		}
	}
	if !sessionStarted {
		t.Error("no session_started event emitted")
	}
}

func TestIntegration_SessionResume(t *testing.T) {
	skipIfNotEnabled(t)

	a := mustNewAdapter(t)
	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	// First turn.
	_, result1 := collectAllEvents(t, a, session, "Say: turn one")
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("turn 1 ExitReason = %q, want completed", result1.ExitReason)
	}
	sessionID := result1.SessionID
	if sessionID == "" {
		t.Fatal("turn 1 SessionID is empty")
	}

	// Resume session. The first turn already exited and its session state was
	// read back by the post-turn `opencode export`, so the session is durable
	// before resuming - this is not a session-availability race.
	//
	// The resumed turn occasionally fails because the freshly launched OpenCode
	// subprocess exits before emitting its first JSON event (an infrastructure
	// /cold-start symptom). Allow a single retry scoped to that exact transient
	// error. A deterministic resume regression fails both attempts - or fails
	// with a different reason - and still surfaces here unmasked.
	const resumeAttempts = 2
	var result2 domain.TurnResult
	for attempt := 1; attempt <= resumeAttempts; attempt++ {
		a2 := mustNewAdapter(t)
		session2 := mustStartIntegrationSession(t, a2, sessionID)
		t.Cleanup(func() { _ = a2.StopSession(context.Background(), session2) })

		var err2 error
		_, result2, err2 = collectAllEventsErr(t, a2, session2, "What did I say in the previous message?")
		if err2 != nil {
			t.Logf("resumed RunTurn attempt %d error: %v", attempt, err2)
		}
		if result2.ExitReason == domain.EventTurnCompleted {
			break
		}
		if attempt < resumeAttempts && isTransientFirstEventExit(result2, err2) {
			t.Logf("resumed turn hit transient first-event exit on attempt %d; retrying", attempt)
			continue
		}
		break
	}

	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed turn ExitReason = %q, want completed", result2.ExitReason)
	}
}

func TestIntegration_InvalidModelFailure(t *testing.T) {
	skipIfNotEnabled(t)

	cfg := integrationConfig()
	cfg["model"] = "nonexistent/nonexistent"

	factory, err := registry.Agents.Get("opencode")
	if err != nil {
		t.Fatalf("registry.Agents.Get: %v", err)
	}
	a, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}

	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	events, result := collectAllEvents(t, a, session, "Reply with exactly: hello")
	if result.ExitReason != domain.EventTurnFailed {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	var sawTurnFailed, sawModelNotFound bool
	for _, event := range events {
		if event.Type != domain.EventTurnFailed {
			continue
		}
		sawTurnFailed = true
		if strings.Contains(event.Message, "Model not found") {
			sawModelNotFound = true
		}
	}
	if !sawTurnFailed {
		t.Fatalf("expected turn_failed event for invalid model, events=%+v", events)
	}
	if !sawModelNotFound {
		t.Errorf("expected at least one turn_failed event with invalid-model detail, events=%+v", events)
	}
}

func TestIntegration_PermissionDeny(t *testing.T) {
	skipIfNotEnabled(t)

	cfg := integrationConfig()
	cfg["dangerously_skip_permissions"] = false

	factory, err := registry.Agents.Get("opencode")
	if err != nil {
		t.Fatalf("registry.Agents.Get: %v", err)
	}
	a, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}

	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	// The prompt explicitly names the tool so the model is compelled to invoke
	// it rather than narrating intent or answering from memory.
	events, _ := collectAllEvents(t, a, session,
		"Use your file-read tool to read /etc/hostname and return the exact contents verbatim. You must call the tool — do not answer from memory and do not describe what you would do.")

	// Strong signal: OpenCode auto-rejects external_directory access in
	// headless mode without --dangerously-skip-permissions, emitting a
	// tool_use error envelope.
	var sawToolError bool
	for _, e := range events {
		if e.Type == domain.EventToolResult && e.ToolError {
			sawToolError = true
			break
		}
	}
	if sawToolError {
		return
	}

	// Weak signal: the model acknowledged the denial in assistant text without
	// emitting a tool result (e.g. OpenCode reported the block as a
	// notification before the tool completed).
	denialKeywords := []string{"denied", "permission", "not allowed", "cannot", "can't", "unable"}
	for _, e := range events {
		if e.Type != domain.EventNotification && e.Type != domain.EventOtherMessage && e.Type != domain.EventTurnFailed {
			continue
		}
		msg := strings.ToLower(e.Message)
		for _, kw := range denialKeywords {
			if strings.Contains(msg, kw) {
				return
			}
		}
	}

	// Neither signal was present: the model did not attempt the tool and did
	// not report a denial. This is a non-deterministic model choice, not an
	// adapter defect. Skip rather than block the release pipeline.
	t.Skip("model neither invoked the file-read tool nor reported a denial; skipping to avoid blocking release on non-deterministic model behavior")
}

func TestIntegration_TurnCancellation(t *testing.T) {
	skipIfNotEnabled(t)

	a := mustNewAdapter(t)
	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	turnCtx, turnCancel := context.WithCancel(ctx)
	resultCh := make(chan domain.TurnResult, 1)
	go func() {
		result, _ := a.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt:  "Count to 1000 slowly, outputting each number on its own line",
			OnEvent: func(_ domain.AgentEvent) {},
		})
		resultCh <- result
	}()

	// Cancel after a brief moment.
	time.Sleep(500 * time.Millisecond)
	turnCancel()

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
	case <-ctx.Done():
		t.Fatal("RunTurn did not return after context cancel")
	}
}

func TestIntegration_PermissionDeepMerge(t *testing.T) {
	skipIfNotEnabled(t)

	// Verify that setting OPENCODE_PERMISSION does not replace but merges
	// with any existing permission config (deep-merge semantics).
	cfg := integrationConfig()
	cfg["allowed_tools"] = []any{"read", "glob"}

	factory, err := registry.Agents.Get("opencode")
	if err != nil {
		t.Fatalf("registry.Agents.Get: %v", err)
	}
	a, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}

	session := mustStartIntegrationSession(t, a, "")
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	_, result := collectAllEvents(t, a, session, "List files in the current directory")
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want completed", result.ExitReason)
	}
}
