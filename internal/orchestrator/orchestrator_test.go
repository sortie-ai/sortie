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

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

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

type observerFunc func()

func (f observerFunc) OnStateChange() { f() }

type stubStore struct {
	unsupportedReactionObservationStore

	mu              sync.Mutex
	runHistories    []persistence.RunHistory
	aggregates      []persistence.AggregateMetrics
	sessions        []persistence.SessionMetadata
	savedRetries    []persistence.RetryEntry
	deletedRetryIDs []string
	// budgetExhaustedIDs maps an issue to its run-history session count.
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
	tokenUnaccounted  int

	// tokenIncompleteIDs reports a candidate below the ceiling with one
	// unmeasured session (the "cannot be fully evaluated" outcome).
	tokenIncompleteIDs []string

	// tokenUnaccountedIDs reports a candidate below the ceiling, all
	// sessions measured but an unaccounted turn, so its unmeasured count
	// is zero.
	tokenUnaccountedIDs []string

	// tokenExhaustedUsage overrides the fixed {TotalTokens: 1000} usage
	// for a tokenExhaustedIDs member, for a test controlling every field.
	tokenExhaustedUsage map[string]persistence.IssueTokenUsage

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

	// onAppendRunHistory and onUpsertSessionMetadata, when set, run
	// synchronously after the write so a test can capture single-writer
	// state at the moment a record is persisted rather than by polling.
	onAppendRunHistory      func(persistence.RunHistory)
	onUpsertSessionMetadata func(persistence.SessionMetadata)
}

var _ OrchestratorStore = (*stubStore)(nil)

func (s *stubStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.mu.Lock()
	run.ID = int64(len(s.runHistories) + 1)
	s.runHistories = append(s.runHistories, run)
	hook := s.onAppendRunHistory
	s.mu.Unlock()
	if hook != nil {
		hook(run)
	}
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
	s.sessions = append(s.sessions, m)
	hook := s.onUpsertSessionMetadata
	err := s.upsertSessionMetadataErr
	s.mu.Unlock()
	if hook != nil {
		hook(m)
	}
	return err
}

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
		UnaccountedTurns:   s.tokenUnaccounted,
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

// QueryTokenBudgetUsage reports a tokenExhaustedIDs member at the fixed
// 1000 threshold every test here configures; absent candidates read as
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
			if custom, ok := s.tokenExhaustedUsage[id]; ok {
				usage[id] = custom
				continue
			}
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 1000}
		case slices.Contains(s.tokenIncompleteIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 0, UnmeasuredSessions: 1}
		case slices.Contains(s.tokenUnaccountedIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 500, Sessions: 1, UnaccountedTurns: 1}
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

type stubObserver struct {
	calls atomic.Int64
}

func (o *stubObserver) OnStateChange() { o.calls.Add(1) }

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

			s := NewState(1000, 10, 0, nil, AgentTotals{})
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
		s := NewState(1000, 10, 0, nil, AgentTotals{})
		want := ShouldDispatch(issue, s, active, terminal)
		got := ShouldDispatchWithSets(issue, s, aSet, tSet)
		if got != want {
			t.Errorf("parity mismatch for %q: WithSets=%t, original=%t",
				issue.Identifier, got, want)
		}
	}
}

func TestNewOrchestrator(t *testing.T) {
	t.Parallel()

	t.Run("channel buffer sizes", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, 0, nil, AgentTotals{})
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

		state := NewState(1000, 100, 0, nil, AgentTotals{})
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

		state := NewState(1000, 1, 0, nil, AgentTotals{})
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

		state := NewState(1000, 1, 0, nil, AgentTotals{})
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

// TestNewOrchestrator_SSHPassEnvNoHostsWarnings covers the no-hosts
// warning for ssh_pass_env and ssh_disallow_pass_env: once per named
// key when no hosts are configured, none when the key is absent or a
// host is named.
func TestNewOrchestrator_SSHPassEnvNoHostsWarnings(t *testing.T) {
	t.Parallel()

	t.Run("both keys present without hosts", func(t *testing.T) {
		t.Parallel()

		cfg := config.ServiceConfig{}
		cfg.SetExtensionSection("worker", map[string]any{
			"ssh_pass_env":          []any{"EXAMPLE_TOKEN"},
			"ssh_disallow_pass_env": []any{"GITHUB_TOKEN"},
		})

		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		state := NewState(1000, 1, 0, nil, AgentTotals{})
		NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          logger,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: cfg},
			Store:           &stubStore{},
		})

		output := logs.String()
		if !strings.Contains(output, "ssh_pass_env has no effect without worker.ssh_hosts") {
			t.Errorf("NewOrchestrator() log output = %s, want the ssh_pass_env no-hosts warning", output)
		}
		if !strings.Contains(output, "ssh_disallow_pass_env has no effect without worker.ssh_hosts") {
			t.Errorf("NewOrchestrator() log output = %s, want the ssh_disallow_pass_env no-hosts warning", output)
		}
	})

	t.Run("neither key present without hosts", func(t *testing.T) {
		t.Parallel()

		cfg := config.ServiceConfig{}
		cfg.SetExtensionSection("worker", map[string]any{})

		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		state := NewState(1000, 1, 0, nil, AgentTotals{})
		NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          logger,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: cfg},
			Store:           &stubStore{},
		})

		output := logs.String()
		if strings.Contains(output, "ssh_pass_env has no effect") {
			t.Errorf("NewOrchestrator() log output = %s, want no ssh_pass_env no-hosts warning when the key is absent", output)
		}
		if strings.Contains(output, "ssh_disallow_pass_env has no effect") {
			t.Errorf("NewOrchestrator() log output = %s, want no ssh_disallow_pass_env no-hosts warning when the key is absent", output)
		}
	})

	t.Run("both keys present with hosts configured", func(t *testing.T) {
		t.Parallel()

		cfg := config.ServiceConfig{}
		cfg.SetExtensionSection("worker", map[string]any{
			"ssh_hosts":                      []any{"build01.internal"},
			"max_concurrent_agents_per_host": 2,
			"ssh_pass_env":                   []any{"EXAMPLE_TOKEN"},
			"ssh_disallow_pass_env":          []any{"GITHUB_TOKEN"},
		})

		var logs bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logs, nil))

		state := NewState(1000, 1, 0, nil, AgentTotals{})
		NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          logger,
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: cfg},
			Store:           &stubStore{},
		})

		if output := logs.String(); strings.Contains(output, "has no effect without worker.ssh_hosts") {
			t.Errorf("NewOrchestrator() log output = %s, want no no-hosts warning when worker.ssh_hosts names a host", output)
		}
	})
}

// TestOrchestratorTick_SSHPassEnvFieldsUpdateOnReload asserts a tick
// updates sshPassEnv and sshDisallowPassEnv from the worker block, and
// a reload removing both keys clears them on the next tick.
func TestOrchestratorTick_SSHPassEnvFieldsUpdateOnReload(t *testing.T) {
	t.Parallel()

	cfg := config.ServiceConfig{
		Polling: config.PollingConfig{IntervalMS: 60000},
		Agent:   config.AgentConfig{Kind: "mock", Command: "/usr/bin/agent", MaxConcurrentAgents: 1},
		Tracker: config.TrackerConfig{Kind: "mock", APIKey: "key", ActiveStates: []string{"To Do"}},
	}
	cfg.SetExtensionSection("worker", map[string]any{
		"ssh_hosts":             []any{"build01.internal"},
		"ssh_pass_env":          []any{"EXAMPLE_TOKEN"},
		"ssh_disallow_pass_env": []any{"GITHUB_TOKEN"},
	})

	wm := &stubWorkflowManager{config: cfg}
	regs := passingPreflightRegistries()

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	ctx := context.Background()
	o.handleTick(ctx)

	if !slices.Equal(o.sshPassEnv, []string{"EXAMPLE_TOKEN"}) {
		t.Fatalf("sshPassEnv after first tick = %v, want [EXAMPLE_TOKEN]", o.sshPassEnv)
	}
	if !slices.Equal(o.sshDisallowPassEnv, []string{"GITHUB_TOKEN"}) {
		t.Fatalf("sshDisallowPassEnv after first tick = %v, want [GITHUB_TOKEN]", o.sshDisallowPassEnv)
	}

	reloaded := cfg
	reloaded.SetExtensionSection("worker", map[string]any{
		"ssh_hosts": []any{"build01.internal"},
	})
	wm.setConfig(reloaded)

	o.handleTick(ctx)

	if o.sshPassEnv != nil {
		t.Errorf("sshPassEnv after a reload removing the key = %v, want nil", o.sshPassEnv)
	}
	if o.sshDisallowPassEnv != nil {
		t.Errorf("sshDisallowPassEnv after a reload removing the key = %v, want nil", o.sshDisallowPassEnv)
	}
}

// TestMakeWorkerFn_SSHEnvNamesJoinsRegistryAndOperatorLists asserts the
// SSHEnvNamesFunc closure joins a kind's registry-declared names with
// the operator's listed names, drops every disallowed name from both
// sources, and keeps a name that is both declared and listed only once.
func TestMakeWorkerFn_SSHEnvNamesJoinsRegistryAndOperatorLists(t *testing.T) {
	t.Parallel()

	const kind = "ssh-join-test-kind"

	state := NewState(1000, 5, 0, nil, AgentTotals{})
	tmpDir := t.TempDir()
	cfg := defaultWorkerConfig(tmpDir)
	cfg.Agent.Kind = kind
	tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

	var capturedSSHEnvNames []string
	agent := &mockAgentAdapter{
		startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
			capturedSSHEnvNames = params.SSHEnvNames
			return domain.Session{ID: "sess-1"}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}

	agentRegistry := &stubAgentRegistry{
		getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
		metaFunc: func(k string) (registry.AgentMeta, bool) {
			if k != kind {
				return registry.AgentMeta{}, false
			}
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("K1", "K2")}, true
		},
	}

	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    agent,
		WorkflowManager: wm,
		Store:           &stubStore{},
		PreflightParams: PreflightParams{AgentRegistry: agentRegistry},
	})

	o.sshPassEnv = []string{"L", "K1", "D"}
	o.sshDisallowPassEnv = []string{"K2", "D"}

	issue := workerTestIssue()
	state.Running[issue.ID] = &RunningEntry{
		Identifier: issue.Identifier,
		Issue:      issue,
	}

	wfn := o.makeWorkerFn("", "user@stand-in-host", kind, "", "", nil, registry.UsageArrivalUndeclared)

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

	if !slices.Equal(capturedSSHEnvNames, []string{"K1", "L"}) {
		t.Errorf("StartSessionParams.SSHEnvNames = %v, want [K1 L]", capturedSSHEnvNames)
	}
}

func TestPreflightOK_InitialValue(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 1, 0, nil, AgentTotals{})
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

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	if !o.PreflightOK() {
		t.Fatal("PreflightOK() = false before tick, want true")
	}

	ctx := context.Background()
	o.handleTick(ctx)

	if o.PreflightOK() {
		t.Error("PreflightOK() = true after tick with failing preflight, want false")
	}

	o.preflightParams.ReloadWorkflow = func() error { return nil }
	o.handleTick(ctx)

	if !o.PreflightOK() {
		t.Error("PreflightOK() = false after tick with passing preflight, want true")
	}
}

func TestOrchestratorShutdown(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3 seconds of context cancellation")
	}
}

// TestApplyQueued pins applyQueued's bound: it drains in receive order,
// stops on the first empty receive, and never runs more than cap(ch)
// iterations even when apply refills the channel.
func TestApplyQueued(t *testing.T) {
	t.Parallel()

	t.Run("drains three messages from a channel of capacity four, in order", func(t *testing.T) {
		t.Parallel()

		ch := make(chan int, 4)
		ch <- 1
		ch <- 2
		ch <- 3

		var got []int
		applyQueued(ch, func(v int) { got = append(got, v) })

		want := []int{1, 2, 3}
		if len(got) != len(want) {
			t.Fatalf("applyQueued() called apply %d times, want %d: %v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("got[%d] = %d, want %d", i, got[i], w)
			}
		}
	})

	t.Run("an empty channel returns without calling apply", func(t *testing.T) {
		t.Parallel()

		ch := make(chan int, 4)
		called := false
		applyQueued(ch, func(int) { called = true })

		if called {
			t.Error("apply was called on an empty channel, want no calls")
		}
	})

	t.Run("a channel that refills itself during the drain stops after cap(ch) calls", func(t *testing.T) {
		t.Parallel()

		ch := make(chan int, 4)
		ch <- 1
		ch <- 2
		ch <- 3
		ch <- 4

		var got []int
		applyQueued(ch, func(v int) {
			got = append(got, v)
			ch <- v + 100
		})

		want := []int{1, 2, 3, 4}
		if len(got) != len(want) {
			t.Fatalf("applyQueued() called apply %d times, want exactly %d: %v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i] != w {
				t.Errorf("got[%d] = %d, want %d", i, got[i], w)
			}
		}
		if got := len(ch); got != 4 {
			t.Errorf("len(ch) after the drain = %d, want 4 (the four newly sent messages left queued)", got)
		}
	})
}

// queuedOrderingIssueEntry builds a minimal RunningEntry for the
// queued-before-exit ordering tests: active per budgetTickConfig's
// state list, so the tick reconcile pass this fixture's Run leg admits
// leaves the entry running rather than cancelling it as non-active.
func queuedOrderingIssueEntry(issueID string) *RunningEntry {
	return &RunningEntry{
		Identifier: issueID + "-ident",
		Issue:      domain.Issue{ID: issueID, Identifier: issueID + "-ident", State: "To Do"},
		DispatchID: issueID + "-dispatch",
		StartedAt:  time.Now().UTC(),
		CancelFunc: func() {},
	}
}

// queuedOrderingCrossingEvent returns a token_usage message reporting
// model and a cumulative total of 30, the fixture the queued-before-exit
// ordering tests share.
func queuedOrderingCrossingEvent(issueID, model string) agentEventMsg {
	return agentEventMsg{
		IssueID: issueID,
		Event: domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Model:     model,
			Usage:     domain.TokenUsage{TotalTokens: 30},
		},
	}
}

// TestOrchestrator_QueuedMessagesApplyAheadOfExit pins that a message a
// worker delivered before its WorkerResult is applied to that run's
// entry before the WorkerResult is handled, in both Run and
// drainRunningWorkers. 64 trials per leg, since a random select between
// the two would otherwise pass by chance.
func TestOrchestrator_QueuedMessagesApplyAheadOfExit(t *testing.T) {
	t.Parallel()

	const trials = 64

	t.Run("Run applies a queued agent event and a queued self-review message before the exit", func(t *testing.T) {
		t.Parallel()

		for trial := range trials {
			issueID := fmt.Sprintf("ISSUE-P9-RUN-%d", trial)
			state := NewState(60000, 4, 0, nil, AgentTotals{})
			state.Running[issueID] = queuedOrderingIssueEntry(issueID)

			store := &stubStore{}
			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

			// HandleWorkerExit's own session_metadata write always carries
			// an empty DispatchID (exit.go clears it ahead of the
			// run_history append), while the queued event's earlier
			// throttled incremental write carries the entry's non-empty
			// DispatchID. So the first UpsertSessionMetadata call carrying
			// an empty DispatchID is HandleWorkerExit's own, distinguishable
			// from the queued write regardless of call order.
			type observedRow struct {
				meta               persistence.SessionMetadata
				selfReviewQueueLen int
			}
			captured := make(chan observedRow, 1)
			store.onUpsertSessionMetadata = func(meta persistence.SessionMetadata) {
				if meta.DispatchID != "" {
					return
				}
				captured <- observedRow{meta: meta, selfReviewQueueLen: len(o.selfReviewCh)}
			}

			o.agentEventCh <- queuedOrderingCrossingEvent(issueID, "m")
			o.selfReviewCh <- selfReviewProgressMsg{IssueID: issueID, Message: "self_review_iteration", Iteration: 1, MaxIterations: 2}
			o.workerExitCh <- WorkerResult{IssueID: issueID, Identifier: issueID + "-ident", ExitKind: WorkerExitNormal}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				o.Run(ctx)
				close(done)
			}()

			var row observedRow
			select {
			case row = <-captured:
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatalf("trial %d: timed out waiting for HandleWorkerExit's session_metadata row", trial)
			}

			cancel()
			<-done

			if row.meta.ModelName != "m" {
				t.Fatalf("trial %d: SessionMetadata.ModelName = %q, want %q", trial, row.meta.ModelName, "m")
			}
			if row.meta.TotalTokens != 30 {
				t.Fatalf("trial %d: SessionMetadata.TotalTokens = %d, want 30", trial, row.meta.TotalTokens)
			}
			if row.selfReviewQueueLen != 0 {
				t.Fatalf("trial %d: len(selfReviewCh) = %d at the time the row was upserted, want 0 (applied ahead of the exit)", trial, row.selfReviewQueueLen)
			}
		}
	})

	t.Run("drainRunningWorkers applies a queued agent event and a queued self-review message before the exit", func(t *testing.T) {
		t.Parallel()

		for trial := range trials {
			issueID := fmt.Sprintf("ISSUE-P9-DRAIN-%d", trial)
			state := NewState(60000, 4, 0, nil, AgentTotals{})
			state.Running[issueID] = queuedOrderingIssueEntry(issueID)

			store := &stubStore{}
			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

			// The hooks run on the drain goroutine, which has returned before
			// the test reads selfReviewQueueLen. HandleWorkerExit's own
			// session_metadata write always carries an empty DispatchID
			// (exit.go clears it ahead of the run_history append), which
			// distinguishes it from the queued event's earlier throttled
			// incremental write, carrying the entry's non-empty DispatchID.
			selfReviewQueueLen := -1
			store.onUpsertSessionMetadata = func(meta persistence.SessionMetadata) {
				if meta.DispatchID != "" {
					return
				}
				selfReviewQueueLen = len(o.selfReviewCh)
			}

			o.agentEventCh <- queuedOrderingCrossingEvent(issueID, "m")
			o.selfReviewCh <- selfReviewProgressMsg{IssueID: issueID, Message: "self_review_iteration", Iteration: 1, MaxIterations: 2}
			o.workerExitCh <- WorkerResult{IssueID: issueID, Identifier: issueID + "-ident", ExitKind: WorkerExitNormal}

			done := make(chan struct{})
			go func() {
				o.drainRunningWorkers()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("trial %d: drainRunningWorkers did not return within 10 seconds", trial)
			}

			// The queued agent event's own throttled incremental write may
			// land before HandleWorkerExit's unconditional exit-time write,
			// so the property under test is pinned on the last row: the one
			// HandleWorkerExit itself upserted.
			writes := store.sessionWrites()
			if len(writes) == 0 {
				t.Fatalf("trial %d: UpsertSessionMetadata was never called, want at least 1", trial)
			}
			exitWrite := writes[len(writes)-1]
			if exitWrite.ModelName != "m" {
				t.Fatalf("trial %d: SessionMetadata.ModelName = %q, want %q", trial, exitWrite.ModelName, "m")
			}
			if exitWrite.TotalTokens != 30 {
				t.Fatalf("trial %d: SessionMetadata.TotalTokens = %d, want 30", trial, exitWrite.TotalTokens)
			}
			if selfReviewQueueLen != 0 {
				t.Fatalf("trial %d: len(selfReviewCh) = %d at the time the row was upserted, want 0 (applied ahead of the exit)", trial, selfReviewQueueLen)
			}
		}
	})
}

// budgetCeilingExemptionFixture builds an *Orchestrator, a *stubStore,
// a *spyMetrics, and a log buffer for the ceiling-exemption tests: a
// per-issue token ceiling of 100 with every issue's completed-session
// sum reported as 90, and a workflow config with no handoff state and
// an active-state list that never matches an entry's own (empty)
// Issue.State, so a normal exit's disposition falls through to the
// non-active default arm rather than scheduling a real retry timer.
func budgetCeilingExemptionFixture(t *testing.T) (o *Orchestrator, store *stubStore, spy *spyMetrics, logBuf *bytes.Buffer) {
	t.Helper()

	cfg := config.ServiceConfig{Tracker: config.TrackerConfig{ActiveStates: []string{"Done"}}}
	store = &stubStore{tokenSum: 90}
	spy = &spyMetrics{}
	logBuf = &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	state := NewState(60000, 4, 100, nil, AgentTotals{})

	o = &Orchestrator{
		state:           state,
		logger:          logger,
		metrics:         spy,
		store:           store,
		workflowManager: &stubWorkflowManager{config: cfg},
		agentEventCh:    make(chan agentEventMsg, 8),
		workerExitCh:    make(chan WorkerResult, 8),
		selfReviewCh:    make(chan selfReviewProgressMsg, 8),
		retryTimerCh:    make(chan string, 8),
		hostPool:        NewHostPool(nil, 0),
	}
	return o, store, spy, logBuf
}

// budgetCeilingCrossingEvent returns a token_usage message reporting a
// cumulative total of 30, which, added to the 90 the fixture's store
// reports as already completed, crosses the fixture's ceiling of 100.
func budgetCeilingCrossingEvent(issueID string) agentEventMsg {
	return agentEventMsg{
		IssueID: issueID,
		Event: domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     domain.TokenUsage{TotalTokens: 30},
		},
	}
}

// TestHandleWorkerExit_NoBudgetStopForExitingRun pins the ceiling
// exemption: a crossing figure applied ahead of its own run's
// WorkerResult is never evaluated against the in-flight ceiling, so
// that run's exit is recorded on its own terms.
func TestHandleWorkerExit_NoBudgetStopForExitingRun(t *testing.T) {
	t.Parallel()

	t.Run("a normal exit records succeeded with no budget stop", func(t *testing.T) {
		t.Parallel()

		o, store, spy, logBuf := budgetCeilingExemptionFixture(t)
		var cancelCalls atomic.Int32
		o.state.Running["X"] = &RunningEntry{
			Identifier:             "X-ident",
			Issue:                  domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:              time.Now().UTC(),
			IssueTokensCompleted:   90,
			TokenCeilingCancelFunc: func() { cancelCalls.Add(1) },
		}
		o.state.Claimed["X"] = struct{}{}
		o.agentEventCh <- budgetCeilingCrossingEvent("X")

		o.handleWorkerExit(context.Background(), WorkerResult{
			IssueID:       "X",
			Identifier:    "X-ident",
			ExitKind:      WorkerExitNormal,
			Usage:         domain.TokenUsage{TotalTokens: 30},
			UsageMeasured: true,
		})

		if len(spy.runsStoppedByBudget) != 0 {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want none", spy.runsStoppedByBudget)
		}
		if strings.Contains(logBuf.String(), "run stopped by token ceiling") {
			t.Error(`log contains "run stopped by token ceiling", want no such record`)
		}
		if cancelCalls.Load() != 0 {
			t.Errorf("TokenCeilingCancelFunc called %d times, want 0", cancelCalls.Load())
		}
		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if run.Status != "succeeded" {
			t.Errorf("RunHistory.Status = %q, want %q", run.Status, "succeeded")
		}
		if run.TotalTokens != 30 {
			t.Errorf("RunHistory.TotalTokens = %d, want 30", run.TotalTokens)
		}
	})

	t.Run("a cancelled exit records cancelled with no budget stop", func(t *testing.T) {
		t.Parallel()

		o, store, spy, logBuf := budgetCeilingExemptionFixture(t)
		var cancelCalls atomic.Int32
		o.state.Running["X"] = &RunningEntry{
			Identifier:             "X-ident",
			Issue:                  domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:              time.Now().UTC(),
			IssueTokensCompleted:   90,
			TokenCeilingCancelFunc: func() { cancelCalls.Add(1) },
		}
		o.state.Claimed["X"] = struct{}{}
		o.agentEventCh <- budgetCeilingCrossingEvent("X")
		stallErr := errors.New("stall timeout")

		o.handleWorkerExit(context.Background(), WorkerResult{
			IssueID:    "X",
			Identifier: "X-ident",
			ExitKind:   WorkerExitCancelled,
			Error:      stallErr,
		})

		if len(spy.runsStoppedByBudget) != 0 {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want none", spy.runsStoppedByBudget)
		}
		if strings.Contains(logBuf.String(), "run stopped by token ceiling") {
			t.Error(`log contains "run stopped by token ceiling", want no such record`)
		}
		if cancelCalls.Load() != 0 {
			t.Errorf("TokenCeilingCancelFunc called %d times, want 0", cancelCalls.Load())
		}
		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if run.Status != "cancelled" {
			t.Errorf("RunHistory.Status = %q, want %q (not budget_stopped)", run.Status, "cancelled")
		}
		if run.Error == nil || !strings.Contains(*run.Error, stallErr.Error()) {
			t.Errorf("RunHistory.Error = %v, want it to contain %q", run.Error, stallErr.Error())
		}
	})

	t.Run("a queued figure for an issue whose exit is already waiting records no budget stop", func(t *testing.T) {
		t.Parallel()

		o, store, spy, logBuf := budgetCeilingExemptionFixture(t)
		var yCancels atomic.Int32
		o.state.Running["X"] = &RunningEntry{
			Identifier: "X-ident",
			Issue:      domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:  time.Now().UTC(),
		}
		o.state.Running["Y"] = &RunningEntry{
			Identifier:             "Y-ident",
			Issue:                  domain.Issue{ID: "Y", Identifier: "Y-ident"},
			StartedAt:              time.Now().UTC(),
			IssueTokensCompleted:   90,
			TokenCeilingCancelFunc: func() { yCancels.Add(1) },
		}
		o.state.Claimed["X"] = struct{}{}
		o.state.Claimed["Y"] = struct{}{}
		o.agentEventCh <- budgetCeilingCrossingEvent("Y")
		o.workerExitCh <- WorkerResult{
			IssueID:       "Y",
			Identifier:    "Y-ident",
			ExitKind:      WorkerExitNormal,
			Usage:         domain.TokenUsage{TotalTokens: 30},
			UsageMeasured: true,
		}

		o.handleWorkerExit(context.Background(), WorkerResult{
			IssueID:    "X",
			Identifier: "X-ident",
			ExitKind:   WorkerExitNormal,
		})

		if len(spy.runsStoppedByBudget) != 0 {
			t.Errorf("IncRunsStoppedByBudget calls = %v, want none", spy.runsStoppedByBudget)
		}
		if strings.Contains(logBuf.String(), "run stopped by token ceiling") {
			t.Error(`log contains "run stopped by token ceiling", want no such record`)
		}
		if yCancels.Load() != 0 {
			t.Errorf("Y's TokenCeilingCancelFunc called %d times, want 0", yCancels.Load())
		}
		if len(store.runHistories) != 2 {
			t.Fatalf("AppendRunHistory called %d times, want 2", len(store.runHistories))
		}
		if run := store.runHistories[1]; run.IssueID != "Y" || run.Status != "succeeded" {
			t.Errorf("second RunHistory = {IssueID: %q, Status: %q}, want {IssueID: %q, Status: %q}", run.IssueID, run.Status, "Y", "succeeded")
		}
	})
}

// TestApplyQueuedAheadOfExit_EnforcesCeilingForOtherIssues pins the
// other half of the exemption: a queued message for any issue other
// than the one whose exit is being handled still gets the ceiling
// evaluated against it, so that issue can still be stopped in flight.
func TestApplyQueuedAheadOfExit_EnforcesCeilingForOtherIssues(t *testing.T) {
	t.Parallel()

	t.Run("a direct call stops Y and leaves X exempt", func(t *testing.T) {
		t.Parallel()

		o, _, spy, logBuf := budgetCeilingExemptionFixture(t)
		var xCancels, yCancels atomic.Int32
		o.state.Running["X"] = &RunningEntry{
			Identifier:             "X-ident",
			Issue:                  domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:              time.Now().UTC(),
			IssueTokensCompleted:   90,
			TokenCeilingCancelFunc: func() { xCancels.Add(1) },
		}
		o.state.Running["Y"] = &RunningEntry{
			Identifier:             "Y-ident",
			Issue:                  domain.Issue{ID: "Y", Identifier: "Y-ident"},
			StartedAt:              time.Now().UTC(),
			IssueTokensCompleted:   90,
			TokenCeilingCancelFunc: func() { yCancels.Add(1) },
		}
		o.agentEventCh <- budgetCeilingCrossingEvent("Y")
		o.agentEventCh <- budgetCeilingCrossingEvent("X")

		o.applyQueuedAheadOfExit(context.Background(), map[string]struct{}{"X": {}})

		if got := len(spy.runsStoppedByBudget); got != 0 {
			t.Fatalf("IncRunsStoppedByBudget called %d times, want 0 (the stop request alone reports nothing): %v", got, spy.runsStoppedByBudget)
		}
		if strings.Contains(logBuf.String(), "run stopped by token ceiling") {
			t.Fatalf(`log contains "run stopped by token ceiling", want no such record: %s`, logBuf.String())
		}
		if yCancels.Load() != 1 {
			t.Errorf("Y's TokenCeilingCancelFunc called %d times, want 1", yCancels.Load())
		}
		if xCancels.Load() != 0 {
			t.Errorf("X's TokenCeilingCancelFunc called %d times, want 0", xCancels.Load())
		}
		if o.state.Running["Y"].TokenCeilingStopRequest == nil {
			t.Error("Y's TokenCeilingStopRequest = nil, want non-nil")
		}
		if o.state.Running["X"].TokenCeilingStopRequest != nil {
			t.Error("X's TokenCeilingStopRequest is non-nil, want nil")
		}
	})

	t.Run("Run enforces the ceiling for another issue's queued message across 64 trials", func(t *testing.T) {
		t.Parallel()

		const trials = 64
		for trial := range trials {
			issueX := fmt.Sprintf("ISSUE-P13-X-%d", trial)
			issueY := fmt.Sprintf("ISSUE-P13-Y-%d", trial)

			wm := budgetTickConfig(0)
			wm.config.Agent.MaxTokens = 100

			store := &stubStore{tokenSum: 90}
			spy := &spyMetrics{}
			var yCancels atomic.Int32
			state := NewState(60000, 4, 100, nil, AgentTotals{})
			state.Running[issueX] = queuedOrderingIssueEntry(issueX)
			state.Running[issueX].IssueTokensCompleted = 90
			state.Running[issueY] = queuedOrderingIssueEntry(issueY)
			state.Running[issueY].IssueTokensCompleted = 90
			state.Running[issueY].TokenCeilingCancelFunc = func() { yCancels.Add(1) }

			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestratorWithMetrics(state, wm, store, tracker, discardLogger(), spy)

			// X's row is the trial's first run_history row, appended after
			// applyQueuedAheadOfExit has already applied (and, for Y,
			// ceiling-enforced) every message queued ahead of X's exit. The
			// hook observes Y's TokenCeilingCancelFunc and the budget-stop counter at
			// that moment, from the loop goroutine, before Y's own exit or
			// the shutdown drain can call Y's TokenCeilingCancelFunc again. The send
			// never blocks, so Y's later row cannot stall the loop.
			type observedRow struct {
				yCancels        int32
				stoppedByBudget int
			}
			captured := make(chan observedRow, 1)
			store.onAppendRunHistory = func(persistence.RunHistory) {
				select {
				case captured <- observedRow{
					yCancels:        yCancels.Load(),
					stoppedByBudget: len(spy.runsStoppedByBudget),
				}:
				default:
				}
			}

			o.agentEventCh <- budgetCeilingCrossingEvent(issueY)
			o.workerExitCh <- WorkerResult{IssueID: issueX, Identifier: issueX + "-ident", ExitKind: WorkerExitNormal}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				o.Run(ctx)
				close(done)
			}()

			var row observedRow
			select {
			case row = <-captured:
			case <-time.After(10 * time.Second):
				cancel()
				<-done
				t.Fatalf("trial %d: timed out waiting for X's run_history row", trial)
			}

			// Y exits too, so the shutdown drain finds no running entry to
			// wait for.
			o.workerExitCh <- WorkerResult{IssueID: issueY, Identifier: issueY + "-ident", ExitKind: WorkerExitNormal}
			cancel()
			<-done

			if row.yCancels != 1 {
				t.Fatalf("trial %d: Y's TokenCeilingCancelFunc called %d times by the time X's row was upserted, want 1", trial, row.yCancels)
			}
			// The counter fires only once Y's own exit confirms the ceiling ended it.
			if row.stoppedByBudget != 0 {
				t.Fatalf("trial %d: IncRunsStoppedByBudget called %d times by the time X's row was upserted, want 0", trial, row.stoppedByBudget)
			}
		}
	})
}

func TestApplyTurnStarted(t *testing.T) {
	t.Parallel()

	t.Run("sets TurnCount for a present entry", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 4, 0, nil, AgentTotals{})
		state.Running["A"] = &RunningEntry{Identifier: "A-ident", TurnCount: 2}
		o := &Orchestrator{state: state}

		o.applyTurnStarted(turnStartedMsg{IssueID: "A", TurnsStarted: 7})

		if got := state.Running["A"].TurnCount; got != 7 {
			t.Errorf("TurnCount = %d, want 7", got)
		}
	})

	t.Run("is a no-op for an absent entry", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 4, 0, nil, AgentTotals{})
		o := &Orchestrator{state: state}

		o.applyTurnStarted(turnStartedMsg{IssueID: "missing", TurnsStarted: 3})

		if len(state.Running) != 0 {
			t.Errorf("state.Running = %v, want empty", state.Running)
		}
	})
}

// TestOrchestrator_TurnStartedAppliesAheadOfExit pins that a turn-started
// message queued before a worker's exit reaches that run's entry, never
// a later run of the same issue.
func TestOrchestrator_TurnStartedAppliesAheadOfExit(t *testing.T) {
	t.Parallel()

	t.Run("applyQueuedAheadOfExit (Orchestrator.Run's workerExitCh path)", func(t *testing.T) {
		t.Parallel()

		issueID := "ISSUE-TURN-ORDER-RUN"
		state := NewState(60000, 4, 0, nil, AgentTotals{})
		state.Running[issueID] = queuedOrderingIssueEntry(issueID)
		entry := state.Running[issueID]

		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

		o.turnStartedCh <- turnStartedMsg{IssueID: issueID, TurnsStarted: 5}

		o.handleWorkerExit(context.Background(), WorkerResult{
			IssueID:      issueID,
			Identifier:   entry.Identifier,
			ExitKind:     WorkerExitNormal,
			TurnsStarted: 5,
		})

		if _, stillRunning := state.Running[issueID]; stillRunning {
			t.Fatal("state.Running still holds the exited issue, want it removed")
		}
		if entry.TurnCount != 5 {
			t.Errorf("entry.TurnCount = %d, want 5 (applied before HandleWorkerExit removed the entry)", entry.TurnCount)
		}

		state.Running[issueID] = queuedOrderingIssueEntry(issueID)
		o.applyQueuedAheadOfExit(context.Background(), map[string]struct{}{})
		if got := state.Running[issueID].TurnCount; got != 0 {
			t.Errorf("a later entry's TurnCount = %d, want 0 (no stray message left queued)", got)
		}
	})

	t.Run("workerExitCh case of drainRunningWorkers", func(t *testing.T) {
		t.Parallel()

		// The drain's own turnStartedCh case wins the random select about
		// half the time, so a single trial cannot catch a missing drain.
		const trials = 64

		for trial := range trials {
			issueID := fmt.Sprintf("ISSUE-TURN-ORDER-DRAIN-%d", trial)
			state := NewState(60000, 4, 0, nil, AgentTotals{})
			state.Running[issueID] = queuedOrderingIssueEntry(issueID)
			entry := state.Running[issueID]

			store := &stubStore{}
			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)

			o.turnStartedCh <- turnStartedMsg{IssueID: issueID, TurnsStarted: 9}
			o.workerExitCh <- WorkerResult{
				IssueID:      issueID,
				Identifier:   entry.Identifier,
				ExitKind:     WorkerExitNormal,
				TurnsStarted: 9,
			}

			done := make(chan struct{})
			go func() {
				o.drainRunningWorkers()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("trial %d: drainRunningWorkers did not return within 10 seconds", trial)
			}

			if entry.TurnCount != 9 {
				t.Fatalf("trial %d: entry.TurnCount = %d, want 9 (applied before the drain's HandleWorkerExit removed the entry)", trial, entry.TurnCount)
			}
		}
	})
}

// waitForTurnCount polls snapshots until issueID reports want, failing on
// a larger value or at the deadline. Run applies turn-started messages
// asynchronously to snapshot requests, so a single read can race ahead.
func waitForTurnCount(t *testing.T, snapshot func() (RuntimeSnapshotResult, error), issueID string, want int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := snapshot()
		if err != nil {
			t.Fatalf("snapshot(): %v", err)
		}
		idx := slices.IndexFunc(snap.Running, func(e SnapshotRunningEntry) bool { return e.IssueID == issueID })
		if idx < 0 {
			t.Fatalf("snapshot has no running entry for %q", issueID)
		}
		got := snap.Running[idx].TurnCount
		if got == want {
			return
		}
		if got > want {
			t.Fatalf("TurnCount = %d, want %d (observed ahead of releasing this turn)", got, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("TurnCount did not reach %d within the deadline, last observed %d", want, got)
		}
	}
}

// TestOrchestrator_TurnCountTracksWorkerTally pins that a live snapshot
// reports TurnCount k while turn k runs, whatever the session_started
// pattern.
func TestOrchestrator_TurnCountTracksWorkerTally(t *testing.T) {
	t.Parallel()

	const wantTurns = 3

	patterns := []struct {
		name  string
		emits func(int) bool
	}{
		{"session_started every turn", emitsSessionStartedEveryTurn},
		{"session_started first turn only", emitsSessionStartedFirstTurnOnly},
		{"session_started never", emitsSessionStartedNever},
	}

	for _, pattern := range patterns {
		t.Run(pattern.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			cfg := defaultWorkerConfig(tmpDir)
			cfg.Agent.MaxTurns = wantTurns
			// Keeps a second tick from reconciling mid-test.
			cfg.Polling.IntervalMS = 3_600_000
			wm := &stubWorkflowManager{config: cfg, template: mustParseTemplate(t, "{{ .issue.title }}")}

			issue := workerTestIssue()
			state := NewState(cfg.Polling.IntervalMS, 4, 0, nil, AgentTotals{})
			state.Running[issue.ID] = &RunningEntry{
				Identifier: issue.Identifier,
				Issue:      issue,
				DispatchID: issue.ID + "-dispatch",
				StartedAt:  time.Now().UTC(),
				CancelFunc: func() {},
			}

			adapter := &turnEmissionAdapter{
				emits:   pattern.emits,
				entered: make(chan int),
				release: make(chan struct{}),
			}

			regs := passingPreflightRegistries()
			regs.ReloadWorkflow = func() error { return nil }
			regs.ConfigFunc = wm.Config

			o := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  &mockTrackerAdapter{},
				AgentAdapter:    adapter,
				WorkflowManager: wm,
				Store:           &stubStore{},
				PreflightParams: regs,
			})

			// Built before Run, whose first tick writes
			// o.sshStrictHostKeyChecking concurrently.
			wfn := o.makeWorkerFn("", "", "", "", "", nil, registry.UsageArrivalUndeclared)

			ctx, cancel := context.WithCancel(context.Background())
			runDone := make(chan struct{})
			go func() {
				o.Run(ctx)
				close(runDone)
			}()
			t.Cleanup(func() {
				cancel()
				<-runDone
			})

			workerDone := make(chan struct{})
			go func() {
				wfn(ctx, issue, nil)
				close(workerDone)
			}()

			snapshot := o.SnapshotFunc()
			for k := 1; k <= wantTurns; k++ {
				turn := <-adapter.entered
				if turn != k {
					t.Fatalf("adapter entered turn %d, want %d", turn, k)
				}
				waitForTurnCount(t, snapshot, issue.ID, k)
				adapter.release <- struct{}{}
			}

			select {
			case <-workerDone:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not exit within 10 seconds")
			}
		})
	}
}

func TestMakeWorkerFn(t *testing.T) {
	t.Parallel()

	t.Run("OnEvent delivers to agentEventCh non-blocking", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, 0, nil, AgentTotals{})

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, registry.UsageArrivalUndeclared)

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

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

		state := NewState(1000, 5, 0, nil, AgentTotals{})
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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, registry.UsageArrivalUndeclared)

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

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

		state := NewState(1000, 5, 0, nil, AgentTotals{})
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

		wfn := o.makeWorkerFn("resume-sess-42", "", "", "", "", nil, registry.UsageArrivalUndeclared)

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

		state := NewState(1000, 5, 0, nil, AgentTotals{})
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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, registry.UsageArrivalUndeclared)

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

	t.Run("DispatchID from context reaches the tool server env", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, 0, nil, AgentTotals{})
		tmpDir := t.TempDir()
		cfg := defaultWorkerConfig(tmpDir)
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

		var capturedMCPConfigPath atomic.Value
		agent := &mockAgentAdapter{
			startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
				capturedMCPConfigPath.Store(params.MCPConfigPath)
				return domain.Session{ID: "sess-1"}, nil
			},
		}

		wm := &stubWorkflowManager{config: cfg, template: tmpl, absPath: "/fake/WORKFLOW.md"}

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, registry.UsageArrivalUndeclared)

		exitDone := make(chan struct{})
		go func() {
			wfn(withDispatchID(context.Background(), "dispatch-mkw-123"), issue, nil)
			close(exitDone)
		}()

		select {
		case <-exitDone:
		case <-time.After(10 * time.Second):
			t.Fatal("worker did not exit within 10 seconds")
		}

		mcpConfigPath, _ := capturedMCPConfigPath.Load().(string)
		if mcpConfigPath == "" {
			t.Fatal("StartSessionParams.MCPConfigPath is empty, want non-empty")
		}

		workspacePath := filepath.Dir(filepath.Dir(mcpConfigPath))
		entry := sortieEntry(t, readMCPConfig(t, workspacePath))
		env, ok := entry["env"].(map[string]any)
		if !ok {
			t.Fatal("env is not an object")
		}
		got, _ := env["SORTIE_DISPATCH_ID"].(string)
		if got != "dispatch-mkw-123" {
			t.Errorf("env[%q] = %q, want %q (WorkerDeps.DispatchID from dispatchIDFromContext)", "SORTIE_DISPATCH_ID", got, "dispatch-mkw-123")
		}
	})
}

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
			state := NewState(1000, 5, 0, nil, AgentTotals{})
			o := NewOrchestrator(OrchestratorParams{
				State:           state,
				Logger:          discardLogger(),
				TrackerAdapter:  tracker,
				AgentAdapter:    &mockAgentAdapter{},
				WorkflowManager: &stubWorkflowManager{config: cfg, template: tmpl},
				Store:           &stubStore{},
			})

			issue := workerTestIssue()
			wfn := o.makeWorkerFn("", "", "", "", tt.reactionKind, nil, registry.UsageArrivalUndeclared)

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

// TestMakeWorkerFn_PostureMappingSharedWithHandleWorkerExit pins that
// HandleWorkerExit and makeWorkerFn derive posture from the same
// dispatchPostureForReactionKind mapping, so they can never disagree.
// Observed via the continuation-retry branch, which fires only when
// DrivesIssueState is true.
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
			state := NewState(1000, 5, 0, nil, AgentTotals{})
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

func TestOnRetryFire(t *testing.T) {
	t.Parallel()

	t.Run("delivers issue ID to retryTimerCh", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 5, 0, nil, AgentTotals{})
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

		state := NewState(1000, 1, 0, nil, AgentTotals{})

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

		bufSize := cap(o.retryTimerCh)
		for i := range bufSize {
			o.retryTimerCh <- "fill-" + string(rune('A'+i))
		}

		o.onRetryFire("OVERFLOW")

		logOutput := buf.String()
		if logOutput == "" {
			t.Error("expected log output when channel full, got empty")
		}

		if len(o.retryTimerCh) != bufSize {
			t.Errorf("retryTimerCh length = %d, want %d", len(o.retryTimerCh), bufSize)
		}
	})
}

func TestNotifyObservers(t *testing.T) {
	t.Parallel()

	obs1 := &stubObserver{}
	obs2 := &stubObserver{}

	state := NewState(1000, 1, 0, nil, AgentTotals{})
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

func TestOrchestratorDynamicConfig(t *testing.T) {
	t.Parallel()

	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "To Do"
			}
			return result, nil
		},
	}

	candidateTracker := &candidateTrackerAdapter{
		mockTrackerAdapter: tracker,
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, nil
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

	state := NewState(1000, 2, 0, nil, AgentTotals{})
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

	o.handleTick(ctx)
	if state.MaxConcurrentAgents != 2 {
		t.Errorf("after first tick MaxConcurrentAgents = %d, want 2", state.MaxConcurrentAgents)
	}

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

	if got := obs.calls.Load(); got != 2 {
		t.Errorf("observer calls = %d, want 2", got)
	}
}

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

func TestOrchestratorPreflightFailure(t *testing.T) {
	t.Parallel()

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

	state := NewState(1000, 5, 0, nil, AgentTotals{})
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

	if fetchCalled.Load() {
		t.Error("FetchCandidateIssues was called despite preflight failure")
	}

	if len(state.Running) != 0 {
		t.Errorf("Running count = %d, want 0", len(state.Running))
	}

	if got := obs.calls.Load(); got != 1 {
		t.Errorf("observer calls = %d, want 1", got)
	}
}

var errPreflightFailed = errorString("preflight: workflow reload failed")

type errorString string

func (e errorString) Error() string { return string(e) }

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
		State:  NewState(1000, 5, 0, nil, AgentTotals{}),
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
		State:  NewState(1000, 5, 0, nil, AgentTotals{}),
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
		State:           NewState(1000, 5, 0, nil, AgentTotals{}),
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
		State:  NewState(1000, 5, 0, nil, AgentTotals{}),
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

func lifecycleIssues() []domain.Issue {
	return []domain.Issue{
		{ID: "id-1", Identifier: "TEST-1", Title: "First", State: "To Do"},
		{ID: "id-2", Identifier: "TEST-2", Title: "Second", State: "To Do"},
		{ID: "id-3", Identifier: "TEST-3", Title: "Third", State: "To Do"},
	}
}

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
					// "Done" is non-active, so the turn loop breaks after one.
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

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

	// state belongs to the event loop, so progress is read from the
	// mutex-protected store rather than from state directly.
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

	for _, issue := range lifecycleIssues() {
		if _, ok := state.Completed[issue.ID]; !ok {
			t.Errorf("issue %s not in Completed set", issue.Identifier)
		}
	}

	if len(state.Running) != 0 {
		t.Errorf("Running count = %d, want 0", len(state.Running))
	}

	store.mu.Lock()
	historyCount := len(store.runHistories)
	store.mu.Unlock()
	if historyCount != 3 {
		t.Errorf("run history count = %d, want 3", historyCount)
	}

	if got := obs.calls.Load(); got < 1 {
		t.Errorf("observer calls = %d, want >= 1", got)
	}
}

// TestOrchestratorLifecycle_TokenCeilingStopsRunMidTurn asserts the
// in-flight ceiling stops a real dispatched run mid-turn, before the
// adapter's max_turns is reached.
func TestOrchestratorLifecycle_TokenCeilingStopsRunMidTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)
	cfg.Agent.MaxTurns = 5
	cfg.Agent.MaxTokens = 150
	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")

	issue := domain.Issue{ID: "id-ceiling", Identifier: "TEST-CEIL", Title: "Ceiling", State: "To Do"}

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
			return []domain.Issue{issue}, nil
		},
	}

	var turnNumber atomic.Int32
	agent := &mockAgentAdapter{
		runTurnFn: func(ctx context.Context, sess domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
			n := turnNumber.Add(1)
			if n == 1 {
				params.OnEvent(domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 50, InputTokens: 30, OutputTokens: 20}})
				return domain.TurnResult{SessionID: sess.ID, ExitReason: domain.EventTurnCompleted}, nil
			}
			// This session's cumulative spend (200) crosses the
			// configured ceiling (150) partway through the second
			// turn; the run must be cancelled before this turn, or any
			// later one, completes.
			params.OnEvent(domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 200, InputTokens: 120, OutputTokens: 80}})
			<-ctx.Done()
			return domain.TurnResult{}, ctx.Err()
		},
	}

	agentRegistry := &stubAgentRegistry{
		getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
		metaFunc: func(string) (registry.AgentMeta, bool) {
			return registry.AgentMeta{UsageArrival: registry.UsageArrivalIncremental, UsageAttribution: registry.UsageAttributionPerModel}, true
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl}
	store := &stubStore{}
	regs := passingPreflightRegistries()
	spy := &spyMetrics{}

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, cfg.Agent.MaxTokens, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    agent,
		WorkflowManager: wm,
		Store:           store,
		Metrics:         spy,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   agentRegistry,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timed out waiting for a run_history record")
		default:
		}
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The run already exited through the normal event-loop path above;
	// nothing is left in state.Running for a later shutdown-time
	// drainRunningWorkers pass to act on, so drainRunningWorkers is
	// provably not what recorded this stop.
	if _, running := state.Running[issue.ID]; running {
		t.Error("Running[id-ceiling] still present after the run history was recorded")
	}

	cancel()
	<-done

	store.mu.Lock()
	runs := append([]persistence.RunHistory(nil), store.runHistories...)
	store.mu.Unlock()
	if len(runs) != 1 {
		t.Fatalf("run history count = %d, want 1", len(runs))
	}
	run := runs[0]

	if run.Status != "budget_stopped" {
		t.Errorf("RunHistory.Status = %q, want %q", run.Status, "budget_stopped")
	}
	if run.Error == nil || !strings.Contains(*run.Error, "200") || !strings.Contains(*run.Error, "150") {
		t.Errorf("RunHistory.Error = %v, want it to name used tokens 200 and budgeted tokens 150", run.Error)
	}
	if run.TurnsCompleted != 1 {
		t.Errorf("RunHistory.TurnsCompleted = %d, want 1 (stopped mid-turn-2, before max_turns=%d)", run.TurnsCompleted, cfg.Agent.MaxTurns)
	}
	if got := turnNumber.Load(); got != 2 {
		t.Errorf("turns started = %d, want 2 (the ceiling fired during the second turn, not the first)", got)
	}
	if len(spy.runsStoppedByBudget) != 1 || spy.runsStoppedByBudget[0] != budgetReasonToken {
		t.Errorf("IncRunsStoppedByBudget calls = %v, want exactly one %q", spy.runsStoppedByBudget, budgetReasonToken)
	}
}

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

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

	if _, ok := state.Completed["id-ok"]; !ok {
		t.Error("issue id-ok not in Completed set")
	}

	store.mu.Lock()
	retriesSaved := len(store.savedRetries)
	store.mu.Unlock()
	if retriesSaved < 1 {
		t.Errorf("saved retries = %d, want >= 1", retriesSaved)
	}

	if _, claimed := state.Claimed["id-fail"]; !claimed {
		t.Error("issue id-fail not in Claimed set after retry scheduling")
	}
}

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

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, cfg.Agent.MaxConcurrentByState, AgentTotals{})
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

	if _, ok := state.Completed["td-1"]; !ok {
		t.Error("issue TD-1 not in Completed set, per-state exhaustion blocked cross-state dispatch")
	}

	store.mu.Lock()
	firstThreeIDs := make(map[string]bool)
	for i := range min(3, len(store.runHistories)) {
		firstThreeIDs[store.runHistories[i].IssueID] = true
	}
	store.mu.Unlock()

	if firstThreeIDs["ip-3"] && !firstThreeIDs["td-1"] {
		t.Error("ip-3 was dispatched before td-1, per-state limit was not enforced")
	}
}

func TestOrchestratorDynamicConfigReload(t *testing.T) {
	t.Parallel()

	t.Run("polling_interval_change", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.Polling.IntervalMS = 60000

		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		obs := &stubObserver{}
		state := NewState(60000, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

	t.Run("concurrency_limit_change", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 1
		cfg.Agent.MaxTurns = 1
		tmpl := mustParseTemplate(t, "do {{ .issue.identifier }}")

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
		state := NewState(cfg.Polling.IntervalMS, 1, 0, nil, AgentTotals{})
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

		cfg.Agent.MaxConcurrentAgents = 3
		wm.setConfig(cfg)

		o.handleTick(ctx)

		if state.MaxConcurrentAgents != 3 {
			t.Errorf("MaxConcurrentAgents = %d, want 3", state.MaxConcurrentAgents)
		}
		if len(state.Running) != 3 {
			t.Errorf("after second tick Running = %d, want 3", len(state.Running))
		}

		cancel()
		for i := 0; i < len(state.Running); i++ {
			select {
			case <-o.workerExitCh:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for worker exit")
			}
		}
	})

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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

		if len(state.Running) != 0 {
			t.Fatalf("after first tick Running = %d, want 0", len(state.Running))
		}

		cfg.Tracker.ActiveStates = []string{"To Do", "QA Review"}
		wm.setConfig(cfg)

		o.handleTick(ctx)

		if len(state.Running) != 1 {
			t.Errorf("after second tick Running = %d, want 1", len(state.Running))
		}
		if _, ok := state.Running["qa-1"]; !ok {
			t.Error("issue qa-1 not in Running map after active state change")
		}

		cancel()
		for range state.Running {
			select {
			case <-o.workerExitCh:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for worker exit")
			}
		}
	})

	t.Run("reconcile_fresh_terminal_states", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Tracker.ActiveStates = []string{"To Do"}
		cfg.Tracker.TerminalStates = []string{"Done"}

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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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
			t.Fatal("entry removed from Running, reconciliation should not remove entries")
			return
		}
		if entry.PendingCleanup {
			t.Fatal("PendingCleanup = true before adding Archived to terminal states")
		}
		if !cancelCalled.Load() {
			t.Fatal("CancelFunc not called for non-active non-terminal issue")
		}

		cancelCalled.Store(false)
		entry.CancelFunc = func() { cancelCalled.Store(true) }
		cfg.Tracker.TerminalStates = []string{"Done", "Archived"}
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		entry = state.Running["arch-1"]
		if entry == nil {
			t.Fatal("entry removed from Running, reconciliation should not remove entries")
			return
		}
		if !entry.PendingCleanup {
			t.Error("PendingCleanup = false after adding Archived to terminal states, want true")
		}
	})

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
					// A terminal state re-dispatches runs across the template swap.
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "To Do"
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

		deadline := time.After(10 * time.Second)
		for {
			if _, ok := capturedPrompts.Load("P-1"); ok {
				break
			}
			select {
			case <-deadline:
				cancel()
				<-done
				t.Fatal("timed out waiting for the prompt for P-1")
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}

		tmpl2 := mustParseTemplate(t, "review {{ .issue.identifier }}")
		wm.setTemplate(tmpl2)
		issueSet.Store(1)

		deadline = time.After(10 * time.Second)
		for {
			if _, ok := capturedPrompts.Load("P-2"); ok {
				break
			}
			select {
			case <-deadline:
				cancel()
				<-done
				t.Fatal("timed out waiting for the prompt for P-2")
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

	t.Run("inflight_not_restarted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		cfg := lifecycleConfig(tmpDir)
		cfg.Agent.MaxConcurrentAgents = 2

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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

		cfg.Agent.MaxConcurrentAgents = 10
		wm.setConfig(cfg)

		o.handleTick(context.Background())

		if state.MaxConcurrentAgents != 10 {
			t.Errorf("MaxConcurrentAgents = %d, want 10", state.MaxConcurrentAgents)
		}

		entry = state.Running["f-1"]
		if entry == nil {
			t.Fatal("issue f-1 removed from Running after config change")
			return
		}

		if entry.CancelFunc == nil {
			t.Fatal("CancelFunc is nil after config change")
		}

		originalCancel()

		select {
		case <-o.workerExitCh:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not exit after cancel")
		}
	})

	t.Run("state_updates_on_preflight_failure", func(t *testing.T) {
		t.Parallel()

		cfg := lifecycleConfig(t.TempDir())
		cfg.Polling.IntervalMS = 5000
		cfg.Agent.MaxConcurrentAgents = 3
		cfg.Tracker.TerminalStates = []string{"Done"}

		wm := &stubWorkflowManager{config: cfg}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		obs := &stubObserver{}

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

		if state.PollIntervalMS != 5000 {
			t.Errorf("PollIntervalMS = %d, want 5000", state.PollIntervalMS)
		}
		if state.MaxConcurrentAgents != 3 {
			t.Errorf("MaxConcurrentAgents = %d, want 3", state.MaxConcurrentAgents)
		}

		entry := state.Running["g-1"]
		if entry == nil {
			t.Fatal("entry g-1 removed from Running, reconciliation should not remove entries")
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

	// agent.max_consecutive_absences takes effect on the next
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

	// A reload whose agent.max_consecutive_absences fails
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

	// reactions.ci_failure.watch_window_ms takes effect on
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
			WatchWindowMS: 24 * 3600 * 1000,
		}

		wm := &stubWorkflowManager{config: cfg}
		regs := passingPreflightRegistries()
		obs := &stubObserver{}
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

	// The ci_failure triage block is frozen at construction,
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})

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

	time.Sleep(50 * time.Millisecond)

	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return nil, nil
		},
	}

	regs := passingPreflightRegistries()
	state := NewState(100, 2, 0, nil, AgentTotals{})

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

func TestReconciliationGuardOnInvalidReload(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

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

	state := NewState(60000, 5, 0, nil, AgentTotals{})
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

	cfg := wm.Config()
	if len(cfg.Tracker.ActiveStates) != 1 || cfg.Tracker.ActiveStates[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", cfg.Tracker.ActiveStates)
	}

	if cancelCalled.Load() {
		t.Error("running worker was cancelled; expected it to be preserved")
	}

	if _, ok := state.Running["issue-1"]; !ok {
		t.Error("running entry removed; expected it to remain")
	}
}

func TestReconciliationGuardEndToEnd(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	workflowPath := filepath.Join(tmpDir, "WORKFLOW.md")

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

	wm, err := workflow.NewManager(workflowPath, discardLogger(),
		workflow.WithValidateFunc(ValidateConfigForPromotion))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	cfg := wm.Config()
	if len(cfg.Tracker.ActiveStates) == 0 {
		t.Fatal("initial ActiveStates is empty; expected [In Progress]")
	}

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

	state := NewState(60000, 5, 0, nil, AgentTotals{})
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
	// semantically dangerous: both active_states and terminal_states
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

	o.handleTick(context.Background())

	cfg = wm.Config()
	if len(cfg.Tracker.ActiveStates) != 1 || cfg.Tracker.ActiveStates[0] != "In Progress" {
		t.Errorf("Config().Tracker.ActiveStates = %v, want [In Progress]", cfg.Tracker.ActiveStates)
	}
	if len(cfg.Tracker.TerminalStates) != 1 || cfg.Tracker.TerminalStates[0] != "Done" {
		t.Errorf("Config().Tracker.TerminalStates = %v, want [Done]", cfg.Tracker.TerminalStates)
	}

	if cancelCalled.Load() {
		t.Error("running worker was cancelled; expected it to be preserved by validation guard")
	}

	if _, ok := state.Running["issue-1"]; !ok {
		t.Error("running entry removed; expected it to remain")
	}

	if wm.LastLoadError() == nil {
		t.Error("LastLoadError() = nil, want validation error")
	}
}

// TestReloadPromotesConfigWithMissingBlockFault pins the fail-safe
// invariant: a dispatch-routed kind with no settings block still
// promotes through Reload without error and surfaces the
// dispatch.agent.missing_block fault only at tick preflight.
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

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	t.Run("no_running_workers", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
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
		cancel()

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

		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

		cancel()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return within 10 seconds of cancellation")
		}

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

		state := NewState(60000, 1, 0, nil, AgentTotals{})

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

		workerCtx, workerCancel := context.WithCancel(context.Background())
		defer workerCancel()
		state.Running["hang-1"] = &RunningEntry{
			Identifier: "HANG-1",
			Issue:      domain.Issue{ID: "hang-1", Identifier: "HANG-1", Title: "Hung", State: "To Do"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: workerCancel,
		}
		state.Claimed["hang-1"] = struct{}{}

		go func() {
			<-workerCtx.Done()
			// Worker context cancelled but no result sent, hung.
		}()

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

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

		state := NewState(60000, 1, 0, nil, AgentTotals{})
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

		cancel()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return within 3 seconds")
		}

		time.Sleep(200 * time.Millisecond)

		select {
		case id := <-o.retryTimerCh:
			t.Errorf("retryTimerCh received %q after shutdown, want no late fires", id)
		default:
			// No message, timer was stopped correctly.
		}
	})

	t.Run("drains_in_flight_triage_run", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
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

func TestSnapshotFunc(t *testing.T) {
	t.Parallel()

	t.Run("round-trip through event loop", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{InputTokens: 42})

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

		state := NewState(60000, 1, 0, nil, AgentTotals{})
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

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
		})

		refreshFn := o.RefreshFunc()

		if !refreshFn() {
			t.Fatal("first RefreshFunc() = false, want true")
		}

		got := refreshFn()
		if got {
			t.Error("RefreshFunc() = true, want false (channel full, should coalesce)")
		}
	})

	t.Run("rejected during drain", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
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

		time.Sleep(100 * time.Millisecond)

		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return within 5 seconds")
		}

		refreshFn := o.RefreshFunc()
		if refreshFn() {
			t.Error("RefreshFunc() = true after drain, want false")
		}
	})
}

func TestAddObserver(t *testing.T) {
	t.Parallel()

	state := NewState(1000, 1, 0, nil, AgentTotals{})
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

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)

	cancel()

	time.Sleep(50 * time.Millisecond)

	snapFn := o.SnapshotFunc()

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

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	if !refreshFn() {
		t.Fatal("RefreshFunc() = false before drain, want true")
	}

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

	time.Sleep(100 * time.Millisecond)

	cancel()

	o.workerExitCh <- WorkerResult{IssueID: "id-1"}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds")
	}

	if refreshFn() {
		t.Error("RefreshFunc() = true after drain, want false")
	}
}

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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{Reason: budgetReasonSession}
		tracker := &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
		}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issue.ID]; !ok {
			t.Errorf("BudgetExhausted[%s] cleared on store error, want retained", issue.ID)
		}
	})

	t.Run("max_sessions zero clears BudgetExhausted", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfig(0)
		store := &stubStore{}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{}
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{}
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

func budgetTickConfigTokens(maxSessions, maxTokens int) *stubWorkflowManager {
	wm := budgetTickConfig(maxSessions)
	wm.config.Agent.MaxTokens = maxTokens
	return wm
}

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
		state := NewState(60000, 10, 0, nil, AgentTotals{})

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
		state := NewState(60000, 10, 0, nil, AgentTotals{})

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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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

	t.Run("a candidate below the ceiling whose measured session left a turn's spend unknown permits dispatch and warns once", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenUnaccountedIDs: []string{issueA.ID}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		orch := budgetOrchestratorWithLogger(state, wm, store, candidates(issueA), logger)
		orch.handleTick(context.Background())

		if _, ok := state.BudgetExhausted[issueA.ID]; ok {
			t.Errorf("BudgetExhausted[%s] present, want absent (below the ceiling permits dispatch)", issueA.ID)
		}
		if _, ok := state.TokenBudgetIncomplete[issueA.ID]; !ok {
			t.Errorf("TokenBudgetIncomplete[%s] missing, want present: the recorded total is a lower bound", issueA.ID)
		}
		output := buf.String()
		if got := strings.Count(output, "token budget cannot be fully evaluated, allowing dispatch"); got != 1 {
			t.Fatalf("warning count after first tick = %d, want 1; log:\n%s", got, output)
		}
		for _, want := range []string{
			`issue_id=` + issueA.ID,
			`issue_identifier=` + issueA.Identifier,
			"used_tokens=500",
			"budget_tokens=1000",
			"unmeasured_sessions=0",
			"unaccounted_turns=1",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("warning log missing %q; log:\n%s", want, output)
			}
		}

		orch.handleTick(context.Background())
		if got := strings.Count(buf.String(), "token budget cannot be fully evaluated, allowing dispatch"); got != 1 {
			t.Errorf("warning count after second tick = %d, want 1 (edge-triggered)", got)
		}
	})

	t.Run("a candidate reaching the ceiling is blocked and emits no unmeasured-budget warning", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issueA.ID}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})

		budgetOrchestrator(state, wm, store, candidates(issueA)).handleTick(context.Background())

		if len(state.TokenBudgetIncomplete) != 0 {
			t.Errorf("TokenBudgetIncomplete = %v, want empty when agent.max_tokens is unset", state.TokenBudgetIncomplete)
		}
	})

	t.Run("a live config reload removing the token ceiling clears TokenBudgetIncomplete", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenIncompleteIDs: []string{issueA.ID}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})

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

func TestHandleTick_TokenWarningThreshold(t *testing.T) {
	t.Parallel()

	t.Run("derived from the tick's config beside MaxTokens", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		wm.config.Agent.TokenWarningPercent = 80
		store := &stubStore{}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if state.MaxTokens != 1000 {
			t.Fatalf("state.MaxTokens = %d, want 1000", state.MaxTokens)
		}
		if state.TokenWarningThreshold != 800 {
			t.Errorf("state.TokenWarningThreshold = %d, want 800 (cfg.Agent.TokenWarningThreshold())", state.TokenWarningThreshold)
		}
	})

	t.Run("absent token_warning_percent leaves the threshold at zero", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}

		budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

		if state.TokenWarningThreshold != 0 {
			t.Errorf("state.TokenWarningThreshold = %d, want 0", state.TokenWarningThreshold)
		}
	})
}

func TestHandleTick_BudgetLogRecord(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-log", Identifier: "PROJ-LOG", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetLogRecordOnce(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-once", Identifier: "PROJ-ONCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetLogRecordCeilingSetting(t *testing.T) {
	t.Parallel()

	t.Run("session hold names agent.max_sessions", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-ceil-sess", Identifier: "PROJ-CEIL-SESS", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetLogRecordTokenAxis(t *testing.T) {
	t.Parallel()

	t.Run("token ceiling alone logs reason=token_budget", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-tok-log", Identifier: "PROJ-TOK-LOG", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(0, 1000)
		store := &stubStore{tokenExhaustedIDs: []string{issue.ID}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetLogRecordQueryError(t *testing.T) {
	t.Parallel()

	t.Run("session query error folds forward without announcing", func(t *testing.T) {
		t.Parallel()

		priorAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		issue := domain.Issue{ID: "iss-sess-err", Identifier: "PROJ-SESS-ERR", Title: "title", State: "To Do"}
		wm := budgetTickConfig(3)
		store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetAnnouncementLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("reason change from session to token re-announces", func(t *testing.T) {
		t.Parallel()

		issue := domain.Issue{ID: "iss-reason-change", Identifier: "PROJ-REASON", Title: "title", State: "To Do"}
		wm := budgetTickConfigTokens(3, 1000)
		store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		store.budgetExhaustedIDs = map[string]int{}
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetTickSummary(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-summary", Identifier: "PROJ-SUMMARY", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func incrementalWriteOrchestrator(t *testing.T, store *stubStore) (*Orchestrator, *RunningEntry) {
	t.Helper()
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

	t.Run("unmeasured entry persists a zero count despite a non-zero raw count", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		// turn_end never reports during the turn, so the verdict is
		// false regardless of the pre-set raw count of 2.
		entry.UsageArrival = registry.UsageArrivalTurnEnd

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		writes := store.sessionWrites()
		if len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1", len(writes))
		}
		if writes[0].APIRequestsMeasured {
			t.Fatal("SessionMetadata.APIRequestsMeasured = true, want false (turn_end never reports during the turn)")
		}
		if writes[0].APIRequestCount != 0 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 0 for an unmeasured row", writes[0].APIRequestCount)
		}
	})

	t.Run("measured entry persists the raw count", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.UsageArrival = registry.UsageArrivalIncremental
		entry.TurnCount = 1
		entry.APIRequestCount = 4

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		writes := store.sessionWrites()
		if len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1", len(writes))
		}
		if !writes[0].APIRequestsMeasured {
			t.Fatal("SessionMetadata.APIRequestsMeasured = false, want true (a request arrived)")
		}
		if writes[0].APIRequestCount != 4 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 4 for a measured row", writes[0].APIRequestCount)
		}
	})

	t.Run("none arrival writes no row", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.UsageArrival = registry.UsageArrivalNone

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 0 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 0 (none arrival)", len(writes))
		}
	})

	t.Run("incremental arrival writes once for the same event", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.UsageArrival = registry.UsageArrivalIncremental

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 1 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 1 (incremental arrival)", len(writes))
		}
	})

	t.Run("DispatchID from the running entry is carried on every write", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.DispatchID = "dispatch-incr-1"

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))
		entry.LastMetadataWrite = time.Now().UTC().Add(-sessionMetadataWriteInterval - time.Second)
		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(11, 21, 33, 6))

		writes := store.sessionWrites()
		if len(writes) != 2 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 2", len(writes))
		}
		for i, w := range writes {
			if w.DispatchID != "dispatch-incr-1" {
				t.Errorf("writes[%d].DispatchID = %q, want %q", i, w.DispatchID, "dispatch-incr-1")
			}
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
		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(11, 21, 33, 6))
		if writes := store.sessionWrites(); len(writes) != 2 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 2 (failed write not throttled)", len(writes))
		}
	})

	t.Run("a pending write bypasses the throttle and clears on success", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.LastMetadataWrite = time.Now().UTC()
		entry.MetadataWritePending = true

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1: a pending write bypasses the throttle", len(writes))
		}
		if entry.MetadataWritePending {
			t.Error("RunningEntry.MetadataWritePending = true after a successful write, want false")
		}
	})

	t.Run("a failing pending write still bypasses the throttle but leaves MetadataWritePending set", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{upsertSessionMetadataErr: fmt.Errorf("disk full")}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.LastMetadataWrite = time.Now().UTC()
		entry.MetadataWritePending = true

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 1 {
			t.Fatalf("UpsertSessionMetadata calls = %d, want 1: a pending write bypasses the throttle even on failure", len(writes))
		}
		if !entry.MetadataWritePending {
			t.Error("RunningEntry.MetadataWritePending = false after a failed write, want true: still owed")
		}
	})

	t.Run("no pending write stays throttled within the interval", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		o, entry := incrementalWriteOrchestrator(t, store)
		entry.LastMetadataWrite = time.Now().UTC()

		o.maybeWriteIncrementalMetadata(ctx, "id-1", tokenUsageEvent(10, 20, 30, 5))

		if writes := store.sessionWrites(); len(writes) != 0 {
			t.Errorf("UpsertSessionMetadata calls = %d, want 0: no write owed, still within the throttle interval", len(writes))
		}
	})
}

// tokenWarningApplyEventOrchestrator returns an orchestrator and running
// entry set up for a single issue whose token warning threshold and
// ceiling are both configurable, for applyAgentEvent-level assertions.
func tokenWarningApplyEventOrchestrator(t *testing.T, maxTokens, warningThreshold int) (*Orchestrator, *RunningEntry) {
	t.Helper()
	state := NewState(60000, 10, maxTokens, nil, AgentTotals{})
	state.TokenWarningThreshold = warningThreshold
	entry := &RunningEntry{
		Identifier:   "ISS-APPLY-ident",
		Issue:        domain.Issue{ID: "id-apply", Identifier: "ISS-APPLY-ident"},
		UsageArrival: registry.UsageArrivalIncremental,
	}
	state.Running["id-apply"] = entry
	tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
	return budgetOrchestrator(state, budgetTickConfig(0), &stubStore{}, tracker), entry
}

// TestApplyAgentEvent_TokenWarningOrderedBeforeCeilingStop covers a figure
// that crosses both the warning threshold and the ceiling in the same
// event: the warning record must land before enforceInFlightTokenCeiling
// invokes TokenCeilingCancelFunc for that figure.
func TestApplyAgentEvent_TokenWarningOrderedBeforeCeilingStop(t *testing.T) {
	t.Parallel()

	o, entry := tokenWarningApplyEventOrchestrator(t, 100, 80)
	var warnedBeforeCancel bool
	entry.TokenCeilingCancelFunc = func() { warnedBeforeCancel = entry.TokenWarningReached }

	msg := agentEventMsg{IssueID: "id-apply", Event: domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 150}}}
	o.applyAgentEvent(context.Background(), msg, true)

	if !entry.TokenWarningReached {
		t.Fatal("entry.TokenWarningReached = false, want true (150 crosses the 80 threshold)")
	}
	if entry.TokenCeilingStopRequest == nil {
		t.Fatal("entry.TokenCeilingStopRequest = nil, want non-nil (150 also crosses the 100 ceiling)")
	}
	if !warnedBeforeCancel {
		t.Error("TokenCeilingCancelFunc observed TokenWarningReached = false, want the warning recorded before the ceiling cancels the run")
	}
}

// TestApplyAgentEvent_EnforceCeilingFalseSuppressesTokenWarning covers the
// drain path, which applies queued events for an exiting run with
// enforceCeiling false: a figure that would otherwise cross the threshold
// must not warn, since that run can no longer be interrupted.
func TestApplyAgentEvent_EnforceCeilingFalseSuppressesTokenWarning(t *testing.T) {
	t.Parallel()

	o, entry := tokenWarningApplyEventOrchestrator(t, 100, 80)

	msg := agentEventMsg{IssueID: "id-apply", Event: domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 150}}}
	o.applyAgentEvent(context.Background(), msg, false)

	if entry.TokenWarningReached {
		t.Error("entry.TokenWarningReached = true, want false: enforceCeiling false must suppress the warning evaluation")
	}
	if entry.TokenCeilingStopRequest != nil {
		t.Error("entry.TokenCeilingStopRequest = non-nil, want nil: enforceCeiling false must suppress the ceiling evaluation")
	}
}

// TestApplyAgentEvent_NoThresholdConfiguredMatchesPriorBehavior covers the
// disabled threshold: applyAgentEvent's only observable effects are then
// HandleAgentEvent's own state and the incremental metadata write, so the
// sequence of events an absent or zero token_warning_percent produces
// stays byte-for-byte what it was before this feature existed.
func TestApplyAgentEvent_NoThresholdConfiguredMatchesPriorBehavior(t *testing.T) {
	t.Parallel()

	o, entry := tokenWarningApplyEventOrchestrator(t, 0, 0)

	msg := agentEventMsg{IssueID: "id-apply", Event: domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 150}}}
	o.applyAgentEvent(context.Background(), msg, true)

	if entry.TokenWarningReached {
		t.Error("entry.TokenWarningReached = true, want false: no threshold configured")
	}
	if entry.MetadataWritePending {
		t.Error("entry.MetadataWritePending = true, want false: no threshold configured")
	}
	if entry.AgentTotalTokens != 150 {
		t.Errorf("entry.AgentTotalTokens = %d, want 150 (HandleAgentEvent's own accounting is unaffected)", entry.AgentTotalTokens)
	}
}

func TestDrainRunningWorkers_SelfReviewProgressUpdatesRunningEntry(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, 0, nil, AgentTotals{})
	state.Running["id-1"] = queuedOrderingIssueEntry("id-1")
	store := &stubStore{}
	tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
	o := budgetOrchestrator(state, budgetTickConfig(0), store, tracker)
	// The drain must still serve snapshot requests when the poll below
	// gives up, so its own deadline sits well beyond the poll's.
	o.drainTimeout = time.Minute

	done := make(chan struct{})
	go func() {
		o.drainRunningWorkers()
		close(done)
	}()

	o.selfReviewCh <- selfReviewProgressMsg{IssueID: "id-1", Message: "self_review_iteration", Iteration: 2, MaxIterations: 3}

	deadline := time.After(10 * time.Second)
	for {
		reply := make(chan RuntimeSnapshotResult, 1)
		o.snapshotCh <- snapshotRequest{ReplyCh: reply}
		snap := <-reply
		if len(snap.Running) == 1 && snap.Running[0].SelfReviewActive && snap.Running[0].SelfReviewIteration == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("RuntimeSnapshot().Running = %+v after 10s of drain, want one entry with SelfReviewActive true and SelfReviewIteration 2", snap.Running)
		case <-time.After(10 * time.Millisecond):
		}
	}

	o.workerExitCh <- WorkerResult{IssueID: "id-1", Identifier: "id-1-ident"}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainRunningWorkers did not return within 10 seconds")
	}
}

func TestDrainRunningWorkers_TokenUsageEventTriggersIncrementalWrite(t *testing.T) {
	t.Parallel()

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	o.workerExitCh <- WorkerResult{IssueID: "id-1", Identifier: "MT-1"}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainRunningWorkers did not return within 10 seconds")
	}
}

func TestDrainRunningWorkers_AbsenceCeiling(t *testing.T) {
	t.Parallel()

	t.Run("parks at a ceiling below the built-in default", func(t *testing.T) {
		t.Parallel()

		const issueID = "DRAIN-ABS-BELOW"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		state := NewState(60000, 1, 0, nil, AgentTotals{})
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
		state := NewState(60000, 1, 0, nil, AgentTotals{})
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

// TestDrainRunningWorkers_CeilingFormula pins the ceiling with no
// o.drainTimeout override: 50s at defaults, else the grace plus
// drainExitMargin. Asserted against the formula directly, since running
// to that bound would spend at least 45s of real wait.
func TestDrainRunningWorkers_CeilingFormula(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stopGraceMS int
		want        time.Duration
	}{
		{"DefaultConfiguration", 0, 50 * time.Second},
		{"ConfiguredGraceRaisesTheCeiling", 60000, 60*time.Second + 3*procutil.DefaultDrainGrace + drainExitMargin},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := config.ServiceConfig{Agent: config.AgentConfig{StopGraceMS: tt.stopGraceMS}}
			got := stopSessionDeadline(cfg) + drainExitMargin
			if got != tt.want {
				t.Errorf("stopSessionDeadline(cfg) + drainExitMargin at StopGraceMS=%d = %v, want %v", tt.stopGraceMS, got, tt.want)
			}
		})
	}
}

// TestDrainRunningWorkers_AbandonChannel: a closed AbandonCh ends the
// wait promptly with its own warning; a nil AbandonCh leaves the
// existing bound in force, asserted without spending it in wall clock.
func TestDrainRunningWorkers_AbandonChannel(t *testing.T) {
	t.Parallel()

	t.Run("closed AbandonCh ends drainRunningWorkers promptly", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.Running["id-1"] = &RunningEntry{
			Identifier: "PROJ-1",
			Issue:      domain.Issue{ID: "id-1", Identifier: "PROJ-1", State: "In Progress"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: func() {},
		}
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		abandonCh := make(chan struct{})
		close(abandonCh)
		var buf bytes.Buffer
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, slog.New(slog.NewTextHandler(&buf, nil)), abandonCh)
		o.drainTimeout = time.Hour // would time out long after the test itself, if the abandon arm did not fire first

		done := make(chan struct{})
		go func() {
			o.drainRunningWorkers()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("drainRunningWorkers did not return promptly with a closed AbandonCh")
		}

		output := buf.String()
		if !strings.Contains(output, "worker drain abandoned at the operator's request") {
			t.Errorf("drainRunningWorkers() did not log the abandon warning: %s", output)
		}
		if strings.Contains(output, "drain timeout exceeded") {
			t.Errorf("drainRunningWorkers() logged the timeout message for an operator abort, want the two distinguishable: %s", output)
		}
	})

	t.Run("nil AbandonCh leaves the existing bound in force", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.Running["id-1"] = &RunningEntry{
			Identifier: "PROJ-1",
			Issue:      domain.Issue{ID: "id-1", Identifier: "PROJ-1", State: "In Progress"},
			StartedAt:  time.Now().UTC(),
			CancelFunc: func() {},
		}
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, discardLogger(), nil)
		o.drainTimeout = 300 * time.Millisecond // short override so the timeout arm, not the 50s default ceiling, ends this test

		start := time.Now()
		done := make(chan struct{})
		go func() {
			o.drainRunningWorkers()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("drainRunningWorkers did not return within 5s")
		}
		elapsed := time.Since(start)

		if elapsed < 300*time.Millisecond {
			t.Errorf("drainRunningWorkers() with a nil AbandonCh returned after %v, want it to have honored the 300ms drainTimeout override rather than returning early through a nil-channel receive", elapsed)
		}
	})
}

// TestDrainTrackerOps_AbandonChannel and TestDrainTriageRuns_AbandonChannel
// mirror the worker-drain abandon test. Both wait on their own WaitGroup,
// so in-flight work is simulated by holding that group open rather than
// by populating Running.
func TestDrainTrackerOps_AbandonChannel(t *testing.T) {
	t.Parallel()

	t.Run("closed AbandonCh ends drainTrackerOps promptly", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.TrackerOpsWg.Add(1)
		t.Cleanup(state.TrackerOpsWg.Done)
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		abandonCh := make(chan struct{})
		close(abandonCh)
		var buf bytes.Buffer
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, slog.New(slog.NewTextHandler(&buf, nil)), abandonCh)

		done := make(chan struct{})
		go func() {
			o.drainTrackerOps()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("drainTrackerOps did not return promptly with a closed AbandonCh")
		}

		output := buf.String()
		if !strings.Contains(output, "tracker ops drain abandoned at the operator's request") {
			t.Errorf("drainTrackerOps() did not log the abandon warning: %s", output)
		}
		if strings.Contains(output, "drain timeout exceeded") {
			t.Errorf("drainTrackerOps() logged the timeout message for an operator abort, want the two distinguishable: %s", output)
		}
	})

	t.Run("nil AbandonCh leaves the wait entered rather than returning early", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.TrackerOpsWg.Add(1)
		t.Cleanup(state.TrackerOpsWg.Done)
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, discardLogger(), nil)

		done := make(chan struct{})
		go func() {
			o.drainTrackerOps()
			close(done)
		}()

		select {
		case <-done:
			t.Fatal("drainTrackerOps returned with a nil AbandonCh and in-flight work still pending, want it to keep waiting")
		case <-time.After(300 * time.Millisecond):
			// Still waiting after 300ms, well short of the 35s
			// trackerOpsDrainTimeout: this is the entered-the-wait
			// evidence the nil arm must not have short-circuited past.
		}
	})
}

func TestDrainTriageRuns_AbandonChannel(t *testing.T) {
	t.Parallel()

	t.Run("closed AbandonCh ends drainTriageRuns promptly", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.TriageWg.Add(1)
		t.Cleanup(state.TriageWg.Done)
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		abandonCh := make(chan struct{})
		close(abandonCh)
		var buf bytes.Buffer
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, slog.New(slog.NewTextHandler(&buf, nil)), abandonCh)

		done := make(chan struct{})
		go func() {
			o.drainTriageRuns()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("drainTriageRuns did not return promptly with a closed AbandonCh")
		}

		output := buf.String()
		if !strings.Contains(output, "reaction triage drain abandoned at the operator's request") {
			t.Errorf("drainTriageRuns() did not log the abandon warning: %s", output)
		}
		if strings.Contains(output, "drain timeout exceeded") {
			t.Errorf("drainTriageRuns() logged the timeout message for an operator abort, want the two distinguishable: %s", output)
		}
	})

	t.Run("nil AbandonCh leaves the wait entered rather than returning early", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		state.TriageWg.Add(1)
		t.Cleanup(state.TriageWg.Done)
		store := &stubStore{}
		tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
		o := drainTestOrchestrator(state, budgetTickConfig(0), store, tracker, discardLogger(), nil)

		done := make(chan struct{})
		go func() {
			o.drainTriageRuns()
			close(done)
		}()

		select {
		case <-done:
			t.Fatal("drainTriageRuns returned with a nil AbandonCh and in-flight work still pending, want it to keep waiting")
		case <-time.After(300 * time.Millisecond):
			// Still waiting after 300ms, well short of the 35s
			// trackerOpsDrainTimeout: this is the entered-the-wait
			// evidence the nil arm must not have short-circuited past.
		}
	})
}

func drainTestOrchestrator(state *State, wm *stubWorkflowManager, store *stubStore, tracker *candidateTrackerAdapter, logger *slog.Logger, abandonCh <-chan struct{}) *Orchestrator {
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
		AbandonCh:       abandonCh,
	})
}

func TestBudgetExhaustionPreventsRedispatch(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-redisp", Identifier: "PROJ-1", Title: "Work", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 1}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestBudgetExhaustionClearsWhenMaxSessionsZero(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-clear", Identifier: "PROJ-2", Title: "Retry", State: "To Do"}
	wm := budgetTickConfig(0)
	store := &stubStore{}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
	state.BudgetExhausted[issue.ID] = &BudgetExhaustedEntry{}
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
	}

	budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())

	if _, ok := state.BudgetExhausted[issue.ID]; ok {
		t.Errorf("BudgetExhausted[%s] still set after MaxSessions=0 tick, want cleared", issue.ID)
	}
}

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
		// Both execute sequentially in the same worker goroutine, no race.
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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
		cfg := handoffConfig(tmpDir)
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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
		}

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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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
		cfg.Polling.IntervalMS = 100
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
		state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

// sweepThrottleTracker records FetchIssueStatesByIdentifiers calls; its
// other methods return zero values so tick-loop side effects do not
// panic.
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

	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	for range sweepEveryNTicks - 1 {
		o.handleTick(ctx)
	}
	if got := tracker.callCount(); got != 0 {
		t.Errorf("FetchIssueStatesByIdentifiers called %d times before tick %d, want 0",
			got, sweepEveryNTicks)
	}

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
	state := NewState(60000, 1, 0, nil, AgentTotals{})
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

	o.handleTick(ctx)
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), warnMsg); got != 1 {
		t.Errorf("warning count after two identical ticks = %d, want 1\nlog:\n%s", got, buf.String())
	}

	cfg.SetExtensionSection("worker", map[string]any{
		"ssh_strict_host_key_checking": "strict",
	})
	wm.setConfig(cfg)
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), warnMsg); got != 2 {
		t.Errorf("warning count after changing value to 'strict' = %d, want 2\nlog:\n%s", got, buf.String())
	}

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

func TestTickLogging_DispatchBreakdown(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)

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
		State:  NewState(1000, 5, 0, nil, AgentTotals{}),
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
	if !strings.Contains(got, "dispatched_by_rule=1") {
		t.Errorf("log missing dispatched_by_rule=1: %s", got)
	}
	if !strings.Contains(got, "dispatched_by_default=0") {
		t.Errorf("log missing dispatched_by_default=0: %s", got)
	}
	if !strings.Contains(got, "dispatched_by_fallback=1") {
		t.Errorf("log missing dispatched_by_fallback=1: %s", got)
	}
}

// TestDispatch_RuleResolvedKindPersistsToRunHistory pins the
// freeze-on-dispatch wiring: a rule-routed non-default agent kind must
// reach RunHistory.AgentAdapter, not cfg.Agent.Kind.
func TestDispatch_RuleResolvedKindPersistsToRunHistory(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)

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

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
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

	store.mu.Lock()
	rows := make(map[string]persistence.RunHistory, len(store.runHistories))
	for _, rh := range store.runHistories {
		rows[rh.IssueID] = rh
	}
	store.mu.Unlock()

	if len(rows) != 2 {
		t.Fatalf("len(runHistories) = %d, want 2", len(rows))
	}

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

func TestHandleTick_DispatchFreezesUsageDisposition(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cfg := lifecycleConfig(tmpDir)
	cfg.Agent.Kind = "mock"
	cfg.Dispatch = config.DispatchConfig{
		Rules: []config.DispatchRule{
			{
				Name:      "narrowed-router",
				Match:     config.DispatchMatch{Labels: []string{"narrowed"}},
				Selection: config.DispatchSelection{AgentKind: "narrowed-kind"},
			},
		},
	}
	cfg.SetExtensionSection("narrowed-kind", map[string]any{})

	narrowedIssue := domain.Issue{
		ID: "id-narrowed", Identifier: "USG-1", Title: "Narrowed", State: "To Do",
		Labels: []string{"narrowed"},
	}
	defaultIssue := domain.Issue{
		ID: "id-default", Identifier: "USG-2", Title: "Default", State: "To Do",
	}

	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn: func(_ context.Context) ([]domain.Issue, error) {
			return []domain.Issue{narrowedIssue, defaultIssue}, nil
		},
	}

	// Each dispatched worker runs in its own goroutine; RunTurn fires
	// only after workspace preparation for that issue has completed.
	// Waiting for both signals before returning avoids a race between
	// that background filesystem activity and t.TempDir()'s cleanup
	// at the end of the test.
	var runTurnStarted sync.WaitGroup
	runTurnStarted.Add(2)
	reportedUsage := domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CacheReadTokens: 5}
	trackedAdapter := &mockAgentAdapter{
		runTurnFn: func(_ context.Context, sess domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
			runTurnStarted.Done()
			return domain.TurnResult{
				SessionID:     sess.ID,
				ExitReason:    domain.EventTurnCompleted,
				Usage:         reportedUsage,
				UsageMeasured: true,
			}, nil
		},
	}

	wm := &stubWorkflowManager{config: cfg, template: tmpl, absPath: filepath.Join(tmpDir, "WORKFLOW.md")}
	store := &stubStore{}

	agentRegistry := &stubAgentRegistry{
		getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
		metaFunc: func(kind string) (registry.AgentMeta, bool) {
			switch kind {
			case "mock":
				return registry.AgentMeta{
					UsageArrival:     registry.UsageArrivalIncremental,
					UsageAttribution: registry.UsageAttributionPerModel,
				}, true
			case "narrowed-kind":
				return registry.AgentMeta{
					UsageArrival:     registry.UsageArrivalTurnEnd,
					UsageAttribution: registry.UsageAttributionSessionTotal,
					UsageSessionRules: []registry.UsageSessionRule{
						{
							When:        func(map[string]any, bool) bool { return true },
							Arrival:     registry.UsageArrivalNone,
							Attribution: registry.UsageAttributionNone,
						},
					},
				}, true
			default:
				return registry.AgentMeta{}, false
			}
		},
	}

	state := NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{})
	o := NewOrchestrator(OrchestratorParams{
		State:          state,
		Logger:         discardLogger(),
		TrackerAdapter: tracker,
		AgentAdapter:   trackedAdapter,
		AgentAdapterByKind: func(string) (domain.AgentAdapter, error) {
			return trackedAdapter, nil
		},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      wm.Config,
			TrackerRegistry: passingPreflightRegistries().TrackerRegistry,
			AgentRegistry:   agentRegistry,
		},
	})

	o.handleTick(context.Background())
	runTurnStarted.Wait()

	defaultEntry := state.Running[defaultIssue.ID]
	if defaultEntry == nil {
		t.Fatal("no RunningEntry for the default-kind issue")
	}
	if defaultEntry.UsageArrival != registry.UsageArrivalIncremental || defaultEntry.UsageAttribution != registry.UsageAttributionPerModel {
		t.Errorf("default-kind entry (UsageArrival, UsageAttribution) = (%q, %q), want (%q, %q)",
			defaultEntry.UsageArrival, defaultEntry.UsageAttribution, registry.UsageArrivalIncremental, registry.UsageAttributionPerModel)
	}

	narrowedEntry := state.Running[narrowedIssue.ID]
	if narrowedEntry == nil {
		t.Fatal("no RunningEntry for the rule-routed issue")
	}
	if narrowedEntry.UsageArrival != registry.UsageArrivalNone || narrowedEntry.UsageAttribution != registry.UsageAttributionNone {
		t.Errorf("rule-routed entry (UsageArrival, UsageAttribution) = (%q, %q), want (%q, %q)",
			narrowedEntry.UsageArrival, narrowedEntry.UsageAttribution, registry.UsageArrivalNone, registry.UsageAttributionNone)
	}

	exitResults := make(map[string]WorkerResult, 2)
	for i := range 2 {
		select {
		case r := <-o.workerExitCh:
			exitResults[r.IssueID] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for worker exit %d/2", i+1)
		}
	}

	if got := exitResults[defaultIssue.ID].Usage; got != reportedUsage {
		t.Errorf("default-kind WorkerResult.Usage = %+v, want %+v (the reported figure)", got, reportedUsage)
	}
	if got := exitResults[narrowedIssue.ID].Usage; got != (domain.TokenUsage{}) {
		t.Errorf("rule-routed (none) WorkerResult.Usage = %+v, want zero", got)
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

// newBlockerGateOrchestrator wires a mock tracker whose candidates are
// issues, resolver as the blocker resolver, logging at Debug. metrics
// may be nil.
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
		State:  NewState(1000, 10, 0, nil, AgentTotals{}),
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

// TestHandleTick_EveryAttemptedReadFailedWarning pins that the WARN
// fires only when every attempted read failed transiently and the tick
// dispatched nothing, and carries reads_failed as reads attempted, not
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

func TestHandleTick_BudgetHoldNoticeOnce(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-once", Identifier: "PROJ-NOTICE-ONCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

// TestBudgetHoldNoticeSurvivesRestart pins that the durable row, not the
// in-memory latch, suppresses a repeat notice across a restart: it
// reopens a file-backed store, reloads State.BudgetHoldNoticed, and
// drives a second tick over the same candidate.
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
	state1 := NewState(60000, 10, 0, nil, AgentTotals{})
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

	state2 := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetHoldNoticeTrackerFailureIsolated(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-fail", Identifier: "PROJ-NOTICE-FAIL", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetHoldNoticeReasonChange(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-reason", Identifier: "PROJ-NOTICE-REASON", Title: "title", State: "To Do"}
	wm := budgetTickConfigTokens(3, 1000)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetHoldNoticeReleaseOnClear(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-clear", Identifier: "PROJ-NOTICE-CLEAR", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

	store.budgetExhaustedIDs = map[string]int{}
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

func TestHandleTick_BudgetHoldNoticeAbsenceThenReturn(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-absence", Identifier: "PROJ-NOTICE-ABSENCE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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
	store.budgetExhaustedIDs = map[string]int{}
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

func TestHandleTick_BudgetHoldNoticeFoldedForward(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-fold", Identifier: "PROJ-NOTICE-FOLD", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetHoldNoticeDisableReenable(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-disable", Identifier: "PROJ-NOTICE-DISABLE", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 5}}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

// TestHandleTick_BudgetHoldNoticePacingWindow pins the per-wall-clock-window
// bound: maxBudgetHoldNoticesPerWindow notices per window, no drops or
// duplicates across windows, derived per window rather than per tick.
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
		state := NewState(60000, 10, 0, nil, AgentTotals{})
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
		state := NewState(1000, 10, 0, nil, AgentTotals{})
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

func TestHandleTick_BudgetHoldNoticeParkedIssue(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-parked", Identifier: "PROJ-NOTICE-PARKED", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{
		budgetExhaustedIDs: map[string]int{issue.ID: 5},
		absenceCounts:      map[string]int{issue.ID: 3},
	}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

func TestPostBudgetHoldNotice_NilTrackerAdapterWritesNoRow(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, 0, nil, AgentTotals{})
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

func TestPostBudgetHoldNotice_UpsertFails(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	state := NewState(5000, 4, 0, nil, AgentTotals{})
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

// TestHandleTick_BudgetHoldNoticeQueryErrorWithholdsRelease pins that a
// failed budget query withholds release: after a restart the prior set
// is empty, so releasing on that evidence would drop a durable record
// and re-post a notice for a hold that never ended.
func TestHandleTick_BudgetHoldNoticeQueryErrorWithholdsRelease(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-notice-withheld", Identifier: "PROJ-NOTICE-WITHHELD", Title: "title", State: "To Do"}
	wm := budgetTickConfig(3)
	store := &stubStore{budgetExhaustedErr: fmt.Errorf("db error")}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
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

	store.budgetExhaustedErr = nil
	store.budgetExhaustedIDs = map[string]int{issue.ID: 5}
	orch.handleTick(context.Background())
	state.TrackerOpsWg.Wait()

	if got := len(tracker.commentCalls); got != 0 {
		t.Errorf("commentCalls = %d, want 0 (the surviving notice record suppresses a duplicate comment)", got)
	}
}

// TestReconcilePasses_DoNotBlockOnInFlightTriage pins that each
// triage-gated reconcile pass short-circuits ahead of its provider on an
// in-flight triage run. Each provider double would block forever if
// called, so a prompt return proves the short-circuit.
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
