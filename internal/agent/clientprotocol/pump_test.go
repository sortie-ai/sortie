package clientprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func TestRefusalNoticesUseSharedNotificationEmitter(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "pump.go", nil, 0)
	if err != nil {
		t.Fatalf("parser.ParseFile(%q) error = %v", "pump.go", err)
	}

	wantFunctions := map[string]bool{
		"handlePermissionRequest": false,
		"answerMethodNotFound":    false,
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, wanted := wantFunctions[fn.Name.Name]; !wanted {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "EmitNotification" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if ok && pkg.Name == "agentcore" {
				wantFunctions[fn.Name.Name] = true
			}
			return true
		})
	}

	for function, found := range wantFunctions {
		if !found {
			t.Errorf("%s() calls agentcore.EmitNotification = false, want true", function)
		}
	}
}

func startTurnAwaitingPrompt(t *testing.T, state *sessionState, out *outboundReader) <-chan turnOutcome {
	t.Helper()
	markSessionKnown(state)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: func(domain.AgentEvent) {}})
	out.awaitMethod(t, methodSessionPrompt)
	return outcomeCh
}

func TestPumpStreamEndCleanExit(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)
	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{
		Prompt: "do something",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial assistant message"},"messageId":"message-1"}}}`)

	if err := inPw.Close(); err != nil {
		t.Fatalf("close the simulated agent stream: %v", err)
	}

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	}, outcome.result, outcome.err)

	emptyNotifications := 0
	completeMessages := 0
	for _, event := range events {
		if event.Type != domain.EventNotification {
			continue
		}
		switch event.Message {
		case "":
			emptyNotifications++
		case "partial assistant message":
			completeMessages++
		}
	}
	if emptyNotifications != 1 {
		t.Errorf("empty liveness notifications = %d, want 1", emptyNotifications)
	}
	if completeMessages != 1 {
		t.Errorf("completed message notifications = %d, want 1", completeMessages)
	}
}

func TestPumpStreamEndReadFailure(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	if err := inPw.CloseWithError(errors.New("simulated read failure")); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	}, outcome.result, outcome.err)
}

func TestFinalizeStreamEndLineBound(t *testing.T) {
	t.Parallel()

	const smallLineBound = 256

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, smallLineBound)
	out := newOutboundReader(outPr)
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	oversized := make([]byte, smallLineBound*4)
	for i := range oversized {
		oversized[i] = 'a'
	}
	oversized = append(oversized, '\n')
	// The scanner stops reading past the bound, so a synchronous Write would
	// block forever.
	go func() { _, _ = inPw.Write(oversized) }()

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
		TerminalMessage:   lineTooLongMessage,
	}, outcome.result, outcome.err)

	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("RunTurn() error = %v, want *domain.AgentError", outcome.err)
	}
	if agentErr.Kind == domain.ErrPortExit {
		t.Errorf("RunTurn() error kind = %q, want anything but the retryable %q", agentErr.Kind, domain.ErrPortExit)
	}
}

func TestPumpDispatchNullID(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":null,"method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"a null-id update"}}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","id":4242,"method":"fs/read_text_file","params":{}}`)

	line := out.next(t)
	assertRawID(t, line, "4242")
	resp := decodeResponse(t, line)
	if resp.Error == nil || resp.Error.Code != jsonrpc.MethodNotFoundCode {
		t.Fatalf("response to the sentinel request = %+v, want a %d error; a different id or a decoded outcome here means the null-id session/update was answered instead of normalized", resp, jsonrpc.MethodNotFoundCode)
	}
}

func TestSessionUpdateBeforeAnyPromptNotLost(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"queued before any turn"}}}}`)

	eventCh := make(chan domain.AgentEvent, 16)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{
		Prompt:  "go",
		OnEvent: func(e domain.AgentEvent) { eventCh <- e },
	})

	first := waittest.Receive(t, eventCh, "an agent event")
	if first.Type != domain.EventSessionStarted {
		t.Fatalf("first event type = %q, want %q", first.Type, domain.EventSessionStarted)
	}
	second := waittest.Receive(t, eventCh, "an agent event")
	if second.Type != domain.EventNotification {
		t.Fatalf("second event type = %q, want %q (the once-per-session capability notice)", second.Type, domain.EventNotification)
	}
	third := waittest.Receive(t, eventCh, "an agent event")
	if third.Type != domain.EventNotification || third.Message != "" {
		t.Fatalf("third event = %+v, want the queued chunk's liveness notification", third)
	}
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
	fourth := waittest.Receive(t, eventCh, "an agent event")
	if fourth.Type != domain.EventNotification || fourth.Message != "queued before any turn" {
		t.Fatalf("fourth event = %+v, want the queued message", fourth)
	}
	if !fourth.Timestamp.Equal(third.Timestamp) {
		t.Errorf("queued message timestamp = %v, want %v, when its chunk arrived before the turn", fourth.Timestamp, third.Timestamp)
	}
}

func TestAnsweredIDEcho(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		rawID  string
	}{
		{"session/request_permission with a string id", methodSessionRequestPermission, `"perm-str-1"`},
		{"session/request_permission with the number zero", methodSessionRequestPermission, `0`},
		{"session/request_permission with a null id", methodSessionRequestPermission, `null`},
		{"an unimplemented method with a string id", "fs/read_text_file", `"nf-str-1"`},
		{"an unimplemented method with the number zero", "fs/read_text_file", `0`},
		{"an unimplemented method with a null id", "fs/read_text_file", `null`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			params := `{}`
			if tt.method == methodSessionRequestPermission {
				params = `{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"work"}}`
			}
			sendLine(t, inPw, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, tt.rawID, tt.method, params))

			line := out.next(t)
			assertRawID(t, line, tt.rawID)
			resp := decodeResponse(t, line)

			if tt.method == methodSessionRequestPermission {
				if resp.Error != nil {
					t.Fatalf("response = %+v, want a successful permission reply", resp)
				}
				if resp.Result.Outcome.Outcome != outcomeSelected || resp.Result.Outcome.OptionID != "reject-id" {
					t.Errorf("permission reply = %+v, want outcome %q with optionId %q", resp.Result.Outcome, outcomeSelected, "reject-id")
				}
				return
			}
			if resp.Error == nil || resp.Error.Code != jsonrpc.MethodNotFoundCode {
				t.Errorf("response = %+v, want a %d error for an unimplemented method", resp, jsonrpc.MethodNotFoundCode)
			}
		})
	}
}

func TestToolDeliveryReportSkippedDuringWindDownAndTeardown(t *testing.T) {
	t.Parallel()

	t.Run("a turn winding down toward cancellation answers a permission request without reporting", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
		out := newOutboundReader(outPr)
		state.inbox.Put(pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}})
		markSessionKnown(state)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var events []domain.AgentEvent
		outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

		promptID := out.awaitMethod(t, methodSessionPrompt)
		cancel()
		out.awaitMethod(t, methodSessionCancel)

		sendLine(t, inPw, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`)

		respLine := out.next(t)
		resp := decodeResponse(t, respLine)
		if resp.Result.Outcome.Outcome != outcomeCancelled {
			t.Errorf("permission reply outcome during wind-down = %q, want %q", resp.Result.Outcome.Outcome, outcomeCancelled)
		}

		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})
		waittest.Receive(t, outcomeCh, "turn outcome")

		if got := countToolDeliveryNotifications(events); got != 0 {
			t.Errorf("tool delivery uncallable notifications during wind-down = %d, want 0", got)
		}
		if strings.Contains(buf.String(), toolDeliveryUncallableLog) {
			t.Errorf("unexpected tool delivery uncallable Warn record during wind-down: %s", buf.String())
		}
	})

	// A live pump never leaves an openRequests entry behind, so this path needs a
	// pumpState constructed with one.
	t.Run("handleAnswerOpen answers a still-open permission request without reporting", func(t *testing.T) {
		t.Parallel()

		outPr, outPw := io.Pipe()
		inPr, _ := io.Pipe()
		var buf bytes.Buffer
		state := &sessionState{
			caps:   newCapabilityRecord(),
			stopCh: make(chan struct{}),
			logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
		state.inbox = jsonrpc.NewInbox[pumpItem]()
		state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
			jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))
		t.Cleanup(func() {
			_ = outPr.Close()
			_ = outPw.Close()
			_ = inPr.Close()
		})
		go func() { _, _ = io.Copy(io.Discard, outPr) }()

		p := &pumpState{
			state:                state,
			toolServersDelivered: true,
			openRequests:         map[jsonrpc.ID]string{jsonrpc.NumberID(1): methodSessionRequestPermission},
		}

		p.handleAnswerOpen()

		if len(p.openRequests) != 0 {
			t.Errorf("openRequests after handleAnswerOpen() = %+v, want empty", p.openRequests)
		}
		if len(p.queued) != 0 {
			t.Errorf("events queued by handleAnswerOpen() = %+v, want none", p.queued)
		}
		if strings.Contains(buf.String(), toolDeliveryUncallableLog) {
			t.Errorf("unexpected tool delivery uncallable Warn record from handleAnswerOpen(): %s", buf.String())
		}
	})
}

func TestHandshakeToolServersDeliveredReachesPump(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)

	state.inbox.Put(pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}})
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`)
	out.next(t)

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	if got := countToolDeliveryNotifications(events); got != 1 {
		t.Errorf("tool delivery uncallable notifications = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), toolDeliveryUncallableLog) {
		t.Errorf("log output missing %q: %s", toolDeliveryUncallableLog, buf.String())
	}
}

// stalledConsumerBurstCount is well past any bounded hand-off's capacity.
const stalledConsumerBurstCount = 4096

const stalledConsumerChunkAlphabet = "!#$%&*+/:;<=>?@^~|"

// Chunks avoid every character a registered secret can start with: parallel
// tests register hex bearers process-wide, and truncating an aggregate drops a
// trailing fragment that prefixes a registered value.
func stalledConsumerChunkText(i int) string {
	var text strings.Builder
	base := len(stalledConsumerChunkAlphabet)
	for n := i; ; n /= base {
		text.WriteByte(stalledConsumerChunkAlphabet[n%base])
		if n < base {
			return text.String()
		}
	}
}

func TestRunTurn_StalledConsumerDoesNotParkPump(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	gate := make(chan struct{})
	var mu sync.Mutex
	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{
		Prompt: "go",
		OnEvent: func(e domain.AgentEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
			// Only a burst chunk's liveness event holds the gate; the session-started
			// event and capability notice must arrive before the prompt is sent.
			if e.Type == domain.EventNotification && e.Message == "" {
				<-gate
			}
		},
	})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	var wantText strings.Builder
	chunkTexts := make(map[string]struct{}, stalledConsumerBurstCount)
	for i := range stalledConsumerBurstCount {
		chunkText := stalledConsumerChunkText(i)
		line := fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}}}`, chunkText)
		wantText.WriteString(chunkText)
		chunkTexts[chunkText] = struct{}{}
		errCh := make(chan error, 1)
		go func() {
			_, writeErr := fmt.Fprintln(inPw, line)
			errCh <- writeErr
		}()
		if writeErr := waittest.Receive(t, errCh, fmt.Sprintf("peer write %d while the OnEvent gate was held, with the reader never parked behind the pump's own stalled delivery", i)); writeErr != nil {
			t.Fatalf("write session/update chunk %d: %v", i, writeErr)
		}
	}
	close(gate)

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	var emptyNotifications int
	var partialChunks int
	var completeMessages int
	wantRunes := []rune(wantText.String())
	wantMessage := string(wantRunes[:messageTruncateLimit]) + "…"
	for _, e := range events {
		if e.Type != domain.EventNotification {
			continue
		}
		switch e.Message {
		case "":
			emptyNotifications++
		case wantMessage:
			completeMessages++
		default:
			if _, isChunk := chunkTexts[e.Message]; isChunk {
				partialChunks++
			}
		}
	}
	if emptyNotifications != stalledConsumerBurstCount {
		t.Errorf("empty liveness notifications = %d, want %d", emptyNotifications, stalledConsumerBurstCount)
	}
	if completeMessages != 1 {
		t.Errorf("completed message notifications = %d, want 1 carrying the truncated aggregate", completeMessages)
	}
	if partialChunks != 0 {
		t.Errorf("partial chunk notifications = %d, want 0", partialChunks)
	}
}

func TestPumpAgentMessageChunksAreCoalesced(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{
		Prompt: "go",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello "},"messageId":"message-1"}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"usage_update","used":12000,"size":200000}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"world"},"messageId":"message-1"}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Next"},"messageId":"message-2"}}}`)
	sendLine(t, inPw, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"plan","entries":[]}}}`)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	if outcome := waittest.Receive(t, outcomeCh, "turn outcome"); outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	var chunkNotifications []string
	for _, event := range events {
		isChunk := event.Type == domain.EventNotification && (event.Message == "" || event.Message == "hello world" || event.Message == "Next")
		if isChunk || event.Type == domain.EventOtherMessage {
			chunkNotifications = append(chunkNotifications, event.Message)
		}
	}
	want := []string{"", "", "hello world", "", "Next", ""}
	if len(chunkNotifications) != len(want) {
		t.Fatalf("chunk notifications = %q, want %q", chunkNotifications, want)
	}
	for i := range want {
		if chunkNotifications[i] != want[i] {
			t.Errorf("chunk notification %d = %q, want %q", i, chunkNotifications[i], want[i])
		}
	}
}

func loadAgentInfoFixture(t *testing.T) *implementation {
	t.Helper()
	data, err := os.ReadFile("testdata/agent_info/pulled_usage_agent.json")
	if err != nil {
		t.Fatalf("loadAgentInfoFixture(): %v", err)
	}
	var info implementation
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("loadAgentInfoFixture(): %v", err)
	}
	return &info
}

func declarationFor(info *implementation, target agentcore.LaunchTarget) usagesource.Declaration {
	return chooseDeclaration(usagesource.Declarations, info, target).declaration
}

func windowEvents(events []domain.AgentEvent) []domain.AgentEvent {
	var withWindows []domain.AgentEvent
	for _, e := range events {
		if len(e.UsageWindows) > 0 {
			withWindows = append(withWindows, e)
		}
	}
	return withWindows
}

const pulledUsageChunkLine = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ok"},"messageId":"message-1"}}}`

type usageReply struct {
	method  string
	fixture string
}

func loadPulledUsageFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/pulled_usage/" + name)
	if err != nil {
		t.Fatalf("loadPulledUsageFixture(%q): %v", name, err)
	}
	return json.RawMessage(data)
}

type pulledUsageRuntime struct {
	Launch            agentcore.LaunchTarget
	RemoteLaunches    []string
	OtherRemoteLaunch string
	Execute           string
	GetUsage          string
}

func sshLaunch(remoteCommand string) agentcore.LaunchTarget {
	return agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: remoteCommand}
}

func loadPulledUsageRuntime(t *testing.T) pulledUsageRuntime {
	t.Helper()
	var runtime pulledUsageRuntime
	if err := json.Unmarshal(loadPulledUsageFixture(t, "runtime.json"), &runtime); err != nil {
		t.Fatalf("loadPulledUsageRuntime: %v", err)
	}
	return runtime
}

func newPulledUsageSession(t *testing.T, info *implementation, target agentcore.LaunchTarget) (*sessionState, *outboundReader, *io.PipeWriter) {
	t.Helper()
	return newDeclaredSession(t, declarationFor(info, target), nil)
}

func newDeclaredSession(t *testing.T, declaration usagesource.Declaration, ledger *usage.Ledger) (*sessionState, *outboundReader, *io.PipeWriter) {
	t.Helper()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	state.sessionID = "sess-test"
	facts := &handshakeFacts{declaration: declaration, puller: newSessionPuller(declaration, state, false), ledger: ledger}
	state.inbox.Put(pumpItem{control: &pumpControl{handshake: facts}})
	markSessionKnown(state)
	return state, newOutboundReader(outPr), inPw
}

func answerUsageRequests(t *testing.T, out *outboundReader, inPw io.Writer, replies []usageReply) {
	t.Helper()
	for _, reply := range replies {
		id := out.awaitMethod(t, reply.method)
		if reply.fixture == "" {
			respondErrorLine(t, inPw, id, jsonrpc.MethodNotFoundCode, "method not found")
			continue
		}
		respondLine(t, inPw, id, loadPulledUsageFixture(t, reply.fixture))
	}
}

func runPulledUsageTurn(t *testing.T, state *sessionState, out *outboundReader, inPw io.Writer, replies ...usageReply) []domain.AgentEvent {
	t.Helper()
	events, _ := runPulledUsageTurnResult(t, state, out, inPw, replies...)
	return events
}

func runPulledUsageTurnResult(t *testing.T, state *sessionState, out *outboundReader, inPw io.Writer, replies ...usageReply) ([]domain.AgentEvent, domain.TurnResult) {
	t.Helper()

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	sendLine(t, inPw, pulledUsageChunkLine)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	answerUsageRequests(t, out, inPw, replies)

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
	return events, outcome.result
}

func assertNoOutboundLine(t *testing.T, out *outboundReader) {
	t.Helper()
	select {
	case line := <-out.ch:
		t.Errorf("outbound line after the turn = %s, want none", line)
	default:
	}
}

func assertMonthlyCreditWindowBeforeMessage(t *testing.T, events []domain.AgentEvent) {
	t.Helper()

	var windows []domain.UsageWindow
	windowAt, messageAt, endAt, messages := -1, -1, -1, 0
	for i, e := range events {
		switch {
		case len(e.UsageWindows) > 0:
			windows = append(windows, e.UsageWindows...)
			windowAt = i
		case e.Type == domain.EventNotification && e.Message == "ok":
			messages++
			messageAt = i
		case e.Type == domain.EventTurnCompleted:
			endAt = i
		}
	}

	if len(windows) != 1 {
		t.Fatalf("pulled windows = %+v, want exactly one", windows)
	}
	window := windows[0]
	if window.Label != "Monthly limit" {
		t.Errorf("window Label = %q, want %q", window.Label, "Monthly limit")
	}
	if want := (domain.UsageWindowAmount{Used: 7.08, Limit: 1000, Unit: "credits"}); window.Amount == nil || *window.Amount != want {
		t.Errorf("window Amount = %+v, want %+v", window.Amount, want)
	}
	if want := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC); !window.ResetsAt.Equal(want) {
		t.Errorf("window ResetsAt = %v, want %v", window.ResetsAt, want)
	}
	if messages != 1 {
		t.Errorf("agent message notifications = %d, want 1", messages)
	}
	if windowAt >= messageAt || messageAt >= endAt {
		t.Errorf("event positions window=%d message=%d turn end=%d, want window < message < turn end", windowAt, messageAt, endAt)
	}
}

func TestPumpPulledUsage(t *testing.T) {
	t.Parallel()

	runtime := loadPulledUsageRuntime(t)

	pulledAgent := loadAgentInfoFixture(t)
	fallbackReplies := []usageReply{{method: runtime.Execute}, {method: runtime.GetUsage, fixture: "get_usage_result.json"}}
	tests := []struct {
		name       string
		info       *implementation
		target     agentcore.LaunchTarget
		replies    []usageReply
		wantWindow bool
	}{
		{
			name:       "the engine named at handshake answers its usage command",
			info:       pulledAgent,
			replies:    []usageReply{{method: runtime.Execute, fixture: "execute_result.json"}},
			wantWindow: true,
		},
		{name: "a nameless handshake and a local launch", target: runtime.Launch, replies: fallbackReplies, wantWindow: true},
		{name: "a nameless handshake and an ssh launch of the bare executable", target: sshLaunch(runtime.RemoteLaunches[0]), replies: fallbackReplies, wantWindow: true},
		{name: "a nameless handshake and an ssh launch of the executable by path", target: sshLaunch(runtime.RemoteLaunches[1]), replies: fallbackReplies, wantWindow: true},
		{name: "a nameless handshake and an ssh launch of another runtime", target: sshLaunch(runtime.OtherRemoteLaunch)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, out, inPw := newPulledUsageSession(t, tt.info, tt.target)

			events := runPulledUsageTurn(t, state, out, inPw, tt.replies...)

			if !tt.wantWindow {
				assertNoOutboundLine(t, out)
				if got := len(windowEvents(events)); got != 0 {
					t.Errorf("events carrying windows for %+v = %d, want 0", tt.target, got)
				}
				return
			}
			assertMonthlyCreditWindowBeforeMessage(t, events)
		})
	}

	t.Run("a runtime that knows neither method is not asked again", func(t *testing.T) {
		t.Parallel()

		state, out, inPw := newPulledUsageSession(t, loadAgentInfoFixture(t), agentcore.LaunchTarget{})

		first := runPulledUsageTurn(t, state, out, inPw, usageReply{method: runtime.Execute}, usageReply{method: runtime.GetUsage})
		second := runPulledUsageTurn(t, state, out, inPw)

		assertNoOutboundLine(t, out)
		if got := len(windowEvents(slices.Concat(first, second))); got != 0 {
			t.Errorf("events carrying windows = %d, want 0", got)
		}
	})

	t.Run("an unanswered usage request keeps the turn's outcome", func(t *testing.T) {
		t.Parallel()

		synctest.Test(t, func(t *testing.T) {
			state, out, inPw := newPulledUsageSession(t, loadAgentInfoFixture(t), agentcore.LaunchTarget{})
			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})

			promptID := out.awaitMethod(t, methodSessionPrompt)
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
			out.awaitMethod(t, runtime.Execute)

			outcome := waittest.Receive(t, outcomeCh, "turn outcome")
			if outcome.err != nil {
				t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
			}
			if outcome.result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("RunTurn() exit reason = %q, want %q", outcome.result.ExitReason, domain.EventTurnCompleted)
			}
		})
	})

	t.Run("a cancelled turn sends no usage request", func(t *testing.T) {
		t.Parallel()

		state, out, inPw := newPulledUsageSession(t, loadAgentInfoFixture(t), agentcore.LaunchTarget{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var events []domain.AgentEvent
		outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})

		promptID := out.awaitMethod(t, methodSessionPrompt)
		cancel()
		out.awaitMethod(t, methodSessionCancel)
		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})
		waittest.Receive(t, outcomeCh, "turn outcome")

		assertNoOutboundLine(t, out)
		if got := len(windowEvents(events)); got != 0 {
			t.Errorf("events carrying windows = %d, want 0", got)
		}
	})

	t.Run("a turn ended for human input starts no pull", func(t *testing.T) {
		t.Parallel()

		var pulls atomic.Int32
		turn := newActiveTurnForDirectDispatch()
		turn.pendingEnd = turnEndHumanInput
		p := &pumpState{
			state: &sessionState{
				logger: discardLogger(),
				stopCh: make(chan struct{}),
				inbox:  jsonrpc.NewInbox[pumpItem](),
			},
			activeTurn: turn,
			puller: usage.NewPuller(usage.Source{}, time.Second, false, func(context.Context) ([]usage.WindowReading, error) {
				pulls.Add(1)
				return nil, nil
			}),
		}

		p.armPull(turn)
		p.launchPull(turn)

		if p.pullInFlight || turn.pullPending {
			t.Errorf("armPull() with a human-input end left pullInFlight = %v, pullPending = %v, want both false", p.pullInFlight, turn.pullPending)
		}
		if got := pulls.Load(); got != 0 {
			t.Errorf("armPull() with a human-input end ran %d pulls, want 0", got)
		}
	})

	t.Run("a stop while the pull is in flight still ends the turn", func(t *testing.T) {
		t.Parallel()

		pulling := make(chan struct{}, 1)
		pullCtx := waittest.Context(t)
		declaration := usagesource.Declaration{
			PullWindows: func(context.Context, usagesource.PullRuntime) ([]usage.WindowReading, error) {
				pulling <- struct{}{}
				<-pullCtx.Done()
				return nil, nil
			},
		}
		state, out, inPw := newDeclaredSession(t, declaration, nil)
		outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
		promptID := out.awaitMethod(t, methodSessionPrompt)
		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})
		waittest.Receive(t, pulling, "usage pull start")
		_ = inPw.Close()

		if err := stopSession(t.Context(), fakeSession(state)); err != nil {
			t.Fatalf("stopSession() = %v, want nil", err)
		}

		waittest.Receive(t, outcomeCh, "turn outcome after the stop")
	})
}

func spendReading(input, output, cacheRead int64, model, identity string) usage.SpendReading {
	cacheWrite := int64(0)
	return usage.SpendReading{Input: &input, Output: &output, CacheRead: &cacheRead, CacheWrite: &cacheWrite, Model: model, Identity: identity}
}

func scriptedSpendDeclaration(live *usage.SpendReading, records []usage.SpendReading, readErr error) usagesource.Declaration {
	return usagesource.Declaration{
		Live: usage.Source{
			Strategy: usage.StrategyLive,
			Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, CacheInInput: true, ReasoningInOutput: true},
		},
		PromptSpend: func(json.RawMessage) *usage.SpendReading { return live },
		Records: usage.Source{
			Strategy: usage.StrategyRecords,
			Spend:    usage.SpendDeclaration{Shape: usage.SpendPerRequest, CacheInInput: true, ReasoningInOutput: true, ServedModel: true},
		},
		ReadRecords: func(context.Context, usagesource.PullRuntime) (usagesource.Records, error) {
			return usagesource.Records{Spend: records}, readErr
		},
	}
}

func mustLedger(t *testing.T, declaration usagesource.Declaration, boundary usage.RecordsRead) *usage.Ledger {
	t.Helper()
	ledger, err := usage.NewLedger(declaration.Live, declaration.Records)
	if err != nil {
		t.Fatalf("usage.NewLedger() error = %v, want nil", err)
	}
	ledger.Boundary(boundary)
	return ledger
}

func TestPumpSpendLedgerSettlesTurn(t *testing.T) {
	t.Parallel()

	liveReading := spendReading(1000, 70, 400, "", "")
	tests := []struct {
		name            string
		live            *usage.SpendReading
		records         []usage.SpendReading
		readErr         error
		wantUnaccounted bool
		wantUsage       *domain.TokenUsage
		wantByModel     map[string]domain.TokenUsage
		wantModel       string
		wantTokenCounts capabilityState
	}{
		{
			name:            "no live reading and a failed records read",
			readErr:         errors.New("journal unreadable"),
			wantUnaccounted: true,
			wantTokenCounts: capabilityGap,
		},
		{
			name: "a live reading and readable records",
			live: &liveReading,
			records: []usage.SpendReading{
				spendReading(600, 30, 200, "model-a", "u1"),
				spendReading(400, 40, 200, "model-b", "u2"),
			},
			wantUsage: &domain.TokenUsage{InputTokens: 1000, OutputTokens: 70, TotalTokens: 1070, CacheReadTokens: 400},
			wantByModel: map[string]domain.TokenUsage{
				"model-a": {InputTokens: 600, OutputTokens: 30, TotalTokens: 630, CacheReadTokens: 200},
				"model-b": {InputTokens: 400, OutputTokens: 40, TotalTokens: 440, CacheReadTokens: 200},
			},
			wantModel:       "model-a",
			wantTokenCounts: capabilityOurs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration := scriptedSpendDeclaration(tt.live, tt.records, tt.readErr)
			state, out, inPw := newDeclaredSession(t, declaration, mustLedger(t, declaration, usage.RecordsRead{OK: true}))

			events, result := runPulledUsageTurnResult(t, state, out, inPw)

			var usageEvents []domain.AgentEvent
			usageAt, endAt := -1, -1
			for i, e := range events {
				switch e.Type {
				case domain.EventTokenUsage:
					usageEvents = append(usageEvents, e)
					usageAt = i
				case domain.EventTurnCompleted:
					endAt = i
				}
			}
			if result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("RunTurn() SpendUnaccounted = %v, want %v", result.SpendUnaccounted, tt.wantUnaccounted)
			}
			if state.caps.tokenCounts != tt.wantTokenCounts {
				t.Errorf("tokenCounts = %q, want %q", state.caps.tokenCounts, tt.wantTokenCounts)
			}
			if tt.wantUsage == nil {
				if len(usageEvents) != 0 {
					t.Errorf("token_usage events = %+v, want none", usageEvents)
				}
				return
			}
			if len(usageEvents) != 1 {
				t.Fatalf("token_usage events = %d, want 1", len(usageEvents))
			}
			got := usageEvents[0]
			if got.Usage != *tt.wantUsage || got.Model != tt.wantModel || !maps.Equal(got.UsageByModel, tt.wantByModel) {
				t.Errorf("token_usage event = {Usage %+v, Model %q, ByModel %+v}, want {%+v, %q, %+v}",
					got.Usage, got.Model, got.UsageByModel, *tt.wantUsage, tt.wantModel, tt.wantByModel)
			}
			if endAt < 0 || usageAt > endAt {
				t.Errorf("token_usage at %d, turn completed at %d, want the figure first", usageAt, endAt)
			}
		})
	}
}

func TestEndUnrepliedSettlesARemoteTurnThroughItsRecordsRead(t *testing.T) {
	t.Parallel()

	state, _, _ := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	state.target = agentcore.LaunchTarget{Command: "ssh", SSHHost: "worker", RemoteCommand: "agent"}
	declaration := scriptedSpendDeclaration(nil, nil, nil)
	pump := &pumpState{state: state, ledger: mustLedger(t, declaration, usage.RecordsRead{}), declaration: declaration}
	turn := &activeTurn{sink: make(chan domain.AgentEvent, 16), resultCh: make(chan turnEnd, 1), done: make(chan struct{})}
	turn.spend.MarkModelReached()
	pump.activeTurn = turn

	pump.endUnreplied(streamEndedEvidence(state, "stream ended", nil))

	if pump.activeTurn != turn || !turn.recordsPending {
		t.Fatalf("endUnreplied() left activeTurn set = %v, recordsPending %v, want the turn held for its records read", pump.activeTurn == turn, turn.recordsPending)
	}
	pump.handleRecordsRead(&recordsResult{turn: turn, read: usage.RecordsRead{OK: true}})
	end := waittest.Receive(t, turn.resultCh, "turn result")
	if !end.result.SpendUnaccounted {
		t.Errorf("turn result SpendUnaccounted = false, want true: a turn that reached the model with no figure")
	}
}

func TestPumpAgentTextBeforeTheFirstUsageUpdateIsNotServedOutput(t *testing.T) {
	t.Parallel()

	const usageUpdateLine = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"usage_update","used":12000,"size":200000}}}`
	tests := []struct {
		name            string
		lines           []string
		wantUnaccounted bool
	}{
		{name: "text before the usage_update", lines: []string{pulledUsageChunkLine, usageUpdateLine}},
		{name: "text after the usage_update", lines: []string{usageUpdateLine, pulledUsageChunkLine}, wantUnaccounted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration := scriptedSpendDeclaration(nil, nil, errors.New("journal unreadable"))
			declaration.UsageUpdateBeforeRequest = true
			state, out, inPw := newDeclaredSession(t, declaration, mustLedger(t, declaration, usage.RecordsRead{OK: true}))
			ctx, cancel := context.WithCancel(t.Context())
			outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
			promptID := out.awaitMethod(t, methodSessionPrompt)
			for _, line := range tt.lines {
				sendLine(t, inPw, line)
			}
			cancel()
			out.awaitMethod(t, methodSessionCancel)
			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonCancelled})

			outcome := waittest.Receive(t, outcomeCh, "turn outcome")

			if outcome.result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("RunTurn() SpendUnaccounted = %v, want %v", outcome.result.SpendUnaccounted, tt.wantUnaccounted)
			}
			if lowered := state.caps.tokenCounts == capabilityGap; lowered != tt.wantUnaccounted {
				t.Errorf("tokenCounts = %q, want the entry lowered = %v", state.caps.tokenCounts, tt.wantUnaccounted)
			}
		})
	}
}

func TestPumpStopLeavesARecordsReadToSettleTheTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		release bool
		want    domain.TurnResult
	}{
		{"a read released after the stop", true, domain.TurnResult{Usage: domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}, UsageMeasured: true, SpendUnaccounted: true}},
		{"a read never released", false, domain.TurnResult{SpendUnaccounted: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}, 1), make(chan struct{}, 1)
				declaration := scriptedSpendDeclaration(nil, nil, nil)
				declaration.ReadRecords = func(context.Context, usagesource.PullRuntime) (usagesource.Records, error) {
					started <- struct{}{}
					<-release
					return usagesource.Records{Spend: []usage.SpendReading{spendReading(100, 20, 0, "model-a", "u1")}}, nil
				}
				state, out, inPw := newDeclaredSession(t, declaration, mustLedger(t, declaration, usage.RecordsRead{OK: true}))
				outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
				out.awaitMethod(t, methodSessionPrompt)
				sendLine(t, inPw, pulledUsageChunkLine)
				_ = inPw.Close()
				waittest.Receive(t, started, "records read start")

				stopped := make(chan struct{})
				go func() { _ = stopSession(t.Context(), fakeSession(state)); close(stopped) }()
				waittest.Receive(t, state.stopCh, "the stop request")
				if tt.release {
					release <- struct{}{}
				}
				outcome := waittest.Receive(t, outcomeCh, "turn outcome after the stop")
				release <- struct{}{}
				waittest.Receive(t, stopped, "stopSession to return")

				if got := outcome.result; got.Usage != tt.want.Usage || got.UsageMeasured != tt.want.UsageMeasured || got.SpendUnaccounted != tt.want.SpendUnaccounted {
					t.Errorf("RunTurn() after a stop = {Usage %+v, UsageMeasured %v, SpendUnaccounted %v}, want {%+v, %v, %v}", got.Usage, got.UsageMeasured, got.SpendUnaccounted, tt.want.Usage, tt.want.UsageMeasured, tt.want.SpendUnaccounted)
				}
			})
		})
	}
}

func TestPumpAgentMessageBufferIsBounded(t *testing.T) {
	pump := &pumpState{}
	id := messageId("message-1")
	pump.handleAgentMessageChunk(&id, strings.Repeat("界", messageTruncateLimit+100), time.Now().UTC())
	if got := pump.agentMessageText.Len(); got != len([]byte(strings.Repeat("界", messageTruncateLimit))) {
		t.Fatalf("buffer bytes = %d, want %d", got, len([]byte(strings.Repeat("界", messageTruncateLimit))))
	}
	if !pump.agentMessageTruncated {
		t.Fatal("buffer truncated = false, want true after the message limit")
	}

	pump.flushAgentMessage()
	if len(pump.queued) != 1 {
		t.Fatalf("queued events = %d, want 1 completed message", len(pump.queued))
	}
	if got, want := len([]rune(pump.queued[0].Message)), messageTruncateLimit+1; got != want {
		t.Errorf("completed message rune count = %d, want %d", got, want)
	}
	if !strings.HasSuffix(pump.queued[0].Message, "…") {
		t.Errorf("completed message = %q, want truncation marker", pump.queued[0].Message)
	}
}

func TestPumpAgentMessageTruncationDoesNotLeakSecretPrefix(t *testing.T) {
	secret := fmt.Sprintf("message-secret-%d", time.Now().UnixNano())
	redact.Add(t.Name(), secret)

	pump := &pumpState{}
	id := messageId("message-1")
	pump.handleAgentMessageChunk(&id, strings.Repeat("x", messageTruncateLimit-2)+secret, time.Now().UTC())
	pump.flushAgentMessage()

	if len(pump.queued) != 1 {
		t.Fatalf("queued events = %d, want 1 completed message", len(pump.queued))
	}
	message := pump.queued[0].Message
	if strings.Contains(message, secret[:2]) {
		t.Errorf("completed message = %q, leaks a truncated prefix of the registered secret", message)
	}
	if !strings.HasSuffix(message, "…") {
		t.Errorf("completed message = %q, want truncation marker", message)
	}
}

// buildTestOwnedPipes returns pipes never wired to the connection under test,
// so CloseStdout cannot unpark its reader and only abandonment can end the turn.
func buildTestOwnedPipes(t *testing.T) *procutil.OwnedPipes {
	t.Helper()
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		_ = outWrite.Close()
		_ = outRead.Close()
		_ = errWrite.Close()
		_ = errRead.Close()
	})
	return &procutil.OwnedPipes{Stdout: outRead, Stderr: errRead}
}

func newTestSessionWithRelease(t *testing.T, grace time.Duration, reaped <-chan struct{}) (*sessionState, *io.PipeReader, *io.PipeWriter) {
	t.Helper()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()

	state := &sessionState{
		caps:     newCapabilityRecord(),
		usage:    agentcore.NewTurnEndUsage(),
		stopCh:   make(chan struct{}),
		pumpDone: make(chan struct{}),
		logger:   discardLogger(),
		origins:  &sessionOrigins{},
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()
	state.conn = jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))

	state.release = procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      buildTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: state.conn.Done(),
		Grace:      grace,
		Logger:     discardLogger(),
	})

	go runPump(state)

	t.Cleanup(func() {
		_ = inPw.Close()
		state.stopOnce.Do(func() { close(state.stopCh) })
		<-state.pumpDone
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
	})

	return state, outPr, inPw
}

func TestPumpAbandonmentFinalizesActiveTurnWithoutWaitingOnConnDone(t *testing.T) {
	t.Parallel()

	const grace = 150 * time.Millisecond
	reaped := make(chan struct{})
	state, outPr, _ := newTestSessionWithRelease(t, grace, reaped)
	out := newOutboundReader(outPr)
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	start := time.Now()
	close(reaped)

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	if elapsed := time.Since(start); elapsed > procutil.DefaultDrainGrace {
		t.Errorf("the turn finalized after %v, want it bounded by the injected grace %v rather than the default drain grace %v", elapsed, grace, procutil.DefaultDrainGrace)
	}
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   procutil.OutputAbandonedMessage,
	}, outcome.result, outcome.err)
}

func TestPumpReaderDoneInsideGraceLeavesStreamEndOutcomeUntouched(t *testing.T) {
	t.Parallel()

	const grace = 2 * time.Second
	reaped := make(chan struct{})
	close(reaped)
	state, outPr, inPw := newTestSessionWithRelease(t, grace, reaped)
	out := newOutboundReader(outPr)
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	if err := inPw.Close(); err != nil {
		t.Fatalf("close the simulated agent stream: %v", err)
	}

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	}, outcome.result, outcome.err)

	// Waiting out grace proves the ReaderDone arm caught the close, rather than
	// the assertion running before the timer could fire.
	waittest.Never(t, state.release.Abandoned(), grace+500*time.Millisecond, "the release to abandon though the reader ended inside its grace")
}

func TestPumpStartTurnAfterAbandonmentRefusedWithReleaseMessage(t *testing.T) {
	t.Parallel()

	const grace = 30 * time.Millisecond
	reaped := make(chan struct{})
	state, _, _ := newTestSessionWithRelease(t, grace, reaped)
	markSessionKnown(state)

	close(reaped)
	waittest.Receive(t, state.release.Abandoned(), "the release to abandon")

	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
	outcome := waittest.Receive(t, outcomeCh, "turn outcome")

	var agentErr *domain.AgentError
	if !errors.As(outcome.err, &agentErr) {
		t.Fatalf("RunTurn() error = %v, want *domain.AgentError", outcome.err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("RunTurn() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != procutil.OutputAbandonedMessage {
		t.Errorf("RunTurn() error message = %q, want %q", agentErr.Message, procutil.OutputAbandonedMessage)
	}
}

func TestHandleStartTurnRefusedOnceReleaseAbandonedBeforePumpHandlesIt(t *testing.T) {
	t.Parallel()

	outPr, outPw := io.Pipe()
	inPr, inPw := io.Pipe()
	inbox := jsonrpc.NewInbox[pumpItem]()
	conn := jsonrpc.NewConn(outPw, inPr, jsonrpc.Deliver(inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))
	t.Cleanup(func() {
		conn.Close()
		_ = inPw.Close()
		_ = outPr.Close()
		_ = outPw.Close()
		_ = inPr.Close()
	})

	p := &pumpState{
		state: &sessionState{
			caps:    newCapabilityRecord(),
			logger:  discardLogger(),
			usage:   agentcore.NewTurnEndUsage(),
			release: buildAbandonedRelease(t),
			inbox:   inbox,
			conn:    conn,
		},
	}
	ts := &turnStart{
		prompt:   "go",
		sink:     make(chan domain.AgentEvent, 4),
		resultCh: make(chan turnEnd, 1),
		done:     make(chan struct{}),
		cancelCh: make(chan struct{}),
		reply:    make(chan turnVerdict, 1),
	}

	p.handleStartTurn(ts)

	var verdict turnVerdict
	select {
	case verdict = <-ts.reply:
	default:
		t.Fatal("handleStartTurn() sent no verdict")
	}
	if verdict.accepted {
		t.Fatal("handleStartTurn() accepted a turn after the release had given up, want it refused")
	}
	if verdict.err == nil || verdict.err.Kind != domain.ErrPortExit || verdict.err.Message != procutil.OutputAbandonedMessage {
		t.Errorf("handleStartTurn() refusal = %v, want kind %q with message %q", verdict.err, domain.ErrPortExit, procutil.OutputAbandonedMessage)
	}
	if p.activeTurn != nil {
		t.Error("handleStartTurn() left an active turn after refusing, want none")
	}
}

func TestPumpAbandonmentKeepsPendingCancelledOutcome(t *testing.T) {
	t.Parallel()

	const grace = 150 * time.Millisecond
	reaped := make(chan struct{})
	state, outPr, _ := newTestSessionWithRelease(t, grace, reaped)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcomeCh := runTurnAsyncCtx(ctx, state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
	out.awaitMethod(t, methodSessionPrompt)

	cancel()
	out.awaitMethod(t, methodSessionCancel)

	close(reaped)

	outcome := waittest.Receive(t, outcomeCh, "turn outcome")
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal: agentcore.TerminalCancelled,
	}, outcome.result, outcome.err)
}

func TestPumpReturnsOnStopChAfterAbandonmentWithReaderDoneNeverClosing(t *testing.T) {
	t.Parallel()

	const grace = 30 * time.Millisecond
	reaped := make(chan struct{})
	state, _, _ := newTestSessionWithRelease(t, grace, reaped)

	close(reaped)
	waittest.Receive(t, state.release.Abandoned(), "the release to abandon")

	state.stopOnce.Do(func() { close(state.stopCh) })

	waittest.Receive(t, state.pumpDone, "runPump to return on state.stopCh after abandonment, though the connection's reader never ended")
}

func buildAbandonedRelease(t *testing.T) *procutil.OutputRelease {
	t.Helper()
	reaped := make(chan struct{})
	close(reaped)
	r := procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      buildTestOwnedPipes(t),
		Reaped:     reaped,
		ReaderDone: make(chan struct{}),
		Grace:      time.Millisecond,
		Logger:     discardLogger(),
	})
	waittest.Receive(t, r.Abandoned(), "the release to abandon")
	return r
}

func newActiveTurnForDirectDispatch() *activeTurn {
	return &activeTurn{
		sink:     make(chan domain.AgentEvent, 4),
		resultCh: make(chan turnEnd, 1),
		done:     make(chan struct{}),
		cancelCh: make(chan struct{}),
	}
}

func TestStreamEndSitesReportAbandonmentMessageOnceReleaseHasGivenUp(t *testing.T) {
	t.Parallel()

	t.Run("handleStreamEnd", func(t *testing.T) {
		t.Parallel()

		turn := newActiveTurnForDirectDispatch()
		p := &pumpState{
			state: &sessionState{
				logger:  discardLogger(),
				usage:   agentcore.NewTurnEndUsage(),
				release: buildAbandonedRelease(t),
				inbox:   jsonrpc.NewInbox[pumpItem](),
			},
			activeTurn: turn,
		}

		p.handleStreamEnd()

		end := waittest.Receive(t, turn.resultCh, "handleStreamEnd() to finalize the active turn")
		if end.err == nil || end.err.Message != procutil.OutputAbandonedMessage {
			t.Errorf("handleStreamEnd() turn error = %v, want message %q", end.err, procutil.OutputAbandonedMessage)
		}
	})

	t.Run("handleStreamEndMessage", func(t *testing.T) {
		t.Parallel()

		turn := newActiveTurnForDirectDispatch()
		p := &pumpState{
			state: &sessionState{
				logger:  discardLogger(),
				usage:   agentcore.NewTurnEndUsage(),
				release: buildAbandonedRelease(t),
			},
			activeTurn: turn,
		}

		p.handleStreamEndMessage(&jsonrpc.Message{Kind: jsonrpc.KindStreamEnd, Err: io.ErrUnexpectedEOF})

		end := waittest.Receive(t, turn.resultCh, "handleStreamEndMessage() to finalize the active turn")
		if end.err == nil || end.err.Message != procutil.OutputAbandonedMessage {
			t.Errorf("handleStreamEndMessage() turn error = %v, want message %q", end.err, procutil.OutputAbandonedMessage)
		}
	})
}

func TestHandleAbandonmentDrainsQueuedResponseBeforeFinalizing(t *testing.T) {
	t.Parallel()

	awaitedID := jsonrpc.NumberID(7)
	turn := newActiveTurnForDirectDispatch()
	turn.awaitedID = awaitedID

	p := &pumpState{
		state: &sessionState{
			logger:  discardLogger(),
			usage:   agentcore.NewTurnEndUsage(),
			release: buildAbandonedRelease(t),
			inbox:   jsonrpc.NewInbox[pumpItem](),
		},
		activeTurn: turn,
	}

	result, err := json.Marshal(promptResponse{StopReason: stopReasonEndTurn})
	if err != nil {
		t.Fatalf("marshal promptResponse: %v", err)
	}
	p.state.inbox.Put(pumpItem{msg: &jsonrpc.Message{Kind: jsonrpc.KindResponse, ID: awaitedID, Result: result}})

	p.handleAbandonment()

	end := waittest.Receive(t, turn.resultCh, "handleAbandonment() to finalize the active turn")
	if end.err != nil {
		t.Errorf("handleAbandonment() with a queued prompt response finalized the turn with error %v, want the queued response's own successful outcome (nil error): the response was already queued and must be drained before the turn is finalized as abandoned", end.err)
	}
}

func TestPumpPromptErrorKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		code     int
		wantKind domain.AgentErrorKind
	}{
		{"authentication required ends as a refused credential", -32000, domain.ErrCredentialUnverified},
		{"any other code stays a response error", -32603, domain.ErrResponseError},
	}

	const runtimeText = "API key is invalid."
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			var events []domain.AgentEvent
			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
			respondErrorLine(t, inPw, out.awaitMethod(t, methodSessionPrompt), tt.code, runtimeText)
			outcome := waittest.Receive(t, outcomeCh, "turn outcome")

			agentErr, ok := errors.AsType[*domain.AgentError](outcome.err)
			if !ok || agentErr.Kind != tt.wantKind {
				t.Fatalf("RunTurn(prompt error %d) error = %v, want *domain.AgentError of kind %q", tt.code, outcome.err, tt.wantKind)
			}
			if !strings.Contains(agentErr.Message, runtimeText) {
				t.Errorf("RunTurn(prompt error %d) message = %q, want it to carry the runtime text %q", tt.code, agentErr.Message, runtimeText)
			}
		})
	}
}

func TestPumpRefusalResetSourcing(t *testing.T) {
	t.Parallel()

	base := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	later := base.Add(time.Hour)
	window := func(key string, at time.Time) usage.WindowReading {
		return usage.WindowReading{Window: key, Status: "exhausted", ResetsAt: at.Unix()}
	}
	record := func(name string, at time.Time) usagesource.Records {
		return usagesource.Records{Refusal: usagesource.RecordedRefusal{Name: name, ResetsAt: at}}
	}
	fromWindow, fromRecord := usagesource.Refusal{WindowReset: true}, usagesource.Refusal{Name: "quota", WindowReset: true}
	tests := []struct {
		name      string
		refusal   usagesource.Refusal
		model     string
		windows   []usage.WindowReading
		pullErr   error
		records   usagesource.Records
		readErr   error
		wantReset time.Time
	}{
		{name: "exhausted window", refusal: fromWindow, windows: []usage.WindowReading{window("m1", base)}, wantReset: base},
		{name: "no window reset", windows: []usage.WindowReading{window("m1", base)}},
		{name: "records beat an earlier window", refusal: fromRecord, windows: []usage.WindowReading{window("m1", base)}, records: record("quota", later), wantReset: later},
		{name: "another model's window", refusal: fromWindow, model: "m1", windows: []usage.WindowReading{window("m2/daily", base)}},
		{name: "the model's own window", refusal: fromWindow, model: "m1", windows: []usage.WindowReading{window("m2/daily", base), window("m1/daily", later)}, wantReset: later},
		{name: "failed pull", refusal: fromWindow, windows: []usage.WindowReading{window("m1", base)}, pullErr: errors.New("pull failed")},
		{name: "failed records read", refusal: usagesource.Refusal{Name: "quota"}, records: record("quota", base), readErr: errors.New("unreadable")},
		{name: "record of another refusal", refusal: usagesource.Refusal{Name: "quota"}, records: record("budget", base)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			declaration := scriptedSpendDeclaration(nil, nil, nil)
			declaration.ReadRecords = func(context.Context, usagesource.PullRuntime) (usagesource.Records, error) {
				return tt.records, tt.readErr
			}
			declaration.Pull = usage.Source{Strategy: usage.StrategyPull, Windows: usage.WindowDeclaration{Statuses: map[string]domain.UsageWindowStatus{"exhausted": domain.UsageWindowExhausted}}}
			declaration.PullWindows = func(context.Context, usagesource.PullRuntime) ([]usage.WindowReading, error) {
				return tt.windows, tt.pullErr
			}
			declaration.WindowModel = func(key string) string { model, _, _ := strings.Cut(key, "/"); return model }
			declaration.PromptError = func(jsonrpc.Error) usagesource.PromptVerdict { return usagesource.PromptVerdict{Refusal: &tt.refusal} }
			state, out, inPw := newDeclaredSession(t, declaration, mustLedger(t, declaration, usage.RecordsRead{OK: true}))
			state.configuredModel = tt.model

			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
			respondErrorLine(t, inPw, out.awaitMethod(t, methodSessionPrompt), 429, "quota exhausted")
			agentErr, ok := errors.AsType[*domain.AgentError](waittest.Receive(t, outcomeCh, "turn outcome").err)

			if !ok || agentErr.Kind != domain.ErrUsageLimit || !agentErr.UsageLimit.ResetsAt.Equal(tt.wantReset) {
				t.Errorf("RunTurn() error = %+v, want kind %q with ResetsAt %v", agentErr, domain.ErrUsageLimit, tt.wantReset)
			}
		})
	}
}

func TestLaunchPullWaitsForTheTurnsRecordsRead(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		readInFlight bool
		pullsAtOnce  int32
	}{{"read in flight", true, 0}, {"no read", false, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				var pulls atomic.Int32
				turn := newActiveTurnForDirectDispatch()
				turn.recordsPending = tt.readInFlight
				state := &sessionState{logger: discardLogger(), stopCh: make(chan struct{}), inbox: jsonrpc.NewInbox[pumpItem]()}
				ledger := mustLedger(t, scriptedSpendDeclaration(nil, nil, nil), usage.RecordsRead{})
				p := &pumpState{state: state, activeTurn: turn, ledger: ledger}
				p.puller = usage.NewPuller(usage.Source{}, time.Second, false, func(context.Context) ([]usage.WindowReading, error) {
					pulls.Add(1)
					return nil, nil
				})

				p.armPull(turn)
				p.launchPull(turn)
				synctest.Wait()
				if got := pulls.Load(); got != tt.pullsAtOnce {
					t.Fatalf("pulls after launchPull() = %d, want %d", got, tt.pullsAtOnce)
				}
				p.settleSpend(turn, usage.TurnEnd{}, nil, usage.RecordsRead{})
				synctest.Wait()
				if got := pulls.Load(); got != 1 {
					t.Errorf("pulls once the read ended = %d, want 1", got)
				}
			})
		})
	}
}
