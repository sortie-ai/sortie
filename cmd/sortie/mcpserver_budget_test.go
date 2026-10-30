package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// Preflight resolves configured kinds through the registries even when this
// harness injects adapters directly.
const (
	fixtureBudgetAgentKind   = "mcpserver-budget-e2e-agent"
	fixtureBudgetTrackerKind = "mcpserver-budget-e2e-tracker"
)

func init() {
	registry.Agents.RegisterWithMeta(fixtureBudgetAgentKind, func() (domain.AgentAdapter, error) {
		return nil, fmt.Errorf("%s is resolved through AgentAdapterByKind, never constructed from the registry", fixtureBudgetAgentKind)
	}, registry.AgentMeta{
		UsageSources: usage.Sources{Live: usage.Source{
			Category: usage.SourceOutput,
			Strategy: usage.StrategyLive,
			Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, DuringTurn: true},
		}},
	})

	registry.Trackers.RegisterWithMeta(fixtureBudgetTrackerKind, func(map[string]any) (domain.TrackerAdapter, error) {
		return nil, fmt.Errorf("%s is resolved directly through OrchestratorParams.TrackerAdapter, never constructed from the registry", fixtureBudgetTrackerKind)
	}, registry.TrackerMeta{})
}

type budgetE2ETracker struct {
	issueID    string
	identifier string
}

var _ domain.TrackerAdapter = (*budgetE2ETracker)(nil)

func (tr *budgetE2ETracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return []domain.Issue{{
		ID:         tr.issueID,
		Identifier: tr.identifier,
		Title:      "Budget E2E fixture",
		State:      "todo",
	}}, nil
}

func (tr *budgetE2ETracker) SetTrackerStates(func() domain.TrackerStates) {}

func (tr *budgetE2ETracker) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}

func (tr *budgetE2ETracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (tr *budgetE2ETracker) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if id == tr.issueID {
			out[id] = "todo"
		}
	}
	return out, nil
}

func (tr *budgetE2ETracker) FetchIssueStatesAndLabelsByIDs(context.Context, []string) (map[string]domain.IssueStateAndLabels, error) {
	return nil, nil
}

func (tr *budgetE2ETracker) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (tr *budgetE2ETracker) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func (tr *budgetE2ETracker) TransitionIssue(_ context.Context, _ string, _ string) error { return nil }
func (tr *budgetE2ETracker) CommentIssue(_ context.Context, _ string, _ string) error    { return nil }
func (tr *budgetE2ETracker) AddLabel(_ context.Context, _ string, _ string) error        { return nil }

func (tr *budgetE2ETracker) RemoveLabel(_ context.Context, _ string, _ string) error { return nil }

type budgetE2EAgent struct {
	started         chan domain.StartSessionParams
	emitUsage       chan int64
	proceedTurn     chan struct{}
	cacheReadTokens int64
}

func newBudgetE2EAgent() *budgetE2EAgent {
	return &budgetE2EAgent{
		started:     make(chan domain.StartSessionParams, 1),
		emitUsage:   make(chan int64, 1),
		proceedTurn: make(chan struct{}),
	}
}

var _ domain.AgentAdapter = (*budgetE2EAgent)(nil)

type budgetE2ESession struct {
	credentialVerification bool
}

func (a *budgetE2EAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.CredentialVerification {
		return domain.Session{ID: "sess-budget-e2e-verify", Internal: &budgetE2ESession{credentialVerification: true}}, nil
	}
	a.started <- params
	return domain.Session{ID: "sess-budget-e2e", Internal: &budgetE2ESession{}}, nil
}

func (a *budgetE2EAgent) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if state, ok := session.Internal.(*budgetE2ESession); ok && state.credentialVerification {
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted, UsageMeasured: true}, nil
	}

	for {
		select {
		case total := <-a.emitUsage:
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventTokenUsage,
				Timestamp: time.Now().UTC(),
				Usage:     domain.TokenUsage{TotalTokens: total, CacheReadTokens: a.cacheReadTokens},
			})
		case <-a.proceedTurn:
			return domain.TurnResult{
				SessionID:     session.ID,
				ExitReason:    domain.EventTurnCompleted,
				UsageMeasured: true,
			}, nil
		case <-ctx.Done():
			return domain.TurnResult{}, ctx.Err()
		}
	}
}

func (a *budgetE2EAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

type budgetE2EWorkflowManager struct {
	config   config.ServiceConfig
	template *prompt.Template
	absPath  string
}

var _ orchestrator.WorkflowManager = (*budgetE2EWorkflowManager)(nil)

func (m *budgetE2EWorkflowManager) Config() config.ServiceConfig     { return m.config }
func (m *budgetE2EWorkflowManager) Rates() *pricing.Rates            { return nil }
func (m *budgetE2EWorkflowManager) PromptTemplate() *prompt.Template { return m.template }

func (m *budgetE2EWorkflowManager) PromptTemplateByID(id string) *prompt.Template {
	if id != "" {
		return nil
	}
	return m.template
}

func (m *budgetE2EWorkflowManager) Reload() error           { return nil }
func (m *budgetE2EWorkflowManager) WorkflowAbsPath() string { return m.absPath }

func (m *budgetE2EWorkflowManager) LoadStatus() workflow.LoadStatus { return workflow.LoadStatus{} }

func waitForSessionMetadataTotal(t *testing.T, store *persistence.Store, issueID string, want int64) persistence.SessionMetadata {
	t.Helper()
	var meta persistence.SessionMetadata
	waittest.Until(t, func() error {
		var found bool
		var err error
		meta, found, err = store.LoadSessionMetadata(t.Context(), issueID)
		if err != nil {
			t.Fatalf("LoadSessionMetadata: %v", err)
		}
		if !found || meta.TotalTokens != want {
			return fmt.Errorf("session_metadata.total_tokens: found=%v, total=%d, want %d", found, meta.TotalTokens, want)
		}
		return nil
	})
	return meta
}

type costBudgetResult struct {
	UsedTokens         int64  `json:"used_tokens"`
	UsedTokensComplete bool   `json:"used_tokens_complete"`
	RemainingTokens    *int64 `json:"remaining_tokens"`
	WarningTokens      *int64 `json:"warning_tokens"`
	WarningReached     *bool  `json:"warning_reached"`
}

func executeBudgetTool(t *testing.T, tool domain.AgentTool) costBudgetResult {
	t.Helper()
	raw, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("cost_budget Execute: %v", err)
	}
	var envelope struct {
		Success bool             `json:"success"`
		Data    costBudgetResult `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal cost_budget response %q: %v", raw, err)
	}
	if !envelope.Success {
		t.Fatalf("cost_budget response success = false: %s", raw)
	}
	return envelope.Data
}

func TestMCPServerBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "budget-e2e.db")

	rw, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := rw.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	const issueID = "budget-e2e-issue"
	const identifier = "BUDGET-E2E-1"
	const budgetTokens = 1000

	if _, err := rw.AppendRunHistory(ctx, persistence.RunHistory{
		IssueID:         issueID,
		Identifier:      identifier,
		Attempt:         1,
		AgentAdapter:    fixtureBudgetAgentKind,
		Workspace:       "/tmp/ws/BUDGET-E2E-1",
		StartedAt:       "2026-03-19T09:00:00Z",
		CompletedAt:     "2026-03-19T09:05:00Z",
		Status:          "succeeded",
		TotalTokens:     1500,
		CacheReadTokens: 900,
		TokensMeasured:  true,
	}); err != nil {
		t.Fatalf("AppendRunHistory: %v", err)
	}

	tracker := &budgetE2ETracker{issueID: issueID, identifier: identifier}
	agent := newBudgetE2EAgent()

	cfg := config.ServiceConfig{
		Polling:   config.PollingConfig{IntervalMS: 20},
		Workspace: config.WorkspaceConfig{Root: tmpDir},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Tracker: config.TrackerConfig{
			Kind:            fixtureBudgetTrackerKind,
			ActiveStates:    []string{"todo"},
			TerminalStates:  []string{"done"},
			HandoffState:    "done",
			HandoffEvidence: config.HandoffEvidenceOff,
		},
		Agent: config.AgentConfig{
			Kind:                fixtureBudgetAgentKind,
			MaxTurns:            1,
			MaxConcurrentAgents: 1,
			ReadTimeoutMS:       5000,
			TurnTimeoutMS:       30000,
			StopGraceMS:         5000,
			MaxTokens:           budgetTokens,
		},
	}

	tmpl, err := prompt.Parse("do {{ .issue.identifier }}", "test", 0)
	if err != nil {
		t.Fatalf("prompt.Parse: %v", err)
	}
	wm := &budgetE2EWorkflowManager{
		config:   cfg,
		template: tmpl,
		absPath:  filepath.Join(tmpDir, "WORKFLOW.md"),
	}

	state := orchestrator.NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, budgetTokens, nil, orchestrator.AgentTotals{})

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:           state,
		Logger:          slog.New(slog.DiscardHandler),
		TrackerAdapter:  tracker,
		WorkflowManager: wm,
		Store:           rw,
		DBPath:          dbPath,
		PreflightParams: orchestrator.PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      func() config.ServiceConfig { return cfg },
			TrackerRegistry: registry.Trackers,
			AgentRegistry:   registry.Agents,
		},
		AgentAdapterByKind: func(kind string) (domain.AgentAdapter, error) {
			if kind == fixtureBudgetAgentKind {
				return agent, nil
			}
			return nil, fmt.Errorf("unexpected agent kind %q", kind)
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		o.Run(runCtx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-runDone
	})

	startParams := waittest.Receive(t, agent.started, "StartSession call")
	if startParams.MCPConfigPath == "" {
		t.Fatal("StartSessionParams.MCPConfigPath is empty, want non-empty")
	}

	rawMCP, err := os.ReadFile(startParams.MCPConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", startParams.MCPConfigPath, err)
	}
	var mcpConfig struct {
		McpServers map[string]struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawMCP, &mcpConfig); err != nil {
		t.Fatalf("unmarshal %q: %v", startParams.MCPConfigPath, err)
	}
	entry, ok := mcpConfig.McpServers["sortie-tools"]
	if !ok {
		t.Fatalf("mcp.json %q has no sortie-tools entry", startParams.MCPConfigPath)
	}
	if len(entry.Args) == 0 {
		t.Fatal("sortie-tools entry has no args, want the --workflow argument the worker wrote")
	}
	if entry.Env["SORTIE_DISPATCH_ID"] == "" {
		t.Fatal("SORTIE_DISPATCH_ID recorded in mcp.json is empty, want non-empty")
	}

	getenv := func(key string) string { return entry.Env[key] }
	params := sessionToolParamsFromEnv(getenv, cfg, tracker)
	if params.DispatchID == "" {
		t.Fatal("sessionToolParamsFromEnv().DispatchID is empty, want the dispatch ID recorded in mcp.json")
	}
	if params.IssueID != issueID {
		t.Fatalf("sessionToolParamsFromEnv().IssueID = %q, want %q", params.IssueID, issueID)
	}

	reg, err := BuildSessionToolRegistry(ctx, slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry: %v", err)
	}
	t.Cleanup(func() {
		if reg.Store != nil {
			if err := reg.Store.Close(); err != nil {
				t.Errorf("close session tool registry store: %v", err)
			}
		}
	})
	if reg.Store == nil {
		t.Fatal("SessionToolRegistry.Store is nil, want a read-only store backing cost_budget")
	}

	costBudget, ok := reg.Registry.Get("cost_budget")
	if !ok {
		t.Fatal("cost_budget tool not registered")
	}

	before := executeBudgetTool(t, costBudget)
	if before.UsedTokens != 600 {
		t.Errorf("before the usage event: used_tokens = %d, want 600 (seeded uncached)", before.UsedTokens)
	}
	if before.UsedTokensComplete {
		t.Error("before the usage event: used_tokens_complete = true, want false")
	}

	const eventTotal int64 = 900
	agent.cacheReadTokens = 600
	agent.emitUsage <- eventTotal

	meta := waitForSessionMetadataTotal(t, reg.Store, issueID, eventTotal)
	if meta.DispatchID != params.DispatchID {
		t.Fatalf("session_metadata.dispatch_id = %q, want %q (the dispatch id recorded in mcp.json)", meta.DispatchID, params.DispatchID)
	}

	after := executeBudgetTool(t, costBudget)
	if after.UsedTokens != 900 {
		t.Errorf("after the usage event: used_tokens = %d, want 900 (completed 600 + running 300 uncached)", after.UsedTokens)
	}
	if after.RemainingTokens == nil || *after.RemainingTokens != 100 {
		t.Errorf("after the usage event: remaining_tokens = %v, want 100", after.RemainingTokens)
	}
	if !after.UsedTokensComplete {
		t.Error("after the usage event: used_tokens_complete = false, want true")
	}

	close(agent.proceedTurn)
}

func TestMCPServerBudget_WarningThreshold(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "budget-warning-e2e.db")

	rw, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	if err := rw.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := rw.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	const issueID = "budget-warning-e2e-issue"
	const identifier = "BUDGET-WARN-E2E-1"

	tracker := &budgetE2ETracker{issueID: issueID, identifier: identifier}
	agent := newBudgetE2EAgent()

	cfg := config.ServiceConfig{
		Polling:   config.PollingConfig{IntervalMS: 20},
		Workspace: config.WorkspaceConfig{Root: tmpDir},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Tracker: config.TrackerConfig{
			Kind:            fixtureBudgetTrackerKind,
			ActiveStates:    []string{"todo"},
			TerminalStates:  []string{"done"},
			HandoffState:    "done",
			HandoffEvidence: config.HandoffEvidenceOff,
		},
		Agent: config.AgentConfig{
			Kind:                fixtureBudgetAgentKind,
			MaxTurns:            1,
			MaxConcurrentAgents: 1,
			ReadTimeoutMS:       5000,
			TurnTimeoutMS:       30000,
			StopGraceMS:         5000,
			MaxTokens:           100000,
			TokenWarningPercent: 1,
		},
	}

	tmpl, err := prompt.Parse("do {{ .issue.identifier }}", "test", 0)
	if err != nil {
		t.Fatalf("prompt.Parse: %v", err)
	}
	wm := &budgetE2EWorkflowManager{
		config:   cfg,
		template: tmpl,
		absPath:  filepath.Join(tmpDir, "WORKFLOW.md"),
	}

	state := orchestrator.NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, cfg.Agent.MaxTokens, nil, orchestrator.AgentTotals{})
	state.TokenWarningThreshold = cfg.Agent.TokenWarningThreshold()
	if state.TokenWarningThreshold != 1000 {
		t.Fatalf("setup: cfg.Agent.TokenWarningThreshold() = %d, want 1000", state.TokenWarningThreshold)
	}

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:           state,
		Logger:          slog.New(slog.DiscardHandler),
		TrackerAdapter:  tracker,
		WorkflowManager: wm,
		Store:           rw,
		DBPath:          dbPath,
		PreflightParams: orchestrator.PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      func() config.ServiceConfig { return cfg },
			TrackerRegistry: registry.Trackers,
			AgentRegistry:   registry.Agents,
		},
		AgentAdapterByKind: func(kind string) (domain.AgentAdapter, error) {
			if kind == fixtureBudgetAgentKind {
				return agent, nil
			}
			return nil, fmt.Errorf("unexpected agent kind %q", kind)
		},
	})

	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		o.Run(runCtx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-runDone
	})

	startParams := waittest.Receive(t, agent.started, "StartSession call")

	rawMCP, err := os.ReadFile(startParams.MCPConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", startParams.MCPConfigPath, err)
	}
	var mcpConfig struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawMCP, &mcpConfig); err != nil {
		t.Fatalf("unmarshal %q: %v", startParams.MCPConfigPath, err)
	}
	mcpEntry, ok := mcpConfig.McpServers["sortie-tools"]
	if !ok {
		t.Fatalf("mcp.json %q has no sortie-tools entry", startParams.MCPConfigPath)
	}

	getenv := func(key string) string { return mcpEntry.Env[key] }
	params := sessionToolParamsFromEnv(getenv, cfg, tracker)
	if params.TokenWarningThreshold != 1000 {
		t.Fatalf("sessionToolParamsFromEnv().TokenWarningThreshold = %d, want 1000", params.TokenWarningThreshold)
	}

	reg, err := BuildSessionToolRegistry(ctx, slog.New(slog.DiscardHandler), params)
	if err != nil {
		t.Fatalf("BuildSessionToolRegistry: %v", err)
	}
	t.Cleanup(func() {
		if reg.Store != nil {
			if err := reg.Store.Close(); err != nil {
				t.Errorf("close session tool registry store: %v", err)
			}
		}
	})

	costBudget, ok := reg.Registry.Get("cost_budget")
	if !ok {
		t.Fatal("cost_budget tool not registered")
	}

	agent.emitUsage <- 600
	meta1 := waitForSessionMetadataTotal(t, reg.Store, issueID, 600)
	if meta1.DispatchID != params.DispatchID {
		t.Fatalf("session_metadata.dispatch_id = %q, want %q", meta1.DispatchID, params.DispatchID)
	}

	below := executeBudgetTool(t, costBudget)
	if below.UsedTokens != 600 {
		t.Errorf("below the threshold: used_tokens = %d, want 600", below.UsedTokens)
	}
	if below.WarningTokens == nil || *below.WarningTokens != 1000 {
		t.Errorf("below the threshold: warning_tokens = %v, want 1000", below.WarningTokens)
	}
	if below.WarningReached == nil || *below.WarningReached {
		t.Errorf("below the threshold: warning_reached = %v, want false", below.WarningReached)
	}

	// This figure crosses the threshold and arrives well inside
	// sessionMetadataWriteInterval of the write above: only the pending
	// write bypassing the throttle lets it reach session_metadata at all.
	agent.emitUsage <- 1100
	waitForSessionMetadataTotal(t, reg.Store, issueID, 1100)

	above := executeBudgetTool(t, costBudget)
	if above.UsedTokens != 1100 {
		t.Errorf("at the threshold: used_tokens = %d, want 1100", above.UsedTokens)
	}
	if above.WarningReached == nil || !*above.WarningReached {
		t.Errorf("at the threshold: warning_reached = %v, want true", above.WarningReached)
	}

	close(agent.proceedTurn)
}
