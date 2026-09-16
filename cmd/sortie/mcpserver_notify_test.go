package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
)

// fixtureNotifyAgentKind and fixtureNotifyTrackerKind name the agent and
// tracker kinds this test's fixtures register. The orchestrator's
// dispatch preflight requires cfg.Agent.Kind and cfg.Tracker.Kind to
// resolve in their registries even though this harness supplies both
// adapters directly, so the kinds are registered here rather than
// depending on a real adapter package's own registration.
const (
	fixtureNotifyAgentKind   = "mcpserver-notify-e2e-agent"
	fixtureNotifyTrackerKind = "mcpserver-notify-e2e-tracker"
)

func init() {
	registry.Agents.RegisterWithMeta(fixtureNotifyAgentKind, func(map[string]any) (domain.AgentAdapter, error) {
		return nil, fmt.Errorf("%s is resolved through AgentAdapterByKind, never constructed from the registry", fixtureNotifyAgentKind)
	}, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionSessionTotal,
	})

	registry.Trackers.RegisterWithMeta(fixtureNotifyTrackerKind, func(map[string]any) (domain.TrackerAdapter, error) {
		return nil, fmt.Errorf("%s is resolved directly through OrchestratorParams.TrackerAdapter, never constructed from the registry", fixtureNotifyTrackerKind)
	}, registry.TrackerMeta{})
}

// notifyE2ETracker is a minimal domain.TrackerAdapter fixture reporting
// a fixed set of issues as active candidates.
type notifyE2ETracker struct {
	issues []domain.Issue
}

var _ domain.TrackerAdapter = (*notifyE2ETracker)(nil)

func (tr *notifyE2ETracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return tr.issues, nil
}

func (tr *notifyE2ETracker) FetchIssueByID(_ context.Context, issueID string) (domain.Issue, error) {
	for _, iss := range tr.issues {
		if iss.ID == issueID {
			return iss, nil
		}
	}
	return domain.Issue{}, nil
}

func (tr *notifyE2ETracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}

func (tr *notifyE2ETracker) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, iss := range tr.issues {
		for _, id := range ids {
			if id == iss.ID {
				out[id] = iss.State
			}
		}
	}
	return out, nil
}

func (tr *notifyE2ETracker) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}

func (tr *notifyE2ETracker) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}

func (tr *notifyE2ETracker) TransitionIssue(_ context.Context, _ string, _ string) error { return nil }
func (tr *notifyE2ETracker) CommentIssue(_ context.Context, _ string, _ string) error    { return nil }
func (tr *notifyE2ETracker) AddLabel(_ context.Context, _ string, _ string) error        { return nil }

// turnController lets a test drive one open agent turn from the outside:
// wait for RunTurn to begin, hand the adapter an event to relay and
// wait until the relay's OnEvent call has returned, or let the turn
// complete.
type turnController struct {
	sessionID   string
	startParams domain.StartSessionParams

	entered  chan struct{}
	events   chan domain.AgentEvent
	reported chan struct{}
	finish   chan struct{}
}

func newTurnController(sessionID string, startParams domain.StartSessionParams) *turnController {
	return &turnController{
		sessionID:   sessionID,
		startParams: startParams,
		entered:     make(chan struct{}),
		events:      make(chan domain.AgentEvent),
		reported:    make(chan struct{}),
		finish:      make(chan struct{}),
	}
}

func runControlledTurn(ctx context.Context, tc *turnController, session domain.Session, onEvent func(domain.AgentEvent)) (domain.TurnResult, error) {
	close(tc.entered)
	for {
		select {
		case ev := <-tc.events:
			onEvent(ev)
			tc.reported <- struct{}{}
		case <-tc.finish:
			return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
		case <-ctx.Done():
			return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCancelled}, ctx.Err()
		}
	}
}

// sequentialAgent is a domain.AgentAdapter fixture for scenarios with
// at most one open turn at a time. StartSession adopts a non-empty
// ResumeSessionID as its own session id, simulating a runtime that
// resumes a conversation; otherwise it consults sessionIDFor.
type sequentialAgent struct {
	sessionIDFor func(call int) string
	calls        atomic.Int64
	pending      chan *turnController
	handles      chan *turnController
}

func newSequentialAgent(sessionIDFor func(call int) string) *sequentialAgent {
	return &sequentialAgent{
		sessionIDFor: sessionIDFor,
		pending:      make(chan *turnController, 1),
		handles:      make(chan *turnController, 4),
	}
}

var _ domain.AgentAdapter = (*sequentialAgent)(nil)

func (a *sequentialAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	call := int(a.calls.Add(1)) - 1
	id := params.ResumeSessionID
	if id == "" {
		id = a.sessionIDFor(call)
	}
	tc := newTurnController(id, params)
	a.pending <- tc
	a.handles <- tc
	return domain.Session{ID: id}, nil
}

func (a *sequentialAgent) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	tc := <-a.pending
	return runControlledTurn(ctx, tc, session, params.OnEvent)
}

func (a *sequentialAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

// concurrentAgent is a domain.AgentAdapter fixture for scenarios with
// more than one open turn at once. It correlates a RunTurn call with
// the StartSession call that produced its session, so sessionIDFor
// must return a unique, non-empty value for every call.
type concurrentAgent struct {
	sessionIDFor func(call int) string
	calls        atomic.Int64
	handles      chan *turnController

	mu          sync.Mutex
	controllers map[string]*turnController
}

func newConcurrentAgent(sessionIDFor func(call int) string) *concurrentAgent {
	return &concurrentAgent{
		sessionIDFor: sessionIDFor,
		handles:      make(chan *turnController, 8),
		controllers:  make(map[string]*turnController),
	}
}

var _ domain.AgentAdapter = (*concurrentAgent)(nil)

func (a *concurrentAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	call := int(a.calls.Add(1)) - 1
	id := a.sessionIDFor(call)
	tc := newTurnController(id, params)
	a.mu.Lock()
	a.controllers[id] = tc
	a.mu.Unlock()
	a.handles <- tc
	return domain.Session{ID: id}, nil
}

func (a *concurrentAgent) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	a.mu.Lock()
	tc := a.controllers[session.ID]
	a.mu.Unlock()
	return runControlledTurn(ctx, tc, session, params.OnEvent)
}

func (a *concurrentAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

// notifyE2EWorkflowManager implements orchestrator.WorkflowManager with
// a frozen configuration and one prompt template.
type notifyE2EWorkflowManager struct {
	config   config.ServiceConfig
	template *prompt.Template
	absPath  string
}

var _ orchestrator.WorkflowManager = (*notifyE2EWorkflowManager)(nil)

func (m *notifyE2EWorkflowManager) Config() config.ServiceConfig     { return m.config }
func (m *notifyE2EWorkflowManager) PromptTemplate() *prompt.Template { return m.template }

func (m *notifyE2EWorkflowManager) PromptTemplateByID(id string) *prompt.Template {
	if id != "" {
		return nil
	}
	return m.template
}

func (m *notifyE2EWorkflowManager) Reload() error           { return nil }
func (m *notifyE2EWorkflowManager) WorkflowAbsPath() string { return m.absPath }

// notifyE2EBaseConfig returns the shared configuration shape every
// scenario in this file starts from: a single active tracker state, no
// handoff state (so a normally exited issue is naturally continued),
// and one turn per dispatch, held open by the test through a
// turnController.
func notifyE2EBaseConfig(root string) config.ServiceConfig {
	return config.ServiceConfig{
		Polling:   config.PollingConfig{IntervalMS: 20},
		Workspace: config.WorkspaceConfig{Root: root},
		Hooks:     config.HooksConfig{TimeoutMS: 5000},
		Tracker: config.TrackerConfig{
			Kind:            fixtureNotifyTrackerKind,
			ActiveStates:    []string{"todo"},
			TerminalStates:  []string{"done"},
			HandoffState:    "",
			HandoffEvidence: config.HandoffEvidenceOff,
		},
		Agent: config.AgentConfig{
			Kind:                fixtureNotifyAgentKind,
			MaxTurns:            1,
			MaxConcurrentAgents: 1,
			ReadTimeoutMS:       5000,
			TurnTimeoutMS:       30000,
			StopGraceMS:         5000,
		},
	}
}

// startNotifyOrchestrator migrates a fresh database at dbPath, wires
// tracker and agent into a real orchestrator.Orchestrator, and runs it
// in the background until the test ends.
func startNotifyOrchestrator(t *testing.T, cfg config.ServiceConfig, tracker domain.TrackerAdapter, agent domain.AgentAdapter, dbPath string) {
	t.Helper()

	ctx := context.Background()
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

	tmpl, err := prompt.Parse("do {{ .issue.identifier }}", "test", 0)
	if err != nil {
		t.Fatalf("prompt.Parse: %v", err)
	}
	wm := &notifyE2EWorkflowManager{
		config:   cfg,
		template: tmpl,
		absPath:  filepath.Join(filepath.Dir(dbPath), "WORKFLOW.md"),
	}

	state := orchestrator.NewState(cfg.Polling.IntervalMS, cfg.Agent.MaxConcurrentAgents, 0, nil, orchestrator.AgentTotals{})

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
			if kind == cfg.Agent.Kind {
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
}

// waitHandle receives the next turnController a fixture agent hands out
// through handles, failing the test if none arrives promptly.
func waitHandle(t *testing.T, handles <-chan *turnController) *turnController {
	t.Helper()
	select {
	case tc := <-handles:
		return tc
	case <-time.After(20 * time.Second):
		t.Fatal("StartSession was not called within 20 seconds")
		return nil
	}
}

// waitEntered blocks until tc's RunTurn call has begun, meaning the
// session-start and turn-start dispatch identity records have already
// been written for that dispatch.
func waitEntered(t *testing.T, tc *turnController) {
	t.Helper()
	select {
	case <-tc.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("RunTurn did not begin within 20 seconds")
	}
}

// sendEventAndWait hands event to the adapter's open turn and blocks
// until the relay's OnEvent call has returned, so the caller observes
// only post-relay state, never a fixed sleep.
func sendEventAndWait(t *testing.T, tc *turnController, event domain.AgentEvent) {
	t.Helper()
	select {
	case tc.events <- event:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out handing an event to the open turn")
	}
	select {
	case <-tc.reported:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the event to be reported as relayed")
	}
}

// readMCPEnv reads the sortie-tools env block out of the mcp.json the
// worker wrote at mcpConfigPath.
func readMCPEnv(t *testing.T, mcpConfigPath string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", mcpConfigPath, err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", mcpConfigPath, err)
	}
	entry, ok := parsed.McpServers["sortie-tools"]
	if !ok {
		t.Fatalf("mcp.json %q has no sortie-tools entry", mcpConfigPath)
	}
	return entry.Env
}

// sessionParamsFromMCPConfig resolves SessionToolParams the same way
// the sidecar does: reading the generated mcp.json's environment
// through sessionToolParamsFromEnv.
func sessionParamsFromMCPConfig(t *testing.T, cfg config.ServiceConfig, mcpConfigPath string) SessionToolParams {
	t.Helper()
	env := readMCPEnv(t, mcpConfigPath)
	getenv := func(key string) string { return env[key] }
	return sessionToolParamsFromEnv(getenv, cfg, nil)
}

// buildNotifyRegistry builds the session tool registry through the same
// entry point the sidecar uses, closing its store, if any, at test end.
func buildNotifyRegistry(t *testing.T, params SessionToolParams) SessionToolRegistry {
	t.Helper()
	reg, err := BuildSessionToolRegistry(context.Background(), slog.New(slog.DiscardHandler), params)
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
	return reg
}

// mustNotifyTool retrieves notify_operator from reg or fails the test.
func mustNotifyTool(t *testing.T, reg SessionToolRegistry) domain.AgentTool {
	t.Helper()
	tool, ok := reg.Registry.Get("notify_operator")
	if !ok {
		t.Fatal("notify_operator not registered")
	}
	return tool
}

// notifyExecResult decodes the fields of notify_operator's response
// this file's tests assert on.
type notifyExecResult struct {
	Success bool `json:"success"`
	Error   struct {
		Kind string `json:"kind"`
	} `json:"error"`
}

// execNotify calls notify_operator with a minimal valid input and
// decodes its result envelope.
func execNotify(t *testing.T, tool domain.AgentTool) notifyExecResult {
	t.Helper()
	raw, err := tool.Execute(context.Background(), json.RawMessage(`{"severity":"info","title":"T","body":"B"}`))
	if err != nil {
		t.Fatalf("notify_operator Execute: %v", err)
	}
	var res notifyExecResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal notify_operator result %q: %v", raw, err)
	}
	return res
}

// bodyRecorder captures posted webhook bodies from a spawned server
// goroutine, safe for concurrent reads from the test body.
type bodyRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *bodyRecorder) add(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, append([]byte(nil), b...))
}

func (r *bodyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// latest decodes the most recently captured body, failing the test if
// none has arrived yet.
func (r *bodyRecorder) latest(t *testing.T) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		t.Fatal("no webhook request captured yet")
	}
	var m map[string]any
	if err := json.Unmarshal(r.bodies[len(r.bodies)-1], &m); err != nil {
		t.Fatalf("unmarshal captured webhook body: %v", err)
	}
	return m
}

// newCapturingServer starts an httptest.Server recording every posted
// body into the returned recorder.
func newCapturingServer(t *testing.T) (*httptest.Server, *bodyRecorder) {
	t.Helper()
	rec := &bodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err == nil {
			rec.add(b)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// assertNotifyBody fails the test unless body carries exactly
// wantDispatchID and wantSessionID.
func assertNotifyBody(t *testing.T, body map[string]any, wantDispatchID, wantSessionID string) {
	t.Helper()
	if got, _ := body["dispatch_id"].(string); got != wantDispatchID {
		t.Errorf("body[dispatch_id] = %q, want %q", got, wantDispatchID)
	}
	got, present := body["session_id"]
	if !present {
		t.Fatal("body has no session_id key, want present")
	}
	gotStr, _ := got.(string)
	if gotStr != wantSessionID {
		t.Errorf("body[session_id] = %q, want %q", gotStr, wantSessionID)
	}
}

// TestMCPServerNotify_EndToEnd runs a real orchestrator.Orchestrator
// dispatch against a fake tracker and a fake domain.AgentAdapter, reads
// the worker's own generated mcp.json, resolves it through
// sessionToolParamsFromEnv, builds the registry through
// BuildSessionToolRegistry, and executes the registered notify_operator
// tool. No dispatch id or session id enters SessionToolParams,
// NotificationEnvelopeContext, notify.New, or .sortie/dispatch.json
// except through DispatchIssue, the worker, and the fake adapter's own
// StartSession result and events. It carries no build tag and no
// SORTIE_*_TEST gate, so it runs on every CI platform.
func TestMCPServerNotify_EndToEnd(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tracker := &notifyE2ETracker{issues: []domain.Issue{
		{ID: "issue-e2e", Identifier: "NOTIFY-E2E-1", Title: "notify e2e fixture", State: "todo"},
	}}
	agent := newSequentialAgent(func(int) string { return "" })

	cfg := notifyE2EBaseConfig(tmpDir)

	srv, bodies := newCapturingServer(t)
	cfg.Notifications = config.NotificationsConfig{Backends: []config.NotificationBackend{
		{Kind: "webhook", Config: map[string]any{"url": srv.URL}, MaxPerSession: 3},
	}}

	startNotifyOrchestrator(t, cfg, tracker, agent, filepath.Join(tmpDir, "notify-e2e.db"))

	tc := waitHandle(t, agent.handles)
	waitEntered(t, tc)

	params := sessionParamsFromMCPConfig(t, cfg, tc.startParams.MCPConfigPath)
	dispatchID := params.DispatchID
	if dispatchID == "" {
		t.Fatal("SORTIE_DISPATCH_ID recorded in mcp.json is empty, want non-empty")
	}

	reg := buildNotifyRegistry(t, params)
	tool := mustNotifyTool(t, reg)

	res := execNotify(t, tool)
	if !res.Success {
		t.Fatalf("first notify_operator call failed: %+v", res)
	}
	assertNotifyBody(t, bodies.latest(t), dispatchID, "")

	sendEventAndWait(t, tc, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "S1", Timestamp: time.Now().UTC()})
	res = execNotify(t, tool)
	if !res.Success {
		t.Fatalf("second notify_operator call failed: %+v", res)
	}
	assertNotifyBody(t, bodies.latest(t), dispatchID, "S1")

	sendEventAndWait(t, tc, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "S2", Timestamp: time.Now().UTC()})
	res = execNotify(t, tool)
	if !res.Success {
		t.Fatalf("third notify_operator call failed: %+v", res)
	}
	assertNotifyBody(t, bodies.latest(t), dispatchID, "S2")

	sendEventAndWait(t, tc, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "S3", Timestamp: time.Now().UTC()})
	res = execNotify(t, tool)
	if res.Success {
		t.Fatal("fourth notify_operator call succeeded, want rate_limited")
	}
	if res.Error.Kind != "rate_limited" {
		t.Errorf("fourth call error.kind = %q, want %q", res.Error.Kind, "rate_limited")
	}
	if got := bodies.count(); got != 3 {
		t.Errorf("webhook server received %d requests, want exactly 3", got)
	}

	close(tc.finish)
}

// TestMCPServerNotify_Continuation proves a continuation keeps the
// runtime session id while minting a new dispatch id: the first
// dispatch's StartSession returns a non-empty session id and sends one
// notification, exits normally with the issue still active and no
// handoff state configured, and the natural continuation retry that
// follows resumes under that session id while its own tool server posts
// a distinct dispatch id.
func TestMCPServerNotify_Continuation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tracker := &notifyE2ETracker{issues: []domain.Issue{
		{ID: "issue-cont", Identifier: "NOTIFY-CONT-1", Title: "notify continuation fixture", State: "todo"},
	}}
	agent := newSequentialAgent(func(call int) string {
		if call == 0 {
			return "S0"
		}
		return ""
	})

	cfg := notifyE2EBaseConfig(tmpDir)

	srv, bodies := newCapturingServer(t)
	cfg.Notifications = config.NotificationsConfig{Backends: []config.NotificationBackend{
		{Kind: "webhook", Config: map[string]any{"url": srv.URL}, MaxPerSession: 10},
	}}

	startNotifyOrchestrator(t, cfg, tracker, agent, filepath.Join(tmpDir, "notify-continuation.db"))

	tc1 := waitHandle(t, agent.handles)
	waitEntered(t, tc1)
	if tc1.sessionID != "S0" {
		t.Fatalf("first dispatch StartSession returned %q, want %q", tc1.sessionID, "S0")
	}

	params1 := sessionParamsFromMCPConfig(t, cfg, tc1.startParams.MCPConfigPath)
	dispatch1 := params1.DispatchID
	if dispatch1 == "" {
		t.Fatal("first dispatch DispatchID empty, want non-empty")
	}

	tool1 := mustNotifyTool(t, buildNotifyRegistry(t, params1))
	res1 := execNotify(t, tool1)
	if !res1.Success {
		t.Fatalf("first dispatch notify_operator call failed: %+v", res1)
	}
	assertNotifyBody(t, bodies.latest(t), dispatch1, "S0")

	close(tc1.finish)

	tc2 := waitHandle(t, agent.handles)
	waitEntered(t, tc2)

	if tc2.startParams.ResumeSessionID != "S0" {
		t.Fatalf("continuation StartSessionParams.ResumeSessionID = %q, want %q", tc2.startParams.ResumeSessionID, "S0")
	}
	if tc2.sessionID != "S0" {
		t.Fatalf("continuation StartSession returned %q, want %q (adopts the resume id)", tc2.sessionID, "S0")
	}

	params2 := sessionParamsFromMCPConfig(t, cfg, tc2.startParams.MCPConfigPath)
	dispatch2 := params2.DispatchID
	if dispatch2 == "" {
		t.Fatal("continuation DispatchID empty, want non-empty")
	}
	if dispatch2 == dispatch1 {
		t.Fatalf("continuation dispatch id %q equals the first dispatch's, want a distinct id", dispatch2)
	}

	tool2 := mustNotifyTool(t, buildNotifyRegistry(t, params2))
	res2 := execNotify(t, tool2)
	if !res2.Success {
		t.Fatalf("continuation notify_operator call failed: %+v", res2)
	}
	assertNotifyBody(t, bodies.latest(t), dispatch2, "S0")

	close(tc2.finish)
}

// TestMCPServerNotify_NoSessionID proves that when StartSession returns
// "" and no event ever carries a non-empty session id, including a
// session_started event with an empty one, every notification of the
// dispatch carries a non-empty dispatch id and an empty session id.
func TestMCPServerNotify_NoSessionID(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tracker := &notifyE2ETracker{issues: []domain.Issue{
		{ID: "issue-nosess", Identifier: "NOTIFY-NOSESS-1", Title: "notify no-session fixture", State: "todo"},
	}}
	agent := newSequentialAgent(func(int) string { return "" })

	cfg := notifyE2EBaseConfig(tmpDir)

	srv, bodies := newCapturingServer(t)
	cfg.Notifications = config.NotificationsConfig{Backends: []config.NotificationBackend{
		{Kind: "webhook", Config: map[string]any{"url": srv.URL}, MaxPerSession: 10},
	}}

	startNotifyOrchestrator(t, cfg, tracker, agent, filepath.Join(tmpDir, "notify-nosession.db"))

	tc := waitHandle(t, agent.handles)
	waitEntered(t, tc)

	params := sessionParamsFromMCPConfig(t, cfg, tc.startParams.MCPConfigPath)
	dispatchID := params.DispatchID
	if dispatchID == "" {
		t.Fatal("DispatchID empty, want non-empty")
	}

	tool := mustNotifyTool(t, buildNotifyRegistry(t, params))

	res := execNotify(t, tool)
	if !res.Success {
		t.Fatalf("first notify_operator call failed: %+v", res)
	}
	assertNotifyBody(t, bodies.latest(t), dispatchID, "")

	sendEventAndWait(t, tc, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "", Timestamp: time.Now().UTC()})

	res = execNotify(t, tool)
	if !res.Success {
		t.Fatalf("second notify_operator call failed: %+v", res)
	}
	assertNotifyBody(t, bodies.latest(t), dispatchID, "")

	close(tc.finish)
}

// TestMCPServerNotify_Concurrent runs two dispatches at once and proves
// each notification carries only its own dispatch's identity: each is
// sent after the other dispatch's session_started event, so a leak
// through a shared workspace key or a misrouted read would surface as
// the wrong session id or a colliding dispatch id.
func TestMCPServerNotify_Concurrent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	tracker := &notifyE2ETracker{issues: []domain.Issue{
		{ID: "issue-conc-a", Identifier: "NOTIFY-CONC-A", Title: "notify concurrent fixture A", State: "todo"},
		{ID: "issue-conc-b", Identifier: "NOTIFY-CONC-B", Title: "notify concurrent fixture B", State: "todo"},
	}}
	agent := newConcurrentAgent(func(call int) string { return fmt.Sprintf("corr-%d", call) })

	cfg := notifyE2EBaseConfig(tmpDir)
	cfg.Agent.MaxConcurrentAgents = 2

	srv, bodies := newCapturingServer(t)
	cfg.Notifications = config.NotificationsConfig{Backends: []config.NotificationBackend{
		{Kind: "webhook", Config: map[string]any{"url": srv.URL}, MaxPerSession: 10},
	}}

	startNotifyOrchestrator(t, cfg, tracker, agent, filepath.Join(tmpDir, "notify-concurrent.db"))

	tcA := waitHandle(t, agent.handles)
	tcB := waitHandle(t, agent.handles)
	waitEntered(t, tcA)
	waitEntered(t, tcB)

	paramsA := sessionParamsFromMCPConfig(t, cfg, tcA.startParams.MCPConfigPath)
	paramsB := sessionParamsFromMCPConfig(t, cfg, tcB.startParams.MCPConfigPath)
	if paramsA.DispatchID == "" || paramsB.DispatchID == "" {
		t.Fatalf("dispatch id empty: A=%q B=%q", paramsA.DispatchID, paramsB.DispatchID)
	}
	if paramsA.DispatchID == paramsB.DispatchID {
		t.Fatalf("both dispatches carry the same dispatch id %q, want distinct", paramsA.DispatchID)
	}

	toolA := mustNotifyTool(t, buildNotifyRegistry(t, paramsA))
	toolB := mustNotifyTool(t, buildNotifyRegistry(t, paramsB))

	// Each dispatch reports its own accepted session id before either
	// notify_operator call, so each dispatch's notification is sent
	// after the other dispatch's session_started event.
	sendEventAndWait(t, tcA, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "S-A", Timestamp: time.Now().UTC()})
	sendEventAndWait(t, tcB, domain.AgentEvent{Type: domain.EventSessionStarted, SessionID: "S-B", Timestamp: time.Now().UTC()})

	resA := execNotify(t, toolA)
	if !resA.Success {
		t.Fatalf("dispatch A notify_operator call failed: %+v", resA)
	}
	bodyA := bodies.latest(t)
	assertNotifyBody(t, bodyA, paramsA.DispatchID, "S-A")

	resB := execNotify(t, toolB)
	if !resB.Success {
		t.Fatalf("dispatch B notify_operator call failed: %+v", resB)
	}
	bodyB := bodies.latest(t)
	assertNotifyBody(t, bodyB, paramsB.DispatchID, "S-B")

	close(tcA.finish)
	close(tcB.finish)
}
