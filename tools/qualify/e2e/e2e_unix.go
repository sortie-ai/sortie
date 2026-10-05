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

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tracker/file"
	"github.com/sortie-ai/sortie/internal/workspace"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/procgroup"
)

// fixtureAgentKind is the agent kind the deterministic harness configures;
// this package registers it itself since the harness supplies its adapter directly.
const fixtureAgentKind = "qualification-e2e-fixture"

func init() {
	registry.Agents.RegisterWithMeta(fixtureAgentKind, func() (domain.AgentAdapter, error) {
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

// The live ceiling step's fixed bounds: a one-token ceiling low enough that
// the first measured turn crosses it, two turns so work is still pending
// after the crossing, and room for one further dispatch so a defective
// redispatch is observable rather than withheld by the session budget.
const (
	ceilingRunTokens   = 1
	ceilingRunTurns    = 2
	ceilingRunSessions = 2

	// ceilingQuietPolls is one second of the observer's 20ms polls: how long a
	// dispatch has to appear after the hold before the observation ends.
	ceilingQuietPolls = 50
)

// fixturePrompt is the isolated harness's only agent prompt; omitting the
// "no-change-needed" signal left a live model hunting for a nonexistent task
// instead of exercising the orchestrator's handoff plumbing.
const fixturePrompt = `The workspace for {{ .issue.identifier }} is intentionally empty: it holds only a git directory and an empty state directory, and no fixture task lives anywhere in it. The requested outcome already holds, so do not search for a task or make any change. Signal completion immediately by running:

    mkdir -p .sortie && echo "no-change-needed" > .sortie/status`

// effectiveSample carries the only effective sample fields the isolated harness
// extracts. Nothing else from a sample contract crosses into the harness.
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

// PromptTemplateByID serves the fixture's only template for the default id,
// and nil otherwise, as a real workflow does; answering every id would hide a resolution failure.
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

// fakeAgent is the fake protocol agent the deterministic oracle drives: a
// real bounded child process in its own group gives the PGID postcondition
// something to check, with no model traffic.
type fakeAgent struct {
	mu sync.Mutex

	startCalls int
	runCalls   int
	stopCalls  int
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{}
}

type fakeAgentSession struct {
	cmd                    *exec.Cmd
	credentialVerification bool
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
		Internal: &fakeAgentSession{cmd: cmd, credentialVerification: params.CredentialVerification},
	}, nil
}

// RunTurn writes one marker file into the workspace as the fake agent's
// work evidence and reports a successful terminal disposition. A
// verification turn writes none, since it must not read as work.
func (a *fakeAgent) RunTurn(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	fs, ok := session.Internal.(*fakeAgentSession)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "unexpected session internal type"}
	}
	if !fs.credentialVerification {
		if err := os.WriteFile(filepath.Join(fs.cmd.Dir, "agent-work-marker"), []byte("work"), 0o600); err != nil {
			return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "fake agent work failed", Err: err}
		}
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
	fs, ok := session.Internal.(*fakeAgentSession)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	_ = procgroup.Signal(fs.cmd.Process.Pid, syscall.SIGKILL)
	_, _ = fs.cmd.Process.Wait()

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
	turnCalls  int
	figureTurn int
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

// RunTurn delegates, counting this call and noting the first relayed event
// carrying a positive token figure against the call's own index.
func (o *AdapterObserver) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	o.mu.Lock()
	o.turnCalls++
	call := o.turnCalls
	o.mu.Unlock()

	if params.OnEvent != nil {
		onEvent := params.OnEvent
		params.OnEvent = func(event domain.AgentEvent) {
			if event.Usage.TotalTokens > 0 {
				o.mu.Lock()
				if o.figureTurn == 0 {
					o.figureTurn = call
				}
				o.mu.Unlock()
			}
			onEvent(event)
		}
	}
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

// TurnsStarted returns the number of RunTurn calls observed so far.
func (o *AdapterObserver) TurnsStarted() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.turnCalls
}

// FigureTurn returns the 1-based RunTurn call during which the first event
// carrying a positive token figure was relayed, or 0 when none has been.
func (o *AdapterObserver) FigureTurn() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.figureTurn
}

// toolServerBinary builds the sortie binary once per test process and returns
// its path: the test binary speaks no MCP, so a runtime handed that path
// blocks until timeout. Later harnesses in the process share the binary.
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

// Budgets are the per-run bounds one harness gives its agent, plus how long an
// observer may wait for a terminal condition. The fake agent takes the zero
// value; a live runtime must state its own or a real turn ends unobserved for a harness reason.
type Budgets struct {
	ReadTimeoutMS  int
	TurnTimeoutMS  int
	StallTimeoutMS int

	// MaxTokens is the per-issue token ceiling, zero for unbounded; set it only
	// when the run's own subject is the stop, or it ends an unrelated run early.
	MaxTokens int

	// MaxSessions is the per-issue session budget, defaulting to the one a
	// single deterministic run needs. A harness whose subject is another budget
	// raises it, so this one cannot stand in for the budget under measurement.
	MaxSessions int

	// MaxTurns is the per-run turn bound, defaulting to the single turn a
	// deterministic run needs.
	MaxTurns int

	// Observation bounds the wait for a terminal condition. It is not a shutdown
	// bound: procgroup.ShutdownDeadline governs that, and spending one on
	// the other gives a live run a shutdown's worth of time to do a turn's work.
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
		b.Observation = procgroup.ShutdownDeadline
	}
	if b.MaxSessions == 0 {
		b.MaxSessions = 1
	}
	if b.MaxTurns == 0 {
		b.MaxTurns = 1
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
// agent, command, and kind. agentKind must be registered in the calling test
// binary; a real adapter's own kind reads production MCP metadata, unlike fixtureAgentKind.
func NewHarnessWithAgent(t *testing.T, agent domain.AgentAdapter, agentCommand, agentKind string, budgets Budgets) *Harness {
	return newHarness(t, agent, agentCommand, agentKind, budgets, fixturePrompt)
}

// NewCeilingHarness assembles the harness for the live token-ceiling step: a
// one-token ceiling, two turns, and room for one further dispatch so a
// defective redispatch after the stop is observable rather than withheld by
// the session budget. Every other bound comes from bounds.
func NewCeilingHarness(t *testing.T, agent domain.AgentAdapter, agentCommand, agentKind, prompt string, bounds Budgets) *Harness {
	budgets := Budgets{
		ReadTimeoutMS:  bounds.ReadTimeoutMS,
		TurnTimeoutMS:  bounds.TurnTimeoutMS,
		StallTimeoutMS: bounds.StallTimeoutMS,
		Observation:    bounds.Observation,
		MaxTokens:      ceilingRunTokens,
		MaxTurns:       ceilingRunTurns,
		MaxSessions:    ceilingRunSessions,
	}
	return newHarness(t, agent, agentCommand, agentKind, budgets, prompt)
}

// newHarness assembles the harness under t.TempDir() with the given agent,
// command, kind, budgets, and workflow prompt template.
func newHarness(t *testing.T, agent domain.AgentAdapter, agentCommand, agentKind string, budgets Budgets, workflowPrompt string) *Harness {
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
		MaxTurns:       budgets.MaxTurns,
		MaxSessions:    budgets.MaxSessions,
		MaxTokens:      budgets.MaxTokens,
	}
	cfg := serviceConfig(workspaceRoot, sample)
	tmpl, err := prompt.Parse(workflowPrompt, "fixture", 0)
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

// ObserveTerminalCondition evaluates the terminal condition once against the
// harness's collaborators.
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

// CeilingCondition is the observed state of one live run through the
// one-token ceiling harness, evaluated through the run-history store, the
// adapter observer's turn counters, and, for a budget-stopped run, the
// dispatch watch.
type CeilingCondition struct {
	RunEnded         bool
	Status           string
	TokensMeasured   bool
	TotalTokens      int64
	UnaccountedTurns int
	TurnsStarted     int
	FigureTurn       int
	HoldObserved     bool
	Runs             int
	Running          bool
}

// ceilingSample is one poll tick's reading of the fixture issue's dispatch
// state: its run_history row count, whether a token-budget hold covers it,
// and whether a session is in flight.
type ceilingSample struct {
	runs    int
	held    bool
	running bool
}

// dispatchWatch folds a sequence of post-stop ceilingSample readings into
// whether a redispatch happened and, once the issue entered the hold,
// whether it stayed there through the quiet window.
type dispatchWatch struct {
	held       bool
	runs       int
	running    bool
	quietPolls int
}

// add folds one sample into the watch and reports whether the observation
// should end: true on the first sign of a dispatch, or once quietPolls
// post-hold samples arrived with no such sign.
func (w *dispatchWatch) add(sample ceilingSample) bool {
	w.runs, w.running = sample.runs, sample.running
	if sample.runs > 1 || sample.running {
		return true
	}
	if w.held {
		w.quietPolls++
	}
	if sample.held {
		w.held = true
	}
	return w.quietPolls >= ceilingQuietPolls
}

// readCeilingSample reads one ceilingSample for the fixture issue from the
// harness's store and runtime snapshot.
func readCeilingSample(harness *Harness) (ceilingSample, error) {
	rows, err := harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
	if err != nil {
		return ceilingSample{}, fmt.Errorf("query run history: %w", err)
	}
	snapshot, err := harness.orchestrator.SnapshotFunc()()
	if err != nil {
		return ceilingSample{}, fmt.Errorf("runtime snapshot: %w", err)
	}
	held := false
	for _, entry := range snapshot.BudgetExhausted {
		if entry.IssueID == issueID && entry.Reason == "token_budget" {
			held = true
			break
		}
	}
	return ceilingSample{
		runs:    len(rows),
		held:    held,
		running: len(snapshot.Running) > 0,
	}, nil
}

// ObserveCeilingStop waits for the fixture issue's first run_history row,
// then, only for a run that ended budget_stopped, watches for a redispatch
// through the quiet window. It never cancels the workflow; it fails the test
// only on a store or snapshot read error.
func ObserveCeilingStop(t *testing.T, harness *Harness) CeilingCondition {
	t.Helper()

	deadline := time.Now().Add(harness.Observation())
	var rows []persistence.RunHistory
	for {
		var err error
		rows, err = harness.store.QueryRunHistoryByIssue(context.Background(), issueID)
		if err != nil {
			t.Fatalf("query run history: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			return CeilingCondition{}
		}
		time.Sleep(20 * time.Millisecond)
	}

	row := rows[0]
	for _, candidate := range rows[1:] {
		if candidate.ID < row.ID {
			row = candidate
		}
	}

	condition := CeilingCondition{
		RunEnded:         true,
		Status:           row.Status,
		TokensMeasured:   row.TokensMeasured,
		TotalTokens:      row.TotalTokens,
		UnaccountedTurns: row.UnaccountedTurns,
		TurnsStarted:     harness.agent.TurnsStarted(),
		FigureTurn:       harness.agent.FigureTurn(),
	}
	if row.Status != "budget_stopped" {
		return condition
	}

	watch := &dispatchWatch{}
	deadline = time.Now().Add(harness.Observation())
	for {
		sample, err := readCeilingSample(harness)
		if err != nil {
			t.Fatalf("read ceiling sample: %v", err)
		}
		if watch.add(sample) {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	condition.HoldObserved, condition.Runs, condition.Running = watch.held, watch.runs, watch.running
	return condition
}

// CeilingStopObservation maps one CeilingCondition onto the observation the
// live ceiling step records for the protocol surface's token-ceiling row. It
// is pure and total: the result always passes
// [evidence.CheckObservationAdmitted]. An empty sessionID writes a null
// session_id.
func CeilingStopObservation(condition CeilingCondition, sessionID string) evidence.Observation {
	grade, outcome, detail := ceilingStopGrading(condition)
	return evidence.Observation{
		Grade:     grade,
		Outcome:   outcome,
		Detail:    evidence.BoundDetail(detail),
		SessionID: sessionID,
	}
}

// ceilingStopGrading grades one CeilingCondition against the closed nine-row
// table: whether the run ended, whether it stopped at the ceiling with no
// later dispatch, and, for a run that ended on its own, whether it crossed
// the ceiling unmeasured, unbounded, or without stopping in time.
func ceilingStopGrading(condition CeilingCondition) (evidence.Grade, evidence.Outcome, string) {
	switch {
	case !condition.RunEnded:
		return evidence.GradeNotObserved, evidence.OutcomeRuntimeFailed,
			"no run under the one-token ceiling ended within the observation bound"

	case condition.Status == "budget_stopped" && (condition.Runs > 1 || condition.Running):
		return evidence.GradeGap, evidence.OutcomePass,
			fmt.Sprintf("the ceiling stopped the run (status %s) and a dispatch followed: %d recorded run(s), running=%t",
				condition.Status, condition.Runs, condition.Running)

	case condition.Status == "budget_stopped" && !condition.HoldObserved:
		return evidence.GradeNotObserved, evidence.OutcomeRuntimeFailed,
			fmt.Sprintf("the ceiling stopped the run (status %s), but no poll tick held the issue within the bound, so the absence of a later dispatch is unproven", condition.Status)

	case condition.Status == "budget_stopped":
		return evidence.GradeUsable, evidence.OutcomePass,
			fmt.Sprintf("stopped at %d token(s), the issue entered the token budget hold, no dispatch within the quiet window", condition.TotalTokens)

	case !condition.TokensMeasured && condition.UnaccountedTurns > 0:
		return evidence.GradeGap, evidence.OutcomePass,
			fmt.Sprintf("the run spent tokens and recorded no figure, so the ceiling could not bound it: %d unaccounted turn(s), status %s",
				condition.UnaccountedTurns, condition.Status)

	case !condition.TokensMeasured:
		return evidence.GradeNotObserved, evidence.OutcomePrerequisiteFailed,
			fmt.Sprintf("the run recorded no token figure and no turn reported unaccounted spend, so the ceiling could not be crossed: status %s", condition.Status)

	case condition.TotalTokens < ceilingRunTokens:
		return evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed,
			fmt.Sprintf("measured total %d token(s) is below the ceiling %d: status %s", condition.TotalTokens, ceilingRunTokens, condition.Status)

	case condition.FigureTurn == 0:
		return evidence.GradeGap, evidence.OutcomePass,
			fmt.Sprintf("the run recorded its tokens but none reached the orchestrator while it ran, so the ceiling could not stop it: status %s", condition.Status)

	case condition.TurnsStarted > condition.FigureTurn:
		return evidence.GradeGap, evidence.OutcomePass,
			fmt.Sprintf("a turn began after the run crossed the ceiling, and the run ended with its status %s rather than budget_stopped", condition.Status)

	default:
		return evidence.GradeNotObserved, evidence.OutcomeFixtureInductionFailed,
			fmt.Sprintf("the run crossed the ceiling on its last turn and ended with its status %s on its own before a stop could end it", condition.Status)
	}
}

// TerminalRecord builds the single end-to-end record from the observed
// terminal condition, using the adapter's actual session and handshake identity.
func TerminalRecord(condition TerminalCondition, groupClean bool, sessionID, agentName, agentVersion string) evidence.Record {
	rec := evidence.Record{
		SchemaVersion:   1,
		ObservedAt:      evidencetest.FixtureTime,
		Scenario:        evidence.ScenarioEndToEnd,
		Surface:         evidence.SurfaceProtocol,
		Capability:      evidence.CapabilityTurnDisposition,
		Source:          evidence.SourceProcessObservation,
		InputID:         evidence.InputE2E,
		EvidencePath:    new("/run_history/status"),
		SessionID:       new(sessionID),
		AgentName:       new(agentName),
		AgentVersion:    new(agentVersion),
		ProtocolVersion: new(1),
	}
	switch {
	case condition.Reached() && groupClean:
		rec.Grade = evidence.GradeUsable
		rec.Outcome = evidence.OutcomePass
		rec.Detail = "one succeeded history row and the issue reached its handoff state"
	case !condition.Reached():
		rec.Grade = evidence.GradeNotObserved
		rec.Outcome = evidence.OutcomeRuntimeFailed
		rec.Detail = condition.unmetDetail()
	default:
		rec.Grade = evidence.GradeNotObserved
		rec.Outcome = evidence.OutcomeRuntimeFailed
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
		case <-time.After(procgroup.ShutdownDeadline):
			// A run still going after cancellation leaks its goroutine into the
			// rest of the package's tests. Errorf, not Fatalf, so the remaining
			// cleanups still run.
			t.Errorf("orchestrator still running %s after cancellation; the harness leaked its run goroutine", procgroup.ShutdownDeadline)
		}
	})
	return cancel, runDone
}
