package clientprotocol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
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

// startTurnAwaitingPrompt starts a turn on state and waits for the
// resulting session/prompt request to appear on out, returning its raw
// id and the turn's outcome channel. The caller drives the stream-end
// condition next; it must not answer the prompt itself for these
// tests, because the assertion is about what happens when no answer
// ever arrives.
func startTurnAwaitingPrompt(t *testing.T, state *sessionState, out *outboundReader) <-chan turnOutcome {
	t.Helper()
	markSessionKnown(state)
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: func(domain.AgentEvent) {}})
	out.awaitMethod(t, methodSessionPrompt)
	return outcomeCh
}

// TestPumpStreamEndCleanExit covers a clean end of stream mid-turn: it
// delivers no handler message, closes jsonrpc.Conn.Done() alone, and
// the turn must finalize on the process-exit row from that arm rather
// than being left to the orchestrator's wall-clock ceiling.
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

// TestPumpStreamEndReadFailure covers a read failure mid-turn: it
// delivers exactly one KindStreamEnd message and then closes Done(),
// and the turn must finalize once, not twice, on the same process-
// exit row a lost subprocess reports.
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

// TestFinalizeStreamEndLineBound covers the drain's own classification:
// a KindStreamEnd carrying bufio.ErrTooLong, already queued ahead of
// Done() firing, must settle the turn as turn_outcome_unknown and not
// retryable, never as the retryable port_exit the Done() arm alone
// would report. The connection here uses a small line bound so an
// ordinary-sized fixture line trips it without a synthetic error.
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
	// The scanner abandons the read once it exceeds the bound, without
	// draining the rest of this write, so a synchronous Write here
	// would block forever waiting for a reader that has already given
	// up. Writing from a goroutine lets the too-long condition end the
	// stream on its own; t.Cleanup closes inPw regardless of how far
	// this write got.
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

// TestPumpDispatchNullID confirms a session/update notification
// carrying a null id is normalized and left unanswered, so no
// response line is written for it. The sentinel request sent right
// behind it proves the absence: if the update had produced a
// response, it would occupy the next line instead of the sentinel's
// own answer.
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
	if resp.Error == nil || resp.Error.Code != jsonrpcMethodNotFound {
		t.Fatalf("response to the sentinel request = %+v, want a %d error; a different id or a decoded outcome here means the null-id session/update was answered instead of normalized", resp, jsonrpcMethodNotFound)
	}
}

// TestSessionUpdateBeforeAnyPromptNotLost confirms a message arriving
// before any prompt is not lost. It is queued and flushed into the
// next turn's sink after that turn's own session_started event and the
// once-per-session capability notice.
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

// waitEvent returns the next event from ch, failing t if none arrives
// within awaitTimeout.
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

// TestAnsweredIDEcho confirms a session/request_permission request,
// and a request naming a method this client does not implement, are
// each answered on one response line whose id member matches the
// request's id byte for byte, for each of a JSON string, the number
// zero, and JSON null.
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
			if resp.Error == nil || resp.Error.Code != jsonrpcMethodNotFound {
				t.Errorf("response = %+v, want a %d error for an unimplemented method", resp, jsonrpcMethodNotFound)
			}
		})
	}
}

// TestToolDeliveryReportSkippedDuringWindDownAndTeardown confirms the
// uncallable-tool report never fires for a permission request answered
// by a path other than handlePermissionRequest's own non-winding-down
// branch: neither a request a winding-down turn cancels immediately,
// nor one teardown's handleAnswerOpen answers directly.
func TestToolDeliveryReportSkippedDuringWindDownAndTeardown(t *testing.T) {
	t.Parallel()

	t.Run("a turn winding down toward cancellation answers a permission request without reporting", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
		out := newOutboundReader(outPr)
		state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
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

	// openRequests holds an entry only for the duration of the
	// handlePermissionRequest (or answerMethodNotFound) call that
	// created it: both delete their own entry via defer before
	// returning, and the pump processes one item from its input
	// channel to completion before the next, so a live pump driven
	// only through that channel can never observe handleAnswerOpen's
	// defensive walk find anything but an empty map (see pump.go's own
	// "ordinary case" comment on handleAnswerOpen). Proving
	// handleAnswerOpen's own cancellation path never reports therefore
	// needs a pumpState with a still-open entry constructed directly,
	// the same way newPumpForCapabilityTests in capability_test.go
	// exercises a pump method in isolation from a live pump goroutine.
	t.Run("handleAnswerOpen answers a still-open permission request without reporting", func(t *testing.T) {
		t.Parallel()

		outPr, outPw := io.Pipe()
		inPr, _ := io.Pipe()
		var buf bytes.Buffer
		state := &sessionState{
			caps:   newCapabilityRecord(false),
			itemCh: make(chan pumpItem, pumpChannelCapacity),
			stopCh: make(chan struct{}),
			logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
		state.conn = jsonrpc.NewConn(outPw, inPr, pumpHandler(state.itemCh, state.stopCh),
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

// TestHandshakeToolServersDeliveredReachesPump confirms
// toolServersDelivered reaches pumpState only through the handshake
// control message handleControl applies, not from a value set on the
// struct some other way: publishing that control message directly is
// enough, on its own, for a later permission request to trigger the
// uncallable-tool report.
func TestHandshakeToolServersDeliveredReachesPump(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)

	state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
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
