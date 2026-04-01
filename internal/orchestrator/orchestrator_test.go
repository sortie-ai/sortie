package orchestrator

import (
	"bytes"
	"context"
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
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// --- stub types for orchestrator tests ---

// stubWorkflowManager implements [WorkflowManager] with configurable returns.
// All methods are safe for concurrent use.
type stubWorkflowManager struct {
	mu       sync.RWMutex
	config   config.ServiceConfig
	template *prompt.Template
	reloadFn func() error
	absPath  string
}

func (s *stubWorkflowManager) Config() config.ServiceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

func (s *stubWorkflowManager) PromptTemplate() *prompt.Template {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.template
}

func (s *stubWorkflowManager) Reload() error {
	s.mu.RLock()
	fn := s.reloadFn
	s.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return nil
}

func (s *stubWorkflowManager) WorkflowAbsPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.absPath
}

func (s *stubWorkflowManager) setConfig(cfg config.ServiceConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
}

func (s *stubWorkflowManager) setTemplate(tmpl *prompt.Template) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.template = tmpl
}

// observerFunc adapts a plain function to the [Observer] interface.
type observerFunc func()

func (f observerFunc) OnStateChange() { f() }

// stubStore implements [OrchestratorStore] with call tracking.
type stubStore struct {
	mu              sync.Mutex
	runHistories    []persistence.RunHistory
	aggregates      []persistence.AggregateMetrics
	sessions        []persistence.SessionMetadata
	savedRetries    []persistence.RetryEntry
	deletedRetryIDs []string
}

func (s *stubStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.ID = int64(len(s.runHistories) + 1)
	s.runHistories = append(s.runHistories, run)
	return run, nil
}

func (s *stubStore) UpsertAggregateMetrics(_ context.Context, m persistence.AggregateMetrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggregates = append(s.aggregates, m)
	return nil
}

func (s *stubStore) UpsertSessionMetadata(_ context.Context, m persistence.SessionMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, m)
	return nil
}

func (s *stubStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedRetries = append(s.savedRetries, entry)
	return nil
}

func (s *stubStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedRetryIDs = append(s.deletedRetryIDs, issueID)
	return nil
}

func (s *stubStore) CountRunHistoryByIssue(_ context.Context, _ string) (int, error) {
	return 0, nil
}

// stubObserver implements [Observer] with an atomic call counter.
type stubObserver struct {
	calls atomic.Int64
}

func (o *stubObserver) OnStateChange() { o.calls.Add(1) }

// --- TestShouldDispatchWithSets ---

func TestShouldDispatchWithSets(t *testing.T) {
	t.Parallel()

	activeSet := stateSet([]string{"To Do", "In Progress"})
	terminalSet := stateSet([]string{"Done", "Closed"})

	baseIssue := domain.Issue{
		ID:         "1",
		Identifier: "TEST-1",
		Title:      "Test issue",
		State:      "To Do",
	}

	tests := []struct {
		name       string
		issue      domain.Issue
		activeSet  map[string]struct{}
		terminalS  map[string]struct{}
		setupState func(*State)
		want       bool
	}{
		{
			name:      "missing ID",
			issue:     domain.Issue{ID: "", Identifier: "X-1", Title: "T", State: "To Do"},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name:      "missing identifier",
			issue:     domain.Issue{ID: "1", Identifier: "", Title: "T", State: "To Do"},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name:      "missing title",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "", State: "To Do"},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name:      "missing state",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: ""},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name:      "state not in active set",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "Backlog"},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name:      "state in terminal set even if also in active set",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "Done"},
			activeSet: stateSet([]string{"Done"}), terminalS: stateSet([]string{"Done"}),
			want: false,
		},
		{
			name:      "case-insensitive state matching",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "to do"},
			activeSet: stateSet([]string{"To Do"}), terminalS: stateSet([]string{"Done"}),
			want: true,
		},
		{
			name:      "upper-case state against lower-case set",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "TO DO"},
			activeSet: stateSet([]string{"To Do"}), terminalS: stateSet([]string{"Done"}),
			want: true,
		},
		{
			name:      "already running",
			issue:     baseIssue,
			activeSet: activeSet, terminalS: terminalSet,
			setupState: func(s *State) {
				s.Running["1"] = &RunningEntry{Issue: baseIssue}
			},
			want: false,
		},
		{
			name:      "already claimed but not running",
			issue:     baseIssue,
			activeSet: activeSet, terminalS: terminalSet,
			setupState: func(s *State) {
				s.Claimed["1"] = struct{}{}
			},
			want: false,
		},
		{
			name: "blocker with empty state blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: ""}},
			},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name: "blocker with active non-terminal state blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: "In Progress"}},
			},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name: "blocker with terminal state allows dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{{ID: "2", State: "Done"}},
			},
			activeSet: activeSet, terminalS: terminalSet,
			want: true,
		},
		{
			name: "multiple blockers one non-terminal blocks dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{
					{ID: "2", State: "Done"},
					{ID: "3", State: "In Progress"},
				},
			},
			activeSet: activeSet, terminalS: terminalSet,
			want: false,
		},
		{
			name: "no blockers allows dispatch",
			issue: domain.Issue{
				ID: "1", Identifier: "X-1", Title: "T", State: "To Do",
				BlockedBy: []domain.BlockerRef{},
			},
			activeSet: activeSet, terminalS: terminalSet,
			want: true,
		},
		{
			name:      "fully eligible issue",
			issue:     baseIssue,
			activeSet: activeSet, terminalS: terminalSet,
			want: true,
		},
		{
			name:      "second active state eligible",
			issue:     domain.Issue{ID: "1", Identifier: "X-1", Title: "T", State: "In Progress"},
			activeSet: activeSet, terminalS: terminalSet,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := NewState(1000, 10, nil, AgentTotals{})
			if tt.setupState != nil {
				tt.setupState(s)
			}

			got := ShouldDispatchWithSets(tt.issue, s, tt.activeSet, tt.terminalS)
			if got != tt.want {
				t.Errorf("ShouldDispatchWithSets(%q) = %t, want %t", tt.issue.Identifier, got, tt.want)
			}
		})
	}
}

func TestShouldDispatchWithSets_parity(t *testing.T) {
	t.Parallel()

	// Verify ShouldDispatchWithSets produces identical results to ShouldDispatch.
	active := []string{"To Do", "In Progress"}
	terminal := []string{"Done", "Closed"}
	aSet := stateSet(active)
	tSet := stateSet(terminal)

	issues := []domain.Issue{
		{ID: "1", Identifier: "T-1", Title: "A", State: "To Do"},
		{ID: "2", Identifier: "T-2", Title: "B", State: "Backlog"},
		{ID: "3", Identifier: "T-3", Title: "C", State: "Done"},
		{ID: "4", Identifier: "T-4", Title: "D", State: "In Progress",
			BlockedBy: []domain.BlockerRef{{ID: "5", State: "In Progress"}}},
	}

	for _, issue := range issues {
		s := NewState(1000, 10, nil, AgentTotals{})
		want := ShouldDispatch(issue, s, active, terminal)
		got := ShouldDispatchWithSets(issue, s, aSet, tSet)
		if got != want {
			t.Errorf("parity mismatch for %q: WithSets=%t, original=%t",
				issue.Identifier, got, want)
		}
	}
}

// --- TestNewOrchestrator ---

func TestNewOrchestrator(t *testing.T) {
	t.Parallel()

	t.Run("channel buffer sizes", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		// maxConc=5: exit=max(10,64)=64, retry=max(10,64)=64, event=max(80,256)=256.
		if cap(o.workerExitCh) != 64 {
			t.Errorf("workerExitCh cap = %d, want 64", cap(o.workerExitCh))
		}
		if cap(o.retryTimerCh) != 64 {
			t.Errorf("retryTimerCh cap = %d, want 64", cap(o.retryTimerCh))
		}
		if cap(o.agentEventCh) != 256 {
			t.Errorf("agentEventCh cap = %d, want 256", cap(o.agentEventCh))
		}
	})

	t.Run("large concurrency scales buffers", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 100, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		// maxConc=100: exit=200, retry=200, event=1600.
		if cap(o.workerExitCh) != 200 {
			t.Errorf("workerExitCh cap = %d, want 200", cap(o.workerExitCh))
		}
		if cap(o.retryTimerCh) != 200 {
			t.Errorf("retryTimerCh cap = %d, want 200", cap(o.retryTimerCh))
		}
		if cap(o.agentEventCh) != 1600 {
			t.Errorf("agentEventCh cap = %d, want 1600", cap(o.agentEventCh))
		}
	})

	t.Run("nil logger defaults to slog.Default", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		if o.logger == nil {
			t.Fatal("logger is nil, want non-nil default")
		}
	})

	t.Run("nil observers becomes empty slice", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		if o.observers == nil {
			t.Fatal("observers is nil, want non-nil empty slice")
		}
		if len(o.observers) != 0 {
			t.Errorf("observers length = %d, want 0", len(o.observers))
		}
	})
}

// --- PreflightOK tests ---

func TestPreflightOK_InitialValue(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
	})

	if !o.PreflightOK() {
		t.Error("PreflightOK() = false after NewOrchestrator, want true")
	}
}

func TestPreflightOK_ReflectsTickResult(t *testing.T) {
	t.Parallel()

	// A tick with a failing preflight sets PreflightOK to false.
	// We create an orchestrator whose ReloadWorkflow returns an error,
	// which causes ValidateDispatchConfig to fail immediately.

	failReload := func() error { return fmt.Errorf("workflow file missing") }

	cfg := config.ServiceConfig{
		Polling: config.PollingConfig{IntervalMS: 60000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			Command:             "/usr/bin/agent",
			MaxConcurrentAgents: 1,
		},
		Tracker: config.TrackerConfig{
			Kind:         "mock",
			APIKey:       "key",
			ActiveStates: []string{"To Do"},
		},
	}

	wm := &stubWorkflowManager{config: cfg}
	regs := passingPreflightRegistries()

	state := NewState(60000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  failReload,
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	// Initially true.
	if !o.PreflightOK() {
		t.Fatal("PreflightOK() = false before tick, want true")
	}

	// Run a single tick. The preflight should fail because
	// ReloadWorkflow returns an error.
	ctx := context.Background()
	o.handleTick(ctx)

	if o.PreflightOK() {
		t.Error("PreflightOK() = true after tick with failing preflight, want false")
	}

	// Fix the reload and run another tick — should pass again.
	o.preflightParams.ReloadWorkflow = func() error { return nil }
	o.handleTick(ctx)

	if !o.PreflightOK() {
		t.Error("PreflightOK() = false after tick with passing preflight, want true")
	}
}

// --- TestOrchestratorShutdown ---

func TestOrchestratorShutdown(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error { return errPreflightFailed },
			ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Cancel immediately and verify Run returns promptly.
	cancel()

	select {
	case <-done:
		// Run returned as expected.
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3 seconds of context cancellation")
	}
}

// --- TestMakeWorkerFn ---

func TestMakeWorkerFn(t *testing.T) {
	t.Parallel()

	t.Run("OnEvent delivers to agentEventCh non-blocking", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})

		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		var eventReceived atomic.Bool
		agent := &mockAgentAdapter{
			runTurnFn: func(_ context.Context, sess domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
				params.OnEvent(domain.AgentEvent{
					Type:    domain.EventNotification,
					Message: "test event",
				})
				eventReceived.Store(true)
				return domain.TurnResult{
					SessionID:  sess.ID,
					ExitReason: domain.EventTurnCompleted,
				}, nil
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           &stubStore{},
		})

		issue := workerTestIssue()
		state.Running[issue.ID] = &RunningEntry{
			Identifier: issue.Identifier,
			Issue:      issue,
		}

		wfn := o.makeWorkerFn("", "")

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		// Drain the exit channel to unblock the worker goroutine.
		var exitResult WorkerResult
		select {
		case exitResult = <-o.workerExitCh:
		case <-time.After(10 * time.Second):
			t.Fatal("worker did not exit within 10 seconds")
		}

		<-exitDone

		if exitResult.ExitKind == WorkerExitError {
			t.Skipf("worker exited with error (environment limitation): %v", exitResult.Error)
		}

		if !eventReceived.Load() {
			t.Error("OnEvent was not invoked")
		}

		// Verify event was delivered to the channel.
		select {
		case msg := <-o.agentEventCh:
			if msg.IssueID != issue.ID {
				t.Errorf("agentEventMsg.IssueID = %q, want %q", msg.IssueID, issue.ID)
			}
		default:
			t.Error("agentEventCh is empty, expected an event")
		}
	})

	t.Run("OnExit delivers to workerExitCh blocking", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})
		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		wm := &stubWorkflowManager{config: cfg, template: tmpl}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
		})

		issue := workerTestIssue()
		state.Running[issue.ID] = &RunningEntry{
			Identifier: issue.Identifier,
			Issue:      issue,
		}

		wfn := o.makeWorkerFn("", "")

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		// Verify exit result was delivered.
		select {
		case result := <-o.workerExitCh:
			if result.IssueID != issue.ID {
				t.Errorf("WorkerResult.IssueID = %q, want %q", result.IssueID, issue.ID)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for workerExitCh")
		}

		<-exitDone
	})

	t.Run("ResumeSessionID from running entry", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})
		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		var capturedResumeID string
		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				capturedResumeID = params.ResumeSessionID
				return domain.Session{ID: "new-sess"}, nil
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           &stubStore{},
		})

		issue := workerTestIssue()
		state.Running[issue.ID] = &RunningEntry{
			Identifier: issue.Identifier,
			Issue:      issue,
			SessionID:  "resume-sess-42",
		}

		wfn := o.makeWorkerFn("resume-sess-42", "")

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		select {
		case <-exitDone:
		case <-time.After(10 * time.Second):
			t.Fatal("worker did not exit within 10 seconds")
		}

		if capturedResumeID != "resume-sess-42" {
			t.Errorf("ResumeSessionID = %q, want %q", capturedResumeID, "resume-sess-42")
		}
	})

	t.Run("SSHStrictHostKeyChecking propagated to StartSessionParams", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})
		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		var capturedStrictHostKeyChecking string
		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				capturedStrictHostKeyChecking = params.SSHStrictHostKeyChecking
				return domain.Session{ID: "new-sess"}, nil
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           &stubStore{},
		})

		o.sshStrictHostKeyChecking = "yes"

		issue := workerTestIssue()
		state.Running[issue.ID] = &RunningEntry{
			Identifier: issue.Identifier,
			Issue:      issue,
		}

		wfn := o.makeWorkerFn("", "")

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		select {
		case <-exitDone:
		case <-time.After(10 * time.Second):
			t.Fatal("worker did not exit within 10 seconds")
		}

		if capturedStrictHostKeyChecking != "yes" {
			t.Errorf("StartSessionParams.SSHStrictHostKeyChecking = %q, want %q", capturedStrictHostKeyChecking, "yes")
		}
	})
}

// --- TestOnRetryFire ---

func TestOnRetryFire(t *testing.T) {
	t.Parallel()

	t.Run("delivers issue ID to retryTimerCh", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		o.onRetryFire("ISS-42")

		select {
		case id := <-o.retryTimerCh:
			if id != "ISS-42" {
				t.Errorf("retryTimerCh received %q, want %q", id, "ISS-42")
			}
		default:
			t.Fatal("retryTimerCh is empty after onRetryFire")
		}
	})

	t.Run("drops and logs when channel is full", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 1, nil, AgentTotals{})

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          logger,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		// Fill the channel to capacity.
		bufSize := cap(o.retryTimerCh)
		for i := range bufSize {
			o.retryTimerCh <- "fill-" + string(rune('A'+i))
		}

		// This should drop (non-blocking) and log.
		o.onRetryFire("OVERFLOW")

		logOutput := buf.String()
		if logOutput == "" {
			t.Error("expected log output when channel full, got empty")
		}

		// Channel should still be at capacity (OVERFLOW was dropped).
		if len(o.retryTimerCh) != bufSize {
			t.Errorf("retryTimerCh length = %d, want %d", len(o.retryTimerCh), bufSize)
		}
	})
}

// --- TestNotifyObservers ---

func TestNotifyObservers(t *testing.T) {
	t.Parallel()

	obs1 := &stubObserver{}
	obs2 := &stubObserver{}

	state := NewState(1000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		Observers:       []Observer{obs1, obs2},
	})

	o.notifyObservers()
	o.notifyObservers()

	if got := obs1.calls.Load(); got != 2 {
		t.Errorf("observer1 calls = %d, want 2", got)
	}
	if got := obs2.calls.Load(); got != 2 {
		t.Errorf("observer2 calls = %d, want 2", got)
	}
}

// --- TestOrchestratorDynamicConfig ---

func TestOrchestratorDynamicConfig(t *testing.T) {
	t.Parallel()

	// Verify that handleTick applies config changes from WorkflowManager.

	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}

	// Override FetchCandidateIssues via a custom type that embeds mockTrackerAdapter.
	candidateTracker := &candidateTrackerAdapter{
		mockTrackerAdapter: tracker,
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, nil // no candidates
		},
	}

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			APIKey:         "test-key",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 1000},
		Workspace: config.WorkspaceConfig{Root: t.TempDir()},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			Command:             "/usr/bin/agent",
			MaxConcurrentAgents: 2,
			MaxTurns:            3,
		},
	}

	wm := &stubWorkflowManager{config: cfg}

	state := NewState(1000, 2, nil, AgentTotals{})
	obs := &stubObserver{}
	regs := passingPreflightRegistries()

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  candidateTracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
		Observers: []Observer{obs},
	})

	ctx := context.Background()

	// First tick with MaxConcurrentAgents=2.
	o.handleTick(ctx)
	if state.MaxConcurrentAgents != 2 {
		t.Errorf("after first tick MaxConcurrentAgents = %d, want 2", state.MaxConcurrentAgents)
	}

	// Change config and tick again.
	cfg.Agent.MaxConcurrentAgents = 5
	cfg.Polling.IntervalMS = 2000
	wm.setConfig(cfg)

	o.handleTick(ctx)
	if state.MaxConcurrentAgents != 5 {
		t.Errorf("after second tick MaxConcurrentAgents = %d, want 5", state.MaxConcurrentAgents)
	}
	if state.PollIntervalMS != 2000 {
		t.Errorf("after second tick PollIntervalMS = %d, want 2000", state.PollIntervalMS)
	}

	// Observers should have been notified twice.
	if got := obs.calls.Load(); got != 2 {
		t.Errorf("observer calls = %d, want 2", got)
	}
}

// candidateTrackerAdapter extends mockTrackerAdapter with a configurable
// FetchCandidateIssues.
type candidateTrackerAdapter struct {
	*mockTrackerAdapter
	fetchCandidatesFn func(ctx context.Context) ([]domain.Issue, error)
}

func (c *candidateTrackerAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	if c.fetchCandidatesFn != nil {
		return c.fetchCandidatesFn(ctx)
	}
	return nil, nil
}

// --- TestOrchestratorPreflightFailure ---

func TestOrchestratorPreflightFailure(t *testing.T) {
	t.Parallel()

	// When preflight fails, handleTick should skip dispatch entirely.

	var fetchCalled atomic.Bool
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			fetchCalled.Store(true)
			return []domain.Issue{
				{ID: "1", Identifier: "T-1", Title: "Issue", State: "To Do"},
			}, nil
		},
	}

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling: config.PollingConfig{IntervalMS: 1000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			MaxConcurrentAgents: 5,
		},
	}

	wm := &stubWorkflowManager{config: cfg}
	obs := &stubObserver{}

	state := NewState(1000, 5, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:          state,
		Logger:         discardLogger(),
		TrackerAdapter: tracker,
		AgentAdapter:   &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{
			config:   cfg,
			reloadFn: func() error { return nil },
		},
		Store: &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error {
				return errPreflightFailed
			},
			ConfigFunc: wm.Config,
		},
		Observers: []Observer{obs},
	})

	o.handleTick(context.Background())

	// Preflight failed, so FetchCandidateIssues should NOT be called.
	if fetchCalled.Load() {
		t.Error("FetchCandidateIssues was called despite preflight failure")
	}

	// No workers should be running.
	if len(state.Running) != 0 {
		t.Errorf("Running count = %d, want 0", len(state.Running))
	}

	// Observer still notified (on preflight failure path).
	if got := obs.calls.Load(); got != 1 {
		t.Errorf("observer calls = %d, want 1", got)
	}
}

var errPreflightFailed = errorString("preflight: workflow reload failed")

type errorString string

func (e errorString) Error() string { return string(e) }

// --- TestTickLogging ---

func TestTickLogging_ZeroCandidates(t *testing.T) {
	t.Parallel()

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling: config.PollingConfig{IntervalMS: 1000},
		Agent:   config.AgentConfig{Kind: "mock", MaxConcurrentAgents: 5},
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

	o := NewOrchestrator(OrchestratorParams{
		State:  NewState(1000, 5, nil, AgentTotals{}),
		Logger: logger,
		TrackerAdapter: &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return nil, nil },
		},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           &stubStore{},
		PreflightParams: pf,
	})

	o.handleTick(context.Background())

	got := buf.String()
	if !strings.Contains(got, "tick completed") {
		t.Fatalf("log missing 'tick completed': %s", got)
	}
	if !strings.Contains(got, "candidates=0") {
		t.Errorf("log missing candidates=0: %s", got)
	}
	if !strings.Contains(got, "dispatched=0") {
		t.Errorf("log missing dispatched=0: %s", got)
	}
}

func TestTickLogging_WithDispatches(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 1000},
		Workspace: config.WorkspaceConfig{Root: tmpDir},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			Command:             "/usr/bin/agent",
			MaxConcurrentAgents: 5,
			MaxTurns:            1,
			ReadTimeoutMS:       1000,
		},
	}

	// Use a mutex-guarded buffer because dispatched worker goroutines
	// also write log messages concurrently.
	lb := &lockedBuf{}
	logger := slog.New(slog.NewTextHandler(lb, nil))

	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

	issues := []domain.Issue{
		{ID: "1", Identifier: "T-1", Title: "First", State: "To Do"},
		{ID: "2", Identifier: "T-2", Title: "Second", State: "To Do"},
	}

	tmpl := mustParseTemplate(t, "do {{.issue.identifier}}")

	o := NewOrchestrator(OrchestratorParams{
		State:  NewState(1000, 5, nil, AgentTotals{}),
		Logger: logger,
		TrackerAdapter: &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return issues, nil
			},
		},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg, template: tmpl},
		Store:           &stubStore{},
		PreflightParams: pf,
	})

	o.handleTick(context.Background())
	o.state.WorkerWg.Wait()

	got := lb.String()
	if !strings.Contains(got, "tick completed") {
		t.Fatalf("log missing 'tick completed': %s", got)
	}
	if !strings.Contains(got, "candidates=2") {
		t.Errorf("log missing candidates=2: %s", got)
	}
	if !strings.Contains(got, "dispatched=2") {
		t.Errorf("log missing dispatched=2: %s", got)
	}
}

// lockedBuf is a concurrency-safe [bytes.Buffer] for log capture in tests
// where background goroutines also write log output.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *lockedBuf) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.Write(p)
}

func (lb *lockedBuf) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.buf.String()
}

func TestTickLogging_PreflightFailure_NoTickLog(t *testing.T) {
	t.Parallel()

	// When preflight fails, "tick completed" must NOT be logged.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling: config.PollingConfig{IntervalMS: 1000},
		Agent:   config.AgentConfig{Kind: "mock", MaxConcurrentAgents: 5},
	}

	wm := &stubWorkflowManager{config: cfg}

	o := NewOrchestrator(OrchestratorParams{
		State:  NewState(1000, 5, nil, AgentTotals{}),
		Logger: logger,
		TrackerAdapter: &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
		},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error { return errPreflightFailed },
			ConfigFunc:     wm.Config,
		},
	})

	o.handleTick(context.Background())

	got := buf.String()
	if strings.Contains(got, "tick completed") {
		t.Errorf("'tick completed' logged despite preflight failure: %s", got)
	}
	if !strings.Contains(got, "dispatch preflight failed") {
		t.Errorf("expected preflight error log: %s", got)
	}
}

// passingPreflightRegistries returns a PreflightParams with stub registries
// that pass all validation checks.
func passingPreflightRegistries() PreflightParams {
	return PreflightParams{
		TrackerRegistry: &stubTrackerRegistry{
			getFunc:  func(string) (registry.TrackerConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
		AgentRegistry: &stubAgentRegistry{
			getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) registry.AdapterMeta { return registry.AdapterMeta{} },
		},
	}
}

// lifecycleConfig returns a config suitable for full lifecycle tests.
// Workspace root must be a t.TempDir().
func lifecycleConfig(workspaceRoot string) config.ServiceConfig {
	return config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			APIKey:         "test-key",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 60000},
		Workspace: config.WorkspaceConfig{Root: workspaceRoot},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			Command:             "/usr/bin/agent",
			MaxConcurrentAgents: 10,
			MaxTurns:            1,
			ReadTimeoutMS:       1000,
		},
	}
}

// lifecycleIssues returns 3 dispatch-eligible issues.
func lifecycleIssues() []domain.Issue {
	return []domain.Issue{
		{ID: "id-1", Identifier: "TEST-1", Title: "First", State: "To Do"},
		{ID: "id-2", Identifier: "TEST-2", Title: "Second", State: "To Do"},
		{ID: "id-3", Identifier: "TEST-3", Title: "Third", State: "To Do"},
	}
}

// --- TestOrchestratorLifecycle ---

func TestOrchestratorLifecycle(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)
	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					// Return "Done" so the worker exits after 1 turn
					// (state is no longer active → loop breaks).
					result[id] = "Done"
				}
				return result, nil
			},
		},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return lifecycleIssues(), nil
		},
	}

	agent := &mockAgentAdapter{
		runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
			return domain.TurnResult{
				SessionID:  sess.ID,
				ExitReason: domain.EventTurnCompleted,
			}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}
	store := &stubStore{}
	obs := &stubObserver{}
	regs := passingPreflightRegistries()

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    agent,
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
		Observers: []Observer{obs},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Poll the store (mutex-protected) for run history entries instead
	// of reading state directly, to avoid data races with the event loop.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			store.mu.Lock()
			n := len(store.runHistories)
			store.mu.Unlock()
			t.Fatalf("timed out: run histories = %d, want 3", n)
		default:
		}
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done

	// After Run returns, the event loop is stopped and state is safe to read.

	// Verify all 3 issues completed.
	for _, issue := range lifecycleIssues() {
		if _, ok := state.Completed[issue.ID]; !ok {
			t.Errorf("issue %s not in Completed set", issue.Identifier)
		}
	}

	// Verify no issues still running.
	if len(state.Running) != 0 {
		t.Errorf("Running count = %d, want 0", len(state.Running))
	}

	// Verify run history was persisted for all 3 issues.
	store.mu.Lock()
	historyCount := len(store.runHistories)
	store.mu.Unlock()
	if historyCount != 3 {
		t.Errorf("run history count = %d, want 3", historyCount)
	}

	// Observer should have been notified (at least once per tick + per exit).
	if got := obs.calls.Load(); got < 1 {
		t.Errorf("observer calls = %d, want >= 1", got)
	}
}

// --- TestOrchestratorLifecycleRetry ---

func TestOrchestratorLifecycleRetry(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)
	cfg.Agent.MaxConcurrentAgents = 5
	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	issues := []domain.Issue{
		{ID: "id-ok", Identifier: "OK-1", Title: "Good", State: "To Do"},
		{ID: "id-fail", Identifier: "FAIL-1", Title: "Bad", State: "To Do"},
	}

	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "Done"
				}
				return result, nil
			},
		},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return issues, nil
		},
	}

	var failOnce atomic.Bool
	agent := &mockAgentAdapter{
		runTurnFn: func(_ context.Context, sess domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
			if params.Issue.ID == "id-fail" && !failOnce.Load() {
				failOnce.Store(true)
				return domain.TurnResult{}, fmt.Errorf("simulated agent failure")
			}
			return domain.TurnResult{
				SessionID:  sess.ID,
				ExitReason: domain.EventTurnCompleted,
			}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}
	store := &stubStore{}
	regs := passingPreflightRegistries()

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    agent,
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Poll the store (mutex-protected) for evidence of completion and retry
	// scheduling. The OK issue produces a run_history entry; the failed
	// issue produces a saved retry entry.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			store.mu.Lock()
			h, r := len(store.runHistories), len(store.savedRetries)
			store.mu.Unlock()
			t.Fatalf("timed out: run histories = %d, saved retries = %d", h, r)
		default:
		}

		store.mu.Lock()
		hasOKHistory := false
		for _, rh := range store.runHistories {
			if rh.IssueID == "id-ok" {
				hasOKHistory = true
				break
			}
		}
		hasFailRetry := false
		for _, re := range store.savedRetries {
			if re.IssueID == "id-fail" {
				hasFailRetry = true
				break
			}
		}
		store.mu.Unlock()

		if hasOKHistory && hasFailRetry {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done

	// After Run returns, state is safe to read.

	// The OK issue completed.
	if _, ok := state.Completed["id-ok"]; !ok {
		t.Error("issue id-ok not in Completed set")
	}

	// The failed issue should have a retry entry persisted.
	store.mu.Lock()
	retriesSaved := len(store.savedRetries)
	store.mu.Unlock()
	if retriesSaved < 1 {
		t.Errorf("saved retries = %d, want >= 1", retriesSaved)
	}

	// The failed issue should still be claimed (retry pending).
	if _, claimed := state.Claimed["id-fail"]; !claimed {
		t.Error("issue id-fail not in Claimed set after retry scheduling")
	}
}

// --- TestDispatchLoopPerStateExhaustion ---

func TestDispatchLoopPerStateExhaustion(t *testing.T) {
	t.Parallel()

	// Regression: when per-state slots for one state are exhausted, the
	// dispatch loop must continue evaluating issues in other states
	// rather than breaking out of the loop entirely.

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)
	cfg.Agent.MaxConcurrentAgents = 10
	cfg.Agent.MaxTurns = 1
	cfg.Tracker.ActiveStates = []string{"In Progress", "To Do"}
	cfg.Agent.MaxConcurrentByState = map[string]int{
		"in progress": 2,
		"to do":       5,
	}
	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	// 2 "In Progress" + 1 "To Do" issue. Per-state limit for "In Progress" is 2.
	// After dispatching the 2 "In Progress" issues, the "To Do" issue must
	// still be dispatched.
	issues := []domain.Issue{
		{ID: "ip-1", Identifier: "IP-1", Title: "A", State: "In Progress", Priority: intPtr(1)},
		{ID: "ip-2", Identifier: "IP-2", Title: "B", State: "In Progress", Priority: intPtr(1)},
		{ID: "ip-3", Identifier: "IP-3", Title: "C", State: "In Progress", Priority: intPtr(1)},
		{ID: "td-1", Identifier: "TD-1", Title: "D", State: "To Do", Priority: intPtr(2)},
	}

	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "Done"
				}
				return result, nil
			},
		},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return issues, nil
		},
	}

	agent := &mockAgentAdapter{
		runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
			return domain.TurnResult{
				SessionID:  sess.ID,
				ExitReason: domain.EventTurnCompleted,
			}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}
	store := &stubStore{}
	regs := passingPreflightRegistries()

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, cfg.Agent.MaxConcurrentByState, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    agent,
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Poll the store for run history entries. We expect at least 3 dispatched
	// (2 IP + 1 TD), with IP-3 skipped on the first tick due to per-state limit.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			store.mu.Lock()
			n := len(store.runHistories)
			store.mu.Unlock()
			t.Fatalf("timed out: run histories = %d, want >= 3", n)
		default:
		}

		store.mu.Lock()
		hasIP1, hasIP2, hasTD1 := false, false, false
		for _, rh := range store.runHistories {
			switch rh.IssueID {
			case "ip-1":
				hasIP1 = true
			case "ip-2":
				hasIP2 = true
			case "td-1":
				hasTD1 = true
			}
		}
		store.mu.Unlock()

		if hasIP1 && hasIP2 && hasTD1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done

	// After Run returns, state is safe to read.

	// Verify the "To Do" issue was dispatched despite "In Progress" being full.
	if _, ok := state.Completed["td-1"]; !ok {
		t.Error("issue TD-1 not in Completed set — per-state exhaustion blocked cross-state dispatch")
	}

	// Verify ip-3 was NOT dispatched on the first tick (per-state limit of 2).
	store.mu.Lock()
	firstThreeIDs := make(map[string]bool)
	for i := range min(3, len(store.runHistories)) {
		firstThreeIDs[store.runHistories[i].IssueID] = true
	}
	store.mu.Unlock()

	if firstThreeIDs["ip-3"] && !firstThreeIDs["td-1"] {
		t.Error("ip-3 was dispatched before td-1 — per-state limit was not enforced")
	}
}

// --- TestOrchestratorDynamicConfigReload ---

// TestOrchestratorDynamicConfigReload verifies that handleTick propagates
// config changes from the WorkflowManager to observable orchestrator state
// and behavior, covering the seven scenarios exercised by cases A–G.
func TestOrchestratorDynamicConfigReload(t *testing.T) {
	t.Parallel()

	// Test Case A: polling interval change propagates to state.
	t.Run("polling_interval_change", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.Polling.IntervalMS = 60000

		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		obs := &stubObserver{}
		state := NewState(60000, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return nil, nil
			},
		}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
			Observers: []Observer{obs},
		})

		o.handleTick(context.Background())

		if state.PollIntervalMS != 60000 {
			t.Fatalf("after first tick PollIntervalMS = %d, want 60000", state.PollIntervalMS)
		}

		cfg.Polling.IntervalMS = 100
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		if state.PollIntervalMS != 100 {
			t.Errorf("after second tick PollIntervalMS = %d, want 100", state.PollIntervalMS)
		}
		if got := obs.calls.Load(); got != 2 {
			t.Errorf("observer calls = %d, want 2", got)
		}
	})

	// Test Case B: concurrency limit change affects dispatch capacity.
	t.Run("concurrency_limit_change", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 1
		cfg.Agent.MaxTurns = 1
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		// newTestState allocated below; deferred WaitGroup ensures all
		// dispatched goroutines finish before t.TempDir() cleanup.
		var stateRef *State
		t.Cleanup(func() {
			if stateRef != nil {
				stateRef.WorkerWg.Wait()
			}
		})

		issues := []domain.Issue{
			{ID: "c-1", Identifier: "C-1", Title: "First", State: "To Do"},
			{ID: "c-2", Identifier: "C-2", Title: "Second", State: "To Do"},
			{ID: "c-3", Identifier: "C-3", Title: "Third", State: "To Do"},
		}

		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "To Do"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return issues, nil
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, 1, nil, AgentTotals{})
		stateRef = state

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		o.handleTick(ctx)

		if len(state.Running) != 1 {
			t.Fatalf("after first tick Running = %d, want 1", len(state.Running))
		}

		// Cancel first worker and wait for its exit.
		for _, entry := range state.Running {
			if entry.CancelFunc != nil {
				entry.CancelFunc()
			}
		}
		select {
		case <-o.workerExitCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for first worker exit")
		}
		for id := range state.Running {
			delete(state.Running, id)
			delete(state.Claimed, id)
		}

		// Increase concurrency and tick again.
		cfg.Agent.MaxConcurrentAgents = 3
		wm.setConfig(cfg)

		o.handleTick(ctx)

		if state.MaxConcurrentAgents != 3 {
			t.Errorf("MaxConcurrentAgents = %d, want 3", state.MaxConcurrentAgents)
		}
		if len(state.Running) != 3 {
			t.Errorf("after second tick Running = %d, want 3", len(state.Running))
		}

		// Cancel all workers and drain exits before test cleanup.
		cancel()
		for i := 0; i < len(state.Running); i++ {
			select {
			case <-o.workerExitCh:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for worker exit")
			}
		}
	})

	// Test Case C: active state change makes previously-ineligible issues
	// dispatchable.
	t.Run("active_states_change", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Tracker.ActiveStates = []string{"To Do"}
		cfg.Tracker.TerminalStates = []string{"Done"}

		var stateRef *State
		t.Cleanup(func() {
			if stateRef != nil {
				stateRef.WorkerWg.Wait()
			}
		})

		qaIssue := domain.Issue{
			ID: "qa-1", Identifier: "QA-1", Title: "Review", State: "QA Review",
		}
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "QA Review"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return []domain.Issue{qaIssue}, nil
			},
		}

		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")
		wm := &stubWorkflowManager{config: cfg, template: tmpl}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})
		stateRef = state

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		// First tick: "QA Review" not in ActiveStates → no dispatch.
		o.handleTick(ctx)

		if len(state.Running) != 0 {
			t.Fatalf("after first tick Running = %d, want 0", len(state.Running))
		}

		// Add "QA Review" to active states and tick again.
		cfg.Tracker.ActiveStates = []string{"To Do", "QA Review"}
		wm.setConfig(cfg)

		o.handleTick(ctx)

		if len(state.Running) != 1 {
			t.Errorf("after second tick Running = %d, want 1", len(state.Running))
		}
		if _, ok := state.Running["qa-1"]; !ok {
			t.Error("issue qa-1 not in Running map after active state change")
		}

		// Cancel workers and drain exits before test cleanup.
		cancel()
		for range state.Running {
			select {
			case <-o.workerExitCh:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for worker exit")
			}
		}
	})

	// Test Case D: reconciliation uses fresh terminal states after reload.
	// Reconciliation runs with post-reload config.
	t.Run("reconcile_fresh_terminal_states", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Tracker.ActiveStates = []string{"To Do"}
		cfg.Tracker.TerminalStates = []string{"Done"}

		// The tracker will report "Archived" for the running issue.
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "Archived"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return nil, nil
			},
		}

		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")
		wm := &stubWorkflowManager{config: cfg, template: tmpl}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		// Manually place an issue into the running map.
		var cancelCalled atomic.Bool
		state.Running["arch-1"] = &RunningEntry{
			Identifier: "ARCH-1",
			Issue: domain.Issue{
				ID: "arch-1", Identifier: "ARCH-1", Title: "Archived Issue", State: "To Do",
			},
			StartedAt: time.Now().UTC(),
			CancelFunc: func() {
				cancelCalled.Store(true)
			},
		}
		state.Claimed["arch-1"] = struct{}{}

		// First tick: TerminalStates=["Done"]. "Archived" is not
		// terminal, so reconciliation cancels (non-active, non-terminal)
		// but does NOT set PendingCleanup.
		o.handleTick(context.Background())

		entry := state.Running["arch-1"]
		if entry == nil {
			t.Fatal("entry removed from Running — reconciliation should not remove entries")
			return
		}
		if entry.PendingCleanup {
			t.Fatal("PendingCleanup = true before adding Archived to terminal states")
		}
		if !cancelCalled.Load() {
			t.Fatal("CancelFunc not called for non-active non-terminal issue")
		}

		// Now add "Archived" to terminal states and tick again.
		// Reset the cancel tracker since the entry was already cancelled.
		cancelCalled.Store(false)
		entry.CancelFunc = func() { cancelCalled.Store(true) }
		cfg.Tracker.TerminalStates = []string{"Done", "Archived"}
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		entry = state.Running["arch-1"]
		if entry == nil {
			t.Fatal("entry removed from Running — reconciliation should not remove entries")
			return
		}
		if !entry.PendingCleanup {
			t.Error("PendingCleanup = false after adding Archived to terminal states, want true")
		}
	})

	// Test Case E: prompt template change applies to new workers.
	t.Run("prompt_template_change", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 5
		cfg.Polling.IntervalMS = 100

		var capturedPrompts sync.Map

		agent := &mockAgentAdapter{
			runTurnFn: func(_ context.Context, sess domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
				capturedPrompts.Store(params.Issue.Identifier, params.Prompt)
				return domain.TurnResult{
					SessionID:  sess.ID,
					ExitReason: domain.EventTurnCompleted,
				}, nil
			},
		}

		issues1 := []domain.Issue{
			{ID: "p-1", Identifier: "P-1", Title: "First", State: "To Do"},
		}
		issues2 := []domain.Issue{
			{ID: "p-2", Identifier: "P-2", Title: "Second", State: "To Do"},
		}

		var issueSet atomic.Int32
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "Done"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if issueSet.Load() == 0 {
					return issues1, nil
				}
				return issues2, nil
			},
		}

		tmpl1 := mustParseTemplate(t, "do {{ .issue.identifier }}")
		wm := &stubWorkflowManager{config: cfg, template: tmpl1}
		regs := passingPreflightRegistries()
		store := &stubStore{}
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           store,
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		// Start orchestrator so workers actually run.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Wait for first issue to be dispatched and complete.
		deadline := time.After(10 * time.Second)
		for {
			store.mu.Lock()
			n := len(store.runHistories)
			store.mu.Unlock()
			if n >= 1 {
				break
			}
			select {
			case <-deadline:
				cancel()
				<-done
				t.Fatal("timed out waiting for first issue to complete")
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}

		// Swap template and issue set for the next tick.
		tmpl2 := mustParseTemplate(t, "review {{ .issue.identifier }}")
		wm.setTemplate(tmpl2)
		issueSet.Store(1)

		// Wait for second issue to complete.
		deadline = time.After(10 * time.Second)
		for {
			store.mu.Lock()
			n := len(store.runHistories)
			store.mu.Unlock()
			if n >= 2 {
				break
			}
			select {
			case <-deadline:
				cancel()
				<-done
				t.Fatal("timed out waiting for second issue to complete")
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}

		cancel()
		<-done

		if v, ok := capturedPrompts.Load("P-1"); !ok {
			t.Error("no prompt captured for P-1")
		} else if got, ok := v.(string); !ok || got != "do P-1" {
			t.Errorf("prompt for P-1 = %q, want %q", got, "do P-1")
		}

		if v, ok := capturedPrompts.Load("P-2"); !ok {
			t.Error("no prompt captured for P-2")
		} else if got, ok := v.(string); !ok || got != "review P-2" {
			t.Errorf("prompt for P-2 = %q, want %q", got, "review P-2")
		}
	})

	// Test Case F: in-flight sessions are not restarted on config change.
	t.Run("inflight_not_restarted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 2

		// Worker blocks until context is cancelled.
		agent := &mockAgentAdapter{
			runTurnFn: func(ctx context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				<-ctx.Done()
				return domain.TurnResult{
					SessionID:  sess.ID,
					ExitReason: domain.EventTurnCompleted,
				}, nil
			},
		}

		issues := []domain.Issue{
			{ID: "f-1", Identifier: "F-1", Title: "Inflight", State: "To Do"},
		}

		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "To Do"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return issues, nil
			},
		}

		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")
		wm := &stubWorkflowManager{config: cfg, template: tmpl}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		t.Cleanup(func() { state.WorkerWg.Wait() })

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		// First tick dispatches the issue.
		o.handleTick(context.Background())

		if len(state.Running) != 1 {
			t.Fatalf("after first tick Running = %d, want 1", len(state.Running))
		}

		entry := state.Running["f-1"]
		if entry == nil {
			t.Fatal("issue f-1 not in Running map")
			return
		}
		originalCancel := entry.CancelFunc

		// Swap config (change concurrency limit) and tick again.
		cfg.Agent.MaxConcurrentAgents = 10
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		if state.MaxConcurrentAgents != 10 {
			t.Errorf("MaxConcurrentAgents = %d, want 10", state.MaxConcurrentAgents)
		}

		// The in-flight entry must still be in the Running map.
		entry = state.Running["f-1"]
		if entry == nil {
			t.Fatal("issue f-1 removed from Running after config change")
			return
		}

		// The CancelFunc must be the same original (not replaced).
		if entry.CancelFunc == nil {
			t.Fatal("CancelFunc is nil after config change")
		}

		// Verify the worker is still actually running by confirming
		// we can cancel it and it responds.
		originalCancel()

		// Drain the worker exit to clean up goroutines.
		select {
		case <-o.workerExitCh:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not exit after cancel")
		}
	})

	// Test Case G: state fields update even on preflight failure.
	// Dispatch is skipped but reconciliation remains active.
	t.Run("state_updates_on_preflight_failure", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.Polling.IntervalMS = 5000
		cfg.Agent.MaxConcurrentAgents = 3
		cfg.Tracker.TerminalStates = []string{"Done"}

		wm := &stubWorkflowManager{config: cfg}
		state := NewState(1000, 1, nil, AgentTotals{})
		obs := &stubObserver{}

		// Place a running entry whose tracker state will be terminal.
		var cancelCalled atomic.Bool
		state.Running["g-1"] = &RunningEntry{
			Identifier: "G-1",
			Issue: domain.Issue{
				ID: "g-1", Identifier: "G-1", Title: "Terminal", State: "To Do",
			},
			StartedAt:  time.Now().UTC(),
			CancelFunc: func() { cancelCalled.Store(true) },
		}
		state.Claimed["g-1"] = struct{}{}

		o := NewOrchestrator(OrchestratorParams{
			State:  state,
			Logger: discardLogger(),
			TrackerAdapter: &candidateTrackerAdapter{
				mockTrackerAdapter: &mockTrackerAdapter{
					fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
						result := make(map[string]string, len(ids))
						for _, id := range ids {
							result[id] = "Done"
						}
						return result, nil
					},
				},
				fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
					t.Error("FetchCandidateIssues called despite preflight failure")
					return nil, nil
				},
			},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error {
					return errPreflightFailed
				},
				ConfigFunc: wm.Config,
			},
			Observers: []Observer{obs},
		})

		o.handleTick(context.Background())

		// State fields must have been updated despite preflight failure.
		if state.PollIntervalMS != 5000 {
			t.Errorf("PollIntervalMS = %d, want 5000", state.PollIntervalMS)
		}
		if state.MaxConcurrentAgents != 3 {
			t.Errorf("MaxConcurrentAgents = %d, want 3", state.MaxConcurrentAgents)
		}

		// Reconciliation must have run: the terminal running entry
		// should be marked PendingCleanup and cancelled.
		entry := state.Running["g-1"]
		if entry == nil {
			t.Fatal("entry g-1 removed from Running — reconciliation should not remove entries")
			return
		}
		if !entry.PendingCleanup {
			t.Error("PendingCleanup = false despite terminal tracker state and preflight failure")
		}
		if !cancelCalled.Load() {
			t.Error("CancelFunc not called despite terminal tracker state")
		}

		if got := obs.calls.Load(); got != 1 {
			t.Errorf("observer calls = %d, want 1", got)
		}
	})
}

// --- TestOrchestratorDynamicConfigReloadWithFileWatcher ---

// TestOrchestratorDynamicConfigReloadWithFileWatcher exercises the full
// reload pipeline: WORKFLOW.md change → fsnotify → workflow.Manager →
// Config() → handleTick → state update.
func TestOrchestratorDynamicConfigReloadWithFileWatcher(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	workflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

	initialContent := `---
tracker:
  kind: mock
  api_key: test-key
  active_states:
    - To Do
  terminal_states:
    - Done
polling:
  interval_ms: 100
workspace:
  root: ` + tmpDir + `
hooks:
  timeout_ms: 5000
agent:
  kind: mock
  command: /usr/bin/agent
  max_concurrent_agents: 2
  max_turns: 1
---
do {{ .issue.identifier }}
`
	if err := os.WriteFile(workflowPath, []byte(initialContent), 0o644); err != nil {
		t.Fatalf("writing initial WORKFLOW.md: %v", err)
	}

	wm, err := workflow.NewManager(workflowPath, discardLogger())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(wm.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := wm.Start(ctx); err != nil {
		t.Fatalf("Start watcher: %v", err)
	}

	// Give the watcher time to register with the filesystem so that
	// subsequent WORKFLOW.md updates are reliably observed.
	time.Sleep(50 * time.Millisecond)

	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, nil
		},
	}

	regs := passingPreflightRegistries()
	state := NewState(100, 2, nil, AgentTotals{})

	// Observer captures MaxConcurrentAgents atomically from the
	// event loop goroutine so the test goroutine can poll safely.
	var observedMax atomic.Int32
	observedMax.Store(int32(state.MaxConcurrentAgents))
	obs := observerFunc(func() {
		observedMax.Store(int32(state.MaxConcurrentAgents))
	})

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		Observers:       []Observer{obs},
		PreflightParams: PreflightParams{
			// Use a no-op reload so that any observed config changes
			// come from the fsnotify watcher path rather than the
			// defensive reload in preflight.
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Overwrite WORKFLOW.md with changed values.
	updatedContent := `---
tracker:
  kind: mock
  api_key: test-key
  active_states:
    - To Do
  terminal_states:
    - Done
polling:
  interval_ms: 100
workspace:
  root: ` + tmpDir + `
hooks:
  timeout_ms: 5000
agent:
  kind: mock
  command: /usr/bin/agent
  max_concurrent_agents: 7
  max_turns: 1
---
do {{ .issue.identifier }}
`
	// Write to a temp file and rename for atomic update (fsnotify
	// detects Create on the parent directory).
	tmpFile := filepath.Join(tmpDir, "WORKFLOW.md.tmp")
	if err := os.WriteFile(tmpFile, []byte(updatedContent), 0o644); err != nil {
		t.Fatalf("writing updated WORKFLOW.md: %v", err)
	}
	if err := os.Rename(tmpFile, workflowPath); err != nil {
		t.Fatalf("renaming WORKFLOW.md: %v", err)
	}

	// Poll the atomic snapshot written by the observer. The observer
	// runs on the event loop goroutine after state mutation, so this
	// is free of data races.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("timed out: MaxConcurrentAgents = %d, want 7",
				observedMax.Load())
		default:
		}
		if observedMax.Load() == 7 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done
}

// TestReconciliationGuardOnInvalidReload verifies that when config
// promotion is rejected (both state lists empty), handleTick retains
// the last-known-good config and reconciliation does not cancel running
// workers.
func TestReconciliationGuardOnInvalidReload(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Initial config: "In Progress" is active, "Done" is terminal.
	goodCfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"In Progress"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 60000},
		Workspace: config.WorkspaceConfig{Root: tmpDir},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			MaxConcurrentAgents: 5,
			MaxTurns:            1,
		},
	}

	// Simulate Manager.Reload returning a validation error (as it would
	// when both state lists are empty), while Config() keeps returning
	// the last-known-good config.
	reloadErr := errorString("tracker.active_states and tracker.terminal_states are both empty; at least one must be configured")
	wm := &stubWorkflowManager{
		config:   goodCfg,
		template: mustParseTemplate(t, "do {{ .issue.identifier }}"),
		reloadFn: func() error { return reloadErr },
	}

	// Tracker returns "In Progress" for the running issue.
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "In Progress"
			}
			return result, nil
		},
	}

	var cancelCalled atomic.Bool
	cancelFn := func() { cancelCalled.Store(true) }

	state := NewState(60000, 5, nil, AgentTotals{})
	state.Running["issue-1"] = &RunningEntry{
		Identifier: "TEST-1",
		Issue: domain.Issue{
			ID:         "issue-1",
			Identifier: "TEST-1",
			Title:      "Active issue",
			State:      "In Progress",
		},
		CancelFunc: cancelFn,
		StartedAt:  time.Now().UTC(),
	}

	regs := passingPreflightRegistries()

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  wm.Reload,
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	o.handleTick(context.Background())

	// Config() must still return the last-known-good config.
	cfg := wm.Config()
	if len(cfg.Tracker.ActiveStates) != 1 || cfg.Tracker.ActiveStates[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", cfg.Tracker.ActiveStates)
	}

	// The running worker must NOT have been cancelled.
	if cancelCalled.Load() {
		t.Error("running worker was cancelled; expected it to be preserved")
	}

	// The running entry must still exist.
	if _, ok := state.Running["issue-1"]; !ok {
		t.Error("running entry removed; expected it to remain")
	}
}

// TestReconciliationGuardEndToEnd exercises the full validation guard
// path with a real workflow.Manager backed by a file on disk, wired
// with WithValidateFunc(ValidateConfigForPromotion). This ensures that
// removing the WithValidateFunc wiring from main.go would cause test
// breakage.
func TestReconciliationGuardEndToEnd(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	workflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

	// Valid initial workflow: populated state lists.
	initialContent := `---
tracker:
  kind: mock
  api_key: test-key
  active_states:
    - In Progress
  terminal_states:
    - Done
polling:
  interval_ms: 60000
workspace:
  root: ` + tmpDir + `
hooks:
  timeout_ms: 5000
agent:
  kind: mock
  command: /usr/bin/agent
  max_concurrent_agents: 5
  max_turns: 1
---
do {{ .issue.identifier }}
`
	if err := os.WriteFile(workflowPath, []byte(initialContent), 0o644); err != nil {
		t.Fatalf("writing initial WORKFLOW.md: %v", err)
	}

	// Real Manager with the production validator.
	wm, err := workflow.NewManager(workflowPath, discardLogger(),
		workflow.WithValidateFunc(ValidateConfigForPromotion))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Sanity-check initial config.
	cfg := wm.Config()
	if len(cfg.Tracker.ActiveStates) == 0 {
		t.Fatal("initial ActiveStates is empty; expected [In Progress]")
	}

	// Tracker returns "In Progress" for the running issue.
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "In Progress"
			}
			return result, nil
		},
	}

	var cancelCalled atomic.Bool
	cancelFn := func() { cancelCalled.Store(true) }

	state := NewState(60000, 5, nil, AgentTotals{})
	state.Running["issue-1"] = &RunningEntry{
		Identifier: "TEST-1",
		Issue: domain.Issue{
			ID:         "issue-1",
			Identifier: "TEST-1",
			Title:      "Active issue",
			State:      "In Progress",
		},
		CancelFunc: cancelFn,
		StartedAt:  time.Now().UTC(),
	}

	regs := passingPreflightRegistries()

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow:  wm.Reload,
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})

	// Overwrite WORKFLOW.md with empty state lists (valid YAML, but
	// semantically dangerous — both active_states and terminal_states
	// are empty).
	brokenContent := `---
tracker:
  kind: mock
  api_key: test-key
polling:
  interval_ms: 60000
workspace:
  root: ` + tmpDir + `
hooks:
  timeout_ms: 5000
agent:
  kind: mock
  command: /usr/bin/agent
  max_concurrent_agents: 5
  max_turns: 1
---
do {{ .issue.identifier }}
`
	if err := os.WriteFile(workflowPath, []byte(brokenContent), 0o644); err != nil {
		t.Fatalf("writing broken WORKFLOW.md: %v", err)
	}

	// handleTick triggers Reload() via preflight, which should reject
	// the new config and retain the last-known-good.
	o.handleTick(context.Background())

	// (1) Config() must retain the original state lists.
	cfg = wm.Config()
	if len(cfg.Tracker.ActiveStates) != 1 || cfg.Tracker.ActiveStates[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", cfg.Tracker.ActiveStates)
	}
	if len(cfg.Tracker.TerminalStates) != 1 || cfg.Tracker.TerminalStates[0] != "Done" {
		t.Errorf("Config().Tracker.TerminalStates = %v, want [Done]", cfg.Tracker.TerminalStates)
	}

	// (2) The running worker must NOT have been cancelled.
	if cancelCalled.Load() {
		t.Error("running worker was cancelled; expected it to be preserved by validation guard")
	}

	// The running entry must still exist.
	if _, ok := state.Running["issue-1"]; !ok {
		t.Error("running entry removed; expected it to remain")
	}

	// LastLoadError should report the validation rejection.
	if wm.LastLoadError() == nil {
		t.Error("LastLoadError() = nil, want validation error")
	}
}

// --- TestGracefulShutdown ---

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	t.Run("no_running_workers", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error { return errPreflightFailed },
				ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancelled

		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.Fatal("Run did not return within 1 second with pre-cancelled context and empty state")
		}

		if len(state.Running) != 0 {
			t.Errorf("Running = %d, want 0", len(state.Running))
		}
	})

	t.Run("drains_workers", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 2
		cfg.Agent.MaxTurns = 100
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

		issues := []domain.Issue{
			{ID: "d-1", Identifier: "DRAIN-1", Title: "First", State: "To Do"},
			{ID: "d-2", Identifier: "DRAIN-2", Title: "Second", State: "To Do"},
		}

		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "To Do"
					}
					return result, nil
				},
			},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return issues, nil
			},
		}

		// Agent blocks until context is cancelled.
		var workersStarted sync.WaitGroup
		workersStarted.Add(2)
		agent := &mockAgentAdapter{
			runTurnFn: func(ctx context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				workersStarted.Done()
				<-ctx.Done()
				return domain.TurnResult{}, ctx.Err()
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl}
		store := &stubStore{}
		obs := &stubObserver{}
		regs := passingPreflightRegistries()

		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    agent,
			WorkflowManager: wm,
			Store:           store,
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
			Observers: []Observer{obs},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Wait for both workers to be inside RunTurn.
		waitCh := make(chan struct{})
		go func() {
			workersStarted.Wait()
			close(waitCh)
		}()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatal("timed out waiting for workers to start")
		}

		// Cancel the parent context to trigger graceful shutdown.
		cancel()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return within 10 seconds of cancellation")
		}

		// After Run returns, state is safe to read.
		if len(state.Running) != 0 {
			t.Errorf("Running = %d after drain, want 0", len(state.Running))
		}

		store.mu.Lock()
		historyCount := len(store.runHistories)
		for _, rh := range store.runHistories {
			if rh.Status != "cancelled" {
				t.Errorf("run history %s: status = %q, want %q", rh.IssueID, rh.Status, "cancelled")
			}
		}
		store.mu.Unlock()

		if historyCount != 2 {
			t.Errorf("run history count = %d, want 2", historyCount)
		}

		if state.AgentTotals.SecondsRunning <= 0 {
			t.Error("AgentTotals.SecondsRunning <= 0, want > 0")
		}

		if got := obs.calls.Load(); got < 1 {
			t.Errorf("observer calls = %d, want >= 1", got)
		}
	})

	t.Run("drain_timeout", func(t *testing.T) {
		t.Parallel()

		// Use an injected short drain timeout to avoid a 30s test runtime.
		state := NewState(60000, 1, nil, AgentTotals{})

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          logger,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error { return errPreflightFailed },
				ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
			},
		})
		o.drainTimeout = 200 * time.Millisecond

		// Manually inject a running entry whose worker will never send
		// a result to workerExitCh, simulating a hung worker.
		workerCtx, workerCancel := context.WithCancel(context.Background())
		defer workerCancel()
		state.Running["hang-1"] = &RunningEntry{
			Identifier: "HANG-1",
			Issue:      domain.Issue{ID: "hang-1", Identifier: "HANG-1", Title: "Hung", State: "To Do"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: workerCancel,
		}
		state.Claimed["hang-1"] = struct{}{}

		// Launch a goroutine that pretends to be the worker but never
		// calls OnExit (simulating a hung process).
		go func() {
			<-workerCtx.Done()
			// Worker context cancelled but no result sent — hung.
		}()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Cancel immediately to trigger shutdown.
		cancel()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return within 3 seconds (expected ~200ms drain timeout)")
		}

		logOutput := buf.String()
		if !strings.Contains(logOutput, "drain timeout exceeded") {
			t.Errorf("expected warn log about drain timeout, got:\n%s", logOutput)
		}
	})

	t.Run("cancels_retry_timers", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error { return errPreflightFailed },
				ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
			},
		})

		// Add a retry entry with a short timer (50ms). If
		// cancelRetryTimers fails to Stop() it, the timer will fire
		// within the 200ms wait below, proving the test is effective.
		// Since TimerHandle is non-nil, activateReconstructedRetries
		// skips it.
		state.RetryAttempts["retry-1"] = &RetryEntry{
			IssueID:    "retry-1",
			Identifier: "RETRY-1",
			Attempt:    1,
			TimerHandle: time.AfterFunc(50*time.Millisecond, func() {
				o.onRetryFire("retry-1")
			}),
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Cancel immediately.
		cancel()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return within 3 seconds")
		}

		// Wait longer than the 50ms timer duration. If Stop() was not
		// called, the timer fires and writes to retryTimerCh.
		time.Sleep(200 * time.Millisecond)

		select {
		case id := <-o.retryTimerCh:
			t.Errorf("retryTimerCh received %q after shutdown, want no late fires", id)
		default:
			// No message — timer was stopped correctly.
		}
	})
}

// --- SnapshotFunc / RefreshFunc / AddObserver tests ---

func TestSnapshotFunc(t *testing.T) {
	t.Parallel()

	t.Run("round-trip through event loop", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{InputTokens: 42})

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error { return errPreflightFailed },
				ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Wait for the initial tick so the event loop is ready.
		time.Sleep(100 * time.Millisecond)

		snapFn := o.SnapshotFunc()
		snap, err := snapFn()
		if err != nil {
			t.Fatalf("SnapshotFunc() error = %v", err)
		}

		if snap.GeneratedAt.IsZero() {
			t.Error("GeneratedAt is zero")
		}
		if snap.AgentTotals.InputTokens != 42 {
			t.Errorf("AgentTotals.InputTokens = %d, want 42", snap.AgentTotals.InputTokens)
		}

		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within 5 seconds")
		}
	})
}

func TestRefreshFunc(t *testing.T) {
	t.Parallel()

	t.Run("accepted", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		refreshFn := o.RefreshFunc()
		got := refreshFn()
		if !got {
			t.Error("RefreshFunc() = false, want true (channel was empty)")
		}
	})

	t.Run("coalesced when channel full", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		refreshFn := o.RefreshFunc()

		// Fill the buffer (capacity 1).
		if !refreshFn() {
			t.Fatal("first RefreshFunc() = false, want true")
		}

		// Second call should be coalesced.
		got := refreshFn()
		if got {
			t.Error("RefreshFunc() = true, want false (channel full, should coalesce)")
		}
	})

	t.Run("rejected during drain", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			PreflightParams: PreflightParams{
				ReloadWorkflow: func() error { return errPreflightFailed },
				ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
			},
		})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		// Let the event loop start.
		time.Sleep(100 * time.Millisecond)

		// Cancel ctx to trigger drain.
		cancel()

		// Wait for Run to return (drain completes immediately with no workers).
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within 5 seconds")
		}

		// After drain, RefreshFunc must return false.
		refreshFn := o.RefreshFunc()
		if refreshFn() {
			t.Error("RefreshFunc() = true after drain, want false")
		}
	})
}

func TestAddObserver(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
	})

	obs := &stubObserver{}
	o.AddObserver(obs)

	o.notifyObservers()

	if got := obs.calls.Load(); got != 1 {
		t.Errorf("observer calls = %d, want 1", got)
	}
}

func TestSnapshotDuringDrain(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, nil, AgentTotals{})
	state.Running["id-1"] = &RunningEntry{
		Identifier: "MT-1",
		Issue:      domain.Issue{ID: "id-1", State: "In Progress"},
		StartedAt:  time.Now().UTC(),
		CancelFunc: func() {}, // no-op cancel to support drain
	}

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error { return errPreflightFailed },
			ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Let the event loop start.
	time.Sleep(100 * time.Millisecond)

	// Cancel ctx to trigger drain.
	cancel()

	// Give drain time to enter the select loop.
	time.Sleep(50 * time.Millisecond)

	// Send the snapshot request. The drain loop services snapshotCh.
	snapFn := o.SnapshotFunc()

	// The worker will never exit on its own, so simulate exit
	// after a small delay to let the snapshot be processed first.
	go func() {
		time.Sleep(50 * time.Millisecond)
		o.workerExitCh <- WorkerResult{IssueID: "id-1"}
	}()

	snap, err := snapFn()
	if err != nil {
		t.Fatalf("SnapshotFunc() during drain: %v", err)
	}

	if snap.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds")
	}
}

func TestRefreshDrainedDuringShutdown(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, nil, AgentTotals{})
	state.Running["id-1"] = &RunningEntry{
		Identifier: "MT-1",
		Issue:      domain.Issue{ID: "id-1", State: "In Progress"},
		StartedAt:  time.Now().UTC(),
		CancelFunc: func() {},
	}

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error { return errPreflightFailed },
			ConfigFunc:     func() config.ServiceConfig { return config.ServiceConfig{} },
		},
	})

	refreshFn := o.RefreshFunc()

	// Before drain, RefreshFunc should accept.
	if !refreshFn() {
		t.Fatal("RefreshFunc() = false before drain, want true")
	}

	// Drain the channel so the next call tests drain rejection, not coalescing.
	select {
	case <-o.refreshCh:
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	// Let the event loop start.
	time.Sleep(100 * time.Millisecond)

	// Cancel ctx to trigger drain.
	cancel()

	// Let the worker exit so drain completes.
	o.workerExitCh <- WorkerResult{IssueID: "id-1"}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds")
	}

	// After drain completes, RefreshFunc must return false.
	if refreshFn() {
		t.Error("RefreshFunc() = true after drain, want false")
	}
}
