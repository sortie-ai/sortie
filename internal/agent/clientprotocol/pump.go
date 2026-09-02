package clientprotocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// Compile-time constant operator-facing messages. None interpolates a
// request body, a response body, a raw line, a schema field name, or a
// code identifier.
const (
	streamEndedMessage               = "the agent's connection ended before a response arrived"
	lineTooLongMessage               = "a line from the agent exceeded the connection's line bound"
	promptResponseUndecodedMessage   = "the agent's response to the prompt did not decode"
	promptSendFailedMessage          = "failed to send the prompt to the agent"
	malformedLineMessage             = "received a line that could not be parsed as JSON-RPC"
	unrecognizedSessionUpdateMessage = "received a session update of an unrecognized kind"
	unimplementedMethodMessage       = "the agent called a method this client does not implement"
	jsonrpcMethodNotFoundMessage     = "method not found"
	elicitationDetail                = "an answer to a question"
	turnAlreadyInFlightMessage       = "a turn is already in flight for this session"
	sessionEndedBeforeTurnMessage    = "the agent connection ended before this turn could start"
)

// jsonrpcMethodNotFound is the JSON-RPC error code for a method the
// client does not implement.
const jsonrpcMethodNotFound = -32601

// turnEndKind names why an active turn is winding down toward a forced
// disposition rather than the one its eventual response would
// otherwise report.
type turnEndKind uint8

const (
	turnEndNone turnEndKind = iota
	turnEndCancelled
	turnEndHumanInput
)

// activeTurn is the pump's own bookkeeping for the one turn that may be
// in flight at a time. It is read and written only by the pump's own
// goroutine.
type activeTurn struct {
	sink     chan domain.AgentEvent
	resultCh chan turnEnd
	done     chan struct{}
	cancelCh chan struct{}

	awaitedID    jsonrpc.ID
	workObserved bool

	pendingEnd    turnEndKind
	pendingDetail string
	cancelSent    bool
	deadlineC     <-chan time.Time
}

// pumpState is the pump's own mutable state. It exists only inside
// runPump's goroutine; no other goroutine holds a reference to it, which
// is what makes it the session's sole mutator of protocol state by
// construction rather than by convention.
type pumpState struct {
	state *sessionState

	tracker *agentcore.ToolTracker

	sessionID      string
	sessionIDKnown bool

	agentInfo        implementation
	agentInfoPresent bool
	caps             agentCapabilities

	queued []domain.AgentEvent

	activeTurn *activeTurn

	latchedHumanInput       bool
	latchedHumanInputDetail string

	malformedVariantLogged bool
	streamEnded            bool

	// openRequests tracks, best-effort, a request the pump has received
	// but not yet answered, so the answerOpen control message of
	// teardown's first step has something to walk if a reply is still
	// outstanding when it is processed.
	openRequests map[jsonrpc.ID]string
}

// runPump is the sole goroutine that consumes routed messages and
// control messages for the session's whole lifetime. It is the only
// writer of replies to agent-initiated requests and the only mutator of
// session protocol state, from the moment it starts until it returns.
func runPump(state *sessionState) {
	defer close(state.pumpDone)

	p := &pumpState{
		state:        state,
		tracker:      agentcore.NewToolTracker(),
		openRequests: make(map[jsonrpc.ID]string),
	}

	doneCh := state.conn.Done()
	doneFired := false

	for {
		var cancelSig <-chan struct{}
		var deadlineC <-chan time.Time
		if p.activeTurn != nil {
			deadlineC = p.activeTurn.deadlineC
			if p.activeTurn.pendingEnd == turnEndNone {
				cancelSig = p.activeTurn.cancelCh
			}
		}

		if !doneFired {
			select {
			case item := <-state.itemCh:
				p.handleItem(item)
			case <-doneCh:
				doneFired = true
				p.handleStreamEnd()
			case <-cancelSig:
				p.beginEndAttempt(turnEndCancelled, "")
			case <-deadlineC:
				p.finalizeActiveTurnOnDeadline()
			}
			continue
		}

		// The connection's reader has exited. The pump keeps draining any
		// further control message (a straggler startTurn or answerOpen)
		// until teardown closes the stop channel, which is the second
		// condition the pump's own exit requires.
		select {
		case item := <-state.itemCh:
			p.handleItem(item)
		case <-cancelSig:
			p.beginEndAttempt(turnEndCancelled, "")
		case <-deadlineC:
			p.finalizeActiveTurnOnDeadline()
		case <-state.stopCh:
			p.logDroppedQueue()
			return
		}
	}
}

// handleItem dispatches one pump item, either a routed message or a
// control message.
func (p *pumpState) handleItem(item pumpItem) {
	if item.control != nil {
		p.handleControl(*item.control)
		return
	}
	p.handleMessage(item.msg)
}

// handleControl applies one control message. Exactly one of its fields
// is set.
func (p *pumpState) handleControl(ctrl pumpControl) {
	switch {
	case ctrl.handshake != nil:
		p.agentInfo = ctrl.handshake.agentInfo
		p.agentInfoPresent = ctrl.handshake.agentInfoPresent
		p.caps = ctrl.handshake.caps

	case ctrl.sessionID != "":
		p.sessionID = ctrl.sessionID
		p.sessionIDKnown = true
		// Logged here, once per session, rather than when the handshake
		// control message arrives: the handshake is always published
		// first (StartSession's own publish order), so the agent's
		// implementation record is already known by the time the
		// session identifier is, and this is the first point the log
		// line can carry both.
		p.state.logger.Info("agent implementation",
			slog.String("session_id", p.sessionID),
			slog.String("name", p.agentInfo.Name),
			slog.String("version", p.agentInfo.Version))

	case ctrl.startTurn != nil:
		p.handleStartTurn(ctrl.startTurn)

	case ctrl.answerOpen:
		p.handleAnswerOpen()
	}
}

// drainReadyItems processes every item already in the pump's input
// channel, without blocking.
func (p *pumpState) drainReadyItems() {
	for {
		select {
		case item := <-p.state.itemCh:
			p.handleItem(item)
		default:
			return
		}
	}
}

// handleStreamEnd runs when jsonrpc.Conn.Done() closes. It first drains
// every item already in the pump's input channel, because the shared
// package's reader goroutine completes the handler's send into that
// channel before it closes Done(), so a KindStreamEnd message the
// connection produced is already there for the drain to find. Only when
// the drain leaves a turn still in flight does this method finalize it
// itself, on the process-exit row: a turn the drain already finalized,
// through the ordinary KindStreamEnd message handling below, is not
// touched again.
func (p *pumpState) handleStreamEnd() {
	p.drainReadyItems()
	p.streamEnded = true
	if p.activeTurn == nil {
		return
	}
	p.finalizeTurn(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
	})
}

// logDroppedQueue logs, at warn level, any event this adapter would
// have delivered into a turn that never started before the session
// ended.
func (p *pumpState) logDroppedQueue() {
	if len(p.queued) == 0 {
		return
	}
	p.state.logger.Warn("dropping events queued for a turn that never started", slog.Int("count", len(p.queued)))
	p.queued = nil
}

// handleMessage dispatches one routed message. Dispatch is on method
// before kind: a session/update notification is normalized and never
// answered whether its id is absent or null, and a
// session/request_permission request is always answered, before either
// falls through to the generic kind-based handling every other message
// gets.
func (p *pumpState) handleMessage(msg *jsonrpc.Message) {
	switch msg.Method {
	case methodSessionUpdate:
		p.handleSessionUpdateMessage(msg)
		return
	case methodSessionRequestPermission:
		p.handlePermissionRequest(msg)
		return
	}

	switch msg.Kind {
	case jsonrpc.KindResponse:
		p.handleResponse(msg)
	case jsonrpc.KindRequest:
		p.answerMethodNotFound(msg)
	case jsonrpc.KindNotification:
		p.state.logger.Debug("unhandled notification", slog.String("method", msg.Method))
	case jsonrpc.KindMalformed:
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: malformedLineMessage})
	case jsonrpc.KindStreamEnd:
		p.handleStreamEndMessage(msg)
	}
}

// handleStreamEndMessage runs when a KindStreamEnd message reaches the
// pump through the ordinary message arm (the common case: the shared
// package delivers this message before it closes Done(), and the two
// select arms are chosen at random). A line above the connection's own
// bound is a different condition from the loss of the subprocess and is
// diagnosed differently and is not retried.
func (p *pumpState) handleStreamEndMessage(msg *jsonrpc.Message) {
	p.streamEnded = true
	if p.activeTurn == nil {
		return
	}
	if errors.Is(msg.Err, bufio.ErrTooLong) {
		p.finalizeTurn(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
			TerminalMessage:   lineTooLongMessage,
		})
		return
	}
	p.finalizeTurn(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   streamEndedMessage,
		Cause:             msg.Err,
	})
}

// handleResponse finalizes the active turn when msg answers its
// awaited request id. A response matching no awaited id is recorded at
// debug level and ignored; it never ends a turn.
func (p *pumpState) handleResponse(msg *jsonrpc.Message) {
	turn := p.activeTurn
	if turn == nil || !turn.awaitedID.Present() || !msg.ID.Equal(turn.awaitedID) {
		p.state.logger.Debug("unmatched response", slog.String("id", msg.ID.String()))
		return
	}

	if msg.Error != nil {
		p.finalizeTurn(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrResponseError,
			TerminalMessage:   fmt.Sprintf("session/prompt error %d: %s", msg.Error.Code, msg.Error.Message),
		})
		return
	}

	var resp promptResponse
	if err := json.Unmarshal(msg.Result, &resp); err != nil {
		p.finalizeTurn(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
			TerminalMessage:   promptResponseUndecodedMessage,
		})
		return
	}

	p.finalizeTurn(stopReasonEvidence(resp.StopReason, workReportFrom(turn.workObserved)))
}

// handleSessionUpdateMessage decodes and normalizes one session/update
// notification.
func (p *pumpState) handleSessionUpdateMessage(msg *jsonrpc.Message) {
	var notif sessionNotification
	if err := json.Unmarshal(msg.Params, &notif); err != nil {
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: malformedLineMessage})
		return
	}

	// From the moment the session identifier is known, an update naming
	// a different session describes a conversation this adapter did not
	// create and is dropped. Before that moment every update is treated
	// as the session's own, because the identifier the pump would
	// otherwise compare against is still the zero value.
	if p.sessionIDKnown && string(notif.SessionID) != p.sessionID {
		p.state.logger.Debug("dropping session/update for a foreign session")
		return
	}

	sue, recognized := parseSessionUpdate(notif.Update.Remainder)
	if !recognized {
		if !p.malformedVariantLogged {
			p.malformedVariantLogged = true
			p.state.logger.Debug("unrecognized session update variant", slog.String("session_update", sue.rawVariant))
		}
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: unrecognizedSessionUpdateMessage})
		return
	}

	result := applySessionUpdate(p.tracker, sue)
	if p.activeTurn != nil && result.workPresent {
		p.activeTurn.workObserved = true
	}
	if result.hasEvent {
		p.emitOrQueue(result.event)
	}
}

// emitOrQueue publishes ev into the active turn's sink, or queues it
// when no turn is in flight, to be flushed immediately after the next
// turn's session_started event.
func (p *pumpState) emitOrQueue(ev domain.AgentEvent) {
	if p.activeTurn != nil {
		p.publish(p.activeTurn)(ev)
		return
	}
	p.queued = append(p.queued, ev)
}

// publish returns an emit function that delivers into turn's sink,
// guarded by turn's done channel so a publish can never block once the
// turn has ended.
func (p *pumpState) publish(turn *activeTurn) func(domain.AgentEvent) {
	return func(ev domain.AgentEvent) {
		select {
		case turn.sink <- ev:
		case <-turn.done:
		}
	}
}

// flushQueued delivers every queued out-of-turn event into turn's sink,
// in the order they were observed.
func (p *pumpState) flushQueued(turn *activeTurn) {
	if len(p.queued) == 0 {
		return
	}
	emit := p.publish(turn)
	for _, ev := range p.queued {
		emit(ev)
	}
	p.queued = nil
}

// handleStartTurn accepts or rejects a startTurn control message and,
// on acceptance, starts the turn.
func (p *pumpState) handleStartTurn(ts *turnStart) {
	if p.activeTurn != nil {
		ts.reply <- turnVerdict{accepted: false, err: &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: turnAlreadyInFlightMessage,
		}}
		return
	}
	if p.streamEnded {
		ts.reply <- turnVerdict{accepted: false, err: &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: sessionEndedBeforeTurnMessage,
		}}
		return
	}

	turn := &activeTurn{sink: ts.sink, resultCh: ts.resultCh, done: ts.done, cancelCh: ts.cancelCh}
	p.activeTurn = turn
	ts.reply <- turnVerdict{accepted: true}

	agentcore.EmitSessionStarted(p.publish(turn), strconv.Itoa(p.state.pid), p.sessionID)
	p.flushQueued(turn)

	if p.latchedHumanInput {
		detail := p.latchedHumanInputDetail
		p.latchedHumanInput = false
		p.latchedHumanInputDetail = ""
		p.finalizeTurn(agentcore.HumanInputEvidence(detail))
		return
	}

	id, err := p.state.conn.SendRequest(methodSessionPrompt, promptRequest{
		SessionID: sessionId(p.sessionID),
		Prompt:    []contentBlock{textContentBlock(ts.prompt)},
	})
	if err != nil {
		p.finalizeTurn(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrPortExit,
			TerminalMessage:   promptSendFailedMessage,
			Cause:             err,
		})
		return
	}
	turn.awaitedID = id
}

// finalizeTurn ends the active turn, calling agentcore.FinalizeTurn
// exactly once. When the turn is winding down toward a cancelled or
// human-input-required outcome, that outcome overrides whatever
// evidence the caller passed, per the shared rule's first row and the
// end-attempt sequence's own step 4.
func (p *pumpState) finalizeTurn(ev agentcore.TurnEvidence) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	p.activeTurn = nil

	switch turn.pendingEnd {
	case turnEndCancelled:
		ev = agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, Work: workReportFrom(turn.workObserved)}
	case turnEndHumanInput:
		ev = agentcore.HumanInputEvidence(turn.pendingDetail)
	}

	emit := p.publish(turn)
	meta := agentcore.TurnMeta{SessionID: p.sessionID}
	result, agentErr := agentcore.FinalizeTurn(emit, p.state.logger, ev, meta)

	select {
	case turn.resultCh <- turnEnd{result: result, err: agentErr}:
	case <-turn.done:
	}
}

// beginEndAttempt marks the active turn as winding down toward kind,
// sends session/cancel exactly once for the turn, and arms the bounded
// wait for the prompt response.
func (p *pumpState) beginEndAttempt(kind turnEndKind, detail string) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	if turn.pendingEnd == turnEndNone {
		turn.pendingEnd = kind
		turn.pendingDetail = detail
	}
	if !turn.cancelSent {
		turn.cancelSent = true
		p.state.conn.Notify(methodSessionCancel, cancelNotification{SessionID: sessionId(p.sessionID)}) //nolint:errcheck,gosec // best-effort
	}
	if turn.deadlineC == nil {
		turn.deadlineC = time.After(readTimeout(p.state))
	}
}

// finalizeActiveTurnOnDeadline ends the active turn once its bounded
// wait for the prompt response has elapsed. The evidence passed here is
// always overridden by finalizeTurn's own pendingEnd handling.
func (p *pumpState) finalizeActiveTurnOnDeadline() {
	if p.activeTurn == nil {
		return
	}
	p.finalizeTurn(agentcore.TurnEvidence{Terminal: agentcore.TerminalFailure, TerminalErrorKind: domain.ErrTurnFailed})
}

// handlePermissionRequest answers a session/request_permission request
// and, when the shared posture ends the attempt, latches or begins that
// end.
func (p *pumpState) handlePermissionRequest(msg *jsonrpc.Message) {
	if !msg.ID.Present() {
		return
	}
	p.openRequests[msg.ID] = methodSessionRequestPermission
	defer delete(p.openRequests, msg.ID)

	if turn := p.activeTurn; turn != nil && turn.pendingEnd != turnEndNone {
		p.respondCancelled(msg.ID)
		return
	}

	var params requestPermissionRequest
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		params = requestPermissionRequest{}
	}

	optionID, found := selectRefusingOption(params.Options)
	posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, found, agentcore.AnswerPending)

	if posture.Notice != "" {
		agentcore.EmitNotification(p.emitOrQueue, posture.NoticeWithDetail(""))
	}

	if posture.Transmit {
		p.respondSelected(msg.ID, optionID)
	} else {
		p.respondCancelled(msg.ID)
	}

	if posture.EndAttempt {
		p.latchOrBeginEndAttempt("")
	}
}

// answerMethodNotFound answers any request naming a method this client
// does not implement. elicitation/create additionally ends the attempt,
// per the shared posture for a class of request only a person could
// answer; every other unimplemented method emits one malformed event
// and does not end the attempt.
func (p *pumpState) answerMethodNotFound(msg *jsonrpc.Message) {
	if !msg.ID.Present() {
		return
	}
	p.openRequests[msg.ID] = msg.Method
	defer delete(p.openRequests, msg.ID)

	if err := p.state.conn.RespondError(msg.ID, jsonrpcMethodNotFound, jsonrpcMethodNotFoundMessage); err != nil {
		p.state.logger.Debug("failed to write method-not-found reply", slog.Any("error", err))
	}

	if msg.Method == methodElicitationCreate {
		posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
		if posture.Notice != "" {
			agentcore.EmitNotification(p.emitOrQueue, posture.NoticeWithDetail(elicitationDetail))
		}
		p.latchOrBeginEndAttempt(elicitationDetail)
		return
	}

	p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: unimplementedMethodMessage})
}

// latchOrBeginEndAttempt ends the active turn's attempt, or, when no
// turn is in flight, latches the human-input-required outcome onto the
// session so the next turn ends with it immediately.
func (p *pumpState) latchOrBeginEndAttempt(detail string) {
	if p.activeTurn != nil {
		p.beginEndAttempt(turnEndHumanInput, detail)
		return
	}
	p.latchedHumanInput = true
	p.latchedHumanInputDetail = detail
}

// respondSelected answers a permission request by selecting optionID.
func (p *pumpState) respondSelected(id jsonrpc.ID, optionID string) {
	raw, err := marshalWithDiscriminant(selectedPermissionOutcome{OptionID: permissionOptionId(optionID)}, "outcome", outcomeSelected)
	if err != nil {
		p.state.logger.Debug("failed to construct permission reply", slog.Any("error", err))
		return
	}
	response := requestPermissionResponse{Outcome: requestPermissionOutcome{Outcome: outcomeSelected, Remainder: raw}}
	if err := p.state.conn.Respond(id, response); err != nil {
		p.state.logger.Debug("failed to write permission reply", slog.Any("error", err))
	}
}

// respondCancelled answers a permission request with the cancelled
// outcome.
func (p *pumpState) respondCancelled(id jsonrpc.ID) {
	raw, err := marshalWithDiscriminant(struct{}{}, "outcome", outcomeCancelled)
	if err != nil {
		p.state.logger.Debug("failed to construct permission reply", slog.Any("error", err))
		return
	}
	response := requestPermissionResponse{Outcome: requestPermissionOutcome{Outcome: outcomeCancelled, Remainder: raw}}
	if err := p.state.conn.Respond(id, response); err != nil {
		p.state.logger.Debug("failed to write permission reply", slog.Any("error", err))
	}
}

// handleAnswerOpen walks whatever request the pump has received but not
// yet answered and answers it best-effort, so teardown's first step has
// a receiver even when a reply from before teardown began is still
// outstanding.
func (p *pumpState) handleAnswerOpen() {
	for id, method := range p.openRequests {
		if method == methodSessionRequestPermission {
			p.respondCancelled(id)
		} else if err := p.state.conn.RespondError(id, jsonrpcMethodNotFound, jsonrpcMethodNotFoundMessage); err != nil {
			p.state.logger.Debug("failed to write method-not-found reply", slog.Any("error", err))
		}
		delete(p.openRequests, id)
	}
}
