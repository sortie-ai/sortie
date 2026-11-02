//go:build unix

package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func writePiScript(t *testing.T, dir, body string) string {
	t.Helper()
	return agenttest.WriteScript(t, dir, "fake-pi", versionAnsweringScript(body))
}

func versionAnsweringScript(body string) string {
	return "case \"$1\" in\n  --version) echo '0.99.0'; exit 0;;\nesac\n" + body
}

func mustStartSession(t *testing.T, a domain.AgentAdapter, workDir, cmd string) domain.Session {
	t.Helper()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workDir,
		AgentConfig:   domain.AgentConfig{Command: cmd},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	return session
}

func collectEvents(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult, error) {
	t.Helper()
	var events []domain.AgentEvent
	result, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  prompt,
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	return events, result, err
}

// writeRunFixtureScript writes fixtureName into dir and returns a fake-pi
// that replays it for any invocation. The fixtures carry the session
// header's cwd as the __CWD__ placeholder, which the script replaces with
// its own working directory, because the adapter compares the header's cwd
// with the turn's workspace.
func writeRunFixtureScript(t *testing.T, dir, fixtureName string) string {
	t.Helper()
	runPath := filepath.Join(dir, fixtureName)
	data, err := os.ReadFile(filepath.Join("testdata", fixtureName))
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", fixtureName, err)
	}
	if err := os.WriteFile(runPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fixtureName, err)
	}
	return writePiScript(t, dir, `sed "s|__CWD__|$PWD|g" '`+runPath+`'`)
}

// writeInlineRunScript returns a fake-pi that replays lines after a session
// header naming its own working directory.
func writeInlineRunScript(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "stream.jsonl")
	body := `{"type":"session","version":3,"id":"stream-1","timestamp":"t","cwd":"__CWD__"}` + "\n" +
		strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return writePiScript(t, dir, `sed "s|__CWD__|$PWD|g" '`+path+`'`)
}

func eventsOfType(events []domain.AgentEvent, typ domain.AgentEventType) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func textNotifications(events []domain.AgentEvent) []string {
	var out []string
	for _, e := range eventsOfType(events, domain.EventNotification) {
		if e.Message != "" {
			out = append(out, e.Message)
		}
	}
	return out
}

func agentErrorOf(t *testing.T, err error) *domain.AgentError {
	t.Helper()
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error = %v (%T), want *domain.AgentError", err, err)
	}
	return agentErr
}

func TestRunTurn_SimpleText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "simple_turn.jsonl"))

	events, result, err := collectEvents(t, a, session, "do work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if got := textNotifications(events); len(got) != 1 || got[0] != "Hello, world!" {
		t.Errorf("text notifications = %v, want [Hello, world!]", got)
	}
	if got := eventsOfType(events, domain.EventMalformed); len(got) != 0 {
		t.Errorf("malformed events = %+v, want none", got)
	}
	if result.Usage.InputTokens != 110 || result.Usage.OutputTokens != 20 || result.Usage.CacheReadTokens != 10 {
		t.Errorf("usage = %+v, want input 110 output 20 cacheRead 10", result.Usage)
	}
	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}
}

func TestRunTurn_ToolFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture      string
		wantTool     string
		wantKind     domain.ToolKind
		wantArgument string
		wantToolErr  bool
		wantUsage    domain.TokenUsage
	}{
		{
			fixture: "tool_success.jsonl", wantTool: "write", wantKind: domain.ToolKindEdit, wantArgument: "hello.txt",
			wantUsage: domain.TokenUsage{InputTokens: 90, OutputTokens: 18, TotalTokens: 108},
		},
		{
			fixture: "tool_error.jsonl", wantTool: "bash", wantKind: domain.ToolKindShell, wantToolErr: true,
			wantUsage: domain.TokenUsage{InputTokens: 95, OutputTokens: 22, TotalTokens: 117},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a, _ := NewPiAdapter()
			session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, tt.fixture))

			events, result, err := collectEvents(t, a, session, "work")
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			tools := eventsOfType(events, domain.EventToolResult)
			if len(tools) != 1 {
				t.Fatalf("tool results = %+v, want exactly one", tools)
			}
			if tools[0].ToolName != tt.wantTool || tools[0].ToolKind != tt.wantKind || tools[0].ToolError != tt.wantToolErr {
				t.Errorf("tool result = %+v, want name %q kind %q error %t", tools[0], tt.wantTool, tt.wantKind, tt.wantToolErr)
			}
			if tt.wantArgument != "" && tools[0].ToolArgument != tt.wantArgument {
				t.Errorf("ToolArgument = %q, want %q", tools[0].ToolArgument, tt.wantArgument)
			}
			if result.Usage != tt.wantUsage {
				t.Errorf("usage = %+v, want %+v", result.Usage, tt.wantUsage)
			}
			agenttest.AssertUsageReporting(t, "pi", []agenttest.UsageReportingCase{
				{Name: "a figure per response around the tool result", Events: events, Result: result},
			})
		})
	}
}

// pi exits 0 for a response that stopped in failure.
func TestRunTurn_AssistantFailureWithExitZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture  string
		wantKind domain.AgentErrorKind
		wantMsg  string
	}{
		{fixture: "assistant_error.jsonl", wantKind: domain.ErrTurnFailed, wantMsg: "upstream 529 overloaded"},
		{fixture: "assistant_aborted.jsonl", wantKind: domain.ErrTurnFailed, wantMsg: "aborted"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a, _ := NewPiAdapter()
			session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, tt.fixture))

			_, result, err := collectEvents(t, a, session, "work")
			if err == nil {
				t.Fatal("RunTurn() error = nil, want a failure")
			}
			agentErr := agentErrorOf(t, err)
			if agentErr.Kind != tt.wantKind || !strings.Contains(agentErr.Message, tt.wantMsg) {
				t.Errorf("error = %q %q, want kind %q containing %q", agentErr.Kind, agentErr.Message, tt.wantKind, tt.wantMsg)
			}
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
		})
	}
}

func TestRunTurn_RetryThenSuccessFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "retry_then_success.jsonl"))

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil || result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn() = (%+v, %v), want a completed turn", result, err)
	}
	if result.Usage.InputTokens != 29442 {
		t.Errorf("usage = %+v, want input 29442", result.Usage)
	}
	if got := len(eventsOfType(events, domain.EventTokenUsage)); got != 1 {
		t.Errorf("token_usage events = %d, want one: the failed attempt reports no counts", got)
	}
}

func TestRunTurn_RetryErrorFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "real_retry_error.jsonl"))

	_, _, err := collectEvents(t, a, session, "work")
	if err == nil || agentErrorOf(t, err).Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want turn_failed", err)
	}
}

func TestRunTurn_TruncatedResponseWithExitZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resp := responseSpec{text: "cut off", stop: "length", input: 10, output: 4}
	script := writeInlineRunScript(t, dir, messageEnd(resp), `{"type":"agent_end","messages":[],"willRetry":false}`)
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if err == nil || agentErrorOf(t, err).Kind != domain.ErrTurnTokenLimit {
		t.Fatalf("RunTurn() error = %v, want turn_token_limit", err)
	}
	if result.ExitReason != domain.EventTurnFailed || result.Usage.OutputTokens != 4 {
		t.Errorf("result = %+v, want a failed turn that keeps its usage", result)
	}
}

func TestRunTurn_ExitZeroNoWorkIsFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := writeInlineRunScript(t, dir, `{"type":"agent_start"}`, `{"type":"turn_start"}`, `{"type":"agent_end","messages":[],"willRetry":false}`)
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, script)

	if _, _, err := collectEvents(t, a, session, "work"); err == nil {
		t.Fatal("RunTurn() error = nil, want a zero-work failure")
	}
}

func TestRunTurn_NonZeroExitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	script := writePiScript(t, dir, `printf '{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"%s"}\n' "$PWD"; exit 3`)
	session := mustStartSession(t, a, dir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if err == nil {
		t.Fatal("RunTurn() error = nil, want a failure")
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
}

func TestRunTurn_UsageAccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "simple_turn.jsonl"))

	first, one, err := collectEvents(t, a, session, "one")
	if err != nil {
		t.Fatalf("first RunTurn() error = %v", err)
	}
	second, two, err := collectEvents(t, a, session, "two")
	if err != nil {
		t.Fatalf("second RunTurn() error = %v", err)
	}
	agenttest.AssertUsageContract(t, append(append([]domain.AgentEvent{}, first...), second...))
	if one.Usage.InputTokens != 110 || two.Usage.InputTokens != 220 || two.Usage.OutputTokens != 40 || two.Usage.TotalTokens != 260 {
		t.Errorf("usage = %+v then %+v, want the session total to grow to input 220 output 40", one.Usage, two.Usage)
	}
}

// stageHeldSession stages a session file under the session directory pi names
// after workingDir's physical path.
func stageHeldSession(t *testing.T, workingDir, sessionID string) {
	t.Helper()
	physical, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", workingDir, err)
	}
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	projectDir := filepath.Join(agentDir, "sessions", sessionDirName(physical))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", projectDir, err)
	}
	file := filepath.Join(projectDir, "2026-01-01T00-00-00-000Z_"+sessionID+".jsonl")
	if err := os.WriteFile(file, []byte(`{"type":"session","id":"`+sessionID+`"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", file, err)
	}
}

func TestSessionContinuity(t *testing.T) {
	// Not parallel: the held session is staged under PI_CODING_AGENT_DIR through t.Setenv.
	a, _ := NewPiAdapter()

	agenttest.AssertSessionContinuity(t, func(t *testing.T, held, resumeID string) (domain.Session, error) {
		t.Helper()
		dir := t.TempDir()
		stageHeldSession(t, dir, held)
		session, err := a.StartSession(context.Background(), domain.StartSessionParams{
			WorkspacePath:   dir,
			AgentConfig:     domain.AgentConfig{Command: writePiScript(t, dir, "")},
			ResumeSessionID: resumeID,
		})
		if err == nil {
			t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })
		}
		return session, err
	})
}

func TestStartSession_SessionOfAnotherProjectIsNotResumed(t *testing.T) {
	dir := t.TempDir()
	stageHeldSession(t, t.TempDir(), "7a2b3c4d")
	argvFile := filepath.Join(dir, "argv.txt")
	script := writePiScript(t, dir, `printf '%s\n' "$@" > '`+argvFile+`'
printf '{"type":"session","version":3,"id":"fresh1","timestamp":"t","cwd":"%s"}\n' "$PWD"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"new"}],"provider":"p","model":"m","stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0}}}' '{"type":"agent_end","messages":[],"willRetry":false}'`)
	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   dir,
		AgentConfig:     domain.AgentConfig{Command: script},
		ResumeSessionID: "7a2b3c4d",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if session.CarriedHistory || session.ID != "" {
		t.Errorf("session = {CarriedHistory: %v, ID: %q}, want {false, %q}", session.CarriedHistory, session.ID, "")
	}

	if _, _, err := collectEvents(t, a, session, "go"); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	if strings.Contains(string(argv), "--session") {
		t.Errorf("argv = %q, want no --session", argv)
	}
}

func TestRunTurn_ResumeForwardsSessionID(t *testing.T) {
	dir := t.TempDir()
	stageHeldSession(t, dir, "7a2b3c4d")
	argvFile := filepath.Join(dir, "argv.txt")
	script := writePiScript(t, dir, `printf '%s\n' "$@" > '`+argvFile+`'
printf '{"type":"session","version":3,"id":"7a2b3c4d","timestamp":"t","cwd":"%s"}\n' "$PWD"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"resumed"}],"provider":"p","model":"m","stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0}}}' '{"type":"agent_end","messages":[],"willRetry":false}'`)
	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   dir,
		AgentConfig:     domain.AgentConfig{Command: script},
		ResumeSessionID: "7a2b3c4d",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	events, _, err := collectEvents(t, a, session, "continue")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	if !strings.Contains(string(argv), "--session\n7a2b3c4d\n") {
		t.Errorf("argv = %q, want --session 7a2b3c4d", argv)
	}
	if !session.CarriedHistory {
		t.Errorf("session.CarriedHistory = false, want true")
	}
	started := eventsOfType(events, domain.EventSessionStarted)
	if len(started) != 1 || started[0].SessionID != "7a2b3c4d" {
		t.Errorf("session_started = %+v, want one for the resumed id", started)
	}
}

func TestRunTurn_SessionIDMismatchFailsTheTurn(t *testing.T) {
	dir := t.TempDir()
	stageHeldSession(t, dir, "expected")
	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   dir,
		AgentConfig:     domain.AgentConfig{Command: writeRunFixtureScript(t, dir, "simple_turn.jsonl")},
		ResumeSessionID: "expected",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	events, result, err := collectEvents(t, a, session, "work")
	if err == nil || agentErrorOf(t, err).Kind != domain.ErrResponseError {
		t.Fatalf("RunTurn() error = %v, want response_error", err)
	}
	if result.ExitReason != domain.EventTurnFailed || len(eventsOfType(events, domain.EventSessionStarted)) != 0 {
		t.Errorf("result = %+v, events = %+v, want a failed turn without session_started", result, events)
	}
}

func TestRunTurn_HeaderWorkspaceMismatchFailsTheTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	script := writePiScript(t, dir, `printf '{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"%s"}\n' '`+other+`'
sleep 30`)
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, script)

	_, _, err := collectEvents(t, a, session, "work")
	if err == nil || !strings.Contains(agentErrorOf(t, err).Message, "want the turn's workspace") {
		t.Fatalf("RunTurn() error = %v, want a workspace mismatch", err)
	}
}

func TestRunTurn_PromptIsDeliveredOnStandardInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	script := writePiScript(t, dir, `cat > '`+promptFile+`'
printf '{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"%s"}\n' "$PWD"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[],"willRetry":false}'`)
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, script)

	const prompt = "@/etc/passwd\n--help\n'quoted' $(not run)"
	if _, _, err := collectEvents(t, a, session, prompt); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	got, _ := os.ReadFile(promptFile)
	if string(got) != prompt {
		t.Errorf("stdin = %q, want %q", got, prompt)
	}
}

func TestRunTurn_RemoteLaunch(t *testing.T) {
	// Not parallel: puts a fake ssh client first on PATH.
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "ssh-argv.txt")
	agenttest.WriteScript(t, dir, "ssh", `case "$*" in
  *--version*) echo '0.99.0'; exit 0;;
esac
printf '%s\n' "$@" > '`+argvFile+`'
cat > /dev/null
printf '%s\n' '{"type":"session","version":3,"id":"remote-1","timestamp":"t","cwd":"/private/remote/workspace"}' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop"}}' '{"type":"agent_end","messages":[],"willRetry":false}'`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: "pi"},
		SSHHost:       "worker.example",
		Settings:      map[string]any{"model": "anthropic/claude-sonnet-4-5"},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	events, _, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v: the remote cwd is not compared", err)
	}
	if len(eventsOfType(events, domain.EventSessionStarted)) != 1 {
		t.Errorf("events = %+v, want one session_started", events)
	}
	argv, _ := os.ReadFile(argvFile)
	for _, want := range []string{"worker.example", dir, "--mode", "json", "--model", "--no-approve"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("ssh argv = %q, want it to contain %q", argv, want)
		}
	}
}

func TestRunTurn_NoOutputWithinReadTimeoutFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: writePiScript(t, dir, "sleep 30"), ReadTimeoutMS: 200},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	_, _, err = collectEvents(t, a, session, "work")
	if err == nil || agentErrorOf(t, err).Kind != domain.ErrResponseTimeout {
		t.Fatalf("RunTurn() error = %v, want response_timeout", err)
	}
}

func TestRunTurn_UsageKeptWhenCancelledDuringATool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := writePiScript(t, dir, `printf '{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"%s"}\n' "$PWD"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"sleep 100"}}],"provider":"p","model":"m","responseId":"r1","stopReason":"toolUse","usage":{"input":50,"output":10,"cacheRead":0,"cacheWrite":0}}}' '{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"sleep 100"}}' '{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","partialResult":{}}'
sleep 30`)
	a, _ := NewPiAdapter()
	session := mustStartSession(t, a, dir, script)

	ctx, cancel := context.WithCancel(context.Background())
	var events []domain.AgentEvent
	result, err := a.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: "work",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
			if e.Type == domain.EventNotification && e.Message == "" {
				cancel()
			}
		},
	})
	cancel()
	if err == nil {
		t.Fatal("RunTurn() error = nil, want a cancellation")
	}
	if result.ExitReason != domain.EventTurnCancelled {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
	}
	if result.Usage.InputTokens != 50 || len(eventsOfType(events, domain.EventTokenUsage)) != 1 {
		t.Errorf("usage = %+v, token_usage events = %d, want the finished response's usage", result.Usage, len(eventsOfType(events, domain.EventTokenUsage)))
	}
}

func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("pi")
	if !ok {
		t.Fatal(`registry.Agents.Meta("pi") reported not registered`)
	}
	if declared.MCPDialect != registry.MCPDialectNone {
		t.Fatalf("registered MCPDialect = %q, want %q", declared.MCPDialect, registry.MCPDialectNone)
	}

	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	const generatedConfig = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server","--workflow","/repo/WORKFLOW.md"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`
	if err := os.WriteFile(mcpConfigPath, []byte(generatedConfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	argvFile := filepath.Join(dir, "argv.txt")
	envFile := filepath.Join(dir, "env.txt")
	script := writePiScript(t, dir, `printf '%s\n' "$@" > `+argvFile+`
env > `+envFile+`
exit 0`)

	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: script},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	_, _, _ = collectEvents(t, a, session, "work")

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv) error = %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ReadFile(env) error = %v", err)
	}
	agenttest.AssertMCPInjection(t, declared.MCPDialect, mcpConfigPath, agenttest.MCPLaunchSurface{
		Args: strings.Split(strings.TrimRight(string(argv), "\n"), "\n"),
		Env:  strings.Split(strings.TrimRight(string(env), "\n"), "\n"),
	})
}

func TestReducedVerification_EmptyToolListFollowsTheConfiguredCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	script := agenttest.WriteScript(t, dir, "fake-pi", `case "$*" in
  *--version) echo '0.99.0'; exit 0;;
esac
printf '%s\n' "$@" > `+argvFile+`
exit 0`)

	a, _ := NewPiAdapter()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          dir,
		AgentConfig:            domain.AgentConfig{Command: script + " --tools read"},
		CredentialVerification: true,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	_, _, _ = collectEvents(t, a, session, "work")

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv) error = %v", err)
	}
	args := strings.Split(strings.TrimRight(string(argv), "\n"), "\n")
	last := -1
	for i, arg := range args {
		if arg == "--tools" {
			last = i
		}
	}
	if last < 0 || last+1 >= len(args) || args[last+1] != "" || args[0] != "--tools" {
		t.Errorf("argv = %q, want the configured --tools list first and an empty --tools list after it", args)
	}
}

func TestStageHeldSession_IgnoresTheAmbientAgentDir(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", ambient)

	stageHeldSession(t, t.TempDir(), "7a2b3c4d")

	entries, err := os.ReadDir(ambient)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", ambient, err)
	}
	if len(entries) != 0 {
		t.Errorf("ambient agent dir holds %d entries, want none: the fixture must use its own directory", len(entries))
	}
}
