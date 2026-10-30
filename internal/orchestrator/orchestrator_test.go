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
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workflow"

	_ "github.com/sortie-ai/sortie/internal/agent/mock"
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

func (s *stubWorkflowManager) LoadStatus() workflow.LoadStatus { return workflow.LoadStatus{} }

func (s *stubWorkflowManager) Rates() *pricing.Rates { return nil }

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

// stateChangeSignal returns an observer and the channel it signals. The send
// never blocks because OnStateChange runs on the event loop.
func stateChangeSignal() (Observer, <-chan struct{}) {
	signal := make(chan struct{}, 1)
	return observerFunc(func() {
		select {
		case signal <- struct{}{}:
		default:
		}
	}), signal
}

type stubStore struct {
	reactionActivityLog
	livePassLog
	noopOutcomeRecorder
	noEscalationRecords
	noUsageLimitHolds
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

	tokenExhaustedUsage map[string]persistence.IssueTokenUsage

	upsertSessionMetadataErr error

	windowUsage persistence.IssueTokenUsage

	parkedIssues       []persistence.ParkedIssue
	deletedParkedIDs   []string
	labelAppliedIDs    []string
	listParkedIssues   []persistence.ParkedIssue
	listParkedIssueErr error

	latestRuns      map[string]persistence.RunHistory
	latestRunsErr   error
	latestRunsCalls [][]string

	dispatchMarked    []string
	clearedOutcomeIDs []string
	markDispatchedErr error

	budgetHoldNotices         []persistence.BudgetHoldNotice
	upsertBudgetHoldNoticeErr error
	deletedBudgetHoldIDs      []string
	deleteBudgetHoldNoticeErr error
	deleteAllBudgetHoldCalls  int
	deleteAllBudgetHoldErr    error
	listBudgetHoldNotices     []persistence.BudgetHoldNotice
	listBudgetHoldNoticesErr  error

	storedAcknowledgements    map[string]persistence.AttentionAcknowledgement
	savedAcknowledgements     []persistence.AttentionAcknowledgement
	deletedAcknowledgementIDs []string
	saveAcknowledgementErr    error
	deleteAcknowledgementsErr error

	// scopeBudgetToCandidates makes QueryBudgetExhaustedIssues answer only
	// for the candidate IDs it receives, as persistence.Store does.
	scopeBudgetToCandidates bool

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

func (s *stubStore) AppendRunHistoryWithEvents(ctx context.Context, run persistence.RunHistory, _ []persistence.RunEvent, _ persistence.RunEventLimits) (persistence.RunHistory, error) {
	return s.AppendRunHistory(ctx, run)
}

func (s *stubStore) AppendRunEvents(context.Context, string, string, []persistence.RunEvent, persistence.RunEventLimits) error {
	return nil
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

func (s *stubStore) DeleteIssueOutcome(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearedOutcomeIDs = append(s.clearedOutcomeIDs, issueID)
	return nil
}

func (s *stubStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletedRetryIDs = append(s.deletedRetryIDs, issueID)
	return nil
}

func (s *stubStore) Ping(_ context.Context) error { return nil }

func (s *stubStore) ListReportedModels(context.Context) ([]string, error) { return nil, nil }

func (s *stubStore) CountRunHistoryByIssue(_ context.Context, _ string) (int, error) {
	return 0, nil
}

func (s *stubStore) TokenUsageByIssue(_ context.Context, _ string) (persistence.IssueTokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistence.IssueTokenUsage{
		TotalTokens:        s.tokenSum,
		UncachedTokens:     s.tokenSum,
		Sessions:           s.tokenSessionCount,
		UnmeasuredSessions: s.tokenUnmeasured,
		UnaccountedTurns:   s.tokenUnaccounted,
	}, nil
}

func (s *stubStore) UsageCompletedBetween(context.Context, time.Time, time.Time) (persistence.IssueTokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windowUsage, nil
}

func (s *stubStore) QueryBudgetExhaustedIssues(_ context.Context, candidateIDs []string, _ int) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budgetExhaustedErr != nil {
		return nil, s.budgetExhaustedErr
	}
	result := make(map[string]int, len(s.budgetExhaustedIDs))
	maps.Copy(result, s.budgetExhaustedIDs)
	if s.scopeBudgetToCandidates {
		maps.DeleteFunc(result, func(id string, _ int) bool { return !slices.Contains(candidateIDs, id) })
	}
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

func (s *stubStore) QueryTokenBudgetUsage(_ context.Context, candidateIDs []string) (map[string]persistence.IssueTokenUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokenExhaustedErr != nil {
		return nil, s.tokenExhaustedErr
	}
	usage := make(map[string]persistence.IssueTokenUsage, len(candidateIDs))
	for _, id := range candidateIDs {
		if custom, ok := s.tokenExhaustedUsage[id]; ok {
			usage[id] = custom
			continue
		}
		switch {
		case slices.Contains(s.tokenExhaustedIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 1000, UncachedTokens: 1000}
		case slices.Contains(s.tokenIncompleteIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 0, UnmeasuredSessions: 1}
		case slices.Contains(s.tokenUnaccountedIDs, id):
			usage[id] = persistence.IssueTokenUsage{TotalTokens: 500, UncachedTokens: 500, Sessions: 1, UnaccountedTurns: 1}
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

func (s *stubStore) SaveAttentionAcknowledgement(_ context.Context, ack persistence.AttentionAcknowledgement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveAcknowledgementErr != nil {
		return s.saveAcknowledgementErr
	}
	if s.storedAcknowledgements == nil {
		s.storedAcknowledgements = make(map[string]persistence.AttentionAcknowledgement)
	}
	s.storedAcknowledgements[ack.ItemID] = ack
	s.savedAcknowledgements = append(s.savedAcknowledgements, ack)
	return nil
}

func (s *stubStore) DeleteAttentionAcknowledgements(_ context.Context, itemIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteAcknowledgementsErr != nil {
		return s.deleteAcknowledgementsErr
	}
	for _, id := range itemIDs {
		delete(s.storedAcknowledgements, id)
	}
	s.deletedAcknowledgementIDs = append(s.deletedAcknowledgementIDs, itemIDs...)
	return nil
}

func (s *stubStore) ListAttentionAcknowledgements(_ context.Context) ([]persistence.AttentionAcknowledgement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Values(s.storedAcknowledgements)), nil
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

	wfn := o.makeWorkerFn("", "user@stand-in-host", kind, "", "", nil, AttemptSettings{})

	exitDone := make(chan struct{})
	go func() {
		wfn(context.Background(), issue, nil)
		close(exitDone)
	}()

	waittest.Receive(t, exitDone, "worker exit")

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

	waittest.Receive(t, done, "Run return")
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

			t.Cleanup(func() {
				cancel()
				<-done
			})
			row := waittest.Receive(t, captured, fmt.Sprintf("trial %d: HandleWorkerExit's session_metadata row", trial))

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

			waittest.Receive(t, done, fmt.Sprintf("trial %d: drainRunningWorkers return", trial))

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
			Identifier:           "X-ident",
			Issue:                domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:            time.Now().UTC(),
			IssueTokensCompleted: 90,
			CeilingCancelFunc:    func() { cancelCalls.Add(1) },
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
			t.Errorf("CeilingCancelFunc called %d times, want 0", cancelCalls.Load())
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
			Identifier:           "X-ident",
			Issue:                domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:            time.Now().UTC(),
			IssueTokensCompleted: 90,
			CeilingCancelFunc:    func() { cancelCalls.Add(1) },
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
			t.Errorf("CeilingCancelFunc called %d times, want 0", cancelCalls.Load())
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
			Identifier:           "Y-ident",
			Issue:                domain.Issue{ID: "Y", Identifier: "Y-ident"},
			StartedAt:            time.Now().UTC(),
			IssueTokensCompleted: 90,
			CeilingCancelFunc:    func() { yCancels.Add(1) },
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
			t.Errorf("Y's CeilingCancelFunc called %d times, want 0", yCancels.Load())
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
			Identifier:           "X-ident",
			Issue:                domain.Issue{ID: "X", Identifier: "X-ident"},
			StartedAt:            time.Now().UTC(),
			IssueTokensCompleted: 90,
			CeilingCancelFunc:    func() { xCancels.Add(1) },
		}
		o.state.Running["Y"] = &RunningEntry{
			Identifier:           "Y-ident",
			Issue:                domain.Issue{ID: "Y", Identifier: "Y-ident"},
			StartedAt:            time.Now().UTC(),
			IssueTokensCompleted: 90,
			CeilingCancelFunc:    func() { yCancels.Add(1) },
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
			t.Errorf("Y's CeilingCancelFunc called %d times, want 1", yCancels.Load())
		}
		if xCancels.Load() != 0 {
			t.Errorf("X's CeilingCancelFunc called %d times, want 0", xCancels.Load())
		}
		if o.state.Running["Y"].CeilingStopRequest == nil {
			t.Error("Y's CeilingStopRequest = nil, want non-nil")
		}
		if o.state.Running["X"].CeilingStopRequest != nil {
			t.Error("X's CeilingStopRequest is non-nil, want nil")
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
			state.Running[issueY].CeilingCancelFunc = func() { yCancels.Add(1) }

			tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
			o := budgetOrchestratorWithMetrics(state, wm, store, tracker, discardLogger(), spy)

			// X's row is the trial's first run_history row, appended after
			// applyQueuedAheadOfExit has already applied (and, for Y,
			// ceiling-enforced) every message queued ahead of X's exit. The
			// hook observes Y's CeilingCancelFunc and the budget-stop counter at
			// that moment, from the loop goroutine, before Y's own exit or
			// the shutdown drain can call Y's CeilingCancelFunc again. The send
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

			t.Cleanup(func() {
				cancel()
				<-done
			})
			row := waittest.Receive(t, captured, fmt.Sprintf("trial %d: X's run_history row", trial))

			// Y exits too, so the shutdown drain finds no running entry to
			// wait for.
			o.workerExitCh <- WorkerResult{IssueID: issueY, Identifier: issueY + "-ident", ExitKind: WorkerExitNormal}
			cancel()
			<-done

			if row.yCancels != 1 {
				t.Fatalf("trial %d: Y's CeilingCancelFunc called %d times by the time X's row was upserted, want 1", trial, row.yCancels)
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

func TestApplyIssueRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, issueID, dispatchID, state, reactionKind string
		pendingCleanup, recorded                       bool
	}{
		{name: "active state", issueID: "A", dispatchID: "d1", state: "ai:in-progress", recorded: true},
		{name: "absent entry", issueID: "missing", dispatchID: "d1", state: "ai:in-progress"},
		{name: "mismatched dispatch", issueID: "A", dispatchID: "d0", state: "ai:in-progress"},
		{name: "pending cleanup", issueID: "A", dispatchID: "d1", state: "ai:in-progress", pendingCleanup: true},
		{name: "empty state", issueID: "A", dispatchID: "d1"},
		{name: "terminal state", issueID: "A", dispatchID: "d1", state: "Done"},
		{name: "non-active state", issueID: "A", dispatchID: "d1", state: "Backlog"},
		{name: "handoff without reaction kind", issueID: "A", dispatchID: "d1", state: "Handoff"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wm := &stubWorkflowManager{}
			wm.config.Tracker.ActiveStates = []string{"ai:in-progress"}
			wm.config.Tracker.HandoffState = "Handoff"
			entry := &RunningEntry{DispatchID: "d1", PendingCleanup: tt.pendingCleanup, Issue: domain.Issue{State: "ai:ready", Labels: []string{"ai:ready"}}}
			state := NewState(1000, 4, 0, nil, AgentTotals{})
			state.Running["A"] = entry
			o := &Orchestrator{state: state, workflowManager: wm}
			startedAt := time.Now()

			o.applyIssueRead(issueReadMsg{IssueID: tt.issueID, DispatchID: tt.dispatchID, Read: domain.IssueStateAndLabels{State: tt.state, Labels: []string{"ai:in-progress"}}, StartedAt: startedAt})

			got := fmt.Sprintf("%s %v %t", entry.Issue.State, entry.Issue.Labels, entry.IssueReadAt.Equal(startedAt))
			want := "ai:ready [ai:ready] false"
			if tt.recorded {
				want = tt.state + " [ai:in-progress] true"
			}
			if got != want {
				t.Errorf("applyIssueRead(%q, %q) entry = %q, want %q", tt.dispatchID, tt.state, got, want)
			}
		})
	}
}

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
		o.unaccountedTurnsCh <- unaccountedTurnsMsg{IssueID: issueID, UnaccountedTurns: 2}

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
		if entry.UnaccountedTurns != 2 {
			t.Errorf("entry.UnaccountedTurns = %d, want 2 (applied before HandleWorkerExit removed the entry)", entry.UnaccountedTurns)
		}

		state.Running[issueID] = queuedOrderingIssueEntry(issueID)
		o.applyQueuedAheadOfExit(context.Background(), map[string]struct{}{})
		if got := state.Running[issueID].TurnCount; got != 0 {
			t.Errorf("a later entry's TurnCount = %d, want 0 (no stray message left queued)", got)
		}
		if got := state.Running[issueID].UnaccountedTurns; got != 0 {
			t.Errorf("a later entry's UnaccountedTurns = %d, want 0 (no stray message left queued)", got)
		}
	})

	t.Run("workerExitCh case of drainRunningWorkers", func(t *testing.T) {
		t.Parallel()

		// The drain's own channel cases win the random select about half
		// the time, so a single trial cannot catch a missing drain.
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
			o.unaccountedTurnsCh <- unaccountedTurnsMsg{IssueID: issueID, UnaccountedTurns: 4}
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

			waittest.Receive(t, done, fmt.Sprintf("trial %d: drainRunningWorkers return", trial))

			if entry.TurnCount != 9 {
				t.Fatalf("trial %d: entry.TurnCount = %d, want 9 (applied before the drain's HandleWorkerExit removed the entry)", trial, entry.TurnCount)
			}
			if entry.UnaccountedTurns != 4 {
				t.Fatalf("trial %d: entry.UnaccountedTurns = %d, want 4 (applied before the drain's HandleWorkerExit removed the entry)", trial, entry.UnaccountedTurns)
			}
		}
	})
}

func TestOrchestrator_UnaccountedTurnsRelayWritesWithoutWorkerExit(t *testing.T) {
	t.Parallel()

	t.Run("a relayed count reaches the in-flight row inside the throttle interval", func(t *testing.T) {
		t.Parallel()

		issueID := "ISSUE-RELAY-WRITE"
		state := NewState(60000, 4, 0, nil, AgentTotals{})
		entry := queuedOrderingIssueEntry(issueID)
		entry.LastMetadataWrite = time.Now().UTC()
		state.Running[issueID] = entry
		store := &stubStore{}
		o := budgetOrchestrator(state, budgetTickConfig(0), store, &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}})
		o.drainTimeout = 100 * time.Millisecond
		written := make(chan persistence.SessionMetadata, 8)
		store.onUpsertSessionMetadata = func(meta persistence.SessionMetadata) { written <- meta }
		o.unaccountedTurnsCh <- unaccountedTurnsMsg{IssueID: issueID, UnaccountedTurns: 3}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			o.Run(ctx)
			close(done)
		}()

		t.Cleanup(func() {
			cancel()
			<-done
		})
		got := waittest.Receive(t, written, "UpsertSessionMetadata call after the relay")
		cancel()
		<-done

		if got.UnaccountedTurns != 3 || got.DispatchID != entry.DispatchID {
			t.Errorf("SessionMetadata (UnaccountedTurns, DispatchID) = (%d, %q), want (3, %q)", got.UnaccountedTurns, got.DispatchID, entry.DispatchID)
		}
	})
}

func TestOrchestrator_RunNotifiesObserversAfterAgentLaneMessage(t *testing.T) {
	t.Parallel()

	const id = "ISSUE-LANE-NOTIFY"
	tests := []struct {
		name string
		send func(o *Orchestrator)
	}{
		{"token usage event", func(o *Orchestrator) {
			o.agentEventCh <- agentEventMsg{IssueID: id, Event: tokenUsageEvent(1, 1, 2, 0)}
		}},
		{"turn start", func(o *Orchestrator) { o.turnStartedCh <- turnStartedMsg{IssueID: id, TurnsStarted: 2} }},
		{"self-review progress", func(o *Orchestrator) { o.selfReviewCh <- selfReviewProgressMsg{IssueID: id, Iteration: 1} }},
		{"unaccounted turns", func(o *Orchestrator) { o.unaccountedTurnsCh <- unaccountedTurnsMsg{IssueID: id, UnaccountedTurns: 3} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(60000, 4, 0, nil, AgentTotals{})
			state.Running[id] = queuedOrderingIssueEntry(id)
			o := budgetOrchestrator(state, budgetTickConfig(0), &stubStore{}, &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}})
			o.drainTimeout = 100 * time.Millisecond
			notified := make(chan struct{}, 16)
			o.AddObserver(observerFunc(func() { notified <- struct{}{} }))
			var running sync.WaitGroup
			running.Go(func() { o.Run(t.Context()) })
			t.Cleanup(running.Wait)

			<-notified
			_, _ = o.SnapshotFunc()()
			for len(notified) > 0 {
				<-notified
			}
			tt.send(o)

			waittest.Receive(t, notified, "observer call after the "+tt.name+", with no tick in between")
		})
	}
}

// waitForTurnCount polls snapshots until issueID reports want, failing on
// a larger value. Run applies turn-started messages asynchronously to
// snapshot requests, so a single read can race ahead.
func waitForTurnCount(t *testing.T, snapshot func() (RuntimeSnapshotResult, error), issueID string, want int) {
	t.Helper()

	waittest.Until(t, func() error {
		snap, err := snapshot()
		if err != nil {
			t.Fatalf("snapshot(): %v", err)
		}
		idx := slices.IndexFunc(snap.Running, func(e SnapshotRunningEntry) bool { return e.IssueID == issueID })
		if idx < 0 {
			t.Fatalf("snapshot has no running entry for %q", issueID)
		}
		got := snap.Running[idx].TurnCount
		if got > want {
			t.Fatalf("TurnCount = %d, want %d (observed ahead of releasing this turn)", got, want)
		}
		if got < want {
			return fmt.Errorf("TurnCount = %d, want %d", got, want)
		}
		return nil
	})
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
			wfn := o.makeWorkerFn("", "", "", "", "", nil, AttemptSettings{})

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

			waittest.Receive(t, workerDone, "worker exit")
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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, AttemptSettings{})

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		exitResult := waittest.Receive(t, o.workerExitCh, "worker exit")

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, AttemptSettings{})

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		result := waittest.Receive(t, o.workerExitCh, "worker exit result")
		if result.IssueID != issue.ID {
			t.Errorf("WorkerResult.IssueID = %q, want %q", result.IssueID, issue.ID)
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

		wfn := o.makeWorkerFn("resume-sess-42", "", "", "", "", nil, AttemptSettings{})

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		waittest.Receive(t, exitDone, "worker exit")

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, AttemptSettings{})

		exitDone := make(chan struct{})
		go func() {
			wfn(context.Background(), issue, nil)
			close(exitDone)
		}()

		waittest.Receive(t, exitDone, "worker exit")

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

		wfn := o.makeWorkerFn("", "", "", "", "", nil, AttemptSettings{})

		exitDone := make(chan struct{})
		go func() {
			wfn(withDispatchID(context.Background(), "dispatch-mkw-123"), issue, nil)
			close(exitDone)
		}()

		waittest.Receive(t, exitDone, "worker exit")

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
			wfn := o.makeWorkerFn("", "", "", "", tt.reactionKind, nil, AttemptSettings{})

			exitDone := make(chan struct{})
			go func() {
				wfn(context.Background(), issue, nil)
				close(exitDone)
			}()

			waittest.Receive(t, o.workerExitCh, "worker exit")
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

func (c *candidateTrackerAdapter) SetTrackerStates(func() domain.TrackerStates) {}

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

func TestTickLogging_TickCompletedLevelFollowsSummaryChange(t *testing.T) {
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
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var fetched []domain.Issue
	pf := passingPreflightRegistries()
	pf.ReloadWorkflow = func() error { return nil }
	pf.ConfigFunc = func() config.ServiceConfig { return cfg }

	o := NewOrchestrator(OrchestratorParams{
		State:  NewState(1000, 5, 0, nil, AgentTotals{}),
		Logger: logger,
		TrackerAdapter: &candidateTrackerAdapter{
			mockTrackerAdapter: &mockTrackerAdapter{},
			fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return fetched, nil },
		},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           &stubStore{},
		PreflightParams: pf,
	})

	for i, want := range []string{"INFO", "DEBUG", "INFO"} {
		if i == 2 {
			fetched = []domain.Issue{{ID: "1", Identifier: "T-1", State: "Backlog"}}
		}
		buf.Reset()
		o.handleTick(context.Background())
		if got := buf.String(); !strings.Contains(got, "level="+want+` msg="tick completed"`) {
			t.Errorf("tick %d: log lacks tick completed at %s: %s", i+1, want, got)
		}
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
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n < 3 {
			return fmt.Errorf("run histories = %d, want 3", n)
		}
		return nil
	})

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
			return registry.AgentMeta{UsageSources: usage.Sources{Live: tokenLive}}, true
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

	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n < 1 {
			return errors.New("no run_history record yet")
		}
		return nil
	})

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

	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		store.mu.Lock()
		defer store.mu.Unlock()
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
		if !hasOKHistory || !hasFailRetry {
			return fmt.Errorf("run histories = %d, saved retries = %d, want id-ok history and id-fail retry", len(store.runHistories), len(store.savedRetries))
		}
		return nil
	})

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

	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		store.mu.Lock()
		defer store.mu.Unlock()
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
		if !hasIP1 || !hasIP2 || !hasTD1 {
			return fmt.Errorf("run histories = %d, want ip-1, ip-2 and td-1", len(store.runHistories))
		}
		return nil
	})

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

func newHeldHarness(t *testing.T, tune func(*config.ServiceConfig), candidates ...domain.Issue) *runHarness {
	t.Helper()
	cfg := runConfig(t, map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}})
	if tune != nil {
		tune(&cfg)
	}
	h := newRunHarness(t, cfg, "kind-a")
	h.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	h.state.MaxConcurrentByState = cfg.Agent.MaxConcurrentByState
	h.tracker.candidates = candidates
	h.build()
	return h
}

func requireHeld(t *testing.T, h *runHarness, id string, reason SkipReason) *HeldEntry {
	t.Helper()
	got := h.state.Held[id]
	if got == nil || got.Reason != reason {
		t.Fatalf("State.Held[%q] = %+v, want a hold for %q", id, got, reason)
	}
	return got
}

func blockedCandidate(state string) domain.Issue {
	issue := candidate("id-1", "A-1")
	issue.BlockedBy = []domain.BlockerRef{{ID: "b-1", Identifier: "B-1", State: state}}
	return issue
}

func TestHandleTick_Held(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	oneSlot := func(cfg *config.ServiceConfig) { cfg.Agent.MaxConcurrentAgents = 1 }
	spendHeld := func(h *runHarness) {
		h.store.windowUsage = persistence.IssueTokenUsage{RecordedCostUSD: 5, PricedSessions: 1, Sessions: 1}
	}
	dailyCeiling := func(cfg *config.ServiceConfig) { cfg.Agent.MaxDailyCostUSD = 5 }
	dailyResetsAt := func(h *runHarness) time.Time {
		for _, status := range h.state.SpendCeilings {
			if status.Window == SpendWindowDaily {
				return status.ResetsAt
			}
		}
		return time.Time{}
	}

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"an open blocker holds the issue until it turns terminal", func(t *testing.T) {
			h := newHeldHarness(t, nil, blockedCandidate("In Progress"))

			h.o.handleTick(ctx)

			got := requireHeld(t, h, "id-1", SkipBlockedBy)
			if len(got.Blockers) != 1 || got.Blockers[0].Identifier != "B-1" || got.Blockers[0].State != "In Progress" {
				t.Errorf("State.Held[id-1].Blockers = %+v, want B-1 in In Progress", got.Blockers)
			}

			h.tracker.candidates = []domain.Issue{blockedCandidate("Done")}
			h.o.handleTick(ctx)

			h.waitForSessions(1)
			if len(h.state.Held) != 0 {
				t.Errorf("State.Held after the blocker turned terminal = %+v, want none", h.state.Held)
			}
		}},
		{"a full global slot limit holds the issue without reading its blockers", func(t *testing.T) {
			h := newHeldHarness(t, oneSlot, blockedCandidate("In Progress"))
			h.state.Running["r"] = &RunningEntry{Issue: candidate("r", "R-1")}
			resolver := &fakeBlockerResolver{needsReadFn: func(domain.Issue) bool { return true }}
			h.o.blockerResolver = resolver

			h.o.handleTick(ctx)

			got := requireHeld(t, h, "id-1", SkipAgentSlotsFull)
			if want := (SlotHold{Scope: SlotScopeGlobal, Used: 1, Limit: 1}); got.Slot == nil || *got.Slot != want {
				t.Errorf("State.Held[id-1].Slot = %+v, want %+v", got.Slot, want)
			}
			if len(resolver.callOrder) != 0 {
				t.Errorf("blocker reads under a full slot limit = %v, want none", resolver.callOrder)
			}

			delete(h.state.Running, "r")
			h.tracker.candidates = []domain.Issue{candidate("id-1", "A-1")}
			h.o.handleTick(ctx)

			h.waitForSessions(1)
			if len(h.state.Held) != 0 {
				t.Errorf("State.Held after a slot freed = %+v, want none", h.state.Held)
			}
		}},
		{"a full per-state slot limit holds the issue", func(t *testing.T) {
			h := newHeldHarness(t, func(cfg *config.ServiceConfig) {
				cfg.Agent.MaxConcurrentByState = map[string]int{"to do": 1}
			}, candidate("id-1", "A-1"))
			h.state.Running["r"] = &RunningEntry{Issue: candidate("r", "R-1")}

			h.o.handleTick(ctx)

			got := requireHeld(t, h, "id-1", SkipAgentSlotsFull)
			if want := (SlotHold{Scope: SlotScopeState, State: "To Do", Used: 1, Limit: 1}); got.Slot == nil || *got.Slot != want {
				t.Errorf("State.Held[id-1].Slot = %+v, want %+v", got.Slot, want)
			}
		}},
		{"a full SSH host pool holds the issue", func(t *testing.T) {
			raw := map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}}
			raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}, "max_concurrent_agents_per_host": 1}
			h := newRunHarness(t, runConfig(t, raw), "kind-a")
			h.tracker.candidates = []domain.Issue{candidate("id-1", "A-1")}
			h.build()
			h.o.applyWorkerConfig(h.wm.Config())
			if _, ok := h.o.hostPool.AcquireHost("r", ""); !ok {
				t.Fatal("AcquireHost() = false on an empty pool, want true")
			}

			h.o.handleTick(ctx)

			requireHeld(t, h, "id-1", SkipSSHHostsFull)
		}},
		{"a failed candidate fetch keeps the hold and its first-held time", func(t *testing.T) {
			h := newHeldHarness(t, nil, blockedCandidate("In Progress"))
			h.o.handleTick(ctx)
			since := requireHeld(t, h, "id-1", SkipBlockedBy).Since

			h.tracker.fetchErr = errors.New("tracker unreachable")
			h.o.handleTick(ctx)
			requireHeld(t, h, "id-1", SkipBlockedBy)

			h.tracker.fetchErr = nil
			h.o.handleTick(ctx)

			if got := requireHeld(t, h, "id-1", SkipBlockedBy).Since; !got.Equal(since) {
				t.Errorf("State.Held[id-1].Since after a failed fetch = %v, want %v", got, since)
			}
		}},
		{"an unconfirmed stage move holds the issue on its between-stages row", func(t *testing.T) {
			h := newHeldHarness(t, nil, candidate("id-1", "A-1"))
			h.state.StageHops["id-1"] = &StageHopEntry{Identifier: "A-1", TargetRule: "review", TargetLabel: "stage:review"}

			h.o.handleTick(ctx)

			requireHeld(t, h, "id-1", SkipStageHopUnconfirmed)
			snap := RuntimeSnapshot(h.state, time.Now())
			if len(snap.Held) != 0 || len(snap.BetweenStages) != 1 || snap.BetweenStages[0].Hold == nil || snap.BetweenStages[0].Hold.Reason != SkipStageHopUnconfirmed {
				t.Errorf("RuntimeSnapshot() Held, BetweenStages = %+v, %+v, want no held row and a between-stages row holding %q",
					snap.Held, snap.BetweenStages, SkipStageHopUnconfirmed)
			}
		}},
		{"a spend ceiling holds every candidate with its reset while a slot is free", func(t *testing.T) {
			h := newHeldHarness(t, dailyCeiling, candidate("id-1", "A-1"), candidate("id-2", "A-2"))
			spendHeld(h)

			h.o.handleTick(ctx)

			resetsAt := dailyResetsAt(h)
			for _, id := range []string{"id-1", "id-2"} {
				if got := requireHeld(t, h, id, SkipDailySpendCeiling); resetsAt.IsZero() || !got.ResetsAt.Equal(resetsAt) {
					t.Errorf("State.Held[%s].ResetsAt = %v, want the window's reset %v", id, got.ResetsAt, resetsAt)
				}
			}
		}},
		{"a spend ceiling outranks a full slot limit and an open blocker", func(t *testing.T) {
			h := newHeldHarness(t, func(cfg *config.ServiceConfig) { oneSlot(cfg); dailyCeiling(cfg) }, blockedCandidate("In Progress"), candidate("id-2", "A-2"))
			spendHeld(h)
			h.state.Running["r"] = &RunningEntry{Issue: candidate("r", "R-1")}

			h.o.handleTick(ctx)

			for _, id := range []string{"id-1", "id-2"} {
				requireHeld(t, h, id, SkipDailySpendCeiling)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.run(t)
		})
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
		waittest.Receive(t, o.workerExitCh, "first worker exit")
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
			waittest.Receive(t, o.workerExitCh, "worker exit")
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
			waittest.Receive(t, o.workerExitCh, "worker exit")
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

		t.Cleanup(func() {
			cancel()
			<-done
		})
		promptCaptured := func(identifier string) func() error {
			return func() error {
				if _, ok := capturedPrompts.Load(identifier); !ok {
					return fmt.Errorf("no prompt captured for %s yet", identifier)
				}
				return nil
			}
		}
		waittest.Until(t, promptCaptured("P-1"))

		tmpl2 := mustParseTemplate(t, "review {{ .issue.identifier }}")
		wm.setTemplate(tmpl2)
		issueSet.Store(1)

		waittest.Until(t, promptCaptured("P-2"))

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

		waittest.Receive(t, o.workerExitCh, "worker exit")
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
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		if got := observedMax.Load(); got != 7 {
			return fmt.Errorf("MaxConcurrentAgents = %d, want 7", got)
		}
		return nil
	})
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

func promotionGate(cfg config.ServiceConfig) error {
	return ValidateConfigForPromotion(cfg, registry.TrackerMeta{})
}

const validStateLists = `  active_states:
    - In Progress
  terminal_states:
    - Done
`

func realWorkflowContent(root, stateLists string) string {
	return `---
tracker:
  kind: mock
  api_key: test-key
` + stateLists + `polling:
  interval_ms: 60000
workspace:
  root: ` + root + `
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
}

func writeRealWorkflow(t *testing.T, path, stateLists string) {
	t.Helper()
	content := realWorkflowContent(filepath.Dir(path), stateLists)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing WORKFLOW.md: %v", err)
	}
}

func mustRealWorkflowManager(t *testing.T) (*workflow.Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	writeRealWorkflow(t, path, validStateLists)
	wm, err := workflow.NewManager(path, discardLogger(),
		workflow.WithValidateFunc(promotionGate))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return wm, path
}

func TestReconciliationGuardEndToEnd(t *testing.T) {
	t.Parallel()

	wm, workflowPath := mustRealWorkflowManager(t)

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

	// Valid YAML, but both state lists are empty, which promotion must refuse.
	writeRealWorkflow(t, workflowPath, "")

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

func TestHealthReportsInvalidReloadAndFailedPreflight(t *testing.T) {
	t.Parallel()

	wm, workflowPath := mustRealWorkflowManager(t)
	regs := passingPreflightRegistries()
	o := NewOrchestrator(OrchestratorParams{
		State:           NewState(60000, 5, 0, nil, AgentTotals{}),
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
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

	validFrom, validTo := tickWithin(o)

	clean := o.Health()
	for name, check := range map[string]SnapshotHealthCheck{
		"Database":  clean.Database,
		"Preflight": clean.Preflight.SnapshotHealthCheck,
		"Workflow":  clean.Workflow.SnapshotHealthCheck,
		"Poll":      clean.Poll.SnapshotHealthCheck,
	} {
		if check.ConsecutiveFailures != 0 || check.Error != "" || check.CheckedAt.IsZero() {
			t.Errorf("Health().%s after a clean tick = %+v, want a passing check with CheckedAt set", name, check)
		}
	}
	if clean.Poll.Outcome != "succeeded" {
		t.Errorf("Health().Poll.Outcome after a clean tick = %q, want %q", clean.Poll.Outcome, "succeeded")
	}
	if clean.SCM != nil {
		t.Errorf("Health().SCM = %+v, want nil without a provider", clean.SCM)
	}

	writeRealWorkflow(t, workflowPath, "")
	failFrom, failTo := tickWithin(o)
	tickWithin(o)

	failed := o.Health()
	if failed.Workflow.ConsecutiveFailures != 2 {
		t.Errorf("Health().Workflow.ConsecutiveFailures = %d, want 2", failed.Workflow.ConsecutiveFailures)
	}
	requireWithin(t, "Health().Workflow.LastSuccessAt", failed.Workflow.LastSuccessAt, validFrom, validTo)
	requireWithin(t, "Health().Workflow.FailingSince", failed.Workflow.FailingSince, failFrom, failTo)
	loadErr := wm.LastLoadError()
	if loadErr == nil || failed.Workflow.Error != loadErr.Error() {
		t.Errorf("Health().Workflow.Error = %q, want LastLoadError() text %v", failed.Workflow.Error, loadErr)
	}
	if failed.Preflight.ConsecutiveFailures != 2 || !strings.Contains(failed.Preflight.Error, "workflow file cannot be loaded") {
		t.Errorf("Health().Preflight = %+v, want 2 failures carrying the workflow load failure", failed.Preflight)
	}
	if failed.Poll.Outcome != "preflight_failed" || failed.Poll.Error != "" {
		t.Errorf("Health().Poll = %q with error %q, want outcome %q and no error", failed.Poll.Outcome, failed.Poll.Error, "preflight_failed")
	}

	if err := os.WriteFile(workflowPath, []byte("---\ntracker: [\n---\n"), 0o644); err != nil {
		t.Fatalf("writing malformed WORKFLOW.md: %v", err)
	}
	tickWithin(o)
	malformed := o.Health()
	if !strings.Contains(wm.LastLoadError().Error(), workflowPath) {
		t.Fatalf("LastLoadError() = %v, want the malformed file's error naming its path", wm.LastLoadError())
	}
	dir := filepath.Dir(workflowPath)
	for name, text := range map[string]string{"Workflow": malformed.Workflow.Error, "Preflight": malformed.Preflight.Error} {
		if !strings.Contains(text, "WORKFLOW.md") || strings.Contains(text, dir) {
			t.Errorf("Health().%s.Error = %q, want the base name without directory %s", name, text, dir)
		}
	}

	cfg := wm.Config()
	if !slices.Equal(cfg.Tracker.ActiveStates, []string{"In Progress"}) || !slices.Equal(cfg.Tracker.TerminalStates, []string{"Done"}) {
		t.Errorf("Config() state lists = %v and %v, want the valid file's [In Progress] and [Done]", cfg.Tracker.ActiveStates, cfg.Tracker.TerminalStates)
	}
}

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
		workflow.WithValidateFunc(promotionGate))
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

		waittest.Receive(t, done, "Run return with a pre-cancelled context and empty state")

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
		t.Cleanup(func() {
			cancel()
			<-done
		})
		waittest.Receive(t, waitCh, "workers to start")

		cancel()
		waittest.Receive(t, done, "Run return after cancellation")

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

		waittest.Receive(t, done, "Run return after the drain timeout")

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

		waittest.Receive(t, done, "Run return")
		waittest.Never(t, o.retryTimerCh, 200*time.Millisecond, "a retry timer fire after shutdown")
	})

	t.Run("drains_in_flight_triage_run", func(t *testing.T) {
		t.Parallel()

		state := NewState(60000, 1, 0, nil, AgentTotals{})
		obs, started := stateChangeSignal()
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			Observers:       []Observer{obs},
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

		waittest.Receive(t, started, "first state change")
		cancel()

		waittest.Receive(t, runReturned, "Run return after context cancellation")
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
		obs, started := stateChangeSignal()

		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			Observers:       []Observer{obs},
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

		waittest.Receive(t, started, "first state change")

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
		waittest.Receive(t, done, "Run return")
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
		obs, started := stateChangeSignal()
		o := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  &mockTrackerAdapter{},
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{},
			Store:           &stubStore{},
			Observers:       []Observer{obs},
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

		waittest.Receive(t, started, "first state change")

		cancel()
		waittest.Receive(t, done, "Run return")

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

	obs, stateChanged := stateChangeSignal()
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		Observers:       []Observer{obs},
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

	waittest.Receive(t, stateChanged, "first state change")
	select {
	case <-stateChanged:
	default:
	}

	cancel()
	waittest.Receive(t, stateChanged, "state change as shutdown starts")

	snap, err := o.SnapshotFunc()()
	if err != nil {
		t.Fatalf("SnapshotFunc() during drain: %v", err)
	}

	if snap.GeneratedAt.IsZero() {
		t.Error("GeneratedAt is zero")
	}

	waittest.Send(t, o.workerExitCh, WorkerResult{IssueID: "id-1"}, "worker exit")
	waittest.Receive(t, done, "Run return")
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

	obs, started := stateChangeSignal()
	o := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  &mockTrackerAdapter{},
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{},
		Store:           &stubStore{},
		Observers:       []Observer{obs},
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

	waittest.Receive(t, started, "first state change")

	cancel()

	o.workerExitCh <- WorkerResult{IssueID: "id-1"}

	waittest.Receive(t, done, "Run return")

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
		Router:          route.NewRouter(tracker, nil),
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
		Router:          route.NewRouter(tracker, nil),
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
		Router:          route.NewRouter(tracker, nil),
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: wm,
		Store:           store,
		PreflightParams: regs,
		Metrics:         metrics,
	})
}

func TestHandleTick_FillsRetryHoldingNoRead(t *testing.T) {
	t.Parallel()

	candidates := []domain.Issue{
		{ID: "iss-1", Identifier: "TEST-1", DisplayID: "acme/web#1", Title: "first title", URL: "https://example.test/1", IssueType: "Bug", Labels: []string{"a", "b"}, State: "To Do"},
		{ID: "iss-2", Identifier: "TEST-2", DisplayID: "acme/web#2", Title: "candidate title", URL: "https://example.test/2", IssueType: "Story", Labels: []string{"c"}, State: "In Progress"},
		{ID: "iss-4", Identifier: "TEST-4", Title: "fresh title", URL: "https://example.test/4", IssueType: "Task", Labels: []string{"new"}, State: "Done"},
	}
	partial := IssueIdentity{Title: "held title", URL: "https://example.test/held"}
	complete := IssueIdentity{Title: "read title", URL: "https://example.test/read", IssueType: "Epic", Labels: []string{}, State: "Blocked"}
	state := NewState(60000, 10, 0, nil, AgentTotals{})
	for id, entry := range map[string]*RetryEntry{
		"iss-1": {IssueID: "iss-1", Identifier: "TEST-1"},
		"iss-2": {IssueID: "iss-2", Identifier: "TEST-2", DisplayID: "acme/web#held", IssueIdentity: partial},
		"iss-3": {IssueID: "iss-3", Identifier: "TEST-3"},
		"iss-4": {IssueID: "iss-4", Identifier: "TEST-4", IssueIdentity: complete},
	} {
		state.RetryAttempts[id] = entry
		state.Claimed[id] = struct{}{}
	}
	tracker := &candidateTrackerAdapter{
		mockTrackerAdapter: &mockTrackerAdapter{},
		fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return candidates, nil },
	}

	budgetOrchestrator(state, budgetTickConfig(0), &stubStore{}, tracker).handleTick(context.Background())

	type row struct{ displayID, title, url, issueType, labels, state string }
	got := map[string]row{}
	for _, r := range RuntimeSnapshot(state, time.Now()).Retrying {
		got[r.IssueID] = row{r.DisplayID, r.IssueTitle, r.IssueURL, r.IssueType, strings.Join(r.IssueLabels, ","), r.State}
	}
	want := map[string]row{
		"iss-1": {"acme/web#1", "first title", "https://example.test/1", "Bug", "a,b", "To Do"},
		"iss-2": {"acme/web#held", "held title", "https://example.test/held", "Story", "c", "In Progress"},
		"iss-3": {},
		"iss-4": {"", "read title", "https://example.test/read", "Epic", "", "Blocked"},
	}
	if !maps.Equal(got, want) {
		t.Errorf("Retrying rows after handleTick = %+v, want %+v", got, want)
	}
}

func TestHandleTick_BudgetExhaustionRebuildsState(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-1", Identifier: "TEST-1", Title: "title", State: "To Do"}

	t.Run("store returns exhausted IDs state populated and the issue is not dispatched", func(t *testing.T) {
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
		requireLivePasses(t, "DeleteLivePass", store.deleted, []persistence.LivePass{{IssueID: issue.ID, Kind: persistence.LivePassBudget}})
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
		requireLivePasses(t, "DeleteLivePass", store.deleted, []persistence.LivePass{{IssueID: issue.ID, Kind: persistence.LivePassBudget}})
	})

	t.Run("dispatch context of a hold", func(t *testing.T) {
		t.Parallel()

		type holdContext struct {
			rule, kind, model string
			lineage           StageLineage
		}
		passAt := time.Date(2026, 3, 19, 9, 0, 0, 0, time.UTC)
		fromRun := holdContext{"plan", "kind-b", "model-run", StageLineage{PreviousRule: "specify", PreviousOutcome: "succeeded", ChainID: "chain-run", DispatchedAt: passAt}}
		fromHop := holdContext{"plan", "kind-b", "model-plan", StageLineage{PreviousRule: "specify", PreviousOutcome: "succeeded", ChainID: "chain-hop", DispatchedAt: passAt}}
		held := holdContext{"implement", "kind-c", "model-held", StageLineage{ChainID: "chain-held", DispatchedAt: passAt.Add(-time.Hour)}}
		latest := map[string]persistence.RunHistory{issue.ID: {
			RuleName: "plan", AgentAdapter: "kind-b", ConfiguredModel: "model-run", StagePrevious: "specify", StagePreviousOutcome: "succeeded",
			ChainID: "chain-run", DispatchedAt: "2026-03-19T09:00:00Z",
		}}
		waiting := persistence.StageHop{
			IssueID: issue.ID, Identifier: issue.Identifier, HopCount: 1, SourceRule: "specify", TargetRule: "plan", TargetLabel: "Stage-Plan",
			PreviousOutcome: "succeeded", ChainID: "chain-hop", HoppedAt: "2026-03-19T10:00:00Z", DispatchedAt: "2026-03-19T09:00:00Z",
		}
		dispatched := waiting
		dispatched.RunDispatched = true
		cfg := runConfig(t, map[string]any{
			"agent":  map[string]any{"kind": "kind-a", "command": "a-cmd"},
			"kind-b": map[string]any{},
			"kind-c": map[string]any{},
			"dispatch": map[string]any{"rules": []any{
				map[string]any{"name": "plan", "stage": "Stage-Plan", "agent": "kind-b", "kind-b": map[string]any{"model": "model-plan"}},
				map[string]any{"name": "implement", "stage": "stage-implement", "agent": "kind-c"},
			}},
		})
		cfg.Agent.MaxSessions = 3
		labeled := issue
		labeled.Labels = []string{"Stage-Plan"}

		tests := []struct {
			name      string
			prior     *BudgetExhaustedEntry
			hop       *persistence.StageHop
			runsErr   error
			ticks     int
			want      holdContext
			wantReads int
			wantRow   bool
		}{
			{name: "no prior record reads the latest agent run", ticks: 1, want: fromRun, wantReads: 1, wantRow: true},
			{name: "a waiting hop describes its target with no run history read", hop: &waiting, ticks: 1, want: fromHop, wantRow: true},
			{name: "a hop already followed by a run reads the latest agent run", hop: &dispatched, ticks: 1, want: fromRun, wantReads: 1, wantRow: true},
			{
				name:  "a record the retry lane described keeps its context with no read",
				prior: &BudgetExhaustedEntry{RuleName: held.rule, AgentKind: held.kind, ConfiguredModel: held.model, StageLineage: held.lineage},
				ticks: 2, want: held,
			},
			{name: "a failed read leaves no context and the next tick reads again", runsErr: errors.New("db error"), ticks: 2, wantReads: 2},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				store := &stubStore{budgetExhaustedIDs: map[string]int{issue.ID: 1}, latestRuns: latest, latestRunsErr: tt.runsErr}
				state := NewState(60000, 10, 0, nil, AgentTotals{})
				if tt.prior != nil {
					state.BudgetExhausted[issue.ID] = tt.prior
				}
				if tt.hop != nil {
					PopulateStageHops(state, []persistence.StageHop{*tt.hop}, nil)
				}
				tracker := &candidateTrackerAdapter{
					mockTrackerAdapter: &mockTrackerAdapter{},
					fetchCandidatesFn:  func(context.Context) ([]domain.Issue, error) { return []domain.Issue{labeled}, nil },
				}
				orch := budgetOrchestrator(state, &stubWorkflowManager{config: cfg}, store, tracker)

				for range tt.ticks {
					orch.handleTick(context.Background())
				}

				entry := state.BudgetExhausted[issue.ID]
				if entry == nil {
					t.Fatalf("BudgetExhausted[%s] missing after %d ticks", issue.ID, tt.ticks)
				}
				got := holdContext{entry.RuleName, entry.AgentKind, entry.ConfiguredModel, entry.StageLineage}
				if got != tt.want {
					t.Errorf("BudgetExhausted[%s] rule, kind, model, lineage = %+v, want %+v", issue.ID, got, tt.want)
				}
				if reads := len(store.latestRunsCalls); reads != tt.wantReads {
					t.Errorf("LatestAgentRuns calls = %d over %d ticks, want %d", reads, tt.ticks, tt.wantReads)
				}
				wantPasses := []persistence.LivePass{{IssueID: issue.ID, Kind: persistence.LivePassBudget, ChainID: tt.want.lineage.ChainID, RuleName: tt.want.rule, StagePrevious: tt.want.lineage.PreviousRule}}
				if !tt.wantRow {
					wantPasses = nil
				}
				requireLivePasses(t, "UpsertLivePass", store.upserted, wantPasses)
			})
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

	t.Run("the hold follows uncached tokens, not the raw total", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{
			tokenExhaustedIDs: []string{issueA.ID, issueB.ID},
			tokenExhaustedUsage: map[string]persistence.IssueTokenUsage{
				issueA.ID: {TotalTokens: 1600, UncachedTokens: 600},
				issueB.ID: {TotalTokens: 2500, UncachedTokens: 1100},
			},
		}
		state := NewState(60000, 10, 0, nil, AgentTotals{})

		budgetOrchestrator(state, budgetTickConfigTokens(0, 1000), store, candidates(issueA, issueB)).handleTick(context.Background())

		if _, held := state.BudgetExhausted[issueA.ID]; held {
			t.Errorf("BudgetExhausted[%s] present, want absent (600 uncached under 1000)", issueA.ID)
		}
		held, ok := state.BudgetExhausted[issueB.ID]
		if !ok || held.UsedTokens == nil || *held.UsedTokens != 1100 {
			t.Errorf("BudgetExhausted[%s] = %+v, want held with UsedTokens 1100", issueB.ID, held)
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

func TestHandleTick_MoneyBudgetRebuild(t *testing.T) {
	t.Parallel()

	issue := domain.Issue{ID: "iss-usd", Identifier: "TEST-USD", Title: "title", State: "To Do"}
	first, second := 0.7, 0.1
	atCeiling := persistence.IssueTokenUsage{RecordedCostUSD: first + second, PricedSessions: 2, Sessions: 2}

	tests := []struct {
		name            string
		maxTokens       int
		maxCostUSD      float64
		usage           persistence.IssueTokenUsage
		queryErr        error
		priorHold       *BudgetExhaustedEntry
		priorIncomplete bool
		wantReason      string
		wantIncomplete  bool
	}{
		{
			name:       "recorded costs summing to the ceiling hold the issue under the money reason",
			maxCostUSD: 0.8,
			usage:      atCeiling,
			wantReason: budgetReasonMoney,
		},
		{
			name:       "recorded costs one cent below the ceiling hold nothing",
			maxCostUSD: 0.8,
			usage:      persistence.IssueTokenUsage{RecordedCostUSD: 0.79, PricedSessions: 2, Sessions: 2},
		},
		{
			name:       "past the money and token ceilings the reason is money",
			maxTokens:  1000,
			maxCostUSD: 0.8,
			usage:      persistence.IssueTokenUsage{TotalTokens: 1500, UncachedTokens: 1500, RecordedCostUSD: first + second, PricedSessions: 2, Sessions: 2},
			wantReason: budgetReasonMoney,
		},
		{
			name:           "an unpriced run beside a priced run below the ceiling holds nothing and is flagged incomplete",
			maxCostUSD:     0.8,
			usage:          persistence.IssueTokenUsage{RecordedCostUSD: 0.5, PricedSessions: 1, UnpricedSessions: 1, Sessions: 2},
			wantIncomplete: true,
		},
		{
			name:       "a failed shared query folds the prior money hold forward",
			maxTokens:  1000,
			maxCostUSD: 0.8,
			queryErr:   fmt.Errorf("db error"),
			priorHold:  &BudgetExhaustedEntry{Reason: budgetReasonMoney},
			wantReason: budgetReasonMoney,
		},
		{
			name:       "a failed shared query drops a prior token hold once the token ceiling is disabled",
			maxCostUSD: 0.8,
			queryErr:   fmt.Errorf("db error"),
			priorHold:  &BudgetExhaustedEntry{Reason: budgetReasonToken},
		},
		{
			name:            "no ceiling configured resets the incomplete flags",
			priorIncomplete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wm := budgetTickConfigTokens(0, tt.maxTokens)
			wm.config.Agent.MaxCostUSD = tt.maxCostUSD
			store := &stubStore{
				tokenExhaustedUsage: map[string]persistence.IssueTokenUsage{issue.ID: tt.usage},
				tokenExhaustedErr:   tt.queryErr,
			}
			state := NewState(60000, 10, 0, nil, AgentTotals{})
			if tt.priorHold != nil {
				state.BudgetExhausted[issue.ID] = tt.priorHold
			}
			if tt.priorIncomplete {
				state.MoneyBudgetIncomplete[issue.ID] = struct{}{}
			}
			tracker := &candidateTrackerAdapter{
				mockTrackerAdapter: &mockTrackerAdapter{},
				fetchCandidatesFn:  func(_ context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
			}

			budgetOrchestrator(state, wm, store, tracker).handleTick(context.Background())
			state.TrackerOpsWg.Wait()

			entry := state.BudgetExhausted[issue.ID]
			switch {
			case tt.wantReason == "" && entry != nil:
				t.Errorf("BudgetExhausted[%s] = %+v, want absent", issue.ID, entry)
			case tt.wantReason != "" && (entry == nil || entry.Reason != tt.wantReason):
				t.Fatalf("BudgetExhausted[%s] = %+v, want reason %q", issue.ID, entry, tt.wantReason)
			case entry != nil && tt.queryErr == nil:
				requireUsedUSD(t, entry, 0.8, true)
			}
			if _, incomplete := state.MoneyBudgetIncomplete[issue.ID]; incomplete != tt.wantIncomplete {
				t.Errorf("MoneyBudgetIncomplete[%s] present = %v, want %v", issue.ID, incomplete, tt.wantIncomplete)
			}
		})
	}
}

func TestHandleTick_TokenWarningThreshold(t *testing.T) {
	t.Parallel()

	t.Run("derived from the tick's config beside MaxTokens", func(t *testing.T) {
		t.Parallel()

		wm := budgetTickConfigTokens(0, 1000)
		wm.config.Agent.TokenWarningPercent = 80
		wm.config.CIFeedback.MaxRetries = 4
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
		if got, ok := state.ReactionMaxAttempts[ReactionKindCI]; !ok || got != 4 {
			t.Errorf("state.ReactionMaxAttempts[%q] = %d, %v, want 4, true (loaded max_retries)", ReactionKindCI, got, ok)
		}
		if got, ok := state.ReactionMaxAttempts[ReactionKindAutoMerge]; ok {
			t.Errorf("state.ReactionMaxAttempts[%q] = %d, want absent at max_retries 0", ReactionKindAutoMerge, got)
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
		CacheWriteTokens:  3,
		ModelName:         "test-model",
		APIRequestCount:   2,
		UnaccountedTurns:  2,
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
		if meta.InputTokens != 10 || meta.OutputTokens != 20 || meta.TotalTokens != 30 ||
			meta.CacheReadTokens != 5 || meta.CacheWriteTokens != 3 {
			t.Errorf("SessionMetadata tokens = (%d, %d, %d, %d, %d), want (10, 20, 30, 5, 3)",
				meta.InputTokens, meta.OutputTokens, meta.TotalTokens, meta.CacheReadTokens, meta.CacheWriteTokens)
		}
		if meta.ModelName != "test-model" {
			t.Errorf("SessionMetadata.ModelName = %q, want %q", meta.ModelName, "test-model")
		}
		if meta.UnaccountedTurns != 2 {
			t.Errorf("SessionMetadata.UnaccountedTurns = %d, want 2", meta.UnaccountedTurns)
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
// invokes CeilingCancelFunc for that figure.
func TestApplyAgentEvent_TokenWarningOrderedBeforeCeilingStop(t *testing.T) {
	t.Parallel()

	o, entry := tokenWarningApplyEventOrchestrator(t, 100, 80)
	var warnedBeforeCancel bool
	entry.CeilingCancelFunc = func() { warnedBeforeCancel = entry.TokenWarningReached }

	msg := agentEventMsg{IssueID: "id-apply", Event: domain.AgentEvent{Type: domain.EventTokenUsage, Usage: domain.TokenUsage{TotalTokens: 150}}}
	o.applyAgentEvent(context.Background(), msg, true)

	if !entry.TokenWarningReached {
		t.Fatal("entry.TokenWarningReached = false, want true (150 crosses the 80 threshold)")
	}
	if entry.CeilingStopRequest == nil {
		t.Fatal("entry.CeilingStopRequest = nil, want non-nil (150 also crosses the 100 ceiling)")
	}
	if !warnedBeforeCancel {
		t.Error("CeilingCancelFunc observed TokenWarningReached = false, want the warning recorded before the ceiling cancels the run")
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
	if entry.CeilingStopRequest != nil {
		t.Error("entry.CeilingStopRequest = non-nil, want nil: enforceCeiling false must suppress the ceiling evaluation")
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
	o.drainTimeout = time.Hour

	done := make(chan struct{})
	go func() {
		o.drainRunningWorkers()
		close(done)
	}()

	o.selfReviewCh <- selfReviewProgressMsg{IssueID: "id-1", Message: "self_review_iteration", Iteration: 2, MaxIterations: 3}

	waittest.Until(t, func() error {
		reply := make(chan RuntimeSnapshotResult, 1)
		o.snapshotCh <- snapshotRequest{ReplyCh: reply}
		snap := <-reply
		if len(snap.Running) != 1 || !snap.Running[0].SelfReviewActive || snap.Running[0].SelfReviewIteration != 2 {
			return fmt.Errorf("RuntimeSnapshot().Running = %+v, want one entry with SelfReviewActive true and SelfReviewIteration 2", snap.Running)
		}
		return nil
	})

	o.workerExitCh <- WorkerResult{IssueID: "id-1", Identifier: "id-1-ident"}
	waittest.Receive(t, done, "drainRunningWorkers return")
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
	waittest.Until(t, func() error {
		if len(store.sessionWrites()) == 0 {
			return errors.New("no incremental session_metadata write during drain yet")
		}
		return nil
	})

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
	waittest.Receive(t, done, "drainRunningWorkers return")
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

		waittest.Receive(t, done, "drainRunningWorkers return")
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

		waittest.Receive(t, done, "drainRunningWorkers return")
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

		waittest.Receive(t, done, "drainRunningWorkers return with a closed AbandonCh")

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

		waittest.Receive(t, done, "drainRunningWorkers return")
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

		waittest.Receive(t, done, "drainTrackerOps return with a closed AbandonCh")

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

		// The window sits well short of trackerOpsDrainTimeout, so a drain that
		// is still waiting has entered the wait the nil arm must not skip.
		waittest.Never(t, done, 300*time.Millisecond, "drainTrackerOps return with a nil AbandonCh and in-flight work pending")
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

		waittest.Receive(t, done, "drainTriageRuns return with a closed AbandonCh")

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

		// The window sits well short of trackerOpsDrainTimeout, so a drain that
		// is still waiting has entered the wait the nil arm must not skip.
		waittest.Never(t, done, 300*time.Millisecond, "drainTriageRuns return with a nil AbandonCh and in-flight work pending")
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
		Router:          route.NewRouter(tracker, nil),
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

	pollStore := func(t *testing.T, store *stubStore, cond func(*stubStore) bool) {
		t.Helper()
		waittest.Until(t, func() error {
			store.mu.Lock()
			defer store.mu.Unlock()
			if !cond(store) {
				return errors.New("store does not hold the expected rows yet")
			}
			return nil
		})
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
		var candidateFetches atomic.Int64

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
				candidateFetches.Add(1)
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

		t.Cleanup(func() {
			cancel()
			<-done
		})
		waittest.Until(t, func() error {
			store.mu.Lock()
			defer store.mu.Unlock()
			if !handoffDone.Load() || len(store.runHistories) < 1 {
				return fmt.Errorf("handoff done = %t, run histories = %d, want a handoff and one history", handoffDone.Load(), len(store.runHistories))
			}
			return nil
		})

		waittest.Until(t, func() error {
			reply := make(chan RuntimeSnapshotResult, 1)
			waittest.Send(t, o.snapshotCh, snapshotRequest{ReplyCh: reply}, "snapshot request")
			snap := waittest.Receive(t, reply, "snapshot reply")
			if len(snap.Running) != 0 || len(snap.Retrying) != 0 {
				return fmt.Errorf("snapshot running = %d, retrying = %d, want the orchestrator to hold the issue nowhere", len(snap.Running), len(snap.Retrying))
			}
			return nil
		})

		// The fetch after the next one starts only once the tick that began
		// with the next one has finished dispatching, so the orchestrator has
		// had a full tick in which to re-dispatch the handed-off issue.
		wantFetches := candidateFetches.Load() + 2
		waittest.Until(t, func() error {
			if got := candidateFetches.Load(); got < wantFetches {
				return fmt.Errorf("candidate fetches = %d, want at least %d", got, wantFetches)
			}
			return nil
		})

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

func (s *sweepThrottleTracker) SetTrackerStates(func() domain.TrackerStates) {}

func (s *sweepThrottleTracker) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (s *sweepThrottleTracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (s *sweepThrottleTracker) FetchIssueStatesAndLabelsByIDs(context.Context, []string) (map[string]domain.IssueStateAndLabels, error) {
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
func (s *sweepThrottleTracker) AddLabel(_ context.Context, _, _ string) error     { return nil }

func (s *sweepThrottleTracker) RemoveLabel(_ context.Context, _ string, _ string) error { return nil }

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

	const clampMsg = "clamped label_commands poll_interval_ms to floor"
	cfg.AddAdvisories(config.Advisory{
		Check:   "reactions.label_commands.poll_interval_ms.clamped",
		Text:    "reactions.label_commands.poll_interval_ms is 5000, below the minimum of 30000; 30000 is used",
		Message: clampMsg,
		Attrs:   []slog.Attr{slog.Int("configured_ms", 5000), slog.Int("floor_ms", 30000)},
	})
	wm.setConfig(cfg)
	o.handleTick(ctx)
	if got := strings.Count(buf.String(), clampMsg); got != 1 {
		t.Errorf("configuration advisory count after adding it alongside the worker warning = %d, want 1\nlog:\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), warnMsg); got != 2 {
		t.Errorf("worker warning count after adding an unrelated configuration advisory = %d, want 2 (unchanged)\nlog:\n%s", got, buf.String())
	}

	o.handleTick(ctx)
	if got := strings.Count(buf.String(), clampMsg); got != 1 {
		t.Errorf("configuration advisory count after a repeat tick with the same configuration = %d, want 1 (must not repeat)\nlog:\n%s", got, buf.String())
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

	t.Cleanup(func() {
		cancel()
		<-done
	})
	waittest.Until(t, func() error {
		store.mu.Lock()
		n := len(store.runHistories)
		store.mu.Unlock()
		if n < 2 {
			return fmt.Errorf("run histories = %d, want 2", n)
		}
		return nil
	})

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
				return registry.AgentMeta{UsageSources: usage.Sources{Live: tokenLive}}, true
			case "narrowed-kind":
				return registry.AgentMeta{UsageSources: usage.Sources{Live: vendorOnlyLive}}, true
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
	if defaultEntry.UsageArrival != registry.UsageArrivalIncremental || defaultEntry.UsageAttribution != registry.UsageAttributionPerModel || !defaultEntry.CarriesTokenFigure {
		t.Errorf("default-kind entry (UsageArrival, UsageAttribution, CarriesTokenFigure) = (%q, %q, %t), want (%q, %q, true)",
			defaultEntry.UsageArrival, defaultEntry.UsageAttribution, defaultEntry.CarriesTokenFigure, registry.UsageArrivalIncremental, registry.UsageAttributionPerModel)
	}

	narrowedEntry := state.Running[narrowedIssue.ID]
	if narrowedEntry == nil {
		t.Fatal("no RunningEntry for the rule-routed issue")
	}
	if narrowedEntry.UsageArrival != registry.UsageArrivalNone || narrowedEntry.UsageAttribution != registry.UsageAttributionNone || narrowedEntry.CarriesTokenFigure {
		t.Errorf("vendor-only entry (UsageArrival, UsageAttribution, CarriesTokenFigure) = (%q, %q, %t), want (%q, %q, false)",
			narrowedEntry.UsageArrival, narrowedEntry.UsageAttribution, narrowedEntry.CarriesTokenFigure, registry.UsageArrivalNone, registry.UsageAttributionNone)
	}

	exitResults := make(map[string]WorkerResult, 2)
	for i := range 2 {
		r := waittest.Receive(t, o.workerExitCh, fmt.Sprintf("worker exit %d/2", i+1))
		exitResults[r.IssueID] = r
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

func TestHandleTick_WarnsOnceWhenAnIssueCarriesSeveralStageLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		labels          []string
		wantKind        string
		wantStageLabels []any
	}{
		{name: "two stage labels", labels: []string{"stage-implement", "stage-plan"}, wantKind: "kind-b", wantStageLabels: []any{"Stage-Plan", "stage-implement"}},
		{name: "two stage labels beside others", labels: []string{"bug", "stage-plan", "stage-implement"}, wantKind: "kind-b", wantStageLabels: []any{"Stage-Plan", "stage-implement"}},
		{name: "one stage label", labels: []string{"bug", "stage-implement"}, wantKind: "kind-c"},
		{name: "no stage label", labels: []string{"bug"}, wantKind: "kind-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
			h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1", tt.labels...)}
			h.build()

			h.o.handleTick(context.Background())
			h.o.state.WorkerWg.Wait()

			kind, _ := sessionOfWorkspace(t, h.started(), "S-1")
			if kind != tt.wantKind {
				t.Errorf("handleTick(labels %v) dispatched on adapter %q, want %q", tt.labels, kind, tt.wantKind)
			}
			warnings := logRecords(t, h.logs, severalStageLabelsMessage)
			if tt.wantStageLabels == nil {
				if len(warnings) != 0 {
					t.Fatalf("handleTick(labels %v) logged %v, want no %q record", tt.labels, warnings, severalStageLabelsMessage)
				}
				return
			}
			if len(warnings) != 1 {
				t.Fatalf("handleTick(labels %v) logged %d %q records, want 1: %v", tt.labels, len(warnings), severalStageLabelsMessage, warnings)
			}
			record := warnings[0]
			if labels, _ := record["stage_labels"].([]any); record["level"] != "WARN" || !slices.Equal(labels, tt.wantStageLabels) || record["rule_name"] != "plan" || record["issue_identifier"] != "S-1" {
				t.Errorf("handleTick(labels %v) record = %v, want a WARN for S-1 with stage_labels %v and rule_name %q", tt.labels, record, tt.wantStageLabels, "plan")
			}
		})
	}
}

func TestHandleTick_StageLineageChain(t *testing.T) {
	t.Parallel()

	const fresh = "<fresh>"
	passAt := time.Date(2026, 3, 19, 9, 0, 0, 500_000_000, time.UTC)
	hopReachedPlan := persistence.StageHop{
		IssueID: "id-1", Identifier: "S-1", HopCount: 1, SourceRule: "specify", TargetRule: "plan", TargetLabel: "Stage-Plan",
		PreviousOutcome: "succeeded", SourceDispatchID: "d0", HoppedAt: "2026-03-19T10:00:00Z", ChainID: "chain-hop",
		DispatchedAt: "2026-03-19T09:00:00.5Z",
	}
	followedByRun := hopReachedPlan
	followedByRun.RunDispatched = true
	reachedPlan := StageLineage{PreviousRule: "specify", PreviousOutcome: "succeeded", ChainID: "chain-hop", DispatchedAt: passAt}
	tests := []struct {
		name       string
		hops       []persistence.StageHop
		want       map[string]StageLineage
		wantBefore int
		wantMarked []string
		wantFirst  string
	}{
		{name: "dispatches no hop reached each start a chain", want: map[string]StageLineage{"id-1": {ChainID: fresh}, "id-2": {ChainID: fresh}}},
		{
			name: "a restored hop record hands its chain and pass time to the dispatch it reached and to no other",
			hops: []persistence.StageHop{hopReachedPlan}, want: map[string]StageLineage{"id-1": reachedPlan, "id-2": {ChainID: fresh}},
			wantBefore: 1, wantMarked: []string{"id-1"}, wantFirst: "id-1",
		},
		{
			name: "a restored hop record already followed by a run is neither listed nor marked again",
			hops: []persistence.StageHop{followedByRun}, want: map[string]StageLineage{"id-1": reachedPlan, "id-2": {ChainID: fresh}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
			PopulateStageHops(h.state, tt.hops, nil)
			h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1", "Stage-Plan"), candidate("id-2", "S-2", "Stage-Plan")}
			h.build()
			if before := len(RuntimeSnapshot(h.state, time.Now()).BetweenStages); before != tt.wantBefore {
				t.Fatalf("len(BetweenStages) before the tick = %d, want %d", before, tt.wantBefore)
			}

			h.o.handleTick(context.Background())
			h.o.state.WorkerWg.Wait()

			seen := map[string]bool{}
			for id, want := range tt.want {
				entry := h.o.state.Running[id]
				if entry == nil {
					t.Fatalf("Running[%q] missing after handleTick", id)
				}
				if entry.StartedAt.IsZero() {
					t.Fatalf("Running[%q].StartedAt is zero after handleTick", id)
				}
				if want.DispatchedAt.IsZero() {
					want.DispatchedAt = entry.StartedAt
				}
				got := entry.StageLineage
				if want.ChainID == fresh {
					if got.ChainID == entry.DispatchID || seen[got.ChainID] {
						t.Errorf("Running[%q] chain = %q, want a value that is neither the dispatch ID %q nor another entry's chain", id, got.ChainID, entry.DispatchID)
					}
					seen[got.ChainID] = true
					if got.ChainID != "" {
						got.ChainID = fresh
					}
				}
				if got != want {
					t.Errorf("Running[%q].StageLineage = %+v, want %+v", id, got, want)
				}
				if wantFirst := id == tt.wantFirst; entry.FirstAfterHop != wantFirst {
					t.Errorf("Running[%q].FirstAfterHop = %t, want %t", id, entry.FirstAfterHop, wantFirst)
				}
				wantPass := persistence.LivePass{IssueID: id, Kind: persistence.LivePassRunning, ChainID: entry.StageLineage.ChainID, RuleName: "plan", StagePrevious: want.PreviousRule}
				if !slices.Contains(h.store.upserted, wantPass) {
					t.Errorf("UpsertLivePass calls = %+v, want one for %+v", h.store.upserted, wantPass)
				}
			}
			if len(h.store.upserted) != len(tt.want) {
				t.Errorf("UpsertLivePass calls = %+v, want one per dispatch (%d)", h.store.upserted, len(tt.want))
			}
			if after := RuntimeSnapshot(h.state, time.Now()).BetweenStages; len(after) != 0 {
				t.Errorf("BetweenStages after the dispatch = %+v, want none", after)
			}
			for _, hop := range tt.hops {
				if !h.state.StageHops[hop.IssueID].RunDispatched {
					t.Errorf("StageHops[%q].RunDispatched = false after the dispatch, want true", hop.IssueID)
				}
			}
			if !slices.Equal(h.store.dispatchMarked, tt.wantMarked) {
				t.Errorf("MarkStageHopDispatched calls = %q, want %q", h.store.dispatchMarked, tt.wantMarked)
			}
		})
	}
}

func TestHandleTick_StageHopDirectReadWithoutStateHoldsTheIssue(t *testing.T) {
	t.Parallel()

	h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
	h.state.StageHops["id-1"] = &StageHopEntry{Identifier: "S-1", Count: 1, SourceRule: "specify", TargetRule: "plan", TargetLabel: "Stage-Plan"}
	h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1")}
	h.tracker.issues["id-1"] = domain.Issue{ID: "id-1", Identifier: "S-1", Title: "S-1"}
	h.build()

	h.o.handleTick(context.Background())
	h.o.state.WorkerWg.Wait()

	if _, running := h.o.state.Running["id-1"]; running {
		t.Error("handleTick() dispatched an issue whose direct read showed no state, want held")
	}
	if hop := h.o.state.StageHops["id-1"]; hop == nil || hop.TargetObserved {
		t.Errorf("handleTick() hop record = %+v, want kept and unobserved", hop)
	}
}

func TestHandleTick_ParkReleasedByLabelRemovalEndsStandingEscalation(t *testing.T) {
	t.Parallel()

	h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
	rkey := ReactionKey("id-1", ReactionKindReview)
	h.state.Parked["id-1"] = &ParkedEntry{Identifier: "S-1", Reason: parkReasonAgentBlocked, ParkedState: "To Do", Label: "needs-human", LabelApplied: true}
	h.state.PendingReactions[rkey] = &PendingReaction{
		IssueID: "id-1", Identifier: "S-1", Kind: ReactionKindReview,
		EscalatedAt: time.Now().Add(-time.Hour), EscalationTrigger: EscalationTriggerBudget, EscalationSubject: "comment-set",
	}
	h.state.ReactionAttempts[rkey] = 3
	h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1")}
	h.tracker.issues["id-1"] = candidate("id-1", "S-1")
	h.build()

	h.o.handleTick(context.Background())
	h.o.state.WorkerWg.Wait()

	sessionOfWorkspace(t, h.started(), "S-1")
	if _, ok := h.state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[review] after the park released and dispatched = present, want dropped")
	}
	if got, ok := h.state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[review] after the park released and dispatched = %d, want none", got)
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
	want := "Sortie stopped dispatching this issue.\n" +
		"Recorded: " + entry.ExhaustedAt.Format(time.RFC3339) + "\n" +
		"Ceiling: session budget (agent.max_sessions)\n" +
		"Sessions: 5 of 3\n" +
		"Raising agent.max_sessions raises this ceiling."
	if calls[0].Text != want {
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
		Router:          route.NewRouter(tracker1, nil),
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
		Router:          route.NewRouter(tracker2, nil),
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
		IssueID: "ISS-UPSERT-FAIL",
		Entry:   entry,
		Store:   store,
		Metrics: &spyMetrics{},
		Logger:  logger,
		Ctx:     context.Background(),
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

	tests := []struct {
		name  string
		setup func(t *testing.T) (pass func(), providerCalls func() int)
	}{
		{"ci", func(t *testing.T) (func(), func() int) {
			state := stateWithPendingReaction(t, "ISS-NB-CI", "feature/nb", 1)
			state.PendingReactions[ReactionKey("ISS-NB-CI", ReactionKindCI)].Triage = inFlightTriageRun("fp-nb", func() {})
			scm := defaultCISCM()
			ci := &mockCIProvider{}
			params := ciParams(t, &ciReconcileStore{}, ci, nil, scm)
			return func() { reconcileCIStatus(state, params, discardLogger(), context.Background(), newCIMetricsSpy()) },
				func() int { return scm.calls + ci.calls }
		}},
		{"review", func(t *testing.T) (func(), func() int) {
			state := stateWithReviewReaction(t, "ISS-NB-REVIEW", 10)
			state.PendingReactions[ReactionKey("ISS-NB-REVIEW", ReactionKindReview)].Triage = inFlightTriageRun("fp-nb", func() {})
			scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
			params := reviewParams(&reviewReconcileStore{}, scm, nil)
			return func() {
					reconcileReviewComments(state, params, discardLogger(), context.Background(), newReviewMetricsSpy())
				},
				func() int { return scm.calls }
		}},
		{"bot-review", func(t *testing.T) (func(), func() int) {
			state := stateWithBotReviewReaction(t, "ISS-NB-BOT", 10)
			state.PendingReactions[ReactionKey("ISS-NB-BOT", ReactionKindBotReview)].Triage = inFlightTriageRun("fp-nb", func() {})
			scm := &mockSCMAdapter{botComments: actionableBotReviewComments()}
			params := botReviewParams(&reviewReconcileStore{}, scm, nil)
			return func() {
					reconcileBotReviewComments(state, params, discardLogger(), context.Background(), newBotReviewMetricsSpy())
				},
				func() int { return scm.botCalls }
		}},
		{"merge-conflict", func(t *testing.T) (func(), func() int) {
			state := stateWithMergeConflict(t, "ISS-NB-MC", 10)
			state.PendingReactions[ReactionKey("ISS-NB-MC", ReactionKindMergeConflict)].Triage = inFlightTriageRun("fp-nb", func() {})
			scm := &mergeabilitySCM{}
			params := mergeConflictParams(newStatefulFingerprintStore(), scm, nil)
			return func() {
					reconcileMergeConflicts(state, params, discardLogger(), context.Background(), newMergeConflictMetricsSpy())
				},
				func() int { return scm.calls }
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pass, providerCalls := tt.setup(t)
			done := make(chan struct{})
			go func() {
				pass()
				close(done)
			}()
			waittest.Receive(t, done, "reconcile pass return while a triage run is in flight")
			if got := providerCalls(); got != 0 {
				t.Errorf("provider calls = %d, want 0 while a triage run is in flight", got)
			}
		})
	}
}

type sessionRecorder struct {
	adapter *startParamsRecorder
}

func newSessionRecorder(kind string) *sessionRecorder {
	return &sessionRecorder{adapter: &startParamsRecorder{mockAgentAdapter: mockAgentAdapter{
		startSessionFn: func(context.Context, domain.StartSessionParams) (domain.Session, error) {
			return domain.Session{ID: "sess-" + kind}, nil
		},
	}}}
}

func (r *sessionRecorder) started() []domain.StartSessionParams {
	return slices.DeleteFunc(r.adapter.recorded(), func(p domain.StartSessionParams) bool { return p.CredentialVerification })
}

func (r *sessionRecorder) verified() []domain.StartSessionParams {
	return slices.DeleteFunc(r.adapter.recorded(), func(p domain.StartSessionParams) bool { return !p.CredentialVerification })
}

type runTracker struct {
	*mockTrackerAdapter
	mu               sync.Mutex
	candidates       []domain.Issue
	issues           map[string]domain.Issue
	candidateFetches atomic.Int64
	fetchErr         error
}

func (r *runTracker) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	r.candidateFetches.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.candidates), r.fetchErr
}

func (r *runTracker) SetTrackerStates(func() domain.TrackerStates) {}

func (r *runTracker) FetchIssueByID(_ context.Context, id string) (domain.Issue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	issue, ok := r.issues[id]
	if !ok {
		return domain.Issue{}, &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "no such issue"}
	}
	return issue, nil
}

type runHarness struct {
	t         *testing.T
	wm        *stubWorkflowManager
	state     *State
	tracker   *runTracker
	logs      *lockedBuf
	recorders map[string]*sessionRecorder
	store     *stubStore
	metrics   domain.Metrics
	meta      map[string]registry.AgentMeta

	orchestratorStore OrchestratorStore

	mu          sync.Mutex
	unavailable map[string]bool

	o      *Orchestrator
	cancel context.CancelFunc
	done   chan struct{}
}

func runConfig(t *testing.T, raw map[string]any) config.ServiceConfig {
	t.Helper()
	cfg := convertedConfig(t, raw)
	cfg.Tracker.Kind = "mock"
	cfg.Tracker.APIKey = "test-key"
	cfg.Tracker.ActiveStates = []string{"To Do"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	cfg.Tracker.HandoffState = "In Review"
	cfg.Workspace.Root = t.TempDir()
	cfg.Polling.IntervalMS = 60000
	cfg.Agent.MaxTurns = 1
	cfg.Agent.MaxRetryBackoffMS = 50
	return cfg
}

func newRunHarness(t *testing.T, cfg config.ServiceConfig, kinds ...string) *runHarness {
	t.Helper()
	tmpl := mustParseTemplate(t, "work on {{ .issue.identifier }}")
	h := &runHarness{
		t:           t,
		wm:          &stubWorkflowManager{config: cfg, template: tmpl, templateIndex: map[string]*prompt.Template{"/prompts/held.md": tmpl}},
		state:       NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, AgentTotals{}),
		tracker:     &runTracker{mockTrackerAdapter: &mockTrackerAdapter{}, issues: map[string]domain.Issue{}},
		logs:        &lockedBuf{},
		recorders:   map[string]*sessionRecorder{},
		store:       &stubStore{},
		unavailable: map[string]bool{},
	}
	for _, kind := range kinds {
		h.recorders[kind] = newSessionRecorder(kind)
	}
	t.Cleanup(h.stop)
	return h
}

func (h *runHarness) setAvailable(kind string, available bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unavailable[kind] = !available
}

func (h *runHarness) build() {
	h.t.Helper()
	if h.o != nil {
		return
	}
	regs := passingPreflightRegistries()
	regs.AgentRegistry = &stubAgentRegistry{
		getFunc: func(string) (registry.AgentConstructor, error) { return nil, nil },
		metaFunc: func(kind string) (registry.AgentMeta, bool) {
			meta, ok := h.meta[kind]
			return meta, ok
		},
	}
	cfg := h.wm.Config()
	store := OrchestratorStore(h.store)
	if h.orchestratorStore != nil {
		store = h.orchestratorStore
	}
	h.o = NewOrchestrator(OrchestratorParams{
		State:          h.state,
		Logger:         slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		TrackerAdapter: h.tracker,
		AgentAdapter:   h.recorders[cfg.Agent.Kind].adapter,
		AgentAdapterByKind: func(kind string) (domain.AgentAdapter, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			recorder, ok := h.recorders[kind]
			if !ok || h.unavailable[kind] {
				return nil, fmt.Errorf("agent kind %q has no constructed adapter", kind)
			}
			return recorder.adapter, nil
		},
		WorkflowManager: h.wm,
		Store:           store,
		Metrics:         h.metrics,
		PreflightParams: PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      h.wm.Config,
			TrackerRegistry: regs.TrackerRegistry,
			AgentRegistry:   regs.AgentRegistry,
		},
	})
}

func (h *runHarness) run() {
	h.t.Helper()
	h.build()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.o.Run(ctx)
		close(h.done)
	}()
}

func (h *runHarness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	<-h.done
	h.cancel = nil
}

func (h *runHarness) started() map[string][]domain.StartSessionParams {
	got := map[string][]domain.StartSessionParams{}
	for kind, recorder := range h.recorders {
		if sessions := recorder.started(); len(sessions) > 0 {
			got[kind] = sessions
		}
	}
	return got
}

func (h *runHarness) waitForSessions(n int) map[string][]domain.StartSessionParams {
	h.t.Helper()
	var got map[string][]domain.StartSessionParams
	waittest.Until(h.t, func() error {
		got = h.started()
		total := 0
		for _, sessions := range got {
			total += len(sessions)
		}
		if total < n {
			return fmt.Errorf("sessions started = %v, want at least %d; log: %s", got, n, h.logs.String())
		}
		return nil
	})
	return got
}

func (h *runHarness) waitForLog(message string) {
	h.t.Helper()
	waittest.Until(h.t, func() error {
		if len(logRecords(h.t, h.logs, message)) == 0 {
			return fmt.Errorf("log holds no %q record; log: %s", message, h.logs.String())
		}
		return nil
	})
}

func rawWithBackendRule(agent map[string]any, ruleAgent string) map[string]any {
	return map[string]any{
		"agent":    agent,
		"dispatch": map[string]any{"rules": []any{backendRuleRaw(ruleAgent)}},
	}
}

func withSSHHosts(raw map[string]any) {
	raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}}
}

func candidate(id, identifier string, labels ...string) domain.Issue {
	return domain.Issue{ID: id, Identifier: identifier, Title: identifier, State: "To Do", Labels: labels}
}

func sessionOfWorkspace(t *testing.T, sessions map[string][]domain.StartSessionParams, identifier string) (string, domain.StartSessionParams) {
	t.Helper()
	for kind, list := range sessions {
		for _, params := range list {
			if filepath.Base(params.WorkspacePath) == identifier {
				return kind, params
			}
		}
	}
	t.Fatalf("no session started for %s; sessions: %v", identifier, sessions)
	return "", domain.StartSessionParams{}
}

func TestRun_SessionsLaunchTheirOwnKindsCommand(t *testing.T) {
	t.Parallel()

	type want struct {
		kind, local, remote string
		argv                []string
	}
	tests := []struct {
		name   string
		raw    func() map[string]any
		issues []domain.Issue
		want   map[string]want
	}{
		{
			name: "default kind is the retired kind",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "legacy", "command": "legacy-wrapper --x"}, "legacy")
			},
			issues: []domain.Issue{candidate("id-1", "P-1"), candidate("id-2", "B-1", "backend")},
			want: map[string]want{
				"P-1": {kind: "modern", local: "legacy-wrapper --x serve", remote: "legacy-wrapper --x serve --remote"},
				"B-1": {kind: "modern", local: "legacy-wrapper --x serve", remote: "legacy-wrapper --x serve --remote"},
			},
		},
		{
			name: "another kind is the default and a rule selects the retired kind",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
			},
			issues: []domain.Issue{candidate("id-1", "P-1"), candidate("id-2", "B-1", "backend")},
			want: map[string]want{
				"P-1": {kind: "plain", local: "plain-cmd", remote: "plain-cmd"},
				"B-1": {kind: "modern", local: "legacy-cli serve", remote: "legacy-cli serve --remote"},
			},
		},
		{
			name: "the replacement kind is the default kind",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "modern", "command": "modern-cmd"}, "legacy")
			},
			issues: []domain.Issue{candidate("id-2", "B-1", "backend")},
			want:   map[string]want{"B-1": {kind: "modern", local: "modern-cmd", remote: "modern-cmd"}},
		},
		{
			name: "a rule selects a kind that no conversion covers",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "kind-b")
			},
			issues: []domain.Issue{candidate("id-1", "P-1"), candidate("id-2", "B-1", "backend")},
			want: map[string]want{
				"P-1": {kind: "plain", local: "plain-cmd", remote: "plain-cmd"},
				"B-1": {kind: "kind-b"},
			},
		},
		{
			name: "list agent.command belongs to the default kind",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": []any{"plain cmd", "--x"}}, "kind-b")
			},
			issues: []domain.Issue{candidate("id-1", "P-1"), candidate("id-2", "B-1", "backend")},
			want: map[string]want{
				"P-1": {kind: "plain", argv: []string{"plain cmd", "--x"}},
				"B-1": {kind: "kind-b"},
			},
		},
	}

	for _, tt := range tests {
		for _, remote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, remote %v", tt.name, remote), func(t *testing.T) {
				t.Parallel()

				raw := tt.raw()
				if remote {
					withSSHHosts(raw)
				}
				h := newRunHarness(t, runConfig(t, raw), "plain", "modern", "kind-b", "legacy")
				h.tracker.candidates = tt.issues

				h.run()
				sessions := h.waitForSessions(len(tt.issues))

				for identifier, w := range tt.want {
					kind, params := sessionOfWorkspace(t, sessions, identifier)
					wantCommand := w.local
					wantHost := ""
					if remote {
						wantCommand, wantHost = w.remote, "host-1"
					}
					if kind != w.kind || params.AgentConfig.Kind != w.kind {
						t.Errorf("%s ran on adapter %q with AgentConfig.Kind %q, want %q", identifier, kind, params.AgentConfig.Kind, w.kind)
					}
					if params.AgentConfig.Command != wantCommand || !slices.Equal(params.AgentConfig.CommandArgv, w.argv) {
						t.Errorf("%s AgentConfig command = %q / %q, want %q / %q", identifier, params.AgentConfig.Command, params.AgentConfig.CommandArgv, wantCommand, w.argv)
					}
					if params.SSHHost != wantHost {
						t.Errorf("%s SSHHost = %q, want %q", identifier, params.SSHHost, wantHost)
					}
				}
			})
		}
	}
}

func TestRun_CredentialNamesFollowTheGovernedKind(t *testing.T) {
	const credential = "LEGACY_KEY"

	tests := []struct {
		name          string
		raw           func() map[string]any
		set           bool
		wantNames     []string
		wantPlain     []string
		wantOptionEnv bool
		listedInPass  bool
	}{
		{
			name: "variable set",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
			},
			set:           true,
			wantNames:     []string{credential},
			wantOptionEnv: true,
		},
		{
			name: "variable unset",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
			},
			wantNames: []string{credential},
		},
		{
			name: "variable disallowed",
			raw: func() map[string]any {
				raw := rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
				raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}, "ssh_disallow_pass_env": []any{credential}}
				return raw
			},
			set: true,
		},
		{
			name: "conversion that governs nothing carries no name",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "modern", "command": "modern-cmd"}, "legacy")
			},
			set: true,
		},
		{
			name: "operator lists the variable and it is unset",
			raw: func() map[string]any {
				raw := rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
				raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}, "ssh_pass_env": []any{credential}}
				return raw
			},
			wantNames:    []string{credential},
			wantPlain:    []string{credential},
			listedInPass: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(credential, "secret-value")
			} else {
				unsetEnvForRun(t, credential)
			}
			raw := tt.raw()
			if _, hasWorker := raw["worker"]; !hasWorker {
				withSSHHosts(raw)
			}
			h := newRunHarness(t, runConfig(t, raw), "plain", "modern")
			h.tracker.candidates = []domain.Issue{candidate("id-1", "P-1"), candidate("id-2", "B-1", "backend")}

			h.run()
			sessions := h.waitForSessions(2)
			h.stop()

			_, plain := sessionOfWorkspace(t, sessions, "P-1")
			if !slices.Equal(plain.SSHEnvNames, tt.wantPlain) {
				t.Errorf("default kind session SSHEnvNames = %v, want %v", plain.SSHEnvNames, tt.wantPlain)
			}
			_, governed := sessionOfWorkspace(t, sessions, "B-1")
			if !slices.Equal(governed.SSHEnvNames, tt.wantNames) {
				t.Errorf("governed kind session SSHEnvNames = %v, want %v", governed.SSHEnvNames, tt.wantNames)
			}
			target := agentcore.LaunchTarget{SSHEnvNames: governed.SSHEnvNames}
			carried := len(target.SSHOptions().Env) == 1
			if carried != tt.wantOptionEnv {
				t.Errorf("SSHOptions carries the credential = %v, want %v", carried, tt.wantOptionEnv)
			}
			named := false
			for _, record := range logRecords(t, h.logs, "ssh_pass_env variable is not set or empty in the orchestrator environment") {
				named = named || record["variable"] == credential
			}
			for _, record := range logRecords(t, h.logs, "ssh_pass_env variable is disallowed by ssh_disallow_pass_env, not carrying it") {
				named = named || record["variable"] == credential
			}
			if named != tt.listedInPass {
				t.Errorf("worker ssh_pass_env advisory names %s = %v, want %v", credential, named, tt.listedInPass)
			}
		})
	}
}

func unsetEnvForRun(t *testing.T, name string) {
	t.Helper()
	if prior, ok := os.LookupEnv(name); ok {
		t.Setenv(name, prior)
	}
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%s): %v", name, err)
	}
}

func TestRun_ConversionAdvisoryIsLoggedOnceAcrossTicks(t *testing.T) {
	t.Parallel()

	cfg := runConfig(t, rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy"))
	cfg.Polling.IntervalMS = 20
	h := newRunHarness(t, cfg, "plain", "modern")

	h.run()
	waittest.Until(t, func() error {
		if fetches := h.tracker.candidateFetches.Load(); fetches < 4 {
			return fmt.Errorf("candidate fetches = %d, want at least 4 ticks", fetches)
		}
		return nil
	})
	h.stop()

	records := logRecords(t, h.logs, "agent kind was removed and its configuration was converted to the replacement kind; the conversion will be removed in a later release")
	if len(records) != 1 {
		t.Fatalf("advisory records after several ticks = %v, want exactly one", records)
	}
	if records[0]["level"] != "WARN" || records[0]["agent_kind"] != "legacy" || records[0]["replacement_kind"] != "modern" {
		t.Errorf("advisory record = %v, want a WARN carrying agent_kind %q and replacement_kind %q", records[0], "legacy", "modern")
	}
}

type retryRow struct {
	name        string
	timerOnly   bool
	raw         func() map[string]any
	frozen      RetryEntry
	issue       domain.Issue
	unavailable string
	wantKind    string
	wantCommand string
	wantArgv    []string
	wantResume  string
	wantMoved   bool
	wantMovedTo DispatchResolution

	wantStageLabels []any
}

const severalStageLabelsMessage = "several stage labels found"

func stagedRulesRaw() map[string]any {
	return map[string]any{
		"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"},
		"dispatch": map[string]any{"rules": []any{
			map[string]any{"name": "plan", "stage": "Stage-Plan", "agent": "kind-b"},
			map[string]any{"name": "implement", "stage": "stage-implement", "agent": "kind-c"},
		}},
	}
}

func seedRetry(h *runHarness, mode string, entry RetryEntry) {
	h.t.Helper()
	entry.IssueID, entry.Identifier = "id-retry", "R-1"
	h.state.Claimed[entry.IssueID] = struct{}{}
	if mode == "recovered" {
		session := entry.SessionID
		PopulateRetries(h.state, []persistence.PendingRetry{{Entry: persistence.RetryEntry{
			IssueID: entry.IssueID, Identifier: entry.Identifier, Attempt: entry.Attempt, SessionID: &session,
			RuleName: entry.RuleName, TemplateID: entry.TemplateID, AgentKind: entry.AgentKind,
		}}}, nil)
		h.build()
		return
	}
	h.build()
	ScheduleRetry(h.state, ScheduleRetryParams{
		IssueID: entry.IssueID, Identifier: entry.Identifier, Attempt: entry.Attempt, DelayMS: 20,
		SessionID: entry.SessionID, AgentKind: entry.AgentKind, RuleName: entry.RuleName, TemplateID: entry.TemplateID,
		ReactionKind: entry.ReactionKind, RuleSettingsApplied: entry.RuleSettingsApplied,
	}, h.o.onRetryFire)
}

func TestRun_RetriesDispatchOnTheSelectionTheConfigurationInForceGives(t *testing.T) {
	t.Parallel()

	const held = "/prompts/held.md"
	plainDefault := func() map[string]any {
		return map[string]any{"agent": map[string]any{"kind": "kind-b", "command": "b-cmd"}}
	}
	frozenA := RetryEntry{Attempt: 1, AgentKind: "kind-a", RuleName: "old-work", SessionID: "sess-old"}

	rows := []retryRow{
		{
			name:        "kind moved off the frozen kind",
			raw:         plainDefault,
			frozen:      frozenA,
			issue:       candidate("id-retry", "R-1"),
			wantKind:    "kind-b",
			wantCommand: "b-cmd",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-b"},
		},
		{
			name:        "retired kind hand-migrated away",
			raw:         plainDefault,
			frozen:      RetryEntry{Attempt: 1, AgentKind: "legacy", RuleName: "backend", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1"),
			wantKind:    "kind-b",
			wantCommand: "b-cmd",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-b"},
		},
		{
			name:        "kind this binary does not register",
			raw:         plainDefault,
			frozen:      RetryEntry{Attempt: 1, AgentKind: "ghost", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1"),
			wantKind:    "kind-b",
			wantCommand: "b-cmd",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-b"},
		},
		{
			name: "kind only a rule reaches after agent.kind moved",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "kind-b", "command": "b-cmd"}, "kind-a")
			},
			frozen:     frozenA,
			issue:      candidate("id-retry", "R-1", "unrelated"),
			wantKind:   "kind-a",
			wantResume: "sess-old",
		},
		{
			name: "agent.command changed under the same kind",
			raw: func() map[string]any {
				return map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "new-cmd"}}
			},
			frozen:      RetryEntry{Attempt: 1, AgentKind: "kind-a", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1"),
			wantKind:    "kind-a",
			wantCommand: "new-cmd",
			wantResume:  "sess-old",
		},
		{
			name: "retired kind with a conversion record",
			raw: func() map[string]any {
				return rawWithBackendRule(map[string]any{"kind": "plain", "command": "plain-cmd"}, "legacy")
			},
			frozen:      RetryEntry{Attempt: 1, AgentKind: "legacy", RuleName: "backend", TemplateID: held, SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1", "unrelated"),
			wantKind:    "modern",
			wantCommand: "legacy-cli serve",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "modern", TemplateID: held, RuleName: "backend"},
		},
		{
			name: "template that is not held",
			raw: func() map[string]any {
				return map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}}
			},
			frozen:      RetryEntry{Attempt: 1, AgentKind: "kind-a", RuleName: "docs", TemplateID: "/prompts/gone.md", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1"),
			wantKind:    "kind-a",
			wantCommand: "a-cmd",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-a"},
		},
		{
			name:        "review reaction on a handoff issue after the kind moved",
			timerOnly:   true,
			raw:         plainDefault,
			frozen:      RetryEntry{Attempt: 1, AgentKind: "ghost", ReactionKind: ReactionKindReview, SessionID: "sess-old"},
			issue:       domain.Issue{ID: "id-retry", Identifier: "R-1", Title: "R-1", State: "In Review"},
			wantKind:    "kind-b",
			wantCommand: "b-cmd",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-b"},
		},
		{
			name:            "several stage labels on the issue when the frozen kind is gone",
			raw:             stagedRulesRaw,
			frozen:          RetryEntry{Attempt: 1, AgentKind: "ghost", RuleName: "old-work", SessionID: "sess-old"},
			issue:           candidate("id-retry", "R-1", "stage-implement", "stage-plan"),
			wantKind:        "kind-b",
			wantMoved:       true,
			wantMovedTo:     DispatchResolution{AgentKind: "kind-b", RuleName: "plan"},
			wantStageLabels: []any{"Stage-Plan", "stage-implement"},
		},
		{
			name:        "one stage label on the issue when the frozen kind is gone",
			raw:         stagedRulesRaw,
			frozen:      RetryEntry{Attempt: 1, AgentKind: "ghost", RuleName: "old-work", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1", "stage-implement"),
			wantKind:    "kind-c",
			wantMoved:   true,
			wantMovedTo: DispatchResolution{AgentKind: "kind-c", RuleName: "implement"},
		},
		{
			name:       "frozen staged selection stands after the stage label on the issue moved",
			raw:        stagedRulesRaw,
			frozen:     RetryEntry{Attempt: 1, AgentKind: "kind-b", RuleName: "plan", SessionID: "sess-old"},
			issue:      candidate("id-retry", "R-1", "stage-implement"),
			wantKind:   "kind-b",
			wantResume: "sess-old",
		},
		{
			name:       "frozen staged selection stands without a warning although the issue carries several stage labels",
			raw:        stagedRulesRaw,
			frozen:     RetryEntry{Attempt: 1, AgentKind: "kind-c", RuleName: "implement", SessionID: "sess-old"},
			issue:      candidate("id-retry", "R-1", "stage-implement", "stage-plan"),
			wantKind:   "kind-c",
			wantResume: "sess-old",
		},
		{
			name: "adapter lookup that fails and later succeeds",
			raw: func() map[string]any {
				return map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}}
			},
			frozen:      RetryEntry{Attempt: 1, AgentKind: "kind-a", SessionID: "sess-old"},
			issue:       candidate("id-retry", "R-1"),
			unavailable: "kind-a",
			wantKind:    "kind-a",
			wantCommand: "a-cmd",
			wantResume:  "sess-old",
		},
	}

	for _, tt := range rows {
		modes := []string{"timer", "recovered"}
		if tt.timerOnly {
			modes = modes[:1]
		}
		for _, mode := range modes {
			t.Run(tt.name+", "+mode, func(t *testing.T) {
				t.Parallel()

				h := newRunHarness(t, runConfig(t, tt.raw()), "kind-a", "kind-b", "kind-c", "plain", "modern")
				h.tracker.issues["id-retry"] = tt.issue
				if tt.unavailable != "" {
					h.setAvailable(tt.unavailable, false)
				}
				seedRetry(h, mode, tt.frozen)

				h.run()
				if tt.unavailable != "" {
					h.waitForLog("retry agent kind unavailable")
					h.setAvailable(tt.unavailable, true)
				}
				sessions := h.waitForSessions(1)
				h.stop()

				kind, params := sessionOfWorkspace(t, sessions, "R-1")
				if kind != tt.wantKind || params.AgentConfig.Kind != tt.wantKind {
					t.Errorf("retry ran on adapter %q with AgentConfig.Kind %q, want %q", kind, params.AgentConfig.Kind, tt.wantKind)
				}
				if params.AgentConfig.Command != tt.wantCommand || !slices.Equal(params.AgentConfig.CommandArgv, tt.wantArgv) {
					t.Errorf("retry AgentConfig command = %q / %q, want %q / %q", params.AgentConfig.Command, params.AgentConfig.CommandArgv, tt.wantCommand, tt.wantArgv)
				}
				if params.ResumeSessionID != tt.wantResume {
					t.Errorf("retry ResumeSessionID = %q, want %q", params.ResumeSessionID, tt.wantResume)
				}

				warnings := logRecords(t, h.logs, severalStageLabelsMessage)
				if tt.wantStageLabels == nil {
					if len(warnings) != 0 {
						t.Errorf("%q records = %v, want none", severalStageLabelsMessage, warnings)
					}
				} else if len(warnings) != 1 {
					t.Errorf("%q records = %v, want exactly one", severalStageLabelsMessage, warnings)
				} else if labels, _ := warnings[0]["stage_labels"].([]any); !slices.Equal(labels, tt.wantStageLabels) || warnings[0]["rule_name"] != tt.wantMovedTo.RuleName || warnings[0]["level"] != "WARN" {
					t.Errorf("%q record = %v, want a WARN with stage_labels %v and rule_name %q", severalStageLabelsMessage, warnings[0], tt.wantStageLabels, tt.wantMovedTo.RuleName)
				}

				records := logRecords(t, h.logs, selectionChangedMessage)
				if !tt.wantMoved {
					if len(records) != 0 {
						t.Errorf("selection records = %v, want none", records)
					}
					return
				}
				if len(records) != 1 {
					t.Fatalf("selection records = %v, want exactly one", records)
				}
				got := records[0]
				if got["dispatch_agent_kind"] != tt.wantMovedTo.AgentKind || got["dispatch_template_id"] != tt.wantMovedTo.TemplateID || got["dispatch_rule_name"] != tt.wantMovedTo.RuleName ||
					got["agent_kind"] != tt.frozen.AgentKind || got["template_id"] != tt.frozen.TemplateID || got["rule_name"] != tt.frozen.RuleName {
					t.Errorf("selection record = %v, want frozen {%q %q %q} and dispatched %+v", got, tt.frozen.AgentKind, tt.frozen.TemplateID, tt.frozen.RuleName, tt.wantMovedTo)
				}
			})
		}
	}
}

func (h *runHarness) waitForRunHistories(n int) []persistence.RunHistory {
	h.t.Helper()
	var rows []persistence.RunHistory
	waittest.Until(h.t, func() error {
		h.store.mu.Lock()
		rows = slices.Clone(h.store.runHistories)
		h.store.mu.Unlock()
		if len(rows) < n {
			return fmt.Errorf("run_history rows = %d, want at least %d; log: %s", len(rows), n, h.logs.String())
		}
		return nil
	})
	return rows
}

func ruleBlock(name, label, agent string, block map[string]any) map[string]any {
	return map[string]any{"name": name, "match": map[string]any{"labels": []any{label}}, "agent": agent, agent: block}
}

func TestRun_TwoRulesOnOneKindDispatchWithTheirOwnSettings(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"agent":  map[string]any{"kind": "plain", "command": "plain-cmd"},
		"kind-b": map[string]any{"model": "top-model", "effort": "high", "keep": 1},
		"dispatch": map[string]any{"rules": []any{
			ruleBlock("cheap", "cheap", "kind-b", map[string]any{"model": "model-a", "effort": nil}),
			ruleBlock("strong", "strong", "kind-b", map[string]any{"model": "model-b"}),
		}},
	}
	h := newRunHarness(t, runConfig(t, raw), "plain", "kind-b")
	h.tracker.candidates = []domain.Issue{candidate("id-1", "C-1", "cheap"), candidate("id-2", "S-1", "strong"), candidate("id-3", "P-1")}

	h.run()
	sessions := h.waitForSessions(3)
	rows := h.waitForRunHistories(3)
	h.stop()

	tests := []struct {
		identifier, kind, rule, model, effort string
		wantSettings                          map[string]any
	}{
		{identifier: "C-1", kind: "kind-b", rule: "cheap", model: "model-a", wantSettings: map[string]any{"model": "model-a", "keep": 1}},
		{identifier: "S-1", kind: "kind-b", rule: "strong", model: "model-b", effort: "high", wantSettings: map[string]any{"model": "model-b", "effort": "high", "keep": 1}},
		{identifier: "P-1", kind: "plain", wantSettings: map[string]any{}},
	}
	dispatched := map[string]map[string]any{}
	for _, record := range logRecords(t, h.logs, "issue dispatched") {
		dispatched[record["issue_identifier"].(string)] = record
	}
	for _, tt := range tests {
		kind, params := sessionOfWorkspace(t, sessions, tt.identifier)
		gotSettings := map[string]any{}
		for _, key := range []string{"model", "effort", "keep"} {
			if value, present := params.Settings[key]; present {
				gotSettings[key] = value
			}
		}
		if kind != tt.kind || !maps.Equal(gotSettings, tt.wantSettings) {
			t.Errorf("%s working StartSession adapter, Settings = %q, %v, want %q, %v", tt.identifier, kind, gotSettings, tt.kind, tt.wantSettings)
		}
		row := rowOfIdentifier(t, rows, tt.identifier)
		if row.ConfiguredModel != tt.model || row.ConfiguredEffort != tt.effort || row.RuleName != tt.rule {
			t.Errorf("%s run_history configured model, effort, rule = %q, %q, %q, want %q, %q, %q", tt.identifier, row.ConfiguredModel, row.ConfiguredEffort, row.RuleName, tt.model, tt.effort, tt.rule)
		}
		record := dispatched[tt.identifier]
		if record["agent_kind"] != tt.kind || record["rule_name"] != tt.rule || record["model"] != tt.model || record["effort"] != tt.effort {
			t.Errorf("%s dispatch record = %v, want agent_kind %q, rule_name %q, model %q, effort %q", tt.identifier, record, tt.kind, tt.rule, tt.model, tt.effort)
		}
	}

	var verifiedModels []string
	for _, params := range h.recorders["kind-b"].verified() {
		model, _ := params.Settings["model"].(string)
		verifiedModels = append(verifiedModels, model)
	}
	slices.Sort(verifiedModels)
	if !slices.Equal(verifiedModels, []string{"model-a", "model-b"}) {
		t.Errorf("credential-verification StartSession models = %v, want each rule's model once", verifiedModels)
	}
}

func rowOfIdentifier(t *testing.T, rows []persistence.RunHistory, identifier string) persistence.RunHistory {
	t.Helper()
	for _, row := range rows {
		if row.Identifier == identifier {
			return row
		}
	}
	t.Fatalf("no run_history row for %s; rows: %+v", identifier, rows)
	return persistence.RunHistory{}
}

func interactiveModeRaw(withBadBlock bool) map[string]any {
	raw := map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}}
	if withBadBlock {
		raw["kind-a"] = map[string]any{"permission_mode": "default"}
	}
	withSSHHosts(raw)
	return raw
}

func TestHandleTick_RefusedSettingsBlockStartsNothing(t *testing.T) {
	t.Parallel()

	spy := &spyMetrics{}
	h := newRunHarness(t, runConfig(t, interactiveModeRaw(true)), "kind-a")
	h.metrics = spy
	h.meta = map[string]registry.AgentMeta{"kind-a": settingsMeta()}
	h.tracker.candidates = []domain.Issue{candidate("id-1", "A-1")}
	h.build()
	clean := runConfig(t, interactiveModeRaw(false))
	h.o.preflightParams.ConfigFunc = func() config.ServiceConfig { return clean }

	h.o.handleTick(context.Background())

	if started := h.started(); len(started) != 0 || len(h.state.Running) != 0 || len(h.store.runHistories) != 0 {
		t.Errorf("sessions, Running, run_history rows = %v, %v, %+v, want none for a refused block", started, h.state.Running, h.store.runHistories)
	}
	if used := h.o.hostPool.Snapshot()["host-1"]; used != 0 {
		t.Errorf("host-1 usage = %d, want the acquired host released", used)
	}
	if !slices.Equal(spy.dispatches, []string{outcomeError}) {
		t.Errorf("dispatch outcomes = %v, want one %q", spy.dispatches, outcomeError)
	}
	requireOneRecord(t, h.logs, "agent settings refused", map[string]any{"level": "ERROR", "agent_kind": "kind-a", "check": "kind-a.permission_mode.interactive"})

	h.wm.setConfig(clean)
	h.o.handleTick(context.Background())

	if started := h.waitForSessions(1); len(started["kind-a"]) != 1 {
		t.Errorf("sessions started after the block was fixed = %v, want the candidate dispatched on the next tick", started)
	}
}

func TestRun_RetryAppliesTheSettingsInForceAtItsStart(t *testing.T) {
	t.Parallel()

	ruleRaw := func(block map[string]any) func() map[string]any {
		return func() map[string]any {
			return map[string]any{
				"agent":  map[string]any{"kind": "kind-a", "command": "a-cmd"},
				"kind-b": map[string]any{"model": "top-model", "effort": "high"},
				"dispatch": map[string]any{"rules": []any{
					map[string]any{"name": "cheap", "match": map[string]any{"labels": []any{"cheap"}}, "agent": "kind-b", "kind-b": maps.Clone(block)},
				}},
			}
		}
	}
	frozenRule := RetryEntry{Attempt: 1, AgentKind: "kind-b", RuleName: "cheap", SessionID: "sess-old", RuleSettingsApplied: true}
	reaction := frozenRule
	reaction.ReactionKind = ReactionKindReview
	kindA := RetryEntry{Attempt: 1, AgentKind: "kind-a", SessionID: "sess-old"}
	topLevelChanged := func() map[string]any {
		return map[string]any{"agent": map[string]any{"kind": "kind-a", "command": "a-cmd"}, "kind-a": map[string]any{"model": "new-model"}}
	}

	rows := []struct {
		name                            string
		raw                             func() map[string]any
		meta                            bool
		frozen                          RetryEntry
		issue                           domain.Issue
		modes                           []string
		wantKind, wantModel, wantEffort string
		wantRefusal                     bool
	}{
		{name: "top-level block changed since the frozen attempt", raw: topLevelChanged, frozen: kindA, issue: candidate("id-retry", "R-1"), wantKind: "kind-a", wantModel: "new-model"},
		{name: "rule block in force, label since moved off the rule", raw: ruleRaw(map[string]any{"model": "rule-model"}), frozen: frozenRule, issue: candidate("id-retry", "R-1", "strong"), wantKind: "kind-b", wantModel: "rule-model", wantEffort: "high"},
		{name: "reaction continuation of a held claim", modes: []string{"timer"}, raw: ruleRaw(map[string]any{"model": "rule-model", "effort": "low"}), frozen: reaction, issue: domain.Issue{ID: "id-retry", Identifier: "R-1", Title: "R-1", State: "In Review"}, wantKind: "kind-b", wantModel: "rule-model", wantEffort: "low"},
		{name: "block a reload made refusable", modes: []string{"timer"}, meta: true, raw: func() map[string]any { return interactiveModeRaw(true) }, frozen: kindA, issue: candidate("id-retry", "R-1"), wantRefusal: true},
	}

	for _, tt := range rows {
		modes := tt.modes
		if modes == nil {
			modes = []string{"timer", "recovered"}
		}
		for _, mode := range modes {
			t.Run(tt.name+", "+mode, func(t *testing.T) {
				t.Parallel()

				h := newRunHarness(t, runConfig(t, tt.raw()), "kind-a", "kind-b")
				h.tracker.issues["id-retry"] = tt.issue
				if tt.meta {
					h.meta = map[string]registry.AgentMeta{"kind-a": settingsMeta()}
				}
				seedRetry(h, mode, tt.frozen)

				h.run()
				if tt.wantRefusal {
					h.waitForLog("retry agent settings refused")
					h.stop()
					if _, claimed := h.state.Claimed["id-retry"]; len(h.started()) != 0 || !claimed {
						t.Errorf("sessions started, claim kept = %v, %v, want none and the claim kept for a refused retry", h.started(), claimed)
					}
					return
				}
				sessions := h.waitForSessions(1)
				h.stop()

				kind, params := sessionOfWorkspace(t, sessions, "R-1")
				model, _ := params.Settings["model"].(string)
				effort, _ := params.Settings["effort"].(string)
				if kind != tt.wantKind || model != tt.wantModel || effort != tt.wantEffort || params.ResumeSessionID != "sess-old" {
					t.Errorf("retry adapter, model, effort, resume = %q, %q, %q, %q, want %q, %q, %q, %q: a settings change never clears the resume identifier",
						kind, model, effort, params.ResumeSessionID, tt.wantKind, tt.wantModel, tt.wantEffort, "sess-old")
				}
			})
		}
	}
}

func (s *stubStore) AddReactionHandedOffComments(_ context.Context, _, _ string, _ []string) error {
	return nil
}

func (s *stubStore) ListReactionHandedOffComments(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}

func TestHandleTick_FreshDispatchIsGivenPullRequestComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		configured bool
		noIdentity bool
		wantIDs    []string
	}{
		{"a configured kind with a new comment", true, false, []string{"a", "b"}},
		{"an unconfigured kind", false, false, nil},
		{"a workspace without a pull request identity", true, true, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := domain.Issue{ID: "iss-fresh-seed", Identifier: "TEST-SEED", Title: "title", State: "To Do"}
			root := t.TempDir()
			if !tt.noIdentity {
				writeRecoverySCM(t, root, issue.Identifier, domain.SCMMetadata{Branch: "feature/fix", PRNumber: 10, Owner: "owner", Repo: "repo"})
			}
			wm := budgetTickConfig(0)
			wm.config.Workspace.Root = root
			wm.template = mustParseTemplate(t, commentsPromptTemplate)
			state := NewState(60000, 10, 0, nil, AgentTotals{})
			tracker := &candidateTrackerAdapter{
				mockTrackerAdapter: &mockTrackerAdapter{},
				fetchCandidatesFn:  func(context.Context) ([]domain.Issue, error) { return []domain.Issue{issue}, nil },
			}
			regs := passingPreflightRegistries()
			regs.ReloadWorkflow = func() error { return nil }
			regs.ConfigFunc = wm.Config
			orch := NewOrchestrator(OrchestratorParams{
				State:            state,
				Logger:           discardLogger(),
				TrackerAdapter:   tracker,
				Router:           route.NewRouter(tracker, nil),
				AgentAdapter:     &mockAgentAdapter{},
				WorkflowManager:  wm,
				Store:            &stubStore{},
				PreflightParams:  regs,
				SCMAdapter:       &mockSCMAdapter{comments: []domain.ReviewComment{summaryComment("a"), inlineComment("b")}},
				ReviewConfig:     defaultReviewConfig(),
				ReviewConfigured: tt.configured,
			})

			orch.handleTick(context.Background())
			t.Cleanup(state.WorkerWg.Wait)

			running, ok := state.Running[issue.ID]
			if !ok {
				t.Fatalf("Running[%s] missing after the tick, want a dispatched run", issue.ID)
			}
			t.Cleanup(running.CancelFunc)
			if running.ReactionKind != "" {
				t.Errorf("Running[%s].ReactionKind = %q, want empty for a fresh run", issue.ID, running.ReactionKind)
			}
			got := slices.Sorted(slices.Values(continuationCommentIDs(ReactionKindReview, running.ContinuationContext)))
			if !slices.Equal(got, tt.wantIDs) {
				t.Errorf("Running[%s] carries review comments %v, want %v", issue.ID, got, tt.wantIDs)
			}
			if tt.wantIDs == nil && running.ContinuationContext != nil {
				t.Errorf("Running[%s].ContinuationContext = %v, want nil", issue.ID, running.ContinuationContext)
			}
		})
	}
}

func (s *stubStore) DeleteStageHop(context.Context, string) error { return nil }

func (s *stubStore) RecordStageHop(context.Context, persistence.StageHop) error { return nil }

func (s *stubStore) RecordRunStageResult(context.Context, int64, string, string) error { return nil }

func (s *stubStore) MarkStageHopObserved(context.Context, string) error { return nil }

func (s *stubStore) MarkStageHopDispatched(_ context.Context, issueID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatchMarked = append(s.dispatchMarked, issueID)
	return s.markDispatchedErr
}

func (s *stubStore) LatestAgentRuns(_ context.Context, issueIDs []string) (map[string]persistence.RunHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latestRunsCalls = append(s.latestRunsCalls, slices.Clone(issueIDs))
	if s.latestRunsErr != nil {
		return nil, s.latestRunsErr
	}
	runs := make(map[string]persistence.RunHistory)
	for _, id := range issueIDs {
		if run, ok := s.latestRuns[id]; ok {
			runs[id] = run
		}
	}
	return runs, nil
}

func (s *stubStore) ListStageHops(context.Context) ([]persistence.StageHop, error) {
	return nil, nil
}

func TestWriteIncrementalMetadata_LiveEstimate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		kind       string
		measured   bool
		wantCost   *float64
		wantStatus string
	}{
		{name: "priced kind", kind: "k", measured: true, wantCost: new(12.0), wantStatus: "estimated"},
		{name: "kind without rates", kind: "u", measured: true, wantStatus: "unpriced"},
		{name: "unmeasured session", kind: "k"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := openInMemoryStore(t)
			state := NewState(60000, 10, 0, nil, AgentTotals{})
			state.Rates = fixtureRates(t, 2)
			o := &Orchestrator{state: state, store: store, logger: discardLogger()}
			entry := &RunningEntry{
				AgentKind:         tt.kind,
				UsageMeasured:     tt.measured,
				AgentInputTokens:  usageS.InputTokens,
				AgentOutputTokens: usageS.OutputTokens,
				AgentTotalTokens:  usageS.TotalTokens,
			}

			o.writeIncrementalMetadata(ctx, "id-1", entry, time.Now().UTC())

			meta, found, err := store.LoadSessionMetadata(ctx, "id-1")
			if err != nil || !found {
				t.Fatalf("LoadSessionMetadata(%q) = found %v, err %v, want found", "id-1", found, err)
			}
			if !reflect.DeepEqual(meta.EstimatedCostUSD, tt.wantCost) || meta.CostStatus != tt.wantStatus {
				t.Errorf("session_metadata cost for kind %q = (%v, %q), want (%v, %q)",
					tt.kind, meta.EstimatedCostUSD, meta.CostStatus, tt.wantCost, tt.wantStatus)
			}
		})
	}
}

// TestApplyAgentEvent_WorkspaceReadyShortensLaterPaths covers a live run:
// the workspace message arrives before the agent's first tool call, so a read
// of a file inside the workspace is retained relative to it, not as a path
// of the host.
func TestApplyAgentEvent_WorkspaceReadyShortensLaterPaths(t *testing.T) {
	t.Parallel()

	o, entry := tokenWarningApplyEventOrchestrator(t, 0, 0)
	entry.events = o.state.RetainedEvents.register("d-ws")

	o.applyAgentEvent(context.Background(), agentEventMsg{IssueID: "id-apply", Workspace: "/ws/ISS-1"}, true)
	o.applyAgentEvent(context.Background(), agentEventMsg{IssueID: "id-apply", Event: domain.AgentEvent{
		Type: domain.EventToolResult, ToolName: "Read", ToolKind: domain.ToolKindRead, ToolArgument: "/ws/ISS-1/internal/foo.go",
	}}, true)

	events := entry.events.snapshot()
	if len(events) != 1 || events[0].ToolArgument != "internal/foo.go" {
		t.Fatalf("retained events = %+v, want one tool_result with ToolArgument %q", events, "internal/foo.go")
	}
}

func backWithMockAdapter(t *testing.T, recorder *sessionRecorder) {
	t.Helper()
	construct, err := registry.Agents.Get("mock")
	if err != nil {
		t.Fatalf("registry.Agents.Get(mock) error = %v", err)
	}
	adapter, err := construct()
	if err != nil {
		t.Fatalf("mock constructor error = %v", err)
	}
	recorder.adapter.startSessionFn = adapter.StartSession
	recorder.adapter.runTurnFn = adapter.RunTurn
	recorder.adapter.stopSessionFn = adapter.StopSession
}

func TestRun_UsageLimitRefusalHoldsItsKindOnly(t *testing.T) {
	t.Parallel()

	resetsAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	raw := rawWithBackendRule(map[string]any{
		"kind":                  "mock",
		"command":               "mock-cmd",
		"max_concurrent_agents": 1,
		"max_sessions":          1,
	}, "mock-b")
	raw["mock"] = map[string]any{
		"turn_outcomes":         []any{"usage_limit"},
		"usage_limit_resets_at": resetsAt.Format(time.RFC3339),
		"report_token_usage":    true,
	}
	cfg := runConfig(t, raw)
	h := newRunHarness(t, cfg, "mock", "mock-b")
	backWithMockAdapter(t, h.recorders["mock"])
	backWithMockAdapter(t, h.recorders["mock-b"])
	h.orchestratorStore = openParkStore(t)
	h.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	h.tracker.candidates = []domain.Issue{
		candidate("id-1", "A-1"),
		candidate("id-2", "A-2"),
		candidate("id-3", "B-1", "backend"),
	}

	h.run()
	snapshot := h.o.SnapshotFunc()
	var refused SnapshotRetryEntry
	waittest.Until(t, func() error {
		snap, err := snapshot()
		if err != nil {
			return err
		}
		for _, retry := range snap.Retrying {
			if retry.Identifier == "A-1" {
				refused = retry
				return nil
			}
		}
		return fmt.Errorf("no retry for A-1 yet; log: %s", h.logs.String())
	})

	if refused.DueAtMS != resetsAt.UnixMilli() || refused.Trigger != triggerUsageLimit {
		t.Errorf("A-1 retry = {DueAtMS: %d, Trigger: %q}, want {%d, %q}", refused.DueAtMS, refused.Trigger, resetsAt.UnixMilli(), triggerUsageLimit)
	}

	h.o.RefreshFunc()()
	var snap RuntimeSnapshotResult
	waittest.Until(t, func() error {
		var err error
		snap, err = snapshot()
		if err != nil {
			return err
		}
		held := slices.IndexFunc(snap.Held, func(e SnapshotHeldEntry) bool {
			return e.Identifier == "A-2" && e.Hold.Reason == SkipUsageLimit
		})
		if held < 0 || len(h.recorders["mock-b"].started()) == 0 {
			return fmt.Errorf("held = %+v, mock-b sessions = %d, want A-2 held and B-1 started; log: %s", snap.Held, len(h.recorders["mock-b"].started()), h.logs.String())
		}
		return nil
	})

	held := snap.Held[slices.IndexFunc(snap.Held, func(e SnapshotHeldEntry) bool { return e.Identifier == "A-2" })]
	if !held.Hold.ResetsAt.Equal(resetsAt) || held.Hold.AgentKind != "mock" || held.Hold.ModelOnly {
		t.Errorf("A-2 hold = %+v, want resets_at %v on agent kind %q for every model", held.Hold, resetsAt, "mock")
	}
	if len(snap.BudgetExhausted) != 0 {
		t.Errorf("budget_exhausted = %+v, want none after a refused run", snap.BudgetExhausted)
	}
}
