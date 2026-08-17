package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workspace"
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

// transitionIssueCall records a single invocation of TransitionIssue.
type transitionIssueCall struct {
	IssueID     string
	TargetState string
}

// mockTrackerAdapter is a configurable test double for domain.TrackerAdapter.
type mockTrackerAdapter struct {
	fetchStatesFn     func(ctx context.Context, ids []string) (map[string]string, error)
	transitionIssueFn func(ctx context.Context, issueID, targetState string) error
	transitionCalls   []transitionIssueCall
	commentIssueFn    func(ctx context.Context, issueID, text string) error
	commentCalls      []commentIssueCall

	// fetchStatesCalls counts every FetchIssueStatesByIDs invocation,
	// whether it originates from the worker goroutine's per-turn refresh
	// or from the exit handler's verification read. atomic.Int64 because
	// the double is shared across the worker goroutine and the test
	// goroutine under -race.
	fetchStatesCalls atomic.Int64
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
	m.fetchStatesCalls.Add(1)
	if m.fetchStatesFn != nil {
		return m.fetchStatesFn(ctx, ids)
	}
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		result[id] = "To Do"
	}
	return result, nil
}

func (m *mockTrackerAdapter) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func (m *mockTrackerAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	m.transitionCalls = append(m.transitionCalls, transitionIssueCall{IssueID: issueID, TargetState: targetState})
	if m.transitionIssueFn != nil {
		return m.transitionIssueFn(ctx, issueID, targetState)
	}
	return nil
}

type commentIssueCall struct {
	IssueID string
	Text    string
}

func (m *mockTrackerAdapter) CommentIssue(ctx context.Context, issueID string, text string) error {
	m.commentCalls = append(m.commentCalls, commentIssueCall{IssueID: issueID, Text: text})
	if m.commentIssueFn != nil {
		return m.commentIssueFn(ctx, issueID, text)
	}
	return nil
}

func (m *mockTrackerAdapter) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

// stubAgentTool is a minimal domain.AgentTool for worker tests.
type stubAgentTool struct {
	toolName string
	desc     string
}

func (s *stubAgentTool) Name() string                 { return s.toolName }
func (s *stubAgentTool) Description() string          { return s.desc }
func (s *stubAgentTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (s *stubAgentTool) Execute(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}

var _ domain.AgentTool = (*stubAgentTool)(nil)

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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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

	t.Run("usage_fold_from_turn_result_only", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		wantUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					// The relay delivers no usage-bearing event for this
					// turn: only a plain notification.
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
					}
					return domain.TurnResult{
						SessionID:  session.ID,
						ExitReason: domain.EventTurnCompleted,
						Usage:      wantUsage,
					}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.Usage != wantUsage {
			t.Errorf("WorkerResult.Usage = %+v, want %+v (folded from TurnResult.Usage alone)", result.Usage, wantUsage)
		}
	})

	t.Run("usage_fold_event_and_turn_result_not_doubled", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		sharedUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					// The same usage value arrives through both the event
					// relay and TurnResult.Usage.
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{
							Type:      domain.EventTokenUsage,
							Timestamp: time.Now().UTC(),
							Usage:     sharedUsage,
						})
					}
					return domain.TurnResult{
						SessionID:  session.ID,
						ExitReason: domain.EventTurnCompleted,
						Usage:      sharedUsage,
					}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.Usage != sharedUsage {
			t.Errorf("WorkerResult.Usage = %+v, want %+v (single value, not doubled)", result.Usage, sharedUsage)
		}
	})

	t.Run("usage_measured_exit_before_first_turn_stays_true", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return nil },
			TemplateID:             "/nonexistent/template.md",
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                &domain.NoopMetrics{},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if !result.UsageMeasured {
			t.Error("WorkerResult.UsageMeasured = false, want true for an exit before any turn was entered")
		}
	})

	t.Run("usage_measured_drops_to_false_before_the_first_turn", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					// No usage-bearing event and no UsageMeasured signal:
					// the mirror must stay at the false value it dropped to
					// before this call.
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnFailed}, errors.New("boom")
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.UsageMeasured {
			t.Error("WorkerResult.UsageMeasured = true, want false after entering the first turn with no usage signal")
		}
	})

	t.Run("usage_measured_flips_true_on_a_usage_bearing_event", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{
							Type:      domain.EventTurnCompleted,
							Timestamp: time.Now().UTC(),
							Usage:     domain.TokenUsage{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
						})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnFailed}, errors.New("boom")
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if !result.UsageMeasured {
			t.Error("WorkerResult.UsageMeasured = false, want true after a usage-bearing event, even on the error exit path")
		}
	})

	t.Run("usage_measured_flips_true_on_an_all_zero_token_usage_event", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					if params.OnEvent != nil {
						params.OnEvent(domain.AgentEvent{
							Type:      domain.EventTokenUsage,
							Timestamp: time.Now().UTC(),
						})
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnFailed}, errors.New("boom")
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if !result.UsageMeasured {
			t.Error("WorkerResult.UsageMeasured = false, want true after an all-zero token_usage event")
		}
	})

	t.Run("usage_measured_flips_true_on_turn_result_alone", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					// No event carries usage; only TurnResult reports the
					// run measured.
					return domain.TurnResult{
						SessionID:     session.ID,
						ExitReason:    domain.EventTurnFailed,
						UsageMeasured: true,
					}, errors.New("boom")
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if !result.UsageMeasured {
			t.Error("WorkerResult.UsageMeasured = false, want true when only TurnResult.UsageMeasured reports the run measured")
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			PromptTemplateByIDFunc: func(_ string) *prompt.Template {
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
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			ResumeSessionID:        "prev-sess-123",
			Logger:                 discardLogger(),
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
		if runtime.GOOS == "windows" {
			t.Skip("after_run hook uses touch command")
		}
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
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

	t.Run("tool_advertisement_on_turn_1", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.Project = "TESTPROJ"

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		reg := domain.NewToolRegistry()
		reg.Register(&stubAgentTool{toolName: "tracker_api", desc: "Query issues"})

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			ToolRegistry:           reg,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if !strings.Contains(p, "tracker_api") {
			t.Errorf("turn 1 prompt missing tool name \"tracker_api\":\n%s", p)
		}
		if !strings.Contains(p, "Query issues") {
			t.Errorf("turn 1 prompt missing tool description:\n%s", p)
		}
		if !strings.Contains(p, "TESTPROJ") {
			t.Errorf("turn 1 prompt missing project name \"TESTPROJ\":\n%s", p)
		}
	})

	t.Run("tool_advertisement_not_on_continuation_turns", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 3
		cfg.Tracker.Project = "TESTPROJ"

		ec := newExitCapture()
		var capturedPrompts []string
		var mu sync.Mutex

		reg := domain.NewToolRegistry()
		reg.Register(&stubAgentTool{toolName: "tracker_api", desc: "Query issues"})

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompts = append(capturedPrompts, params.Prompt)
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "turn={{ .run.turn_number }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			ToolRegistry:           reg,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.TurnsCompleted != 3 {
			t.Fatalf("TurnsCompleted = %d, want 3", result.TurnsCompleted)
		}

		mu.Lock()
		prompts := make([]string, len(capturedPrompts))
		copy(prompts, capturedPrompts)
		mu.Unlock()

		if len(prompts) != 3 {
			t.Fatalf("captured %d prompts, want 3", len(prompts))
		}

		// Turn 1 should have the advertisement.
		if !strings.Contains(prompts[0], "tracker_api") {
			t.Errorf("turn 1 prompt missing tool advertisement:\n%s", prompts[0])
		}

		// Turns 2 and 3 should NOT have the advertisement.
		for i := 1; i < len(prompts); i++ {
			if strings.Contains(prompts[i], "tracker_api") {
				t.Errorf("turn %d prompt should not contain tool advertisement:\n%s", i+1, prompts[i])
			}
		}
	})

	t.Run("nil_tool_registry_no_advertisement", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			ToolRegistry:           nil,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if strings.Contains(p, "Available Sortie tools") {
			t.Errorf("prompt should not contain tool advertisement with nil ToolRegistry:\n%s", p)
		}
	})

	t.Run("empty_tool_registry_no_advertisement", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			ToolRegistry:           domain.NewToolRegistry(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if strings.Contains(p, "Available Sortie tools") {
			t.Errorf("prompt should not contain tool advertisement with empty ToolRegistry:\n%s", p)
		}
	})

	t.Run("SSHStrictHostKeyChecking propagated to StartSessionParams", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)

		var capturedSSHStrictHostKeyChecking string
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedSSHStrictHostKeyChecking = params.SSHStrictHostKeyChecking
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:               func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc:   func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                  func(_ string, _ domain.AgentEvent) {},
			OnExit:                   ec.onExit,
			Logger:                   discardLogger(),
			SSHStrictHostKeyChecking: "yes",
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		ec.waitResult(t)

		if capturedSSHStrictHostKeyChecking != "yes" {
			t.Errorf("StartSessionParams.SSHStrictHostKeyChecking = %q, want %q", capturedSSHStrictHostKeyChecking, "yes")
		}
	})
}

// TestRunWorkerAttempt_ObservedIssueStatePropagated verifies that
// WorkerResult.ObservedIssueState carries the state returned by the
// per-turn refresh that ended the turn loop.
func TestRunWorkerAttempt_ObservedIssueStatePropagated(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 5

	var turnCount atomic.Int64
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			turn := turnCount.Load()
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				if turn >= 1 {
					result[id] = "Done"
				} else {
					result[id] = "To Do"
				}
			}
			return result, nil
		},
	}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter: tracker,
		AgentAdapter: &mockAgentAdapter{
			runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
				turnCount.Add(1)
				if params.OnEvent != nil {
					params.OnEvent(domain.AgentEvent{Type: domain.EventNotification, Timestamp: time.Now().UTC()})
				}
				return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.ExitKind != WorkerExitNormal {
		t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
	}
	if result.ObservedIssueState != "Done" {
		t.Errorf("ObservedIssueState = %q, want %q", result.ObservedIssueState, "Done")
	}
}

// TestRunWorkerAttempt_ObservedIssueStateEmptyWhenNoRefresh verifies that
// WorkerResult.ObservedIssueState stays empty when the dispatch posture
// does not drive issue state, so the per-turn refresh never runs.
func TestRunWorkerAttempt_ObservedIssueStateEmptyWhenNoRefresh(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 2

	var fetchCalls atomic.Int32
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			fetchCalls.Add(1)
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if fetchCalls.Load() != 0 {
		t.Errorf("FetchIssueStatesByIDs call count = %d, want 0 (review posture drives no issue state)", fetchCalls.Load())
	}
	if result.ObservedIssueState != "" {
		t.Errorf("ObservedIssueState = %q, want empty", result.ObservedIssueState)
	}
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

// --- stopSessionBestEffort log message tests ---

func TestStopSessionBestEffort_LogMessage(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	adapter := &mockAgentAdapter{
		stopSessionFn: func(_ context.Context, _ domain.Session) error {
			return errors.New("connection refused")
		},
	}

	cfg := config.ServiceConfig{
		Agent: config.AgentConfig{ReadTimeoutMS: 1000},
	}

	stopSessionBestEffort(context.Background(), adapter, domain.Session{ID: "s1"}, cfg, logger)

	output := buf.String()
	if !strings.Contains(output, "stop session failed") {
		t.Errorf("want log message %q, got: %s", "stop session failed", output)
	}
	if strings.Contains(output, "StopSession") {
		t.Errorf("log message contains uppercase StopSession, want lowercase: %s", output)
	}
	if !strings.Contains(output, "connection refused") {
		t.Errorf("log output missing error attribute, got: %s", output)
	}
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

// TestRunWorkerAttempt_DispatchTransition covers the dispatch-time
// in-progress transition logic added to the top of RunWorkerAttempt.
func TestRunWorkerAttempt_DispatchTransition(t *testing.T) {
	t.Parallel()

	t.Run("CalledWhenConfigured", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		issue := workerTestIssue()
		RunWorkerAttempt(context.Background(), issue, nil, deps)

		ec.waitResult(t)

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue call count = %d, want 1", len(tracker.transitionCalls))
		}
		if got := tracker.transitionCalls[0].IssueID; got != issue.ID {
			t.Errorf("TransitionIssue IssueID = %q, want %q", got, issue.ID)
		}
		if got := tracker.transitionCalls[0].TargetState; got != "In Progress" {
			t.Errorf("TransitionIssue TargetState = %q, want %q", got, "In Progress")
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeSuccess {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeSuccess)
		}
	})

	t.Run("WorkerProceedsOnFailure", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				return errors.New("tracker forbidden")
			},
		}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)

		// Transition failure must be non-fatal: worker reaches workspace prep.
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty (worker proceeded past transition failure)")
		}

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue call count = %d, want 1", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeError {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeError)
		}
	})

	t.Run("SkippedWhenAbsent", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		// InProgressState is deliberately left empty (zero value).

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		ec.waitResult(t)

		// TransitionIssue must not be called when InProgressState is empty.
		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue call count = %d, want 0", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 0 {
			t.Errorf("dispatchTransitions = %v, want empty", transitions)
		}
	})

	t.Run("DynamicReload", func(t *testing.T) {
		t.Parallel()

		// ConfigFunc returns different InProgressState on each call to
		// simulate a config reload between worker attempts.
		states := []string{"State A", "State B"}
		var callIdx atomic.Int64

		makeConfig := func(tmpDir string) func() config.ServiceConfig {
			return func() config.ServiceConfig {
				idx := int(callIdx.Add(1)) - 1
				if idx >= len(states) {
					idx = len(states) - 1
				}
				cfg := defaultWorkerConfig(tmpDir)
				cfg.Tracker.InProgressState = states[idx]
				cfg.Tracker.ActiveStates = append(cfg.Tracker.ActiveStates, states[idx])
				return cfg
			}
		}

		tmpDir1 := t.TempDir()
		tracker1 := &mockTrackerAdapter{}
		ec1 := newExitCapture()
		deps1 := WorkerDeps{
			TrackerAdapter:         tracker1,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             makeConfig(tmpDir1),
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec1.onExit,
			Logger:                 discardLogger(),
			Metrics:                &spyMetrics{},
		}
		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps1)
		ec1.waitResult(t)

		tmpDir2 := t.TempDir()
		tracker2 := &mockTrackerAdapter{}
		ec2 := newExitCapture()
		deps2 := WorkerDeps{
			TrackerAdapter:         tracker2,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             makeConfig(tmpDir2),
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec2.onExit,
			Logger:                 discardLogger(),
			Metrics:                &spyMetrics{},
		}
		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps2)
		ec2.waitResult(t)

		if len(tracker1.transitionCalls) != 1 {
			t.Fatalf("attempt 1: TransitionIssue call count = %d, want 1", len(tracker1.transitionCalls))
		}
		if got := tracker1.transitionCalls[0].TargetState; got != "State A" {
			t.Errorf("attempt 1: TargetState = %q, want %q", got, "State A")
		}

		if len(tracker2.transitionCalls) != 1 {
			t.Fatalf("attempt 2: TransitionIssue call count = %d, want 1", len(tracker2.transitionCalls))
		}
		if got := tracker2.transitionCalls[0].TargetState; got != "State B" {
			t.Errorf("attempt 2: TargetState = %q, want %q", got, "State B")
		}
	})

	// Skip-when-already-in-progress: dispatch transition must be skipped
	// (no API call, metrics "skipped") when issue.State already matches
	// InProgressState. Tests cover exact match, case-insensitive match,
	// and the "states differ → call proceeds" counterpart.

	t.Run("SkippedWhenAlreadyInTargetState", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		// Issue state exactly equals InProgressState — transition must be skipped.
		issue := workerTestIssue()
		issue.State = "In Progress"

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), issue, nil, deps)
		ec.waitResult(t)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue call count = %d, want 0 (issue already in target state)", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeSkipped {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeSkipped)
		}
	})

	t.Run("SkippedWhenAlreadyInTargetStateCaseInsensitive", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		// issue.State differs only in casing — skip must still apply.
		issue := workerTestIssue()
		issue.State = "in progress"

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), issue, nil, deps)
		ec.waitResult(t)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue call count = %d, want 0 (case-insensitive match)", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeSkipped {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeSkipped)
		}
	})

	t.Run("NotSkippedWhenStatesDiffer", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		// issue.State = "To Do" (default from workerTestIssue) — states differ,
		// so TransitionIssue must be called.
		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		ec.waitResult(t)

		if len(tracker.transitionCalls) != 1 {
			t.Errorf("TransitionIssue call count = %d, want 1 (states differ)", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeSuccess {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeSuccess)
		}
	})

	t.Run("SkippedOnRetryWhenAlreadyInTargetState", func(t *testing.T) {
		t.Parallel()

		// Simulates a continuation retry where the issue was transitioned on the
		// first attempt and stays in InProgressState for the retry attempt.
		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.InProgressState = "In Progress"

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		issue := workerTestIssue()
		issue.State = "In Progress" // already transitioned on prior attempt

		attempt := 2
		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), issue, &attempt, deps)
		ec.waitResult(t)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue call count = %d, want 0 (retry: already in target state)", len(tracker.transitionCalls))
		}

		spy.mu.Lock()
		transitions := append([]string(nil), spy.dispatchTransitions...)
		spy.mu.Unlock()

		if len(transitions) != 1 || transitions[0] != outcomeSkipped {
			t.Errorf("dispatchTransitions = %v, want [%q]", transitions, outcomeSkipped)
		}
	})
}

func TestRunWorkerAttempt_HandoffEvidenceBaselineBoundary(t *testing.T) {
	t.Run("full preparation captures after pre-run hook", func(t *testing.T) {
		root := t.TempDir()
		cfg := defaultWorkerConfig(root)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
		cfg.Hooks.AfterCreate = "git init && git config user.email test@example.com && git config user.name Test && git commit --allow-empty -m initial"
		cfg.Hooks.BeforeRun = "echo prepared > hook-output.txt"
		ec := newExitCapture()

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		})
		result := ec.waitResult(t)

		if result.HandoffEvidenceBaseline == nil {
			t.Fatalf("HandoffEvidenceBaseline is nil; capture error: %v", result.HandoffEvidenceBaselineError)
		}
		change, err := workspace.CompareHandoffEvidenceBaseline(context.Background(), result.WorkspacePath, *result.HandoffEvidenceBaseline)
		if err != nil {
			t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
		}
		if change.CommitMoved || change.WorktreeChanged {
			t.Errorf("pre-run hook output counted as agent work: %+v", change)
		}
	})

	t.Run("agent start runs after baseline capture", func(t *testing.T) {
		root := t.TempDir()
		cfg := defaultWorkerConfig(root)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
		cfg.Hooks.AfterCreate = "git init && git config user.email test@example.com && git config user.name Test && git commit --allow-empty -m initial"
		cfg.Hooks.BeforeRun = "echo prepared > hook-output.txt"
		ec := newExitCapture()

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					if err := os.WriteFile(filepath.Join(params.WorkspacePath, "hook-output.txt"), []byte("changed by agent start\n"), 0o600); err != nil {
						t.Fatalf("update hook output: %v", err)
					}
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		})
		result := ec.waitResult(t)

		if result.HandoffEvidenceBaseline == nil {
			t.Fatalf("HandoffEvidenceBaseline is nil; capture error: %v", result.HandoffEvidenceBaselineError)
		}
		change, err := workspace.CompareHandoffEvidenceBaseline(context.Background(), result.WorkspacePath, *result.HandoffEvidenceBaseline)
		if err != nil {
			t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
		}
		if !change.WorktreeChanged {
			t.Errorf("change = %+v, want StartSession mutation after baseline", change)
		}
	})

	t.Run("ensure-only path reaches the same boundary", func(t *testing.T) {
		root := t.TempDir()
		issue := workerTestIssue()
		ensured, err := workspace.Ensure(root, issue.Identifier)
		if err != nil {
			t.Fatalf("workspace.Ensure: %v", err)
		}
		initGitRepo(t, ensured.Path)
		cfg := defaultWorkerConfig(root)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
		ec := newExitCapture()

		RunWorkerAttempt(context.Background(), issue, nil, WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					if err := os.WriteFile(filepath.Join(params.WorkspacePath, "review-output.txt"), []byte("work\n"), 0o600); err != nil {
						t.Fatalf("write review output: %v", err)
					}
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Posture:                PostureReview,
		})
		result := ec.waitResult(t)

		if result.HandoffEvidenceBaseline == nil {
			t.Fatalf("HandoffEvidenceBaseline is nil; capture error: %v", result.HandoffEvidenceBaselineError)
		}
		change, err := workspace.CompareHandoffEvidenceBaseline(context.Background(), result.WorkspacePath, *result.HandoffEvidenceBaseline)
		if err != nil {
			t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
		}
		if !change.WorktreeChanged {
			t.Errorf("change = %+v, want Ensure-path StartSession mutation", change)
		}
	})
}

func TestRunWorkerAttempt_HandoffEvidencePolicyIsFrozen(t *testing.T) {
	root := t.TempDir()
	cfg := defaultWorkerConfig(root)
	cfg.Agent.MaxTurns = 1
	cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
	cfg.Hooks.AfterCreate = "git init && git config user.email test@example.com && git config user.name Test && git commit --allow-empty -m initial"
	ec := newExitCapture()

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{},
		AgentAdapter: &mockAgentAdapter{
			startSessionFn: func(_ context.Context, _ domain.StartSessionParams) (domain.Session, error) {
				cfg.Tracker.HandoffEvidence = config.HandoffEvidenceOff
				return domain.Session{ID: "sess-1"}, nil
			},
		},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
	})
	result := ec.waitResult(t)

	if got := result.HandoffEvidencePolicy; got != config.HandoffEvidenceObserved {
		t.Errorf("HandoffEvidencePolicy = %q, want frozen %q", got, config.HandoffEvidenceObserved)
	}
	if result.HandoffEvidenceBaseline == nil {
		t.Fatalf("HandoffEvidenceBaseline is nil; capture error: %v", result.HandoffEvidenceBaselineError)
	}
}

func TestRunWorkerAttempt_HandoffEvidenceOffSkipsCapture(t *testing.T) {
	for _, tt := range []struct {
		name           string
		policy         config.HandoffEvidencePolicy
		wantCaptureErr bool
	}{
		{name: "observed attempts capture", policy: config.HandoffEvidenceObserved, wantCaptureErr: true},
		{name: "off skips capture", policy: config.HandoffEvidenceOff, wantCaptureErr: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultWorkerConfig(t.TempDir())
			cfg.Agent.MaxTurns = 1
			cfg.Tracker.HandoffEvidence = tt.policy
			ec := newExitCapture()

			RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
				TrackerAdapter:         &mockTrackerAdapter{},
				AgentAdapter:           &mockAgentAdapter{},
				ConfigFunc:             func() config.ServiceConfig { return cfg },
				PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
				OnEvent:                func(_ string, _ domain.AgentEvent) {},
				OnExit:                 ec.onExit,
				Logger:                 discardLogger(),
			})
			result := ec.waitResult(t)

			if result.HandoffEvidenceBaseline != nil {
				t.Errorf("HandoffEvidenceBaseline = %+v, want nil for non-Git workspace", result.HandoffEvidenceBaseline)
			}
			if got := result.HandoffEvidenceBaselineError != nil; got != tt.wantCaptureErr {
				t.Errorf("capture error present = %v, want %v (error: %v)", got, tt.wantCaptureErr, result.HandoffEvidenceBaselineError)
			}
		})
	}
}

func TestRunWorkerAttempt_AfterRunCommitPushCountsAsHandoffWork(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	cfg := defaultWorkerConfig(root)
	cfg.Agent.MaxTurns = 1
	cfg.Tracker.HandoffEvidence = config.HandoffEvidenceObserved
	cfg.Hooks.AfterCreate = fmt.Sprintf(
		"git init && git config user.email test@example.com && git config user.name Test && git commit --allow-empty -m initial && git branch -M main && git remote add origin %q && git push -u origin main",
		filepath.ToSlash(remote),
	)
	cfg.Hooks.AfterRun = "git add agent-output.txt && git commit -m after-run && git push origin HEAD"
	ec := newExitCapture()
	var agentWorkspace string

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{},
		AgentAdapter: &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				agentWorkspace = params.WorkspacePath
				return domain.Session{ID: "sess-1"}, nil
			},
			runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
				if err := os.WriteFile(filepath.Join(agentWorkspace, "agent-output.txt"), []byte("work\n"), 0o600); err != nil {
					t.Fatalf("write agent output: %v", err)
				}
				return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
	})
	result := ec.waitResult(t)

	if result.HandoffEvidenceBaseline == nil {
		t.Fatalf("HandoffEvidenceBaseline is nil; capture error: %v", result.HandoffEvidenceBaselineError)
	}
	if got := strings.TrimSpace(handoffEvidenceGitOutput(t, result.WorkspacePath, "status", "--porcelain")); got != "" {
		t.Fatalf("workspace is dirty after after_run commit/push: %q", got)
	}
	change, err := workspace.CompareHandoffEvidenceBaseline(context.Background(), result.WorkspacePath, *result.HandoffEvidenceBaseline)
	if err != nil {
		t.Fatalf("CompareHandoffEvidenceBaseline: %v", err)
	}
	if !change.CommitMoved {
		t.Fatalf("change = %+v, want commit movement from after_run", change)
	}

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, result.IssueID, "To Do")
	params := handoffEvidenceExitParams(t, store, tracker, &spyMetrics{})
	params.ActiveStates = []string{"To Do"}
	HandleWorkerExit(state, result, params)
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1 for clean after_run commit", len(tracker.transitionCalls))
	}
}

func handoffEvidenceGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

// TestRunWorkerAttempt_MCPConfig covers MCP config generation, which
// is skipped when WorkflowPath is empty, and the generated config path is
// forwarded to StartSessionParams when WorkflowPath is non-empty.
func TestRunWorkerAttempt_MCPConfig(t *testing.T) {
	t.Parallel()

	t.Run("skipped_when_workflow_path_empty", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		var capturedMCPConfigPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedMCPConfigPath.Store(params.MCPConfigPath)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			WorkflowPath:           "", // empty → MCP config skipped
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		got, _ := capturedMCPConfigPath.Load().(string)
		if got != "" {
			t.Errorf("StartSessionParams.MCPConfigPath = %q, want empty (workflow path not set)", got)
		}

		// Verify the .sortie directory was NOT created.
		sortieDir := filepath.Join(result.WorkspacePath, ".sortie")
		if _, err := os.Stat(sortieDir); !os.IsNotExist(err) {
			t.Errorf(".sortie dir %q exists, want absent when MCP config skipped", sortieDir)
		}
	})

	t.Run("mcp_config_path_populated_in_start_session_params", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		var capturedMCPConfigPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedMCPConfigPath.Store(params.MCPConfigPath)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			WorkflowPath:           "/fake/WORKFLOW.md", // non-empty → MCP config generated
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		got, _ := capturedMCPConfigPath.Load().(string)
		if got == "" {
			t.Fatal("StartSessionParams.MCPConfigPath is empty, want non-empty")
		}
		if !strings.HasSuffix(got, filepath.Join(".sortie", "mcp.json")) {
			t.Errorf("MCPConfigPath = %q, want suffix %q", got, filepath.Join(".sortie", "mcp.json"))
		}

		// Confirm the file actually exists on disk.
		if _, err := os.Stat(got); err != nil {
			t.Errorf("mcp.json not found at %q: %v", got, err)
		}

		// Confirm the gitignore was also written alongside mcp.json.
		gitignorePath := filepath.Join(filepath.Dir(got), ".gitignore")
		giData, err := os.ReadFile(gitignorePath)
		if err != nil {
			t.Fatalf(".sortie/.gitignore not found: %v", err)
		}
		if string(giData) != "*\n" {
			t.Errorf(".sortie/.gitignore = %q, want %q", string(giData), "*\n")
		}
	})

	// Error path: GenerateMCPConfig failure must be fatal to the attempt.
	// Triggered by an operator config that contains the reserved name "sortie-tools".
	t.Run("generate_fails_fatal_to_attempt", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		// Operator config contains a reserved "sortie-tools" entry → name collision.
		operatorPath := filepath.Join(tmpDir, "op-mcp.json")
		collision := map[string]any{
			"mcpServers": map[string]any{
				"sortie-tools": map[string]any{"type": "stdio", "command": "/other"},
			},
		}
		collisionData, _ := json.Marshal(collision)
		if err := os.WriteFile(operatorPath, collisionData, 0o600); err != nil {
			t.Fatalf("WriteFile operator config: %v", err)
		}

		cfg.Extensions = map[string]any{
			"mock": map[string]any{"mcp_config": operatorPath},
		}

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
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			WorkflowPath:           "/fake/WORKFLOW.md",
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitError {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
		}
		if result.Error == nil {
			t.Fatal("Error is nil, want non-nil")
		}
		if !strings.Contains(result.Error.Error(), "mcp config generation") {
			t.Errorf("Error = %q, want to contain %q", result.Error, "mcp config generation")
		}
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty (workspace was prepared before Phase 1.5)")
		}
		if startCalled.Load() {
			t.Error("StartSession was called, want no call after mcp config generation failure")
		}
	})

	// Extension lookup: operator mcp_config from cfg.Extensions[agentKind]
	// must be merged into the generated .sortie/mcp.json.
	t.Run("operator_config_merged_from_extensions", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		// Write a valid operator config with a distinct server entry.
		operatorPath := filepath.Join(tmpDir, "op-mcp.json")
		opConfig := map[string]any{
			"mcpServers": map[string]any{
				"my-custom-tool": map[string]any{"type": "stdio", "command": "/usr/bin/my-tool"},
			},
		}
		opData, _ := json.Marshal(opConfig)
		if err := os.WriteFile(operatorPath, opData, 0o600); err != nil {
			t.Fatalf("WriteFile operator config: %v", err)
		}

		cfg.Extensions = map[string]any{
			"mock": map[string]any{"mcp_config": operatorPath},
		}

		var capturedMCPConfigPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedMCPConfigPath.Store(params.MCPConfigPath)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			WorkflowPath:           "/fake/WORKFLOW.md",
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mcpPath, _ := capturedMCPConfigPath.Load().(string)
		if mcpPath == "" {
			t.Fatal("MCPConfigPath is empty")
		}

		rawData, err := os.ReadFile(mcpPath)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", mcpPath, err)
		}
		var merged map[string]any
		if err := json.Unmarshal(rawData, &merged); err != nil {
			t.Fatalf("Unmarshal mcp.json: %v", err)
		}
		servers, ok := merged["mcpServers"].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers is not an object: %v", merged["mcpServers"])
		}
		if _, ok := servers["sortie-tools"]; !ok {
			t.Error("sortie-tools missing from merged config")
		}
		if _, ok := servers["my-custom-tool"]; !ok {
			t.Error("my-custom-tool missing from merged config (operator entry was not preserved)")
		}
	})

	// Path resolution: a relative mcp_config extension value must be
	// resolved relative to the directory containing deps.WorkflowPath.
	t.Run("relative_operator_path_resolved_from_workflow_dir", func(t *testing.T) {
		t.Parallel()

		// workflowDir acts as the directory containing WORKFLOW.md.
		workflowDir := t.TempDir()
		workspaceTmpDir := t.TempDir()
		cfg := defaultWorkerConfig(workspaceTmpDir)
		cfg.Agent.MaxTurns = 1

		// Operator config placed in workflowDir with a relative name.
		relName := "op.json"
		opData, _ := json.Marshal(map[string]any{
			"mcpServers": map[string]any{
				"relative-tool": map[string]any{"type": "stdio", "command": "/bin/rel"},
			},
		})
		if err := os.WriteFile(filepath.Join(workflowDir, relName), opData, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg.Extensions = map[string]any{
			// Relative path — worker must resolve it via filepath.Dir(WorkflowPath).
			"mock": map[string]any{"mcp_config": relName},
		}

		var capturedMCPConfigPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedMCPConfigPath.Store(params.MCPConfigPath)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			// WorkflowPath is inside workflowDir; the file itself need not exist.
			WorkflowPath: filepath.Join(workflowDir, "WORKFLOW.md"),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mcpPath, _ := capturedMCPConfigPath.Load().(string)
		rawData, err := os.ReadFile(mcpPath)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", mcpPath, err)
		}
		var merged map[string]any
		if err := json.Unmarshal(rawData, &merged); err != nil {
			t.Fatalf("Unmarshal mcp.json: %v", err)
		}
		servers, _ := merged["mcpServers"].(map[string]any)
		if _, ok := servers["relative-tool"]; !ok {
			t.Errorf("relative-tool missing from merged config: relative operator path was not resolved from workflow dir")
		}
	})

	// Env forwarding: deps.DBPath must appear as SORTIE_DB_PATH in the
	// generated config's env block.
	t.Run("dbpath_forwarded_to_mcp_config_env", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		const testDBPath = "/var/sortie/test.sqlite"

		var capturedMCPConfigPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					capturedMCPConfigPath.Store(params.MCPConfigPath)
					return domain.Session{ID: "sess-1"}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			WorkflowPath:           "/fake/WORKFLOW.md",
			DBPath:                 testDBPath,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mcpPath, _ := capturedMCPConfigPath.Load().(string)
		rawData, err := os.ReadFile(mcpPath)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", mcpPath, err)
		}
		var mcpCfg map[string]any
		if err := json.Unmarshal(rawData, &mcpCfg); err != nil {
			t.Fatalf("Unmarshal mcp.json: %v", err)
		}
		servers, _ := mcpCfg["mcpServers"].(map[string]any)
		sortieEntry, _ := servers["sortie-tools"].(map[string]any)
		env, _ := sortieEntry["env"].(map[string]any)

		if got := env["SORTIE_DB_PATH"]; got != testDBPath {
			t.Errorf("env[SORTIE_DB_PATH] = %q, want %q", got, testDBPath)
		}
	})
}

func TestBuildDispatchComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		agentKind    string
		attempt      int
		wantContains []string
	}{
		{
			name:         "first dispatch attempt 1",
			agentKind:    "claude-code",
			attempt:      1,
			wantContains: []string{"Sortie session started.", "claude-code", "Attempt: 1", "Session: pending", "Workspace: pending"},
		},
		{
			name:         "retry attempt 3",
			agentKind:    "claude-code",
			attempt:      3,
			wantContains: []string{"Attempt: 3"},
		},
		{
			name:         "attempt 0 propagated as-is",
			agentKind:    "mock",
			attempt:      0,
			wantContains: []string{"Attempt: 0"},
		},
		{
			name:         "agent kind included verbatim",
			agentKind:    "mock-agent",
			attempt:      1,
			wantContains: []string{"mock-agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildDispatchComment(tt.agentKind, tt.attempt)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildDispatchComment(%q, %d) missing %q\ngot: %q",
						tt.agentKind, tt.attempt, want, got)
				}
			}
		})
	}
}

// TestRunWorkerAttempt_DispatchComment covers the dispatch comment path
// in RunWorkerAttempt: CommentIssue call gating, metrics recording, and
// non-fatal error handling.
func TestRunWorkerAttempt_DispatchComment(t *testing.T) {
	t.Parallel()

	t.Run("CalledWhenOnDispatchEnabled", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.Comments.OnDispatch = true

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		issue := workerTestIssue()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), issue, nil, deps)
		ec.waitResult(t)

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
		}
		if got := tracker.commentCalls[0].IssueID; got != issue.ID {
			t.Errorf("CommentIssue IssueID = %q, want %q", got, issue.ID)
		}
		if tracker.commentCalls[0].Text == "" {
			t.Error("CommentIssue Text is empty, want non-empty dispatch comment")
		}

		spy.mu.Lock()
		comments := append([]trackerCommentCall(nil), spy.trackerComments...)
		spy.mu.Unlock()

		if len(comments) < 1 {
			t.Fatalf("IncTrackerComments call count = %d, want >= 1", len(comments))
		}
		found := false
		for _, c := range comments {
			if c.lifecycle == "dispatch" && c.result == "success" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IncTrackerComments(\"dispatch\", \"success\") not recorded; got %v", comments)
		}
	})

	t.Run("NotCalledWhenOnDispatchDisabled", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.Comments.OnDispatch = false // explicit zero value

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		ec.waitResult(t)

		if len(tracker.commentCalls) != 0 {
			t.Errorf("CommentIssue call count = %d, want 0 (OnDispatch disabled)", len(tracker.commentCalls))
		}

		spy.mu.Lock()
		comments := append([]trackerCommentCall(nil), spy.trackerComments...)
		spy.mu.Unlock()

		for _, c := range comments {
			if c.lifecycle == "dispatch" {
				t.Errorf("IncTrackerComments recorded dispatch metric when OnDispatch is false: %v", c)
			}
		}
	})

	t.Run("ErrorIsNonFatal", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.Comments.OnDispatch = true

		spy := &spyMetrics{}
		tracker := &mockTrackerAdapter{
			commentIssueFn: func(_ context.Context, _, _ string) error {
				return errors.New("comment API unavailable")
			},
		}
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                spy,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		result := ec.waitResult(t)

		// Comment failure must be non-fatal: worker reaches normal exit.
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q (comment error must be non-fatal)", result.ExitKind, WorkerExitNormal)
		}
		if result.Error != nil {
			t.Errorf("Error = %v, want nil (comment error must be non-fatal)", result.Error)
		}

		spy.mu.Lock()
		comments := append([]trackerCommentCall(nil), spy.trackerComments...)
		spy.mu.Unlock()

		found := false
		for _, c := range comments {
			if c.lifecycle == "dispatch" && c.result == "error" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IncTrackerComments(\"dispatch\", \"error\") not recorded; got %v", comments)
		}
	})

	t.Run("AttemptIncludedInCommentText", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Tracker.Comments.OnDispatch = true

		tracker := &mockTrackerAdapter{}
		ec := newExitCapture()

		attempt := intPtr(2)

		deps := WorkerDeps{
			TrackerAdapter:         tracker,
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			Metrics:                &domain.NoopMetrics{},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), attempt, deps)
		ec.waitResult(t)

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
		}
		if !strings.Contains(tracker.commentCalls[0].Text, "2") {
			t.Errorf("CommentIssue Text = %q, want attempt number 2 present", tracker.commentCalls[0].Text)
		}
	})
}

// TestRunWorkerAttempt_A2OStatusSignal covers the A2O status file integration
// inside the RunWorkerAttempt turn loop: blocked/needs-human-review signals
// trigger a soft stop, absent signals allow the loop to continue, and
// stale status files from a previous run are cleaned before dispatch.
func TestRunWorkerAttempt_A2OStatusSignal(t *testing.T) {
	t.Parallel()

	t.Run("blocked_signal_exits_with_soft_stop", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		// Capture the workspace path from StartSession to write the status
		// file in RunTurn without knowing the path ahead of time.
		var wsPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					wsPath.Store(params.WorkspacePath)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					if p, ok := wsPath.Load().(string); ok && p != "" {
						statusDir := filepath.Join(p, ".sortie")
						if err := os.MkdirAll(statusDir, 0o755); err == nil {
							_ = os.WriteFile(filepath.Join(statusDir, "status"), []byte("blocked\n"), 0o644)
						}
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if !result.SoftStop {
			t.Error("SoftStop = false, want true (agent wrote 'blocked')")
		}
		if result.SoftStopReason != "blocked" {
			t.Errorf("SoftStopReason = %q, want %q", result.SoftStopReason, "blocked")
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1 (stopped after first turn with blocked signal)", result.TurnsCompleted)
		}
		if result.WorkspacePath == "" {
			t.Error("WorkspacePath is empty, want non-empty")
		}
	})

	t.Run("needs_human_review_signal_exits_with_soft_stop", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var wsPath atomic.Value
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					wsPath.Store(params.WorkspacePath)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
					if p, ok := wsPath.Load().(string); ok && p != "" {
						statusDir := filepath.Join(p, ".sortie")
						if err := os.MkdirAll(statusDir, 0o755); err == nil {
							_ = os.WriteFile(filepath.Join(statusDir, "status"), []byte("needs-human-review\n"), 0o644)
						}
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if !result.SoftStop {
			t.Error("SoftStop = false, want true (agent wrote 'needs-human-review')")
		}
		if result.SoftStopReason != "needs-human-review" {
			t.Errorf("SoftStopReason = %q, want %q", result.SoftStopReason, "needs-human-review")
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1", result.TurnsCompleted)
		}
	})

	t.Run("absent_signal_does_not_trigger_soft_stop", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 2

		var fetchStatesCalled atomic.Int64
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					fetchStatesCalled.Add(1)
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "To Do"
					}
					return result, nil
				},
			},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		if result.SoftStop {
			t.Error("SoftStop = true, want false (no status file written)")
		}
		if result.SoftStopReason != "" {
			t.Errorf("SoftStopReason = %q, want empty", result.SoftStopReason)
		}
		if result.TurnsCompleted != 2 {
			t.Errorf("TurnsCompleted = %d, want 2 (ran all max_turns)", result.TurnsCompleted)
		}
		// FetchIssueStatesByIDs was called (signal check happens before state refresh).
		if fetchStatesCalled.Load() == 0 {
			t.Error("FetchIssueStatesByIDs not called; want called when no status signal")
		}
	})

	t.Run("stale_status_cleaned_before_dispatch", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		// Pre-create the workspace and write a stale status file to simulate
		// a previous worker run that left "blocked" behind.
		wsPath := filepath.Join(tmpDir, "TEST-1")
		statusDir := filepath.Join(wsPath, ".sortie")
		if err := os.MkdirAll(statusDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		staleStatus := filepath.Join(statusDir, "status")
		if err := os.WriteFile(staleStatus, []byte("blocked\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(stale status): %v", err)
		}

		ec := newExitCapture()

		deps := WorkerDeps{
			// runTurnFn writes nothing; the stale file should be gone by now.
			TrackerAdapter:         &mockTrackerAdapter{},
			AgentAdapter:           &mockAgentAdapter{},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}
		// The stale file was cleaned by PreRunFunc before the turn ran,
		// so ReadStatusFile finds nothing → SoftStop must be false.
		if result.SoftStop {
			t.Error("SoftStop = true, want false (stale status should have been cleaned by PreRunFunc)")
		}
		if result.TurnsCompleted != 1 {
			t.Errorf("TurnsCompleted = %d, want 1", result.TurnsCompleted)
		}
	})
}

// TestRuntimeStatusSuffixInjection verifies the first-turn-only injection of
// prompt.RuntimeStatusSuffix into the prompt passed to RunTurn, including
// ordering relative to tool advertisement and the absence of the suffix on
// continuation turns.
func TestRuntimeStatusSuffixInjection(t *testing.T) {
	t.Parallel()

	// writeSoftStop writes "blocked" to the .sortie/status file inside the
	// given workspace path so the worker exits cleanly after one turn.
	writeSoftStop := func(wsPath string) {
		statusDir := filepath.Join(wsPath, ".sortie")
		if err := os.MkdirAll(statusDir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(statusDir, "status"), []byte("blocked\n"), 0o644)
		}
	}

	t.Run("first_turn_contains_suffix", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var wsPath atomic.Value
		var capturedPrompt string
		var mu sync.Mutex
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					wsPath.Store(params.WorkspacePath)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					if p, ok := wsPath.Load().(string); ok && p != "" {
						writeSoftStop(p)
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		ec.waitResult(t)

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if !strings.Contains(p, prompt.RuntimeStatusSuffix) {
			t.Errorf("first-turn prompt missing RuntimeStatusSuffix:\n%s", p)
		}
	})

	t.Run("suffix_after_tool_advertisement", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5
		cfg.Tracker.Project = "TESTPROJ"

		var wsPath atomic.Value
		var capturedPrompt string
		var mu sync.Mutex
		ec := newExitCapture()

		reg := domain.NewToolRegistry()
		reg.Register(&stubAgentTool{toolName: "tracker_api", desc: "Query issues"})

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					wsPath.Store(params.WorkspacePath)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					if p, ok := wsPath.Load().(string); ok && p != "" {
						writeSoftStop(p)
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "do work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			ToolRegistry:           reg,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		ec.waitResult(t)

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		toolIdx := strings.Index(p, "## Available Sortie tools")
		if toolIdx < 0 {
			t.Fatalf("first-turn prompt missing tool advertisement header:\n%s", p)
		}
		suffixIdx := strings.Index(p, prompt.RuntimeStatusSuffix)
		if suffixIdx < 0 {
			t.Fatalf("first-turn prompt missing RuntimeStatusSuffix:\n%s", p)
		}
		if suffixIdx <= toolIdx {
			t.Errorf("RuntimeStatusSuffix (idx=%d) must appear after tool advertisement (idx=%d)", suffixIdx, toolIdx)
		}
	})

	t.Run("continuation_turn_omits_suffix", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 2

		var prompts [2]string
		var turnCounter atomic.Int64
		var mu sync.Mutex
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					n := turnCounter.Add(1)
					mu.Lock()
					if n <= 2 {
						prompts[n-1] = params.Prompt
					}
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "turn={{ .run.turn_number }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.TurnsCompleted != 2 {
			t.Fatalf("TurnsCompleted = %d, want 2", result.TurnsCompleted)
		}

		mu.Lock()
		p1, p2 := prompts[0], prompts[1]
		mu.Unlock()

		if !strings.Contains(p1, prompt.RuntimeStatusSuffix) {
			t.Errorf("turn 1 prompt missing RuntimeStatusSuffix:\n%s", p1)
		}
		if strings.Contains(p2, prompt.RuntimeStatusSuffix) {
			t.Errorf("turn 2 prompt must not contain RuntimeStatusSuffix:\n%s", p2)
		}
	})

	t.Run("empty_template_first_turn_contains_suffix", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 5

		var wsPath atomic.Value
		var capturedPrompt string
		var mu sync.Mutex
		ec := newExitCapture()

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					wsPath.Store(params.WorkspacePath)
					return domain.Session{ID: "sess-1"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					if p, ok := wsPath.Load().(string); ok && p != "" {
						writeSoftStop(p)
					}
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		ec.waitResult(t)

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if !strings.Contains(p, prompt.RuntimeStatusSuffix) {
			t.Errorf("first-turn prompt missing RuntimeStatusSuffix with empty template:\n%s", p)
		}
	})
}

// --- PromptTemplateByIDFunc dispatch ID tests ---

// TestRunWorkerAttempt_PromptTemplateByIDFunc_ForwardsTemplateID verifies that
// RunWorkerAttempt calls PromptTemplateByIDFunc with the TemplateID from
// WorkerDeps, allowing the frozen dispatch selection to resolve the correct
// per-rule template.
func TestRunWorkerAttempt_PromptTemplateByIDFunc_ForwardsTemplateID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)

	const wantTemplateID = "/abs/prompts/bug.md"

	var capturedID string
	ruleTemplate := mustParseTemplate(t, "rule prompt: {{ .issue.title }}")

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "Done"
				}
				return result, nil
			},
		},
		AgentAdapter: &mockAgentAdapter{},
		ConfigFunc:   func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(id string) *prompt.Template {
			capturedID = id
			return ruleTemplate
		},
		TemplateID: wantTemplateID,
		OnEvent:    func(_ string, _ domain.AgentEvent) {},
		OnExit:     ec.onExit,
		Logger:     discardLogger(),
		Metrics:    &domain.NoopMetrics{},
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if capturedID != wantTemplateID {
		t.Errorf("PromptTemplateByIDFunc(id) = %q, want %q", capturedID, wantTemplateID)
	}
}

// TestRunWorkerAttempt_PromptTemplateByIDFunc_NilTemplateExitsWithError verifies
// that when PromptTemplateByIDFunc returns nil for the configured TemplateID,
// the worker calls OnExit with WorkerExitError rather than panicking.
func TestRunWorkerAttempt_PromptTemplateByIDFunc_NilTemplateExitsWithError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)

	const badTemplateID = "/nonexistent/template.md"

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{},
		AgentAdapter:   &mockAgentAdapter{},
		ConfigFunc:     func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template {
			return nil
		},
		TemplateID: badTemplateID,
		OnEvent:    func(_ string, _ domain.AgentEvent) {},
		OnExit:     ec.onExit,
		Logger:     discardLogger(),
		Metrics:    &domain.NoopMetrics{},
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.ExitKind != WorkerExitError {
		t.Errorf("WorkerResult.ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
	}
	if result.Error == nil {
		t.Error("WorkerResult.Error = nil, want non-nil error for nil template")
	}
}

// TestRunWorkerAttempt_SessionToolRegistryFunc covers the injected
// SessionToolRegistryFunc seam: all five tool headings in the first-turn
// advertisement, the advertised side extracted from the rendered string,
// suffix ordering and continuation-turn omission preserved, and a builder
// error degrading without failing the attempt.
func TestRunWorkerAttempt_SessionToolRegistryFunc(t *testing.T) {
	t.Parallel()

	// fakeAllToolsRegistry builds a registry containing all five expected
	// per-session tools as stub entries.
	fakeAllToolsRegistry := func() *domain.ToolRegistry {
		reg := domain.NewToolRegistry()
		for _, name := range []string{
			"tracker_api",
			"sortie_status",
			"workspace_history",
			"cost_budget",
			"notify_operator",
		} {
			reg.Register(&stubAgentTool{toolName: name, desc: "stub description for " + name})
		}
		return reg
	}

	t.Run("injected_builder_all_five_tools_advertised", func(t *testing.T) {
		// The first-turn prompt contains a ### heading for each of the
		// five per-session tools when the injected builder returns all five.
		// Advertised side: names are extracted from the rendered string,
		// not from registry.List(), so a buildToolAdvertisement regression
		// would be caught here.
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.Project = "TESTPROJ"

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			SessionToolRegistryFunc: func(_ context.Context, _, _, _ string) (*domain.ToolRegistry, error) {
				return fakeAllToolsRegistry(), nil
			},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.ExitKind != WorkerExitNormal {
			t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
		}

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		wantHeadings := []string{
			"### tracker_api",
			"### sortie_status",
			"### workspace_history",
			"### cost_budget",
			"### notify_operator",
		}
		for _, heading := range wantHeadings {
			if !strings.Contains(p, heading) {
				t.Errorf("first-turn prompt missing heading %q:\n%s", heading, p)
			}
		}

		// Confirm the advertisement section header is present.
		if !strings.Contains(p, "## Available Sortie tools") {
			t.Errorf("first-turn prompt missing advertisement header:\n%s", p)
		}
	})

	t.Run("injected_builder_suffix_after_advertisement", func(t *testing.T) {
		// RuntimeStatusSuffix appears after the tool advertisement when
		// SessionToolRegistryFunc is injected, preserving suffix ordering.
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.Project = "TESTPROJ"

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			SessionToolRegistryFunc: func(_ context.Context, _, _, _ string) (*domain.ToolRegistry, error) {
				return fakeAllToolsRegistry(), nil
			},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		ec.waitResult(t)

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		advIdx := strings.Index(p, "## Available Sortie tools")
		if advIdx < 0 {
			t.Fatalf("first-turn prompt missing tool advertisement header:\n%s", p)
		}
		suffixIdx := strings.Index(p, prompt.RuntimeStatusSuffix)
		if suffixIdx < 0 {
			t.Fatalf("first-turn prompt missing RuntimeStatusSuffix:\n%s", p)
		}
		if suffixIdx <= advIdx {
			t.Errorf("RuntimeStatusSuffix (idx=%d) must appear after advertisement (idx=%d)", suffixIdx, advIdx)
		}
	})

	t.Run("injected_builder_no_advertisement_on_continuation_turns", func(t *testing.T) {
		// Continuation turns still omit the advertisement when
		// SessionToolRegistryFunc is injected.
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 3
		cfg.Tracker.Project = "TESTPROJ"

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
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "turn={{ .run.turn_number }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 discardLogger(),
			SessionToolRegistryFunc: func(_ context.Context, _, _, _ string) (*domain.ToolRegistry, error) {
				return fakeAllToolsRegistry(), nil
			},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)
		if result.TurnsCompleted != 3 {
			t.Fatalf("TurnsCompleted = %d, want 3", result.TurnsCompleted)
		}

		mu.Lock()
		prompts := make([]string, len(capturedPrompts))
		copy(prompts, capturedPrompts)
		mu.Unlock()

		if len(prompts) != 3 {
			t.Fatalf("captured %d prompts, want 3", len(prompts))
		}

		// Turn 1 must have the advertisement.
		if !strings.Contains(prompts[0], "## Available Sortie tools") {
			t.Errorf("turn 1 prompt missing tool advertisement:\n%s", prompts[0])
		}

		// Turns 2 and 3 must NOT have the advertisement.
		for i := 1; i < len(prompts); i++ {
			if strings.Contains(prompts[i], "## Available Sortie tools") {
				t.Errorf("turn %d prompt must not contain tool advertisement:\n%s", i+1, prompts[i])
			}
		}
	})

	t.Run("injected_builder_error_degrades_no_advertisement", func(t *testing.T) {
		// Degrade path: when SessionToolRegistryFunc returns an error the
		// worker logs a Warn and renders no advertisement section, still appends
		// RuntimeStatusSuffix, and does not fail the attempt.
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:             func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                func(_ string, _ domain.AgentEvent) {},
			OnExit:                 ec.onExit,
			Logger:                 logger,
			SessionToolRegistryFunc: func(_ context.Context, _, _, _ string) (*domain.ToolRegistry, error) {
				return nil, errors.New("simulated builder failure")
			},
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)

		result := ec.waitResult(t)

		// Attempt must not fail.
		if result.ExitKind != WorkerExitNormal {
			t.Errorf("ExitKind = %q, want %q (degrade not fail)", result.ExitKind, WorkerExitNormal)
		}

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		// No advertisement section rendered.
		if strings.Contains(p, "## Available Sortie tools") {
			t.Errorf("first-turn prompt must not contain tool advertisement after builder error:\n%s", p)
		}

		// RuntimeStatusSuffix must still be present.
		if !strings.Contains(p, prompt.RuntimeStatusSuffix) {
			t.Errorf("first-turn prompt missing RuntimeStatusSuffix after builder error:\n%s", p)
		}

		// Warn must have been logged.
		if !strings.Contains(logBuf.String(), "failed to build session tool advertisement") {
			t.Errorf("expected Warn log not found; got:\n%s", logBuf.String())
		}
	})

	t.Run("nil_builder_falls_back_to_tool_registry", func(t *testing.T) {
		// When SessionToolRegistryFunc is nil the worker falls back to the
		// static ToolRegistry (existing behavior preserved).
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		cfg.Agent.MaxTurns = 1
		cfg.Tracker.Project = "TESTPROJ"

		ec := newExitCapture()
		var capturedPrompt string
		var mu sync.Mutex

		reg := domain.NewToolRegistry()
		reg.Register(&stubAgentTool{toolName: "tracker_api", desc: "fallback tool"})

		deps := WorkerDeps{
			TrackerAdapter: &mockTrackerAdapter{},
			AgentAdapter: &mockAgentAdapter{
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					mu.Lock()
					capturedPrompt = params.Prompt
					mu.Unlock()
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			},
			ConfigFunc:              func() config.ServiceConfig { return cfg },
			PromptTemplateByIDFunc:  func(_ string) *prompt.Template { return mustParseTemplate(t, "work on {{ .issue.title }}") },
			OnEvent:                 func(_ string, _ domain.AgentEvent) {},
			OnExit:                  ec.onExit,
			Logger:                  discardLogger(),
			ToolRegistry:            reg,
			SessionToolRegistryFunc: nil,
		}

		RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
		ec.waitResult(t)

		mu.Lock()
		p := capturedPrompt
		mu.Unlock()

		if !strings.Contains(p, "### tracker_api") {
			t.Errorf("first-turn prompt missing fallback tool heading \"### tracker_api\":\n%s", p)
		}
	})
}

// --- Read-only (label-review) dispatch tests ---

// TestRunWorkerAttempt_ReadOnly_NoCloneWorkspace verifies that a read-only
// attempt creates its workspace via workspace.Ensure: the directory exists,
// but neither after_create nor before_run runs even when both are
// configured (A4, A9).
func TestRunWorkerAttempt_ReadOnly_NoCloneWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks use touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	afterCreateMarker := filepath.Join(tmpDir, "after_create_marker")
	beforeRunMarker := filepath.Join(tmpDir, "before_run_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.Hooks.AfterCreate = fmt.Sprintf("touch %s", afterCreateMarker)
	cfg.Hooks.BeforeRun = fmt.Sprintf("touch %s", beforeRunMarker)

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.WorkspacePath == "" {
		t.Fatal("WorkspacePath is empty, want a scratch workspace to be created")
	}
	if _, err := os.Stat(result.WorkspacePath); err != nil {
		t.Errorf("workspace directory not found: %v", err)
	}
	if _, err := os.Stat(afterCreateMarker); err == nil {
		t.Error("after_create hook ran for a read-only dispatch; want no hook execution")
	}
	if _, err := os.Stat(beforeRunMarker); err == nil {
		t.Error("before_run hook ran for a read-only dispatch; want no hook execution")
	}
}

// TestRunWorkerAttempt_ReadOnly_StaleStatusCleaned verifies that the
// read-only path clears a stale .sortie/status left in the reused per-issue
// workspace, so a prior recognized signal does not end the review on turn
// one. The normal path does this via PreRunFunc; the read-only path must do
// the same best-effort cleanup after workspace.Ensure.
func TestRunWorkerAttempt_ReadOnly_StaleStatusCleaned(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 1

	// Pre-create the reused workspace with a stale "blocked" status from a
	// prior session's exit.
	wsPath := filepath.Join(tmpDir, "TEST-1")
	statusDir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(statusDir, "status"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(stale status): %v", err)
	}

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.SoftStop {
		t.Error("SoftStop = true, want false (stale status should have been cleaned after workspace.Ensure)")
	}
	if result.ExitKind != WorkerExitNormal {
		t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
	}
	if result.TurnsCompleted != 1 {
		t.Errorf("TurnsCompleted = %d, want 1 (the read-only session ran a turn instead of soft-stopping)", result.TurnsCompleted)
	}
}

// TestRunWorkerAttempt_ReadOnly_SuppressesInProgressTransition verifies that
// a read-only attempt never calls TransitionIssue even when
// cfg.Tracker.InProgressState is set (A4).
func TestRunWorkerAttempt_ReadOnly_SuppressesInProgressTransition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Tracker.InProgressState = "In Progress"

	tracker := &mockTrackerAdapter{}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue call count = %d, want 0 (a read-only review changes no issue state)", len(tracker.transitionCalls))
	}
}

// TestRunWorkerAttempt_ReadOnly_SuppressesDispatchComment verifies that a
// read-only attempt never posts the dispatch comment even when
// cfg.Tracker.Comments.OnDispatch is true (A4).
func TestRunWorkerAttempt_ReadOnly_SuppressesDispatchComment(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Tracker.Comments.OnDispatch = true

	tracker := &mockTrackerAdapter{}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (a read-only review is not a work claim)", len(tracker.commentCalls))
	}
}

// TestRunWorkerAttempt_ReadOnly_SuppressesPerTurnRefresh verifies that a
// read-only attempt never calls FetchIssueStatesByIDs during the turn loop;
// the loop terminates via max_turns instead (A4).
func TestRunWorkerAttempt_ReadOnly_SuppressesPerTurnRefresh(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 3

	var fetchCalls atomic.Int32
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			fetchCalls.Add(1)
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if fetchCalls.Load() != 0 {
		t.Errorf("FetchIssueStatesByIDs call count = %d, want 0 (read-only loop rests on max_turns/status file only)", fetchCalls.Load())
	}
	if result.TurnsCompleted != cfg.Agent.MaxTurns {
		t.Errorf("TurnsCompleted = %d, want %d (loop terminates via max_turns, not tracker-state gating)",
			result.TurnsCompleted, cfg.Agent.MaxTurns)
	}
}

// TestRunWorkerAttempt_ReadOnly_SuppressesSelfReview verifies that a
// read-only attempt never runs the self-review loop even when self-review
// is enabled (A4).
func TestRunWorkerAttempt_ReadOnly_SuppressesSelfReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("self-review verification command uses touch")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "self_review_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.SelfReview = config.SelfReviewConfig{
		Enabled:               true,
		MaxIterations:         1,
		VerificationCommands:  []string{fmt.Sprintf("touch %s", markerPath)},
		VerificationTimeoutMS: 5000,
	}

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if _, err := os.Stat(markerPath); err == nil {
		t.Error("self-review verification command ran for a read-only dispatch; want no self-review")
	}
}

// TestRunWorkerAttempt_ReadOnly_SuppressesAfterRunHook verifies that a
// read-only attempt never runs the after_run hook even when cfg.Hooks.AfterRun
// is set (A4).
func TestRunWorkerAttempt_ReadOnly_SuppressesAfterRunHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("after_run hook uses touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "after_run_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.Hooks.AfterRun = fmt.Sprintf("touch %s", markerPath)

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureReview,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if _, err := os.Stat(markerPath); err == nil {
		t.Error("after_run hook ran for a read-only dispatch; want no operator teardown hook")
	}
}

// TestRunWorkerAttempt_NormalDispatchUnaffected is the regression
// counterpart to the read-only suppression tests: with WorkerDeps.ReadOnly
// == false, every guard added for the read-only path is a no-op and the
// in-progress transition, dispatch comment, per-turn refresh, self-review
// loop, and after_run hook all fire exactly as before.
func TestRunWorkerAttempt_NormalDispatchUnaffected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks use touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	afterCreateMarker := filepath.Join(tmpDir, "after_create_marker")
	afterRunMarker := filepath.Join(tmpDir, "after_run_marker")
	selfReviewMarker := filepath.Join(tmpDir, "self_review_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 3
	cfg.Tracker.InProgressState = "In Progress"
	cfg.Tracker.Comments.OnDispatch = true
	cfg.Hooks.AfterCreate = fmt.Sprintf("touch %s", afterCreateMarker)
	cfg.Hooks.AfterRun = fmt.Sprintf("touch %s", afterRunMarker)
	cfg.SelfReview = config.SelfReviewConfig{
		Enabled:               true,
		MaxIterations:         1,
		VerificationCommands:  []string{fmt.Sprintf("touch %s", selfReviewMarker)},
		VerificationTimeoutMS: 5000,
	}

	var fetchCalls atomic.Int32
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			fetchCalls.Add(1)
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureNormal,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue call count = %d, want 1 (normal dispatch still transitions in-progress)", len(tracker.transitionCalls))
	}
	if len(tracker.commentCalls) != 1 {
		t.Errorf("CommentIssue call count = %d, want 1 (normal dispatch still posts the dispatch comment)", len(tracker.commentCalls))
	}
	if fetchCalls.Load() == 0 {
		t.Error("FetchIssueStatesByIDs was never called; want per-turn refresh for a normal dispatch")
	}
	if _, err := os.Stat(afterCreateMarker); err != nil {
		t.Errorf("after_create hook did not run for a normal dispatch: %v", err)
	}
	if _, err := os.Stat(afterRunMarker); err != nil {
		t.Errorf("after_run hook did not run for a normal dispatch: %v", err)
	}
	if _, err := os.Stat(selfReviewMarker); err != nil {
		t.Errorf("self-review did not run for a normal dispatch: %v", err)
	}
}

// TestRunWorkerAttempt_Fix_RunsSetupHooksAndClones verifies that a
// PostureFix attempt runs the operator after_create/before_run setup
// hooks, proving it takes the workspace.Prepare clone path rather than
// the read-only path's scratch workspace.Ensure path, which accepts no
// hook configuration at all (A4).
func TestRunWorkerAttempt_Fix_RunsSetupHooksAndClones(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks use touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	afterCreateMarker := filepath.Join(tmpDir, "after_create_marker")
	beforeRunMarker := filepath.Join(tmpDir, "before_run_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.Hooks.AfterCreate = fmt.Sprintf("touch %s", afterCreateMarker)
	cfg.Hooks.BeforeRun = fmt.Sprintf("touch %s", beforeRunMarker)

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.WorkspacePath == "" {
		t.Fatal("WorkspacePath is empty, want the cloned per-issue workspace")
	}
	if _, err := os.Stat(afterCreateMarker); err != nil {
		t.Errorf("after_create hook did not run for a fix dispatch: %v", err)
	}
	if _, err := os.Stat(beforeRunMarker); err != nil {
		t.Errorf("before_run hook did not run for a fix dispatch: %v", err)
	}
}

// TestRunWorkerAttempt_Fix_FreshSessionClearsStaleStatus verifies that a
// PostureFix attempt starts a fresh session (StartSessionParams.ResumeSessionID
// empty) and clears a stale .sortie/status left in the reused per-issue
// workspace via the Prepare PreRunFunc, so a prior recognized signal does
// not end the fix session on turn one (A4).
func TestRunWorkerAttempt_Fix_FreshSessionClearsStaleStatus(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 1

	// Pre-create the reused workspace with a stale "blocked" status from a
	// prior session's exit.
	wsPath := filepath.Join(tmpDir, "TEST-1")
	statusDir := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(statusDir, "status"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(stale status): %v", err)
	}

	var gotResumeSessionID string
	var resumeSessionIDCaptured bool
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter: &mockTrackerAdapter{},
		AgentAdapter: &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				gotResumeSessionID = params.ResumeSessionID
				resumeSessionIDCaptured = true
				return domain.Session{ID: "sess-1"}, nil
			},
		},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if !resumeSessionIDCaptured {
		t.Fatal("StartSession was never called")
	}
	if gotResumeSessionID != "" {
		t.Errorf("StartSessionParams.ResumeSessionID = %q, want empty (a fix dispatch starts a fresh session)", gotResumeSessionID)
	}
	if result.SoftStop {
		t.Error("SoftStop = true, want false (stale status should have been cleaned via the Prepare PreRunFunc)")
	}
	if result.ExitKind != WorkerExitNormal {
		t.Errorf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
	}
	if result.TurnsCompleted != 1 {
		t.Errorf("TurnsCompleted = %d, want 1 (the fix session ran a turn instead of soft-stopping)", result.TurnsCompleted)
	}
}

// TestRunWorkerAttempt_Fix_SuppressesInProgressTransition verifies that a
// fix attempt never calls TransitionIssue even when
// cfg.Tracker.InProgressState is set (A4).
func TestRunWorkerAttempt_Fix_SuppressesInProgressTransition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Tracker.InProgressState = "In Progress"

	tracker := &mockTrackerAdapter{}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue call count = %d, want 0 (a fix dispatch drives no issue state)", len(tracker.transitionCalls))
	}
}

// TestRunWorkerAttempt_Fix_SuppressesDispatchComment verifies that a fix
// attempt never posts the dispatch comment even when
// cfg.Tracker.Comments.OnDispatch is true (A4).
func TestRunWorkerAttempt_Fix_SuppressesDispatchComment(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Tracker.Comments.OnDispatch = true

	tracker := &mockTrackerAdapter{}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (a fix dispatch is not a work claim)", len(tracker.commentCalls))
	}
}

// TestRunWorkerAttempt_Fix_SuppressesPerTurnRefresh verifies that a fix
// attempt never calls FetchIssueStatesByIDs during the turn loop; the loop
// terminates via max_turns instead, since a PR under review usually has its
// linked issue in a non-active state (A4).
func TestRunWorkerAttempt_Fix_SuppressesPerTurnRefresh(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.MaxTurns = 3

	var fetchCalls atomic.Int32
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			fetchCalls.Add(1)
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}
	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         tracker,
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if fetchCalls.Load() != 0 {
		t.Errorf("FetchIssueStatesByIDs call count = %d, want 0 (fix loop rests on max_turns/status file only)", fetchCalls.Load())
	}
	if result.TurnsCompleted != cfg.Agent.MaxTurns {
		t.Errorf("TurnsCompleted = %d, want %d (loop terminates via max_turns, not tracker-state gating)",
			result.TurnsCompleted, cfg.Agent.MaxTurns)
	}
}

// TestRunWorkerAttempt_Fix_SuppressesSelfReview verifies that a fix attempt
// never runs the self-review loop even when self-review is enabled (A4).
func TestRunWorkerAttempt_Fix_SuppressesSelfReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("self-review verification command uses touch")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "self_review_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.SelfReview = config.SelfReviewConfig{
		Enabled:               true,
		MaxIterations:         1,
		VerificationCommands:  []string{fmt.Sprintf("touch %s", markerPath)},
		VerificationTimeoutMS: 5000,
	}

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	ec.waitResult(t)

	if _, err := os.Stat(markerPath); err == nil {
		t.Error("self-review verification command ran for a fix dispatch; want no self-review")
	}
}

// TestRunWorkerAttempt_Fix_AfterRunHookOnCleanExit verifies that a fix
// attempt runs the after_run teardown hook on a clean exit, unlike the
// read-only path, because RunsSetupHooks is true for PostureFix (A4).
func TestRunWorkerAttempt_Fix_AfterRunHookOnCleanExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("after_run hook uses touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "after_run_marker")

	cfg := defaultWorkerConfig(tmpDir)
	cfg.Hooks.AfterRun = fmt.Sprintf("touch %s", markerPath)

	ec := newExitCapture()
	deps := WorkerDeps{
		TrackerAdapter:         &mockTrackerAdapter{},
		AgentAdapter:           &mockAgentAdapter{},
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.ExitKind != WorkerExitNormal {
		t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitNormal)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("after_run hook did not run for a fix dispatch on clean exit: %v", err)
	}
}

// TestRunWorkerAttempt_Fix_AfterRunHookOnPanic verifies that a fix attempt
// runs the after_run teardown hook during panic recovery, because
// RunsSetupHooks is true for PostureFix and its teardown must run on every
// exit path for symmetry with the setup hooks that ran (A4).
func TestRunWorkerAttempt_Fix_AfterRunHookOnPanic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("after_run hook uses touch, unavailable on windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "after_run_marker")

	cfg := defaultWorkerConfig(tmpDir)
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
		ConfigFunc:             func() config.ServiceConfig { return cfg },
		PromptTemplateByIDFunc: func(_ string) *prompt.Template { return mustParseTemplate(t, "{{ .issue.title }}") },
		OnEvent:                func(_ string, _ domain.AgentEvent) {},
		OnExit:                 ec.onExit,
		Logger:                 discardLogger(),
		Posture:                PostureFix,
	}

	RunWorkerAttempt(context.Background(), workerTestIssue(), nil, deps)
	result := ec.waitResult(t)

	if result.ExitKind != WorkerExitError {
		t.Fatalf("ExitKind = %q, want %q", result.ExitKind, WorkerExitError)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("after_run hook did not run during panic recovery for a fix dispatch: %v", err)
	}
	if !stopCalled.Load() {
		t.Error("StopSession was not called during panic recovery, want teardown")
	}
}
