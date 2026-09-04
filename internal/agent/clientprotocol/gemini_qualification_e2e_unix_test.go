//go:build unix

package clientprotocol

import (
	"context"
	"fmt"
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

// geminiE2EStateNames are the fixture's active and non-active handoff
// states: the issue starts in the active state and, on success, reaches
// the non-active handoff state.
const (
	geminiE2EActiveState  = "todo"
	geminiE2EHandoffState = "done"
	geminiE2EIssueID      = "sortie-e2e-1"
	geminiE2EIdentifier   = "SORTIE-E2E-1"
)

// geminiE2EEffectiveSample carries the only effective sample fields the
// isolated end-to-end harness extracts: agent.kind, agent.command, the
// agent read/turn/stall bounds, max_turns, max_sessions, max_tokens,
// and the optional agent-client-protocol mcp_config shape. Nothing else
// from a sample contract crosses into the harness.
type geminiE2EEffectiveSample struct {
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

// geminiE2EConfig builds the harness's service configuration from the
// effective sample fields alone, with shorter positive test bounds that
// preserve the sample's semantics. The harness carries no hooks block,
// no notification backend, no server listener, and no network-backed
// tracker.
func geminiE2EConfig(workspaceRoot string, sample geminiE2EEffectiveSample) config.ServiceConfig {
	return config.ServiceConfig{
		Polling:   config.PollingConfig{IntervalMS: 20},
		Workspace: config.WorkspaceConfig{Root: workspaceRoot},
		Tracker: config.TrackerConfig{
			Kind:            "file",
			ActiveStates:    []string{geminiE2EActiveState},
			HandoffState:    geminiE2EHandoffState,
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

// geminiE2EWorkflowManager implements [orchestrator.WorkflowManager]
// with the harness's frozen configuration and one prompt template.
type geminiE2EWorkflowManager struct {
	mu       sync.RWMutex
	config   config.ServiceConfig
	template *prompt.Template
}

func (m *geminiE2EWorkflowManager) Config() config.ServiceConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func (m *geminiE2EWorkflowManager) PromptTemplate() *prompt.Template {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.template
}

func (m *geminiE2EWorkflowManager) PromptTemplateByID(string) *prompt.Template {
	return m.PromptTemplate()
}

func (m *geminiE2EWorkflowManager) Reload() error { return nil }

func (m *geminiE2EWorkflowManager) WorkflowAbsPath() string { return "WORKFLOW.md" }

// geminiE2EFakeAgent is the fake protocol agent the deterministic E2E
// oracle drives: it launches a real bounded child process in its own
// process group so the exact PGID postcondition has a group to check,
// records every session lifecycle call, and performs no model traffic.
type geminiE2EFakeAgent struct {
	mu sync.Mutex

	startCalls int
	runCalls   int
	stopCalls  int
}

func newGeminiE2EFakeAgent() *geminiE2EFakeAgent {
	return &geminiE2EFakeAgent{}
}

// StartSession launches a bounded fake runtime process in its own
// process group, so the run's teardown and the exact PGID postcondition
// have an attributable group.
func (a *geminiE2EFakeAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	cmd := exec.Command("sleep", "120") //nolint:gosec // a bounded fake local process the fake agent's own teardown kills
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
func (a *geminiE2EFakeAgent) RunTurn(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
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
func (a *geminiE2EFakeAgent) StopSession(_ context.Context, session domain.Session) error {
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

// geminiE2EFixture is the isolated end-to-end harness: a file tracker
// over a temporary issue file, a controlled git workspace under the
// same temporary root, a real orchestrator over a temporary store, and
// the fake protocol agent.
type geminiE2EFixture struct {
	TempRoot      string
	IssueFile     string
	WorkspaceRoot string
	Sample        geminiE2EEffectiveSample
	Tracker       domain.TrackerAdapter
	Agent         *geminiE2EAdapterObserver
	Manager       *geminiE2EWorkflowManager
	Store         *persistence.Store
	Orchestrator  *orchestrator.Orchestrator
}

// geminiE2EAdapterObserver wraps the harness's agent adapter and
// records the two lifecycle facts the terminal condition and the
// process-group postcondition need: each session's captured process
// group and each completed StopSession call.
type geminiE2EAdapterObserver struct {
	inner domain.AgentAdapter

	mu         sync.Mutex
	pgids      []int
	sessionIDs []string
	stops      int
}

// StartSession delegates and captures the session's process-group
// leader PID, which procutil.SetProcessGroup makes the group's PGID,
// and the actual protocol session identifier.
func (o *geminiE2EAdapterObserver) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
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
func (o *geminiE2EAdapterObserver) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	return o.inner.RunTurn(ctx, session, params)
}

// StopSession delegates and records the completed call.
func (o *geminiE2EAdapterObserver) StopSession(ctx context.Context, session domain.Session) error {
	err := o.inner.StopSession(ctx, session)
	o.mu.Lock()
	o.stops++
	o.mu.Unlock()
	return err
}

// StopObserved reports whether at least one StopSession completed.
func (o *geminiE2EAdapterObserver) StopObserved() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stops > 0
}

// PGIDs returns every captured process group.
func (o *geminiE2EAdapterObserver) PGIDs() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.pgids...)
}

// SessionIDs returns every actual protocol session identifier the
// observed adapter StartSession calls returned.
func (o *geminiE2EAdapterObserver) SessionIDs() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sessionIDs...)
}

// geminiNewE2EFixture assembles the deterministic harness: the fake
// protocol agent behind the same builder the live collector uses.
func geminiNewE2EFixture(t *testing.T) *geminiE2EFixture {
	t.Helper()
	return geminiNewE2EFixtureWithAgent(t, newGeminiE2EFakeAgent(), "sortie-qualification-fake-agent --session-fixture")
}

// geminiNewE2EFixtureWithAgent assembles the harness under t.TempDir()
// with the given agent adapter and agent.command coordinate: a
// temporary issue file, a controlled git workspace, a temporary store,
// and the orchestrator wired to the file tracker and that agent. The
// live collector passes the real protocol adapter; the deterministic
// tests pass the fake protocol agent.
func geminiNewE2EFixtureWithAgent(t *testing.T, agent domain.AgentAdapter, agentCommand string) *geminiE2EFixture {
	t.Helper()

	root := t.TempDir()
	issueFile := filepath.Join(root, "issues.json")
	issueJSON := fmt.Sprintf(`[{"id":%q,"identifier":%q,"title":"Gemini qualification end-to-end fixture","description":"drive one isolated run","state":%q,"priority":2,"branch_name":"","url":"","labels":[],"assignee":"","issue_type":"task","comments":[],"blocked_by":[],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`,
		geminiE2EIssueID, geminiE2EIdentifier, geminiE2EActiveState)
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
	pathResult, err := workspace.ComputePath(workspaceRoot, geminiE2EIdentifier)
	if err != nil {
		t.Fatalf("compute workspace path: %v", err)
	}
	if err := os.MkdirAll(pathResult.Path, 0o750); err != nil {
		t.Fatalf("create controlled workspace: %v", err)
	}
	gitInit := exec.Command("git", "-C", pathResult.Path, "init")
	if out, gitErr := gitInit.CombinedOutput(); gitErr != nil {
		t.Fatalf("git init the controlled workspace: %v\n%s", gitErr, out)
	}

	tracker, err := file.NewFileAdapter(map[string]any{"path": issueFile, "active_states": []string{geminiE2EActiveState}})
	if err != nil {
		t.Fatalf("create file tracker: %v", err)
	}

	sample := geminiE2EEffectiveSample{
		AgentKind:      "agent-client-protocol",
		AgentCommand:   agentCommand,
		ReadTimeoutMS:  5000,
		TurnTimeoutMS:  10000,
		StallTimeoutMS: 10000,
		MaxTurns:       1,
		MaxSessions:    1,
		MaxTokens:      0,
	}
	cfg := geminiE2EConfig(workspaceRoot, sample)
	tmpl, err := prompt.Parse("Work the fixture task for {{ .issue.identifier }}.", "fixture", 0)
	if err != nil {
		t.Fatalf("parse fixture prompt template: %v", err)
	}
	manager := &geminiE2EWorkflowManager{config: cfg, template: tmpl}

	store, err := persistence.Open(context.Background(), filepath.Join(root, "e2e.db"))
	if err != nil {
		t.Fatalf("open temporary store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate temporary store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	observer := &geminiE2EAdapterObserver{inner: agent}
	state := orchestrator.NewState(20, 1, nil, orchestrator.AgentTotals{})
	orch := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
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

	return &geminiE2EFixture{
		TempRoot:      root,
		IssueFile:     issueFile,
		WorkspaceRoot: workspaceRoot,
		Sample:        sample,
		Tracker:       tracker,
		Agent:         observer,
		Manager:       manager,
		Store:         store,
		Orchestrator:  orch,
	}
}

// TestGeminiQualificationE2EFixtureContract confirms the isolated
// end-to-end fixture's contract: tracker.kind file with a temporary
// issue file, a controlled git workspace under the same temporary root,
// no hooks, notifications, server, or network tracker, max_turns 1,
// an active and a non-active handoff state, and only the permitted
// effective sample fields.
func TestGeminiQualificationE2EFixtureContract(t *testing.T) {
	t.Parallel()

	fixture := geminiNewE2EFixture(t)

	t.Run("file tracker over a temporary issue file", func(t *testing.T) {
		t.Parallel()

		if fixture.Sample == (geminiE2EEffectiveSample{}) {
			t.Fatal("fixture sample is empty")
		}
		raw, err := os.ReadFile(fixture.IssueFile)
		if err != nil {
			t.Fatalf("read issue file: %v", err)
		}
		if !strings.Contains(string(raw), geminiE2EIdentifier) {
			t.Errorf("issue file %s does not name the fixture issue", filepath.Base(fixture.IssueFile))
		}
		if !strings.Contains(string(raw), geminiE2EActiveState) {
			t.Errorf("issue file does not start the issue in the active state %q", geminiE2EActiveState)
		}
	})

	t.Run("controlled git workspace under the same temporary root", func(t *testing.T) {
		t.Parallel()

		if filepath.Dir(fixture.WorkspaceRoot) != fixture.TempRoot {
			t.Errorf("workspace root %q is not under the fixture's temporary root", fixture.WorkspaceRoot)
		}
	})

	t.Run("no hooks, notifications, server, or network tracker", func(t *testing.T) {
		t.Parallel()

		hooks := fixture.Manager.Config().Hooks
		if hooks.AfterCreate != "" || hooks.BeforeRun != "" || hooks.AfterRun != "" || hooks.BeforeRemove != "" {
			t.Errorf("harness carries a hooks block: %+v", hooks)
		}
		if notifications := fixture.Manager.Config().Notifications; len(notifications.Backends) != 0 {
			t.Errorf("harness carries a notification backend: %+v", notifications)
		}
		if fixture.Manager.Config().Tracker.Kind != "file" {
			t.Errorf("tracker kind = %q, want file", fixture.Manager.Config().Tracker.Kind)
		}
	})

	t.Run("active and non-active handoff states", func(t *testing.T) {
		t.Parallel()

		cfg := fixture.Manager.Config().Tracker
		if !slices.Contains(cfg.ActiveStates, geminiE2EActiveState) {
			t.Errorf("active states = %v, want %q among them", cfg.ActiveStates, geminiE2EActiveState)
		}
		if slices.Contains(cfg.ActiveStates, geminiE2EHandoffState) {
			t.Errorf("handoff state %q must be non-active", geminiE2EHandoffState)
		}
		if cfg.HandoffState != geminiE2EHandoffState {
			t.Errorf("handoff state = %q, want %q", cfg.HandoffState, geminiE2EHandoffState)
		}
	})

	t.Run("only the permitted effective sample fields are set", func(t *testing.T) {
		t.Parallel()

		cfg := fixture.Manager.Config()
		if cfg.Agent.Kind != fixture.Sample.AgentKind || cfg.Agent.Command != fixture.Sample.AgentCommand {
			t.Errorf("agent fields = %q/%q, want the sample's kind and command", cfg.Agent.Kind, cfg.Agent.Command)
		}
		if cfg.Agent.TurnTimeoutMS != fixture.Sample.TurnTimeoutMS ||
			cfg.Agent.ReadTimeoutMS != fixture.Sample.ReadTimeoutMS ||
			cfg.Agent.StallTimeoutMS != fixture.Sample.StallTimeoutMS {
			t.Errorf("agent bounds do not match the extracted sample fields")
		}
		if cfg.Agent.MaxTurns != 1 {
			t.Errorf("max_turns = %d, want 1", cfg.Agent.MaxTurns)
		}
		if cfg.Agent.MaxSessions != fixture.Sample.MaxSessions || cfg.Agent.MaxTokens != fixture.Sample.MaxTokens {
			t.Errorf("session and token bounds do not match the extracted sample fields")
		}
		if cfg.Agent.MaxRetryBackoffMS != 0 || cfg.Agent.MaxConsecutiveAbsences != 0 {
			t.Errorf("agent carries an unpermitted bound: retry backoff %d, absences %d",
				cfg.Agent.MaxRetryBackoffMS, cfg.Agent.MaxConsecutiveAbsences)
		}
		if len(cfg.Reactions) != 0 {
			t.Errorf("reactions = %v, want none", cfg.Reactions)
		}
		if cfg.CIFeedback.Kind != "" {
			t.Errorf("CI feedback configured: %q", cfg.CIFeedback.Kind)
		}
		if len(cfg.Notifications.Backends) != 0 {
			t.Errorf("notifications configured: %+v", cfg.Notifications)
		}
	})
}

// geminiE2ETerminalCondition is the observed state of R11's terminal
// condition, evaluated through the tracker, the run-history store, the
// runtime snapshot, and the fake agent's own StopSession observation.
type geminiE2ETerminalCondition struct {
	SucceededRow    bool
	HandoffReached  bool
	NoRunningEntry  bool
	NoRetryEntry    bool
	StopSessionDone bool
}

// reached reports whether every part of the terminal condition holds.
func (c geminiE2ETerminalCondition) reached() bool {
	return c.SucceededRow && c.HandoffReached && c.NoRunningEntry && c.StopSessionDone
}

// geminiObserveE2ETerminalCondition evaluates the terminal condition
// once against the fixture's collaborators.
func geminiObserveE2ETerminalCondition(t *testing.T, fixture *geminiE2EFixture) geminiE2ETerminalCondition {
	t.Helper()

	condition := geminiE2ETerminalCondition{}

	rows, err := fixture.Store.QueryRunHistoryByIssue(context.Background(), geminiE2EIssueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	condition.SucceededRow = len(rows) == 1 && rows[0].Status == "succeeded"

	states, err := fixture.Tracker.FetchIssueStatesByIDs(context.Background(), []string{geminiE2EIssueID})
	if err != nil {
		t.Fatalf("fetch issue state: %v", err)
	}
	condition.HandoffReached = states[geminiE2EIssueID] == geminiE2EHandoffState

	snapshot, err := fixture.Orchestrator.SnapshotFunc()()
	if err != nil {
		t.Fatalf("runtime snapshot: %v", err)
	}
	condition.NoRunningEntry = len(snapshot.Running) == 0 && len(snapshot.Retrying) == 0

	condition.StopSessionDone = fixture.Agent.StopObserved()
	return condition
}

// geminiE2ERecord builds the single end_to_end scenario record from the
// observed terminal condition and the run's process-group outcome. Its
// verdict is pass only when every part of the terminal condition held
// and the process-group postcondition drained within the shared
// deadline.
// geminiE2ERecord builds the single end-to-end record from the observed
// terminal condition. The session identifier is the actual protocol
// session the harness's adapter session held, and the identity fields
// are that session's handshake facts; only the deterministic harness
// passes its fixture values.
func geminiE2ERecord(condition geminiE2ETerminalCondition, groupClean bool, sessionID, agentName, agentVersion string) qualification.Record {
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
	case condition.reached() && groupClean:
		rec.Grade = qualification.GradeUsable
		rec.Outcome = qualification.OutcomePass
	case !condition.reached():
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeNotObserved
	default:
		rec.Grade = qualification.GradeNotObserved
		rec.Outcome = qualification.OutcomeRuntimeFailed
	}
	return rec
}

// TestGeminiQualificationE2ETerminalOracle drives the isolated
// file-tracker harness with the fake protocol agent and temporary file
// tracker only: it reaches the terminal condition, cancels and drains
// within the shared shutdown bound, and then applies the exact PGID
// postcondition with the signal-zero oracle.
func TestGeminiQualificationE2ETerminalOracle(t *testing.T) {
	fixture := geminiNewE2EFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		fixture.Orchestrator.Run(ctx)
		close(runDone)
	}()

	// A bounded wait for the terminal condition: one succeeded history
	// row, the file-tracker issue in its handoff state, no running or
	// retry snapshot entry, and a completed StopSession.
	deadline := time.Now().Add(geminiQualificationShutdownDeadline)
	var condition geminiE2ETerminalCondition
	for {
		condition = geminiObserveE2ETerminalCondition(t, fixture)
		if condition.reached() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-runDone
			t.Fatalf("the isolated end-to-end harness never reached its terminal condition; last observation = %+v", condition)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Reaching the condition triggers cleanup: cancel the orchestrator
	// and allow at most the shared 30-second shutdown bound for the
	// drain.
	cancel()
	select {
	case <-runDone:
	case <-time.After(geminiQualificationShutdownDeadline):
		t.Fatal("the orchestrator did not drain within the shared shutdown bound")
	}

	// The exact PGID postcondition: every captured group is gone.
	if len(fixture.Agent.PGIDs()) != 1 {
		t.Fatalf("captured group count = %d, want 1 for the single fixture session", len(fixture.Agent.PGIDs()))
	}
	for _, pgid := range fixture.Agent.PGIDs() {
		geminiAwaitProcessGroupAbsence(t, pgid)
	}

	// The terminal record from the observed condition is pass and
	// decodes cleanly.
	rec := geminiE2ERecord(condition, true, qualification.FixtureSession(qualification.SurfaceProtocol, "e2e"), qualification.FixtureAgentName, qualification.FixtureAgentVer)
	if rec.Outcome != qualification.OutcomePass || rec.Grade != qualification.GradeUsable {
		t.Errorf("end-to-end record = %s/%s, want pass/usable at the terminal condition", rec.Outcome, rec.Grade)
	}
	line, err := qualification.MarshalRecord(rec)
	if err != nil {
		t.Fatalf("qualification.MarshalRecord() error = %v", err)
	}
	if _, err := qualification.DecodeRecord(line); err != nil {
		t.Errorf("qualification.DecodeRecord() error = %v, want the end-to-end record to decode cleanly", err)
	}

	// The run-history assertion: exactly one row, succeeded, for this
	// issue's single attempt.
	rows, err := fixture.Store.QueryRunHistoryByIssue(context.Background(), geminiE2EIssueID)
	if err != nil {
		t.Fatalf("query run history: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "succeeded" || rows[0].Attempt != 1 {
		t.Errorf("run history rows = %+v, want exactly one succeeded first attempt", rows)
	}
}

// geminiE2EStartWorkflow starts the orchestrator loop and returns the
// cancel function and the loop-completion channel, for the live
// collector's isolated end-to-end run.
func geminiE2EStartWorkflow(t *testing.T, fixture *geminiE2EFixture) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		fixture.Orchestrator.Run(ctx)
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(geminiQualificationShutdownDeadline):
		}
	})
	return cancel, runDone
}
