package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	mu            sync.RWMutex
	config        config.ServiceConfig
	template      *prompt.Template
	templateIndex map[string]*prompt.Template
	reloadFn      func() error
	absPath       string
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

func (s *stubWorkflowManager) PromptTemplateByID(id string) *prompt.Template {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.templateIndex != nil {
		if tmpl, ok := s.templateIndex[id]; ok {
			return tmpl
		}
	}
	if id == "" {
		return s.template
	}
	return nil
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
	unsupportedReactionObservationStore

	mu              sync.Mutex
	runHistories    []persistence.RunHistory
	aggregates      []persistence.AggregateMetrics
	sessions        []persistence.SessionMetadata
	savedRetries    []persistence.RetryEntry
	deletedRetryIDs []string
	// Budget exhaustion query configuration (per-tick rebuild). The map
	// value is the run-history session count for that issue.
	budgetExhaustedIDs map[string]int
	budgetExhaustedErr error
	absenceCounts      map[string]int
	absenceCountErr    error
	absenceQueryCalls  int
	absenceResetOf     []string

	// Token budget query configuration (per-tick rebuild and single-issue gate).
	tokenExhaustedIDs []string
	tokenExhaustedErr error
	tokenSum          int64
	tokenSessionCount int
	tokenUnmeasured   int

	// tokenIncompleteIDs reports a candidate below the token ceiling with
	// one unmeasured session, for the per-tick token budget rebuild's
	// "cannot be fully evaluated" outcome. Distinct from tokenExhaustedIDs,
	// which reports a candidate at the ceiling.
	tokenIncompleteIDs []string

	upsertSessionMetadataErr error

	parkedIssues       []persistence.ParkedIssue
	deletedParkedIDs   []string
	labelAppliedIDs    []string
	listParkedIssues   []persistence.ParkedIssue
	listParkedIssueErr error

	budgetHoldNotices         []persistence.BudgetHoldNotice
	upsertBudgetHoldNoticeErr error
	deletedBudgetHoldIDs      []string
	deleteBudgetHoldNoticeErr error
	deleteAllBudgetHoldCalls  int
	deleteAllBudgetHoldErr    error
	listBudgetHoldNotices     []persistence.BudgetHoldNotice
	listBudgetHoldNoticesErr  error
}

var _ OrchestratorStore = (*stubStore)(nil)

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
	return s.upsertSessionMetadataErr
}

// sessionWrites returns a copy of the captured session metadata writes.
func (s *stubStore) sessionWrites() []persistence.SessionMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]persistence.SessionMetadata, len(s.sessions))
	copy(out, s.sessions)
	return out
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

func (s *stubStore) TokenUsageByIssue(_ context.Context, _ string) (persistence.IssueTokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.IssueTokenUsage{
		TotalTokens:        s.tokenSum,
		Sessions:           s.tokenSessionCount,
		UnmeasuredSessions: s.tokenUnmeasured,
	}, nil
}

func (s *stubStore) QueryBudgetExhaustedIssues(_ context.Context, _ []string, _ int) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budgetExhaustedErr != nil {
		return nil, s.budgetExhaustedErr
	}
	result := make(map[string]int, len(s.budgetExhaustedIDs))
	maps.Copy(result, s.budgetExhaustedIDs)
	return result, nil
}

func (s *stubStore) QueryConsecutiveHandoffAbsenceCounts(_ context.Context, candidateIDs []string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absenceQueryCalls++
	if s.absenceCountErr != nil {
		return nil, s.absenceCountErr
	}
	result := make(map[string]int, len(candidateIDs))
	for _, id := range candidateIDs {
		result[id] = s.absenceCounts[id]
	}
	return result, nil
}

func (s *stubStore) ResetHandoffAbsenceSequence(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absenceResetOf = append(s.absenceResetOf, issueID)
	delete(s.absenceCounts, issueID)
	return nil
}

// QueryTokenBudgetUsage reports a candidate named in tokenExhaustedIDs as
// carrying a total at the fixed threshold every test in this file
// configures (1000), so the caller's threshold comparison marks it
// exhausted; every other candidate is absent, which the caller reads as
// zero spend.
func (s *stubStore) QueryTokenBudgetUsage(_ context.Context, candidateIDs []string) (map[string]persistence.IssueTokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenExhaustedErr != nil {
		return nil, s.tokenExhaustedErr
	}
	usage := make(map[string]persistence.IssueTokenUsage, len(candidateIDs))
	for _, id := range candidateIDs {
		switch {
		case slices.Contains(s.tokenExhaustedIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 1000}
		case slices.Contains(s.tokenIncompleteIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 0, UnmeasuredSessions: 1}
		}
	}
	return usage, nil
}

func (s *stubStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	return nil
}

func (s *stubStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

func (s *stubStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	return nil
}

func (s *stubStore) LatestRunCompletionByIdentifier(_ context.Context, _ []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *stubStore) UpsertParkedIssue(_ context.Context, entry persistence.ParkedIssue) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parkedIssues = append(s.parkedIssues, entry)
	return nil
}

func (s *stubStore) DeleteParkedIssue(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedParkedIDs = append(s.deletedParkedIDs, issueID)
	return nil
}

func (s *stubStore) MarkParkedIssueLabelApplied(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labelAppliedIDs = append(s.labelAppliedIDs, issueID)
	return nil
}

func (s *stubStore) ListParkedIssues(_ context.Context) ([]persistence.ParkedIssue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listParkedIssueErr != nil {
		return nil, s.listParkedIssueErr
	}
	out := make([]persistence.ParkedIssue, len(s.listParkedIssues))
	copy(out, s.listParkedIssues)
	return out, nil
}

func (s *stubStore) UpsertBudgetHoldNotice(_ context.Context, notice persistence.BudgetHoldNotice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertBudgetHoldNoticeErr != nil {
		return s.upsertBudgetHoldNoticeErr
	}
	s.budgetHoldNotices = append(s.budgetHoldNotices, notice)
	return nil
}

func (s *stubStore) DeleteBudgetHoldNotice(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedBudgetHoldIDs = append(s.deletedBudgetHoldIDs, issueID)
	return s.deleteBudgetHoldNoticeErr
}

func (s *stubStore) DeleteAllBudgetHoldNotices(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAllBudgetHoldCalls++
	return s.deleteAllBudgetHoldErr
}

func (s *stubStore) ListBudgetHoldNotices(_ context.Context) ([]persistence.BudgetHoldNotice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listBudgetHoldNoticesErr != nil {
		return nil, s.listBudgetHoldNoticesErr
	}
	out := make([]persistence.BudgetHoldNotice, len(s.listBudgetHoldNotices))
	copy(out, s.listBudgetHoldNotices)
	return out, nil
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
		// Effort budget exhausted.
		{
			name:      "budget exhausted blocks dispatch",
			issue:     baseIssue,
			activeSet: activeSet, terminalS: terminalSet,
			setupState: func(s *State) {
				s.BudgetExhausted[baseIssue.ID] = &BudgetExhaustedEntry{}
			},
			want: false,
		},
		{
			name:      "budget exhausted for different ID allows dispatch",
			issue:     baseIssue,
			activeSet: activeSet, terminalS: terminalSet,
			setupState: func(s *State) {
				s.BudgetExhausted["other-id"] = &BudgetExhaustedEntry{}
			},
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

		wfn := o.makeWorkerFn("", "", "", "", "", nil)

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil)

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

		wfn := o.makeWorkerFn("resume-sess-42", "", "", "", "", nil)

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil)

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

// TestMakeWorkerFn_DerivesPostureFromReactionKind verifies that
// makeWorkerFn derives WorkerDeps.Posture from the reactionKind argument
// via dispatchPostureForReactionKind: ReactionKindLabelReview selects
// PostureReview, ReactionKindLabelFix selects PostureFix, and every other
// kind (including empty) selects PostureNormal (A3). Each case asserts
// the pure mapping output directly, then asserts the derived
// WorkerDeps.Posture indirectly via the dispatch-time in-progress
// transition, which only a DrivesIssueState-true posture performs.
func TestMakeWorkerFn_DerivesPostureFromReactionKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reactionKind string
		wantPosture  DispatchPosture
	}{
		{"label-review reaction kind selects PostureReview", ReactionKindLabelReview, PostureReview},
		{"label-fix reaction kind selects PostureFix", ReactionKindLabelFix, PostureFix},
		{"empty reaction kind selects PostureNormal", "", PostureNormal},
		{"other known reaction kind selects PostureNormal", ReactionKindReview, PostureNormal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := dispatchPostureForReactionKind(tt.reactionKind); got != tt.wantPosture {
				t.Errorf("dispatchPostureForReactionKind(%q) = %v, want %v", tt.reactionKind, got, tt.wantPosture)
			}

			tmpDir := t.TempDir()
			cfg := defaultWorkerConfig(tmpDir)
			cfg.Tracker.InProgressState = "In Progress"
			tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

			tracker := &mockTrackerAdapter{}
			state := NewState(1000, 5, nil, AgentTotals{})
			o := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  tracker,
				AgentAdapter:    &mockAgentAdapter{},
				WorkflowManager: &stubWorkflowManager{config: cfg, template: tmpl},
				Store:           &stubStore{},
			})

			issue := workerTestIssue()
			wfn := o.makeWorkerFn("", "", "", "", tt.reactionKind, nil)

			exitDone := make(chan struct{})
			go func() {
				wfn(context.Background(), issue, nil)
				close(exitDone)
			}()

			select {
			case <-o.workerExitCh:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not exit within 10 seconds")
			}
			<-exitDone

			gotTransitioned := len(tracker.transitionCalls) > 0
			if gotTransitioned != tt.wantPosture.DrivesIssueState() {
				t.Errorf("makeWorkerFn(reactionKind=%q): TransitionIssue called = %v, want %v",
					tt.reactionKind, gotTransitioned, tt.wantPosture.DrivesIssueState())
			}
		})
	}
}

// TestMakeWorkerFn_PostureMappingSharedWithHandleWorkerExit verifies that
// HandleWorkerExit derives its drivesIssue gate from the same
// dispatchPostureForReactionKind mapping makeWorkerFn uses (A3), so the
// dispatch builder and the exit handler can never disagree on a reaction
// kind's posture. Observed via the continuation-retry branch, which fires
// on a normal exit with an active issue and no handoff configured only
// when DrivesIssueState is true.
func TestMakeWorkerFn_PostureMappingSharedWithHandleWorkerExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		reactionKind string
		wantPosture  DispatchPosture
	}{
		{"label-review reaction kind selects PostureReview", ReactionKindLabelReview, PostureReview},
		{"label-fix reaction kind selects PostureFix", ReactionKindLabelFix, PostureFix},
		{"empty reaction kind selects PostureNormal", "", PostureNormal},
		{"other known reaction kind selects PostureNormal", ReactionKindReview, PostureNormal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "issue-1"
			state := NewState(1000, 5, nil, AgentTotals{})
			state.Claimed[issueID] = struct{}{}
			state.Running[issueID] = &RunningEntry{
				Identifier:   "TEST-1",
				ReactionKind: tt.reactionKind,
				Issue:        domain.Issue{ID: issueID, State: "To Do"},
			}

			HandleWorkerExit(state, WorkerResult{
				IssueID:    issueID,
				Identifier: "TEST-1",
				ExitKind:   WorkerExitNormal,
			}, HandleWorkerExitParams{
				Store:        &stubStore{},
				Logger:       discardLogger(),
				OnRetryFire:  func(_ string) {},
				ActiveStates: []string{"To Do"},
			})

			_, gotRetryScheduled := state.RetryAttempts[issueID]
			if gotRetryScheduled != tt.wantPosture.DrivesIssueState() {
				t.Errorf("HandleWorkerExit(reactionKind=%q): continuation retry scheduled = %v, want %v",
					tt.reactionKind, gotRetryScheduled, tt.wantPosture.DrivesIssueState())
			}
		})
	}
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

// TestHandleTick_PassesEmptyReactionKind verifies that the poll-tick
// candidate-dispatch call site invokes makeWorkerFn with an empty
// reactionKind (A9): a freshly dispatched candidate issue is never
// read-only, so its dispatch-time in-progress transition still fires.
func TestHandleTick_PassesEmptyReactionKind(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Tracker.Kind = "mock"
	cfg.Tracker.InProgressState = "In Progress"
	cfg.Agent.MaxConcurrentAgents = 5

	issue := domain.Issue{ID: "iss-tick-1", Identifier: "TICK-1", Title: "title", State: "To Do"}
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return []domain.Issue{issue}, nil
		},
	}

	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

	tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

	o := NewOrchestrator(OrchestratorParams{
		State:           NewState(1000, 5, nil, AgentTotals{}),
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg, template: tmpl},
		Store:           &stubStore{},
		PreflightParams: pf,
	})

	o.handleTick(context.Background())
	o.state.WorkerWg.Wait()

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1 (a fresh poll-tick dispatch is never read-only)", len(tracker.transitionCalls))
	}
	if tracker.transitionCalls[0].IssueID != issue.ID {
		t.Errorf("TransitionIssue IssueID = %q, want %q", tracker.transitionCalls[0].IssueID, issue.ID)
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
			metaFunc: func(string) (registry.TrackerMeta, bool) { return registry.TrackerMeta{}, true },
		},
		AgentRegistry: &stubAgentRegistry{
			getFunc:  func(string) (registry.AgentConstructor, error) { return nil, nil },
			metaFunc: func(string) (registry.AgentMeta, bool) { return registry.AgentMeta{}, true },
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
		{ID: "ip-1", Identifier: "IP-1", Title: "A", State: "In Progress", Priority: new(1)},
		{ID: "ip-2", Identifier: "IP-2", Title: "B", State: "In Progress", Priority: new(1)},
		{ID: "ip-3", Identifier: "IP-3", Title: "C", State: "In Progress", Priority: new(1)},
		{ID: "td-1", Identifier: "TD-1", Title: "D", State: "To Do", Priority: new(2)},
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
		} else if got, ok := v.(string); !ok || !strings.HasPrefix(got, "do P-1") {
			t.Errorf("prompt for P-1 = %q, want prefix %q", got, "do P-1")
		}

		if v, ok := capturedPrompts.Load("P-2"); !ok {
			t.Error("no prompt captured for P-2")
		} else if got, ok := v.(string); !ok || !strings.HasPrefix(got, "review P-2") {
			t.Errorf("prompt for P-2 = %q, want prefix %q", got, "review P-2")
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

	// Test Case: agent.max_consecutive_absences takes effect on the next
	// poll tick, the next retry timer fire, and the next worker exit
	// with no restart. The retry-timer and worker-exit lanes are driven
	// directly rather than through the running event loop, reading
	// wm.Config() after the reload exactly as the event loop's own
	// select arms do (cfg := o.workflowManager.Config() per event).
	t.Run("max_consecutive_absences_change", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.Agent.MaxConsecutiveAbsences = 10
		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		const pollIssueID = "RELOAD-ABS-POLL"
		pollIssue := domain.Issue{ID: pollIssueID, Identifier: "PROJ-POLL", Title: "T", State: "In Progress"}
		store := &stubStore{absenceCounts: map[string]int{pollIssueID: 3}}
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return []domain.Issue{pollIssue}, nil
			},
		}

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           store,
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
		})

		o.handleTick(context.Background())
		if _, ok := state.Parked[pollIssueID]; ok {
			t.Fatal("parked at three absences before the ten-value ceiling was reloaded down")
		}

		cfg.Agent.MaxConsecutiveAbsences = 3
		wm.setConfig(cfg)
		o.handleTick(context.Background())
		if _, ok := state.Parked[pollIssueID]; !ok {
			t.Error("poll tick did not observe the reloaded ceiling with no restart")
		}

		const retryIssueID = "RELOAD-ABS-RETRY"
		retryStore := &mockRetryStore{absenceCounts: map[string]int{retryIssueID: 3}}
		retryTracker := &mockRetryTracker{}
		retryIssueState := retryState(t, retryIssueID, "PROJ-RETRY", 1)
		retryParams := defaultRetryParams(t, retryStore, retryTracker)
		retryParams.MaxConsecutiveAbsences = wm.Config().Agent.MaxConsecutiveAbsences

		HandleRetryTimer(retryIssueState, retryIssueID, retryParams)
		retryIssueState.TrackerOpsWg.Wait()

		if entry := retryIssueState.Parked[retryIssueID]; entry == nil || entry.Reason != parkReasonHandoffAbsence {
			t.Errorf("Parked[%s] = %+v, want the reloaded ceiling to park at three absences with no restart", retryIssueID, entry)
		}

		const exitIssueID = "RELOAD-ABS-EXIT"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		exitStore := &mockExitStore{}
		seedMockHandoffAbsences(exitStore, exitIssueID, 2)
		exitTracker := newRecordingHandoffTracker()
		exitIssueState := exitStateWithIssue(t, exitIssueID, "In Progress")
		exitParams := handoffEvidenceExitParams(t, exitStore, exitTracker.mockTrackerAdapter, &spyMetrics{})
		exitParams.TrackerAdapter = exitTracker
		exitParams.MaxConsecutiveAbsences = wm.Config().Agent.MaxConsecutiveAbsences

		HandleWorkerExit(exitIssueState, WorkerResult{
			IssueID:                 exitIssueID,
			Identifier:              "PROJ-EXIT",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, exitParams)
		exitIssueState.TrackerOpsWg.Wait()

		if entry := exitIssueState.Parked[exitIssueID]; entry == nil || entry.Reason != parkReasonHandoffAbsence {
			t.Errorf("Parked[%s] = %+v, want the reloaded ceiling to park the third absence with no restart", exitIssueID, entry)
		}
	})

	// Test Case: a reload whose agent.max_consecutive_absences fails
	// validation retains the previously loaded value and reports the
	// failure through LastLoadError(), the same fail-safe path the
	// sibling agent.turn_timeout_ms field already relies on.
	t.Run("max_consecutive_absences_reload_failure_retains_previous_value", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		workflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

		validContent := `---
tracker:
  kind: mock
  api_key: test-key
  active_states:
    - To Do
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
  max_consecutive_absences: 7
---
do {{ .issue.identifier }}
`
		if err := os.WriteFile(workflowPath, []byte(validContent), 0o644); err != nil {
			t.Fatalf("writing initial WORKFLOW.md: %v", err)
		}

		wm, err := workflow.NewManager(workflowPath, discardLogger())
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		if got := wm.Config().Agent.MaxConsecutiveAbsences; got != 7 {
			t.Fatalf("initial Config().Agent.MaxConsecutiveAbsences = %d, want 7", got)
		}

		invalidContent := `---
tracker:
  kind: mock
  api_key: test-key
  active_states:
    - To Do
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
  max_consecutive_absences: 0
---
do {{ .issue.identifier }}
`
		if err := os.WriteFile(workflowPath, []byte(invalidContent), 0o644); err != nil {
			t.Fatalf("writing broken WORKFLOW.md: %v", err)
		}

		if err := wm.Reload(); err == nil {
			t.Fatal("Reload() error = nil, want error")
		}

		if got := wm.Config().Agent.MaxConsecutiveAbsences; got != 7 {
			t.Errorf("after failed Reload: Config().Agent.MaxConsecutiveAbsences = %d, want 7 (retained)", got)
		}
		if wm.LastLoadError() == nil {
			t.Error("after failed Reload: LastLoadError() is nil, want non-nil")
		}
	})

	// Test Case C: reactions.ci_failure.watch_window_ms takes effect on
	// the next reconcile tick without a process restart, since
	// cfg.CIFeedback is rebuilt from o.workflowManager.Config() every
	// tick.
	t.Run("ci_watch_window_change", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.CIFeedback = config.CIFeedbackConfig{
			Kind:          "mock",
			MaxRetries:    2,
			Escalation:    "label",
			WatchWindowMS: 24 * 3600 * 1000, // 24h: comfortably survives the first tick
		}

		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		obs := &stubObserver{}
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		const issueID = "CI-RELOAD-1"
		rkey := ReactionKey(issueID, ReactionKindCI)
		state.PendingReactions[rkey] = &PendingReaction{
			IssueID:    issueID,
			Identifier: issueID + "-ident",
			DisplayID:  issueID + "-ident",
			Kind:       ReactionKindCI,
			CreatedAt:  time.Now().UTC(),
			KindData:   &CIReactionData{PRNumber: 1, Owner: "acme", Repo: "widgets", Branch: "main"},
		}

		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				return nil, nil
			},
		}
		ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
		scm := defaultCISCM()

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: wm,
			Store:           &stubStore{},
			CIProvider:      ci,
			SCMAdapter:      scm,
			PreflightParams: PreflightParams{
				ReloadWorkflow:  func() error { return nil },
				ConfigFunc:      wm.Config,
				TrackerRegistry: regs.TrackerRegistry,
				AgentRegistry:   regs.AgentRegistry,
			},
			Observers: []Observer{obs},
		})

		// The first tick observes the head for the first time (the
		// fingerprint store has no prior row), which records
		// HeadRecordedAt as the entry's age basis; a passing status
		// keeps the watch open per the current-head contract.
		o.handleTick(context.Background())

		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Fatal("PendingReactions entry dropped on the first tick; want kept (well inside the 24h window)")
		}

		// Age the entry deterministically rather than sleeping for a
		// wall-clock gap: push the recorded head an hour into the past,
		// then reload a window far smaller than that. The assertion then
		// tests the reload path itself instead of racing a loaded runner.
		state.PendingReactions[rkey].HeadRecordedAt = time.Now().UTC().Add(-time.Hour)
		cfg.CIFeedback.WatchWindowMS = 5
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry kept after the watch window was reloaded to 5ms; want dropped without a restart")
		}
	})

	// Test Case H: the ci_failure triage block is frozen at construction,
	// unlike every other ci_failure field, which the tick above already
	// shows takes effect live.
	t.Run("ci_triage_frozen_at_construction", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.CIFeedback = config.CIFeedbackConfig{
			Kind:       "github",
			MaxRetries: 5,
			Triage:     config.ReactionTriageConfig{Script: "script-A", TimeoutMS: 1000},
		}

		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
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

		if o.ciTriage.Script != "script-A" {
			t.Fatalf("ciTriage.Script at construction = %q, want %q", o.ciTriage.Script, "script-A")
		}

		o.handleTick(context.Background())

		if o.ciTriage.Script != "script-A" {
			t.Errorf("ciTriage.Script after the first tick = %q, want %q", o.ciTriage.Script, "script-A")
		}

		// A sibling ci_failure field changes in the same reload as the
		// triage script.
		cfg.CIFeedback.Triage.Script = "script-B"
		cfg.CIFeedback.MaxRetries = 2
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		if o.ciTriage.Script != "script-A" {
			t.Errorf("ciTriage.Script after reload = %q, want %q (frozen at construction, never re-read)", o.ciTriage.Script, "script-A")
		}
		if got := wm.Config().CIFeedback.MaxRetries; got != 2 {
			t.Errorf("workflowManager.Config().CIFeedback.MaxRetries after reload = %d, want 2 (a sibling field the reload does refresh)", got)
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

// TestReloadPromotesConfigWithMissingBlockFault asserts that a workflow
// whose only fault is a dispatch-routed kind with no settings block
// still loads and promotes through Manager.Reload() without error, and
// that ValidateDispatchConfig over the promoted config reports the
// dispatch.agent.missing_block error. This pins the fail-safe reload
// invariant: the fault surfaces at tick preflight, never at reload, so
// Reload() itself must never fail on it.
func TestReloadPromotesConfigWithMissingBlockFault(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	workflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

	content := `---
tracker:
  kind: mock
  active_states:
    - In Progress
  terminal_states:
    - Done
workspace:
  root: ` + tmpDir + `
agent:
  kind: mock
  command: /usr/bin/agent
dispatch:
  rules:
    - agent: codex
---
do {{ .issue.identifier }}
`
	if err := os.WriteFile(workflowPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing WORKFLOW.md: %v", err)
	}

	wm, err := workflow.NewManager(workflowPath, discardLogger(),
		workflow.WithValidateFunc(ValidateConfigForPromotion))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := wm.Reload(); err != nil {
		t.Fatalf("Reload() = %v, want nil: a missing settings block must not block promotion", err)
	}

	cfg := wm.Config()
	if cfg.Agent.Kind != "mock" {
		t.Fatalf("Config().Agent.Kind = %q, want %q: the promoted config must reflect the reloaded file", cfg.Agent.Kind, "mock")
	}

	regs := passingPreflightRegistries()
	result := ValidateDispatchConfig(PreflightParams{
		ReloadWorkflow:  func() error { return nil },
		ConfigFunc:      func() config.ServiceConfig { return cfg },
		TrackerRegistry: regs.TrackerRegistry,
		AgentRegistry:   regs.AgentRegistry,
	})

	requireCheck(t, result, "dispatch.agent.missing_block")
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
		// skips it. DueAtMS reflects the timer's real due time so the
		// reconcile loop's overdue-retry re-arm pass does not treat this
		// freshly-scheduled entry as one whose timer event was dropped.
		state.RetryAttempts["retry-1"] = &RetryEntry{
			IssueID:    "retry-1",
			Identifier: "RETRY-1",
			Attempt:    1,
			DueAtMS:    time.Now().Add(50 * time.Millisecond).UnixMilli(),
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

	t.Run("drains_in_flight_triage_run", func(t *testing.T) {
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

		const runDuration = 150 * time.Millisecond
		start := time.Now()
		finished := make(chan struct{})
		state.TriageInFlight.Add(1)
		state.TriageWg.Go(func() {
			time.Sleep(runDuration)
			state.TriageInFlight.Add(-1)
			close(finished)
		})

		ctx, cancel := context.WithCancel(context.Background())
		runReturned := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(runReturned)
		}()

		// Let Run enter its steady-state select before cancelling, so the
		// cancellation is observed on the ctx.Done() arm rather than
		// racing the initial immediate tick.
		time.Sleep(10 * time.Millisecond)
		cancel()

		select {
		case <-runReturned:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after context cancellation")
		}
		elapsed := time.Since(start)

		select {
		case <-finished:
		default:
			t.Error("Run returned before the in-flight triage goroutine finished")
		}
		if elapsed < runDuration {
			t.Errorf("Run returned after %v, want at least %v (shutdown must wait for the in-flight triage run)", elapsed, runDuration)
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

// --- Budget Exhaustion Tick Tests ---

// budgetTickConfig returns a workflow manager configured for per-tick
// budget exhaustion tests.
func budgetTickConfig(maxSessions int) *stubWorkflowManager {
	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			APIKey:         "key",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling: config.PollingConfig{IntervalMS: 60000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			MaxConcurrentAgents: 10,
			MaxSessions:         maxSessions,
		},
	}
	return &stubWorkflowManager{config: cfg}
}

// budgetOrchestrator builds an orchestrator wired for budget-exhaustion tick tests.
func budgetOrchestrator(state *State, wm *stubWorkflowManager, store *stubStore, tracker *candidateTrackerAdapter) *Orchestrator {
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config
	return NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: regs,
	})
}

// budgetOrchestratorWithLogger mirrors budgetOrchestrator but accepts an
// explicit logger, for tests asserting on the token-budget-incomplete
// warning's log output.
func budgetOrchestratorWithLogger(state *State, wm *stubWorkflowManager, store *stubStore, tracker *candidateTrackerAdapter, logger *slog.Logger) *Orchestrator {
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config
	return NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          logger,
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: regs,
	})
}

// budgetOrchestratorWithMetrics mirrors budgetOrchestrator but accepts an
// explicit logger and metrics implementation, for tests asserting on the
// per-issue budget-ceiling log record and the counter together.
func budgetOrchestratorWithMetrics(state *State, wm *stubWorkflowManager, store *stubStore, tracker *candidateTrackerAdapter, logger *slog.Logger, metrics domain.Metrics) *Orchestrator {
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config
	return NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          logger,
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: regs,
		Metrics:         metrics,
	})
}

func TestHandleTick_BudgetExhaustionRebuildsState(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-1", Identifier: "TEST-1", Title: "title", State: "To Do"}

	t.Run("store returns exhausted IDs state populated", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 1}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issue.ID]; !ok {
			t.Errorf("BudgetExhausted[%s] missing after tick, want present", issue.ID)
		}
	})

	t.Run("exhausted issue not dispatched", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 1}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if _, running := state.Running[issue.ID]; running {
			t.Errorf("Running[%s] present, want absent (budget exhausted)", issue.ID)
		}
	})

	t.Run("store error retains previous BudgetExhausted", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{Reason: budgetReasonSession} // pre-populated
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		// Store error → retains the previous set without clearing.
		if _, ok := state.BudgetExhausted[issue.ID]; !ok {
			t.Errorf("BudgetExhausted[%s] cleared on store error, want retained", issue.ID)
		}
	})

	t.Run("max_sessions zero clears BudgetExhausted", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(0) // MaxSessions=0 → unlimited
		store := &stubStore{}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{} // pre-populated
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issue.ID]; ok {
			t.Errorf("BudgetExhausted[%s] remains with MaxSessions=0, want cleared", issue.ID)
		}
	})

	t.Run("empty candidate list with budget enabled clears stale set", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(3)
		store := &stubStore{}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{} // pre-populated
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return nil, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		// With a budget enabled, the rebuild scopes its batch queries to the
		// candidate set and assigns the fresh result. An empty candidate set
		// yields an empty result, so a stale entry is dropped. There are no
		// candidates to dispatch this tick, so the set is repopulated from
		// run_history on the next tick that has candidates.
		if _, ok := state.BudgetExhausted[issue.ID]; ok {
			t.Errorf("BudgetExhausted[%s] retained on empty candidate list, want cleared", issue.ID)
		}
	})
}

// budgetTickConfigTokens returns a workflow manager configured with both
// per-issue ceilings for per-tick rebuild tests.
func budgetTickConfigTokens(maxSessions, maxTokens int) *stubWorkflowManager {
	wm := budgetTickConfig(maxSessions)
	wm.config.Agent.MaxTokens = maxTokens
	return wm
}

// TestHandleTick_TokenBudgetRebuild covers the per-tick union rebuild of
// BudgetExhausted across the session and token ceilings, with per-issue
// reason attribution, token precedence, lockstep pruning, and the
// per-axis fail-open that folds the prior set in on a query error.
func TestHandleTick_TokenBudgetRebuild(t *testing.T) {
	t.Parallel()

	issueA := domain.Issue{ID: "iss-a", Identifier: "TEST-A", Title: "title", State: "To Do"}
	issueB := domain.Issue{ID: "iss-b", Identifier: "TEST-B", Title: "title", State: "To Do"}

	candidates := func(issues ...domain.Issue) *candidateTrackerAdapter {
		return &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return issues, nil },
		}
	}

	t.Run("token ceiling alone populates set with token reason", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issueA.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] missing after tick, want present (token ceiling)", issueA.ID)
		}
		if entry.Reason != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q", issueA.ID, entry.Reason, budgetReasonToken)
		}
		if _, running := state.Running[issueA.ID]; running {
			t.Errorf("Running[%s] present, want absent (token budget exhausted)", issueA.ID)
		}
	})

	t.Run("issue exhausted on both axes reports token budget", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{
			budgetExhaustedIDs: map[string]int{issueA.ID: 1},
			tokenExhaustedIDs:  []string{issueA.ID},
		}
		state := NewState(60000, 10, nil, AgentTotals{})

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] missing after tick, want present", issueA.ID)
		}
		if entry.Reason != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q (token precedence)", issueA.ID, entry.Reason, budgetReasonToken)
		}
	})

	t.Run("every issue in the rebuilt set carries a reason and stale entries are pruned", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{
			budgetExhaustedIDs: map[string]int{issueA.ID: 1},
			tokenExhaustedIDs:  []string{issueB.ID},
		}
		state := NewState(60000, 10, nil, AgentTotals{})
		// Stale entry from a previous tick: must be pruned from the set.
		state.BudgetExhausted["iss-stale"] = &BudgetExhaustedEntry{Reason: budgetReasonSession}

		budgetOrchestrator(state, wm, store, candidates(issueA, issueB)).handleTick(context.Background())

		if got := state.BudgetExhausted[issueA.ID].Reason; got != budgetReasonSession {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q", issueA.ID, got, budgetReasonSession)
		}
		if got := state.BudgetExhausted[issueB.ID].Reason; got != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q", issueB.ID, got, budgetReasonToken)
		}
		if _, ok := state.BudgetExhausted["iss-stale"]; ok {
			t.Error("BudgetExhausted[iss-stale] survived the rebuild, want pruned")
		}
		// Total coverage: every entry in the rebuilt set carries a reason.
		for id, entry := range state.BudgetExhausted {
			if entry.Reason == "" {
				t.Errorf("BudgetExhausted[%s].Reason empty, want a reason for every exhausted issue", id)
			}
		}
	})

	t.Run("token query failure folds prior set after session query would drop the issue", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		// Session query succeeds and returns nothing (would drop the issue);
		// token query fails: the prior set must be folded in for the token
		// axis so the issue stays blocked this tick.
		store := &stubStore{
			budgetExhaustedIDs: map[string]int{},
			tokenExhaustedErr:  fmt.Errorf("db error"),
		}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] dropped on token query error, want retained (prior set folded)", issueA.ID)
		}
		if entry.Reason != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q (prior reason carried)", issueA.ID, entry.Reason, budgetReasonToken)
		}
		if _, running := state.Running[issueA.ID]; running {
			t.Errorf("Running[%s] present, want absent (issue stays blocked this tick)", issueA.ID)
		}
	})

	t.Run("session query failure folds prior set with carried reasons", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 0)
		store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonSession}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] dropped on session query error, want retained", issueA.ID)
		}
		if entry.Reason != budgetReasonSession {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q (prior reason carried)", issueA.ID, entry.Reason, budgetReasonSession)
		}
	})

	t.Run("session query failure carries only session-attributed entries", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		// Session query fails while the token query succeeds and reports
		// the issue back under budget. The session-axis fold must not
		// resurrect an entry attributed to the token budget.
		store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		if entry, ok := state.BudgetExhausted[issueA.ID]; ok {
			t.Errorf("BudgetExhausted[%s] = %+v, want dropped (fresh token result cleared it)", issueA.ID, entry)
		}
	})

	t.Run("token query failure carries only token-attributed entries when session ceiling disabled", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		// With the session ceiling disabled, a prior session-attributed
		// entry has no axis to survive on: the token-axis fold must not
		// carry it forward.
		store := &stubStore{tokenExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonSession}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		if entry, ok := state.BudgetExhausted[issueA.ID]; ok {
			t.Errorf("BudgetExhausted[%s] = %+v, want dropped (session ceiling disabled)", issueA.ID, entry)
		}
	})

	t.Run("token query failure reports token reason when session query also finds the issue", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		// Session query succeeds and blocks the issue; the token query
		// fails and folds the prior token-attributed entry back in. The
		// carried entry reports the token budget, matching the precedence
		// the success path applies when both axes block an issue.
		store := &stubStore{
			budgetExhaustedIDs: map[string]int{issueA.ID: 1},
			tokenExhaustedErr:  fmt.Errorf("db error"),
		}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] missing after tick, want present (blocked on both axes)", issueA.ID)
		}
		if entry.Reason != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q (token precedence on carried entry)", issueA.ID, entry.Reason, budgetReasonToken)
		}
	})

	t.Run("both ceilings zero clears set and reason map", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 0)
		store := &stubStore{}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken}

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		if len(state.BudgetExhausted) != 0 {
			t.Errorf("BudgetExhausted = %v, want empty with both ceilings disabled", state.BudgetExhausted)
		}
	})

	t.Run("empty candidate list clears set and reason map in lockstep", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issueA.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken}

		budgetOrchestrator(state, wm, store, candidates()).handleTick(context.Background())

		// Both batch queries return empty for an empty candidate list, so
		// the rebuild assigns an empty fresh set; the issue is re-blocked on
		// the next tick that has candidates, from the run_history ledger.
		if len(state.BudgetExhausted) != 0 {
			t.Errorf("BudgetExhausted = %v, want cleared on empty candidate list", state.BudgetExhausted)
		}
	})

	t.Run("a candidate below the ceiling with an unmeasured session permits dispatch and warns once, then stays silent on the next tick", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenIncompleteIDs: []string{issueA.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		orch := budgetOrchestratorWithLogger(state, wm, store, candidates(issueA), logger)
		orch.handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issueA.ID]; ok {
			t.Errorf("BudgetExhausted[%s] present, want absent (below the ceiling permits dispatch)", issueA.ID)
		}
		if _, ok := state.TokenBudgetIncomplete[issueA.ID]; !ok {
			t.Errorf("TokenBudgetIncomplete[%s] missing, want present", issueA.ID)
		}
		output := buf.String()
		if got := strings.Count(output, "token budget cannot be fully evaluated, allowing dispatch"); got != 1 {
			t.Fatalf("warning count after first tick = %d, want 1; log:\n%s", got, output)
		}
		for _, want := range []string{
			`issue_id=` + issueA.ID,
			`issue_identifier=` + issueA.Identifier,
			"used_tokens=0",
			"budget_tokens=1000",
			"unmeasured_sessions=1",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("warning log missing %q; log:\n%s", want, output)
			}
		}

		// A second consecutive tick with the same condition must not log
		// a further warning: edge-triggered against the prior set.
		orch.handleTick(context.Background())
		if got := strings.Count(buf.String(), "token budget cannot be fully evaluated, allowing dispatch"); got != 1 {
			t.Errorf("warning count after second tick = %d, want 1 (edge-triggered)", got)
		}
	})

	t.Run("a candidate reaching the ceiling is blocked and emits no unmeasured-budget warning", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issueA.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		budgetOrchestratorWithLogger(state, wm, store, candidates(issueA), logger).handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		entry, ok := state.BudgetExhausted[issueA.ID]
		if !ok {
			t.Fatalf("BudgetExhausted[%s] missing, want present (at the ceiling)", issueA.ID)
		}
		if entry.Reason != budgetReasonToken {
			t.Errorf("BudgetExhausted[%s].Reason = %q, want %q", issueA.ID, entry.Reason, budgetReasonToken)
		}
		if _, ok := state.TokenBudgetIncomplete[issueA.ID]; ok {
			t.Errorf("TokenBudgetIncomplete[%s] present, want absent for a blocked issue", issueA.ID)
		}
		if strings.Contains(buf.String(), "token budget cannot be fully evaluated") {
			t.Errorf("unexpected incomplete-budget warning for a blocked issue; log:\n%s", buf.String())
		}
	})

	t.Run("only agent.max_sessions configured issues no token query and leaves TokenBudgetIncomplete empty", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(3, 0)
		store := &stubStore{tokenIncompleteIDs: []string{issueA.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		if len(state.TokenBudgetIncomplete) != 0 {
			t.Errorf("TokenBudgetIncomplete = %v, want empty when agent.max_tokens is unset", state.TokenBudgetIncomplete)
		}
	})

	t.Run("a live config reload removing the token ceiling clears TokenBudgetIncomplete", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenIncompleteIDs: []string{issueA.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})

		orch := budgetOrchestrator(state, wm, store, candidates(issueA))
		orch.handleTick(context.Background())
		if len(state.TokenBudgetIncomplete) == 0 {
			t.Fatal("TokenBudgetIncomplete empty after the first tick, want the candidate present")
		}

		wm.config.Agent.MaxTokens = 0
		orch.handleTick(context.Background())

		if len(state.TokenBudgetIncomplete) != 0 {
			t.Errorf("TokenBudgetIncomplete = %v, want cleared after the token ceiling was removed", state.TokenBudgetIncomplete)
		}
	})
}

// TestHandleTick_BudgetLogRecord fails if the polling lane stops
// announcing a session-ceiling hold on the tick it enters the set.
func TestHandleTick_BudgetLogRecord(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-log", Identifier: "PROJ-LOG", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	output := buf.String()
	if !strings.Contains(output, "candidate held by budget ceiling") {
		t.Fatalf("log output = %q, want to contain the budget-ceiling record", output)
	}
	for _, attr := range []string{
		"reason=session_budget",
		"used_sessions=5",
		"budget_sessions=3",
		"issue_id=" + issue.ID,
		"issue_identifier=" + issue.Identifier,
	} {
		if !strings.Contains(output, attr) {
			t.Errorf("log output missing attribute %q; log:\n%s", attr, output)
		}
	}
}

// TestHandleTick_BudgetLogRecordOnce fails if the per-issue record or the
// counter starts repeating on a tick that re-observes the same hold.
func TestHandleTick_BudgetLogRecordOnce(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-once", Identifier: "PROJ-ONCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	spy := &spyMetrics{}

	orch := budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy)
	orch.handleTick(context.Background())
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := strings.Count(buf.String(), "candidate held by budget ceiling"); got != 1 {
		t.Errorf("record count across two ticks = %d, want 1 (edge-triggered)", got)
	}
	if len(spy.budgetExhaustions) != 1 || spy.budgetExhaustions[0] != budgetReasonSession {
		t.Errorf("IncBudgetExhaustions calls = %v, want [%q]", spy.budgetExhaustions, budgetReasonSession)
	}
}

// TestHandleTick_BudgetLogRecordCeilingSetting covers the ceiling_setting
// attribute the budget-hold record gains, across the two known reasons
// and the closed reason vocabulary's absent third arm.
func TestHandleTick_BudgetLogRecordCeilingSetting(t *testing.T) {
	t.Parallel()

	t.Run("session hold names agent.max_sessions", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-ceil-sess", Identifier: "PROJ-CEIL-SESS", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		if !strings.Contains(buf.String(), "ceiling_setting=agent.max_sessions") {
			t.Errorf("log output missing ceiling_setting=agent.max_sessions; log:\n%s", buf.String())
		}
	})

	t.Run("token hold names agent.max_tokens", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-ceil-tok", Identifier: "PROJ-CEIL-TOK", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issue.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		if !strings.Contains(buf.String(), "ceiling_setting=agent.max_tokens") {
			t.Errorf("log output missing ceiling_setting=agent.max_tokens; log:\n%s", buf.String())
		}
	})

	t.Run("a reason absent from the lookup has no known governing setting", func(t *testing.T) {
		t.Parallel()

		if setting, known := ceilingSettingByBudgetReason["unknown_reason"]; known {
			t.Errorf("ceilingSettingByBudgetReason[%q] = %q, want not known: the reason vocabulary is closed to the two mapped constants", "unknown_reason", setting)
		}
	})
}

// TestHandleTick_BudgetLogRecordTokenAxis covers the token ceiling on the
// polling lane and the both-axes-in-one-pass precedence case.
func TestHandleTick_BudgetLogRecordTokenAxis(t *testing.T) {
	t.Parallel()

	t.Run("token ceiling alone logs reason=token_budget", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-tok-log", Identifier: "PROJ-TOK-LOG", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issue.ID}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		output := buf.String()
		if !strings.Contains(output, "candidate held by budget ceiling") {
			t.Fatalf("log output = %q, want to contain the budget-ceiling record", output)
		}
		for _, attr := range []string{"reason=token_budget", "used_tokens=1000", "budget_tokens=1000"} {
			if !strings.Contains(output, attr) {
				t.Errorf("log output missing attribute %q; log:\n%s", attr, output)
			}
		}
	})

	t.Run("exhausted on both axes in one evaluation logs reason=token_budget", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-both-log", Identifier: "PROJ-BOTH-LOG", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{
			budgetExhaustedIDs: map[string]int{issue.ID: 5},
			tokenExhaustedIDs:  []string{issue.ID},
		}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		output := buf.String()
		if !strings.Contains(output, "reason=token_budget") {
			t.Errorf("log output missing %q (token precedence); log:\n%s", "reason=token_budget", output)
		}
		if strings.Contains(output, "reason=session_budget") {
			t.Errorf("log output contains %q, want only the token reason announced; log:\n%s", "reason=session_budget", output)
		}
		if got := strings.Count(output, "candidate held by budget ceiling"); got != 1 {
			t.Errorf("record count = %d, want 1 (one entry, one announcement)", got)
		}
	})
}

// TestHandleTick_BudgetLogRecordQueryError fails if a transient query
// error on either axis re-announces a hold that was already told, which
// would reproduce the repeating-log defect this unit fixes.
func TestHandleTick_BudgetLogRecordQueryError(t *testing.T) {
	t.Parallel()

	t.Run("session query error folds forward without announcing", func(t *testing.T) {
		t.Parallel()

		priorAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		issue := domain.Issue{ID: "iss-sess-err", Identifier: "PROJ-SESS-ERR", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{Reason: budgetReasonSession, ExhaustedAt: priorAt}
		state.BudgetAnnounced[issue.ID] = BudgetAnnouncement{Reason: budgetReasonSession, At: priorAt}
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		spy := &spyMetrics{}

		budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy).handleTick(context.Background())

		output := buf.String()
		if !strings.Contains(output, "budget exhaustion query failed, retaining previous set") {
			t.Errorf("log output missing the existing fail-open warning; log:\n%s", output)
		}
		if strings.Contains(output, "candidate held by budget ceiling") {
			t.Errorf("log output contains a new announcement for a folded-forward entry; log:\n%s", output)
		}
		if len(spy.budgetExhaustions) != 0 {
			t.Errorf("IncBudgetExhaustions calls = %v, want none on a query error", spy.budgetExhaustions)
		}
	})

	t.Run("token query error folds forward without announcing", func(t *testing.T) {
		t.Parallel()

		priorAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		issue := domain.Issue{ID: "iss-tok-err", Identifier: "PROJ-TOK-ERR", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, nil, AgentTotals{})
		usedTokens := int64(1500)
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{Reason: budgetReasonToken, UsedTokens: &usedTokens, ExhaustedAt: priorAt}
		state.BudgetAnnounced[issue.ID] = BudgetAnnouncement{Reason: budgetReasonToken, At: priorAt}
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		spy := &spyMetrics{}

		budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy).handleTick(context.Background())

		output := buf.String()
		if !strings.Contains(output, "token budget exhaustion query failed, retaining previous set") {
			t.Errorf("log output missing the existing fail-open warning; log:\n%s", output)
		}
		if strings.Contains(output, "candidate held by budget ceiling") {
			t.Errorf("log output contains a new announcement for a folded-forward entry; log:\n%s", output)
		}
		if len(spy.budgetExhaustions) != 0 {
			t.Errorf("IncBudgetExhaustions calls = %v, want none on a query error", spy.budgetExhaustions)
		}
	})
}

// TestHandleTick_BudgetAnnouncementLifecycle covers the announcement
// decision table's remaining reachable rows: a reason change re-announces,
// a candidacy gap does not re-announce a still-held reason, and a genuine
// clearance under the ceiling lets a later hold announce again.
func TestHandleTick_BudgetAnnouncementLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("reason change from session to token re-announces", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-reason-change", Identifier: "PROJ-REASON", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		spy := &spyMetrics{}
		orch := budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy)

		orch.handleTick(context.Background())

		store.budgetExhaustedIDs = map[string]int{}
		store.tokenExhaustedIDs = []string{issue.ID}
		orch.handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		if got := strings.Count(buf.String(), "candidate held by budget ceiling"); got != 2 {
			t.Errorf("record count across the reason change = %d, want 2 (re-announced)", got)
		}
		if len(spy.budgetExhaustions) != 2 {
			t.Fatalf("IncBudgetExhaustions calls = %v, want 2", spy.budgetExhaustions)
		}
		if spy.budgetExhaustions[0] != budgetReasonSession || spy.budgetExhaustions[1] != budgetReasonToken {
			t.Errorf("IncBudgetExhaustions calls = %v, want [%q %q]", spy.budgetExhaustions, budgetReasonSession, budgetReasonToken)
		}
	})

	t.Run("held, absent, held again under the same reason produces exactly one record and unchanged ExhaustedAt", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-flap", Identifier: "PROJ-FLAP", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, nil, AgentTotals{})
		present := true
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if present {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
			},
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		spy := &spyMetrics{}
		orch := budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy)

		orch.handleTick(context.Background())
		entry1, ok := state.BudgetExhausted[issue.ID]
		if !ok {
			t.Fatal("BudgetExhausted missing after the first tick, want present")
		}
		firstExhaustedAt := entry1.ExhaustedAt

		present = false
		store.budgetExhaustedIDs = map[string]int{} // stub mirrors production: query results are bounded by candidateIDs
		orch.handleTick(context.Background())
		if _, ok := state.BudgetExhausted[issue.ID]; ok {
			t.Fatal("BudgetExhausted present while the issue is not a candidate, want absent")
		}

		present = true
		store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
		orch.handleTick(context.Background())

		entry3, ok := state.BudgetExhausted[issue.ID]
		if !ok {
			t.Fatal("BudgetExhausted missing after the third tick, want present")
		}
		if !entry3.ExhaustedAt.Equal(firstExhaustedAt) {
			t.Errorf("ExhaustedAt = %v, want %v (unchanged across the candidacy gap)", entry3.ExhaustedAt, firstExhaustedAt)
		}
		state.TrackerOpsWg.Wait()
		if got := strings.Count(buf.String(), "candidate held by budget ceiling"); got != 1 {
			t.Errorf("record count across three ticks = %d, want 1", got)
		}
		if len(spy.budgetExhaustions) != 1 {
			t.Errorf("IncBudgetExhaustions calls = %v, want 1", spy.budgetExhaustions)
		}
	})

	t.Run("clearing under the ceiling prunes the memory so a later hold announces again", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-clear", Identifier: "PROJ-CLEAR", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		spy := &spyMetrics{}
		orch := budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy)

		orch.handleTick(context.Background())

		store.budgetExhaustedIDs = map[string]int{}
		orch.handleTick(context.Background())
		if _, ok := state.BudgetExhausted[issue.ID]; ok {
			t.Fatal("BudgetExhausted present after clearing under the ceiling, want absent")
		}

		store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
		orch.handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issue.ID]; !ok {
			t.Fatal("BudgetExhausted missing after re-exhaustion, want present")
		}
		state.TrackerOpsWg.Wait()
		if got := strings.Count(buf.String(), "candidate held by budget ceiling"); got != 2 {
			t.Errorf("record count across the clearance = %d, want 2 (announced again after a genuine clearance)", got)
		}
		if len(spy.budgetExhaustions) != 2 {
			t.Errorf("IncBudgetExhaustions calls = %v, want 2", spy.budgetExhaustions)
		}
	})
}

// TestHandleTick_BudgetCrossLaneAnnouncement fails if the polling lane
// re-announces a hold the retry lane already discovered and reported,
// or if it disagrees with the retry lane about when the hold began.
func TestHandleTick_BudgetCrossLaneAnnouncement(t *testing.T) {
	t.Parallel()

	issueID := "iss-cross"
	identifier := "PROJ-CROSS"
	spy := &spyMetrics{}

	state := retryState(t, issueID, identifier, 1)
	retryStore := &mockRetryStore{runHistoryCount: 5}
	retryTracker := &mockRetryTracker{}
	retryParams := defaultRetryParams(t, retryStore, retryTracker)
	retryParams.MaxSessions = 3
	retryParams.Metrics = spy

	HandleRetryTimer(state, issueID, retryParams)

	blockedEntry, ok := state.BudgetExhausted[issueID]
	if !ok {
		t.Fatal("BudgetExhausted missing after the retry-lane block, want present")
	}
	if len(spy.budgetExhaustions) != 1 || spy.budgetExhaustions[0] != budgetReasonSession {
		t.Fatalf("IncBudgetExhaustions calls after the retry-lane block = %v, want [%q]", spy.budgetExhaustions, budgetReasonSession)
	}
	retryExhaustedAt := blockedEntry.ExhaustedAt

	issue := domain.Issue{ID: issueID, Identifier: identifier, Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	pollStore := &stubStore{budgetExhaustedIDs: map[string]int{issueID: 5}}
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	budgetOrchestratorWithMetrics(state, wm, pollStore, tracker, logger, spy).handleTick(context.Background())

	if strings.Contains(buf.String(), "candidate held by budget ceiling") {
		t.Errorf("poll tick emitted a new announcement for a hold the retry lane already announced; log:\n%s", buf.String())
	}
	if len(spy.budgetExhaustions) != 1 {
		t.Errorf("IncBudgetExhaustions calls after the poll tick = %v, want still [%q] (one increment in total)", spy.budgetExhaustions, budgetReasonSession)
	}
	rebuiltEntry, ok := state.BudgetExhausted[issueID]
	if !ok {
		t.Fatal("BudgetExhausted missing after the poll tick, want present")
	}
	if !rebuiltEntry.ExhaustedAt.Equal(retryExhaustedAt) {
		t.Errorf("ExhaustedAt = %v, want %v (the retry lane's own timestamp, not the tick's)", rebuiltEntry.ExhaustedAt, retryExhaustedAt)
	}
}

// TestHandleTick_BudgetTickSummary fails if the "tick completed" record
// stops carrying the held-candidate count the poll summary depends on.
func TestHandleTick_BudgetTickSummary(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-summary", Identifier: "PROJ-SUMMARY", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	budgetOrchestratorWithLogger(state, wm, store, tracker, logger).handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	var tickLine string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "tick completed") {
			tickLine = line
			break
		}
	}
	if tickLine == "" {
		t.Fatalf("no %q record found; log:\n%s", "tick completed", buf.String())
	}
	for _, attr := range []string{"candidates=1", "dispatched=0", "budget_exhausted=1"} {
		if !strings.Contains(tickLine, attr) {
			t.Errorf("tick completed line missing %q: %q", attr, tickLine)
		}
	}
}

// --- Incremental session_metadata write tests ---

// tokenUsageEvent returns a token_usage agent event with cumulative counters.
func tokenUsageEvent(input, output, total, cacheRead int64) domain.AgentEvent {
	return domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Usage: domain.TokenUsage{
			InputTokens:     input,
			OutputTokens:    output,
			TotalTokens:     total,
			CacheReadTokens: cacheRead,
		},
	}
}

// incrementalWriteOrchestrator builds an orchestrator with a running entry
// whose accumulated token counters are pre-set, for driving
// maybeWriteIncrementalMetadata directly.
func incrementalWriteOrchestrator(t *testing.T, store *stubStore) (*Orchestrator, *RunningEntry) {
	t.Helper()
	state := NewState(60000, 10, nil, AgentTotals{})
	entry := &RunningEntry{
		Identifier:        "MT-1",
		Issue:             domain.Issue{ID: "id-1", Identifier: "MT-1", State: "In Progress"},
		StartedAt:         time.Now().UTC(),
		SessionID:         "sess-1",
		AgentInputTokens:  10,
		AgentOutputTokens: 20,
		AgentTotalTokens:  30,
		CacheReadTokens:   5,
		ModelName:         "test-model",
		APIRequestCount:   2,
	}
	state.Running["id-1"] = entry
	tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
	return budgetOrchestrator(state, budgetTickConfig(0), store, tracker), entry
}

func TestMaybeWriteIncrementalMetadata(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("first token usage event writes immediately", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		writes := store.sessionWrites()
		if len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1", len(writes))
		}
		meta := writes[0]
		if meta.IssueID != "id-1" {
			t.Errorf("SessionMetadata.IssueID = %q, want %q", meta.IssueID, "id-1")
		}
		if meta.SessionID != "sess-1" {
			t.Errorf("SessionMetadata.SessionID = %q, want %q", meta.SessionID, "sess-1")
		}
		if meta.InputTokens != 10 || meta.OutputTokens != 20 || meta.TotalTokens != 30 || meta.CacheReadTokens != 5 {
			t.Errorf("SessionMetadata tokens = (%d, %d, %d, %d), want (10, 20, 30, 5)",
				meta.InputTokens, meta.OutputTokens, meta.TotalTokens, meta.CacheReadTokens)
		}
		if meta.ModelName != "test-model" {
			t.Errorf("SessionMetadata.ModelName = %q, want %q", meta.ModelName, "test-model")
		}
		if entry.LastMetadataWrite.IsZero() {
			t.Error("RunningEntry.LastMetadataWrite still zero after successful write")
		}
	})

	t.Run("second event within interval is throttled", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, _ := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))
		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(11, 21, 33, 5))

		if writes := store.sessionWrites(); len(writes) != 1 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 1 (second event throttled)", len(writes))
		}
	})

	t.Run("event after interval elapses writes again", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))
		// Backdate the last write past the throttle interval.
		entry.LastMetadataWrite = time.Now().UTC().Add(-sessionMetadataWriteInterval - time.Second)
		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(11, 21, 33, 6))

		if writes := store.sessionWrites(); len(writes) != 2 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 2 (interval elapsed)", len(writes))
		}
	})

	t.Run("non token_usage event is ignored", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, _ := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", domain.AgentEvent{
			Type:      domain.EventTurnCompleted,
			Timestamp: time.Now().UTC(),
		})

		if writes := store.sessionWrites(); len(writes) != 0 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 0 (non-token event)", len(writes))
		}
	})

	t.Run("turn_completed event with non-zero usage triggers a write", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, _ := incrementalWriteOrchestrator(t, store)

		event := tokenUsageEvent(10, 20, 30, 5)
		event.Type = domain.EventTurnCompleted
		o.maybeWriteIncrementalMetadata(ctx, "id-1", event)

		writes := store.sessionWrites()
		if len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1 (usage-bearing turn_completed event)", len(writes))
		}
		if writes[0].TotalTokens != 30 {
			t.Errorf("SessionMetadata.TotalTokens = %d, want 30", writes[0].TotalTokens)
		}
	})

	// A session whose only usage event carries an all-zero payload (a
	// measurement of zero) still writes a row, so row presence is exact
	// rather than conservative for such a session.
	t.Run("all-zero token_usage event still writes a row", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, _ := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
		})

		// The row's contents come from the running entry's own totals
		// (pre-set by incrementalWriteOrchestrator), not from the
		// triggering event's own zero payload; only row presence is
		// under test here.
		if writes := store.sessionWrites(); len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1 (a measurement of zero still writes a row)", len(writes))
		}
	})

	t.Run("unknown issue is ignored", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, _ := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-unknown", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 0 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 0 (unknown issue)", len(writes))
		}
	})

	t.Run("store error does not advance the throttle timestamp", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{upsertSessionMetadataErr: fmt.Errorf("disk full")}
		o, entry := incrementalWriteOrchestrator(t, store)

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if !entry.LastMetadataWrite.IsZero() {
			t.Error("RunningEntry.LastMetadataWrite advanced after failed write, want zero")
		}
		// The next event retries immediately because the timestamp did not move.
		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(11, 21, 33, 6))
		if writes := store.sessionWrites(); len(writes) != 2 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 2 (failed write not throttled)", len(writes))
		}
	})
}

// TestDrainRunningWorkers_TokenUsageEventTriggersIncrementalWrite delivers a
// token_usage event on the drain-loop agentEventCh site and asserts an
// incremental session_metadata write occurs before the worker exits, so the
// advisory staleness bound holds during graceful drain.
func TestDrainRunningWorkers_TokenUsageEventTriggersIncrementalWrite(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, nil, AgentTotals{})
	state.Running["id-1"] = &RunningEntry{
		Identifier: "MT-1",
		Issue:      domain.Issue{ID: "id-1", Identifier: "MT-1", State: "In Progress"},
		StartedAt:  time.Now().UTC(),
		SessionID:  "sess-1",
		CancelFunc: func() {},
	}
	store := &stubStore{}
	tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
	o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

	// Run never starts: the drain loop is the only consumer of
	// agentEventCh, so the event below is processed at the drain site.
	done := make(chan struct{})
	go func() {
		o.drainRunningWorkers()
		close(done)
	}()

	o.agentEventCh <- agentEventMsg{IssueID: "id-1", Event: tokenUsageEvent(10, 20, 30, 5)}

	// The incremental write must land before the worker exit is delivered;
	// the exit-path metadata write cannot have happened yet.
	deadline := time.After(10 * time.Second)
	for len(store.sessionWrites()) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for incremental session_metadata write during drain")
		case <-time.After(10 * time.Millisecond):
		}
	}

	meta := store.sessionWrites()[0]
	if meta.IssueID != "id-1" {
		t.Errorf("SessionMetadata.IssueID = %q, want %q", meta.IssueID, "id-1")
	}
	if meta.SessionID != "sess-1" {
		t.Errorf("SessionMetadata.SessionID = %q, want %q", meta.SessionID, "sess-1")
	}
	if meta.TotalTokens != 30 {
		t.Errorf("SessionMetadata.TotalTokens = %d, want 30", meta.TotalTokens)
	}

	// Let the drain complete.
	o.workerExitCh <- WorkerResult{IssueID: "id-1", Identifier: "MT-1"}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainRunningWorkers did not return within 10 seconds")
	}
}

// TestDrainRunningWorkers_AbsenceCeiling verifies that the shutdown-drain
// lane's own HandleWorkerExitParams construction site reads the
// configured consecutive-absence ceiling rather than silently resolving
// the built-in fallback of three, on both a park decision at a value
// below the fallback and a recorded ceiling at a value above it.
func TestDrainRunningWorkers_AbsenceCeiling(t *testing.T) {
	t.Parallel()

	t.Run("parks at a ceiling below the built-in default", func(t *testing.T) {
		t.Parallel()

		const issueID = "DRAIN-ABS-BELOW"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		state := NewState(60000, 1, nil, AgentTotals{})
		state.Running[issueID] = &RunningEntry{
			Identifier: "PROJ-DRAIN-BELOW",
			Issue:      domain.Issue{ID: issueID, Identifier: "PROJ-DRAIN-BELOW", State: "In Progress"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: func() {},
		}
		store := &stubStore{absenceCounts: map[string]int{issueID: 2}}
		wm := budgetTickConfig(0)
		wm.config.Agent.MaxConsecutiveAbsences = 2
		wm.config.Tracker.ActiveStates = []string{"In Progress"}
		wm.config.Tracker.TerminalStates = []string{"Done"}
		wm.config.Tracker.HandoffState = "Human Review"
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		o := budgetOrchestrator(state, wm, store, tracker)
		o.drainTimeout = 5 * time.Second

		done := make(chan struct{})
		go func() {
			o.drainRunningWorkers()
			close(done)
		}()

		o.workerExitCh <- WorkerResult{
			IssueID:                 issueID,
			Identifier:              "PROJ-DRAIN-BELOW",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("drainRunningWorkers did not return within 10 seconds")
		}
		state.TrackerOpsWg.Wait()

		if _, ok := state.Parked[issueID]; !ok {
			t.Error("issue not parked at the two-absence ceiling on the shutdown-drain lane")
		}
	})

	t.Run("records the configured ceiling above the built-in default", func(t *testing.T) {
		t.Parallel()

		const issueID = "DRAIN-ABS-ABOVE"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		state := NewState(60000, 1, nil, AgentTotals{})
		state.Running[issueID] = &RunningEntry{
			Identifier: "PROJ-DRAIN-ABOVE",
			Issue:      domain.Issue{ID: issueID, Identifier: "PROJ-DRAIN-ABOVE", State: "In Progress"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: func() {},
		}
		store := &stubStore{absenceCounts: map[string]int{issueID: 5}}
		wm := budgetTickConfig(0)
		wm.config.Agent.MaxConsecutiveAbsences = 5
		wm.config.Tracker.ActiveStates = []string{"In Progress"}
		wm.config.Tracker.TerminalStates = []string{"Done"}
		wm.config.Tracker.HandoffState = "Human Review"
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		o := budgetOrchestratorWithLogger(state, wm, store, tracker, logger)
		o.drainTimeout = 5 * time.Second

		done := make(chan struct{})
		go func() {
			o.drainRunningWorkers()
			close(done)
		}()

		o.workerExitCh <- WorkerResult{
			IssueID:                 issueID,
			Identifier:              "PROJ-DRAIN-ABOVE",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("drainRunningWorkers did not return within 10 seconds")
		}
		state.TrackerOpsWg.Wait()

		if _, ok := state.Parked[issueID]; !ok {
			t.Fatal("issue not parked at the five-absence ceiling on the shutdown-drain lane")
		}
		if !strings.Contains(buf.String(), "absence_ceiling=5") {
			t.Errorf("issue-parked log missing absence_ceiling=5, want the configured ceiling rather than the built-in default 3\nlogs: %s", buf.String())
		}
	})
}

// TestBudgetExhaustionPreventsRedispatch verifies that an issue whose budget is
// exhausted in the persistence store is not dispatched on a fresh-state tick,
// simulating an orchestrator restart where in-memory state was reset.
func TestBudgetExhaustionPreventsRedispatch(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-redisp", Identifier: "PROJ-1", Title: "Work", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 1}}
	state := NewState(60000, 10, nil, AgentTotals{}) // fresh; BudgetExhausted is empty
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

	if _, running := state.Running[issue.ID]; running {
		t.Error("budget-exhausted issue dispatched on fresh-state tick, want blocked")
	}
	if _, exhausted := state.BudgetExhausted[issue.ID]; !exhausted {
		t.Error("BudgetExhausted missing after tick, want rebuilt from store")
	}
}

// TestBudgetExhaustionClearsWhenMaxSessionsZero verifies that setting
// max_sessions=0 on a tick clears the BudgetExhausted set, unblocking issues
// that were previously blocked.
func TestBudgetExhaustionClearsWhenMaxSessionsZero(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-clear", Identifier: "PROJ-2", Title: "Retry", State: "To Do"}
	wm := budgetTickConfig(0) // max_sessions=0 → all issues eligible
	store := &stubStore{}
	state := NewState(60000, 10, nil, AgentTotals{})
	state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{} // was previously blocked
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

	if _, ok := state.BudgetExhausted[issue.ID]; ok {
		t.Errorf("BudgetExhausted[%s] still set after MaxSessions=0 tick, want cleared", issue.ID)
	}
}

// --- TestOrchestratorScenarios ---

// TestOrchestratorScenarios covers dispatch-to-exit edge cases through the
// real event loop: soft-stop signals, handoff transitions, handoff failures,
// reconciliation cancellation, and re-dispatch prevention after handoff.
func TestOrchestratorScenarios(t *testing.T) {
	t.Parallel()

	// handoffConfig returns a lifecycle config with HandoffState set to "In Review".
	handoffConfig := func(tmpDir string) config.ServiceConfig {
		cfg := lifecycleConfig(tmpDir)
		cfg.Tracker.HandoffState = "In Review"
		return cfg
	}

	// scenarioIssue returns a single dispatch-eligible issue.
	scenarioIssue := func(id, identifier string) domain.Issue {
		return domain.Issue{ID: id, Identifier: identifier, Title: "Scenario Issue", State: "To Do"}
	}

	// pollStore polls the store until cond returns true or the deadline fires.
	pollStore := func(t *testing.T, store *stubStore, cond func(*stubStore) bool) {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatal("timed out polling store for expected condition")
			default:
			}
			store.mu.Lock()
			ok := cond(store)
			store.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// writeStatusFile creates .sortie/status in the given workspace directory.
	// Both MkdirAll and WriteFile errors are intentionally ignored: the worker
	// reads the file after RunTurn returns, so any write failure causes a
	// StatusNone read (no soft-stop) rather than a test failure.
	writeStatusFile := func(workspacePath, signal string) {
		sortieDir := filepath.Join(workspacePath, ".sortie")
		_ = os.MkdirAll(sortieDir, 0o755)
		_ = os.WriteFile(filepath.Join(sortieDir, "status"), []byte(signal+"\n"), 0o644)
	}

	t.Run("soft_stop_needs_human_review_triggers_handoff", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := handoffConfig(tmpDir)
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("hs-1", "HS-1")

		// workspacePath is written by startSessionFn and read by runTurnFn.
		// Both execute sequentially in the same worker goroutine — no race.
		var workspacePath string

		mockTracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "To Do"
				}
				return result, nil
			},
		}

		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				workspacePath = params.WorkspacePath
				return domain.Session{ID: "sess-hs-1"}, nil
			},
			runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				writeStatusFile(workspacePath, "needs-human-review")
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		}

		// Return the issue once; subsequent calls return nil to prevent re-dispatch
		// after the claim is released.
		var dispatched atomic.Bool
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if dispatched.CompareAndSwap(false, true) {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
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

		pollStore(t, store, func(s *stubStore) bool { return len(s.runHistories) >= 1 })

		cancel()
		<-done

		if got := len(mockTracker.transitionCalls); got != 1 {
			t.Errorf("transitionCalls = %d, want 1", got)
		} else if mockTracker.transitionCalls[0].TargetState != "In Review" {
			t.Errorf("transitionCalls[0].TargetState = %q, want %q",
				mockTracker.transitionCalls[0].TargetState, "In Review")
		}

		if _, ok := state.Running[issue.ID]; ok {
			t.Error("issue still in Running after handoff, want absent")
		}
		if _, ok := state.Claimed[issue.ID]; ok {
			t.Error("issue still in Claimed after handoff, want absent")
		}

		store.mu.Lock()
		retries := len(store.savedRetries)
		histStatus := store.runHistories[0].Status
		store.mu.Unlock()

		if retries != 0 {
			t.Errorf("savedRetries = %d, want 0", retries)
		}
		if histStatus != "succeeded" {
			t.Errorf("run history status = %q, want %q", histStatus, "succeeded")
		}
	})

	t.Run("soft_stop_blocked_no_handoff", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := handoffConfig(tmpDir) // HandoffState configured but must NOT be called for "blocked"
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("bl-1", "BL-1")

		var workspacePath string

		mockTracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "To Do"
				}
				return result, nil
			},
		}

		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				workspacePath = params.WorkspacePath
				return domain.Session{ID: "sess-bl-1"}, nil
			},
			runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				writeStatusFile(workspacePath, "blocked")
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		}

		var dispatched atomic.Bool
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if dispatched.CompareAndSwap(false, true) {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
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

		pollStore(t, store, func(s *stubStore) bool { return len(s.runHistories) >= 1 })

		cancel()
		<-done

		// "blocked" takes the first switch case: suppresses retry, skips handoff.
		if got := len(mockTracker.transitionCalls); got != 0 {
			t.Errorf("transitionCalls = %d, want 0 (blocked skips handoff transition)", got)
		}

		if _, ok := state.Running[issue.ID]; ok {
			t.Error("issue still in Running after blocked soft-stop, want absent")
		}
		if _, ok := state.Claimed[issue.ID]; ok {
			t.Error("issue still in Claimed after blocked soft-stop, want absent")
		}

		store.mu.Lock()
		retries := len(store.savedRetries)
		store.mu.Unlock()
		if retries != 0 {
			t.Errorf("savedRetries = %d, want 0", retries)
		}
	})

	t.Run("handoff_transition_failure_no_soft_stop_retries", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := handoffConfig(tmpDir)
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("hf-1", "HF-1")

		mockTracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				return fmt.Errorf("tracker unavailable")
			},
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "To Do"
				}
				return result, nil
			},
		}

		// Normal exit: no soft-stop signal written to .sortie/status.
		agent := &mockAgentAdapter{
			runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		}

		var dispatched atomic.Bool
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if dispatched.CompareAndSwap(false, true) {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
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

		// Poll for the retry entry: handoff failure schedules a continuation retry.
		pollStore(t, store, func(s *stubStore) bool { return len(s.savedRetries) >= 1 })

		cancel()
		<-done

		if got := len(mockTracker.transitionCalls); got != 1 {
			t.Errorf("transitionCalls = %d, want 1", got)
		}

		store.mu.Lock()
		retries := len(store.savedRetries)
		store.mu.Unlock()
		if retries == 0 {
			t.Error("savedRetries empty, want >= 1 (handoff failure schedules continuation retry)")
		}

		// Claim retained: retry pending keeps the issue as claimed.
		if _, ok := state.Claimed[issue.ID]; !ok {
			t.Error("issue not in Claimed after handoff failure retry, want present")
		}
	})

	t.Run("handoff_transition_failure_with_soft_stop_no_retry", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := handoffConfig(tmpDir)
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("sf-1", "SF-1")

		var workspacePath string

		mockTracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				return fmt.Errorf("tracker unavailable")
			},
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "To Do"
				}
				return result, nil
			},
		}

		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				workspacePath = params.WorkspacePath
				return domain.Session{ID: "sess-sf-1"}, nil
			},
			runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				writeStatusFile(workspacePath, "needs-human-review")
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		}

		var dispatched atomic.Bool
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if dispatched.CompareAndSwap(false, true) {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
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

		pollStore(t, store, func(s *stubStore) bool { return len(s.runHistories) >= 1 })

		cancel()
		<-done

		// Handoff was attempted (one transition call).
		if got := len(mockTracker.transitionCalls); got != 1 {
			t.Errorf("transitionCalls = %d, want 1", got)
		}

		// Soft-stop + handoff failure: claim released WITHOUT scheduling retry.
		store.mu.Lock()
		retries := len(store.savedRetries)
		store.mu.Unlock()
		if retries != 0 {
			t.Errorf("savedRetries = %d, want 0 (soft-stop+handoff failure releases claim without retry)", retries)
		}

		if _, ok := state.Claimed[issue.ID]; ok {
			t.Error("issue still in Claimed after soft-stop+handoff failure, want absent")
		}
	})

	t.Run("reconciliation_cancels_terminal_issue", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		// Small polling interval so reconciliation fires while the worker is running.
		cfg := lifecycleConfig(tmpDir)
		cfg.Polling.IntervalMS = 100
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("rc-1", "RC-1")

		mockTracker := &mockTrackerAdapter{
			// Always report the issue as terminal so reconciliation marks PendingCleanup.
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "Done"
				}
				return result, nil
			},
		}

		// Worker blocks until its context is cancelled by reconciliation.
		agent := &mockAgentAdapter{
			runTurnFn: func(ctx context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				<-ctx.Done()
				return domain.TurnResult{}, ctx.Err()
			},
		}

		var dispatched atomic.Bool
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				if dispatched.CompareAndSwap(false, true) {
					return []domain.Issue{issue}, nil
				}
				return nil, nil
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

		// Poll for a "cancelled" run history entry: evidence that reconciliation
		// cancelled the worker and HandleWorkerExit ran.
		pollStore(t, store, func(s *stubStore) bool {
			for _, rh := range s.runHistories {
				if rh.IssueID == issue.ID && rh.Status == "cancelled" {
					return true
				}
			}
			return false
		})

		cancel()
		<-done

		if _, ok := state.Running[issue.ID]; ok {
			t.Error("issue still in Running after reconciliation cancel, want absent")
		}

		// PendingCleanup=true in HandleWorkerExit triggers workspace removal.
		wsPath := filepath.Join(tmpDir, "RC-1")
		if _, statErr := os.Stat(wsPath); !os.IsNotExist(statErr) {
			t.Errorf("workspace dir %q still exists after PendingCleanup cleanup", wsPath)
		}

		store.mu.Lock()
		var gotStatus string
		for _, rh := range store.runHistories {
			if rh.IssueID == issue.ID {
				gotStatus = rh.Status
				break
			}
		}
		store.mu.Unlock()
		if gotStatus != "cancelled" {
			t.Errorf("run history status = %q, want %q", gotStatus, "cancelled")
		}
	})

	t.Run("no_redispatch_after_handoff_to_non_active_state", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Tracker.HandoffState = "In Review"
		cfg.Polling.IntervalMS = 100 // fast ticks to verify no re-dispatch
		tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
		issue := scenarioIssue("nd-1", "ND-1")

		// handoffDone gates the tracker state returned after the handoff transition.
		// It is written by transitionIssueFn and read by fetchStatesFn, which may run
		// from the event loop or from worker-driven state refreshes. Atomic access
		// also keeps the value visible to the test-goroutine poll below.
		var handoffDone atomic.Bool

		mockTracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				handoffDone.Store(true)
				return nil
			},
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					if handoffDone.Load() {
						result[id] = "In Review"
					} else {
						result[id] = "To Do"
					}
				}
				return result, nil
			},
		}

		agent := &mockAgentAdapter{
			runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			},
		}

		// After handoff, return the issue in "In Review" so ShouldDispatch filters it out.
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: mockTracker,
			fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
				issueState := "To Do"
				if handoffDone.Load() {
					issueState = "In Review"
				}
				return []domain.Issue{{
					ID:         issue.ID,
					Identifier: issue.Identifier,
					Title:      issue.Title,
					State:      issueState,
				}}, nil
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

		// Wait for the handoff transition to complete.
		deadline := time.After(15 * time.Second)
		for !handoffDone.Load() {
			select {
			case <-deadline:
				cancel()
				<-done
				t.Fatal("timed out waiting for handoff transition")
			default:
			}
			time.Sleep(50 * time.Millisecond)
		}

		// Allow several more ticks (≥5 at 100ms interval) to confirm no re-dispatch.
		time.Sleep(500 * time.Millisecond)

		cancel()
		<-done

		store.mu.Lock()
		histCount := len(store.runHistories)
		store.mu.Unlock()
		if histCount != 1 {
			t.Errorf("run history entries = %d, want 1 (issue must not be re-dispatched after handoff)", histCount)
		}

		if got := len(mockTracker.transitionCalls); got != 1 {
			t.Errorf("transitionCalls = %d, want 1", got)
		}

		if _, ok := state.Running[issue.ID]; ok {
			t.Error("issue still in Running after handoff, want absent")
		}
	})
}

// sweepThrottleTracker is a test double for [TestHandleTickSweepThrottle].
// It records invocations of FetchIssueStatesByIdentifiers and returns
// configurable state data. All other methods return safe zero values so
// that tick-loop side effects (reconcile, dispatch) do not panic.
type sweepThrottleTracker struct {
	mu          sync.Mutex
	calls       int
	statesByKey map[string]string
}

var _ domain.TrackerAdapter = (*sweepThrottleTracker)(nil)

func (s *sweepThrottleTracker) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.statesByKey, nil
}

func (s *sweepThrottleTracker) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *sweepThrottleTracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}

func (s *sweepThrottleTracker) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (s *sweepThrottleTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (s *sweepThrottleTracker) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (s *sweepThrottleTracker) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func (s *sweepThrottleTracker) TransitionIssue(_ context.Context, _, _ string) error { return nil }

func (s *sweepThrottleTracker) CommentIssue(_ context.Context, _, _ string) error { return nil }

func (s *sweepThrottleTracker) AddLabel(_ context.Context, _, _ string) error { return nil }

// --- TestHandleTickSweepThrottle ---

func TestHandleTickSweepThrottle(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmpDir, "PROJ-10"), 0o755); err != nil {
		t.Fatalf("os.Mkdir PROJ-10: %v", err)
	}

	tracker := &sweepThrottleTracker{
		statesByKey: map[string]string{"PROJ-10": "Done"},
	}

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			APIKey:         "test-key",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 60000},
		Workspace: config.WorkspaceConfig{Root: tmpDir},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Agent: config.AgentConfig{
			Kind:                "mock",
			Command:             "/usr/bin/agent",
			MaxConcurrentAgents: 1,
		},
	}

	wm := &stubWorkflowManager{config: cfg}
	regs := passingPreflightRegistries()

	state := NewState(60000, 1, nil, AgentTotals{})
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

	ctx := context.Background()

	// Ticks 1 through sweepEveryNTicks-1: sweep must not fire.
	for range sweepEveryNTicks - 1 {
		o.handleTick(ctx)
	}
	if got := tracker.callCount(); got != 0 {
		t.Errorf("FetchIssueStatesByIdentifiers called %d times before tick %d, want 0",
			got, sweepEveryNTicks)
	}

	// Tick sweepEveryNTicks: sweep fires exactly once.
	o.handleTick(ctx)
	if got := tracker.callCount(); got != 1 {
		t.Errorf("FetchIssueStatesByIdentifiers called %d times at tick %d, want 1",
			got, sweepEveryNTicks)
	}

	wsPath := filepath.Join(tmpDir, "PROJ-10")
	if _, err := os.Stat(wsPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("workspace PROJ-10 still exists after sweep, want removed")
	}

	if got := o.state.SweepTickCounter; got != 0 {
		t.Errorf("SweepTickCounter = %d after sweep, want 0", got)
	}
}

// --- TestHandleTick_WorkerWarningChangeDetection ---

func TestHandleTick_WorkerWarningChangeDetection(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           "mock",
			ActiveStates:   []string{"To Do"},
			TerminalStates: []string{"Done"},
		},
		Polling:   config.PollingConfig{IntervalMS: 60000},
		Workspace: config.WorkspaceConfig{Root: t.TempDir()},
		Agent: config.AgentConfig{
			Kind:                "mock",
			MaxConcurrentAgents: 1,
		},
	}
	cfg.SetExtensionSection("worker", map[string]any{
		"ssh_strict_host_key_checking": "ask",
	})

	wm := &stubWorkflowManager{config: cfg}
	state := NewState(60000, 1, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          logger,
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{
			ReloadWorkflow: func() error { return errPreflightFailed },
			ConfigFunc:     wm.Config,
		},
	})

	ctx := context.Background()

	const warnMsg = "rejected unrecognized ssh_strict_host_key_checking value"

	// First tick: warning for "ask" must be logged.
	o.handleTick(ctx)
	// Second tick: same config, warning must be suppressed.
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), warnMsg); got != 1 {
		t.Errorf("warning count after two identical ticks = %d, want 1\nlog:\n%s", got, buf.String())
	}

	// Change to a different invalid value — a new warning must appear.
	cfg.SetExtensionSection("worker", map[string]any{
		"ssh_strict_host_key_checking": "strict",
	})
	wm.setConfig(cfg)
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), warnMsg); got != 2 {
		t.Errorf("warning count after changing value to 'strict' = %d, want 2\nlog:\n%s", got, buf.String())
	}

	// Change SSHHosts while keeping the same invalid value — warning must be suppressed.
	cfg.SetExtensionSection("worker", map[string]any{
		"ssh_strict_host_key_checking": "strict",
		"ssh_hosts":                    []any{"host-a"},
	})
	wm.setConfig(cfg)
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), warnMsg); got != 2 {
		t.Errorf("warning count after changing SSHHosts only = %d, want 2\nlog:\n%s", got, buf.String())
	}
}

// TestTickLogging_DispatchBreakdown verifies that the "tick completed" log
// entry carries the dispatched_by_rule, dispatched_by_default, and
// dispatched_by_fallback counters reflecting the actual resolution layers
// used for each dispatched issue.
func TestTickLogging_DispatchBreakdown(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)

	// Wire a dispatch config with one named rule that matches label "bug".
	// Issue A-1 carries the label → resolved from rule.
	// Issue A-2 carries no label → falls through to fallback (no default configured).
	cfg.Dispatch = config.DispatchConfig{
		Rules: []config.DispatchRule{
			{
				Name:       "bug-rule",
				Match:      config.DispatchMatch{Labels: []string{"bug"}},
				Selection:  config.DispatchSelection{},
				IsCatchAll: false,
			},
		},
	}

	issues := []domain.Issue{
		{ID: "id-1", Identifier: "A-1", Title: "Bug fix", State: "To Do", Labels: []string{"bug"}},
		{ID: "id-2", Identifier: "A-2", Title: "Feature", State: "To Do"},
	}

	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	lb := &lockedBuf{}
	logger := slog.New(slog.NewTextHandler(lb, nil))

	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

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
	if !strings.Contains(got, "dispatched=2") {
		t.Errorf("log missing dispatched=2: %s", got)
	}
	// One issue matched the named rule.
	if !strings.Contains(got, "dispatched_by_rule=1") {
		t.Errorf("log missing dispatched_by_rule=1: %s", got)
	}
	// No default configured, no default dispatch.
	if !strings.Contains(got, "dispatched_by_default=0") {
		t.Errorf("log missing dispatched_by_default=0: %s", got)
	}
	// One issue fell through to fallback.
	if !strings.Contains(got, "dispatched_by_fallback=1") {
		t.Errorf("log missing dispatched_by_fallback=1: %s", got)
	}
}

// TestDispatch_RuleResolvedKindPersistsToRunHistory is a regression test for
// the freeze-on-dispatch wiring through
// ResolveRule → RunningEntry.AgentKind → WorkerDeps.AgentKind →
// WorkerResult.AgentAdapter → RunHistory.AgentAdapter.
//
// Before the fix, RunWorkerAttempt always populated WorkerResult.AgentAdapter
// from cfg.Agent.Kind (the workflow default) instead of deps.AgentKind, so
// a rule that routed an issue to a non-default agent kind caused run_history
// to record the wrong adapter name.
func TestDispatch_RuleResolvedKindPersistsToRunHistory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)

	// Workflow default is "claude-code". One rule routes issues labelled "bug"
	// to "codex". The second issue carries no "bug" label and falls through to
	// the workflow-wide fallback ("claude-code").
	cfg.Agent.Kind = "claude-code"
	cfg.Dispatch = config.DispatchConfig{
		Rules: []config.DispatchRule{
			{
				Name:      "bug-router",
				Match:     config.DispatchMatch{Labels: []string{"bug"}},
				Selection: config.DispatchSelection{AgentKind: "codex"},
			},
		},
	}

	// A non-empty WorkflowAbsPath is the only thing standing between this
	// test and MCP config generation; the file it names need not exist.
	// Both agent blocks name an operator MCP config carrying a marker
	// server unique to that block, so the routed session's generated
	// file can be told apart from a session that read the wrong block.
	absWorkflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

	claudeMCPConfigPath := filepath.Join(tmpDir, "claude-mcp.json")
	writeMarkerMCPConfig(t, claudeMCPConfigPath, "claude-marker")

	codexMCPConfigPath := filepath.Join(tmpDir, "codex-mcp.json")
	writeMarkerMCPConfig(t, codexMCPConfigPath, "codex-marker")

	cfg.SetExtensionSection("claude-code", map[string]any{"mcp_config": claudeMCPConfigPath})
	cfg.SetExtensionSection("codex", map[string]any{"mcp_config": codexMCPConfigPath})

	bugIssue := domain.Issue{
		ID:         "id-bug",
		Identifier: "DISP-1",
		Title:      "Bug fix",
		State:      "To Do",
		Labels:     []string{"bug"},
	}
	docsIssue := domain.Issue{
		ID:         "id-docs",
		Identifier: "DISP-2",
		Title:      "Docs update",
		State:      "To Do",
		Labels:     []string{"docs"},
	}

	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	// Captured per adapter: the two issues dispatch concurrently through
	// one run loop, so a shared capture would race between them.
	var codexGeneratedMCPConfigPath atomic.Value
	var claudeGeneratedMCPConfigPath atomic.Value

	codexAdapter := &mockAgentAdapter{
		startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
			codexGeneratedMCPConfigPath.Store(params.MCPConfigPath)
			return domain.Session{ID: "sess-codex"}, nil
		},
	}
	claudeAdapter := &mockAgentAdapter{
		startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
			claudeGeneratedMCPConfigPath.Store(params.MCPConfigPath)
			return domain.Session{ID: "sess-claude"}, nil
		},
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
			return []domain.Issue{bugIssue, docsIssue}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl, absPath: absWorkflowPath}
	store := &stubStore{}
	regs := passingPreflightRegistries()

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:          state,
		Logger:         discardLogger(),
		TrackerAdapter: tracker,
		AgentAdapter:   claudeAdapter,
		AgentAdapterByKind: func(kind string) (domain.AgentAdapter, error) {
			switch kind {
			case "codex":
				return codexAdapter, nil
			case "claude-code":
				return claudeAdapter, nil
			default:
				return nil, fmt.Errorf("unknown agent kind %q", kind)
			}
		},
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

	// Wait until both issues have produced a RunHistory row.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			store.mu.Lock()
			n := len(store.runHistories)
			store.mu.Unlock()
			t.Fatalf("timed out: run histories = %d, want 2", n)
		default:
		}
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done

	// Index rows by IssueID so assertion order is independent of dispatch order.
	store.mu.Lock()
	rows := make(map[string]persistence.RunHistory, len(store.runHistories))
	for _, rh := range store.runHistories {
		rows[rh.IssueID] = rh
	}
	store.mu.Unlock()

	if len(rows) != 2 {
		t.Fatalf("len(runHistories) = %d, want 2", len(rows))
	}

	// Rule-routed issue: must record the rule-resolved kind, not the workflow default.
	bugRow, ok := rows[bugIssue.ID]
	if !ok {
		t.Fatalf("no RunHistory row for bug issue %q", bugIssue.ID)
	}
	if bugRow.AgentAdapter != "codex" {
		t.Errorf("RunHistory(%q).AgentAdapter = %q, want %q", bugIssue.Identifier, bugRow.AgentAdapter, "codex")
	}
	if bugRow.RuleName != "bug-router" {
		t.Errorf("RunHistory(%q).RuleName = %q, want %q", bugIssue.Identifier, bugRow.RuleName, "bug-router")
	}

	// Fallback issue: must record the workflow-wide default kind.
	docsRow, ok := rows[docsIssue.ID]
	if !ok {
		t.Fatalf("no RunHistory row for docs issue %q", docsIssue.ID)
	}
	if docsRow.AgentAdapter != "claude-code" {
		t.Errorf("RunHistory(%q).AgentAdapter = %q, want %q", docsIssue.Identifier, docsRow.AgentAdapter, "claude-code")
	}
	if docsRow.RuleName != "" {
		t.Errorf("RunHistory(%q).RuleName = %q, want %q", docsIssue.Identifier, docsRow.RuleName, "")
	}

	// The routed session's generated MCP config must carry the codex
	// block's marker server and must not carry the claude-code block's,
	// proving the routed kind selected its own operator block rather
	// than the workflow default's.
	gotPath, _ := codexGeneratedMCPConfigPath.Load().(string)
	if gotPath == "" {
		t.Fatal("codex adapter's StartSessionParams.MCPConfigPath is empty, want a generated file")
	}
	servers := readMCPServers(t, gotPath)
	if _, ok := servers["codex-marker"]; !ok {
		t.Errorf("mcpServers in %q missing %q, want present (routed kind's own block)", gotPath, "codex-marker")
	}
	if _, ok := servers["claude-marker"]; ok {
		t.Errorf("mcpServers in %q contains %q, want absent (default kind's block must not leak into a routed session)", gotPath, "claude-marker")
	}
}

// writeMarkerMCPConfig writes an operator MCP config file at path
// declaring a single stdio server named marker, so a test can tell
// apart the generated config produced from that block versus another.
func writeMarkerMCPConfig(t *testing.T, path, marker string) {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			marker: map[string]any{"type": "stdio", "command": "/bin/" + marker},
		},
	})
	if err != nil {
		t.Fatalf("Marshal operator MCP config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// readMCPServers reads and parses the mcpServers object out of the
// generated MCP config file at path.
func readMCPServers(t *testing.T, path string) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal(%q): %v", path, err)
	}
	servers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("%q: mcpServers is not an object: %v", path, parsed["mcpServers"])
	}
	return servers
}

// --- Blocker gate observability (handleTick) ---

// newBlockerGateOrchestrator builds an Orchestrator wired with a "mock"
// tracker returning issues as its candidates and resolver as the
// blocker resolver, logging at Debug so per-issue records are
// captured. metrics may be nil, in which case NewOrchestrator's own
// default applies.
func newBlockerGateOrchestrator(t *testing.T, issues []domain.Issue, resolver BlockerResolver, metrics domain.Metrics) (*Orchestrator, *lockedBuf) {
	t.Helper()

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
			MaxConcurrentAgents: 10,
			MaxTurns:            1,
			ReadTimeoutMS:       1000,
		},
	}

	lb := &lockedBuf{}
	logger := slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug}))

	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

	tmpl := mustParseTemplate(t, "do {{.issue.identifier}}")

	o := NewOrchestrator(OrchestratorParams{
		State:  NewState(1000, 10, nil, AgentTotals{}),
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
		BlockerResolver: resolver,
		Metrics:         metrics,
	})

	return o, lb
}

// TestHandleTick_DispatchUsesResolvedIssue pins that a dispatched
// candidate's running entry carries the resolver's returned issue, not
// the raw candidate the tracker produced.
func TestHandleTick_DispatchUsesResolvedIssue(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "R-1", Identifier: "R-1", Title: "T", State: "To Do", BlockersUnresolved: true}

	resolver := &fakeBlockerResolver{
		needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
		resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
			i.BlockersUnresolved = false
			i.Description = "resolved-by-test"
			return i, nil
		},
	}

	o, _ := newBlockerGateOrchestrator(t, []domain.Issue{issue}, resolver, nil)

	o.handleTick(context.Background())
	o.state.WorkerWg.Wait()

	entry, ok := o.state.Running[issue.ID]
	if !ok {
		t.Fatal("issue was not dispatched")
	}
	if entry.Issue.BlockersUnresolved {
		t.Error("dispatched entry carries BlockersUnresolved = true, want the resolved issue")
	}
	if entry.Issue.Description != "resolved-by-test" {
		t.Errorf("dispatched entry.Issue.Description = %q, want %q (the resolver's return value, not the raw candidate)",
			entry.Issue.Description, "resolved-by-test")
	}
}

// TestHandleTick_CandidateHoldReasons pins that IncCandidateHolds
// increments once per hold with the matching reason, across all four
// blocker-related reasons, never for an ineligible candidate, and
// never for the pass-level halt record.
func TestHandleTick_CandidateHoldReasons(t *testing.T) {
	t.Parallel()

	blockedByIssue := domain.Issue{
		ID: "H-1", Identifier: "H-1", Title: "T", State: "To Do",
		BlockedBy: []domain.BlockerRef{{ID: "b", State: "To Do"}},
	}
	incompleteIssue := domain.Issue{
		ID: "H-2", Identifier: "H-2", Title: "T", State: "To Do", BlockersUnresolved: true,
	}
	unresolvedFailIssue := domain.Issue{
		ID: "H-3", Identifier: "H-3", Title: "T", State: "To Do", BlockersUnresolved: true,
	}
	ineligibleIssue := domain.Issue{ID: "", Identifier: "H-4", Title: "T", State: "To Do"}

	transientErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport}

	resolver := &fakeBlockerResolver{
		needsReadFn: func(i domain.Issue) bool { return i.ID == unresolvedFailIssue.ID },
		resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
			i.BlockersUnresolved = true
			return i, transientErr
		},
	}

	spy := &spyMetrics{}
	o, _ := newBlockerGateOrchestrator(t, []domain.Issue{
		blockedByIssue, incompleteIssue, unresolvedFailIssue, ineligibleIssue,
	}, resolver, spy)

	o.handleTick(context.Background())
	o.state.WorkerWg.Wait()

	spy.mu.Lock()
	holds := append([]string(nil), spy.candidateHolds...)
	spy.mu.Unlock()

	wantCounts := map[string]int{
		string(SkipBlockedBy):          1,
		string(SkipBlockersIncomplete): 1,
		string(SkipBlockersUnresolved): 1,
	}
	gotCounts := map[string]int{}
	for _, reason := range holds {
		gotCounts[reason]++
	}
	for reason, want := range wantCounts {
		if gotCounts[reason] != want {
			t.Errorf("IncCandidateHolds(%q) called %d times, want %d (holds=%v)", reason, gotCounts[reason], want, holds)
		}
	}
	if gotCounts[string(SkipIneligible)] != 0 {
		t.Errorf("IncCandidateHolds(%q) called %d times, want 0: an ineligible candidate must never be counted", SkipIneligible, gotCounts[string(SkipIneligible)])
	}
	if len(holds) != 3 {
		t.Errorf("total IncCandidateHolds calls = %d, want 3 (holds=%v)", len(holds), holds)
	}
}

// TestHandleTick_EveryAttemptedReadFailedWarning pins the four
// distinguishing cases the "every attempted read failed" WARN
// depends on: it fires only when every read the pass attempted
// failed transiently and the tick dispatched nothing, and it carries
// reads_failed as the count of reads attempted, not the count of
// candidates held.
func TestHandleTick_EveryAttemptedReadFailedWarning(t *testing.T) {
	t.Parallel()

	const warnMsg = "tick dispatched nothing: every attempted candidate blocker read failed"
	transientErr := &domain.TrackerError{Kind: domain.ErrTrackerTransport}

	t.Run("fires when every attempted read fails and nothing dispatches", func(t *testing.T) {
		t.Parallel()

		issues := []domain.Issue{
			{ID: "W-1", Identifier: "W-1", Title: "T", State: "To Do", BlockersUnresolved: true},
			{ID: "W-2", Identifier: "W-2", Title: "T", State: "To Do", BlockersUnresolved: true},
		}
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				i.BlockersUnresolved = true
				return i, transientErr
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if strings.Count(got, warnMsg) != 1 {
			t.Fatalf("WARN count = %d, want 1: %s", strings.Count(got, warnMsg), got)
		}
		if !strings.Contains(got, "reads_failed=2") {
			t.Errorf("log missing reads_failed=2: %s", got)
		}
	})

	t.Run("does not fire when only one of several attempted reads failed", func(t *testing.T) {
		t.Parallel()

		issues := []domain.Issue{
			{ID: "W-3", Identifier: "W-3", Title: "T", State: "To Do", BlockersUnresolved: true},
			{ID: "W-4", Identifier: "W-4", Title: "T", State: "To Do", BlockersUnresolved: true},
		}
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				if i.ID == "W-3" {
					i.BlockersUnresolved = true
					return i, transientErr
				}
				// W-4 resolves successfully but stays held by a live
				// non-terminal blocker, so the tick still dispatches
				// nothing even though this read did not fail.
				i.BlockersUnresolved = false
				i.BlockedBy = []domain.BlockerRef{{ID: "b", State: "To Do"}}
				return i, nil
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if strings.Contains(got, warnMsg) {
			t.Errorf("WARN fired with only one of two attempted reads failing: %s", got)
		}
	})

	t.Run("does not fire on a pass that attempted no read", func(t *testing.T) {
		t.Parallel()

		issues := []domain.Issue{
			{ID: "W-5", Identifier: "W-5", Title: "T", State: "To Do", BlockersUnresolved: true},
		}
		resolver := &fakeBlockerResolver{
			needsReadFn: func(domain.Issue) bool { return false },
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if strings.Contains(got, warnMsg) {
			t.Errorf("WARN fired on a pass with zero attempted reads: %s", got)
		}
	})

	t.Run("carries the attempted-and-failed count, not the held count, when the budget binds", func(t *testing.T) {
		t.Parallel()

		issues := budgetWindowIssues()
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				i.BlockersUnresolved = true
				return i, transientErr
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if strings.Count(got, warnMsg) != 1 {
			t.Fatalf("WARN count = %d, want 1: %s", strings.Count(got, warnMsg), got)
		}
		if !strings.Contains(got, fmt.Sprintf("reads_failed=%d", maxBlockerReadsPerPass)) {
			t.Errorf("log missing reads_failed=%d (the budget, not the 6 candidates held): %s", maxBlockerReadsPerPass, got)
		}
	})

	t.Run("suppressed when the pass already halted", func(t *testing.T) {
		t.Parallel()

		deploymentErr := &domain.TrackerError{Kind: domain.ErrTrackerAuth}
		issues := []domain.Issue{
			{ID: "W-6", Identifier: "W-6", Title: "T", State: "To Do", BlockersUnresolved: true},
		}
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				i.BlockersUnresolved = true
				return i, deploymentErr
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if strings.Contains(got, warnMsg) {
			t.Errorf("WARN fired on a halted pass, want it suppressed in favor of the pass-level ERROR: %s", got)
		}
		if !strings.Contains(got, "blocker reads halted for this tick") {
			t.Errorf("log missing the pass-level halt ERROR: %s", got)
		}
	})
}

// TestHandleTick_BudgetSkipAndHaltSkipLogRecordsDiffer pins that a
// budget-skipped candidate and a halt-skipped candidate emit distinct
// DEBUG messages, even though both share the blockers_unresolved and
// blockers_not_read reason vocabulary respectively.
func TestHandleTick_BudgetSkipAndHaltSkipLogRecordsDiffer(t *testing.T) {
	t.Parallel()

	t.Run("budget-skipped candidate logs the not-read-this-tick record", func(t *testing.T) {
		t.Parallel()

		issues := budgetWindowIssues()
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				i.BlockersUnresolved = false
				return i, nil
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if !strings.Contains(got, "candidate blockers not read this tick, holding issue") {
			t.Errorf("log missing the budget-skip DEBUG record: %s", got)
		}
		if strings.Contains(got, "candidate blockers not read this tick, pass halted") {
			t.Errorf("log carries the halt-skip DEBUG record on a pass that never halted: %s", got)
		}
	})

	t.Run("halt-skipped candidate logs the pass-halted record", func(t *testing.T) {
		t.Parallel()

		deploymentErr := &domain.TrackerError{Kind: domain.ErrTrackerAuth}
		issues := []domain.Issue{
			{ID: "HS-1", Identifier: "HS-1", Title: "T", State: "To Do", BlockersUnresolved: true},
			{ID: "HS-2", Identifier: "HS-2", Title: "T", State: "To Do", BlockersUnresolved: true},
		}
		resolver := &fakeBlockerResolver{
			needsReadFn: func(i domain.Issue) bool { return i.BlockersUnresolved },
			resolveFn: func(_ context.Context, i domain.Issue) (domain.Issue, error) {
				i.BlockersUnresolved = true
				return i, deploymentErr
			},
		}

		o, lb := newBlockerGateOrchestrator(t, issues, resolver, nil)
		o.handleTick(context.Background())
		o.state.WorkerWg.Wait()

		got := lb.String()
		if !strings.Contains(got, "candidate blockers not read this tick, pass halted") {
			t.Errorf("log missing the halt-skip DEBUG record: %s", got)
		}
		if strings.Contains(got, "candidate blockers not read this tick, holding issue") {
			t.Errorf("log carries the budget-skip DEBUG record on a pass that halted before the budget bound: %s", got)
		}
	})
}

// --- Budget Hold Tracker Notice Tests ---

// TestHandleTick_BudgetHoldNoticeOnce fails if the notice repeats on a
// second tick that re-observes the same hold, rather than posting exactly
// once across both ticks.
func TestHandleTick_BudgetHoldNoticeOnce(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-once", Identifier: "PROJ-NOTICE-ONCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	orch := budgetOrchestrator(state, wm, store, tracker)
	orch.handleTick(context.Background())
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	calls := tracker.commentCalls
	if len(calls) != 1 {
		t.Fatalf("commentCalls across two ticks = %+v, want exactly one", calls)
	}
	if calls[0].IssueID != issue.ID {
		t.Errorf("commentCalls[0].IssueID = %q, want %q", calls[0].IssueID, issue.ID)
	}

	entry, ok := state.BudgetExhausted[issue.ID]
	if !ok {
		t.Fatal("BudgetExhausted missing after the ticks, want present")
	}
	if want := buildBudgetHoldComment(entry); calls[0].Text != want {
		t.Errorf("commentCalls[0].Text =\n%q\nwant\n%q", calls[0].Text, want)
	}

	if len(store.budgetHoldNotices) != 1 || store.budgetHoldNotices[0].IssueID != issue.ID {
		t.Errorf("store.budgetHoldNotices = %+v, want exactly one upsert for %q", store.budgetHoldNotices, issue.ID)
	}
}

// TestBudgetHoldNoticeSurvivesRestart fails if the notice repeats after a
// simulated restart. It drives one tick against a real, file-backed store,
// closes it to simulate process exit, reopens the same file, loads
// [State.BudgetHoldNoticed] from the durable rows, and drives a second
// tick over the same candidate: the row, not the in-memory latch, is the
// mechanism proven durable here.
func TestBudgetHoldNoticeSurvivesRestart(t *testing.T) {
	t.Parallel()

	const issueID = "BUDGET-RESTART"
	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"

	store1, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	if err := store1.Migrate(ctx); err != nil {
		t.Fatalf("store1.Migrate: %v", err)
	}

	for range 3 {
		if _, err := store1.AppendRunHistory(ctx, persistence.RunHistory{
			IssueID:      issueID,
			Identifier:   "PROJ-RESTART",
			Attempt:      1,
			AgentAdapter: "mock",
			Workspace:    "/tmp/" + issueID,
			StartedAt:    "2026-08-17T00:00:00Z",
			CompletedAt:  "2026-08-17T00:01:00Z",
			Status:       "succeeded",
		}); err != nil {
			t.Fatalf("AppendRunHistory: %v", err)
		}
	}

	issue := domain.Issue{ID: issueID, Identifier: "PROJ-RESTART", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	regs := passingPreflightRegistries()
	regs.ReloadWorkflow = func() error { return nil }
	regs.ConfigFunc = wm.Config

	tracker1 := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	state1 := NewState(60000, 10, nil, AgentTotals{})
	orch1 := NewOrchestrator(OrchestratorParams{
		State:           state1,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker1,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store1,
		PreflightParams: regs,
	})
	orch1.handleTick(ctx)
	state1.TrackerOpsWg.Wait()

	if len(tracker1.commentCalls) != 1 {
		t.Fatalf("commentCalls before the restart = %+v, want exactly one", tracker1.commentCalls)
	}

	rows, err := store1.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices before the restart: %v", err)
	}
	if len(rows) != 1 || rows[0].IssueID != issueID {
		t.Fatalf("ListBudgetHoldNotices before the restart = %+v, want exactly one row for %q", rows, issueID)
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	store2, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open (reopen): %v", err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("store2.Close: %v", err)
		}
	})

	rows2, err := store2.ListBudgetHoldNotices(ctx)
	if err != nil {
		t.Fatalf("ListBudgetHoldNotices after reopening: %v", err)
	}

	state2 := NewState(60000, 10, nil, AgentTotals{})
	PopulateBudgetHoldNotices(state2, rows2, discardLogger())

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tracker2 := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch2 := NewOrchestrator(OrchestratorParams{
		State:           state2,
		Logger:          logger,
		TrackerAdapter:  tracker2,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store2,
		PreflightParams: regs,
	})
	orch2.handleTick(ctx)
	state2.TrackerOpsWg.Wait()

	if len(tracker2.commentCalls) != 0 {
		t.Errorf("commentCalls after the simulated restart = %+v, want none (the durable row deduplicates)", tracker2.commentCalls)
	}
	if !strings.Contains(buf.String(), "candidate held by budget ceiling") {
		t.Error("second orchestrator's log output missing the budget-ceiling record: the in-memory log latch is empty after a restart and must re-announce")
	}
}

// TestHandleTick_BudgetHoldNoticeTrackerFailureIsolated fails if an
// always-failing CommentIssue reaches the poll tick's outputs, or if the
// failed write is retried on a following tick that observes the same,
// still-unresolved hold.
func TestHandleTick_BudgetHoldNoticeTrackerFailureIsolated(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-fail", Identifier: "PROJ-NOTICE-FAIL", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{
			commentIssueFn: func(_ context.Context, _, _ string) error {
				return fmt.Errorf("tracker unavailable")
			},
		},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	spy := &spyMetrics{}

	orch := budgetOrchestratorWithMetrics(state, wm, store, tracker, logger, spy)
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if _, ok := state.BudgetExhausted[issue.ID]; !ok {
		t.Error("BudgetExhausted missing after a failing tracker write, want present (unaffected by tracker failure)")
	}
	if _, running := state.Running[issue.ID]; running {
		t.Error("Running present for a budget-held issue, want absent")
	}
	if !strings.Contains(buf.String(), "budget hold notice failed") {
		t.Errorf("log output missing the failure record; log:\n%s", buf.String())
	}
	found := false
	for _, call := range spy.trackerComments {
		if call.lifecycle == "budget_hold" && call.result == "error" {
			found = true
		}
	}
	if !found {
		t.Errorf("trackerComments = %+v, want a budget_hold/error entry", spy.trackerComments)
	}

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 1 {
		t.Errorf("commentCalls across two ticks = %d, want 1 (no retry of a failed write)", got)
	}
}

// TestHandleTick_BudgetHoldNoticeReasonChange fails if a governing-ceiling
// change from session to token does not post a second comment naming the
// new ceiling's setting.
func TestHandleTick_BudgetHoldNoticeReasonChange(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-reason", Identifier: "PROJ-NOTICE-REASON", Title: "title", State: "To Do"}
	wm := budgetTickConfigTokens(3, 1000)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait() // ordering-load-bearing: the detached CommentIssue goroutines are not otherwise ordered across ticks

	store.budgetExhaustedIDs = map[string]int{}
	store.tokenExhaustedIDs = []string{issue.ID}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	calls := tracker.commentCalls
	if len(calls) != 2 {
		t.Fatalf("commentCalls = %+v, want 2 (one per governing ceiling)", calls)
	}
	if !strings.Contains(calls[1].Text, "agent.max_tokens") {
		t.Errorf("second comment = %q, want it to name agent.max_tokens", calls[1].Text)
	}
	if len(store.budgetHoldNotices) != 2 {
		t.Fatalf("store.budgetHoldNotices = %+v, want 2 upsert calls (one per notice)", store.budgetHoldNotices)
	}
	if store.budgetHoldNotices[1].Reason != budgetReasonToken {
		t.Errorf("second upsert Reason = %q, want %q", store.budgetHoldNotices[1].Reason, budgetReasonToken)
	}
	if state.BudgetHoldNoticed[issue.ID] != budgetReasonToken {
		t.Errorf("BudgetHoldNoticed[%s] = %q, want %q", issue.ID, state.BudgetHoldNoticed[issue.ID], budgetReasonToken)
	}
}

// TestHandleTick_BudgetHoldNoticeReleaseOnClear fails if a genuine
// clearance (the issue is still a candidate this tick but no longer
// held) does not release both the memory entry and the durable row, or if
// a later re-hold under the same reason does not post a second comment.
func TestHandleTick_BudgetHoldNoticeReleaseOnClear(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-clear", Identifier: "PROJ-NOTICE-CLEAR", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("commentCalls after the first tick = %+v, want 1", tracker.commentCalls)
	}
	if _, noticed := state.BudgetHoldNoticed[issue.ID]; !noticed {
		t.Fatal("BudgetHoldNoticed missing after the first tick, want present")
	}

	store.budgetExhaustedIDs = map[string]int{} // the issue is still a candidate; the ceiling clears
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if _, noticed := state.BudgetHoldNoticed[issue.ID]; noticed {
		t.Error("BudgetHoldNoticed still present after a genuine clearance, want released")
	}
	if len(store.deletedBudgetHoldIDs) != 1 || store.deletedBudgetHoldIDs[0] != issue.ID {
		t.Errorf("store.deletedBudgetHoldIDs = %v, want [%q]", store.deletedBudgetHoldIDs, issue.ID)
	}

	store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 2 {
		t.Errorf("commentCalls after the third tick = %d, want 2 (released, then re-noticed)", got)
	}
}

// TestHandleTick_BudgetHoldNoticeAbsenceThenReturn fails if a hold that
// leaves the tracker's candidate set for one or more ticks (as opposed to
// being observed and found cleared) is treated as released: absence alone
// must not delete the memory or the row, so a later return under the same
// reason posts nothing.
func TestHandleTick_BudgetHoldNoticeAbsenceThenReturn(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-absence", Identifier: "PROJ-NOTICE-ABSENCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	present := true
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			if present {
				return []domain.Issue{issue}, nil
			}
			return nil, nil
		},
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("commentCalls after the first tick = %+v, want 1", tracker.commentCalls)
	}

	present = false
	store.budgetExhaustedIDs = map[string]int{} // the rebuild's query is scoped to this tick's candidateIDs
	orch.handleTick(context.Background())
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if len(store.deletedBudgetHoldIDs) != 0 {
		t.Errorf("store.deletedBudgetHoldIDs = %v, want none (absence from the candidate set is not evidence the hold cleared)", store.deletedBudgetHoldIDs)
	}
	if _, noticed := state.BudgetHoldNoticed[issue.ID]; !noticed {
		t.Error("BudgetHoldNoticed released during the absent ticks, want retained")
	}

	present = true
	store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 1 {
		t.Errorf("commentCalls after the return = %d, want still 1 (same reason already noticed)", got)
	}
}

// TestHandleTick_BudgetHoldNoticeFoldedForward fails if a query error that
// folds the prior hold forward posts a comment for an entry that is last
// tick's evidence, not this tick's, or if the following successful tick
// re-announces a hold the memory already knows about.
func TestHandleTick_BudgetHoldNoticeFoldedForward(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-fold", Identifier: "PROJ-NOTICE-FOLD", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("commentCalls after the first tick = %+v, want 1", tracker.commentCalls)
	}

	store.budgetExhaustedErr = fmt.Errorf("db error")
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if got := len(tracker.commentCalls); got != 1 {
		t.Errorf("commentCalls after the query-error tick = %d, want still 1 (a folded-forward entry is not this tick's evidence)", got)
	}

	store.budgetExhaustedErr = nil
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if got := len(tracker.commentCalls); got != 1 {
		t.Errorf("commentCalls after the recovered tick = %d, want still 1 (same reason already noticed)", got)
	}
}

// TestHandleTick_BudgetHoldNoticeDisableReenable fails if disabling both
// budgets does not clear the memory in one DeleteAllBudgetHoldNotices
// call, if an idle disabled tick issues a redundant call, or if
// re-enabling a ceiling does not post a fresh notice for the hold that
// re-forms.
func TestHandleTick_BudgetHoldNoticeDisableReenable(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-disable", Identifier: "PROJ-NOTICE-DISABLE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()
	if _, noticed := state.BudgetHoldNoticed[issue.ID]; !noticed {
		t.Fatal("BudgetHoldNoticed missing after the first tick, want present")
	}

	wm.config.Agent.MaxSessions = 0
	wm.config.Agent.MaxTokens = 0
	orch.handleTick(context.Background())

	if len(state.BudgetHoldNoticed) != 0 {
		t.Errorf("BudgetHoldNoticed = %v, want empty after both budgets disabled", state.BudgetHoldNoticed)
	}
	if store.deleteAllBudgetHoldCalls != 1 {
		t.Errorf("DeleteAllBudgetHoldNotices calls = %d, want 1", store.deleteAllBudgetHoldCalls)
	}

	orch.handleTick(context.Background())
	if store.deleteAllBudgetHoldCalls != 1 {
		t.Errorf("DeleteAllBudgetHoldNotices calls after a second disabled tick = %d, want still 1 (memory already empty)", store.deleteAllBudgetHoldCalls)
	}

	wm.config.Agent.MaxSessions = 3
	store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 2 {
		t.Errorf("commentCalls after re-enabling = %d, want 2 (the re-formed hold is announced again)", got)
	}
}

// TestHandleTick_BudgetHoldNoticePacingWindow fails if a rebuild with more
// newly held candidates than the pacing window allows posts more or fewer
// than maxBudgetHoldNoticesPerWindow per window, drops or duplicates a
// candidate across windows, or ignores a short polling.interval_ms and
// derives its bound per tick instead of per wall-clock window.
func TestHandleTick_BudgetHoldNoticePacingWindow(t *testing.T) {
	t.Parallel()

	makeCandidates := func(n int) ([]domain.Issue, map[string]int) {
		issues := make([]domain.Issue, n)
		exhausted := make(map[string]int, n)
		for i := range n {
			id := fmt.Sprintf("iss-pace-%02d", i)
			issues[i] = domain.Issue{ID: id, Identifier: fmt.Sprintf("PROJ-PACE-%02d", i), Title: "title", State: "To Do"}
			exhausted[id] = 5
		}
		return issues, exhausted
	}

	t.Run("25 newly held candidates post 10 per window across three ticks", func(t *testing.T) {
		t.Parallel()

		issues, exhausted := makeCandidates(25)
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: exhausted}
		state := NewState(60000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return issues, nil },
		}
		orch := budgetOrchestrator(state, wm, store, tracker)

		orch.handleTick(context.Background())
		state.TrackerOpsWg.Wait()
		if got := len(tracker.commentCalls); got != maxBudgetHoldNoticesPerWindow {
			t.Fatalf("commentCalls after the first tick = %d, want %d", got, maxBudgetHoldNoticesPerWindow)
		}

		state.BudgetHoldNoticeWindowStart = state.BudgetHoldNoticeWindowStart.Add(-budgetHoldNoticeWindow)
		orch.handleTick(context.Background())
		state.TrackerOpsWg.Wait()
		if got := len(tracker.commentCalls); got != 2*maxBudgetHoldNoticesPerWindow {
			t.Fatalf("commentCalls after the second tick = %d, want %d", got, 2*maxBudgetHoldNoticesPerWindow)
		}

		state.BudgetHoldNoticeWindowStart = state.BudgetHoldNoticeWindowStart.Add(-budgetHoldNoticeWindow)
		orch.handleTick(context.Background())
		state.TrackerOpsWg.Wait()
		if got := len(tracker.commentCalls); got != 25 {
			t.Fatalf("commentCalls after the third tick = %d, want 25", got)
		}

		seen := make(map[string]int, 25)
		for _, call := range tracker.commentCalls {
			seen[call.IssueID]++
		}
		if len(seen) != 25 {
			t.Errorf("distinct issues notified = %d, want 25", len(seen))
		}
		for id, count := range seen {
			if count != 1 {
				t.Errorf("issue %s received %d comments, want exactly 1", id, count)
			}
		}

		firstTickIDs := make(map[string]struct{}, maxBudgetHoldNoticesPerWindow)
		for _, call := range tracker.commentCalls[:maxBudgetHoldNoticesPerWindow] {
			firstTickIDs[call.IssueID] = struct{}{}
		}
		for i := range maxBudgetHoldNoticesPerWindow {
			wantID := fmt.Sprintf("iss-pace-%02d", i)
			if _, ok := firstTickIDs[wantID]; !ok {
				t.Errorf("first tick's notices = %v, missing %q: the deterministic (Identifier, id) order must land the lexicographically first 10", firstTickIDs, wantID)
			}
		}
	})

	t.Run("the same 10-per-window ceiling holds independent of a short polling interval", func(t *testing.T) {
		t.Parallel()

		issues, exhausted := makeCandidates(25)
		wm := budgetTickConfig(3)
		wm.config.Polling.IntervalMS = 1000
		store := &stubStore{budgetExhaustedIDs: exhausted}
		state := NewState(1000, 10, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return issues, nil },
		}
		orch := budgetOrchestrator(state, wm, store, tracker)

		orch.handleTick(context.Background())
		state.TrackerOpsWg.Wait()

		if got := len(tracker.commentCalls); got != maxBudgetHoldNoticesPerWindow {
			t.Errorf("commentCalls with a 1000ms poll interval = %d, want %d (the bound is wall-clock, not per-tick)", got, maxBudgetHoldNoticesPerWindow)
		}
	})
}

// TestHandleTick_BudgetHoldNoticeParkedIssue fails if a held issue that is
// also parked in the same tick has its notice suppressed, or if the two
// mechanisms do not both fire.
func TestHandleTick_BudgetHoldNoticeParkedIssue(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-parked", Identifier: "PROJ-NOTICE-PARKED", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{
		budgetExhaustedIDs: map[string]int{issue.ID: 5},
		absenceCounts:      map[string]int{issue.ID: 3},
	}
	state := NewState(60000, 10, nil, AgentTotals{})
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if len(tracker.commentCalls) != 1 {
		t.Errorf("commentCalls = %+v, want exactly 1 (a parked issue still receives the budget-hold notice)", tracker.commentCalls)
	}
	if _, parked := state.Parked[issue.ID]; !parked {
		t.Fatal("Parked missing, want the same tick to also park the issue (absence ceiling reached)")
	}
	if len(store.parkedIssues) != 1 || store.parkedIssues[0].IssueID != issue.ID {
		t.Errorf("store.parkedIssues = %+v, want exactly one park record for %q (the parking write)", store.parkedIssues, issue.ID)
	}
	if state.BudgetHoldNoticesInWindow != 1 {
		t.Errorf("BudgetHoldNoticesInWindow = %d, want 1 (the notice's pacing slot consumed exactly once)", state.BudgetHoldNoticesInWindow)
	}
}

// TestPostBudgetHoldNotice_NilTrackerAdapterWritesNoRow fails if
// [postBudgetHoldNotice] persists a row or records the memory entry when
// the caller's tracker adapter is nil, its safety net for the case both
// call sites already guard against before calling.
func TestPostBudgetHoldNotice_NilTrackerAdapterWritesNoRow(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	store := &stubStore{}
	entry := &BudgetExhaustedEntry{
		Reason: budgetReasonSession, UsedSessions: 4, BudgetSessions: 3, ExhaustedAt: time.Now().UTC(),
	}

	postBudgetHoldNotice(state, budgetHoldNoticeParams{
		IssueID:        "ISS-NIL",
		Entry:          entry,
		Store:          store,
		TrackerAdapter: nil,
		Metrics:        &spyMetrics{},
		Logger:         discardLogger(),
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	if len(store.budgetHoldNotices) != 0 {
		t.Errorf("store.budgetHoldNotices = %+v, want none (a nil tracker adapter writes no row)", store.budgetHoldNotices)
	}
	if _, ok := state.BudgetHoldNoticed["ISS-NIL"]; ok {
		t.Error("BudgetHoldNoticed[ISS-NIL] present, want absent (a nil tracker adapter writes no row)")
	}
}

// TestPostBudgetHoldNotice_UpsertFails fails if a failing
// UpsertBudgetHoldNotice still posts a comment or records the memory
// entry: the comment must not be re-posted every tick for as long as the
// store keeps failing.
func TestPostBudgetHoldNotice_UpsertFails(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	state := NewState(5000, 4, nil, AgentTotals{})
	store := &stubStore{upsertBudgetHoldNoticeErr: fmt.Errorf("disk full")}
	tracker := &mockTrackerAdapter{}
	entry := &BudgetExhaustedEntry{
		Reason: budgetReasonSession, UsedSessions: 4, BudgetSessions: 3, ExhaustedAt: time.Now().UTC(),
	}

	postBudgetHoldNotice(state, budgetHoldNoticeParams{
		IssueID:        "ISS-UPSERT-FAIL",
		Entry:          entry,
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        &spyMetrics{},
		Logger:         logger,
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	if len(tracker.commentCalls) != 0 {
		t.Errorf("commentCalls = %+v, want none (a failed upsert must not post a comment)", tracker.commentCalls)
	}
	if _, ok := state.BudgetHoldNoticed["ISS-UPSERT-FAIL"]; ok {
		t.Error("BudgetHoldNoticed[ISS-UPSERT-FAIL] present, want absent after a failed upsert")
	}
	if !strings.Contains(buf.String(), "failed to persist budget hold notice") {
		t.Errorf("log output missing the persist-failure record; log:\n%s", buf.String())
	}
}

// TestHandleTick_BudgetHoldNoticeQueryErrorWithholdsRelease fails if a
// failed budget query lets the release pass drop a notice. After a
// restart the prior set is empty, so nothing folds forward and every
// candidate looks unheld; releasing on that evidence deletes the durable
// record and lets the next successful tick post a second comment for a
// hold that never ended.
func TestHandleTick_BudgetHoldNoticeQueryErrorWithholdsRelease(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-withheld", Identifier: "PROJ-NOTICE-WITHHELD", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
	state := NewState(60000, 10, nil, AgentTotals{})
	// The state a restart leaves behind: the notice memory is reloaded
	// from the durable rows, while the exhausted set starts empty.
	PopulateBudgetHoldNotices(state, []persistence.BudgetHoldNotice{
		{IssueID: issue.ID, Reason: budgetReasonSession, NoticedAt: "2026-08-25T09:14:03Z"},
	}, discardLogger())
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}
	orch := budgetOrchestrator(state, wm, store, tracker)

	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if len(store.deletedBudgetHoldIDs) != 0 {
		t.Fatalf("deletedBudgetHoldIDs after the query-error tick = %+v, want none (absence from the fresh set is not evidence the hold cleared)", store.deletedBudgetHoldIDs)
	}
	if _, ok := state.BudgetHoldNoticed[issue.ID]; !ok {
		t.Fatalf("BudgetHoldNoticed lost %q on a tick whose budget evidence was never read", issue.ID)
	}

	// The query recovers and the hold is confirmed still in force.
	store.budgetExhaustedErr = nil
	store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 0 {
		t.Errorf("commentCalls = %d, want 0 (the surviving notice record suppresses a duplicate comment)", got)
	}
}

// TestReconcilePasses_DoNotBlockOnInFlightTriage seeds each of the four
// triage-gated reconcile passes with a pending entry carrying an
// in-flight (not-done) triage run and asserts that a full pass over
// state.PendingReactions returns without waiting for the subprocess.
// Each pass's own provider double would block forever if called, so a
// pass that returns promptly and never calls it proves the early
// short-circuit runs ahead of every provider call.
func TestReconcilePasses_DoNotBlockOnInFlightTriage(t *testing.T) {
	t.Parallel()

	const nonBlockingBound = 200 * time.Millisecond

	t.Run("ci", func(t *testing.T) {
		t.Parallel()

		state := stateWithPendingReaction(t, "ISS-NB-CI", "feature/nb", 1)
		rkey := ReactionKey("ISS-NB-CI", ReactionKindCI)
		state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-nb", func() {})
		scm := defaultCISCM()
		ci := &mockCIProvider{}
		params := ciParams(t, &ciReconcileStore{}, ci, nil, scm)

		done := make(chan struct{})
		go func() {
			reconcileCIStatus(state, params, discardLogger(), context.Background(), newCIMetricsSpy())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(nonBlockingBound):
			t.Fatal("reconcileCIStatus blocked on an in-flight triage run")
		}
		if scm.calls != 0 || ci.calls != 0 {
			t.Errorf("provider calls (scm=%d, ci=%d), want 0 while a triage run is in flight", scm.calls, ci.calls)
		}
	})

	t.Run("review", func(t *testing.T) {
		t.Parallel()

		state := stateWithReviewReaction(t, "ISS-NB-REVIEW", 10)
		rkey := ReactionKey("ISS-NB-REVIEW", ReactionKindReview)
		state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-nb", func() {})
		scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
		params := reviewParams(&reviewReconcileStore{}, scm, nil)

		done := make(chan struct{})
		go func() {
			reconcileReviewComments(state, params, discardLogger(), context.Background(), newReviewMetricsSpy())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(nonBlockingBound):
			t.Fatal("reconcileReviewComments blocked on an in-flight triage run")
		}
		if scm.calls != 0 {
			t.Errorf("FetchPendingReviews calls = %d, want 0 while a triage run is in flight", scm.calls)
		}
	})

	t.Run("bot-review", func(t *testing.T) {
		t.Parallel()

		state := stateWithBotReviewReaction(t, "ISS-NB-BOT", 10)
		rkey := ReactionKey("ISS-NB-BOT", ReactionKindBotReview)
		state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-nb", func() {})
		scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
		params := botReviewParams(&reviewReconcileStore{}, scm, nil)

		done := make(chan struct{})
		go func() {
			reconcileBotReviewComments(state, params, discardLogger(), context.Background(), newBotReviewMetricsSpy())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(nonBlockingBound):
			t.Fatal("reconcileBotReviewComments blocked on an in-flight triage run")
		}
		if scm.botCalls != 0 {
			t.Errorf("FetchBotReviewComments calls = %d, want 0 while a triage run is in flight", scm.botCalls)
		}
	})

	t.Run("merge-conflict", func(t *testing.T) {
		t.Parallel()

		state := stateWithMergeConflict(t, "ISS-NB-MC", 10)
		rkey := ReactionKey("ISS-NB-MC", ReactionKindMergeConflict)
		state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-nb", func() {})
		scm := &mergeabilitySCM{}
		params := mergeConflictParams(newStatefulFingerprintStore(), scm, nil)

		done := make(chan struct{})
		go func() {
			reconcileMergeConflicts(state, params, discardLogger(), context.Background(), newMergeConflictMetricsSpy())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(nonBlockingBound):
			t.Fatal("reconcileMergeConflicts blocked on an in-flight triage run")
		}
		if scm.calls != 0 {
			t.Errorf("GetMergeability calls = %d, want 0 while a triage run is in flight", scm.calls)
		}
	})
}
