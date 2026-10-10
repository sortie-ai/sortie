package fakemodel

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	conformanceNonce         = "nonce-0123456789abcdef"
	conformanceSentinel      = "sortie-scripted-credential-fixture"
	conformanceCallID        = "call-1"
	conformanceToolName      = "read_file"
	conformanceMessage       = "scripted model: request 2 (POST /v1/messages) is beyond the script"
	conformanceArmEnv        = "FAKEMODEL_CONFORMANCE_ARM"
	conformanceCommandKind   = "conformance-scripted-command"
	conformancePlainKind     = "conformance-scripted-plain"
	conformanceDeclaredToken = "CONFORMANCE_DECLARED_TOKEN"
	conformanceNotice        = "tool call refused by the session policy"
	conformanceServerTool    = "sortie_status"
	conformanceServerTurn    = "731948265"
)

const (
	conformanceAuxiliaryAt = iota
	conformanceFirstTurnAt
	conformanceSecondTurnAt
)

const (
	conformanceEventStartedAt = iota
	conformanceEventToolAt
	conformanceEventUsageAt
)

var (
	conformanceFirstUsage     = Usage{Prompt: 211, Candidates: 13, Thoughts: 5, CachedContent: 53, CacheWrite: 19}
	conformanceSecondUsage    = Usage{Prompt: 307, Candidates: 17, Thoughts: 7, CachedContent: 61, CacheWrite: 23}
	conformanceAuxiliaryUsage = Usage{Prompt: 7, Candidates: 3, Thoughts: 2}

	conformanceKindsOnce     sync.Once
	errConformanceFactory    = errors.New("conformance factory refused")
	conformanceUnservedKinds = []ExchangeKind{ExchangeExhausted, ExchangeUnmatched, ExchangeUnrouted, ExchangeUnsupported, ExchangeInvalid}
)

type conformanceCase[S any] struct {
	name   string
	mutate func(s *S)
	want   []string
	also   []string
}

type conformanceUsageState struct {
	run     Run
	arrival registry.UsageArrival
}

type conformanceContainmentState struct {
	run     Run
	streams []streamFile
}

type conformanceExhaustionState struct {
	run      Run
	timedOut bool
	failures []string
	nonce    string
}

type conformanceSpyTB struct {
	testing.TB

	errors []string
}

func (s *conformanceSpyTB) Errorf(format string, args ...any) {
	s.errors = append(s.errors, fmt.Sprintf(format, args...))
}

func conformanceTurnRun() Run {
	usage := domain.TokenUsage{InputTokens: 525, OutputTokens: 47, TotalTokens: 572, CacheReadTokens: 114, CacheWriteTokens: 42}
	credentials := func() []string { return []string{conformanceSentinel} }
	return Run{
		Environment: Environment{Scenario: ScenarioTurn, Sentinel: conformanceSentinel},
		Result:      domain.TurnResult{SessionID: "session-1", ExitReason: domain.EventTurnCompleted, Usage: usage, UsageMeasured: true},
		Events: []domain.AgentEvent{
			{Type: domain.EventSessionStarted, SessionID: "session-1"},
			{Type: domain.EventToolResult, ToolName: conformanceToolName, ToolDurationMS: 3},
			{Type: domain.EventTokenUsage, Usage: usage},
		},
		Exchanges: []Exchange{
			{
				Seq: 1, Kind: ExchangeAuxiliary, Method: "POST", Path: "/aux/one", Step: -1,
				Body: []byte(`{}`), Credentials: credentials(),
				Answer: Answer{Status: 200, Text: "title", Usage: conformanceAuxiliaryUsage},
			},
			{
				Seq: 2, Kind: ExchangeTurn, Method: "POST", Path: "/v1/messages", Step: 0,
				Body: []byte(`{}`), Credentials: credentials(),
				Answer: Answer{Status: 200, Call: &FunctionCall{Name: conformanceToolName}, CallID: conformanceCallID, Usage: conformanceFirstUsage},
			},
			{
				Seq: 3, Kind: ExchangeTurn, Method: "POST", Path: "/v1/messages", Step: 1,
				Body: []byte(`{}`), Credentials: credentials(),
				ToolResults: []ToolResult{{CallID: conformanceCallID, Name: conformanceToolName, Output: conformanceNonce + "\n"}},
				Answer:      Answer{Status: 200, Text: "done", Usage: conformanceSecondUsage},
			},
		},
	}
}

func conformanceToolServerRun() Run {
	preamble := ToolResult{CallID: "call-0", Name: conformanceToolName, Output: "permission denied"}
	server := ToolResult{CallID: conformanceCallID, Name: conformanceServerTool, Output: `{"turn_number":` + conformanceServerTurn + `}`}
	turn := func(seq, step int, answer Answer, results ...ToolResult) Exchange {
		return Exchange{Seq: seq, Kind: ExchangeTurn, Method: "POST", Path: "/v1/messages", Step: step, ToolResults: results, Answer: answer}
	}
	return Run{
		Environment: Environment{Scenario: ScenarioToolServer, Sentinel: conformanceSentinel},
		Events: []domain.AgentEvent{
			{Type: domain.EventToolResult, ToolName: conformanceToolName, ToolError: true},
			{Type: domain.EventToolResult, ToolName: conformanceServerTool},
			{Type: domain.EventToolResult, ToolName: "task_complete", ToolError: true},
		},
		Exchanges: []Exchange{
			turn(1, 0, Answer{Call: &FunctionCall{Name: conformanceToolName}, CallID: "call-0"}),
			turn(2, 1, Answer{Call: &FunctionCall{Name: conformanceServerTool}, CallID: conformanceCallID}, preamble),
			turn(3, 2, Answer{Call: &FunctionCall{Name: "task_complete"}, CallID: "call-2"}, preamble, server),
		},
	}
}

func conformanceRefusedRun() Run {
	run := conformanceTurnRun()
	run.Exchanges[conformanceSecondTurnAt].ToolResults[0].Output = "refused by the session policy"
	run.Events[conformanceEventToolAt].ToolError = true
	run.Events = append(run.Events, domain.AgentEvent{Type: domain.EventNotification, Message: conformanceNotice})
	return run
}

func conformanceNewExhaustion() conformanceExhaustionState {
	usage := domain.TokenUsage{InputTokens: 211, OutputTokens: 18, TotalTokens: 229, CacheReadTokens: 53, CacheWriteTokens: 19}
	return conformanceExhaustionState{
		run: Run{
			Environment: Environment{Scenario: ScenarioExhaustion, Sentinel: conformanceSentinel},
			Err:         errors.New("agent failed: " + conformanceMessage),
			Result:      domain.TurnResult{ExitReason: domain.EventTurnFailed, Usage: usage, UsageMeasured: true},
			Events:      []domain.AgentEvent{{Type: domain.EventTurnFailed, Message: "provider said: " + conformanceMessage}},
			Exchanges: []Exchange{
				{
					Seq: 1, Kind: ExchangeTurn, Method: "POST", Path: "/v1/messages", Step: 0,
					Answer: Answer{Status: 200, Call: &FunctionCall{Name: conformanceToolName}, CallID: conformanceCallID, Usage: conformanceFirstUsage},
				},
				{
					Seq: 2, Kind: ExchangeExhausted, Method: "POST", Path: "/v1/messages", Step: -1,
					ToolResults: []ToolResult{{CallID: conformanceCallID, Name: conformanceToolName, Output: conformanceNonce + "\n"}},
					Answer:      Answer{Status: 400, Text: conformanceMessage},
				},
			},
		},
		failures: []string{"scripted model: request 2 (POST /v1/messages) arrived after the last script entry"},
		nonce:    conformanceNonce,
	}
}

func conformanceReport(run *Run, mutate func(u *domain.TokenUsage)) {
	mutate(&run.Result.Usage)
	for i := range run.Events {
		if run.Events[i].Usage != (domain.TokenUsage{}) {
			mutate(&run.Events[i].Usage)
		}
	}
}

func conformanceAssertViolations(t *testing.T, property string, got, want, also []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("violations = %q, want %d violations matching %q", got, len(want), want)
	}
	for i, violation := range got {
		if !strings.HasPrefix(violation, property+": ") {
			t.Errorf("violation %d = %q, want a prefix naming property %q", i, violation, property)
		}
		if !strings.Contains(violation, want[i]) {
			t.Errorf("violation %d = %q, want it to contain %q", i, violation, want[i])
		}
	}
	joined := strings.Join(got, "\n")
	for _, fragment := range also {
		if !strings.Contains(joined, fragment) {
			t.Errorf("violations = %q, want them to contain %q", got, fragment)
		}
	}
}

func conformanceRunCases[S any](t *testing.T, property string, cases []conformanceCase[S], build func() S, check func(S) []string) {
	t.Helper()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := build()
			if tt.mutate != nil {
				tt.mutate(&state)
			}

			got := check(state)

			conformanceAssertViolations(t, property, got, tt.want, tt.also)
		})
	}
}

func conformanceRegisterKinds() {
	conformanceKindsOnce.Do(func() {
		refuse := func() (domain.AgentAdapter, error) { return nil, errConformanceFactory }
		registry.Agents.RegisterWithMeta(conformanceCommandKind, refuse, registry.AgentMeta{
			RequiresCommand: true,
			CredentialEnv:   registry.DeclareCredentialEnv(conformanceDeclaredToken),
		})
		registry.Agents.RegisterWithMeta(conformancePlainKind, refuse, registry.AgentMeta{})
	})
}

func conformanceExchange(kind ExchangeKind, usage Usage) Exchange {
	return Exchange{Kind: kind, Answer: Answer{Usage: usage}}
}

func TestConformanceTurnUsage(t *testing.T) {
	t.Parallel()

	if firstTurnUsage != conformanceFirstUsage {
		t.Errorf("firstTurnUsage = %+v, want %+v", firstTurnUsage, conformanceFirstUsage)
	}
	if secondTurnUsage != conformanceSecondUsage {
		t.Errorf("secondTurnUsage = %+v, want %+v", secondTurnUsage, conformanceSecondUsage)
	}
	if auxiliaryUsage != conformanceAuxiliaryUsage {
		t.Errorf("auxiliaryUsage = %+v, want %+v", auxiliaryUsage, conformanceAuxiliaryUsage)
	}
	auxiliaryBasis := auxiliaryUsage.Prompt + auxiliaryUsage.Candidates
	for name, turn := range map[string]Usage{"first": firstTurnUsage, "second": secondTurnUsage} {
		if basis := turn.Prompt + turn.Candidates; basis <= auxiliaryBasis {
			t.Errorf("%s turn basis = %d, want above the auxiliary basis %d", name, basis, auxiliaryBasis)
		}
	}
}

func TestServerToolChoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared []Tool
		want     FunctionCall
	}{
		{name: "bare name", declared: []Tool{{Name: "sortie_status"}}, want: FunctionCall{Name: "sortie_status"}},
		{name: "server prefix", declared: []Tool{{Name: "mcp__sortie-tools__sortie_status"}}, want: FunctionCall{Name: "mcp__sortie-tools__sortie_status"}},
		{name: "namespace member", declared: []Tool{{Name: "sortie_status", Namespace: "mcp__sortie_tools"}}, want: FunctionCall{Name: "sortie_status", Namespace: "mcp__sortie_tools"}},
		{name: "suffix without a separator", declared: []Tool{{Name: "notsortie_status"}}},
		{name: "name inside a longer one", declared: []Tool{{Name: "sortie_status_v2"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := serverTool("sortie_status")(tt.declared)

			if (err == nil) != (tt.want.Name != "") || got.Name != tt.want.Name || got.Namespace != tt.want.Namespace {
				t.Errorf("serverTool(%q)(%+v) = %+v, %v, want %+v", "sortie_status", tt.declared, got, err, tt.want)
			}
		})
	}
}

func TestConformanceClosingResponse(t *testing.T) {
	t.Parallel()

	finish := func([]Tool) (FunctionCall, error) { return FunctionCall{Name: "task_complete"}, nil }
	tests := []struct {
		name     string
		binding  Binding
		wantText bool
	}{
		{name: "no finish answers with text", binding: Binding{}, wantText: true},
		{name: "finish answers with its call", binding: Binding{Finish: finish}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := closingResponse(tt.binding)

			if (got.Text != "") != tt.wantText || (got.Call == nil) != tt.wantText {
				t.Errorf("closingResponse(%+v) = text %q, call set %t, want text %t", tt.binding, got.Text, got.Call != nil, tt.wantText)
			}
			if got.Usage != conformanceSecondUsage {
				t.Errorf("closingResponse(%+v).Usage = %+v, want %+v", tt.binding, got.Usage, conformanceSecondUsage)
			}
		})
	}
}

func TestConformanceExpectedUsage(t *testing.T) {
	t.Parallel()

	turnOne := domain.TokenUsage{InputTokens: 211, OutputTokens: 18, TotalTokens: 229, CacheReadTokens: 53, CacheWriteTokens: 19}
	ignored := []Exchange{}
	for _, kind := range conformanceUnservedKinds {
		ignored = append(ignored, conformanceExchange(kind, Usage{Prompt: 1000, Candidates: 1000, Thoughts: 1000, CachedContent: 1000, CacheWrite: 1000}))
	}
	tests := []struct {
		name      string
		exchanges []Exchange
		want      domain.TokenUsage
	}{
		{name: "no exchanges", want: domain.TokenUsage{}},
		{
			name:      "one turn sums thoughts into output and keeps cache counts out of input",
			exchanges: []Exchange{conformanceExchange(ExchangeTurn, conformanceFirstUsage)},
			want:      turnOne,
		},
		{
			name:      "auxiliary alone",
			exchanges: []Exchange{conformanceExchange(ExchangeAuxiliary, conformanceAuxiliaryUsage)},
			want:      domain.TokenUsage{InputTokens: 7, OutputTokens: 5, TotalTokens: 12},
		},
		{
			name: "mixed turns and auxiliary",
			exchanges: []Exchange{
				conformanceExchange(ExchangeAuxiliary, conformanceAuxiliaryUsage),
				conformanceExchange(ExchangeTurn, conformanceFirstUsage),
				conformanceExchange(ExchangeTurn, conformanceSecondUsage),
			},
			want: domain.TokenUsage{InputTokens: 525, OutputTokens: 47, TotalTokens: 572, CacheReadTokens: 114, CacheWriteTokens: 42},
		},
		{
			name:      "exchanges that were not answered from the script or as auxiliary add nothing",
			exchanges: append([]Exchange{conformanceExchange(ExchangeTurn, conformanceFirstUsage)}, ignored...),
			want:      turnOne,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := expectedUsage(tt.exchanges)

			if got != tt.want {
				t.Errorf("expectedUsage(%d exchanges) = %+v, want %+v", len(tt.exchanges), got, tt.want)
			}
		})
	}
}

func TestConformancePropertyCleanRuns(t *testing.T) {
	t.Parallel()

	run := conformanceTurnRun()
	toolServer := conformanceToolServerRun()
	exhaustion := conformanceNewExhaustion()
	streams := []streamFile{{path: "client.log", content: []byte("traffic")}}
	got := map[string][]string{
		"turn outcome":            turnOutcomeViolations(run, 2),
		"deterministic tool path": toolPathViolations(run, conformanceNonce),
		"tool server":             toolServerViolations(toolServer, conformanceServerTurn),
		"exact usage":             usageViolations(run, registry.UsageArrivalTurnEnd),
		"credential containment":  containmentViolations(run, streams),
		"script exhaustion":       exhaustionViolations(exhaustion.run, conformanceNonce, false, exhaustion.failures),
		"refused call":            refusedCallViolations(conformanceRefusedRun(), conformanceNonce, conformanceNotice),
	}

	for property, violations := range got {
		if len(violations) != 0 {
			t.Errorf("%s violations over a clean run = %q, want none", property, violations)
		}
	}
}

func TestConformancePropertyTurnOutcome(t *testing.T) {
	t.Parallel()

	appendExchange := func(kind ExchangeKind) func(run *Run) {
		return func(run *Run) {
			run.Exchanges = append(run.Exchanges, Exchange{Seq: 4, Kind: kind, Method: "POST", Path: "/x", Step: -1, Answer: Answer{Status: 400, Text: "refused"}})
		}
	}
	cases := []conformanceCase[Run]{
		{name: "clean run"},
		{
			name:   "RunTurn returned an error",
			mutate: func(run *Run) { run.Err = errors.New("boom") },
			want:   []string{"RunTurn returned error"},
		},
		{
			name:   "exit reason is not turn_completed",
			mutate: func(run *Run) { run.Result.ExitReason = domain.EventTurnFailed },
			want:   []string{"ExitReason"},
		},
		{
			name:   "result session id is empty",
			mutate: func(run *Run) { run.Result.SessionID = "" },
			want:   []string{"Result.SessionID is empty"},
		},
		{
			name: "turn_failed event delivered",
			mutate: func(run *Run) {
				run.Events = append(run.Events, domain.AgentEvent{Type: domain.EventTurnFailed, Message: "provider error"})
			},
			want: []string{"turn_failed"},
		},
		{
			name: "startup_failed event delivered",
			mutate: func(run *Run) {
				run.Events = append(run.Events, domain.AgentEvent{Type: domain.EventStartupFailed, Message: "launch error"})
			},
			want: []string{"startup_failed"},
		},
		{
			name: "no session_started event",
			mutate: func(run *Run) {
				run.Events = slices.Delete(run.Events, conformanceEventStartedAt, conformanceEventStartedAt+1)
			},
			want: []string{"session_started"},
		},
		{
			name:   "session_started event without a session id",
			mutate: func(run *Run) { run.Events[conformanceEventStartedAt].SessionID = "" },
			want:   []string{"session_started"},
		},
		{
			name: "fewer turn exchanges than script entries",
			mutate: func(run *Run) {
				run.Exchanges = slices.Delete(run.Exchanges, conformanceSecondTurnAt, conformanceSecondTurnAt+1)
			},
			want: []string{"want one per script entry (2)"},
		},
		{
			name:   "turn answered a different script entry than its position",
			mutate: func(run *Run) { run.Exchanges[conformanceSecondTurnAt].Step = 5 },
			want:   []string{"answered script entry 5, want entry 1"},
		},
		{name: "exhausted exchange", mutate: appendExchange(ExchangeExhausted), want: []string{"ended exhausted"}},
		{name: "unmatched exchange", mutate: appendExchange(ExchangeUnmatched), want: []string{"ended unmatched"}},
		{name: "unrouted exchange", mutate: appendExchange(ExchangeUnrouted), want: []string{"ended unrouted"}},
		{name: "unsupported exchange", mutate: appendExchange(ExchangeUnsupported), want: []string{"ended unsupported"}},
		{name: "invalid exchange", mutate: appendExchange(ExchangeInvalid), want: []string{"ended invalid"}},
	}

	conformanceRunCases(t, "turn outcome", cases,
		func() Run { return conformanceTurnRun() },
		func(run Run) []string { return turnOutcomeViolations(run, 2) },
	)
}

func TestConformancePropertyToolPath(t *testing.T) {
	t.Parallel()

	cases := []conformanceCase[Run]{
		{name: "clean run"},
		{
			name: "a request resending an earlier call's result",
			mutate: func(run *Run) {
				second := &run.Exchanges[conformanceSecondTurnAt]
				second.ToolResults = append([]ToolResult{{CallID: "call-0", Name: "other_tool"}}, second.ToolResults...)
			},
		},
		{
			name: "generateContent run correlates by tool name",
			mutate: func(run *Run) {
				run.Exchanges[conformanceFirstTurnAt].Answer.CallID = ""
				run.Exchanges[conformanceSecondTurnAt].ToolResults[0].CallID = ""
			},
		},
		{
			name: "first turn answered with text",
			mutate: func(run *Run) {
				run.Exchanges[conformanceFirstTurnAt].Answer.Call = nil
				run.Events = slices.Delete(run.Events, conformanceEventToolAt, conformanceEventToolAt+1)
			},
			want: []string{"answered with text"},
		},
		{
			name:   "no tool result on the second turn",
			mutate: func(run *Run) { run.Exchanges[conformanceSecondTurnAt].ToolResults = nil },
			want:   []string{"carries 0 tool results"},
		},
		{
			name: "two tool results on the second turn",
			mutate: func(run *Run) {
				second := &run.Exchanges[conformanceSecondTurnAt]
				second.ToolResults = append(second.ToolResults, second.ToolResults[0])
			},
			want: []string{"carries 2 tool results"},
		},
		{
			name:   "tool result answers another call id",
			mutate: func(run *Run) { run.Exchanges[conformanceSecondTurnAt].ToolResults[0].CallID = "call-2" },
			want:   []string{`carries 0 tool results answering call "call-1"`},
		},
		{
			name: "tool result names another tool when no call id was issued",
			mutate: func(run *Run) {
				run.Exchanges[conformanceFirstTurnAt].Answer.CallID = ""
				run.Exchanges[conformanceSecondTurnAt].ToolResults[0].CallID = ""
				run.Exchanges[conformanceSecondTurnAt].ToolResults[0].Name = "other_tool"
			},
			want: []string{`carries 0 tool results answering call "" of tool "read_file"`},
		},
		{
			name:   "tool result output lacks the nonce",
			mutate: func(run *Run) { run.Exchanges[conformanceSecondTurnAt].ToolResults[0].Output = "something else" },
			want:   []string{`does not contain "` + conformanceNonce + `"`},
		},
		{
			name:   "tool event reports an error",
			mutate: func(run *Run) { run.Events[conformanceEventToolAt].ToolError = true },
			want:   []string{"reports a tool error"},
		},
		{
			name:   "tool event has no tool name",
			mutate: func(run *Run) { run.Events[conformanceEventToolAt].ToolName = "" },
			want:   []string{"tool name"},
		},
		{
			name:   "tool event has the unknown tool name",
			mutate: func(run *Run) { run.Events[conformanceEventToolAt].ToolName = "unknown" },
			want:   []string{"tool name"},
		},
		{
			name:   "tool event has a negative duration",
			mutate: func(run *Run) { run.Events[conformanceEventToolAt].ToolDurationMS = -1 },
			want:   []string{"tool duration"},
		},
		{
			name: "no tool_result event for the answered call",
			mutate: func(run *Run) {
				run.Events = slices.Delete(run.Events, conformanceEventToolAt, conformanceEventToolAt+1)
			},
			want: []string{"0 tool_result events, want one per call answered (1)"},
		},
		{
			name: "more tool_result events than calls answered",
			mutate: func(run *Run) {
				run.Events = append(run.Events, run.Events[conformanceEventToolAt])
			},
			want: []string{"2 tool_result events, want one per call answered (1)"},
		},
	}

	conformanceRunCases(t, "deterministic tool path", cases,
		func() Run { return conformanceTurnRun() },
		func(run Run) []string { return toolPathViolations(run, conformanceNonce) },
	)
}

func TestConformancePropertyToolServer(t *testing.T) {
	t.Parallel()

	cases := []conformanceCase[Run]{
		{name: "failed preamble and a closing call"},
		{
			name: "server call answered with text",
			mutate: func(run *Run) {
				run.Exchanges[1].Answer.Call = nil
				run.Events[2].ToolError = false
				run.Events = slices.Delete(run.Events, 1, 2)
			},
			want: []string{"answered with text"},
		},
		{
			name:   "server call's result missing from the next request",
			mutate: func(run *Run) { run.Exchanges[2].ToolResults = run.Exchanges[2].ToolResults[:1] },
			want:   []string{`carries 0 tool results answering call "call-1"`},
		},
		{
			name:   "server call's result lacks the turn number",
			mutate: func(run *Run) { run.Exchanges[2].ToolResults[1].Output = "{}" },
			want:   []string{`does not contain "` + conformanceServerTurn + `"`},
		},
		{
			name:   "server call's event reports an error",
			mutate: func(run *Run) { run.Events[1].ToolError = true },
			want:   []string{"reports a tool error"},
		},
		{
			name:   "server call's event has the unknown tool name",
			mutate: func(run *Run) { run.Events[1].ToolName = "unknown" },
			want:   []string{"tool name"},
		},
		{
			name:   "no event for the closing call",
			mutate: func(run *Run) { run.Events = run.Events[:2] },
			want:   []string{"2 tool_result events, want one per call answered (3)"},
		},
	}

	conformanceRunCases(t, "tool server", cases,
		conformanceToolServerRun,
		func(run Run) []string { return toolServerViolations(run, conformanceServerTurn) },
	)
}

func TestConformancePropertyRefusedCall(t *testing.T) {
	t.Parallel()

	cases := []conformanceCase[Run]{
		{name: "clean run"},
		{
			name:   "no tool result answers the guarded call",
			mutate: func(run *Run) { run.Exchanges[conformanceSecondTurnAt].ToolResults = nil },
			want:   []string{"carries 0 tool results answering the guarded call"},
		},
		{
			name: "the file's content in an auxiliary request body",
			mutate: func(run *Run) {
				run.Exchanges[conformanceAuxiliaryAt].Body = []byte(`{"text":"` + conformanceNonce + `"}`)
			},
			want: []string{"carried the file's content"},
		},
		{
			name:   "tool result reported without an error",
			mutate: func(run *Run) { run.Events[conformanceEventToolAt].ToolError = false },
			want:   []string{"reports a successful tool result"},
		},
		{
			name: "no tool_result event",
			mutate: func(run *Run) {
				run.Events = slices.Delete(run.Events, conformanceEventToolAt, conformanceEventToolAt+1)
			},
			want: []string{"0 tool_result events, want exactly one"},
		},
		{
			name:   "no refusal notice",
			mutate: func(run *Run) { run.Events[len(run.Events)-1].Message = "something else" },
			want:   []string{"0 notification events"},
		},
	}

	conformanceRunCases(t, "refused call", cases,
		conformanceRefusedRun,
		func(run Run) []string { return refusedCallViolations(run, conformanceNonce, conformanceNotice) },
	)
}

func TestConformancePropertyExactUsage(t *testing.T) {
	t.Parallel()

	fields := []struct {
		name string
		bump func(u *domain.TokenUsage)
	}{
		{"input tokens", func(u *domain.TokenUsage) { u.InputTokens++ }},
		{"output tokens", func(u *domain.TokenUsage) { u.OutputTokens++ }},
		{"total tokens", func(u *domain.TokenUsage) { u.TotalTokens++ }},
		{"cache read tokens", func(u *domain.TokenUsage) { u.CacheReadTokens++ }},
		{"cache write tokens", func(u *domain.TokenUsage) { u.CacheWriteTokens++ }},
	}
	cases := []conformanceCase[conformanceUsageState]{
		{name: "clean turn_end run"},
		{
			name: "measured usage is not reported",
			mutate: func(s *conformanceUsageState) {
				s.run.Result.UsageMeasured = false
			},
			want: []string{"UsageMeasured is false"},
		},
		{
			name: "auxiliary answer left out of the reported figure",
			mutate: func(s *conformanceUsageState) {
				conformanceReport(&s.run, func(u *domain.TokenUsage) {
					u.InputTokens -= 7
					u.OutputTokens -= 5
					u.TotalTokens -= 12
				})
			},
			want: []string{"Result.Usage ="},
			also: []string{"POST", "/aux/one"},
		},
		{
			name: "every auxiliary exchange is listed on a mismatch",
			mutate: func(s *conformanceUsageState) {
				s.run.Exchanges = append(s.run.Exchanges, Exchange{
					Seq: 4, Kind: ExchangeAuxiliary, Method: "POST", Path: "/aux/two", Step: -1,
					Credentials: []string{conformanceSentinel},
					Answer:      Answer{Status: 200, Text: "title", Usage: Usage{Prompt: 11, Candidates: 2}},
				})
			},
			want: []string{"Result.Usage ="},
			also: []string{"/aux/one", "/aux/two"},
		},
		{
			name: "last usage-carrying event differs from the result under an incremental arrival",
			mutate: func(s *conformanceUsageState) {
				s.arrival = registry.UsageArrivalIncremental
				s.run.Events[conformanceEventUsageAt].Usage.InputTokens--
			},
			want: []string{"the last event carrying usage"},
		},
		{
			name: "earlier token_usage event differs from the result under turn_end",
			mutate: func(s *conformanceUsageState) {
				stale := s.run.Events[conformanceEventUsageAt]
				stale.Usage.InputTokens--
				s.run.Events = slices.Insert(s.run.Events, conformanceEventUsageAt, stale)
			},
			want: []string{"token_usage event"},
		},
		{
			name: "no token_usage event under turn_end",
			mutate: func(s *conformanceUsageState) {
				s.run.Events = slices.Delete(s.run.Events, conformanceEventUsageAt, conformanceEventUsageAt+1)
			},
			want: []string{"no token_usage event"},
		},
		{
			name: "no token_usage event under an incremental arrival",
			mutate: func(s *conformanceUsageState) {
				s.arrival = registry.UsageArrivalIncremental
				s.run.Events = slices.Delete(s.run.Events, conformanceEventUsageAt, conformanceEventUsageAt+1)
			},
		},
	}
	for _, field := range fields {
		cases = append(cases, conformanceCase[conformanceUsageState]{
			name: field.name + " off by one",
			mutate: func(s *conformanceUsageState) {
				conformanceReport(&s.run, field.bump)
			},
			want: []string{"Result.Usage ="},
		})
	}

	conformanceRunCases(t, "exact usage", cases,
		func() conformanceUsageState {
			return conformanceUsageState{run: conformanceTurnRun(), arrival: registry.UsageArrivalTurnEnd}
		},
		func(s conformanceUsageState) []string { return usageViolations(s.run, s.arrival) },
	)
}

func TestConformancePropertyContainment(t *testing.T) {
	t.Parallel()

	cases := []conformanceCase[conformanceContainmentState]{
		{name: "clean run"},
		{
			name: "no turn exchange reached the endpoint",
			mutate: func(s *conformanceContainmentState) {
				s.run.Exchanges = s.run.Exchanges[:conformanceFirstTurnAt]
			},
			want: []string{"no turn exchange reached the endpoint"},
		},
		{
			name: "turn exchange without the sentinel credential",
			mutate: func(s *conformanceContainmentState) {
				s.run.Exchanges[conformanceFirstTurnAt].Credentials = nil
			},
			want: []string{"no credential equal to the sentinel"},
		},
		{
			name: "turn exchange with a credential besides the sentinel",
			mutate: func(s *conformanceContainmentState) {
				turn := &s.run.Exchanges[conformanceFirstTurnAt]
				turn.Credentials = append(turn.Credentials, "real-key")
			},
			want: []string{"a credential other than the sentinel"},
		},
		{
			name: "auxiliary exchange with a credential besides the sentinel",
			mutate: func(s *conformanceContainmentState) {
				aux := &s.run.Exchanges[conformanceAuxiliaryAt]
				aux.Credentials = append(aux.Credentials, "real-key")
			},
			want: []string{"a credential other than the sentinel"},
		},
		{
			name: "sentinel in a request body",
			mutate: func(s *conformanceContainmentState) {
				s.run.Exchanges[conformanceSecondTurnAt].Body = []byte(`{"key":"` + conformanceSentinel + `"}`)
			},
			want: []string{"in its body"},
		},
		{
			name: "sentinel in an event",
			mutate: func(s *conformanceContainmentState) {
				s.run.Events[conformanceEventStartedAt].Message = "token " + conformanceSentinel
			},
			want: []string{"event 0"},
			also: []string{"carries the sentinel"},
		},
		{
			name: "event that cannot be encoded",
			mutate: func(s *conformanceContainmentState) {
				s.run.Events[conformanceEventToolAt].RateLimits = map[string]any{"unencodable": make(chan int)}
			},
			want: []string{"cannot be encoded"},
		},
		{
			name: "stream file that cannot be read",
			mutate: func(s *conformanceContainmentState) {
				s.streams = []streamFile{{path: "missing.log", err: fs.ErrNotExist}}
			},
			want: []string{"is unreadable"},
			also: []string{"missing.log"},
		},
		{
			name: "sentinel in a stream file",
			mutate: func(s *conformanceContainmentState) {
				s.streams = []streamFile{{path: "client.log", content: []byte("Authorization: " + conformanceSentinel)}}
			},
			want: []string{"stream file"},
			also: []string{"client.log", "carries the sentinel"},
		},
	}

	conformanceRunCases(t, "credential containment", cases,
		func() conformanceContainmentState {
			return conformanceContainmentState{
				run:     conformanceTurnRun(),
				streams: []streamFile{{path: "client.log", content: []byte("traffic")}},
			}
		},
		func(s conformanceContainmentState) []string { return containmentViolations(s.run, s.streams) },
	)
}

func TestConformanceReadStreams(t *testing.T) {
	t.Parallel()

	present := filepath.Join(t.TempDir(), "client.log")
	if err := os.WriteFile(present, []byte("traffic"), 0o600); err != nil {
		t.Fatalf("write %s: %v", present, err)
	}
	missing := filepath.Join(t.TempDir(), "absent.log")

	got := readStreams([]string{present, missing})

	if len(got) != 2 {
		t.Fatalf("readStreams(2 paths) returned %d streams, want 2", len(got))
	}
	if got[0].err != nil || string(got[0].content) != "traffic" {
		t.Errorf("readStreams(present) = content %q, err %v, want %q, nil", got[0].content, got[0].err, "traffic")
	}
	if !errors.Is(got[1].err, fs.ErrNotExist) {
		t.Errorf("readStreams(missing) error = %v, want %v", got[1].err, fs.ErrNotExist)
	}
}

func TestConformancePropertyExhaustion(t *testing.T) {
	t.Parallel()

	cases := []conformanceCase[conformanceExhaustionState]{
		{name: "clean run"},
		{
			name:   "RunTurn did not return inside the bound",
			mutate: func(s *conformanceExhaustionState) { s.timedOut = true },
			want:   []string{"did not return inside"},
		},
		{
			name:   "RunTurn returned nil",
			mutate: func(s *conformanceExhaustionState) { s.run.Err = nil },
			want:   []string{"returned nil, want an error", "does not contain the endpoint message"},
		},
		{
			name:   "exit reason is not turn_failed",
			mutate: func(s *conformanceExhaustionState) { s.run.Result.ExitReason = domain.EventTurnCompleted },
			want:   []string{"ExitReason"},
		},
		{
			name: "second turn exchange",
			mutate: func(s *conformanceExhaustionState) {
				s.run.Exchanges = append(s.run.Exchanges, Exchange{Seq: 3, Kind: ExchangeTurn, Answer: Answer{Call: &FunctionCall{Name: conformanceToolName}}})
			},
			want: []string{"2 turn exchanges, want exactly one"},
		},
		{
			name:   "turn exchange answered a later script entry",
			mutate: func(s *conformanceExhaustionState) { s.run.Exchanges[0].Step = 1 },
			want:   []string{"1 turn exchanges, want exactly one, step 0"},
		},
		{
			name:   "turn exchange answered with text",
			mutate: func(s *conformanceExhaustionState) { s.run.Exchanges[0].Answer.Call = nil },
			want:   []string{"1 turn exchanges, want exactly one, step 0"},
		},
		{
			name:   "no exhausted exchange",
			mutate: func(s *conformanceExhaustionState) { s.run.Exchanges = s.run.Exchanges[:1] },
			want:   []string{"0 exhausted exchanges, want at least one"},
		},
		{
			name:   "rejected request resent",
			mutate: conformanceResendExhausted,
		},
		{
			name: "resent request carries no tool result",
			mutate: func(s *conformanceExhaustionState) {
				conformanceResendExhausted(s)
				s.run.Exchanges[2].ToolResults = nil
			},
			want: []string{"request 3 (POST /v1/messages) carries 0 tool results"},
		},
		{
			name: "resent request not reported by the endpoint",
			mutate: func(s *conformanceExhaustionState) {
				conformanceResendExhausted(s)
				s.failures = s.failures[:1]
			},
			want: []string{"the endpoint reported 1 failures"},
		},
		{
			name: "turn reports the first rejection instead of the last",
			mutate: func(s *conformanceExhaustionState) {
				conformanceResendExhausted(s)
				s.run.Exchanges[2].Answer.Text = "the second rejection"
			},
			want: []string{"does not contain the endpoint message", "the last"},
		},
		{
			name:   "usage not measured",
			mutate: func(s *conformanceExhaustionState) { s.run.Result.UsageMeasured = false },
			want:   []string{"UsageMeasured is false"},
		},
		{
			name:   "usage differs from what the endpoint served",
			mutate: func(s *conformanceExhaustionState) { s.run.Result.Usage.InputTokens++ },
			want:   []string{"Result.Usage ="},
		},
		{
			name:   "error lacks the endpoint message",
			mutate: func(s *conformanceExhaustionState) { s.run.Err = errors.New("network down") },
			want:   []string{"does not contain the endpoint message"},
		},
		{
			name:   "no turn_failed event",
			mutate: func(s *conformanceExhaustionState) { s.run.Events = nil },
			want:   []string{"event was delivered"},
		},
		{
			name: "last turn_failed event lacks the endpoint message",
			mutate: func(s *conformanceExhaustionState) {
				s.run.Events = append(s.run.Events, domain.AgentEvent{Type: domain.EventTurnFailed, Message: "something else"})
			},
			want: []string{"the last"},
		},
		{
			name: "only an earlier turn_failed event lacks the endpoint message",
			mutate: func(s *conformanceExhaustionState) {
				s.run.Events = slices.Insert(s.run.Events, 0, domain.AgentEvent{Type: domain.EventTurnFailed, Message: "something else"})
			},
		},
		{
			name:   "exhausted request carries no tool result",
			mutate: func(s *conformanceExhaustionState) { s.run.Exchanges[1].ToolResults = nil },
			want:   []string{"carries 0 tool results"},
		},
		{
			name:   "endpoint reported no failure",
			mutate: func(s *conformanceExhaustionState) { s.failures = nil },
			want:   []string{"the endpoint reported 0 failures"},
		},
		{
			name: "endpoint reported a second failure",
			mutate: func(s *conformanceExhaustionState) {
				s.failures = append(s.failures, "scripted model: another failure")
			},
			want: []string{"the endpoint reported 2 failures"},
		},
		{
			name: "endpoint failure names another request",
			mutate: func(s *conformanceExhaustionState) {
				s.failures = []string{"scripted model: request 9 (POST /v1/messages) failed"}
			},
			want: []string{"the endpoint reported 1 failures"},
		},
	}

	conformanceRunCases(t, "script exhaustion", cases,
		conformanceNewExhaustion,
		func(s conformanceExhaustionState) []string {
			return exhaustionViolations(s.run, s.nonce, s.timedOut, s.failures)
		},
	)
}

func conformanceResendExhausted(s *conformanceExhaustionState) {
	resent := s.run.Exchanges[1]
	resent.Seq = 3
	resent.ToolResults = slices.Clone(resent.ToolResults)
	s.run.Exchanges = append(s.run.Exchanges, resent)
	s.failures = append(s.failures, "scripted model: request 3 (POST /v1/messages) arrived after the last script entry")
}

func TestConformanceOneFailurePerRequest(t *testing.T) {
	t.Parallel()

	exhausted := []Exchange{{Seq: 2, Method: "POST", Path: "/v1/messages"}}
	resent := append(slices.Clone(exhausted), Exchange{Seq: 3, Method: "POST", Path: "/v1/messages"})
	tests := []struct {
		name      string
		failures  []string
		exhausted []Exchange
		want      bool
	}{
		{name: "no failures", want: false},
		{name: "one failure naming the request", failures: []string{"scripted model: request 2 (POST /v1/messages) failed"}, want: true},
		{name: "one failure naming another request", failures: []string{"scripted model: request 12 (POST /v1/messages) failed"}, want: false},
		{
			name: "two failures, one naming the request",
			failures: []string{
				"scripted model: request 2 (POST /v1/messages) failed",
				"scripted model: script response 1 was never served",
			},
			want: false,
		},
		{
			name: "one failure naming each resent request",
			failures: []string{
				"scripted model: request 2 (POST /v1/messages) failed",
				"scripted model: request 3 (POST /v1/messages) failed",
			},
			exhausted: resent,
			want:      true,
		},
		{
			name: "two failures naming the same request",
			failures: []string{
				"scripted model: request 2 (POST /v1/messages) failed",
				"scripted model: request 2 (POST /v1/messages) failed",
			},
			exhausted: resent,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requests := tt.exhausted
			if requests == nil {
				requests = exhausted
			}

			got := oneFailurePerRequest(tt.failures, requests)

			if got != tt.want {
				t.Errorf("oneFailurePerRequest(%q, %d requests) = %t, want %t", tt.failures, len(requests), got, tt.want)
			}
		})
	}
}

func TestConformanceRecordingReporter(t *testing.T) {
	t.Parallel()

	t.Run("records failures without failing the test", func(t *testing.T) {
		t.Parallel()

		spy := &conformanceSpyTB{TB: t}
		reporter := &recordingReporter{TB: spy}

		reporter.Errorf("first %d", 1)
		reporter.Errorf("second")

		if got, want := reporter.snapshot(), []string{"first 1", "second"}; !slices.Equal(got, want) {
			t.Errorf("snapshot() = %q, want %q", got, want)
		}
		if len(spy.errors) != 0 {
			t.Errorf("failures forwarded before reraise = %q, want none", spy.errors)
		}
	})

	t.Run("reraise fails the test with each recorded failure once, unformatted again", func(t *testing.T) {
		t.Parallel()

		spy := &conformanceSpyTB{TB: t}
		reporter := &recordingReporter{TB: spy}
		reporter.Errorf("progress %d%%", 50)
		reporter.Errorf("second")

		reporter.reraise()

		if want := []string{"progress 50%", "second"}; !slices.Equal(spy.errors, want) {
			t.Errorf("failures after reraise = %q, want %q", spy.errors, want)
		}
	})

	t.Run("discard drops what reraise would have failed with", func(t *testing.T) {
		t.Parallel()

		spy := &conformanceSpyTB{TB: t}
		reporter := &recordingReporter{TB: spy}
		reporter.Errorf("consumed")

		reporter.discard()
		reporter.reraise()

		if got := reporter.snapshot(); len(got) != 0 {
			t.Errorf("snapshot() after discard = %q, want none", got)
		}
		if len(spy.errors) != 0 {
			t.Errorf("failures after discard and reraise = %q, want none", spy.errors)
		}
	})

	t.Run("failures recorded after discard are re-raised", func(t *testing.T) {
		t.Parallel()

		spy := &conformanceSpyTB{TB: t}
		reporter := &recordingReporter{TB: spy}
		reporter.Errorf("consumed")
		reporter.discard()
		reporter.Errorf("late")

		reporter.reraise()

		if want := []string{"late"}; !slices.Equal(spy.errors, want) {
			t.Errorf("failures after discard, a new failure and reraise = %q, want %q", spy.errors, want)
		}
	})
}

func TestConformanceEventCollector(t *testing.T) {
	t.Parallel()

	const events = 50
	var collector eventCollector
	var wg sync.WaitGroup
	for range events {
		wg.Go(func() { collector.collect(domain.AgentEvent{Type: domain.EventNotification}) })
	}
	wg.Wait()

	snapshot := collector.snapshot()
	snapshot[0].Type = domain.EventTurnFailed

	if len(snapshot) != events {
		t.Fatalf("snapshot() holds %d events, want %d", len(snapshot), events)
	}
	if got := collector.snapshot()[0].Type; got != domain.EventNotification {
		t.Errorf("snapshot()[0].Type after editing an earlier snapshot = %q, want %q", got, domain.EventNotification)
	}
}

func TestConformanceIsolatedEnvironment(t *testing.T) {
	conformanceRegisterKinds()
	home := t.TempDir()
	binding := Binding{Kind: conformanceCommandKind, CredentialEnv: []string{"CONFORMANCE_EXTRA_KEY", conformanceDeclaredToken}}

	isolateEnvironment(t, binding, home, conformanceSentinel)

	want := map[string]string{
		"HOME":                   home,
		"XDG_CONFIG_HOME":        home,
		"XDG_DATA_HOME":          home,
		"XDG_STATE_HOME":         home,
		"XDG_CACHE_HOME":         home,
		"NO_PROXY":               "127.0.0.1,localhost",
		"no_proxy":               "127.0.0.1,localhost",
		conformanceDeclaredToken: conformanceSentinel,
		"CONFORMANCE_EXTRA_KEY":  conformanceSentinel,
	}
	for name, value := range want {
		if got := os.Getenv(name); got != value {
			t.Errorf("os.Getenv(%q) = %q, want %q", name, got, value)
		}
	}
}

func TestConformanceFatalArmChild(t *testing.T) {
	arm := os.Getenv(conformanceArmEnv)
	if arm == "" {
		t.Skip("re-executed by TestConformanceFatalArms, since a Fatalf on the real *testing.T can only be observed from another process")
	}

	conformanceRegisterKinds()
	binding := Binding{
		Kind:   conformanceCommandKind,
		Read:   ReadFile,
		Launch: func(*testing.T, Environment) Launch { return Launch{} },
	}
	switch arm {
	case "unregistered":
		binding.Kind = "conformance-unregistered"
	case "no-command":
		binding.Kind = conformancePlainKind
	case "nil-read":
		binding.Read = nil
	case "nil-launch":
		binding.Launch = nil
	case "nil-guarded":
		binding.PermissionRefusal = &PermissionRefusal{Notice: conformanceNotice}
	case "nil-notice":
		binding.PermissionRefusal = &PermissionRefusal{Guarded: CatFile}
	case "factory-error":
	default:
		t.Fatalf("unknown arm %q", arm)
	}

	AssertConformance(t, binding)
}

func TestConformanceFatalArms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		arm            string
		want           []string
		notWant        []string
		reachesFactory bool
	}{
		{
			name:    "unregistered kind",
			arm:     "unregistered",
			want:    []string{"want a registered kind"},
			notWant: []string{"requires an agent command", "Binding.Read", "Binding.Launch"},
		},
		{
			name:    "kind that requires no command",
			arm:     "no-command",
			want:    []string{"requires an agent command"},
			notWant: []string{"want a registered kind", "Binding.Read", "Binding.Launch"},
		},
		{
			name:    "nil Read",
			arm:     "nil-read",
			want:    []string{"Binding.Read is nil"},
			notWant: []string{"Binding.Kind", "Binding.Launch"},
		},
		{
			name:    "nil Launch",
			arm:     "nil-launch",
			want:    []string{"Binding.Launch is nil"},
			notWant: []string{"Binding.Kind", "Binding.Read"},
		},
		{
			name:    "nil Guarded",
			arm:     "nil-guarded",
			want:    []string{"Binding.PermissionRefusal.Guarded is nil"},
			notWant: []string{"Binding.Kind", "Binding.Read", "Binding.Launch", "Binding.PermissionRefusal.Notice"},
		},
		{
			name:    "empty Notice",
			arm:     "nil-notice",
			want:    []string{"Binding.PermissionRefusal.Notice is empty"},
			notWant: []string{"Binding.Kind", "Binding.Read", "Binding.Launch", "Binding.PermissionRefusal.Guarded"},
		},
		{
			name:           "adapter factory error",
			arm:            "factory-error",
			want:           []string{errConformanceFactory.Error(), "TestConformanceFatalArmChild/scripted_turn", "TestConformanceFatalArmChild/script_exhaustion"},
			notWant:        []string{"Binding."},
			reachesFactory: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConformanceFatalArmChild$", "-test.v") //nolint:gosec // re-executes this test binary
			cmd.Env = append(os.Environ(), conformanceArmEnv+"="+tt.arm)

			out, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("AssertConformance(arm %q) child exit = %v, want exit status 1; output:\n%s", tt.arm, err, out)
			}
			output := string(out)
			for _, fragment := range tt.want {
				if !strings.Contains(output, fragment) {
					t.Errorf("AssertConformance(arm %q) output lacks %q; output:\n%s", tt.arm, fragment, output)
				}
			}
			for _, fragment := range tt.notWant {
				if strings.Contains(output, fragment) {
					t.Errorf("AssertConformance(arm %q) output holds %q, want it absent; output:\n%s", tt.arm, fragment, output)
				}
			}
			if !tt.reachesFactory && strings.Contains(output, "/scripted_turn") {
				t.Errorf("AssertConformance(arm %q) ran a scenario, want a failure before any; output:\n%s", tt.arm, output)
			}
		})
	}
}

func TestToolFailureDiagnostics(t *testing.T) {
	t.Parallel()

	run := conformanceTurnRun()
	run.Environment.Sentinel = conformanceSentinel
	run.Exchanges[conformanceSecondTurnAt].ToolResults[0].Output = "permission denied " + conformanceSentinel + strings.Repeat("x", 3000)

	got := strings.Join(toolPathViolations(run, conformanceNonce), "\n")
	for _, want := range []string{"permission denied", "[redacted]", "[truncated]", `tool "read_file"`} {
		if !strings.Contains(got, want) {
			t.Errorf("toolPathViolations() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, conformanceSentinel) {
		t.Error("toolPathViolations() exposed the credential sentinel")
	}
	if len(got) > 2300 {
		t.Errorf("toolPathViolations() returned %d bytes, want bounded output", len(got))
	}
}
