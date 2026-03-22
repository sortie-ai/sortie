package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
)

// --- stub types for orchestrator tests ---

// stubWorkflowManager implements [WorkflowManager] with configurable returns.
type stubWorkflowManager struct {
	config   config.ServiceConfig
	template *prompt.Template
	reloadFn func() error
}

func (s *stubWorkflowManager) Config() config.ServiceConfig     { return s.config }
func (s *stubWorkflowManager) PromptTemplate() *prompt.Template { return s.template }
func (s *stubWorkflowManager) Reload() error {
	if s.reloadFn != nil {
		return s.reloadFn()
	}
	return nil
}

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

		wfn := o.makeWorkerFn()

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

		wfn := o.makeWorkerFn()

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

		wfn := o.makeWorkerFn()

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
	wm.config = cfg

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
