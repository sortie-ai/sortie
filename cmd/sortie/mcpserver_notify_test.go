package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// Preflight resolves configured kinds through the registries even when this
// harness injects adapters directly.
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

// notifyE2ESessionMeta lets RunTurn answer a credential verification
// session immediately, without consuming a slot in the controller
// queues that drive the working sessions.
type notifyE2ESessionMeta struct {
	credentialVerification bool
}

func (a *sequentialAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.CredentialVerification {
		return domain.Session{ID: "verify", Internal: &notifyE2ESessionMeta{credentialVerification: true}}, nil
	}
	call := int(a.calls.Add(1)) - 1
	id := params.ResumeSessionID
	if id == "" {
		id = a.sessionIDFor(call)
	}
	tc := newTurnController(id, params)
	a.pending <- tc
	a.handles <- tc
	return domain.Session{ID: id, Internal: &notifyE2ESessionMeta{}}, nil
}

func (a *sequentialAgent) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if meta, ok := session.Internal.(*notifyE2ESessionMeta); ok && meta.credentialVerification {
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}
	tc := <-a.pending
	return runControlledTurn(ctx, tc, session, params.OnEvent)
}

func (a *sequentialAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

type concurrentAgent struct {
	sessionIDFor func(call int) string
	calls        atomic.Int64
	verifyCalls  atomic.Int64
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
	if params.CredentialVerification {
		return domain.Session{ID: "verify-" + strconv.FormatInt(a.verifyCalls.Add(1), 10), Internal: &notifyE2ESessionMeta{credentialVerification: true}}, nil
	}
	call := int(a.calls.Add(1)) - 1
	id := a.sessionIDFor(call)
	tc := newTurnController(id, params)
	a.mu.Lock()
	a.controllers[id] = tc
	a.mu.Unlock()
	a.handles <- tc
	return domain.Session{ID: id, Internal: &notifyE2ESessionMeta{}}, nil
}

func (a *concurrentAgent) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if meta, ok := session.Internal.(*notifyE2ESessionMeta); ok && meta.credentialVerification {
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}
	a.mu.Lock()
	tc := a.controllers[session.ID]
	a.mu.Unlock()
	return runControlledTurn(ctx, tc, session, params.OnEvent)
}

func (a *concurrentAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

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

func waitForDispatchIdentityWritten(t *testing.T, tc *turnController) {
	t.Helper()
	select {
	case <-tc.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("RunTurn did not begin within 20 seconds")
	}
}

// Wait for OnEvent to return so callers observe post-relay state without sleeps.
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

func sessionParamsFromMCPConfig(t *testing.T, cfg config.ServiceConfig, mcpConfigPath string) SessionToolParams {
	t.Helper()
	env := readMCPEnv(t, mcpConfigPath)
	getenv := func(key string) string { return env[key] }
	return sessionToolParamsFromEnv(getenv, cfg, nil)
}

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

func mustNotifyTool(t *testing.T, reg SessionToolRegistry) domain.AgentTool {
	t.Helper()
	tool, ok := reg.Registry.Get("notify_operator")
	if !ok {
		t.Fatal("notify_operator not registered")
	}
	return tool
}

type notifyExecResult struct {
	Success bool `json:"success"`
	Error   struct {
		Kind string `json:"kind"`
	} `json:"error"`
}

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

// Protects bodies written by the server goroutine and read by the test.
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

func (r *bodyRecorder) nth(t *testing.T, i int) map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.bodies) {
		t.Fatalf("bodyRecorder.nth(%d): only %d bodies recorded", i, len(r.bodies))
	}
	var m map[string]any
	if err := json.Unmarshal(r.bodies[i], &m); err != nil {
		t.Fatalf("unmarshal captured webhook body %d: %v", i, err)
	}
	return m
}

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
	waitForDispatchIdentityWritten(t, tc)

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
	waitForDispatchIdentityWritten(t, tc1)
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
	waitForDispatchIdentityWritten(t, tc2)

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
	waitForDispatchIdentityWritten(t, tc)

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
	waitForDispatchIdentityWritten(t, tcA)
	waitForDispatchIdentityWritten(t, tcB)

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

	// Deliver both session events before either notification to expose shared-state leaks.
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

type mcpServerLaunch struct {
	Args []string
	Env  map[string]string
}

func parseMCPServerLaunch(mcpConfigPath string) (mcpServerLaunch, error) {
	raw, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		return mcpServerLaunch{}, err
	}
	var parsed struct {
		McpServers map[string]struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return mcpServerLaunch{}, err
	}
	entry, ok := parsed.McpServers["sortie-tools"]
	if !ok {
		return mcpServerLaunch{}, fmt.Errorf("mcp.json %q has no sortie-tools entry", mcpConfigPath)
	}
	return mcpServerLaunch{Args: entry.Args, Env: entry.Env}, nil
}

func workflowPathFromArgs(args []string) (string, error) {
	for i, a := range args {
		if a == "--workflow" && i+1 < len(args) {
			return args[i+1], nil
		}
	}
	return "", fmt.Errorf("mcp-server args %v carry no --workflow value", args)
}

type jsonRPCTestRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func buildMCPRequestLine(method string, id, params any) (string, error) {
	b, err := json.Marshal(jsonRPCTestRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func parseNotifyToolCallLine(line []byte) (notifyExecResult, error) {
	var resp struct {
		Error  any `json:"error"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return notifyExecResult{}, fmt.Errorf("unmarshal tools/call response %q: %w", line, err)
	}
	if resp.Error != nil {
		return notifyExecResult{}, fmt.Errorf("tools/call JSON-RPC error: %v", resp.Error)
	}
	if len(resp.Result.Content) == 0 {
		return notifyExecResult{}, fmt.Errorf("tools/call response has no content: %s", line)
	}
	var res notifyExecResult
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &res); err != nil {
		return notifyExecResult{}, fmt.Errorf("unmarshal notify_operator result %q: %w", resp.Result.Content[0].Text, err)
	}
	return res, nil
}

// runNotifyTurnAsSubprocess runs on the orchestrator's own goroutine, not
// the test's, so it reports failure through its error return rather than
// any *testing.T method; the caller relays that error from the test's own
// goroutine.
func runNotifyTurnAsSubprocess(mcpConfigPath string) (pid int, res notifyExecResult, err error) {
	launch, err := parseMCPServerLaunch(mcpConfigPath)
	if err != nil {
		return 0, notifyExecResult{}, err
	}
	workflowPath, err := workflowPathFromArgs(launch.Args)
	if err != nil {
		return 0, notifyExecResult{}, err
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMCPServerNotify_MultiProcessCapSharing")
	cmd.Env = append(os.Environ(), "SORTIE_TEST_MCP_HELPER=1", "SORTIE_TEST_MCP_WORKFLOW="+workflowPath)
	for k, v := range launch.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, notifyExecResult{}, fmt.Errorf("StdinPipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, notifyExecResult{}, fmt.Errorf("StdoutPipe: %w", err)
	}
	var stderrBuf lockedBuf
	cmd.Stderr = &stderrBuf

	if startErr := cmd.Start(); startErr != nil {
		return 0, notifyExecResult{}, fmt.Errorf("start mcp-server subprocess: %w", startErr)
	}
	pid = cmd.Process.Pid

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10<<20)

	initLine, err := buildMCPRequestLine("initialize", 1, map[string]any{"protocolVersion": "2024-11-05"})
	if err != nil {
		return pid, notifyExecResult{}, err
	}
	if _, writeErr := io.WriteString(stdin, initLine); writeErr != nil {
		return pid, notifyExecResult{}, fmt.Errorf("write initialize request: %w", writeErr)
	}
	if !scanner.Scan() {
		return pid, notifyExecResult{}, fmt.Errorf("no initialize response (scanner err: %v, stderr: %s)", scanner.Err(), stderrBuf.String())
	}

	callLine, err := buildMCPRequestLine("tools/call", 2, map[string]any{
		"name":      "notify_operator",
		"arguments": map[string]any{"severity": "info", "title": "T", "body": "B"},
	})
	if err != nil {
		return pid, notifyExecResult{}, err
	}
	if _, writeErr := io.WriteString(stdin, callLine); writeErr != nil {
		return pid, notifyExecResult{}, fmt.Errorf("write notify_operator request: %w", writeErr)
	}
	if !scanner.Scan() {
		return pid, notifyExecResult{}, fmt.Errorf("no notify_operator response (scanner err: %v, stderr: %s)", scanner.Err(), stderrBuf.String())
	}
	res, err = parseNotifyToolCallLine(scanner.Bytes())
	if err != nil {
		return pid, notifyExecResult{}, err
	}

	if closeErr := stdin.Close(); closeErr != nil {
		return pid, res, fmt.Errorf("close mcp-server subprocess stdin: %w", closeErr)
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		return pid, res, fmt.Errorf("mcp-server subprocess exited with error: %w (stderr: %s)", waitErr, stderrBuf.String())
	}

	return pid, res, nil
}

type multiProcessTurnOutcome struct {
	pid int
	res notifyExecResult
	err error
}

type multiProcessSessionMeta struct {
	credentialVerification bool
	capped                 bool
	mcpConfigPath          string
}

type processPerTurnAgent struct {
	cfg config.ServiceConfig

	calls atomic.Int64

	mu              sync.Mutex
	firstDispatchID string

	turns chan multiProcessTurnOutcome
}

func newProcessPerTurnAgent(cfg config.ServiceConfig) *processPerTurnAgent {
	return &processPerTurnAgent{
		cfg:   cfg,
		turns: make(chan multiProcessTurnOutcome, 8),
	}
}

var _ domain.AgentAdapter = (*processPerTurnAgent)(nil)

func (a *processPerTurnAgent) firstDispatchIDValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.firstDispatchID
}

func (a *processPerTurnAgent) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.CredentialVerification {
		return domain.Session{ID: "verify", Internal: &multiProcessSessionMeta{credentialVerification: true}}, nil
	}

	launch, err := parseMCPServerLaunch(params.MCPConfigPath)
	if err != nil {
		return domain.Session{}, err
	}
	getenv := func(key string) string { return launch.Env[key] }
	dispatchID := sessionToolParamsFromEnv(getenv, a.cfg, nil).DispatchID

	a.mu.Lock()
	if a.firstDispatchID == "" {
		a.firstDispatchID = dispatchID
	}
	capped := dispatchID == a.firstDispatchID
	a.mu.Unlock()

	call := int(a.calls.Add(1)) - 1
	return domain.Session{
		ID: fmt.Sprintf("multiprocess-session-%d", call),
		Internal: &multiProcessSessionMeta{
			capped:        capped,
			mcpConfigPath: params.MCPConfigPath,
		},
	}, nil
}

func (a *processPerTurnAgent) RunTurn(_ context.Context, session domain.Session, _ domain.RunTurnParams) (domain.TurnResult, error) {
	meta, _ := session.Internal.(*multiProcessSessionMeta)
	if meta == nil || meta.credentialVerification || !meta.capped {
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
	}

	pid, res, err := runNotifyTurnAsSubprocess(meta.mcpConfigPath)
	a.turns <- multiProcessTurnOutcome{pid: pid, res: res, err: err}
	if err != nil {
		return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnFailed}, err
	}

	return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
}

func (a *processPerTurnAgent) StopSession(_ context.Context, _ domain.Session) error { return nil }

func writeMultiProcessWorkflowFile(t *testing.T, path, webhookURL string) {
	t.Helper()
	content := fmt.Sprintf(`---
polling:
  interval_ms: 30000
tracker:
  kind: file
  api_key: "unused"
  active_states:
    - To Do
  terminal_states:
    - Done
agent:
  kind: mock
file:
  path: issues.json
notifications:
  - kind: webhook
    url: %q
    max_per_session: 2
---
Do {{ .issue.title }}.
`, webhookURL)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func TestMCPServerNotify_MultiProcessCapSharing(t *testing.T) {
	if os.Getenv("SORTIE_TEST_MCP_HELPER") == "1" {
		wfPath := os.Getenv("SORTIE_TEST_MCP_WORKFLOW")
		os.Exit(runMCPServer(context.Background(), []string{"--workflow", wfPath}, os.Stdout, os.Stderr))
		return // unreachable, silences staticcheck
	}

	tmpDir := t.TempDir()
	tracker := &notifyE2ETracker{issues: []domain.Issue{
		{ID: "issue-multiprocess", Identifier: "NOTIFY-MULTI-1", Title: "notify multi-process fixture", State: "todo"},
	}}

	cfg := notifyE2EBaseConfig(tmpDir)
	cfg.Agent.MaxTurns = 3

	srv, bodies := newCapturingServer(t)

	dbPath := filepath.Join(tmpDir, "notify-multiprocess.db")
	workflowPath := filepath.Join(filepath.Dir(dbPath), "WORKFLOW.md")
	writeMultiProcessWorkflowFile(t, workflowPath, srv.URL)

	agent := newProcessPerTurnAgent(cfg)

	startNotifyOrchestrator(t, cfg, tracker, agent, dbPath)

	var outcomes []multiProcessTurnOutcome
	for turn := 1; turn <= 3; turn++ {
		select {
		case o := <-agent.turns:
			if o.err != nil {
				t.Fatalf("turn %d: %v", turn, o.err)
			}
			outcomes = append(outcomes, o)
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for turn %d's notify_operator call", turn)
		}
	}

	if !outcomes[0].res.Success {
		t.Errorf("turn 1 result = %+v, want success", outcomes[0].res)
	}
	if !outcomes[1].res.Success {
		t.Errorf("turn 2 result = %+v, want success", outcomes[1].res)
	}
	if outcomes[2].res.Success {
		t.Errorf("turn 3 result = %+v, want rate_limited, got success", outcomes[2].res)
	}
	if got := outcomes[2].res.Error.Kind; got != "rate_limited" {
		t.Errorf("turn 3 error.kind = %q, want %q", got, "rate_limited")
	}

	n := bodies.count()
	if n != 2 {
		t.Errorf("webhook server received %d requests, want exactly 2", n)
	}
	dispatchID := agent.firstDispatchIDValue()
	for i := range n {
		body := bodies.nth(t, i)
		if got, _ := body["dispatch_id"].(string); got != dispatchID {
			t.Errorf("request %d dispatch_id = %q, want %q", i, got, dispatchID)
		}
	}
}
