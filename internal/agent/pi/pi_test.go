package pi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// writePiScript writes an executable shell script named fake-pi in dir
// that runs the given body, and returns its path.
func writePiScript(t *testing.T, dir, body string) string {
	t.Helper()
	return agenttest.WriteScript(t, dir, "fake-pi", body)
}

// mustStartSession starts a session with the given command or fatals.
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

// collectEvents runs a turn and collects all emitted events.
func collectEvents(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult, error) {
	t.Helper()
	var events []domain.AgentEvent
	result, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	return events, result, err
}

// writeRunFixtureScript writes fixtureName into dir and returns a
// fake-pi script that replays it for any invocation. The fixtures carry
// the session header's cwd as the __CWD__ placeholder, which the script
// substitutes with the process's own working directory, because the
// adapter compares the header's cwd against the turn's workspace.
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

// eventsOfType returns every event of the given type.
func eventsOfType(events []domain.AgentEvent, typ domain.AgentEventType) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// notificationMessages returns the message of every notification event.
func notificationMessages(events []domain.AgentEvent) []string {
	var out []string
	for _, e := range eventsOfType(events, domain.EventNotification) {
		out = append(out, e.Message)
	}
	return out
}

func TestRunTurn_SimpleText(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "simple_turn.jsonl"))
	events, result, err := collectEvents(t, a, session, "do work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// One notification per assistant response: the deltas, the
	// text_end, the message_end and the turn_end all describe the same
	// response, and exactly one of them may reach the orchestrator.
	got := notificationMessages(events)
	if len(got) != 1 || got[0] != "Hello, world!" {
		t.Errorf("notifications = %v, want exactly [\"Hello, world!\"]", got)
	}
	if len(eventsOfType(events, domain.EventMalformed)) != 0 {
		t.Errorf("malformed events = %+v, want none", eventsOfType(events, domain.EventMalformed))
	}
	if result.Usage.InputTokens != 110 || result.Usage.OutputTokens != 20 || result.Usage.CacheReadTokens != 10 {
		t.Errorf("usage = %+v, want input 110 output 20 cacheRead 10", result.Usage)
	}
	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}
}

// TestRunTurn_ToolResultPerExecution asserts one tool result per
// tool_execution_end, carrying that event's top-level isError flag, and
// no duplicate from the toolcall deltas that precede it.
func TestRunTurn_ToolResultPerExecution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture      string
		wantTool     string
		wantToolErr  bool
		wantTexts    []string
		wantUsageSum domain.TokenUsage
	}{
		{
			fixture:     "tool_success.jsonl",
			wantTool:    "write",
			wantToolErr: false,
			wantTexts:   []string{"done"},
			// Two model responses: 50+10 then 40+8.
			wantUsageSum: domain.TokenUsage{InputTokens: 90, OutputTokens: 18, TotalTokens: 108},
		},
		{
			fixture:      "tool_error.jsonl",
			wantTool:     "bash",
			wantToolErr:  true,
			wantTexts:    []string{"the command failed"},
			wantUsageSum: domain.TokenUsage{InputTokens: 95, OutputTokens: 22, TotalTokens: 117},
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			a, err := NewPiAdapter()
			if err != nil {
				t.Fatalf("NewPiAdapter() error = %v", err)
			}
			session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, tt.fixture))
			events, result, err := collectEvents(t, a, session, "work")
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}

			tools := eventsOfType(events, domain.EventToolResult)
			if len(tools) != 1 {
				t.Fatalf("tool results = %+v, want exactly 1", tools)
			}
			if tools[0].ToolName != tt.wantTool || tools[0].ToolError != tt.wantToolErr {
				t.Errorf("tool result = %+v, want name %q ToolError %t", tools[0], tt.wantTool, tt.wantToolErr)
			}

			got := notificationMessages(events)
			if strings.Join(got, "|") != strings.Join(tt.wantTexts, "|") {
				t.Errorf("notifications = %v, want %v", got, tt.wantTexts)
			}
			if result.Usage != tt.wantUsageSum {
				t.Errorf("usage = %+v, want %+v: every model response in the turn is summed once", result.Usage, tt.wantUsageSum)
			}
		})
	}
}

// TestRunTurn_AssistantErrorAbortsExitZero pins the exit-code gap the
// pi CLI has: --mode json applies no stop-reason check, so a
// provider failure and an abort both exit 0. Neither may be recorded as
// a successful turn, and the failure's stderr reaches the terminal
// report as a bounded excerpt.
func TestRunTurn_AssistantErrorAbortsExitZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture   string
		wantInMsg string
		wantTexts []string
	}{
		{fixture: "assistant_error.jsonl", wantInMsg: "upstream 529 overloaded", wantTexts: []string{"I will start "}},
		{fixture: "assistant_aborted.jsonl", wantInMsg: "aborted"},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fixture := filepath.Join(dir, tt.fixture)
			data, err := os.ReadFile(filepath.Join("testdata", tt.fixture))
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if err := os.WriteFile(fixture, data, 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			script := writePiScript(t, dir, `echo "request failed: upstream said no" >&2
sed "s|__CWD__|$PWD|g" '`+fixture+`'`)

			a, err := NewPiAdapter()
			if err != nil {
				t.Fatalf("NewPiAdapter() error = %v", err)
			}
			session := mustStartSession(t, a, dir, script)
			events, result, err := collectEvents(t, a, session, "work")

			if err == nil {
				t.Fatal("RunTurn() error = nil, want a failure: pi --mode json exits 0 for a failed response")
			}
			agentErr, ok := err.(*domain.AgentError)
			if !ok {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != domain.ErrResponseError {
				t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
			}
			if !strings.Contains(agentErr.Message, tt.wantInMsg) {
				t.Errorf("message = %q, want it to contain %q", agentErr.Message, tt.wantInMsg)
			}
			if !strings.Contains(agentErr.Message, "request failed: upstream said no") {
				t.Errorf("message = %q, want the bounded stderr excerpt in the terminal failure evidence", agentErr.Message)
			}
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			// The partial text is reported once, as the response's own
			// completed text, before the failure is recorded.
			if got := notificationMessages(events); strings.Join(got, "|") != strings.Join(tt.wantTexts, "|") {
				t.Errorf("notifications = %v, want %v", got, tt.wantTexts)
			}
		})
	}
}

// TestRunTurn_ExitZeroNoWorkIsFailure pins the shared zero-exit,
// no-success rule for a pi run that produced no assistant output and no
// tool activity at all.
func TestRunTurn_ExitZeroNoWorkIsFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session := mustStartSession(t, a, dir, writeInlineRunScript(t, dir, `{"type":"agent_start"}`, `{"type":"turn_start"}`, `{"type":"agent_end","messages":[],"willRetry":false}`))
	_, result, err := collectEvents(t, a, session, "work")

	if err == nil {
		t.Fatal("RunTurn() error = nil, want a zero-work failure")
	}
	agentcoreT := agentcore.TurnEvidence{ExitObserved: true, Work: agentcore.WorkAbsent}
	if result.ExitReason != agentcore.DecideTurn(agentcoreT).ExitReason {
		t.Errorf("ExitReason = %q, want the shared zero-work row %q", result.ExitReason, agentcore.DecideTurn(agentcoreT).ExitReason)
	}
}

// TestRunTurn_UsageAccumulatesAcrossTurns asserts the session's
// run-cumulative figure grows monotonically across successive turns.
func TestRunTurn_UsageAccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "simple_turn.jsonl"))

	first, resultOne, err := collectEvents(t, a, session, "one")
	if err != nil {
		t.Fatalf("first RunTurn() error = %v", err)
	}
	second, resultTwo, err := collectEvents(t, a, session, "two")
	if err != nil {
		t.Fatalf("second RunTurn() error = %v", err)
	}

	agenttest.AssertUsageContract(t, append(append([]domain.AgentEvent{}, first...), second...))
	if resultOne.Usage.InputTokens != 110 {
		t.Errorf("first usage = %+v, want input 110", resultOne.Usage)
	}
	if resultTwo.Usage.InputTokens != 220 || resultTwo.Usage.OutputTokens != 40 {
		t.Errorf("second usage = %+v, want the session total input 220 output 40", resultTwo.Usage)
	}
	if resultTwo.Usage.TotalTokens != 260 {
		t.Errorf("second TotalTokens = %d, want 260", resultTwo.Usage.TotalTokens)
	}
}

// TestRunTurn_ResumeSession resumes into a session whose header names
// the id Sortie already held, and reports session_started once.
func TestRunTurn_ResumeSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   dir,
		AgentConfig:     domain.AgentConfig{Command: writeRunFixtureScript(t, dir, "resume_turn.jsonl")},
		ResumeSessionID: "7a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	events, _, err := collectEvents(t, a, session, "continue")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	started := eventsOfType(events, domain.EventSessionStarted)
	if len(started) != 1 {
		t.Fatalf("session_started count = %d, want 1: %+v", len(started), started)
	}
	if started[0].SessionID != "7a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Errorf("session_started SessionID = %q, want the resumed id", started[0].SessionID)
	}
	if got := notificationMessages(events); len(got) != 1 || got[0] != "resumed" {
		t.Errorf("notifications = %v, want [\"resumed\"]", got)
	}
}

// TestAssertUsageReporting proves pi's registered usage-reporting
// declaration (turn_end, per_model) against a real event stream: one
// cumulative usage figure, emitted after the turn's work, carrying the
// model that produced it.
func TestAssertUsageReporting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session := mustStartSession(t, a, dir, writeRunFixtureScript(t, dir, "tool_success.jsonl"))
	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	agenttest.AssertUsageReporting(t, "pi", []agenttest.UsageReportingCase{
		{Name: "one figure after the tool result", Events: events, Result: result},
	})
	agenttest.AssertModelReported(t, events, "claude-sonnet-4-5")
}

func TestRunTurn_NonZeroExitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	script := writePiScript(t, dir, `printf '{"type":"session","version":3,"id":"s1","timestamp":"t","cwd":"%s"}\n' "$PWD"; exit 3`)
	session := mustStartSession(t, a, dir, script)
	_, result, err := collectEvents(t, a, session, "do work")
	if err == nil {
		t.Fatal("RunTurn() error = nil, want port_exit failure")
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
}

// writeInlineRunScript writes lines as a run stream in dir and returns a
// fake-pi that replays it, with the session header's __CWD__ placeholder
// resolved to the process's own working directory.
func writeInlineRunScript(t *testing.T, dir string, lines ...string) string {
	t.Helper()

	path := filepath.Join(dir, "stream.jsonl")
	body := "{\"type\":\"session\",\"version\":3,\"id\":\"stream-1\",\"timestamp\":\"t\",\"cwd\":\"__CWD__\"}\n" +
		strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return writePiScript(t, dir, `sed "s|__CWD__|$PWD|g" '`+path+`'`)
}

// assistantMessage returns the completed-message JSON of one assistant
// response carrying text and a usage figure.
func assistantMessage(text string, input, output int) string {
	return fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":%q}],`+
		`"api":"anthropic-messages","provider":"anthropic","model":"m1",`+
		`"usage":{"input":%d,"output":%d,"cacheRead":0,"cacheWrite":0},"stopReason":"stop"}`,
		text, input, output)
}

// TestRunTurn_SpendMarks pins which stream events mark the turn as having
// reached the model and which settle it, through the spend flags the
// shared rule writes onto the result.
func TestRunTurn_SpendMarks(t *testing.T) {
	t.Parallel()

	msg := assistantMessage("done", 10, 2)
	noUsage := `{"role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m1","stopReason":"stop"}`
	userMsg := `{"role":"user","content":[{"type":"text","text":"go"}]}`

	tests := []struct {
		name            string
		lines           []string
		wantUnaccounted bool
		wantCoverable   bool
	}{
		{
			name:  "text delta reaches the model",
			lines: []string{`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"}}`},

			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name:            "thinking delta reaches the model",
			lines:           []string{`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"x"}}`},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name:            "tool call reaches the model",
			lines:           []string{`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","contentIndex":0}}`},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name: "tool result reaches the model",
			lines: []string{
				`{"type":"tool_execution_start","toolCallId":"c1","toolName":"ls","args":{}}`,
				`{"type":"tool_execution_end","toolCallId":"c1","toolName":"ls","result":{"content":[]},"isError":false}`,
			},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name:            "assistant message_end without usage reaches the model",
			lines:           []string{`{"type":"message_end","message":` + noUsage + `}`},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name: "usage figure on turn_end reaches the model",
			lines: []string{
				`{"type":"turn_end","message":` + msg + `,"toolResults":[]}`,
			},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name: "agent_end without willRetry settles the turn",
			lines: []string{
				`{"type":"message_end","message":` + msg + `}`,
				`{"type":"turn_end","message":` + msg + `,"toolResults":[]}`,
				`{"type":"agent_end","messages":[` + msg + `],"willRetry":false}`,
			},
		},
		{
			name: "agent_end that will retry leaves the turn unsettled",
			lines: []string{
				`{"type":"message_end","message":` + msg + `}`,
				`{"type":"turn_end","message":` + msg + `,"toolResults":[]}`,
				`{"type":"agent_end","messages":[` + msg + `],"willRetry":true}`,
			},
			wantUnaccounted: true,
			wantCoverable:   true,
		},
		{
			name: "user message and framing never reach the model",
			lines: []string{
				`{"type":"agent_start"}`,
				`{"type":"turn_start"}`,
				`{"type":"message_start","message":` + userMsg + `}`,
				`{"type":"message_end","message":` + userMsg + `}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			a, err := NewPiAdapter()
			if err != nil {
				t.Fatalf("NewPiAdapter() error = %v", err)
			}
			session := mustStartSession(t, a, dir, writeInlineRunScript(t, dir, tt.lines...))
			_, result, _ := collectEvents(t, a, session, "work")

			if result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("SpendUnaccounted = %v, want %v", result.SpendUnaccounted, tt.wantUnaccounted)
			}
			if result.SpendCoverable != tt.wantCoverable {
				t.Errorf("SpendCoverable = %v, want %v", result.SpendCoverable, tt.wantCoverable)
			}
		})
	}
}
