package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// turnRun drives a session state's hooks the way the skeleton does for one
// turn, without a subprocess.
type turnRun struct {
	t      *testing.T
	state  *sessionState
	events []domain.AgentEvent
	spend  agentcore.TurnSpend
	last   any
	errs   []error
}

func newTurnRun(t *testing.T) *turnRun {
	t.Helper()
	return newTurnRunIn(t, t.TempDir())
}

func newTurnRunIn(t *testing.T, workspace string) *turnRun {
	t.Helper()
	ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
	if err != nil {
		t.Fatalf("NewLedger() error = %v", err)
	}
	ledger.SessionCreated()
	state := &sessionState{
		target:     agentcore.LaunchTarget{WorkspacePath: workspace},
		baseLogger: testLogger(),
		usage:      agentcore.NewRunUsage(),
		ledger:     ledger,
	}
	state.resetTurn()
	return &turnRun{t: t, state: state}
}

func (r *turnRun) emit(e domain.AgentEvent) { r.events = append(r.events, e) }

// feed passes each line to the ParseLine hook and remembers the last
// terminal result and every error.
func (r *turnRun) feed(lines ...string) {
	r.t.Helper()
	for _, line := range lines {
		result, err := r.state.parseLine([]byte(line), r.emit, "", &r.spend)
		if err != nil {
			r.errs = append(r.errs, err)
			continue
		}
		if result != nil {
			r.last = result
		}
	}
}

// finish runs the finalize hook for a process that exited with code 0.
func (r *turnRun) finish() (domain.TurnResult, *domain.AgentError) {
	r.t.Helper()
	r.state.settleUsage(context.Background(), r.emit, &r.spend, true)
	return r.state.finalizeTurn(r.emit, r.last, 0, nil, nil, nil, &r.spend)
}

func (r *turnRun) ofType(typ domain.AgentEventType) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *turnRun) notifications() []string {
	var out []string
	for _, e := range r.ofType(domain.EventNotification) {
		out = append(out, e.Message)
	}
	return out
}

func (r *turnRun) assertNoMalformed() {
	r.t.Helper()
	if got := r.ofType(domain.EventMalformed); len(got) != 0 {
		r.t.Errorf("malformed events = %+v, want none", got)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func sessionLine(t *testing.T, id, cwd string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": "t", "cwd": cwd})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(b)
}

type responseSpec struct {
	id, provider, model, stop, errMessage, text string
	input, output, cacheRead, cacheWrite        int64
	noUsage                                     bool
	emptyThinking                               bool
}

func (r responseSpec) json() string {
	if r.provider == "" {
		r.provider = "anthropic"
	}
	if r.model == "" {
		r.model = "m1"
	}
	if r.stop == "" {
		r.stop = "stop"
	}
	content := []map[string]any{{"type": "text", "text": r.text}}
	if r.emptyThinking {
		content = append(content, map[string]any{"type": "thinking", "thinking": ""})
	}
	msg := map[string]any{
		"role":       "assistant",
		"content":    content,
		"provider":   r.provider,
		"model":      r.model,
		"stopReason": r.stop,
	}
	if r.id == "" {
		r.id = fmt.Sprintf("%s/%s/%s/%s/%d/%d/%d/%d", r.provider, r.model, r.text, r.errMessage, r.input, r.output, r.cacheRead, r.cacheWrite)
	}
	msg["responseId"] = r.id
	if r.errMessage != "" {
		msg["errorMessage"] = r.errMessage
	}
	if !r.noUsage {
		msg["usage"] = map[string]any{"input": r.input, "output": r.output, "cacheRead": r.cacheRead, "cacheWrite": r.cacheWrite}
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

func messageEnd(r responseSpec) string {
	return `{"type":"message_end","message":` + r.json() + `}`
}

func turnEnd(r responseSpec) string {
	return `{"type":"turn_end","message":` + r.json() + `,"toolResults":[]}`
}

const agentEnd = `{"type":"agent_end","messages":[],"willRetry":false}`

func TestParseLine_UndecodableLineIsAnError(t *testing.T) {
	t.Parallel()
	r := newTurnRun(t)
	r.feed("pi: warming up", `{"cwd":"/ws"}`)
	if len(r.errs) != 2 {
		t.Fatalf("errors = %v, want one per undecodable line", r.errs)
	}
	if len(r.events) != 0 {
		t.Errorf("events = %+v, want none: the skeleton reports the malformed line", r.events)
	}
}

func TestParseLine_SessionHeader(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(sessionLine(t, "s1", r.state.target.WorkspacePath))
	if len(r.errs) != 0 {
		t.Fatalf("errors = %v, want none", r.errs)
	}
	started := r.ofType(domain.EventSessionStarted)
	if len(started) != 1 || started[0].SessionID != "s1" {
		t.Errorf("session_started = %+v, want one for s1", started)
	}
	if got := r.state.currentSessionID(); got != "s1" {
		t.Errorf("session id = %q, want s1", got)
	}
}

func TestParseLine_SessionStartedOncePerSession(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	header := sessionLine(t, "s1", r.state.target.WorkspacePath)
	r.feed(header, header)
	r.state.resetTurn()
	r.feed(header)
	if got := len(r.ofType(domain.EventSessionStarted)); got != 1 {
		t.Errorf("session_started count = %d, want 1", got)
	}
}

func TestParseLine_ResumedSessionIDIsKept(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.state.sessionID = "resumed"
	r.feed(sessionLine(t, "resumed", r.state.target.WorkspacePath))
	if len(r.errs) != 0 {
		t.Fatalf("errors = %v, want none", r.errs)
	}
	started := r.ofType(domain.EventSessionStarted)
	if len(started) != 1 || started[0].SessionID != "resumed" {
		t.Errorf("session_started = %+v, want one for the resumed id", started)
	}
}

func TestParseLine_SessionIDMismatchEndsTurn(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.state.sessionID = "expected"
	r.feed(sessionLine(t, "other", r.state.target.WorkspacePath))
	if len(r.errs) != 1 || !strings.Contains(r.errs[0].Error(), `expected "expected", got "other"`) {
		t.Fatalf("errors = %v, want a session id mismatch", r.errs)
	}
	if got := r.state.currentSessionID(); got != "expected" {
		t.Errorf("session id = %q, want it unchanged", got)
	}
	if len(r.ofType(domain.EventSessionStarted)) != 0 {
		t.Error("session_started emitted for a mismatched header")
	}
}

func TestParseLine_HeaderWorkspaceMismatchEndsTurn(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(sessionLine(t, "s1", t.TempDir()))
	if len(r.errs) != 1 || !strings.Contains(r.errs[0].Error(), "want the turn's workspace") {
		t.Fatalf("errors = %v, want a workspace mismatch", r.errs)
	}
	if len(r.ofType(domain.EventSessionStarted)) != 0 {
		t.Error("session_started emitted for a mismatched header")
	}
}

func TestParseLine_HeaderCwdMissingOnDiskEndsTurn(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(sessionLine(t, "s1", filepath.Join(r.state.target.WorkspacePath, "gone")))
	if len(r.errs) != 1 {
		t.Fatalf("errors = %v, want a workspace mismatch", r.errs)
	}
}

func TestParseLine_HeaderCwdThroughSymlinkedParent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "ws"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tests := []struct{ name, workspace, cwd string }{
		{name: "pi reports the physical path", workspace: filepath.Join(link, "ws"), cwd: filepath.Join(real, "ws")},
		{name: "pi reports the linked path", workspace: filepath.Join(real, "ws"), cwd: filepath.Join(link, "ws")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRunIn(t, tt.workspace)
			r.feed(sessionLine(t, "s1", tt.cwd))
			if len(r.errs) != 0 {
				t.Fatalf("errors = %v, want none: both paths are the same directory", r.errs)
			}
		})
	}
}

func TestParseLine_RemoteLaunchSkipsCwdComparison(t *testing.T) {
	t.Parallel()

	r := newTurnRunIn(t, "/remote/workspace")
	r.state.target.RemoteCommand = "pi"
	r.state.target.SSHHost = "host"

	r.feed(sessionLine(t, "s1", "/private/remote/workspace"))
	if len(r.errs) != 0 {
		t.Fatalf("errors = %v, want none: the remote path cannot be inspected", r.errs)
	}

	r.feed(sessionLine(t, "s2", "/remote/workspace"))
	if len(r.errs) != 1 || !strings.Contains(r.errs[0].Error(), "session id mismatch") {
		t.Errorf("errors = %v, want the session id check to still apply", r.errs)
	}
}

func TestParseLine_NonAssistantMessagesAreNotRead(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"message_end","message":{"role":"user","content":"write hello.txt"}}`,
		`{"type":"turn_end","message":{"role":"user","content":"write hello.txt"},"toolResults":[]}`,
		`{"type":"message_end","message":{"role":"system","content":"be brief"}}`,
	)
	r.assertNoMalformed()
	if len(r.errs) != 0 || len(r.events) != 0 {
		t.Errorf("errors = %v, events = %+v, want none", r.errs, r.events)
	}
}

func TestParseLine_LivenessNotifications(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":"x"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"toolcall_delta","contentIndex":0}}`,
		`{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","partialResult":{}}`,
		`{"type":"compaction_start","reason":"threshold"}`,
		`{"type":"compaction_end","reason":"threshold"}`,
		`{"type":"auto_retry_start"}`,
		`{"type":"auto_retry_end"}`,
	} {
		r := newTurnRun(t)
		r.feed(line)
		r.assertNoMalformed()
		got := r.ofType(domain.EventNotification)
		if len(got) != 1 || got[0].Message != "" {
			t.Errorf("line %s: notifications = %+v, want one empty liveness notification", line, got)
		}
	}
}

func TestParseLine_AssistantTextIsReportedOnce(t *testing.T) {
	t.Parallel()

	resp := responseSpec{text: "Hello", input: 10, output: 2}
	r := newTurnRun(t)
	r.feed(
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hello"}}`,
		messageEnd(resp),
		turnEnd(resp),
		agentEnd,
	)
	r.assertNoMalformed()
	var texts []string
	for _, m := range r.notifications() {
		if m != "" {
			texts = append(texts, m)
		}
	}
	if len(texts) != 1 || texts[0] != "Hello" {
		t.Errorf("text notifications = %v, want [Hello]", texts)
	}
}

func TestParseLine_TurnEndIsTheFallbackWithoutMessageEnd(t *testing.T) {
	t.Parallel()

	resp := responseSpec{text: "Hello", input: 10, output: 2}
	r := newTurnRun(t)
	r.feed(`{"type":"turn_start"}`, turnEnd(resp), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if got := r.notifications(); len(got) != 2 || got[1] != "Hello" {
		t.Errorf("notifications = %v, want the turn_end text once", got)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want input 10 output 2", result.Usage)
	}
	if got := r.ofType(domain.EventTokenUsage); len(got) != 1 {
		t.Errorf("token_usage events = %d, want one from the turn_end fallback", len(got))
	}
}

func TestParseLine_MalformedPayloads(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, line string }{
		{name: "unknown event type", line: `{"type":"future_event"}`},
		{name: "unknown delta variant", line: `{"type":"message_update","assistantMessageEvent":{"type":"future_delta"}}`},
		{name: "message_update without a delta", line: `{"type":"message_update"}`},
		{name: "message_end without a message", line: `{"type":"message_end"}`},
		{name: "assistant content is not a list", line: `{"type":"message_end","message":{"role":"assistant","content":"x"}}`},
		{name: "session header without an id", line: `{"type":"session","cwd":"/ws"}`},
		{name: "tool_execution_end without a name", line: `{"type":"tool_execution_end","toolCallId":"c1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(tt.line)
			if len(r.errs) != 0 {
				t.Fatalf("errors = %v, want none: a payload fault is reported as an event", r.errs)
			}
			if got := r.ofType(domain.EventMalformed); len(got) != 1 {
				t.Errorf("malformed events = %+v, want exactly one", got)
			}
		})
	}
}

func TestParseLine_KnownFramingEventsAreSilent(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"agent_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"queue_update"}`,
		`{"type":"agent_settled"}`,
	)
	if len(r.events) != 0 || len(r.errs) != 0 {
		t.Errorf("events = %+v, errors = %v, want none", r.events, r.errs)
	}
}

func TestParseLine_ToolResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		tool         string
		args         string
		wantKind     domain.ToolKind
		wantArgument string
	}{
		{name: "bash", tool: "bash", args: `{"command":"ls -la"}`, wantKind: domain.ToolKindShell, wantArgument: "ls -la"},
		{name: "read", tool: "read", args: `{"path":"a.txt"}`, wantKind: domain.ToolKindRead, wantArgument: "a.txt"},
		{name: "edit", tool: "edit", args: `{"path":"a.txt","edits":[]}`, wantKind: domain.ToolKindEdit, wantArgument: "a.txt"},
		{name: "write", tool: "write", args: `{"path":"b.txt","content":"x"}`, wantKind: domain.ToolKindEdit, wantArgument: "b.txt"},
		{name: "unmapped tool", tool: "grep", args: `{"pattern":"x"}`},
		{name: "missing args", tool: "bash", wantKind: domain.ToolKindShell},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start := fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":"c1","toolName":%q`, tt.tool)
			if tt.args != "" {
				start += `,"args":` + tt.args
			}
			r := newTurnRun(t)
			r.feed(start+"}", fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":"c1","toolName":%q,"result":{"content":[]},"isError":false}`, tt.tool))
			r.assertNoMalformed()
			got := r.ofType(domain.EventToolResult)
			if len(got) != 1 {
				t.Fatalf("tool results = %+v, want one", got)
			}
			if got[0].ToolName != tt.tool || got[0].ToolKind != tt.wantKind || got[0].ToolArgument != tt.wantArgument {
				t.Errorf("tool result = name %q kind %q argument %q, want %q %q %q",
					got[0].ToolName, got[0].ToolKind, got[0].ToolArgument, tt.tool, tt.wantKind, tt.wantArgument)
			}
		})
	}
}

func TestParseLine_ToolResultDurationAndError(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"x"}}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","durationMs":1234,"result":{"content":[{"type":"text","text":"boom"}]},"isError":true}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"bash","args":{"command":"y"}}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"bash","result":{"content":[]},"isError":false}`,
		`{"type":"tool_execution_end","toolCallId":"never-started","toolName":"read","result":{"content":[]},"isError":false}`,
	)
	got := r.ofType(domain.EventToolResult)
	if len(got) != 3 {
		t.Fatalf("tool results = %+v, want three", got)
	}
	if got[0].ToolDurationMS != 1234 || !got[0].ToolError || got[0].Message != "boom" {
		t.Errorf("first result = %+v, want the reported duration, the error flag and its text", got[0])
	}
	if got[1].ToolDurationMS < 0 || got[1].ToolError || got[1].Message != "" {
		t.Errorf("second result = %+v, want a tracked duration and no error", got[1])
	}
	if got[2].ToolName != "read" || got[2].ToolDurationMS != 0 {
		t.Errorf("third result = %+v, want the name and no duration", got[2])
	}
}

func TestParseLine_AgentEndWithRetryPendingIsNotTerminal(t *testing.T) {
	t.Parallel()

	failed := responseSpec{stop: "error", errMessage: "overloaded", input: 5, output: 1}
	ok := responseSpec{text: "done", input: 5, output: 2}
	r := newTurnRun(t)
	r.feed(
		messageEnd(failed),
		`{"type":"agent_end","messages":[],"willRetry":true}`,
	)
	if r.last != nil {
		t.Fatal("agent_end with willRetry produced a terminal result")
	}
	r.feed(messageEnd(ok), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v: only the last response decides the turn", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
}

// replayFixture feeds a recorded stream to the hooks, with the header's cwd
// placeholder resolved to the turn's workspace.
func replayFixture(t *testing.T, name string) (*turnRun, domain.TurnResult, *domain.AgentError) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", name, err)
	}
	r := newTurnRun(t)
	cwd, _ := json.Marshal(r.state.target.WorkspacePath)
	text := strings.ReplaceAll(string(data), `"__CWD__"`, string(cwd))
	r.feed(strings.Split(strings.TrimSpace(text), "\n")...)
	if len(r.errs) != 0 {
		t.Fatalf("%s: parse errors = %v", name, r.errs)
	}
	r.assertNoMalformed()
	result, agentErr := r.finish()
	return r, result, agentErr
}

func TestReplay_RecordedStreams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture     string
		wantFailure string
		wantInput   int64
		wantOutput  int64
		wantUsages  int
		wantText    string
		wantTool    string
		wantModel   string
	}{
		// The gateway's requested model is "cc/claude-haiku-4-5-20251001"; the provider served the response model.
		{fixture: "real_simple_turn.jsonl", wantInput: 29442, wantOutput: 4, wantUsages: 1, wantText: "OK", wantModel: "9router/claude-haiku-4-5-20251001"},
		{fixture: "real_tool_turn.jsonl", wantTool: "read", wantUsages: 2, wantModel: "9router/claude-haiku-4-5-20251001"},
		// The first response fails with zero counts and reports no figure; the retry succeeds.
		{fixture: "retry_then_success.jsonl", wantInput: 29442, wantOutput: 4, wantUsages: 1, wantText: "OK"},
		{fixture: "real_retry_error.jsonl", wantFailure: "server_error"},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			r, result, agentErr := replayFixture(t, tt.fixture)
			if tt.wantFailure != "" {
				if agentErr == nil || agentErr.Kind != domain.ErrTurnFailed || !strings.Contains(agentErr.Message, tt.wantFailure) {
					t.Fatalf("error = %v, want turn_failed containing %q", agentErr, tt.wantFailure)
				}
				if result.SpendUnaccounted {
					t.Errorf("result = %+v, want the spend accounted: every failed request reported zero counts", result)
				}
				return
			}
			if agentErr != nil {
				t.Fatalf("error = %v, want a completed turn", agentErr)
			}
			if tt.wantInput != 0 && (result.Usage.InputTokens != tt.wantInput || result.Usage.OutputTokens != tt.wantOutput) {
				t.Errorf("usage = %+v, want input %d output %d", result.Usage, tt.wantInput, tt.wantOutput)
			}
			if got := len(r.ofType(domain.EventTokenUsage)); got != tt.wantUsages {
				t.Errorf("token_usage events = %d, want %d, one per response", got, tt.wantUsages)
			}
			for _, e := range r.ofType(domain.EventTokenUsage) {
				if tt.wantModel != "" && e.Model != tt.wantModel {
					t.Errorf("token_usage model = %q, want %q", e.Model, tt.wantModel)
				}
			}
			if tt.wantText != "" {
				var texts []string
				for _, m := range r.notifications() {
					if m != "" {
						texts = append(texts, m)
					}
				}
				if len(texts) != 1 || texts[0] != tt.wantText {
					t.Errorf("text notifications = %v, want [%s]", texts, tt.wantText)
				}
			}
			if tt.wantTool != "" {
				tools := r.ofType(domain.EventToolResult)
				if len(tools) != 1 || tools[0].ToolName != tt.wantTool || tools[0].ToolKind != domain.ToolKindRead || tools[0].ToolArgument != "a.txt" {
					t.Errorf("tool results = %+v, want one %s call on a.txt", tools, tt.wantTool)
				}
			}
		})
	}
}

func TestRetry_FailedResponseThenSuccess(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		messageEnd(responseSpec{stop: "error", errMessage: "overloaded", input: 7, output: 1}),
		turnEnd(responseSpec{stop: "error", errMessage: "overloaded", input: 7, output: 1}),
		`{"type":"agent_end","messages":[],"willRetry":true}`,
		`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"overloaded"}`,
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		messageEnd(responseSpec{text: "OK", input: 20, output: 2}),
		turnEnd(responseSpec{text: "OK", input: 20, output: 2}),
		agentEnd,
		`{"type":"auto_retry_end","success":true,"attempt":2}`,
	)
	r.assertNoMalformed()
	result, err := r.finish()
	if err != nil || result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("finish() = (%+v, %v), want a completed turn: the retry's response decides", result, err)
	}
	if result.Usage.InputTokens != 27 || result.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want both responses summed: input 27 output 3", result.Usage)
	}
	events := r.ofType(domain.EventTokenUsage)
	if len(events) != 2 || events[0].Usage.InputTokens != 7 || events[1].Usage.InputTokens != 27 {
		t.Errorf("token_usage events = %+v, want one per request carrying the run total 7 then 27", events)
	}
}

func TestRetry_AllAttemptsFailReportsTheLastError(t *testing.T) {
	t.Parallel()

	var lines []string
	for i, message := range []string{"first failure", "second failure", "last failure"} {
		resp := responseSpec{stop: "error", errMessage: message, input: 5, output: 1}
		lines = append(lines,
			`{"type":"agent_start"}`, `{"type":"turn_start"}`, messageEnd(resp), turnEnd(resp),
			fmt.Sprintf(`{"type":"agent_end","messages":[],"willRetry":%t}`, i < 2))
		if i < 2 {
			lines = append(lines, `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"x"}`)
		}
	}
	lines = append(lines, `{"type":"auto_retry_end","success":false,"attempt":3}`)

	r := newTurnRun(t)
	r.feed(lines...)
	result, err := r.finish()
	if err == nil || err.Kind != domain.ErrTurnFailed || err.Message != "last failure" {
		t.Fatalf("error = %v, want turn_failed with the last error message", err)
	}
	if result.Usage.InputTokens != 15 {
		t.Errorf("usage = %+v, want all three responses summed", result.Usage)
	}
	if got := len(r.ofType(domain.EventTokenUsage)); got != 3 {
		t.Errorf("token_usage events = %d, want one per failed request", got)
	}
}

func TestFinalizeTurn_LongErrorMessageIsTruncated(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{stop: "error", errMessage: strings.Repeat("e", 5000), input: 5, output: 1}), agentEnd)
	_, err := r.finish()
	if err == nil || err.Kind != domain.ErrTurnFailed {
		t.Fatalf("error = %v, want turn_failed", err)
	}
	if got := len([]rune(err.Message)); got > 600 {
		t.Errorf("error message length = %d runes, want the provider text cut near 500", got)
	}
}

func TestFinalizeTurn_StopReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resp     responseSpec
		wantKind domain.AgentErrorKind
		wantMsg  string
	}{
		{name: "stop completes", resp: responseSpec{text: "ok"}},
		{name: "length is a token limit", resp: responseSpec{text: "cut", stop: "length"}, wantKind: domain.ErrTurnTokenLimit},
		{name: "error is a model failure", resp: responseSpec{stop: "error", errMessage: "upstream 529"}, wantKind: domain.ErrTurnFailed, wantMsg: "upstream 529"},
		{name: "error without a message", resp: responseSpec{stop: "error"}, wantKind: domain.ErrTurnFailed, wantMsg: "pi reported stop reason error"},
		{name: "aborted is a model failure", resp: responseSpec{stop: "aborted"}, wantKind: domain.ErrTurnFailed, wantMsg: "pi reported stop reason aborted"},
		{name: "unknown reason", resp: responseSpec{stop: "refusal"}, wantKind: domain.ErrTurnOutcomeUnknown, wantMsg: "refusal"},
		{name: "tool use ends the run on the exit-based outcome", resp: responseSpec{text: "calling", stop: "toolUse"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(messageEnd(tt.resp), agentEnd)
			result, err := r.finish()

			if tt.wantKind == "" {
				if err != nil || result.ExitReason != domain.EventTurnCompleted {
					t.Fatalf("finish() = (%+v, %v), want a completed turn", result, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("finish() error = nil, want kind %q", tt.wantKind)
			}
			if err.Kind != tt.wantKind || !strings.Contains(err.Message, tt.wantMsg) {
				t.Errorf("error = %q %q, want kind %q containing %q", err.Kind, err.Message, tt.wantKind, tt.wantMsg)
			}
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
		})
	}
}

func TestFinalizeTurn_UndecodableLastResponseReportsUnknownOutcome(t *testing.T) {
	t.Parallel()

	const badMessage = `{"role":"assistant","stopReason":"error","usage":"x"}`
	for _, stop := range []string{"toolUse", "stop"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(messageEnd(responseSpec{id: "r1", text: "working", stop: stop}),
				`{"type":"message_end","message":`+badMessage+`}`,
				`{"type":"turn_end","message":`+badMessage+`,"toolResults":[]}`,
				agentEnd)
			result, err := r.finish()
			if err == nil || err.Kind != domain.ErrTurnOutcomeUnknown {
				t.Fatalf("finish() = (%+v, %v), want kind %q", result, err, domain.ErrTurnOutcomeUnknown)
			}
		})
	}
}

func TestFinalizeTurn_UndecodableTurnEndKeepsTheStopReasonOfTheCountedResponse(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{id: "r1", text: "done"}),
		`{"type":"turn_end","message":{"role":"assistant","stopReason":"error","usage":"x"},"toolResults":[]}`,
		agentEnd)
	result, err := r.finish()
	if err != nil || result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("finish() = (%+v, %v), want a completed turn", result, err)
	}
}

func TestFinalizeTurn_RunWithoutAssistantResponseFallsToWorkEvidence(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(`{"type":"agent_start"}`, agentEnd)
	result, err := r.finish()
	if err == nil {
		t.Fatalf("finish() = %+v, nil, want the shared zero-work failure", result)
	}
}

func TestFinalizeTurn_RunStartedAfterAgentEndDoesNotReuseTheStopReason(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"agent_start"}`,
		messageEnd(responseSpec{text: "first", stop: "stop", input: 5, output: 1}),
		agentEnd,
		`{"type":"agent_start"}`,
	)
	r.state.settleUsage(context.Background(), r.emit, &r.spend, true)
	result, err := r.state.finalizeTurn(r.emit, r.last, 1, errors.New("exit status 1"), nil, nil, &r.spend)
	if err == nil || err.Kind != domain.ErrPortExit {
		t.Errorf("finalizeTurn() error = %v, want a port_exit: the second run never reached its own agent_end", err)
	}
	if !result.SpendUnaccounted {
		t.Errorf("result = %+v, want the spend unaccounted for the unfinished run", result)
	}
}

func TestFinalizeTurn_RunStartedAfterAgentEndWithCleanExit(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		`{"type":"agent_start"}`,
		messageEnd(responseSpec{text: "first", stop: "length", input: 5, output: 1}),
		agentEnd,
		`{"type":"agent_start"}`,
	)
	result, err := r.finish()
	if err != nil || result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("finish() = (%+v, %v), want the exit-based completion on work evidence, not the stale length failure", result, err)
	}
	if !result.SpendUnaccounted {
		t.Errorf("result = %+v, want the spend unaccounted for the unfinished run", result)
	}
}

func TestFinalizeTurn_NoAgentEndHasNoTerminalReport(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "partial", stop: "stop"}))
	result, err := r.finish()
	if err != nil || result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("finish() = (%+v, %v), want the exit-based disposition with work evidence", result, err)
	}

	r = newTurnRun(t)
	r.feed(messageEnd(responseSpec{stop: "error", errMessage: "x"}))
	if _, err = r.state.finalizeTurn(r.emit, nil, 3, errors.New("exit status 3"), nil, nil, &r.spend); err == nil || err.Kind != domain.ErrPortExit {
		t.Errorf("finalizeTurn() error = %v, want a port_exit for a non-zero exit with no agent_end", err)
	}
}

func TestUsage_CacheWriteTokens(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "a", input: 100, output: 20, cacheRead: 10, cacheWrite: 5}), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	want := domain.TokenUsage{InputTokens: 115, OutputTokens: 20, CacheReadTokens: 10, CacheWriteTokens: 5, TotalTokens: 135}
	if result.Usage != want {
		t.Errorf("usage = %+v, want %+v", result.Usage, want)
	}
	events := r.ofType(domain.EventTokenUsage)
	if len(events) != 1 || events[0].Usage != want || events[0].Model != "anthropic/m1" {
		t.Errorf("token_usage events = %+v, want one carrying %+v for anthropic/m1", events, want)
	}
}

func TestUsage_OneEventPerResponse(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{text: "x", input: 10, output: 1}),
		messageEnd(responseSpec{text: "y", input: 20, output: 2}),
		messageEnd(responseSpec{text: "z", input: 30, output: 3}),
		agentEnd,
	)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	events := r.ofType(domain.EventTokenUsage)
	if len(events) != 3 {
		t.Fatalf("token_usage events = %d, want one per response", len(events))
	}
	wantInput := []int64{10, 30, 60}
	for i, e := range events {
		if e.Usage.InputTokens != wantInput[i] || e.Model != "anthropic/m1" {
			t.Errorf("event %d = input %d model %q, want run total %d for anthropic/m1", i, e.Usage.InputTokens, e.Model, wantInput[i])
		}
	}
	if got := r.state.usage.ByModel()["anthropic/m1"]; got.InputTokens != 60 || got.OutputTokens != 6 {
		t.Errorf("anthropic/m1 = %+v, want input 60 output 6", got)
	}
	if result.Usage.InputTokens != 60 {
		t.Errorf("usage = %+v, want input 60", result.Usage)
	}
}

func TestUsage_AccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{model: "a", text: "x", input: 10, output: 1}), agentEnd)
	if _, err := r.finish(); err != nil {
		t.Fatalf("first finish() error = %v", err)
	}

	r.state.resetTurn()
	r.last = nil
	r.feed(messageEnd(responseSpec{model: "b", text: "y", input: 20, output: 2}), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("second finish() error = %v", err)
	}
	if result.Usage.InputTokens != 30 || result.Usage.OutputTokens != 3 || result.Usage.TotalTokens != 33 {
		t.Errorf("usage = %+v, want the session total input 30 output 3", result.Usage)
	}
	events := r.ofType(domain.EventTokenUsage)
	if len(events) != 2 || events[1].Usage.InputTokens != 30 || len(r.state.usage.ByModel()) != 2 {
		t.Errorf("token_usage events = %+v, by model = %+v, want one event per response and both models settled", events, r.state.usage.ByModel())
	}
}

func TestUsage_ZeroCountsReportNothing(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "x"}), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if result.UsageMeasured || len(r.ofType(domain.EventTokenUsage)) != 0 || !result.SpendUnaccounted {
		t.Errorf("UsageMeasured = %v, token_usage events = %d, SpendUnaccounted = %v, want no figure and the spend unaccounted",
			result.UsageMeasured, len(r.ofType(domain.EventTokenUsage)), result.SpendUnaccounted)
	}
}

func TestUsage_AbsentUsageIsNotMeasured(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "x", noUsage: true}), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if result.UsageMeasured || len(r.ofType(domain.EventTokenUsage)) != 0 {
		t.Errorf("UsageMeasured = %v, token_usage events = %d, want neither", result.UsageMeasured, len(r.ofType(domain.EventTokenUsage)))
	}
}

func TestUsage_ReportedBeforeTheTurnEnds(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{text: "running", input: 50, output: 10}),
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"sleep 100"}}`,
	)

	events := r.ofType(domain.EventTokenUsage)
	if len(events) != 1 || events[0].Usage.InputTokens != 50 || events[0].Usage.OutputTokens != 10 {
		t.Fatalf("token_usage events = %+v, want the finished response's usage while the tool runs", events)
	}
	if got := r.state.usage.Snapshot(); got.InputTokens != 50 {
		t.Errorf("snapshot = %+v, want input 50", got)
	}
}

func TestUsage_SpendSettledOnlyByAgentEnd(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "x", input: 5, output: 1}))
	result, _ := r.finish()
	if !result.SpendUnaccounted {
		t.Errorf("result = %+v, want the spend unaccounted without an agent_end", result)
	}

	r = newTurnRun(t)
	r.feed(messageEnd(responseSpec{text: "x", input: 5, output: 1}), agentEnd)
	result, _ = r.finish()
	if result.SpendUnaccounted {
		t.Errorf("result = %+v, want the spend accounted after agent_end", result)
	}
}

func TestUsage_CompactionLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	feed := func(compact bool) (domain.TurnResult, *turnRun) {
		r := newTurnRun(t)
		r.feed(messageEnd(responseSpec{id: "r1", text: "x", input: 5, output: 1}))
		if compact {
			r.feed(`{"type":"compaction_start","reason":"threshold"}`, `{"type":"compaction_end","reason":"threshold","aborted":false,"willRetry":false}`)
		}
		r.feed(agentEnd)
		result, err := r.finish()
		if err != nil {
			t.Fatalf("finish() error = %v", err)
		}
		return result, r
	}

	result, r := feed(true)
	if !result.SpendUnaccounted || !result.UsageMeasured || result.Usage.InputTokens != 5 || len(r.ofType(domain.EventTokenUsage)) != 1 {
		t.Errorf("compacted turn = %+v, want the counted figure reported and the spend unaccounted", result)
	}
	result, _ = feed(false)
	if result.SpendUnaccounted {
		t.Errorf("turn without compaction = %+v, want the spend accounted", result)
	}
}

func TestUsage_CompactionWithoutOutputLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		end  string
		want bool
	}{
		{name: "completed", end: `{"type":"compaction_end","reason":"threshold","result":{"summary":"s"},"aborted":false,"willRetry":false}`, want: true},
		{name: "aborted", end: `{"type":"compaction_end","reason":"threshold","aborted":true,"willRetry":false}`},
		{name: "failed", end: `{"type":"compaction_end","reason":"threshold","aborted":false,"willRetry":false,"errorMessage":"Auto-compaction failed: x"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(`{"type":"compaction_start","reason":"threshold"}`, tt.end, agentEnd)
			result, _ := r.finish()
			if result.SpendUnaccounted != tt.want || result.UsageMeasured {
				t.Errorf("SpendUnaccounted = %v, UsageMeasured = %v, want %v and no figure", result.SpendUnaccounted, result.UsageMeasured, tt.want)
			}
		})
	}
}

func TestUsage_UnmeasuredResponseLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		unmeasured responseSpec
		want       bool
	}{
		{name: "success with zero counts", unmeasured: responseSpec{id: "r2", text: "y"}, want: true},
		{name: "success with no usage", unmeasured: responseSpec{id: "r2", text: "y", noUsage: true}, want: true},
		{name: "tool use with zero counts", unmeasured: responseSpec{id: "r2", stop: "toolUse"}, want: true},
		{name: "failed attempt with zero counts", unmeasured: responseSpec{id: "r2", stop: "error", errMessage: "400"}},
		{name: "aborted with no usage", unmeasured: responseSpec{id: "r2", stop: "aborted", noUsage: true}},
		{name: "failed attempt with an empty thinking block", unmeasured: responseSpec{id: "r2", stop: "error", errMessage: "400", emptyThinking: true, noUsage: true}},
		{name: "failed attempt with output and no usage", unmeasured: responseSpec{id: "r2", stop: "error", errMessage: "cut off", text: "partial", noUsage: true}, want: true},
		{name: "failed attempt with output and zero counts", unmeasured: responseSpec{id: "r2", stop: "aborted", text: "partial"}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(
				messageEnd(responseSpec{id: "r1", text: "x", input: 5, output: 1}),
				messageEnd(tt.unmeasured),
				messageEnd(responseSpec{id: "r3", text: "z", input: 7, output: 1}),
				agentEnd,
			)
			result, err := r.finish()
			if err != nil {
				t.Fatalf("finish() error = %v", err)
			}
			if result.Usage.InputTokens != 12 || result.SpendUnaccounted != tt.want {
				t.Errorf("usage = %+v, SpendUnaccounted = %v, want input 12 and %v", result.Usage, result.SpendUnaccounted, tt.want)
			}
		})
	}
}

func TestUsage_FailureOnlyTurnSpend(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		response responseSpec
		want     bool
	}{
		{name: "empty error", response: responseSpec{stop: "error", errMessage: "400"}},
		{name: "empty abort", response: responseSpec{stop: "aborted", noUsage: true}},
		{name: "error after output", response: responseSpec{stop: "error", errMessage: "cut off", text: "partial"}, want: true},
		{name: "empty error with an empty thinking block", response: responseSpec{stop: "error", errMessage: "400", emptyThinking: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(messageEnd(tt.response), agentEnd)
			result, _ := r.finish()
			if result.SpendUnaccounted != tt.want || result.UsageMeasured {
				t.Errorf("SpendUnaccounted = %v, UsageMeasured = %v, want %v and no figure", result.SpendUnaccounted, result.UsageMeasured, tt.want)
			}
		})
	}
}

func cacheWarmEntry(id, model string, input, output, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"entry_appended","entry":{"type":"usage","id":%q,"parentId":"p1","timestamp":"2026-10-21T08:37:47.605Z","kind":"cache_warm","provider":"anthropic","model":%q,"usage":{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}}}`,
		id, model, input, output, cacheRead)
}

func TestUsage_CacheWarmingRequestIsAccounted(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{id: "r1", model: "a", text: "x", input: 100, output: 5}),
		cacheWarmEntry("w1", "a", 0, 1, 90),
		cacheWarmEntry("w1", "a", 0, 1, 90),
		`{"type":"entry_appended","entry":{"type":"context_edit","id":"e1","parentId":"w1","timestamp":"2026-10-21T08:37:47.605Z","targetId":"w1","replacement":null}}`,
		messageEnd(responseSpec{id: "r2", model: "a", text: "y", input: 7, output: 1}),
		agentEnd,
	)
	r.assertNoMalformed()
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	want := domain.TokenUsage{InputTokens: 197, OutputTokens: 7, CacheReadTokens: 90, TotalTokens: 204}
	if result.Usage != want || result.SpendUnaccounted {
		t.Errorf("usage = %+v, SpendUnaccounted = %v, want %+v with the spend accounted: the warming request is billed", result.Usage, result.SpendUnaccounted, want)
	}
	if got := len(r.ofType(domain.EventTokenUsage)); got != 3 {
		t.Errorf("token_usage events = %d, want one per request, the warming request once", got)
	}
}

func TestUsage_CacheWarmingOnlyTurnIsAccounted(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(cacheWarmEntry("w1", "a", 0, 1, 90), agentEnd)
	result, _ := r.finish()
	if !result.UsageMeasured || result.Usage.CacheReadTokens != 90 || result.SpendUnaccounted {
		t.Errorf("result = %+v, want the warming request counted and the spend accounted", result)
	}
}

func TestUsage_UndecodablePayloadOfABilledRequestLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	const badMessage = `{"role":"assistant","content":"x"}`
	for _, tt := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{name: "message_end", lines: []string{`{"type":"message_end","message":` + badMessage + `}`}, want: true},
		{name: "turn_end without a message_end", lines: []string{`{"type":"turn_end","message":` + badMessage + `,"toolResults":[]}`}, want: true},
		{
			name: "turn_end after a counted message_end",
			lines: []string{
				messageEnd(responseSpec{id: "r2", text: "y", input: 7, output: 1}),
				`{"type":"turn_end","message":` + badMessage + `,"toolResults":[]}`,
			},
		},
		{name: "cache_warm entry with undecodable usage", lines: []string{`{"type":"entry_appended","entry":{"type":"usage","id":"w1","kind":"cache_warm","usage":"x"}}`}, want: true},
		{name: "usage entry with undecodable fields", lines: []string{`{"type":"entry_appended","entry":{"type":"usage","id":5,"kind":"cache_warm"}}`}, want: true},
		{name: "entry of another type that does not decode", lines: []string{`{"type":"entry_appended","entry":{"type":"custom","id":5}}`}},
		{name: "entry that is not an object", lines: []string{`{"type":"entry_appended","entry":5}`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newTurnRun(t)
			r.feed(messageEnd(responseSpec{id: "r1", text: "x", input: 5, output: 1}), `{"type":"turn_start"}`)
			r.feed(tt.lines...)
			r.feed(agentEnd)
			result, err := r.finish()
			if err != nil && err.Kind != domain.ErrTurnOutcomeUnknown {
				t.Fatalf("finish() error = %v", err)
			}
			if got := len(r.ofType(domain.EventMalformed)); got != 1 {
				t.Errorf("malformed events = %d, want 1", got)
			}
			if result.SpendUnaccounted != tt.want {
				t.Errorf("SpendUnaccounted = %v, want %v", result.SpendUnaccounted, tt.want)
			}
		})
	}
}

func TestUsage_CompactionEntryWithoutCompactionStartLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{id: "r1", text: "x", input: 5, output: 1}),
		`{"type":"entry_appended","entry":{"type":"compaction","id":"c1","parentId":"p1","timestamp":"2026-10-21T08:37:47.605Z","summary":"s","firstKeptEntryId":"p1","tokensBefore":10}}`,
		agentEnd,
	)
	r.assertNoMalformed()
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if !result.SpendUnaccounted || !result.UsageMeasured {
		t.Errorf("result = %+v, want the counted figure reported and the spend unaccounted", result)
	}
}

func TestUsage_CompactionDoesNotAffectTheNextTurn(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(messageEnd(responseSpec{id: "r1", text: "x", input: 5, output: 1}), `{"type":"compaction_start"}`, agentEnd)
	if _, err := r.finish(); err != nil {
		t.Fatalf("first finish() error = %v", err)
	}

	r.state.resetTurn()
	r.spend = agentcore.TurnSpend{}
	r.last = nil
	r.feed(messageEnd(responseSpec{id: "r2", text: "y", input: 7, output: 1}), agentEnd)
	second, err := r.finish()
	if err != nil {
		t.Fatalf("second finish() error = %v", err)
	}
	if second.SpendUnaccounted || second.Usage.InputTokens != 12 {
		t.Errorf("second turn = %+v, want the spend accounted and session input 12", second)
	}
}

func TestUsage_MixedModelTurnAttributesEachModel(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{model: "a", text: "x", input: 10, output: 1}),
		messageEnd(responseSpec{model: "b", text: "y", input: 20, output: 2, cacheWrite: 4}),
		messageEnd(responseSpec{model: "a", text: "z", input: 30, output: 3}),
		agentEnd,
	)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if got := len(r.ofType(domain.EventTokenUsage)); got != 3 {
		t.Errorf("token_usage events = %d, want one per response", got)
	}
	byModel := r.state.usage.ByModel()
	wantA := domain.TokenUsage{InputTokens: 40, OutputTokens: 4, TotalTokens: 44}
	wantB := domain.TokenUsage{InputTokens: 24, OutputTokens: 2, CacheWriteTokens: 4, TotalTokens: 26}
	if len(byModel) != 2 || byModel["anthropic/a"] != wantA || byModel["anthropic/b"] != wantB {
		t.Errorf("by model = %+v, want anthropic/a %+v and anthropic/b %+v", byModel, wantA, wantB)
	}
	if result.Usage.TotalTokens != 70 || result.SpendUnaccounted {
		t.Errorf("result usage = %+v, SpendUnaccounted = %v, want total 70 and the spend accounted", result.Usage, result.SpendUnaccounted)
	}
}

func TestUsage_TimestampIdentifiesAResponseWithNoResponseID(t *testing.T) {
	t.Parallel()

	line := func(typ string, timestamp int64, input int64) string {
		return fmt.Sprintf(`{"type":%q,"message":{"role":"assistant","content":[],"provider":"p","model":"m","stopReason":"stop","timestamp":%d,"usage":{"input":%d,"output":1,"cacheRead":0,"cacheWrite":0}}}`, typ, timestamp, input)
	}
	r := newTurnRun(t)
	r.feed(line("message_end", 1000, 10), line("turn_end", 1000, 10), line("message_end", 1001, 20), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if got := len(r.ofType(domain.EventTokenUsage)); got != 2 {
		t.Errorf("token_usage events = %d, want 2: the repeated timestamp is one response, the new one another", got)
	}
	if result.Usage.InputTokens != 30 || result.SpendUnaccounted {
		t.Errorf("usage = %+v, SpendUnaccounted = %v, want input 30 and the spend accounted", result.Usage, result.SpendUnaccounted)
	}
}

func TestUsage_SameMillisecondResponsesWithNoIDCountOnce(t *testing.T) {
	t.Parallel()

	line := func(input int64) string {
		return fmt.Sprintf(`{"type":"message_end","message":{"role":"assistant","content":[],"provider":"p","model":"m","stopReason":"stop","timestamp":1000,"usage":{"input":%d,"output":1,"cacheRead":0,"cacheWrite":0}}}`, input)
	}
	r := newTurnRun(t)
	r.feed(line(10), line(20), agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if got := len(r.ofType(domain.EventTokenUsage)); got != 1 {
		t.Errorf("token_usage events = %d, want 1: two responses with no id and one timestamp share an identity", got)
	}
	if result.Usage.InputTokens != 20 {
		t.Errorf("usage = %+v, want the larger of the two readings, input 20", result.Usage)
	}
}

func TestUsage_ResponseWithNoIdentityLeavesTheSpendUnaccounted(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"x"}],"provider":"p","model":"m","stopReason":"stop","usage":{"input":10,"output":1,"cacheRead":0,"cacheWrite":0}}}`, agentEnd)
	result, err := r.finish()
	if err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if result.UsageMeasured || len(r.ofType(domain.EventTokenUsage)) != 0 || !result.SpendUnaccounted {
		t.Errorf("UsageMeasured = %v, token_usage events = %d, SpendUnaccounted = %v, want no figure and the spend unaccounted",
			result.UsageMeasured, len(r.ofType(domain.EventTokenUsage)), result.SpendUnaccounted)
	}
}

func TestUsage_UnidentifiedResponseKeepsTheIdentifiedUsage(t *testing.T) {
	t.Parallel()

	r := newTurnRun(t)
	r.feed(
		messageEnd(responseSpec{id: "r1", text: "x", input: 100, output: 1}),
		`{"type":"message_end","message":{"role":"assistant","content":[],"provider":"anthropic","model":"m1","stopReason":"stop","usage":{"input":7,"output":1,"cacheRead":0,"cacheWrite":0}}}`,
		agentEnd,
	)
	first, err := r.finish()
	if err != nil {
		t.Fatalf("first finish() error = %v", err)
	}
	if !first.SpendUnaccounted {
		t.Errorf("first turn = %+v, want the spend unaccounted", first)
	}

	r.state.resetTurn()
	r.spend = agentcore.TurnSpend{}
	r.last = nil
	r.feed(messageEnd(responseSpec{id: "r2", text: "y", input: 50, output: 1}), agentEnd)
	second, err := r.finish()
	if err != nil {
		t.Fatalf("second finish() error = %v", err)
	}
	if second.Usage.InputTokens != 150 {
		t.Errorf("session input after the second turn = %d, want 150: the identified response of the first turn counts", second.Usage.InputTokens)
	}
}

func TestSessionDirName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{"unix path", "/home/coder/work/issue-1", "--home-coder-work-issue-1--"},
		{"path with spaces", "/home/my user/work tree", "--home-my user-work tree--"},
		{"trailing slash", "/home/coder/work/", "--home-coder-work--"},
		{"windows drive path", `C:\a\b`, "--C--a-b--"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sessionDirName(tt.path); got != tt.want {
				t.Errorf("sessionDirName(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
