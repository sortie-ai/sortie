package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
)

// --- Test helpers ---

// mustParseTemplate compiles a prompt template or fails the test.
func mustParseTemplate(t *testing.T, body string) *prompt.Template {
	t.Helper()
	tmpl, err := prompt.Parse(body, "test", 0)
	if err != nil {
		t.Fatalf("prompt.Parse(%q): %v", body, err)
	}
	return tmpl
}

// defaultWorkerConfig returns a minimal config suitable for worker tests.
// The workspace root must be overridden with t.TempDir() by the caller.
func defaultWorkerConfig(workspaceRoot string) config.ServiceConfig {
	return config.ServiceConfig{
		Tracker: config.TrackerConfig{
			ActiveStates:   []string{"To Do", "In Progress"},
			TerminalStates: []string{"Done"},
		},
		Workspace: config.WorkspaceConfig{Root: workspaceRoot},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:          "mock",
			MaxTurns:      3,
			ReadTimeoutMS: 1000,
		},
	}
}

// workerTestIssue returns a minimal valid issue for worker tests.
func workerTestIssue() domain.Issue {
	return domain.Issue{
		ID:         "issue-1",
		Identifier: "TEST-1",
		Title:      "Test issue",
		State:      "To Do",
	}
}

// mockAgentAdapter is a configurable test double for domain.AgentAdapter.
type mockAgentAdapter struct {
	startSessionFn func(ctx context.Context, params domain.StartSessionParams) (domain.Session, error)
	runTurnFn      func(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error)
	stopSessionFn  func(ctx context.Context, session domain.Session) error
}

var _ domain.AgentAdapter = (*mockAgentAdapter)(nil)

func (m *mockAgentAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if m.startSessionFn != nil {
		return m.startSessionFn(ctx, params)
	}
	return domain.Session{ID: "sess-1"}, nil
}

func (m *mockAgentAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if m.runTurnFn != nil {
		return m.runTurnFn(ctx, session, params)
	}
	if params.OnEvent != nil {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventNotification,
			Timestamp: time.Now().UTC(),
			Message:   "mock event",
		})
	}
	return domain.TurnResult{
		SessionID:  session.ID,
		ExitReason: domain.EventTurnCompleted,
	}, nil
}

func (m *mockAgentAdapter) StopSession(ctx context.Context, session domain.Session) error {
	if m.stopSessionFn != nil {
		return m.stopSessionFn(ctx, session)
	}
	return nil
}

func (m *mockAgentAdapter) EventStream() <-chan domain.AgentEvent { return nil }

// mockTrackerAdapter is a configurable test double for domain.TrackerAdapter.
type mockTrackerAdapter struct {
	fetchStatesFn func(ctx context.Context, ids []string) (map[string]string, error)
}

var _ domain.TrackerAdapter = (*mockTrackerAdapter)(nil)

func (m *mockTrackerAdapter) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (m *mockTrackerAdapter) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIDs(ctx context.Context, ids []string) (map[string]string, error) {
	if m.fetchStatesFn != nil {
		return m.fetchStatesFn(ctx, ids)
	}
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		result[id] = "To Do"
	}
	return result, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

// exitCapture captures the OnExit callback arguments.
type exitCapture struct {
	mu      sync.Mutex
	results []WorkerResult
	done    chan struct{}
}

func newExitCapture() *exitCapture {
	return &exitCapture{done: make(chan struct{}, 1)}
}

func (c *exitCapture) onExit(_ string, result WorkerResult) {
	c.mu.Lock()
	c.results = append(c.results, result)
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
}

func (c *exitCapture) waitResult(t *testing.T) WorkerResult {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OnExit")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.results) == 0 {
		t.Fatal("OnExit was never called")
	}
	return c.results[0]
}

func (c *exitCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.results)
}

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nopWriter{}, nil))
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// --- Helper unit tests ---

func TestNormalizeAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		attempt *int
		want    int
	}{
		{name: "nil returns 0", attempt: nil, want: 0},
		{name: "ptr(0) returns 0", attempt: intPtr(0), want: 0},
		{name: "ptr(1) returns 1", attempt: intPtr(1), want: 1},
		{name: "ptr(5) returns 5", attempt: intPtr(5), want: 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeAttempt(tt.attempt)
			if got != tt.want {
				t.Errorf("normalizeAttempt(%v) = %d, want %d", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestIsActiveState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		state        string
		activeStates []string
		want         bool
	}{
		{name: "exact match", state: "To Do", activeStates: []string{"To Do", "In Progress"}, want: true},
		{name: "case-insensitive match", state: "to do", activeStates: []string{"To Do"}, want: true},
		{name: "uppercase input", state: "TO DO", activeStates: []string{"To Do"}, want: true},
		{name: "non-match", state: "Done", activeStates: []string{"To Do", "In Progress"}, want: false},
		{name: "empty active states", state: "To Do", activeStates: []string{}, want: false},
		{name: "nil active states", state: "To Do", activeStates: nil, want: false},
		{name: "empty state", state: "", activeStates: []string{"To Do"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isActiveState(tt.state, tt.activeStates)
			if got != tt.want {
				t.Errorf("isActiveState(%q, %v) = %t, want %t", tt.state, tt.activeStates, got, tt.want)
			}
		})
	}
}

func TestIsTurnSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason domain.AgentEventType
		want   bool
	}{
		{name: "turn_completed", reason: domain.EventTurnCompleted, want: true},
		{name: "turn_failed", reason: domain.EventTurnFailed, want: false},
		{name: "turn_cancelled", reason: domain.EventTurnCancelled, want: false},
		{name: "turn_ended_with_error", reason: domain.EventTurnEndedWithError, want: false},
		{name: "turn_input_required", reason: domain.EventTurnInputRequired, want: false},
		{name: "unknown event", reason: domain.AgentEventType("unknown"), want: false},
		{name: "empty string", reason: domain.AgentEventType(""), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isTurnSuccess(tt.reason)
			if got != tt.want {
				t.Errorf("isTurnSuccess(%q) = %t, want %t", tt.reason, got, tt.want)
			}
		})
	}
}

func TestToDomainAgentConfig(t *testing.T) {
	t.Parallel()

	src := config.AgentConfig{
		Kind:           "claude-code",
		Command:        "claude --json",
		TurnTimeoutMS:  30000,
		ReadTimeoutMS:  10000,
		StallTimeoutMS: 60000,
	}

	got := toDomainAgentConfig(src)

	if got.Kind != src.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, src.Kind)
	}
	if got.Command != src.Command {
		t.Errorf("Command = %q, want %q", got.Command, src.Command)
	}
	if got.TurnTimeoutMS != src.TurnTimeoutMS {
		t.Errorf("TurnTimeoutMS = %d, want %d", got.TurnTimeoutMS, src.TurnTimeoutMS)
	}
	if got.ReadTimeoutMS != src.ReadTimeoutMS {
		t.Errorf("ReadTimeoutMS = %d, want %d", got.ReadTimeoutMS, src.ReadTimeoutMS)
	}
	if got.StallTimeoutMS != src.StallTimeoutMS {
		t.Errorf("StallTimeoutMS = %d, want %d", got.StallTimeoutMS, src.StallTimeoutMS)
	}
}

// --- RunWorkerAttempt integration tests ---

func TestRunWorkerAttempt(t *testing.T) {
	t.Parallel()

	t.Run("multi_turn_success", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 3

		var eventCount atomic.Int64
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{
							Type:      domain.EventNotification,
							Timestamp: time.Now().UTC(),
							Message:   "working",
						})
					}
					eventCount.Add(1)
					return domain.TurnResult{
						SessionID:  session.ID,
						ExitReason: domain.EventTurnCompleted,
					}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		issue := workerTestIssue()
		RunWorkerAttempt(context.Background(), issue, nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 3 {
			t.Errorf("TurnsCompleted = %d, want 3", result.TurnsCompleted)
		}
		if result.Error != nil {
			t.Errorf("Error = %v, want nil", result.Error)
		}
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty")
		}
		if result.SessionID != "sess-1" {
			t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-1")
		}
		if got := eventCount.Load(); got < 3 {
			t.Errorf("OnEvent relay count = %d, want >= 3", got)
		}
		if ec.count() != 1 {
			t.Errorf("OnExit call count = %d, want 1", ec.count())
		}
	})

	t.Run("early_exit_on_tracker_state_change", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var turnCount atomic.Int64
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					turn := turnCount.Load()
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						if turn >= 1 {
							result[id] = "Done" // terminal state after turn 1
						} else {
							result[id] = "To Do"
						}
					}
					return result, nil
				},
			},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					turnCount.Add(1)
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1", result.TurnsCompleted)
		}
	})

	t.Run("max_turns_reached", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 2

		ec := newExitCapture()
		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 2 {
			t.Errorf("TurnsCompleted = %d, want 2", result.TurnsCompleted)
		}
	})

	t.Run("agent_failure_on_turn_2", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var turnCount atomic.Int64
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					n := turnCount.Add(1)
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					if n >= 2 {
						return domain.TurnResult{}, fmt.Errorf("agent crashed")
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1", result.TurnsCompleted)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "agent turn 2") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "agent turn 2")
		}
	})

	t.Run("agent_turn_failure_exit_reason", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		ec := newExitCapture()
		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					return domain.TurnResult{
						SessionID:  session.ID,
						ExitReason: domain.EventTurnFailed,
					}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		// Turn completed (got TurnResult) but exit reason was failure.
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1", result.TurnsCompleted)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "turn_failed") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "turn_failed")
		}
	})

	t.Run("workspace_preparation_failure", func(t *testing.T) {
		t.Parallel()

		// Use a non-directory path as workspace root to trigger failure.
		tmpDir := t.TempDir()
		badRoot := tmpDir + "/not-a-dir"
		// Create a file at the path so it's not a directory.
		createFileAtPath(t, badRoot)

		cfg := defaultWorkerConfig(badRoot)
		var startCalled atomic.Bool
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
					startCalled.Store(true)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.TurnsCompleted != 0 {
			t.Errorf("TurnsCompleted = %d, want 0", result.TurnsCompleted)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "workspace preparation") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "workspace preparation")
		}
		if result.SessionID != "" {
			t.Errorf("SessionID = %q, want empty (no session started)", result.SessionID)
		}
		if startCalled.Load() {
			t.Error("StartSession was called, want no call on workspace failure")
		}
	})

	t.Run("session_start_failure", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
					return domain.Session{}, errors.New("session launch failed")
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "agent session start") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "agent session start")
		}
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty (workspace was prepared)")
		}
		if result.SessionID != "" {
			t.Errorf("SessionID = %q, want empty (StartSession failed)", result.SessionID)
		}
	})

	t.Run("context_cancellation", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5
		cfg.Agent.ReadTimeoutMS = 2000

		ec := newExitCapture()

		// Track the context passed to StopSession.
		var stopCtxCancelled atomic.Bool
		var stopCtxHasDeadline atomic.Bool
		stopCalled := make(chan struct{}, 1)

		ctx, cancel := context.WithCancel(context.Background())

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(runCtx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					// Cancel the context to simulate reconciliation kill.
					cancel()
					return domain.TurnResult{}, runCtx.Err()
				},
				stopSessionFn: func(sCtx context.Context, _ domain.Session) error {
					stopCtxCancelled.Store(sCtx.Err() != nil)
					_, hasDeadline := sCtx.Deadline()
					stopCtxHasDeadline.Store(hasDeadline)
					select {
					case stopCalled <- struct{}{}:
					default:
					}
					return nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(ctx, workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitCancelled {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitCancelled)
		}

		// Wait for StopSession to be called.
		select {
		case <-stopCalled:
		case <-time.After(3 * time.Second):
			t.Fatal("StopSession was never called")
		}

		// Verify stopSessionBestEffort detaches context.
		if stopCtxCancelled.Load() {
			t.Error("StopSession received cancelled context, want detached (not cancelled)")
		}
		if !stopCtxHasDeadline.Load() {
			t.Error("StopSession context has no deadline, want deadline from ReadTimeoutMS")
		}
	})

	t.Run("context_cancelled_before_session_start", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)

		ec := newExitCapture()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately.

		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(ctx, workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		// workspace.Prepare returns immediately on cancelled context,
		// so we expect either ExitCancelled (if caught at inter-phase
		// check) or ExitCancelled (if workspace.Prepare itself fails).
		if result.ExitKind != WorkerExitCancelled {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitCancelled)
		}
	})

	t.Run("prompt_turn_semantics", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 2

		ec := newExitCapture()
		var capturedPrompts []string
		var mu sync.Mutex

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompts = append(capturedPrompts, params.Prompt)
					mu.Unlock()
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc: func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template {
				return mustParseTemplate(t, "turn={{ .run.turn_number }} cont={{ .run.is_continuation }}")
			},
			OnEvent: func(_ string, _ domain.AgentEvent) {},
			OnExit:  ec.onExit,
			Logger:  discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 2 {
			t.Fatalf("TurnsCompleted = %d, want 2", result.TurnsCompleted)
		}

		mu.Lock()
		prompts := make([]string, len(capturedPrompts))
		copy(prompts, capturedPrompts)
		mu.Unlock()

		if len(prompts) != 2 {
			t.Fatalf("captured %d prompts, want 2", len(prompts))
		}

		// Turn 1: turn_number=1, is_continuation=false.
		if !strings.Contains(prompts[0], "turn=1") {
			t.Errorf("turn 1 prompt = %q, want to contain %q", prompts[0], "turn=1")
		}
		if !strings.Contains(prompts[0], "cont=false") {
			t.Errorf("turn 1 prompt = %q, want to contain %q", prompts[0], "cont=false")
		}

		// Turn 2: turn_number=2, is_continuation=true.
		if !strings.Contains(prompts[1], "turn=2") {
			t.Errorf("turn 2 prompt = %q, want to contain %q", prompts[1], "turn=2")
		}
		if !strings.Contains(prompts[1], "cont=true") {
			t.Errorf("turn 2 prompt = %q, want to contain %q", prompts[1], "cont=true")
		}
	})

	t.Run("tracker_state_refresh_failure", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
					return nil, fmt.Errorf("tracker API timeout")
				},
			},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "issue state refresh") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "issue state refresh")
		}
	})

	t.Run("panic_recovery", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		ec := newExitCapture()
		var stopCalled atomic.Bool

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					panic("unexpected agent crash")
				},
				stopSessionFn: func(_ context.Context, _ domain.Session) error {
					stopCalled.Store(true)
					return nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		// Should not propagate the panic.
		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "worker panic") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "worker panic")
		}
		if !strings.Contains(result.Error.Error(), "unexpected agent crash") {
			t.Errorf("Error = %q, want to contain panic value %q", result.Error, "unexpected agent crash")
		}
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty (workspace was prepared before panic)")
		}
		if result.SessionID != "sess-1" {
			t.Errorf("SessionID = %q, want %q (session started before panic)", result.SessionID, "sess-1")
		}
		if !stopCalled.Load() {
			t.Error("StopSession was not called during panic recovery, want teardown")
		}
		if ec.count() != 1 {
			t.Errorf("OnExit call count = %d, want 1", ec.count())
		}
	})

	t.Run("attempt_passed_through_to_result", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		attempt := intPtr(3)
		RunWorkerAttempt(context.Background(), workerTestIssue(), attempt, deps)

		result := ec.waitResult(t)
		if result.Attempt == nil {
			t.Fatal("Attempt is nil, want non-nil")
		}
		if *result.Attempt != 3 {
			t.Errorf("Attempt = %d, want 3", *result.Attempt)
		}
		if result.AgentAdapter != "mock" {
			t.Errorf("AgentAdapter = %q, want %q", result.AgentAdapter, "mock")
		}
	})

	t.Run("issue_id_and_identifier_in_result", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		issue := workerTestIssue()
		RunWorkerAttempt(context.Background(), issue, nil, deps)

		result := ec.waitResult(t)
		if result.IssueID != issue.ID {
			t.Errorf("IssueID = %q, want %q", result.IssueID, issue.ID)
		}
		if result.Identifier != issue.Identifier {
			t.Errorf("Identifier = %q, want %q", result.Identifier, issue.Identifier)
		}
	})

	t.Run("resume_session_id_passed_through", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()
		var capturedResumeID atomic.Value

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedResumeID.Store(params.ResumeSessionID)
					return domain.Session{ID: "sess-resumed"}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			ResumeSessionID:    "prev-sess-123",
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		got, ok := capturedResumeID.Load().(string)
		if !ok {
			t.Fatal("StartSession was never called")
		}
		if got != "prev-sess-123" {
			t.Errorf("ResumeSessionID = %q, want %q", got, "prev-sess-123")
		}
		if result.SessionID != "sess-resumed" {
			t.Errorf("result.SessionID = %q, want %q", result.SessionID, "sess-resumed")
		}
	})

	t.Run("panic_after_workspace_calls_finish", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		markerPath := filepath.Join(tmpDir, "after_run_marker")

		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5
		// Hook writes a marker file; workspace.Finish calls this.
		cfg.Hooks.AfterRun = fmt.Sprintf("touch %s", markerPath)

		ec := newExitCapture()
		var stopCalled atomic.Bool

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, _ domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					panic("crash after session")
				},
				stopSessionFn: func(_ context.Context, _ domain.Session) error {
					stopCalled.Store(true)
					return nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}

		// Verify after_run hook was executed during panic recovery.
		if _, err := os.Stat(markerPath); err != nil {
			t.Errorf("after_run marker file not found: %v (workspace.Finish not called during panic recovery)", err)
		}
		if !stopCalled.Load() {
			t.Error("StopSession was not called during panic recovery, want teardown")
		}
	})

	t.Run("max_turns_clamped", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 0 // Invalid: should be clamped to 1.

		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1 (max_turns clamped from 0)", result.TurnsCompleted)
		}
		if result.Error != nil {
			t.Errorf("Error = %v, want nil", result.Error)
		}
	})

	t.Run("max_turns_clamped_negative", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = -5 // Negative: should be clamped to 1.

		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:     &mockTrackerAdapter{},
			AgentAdapter:       &mockAgentAdapter{},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1 (max_turns clamped from -5)", result.TurnsCompleted)
		}
	})

	t.Run("panic_with_turns_completed", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var turnCount atomic.Int64
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					n := turnCount.Add(1)
					if n >= 2 {
						panic("crash on turn 2")
					}
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:            func(_ string, _ domain.AgentEvent) {},
			OnExit:             ec.onExit,
			Logger:             discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "worker panic") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "worker panic")
		}
		// Turn 1 completed successfully before the panic on turn 2.
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1 (turn 1 completed, panic on turn 2)", result.TurnsCompleted)
		}
		if result.SessionID != "sess-1" {
			t.Errorf("SessionID = %q, want %q", result.SessionID, "sess-1")
		}
	})

	t.Run("on_event_relay_copies_rate_limits", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		// adapterMap is the original map the adapter attaches to the event.
		// We hold a reference to verify the relay produces a distinct copy.
		adapterMap := map[string]any{"remaining": 42}

		var relayedMap atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{
							Type:       domain.EventNotification,
							Timestamp:  time.Now().UTC(),
							RateLimits: adapterMap,
						})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:         func() config.ServiceConfig { return cfg },
			PromptTemplateFunc: func() *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent: func(_ string, event domain.AgentEvent) {
				if event.RateLimits != nil {
					relayedMap.Store(event.RateLimits)
				}
			},
			OnExit: ec.onExit,
			Logger: discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		got, ok := relayedMap.Load().(map[string]any)
		if !ok {
			t.Fatal("OnEvent never relayed an event with RateLimits")
		}

		// The relayed map must contain the same data.
		if got["remaining"] != 42 {
			t.Errorf("relayed RateLimits[\"remaining\"] = %v, want 42", got["remaining"])
		}

		// The relayed map must be a distinct object from the adapter's
		// original, proving the OnEvent relay defensive-copied it.
		if fmt.Sprintf("%p", got) == fmt.Sprintf("%p", adapterMap) {
			t.Error("relayed RateLimits has same pointer as adapter map, want defensive copy")
		}
	})
}

// --- stopSessionBestEffort unit tests ---

func TestStopSessionBestEffort(t *testing.T) {
	t.Parallel()

	t.Run("detached_context_with_timeout", func(t *testing.T) {
		t.Parallel()

		var receivedCtxCancelled atomic.Bool
		var receivedCtxHasDeadline atomic.Bool

		adapter := &mockAgentAdapter{
			stopSessionFn: func(ctx context.Context, _ domain.Session) error {
				receivedCtxCancelled.Store(ctx.Err() != nil)
				_, hasDeadline := ctx.Deadline()
				receivedCtxHasDeadline.Store(hasDeadline)
				return nil
			},
		}

		// Create an already-cancelled context.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		cfg := config.ServiceConfig{
			Agent: config.AgentConfig{ReadTimeoutMS: 5000},
		}

		stopSessionBestEffort(ctx, adapter, domain.Session{ID: "s1"}, cfg, discardLogger())

		if receivedCtxCancelled.Load() {
			t.Error("StopSession received cancelled context, want detached (not cancelled)")
		}
		if !receivedCtxHasDeadline.Load() {
			t.Error("StopSession context has no deadline, want deadline from ReadTimeoutMS")
		}
	})

	t.Run("default_timeout_when_zero", func(t *testing.T) {
		t.Parallel()

		var deadlineReceived time.Time

		adapter := &mockAgentAdapter{
			stopSessionFn: func(ctx context.Context, _ domain.Session) error {
				dl, _ := ctx.Deadline()
				deadlineReceived = dl
				return nil
			},
		}

		cfg := config.ServiceConfig{
			Agent: config.AgentConfig{ReadTimeoutMS: 0},
		}

		before := time.Now()
		stopSessionBestEffort(context.Background(), adapter, domain.Session{ID: "s1"}, cfg, discardLogger())

		// Default is 10000ms; deadline should be ~10s from now.
		expectedMin := before.Add(9 * time.Second)
		if deadlineReceived.Before(expectedMin) {
			t.Errorf("deadline = %v, want >= %v (default 10s timeout)", deadlineReceived, expectedMin)
		}
	})

	t.Run("error_is_swallowed", func(t *testing.T) {
		t.Parallel()

		adapter := &mockAgentAdapter{
			stopSessionFn: func(_ context.Context, _ domain.Session) error {
				return errors.New("stop failed")
			},
		}

		cfg := config.ServiceConfig{
			Agent: config.AgentConfig{ReadTimeoutMS: 1000},
		}

		// Should not panic or propagate the error.
		stopSessionBestEffort(context.Background(), adapter, domain.Session{ID: "s1"}, cfg, discardLogger())
	})
}

// --- exitKindForErr unit tests ---

func TestExitKindForErr(t *testing.T) {
	t.Parallel()

	t.Run("live_context_returns_error", func(t *testing.T) {
		t.Parallel()

		got := exitKindForErr(context.Background())
		if got != WorkerExitError {
			t.Errorf("exitKindForErr(live ctx) = %q, want %q", got, WorkerExitError)
		}
	})

	t.Run("cancelled_context_returns_cancelled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		got := exitKindForErr(ctx)
		if got != WorkerExitCancelled {
			t.Errorf("exitKindForErr(cancelled ctx) = %q, want %q", got, WorkerExitCancelled)
		}
	})
}

// createFileAtPath creates an empty regular file, used to make a path
// that is not a directory for workspace preparation failure tests.
func createFileAtPath(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating file at %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing file at %s: %v", path, err)
	}
}
