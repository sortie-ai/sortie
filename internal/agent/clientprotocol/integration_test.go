// Integration tests for the Agent Client Protocol transport against a locally
// installed protocol-speaking binary.
//
// Required environment variables:
//
//	SORTIE_CLIENTPROTOCOL_TEST=1     enable this suite
//	SORTIE_CLIENTPROTOCOL_COMMAND    the binary's launch command, including the
//	                                 flag that puts it into Agent Client
//	                                 Protocol mode
//
// The suite names no default binary, so no single vendor becomes the implicit
// reference for the generic kind. Without both variables every test skips.
// The runtime must complete a turn, answer it with stop reason end_turn,
// stream at least one text message chunk, and report agentInfo in its
// initialize response.
//
// The scripted-model cases launch the same runtime against a loopback scripted
// model endpoint, replace every named credential with a sentinel, spend no
// quota, and skip unless the model endpoint and the credential names are
// given. They also resume a session the runtime created, so the runtime must
// continue it through session/load or session/resume; a runtime that cannot
// gets no scripted case:
//
//	SORTIE_CLIENTPROTOCOL_MODEL_BASE_URL_ENV
//	                                  name of the environment variable through
//	                                  which the runtime takes its model base
//	                                  address
//	SORTIE_CLIENTPROTOCOL_CREDENTIAL_ENV
//	                                  comma-separated names of the environment
//	                                  variables the runtime reads its
//	                                  credential from
//	SORTIE_CLIENTPROTOCOL_ASKING_COMMAND
//	                                  the launch command in a posture that asks
//	                                  before running a tool; unset skips the
//	                                  refusal case
//	SORTIE_CLIENTPROTOCOL_USER_INSTRUCTIONS
//	                                  path of the user-level instruction file
//	                                  relative to the scenario home, for a
//	                                  runtime whose verification request keeps
//	                                  no user instructions
//	SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT
//	                                  provider document for a runtime that
//	                                  takes its model endpoint from a file;
//	                                  {url} stands for the endpoint, and the
//	                                  path of the file written from it goes to
//	                                  the model base address variable
//	SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT_PATH
//	                                  path relative to the scenario home where
//	                                  the model document is written, for a
//	                                  runtime that reads its endpoint from a
//	                                  fixed location; it replaces the model
//	                                  base address variable
//	SORTIE_CLIENTPROTOCOL_HTTP_TOOLS  1 when the runtime advertises
//	                                  mcpCapabilities.http: the local tool-server
//	                                  case must then reach the relay over
//	                                  loopback HTTP, and the tool-server SSH
//	                                  cases run
//	SORTIE_CLIENTPROTOCOL_SSH_COMMAND the launch command on the SSH lab host,
//	                                  which has only what
//	                                  SORTIE_CLIENTPROTOCOL_SSH_INSTALL
//	                                  installs; the SSH cases skip without it
//	SORTIE_CLIENTPROTOCOL_SSH_INSTALL POSIX shell script that installs the
//	                                  runtime into the lab image as root, for
//	                                  instance with npm install -g
//	SORTIE_CLIENTPROTOCOL_SPEND_DECLARATION
//	                                  the handshake name of the usage declaration
//	                                  whose spend sources the scripted model
//	                                  cases prove: exact figure, settling source
//	                                  and strategy, and the kill; unset leaves
//	                                  spend unproven; "default" proves the
//	                                  declaration a runtime with none of its own
//	                                  is measured under, whose figure is a lower
//	                                  bound
//	SORTIE_CLIENTPROTOCOL_REPORTED_MODEL_PREFIX
//	                                  text the runtime puts before a served
//	                                  model's name in the model name it
//	                                  reports; unset when it reports the
//	                                  served name
//	SORTIE_CLIENTPROTOCOL_STATUS_CALL
//	                                  JSON {"name": ..., "arguments": {...}}
//	                                  of the call that reaches sortie_status,
//	                                  for a runtime that offers server tools
//	                                  only through another tool; unset calls
//	                                  the declared sortie_status tool
//
// Run:
//
//	SORTIE_CLIENTPROTOCOL_TEST=1 SORTIE_CLIENTPROTOCOL_COMMAND="..." \
//	  go test ./internal/agent/clientprotocol/... -run Integration
package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

type liveProtocolCapture struct {
	clientPath string
	agentPath  string
	started    time.Time
	expect     captureExpectation
	events     func() []domain.AgentEvent // nil for expectHandshakeOnly
}

func skipUnlessClientProtocolIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CLIENTPROTOCOL_TEST") != "1" {
		t.Skip("skipping Agent Client Protocol integration test: set SORTIE_CLIENTPROTOCOL_TEST=1 to enable")
	}
	if os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND") == "" {
		t.Skip("skipping Agent Client Protocol integration test: SORTIE_CLIENTPROTOCOL_COMMAND must name a protocol-speaking binary and its launch flags")
	}
}

func integrationAgentConfig(t *testing.T, expect captureExpectation, events func() []domain.AgentEvent) domain.AgentConfig {
	t.Helper()
	config, _ := captureAgentConfig(t, os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND"), expect, events)
	return config
}

func captureAgentConfig(t *testing.T, command string, expect captureExpectation, events func() []domain.AgentEvent) (domain.AgentConfig, liveProtocolCapture) {
	t.Helper()
	dir := t.TempDir()
	capture := liveProtocolCapture{
		clientPath: filepath.Join(dir, "client.jsonl"),
		agentPath:  filepath.Join(dir, "agent.jsonl"),
		started:    time.Now(),
		expect:     expect,
		events:     events,
	}
	wrapperPath := filepath.Join(dir, "capture.sh")
	wrapper := fmt.Sprintf("#!/bin/sh\nset -eu\ncase \"$*\" in %[1]q|%[1]q\" \"*) ;; *) exec \"$@\" ;; esac\ntee %[2]q | \"$@\" | tee %[3]q\n", command, capture.clientPath, capture.agentPath)
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("integrationAgentConfig() write capture wrapper: %v", err)
	}
	t.Cleanup(func() { assertLiveProtocolConformance(t, capture) })

	return domain.AgentConfig{
		Command: wrapperPath + " " + command,
		// Generous for a real model's latency.
		TurnTimeoutMS: 300000,
		ReadTimeoutMS: 30000,
	}, capture
}

func capturedJSONLines(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // path is created under this test's temp directory
	if err != nil {
		t.Fatalf("capturedJSONLines(%q) error = %v", path, err)
	}
	return splitCapturedFrames(raw)
}

func splitCapturedFrames(raw []byte) [][]byte {
	// Stopping the session kills the capture's tee mid-copy, so a final line
	// without its newline is a torn copy, not a frame the agent sent.
	text := string(raw)
	if end := strings.LastIndexByte(text, '\n'); end < len(text)-1 {
		text = text[:end+1]
	}
	var lines [][]byte
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if line != "" {
			lines = append(lines, []byte(line))
		}
	}
	return lines
}

func jsonRPCIDKey(raw json.RawMessage) string {
	return string(raw)
}

func assertLiveProtocolConformance(t *testing.T, capture liveProtocolCapture) {
	t.Helper()

	defer func() {
		if t.Failed() {
			logCapturedFrames(t, "client", capture.clientPath, capture.started)
			logCapturedFrames(t, "agent", capture.agentPath, capture.started)
		}
	}()

	requestMethods := make(map[string]string)
	for _, line := range capturedJSONLines(t, capture.clientPath) {
		var header wireHeader
		if err := json.Unmarshal(line, &header); err != nil {
			t.Errorf("decode captured client message %q: %v", line, err)
			continue
		}
		if header.Method != "" && len(header.ID) != 0 {
			requestMethods[jsonRPCIDKey(header.ID)] = header.Method
		}
	}

	defs := loadSchemaDefs(t, schemaAssetsDir)
	var undeclared []string
	for i, line := range capturedJSONLines(t, capture.agentPath) {
		var envelope struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params any             `json:"params"`
			Result any             `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Errorf("decode live agent message %d = %q, want JSON-RPC: %v", i, line, err)
			continue
		}

		var defName string
		var body any
		switch envelope.Method {
		case methodSessionUpdate:
			defName, body = "SessionNotification", envelope.Params
		case methodSessionRequestPermission:
			defName, body = "RequestPermissionRequest", envelope.Params
		case "":
			if envelope.Error != nil {
				t.Logf("live agent response %d carried a JSON-RPC error; no success body to validate", i)
				continue
			}
			switch requestMethods[jsonRPCIDKey(envelope.ID)] {
			case methodInitialize:
				defName = "InitializeResponse"
			case methodSessionNew:
				defName = "NewSessionResponse"
			case methodSessionLoad:
				defName = "LoadSessionResponse"
			case methodSessionResume:
				defName = "ResumeSessionResponse"
			case methodSessionPrompt:
				defName = "PromptResponse"
			default:
				undeclared = append(undeclared, fmt.Sprintf("message[%d].response", i))
				continue
			}
			body = envelope.Result
		default:
			undeclared = append(undeclared, fmt.Sprintf("message[%d].method(%s)", i, envelope.Method))
			continue
		}

		violations, messageUndeclared := checkWeak(defs, defName, body)
		for _, violation := range violations {
			t.Errorf("live agent message %d: %s", i, violation)
		}
		undeclared = append(undeclared, messageUndeclared...)
	}
	t.Logf("live agent undeclared properties: %v", undeclared)

	var events []domain.AgentEvent
	if capture.events != nil {
		events = capture.events()
	}
	shapeViolations, shapeObservations, observed := recordedShapeViolations(capturedJSONLines(t, capture.clientPath), capturedJSONLines(t, capture.agentPath), events, capture.expect)
	for _, v := range shapeViolations {
		t.Errorf("live shape violation: %s", v)
	}
	for _, o := range shapeObservations {
		t.Logf("live shape observation: %s", o)
	}
	t.Logf("live shape observation: %+v", observed)
}

func logCapturedFrames(t *testing.T, side, path string, started time.Time) {
	t.Helper()
	if info, err := os.Stat(path); err == nil {
		t.Logf("%s capture: last write %v after StartSession began", side, info.ModTime().Sub(started).Round(100*time.Millisecond))
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is created under this test's temp directory
	if err != nil {
		t.Logf("%s capture: %v", side, err)
		return
	}
	const maxFrameBytes = 400
	for i, line := range splitCapturedFrames(raw) {
		if len(line) > maxFrameBytes {
			line = append(line[:maxFrameBytes:maxFrameBytes], "..."...)
		}
		t.Logf("%s frame %d: %s", side, i, line)
	}
}

func gitInitWorkspace(t *testing.T) string {
	t.Helper()
	// Several protocol-speaking runtimes expect a git repository by default.
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitInitWorkspace: git init: %v\n%s", err, out)
	}
	return dir
}

func mustNewClientProtocolAdapter(t *testing.T) *ClientProtocolAdapter {
	t.Helper()
	a, err := NewClientProtocolAdapter()
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter: %v", err)
	}
	return a.(*ClientProtocolAdapter)
}

func mustStartClientProtocolSession(t *testing.T, ctx context.Context, adapter *ClientProtocolAdapter, workspace string, expect captureExpectation, events func() []domain.AgentEvent) domain.Session {
	t.Helper()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expect, events),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })
	return session
}

func makeEventCollector(t *testing.T) (onEvent func(domain.AgentEvent), collected func() []domain.AgentEvent) {
	t.Helper()
	var mu sync.Mutex
	var events []domain.AgentEvent
	onEvent = func(e domain.AgentEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	collected = func() []domain.AgentEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]domain.AgentEvent, len(events))
		copy(out, events)
		return out
	}
	return onEvent, collected
}

func assertContainsEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			return
		}
	}
	types := make([]domain.AgentEventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	t.Errorf("expected event type %q not found; got types: %v", eventType, types)
}

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expectHandshakeOnly, nil),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if session.ID == "" {
		t.Error("session.ID is empty; expected a non-empty session id from session/new")
	}
	if session.AgentPID == "" {
		t.Error("session.AgentPID is empty; expected the PID of the launched subprocess")
	}
	if session.Internal == nil {
		t.Error("session.Internal is nil")
	}
}

func TestIntegration_StopSession(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(t, expectHandshakeOnly, nil),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession (second call): %v", err)
	}
}

func TestIntegration_ToolForcingTurn(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter := mustNewClientProtocolAdapter(t)
	workspace := gitInitWorkspace(t)
	targetFile := filepath.Join(workspace, "greeting.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	onEvent, collected := makeEventCollector(t)
	session := mustStartClientProtocolSession(t, ctx, adapter, workspace, expectToolForcingTurn, collected)

	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: fmt.Sprintf(
			"Create a file at %s containing exactly the text: hello from sortie. "+
				"After writing it, read the file back and report exactly what it contains.",
			targetFile,
		),
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	events := collected()
	t.Logf("received %d events, exit reason: %q", len(events), result.ExitReason)

	assertCapabilityGapLabelsMatchProfile(t, events, result)

	if result.SessionID != session.ID {
		t.Errorf("TurnResult.SessionID = %q, want %q", result.SessionID, session.ID)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	assertContainsEventType(t, events, domain.EventSessionStarted)
}

func TestIntegration_CredentialVerification(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	passthrough := map[string]any{}
	adapter, err := NewClientProtocolAdapter()
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}
	params := func(t *testing.T) domain.StartSessionParams {
		return domain.StartSessionParams{
			WorkspacePath: gitInitWorkspace(t),
			AgentConfig: domain.AgentConfig{
				Command:       os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND"),
				TurnTimeoutMS: 300000,
				ReadTimeoutMS: 30000,
			},
		}
	}

	t.Run("working credential verifies", func(t *testing.T) {
		credentialtest.VerifyLiveUsage(t, "agent-client-protocol", adapter, params(t), passthrough)
	})

	t.Run("refused credential ends credential_unverified", func(t *testing.T) {
		// Not parallel: t.Setenv carries the invalid credential.
		credentialtest.SetRefusedCredential(t, "SORTIE_CLIENTPROTOCOL_CREDENTIAL_ENV")

		_, err := credentialtest.VerifyLive(adapter, params(t))
		credentialtest.RequireRefused(t, err)
	})
}

func TestIntegration_EarlyExit(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	adapter, err := NewClientProtocolAdapter()
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}
	unknownSwitchConfig := domain.AgentConfig{
		Command:       os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND") + " --sortie-unknown-switch",
		TurnTimeoutMS: 300000,
		ReadTimeoutMS: 30000,
	}

	t.Run("verification session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{
			WorkspacePath: gitInitWorkspace(t),
			AgentConfig:   unknownSwitchConfig,
		}
		if _, err := credentialtest.VerifyLive(adapter, params); err == nil {
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})

	t.Run("working session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{
			WorkspacePath: gitInitWorkspace(t),
			AgentConfig:   unknownSwitchConfig,
		}
		session, err := adapter.StartSession(context.Background(), params)
		if err == nil {
			_ = adapter.StopSession(context.Background(), session)
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func claimedVerificationContext(command string) registry.VerificationContext {
	words := strings.Fields(command)
	if len(words) == 0 {
		return registry.VerificationContext{}
	}
	addition, _, claimed := usagesource.ClaimVerification(agentcore.LaunchTarget{Command: words[0], Args: words[1:]}, os.Environ())
	if !claimed {
		return registry.VerificationContext{}
	}
	defer addition.Close()
	return addition.Context()
}

func writeModelDocument(t *testing.T, path, template, url string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(template, "{url}", url)), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func sshVariant() *fakemodel.SSHVariant {
	variant := &fakemodel.SSHVariant{
		Install: os.Getenv("SORTIE_CLIENTPROTOCOL_SSH_INSTALL"),
		RemoteCommand: func(fakemodel.Environment) string {
			return os.Getenv("SORTIE_CLIENTPROTOCOL_SSH_COMMAND")
		},
	}
	if os.Getenv("SORTIE_CLIENTPROTOCOL_SSH_COMMAND") == "" {
		variant.Skip = "the runtime's launch command on the lab host is unset: set SORTIE_CLIENTPROTOCOL_SSH_COMMAND"
	}
	if os.Getenv("SORTIE_CLIENTPROTOCOL_HTTP_TOOLS") != "1" {
		variant.ToolServerSkip = "set SORTIE_CLIENTPROTOCOL_HTTP_TOOLS=1 when the runtime advertises mcpCapabilities.http; without the advertisement Sortie declares a tool-server gap and delivers nothing"
	}
	return variant
}

const (
	protocolKind          = "agent-client-protocol"
	emptyDeclarationsKind = "agent-client-protocol-empty-declarations"
)

// The binding builds its adapter from the kind registry, which leaves a test no
// way to set the adapter's declarations; a kind of its own is the only route to
// an adapter that selects the default declaration whatever the handshake names.
var registerEmptyDeclarationsKind = sync.OnceFunc(func() {
	meta, _ := registry.Agents.Meta(protocolKind)
	registry.Agents.RegisterWithMeta(emptyDeclarationsKind, func() (domain.AgentAdapter, error) {
		return &ClientProtocolAdapter{declarations: registry.NewRegistry[usagesource.Declaration, struct{}]("usage declaration")}, nil
	}, meta)
})

func bindingKind() string {
	if os.Getenv(spendDeclarationEnv) != usagesource.DefaultName {
		return protocolKind
	}
	registerEmptyDeclarationsKind()
	return emptyDeclarationsKind
}

func statusCall(t *testing.T) fakemodel.ToolChoice {
	t.Helper()
	raw := os.Getenv("SORTIE_CLIENTPROTOCOL_STATUS_CALL")
	if raw == "" {
		return nil
	}
	var call fakemodel.FunctionCall
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		t.Fatalf("SORTIE_CLIENTPROTOCOL_STATUS_CALL = %q, want a JSON object with name and arguments: %v", raw, err)
	}
	return fakemodel.Named(call.Name, call.Arguments)
}

func spendSources(t *testing.T) *fakemodel.SpendSources {
	t.Helper()

	const variable = spendDeclarationEnv
	name := os.Getenv(variable)
	if name == "" {
		return nil
	}
	defaultSources := &fakemodel.SpendSources{
		Live:       domain.UsageProvenance{Source: usage.SourceProtocol, Strategy: usage.StrategyLive},
		LowerBound: true,
	}
	if name == usagesource.DefaultName {
		return defaultSources
	}
	declaration, err := usagesource.Declarations.Get(name)
	if err != nil {
		t.Fatalf("%s = %q, want the handshake name of a registered usage declaration: %v", variable, name, err)
	}
	liveSpend := declaration.Live.Spend.Shape != usage.SpendUndeclared
	recordsSpend := declaration.Records.Spend.Shape != usage.SpendUndeclared
	if !liveSpend && !recordsSpend {
		return defaultSources
	}
	if !liveSpend || !recordsSpend {
		t.Fatalf("%s = %q, want a declaration whose live and records sources both declare spend, or neither", variable, name)
	}
	return &fakemodel.SpendSources{
		Live:             domain.UsageProvenance{Source: declaration.Live.Category, Strategy: declaration.Live.Strategy},
		Records:          domain.UsageProvenance{Source: declaration.Records.Category, Strategy: declaration.Records.Strategy},
		RecordsAtTurnEnd: declaration.Records.WrittenAtTurnEnd,
	}
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipUnlessClientProtocolIntegration(t)

	baseURLEnv := os.Getenv("SORTIE_CLIENTPROTOCOL_MODEL_BASE_URL_ENV")
	documentPath := os.Getenv("SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT_PATH")
	if baseURLEnv == "" && documentPath == "" {
		t.Skip("skipping scripted model cases: SORTIE_CLIENTPROTOCOL_MODEL_BASE_URL_ENV must name the variable through which the runtime takes its model base address, or SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT_PATH the file it reads it from")
	}
	credentialNames := credentialtest.CredentialNames("SORTIE_CLIENTPROTOCOL_CREDENTIAL_ENV")
	if len(credentialNames) == 0 {
		t.Skip("skipping scripted model cases: SORTIE_CLIENTPROTOCOL_CREDENTIAL_ENV must name the credential variables, or a real key in the environment would reach the endpoint")
	}
	if baseURLEnv != "" && !environmentNamePattern.MatchString(baseURLEnv) {
		t.Fatalf("SORTIE_CLIENTPROTOCOL_MODEL_BASE_URL_ENV = %q, want a name matching %s", baseURLEnv, environmentNamePattern)
	}

	askingCommand := os.Getenv("SORTIE_CLIENTPROTOCOL_ASKING_COMMAND")
	var skipRefusal string
	if askingCommand == "" {
		skipRefusal = "skipping permission refusal: SORTIE_CLIENTPROTOCOL_ASKING_COMMAND must name the runtime's launch command in a posture that asks before running a tool"
	}

	modelDocument := os.Getenv("SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT")
	if documentPath != "" && (modelDocument == "" || !filepath.IsLocal(documentPath)) {
		t.Fatalf("SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT_PATH = %q, want a path inside the scenario home and a non-empty SORTIE_CLIENTPROTOCOL_MODEL_DOCUMENT", documentPath)
	}

	turnExpect := expectScriptedToolTurn
	binding := fakemodel.Binding{
		SortieStatus:         statusCall(t),
		Kind:                 bindingKind(),
		RuntimeFailureFolded: namedProfileFoldsRuntimeFailure(t),
		LocalToolsOverHTTP:   os.Getenv("SORTIE_CLIENTPROTOCOL_HTTP_TOOLS") == "1",
		SSH:                  sshVariant(),
		Spend:                spendSources(t),
		ReportedModelPrefix:  os.Getenv("SORTIE_CLIENTPROTOCOL_REPORTED_MODEL_PREFIX"),
		Passthrough:          map[string]any{},
		CredentialEnv:        credentialNames,
		UserInstructions:     os.Getenv("SORTIE_CLIENTPROTOCOL_USER_INSTRUCTIONS"),
		VerificationContext:  claimedVerificationContext(os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND")),
		Read:                 fakemodel.ReadFile,
		PermissionRefusal: &fakemodel.PermissionRefusal{
			Guarded: fakemodel.CatFile,
			Notice:  agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending).Notice,
			Skip:    skipRefusal,
		},
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			command := os.Getenv("SORTIE_CLIENTPROTOCOL_COMMAND")
			expect := turnExpect
			switch env.Scenario {
			case fakemodel.ScenarioExhaustion, fakemodel.ScenarioCancellation, fakemodel.ScenarioCancellationAfterOutput, fakemodel.ScenarioKill, fakemodel.ScenarioStop:
				expect = expectHandshakeOnly
			case fakemodel.ScenarioContinuation, fakemodel.ScenarioVerificationContext:
				expect = expectCompletedTurn
			case fakemodel.ScenarioPermissionRefusal:
				command = askingCommand
			}
			launchEnv := map[string]string{}
			switch {
			case documentPath != "":
				writeModelDocument(t, filepath.Join(env.Home, documentPath), modelDocument, env.URL)
			case modelDocument != "":
				launchEnv[baseURLEnv] = writeModelDocument(t, filepath.Join(env.Home, "model-document.json"), modelDocument, env.URL)
			default:
				launchEnv[baseURLEnv] = env.URL
			}
			if env.Remote {
				return fakemodel.Launch{
					Config: domain.AgentConfig{TurnTimeoutMS: 300000, ReadTimeoutMS: 30000},
					Env:    launchEnv,
				}
			}
			config, capture := captureAgentConfig(t, command, expect, env.Events)
			return fakemodel.Launch{
				Config:  config,
				Env:     launchEnv,
				Streams: []string{capture.clientPath, capture.agentPath},
			}
		},
	}

	fakemodel.AssertConformance(t, binding)

	t.Run("tool-forcing turn", func(t *testing.T) {
		turnExpect = expectToolForcingTurn
		workspace := gitInitWorkspace(t)
		target := filepath.Join(workspace, "greeting.txt")
		usage := fakemodel.Usage{Prompt: 101, Candidates: 7}

		run := fakemodel.RunCustomTurn(t, binding, fakemodel.CustomTurn{
			Script: []fakemodel.Response{
				{Call: fakemodel.WriteFile(target, "hello from sortie"), Usage: usage},
				{Call: fakemodel.ReadFile(target), Usage: usage},
				{Text: "The file holds: hello from sortie", Usage: usage},
			},
			Prompt:    "Create a file at " + target + " containing exactly the text: hello from sortie. After writing it, read the file back and report exactly what it contains.",
			Workspace: workspace,
		})

		assertCapabilityGapLabelsMatchProfile(t, run.Events, run.Result)
		if run.Result.SessionID != run.Session.ID {
			t.Errorf("TurnResult.SessionID = %q, want %q", run.Result.SessionID, run.Session.ID)
		}
		if run.Result.ExitReason != domain.EventTurnCompleted {
			t.Errorf("TurnResult.ExitReason = %q, want %q", run.Result.ExitReason, domain.EventTurnCompleted)
		}
		assertContainsEventType(t, run.Events, domain.EventSessionStarted)
	})
}
