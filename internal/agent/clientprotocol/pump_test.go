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
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
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
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	if err := inPw.Close(); err != nil {
		t.Fatalf("close the simulated agent stream: %v", err)
	}

	outcome := awaitOutcome(t, outcomeCh)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	}, outcome.result, outcome.err)
}

func TestPumpStreamEndReadFailure(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	outcomeCh := startTurnAwaitingPrompt(t, state, out)

	if err := inPw.CloseWithError(errors.New("simulated read failure")); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	outcome := awaitOutcome(t, outcomeCh)
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
	// The scanner abandons the read once it exceeds the bound without
	// draining the rest, so a synchronous Write here would block forever;
	// writing from a goroutine lets the too-long condition end the stream.
	go func() { _, _ = inPw.Write(oversized) }()

	outcome := awaitOutcome(t, outcomeCh)
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

	first := waitEvent(t, eventCh)
	if first.Type != domain.EventSessionStarted {
		t.Fatalf("first event type = %q, want %q", first.Type, domain.EventSessionStarted)
	}
	second := waitEvent(t, eventCh)
	if second.Type != domain.EventNotification {
		t.Fatalf("second event type = %q, want %q (the once-per-session capability notice)", second.Type, domain.EventNotification)
	}
	third := waitEvent(t, eventCh)
	if third.Type != domain.EventNotification || third.Message != "queued before any turn" {
		t.Fatalf("third event = %+v, want the queued notification flushed after session_started and the capability notice", third)
	}

	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}
}

func waitEvent(t *testing.T, ch <-chan domain.AgentEvent) domain.AgentEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(awaitTimeout):
		t.Fatal("timed out waiting for an event")
		return domain.AgentEvent{}
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
		awaitOutcome(t, outcomeCh)

		if got := countToolDeliveryNotifications(events); got != 0 {
			t.Errorf("tool delivery uncallable notifications during wind-down = %d, want 0", got)
		}
		if strings.Contains(buf.String(), toolDeliveryUncallableLog) {
			t.Errorf("unexpected tool delivery uncallable Warn record during wind-down: %s", buf.String())
		}
	})

	// handlePermissionRequest and answerMethodNotFound each delete their own
	// openRequests entry via defer, and the pump handles one inbox item to
	// completion before the next, so a live pump can never observe
	// handleAnswerOpen's defensive walk find a still-open entry. Exercising
	// that path needs a pumpState with an open entry constructed directly.
	t.Run("handleAnswerOpen answers a still-open permission request without reporting", func(t *testing.T) {
		t.Parallel()

		outPr, outPw := io.Pipe()
		inPr, _ := io.Pipe()
		var buf bytes.Buffer
		state := &sessionState{
			caps:   newCapabilityRecord(false),
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
	outcome := awaitOutcome(t, outcomeCh)
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

// stalledConsumerBurstCount is well past the capacity a bounded hand-off would
// have held, so the write that would have parked such a reader is reached long
// before the burst ends.
const stalledConsumerBurstCount = 4096

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
			// Only a burst chunk's own event holds the gate; the turn's
			// session-started event and capability notice must reach the caller
			// before the prompt request is even sent.
			if _, err := strconv.Atoi(e.Message); err == nil {
				<-gate
			}
		},
	})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	for i := range stalledConsumerBurstCount {
		line := fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-test","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`, i)
		errCh := make(chan error, 1)
		go func() {
			_, writeErr := fmt.Fprintln(inPw, line)
			errCh <- writeErr
		}()
		select {
		case writeErr := <-errCh:
			if writeErr != nil {
				t.Fatalf("write session/update chunk %d: %v", i, writeErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("peer write %d did not return within 10s while the OnEvent gate was held, want the reader to never park behind the pump's own stalled delivery", i)
		}
	}
	close(gate)

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	mu.Lock()
	defer mu.Unlock()
	var texts []string
	for _, e := range events {
		if e.Type == domain.EventNotification {
			if _, err := strconv.Atoi(e.Message); err == nil {
				texts = append(texts, e.Message)
			}
		}
	}
	if len(texts) != stalledConsumerBurstCount {
		t.Fatalf("notification events = %d, want %d", len(texts), stalledConsumerBurstCount)
	}
	for i, text := range texts {
		if want := fmt.Sprintf("%d", i); text != want {
			t.Errorf("notification %d text = %q, want %q (chunks out of order)", i, text, want)
		}
	}
}

// buildTestOwnedPipes returns pipes that satisfy StartOutputRelease's nil
// check but are never wired to the connection under test, so the release's
// CloseStdout cannot unpark the connection's reader; that is what proves the
// pump reacts to abandonment on its own.
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
		caps:     newCapabilityRecord(false),
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

	outcome := awaitOutcome(t, outcomeCh)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the turn finalized after %v, want it bounded by the injected grace %v rather than the parked reader's own unbounded wait", elapsed, grace)
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

	outcome := awaitOutcome(t, outcomeCh)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	}, outcome.result, outcome.err)

	// Waiting out grace, rather than checking right away, proves the release's
	// ReaderDone arm caught the close rather than the assertion simply running
	// before an unattended timer could have fired.
	select {
	case <-state.release.Abandoned():
		t.Error("the release abandoned though the reader ended inside its grace, want the latch to stay unset")
	case <-time.After(grace + 500*time.Millisecond):
	}
}

func TestPumpStartTurnAfterAbandonmentRefusedWithReleaseMessage(t *testing.T) {
	t.Parallel()

	const grace = 30 * time.Millisecond
	reaped := make(chan struct{})
	state, _, _ := newTestSessionWithRelease(t, grace, reaped)
	markSessionKnown(state)

	close(reaped)
	select {
	case <-state.release.Abandoned():
	case <-time.After(awaitTimeout):
		t.Fatal("the release never abandoned within the test's wait bound")
	}

	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: func(domain.AgentEvent) {}})
	outcome := awaitOutcome(t, outcomeCh)

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
			caps:    newCapabilityRecord(false),
			logger:  discardLogger(),
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

	outcome := awaitOutcome(t, outcomeCh)
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
	select {
	case <-state.release.Abandoned():
	case <-time.After(awaitTimeout):
		t.Fatal("the release never abandoned within the test's wait bound")
	}

	state.stopOnce.Do(func() { close(state.stopCh) })

	select {
	case <-state.pumpDone:
	case <-time.After(awaitTimeout):
		t.Fatal("runPump did not return on state.stopCh after abandonment, though the connection's reader never ended")
	}
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
	select {
	case <-r.Abandoned():
	case <-time.After(awaitTimeout):
		t.Fatal("the release never abandoned")
	}
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
				release: buildAbandonedRelease(t),
				inbox:   jsonrpc.NewInbox[pumpItem](),
			},
			activeTurn: turn,
		}

		p.handleStreamEnd()

		select {
		case end := <-turn.resultCh:
			if end.err == nil || end.err.Message != procutil.OutputAbandonedMessage {
				t.Errorf("handleStreamEnd() turn error = %v, want message %q", end.err, procutil.OutputAbandonedMessage)
			}
		case <-time.After(awaitTimeout):
			t.Fatal("handleStreamEnd() did not finalize the active turn")
		}
	})

	t.Run("handleStreamEndMessage", func(t *testing.T) {
		t.Parallel()

		turn := newActiveTurnForDirectDispatch()
		p := &pumpState{
			state: &sessionState{
				logger:  discardLogger(),
				release: buildAbandonedRelease(t),
			},
			activeTurn: turn,
		}

		p.handleStreamEndMessage(&jsonrpc.Message{Kind: jsonrpc.KindStreamEnd, Err: io.ErrUnexpectedEOF})

		select {
		case end := <-turn.resultCh:
			if end.err == nil || end.err.Message != procutil.OutputAbandonedMessage {
				t.Errorf("handleStreamEndMessage() turn error = %v, want message %q", end.err, procutil.OutputAbandonedMessage)
			}
		case <-time.After(awaitTimeout):
			t.Fatal("handleStreamEndMessage() did not finalize the active turn")
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

	select {
	case end := <-turn.resultCh:
		if end.err != nil {
			t.Errorf("handleAbandonment() with a queued prompt response finalized the turn with error %v, want the queued response's own successful outcome (nil error): the response was already queued and must be drained before the turn is finalized as abandoned", end.err)
		}
	case <-time.After(awaitTimeout):
		t.Fatal("handleAbandonment() did not finalize the active turn")
	}
}
