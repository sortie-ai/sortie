//go:build unix

package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/qualification"
	"github.com/sortie-ai/sortie/internal/workspace"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tracker/file"
)

// fixtureAgentKind is the agent kind every harness instance configures.
// The orchestrator's dispatch preflight requires cfg.Agent.Kind to
// resolve in the agent registry even though the harness always
// supplies its adapter directly through AgentAdapterByKind, so this
// package registers the kind itself instead of depending on a specific
// adapter package's own registration being loaded into the same
// binary.
const fixtureAgentKind = "qualification-e2e-fixture"

func init() {
	registry.Agents.RegisterWithMeta(fixtureAgentKind, func(map[string]any) (domain.AgentAdapter, error) {
		return nil, fmt.Errorf("%s is resolved through AgentAdapterByKind, never constructed from the registry", fixtureAgentKind)
	}, registry.AgentMeta{RequiresCommand: true})
}

// The fixture's active and non-active handoff states: the issue starts
// in the active state and, on success, reaches the non-active handoff
// state. issueID and issueIdentifier name the fixture's single issue.
const (
	activeState     = "todo"
	handoffState    = "done"
	issueID         = "sortie-e2e-1"
	issueIdentifier = "SORTIE-E2E-1"
)

// effectiveSample carries the only effective sample fields the isolated
// end-to-end harness extracts: agent.kind, agent.command, the agent
// read/turn/stall bounds, max_turns, max_sessions, max_tokens, and the
// optional agent-client-protocol mcp_config shape. Nothing else from a
// sample contract crosses into the harness.
type effectiveSample struct {
	AgentKind      string
	AgentCommand   string
	ReadTimeoutMS  int
	TurnTimeoutMS  int
	StallTimeoutMS int
	MaxTurns       int
	MaxSessions    int
	MaxTokens      int
	MCPConfigPath  string
}

// serviceConfig builds the harness's service configuration from the
// effective sample fields alone, with shorter positive test bounds that
// preserve the sample's semantics. The harness carries no hooks block,
// no notification backend, no server listener, and no network-backed
// tracker.
func serviceConfig(workspaceRoot string, sample effectiveSample) config.ServiceConfig {
	return config.ServiceConfig{
		Polling:   config.PollingConfig{IntervalMS: 20},
		Workspace: config.WorkspaceConfig{Root: workspaceRoot},
		Tracker: config.TrackerConfig{
			Kind:            "file",
			ActiveStates:    []string{activeState},
			HandoffState:    handoffState,
			HandoffEvidence: config.HandoffEvidenceOff,
			Comments:        config.TrackerCommentsConfig{},
		},
		Hooks: config.HooksConfig{},
		Agent: config.AgentConfig{
			Kind:                sample.AgentKind,
			Command:             sample.AgentCommand,
			TurnTimeoutMS:       sample.TurnTimeoutMS,
			ReadTimeoutMS:       sample.ReadTimeoutMS,
			StallTimeoutMS:      sample.StallTimeoutMS,
			MaxConcurrentAgents: 1,
			MaxTurns:            sample.MaxTurns,
			MaxSessions:         sample.MaxSessions,
			MaxTokens:           sample.MaxTokens,
		},
	}
}

// workflowManager implements [orchestrator.WorkflowManager] with the
// harness's frozen configuration and one prompt template.
type workflowManager struct {
	mu       sync.RWMutex
	config   config.ServiceConfig
	template *prompt.Template
}

func (m *workflowManager) Config() config.ServiceConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func (m *workflowManager) PromptTemplate() *prompt.Template {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.template
}

func (m *workflowManager) PromptTemplateByID(string) *prompt.Template {
	return m.PromptTemplate()
}

func (m *workflowManager) Reload() error { return nil }

func (m *workflowManager) WorkflowAbsPath() string { return "WORKFLOW.md" }

// fakeAgent is the fake protocol agent the deterministic E2E oracle
// drives: it launches a real bounded child process in its own process
// group so the exact PGID postcondition has a group to check, records
// every session lifecycle call, and performs no model traffic.
type fakeAgent struct {
	mu sync.Mutex

	startCalls int
	runCalls   int
	stopCalls  int
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{}
}

// StartSession launches a bounded fake runtime process in its own
// process group, so the run's teardown and the exact PGID postcondition
// have an attributable group.
func (a *fakeAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	cmd := exec.CommandContext(context.Background(), "sleep", "120") //nolint:gosec // a bounded fake local process the fake agent's own teardown kills
	cmd.Dir = params.WorkspacePath
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "fake agent start failed", Err: err}
	}

	a.mu.Lock()
	a.startCalls++
	a.mu.Unlock()

	return domain.Session{
		ID:       "sess-e2e-fake",
		AgentPID: strconv.Itoa(cmd.Process.Pid),
		Internal: cmd,
	}, nil
}

// RunTurn writes one marker file into the workspace as the fake agent's
// work evidence and reports a successful terminal disposition.
func (a *fakeAgent) RunTurn(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	cmd, ok := session.Internal.(*exec.Cmd)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "unexpected session internal type"}
	}
	if err := os.WriteFile(filepath.Join(cmd.Dir, "agent-work-marker"), []byte("work"), 0o600); err != nil {
		return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "fake agent work failed", Err: err}
	}
	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: time.Now().UTC(),
		Message:   "fake protocol agent completed its single bounded turn",
	})

	a.mu.Lock()
	a.runCalls++
	a.mu.Unlock()

	return domain.TurnResult{
		SessionID:  session.ID,
		ExitReason: domain.EventTurnCompleted,
	}, nil
}

// StopSession terminates the fake runtime's process group and reaps it,
// recording that the session teardown completed.
func (a *fakeAgent) StopSession(_ context.Context, session domain.Session) error {
	cmd, ok := session.Internal.(*exec.Cmd)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	_ = procutil.SignalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	_, _ = cmd.Process.Wait()

	a.mu.Lock()
	a.stopCalls++
	a.mu.Unlock()
	return nil
}

// Harness is the isolated end-to-end harness: a file tracker over a
// temporary issue file, a controlled git workspace under the same
// temporary root, a real orchestrator over a temporary store, and the
// fake protocol agent.
type Harness struct {
	tempRoot      string
	issueFile     string
	workspaceRoot string
	sample        effectiveSample
	tracker       domain.TrackerAdapter
	agent         *AdapterObserver
	manager       *workflowManager
	store         *persistence.Store
	orchestrator  *orchestrator.Orchestrator
}

// Agent returns the harness's adapter observer, the only field exposed
// outside the package.
func (h *Harness) Agent() *AdapterObserver {
	return h.agent
}

// AdapterObserver wraps the harness's agent adapter and records the two
// lifecycle facts the terminal condition and the process-group
// postcondition need: each session's captured process group and each
// completed StopSession call.
type AdapterObserver struct {
	inner domain.AgentAdapter

	mu         sync.Mutex
	pgids      []int
	sessionIDs []string
	stops      int
}

// StartSession delegates and captures the session's process-group
// leader PID, which procutil.SetProcessGroup makes the group's PGID,
// and the actual protocol session identifier.
func (o *AdapterObserver) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	session, err := o.inner.StartSession(ctx, params)
	if err != nil {
		return session, err
	}
	o.mu.Lock()
	if session.ID != "" && !slices.Contains(o.sessionIDs, session.ID) {
		o.sessionIDs = append(o.sessionIDs, session.ID)
	}
	o.mu.Unlock()
	if pid, parseErr := strconv.Atoi(session.AgentPID); parseErr == nil && pid > 0 {
		o.mu.Lock()
		if !slices.Contains(o.pgids, pid) {
			o.pgids = append(o.pgids, pid)
		}
		o.mu.Unlock()
	}
	return session, nil
}

// RunTurn delegates unchanged.
func (o *AdapterObserver) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	return o.inner.RunTurn(ctx, session, params)
}

// StopSession delegates and records the completed call.
func (o *AdapterObserver) StopSession(ctx context.Context, session domain.Session) error {
	err := o.inner.StopSession(ctx, session)
	o.mu.Lock()
	o.stops++
	o.mu.Unlock()
	return err
}

// StopObserved reports whether at least one StopSession completed.
func (o *AdapterObserver) StopObserved() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stops > 0
}

// PGIDs returns every captured process group.
func (o *AdapterObserver) PGIDs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.pgids...)
}

// SessionIDs returns every actual protocol session identifier the
// observed adapter StartSession calls returned.
func (o *AdapterObserver) SessionIDs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sessionIDs...)
}

// NewHarness assembles the deterministic harness: the fake protocol
// agent behind the same builder the live collector uses.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	return NewHarnessWithAgent(t, newFakeAgent(), "sortie-qualification-fake-agent --session-fixture")
}

// NewHarnessWithAgent assembles the harness under t.TempDir() with the
// given agent adapter and agent.command coordinate: a temporary issue
// file, a controlled git workspace, a temporary store, and the
// orchestrator wired to the file tracker and that agent. The live
// collector passes the real protocol adapter; the deterministic tests
// pass the fake protocol agent.
func NewHarnessWithAgent(t *testing.T, agent domain.AgentAdapter, agentCommand string) *Harness {
	t.Helper()

	root := t.TempDir()
	issueFile := filepath.Join(root, "issues.json")
	issueJSON := fmt.Sprintf(`[{"id":%q,"identifier":%q,"title":"qualification end-to-end fixture","description":"drive one isolated run","state":%q,"priority":2,"branch_name":"","url":"","labels":[],"assignee":"","issue_type":"task","comments":[],"blocked_by":[],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
		issueID, issueIdentifier, activeState)
	if err := os.WriteFile(issueFile, []byte(issueJSON), 0o600); err != nil {
		t.Fatalf("write issue file: %v", err)
	}

	workspaceRoot := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o750); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}
	// The controlled git workspace under the same temporary root, at
	// the path the orchestrator's workspace manager computes for the
	// fixture issue, so the agent's working directory is a git work
	// tree before launch.
	pathResult, err := workspace.ComputePath(workspaceRoot, issueIdentifier)
	if err != nil {
		t.Fatalf("compute workspace path: %v", err)
	}
	if err := os.MkdirAll(pathResult.Path, 0o750); err != nil {
		t.Fatalf("create controlled workspace: %v", err)
	}
	gitInit := exec.CommandContext(context.Background(), "git", "-C", pathResult.Path, "init") //nolint:gosec // executable is the fixed git binary; only its argument vector varies
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init the controlled workspace: %v\n%s", gitErr, out)
	}

	tracker, err := file.NewFileAdapter(map[string]any{"path": issueFile, "active_states": []string{activeState}})
	if err != nil {
		t.Fatalf("create file tracker: %v", err)
	}

	sample := effectiveSample{
		AgentKind:      fixtureAgentKind,
		AgentCommand:   agentCommand,
		ReadTimeoutMS:  5000,
		TurnTimeoutMS:  10000,
		StallTimeoutMS: 10000,
		MaxTurns:       1,
		MaxSessions:    1,
		MaxTokens:      0,
	}
	cfg := serviceConfig(workspaceRoot, sample)
	tmpl, err := prompt.Parse("Work the fixture task for {{ .issue.identifier }}.", "fixture", 0)
	if err != nil {
		t.Fatalf("parse fixture prompt template: %v", err)
	}
	manager := &workflowManager{config: cfg, template: tmpl}

	store, err := persistence.Open(context.Background(), filepath.Join(root, "e2e.db"))
	if err != nil {
		t.Fatalf("open temporary store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate temporary store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	observer := &AdapterObserver{inner: agent}
	state := orchestrator.NewState(20, 1, nil, orchestrator.AgentTotals{})
	orch := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:           state,
		Logger:          slog.New(slog.DiscardHandler),
		TrackerAdapter:  tracker,
		AgentAdapter:    observer,
		WorkflowManager: manager,
		Store:           store,
		AgentAdapterByKind: func(string) (domain.AgentAdapter, error) {
			return observer, nil
		},
		PreflightParams: orchestrator.PreflightParams{
			ReloadWorkflow:  func() error { return nil },
			ConfigFunc:      manager.Config,
			TrackerRegistry: registry.Trackers,
			AgentRegistry:   registry.Agents,
		},
	})

	return &Harness{
		tempRoot:      root,
		issueFile:     issueFile,
		workspaceRoot: workspaceRoot,
		sample:        sample,
		tracker:       tracker,
		agent:         observer,
		manager:       manager,
		store:         store,
		orchestrator:  orch,
	}
}

// TerminalCondition is the observed state of the harness's terminal
// condition, evaluated through the tracker, the run-history store, the
// runtime snapshot, and the fake agent's own StopSession observation.
type TerminalCondition struct {
	SucceededRow    bool
	HandoffReached  bool
	NoRunningEntry  bool
	NoRetryEntry    bool
	StopSessionDone bool
}

// Reached reports whether every part of the terminal condition holds.
func (c TerminalCondition) Reached() bool {
	return c.SucceededRow && c.HandoffReached && c.NoRunningEntry && c.NoRetryEntry && c.StopSessionDone
}

// ObserveTerminalCondition evaluates the terminal condition once
// against the harness's collaborators.
func ObserveTerminalCondition(t *testing.T, harness *Harness) TerminalCondition {
	t.Helper()

	condition := TerminalCondition{}

	rows, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	condition.SucceededRow = len(rows) == 1 && rows[0].Status == "succeeded"

	states, err := harness.tracker.FetchIssueStatesByIDs(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("fetch issue state: %v", err)
	}
	condition.HandoffReached = states[issueID] == handoffState

	snapshot, err := harness.orchestrator.SnapshotFunc()()
	if err != nil {
		t.Fatalf("runtime snapshot: %v", err)
	}
	condition.NoRunningEntry = len(snapshot.Running) == 0
	condition.NoRetryEntry = len(snapshot.Retrying) == 0

	condition.StopSessionDone = harness.agent.StopObserved()
	return condition
}

// TerminalRecord builds the single end-to-end record from the observed
// terminal condition. The session identifier is the actual protocol
// session the harness's adapter session held, and the identity fields
// are that session's handshake facts; only the deterministic harness
// passes its fixture values.
func TerminalRecord(condition TerminalCondition, groupClean bool, sessionID, agentName, agentVersion string) qualification.Record {
	rec := qualification.Record{
		SchemaVersion:   1,
		ObservedAt:      qualification.FixtureTime,
		Scenario:        qualification.ScenarioEndToEnd,
		Surface:         qualification.SurfaceProtocol,
		Capability:      qualification.CapabilityTurnDisposition,
		Source:          qualification.SourceProcessObservation,
		InputID:         qualification.InputE2E,
		EvidencePath:    new("/run_history/status"),
		SessionID:       new(sessionID),
		AgentName:       new(agentName),
		AgentVersion:    new(agentVersion),
		ProtocolVersion: new(1),
		Detail:          "one succeeded history row and the issue reached its handoff state",
	}
	switch {
	case condition.Reached() && groupClean:
		rec.Grade = qualification.GradeUsable
		rec.Outcome = qualification.OutcomePass
	case !condition.Reached():
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeNotObserved
	default:
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
	}
	return rec
}

// StartWorkflow starts the orchestrator loop and returns the cancel
// function and the loop-completion channel, for the live collector's
// isolated end-to-end run.
func StartWorkflow(t *testing.T, harness *Harness) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		harness.orchestrator.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(qualification.ShutdownDeadline):
		}
	})
	return cancel, runDone
}
