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
	"strings"
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

// fixtureAgentKind is the agent kind the deterministic harness configures. The
// orchestrator's dispatch preflight requires cfg.Agent.Kind to resolve in the
// agent registry even though the harness supplies its adapter directly, so this
// package registers the kind itself rather than depending on an adapter
// package's registration being loaded into the binary.
const fixtureAgentKind = "qualification-e2e-fixture"

func init() {
	registry.Agents.RegisterWithMeta(fixtureAgentKind, func(map[string]any) (domain.AgentAdapter, error) {
		return nil, fmt.Errorf("%s is resolved through AgentAdapterByKind, never constructed from the registry", fixtureAgentKind)
	}, registry.AgentMeta{RequiresCommand: true})
}

// The fixture's active and non-active handoff states, and its single issue's
// id and identifier. The issue starts active and on success reaches handoff.
const (
	activeState     = "todo"
	handoffState    = "done"
	issueID         = "sortie-e2e-1"
	issueIdentifier = "SORTIE-E2E-1"
)

// fixturePrompt is the isolated harness's only agent prompt. It states plainly
// that the workspace is empty and names the "no-change-needed" status signal:
// a prompt that omitted this left a live model searching for a task that was
// never there until its turn budget ran out, measuring whether the model gives
// up rather than the orchestrator's handoff plumbing.
const fixturePrompt = `The workspace for {{ .issue.identifier }} is intentionally empty: it holds only a git directory and an empty state directory, and no fixture task lives anywhere in it. The requested outcome already holds, so do not search for a task or make any change. Signal completion immediately by running:

    mkdir -p .sortie && echo "no-change-needed" > .sortie/status`

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

// serviceConfig builds the harness's service configuration from the effective
// sample fields alone, with shorter positive test bounds. The harness carries
// no hooks block, notification backend, server listener, or network tracker.
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
	mu           sync.RWMutex
	config       config.ServiceConfig
	template     *prompt.Template
	workflowPath string
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

// PromptTemplateByID serves the fixture's only template, the default one, and
// reports nil for any other id as a real workflow does. Answering every id with
// the default would let a dispatch asking for an undeclared template run
// anyway, hiding a resolution failure.
func (m *workflowManager) PromptTemplateByID(id string) *prompt.Template {
	if id != "" {
		return nil
	}
	return m.PromptTemplate()
}

func (m *workflowManager) Reload() error { return nil }

// WorkflowAbsPath reports the fixture's workflow path. The orchestrator resolves
// settings and MCP configuration against its directory, so a relative value
// would resolve those against whatever working directory the test binary ran in.
func (m *workflowManager) WorkflowAbsPath() string { return m.workflowPath }

// fakeAgent is the fake protocol agent the deterministic oracle drives: it
// launches a real bounded child process in its own process group so the exact
// PGID postcondition has a group to check, records every session lifecycle
// call, and performs no model traffic.
type fakeAgent struct {
	mu sync.Mutex

	startCalls int
	runCalls   int
	stopCalls  int
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{}
}

// StartSession launches a bounded fake runtime process in its own process
// group, so the run's teardown and the exact PGID postcondition have an
// attributable group.
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

// Harness is the isolated end-to-end harness: a file tracker over a temporary
// issue file, a controlled git workspace under the same temporary root, a real
// orchestrator over a temporary store, and the fake protocol agent.
type Harness struct {
	// observation bounds the wait for a terminal condition.
	observation time.Duration

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

// Observation reports how long an observer may wait for a terminal condition.
func (h *Harness) Observation() time.Duration {
	return h.observation
}

// Agent returns the harness's adapter observer, the only field exposed outside
// the package.
func (h *Harness) Agent() *AdapterObserver {
	return h.agent
}

// AdapterObserver wraps the harness's agent adapter and records the lifecycle
// facts the terminal condition and PGID postcondition need: each session's
// captured process group and each completed StopSession call.
type AdapterObserver struct {
	inner domain.AgentAdapter

	mu         sync.Mutex
	pgids      []int
	sessionIDs []string
	stops      int
}

// StartSession delegates and captures the session's process-group leader PID
// (its PGID) and the actual protocol session identifier.
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

// SessionIDs returns every actual protocol session identifier the observed
// StartSession calls returned.
func (o *AdapterObserver) SessionIDs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sessionIDs...)
}

// toolServerBinary builds the sortie binary once per test process and returns
// its path. In production the tool server's launch command is the running
// sortie; inside a test the running executable is the test binary, which speaks
// no MCP, so a runtime handed that path blocks on a stdio server that never
// answers and its session creation times out. Building the real binary keeps
// the harness measuring the operator's path. The build directory outlives the
// triggering test, since the binary is shared by later harnesses in the process.
var toolServerBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "sortie-e2e-toolserver")
	if err != nil {
		return "", fmt.Errorf("create the tool server build directory: %w", err)
	}
	binary := filepath.Join(dir, "sortie")
	root, err := filepath.Abs("../../../")
	if err != nil {
		return "", fmt.Errorf("resolve the repository root: %w", err)
	}
	// Looked up only to fail with the cause rather than a bare exec error; the
	// command itself stays a literal.
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("the Go toolchain is required to build the tool server binary: %w", err)
	}
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, "./cmd/sortie") //nolint:gosec // every argument is a literal except the output path, which this function created under a temporary directory of its own
	build.Dir = root
	// The deployment model forbids a C toolchain, so the binary is built the
	// way it ships.
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		return "", fmt.Errorf("build the tool server binary: %w\n%s", buildErr, out)
	}
	return binary, nil
})

// Budgets are the per-run bounds one harness gives the agent it drives, plus
// how long an observer may wait for a terminal condition. The deterministic
// fake agent answers in milliseconds and takes the zero value; a live runtime
// spends model time per turn and must state its own, since the fake agent's
// bounds would end a real turn before the model answered and record the run
// unobserved for a harness reason rather than a runtime one.
type Budgets struct {
	ReadTimeoutMS  int
	TurnTimeoutMS  int
	StallTimeoutMS int

	// MaxTokens is the per-issue token ceiling. Zero leaves the run unbounded;
	// a finite value is carried only by a run whose subject is the stop itself,
	// since a ceiling left in force elsewhere would end an unrelated run the
	// moment its agent reported a figure.
	MaxTokens int

	// MaxSessions is the per-issue session budget, defaulting to the one a
	// single deterministic run needs. A harness whose subject is another budget
	// raises it, so this one cannot stand in for the budget under measurement.
	MaxSessions int

	// Observation bounds the wait for a terminal condition. It is not a
	// shutdown bound: qualification.ShutdownDeadline governs that, and
	// spending one on the other gives a live run a shutdown's worth of
	// time to do a turn's work.
	Observation time.Duration
}

// withDefaults fills every unset bound with the value the deterministic fake
// agent needs, so the zero value stays the fake agent's contract.
func (b Budgets) withDefaults() Budgets {
	if b.ReadTimeoutMS == 0 {
		b.ReadTimeoutMS = 5000
	}
	if b.TurnTimeoutMS == 0 {
		b.TurnTimeoutMS = 10000
	}
	if b.StallTimeoutMS == 0 {
		b.StallTimeoutMS = 10000
	}
	if b.Observation == 0 {
		b.Observation = qualification.ShutdownDeadline
	}
	if b.MaxSessions == 0 {
		b.MaxSessions = 1
	}
	return b
}

// NewHarness assembles the deterministic harness: the fake protocol agent
// behind the same builder the live collector uses, under this package's fixture
// kind.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	return NewHarnessWithAgent(t, newFakeAgent(), "sortie-qualification-fake-agent --session-fixture", fixtureAgentKind, Budgets{})
}

// NewHarnessWithAgent assembles the harness under t.TempDir() with the given
// agent adapter, agent.command, and agent kind: a temporary issue file, a
// controlled git workspace, a temporary store, and the orchestrator wired to
// the file tracker and that agent.
//
// agentKind selects the registry entry the run resolves its metadata through
// and must name a kind registered in the calling test binary. A caller
// qualifying a real adapter passes that adapter's own kind, so the run reads its
// declared MCP injection mode and configuration validator as production does;
// passing this package's fixture kind instead would substitute zero-value
// metadata. A caller driving the fake agent passes fixtureAgentKind, which this
// package registers itself.
func NewHarnessWithAgent(t *testing.T, agent domain.AgentAdapter, agentCommand, agentKind string, budgets Budgets) *Harness {
	budgets = budgets.withDefaults()
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
	// The controlled git workspace at the path the orchestrator's workspace
	// manager computes for the fixture issue, so the agent's working directory
	// is a git work tree before launch.
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
		AgentKind:      agentKind,
		AgentCommand:   agentCommand,
		ReadTimeoutMS:  budgets.ReadTimeoutMS,
		TurnTimeoutMS:  budgets.TurnTimeoutMS,
		StallTimeoutMS: budgets.StallTimeoutMS,
		MaxTurns:       1,
		MaxSessions:    budgets.MaxSessions,
		MaxTokens:      budgets.MaxTokens,
	}
	cfg := serviceConfig(workspaceRoot, sample)
	tmpl, err := prompt.Parse(fixturePrompt, "fixture", 0)
	if err != nil {
		t.Fatalf("parse fixture prompt template: %v", err)
	}
	manager := &workflowManager{config: cfg, template: tmpl, workflowPath: filepath.Join(root, "WORKFLOW.md")}

	store, err := persistence.Open(context.Background(), filepath.Join(root, "e2e.db"))
	if err != nil {
		t.Fatalf("open temporary store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate temporary store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the temporary store: %v", err)
		}
	})

	observer := &AdapterObserver{inner: agent}
	toolServer, err := toolServerBinary()
	if err != nil {
		t.Fatalf("resolve the tool server binary: %v", err)
	}
	state := orchestrator.NewState(20, 1, sample.MaxTokens, nil, orchestrator.AgentTotals{})
	orch := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		MCPServerBinary: toolServer,
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
		observation:   budgets.Observation,
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

// TerminalCondition is the observed state of the harness's terminal condition,
// evaluated through the tracker, run-history store, runtime snapshot, and the
// fake agent's StopSession observation.
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

// unmetDetail names every part of the terminal condition that did not hold.
func (c TerminalCondition) unmetDetail() string {
	var unmet []string
	if !c.SucceededRow {
		unmet = append(unmet, "no succeeded history row")
	}
	if !c.HandoffReached {
		unmet = append(unmet, "the issue never reached its handoff state")
	}
	if !c.NoRunningEntry {
		unmet = append(unmet, "a running snapshot entry remains")
	}
	if !c.NoRetryEntry {
		unmet = append(unmet, "a retry snapshot entry remains")
	}
	if !c.StopSessionDone {
		unmet = append(unmet, "StopSession never completed")
	}
	return strings.Join(unmet, "; ")
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

// TerminalRecord builds the single end-to-end record from the observed terminal
// condition. The session identifier is the actual protocol session the adapter
// held, and the identity fields are that session's handshake facts; only the
// deterministic harness passes its fixture values.
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
	}
	switch {
	case condition.Reached() && groupClean:
		rec.Grade = qualification.GradeUsable
		rec.Outcome = qualification.OutcomePass
		rec.Detail = "one succeeded history row and the issue reached its handoff state"
	case !condition.Reached():
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
		rec.Detail = condition.unmetDetail()
	default:
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
		rec.Detail = "the issue reached its handoff state and a captured process group outlived the run"
	}
	return rec
}

// StartWorkflow starts the orchestrator loop and returns the cancel function
// and the loop-completion channel.
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
			// A run still going after cancellation leaks its goroutine into the
			// rest of the package's tests. Errorf, not Fatalf, so the remaining
			// cleanups still run.
			t.Errorf("orchestrator still running %s after cancellation; the harness leaked its run goroutine", qualification.ShutdownDeadline)
		}
	})
	return cancel, runDone
}
