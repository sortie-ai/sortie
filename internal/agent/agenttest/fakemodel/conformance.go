package fakemodel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/launchtest"
	"github.com/sortie-ai/sortie/internal/agent/toolrelay"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
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

	// ScenarioCancellation scripts one held answer; the driver cancels the
	// turn's context once the endpoint holds the request.
	ScenarioCancellation

	// ScenarioStop scripts one held answer; the driver stops the session
	// once the endpoint holds the request.
	ScenarioStop

	// ScenarioContinuation scripts one answer per turn across sessions that
	// share one home, one workspace and one adapter: an origin session of two
	// turns, a fresh control session, and a session resuming the origin.
	ScenarioContinuation

	// ScenarioCancellationAfterOutput scripts a read call, then one held
	// answer; the driver cancels the turn's context once the endpoint holds
	// the request that follows the tool result, after the model served output.
	ScenarioCancellationAfterOutput

	// ScenarioVerificationContext scripts one plain answer per model request,
	// across a credential-verification session and a working session that share
	// one home and one workspace, each running one turn with the verification
	// prompt.
	ScenarioVerificationContext

	// ScenarioKill scripts a read call, then one held answer; the driver kills
	// the runtime with SIGKILL once the endpoint holds the request that
	// follows the tool result.
	ScenarioKill
)

// Binding is one agent kind's fixture for [AssertConformance]: how to reach
// its installed runtime through the endpoint. The driver supplies everything
// else.
type Binding struct {
	Kind          string         // a registered kind requiring an agent command
	Passthrough   map[string]any // the settings block handed to the session
	CredentialEnv []string       // names given the sentinel beyond the kind's declared credential names
	Read          func(path string) ToolChoice
	SortieStatus  ToolChoice                   // the call that reaches sortie_status; nil calls the declared tool named for it
	Finish        func(text string) ToolChoice // the call ending a turn whose answer is text; nil answers with text
	Launch        func(t *testing.T, env Environment) Launch
	Inspect       func(t *testing.T, run Run) // optional adapter-only assertions

	// ToolServerPassthrough is the tool-server scenario's settings; nil hands
	// Passthrough. A binding sets it when its scripted settings let the
	// runtime approve every tool call by itself, which would hide a refused
	// approval grant.
	ToolServerPassthrough map[string]any

	PermissionRefusal *PermissionRefusal // nil runs no refusal scenario

	// UserInstructions is the path, relative to Environment.Home, of the
	// runtime's user-level instruction file. Required when the binding's
	// declaration does not keep user instructions.
	UserInstructions string

	// ProjectInstructions is the path, relative to the workspace, of a project
	// instruction file the runtime reads beside the generic one the kit always
	// seeds.
	ProjectInstructions string

	// VerificationContext overrides the kind's declaration for this binding's
	// launch, as a protocol runtime's addition does. Zero uses the kind's.
	VerificationContext registry.VerificationContext

	// SSH declares the binding's SSH variant of the tool-server scenario, run
	// against the Docker sshd lab; nil runs no variant.
	SSH *SSHVariant

	// LocalToolsOverHTTP marks a runtime that advertises HTTP tool-server
	// support, so the local scenario must reach the relay over loopback HTTP.
	LocalToolsOverHTTP bool

	// CacheWriteUnreported marks a runtime that reports no cache-write count;
	// its input count still holds those tokens.
	CacheWriteUnreported bool

	// RuntimeFailureFolded marks a runtime whose profile declares the
	// runtime_failure case outcome_folded_into_another: it reports a model
	// request the endpoint failed as a completed turn.
	RuntimeFailureFolded bool

	// ReportedModelPrefix precedes a served model's name in the name the
	// runtime reports. Empty when the runtime reports the served name.
	ReportedModelPrefix string

	// Spend opts the binding into the usage component's contract and names the
	// provenance of each spend source its runtime declares; nil runs the
	// assertions a kind ran before its accounting moved onto the component.
	Spend *SpendSources
}

// SpendSources holds, for each spend source a binding's runtime declares, the
// source's Category and Strategy as its declaration gives them, and the
// completeness the binding expects of a figure its Live source settles.
type SpendSources struct {
	Live    domain.UsageProvenance // settles a completed turn
	Records domain.UsageProvenance // settles a turn cancelled or killed after the model served output

	// LowerBound expects a figure the Live source settles to be a lower bound,
	// SpendUnaccounted true; false, the zero value, expects it complete. The
	// figure is exact per class either way.
	LowerBound bool

	// RecordsAtTurnEnd declares that the records source writes a request's
	// record only when the turn ends, not when the request completes. Records
	// written then cannot settle a killed turn, nor one cancelled after output
	// when CancelEndsProcess is set: the binding expects that turn's amount to
	// be unknown, never zero, with the spend flagged unaccounted.
	RecordsAtTurnEnd bool

	// CancelEndsProcess declares that the kind cancels a turn by ending its
	// runtime process, so a cancel after output leaves the records as a kill
	// does.
	CancelEndsProcess bool
}

// SSHVariant is a binding's fixture for the SSH variant of the tool-server
// scenario.
type SSHVariant struct {
	// Package is the npm package the lab installs at the version HostCommand
	// reports, so the remote run never drifts from the runtime the local
	// scenarios drive.
	Package string

	// HostCommand is the host runtime's command line, queried with --version
	// through [agentcore.QueryRuntimeVersion] as an adapter queries its own.
	HostCommand string

	// Install, when not empty, is the [agenttest.SSHLabParams].Install script
	// in place of the one Package and HostCommand derive.
	Install string

	// RemoteCommand returns the remote shell's command line in place of the
	// one Launch returns, which may name host paths. Nil keeps Launch's.
	RemoteCommand func(env Environment) string

	// Skip, when not empty, skips the variant with this reason.
	Skip string

	// ToolServerSkip, when not empty, skips the scenarios that deliver Sortie's
	// tool server over SSH with this reason; the other SSH scenarios still run.
	ToolServerSkip string
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

	// Remote marks a run in the SSH lab: Launch must name no host path, because
	// the container shares nothing with the host.
	Remote bool
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
	Settings    map[string]any // the settings block the session ran under
	Streams     []string       // Launch.Streams of the run
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

const verificationScriptLength = 16

const (
	turnBound             = 180 * time.Second
	stopBound             = 30 * time.Second
	toolsListedBound      = 30 * time.Second
	toolsRegisteredSettle = 2500 * time.Millisecond

	// interruptBound, from an interrupt until RunTurn returns, sits above the
	// 30 s a persistent kind may wait for an unanswered interrupt at the
	// bindings' read timeout and above the 15 s a fork-per-turn teardown
	// takes at its defaults, and below the 60 s floor of every wait that ends
	// only on a model answer, so inside it only the interrupt can end a held
	// turn.
	interruptBound = 45 * time.Second

	// holdEndBound, from StopSession's return, covers a model connection and a
	// process outside the session's process group, which close and exit a
	// moment after the runtime's own process.
	holdEndBound = 5 * time.Second

	livenessPoll = 25 * time.Millisecond
)

type interruption struct {
	held        bool
	interrupted bool
	returned    bool
	stopped     bool
	stopErr     error
}

type teardown struct {
	aliveWhileHeld []int
	heldLaunches   []func() (released bool, cleanupErr error)
	stillHeld      []Exchange
	remoteLeft     []string
}

// sortieStatusTurn is the turn number the scripted session's state file
// records, so only a sortie_status result can carry it back to the model.
const sortieStatusTurn = 731948265

const serverCallStep = 1

// CustomTurn is one turn a test outside this package scripts.
type CustomTurn struct {
	Script    []Response
	Options   []Option
	Prompt    string         // required
	Settings  map[string]any // nil hands Binding.Passthrough
	Workspace string         // a git work tree; empty creates one
	Remote    bool           // launches in the SSH lab through Binding.SSH
	Sentinel  string         // the credential substituted for every credential variable; empty generates one
}

// AssertConformance drives b's installed runtime through its adapter against a
// scripted model endpoint and fails t when the runtime breaks the adapter
// contract: a turn that does not complete, usage that differs from what the
// endpoint served, a scripted tool call without one normalized tool result, a
// credential outside its header, a refused call that does not reach the model
// as one errored tool result, an interrupted turn or session stop that overruns
// its bound or leaves a process behind, a resumed or fresh session that sends
// the wrong history, or a credential-verification session that carries more
// agent context than its declaration allows.
//
// It sets environment variables for the rest of the test, so it must not be
// called from a test that uses t.Parallel.
func AssertConformance(t *testing.T, b Binding) {
	t.Helper()

	requireValidBinding(t, b)
	t.Run("scripted turn", func(t *testing.T) { runScenario(t, b, ScenarioTurn) })
	t.Run("script exhaustion", func(t *testing.T) { runScenario(t, b, ScenarioExhaustion) })
	t.Run("tool server", func(t *testing.T) { runToolServer(t, b) })
	if b.SSH != nil {
		t.Run("tool server over ssh", func(t *testing.T) { runSSHToolServer(t, b, false) })
		t.Run("tool server over ssh with forwarding refused", func(t *testing.T) { runSSHToolServer(t, b, true) })
		t.Run("turn cancellation over ssh", func(t *testing.T) { runInterruption(t, b, ScenarioCancellation, &remoteLaunch{variant: b.SSH}) })
		if b.Spend != nil {
			t.Run("turn cancellation after output over ssh", func(t *testing.T) {
				runInterruption(t, b, ScenarioCancellationAfterOutput, &remoteLaunch{variant: b.SSH})
			})
			t.Run("turn kill over ssh", func(t *testing.T) { runInterruption(t, b, ScenarioKill, &remoteLaunch{variant: b.SSH}) })
		}
		t.Run("session stop over ssh", func(t *testing.T) { runInterruption(t, b, ScenarioStop, &remoteLaunch{variant: b.SSH}) })
		if b.Spend != nil {
			t.Run("session continuation over ssh", func(t *testing.T) { runContinuation(t, b, &remoteLaunch{variant: b.SSH}) })
		}
	}
	if b.PermissionRefusal != nil {
		t.Run("permission refusal", func(t *testing.T) { runPermissionRefusal(t, b) })
	}
	t.Run("turn cancellation", func(t *testing.T) { runInterruption(t, b, ScenarioCancellation, nil) })
	t.Run("turn cancellation after output", func(t *testing.T) { runInterruption(t, b, ScenarioCancellationAfterOutput, nil) })
	t.Run("session stop", func(t *testing.T) { runInterruption(t, b, ScenarioStop, nil) })
	if b.Spend != nil {
		t.Run("turn kill", func(t *testing.T) { runInterruption(t, b, ScenarioKill, nil) })
	}
	t.Run("session continuation", func(t *testing.T) { runContinuation(t, b, nil) })
	t.Run("credential verification context", func(t *testing.T) { runVerificationContext(t, b) })
}

// RunCustomTurn runs one turn of b's runtime against a scripted endpoint and
// returns the run, asserting nothing about it.
//
// It fails t when turn.Prompt is empty, or when turn.Remote is set and b.SSH is
// nil, and skips a remote turn where the SSH variant would skip. The session
// starts with Environment.Scenario zero and no Sortie tool server. It sets
// environment variables for the rest of the test, so it must not be called from
// a test that uses t.Parallel.
func RunCustomTurn(t *testing.T, b Binding, turn CustomTurn) Run {
	t.Helper()

	requireValidBinding(t, b)
	if turn.Prompt == "" {
		t.Fatalf("CustomTurn.Prompt is empty, want the turn's prompt")
	}
	var remote *remoteLaunch
	if turn.Remote {
		if b.SSH == nil {
			t.Fatalf("CustomTurn.Remote is set, but Binding.SSH is nil, want an SSH variant")
		}
		skipUnlessSSHVariant(t, b)
		remote = &remoteLaunch{variant: b.SSH}
	}
	settings := turn.Settings
	if settings == nil {
		settings = b.Passthrough
	}
	workspace := turn.Workspace
	if workspace == "" {
		workspace = newWorkspace(t)
	}
	d := drive{
		server:    Start(t, turn.Script, turn.Options...),
		workspace: workspace,
		settings:  settings,
		prompt:    turn.Prompt,
		remote:    remote,
		sentinel:  turn.Sentinel,
	}
	run, _ := driveTurn(t, b, d)
	return run
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
	case b.Spend != nil && b.Spend.Live.Source == "":
		t.Fatalf("Binding.Spend.Live names no Source, want the category of the live spend source")
	case b.Spend != nil && b.Spend.Records != (domain.UsageProvenance{}) && b.Spend.Records.Source == "":
		t.Fatalf("Binding.Spend.Records sets a Strategy but names no Source, want the category of the records spend source or none at all")
	}
}

func afterOrBefore(after bool) string {
	if after {
		return "after"
	}
	return "before"
}

func writeScriptedFile(t *testing.T, workspace string) (nonce, file string) {
	t.Helper()

	nonce = "nonce-" + randomHex(t, 8)
	file = filepath.Join(workspace, "scripted-"+nonce+".txt")
	if err := os.WriteFile(file, []byte(nonce+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	return nonce, file
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

	workspace := newWorkspace(t)
	nonce, file := writeScriptedFile(t, workspace)

	script := []Response{{Call: b.Read(file), Usage: firstTurnUsage}}
	if scenario == ScenarioTurn {
		script = append(script, closingResponse(b))
	}
	server := Start(reporter, script)

	run, timedOut := driveTurn(t, b, drive{
		scenario:  scenario,
		server:    server,
		workspace: workspace,
		settings:  b.Passthrough,
		prompt:    "Read the file " + file + " and report its contents.",
	})

	if scenario == ScenarioExhaustion {
		failures := recorded.snapshot()
		reportViolations(t, exhaustionViolations(run, b, usageSettlingOf(b), nonce, timedOut, failures))
		if exhausted := exchangesOfKind(run.Exchanges, ExchangeExhausted); len(exhausted) > 0 && oneFailurePerRequest(failures, exhausted) {
			recorded.discard()
		}
		if b.Inspect != nil {
			b.Inspect(t, run)
		}
		return
	}

	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("exact usage", func(t *testing.T) {
		AssertExactUsage(t, b, run)
	})
	t.Run("deterministic tool path", func(t *testing.T) {
		reportViolations(t, toolPathViolations(run, nonce))
	})
	t.Run("credential containment", func(t *testing.T) {
		AssertContainment(t, run)
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
	run, _ := driveTurn(t, b, drive{
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
		if b.LocalToolsOverHTTP {
			tools.Relay.AssertAcceptNamesBothMedia(t, b.Kind)
		}
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

func skipUnlessSSHVariant(t *testing.T, b Binding) {
	t.Helper()

	if b.SSH.Skip != "" {
		t.Skipf("%s: %s", b.Kind, b.SSH.Skip)
	}
	if reason := agenttest.SSHLabSkipReason(); reason != "" {
		t.Skip(reason)
	}
}

func runSSHToolServer(t *testing.T, b Binding, refused bool) {
	t.Helper()

	skipUnlessSSHVariant(t, b)
	if b.SSH.ToolServerSkip != "" {
		t.Skipf("%s: %s", b.Kind, b.SSH.ToolServerSkip)
	}

	workspace := newWorkspace(t)
	_, file := writeScriptedFile(t, workspace)
	tools := agenttest.NewSortieTools(t, workspace)
	writeSortieState(t, workspace)

	script := []Response{
		{Call: afterToolsListed(tools.Relay, b.Read(file)), Usage: firstTurnUsage},
		{Call: sortieStatusCall(b), Usage: sortieToolUsage},
		closingResponse(b),
	}
	if refused {
		script = []Response{{Call: b.Read(file), Usage: firstTurnUsage}, closingResponse(b)}
	}
	settings := b.ToolServerPassthrough
	if settings == nil {
		settings = b.Passthrough
	}
	run, _ := driveTurn(t, b, drive{
		scenario:      ScenarioToolServer,
		server:        Start(t, script),
		workspace:     workspace,
		mcpConfigPath: tools.ConfigPath,
		settings:      settings,
		prompt:        "Read the file " + file + " and report its contents.",
		remote:        &remoteLaunch{variant: b.SSH, refused: refused},
	})

	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("delivery", func(t *testing.T) {
		reportViolations(t, sshDeliveryViolations(run, refused))
	})
	if refused {
		return
	}
	t.Run("exact usage", func(t *testing.T) {
		AssertExactUsage(t, b, run)
	})
	t.Run("tool server", func(t *testing.T) {
		reportViolations(t, toolServerViolations(run, strconv.Itoa(sortieStatusTurn)))
		tools.Relay.AssertToolCallSucceeded(t, agenttest.SortieStatusTool, tools.Identity)
		tools.Relay.AssertAcceptNamesBothMedia(t, b.Kind)
	})
}

type remoteLaunch struct {
	variant *SSHVariant
	refused bool
}

// startLab seeds the lab from workspace and home, so it must run after the
// binding's Launch prepared that home. The workspace is seeded rather than
// created empty so a read of the scripted file succeeds before the runtime
// validates its arguments.
func (r remoteLaunch) startLab(t *testing.T, workspace, home string) agenttest.SSHLab {
	t.Helper()

	return agenttest.StartSSHLab(t, agenttest.SSHLabParams{
		Install: labInstall(t, r.variant),
		Seed:    []string{workspace, home},
	})
}

// attach carries NO_PROXY to the remote run: it keeps the loopback model
// endpoint off a proxy the runtime honors (claude-code sets HTTPS_PROXY for its
// connection warm-up).
func (r remoteLaunch) attach(b Binding, lab agenttest.SSHLab, env Environment, launch *Launch) (host string, envNames []string) {
	if r.variant.RemoteCommand != nil {
		launch.Config.Command = r.variant.RemoteCommand(env)
		launch.Config.CommandArgv = nil
	}
	if launch.Config.Command != "" {
		launch.Config.Command = "git init -q && " + launch.Config.Command
	}

	meta, _ := registry.Agents.Meta(b.Kind)
	names := slices.Concat(slices.Collect(maps.Keys(launch.Env)), meta.CredentialEnv.Names(), b.CredentialEnv, []string{"NO_PROXY", "no_proxy"})
	slices.Sort(names)
	names = slices.Compact(names)
	if r.refused {
		return lab.RefusedHost, names
	}
	return lab.Host, names
}

func labInstall(t *testing.T, v *SSHVariant) string {
	t.Helper()

	if v.Install != "" {
		return v.Install
	}
	if v.Package == "" || strings.TrimSpace(v.HostCommand) == "" {
		t.Fatal("SSHVariant names neither Install nor both Package and HostCommand")
	}
	target, agentErr := agentcore.ResolveLaunchTarget(domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: v.HostCommand},
	}, "")
	if agentErr != nil {
		t.Fatalf("resolve %q: %v", v.HostCommand, agentErr)
	}
	reported, agentErr := agentcore.QueryRuntimeVersion(t.Context(), target, domain.AgentConfig{}, nil, nil)
	if agentErr != nil {
		t.Fatalf("%s --version: %v", v.HostCommand, agentErr)
	}
	if reported.Version == "" {
		t.Fatalf("%s --version printed no version: %q", v.HostCommand, reported.Line)
	}
	return "npm install -g " + v.Package + "@" + reported.Version
}

type drive struct {
	scenario      Scenario
	server        *Server
	workspace     string
	mcpConfigPath string
	settings      map[string]any
	prompt        string
	remote        *remoteLaunch // nil launches the runtime locally
	sentinel      string        // empty generates one
}

type preparedSession struct {
	environment Environment
	launch      Launch
	events      *eventCollector
	adapter     domain.AgentAdapter
	sshHost     string
	sshEnvNames []string
	lab         agenttest.SSHLab
}

func prepareSession(t *testing.T, b Binding, d drive) preparedSession {
	t.Helper()

	home := t.TempDir()
	sentinel := d.sentinel
	if sentinel == "" {
		sentinel = "sortie-scripted-credential-" + randomHex(t, 16)
	}
	isolateEnvironment(t, b, home, sentinel)

	events := &eventCollector{}
	environment := Environment{
		Scenario:  d.scenario,
		URL:       d.server.URL(),
		Home:      home,
		Workspace: d.workspace,
		Sentinel:  sentinel,
		Events:    events.snapshot,
		Remote:    d.remote != nil,
	}
	launch := b.Launch(t, environment)
	var sshHost string
	var sshEnvNames []string
	var lab agenttest.SSHLab
	if d.remote != nil {
		lab = d.remote.startLab(t, d.workspace, home)
		sshHost, sshEnvNames = d.remote.attach(b, lab, environment, &launch)
	}
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
	return preparedSession{environment: environment, launch: launch, events: events, adapter: adapter, sshHost: sshHost, sshEnvNames: sshEnvNames, lab: lab}
}

func driveTurn(t *testing.T, b Binding, d drive) (run Run, timedOut bool) {
	t.Helper()

	p := prepareSession(t, b, d)
	adapter, events := p.adapter, p.events

	ctx, cancel := context.WithTimeout(context.Background(), turnBound)
	defer cancel()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: d.workspace,
		AgentConfig:   p.launch.Config,
		MCPConfigPath: d.mcpConfigPath,
		Settings:      d.settings,
		SSHHost:       p.sshHost,
		SSHEnvNames:   p.sshEnvNames,
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
		Environment: p.environment,
		Session:     session,
		Result:      result,
		Err:         runErr,
		Events:      events.snapshot(),
		Exchanges:   d.server.Exchanges(),
		Settings:    d.settings,
		Streams:     p.launch.Streams,
	}
	return run, timedOut
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
	run, _ := driveTurn(t, b, drive{
		scenario:  ScenarioPermissionRefusal,
		server:    Start(scriptReporter{t}, script),
		workspace: workspace,
		settings:  settings,
		prompt:    "Read the file " + guarded + " and report its contents.",
	})

	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, refusalOutcomeViolations(run, len(script)))
	})
	t.Run("refused call", func(t *testing.T) {
		reportViolations(t, refusedCallViolations(run, nonce, r.Notice))
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

type turnOutcome struct {
	result domain.TurnResult
	err    error
}

// runInterruption interrupts a turn the endpoint holds, so the interrupt
// lands on a runtime provably waiting on the model rather than on one a fixed
// delay happens to catch mid-turn.
func runInterruption(t *testing.T, b Binding, scenario Scenario, remote *remoteLaunch) {
	t.Helper()

	if remote != nil {
		skipUnlessSSHVariant(t, b)
	}
	if scenario == ScenarioKill && !livenessChecked {
		t.Skip("this platform has no process liveness probe, so the kill has no process group to signal")
	}

	workspace := newWorkspace(t)
	script, prompt := []Response{{Hold: true}}, "Reply with the word ready."
	if scenario == ScenarioCancellationAfterOutput || scenario == ScenarioKill {
		_, file := writeScriptedFile(t, workspace)
		script = []Response{{Call: b.Read(file), Usage: firstTurnUsage}, {Hold: true}}
		prompt = "Read the file " + file + " and report its contents."
	}
	server := Start(t, script)
	d := drive{
		scenario:  scenario,
		server:    server,
		workspace: workspace,
		settings:  b.Passthrough,
		prompt:    prompt,
		remote:    remote,
	}
	p := prepareSession(t, b, d)
	preexisting := agenttest.LiveDescendants(os.Getpid())

	startCtx, startCancel := context.WithTimeout(context.Background(), turnBound)
	defer startCancel()
	session, err := p.adapter.StartSession(startCtx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   p.launch.Config,
		Settings:      d.settings,
		SSHHost:       p.sshHost,
		SSHEnvNames:   p.sshEnvNames,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v%s", err, exchangeLines(server.Exchanges()))
	}

	turnCtx, cancelTurn := context.WithTimeout(context.Background(), turnBound)
	defer cancelTurn()
	outcome := make(chan turnOutcome, 1)
	go func() {
		result, runErr := p.adapter.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt:  d.prompt,
			OnEvent: p.events.collect,
		})
		outcome <- turnOutcome{result: result, err: runErr}
	}()
	stopSession := func() error {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBound)
		defer stopCancel()
		return p.adapter.StopSession(stopCtx, session)
	}

	var (
		in    interruption
		td    teardown
		turn  turnOutcome
		ended bool
	)
	holdCtx, holdCancel := context.WithTimeout(context.Background(), turnBound)
	defer holdCancel()
	select {
	case <-server.holding():
		in.held = true
		td.aliveWhileHeld = slices.DeleteFunc(agenttest.LiveDescendants(os.Getpid()), func(pid int) bool { return slices.Contains(preexisting, pid) })
		td.heldLaunches = heldLaunches(td.aliveWhileHeld)
	case turn = <-outcome:
		ended = true
	case <-holdCtx.Done():
	}
	if !ended {
		select {
		case turn = <-outcome:
			ended = true
		default:
		}
	}

	stopIssued := false
	stopped := make(chan error, 1)
	if ended {
		in.returned = true
	} else {
		in.interrupted = true
		switch scenario {
		case ScenarioStop:
			stopIssued = true
			go func() { stopped <- stopSession() }()
		case ScenarioKill:
			if err := killRuntime(waittest.Context(t), p, remote, td); err != nil {
				t.Fatalf("kill the runtime: %v", err)
			}
		default:
			cancelTurn()
		}
		turn, in.returned = waittest.ReceiveWithin(outcome, interruptBound)
		if !in.returned {
			server.release()
			cancelTurn()
			turn, _ = waittest.ReceiveWithin(outcome, interruptBound)
		}
	}

	if stopIssued {
		in.stopErr, in.stopped = waittest.ReceiveWithin(stopped, stopBound)
	} else {
		stopErr := stopSession()
		if stopErr != nil {
			t.Logf("StopSession() error = %v", stopErr)
		}
		if scenario == ScenarioStop {
			in.stopped, in.stopErr = true, stopErr
		}
	}

	deadline := time.Now().Add(holdEndBound)
	td.stillHeld = server.awaitHolds(holdEndBound)
	if livenessChecked {
		awaitGone(td.aliveWhileHeld, remainingRuntime, td.heldLaunches, deadline)
	}
	if remote != nil {
		td.remoteLeft = awaitRemoteGone(waittest.Context(t), p.lab, deadline)
	}

	run := Run{
		Environment: p.environment,
		Session:     session,
		Result:      turn.result,
		Err:         turn.err,
		Events:      p.events.snapshot(),
		Exchanges:   server.Exchanges(),
		Settings:    d.settings,
		Streams:     p.launch.Streams,
	}
	t.Run("interrupted turn", func(t *testing.T) {
		reportViolations(t, interruptedTurnViolations(run, in))
	})
	if b.Spend != nil && (scenario == ScenarioCancellationAfterOutput || scenario == ScenarioKill) {
		t.Run("exact usage", func(t *testing.T) {
			if b.Spend.RecordsAtTurnEnd && !run.Result.UsageMeasured && (scenario == ScenarioKill || b.Spend.CancelEndsProcess) {
				reportViolations(t, killedBeforeRecordsViolations(run.Result))
				return
			}
			AssertExactUsage(t, b, run)
		})
	}
	t.Run("runtime gone", func(t *testing.T) {
		if !livenessChecked {
			t.Skip("this platform has no process liveness probe")
		}
		reportViolations(t, runtimeGoneViolations(run, remainingRuntime, td))
	})
	if b.Inspect != nil {
		t.Run("adapter", func(t *testing.T) { b.Inspect(t, run) })
	}
}

// killRuntime sends SIGKILL to the runtime without going through the adapter,
// so the adapter sees a crash rather than a stop it requested.
func killRuntime(ctx context.Context, p preparedSession, remote *remoteLaunch, td teardown) error {
	if remote != nil {
		return p.lab.Kill(ctx)
	}
	return killHeldGroups(td.aliveWhileHeld)
}

type sessionRole string

const (
	roleOrigin  sessionRole = "origin"
	roleControl sessionRole = "control"
	roleResumed sessionRole = "resumed"
)

type continuedTurn struct {
	role      sessionRole
	prompt    string // the marker the turn's prompt carries
	answer    string // the marker the turn's scripted answer carries
	result    domain.TurnResult
	events    []domain.AgentEvent
	exchanges []Exchange // everything the endpoint served to the turn's session until the turn returned
}

// The script entries of the continuation scenario that its checks single out.
// The run order is the origin's two turns, the control's, then the resumed
// session's.
const (
	continuedEntry = 1
	controlEntry   = 2
	resumedEntry   = 3
)

// continuation holds what every session of the continuation scenario shares.
// A runtime keys the sessions it stores by home and working directory, and the
// protocol adapter remembers the sessions its instance created, so a resumed
// session needs the origin's adapter, home and workspace.
type continuation struct {
	binding   Binding
	server    *Server
	adapter   domain.AgentAdapter
	workspace string
	home      string
	sentinel  string
	remote    *remoteLaunch // nil launches every session locally
	lab       *agenttest.SSHLab
}

// continuationUsage gives each answer of the continuation scenario a figure no
// other answer carries, so a turn that counts the wrong answer changes its
// total. Their basis (Prompt plus Candidates) must stay above auxiliaryUsage's.
var continuationUsage = [...]Usage{
	{Prompt: 331, Candidates: 19, Thoughts: 4, CachedContent: 67, CacheWrite: 31},
	{Prompt: 347, Candidates: 23, Thoughts: 9, CachedContent: 71, CacheWrite: 37},
	{Prompt: 373, Candidates: 29, Thoughts: 11, CachedContent: 79, CacheWrite: 43},
	{Prompt: 389, Candidates: 31, Thoughts: 6, CachedContent: 83, CacheWrite: 47},
}

func runContinuation(t *testing.T, b Binding, remote *remoteLaunch) {
	t.Helper()

	if remote != nil {
		skipUnlessSSHVariant(t, b)
	}
	roles := []sessionRole{roleOrigin, roleOrigin, roleControl, roleResumed}
	turns := make([]continuedTurn, len(roles))
	script := make([]Response, len(roles))
	for i, role := range roles {
		turns[i] = continuedTurn{role: role, prompt: "nonce-" + randomHex(t, 8), answer: "nonce-" + randomHex(t, 8)}
		script[i] = answerResponse(b, "Noted "+turns[i].answer+".")
		if b.Spend != nil {
			script[i].Usage = continuationUsage[i]
		}
	}

	c := continuation{
		binding:   b,
		server:    Start(t, script),
		workspace: newWorkspace(t),
		home:      t.TempDir(),
		sentinel:  "sortie-scripted-credential-" + randomHex(t, 16),
		remote:    remote,
	}
	isolateEnvironment(t, b, c.home, c.sentinel)
	newAdapter, err := registry.Agents.Get(b.Kind)
	if err != nil {
		t.Fatalf("registry.Agents.Get(%q) error = %v", b.Kind, err)
	}
	if c.adapter, err = newAdapter(); err != nil {
		t.Fatalf("construct %q adapter error = %v", b.Kind, err)
	}

	_, originEvents := c.drive(t, roleOrigin, "", turns[:controlEntry])
	originID := turns[continuedEntry].result.SessionID
	_, controlEvents := c.drive(t, roleControl, "", turns[controlEntry:resumedEntry])
	resumedFrom := len(c.server.Exchanges())
	resumed, resumedEvents := c.drive(t, roleResumed, originID, turns[resumedEntry:])

	run := Run{
		Environment: Environment{Remote: remote != nil},
		Session:     resumed,
		Result:      turns[resumedEntry].result,
		Events:      slices.Concat(originEvents, controlEvents, resumedEvents),
		Exchanges:   c.server.Exchanges(),
		Settings:    b.Passthrough,
	}
	t.Run("turn outcome", func(t *testing.T) {
		reportViolations(t, turnOutcomeViolations(run, len(script)))
	})
	t.Run("continuation", func(t *testing.T) {
		reportViolations(t, continuationViolations(turns, resumed, run.Exchanges, b.Spend, usageSettlingOf(b)))
	})
	if b.Spend != nil {
		for entry, turn := range turns {
			t.Run(fmt.Sprintf("exact usage %s entry %d", turn.role, entry), func(t *testing.T) {
				assertExactUsage(t, b, Run{
					Environment: run.Environment,
					Result:      turn.result,
					Events:      turn.events,
					Exchanges:   turn.exchanges,
					Settings:    b.Passthrough,
				}, fmt.Sprintf("%s entry %d", turn.role, entry), agenttest.AssertLiveUsageCase)
			})
		}
		return
	}
	t.Run("resumed usage", func(t *testing.T) {
		agenttest.AssertLiveUsageCase(t, b.Kind, agenttest.UsageReportingCase{
			Name:   "resumed turn",
			Events: turns[resumedEntry].events,
			Result: turns[resumedEntry].result,
		})
		resumedResult := turns[resumedEntry].result
		if !resumedResult.UsageMeasured {
			return
		}
		if want := served(run.Exchanges[resumedFrom:], turnAndAuxiliary, b); resumedResult.Usage != want {
			t.Errorf("resumed turn Result.Usage = %+v, want %+v, the sum of every answer served to the resumed session", resumedResult.Usage, want)
		}
	})
}

// drive records each turn's result, events and served exchanges in turns.
func (c *continuation) drive(t *testing.T, role sessionRole, resumeID string, turns []continuedTurn) (domain.Session, []domain.AgentEvent) {
	t.Helper()

	events := &eventCollector{}
	environment := Environment{
		Scenario:  ScenarioContinuation,
		URL:       c.server.URL(),
		Home:      c.home,
		Workspace: c.workspace,
		Sentinel:  c.sentinel,
		Events:    events.snapshot,
		Remote:    c.remote != nil,
	}
	launch := c.binding.Launch(t, environment)
	var sshHost string
	var sshEnvNames []string
	if c.remote != nil {
		if c.lab == nil {
			lab := c.remote.startLab(t, c.workspace, c.home)
			c.lab = &lab
		}
		sshHost, sshEnvNames = c.remote.attach(c.binding, *c.lab, environment, &launch)
	}
	for _, key := range slices.Sorted(maps.Keys(launch.Env)) {
		t.Setenv(key, launch.Env[key])
	}

	// A persistent runtime exits when this context ends, so the deferred stop
	// below must run before the cancel.
	ctx, cancel := context.WithTimeout(context.Background(), turnBound)
	defer cancel()
	sessionStart := len(c.server.Exchanges())
	session, err := c.adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   c.workspace,
		AgentConfig:     launch.Config,
		Settings:        c.binding.Passthrough,
		ResumeSessionID: resumeID,
		SSHHost:         sshHost,
		SSHEnvNames:     sshEnvNames,
	})
	if err != nil {
		t.Fatalf("%s session: StartSession() error = %v%s", role, err, exchangeLines(c.server.Exchanges()))
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBound)
		defer stopCancel()
		if err := c.adapter.StopSession(stopCtx, session); err != nil {
			t.Logf("%s session: StopSession() error = %v", role, err)
		}
	}()

	for i := range turns {
		before := len(events.snapshot())
		// The prompt opens with a letter: a runtime may read a leading slash as a
		// command, and one may leave a message opening with a slash or a question
		// mark out of a loaded history.
		result, runErr := c.adapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt:  "Note the code " + turns[i].prompt + " and reply with a short acknowledgment.",
			OnEvent: events.collect,
		})
		turns[i].result = result
		turns[i].events = events.snapshot()[before:]
		turns[i].exchanges = c.server.Exchanges()[sessionStart:]
		if runErr != nil || result.ExitReason != domain.EventTurnCompleted {
			t.Fatalf("%s session turn %d: RunTurn() error = %v, ExitReason = %q, want nil and %q%s",
				role, i+1, runErr, result.ExitReason, domain.EventTurnCompleted, exchangeLines(c.server.Exchanges()))
		}
	}
	return session, events.snapshot()
}

type instructionSeeds struct {
	project string
	user    string
}

// scriptReporter drops unused-entry failures: a request without tools is
// answered outside the script, so how many entries the verification session
// consumes depends on its declaration, and a runtime may end a refused turn
// without requesting the closing entry.
type scriptReporter struct {
	testing.TB
}

func (r scriptReporter) Errorf(format string, args ...any) {
	if strings.HasSuffix(fmt.Sprintf(format, args...), " was never requested") {
		return
	}
	r.TB.Errorf(format, args...)
}

func runVerificationContext(t *testing.T, b Binding) {
	t.Helper()

	declaration := b.VerificationContext
	if !declaration.Declared() {
		meta, _ := registry.Agents.Meta(b.Kind)
		declaration = meta.VerificationContext
	}
	if !declaration.Declared() {
		t.Fatalf("kind %q declares no verification context, want AgentMeta.VerificationContext or Binding.VerificationContext", b.Kind)
	}
	if !declaration.Keeps(registry.VerificationKeepsUserInstructions) && b.UserInstructions == "" {
		t.Fatalf("Binding.UserInstructions is empty, want the runtime's user-level instruction file, since its verification request drops user instructions")
	}

	seeds := instructionSeeds{
		project: "sortie-project-instructions-" + randomHex(t, 8),
		user:    "sortie-user-instructions-" + randomHex(t, 8),
	}
	workspace := newWorkspace(t)
	home := t.TempDir()
	writeSeed(t, filepath.Join(workspace, "AGENTS.md"), seeds.project)
	if b.ProjectInstructions != "" {
		writeSeed(t, filepath.Join(workspace, b.ProjectInstructions), seeds.project)
	}
	if b.UserInstructions != "" {
		writeSeed(t, filepath.Join(home, b.UserInstructions), seeds.user)
	}

	// A scripted tool call would fail a runtime that declares no tool by that
	// name, and the verification session declares fewer tools. A runtime that
	// ends a turn only on a tool call keeps requesting after a plain answer, so
	// the script outlasts its continuation ceiling.
	answers := slices.Repeat([]Response{{Text: "SORTIE_CREDENTIAL_OK", Usage: secondTurnUsage}}, verificationScriptLength)
	c := verificationSessions{
		binding:   b,
		server:    Start(scriptReporter{t}, answers),
		adapter:   newKindAdapter(t, b.Kind),
		workspace: workspace,
		home:      home,
		sentinel:  "sortie-scripted-credential-" + randomHex(t, 16),
	}
	isolateEnvironment(t, b, home, c.sentinel)

	verificationExchanges := c.run(t, true)
	working := exchangesOfKind(c.run(t, false), ExchangeTurn)
	if len(working) == 0 {
		t.Fatalf("the working session sent no model request declaring tools%s", exchangeLines(c.server.Exchanges()))
	}
	verification := modelRequests(verificationExchanges, working[0].Model)
	if len(verification) == 0 {
		t.Fatalf("no verification request reached the working session's model %q: the verification session sent no request carrying %q to it%s",
			working[0].Model, agentcore.CredentialVerificationPrompt, exchangeLines(c.server.Exchanges()))
	}
	reportViolations(t, verificationContextViolations(declaration, seeds, verification, working[0]))
}

func writeSeed(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newKindAdapter(t *testing.T, kind string) domain.AgentAdapter {
	t.Helper()

	newAdapter, err := registry.Agents.Get(kind)
	if err != nil {
		t.Fatalf("registry.Agents.Get(%q) error = %v", kind, err)
	}
	adapter, err := newAdapter()
	if err != nil {
		t.Fatalf("construct %q adapter error = %v", kind, err)
	}
	return adapter
}

// modelRequests keeps auxiliary exchanges too, because the request of a
// tool-free verification session is answered outside the script. A prompt sent
// to another model is the runtime's own routing, not the verification request.
func modelRequests(exchanges []Exchange, workingModel string) []Exchange {
	return slices.DeleteFunc(slices.Clone(exchanges), func(ex Exchange) bool {
		return (ex.Kind != ExchangeTurn && ex.Kind != ExchangeAuxiliary) ||
			ex.Model != workingModel ||
			!strings.Contains(string(ex.Body), agentcore.CredentialVerificationPrompt)
	})
}

// verificationSessions holds what the two sessions share, because a runtime
// keys the context it loads by home and workspace.
type verificationSessions struct {
	binding   Binding
	server    *Server
	adapter   domain.AgentAdapter
	workspace string
	home      string
	sentinel  string
}

func (c verificationSessions) run(t *testing.T, credentialVerification bool) []Exchange {
	t.Helper()

	before := len(c.server.Exchanges())
	c.drive(t, credentialVerification)
	return c.server.Exchanges()[before:]
}

func (c verificationSessions) drive(t *testing.T, credentialVerification bool) {
	t.Helper()

	events := &eventCollector{}
	launch := c.binding.Launch(t, Environment{
		Scenario:  ScenarioVerificationContext,
		URL:       c.server.URL(),
		Home:      c.home,
		Workspace: c.workspace,
		Sentinel:  c.sentinel,
		Events:    events.snapshot,
	})
	for _, key := range slices.Sorted(maps.Keys(launch.Env)) {
		t.Setenv(key, launch.Env[key])
	}

	// A persistent runtime exits when this context ends, so the deferred stop
	// below must run before the cancel.
	ctx, cancel := context.WithTimeout(context.Background(), turnBound)
	defer cancel()
	session, err := c.adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:          c.workspace,
		AgentConfig:            launch.Config,
		Settings:               c.binding.Passthrough,
		CredentialVerification: credentialVerification,
	})
	if err != nil {
		t.Fatalf("StartSession(CredentialVerification = %t) error = %v%s", credentialVerification, err, exchangeLines(c.server.Exchanges()))
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), stopBound)
		defer stopCancel()
		if err := c.adapter.StopSession(stopCtx, session); err != nil {
			t.Logf("StopSession() error = %v", err)
		}
	}()

	result, runErr := c.adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  agentcore.CredentialVerificationPrompt,
		OnEvent: events.collect,
	})
	if !endedAfterModelAnswer(result, runErr) {
		t.Fatalf("RunTurn(CredentialVerification = %t) error = %v, ExitReason = %q, want nil and %q%s",
			credentialVerification, runErr, result.ExitReason, domain.EventTurnCompleted, exchangeLines(c.server.Exchanges()))
	}
}

// endedAfterModelAnswer accepts an incomplete turn too: it still proves the
// request reached the model, which is how the credential check reads it.
func endedAfterModelAnswer(result domain.TurnResult, runErr error) bool {
	if runErr == nil {
		return result.ExitReason == domain.EventTurnCompleted
	}
	agentErr, ok := errors.AsType[*domain.AgentError](runErr)
	return ok && agentErr.Kind == domain.ErrTurnIncomplete
}

// verificationContextViolations uses the working request as the control: a seed
// the runtime never reads must fail, not pass the absence checks.
func verificationContextViolations(declaration registry.VerificationContext, seeds instructionSeeds, verification []Exchange, working Exchange) []string {
	v := &findings{property: "verification context"}

	for _, ex := range verification {
		label := requestLabel(ex)
		if ex.Model != working.Model {
			v.add("%s names model %q, want the working session's %q", label, ex.Model, working.Model)
		}
		if !slices.Equal(ex.Credentials, working.Credentials) {
			v.add("%s carries credentials %q, want the working session's %q", label, ex.Credentials, working.Credentials)
		}
		switch {
		case declaration.Keeps(registry.VerificationKeepsSomeTools):
			if len(ex.Tools) >= len(working.Tools) {
				v.add("%s declares %d tools, want fewer than the working request's %d", label, len(ex.Tools), len(working.Tools))
			}
		case declaration.Keeps(registry.VerificationKeepsAllTools):
			if len(ex.Tools) > len(working.Tools) {
				v.add("%s declares %d tools, want at most the working request's %d", label, len(ex.Tools), len(working.Tools))
			}
		case len(ex.Tools) != 0:
			v.add("%s declares %d tools, want none", label, len(ex.Tools))
		}
		if !declaration.Keeps(registry.VerificationKeepsProjectInstructions) && strings.Contains(string(ex.Body), seeds.project) {
			v.add("%s carries the project instruction file, want it dropped", label)
		}
		if !declaration.Keeps(registry.VerificationKeepsUserInstructions) && strings.Contains(string(ex.Body), seeds.user) {
			v.add("%s carries the user instruction file, want it dropped", label)
		}
	}

	if !declaration.Keeps(registry.VerificationKeepsProjectInstructions) && !strings.Contains(string(working.Body), seeds.project) {
		v.add("the working %s does not carry the project instruction file, so its absence from the verification requests proves nothing", requestLabel(working))
	}
	if !declaration.Keeps(registry.VerificationKeepsUserInstructions) && !strings.Contains(string(working.Body), seeds.user) {
		v.add("the working %s does not carry the user instruction file, so its absence from the verification requests proves nothing", requestLabel(working))
	}
	if !declaration.Keeps(registry.VerificationKeepsSystemPrompt) && !carriesText(verification[0].Body, agentcore.VerificationSystemPrompt) {
		v.add("%s does not carry the verification system prompt %q", requestLabel(verification[0]), agentcore.VerificationSystemPrompt)
	}
	return v.items
}

func carriesText(body []byte, text string) bool {
	escaped, err := json.Marshal(text)
	return bytes.Contains(body, []byte(text)) || (err == nil && bytes.Contains(body, escaped[1:len(escaped)-1]))
}

// awaitGone's deadline is shared with awaitHolds, which may have spent all
// of it; the check still runs once then.
func awaitGone(pids []int, remaining func(pid int) string, launches []func() (bool, error), deadline time.Time) {
	_ = waittest.UntilWithin(max(time.Until(deadline), time.Nanosecond), livenessPoll, func() error {
		if i := slices.IndexFunc(pids, func(pid int) bool { return remaining(pid) != "" }); i >= 0 {
			return fmt.Errorf("process %d is still running", pids[i])
		}
		for _, released := range launches {
			if done, _ := released(); !done {
				return launchtest.ErrNotReleased
			}
		}
		return nil
	})
}

func awaitRemoteGone(ctx context.Context, lab agenttest.SSHLab, deadline time.Time) []string {
	var left []string
	_ = waittest.UntilWithin(max(time.Until(deadline), time.Nanosecond), livenessPoll, func() error {
		procs, err := lab.Processes(ctx)
		left = procs
		if err != nil {
			left = []string{err.Error()}
		}
		if len(left) > 0 {
			return errors.New("the SSH host still runs the launch")
		}
		return nil
	})
	return left
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
	return answerResponse(b, "The file holds the nonce.")
}

func answerResponse(b Binding, text string) Response {
	if b.Finish != nil {
		return Response{Call: b.Finish(text), Usage: secondTurnUsage}
	}
	return Response{Text: text, Usage: secondTurnUsage}
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

var turnAndAuxiliary = []ExchangeKind{ExchangeTurn, ExchangeAuxiliary}

// served sums the usage the endpoint reported in the exchanges of the given
// kinds, as the binding's runtime reports it: a runtime that reports no cache
// write keeps those tokens in its input count and carries zero writes.
func served(exchanges []Exchange, kinds []ExchangeKind, b Binding) domain.TokenUsage {
	var total domain.TokenUsage
	for _, ex := range exchanges {
		if slices.Contains(kinds, ex.Kind) {
			total = agentcore.AddUsage(total, reportedUsage(ex, b))
		}
	}
	return total
}

// servedByModel sums the exchanges served does per model, under the name the
// binding's runtime reports for it.
func servedByModel(exchanges []Exchange, kinds []ExchangeKind, b Binding) map[string]domain.TokenUsage {
	byModel := make(map[string]domain.TokenUsage)
	for _, ex := range exchanges {
		if slices.Contains(kinds, ex.Kind) {
			name := b.ReportedModelPrefix + ex.Model
			byModel[name] = agentcore.AddUsage(byModel[name], reportedUsage(ex, b))
		}
	}
	return byModel
}

func reportedUsage(ex Exchange, b Binding) domain.TokenUsage {
	u := ex.Answer.Usage
	usage := domain.TokenUsage{
		InputTokens:      u.Prompt,
		OutputTokens:     u.Candidates + u.Thoughts,
		CacheReadTokens:  u.CachedContent,
		CacheWriteTokens: u.CacheWrite,
	}
	if b.CacheWriteUnreported {
		usage.CacheWriteTokens = 0
	}
	return usage
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

func sshDeliveryViolations(run Run, refused bool) []string {
	v := &findings{property: "ssh tool delivery"}

	var notices []string
	for _, event := range run.Events {
		if event.Type == domain.EventNotification && strings.Contains(event.Message, toolrelay.GapNoticeStem) {
			notices = append(notices, event.Message)
		}
	}
	if refused {
		switch {
		case len(notices) != 1:
			v.add("%d opening notices, want one", len(notices))
		case !strings.Contains(notices[0], toolrelay.ToolServersLabel):
			v.add("opening notice %q does not name %q", notices[0], toolrelay.ToolServersLabel)
		}
		if !run.Session.NoToolChannel {
			v.add("Session.NoToolChannel = false after a refused forward, want true")
		}
		return v.items
	}

	for _, notice := range notices {
		if strings.Contains(notice, toolrelay.ToolServersLabel) {
			v.add("opening notice %q after a delivered forward, want none", notice)
		}
	}
	if run.Session.NoToolChannel {
		v.add("Session.NoToolChannel = true after a delivered forward, want false")
	}
	return v.items
}

// refusalOutcomeViolations holds a refused turn to the script's entries, except
// a one-exchange turn: a runtime may end on the refusal instead of reporting it
// to the model.
func refusalOutcomeViolations(run Run, entries int) []string {
	if len(exchangesOfKind(run.Exchanges, ExchangeTurn)) == 1 {
		entries = 1
	}
	return turnOutcomeViolations(run, entries)
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
		if event.ToolName == "unknown" {
			v.add("event %d has tool name %q, want the runtime's name for the tool or none", i, event.ToolName)
		}
		if event.ToolDurationMS < 0 {
			v.add("event %d has tool duration %d ms, want at least 0", i, event.ToolDurationMS)
		}
	}
	if toolEvents != called {
		v.add("%d tool_result events, want one per call answered (%d)%s", toolEvents, called, toolTrail(run, turns))
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
		v.add("%d tool_result events, want one per call answered (%d)%s", len(toolEvents), called, toolTrail(run, turns))
	}
	if len(toolEvents) > serverCallStep {
		i := toolEvents[serverCallStep]
		event := run.Events[i]
		if event.ToolError {
			v.add("event %d reports a tool error for %q", i, event.ToolName)
		}
		if event.ToolName == "unknown" {
			v.add("event %d has tool name %q, want the runtime's name for the tool or none", i, event.ToolName)
		}
	}
	return v.items
}

// toolTrail lists each answered call with the result the runtime sent back
// for it, then every event that is neither usage nor an empty notification,
// so a missing tool_result names the call that lacks one.
func toolTrail(run Run, turns []Exchange) string {
	var b strings.Builder
	for i, ex := range turns {
		if ex.Answer.Call == nil {
			continue
		}
		result := "no later request"
		if i+1 < len(turns) {
			result = "no result in the next request"
			for _, r := range turns[i+1].ToolResults {
				if answersCall(ex.Answer, r) {
					result = redact.Truncate(strings.ReplaceAll(r.Output, run.Environment.Sentinel, "[redacted]"), 300)
				}
			}
		}
		fmt.Fprintf(&b, "\n\tcall %q (%s): %q", ex.Answer.Call.Name, requestLabel(ex), result)
	}
	for i, event := range run.Events {
		if event.Type == domain.EventTokenUsage || (event.Type == domain.EventNotification && event.Message == "") {
			continue
		}
		fmt.Fprintf(&b, "\n\tevent %d %s tool=%q error=%t message=%q", i, event.Type, event.ToolName, event.ToolError, redact.Truncate(strings.ReplaceAll(event.Message, run.Environment.Sentinel, "[redacted]"), 120))
	}
	return b.String()
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

// AssertExactUsage fails t unless the run's usage figure equals, per class and
// exactly, the usage the endpoint served in its turn and auxiliary answers, and
// its per-model split matches the usage served per model, under the usage the
// kind's registered sources derive for the run's host. A run whose kind can
// carry no token figure on that host must produce none and flag its spend
// unaccounted. For a binding with Spend, the turn's provenance and
// completeness must also match the source that settled it.
func AssertExactUsage(t *testing.T, b Binding, run Run) {
	t.Helper()

	assertExactUsage(t, b, run, "scripted turn", agenttest.AssertUsageReportingCase)
}

// assertExactUsage is AssertExactUsage with the per-case reporting check
// injected: a text-only turn carries neither a tool result nor a second usage
// event, which AssertUsageReportingCase requires to tell the arrivals apart.
// It is the one place that reads the kind's sources for a run.
func assertExactUsage(t *testing.T, b Binding, run Run, name string, reporting func(*testing.T, string, agenttest.UsageReportingCase)) {
	t.Helper()

	meta, _ := registry.Agents.Meta(b.Kind)
	arrival, _ := meta.UsageDisposition()
	settling := usageSettlingOf(b)
	reportViolations(t, exactUsage(run, arrival,
		served(run.Exchanges, turnAndAuxiliary, b), servedByModel(run.Exchanges, turnAndAuxiliary, b), b.Spend, settling))
	agenttest.AssertUsageContract(t, run.Events)
	if !run.Result.UsageMeasured && (settling.recordsUnread || settling.liveLowerBound) {
		return
	}
	reporting(t, b.Kind, agenttest.UsageReportingCase{
		Name:   name,
		Events: run.Events,
		Result: run.Result,
	})
}

// usageSettlingOf derives from the kind's registered sources what a run of b
// leaves unsettled.
func usageSettlingOf(b Binding) usageSettling {
	meta, _ := registry.Agents.Meta(b.Kind)
	_, attribution := meta.UsageDisposition()
	return usageSettling{
		recordsUnread:  b.Spend != nil && b.Spend.Records == (domain.UsageProvenance{}),
		liveLowerBound: b.Spend == nil && meta.UsageSources.Live.Spend.LowerBound,
		splitExpected:  attribution == registry.UsageAttributionPerModel,
	}
}

// usageSettling is what a run's registered sources leave unsettled.
type usageSettling struct {
	recordsUnread  bool // no records source is read for the run, so an interrupted turn keeps its live figure alone
	liveLowerBound bool // the kind's live source declares a lower bound and the binding states no provenance
	splitExpected  bool // the kind's sources name the served model, so the split is expected even when the last usage event names none
}

// exactUsage returns the violations of the usage property: a measured figure
// equal to want, carried by the last usage event, arriving as the disposition
// declares, and split per model as wantByModel wherever the kind's sources name
// the served model or the last usage event names a model or carries a split. A
// disposition that reports no usage has none to check here. A non-nil spend adds
// the provenance and completeness the turn's exit reason expects. A figure that
// settling leaves a lower bound exceeds want in no class and stands in for the
// exact figure.
func exactUsage(run Run, arrival registry.UsageArrival, want domain.TokenUsage, wantByModel map[string]domain.TokenUsage, spend *SpendSources, settling usageSettling) []string {
	v := &findings{property: "exact usage"}
	if arrival == registry.UsageArrivalNone {
		return v.items
	}

	interrupted := run.Result.ExitReason == domain.EventTurnCancelled || run.Result.ExitReason == domain.EventTurnFailed
	switch {
	case spend != nil && interrupted && settling.recordsUnread:
		liveOnlyInterruptedViolations(v, run.Result, want, *spend)
		return v.items
	case settling.liveLowerBound && (!run.Result.UsageMeasured || run.Result.SpendUnaccounted):
		lowerBoundViolations(v, run.Result, want)
		return v.items
	}

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

	last, found := lastUsageEvent(run.Events)
	if found && last.Usage != run.Result.Usage {
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
	if found && (settling.splitExpected || last.Model != "" || len(last.UsageByModel) > 0) {
		split, exceedsTotals := effectiveSplit(last, reportedModel(run.Events))
		if exceedsTotals {
			v.add("the last event carrying usage splits %+v per model, more than its total %+v", last.UsageByModel, last.Usage)
		}
		if !maps.Equal(split, wantByModel) {
			v.add("the last event carrying usage splits %+v per model, want %+v, the usage served per model", split, wantByModel)
		}
	}
	if spend != nil {
		spendProvenanceViolations(v, run.Result, *spend)
	}
	return v.items
}

// liveOnlyInterruptedViolations adds the violations of a turn interrupted after
// output when no records are read for it: its spend is unaccounted, and it
// carries either the live source's figure, never above what was served, or no
// figure and no provenance.
func liveOnlyInterruptedViolations(v *findings, result domain.TurnResult, served domain.TokenUsage, spend SpendSources) {
	if !result.SpendUnaccounted {
		v.add("Result.SpendUnaccounted is false, want true for a %s turn no records settle", result.ExitReason)
	}
	if !result.UsageMeasured {
		if len(result.UsageProvenance) != 0 {
			v.add("Result.UsageProvenance = %+v, want none for a %s turn with no figure", result.UsageProvenance, result.ExitReason)
		}
		return
	}
	if want := provenanceList(spend.Live); !slices.Equal(result.UsageProvenance, want) {
		v.add("Result.UsageProvenance = %+v, want %+v, the live source, for a %s turn no records settle", result.UsageProvenance, want, result.ExitReason)
	}
	if exceedsInAnyClass(result.Usage, served) {
		v.add("Result.Usage = %+v exceeds %+v, the usage the endpoint served", result.Usage, served)
	}
}

// lowerBoundViolations adds the violations of a turn whose live source
// declares a lower bound: a turn with no figure flags its spend unaccounted,
// and a figure marked a lower bound exceeds what was served in no class.
func lowerBoundViolations(v *findings, result domain.TurnResult, served domain.TokenUsage) {
	if !result.UsageMeasured {
		if !result.SpendUnaccounted {
			v.add("Result.SpendUnaccounted is false, want true for a turn with no figure")
		}
		return
	}
	if exceedsInAnyClass(result.Usage, served) {
		v.add("Result.Usage = %+v, a lower bound, exceeds %+v, the usage the endpoint served", result.Usage, served)
	}
}

// provenanceList is the list a turn settled by p reports; none for the zero
// provenance.
func provenanceList(p domain.UsageProvenance) []domain.UsageProvenance {
	if p == (domain.UsageProvenance{}) {
		return nil
	}
	return []domain.UsageProvenance{p}
}

func exceedsInAnyClass(figure, served domain.TokenUsage) bool {
	return figure.InputTokens > served.InputTokens || figure.OutputTokens > served.OutputTokens ||
		figure.CacheReadTokens > served.CacheReadTokens || figure.CacheWriteTokens > served.CacheWriteTokens
}

// spendProvenanceViolations adds the violations of a turn's settling source and
// completeness: a completed turn settles from the live source, and a turn
// cancelled or failed after output from the records, whose figure still misses
// the request that was held.
func spendProvenanceViolations(v *findings, result domain.TurnResult, spend SpendSources) {
	var want []domain.UsageProvenance
	var wantLowerBound bool
	switch result.ExitReason {
	case domain.EventTurnCompleted:
		want, wantLowerBound = provenanceList(spend.Live), spend.LowerBound
	case domain.EventTurnCancelled, domain.EventTurnFailed:
		want, wantLowerBound = provenanceList(spend.Records), true
	default:
		return
	}
	if !slices.Equal(result.UsageProvenance, want) {
		v.add("Result.UsageProvenance = %+v, want %+v for a %s turn", result.UsageProvenance, want, result.ExitReason)
	}
	if result.SpendUnaccounted != wantLowerBound {
		v.add("Result.SpendUnaccounted = %v, want %v for a %s turn", result.SpendUnaccounted, wantLowerBound, result.ExitReason)
	}
}

// killedBeforeRecordsViolations returns the violations of the honest outcome for
// a turn the interrupt ended while its runtime still held every record of the
// turn's spend, a killed turn or, for a kind whose cancel ends the runtime's
// process, one cancelled after output: spend occurred, its amount is unknown. A
// measured zero claims an amount, so it fails like any measured figure.
//
// Where the interrupt can land after the runtime wrote the turn's records, as
// over SSH, the read then settles the exact figure. Nothing the harness observes
// tells the two apart before the Result does, so a measured Result is held to
// the exact usage of an interrupted turn instead, and only an unmeasured one
// reaches this check.
func killedBeforeRecordsViolations(result domain.TurnResult) []string {
	v := &findings{property: "exact usage"}
	if result.UsageMeasured {
		v.add("Result.UsageMeasured is true with Usage %+v, want false for a turn interrupted before its records were written", result.Usage)
	}
	if !result.SpendUnaccounted {
		v.add("Result.SpendUnaccounted is false, want true for a turn interrupted before its records were written")
	}
	if len(result.UsageProvenance) != 0 {
		v.add("Result.UsageProvenance = %+v, want none for a turn interrupted before its records were written", result.UsageProvenance)
	}
	return v.items
}

// effectiveSplit is the per-model split an event states once its unattributed
// rest is credited to the event's model, or to runModel when the event names
// none, and whether the stated split exceeds the event's total in any class.
func effectiveSplit(event domain.AgentEvent, runModel string) (split map[string]domain.TokenUsage, exceedsTotals bool) {
	model := event.Model
	if model == "" {
		model = runModel
	}
	split = agentcore.NormalizeUsageByModel(event.UsageByModel)
	if len(split) == 0 {
		return map[string]domain.TokenUsage{model: event.Usage}, false
	}
	stated := usage.Sum(split)
	exceedsTotals = event.Usage.InputTokens < stated.InputTokens || event.Usage.OutputTokens < stated.OutputTokens ||
		event.Usage.CacheReadTokens < stated.CacheReadTokens || event.Usage.CacheWriteTokens < stated.CacheWriteTokens
	if rest := agentcore.SubtractUsage(event.Usage, stated); rest != (domain.TokenUsage{}) {
		split[model] = agentcore.AddUsage(split[model], rest)
	}
	return split, exceedsTotals
}

// reportedModel is the model the latest token_usage event naming one reports,
// which the orchestrator credits to an event that names none.
func reportedModel(events []domain.AgentEvent) string {
	for _, event := range slices.Backward(events) {
		if event.Type == domain.EventTokenUsage && event.Model != "" {
			return event.Model
		}
	}
	return ""
}

func lastUsageEvent(events []domain.AgentEvent) (domain.AgentEvent, bool) {
	for _, event := range slices.Backward(events) {
		if event.Usage != (domain.TokenUsage{}) {
			return event, true
		}
	}
	return domain.AgentEvent{}, false
}

// AssertContainment fails t unless the run's credential stayed in the header it
// belongs in: no request body, event or stream file the run's Streams name
// carries the run's sentinel, and every turn request carried it as a credential
// and none carried another.
func AssertContainment(t *testing.T, run Run) {
	t.Helper()

	reportViolations(t, containmentViolations(run, readStreams(run.Streams)))
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

// exhaustionViolations checks a run whose script ran out after one tool call.
// Under a lower-bound settling a turn with no figure, or with its spend
// unaccounted, is held to the lower-bound rule against what the endpoint served
// instead of to an exact figure.
func exhaustionViolations(run Run, b Binding, settling usageSettling, nonce string, timedOut bool, failures []string) []string {
	v := &findings{property: "script exhaustion"}

	if timedOut {
		v.add("RunTurn did not return inside the %s bound", turnBound)
	}
	if b.RuntimeFailureFolded {
		checkFoldedOutcome(v, run)
	} else {
		if run.Err == nil {
			v.add("RunTurn returned nil, want an error")
		}
		if run.Result.ExitReason != domain.EventTurnFailed {
			v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnFailed)
		}
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
		checkExhaustedExchanges(v, run, turns[0], exhausted, nonce, failures, b.RuntimeFailureFolded)
	}

	want := served(run.Exchanges, turnAndAuxiliary, b)
	if settling.liveLowerBound && (!run.Result.UsageMeasured || run.Result.SpendUnaccounted) {
		lowerBoundViolations(v, run.Result, want)
		return v.items
	}
	if !run.Result.UsageMeasured {
		v.add("Result.UsageMeasured is false, want a measured figure")
	}
	if run.Result.Usage != want {
		v.add("Result.Usage = %+v, want %+v, the sum of every answer the endpoint served", run.Result.Usage, want)
	}
	return v.items
}

// checkFoldedOutcome expects the completed turn a folding runtime reports for a
// failed model request; a failed turn means the declaration is stale.
func checkFoldedOutcome(v *findings, run Run) {
	if run.Err != nil || run.Result.ExitReason == domain.EventTurnFailed {
		v.add("the turn was reported as failed (ExitReason = %q, error %v), so the runtime now reports the endpoint failure: drop the outcome_folded_into_another declaration and re-qualify the runtime", run.Result.ExitReason, run.Err)
		return
	}
	if run.Result.ExitReason != domain.EventTurnCompleted {
		v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnCompleted)
	}
}

// checkExhaustedExchanges accepts more than one request beyond the script: a
// runtime may resend a rejected request, such as once more without an
// optional feature.
// A folding runtime reports no endpoint failure, so folded skips only the
// checks that the failure reached the caller.
func checkExhaustedExchanges(v *findings, run Run, call Exchange, exhausted []Exchange, nonce string, failures []string, folded bool) {
	if !folded {
		checkEndpointFailureReported(v, run, exhausted[len(exhausted)-1].Answer.Text)
	}

	for _, ex := range exhausted {
		checkToolResult(v, call, ex, nonce, run.Environment.Sentinel)
	}

	if !oneFailurePerRequest(failures, exhausted) {
		v.add("the endpoint reported %d failures %q, want one naming each of %d exhausted requests", len(failures), failures, len(exhausted))
	}
}

func checkEndpointFailureReported(v *findings, run Run, message string) {
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

func interruptedTurnViolations(run Run, in interruption) []string {
	v := &findings{property: "interrupted turn"}
	scenario := run.Environment.Scenario

	if !in.held {
		v.add("the endpoint held no model request while RunTurn ran, so the interrupt did not land on a turn waiting on the model")
	}
	if in.interrupted && !in.returned {
		v.add("RunTurn did not return within %s of the interrupt", interruptBound)
	}
	if !in.interrupted || in.returned {
		var agentErr *domain.AgentError
		isAgentErr := errors.As(run.Err, &agentErr)
		switch scenario {
		case ScenarioCancellation, ScenarioCancellationAfterOutput:
			if run.Result.ExitReason != domain.EventTurnCancelled {
				v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnCancelled)
			}
			if !isAgentErr || agentErr.Kind != domain.ErrTurnCancelled {
				v.add("RunTurn returned error %v, want a *domain.AgentError of kind %q", run.Err, domain.ErrTurnCancelled)
			}
		case ScenarioKill:
			if run.Result.ExitReason != domain.EventTurnFailed {
				v.add("ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnFailed)
			}
			if !isAgentErr {
				v.add("RunTurn returned error %v, want a *domain.AgentError", run.Err)
			}
		case ScenarioStop:
			if run.Result.ExitReason != domain.EventTurnCancelled && run.Result.ExitReason != domain.EventTurnFailed {
				v.add("ExitReason = %q, want %q or %q", run.Result.ExitReason, domain.EventTurnCancelled, domain.EventTurnFailed)
			}
			if !isAgentErr {
				v.add("RunTurn returned error %v, want a *domain.AgentError", run.Err)
			}
		}
		if want := scenario == ScenarioCancellationAfterOutput || scenario == ScenarioKill; run.Result.SpendUnaccounted != want {
			v.add("Result.SpendUnaccounted = %v, want %v when the interrupt came %s the model served output", run.Result.SpendUnaccounted, want, afterOrBefore(want))
		}
	}
	if scenario == ScenarioStop {
		switch {
		case !in.stopped:
			v.add("StopSession did not return within %s", stopBound)
		case in.stopErr != nil:
			v.add("StopSession returned error %v, want nil", in.stopErr)
		}
	}
	return v.items
}

func runtimeGoneViolations(run Run, remaining func(pid int) string, td teardown) []string {
	v := &findings{property: "runtime gone"}

	reported := reportedProcessIDs(run)
	if len(reported) == 0 {
		v.add("neither Session.AgentPID nor any event's AgentPID names a process")
	}
	var pids []int
	for _, raw := range reported {
		pid, err := strconv.Atoi(raw)
		if err != nil || pid <= 1 {
			v.add("reported process id %q is not an integer above 1, so it was not probed", raw)
			continue
		}
		pids = append(pids, pid)
	}
	for _, pid := range pids {
		if left := remaining(pid); left != "" {
			v.add("%s of reported process %d still answers after StopSession returned", left, pid)
		}
	}
	for _, ex := range td.stillHeld {
		v.add("%s was still held %s after StopSession returned", requestLabel(ex), holdEndBound)
	}
	for _, left := range td.remoteLeft {
		v.add("%q still ran on the SSH host %s after StopSession returned", left, holdEndBound)
	}
	for _, pid := range pids {
		if !slices.Contains(td.aliveWhileHeld, pid) {
			v.add("reported process %d is not among the processes alive while the request was held", pid)
		}
	}
	for _, pid := range td.aliveWhileHeld {
		if left := remaining(pid); left != "" {
			v.add("%s of process %d, alive while the request was held, still answers %s after StopSession returned", left, pid, holdEndBound)
		}
	}
	for _, released := range td.heldLaunches {
		if err := launchtest.ReleaseError(released()); err != nil {
			v.add("launch of a process alive while the request was held, after StopSession returned: %v", err)
		}
	}
	return v.items
}

func reportedProcessIDs(run Run) []string {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(run.Session.AgentPID)
	for _, event := range run.Events {
		add(event.AgentPID)
	}
	return ids
}

// continuationViolations checks a continuation run whose turns[i] answered
// script entry i, in run order: origin, origin, control, resumed.
//
// A non-nil spend leaves every turn's completeness to the provenance property,
// which compares it with the completeness the binding expects.
//
// Under a lower-bound settling the resumed turn may carry no figure, provided
// it flags its spend unaccounted; otherwise it must be measured.
func continuationViolations(turns []continuedTurn, resumed domain.Session, exchanges []Exchange, spend *SpendSources, settling usageSettling) []string {
	v := &findings{property: "continuation"}

	checkContinuationIdentifiers(v, turns, resumed)
	for entry, turn := range turns {
		ex, ok := turnExchangeOf(exchanges, entry)
		if !ok {
			v.add("%s script entry %d: no turn exchange reached the endpoint", turn.role, entry)
			continue
		}
		wanted, forbidden := requestMarkers(turns, entry)
		for _, m := range wanted {
			if !strings.Contains(string(ex.Body), m.text) {
				v.add("%s script entry %d, %s, lacks the %s", turn.role, entry, requestLabel(ex), m.label)
			}
		}
		for _, m := range forbidden {
			if strings.Contains(string(ex.Body), m.text) {
				v.add("%s script entry %d, %s, carries the %s", turn.role, entry, requestLabel(ex), m.label)
			}
		}
	}

	if first := turns[0].result; spend == nil && first.UsageMeasured && first.SpendUnaccounted {
		v.add("%s turn 1: Result.SpendUnaccounted is true, want false for a completed turn with a measured figure", roleOrigin)
	}
	if last := turns[resumedEntry].result; !last.UsageMeasured {
		switch {
		case !settling.liveLowerBound:
			v.add("%s turn: Result.UsageMeasured is false, want a measured figure", roleResumed)
		case !last.SpendUnaccounted:
			v.add("%s turn: Result.SpendUnaccounted is false, want true for a turn with no figure", roleResumed)
		}
	}
	return v.items
}

func checkContinuationIdentifiers(v *findings, turns []continuedTurn, resumed domain.Session) {
	id := turns[continuedEntry].result.SessionID
	if id == "" {
		v.add("%s turn 2: Result.SessionID is empty", roleOrigin)
	}
	if got := turns[0].result.SessionID; got != id {
		v.add("%s turn 1: Result.SessionID = %q, want %q, the id of its last turn", roleOrigin, got, id)
	}

	resumedTurn := turns[resumedEntry]
	if resumed.ID != id {
		v.add("%s session: Session.ID = %q, want %q", roleResumed, resumed.ID, id)
	}
	if got := resumedTurn.result.SessionID; got != id {
		v.add("%s turn: Result.SessionID = %q, want %q", roleResumed, got, id)
	}
	started := 0
	for _, event := range resumedTurn.events {
		if event.Type != domain.EventSessionStarted || event.SessionID == "" {
			continue
		}
		started++
		if event.SessionID != id {
			v.add("%s turn: %q event carries session id %q, want %q", roleResumed, event.Type, event.SessionID, id)
		}
	}
	if started == 0 {
		v.add("%s turn: no %q event carried a session id", roleResumed, domain.EventSessionStarted)
	}

	controlID := turns[controlEntry].result.SessionID
	if controlID == "" || controlID == id {
		v.add("%s turn: Result.SessionID = %q, want a non-empty id other than the origin's %q", roleControl, controlID, id)
	}
}

type marker struct {
	label string
	text  string
}

// requestMarkers lists the origin markers the request answering entry must
// carry and those it must not: the continued and the resumed request replay
// the origin's earlier turns, and the control request sees none of them.
func requestMarkers(turns []continuedTurn, entry int) (wanted, forbidden []marker) {
	switch entry {
	case continuedEntry:
		return originMarkers(turns[:continuedEntry]), nil
	case controlEntry:
		return nil, originMarkers(turns[:controlEntry])
	case resumedEntry:
		return originMarkers(turns[:controlEntry]), nil
	}
	return nil, nil
}

func originMarkers(turns []continuedTurn) []marker {
	var markers []marker
	for i, turn := range turns {
		markers = append(markers,
			marker{label: fmt.Sprintf("%s turn %d prompt marker", turn.role, i+1), text: turn.prompt},
			marker{label: fmt.Sprintf("%s turn %d answer marker", turn.role, i+1), text: turn.answer})
	}
	return markers
}

func turnExchangeOf(exchanges []Exchange, entry int) (Exchange, bool) {
	i := slices.IndexFunc(exchanges, func(ex Exchange) bool { return ex.Kind == ExchangeTurn && ex.Step == entry })
	if i < 0 {
		return Exchange{}, false
	}
	return exchanges[i], true
}
