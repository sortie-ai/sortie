package fakemodel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// Scenario selects which script the driver runs.
type Scenario int

const (
	// ScenarioTurn scripts one read call and one closing answer.
	ScenarioTurn Scenario = iota + 1

	// ScenarioExhaustion scripts the read call alone, so the request that
	// follows its result arrives after the script is exhausted.
	ScenarioExhaustion

	// ScenarioPermissionRefusal scripts a guarded call the session's policy
	// refuses, then a closing answer.
	ScenarioPermissionRefusal

	// ScenarioToolServer scripts a preamble read, the server call and a
	// closing answer, with the Sortie MCP server delivered.
	ScenarioToolServer
)

// Binding is one agent kind's fixture for [AssertConformance]: how to reach
// its installed runtime through the endpoint. The driver supplies everything
// else.
type Binding struct {
	Kind          string         // a registered kind requiring an agent command
	Passthrough   map[string]any // the settings block handed to the session
	CredentialEnv []string       // names given the sentinel beyond the kind's declared credential names
	Read          func(path string) ToolChoice
	SortieStatus  ToolChoice // the call that reaches sortie_status; nil calls the declared tool named for it
	Finish        ToolChoice // the turn's last answer; nil answers with text
	Launch        func(t *testing.T, env Environment) Launch
	Inspect       func(t *testing.T, run Run) // optional adapter-only assertions

	// ToolServerPassthrough is the tool-server scenario's settings; nil hands
	// Passthrough. A binding sets it when its scripted settings let the
	// runtime approve every tool call by itself, which would hide a refused
	// approval grant.
	ToolServerPassthrough map[string]any

	PermissionRefusal *PermissionRefusal // nil runs no refusal scenario
}

// PermissionRefusal is a binding's fixture for the permission refusal
// scenario.
type PermissionRefusal struct {
	Passthrough map[string]any               // the scenario session's settings; nil hands Binding.Passthrough
	Guarded     func(path string) ToolChoice // prints the file at path through a tool the policy gates
	Notice      string                       // the notification the adapter emits for the refusal
	Skip        string                       // non-empty skips the scenario with this reason
}

// Environment is what the driver hands a binding's Launch.
type Environment struct {
	Scenario  Scenario
	URL       string // Server.URL()
	Home      string // HOME and every XDG base directory point here
	Workspace string // a git work tree
	Sentinel  string
	Events    func() []domain.AgentEvent // a copy of the events delivered so far
}

// Launch is how a binding starts its runtime.
type Launch struct {
	Config  domain.AgentConfig
	Env     map[string]string // applied with t.Setenv after the driver's own assignments
	Streams []string          // files capturing the adapter's traffic with the runtime
}

// Run is one driven turn, handed to [Binding.Inspect].
type Run struct {
	Environment Environment
	Session     domain.Session
	Result      domain.TurnResult
	Err         error
	Events      []domain.AgentEvent
	Exchanges   []Exchange
}

// The turn answers carry counts that differ from each other and from every
// other count in play, so a mapping that drops, swaps or double-counts a
// field changes a total. Their basis (Prompt plus Candidates) must stay
// above auxiliaryUsage's.
var (
	firstTurnUsage  = Usage{Prompt: 211, Candidates: 13, Thoughts: 5, CachedContent: 53, CacheWrite: 19}
	sortieToolUsage = Usage{Prompt: 263, Candidates: 11, Thoughts: 3, CachedContent: 41, CacheWrite: 29}
	secondTurnUsage = Usage{Prompt: 307, Candidates: 17, Thoughts: 7, CachedContent: 61, CacheWrite: 23}
)

const (
	turnBound             = 180 * time.Second
	stopBound             = 30 * time.Second
	toolsListedBound      = 30 * time.Second
	toolsRegisteredSettle = 2500 * time.Millisecond
)

// sortieStatusTurn is the turn number the scripted session's state file
// records, so only a sortie_status result can carry it back to the model.
const sortieStatusTurn = 731948265

// serverCallStep is the script entry of the tool-server scenario's server
// call, after the preamble read.
const serverCallStep = 1

// AssertConformance launches b's installed runtime through its adapter
// against a scripted model endpoint and fails t unless the turn completes, the
// reported usage equals everything the endpoint served, each scripted tool
// call reaches one normalized tool result, the credential stays in its header,
// requests beyond the script fail the turn with the endpoint's last message, a
// sortie_status call succeeds through the relay identity the MCP configuration
// declares, and, when b.PermissionRefusal is set, a refused call reaches the
// model as one errored tool result with the notice and without the file's
// content.
//
// It sets environment variables for the rest of the test, so it must not be
// called from a test that uses t.Parallel.
func AssertConformance(t *testing.T, b Binding) {
	t.Helper()

	requireValidBinding(t, b)
	t.Run("scripted turn", func(t *testing.T) { runScenario(t, b, ScenarioTurn) })
	t.Run("script exhaustion", func(t *testing.T) { runScenario(t, b, ScenarioExhaustion) })
	t.Run("tool server", func(t *testing.T) { runToolServer(t, b) })
	if b.PermissionRefusal != nil {
		t.Run("permission refusal", func(t *testing.T) { runPermissionRefusal(t, b) })
	}
}

func requireValidBinding(t *testing.T, b Binding) {
	t.Helper()

	meta, registered := registry.Agents.Meta(b.Kind)
	switch {
	case !registered:
		t.Fatalf("Binding.Kind = %q, want a registered kind", b.Kind)
	case !meta.RequiresCommand:
		t.Fatalf("Binding.Kind = %q, want a kind that requires an agent command", b.Kind)
	case b.Read == nil:
		t.Fatalf("Binding.Read is nil, want a function selecting the read tool")
	case b.Launch == nil:
		t.Fatalf("Binding.Launch is nil, want a function launching the runtime")
	case b.PermissionRefusal != nil && b.PermissionRefusal.Guarded == nil:
		t.Fatalf("Binding.PermissionRefusal.Guarded is nil, want a function selecting the guarded tool")
	case b.PermissionRefusal != nil && b.PermissionRefusal.Notice == "":
		t.Fatalf("Binding.PermissionRefusal.Notice is empty, want the notification the adapter emits for the refusal")
	}
}

func runScenario(t *testing.T, b Binding, scenario Scenario) {
	t.Helper()

	var reporter testing.TB = t
	var recorded *recordingReporter
	if scenario == ScenarioExhaustion {
		recorded = &recordingReporter{TB: t}
		t.Cleanup(recorded.reraise)
		reporter = recorded
	}

	nonce := "nonce-" + randomHex(t, 8)
	workspace := newWorkspace(t)
	file := filepath.Join(workspace, "scripted-"+nonce+".txt")
	if err := os.WriteFile(file, []byte(nonce+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}

	script := []Response{{Call: b.Read(file), Usage: firstTurnUsage}}
	if scenario == ScenarioTurn {
		script = append(script, closingResponse(b))
	}
	server := Start(reporter, script)

	run, launch, timedOut := driveTurn(t, b, drive{
		scenario:  scenario,
		server:    server,
		workspace: workspace,
		settings:  b.Passthrough,
		prompt:    "Read the file " + file + " and report its contents.",
	})

	if scenario == ScenarioExhaustion {
		failures := recorded.snapshot()
		reportViolations(t, exhaustionViolations(run, nonce, timedOut, failures))
		if exhausted := exchangesOfKind(run.Exchanges, ExchangeExhausted); len(exhausted) > 0 && oneFailurePerRequest(failures, exhausted) {
			recorded.discard()
		}
		if b.Inspect != nil {
			b.Inspect(t, run)
		}
		return
	}

	streams := readStreams(launch.Streams)
	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("exact usage", func(t *testing.T) {
		assertExactUsage(t, b, run)
	})
	t.Run("deterministic tool path", func(t *testing.T) {
		reportViolations(t, toolPathViolations(run, nonce))
	})
	t.Run("credential containment", func(t *testing.T) {
		reportViolations(t, containmentViolations(run, streams))
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

func runToolServer(t *testing.T, b Binding) {
	t.Helper()

	nonce := "nonce-" + randomHex(t, 8)
	workspace := newWorkspace(t)
	file := filepath.Join(workspace, "scripted-"+nonce+".txt")
	if err := os.WriteFile(file, []byte(nonce+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}

	tools := agenttest.NewSortieTools(t, workspace)
	writeSortieState(t, workspace)
	script := []Response{
		{Call: afterToolsListed(tools.Relay, b.Read(file)), Usage: firstTurnUsage},
		{Call: sortieStatusCall(b), Usage: sortieToolUsage},
		closingResponse(b),
	}
	settings := b.ToolServerPassthrough
	if settings == nil {
		settings = b.Passthrough
	}
	run, _, _ := driveTurn(t, b, drive{
		scenario:      ScenarioToolServer,
		server:        Start(t, script),
		workspace:     workspace,
		mcpConfigPath: tools.ConfigPath,
		settings:      settings,
		prompt:        "Read the file " + file + " and report its contents.",
	})

	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("tool server", func(t *testing.T) {
		reportViolations(t, toolServerViolations(run, strconv.Itoa(sortieStatusTurn)))
		tools.Relay.AssertToolCallSucceeded(t, agenttest.SortieStatusTool, tools.Identity)
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

type drive struct {
	scenario      Scenario
	server        *Server
	workspace     string
	mcpConfigPath string
	settings      map[string]any
	prompt        string
}

func driveTurn(t *testing.T, b Binding, d drive) (run Run, launch Launch, timedOut bool) {
	t.Helper()

	home := t.TempDir()
	sentinel := "sortie-scripted-credential-" + randomHex(t, 16)
	isolateEnvironment(t, b, home, sentinel)

	events := &eventCollector{}
	environment := Environment{
		Scenario:  d.scenario,
		URL:       d.server.URL(),
		Home:      home,
		Workspace: d.workspace,
		Sentinel:  sentinel,
		Events:    events.snapshot,
	}
	launch = b.Launch(t, environment)
	for _, key := range slices.Sorted(maps.Keys(launch.Env)) {
		t.Setenv(key, launch.Env[key])
	}

	newAdapter, err := registry.Agents.Get(b.Kind)
	if err != nil {
		t.Fatalf("registry.Agents.Get(%q) error = %v", b.Kind, err)
	}
	adapter, err := newAdapter()
	if err != nil {
		t.Fatalf("construct %q adapter error = %v", b.Kind, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), turnBound)
	defer cancel()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: d.workspace,
		AgentConfig:   launch.Config,
		MCPConfigPath: d.mcpConfigPath,
		Settings:      d.settings,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v%s", err, exchangeLines(d.server.Exchanges()))
	}
	result, runErr := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  d.prompt,
		OnEvent: events.collect,
	})
	timedOut = ctx.Err() != nil

	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBound)
	defer stopCancel()
	if err := adapter.StopSession(stopCtx, session); err != nil {
		t.Logf("StopSession() error = %v", err)
	}

	run = Run{
		Environment: environment,
		Session:     session,
		Result:      result,
		Err:         runErr,
		Events:      events.snapshot(),
		Exchanges:   d.server.Exchanges(),
	}
	return run, launch, timedOut
}

func runPermissionRefusal(t *testing.T, b Binding) {
	t.Helper()

	r := b.PermissionRefusal
	if r.Skip != "" {
		t.Skip(r.Skip)
	}

	nonce := "nonce-" + randomHex(t, 8)
	workspace := newWorkspace(t)
	guarded := filepath.Join(t.TempDir(), "guarded.txt")
	if err := os.WriteFile(guarded, []byte(nonce+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", guarded, err)
	}

	script := []Response{{Call: r.Guarded(guarded), Usage: firstTurnUsage}, closingResponse(b)}
	settings := r.Passthrough
	if settings == nil {
		settings = b.Passthrough
	}
	run, _, _ := driveTurn(t, b, drive{
		scenario:  ScenarioPermissionRefusal,
		server:    Start(t, script),
		workspace: workspace,
		settings:  settings,
		prompt:    "Read the file " + guarded + " and report its contents.",
	})

	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("refused call", func(t *testing.T) {
		reportViolations(t, refusedCallViolations(run, nonce, r.Notice))
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

// afterToolsListed holds the answer choice makes until the runtime has listed
// the Sortie tools, so the request after this answer can call one: a runtime
// connects its MCP servers alongside its first model request. The settle
// covers registration that trails the listing with nothing on the wire to
// wait for; without it a runtime was measured leaving the tools out of the
// next request in 6 of 20 runs.
func afterToolsListed(relay agenttest.RecordingMCPRelay, choice ToolChoice) ToolChoice {
	return func(declared []Tool) (FunctionCall, error) {
		if relay.WaitToolsListed(toolsListedBound) {
			time.Sleep(toolsRegisteredSettle)
		}
		return choice(declared)
	}
}

func sortieStatusCall(b Binding) ToolChoice {
	if b.SortieStatus != nil {
		return b.SortieStatus
	}
	return serverTool(agenttest.SortieStatusTool)
}

func closingResponse(b Binding) Response {
	if b.Finish != nil {
		return Response{Call: b.Finish, Usage: secondTurnUsage}
	}
	return Response{Text: "The file holds the nonce.", Usage: secondTurnUsage}
}

// writeSortieState records the session state sortie_status reports, so its
// call succeeds where a session without state would get a tool error.
func writeSortieState(t *testing.T, workspace string) {
	t.Helper()

	state := fmt.Sprintf(`{"turn_number":%d,"max_turns":%d,"started_at":"2026-01-02T03:04:05Z"}`, sortieStatusTurn, sortieStatusTurn)
	if err := workspacekit.WriteSortieFile(workspace, "state.json", []byte(state)); err != nil {
		t.Fatalf("write session state in %s: %v", workspace, err)
	}
}

func newWorkspace(t *testing.T) string {
	t.Helper()

	workspace := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", workspace, "init") //nolint:gosec // workspace is always under t.TempDir()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", workspace, err, out)
	}
	return workspace
}

// isolateEnvironment points HOME and every XDG base directory at home so no
// stored login is reachable.
func isolateEnvironment(t *testing.T, b Binding, home, sentinel string) {
	t.Helper()

	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, home)
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")

	meta, _ := registry.Agents.Meta(b.Kind)
	seen := make(map[string]bool)
	for _, name := range append(meta.CredentialEnv.Names(), b.CredentialEnv...) {
		if !seen[name] {
			seen[name] = true
			t.Setenv(name, sentinel)
		}
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()

	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(raw)
}

func exchangeLines(exchanges []Exchange) string {
	var lines strings.Builder
	for _, ex := range exchanges {
		fmt.Fprintf(&lines, "\n  exchange %d: %s %s %s", ex.Seq, ex.Kind, ex.Method, ex.Path)
	}
	return lines.String()
}

func reportViolations(t *testing.T, violations []string) {
	t.Helper()
	for _, violation := range violations {
		t.Errorf("%s", violation)
	}
}

// eventCollector gathers the events a turn delivers from the adapter's
// goroutines.
type eventCollector struct {
	mu     sync.Mutex
	events []domain.AgentEvent // guarded by mu
}

func (c *eventCollector) collect(event domain.AgentEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *eventCollector) snapshot() []domain.AgentEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

// recordingReporter records the failures a scripted server reports instead of
// failing its test, so a scenario that expects one can consume it.
type recordingReporter struct {
	testing.TB

	mu       sync.Mutex
	failures []string // guarded by mu
}

func (r *recordingReporter) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recordingReporter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.failures)
}

func (r *recordingReporter) discard() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = nil
}

func (r *recordingReporter) reraise() {
	for _, failure := range r.snapshot() {
		r.TB.Errorf("%s", failure)
	}
}

type streamFile struct {
	path    string
	content []byte
	err     error
}

func readStreams(paths []string) []streamFile {
	streams := make([]streamFile, len(paths))
	for i, path := range paths {
		content, err := os.ReadFile(path) //nolint:gosec // the binding names files it created for this run
		streams[i] = streamFile{path: path, content: content, err: err}
	}
	return streams
}

func expectedUsage(exchanges []Exchange) domain.TokenUsage {
	var total domain.TokenUsage
	for _, ex := range exchanges {
		if ex.Kind != ExchangeTurn && ex.Kind != ExchangeAuxiliary {
			continue
		}
		u := ex.Answer.Usage
		total.InputTokens += u.Prompt
		total.OutputTokens += u.Candidates + u.Thoughts
		total.CacheReadTokens += u.CachedContent
		total.CacheWriteTokens += u.CacheWrite
	}
	total.TotalTokens = total.InputTokens + total.OutputTokens
	return total
}

func exchangesOfKind(exchanges []Exchange, kind ExchangeKind) []Exchange {
	var matching []Exchange
	for _, ex := range exchanges {
		if ex.Kind == kind {
			matching = append(matching, ex)
		}
	}
	return matching
}

func requestLabel(ex Exchange) string {
	return fmt.Sprintf("request %d (%s %s)", ex.Seq, ex.Method, ex.Path)
}

type findings struct {
	property string
	items    []string
}

func (v *findings) add(format string, args ...any) {
	v.items = append(v.items, v.property+": "+fmt.Sprintf(format, args...))
}

func turnOutcomeViolations(run Run, entries int) []string {
	v := &findings{property: "turn outcome"}

	if run.Err != nil {
		v.add("RunTurn returned error %v, want nil", run.Err)
	}
	if run.Result.ExitReason != domain.EventTurnCompleted {
		v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnCompleted)
	}
	if run.Result.SessionID == "" {
		v.add("Result.SessionID is empty")
	}

	started := false
	for i, event := range run.Events {
		switch {
		case event.Type == domain.EventSessionStarted && event.SessionID != "":
			started = true
		case event.Type == domain.EventTurnFailed || event.Type == domain.EventStartupFailed:
			v.add("event %d has type %q with message %q, want none", i, event.Type, event.Message)
		}
	}
	if !started {
		v.add("no %q event carried a session id", domain.EventSessionStarted)
	}

	turns := exchangesOfKind(run.Exchanges, ExchangeTurn)
	if len(turns) != entries {
		v.add("%d turn exchanges, want one per script entry (%d)", len(turns), entries)
	}
	for i, ex := range turns {
		if ex.Step != i {
			v.add("%s answered script entry %d, want entry %d", requestLabel(ex), ex.Step, i)
		}
	}
	for _, ex := range run.Exchanges {
		switch ex.Kind {
		case ExchangeExhausted, ExchangeUnmatched, ExchangeUnrouted, ExchangeUnsupported, ExchangeInvalid:
			v.add("%s ended %s: %s", requestLabel(ex), ex.Kind, ex.Answer.Text)
		}
	}
	return v.items
}

func refusedCallViolations(run Run, nonce, notice string) []string {
	v := &findings{property: "refused call"}

	if turns := exchangesOfKind(run.Exchanges, ExchangeTurn); len(turns) >= 2 {
		call, next := turns[0], turns[1]
		answers := slices.DeleteFunc(slices.Clone(next.ToolResults), func(r ToolResult) bool { return !answersCall(call.Answer, r) })
		if len(answers) != 1 {
			v.add("%s carries %d tool results answering the guarded call, want exactly one", requestLabel(next), len(answers))
		}
	}

	for _, ex := range run.Exchanges {
		if strings.Contains(string(ex.Body), nonce) {
			v.add("%s carried the file's content in its body", requestLabel(ex))
		}
	}

	toolResults, erroredResult := 0, -1
	for i, event := range run.Events {
		if event.Type == domain.EventToolResult {
			toolResults++
			if !event.ToolError {
				erroredResult = i
			}
		}
	}
	if toolResults != 1 {
		v.add("%d tool_result events, want exactly one", toolResults)
	}
	if erroredResult >= 0 {
		v.add("event %d reports a successful tool result, want a tool error", erroredResult)
	}

	notices := 0
	for _, event := range run.Events {
		if event.Type == domain.EventNotification && event.Message == notice {
			notices++
		}
	}
	if notices != 1 {
		v.add("%d notification events with message %q, want exactly one", notices, notice)
	}
	return v.items
}

func toolPathViolations(run Run, wantOutput string) []string {
	v := &findings{property: "deterministic tool path"}

	turns := exchangesOfKind(run.Exchanges, ExchangeTurn)
	if len(turns) > 0 && turns[0].Answer.Call == nil {
		v.add("%s was answered with text, want the scripted call", requestLabel(turns[0]))
	}
	if len(turns) > 1 {
		checkToolResult(v, turns[0], turns[1], wantOutput, run.Environment.Sentinel)
	}

	called := 0
	for _, ex := range turns {
		if ex.Answer.Call != nil {
			called++
		}
	}
	toolEvents := 0
	for i, event := range run.Events {
		if event.Type != domain.EventToolResult {
			continue
		}
		toolEvents++
		if event.ToolError {
			v.add("event %d reports a tool error for %q", i, event.ToolName)
		}
		if event.ToolName == "" || event.ToolName == "unknown" {
			v.add("event %d has tool name %q, want the tool's own name", i, event.ToolName)
		}
		if event.ToolDurationMS < 0 {
			v.add("event %d has tool duration %d ms, want at least 0", i, event.ToolDurationMS)
		}
	}
	if toolEvents != called {
		v.add("%d tool_result events, want one per call answered (%d)", toolEvents, called)
	}
	return v.items
}

// toolServerViolations leaves the preamble read unjudged: a runtime may fail
// that read before it runs, and the turn continues with the failure as its
// result.
func toolServerViolations(run Run, wantOutput string) []string {
	v := &findings{property: "tool server"}

	turns := exchangesOfKind(run.Exchanges, ExchangeTurn)
	if len(turns) > serverCallStep {
		call := turns[serverCallStep]
		if call.Answer.Call == nil {
			v.add("%s was answered with text, want the server call", requestLabel(call))
		}
		if serverCallStep+1 < len(turns) {
			checkToolResult(v, call, turns[serverCallStep+1], wantOutput, run.Environment.Sentinel)
		}
	}

	called := 0
	for _, ex := range turns {
		if ex.Answer.Call != nil {
			called++
		}
	}
	var toolEvents []int
	for i, event := range run.Events {
		if event.Type == domain.EventToolResult {
			toolEvents = append(toolEvents, i)
		}
	}
	if len(toolEvents) != called {
		v.add("%d tool_result events, want one per call answered (%d)", len(toolEvents), called)
	}
	if len(toolEvents) > serverCallStep {
		i := toolEvents[serverCallStep]
		event := run.Events[i]
		if event.ToolError {
			v.add("event %d reports a tool error for %q", i, event.ToolName)
		}
		if event.ToolName == "" || event.ToolName == "unknown" {
			v.add("event %d has tool name %q, want the tool's own name", i, event.ToolName)
		}
	}
	return v.items
}

// checkToolResult filters next's tool results down to those answering call,
// because a request resends the whole conversation with earlier calls' results.
func checkToolResult(v *findings, call, next Exchange, want, sentinel string) {
	if call.Answer.Call == nil {
		return
	}
	answers := slices.DeleteFunc(slices.Clone(next.ToolResults), func(r ToolResult) bool { return !answersCall(call.Answer, r) })
	if len(answers) != 1 {
		v.add("%s carries %d tool results answering call %q of tool %q, want exactly one", requestLabel(next), len(answers), call.Answer.CallID, call.Answer.Call.Name)
		return
	}
	result := answers[0]
	if !strings.Contains(result.Output, want) {
		output := result.Output
		if sentinel != "" {
			output = strings.ReplaceAll(output, sentinel, "[redacted]")
		}
		if len(output) > 2000 {
			output = output[:2000] + "... [truncated]"
		}
		v.add("%s carries a tool result whose output does not contain %q; tool %q returned %q", requestLabel(next), want, call.Answer.Call.Name, output)
	}
}

func answersCall(answer Answer, result ToolResult) bool {
	if answer.CallID != "" {
		return result.CallID == answer.CallID
	}
	return result.Name == answer.Call.Name
}

func assertExactUsage(t *testing.T, b Binding, run Run) {
	t.Helper()

	reportViolations(t, usageViolations(run, usageArrival(b)))
	agenttest.AssertUsageContract(t, run.Events)
	agenttest.AssertUsageReportingCase(t, b.Kind, agenttest.UsageReportingCase{
		Name:        "scripted turn",
		Passthrough: b.Passthrough,
		Events:      run.Events,
		Result:      run.Result,
	})
}

func usageArrival(b Binding) registry.UsageArrival {
	meta, _ := registry.Agents.Meta(b.Kind)
	arrival, _ := meta.UsageDisposition(b.Passthrough, false)
	return arrival
}

func usageViolations(run Run, arrival registry.UsageArrival) []string {
	v := &findings{property: "exact usage"}

	want := expectedUsage(run.Exchanges)
	if !run.Result.UsageMeasured {
		v.add("Result.UsageMeasured is false, want a measured figure")
	}
	if run.Result.Usage != want {
		var auxiliary strings.Builder
		for _, ex := range exchangesOfKind(run.Exchanges, ExchangeAuxiliary) {
			fmt.Fprintf(&auxiliary, "; auxiliary %s reported %+v", requestLabel(ex), ex.Answer.Usage)
		}
		v.add("Result.Usage = %+v, want %+v, the sum of every answer the endpoint served%s", run.Result.Usage, want, auxiliary.String())
	}

	if last, ok := lastUsageEvent(run.Events); ok && last.Usage != run.Result.Usage {
		v.add("the last event carrying usage reports %+v, want Result.Usage %+v", last.Usage, run.Result.Usage)
	}
	if arrival == registry.UsageArrivalTurnEnd {
		reported := 0
		for i, event := range run.Events {
			if event.Type != domain.EventTokenUsage {
				continue
			}
			reported++
			if event.Usage != run.Result.Usage {
				v.add("token_usage event %d reports %+v, want Result.Usage %+v", i, event.Usage, run.Result.Usage)
			}
		}
		if reported == 0 {
			v.add("declared turn_end, the turn reported no token_usage event")
		}
	}
	return v.items
}

func lastUsageEvent(events []domain.AgentEvent) (domain.AgentEvent, bool) {
	for _, event := range slices.Backward(events) {
		if event.Usage != (domain.TokenUsage{}) {
			return event, true
		}
	}
	return domain.AgentEvent{}, false
}

func containmentViolations(run Run, streams []streamFile) []string {
	v := &findings{property: "credential containment"}

	sentinel := run.Environment.Sentinel
	turns := exchangesOfKind(run.Exchanges, ExchangeTurn)
	if len(turns) == 0 {
		v.add("no turn exchange reached the endpoint, so the redirect never applied")
	}
	for _, ex := range turns {
		if !slices.Contains(ex.Credentials, sentinel) {
			v.add("%s carried no credential equal to the sentinel", requestLabel(ex))
		}
	}
	for _, ex := range run.Exchanges {
		for _, credential := range ex.Credentials {
			if credential != sentinel {
				v.add("%s carried a credential other than the sentinel", requestLabel(ex))
			}
		}
		if strings.Contains(string(ex.Body), sentinel) {
			v.add("%s carried the sentinel in its body", requestLabel(ex))
		}
	}
	for i, event := range run.Events {
		encoded, err := json.Marshal(event)
		if err != nil {
			v.add("event %d cannot be encoded: %v", i, err)
			continue
		}
		if strings.Contains(string(encoded), sentinel) {
			v.add("event %d (%s) carries the sentinel", i, event.Type)
		}
	}
	for _, stream := range streams {
		switch {
		case stream.err != nil:
			v.add("stream file %s is unreadable: %v", stream.path, stream.err)
		case strings.Contains(string(stream.content), sentinel):
			v.add("stream file %s carries the sentinel", stream.path)
		}
	}
	return v.items
}

func exhaustionViolations(run Run, nonce string, timedOut bool, failures []string) []string {
	v := &findings{property: "script exhaustion"}

	if timedOut {
		v.add("RunTurn did not return inside the %s bound", turnBound)
	}
	if run.Err == nil {
		v.add("RunTurn returned nil, want an error")
	}
	if run.Result.ExitReason != domain.EventTurnFailed {
		v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnFailed)
	}

	turns := exchangesOfKind(run.Exchanges, ExchangeTurn)
	exhausted := exchangesOfKind(run.Exchanges, ExchangeExhausted)
	if len(turns) != 1 || turns[0].Step != 0 || turns[0].Answer.Call == nil {
		v.add("%d turn exchanges, want exactly one, step 0, answered with a call", len(turns))
	}
	if len(exhausted) == 0 {
		v.add("0 exhausted exchanges, want at least one")
	}
	if len(turns) == 1 && len(exhausted) > 0 {
		checkExhaustedExchanges(v, run, turns[0], exhausted, nonce, failures)
	}

	if !run.Result.UsageMeasured {
		v.add("Result.UsageMeasured is false, want a measured figure")
	}
	if want := expectedUsage(run.Exchanges); run.Result.Usage != want {
		v.add("Result.Usage = %+v, want %+v, the sum of every answer the endpoint served", run.Result.Usage, want)
	}
	return v.items
}

// checkExhaustedExchanges accepts more than one request beyond the script: a
// runtime may resend a rejected request, such as once more without an
// optional feature.
func checkExhaustedExchanges(v *findings, run Run, call Exchange, exhausted []Exchange, nonce string, failures []string) {
	message := exhausted[len(exhausted)-1].Answer.Text
	if run.Err == nil || !strings.Contains(run.Err.Error(), message) {
		v.add("RunTurn error %v does not contain the endpoint message %q", run.Err, message)
	}
	failed, ok := lastEventOfType(run.Events, domain.EventTurnFailed)
	switch {
	case !ok:
		v.add("no %q event was delivered", domain.EventTurnFailed)
	case !strings.Contains(failed.Message, message):
		v.add("the last %q event message %q does not contain the endpoint message %q", domain.EventTurnFailed, failed.Message, message)
	}

	for _, ex := range exhausted {
		checkToolResult(v, call, ex, nonce, run.Environment.Sentinel)
	}

	if !oneFailurePerRequest(failures, exhausted) {
		v.add("the endpoint reported %d failures %q, want one naming each of %d exhausted requests", len(failures), failures, len(exhausted))
	}
}

func oneFailurePerRequest(failures []string, exhausted []Exchange) bool {
	if len(failures) != len(exhausted) {
		return false
	}
	for _, ex := range exhausted {
		label := requestLabel(ex)
		if !slices.ContainsFunc(failures, func(f string) bool { return strings.Contains(f, label) }) {
			return false
		}
	}
	return true
}

func lastEventOfType(events []domain.AgentEvent, eventType domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, event := range slices.Backward(events) {
		if event.Type == eventType {
			return event, true
		}
	}
	return domain.AgentEvent{}, false
}
