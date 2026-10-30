package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func quotaMeta(input, output int64) json.RawMessage {
	return json.RawMessage(`{"quota":{"token_count":{"input_tokens":` + strconv.FormatInt(input, 10) +
		`,"output_tokens":` + strconv.FormatInt(output, 10) + `}}}`)
}

func measuredTurn(t *testing.T, state *sessionState, inPw interface {
	Write([]byte) (int, error)
}, out *outboundReader, meta json.RawMessage) ([]domain.AgentEvent, turnOutcome) {
	t.Helper()

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"tool_call","toolCallId":"call_001","title":"Read main.go","kind":"read","status":"pending"}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"tool_call_update","toolCallId":"call_001","status":"completed","title":"Read main.go"}}}`)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn, Meta: meta})

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
	return events, outcome
}

func TestCancelledTurnRecordsSpendOccurred(t *testing.T) {
	t.Parallel()

	// Both cases stream an update before the cancel, so the difference is which
	// kind arrived, not whether anything did; a case that saw nothing would
	// pass for the wrong reason.
	tests := []struct {
		name                 string
		update               string
		wantSpendUnaccounted bool
	}{
		{
			name:                 "a cancelled turn that streamed assistant text is spend of an unknown amount",
			update:               `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}`,
			wantSpendUnaccounted: true,
		},
		{
			name:                 "a cancelled turn that only saw a plan is not spend",
			update:               `{"sessionUpdate":"plan","entries":[{"content":"Look","priority":"high","status":"pending"}]}`,
			wantSpendUnaccounted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			ctx, cancel := context.WithCancel(context.Background())
			var events []domain.AgentEvent
			outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

			promptID := out.awaitMethod(t, methodSessionPrompt)
			sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
				tt.update+`}}`)
			awaitPumpObserved(t, state)
			cancel()
			out.awaitMethod(t, methodSessionCancel)
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})

			outcome := waittest.Receive(t, outcomeCh, "turn outcome")

			if outcome.result.SpendUnaccounted != tt.wantSpendUnaccounted {
				t.Errorf("result.SpendUnaccounted = %v, want %v",
					outcome.result.SpendUnaccounted, tt.wantSpendUnaccounted)
			}
			if outcome.result.UsageMeasured {
				t.Error("result.UsageMeasured = true, want false: unaccounted spend is not a measurement")
			}
			if countTokenUsageEvents(events) != 0 {
				t.Error("a cancelled turn emitted a token_usage event, want none")
			}
		})
	}
}

func TestNoLedgerSessionReportsSpendOccurredFromTheWireExtension(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	events, outcome := measuredTurn(t, state, inPw, out, quotaMeta(900, 30))

	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if !outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = false, want true when the runtime reported spend and nothing measured it")
	}
}

func TestNoLedgerSessionEndingWithoutAReplyReportsSpendOccurred(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	var events []domain.AgentEvent
	_, outcomeCh := modelReachedTurn(t, state, inPw, out, &events)
	inPw.Close() //nolint:errcheck,gosec // ending the simulated stream

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Fatalf("runTurn() error = %v, want *domain.AgentError kind %q", outcome.err, domain.ErrPortExit)
	}
	agenttest.AssertMeasurementAbsent(t, events, outcome.result)
	if !outcome.result.SpendUnaccounted {
		t.Error("result.SpendUnaccounted = false, want true: the turn reached the model and nothing measured it")
	}
}

// modelReachedMarker is distinctive so a barrier keying on it cannot be
// satisfied by the capability notice, which carries the same event type.
const modelReachedMarker = "model-reached"

// The pump records that the turn reached the model before it emits a chunk's
// liveness notification, so seeing that event means the record is set.
func chunkBarrier(events *[]domain.AgentEvent) (func(domain.AgentEvent), <-chan struct{}) {
	reached := make(chan struct{})
	var once sync.Once
	collect := collectEvents(events)
	return func(ev domain.AgentEvent) {
		collect(ev)
		if ev.Type == domain.EventNotification && ev.Message == "" {
			once.Do(func() { close(reached) })
		}
	}, reached
}

func sendModelReachedChunk(t *testing.T, w io.Writer) {
	t.Helper()
	sendLine(t, w, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":`+
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"`+modelReachedMarker+`"}}}}`)
}

func modelReachedTurn(t *testing.T, state *sessionState, inPw io.Writer, out *outboundReader, events *[]domain.AgentEvent) (json.RawMessage, <-chan turnOutcome) {
	t.Helper()

	onEvent, reached := chunkBarrier(events)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: onEvent})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendModelReachedChunk(t, inPw)
	waittest.Receive(t, reached, "the turn's assistant chunk to reach the pump")
	return promptID, outcomeCh
}

// The inbox's single order puts the pump's answer to this control message
// behind every item queued before it.
func awaitPumpObserved(t *testing.T, state *sessionState) {
	t.Helper()

	done := make(chan struct{})
	state.inbox.Put(pumpItem{control: &pumpControl{answerOpen: done}})
	waittest.Receive(t, done, "the pump to drain its inbox")
}

func countTokenUsageEvents(events []domain.AgentEvent) int {
	count := 0
	for _, event := range events {
		if event.Type == domain.EventTokenUsage {
			count++
		}
	}
	return count
}

func launchedBy(executable string) func(agentcore.LaunchTarget) bool {
	return func(target agentcore.LaunchTarget) bool {
		tokens := append([]string{target.Command}, target.Args...)
		return slices.Contains(append(tokens, strings.Fields(target.RemoteCommand)...), executable)
	}
}

func fixtureDeclarations(t *testing.T) *registry.Registry[usagesource.Declaration, struct{}] {
	t.Helper()

	declarations := registry.NewRegistry[usagesource.Declaration, struct{}]("usage declaration")
	windowsOnly := usagesource.Declaration{Live: usage.Source{Category: usage.SourceProtocol}}
	for name, launches := range map[string]func(agentcore.LaunchTarget) bool{
		"alpha": launchedBy("alpha-cli"),
		"beta":  launchedBy("beta-cli"),
		"gamma": nil,
	} {
		declaration := windowsOnly
		declaration.Launches = launches
		declarations.Register(name, declaration)
	}
	return declarations
}

func TestChooseDeclaration(t *testing.T) {
	t.Parallel()

	declarations := fixtureDeclarations(t)
	local := agentcore.LaunchTarget{Command: "alpha-cli", Args: []string{"--acp"}}
	tests := []struct {
		name           string
		info           *implementation
		target         agentcore.LaunchTarget
		wantKey        string
		wantConfigured string
		wantMismatch   bool
	}{
		{"handshake confirms the command", &implementation{Name: "alpha"}, local, "alpha", "alpha", false},
		{"handshake names another declared runtime", &implementation{Name: "beta"}, local, "beta", "alpha", true},
		{"handshake names a declaration with no launch matcher", &implementation{Name: "gamma"}, local, "gamma", "alpha", true},
		{"handshake names an undeclared runtime", &implementation{Name: "undeclared-runtime"}, local, usagesource.DefaultName, "alpha", true},
		{"no handshake name keeps the command's declaration", nil, local, "alpha", "alpha", false},
		{"empty handshake name keeps the command's declaration", &implementation{}, local, "alpha", "alpha", false},
		{"no name and a command naming nothing", nil, agentcore.LaunchTarget{Command: "agent"}, usagesource.DefaultName, "", false},
		{"undeclared name and a command naming nothing", &implementation{Name: "undeclared-runtime"}, agentcore.LaunchTarget{Command: "agent"}, usagesource.DefaultName, "", false},
		{"wrapper command", nil, agentcore.LaunchTarget{Command: "env", Args: []string{"MODEL=x", "alpha-cli", "--acp"}}, "alpha", "alpha", false},
		{"remote target whose command is ssh", nil, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "beta-cli acp --model x"}, "beta", "beta", false},
		{"remote target naming nothing", nil, agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent --acp"}, usagesource.DefaultName, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chooseDeclaration(declarations, tt.info, tt.target)

			if got.key != tt.wantKey || got.configured != tt.wantConfigured || got.mismatch != tt.wantMismatch {
				t.Errorf("chooseDeclaration(%+v, %+v) = {key %q, configured %q, mismatch %v}, want {%q, %q, %v}",
					tt.info, tt.target, got.key, got.configured, got.mismatch, tt.wantKey, tt.wantConfigured, tt.wantMismatch)
			}
			if got.declaration.Live.Spend.Shape == usage.SpendUndeclared || got.declaration.PromptSpend == nil {
				t.Errorf("chooseDeclaration(%+v, %+v) declaration maps no spend, want the default mapping where the selected one maps none", tt.info, tt.target)
			}
		})
	}
}

func TestNewSessionLedgerReadsRecordsOnEveryLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    agentcore.LaunchTarget
		created   bool
		wantReads int
		wantOK    bool
	}{
		{"local continued session", agentcore.LaunchTarget{Command: "agent"}, false, 1, true},
		{"local created session", agentcore.LaunchTarget{Command: "agent"}, true, 0, true},
		{"remote continued session", agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent"}, false, 1, true},
		{"remote created session", agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent"}, true, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reads int
			decl := scriptedSpendDeclaration(nil, nil, nil)
			decl.ReadRecords = func(context.Context, usagesource.PullRuntime) (usagesource.Records, error) {
				reads++
				return usagesource.Records{Spend: []usage.SpendReading{spendReading(10, 2, 0, "model-a", "u1")}}, nil
			}
			state := &sessionState{logger: discardLogger(), target: tt.target}
			live := spendReading(100, 20, 0, "", "")

			ledger := newSessionLedger(t.Context(), state, decl, tt.created, nil)

			if ledger == nil {
				t.Fatalf("newSessionLedger(%+v) = nil, want a ledger built from the declaration's live and records sources", tt.target)
			}
			if reads != tt.wantReads {
				t.Errorf("newSessionLedger(%+v) ran ReadRecords %d times, want %d", tt.target, reads, tt.wantReads)
			}
			settlement := ledger.Settle(usage.TurnEnd{Replied: true, ModelReached: true}, &live, usage.RecordsRead{OK: true})
			if !settlement.Figure || settlement.Run.InputTokens != 100 || settlement.Run.OutputTokens != 20 {
				t.Errorf("Settle() = {Figure %v, Run %+v}, want the live figure 100/20", settlement.Figure, settlement.Run)
			}

			history := usage.RecordsRead{OK: true, Readings: []usage.SpendReading{spendReading(10, 2, 0, "model-a", "u1")}}
			boundary := newSessionLedger(t.Context(), state, decl, tt.created, nil).Settle(usage.TurnEnd{Replied: true, ModelReached: true}, nil, history)
			if boundary.Figure != tt.created {
				t.Errorf("newSessionLedger(%+v, created %v) counts a record that is on the host at session start = %v, want %v", tt.target, tt.created, boundary.Figure, tt.created)
			}

			read := readSessionRecords(t.Context(), state, decl)

			if read.OK != tt.wantOK || (len(read.Readings) != 0) != tt.wantOK {
				t.Errorf("readSessionRecords(%+v) = {OK %v, %d readings}, want OK %v", tt.target, read.OK, len(read.Readings), tt.wantOK)
			}
		})
	}
}

func TestChildSessionSpendSettlesTheTurn(t *testing.T) {
	t.Parallel()

	start := usagesource.ChildSignal{Call: "c1", Started: true}
	done := usagesource.ChildSignal{Call: "c1", Session: "child-1", Completed: true}
	both := domain.TokenUsage{InputTokens: 150, OutputTokens: 15, TotalTokens: 165}
	tests := []struct {
		name            string
		signals         []usagesource.ChildSignal
		target          agentcore.LaunchTarget
		wantUnaccounted bool
		want            domain.TokenUsage
	}{
		{"completed child counted", []usagesource.ChildSignal{start, done}, agentcore.LaunchTarget{Command: "agent"}, false, both},
		{"started child never completed", []usagesource.ChildSignal{start, {Session: "child-1"}}, agentcore.LaunchTarget{Command: "agent"}, true, both},
		{"remote turn that ran a child", []usagesource.ChildSignal{start, done}, sshLaunch("agent"), false, both},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			live := spendReading(100, 10, 0, "", "")
			declaration := scriptedSpendDeclaration(&live, nil, nil)
			declaration.ChildSession = func(_ string, update json.RawMessage) usagesource.ChildSignal {
				var decoded struct{ Signal usagesource.ChildSignal }
				_ = json.Unmarshal(update, &decoded)
				return decoded.Signal
			}
			declaration.ReadRecords = func(_ context.Context, rt usagesource.PullRuntime) (usagesource.Records, error) {
				if rt.SessionID() == "child-1" {
					return usagesource.Records{Spend: []usage.SpendReading{spendReading(50, 5, 0, "model-b", "c1")}}, nil
				}
				return usagesource.Records{Spend: []usage.SpendReading{spendReading(100, 10, 0, "model-a", "p1")}}, nil
			}
			ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
			if err != nil {
				t.Fatalf("usage.NewLedger() error = %v, want nil", err)
			}
			ledger.SessionCreated()
			state, out, inPw := newDeclaredSession(t, declaration, ledger)
			state.target = tt.target

			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
			promptID := out.awaitMethod(t, methodSessionPrompt)
			for _, signal := range tt.signals {
				body, _ := json.Marshal(signal)
				sendLine(t, inPw, fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"},"signal":%s}}}`, body))
			}
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
			result := waittest.Receive(t, outcomeCh, "turn outcome").result

			if result.Usage != tt.want || result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("RunTurn() = {Usage %+v, SpendUnaccounted %v}, want {%+v, %v}", result.Usage, result.SpendUnaccounted, tt.want, tt.wantUnaccounted)
			}
		})
	}
}
