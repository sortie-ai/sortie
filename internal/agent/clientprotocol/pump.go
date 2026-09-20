package clientprotocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/domain"
)

// Compile-time operator-facing messages. None interpolates a request body, a
// response body, a raw line, a schema field name, or a code identifier.
const (
	streamEndedMessage               = "the agent's connection ended before a response arrived"
	lineTooLongMessage               = "a line from the agent exceeded the connection's line bound"
	promptResponseUndecodedMessage   = "the agent's response to the prompt did not decode"
	promptSendFailedMessage          = "failed to send the prompt to the agent"
	malformedLineMessage             = "received a line that could not be parsed as JSON-RPC"
	unrecognizedSessionUpdateMessage = "received a session update of an unrecognized kind"
	unimplementedMethodMessage       = "the agent called a method this client does not implement"
	elicitationDetail                = "an answer to a question"
	turnAlreadyInFlightMessage       = "a turn is already in flight for this session"
	sessionEndedBeforeTurnMessage    = "the agent connection ended before this turn could start"
	toolDeliveryUncallableNotice     = "this session delivered tool servers and the agent runtime asked for consent before running a tool; an unattended run grants no consent, so any delivered tool the runtime gates the same way cannot be called unless that runtime's own configuration allows it"
	toolDeliveryUncallableLog        = "a tool call was gated by consent in a session that delivered tool servers"
)

// turnEndKind names why an active turn is winding down toward a forced
// disposition rather than the one its eventual response would report.
type turnEndKind uint8

const (
	turnEndNone turnEndKind = iota
	turnEndCancelled
	turnEndHumanInput
)

// activeTurn is the pump's bookkeeping for the one turn that may be in flight at
// a time. It is read and written only by the pump's own goroutine.
type activeTurn struct {
	sink     chan domain.AgentEvent
	resultCh chan turnEnd
	done     chan struct{}
	cancelCh chan struct{}

	awaitedID jsonrpc.ID

	// capsSnapshot is the capability record as it stood when this turn began, so
	// a lowering observed while the turn is in flight takes effect from the next
	// turn onward.
	capsSnapshot capabilityRecord

	pendingEnd    turnEndKind
	pendingDetail string
	cancelSent    bool
	deadlineC     <-chan time.Time

	// modelReached records that this turn reached the model. A turn that reached
	// the model and produced no figure is spend that occurred and was not
	// measured.
	modelReached bool

	// drainEvidence is the disposition this turn is finalized on once its drain
	// reports, held while the drain runs. draining distinguishes a turn waiting
	// on a drain from one whose bounded wait belongs to an end attempt.
	drainEvidence agentcore.TurnEvidence
	draining      bool

	// endSettled marks a turn the transport has already seen end, kept in flight
	// only long enough to collect its figure.
	endSettled bool
}

// disposition returns the evidence turn is finalized on. A turn winding down
// toward a forced outcome reports that outcome, unless the transport has already
// seen how the turn ended: an end attempt arriving while such a turn waits for
// its figure stops nothing still running, and taking its outcome would report
// the failure as something else.
func (t *activeTurn) disposition(ev agentcore.TurnEvidence) agentcore.TurnEvidence {
	if t.endSettled {
		return ev
	}
	switch t.pendingEnd {
	case turnEndCancelled:
		return agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled}
	case turnEndHumanInput:
		return agentcore.HumanInputEvidence(t.pendingDetail)
	}
	return ev
}

// pumpState is the pump's mutable state. It exists only inside runPump's
// goroutine, which is what makes the pump the session's sole mutator of
// protocol state by construction rather than by convention.
type pumpState struct {
	state *sessionState

	tracker *agentcore.ToolTracker

	// reader is the session's measurement source while it still applies. It is
	// cleared, and the token-counts entry lowered, the moment the source proves
	// inapplicable.
	reader usageReader

	// drainProven latches once a drain has produced a record. Until it does, a
	// drain that produces nothing drops the source for the rest of the session.
	drainProven bool

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

	// capabilityNoticeSent reports whether the once-per-session gap notice has
	// been emitted. A lowering observed after that point is logged at warn level
	// instead of producing a second notice.
	capabilityNoticeSent bool

	// toolServersDelivered mirrors the handshake fact: whether the
	// session-creation request carried at least one tool server.
	toolServersDelivered bool

	// toolDeliveryReported latches the once-per-session uncallable-tool report
	// so a later permission request does not repeat it.
	toolDeliveryReported bool

	// openRequests records the request the pump is answering while it is
	// answering it. The pump answers each request inside the call that receives
	// it, so teardown's answerOpen step observes an empty map in the ordinary
	// case; the map exists so that step walks a defined set rather than assuming
	// one.
	openRequests map[jsonrpc.ID]string

	// loadExpected reports whether an expectLoad control message has been
	// processed. It gates observeReplay so a chunk observed before the
	// session/load call itself cannot confirm a load that replayed nothing.
	loadExpected bool

	// replayObserved reports whether the pump has seen a chunk replayed for the
	// identifier a session/load control message named. Set at most once per
	// session and never cleared.
	replayObserved bool

	// pendingReplayQuery and replayDeadlineC track a replayQuery still awaiting
	// an answer: set together when the query arrives with no replay observed
	// yet, and cleared together once a replayed chunk or the deadline resolves
	// it.
	pendingReplayQuery *replayQuery
	replayDeadlineC    <-chan time.Time
}

// runPump is the sole goroutine that consumes routed messages and control
// messages for the session's whole lifetime. It is the only writer of replies
// to agent-initiated requests and the only mutator of session protocol state.
func runPump(state *sessionState) {
	defer close(state.pumpDone)

	p := &pumpState{
		state:        state,
		tracker:      agentcore.NewToolTracker(),
		reader:       state.reader,
		openRequests: make(map[jsonrpc.ID]string),
	}

	doneCh := state.conn.Done()
	doneFired := false
	writeFailedCh := state.conn.WriteFailed()
	var writeFailDeadlineC <-chan time.Time

	// abandonedCh closes once the release gives up on the reader parked on a dead
	// runtime's standard output. It is nil for a session built without a release.
	abandonedCh := state.release.Abandoned()
	// stopArm stays nil until abandonment, so only a session whose reader was
	// given up on returns on the stop signal without waiting for that reader.
	var stopArm <-chan struct{}

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
			case <-state.inbox.Ready():
				if item, ok := state.inbox.Take(); ok {
					p.handleItem(item)
				}
			case <-doneCh:
				doneFired = true
				writeFailDeadlineC = nil
				p.handleStreamEnd()
			case <-abandonedCh:
				abandonedCh = nil
				stopArm = state.stopCh
				p.handleAbandonment()
			case <-writeFailedCh:
				writeFailedCh = nil
				writeFailDeadlineC = p.armWriteFailedDeadline()
			case <-writeFailDeadlineC:
				writeFailDeadlineC = nil
				p.handleWriteFailed()
			case <-cancelSig:
				p.beginEndAttempt(turnEndCancelled, "")
			case <-deadlineC:
				p.finalizeActiveTurnOnDeadline()
			case <-p.replayDeadlineC:
				p.finalizeReplayQueryOnDeadline()
			case <-stopArm:
				p.logDroppedQueue()
				return
			}
			continue
		}

		// The connection's reader has exited. The pump keeps draining any further
		// control message until teardown closes the stop channel, the second
		// condition its exit requires.
		select {
		case <-state.inbox.Ready():
			if item, ok := state.inbox.Take(); ok {
				p.handleItem(item)
			}
		case <-cancelSig:
			p.beginEndAttempt(turnEndCancelled, "")
		case <-deadlineC:
			p.finalizeActiveTurnOnDeadline()
		case <-p.replayDeadlineC:
			p.finalizeReplayQueryOnDeadline()
		case <-state.stopCh:
			p.logDroppedQueue()
			return
		}
	}
}

func (p *pumpState) handleItem(item pumpItem) {
	if item.control != nil {
		p.handleControl(*item.control)
		return
	}
	p.handleMessage(item.msg)
}

// handleControl applies one control message. Exactly one of its fields is set.
func (p *pumpState) handleControl(ctrl pumpControl) {
	switch {
	case ctrl.handshake != nil:
		p.agentInfo = ctrl.handshake.agentInfo
		p.agentInfoPresent = ctrl.handshake.agentInfoPresent
		p.caps = ctrl.handshake.caps
		p.toolServersDelivered = ctrl.handshake.toolServersDelivered
		p.applyHandshakeCapabilityLowering(ctrl.handshake)

	case ctrl.sessionID != "":
		p.sessionID = ctrl.sessionID
		p.sessionIDKnown = true
		if p.reader != nil {
			p.reader.Open(p.sessionID)
		}
		// Logged once per session here, not on the handshake control message: the
		// handshake is always published first, so both the implementation record
		// and the session identifier are known only by this point.
		p.state.logger.Info("agent implementation",
			slog.String("session_id", p.sessionID),
			slog.String("name", p.agentInfo.Name),
			slog.String("version", p.agentInfo.Version))

	case ctrl.expectLoad != "":
		// Adopted silently, ahead of the definitive sessionID control message
		// that always follows: the identifier is already known here, so this
		// closes the race a session/new launch leaves open. If the load is not
		// confirmed, handleReplayQuery and finalizeReplayQueryOnDeadline clear
		// both fields back to their deferred-filter state; otherwise the
		// following sessionID message overwrites this value with the one already
		// in place.
		p.sessionID = ctrl.expectLoad
		p.sessionIDKnown = true
		p.loadExpected = true

	case ctrl.query != nil:
		p.handleReplayQuery(ctrl.query)

	case ctrl.startTurn != nil:
		p.handleStartTurn(ctrl.startTurn)

	case ctrl.usage != nil:
		p.handleUsageObserved(ctrl.usage)

	case ctrl.answerOpen != nil:
		p.handleAnswerOpen()
		close(ctrl.answerOpen)
	}
}

// handleReplayQuery applies q. A nil reply means the call already failed: the
// provisional identifier is cleared and the entry lowered at once, with nothing
// sent back. Otherwise a session/load answered success: this answers true when
// replay has already been observed, or defers until a replayed chunk or this
// query's bounded wait resolves it.
func (p *pumpState) handleReplayQuery(q *replayQuery) {
	if q.reply == nil {
		p.cancelPendingReplayQuery()
		p.clearProvisionalSessionID()
		p.lowerCapability(&p.state.caps.sessionContinuation, capabilityLabelSessionContinuation)
		return
	}
	if p.replayObserved {
		q.reply <- true
		return
	}
	p.pendingReplayQuery = q
	p.replayDeadlineC = time.After(readTimeout(p.state))
}

// finalizeReplayQueryOnDeadline ends a pending replay query once its bounded
// wait elapsed with no replay: the session/load succeeded but replayed nothing,
// so this answers false, clears the provisional identifier, and lowers
// sessionContinuation.
func (p *pumpState) finalizeReplayQueryOnDeadline() {
	if p.pendingReplayQuery == nil {
		return
	}
	q := p.pendingReplayQuery
	p.cancelPendingReplayQuery()
	p.clearProvisionalSessionID()
	p.lowerCapability(&p.state.caps.sessionContinuation, capabilityLabelSessionContinuation)
	q.reply <- false
}

// cancelPendingReplayQuery drops a replay query still waiting and disarms its
// deadline, so a wait already answered or abandoned cannot fire later and act on
// a session that has since been replaced.
func (p *pumpState) cancelPendingReplayQuery() {
	p.pendingReplayQuery = nil
	p.replayDeadlineC = nil
}

// clearProvisionalSessionID reverts the identifier expectLoad adopted back to
// unknown. Without this, an update for the session session/new actually created
// would be compared against the loaded session's stale identifier and dropped
// as foreign.
func (p *pumpState) clearProvisionalSessionID() {
	p.sessionID = ""
	p.sessionIDKnown = false
	p.loadExpected = false
}

// observeReplay records that the pump has seen a chunk replayed for the session
// a session/load control message named, and resolves a pending replay query. A
// chunk observed before that control message arrives is not counted: it cannot
// confirm a load not yet attempted.
func (p *pumpState) observeReplay() {
	if !p.loadExpected || p.replayObserved {
		return
	}
	p.replayObserved = true
	if p.pendingReplayQuery == nil {
		return
	}
	q := p.pendingReplayQuery
	p.cancelPendingReplayQuery()
	q.reply <- true
}

// applyHandshakeCapabilityLowering lowers the capability record's stage-two
// entries from what the initialize response advertised. It never raises an
// entry, and always precedes the once-per-session notice.
func (p *pumpState) applyHandshakeCapabilityLowering(facts *handshakeFacts) {
	if !facts.agentInfoPresent {
		p.lowerCapability(&p.state.caps.agentVersion, capabilityLabelAgentVersion)
	}
	if !advertisesSessionContinuation(facts.caps) {
		p.lowerCapability(&p.state.caps.sessionContinuation, capabilityLabelSessionContinuation)
	}
	if facts.toolServersWithheld {
		p.lowerCapability(&p.state.caps.toolServers, capabilityLabelToolServers)
	}
	if p.reader != nil && !p.reader.Recognize(facts.agentInfo.Name, facts.agentInfo.Version) {
		p.dropReader()
	}
}

// dropReader gives up on the measurement source and lowers the token-counts
// entry to the gap it would have held with no source.
func (p *pumpState) dropReader() {
	p.reader = nil
	p.lowerCapability(&p.state.caps.tokenCounts, capabilityLabelTokenCounts)
}

// lowerCapability lowers entry to gap. A lowering after the once-per-session
// notice has been sent is logged at warn level rather than folded into a second
// notice.
func (p *pumpState) lowerCapability(entry *capabilityState, label string) {
	if !lower(entry) {
		return
	}
	if p.capabilityNoticeSent {
		p.state.logger.Warn("capability lowered after the once-per-session notice", slog.String("capability", label))
	}
}

func (p *pumpState) drainReadyItems() {
	for {
		item, ok := p.state.inbox.Take()
		if !ok {
			return
		}
		p.handleItem(item)
	}
}

// handleStreamEnd runs when jsonrpc.Conn.Done() closes. It first drains every
// queued item, because the reader completes its delivery into the inbox before
// it closes Done(), so a KindStreamEnd message it produced is already there.
// Only when the drain leaves a turn still in flight does this finalize it on the
// process-exit row.
func (p *pumpState) handleStreamEnd() {
	p.drainReadyItems()
	p.streamEnded = true
	if p.activeTurn == nil {
		return
	}
	p.recoverThenFinalize(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   p.state.release.TurnEndMessage(streamEndedMessage),
	})
}

// handleAbandonment runs once the release gives up on the connection's reader.
// Unlike handleStreamEnd it does not wait for state.conn.Done(): that reader is
// exactly what the release gave up on, and waiting for it would make this bound
// unbounded on a platform where closing the read end does not unpark a parked
// read. It first handles every queued item, so a response the reader delivered
// before it was given up on still decides the turn.
func (p *pumpState) handleAbandonment() {
	p.drainReadyItems()
	p.streamEnded = true
	if p.activeTurn == nil {
		return
	}
	p.finalizeTurn(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   p.state.release.TurnEndMessage(streamEndedMessage),
	})
}

// releaseAbandoned reports whether the release has given up on the connection's
// reader, even before runPump has handled that.
func (p *pumpState) releaseAbandoned() bool {
	select {
	case <-p.state.release.Abandoned():
		return true
	default:
		return false
	}
}

// armWriteFailedDeadline arms the bounded wait a write failure starts for the
// active turn, or returns nil when no turn is active. The reader stays free to
// end the turn through the stream-end path meanwhile; only a turn still active
// once this deadline elapses falls to handleWriteFailed.
func (p *pumpState) armWriteFailedDeadline() <-chan time.Time {
	if p.activeTurn == nil {
		return nil
	}
	return time.After(readTimeout(p.state))
}

// handleWriteFailed ends the active turn with the send-failure outcome. It runs
// only once a write failure's bounded wait has elapsed with the turn still
// active: a reader that ends inside that wait reports the turn through the
// stream-end path first, making this a no-op.
func (p *pumpState) handleWriteFailed() {
	if p.activeTurn == nil {
		return
	}
	p.recoverThenFinalize(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   promptSendFailedMessage,
	})
}

// logDroppedQueue logs, at warn level, any event this adapter would have
// delivered into a turn that never started before the session ended.
func (p *pumpState) logDroppedQueue() {
	if len(p.queued) == 0 {
		return
	}
	p.state.logger.Warn("dropping events queued for a turn that never started", slog.Int("count", len(p.queued)))
	p.queued = nil
}

// handleMessage dispatches one routed message. Dispatch is on method before
// kind: session/update is normalized and never answered, and
// session/request_permission is always answered, before either falls through to
// the generic kind-based handling.
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

// handleStreamEndMessage runs when a KindStreamEnd message reaches the pump
// through the ordinary message arm (the common case: the shared package
// delivers this message before it closes Done(), and the two select arms are
// chosen at random). A line above the connection's bound is a different
// condition from the loss of the subprocess and is not retried.
func (p *pumpState) handleStreamEndMessage(msg *jsonrpc.Message) {
	p.streamEnded = true
	if p.activeTurn == nil {
		return
	}
	if errors.Is(msg.Err, bufio.ErrTooLong) {
		p.recoverThenFinalize(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
			TerminalMessage:   lineTooLongMessage,
		})
		return
	}
	p.recoverThenFinalize(agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   p.state.release.TurnEndMessage(streamEndedMessage),
		Cause:             msg.Err,
	})
}

// handleResponse finalizes the active turn when msg answers its awaited request
// id. A response matching no awaited id is recorded at debug and ignored.
func (p *pumpState) handleResponse(msg *jsonrpc.Message) {
	turn := p.activeTurn
	if turn == nil || !turn.awaitedID.Present() || !msg.ID.Equal(turn.awaitedID) {
		p.state.logger.Debug("unmatched response", slog.String("id", msg.ID.String()))
		return
	}

	if msg.Error != nil {
		p.recoverThenFinalize(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrResponseError,
			TerminalMessage:   fmt.Sprintf("session/prompt error %d: %s", msg.Error.Code, msg.Error.Message),
		})
		return
	}

	var resp promptResponse
	if err := json.Unmarshal(msg.Result, &resp); err != nil {
		p.recoverThenFinalize(agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnOutcomeUnknown,
			TerminalMessage:   promptResponseUndecodedMessage,
		})
		return
	}

	evidence := stopReasonEvidence(resp.StopReason)
	bound, _ := spendLowerBound(resp.Meta)
	if bound > 0 {
		turn.modelReached = true
	}

	// A turn winding down toward a forced outcome already has a disposition
	// and, on the cancelled path, no figure to wait for.
	if p.reader == nil || bound <= 0 || turn.pendingEnd != turnEndNone {
		p.finalizeTurn(evidence)
		return
	}
	p.armDrain(turn, evidence, bound)
}

// armDrain hands one turn's measurement wait to its own goroutine and leaves the
// pump loop running: the source is batched behind a tick this transport cannot
// shorten, and a blocked pump would stop answering the runtime for that tick.
// The drain publishes its result back through the inbox, so the usage
// accumulator is touched only by the pump goroutine.
func (p *pumpState) armDrain(turn *activeTurn, evidence agentcore.TurnEvidence, bound int64) {
	turn.drainEvidence = evidence
	turn.draining = true
	if turn.deadlineC == nil {
		turn.deadlineC = time.After(readTimeout(p.state))
	}

	reader := p.reader
	inbox := p.state.inbox
	stop := p.state.stopCh

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()

		recovered, source, found := reader.Drain(ctx, bound)
		observed := &usageObserved{turn: turn, source: source, completeness: reader.Completeness()}
		if found {
			observed.recovered = &recovered
		}
		inbox.Put(pumpItem{control: &pumpControl{usage: observed}})
	}()
}

// handleUsageObserved applies one turn's drain result. A result for a turn that
// already ended some other way is discarded.
func (p *pumpState) handleUsageObserved(observed *usageObserved) {
	turn := p.activeTurn
	if turn == nil || turn != observed.turn {
		return
	}

	if observed.recovered == nil {
		if !p.drainProven {
			p.dropReader()
		}
		p.finalizeTurnWithUsage(turn.drainEvidence, observed)
		return
	}

	p.drainProven = true
	p.state.logger.Debug("usage recovered for a turn", slog.String("source", observed.source))
	p.finalizeTurnWithUsage(turn.drainEvidence, observed)
}

func (p *pumpState) handleSessionUpdateMessage(msg *jsonrpc.Message) {
	var notif sessionNotification
	if err := json.Unmarshal(msg.Params, &notif); err != nil {
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: malformedLineMessage})
		return
	}

	// Once the session identifier is known, an update naming a different session
	// describes a conversation this adapter did not create and is dropped.
	// Before that, every update is treated as the session's own.
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

	if sue.kind == updateUserMessageChunk || sue.kind == updateAgentMessageChunk {
		p.observeReplay()
	}
	p.observeModelReached(sue.kind)

	result := applySessionUpdate(p.tracker, sue)
	if result.hasEvent {
		p.emitOrQueue(result.event)
	}
}

// observeModelReached records that the active turn reached the model, for the
// kinds only a served model produces. A thought chunk counts, since reasoning
// tokens are billed; a replayed user chunk does not, being this session's own
// prompt echoed back.
func (p *pumpState) observeModelReached(kind sessionUpdateKind) {
	if p.activeTurn == nil {
		return
	}
	switch kind {
	case updateAgentMessageChunk, updateAgentThoughtChunk, updateToolCall:
		p.activeTurn.modelReached = true
	}
}

// emitOrQueue publishes ev into the active turn's sink, or queues it when no
// turn is in flight, to be flushed after the next turn's session_started event.
func (p *pumpState) emitOrQueue(ev domain.AgentEvent) {
	if p.activeTurn != nil {
		p.publish(p.activeTurn)(ev)
		return
	}
	p.queued = append(p.queued, ev)
}

// publish returns an emit function that delivers into turn's sink, guarded by
// turn's done channel so a publish can never block once the turn has ended.
func (p *pumpState) publish(turn *activeTurn) func(domain.AgentEvent) {
	return func(ev domain.AgentEvent) {
		select {
		case turn.sink <- ev:
		case <-turn.done:
		}
	}
}

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

// handleStartTurn accepts or rejects a startTurn control message and, on
// acceptance, starts the turn.
func (p *pumpState) handleStartTurn(ts *turnStart) {
	select {
	case <-ts.done:
		// The caller stopped waiting for this verdict. Accepting it would prompt
		// the agent for work no one collects and leave the session holding a turn
		// that can never end, rejecting every later turn as already in flight.
		return
	default:
	}

	if p.activeTurn != nil {
		ts.reply <- turnVerdict{accepted: false, err: &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: turnAlreadyInFlightMessage,
		}}
		return
	}
	if p.streamEnded || p.releaseAbandoned() {
		ts.reply <- turnVerdict{accepted: false, err: &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: p.state.release.TurnEndMessage(sessionEndedBeforeTurnMessage),
		}}
		return
	}

	turn := &activeTurn{sink: ts.sink, resultCh: ts.resultCh, done: ts.done, cancelCh: ts.cancelCh, capsSnapshot: *p.state.caps}
	p.activeTurn = turn
	ts.reply <- turnVerdict{accepted: true}

	agentcore.EmitSessionStarted(p.publish(turn), strconv.Itoa(p.state.pid), p.sessionID)
	p.emitCapabilityGapNoticeOnce(turn)
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

// emitCapabilityGapNoticeOnce emits the once-per-session notice listing turn's
// gap entries, the first time any turn starts. It reads turn's snapshot rather
// than the pump's live record, so the notice reports what the first turn saw.
func (p *pumpState) emitCapabilityGapNoticeOnce(turn *activeTurn) {
	if p.capabilityNoticeSent {
		return
	}
	p.capabilityNoticeSent = true

	message, hasGap := turn.capsSnapshot.gapNotice()
	if !hasGap {
		return
	}
	agentcore.EmitNotification(p.publish(turn), message)
}

// finalizeTurn ends the active turn on the disposition [activeTurn.disposition]
// settles.
func (p *pumpState) finalizeTurn(ev agentcore.TurnEvidence) {
	p.finalizeTurnWithUsage(ev, nil)
}

// finalizeTurnWithUsage ends the active turn, settling the drained figure into
// the session's run-cumulative snapshot. A nil observed, or one whose source
// held no record, leaves the snapshot and the measurement verdict as they stood.
func (p *pumpState) finalizeTurnWithUsage(ev agentcore.TurnEvidence, observed *usageObserved) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	p.activeTurn = nil
	ev = turn.disposition(ev)

	var recovered *agentcore.RecoveredUsage
	if observed != nil {
		recovered = observed.recovered
	}

	emit := p.publish(turn)
	result, agentErr := p.state.usage.Finalize(emit, p.state.logger, ev, p.sessionID, 0, recovered)

	// Unmeasured spend and a figure short of its turn are not folded into the
	// measurement verdict, which asserts a measurement exists, not that the
	// accounting is complete.
	result.SpendUnaccounted = turn.modelReached && !accountsForWholeTurn(observed)

	select {
	case turn.resultCh <- turnEnd{result: result, err: agentErr}:
	case <-turn.done:
	}
}

func accountsForWholeTurn(observed *usageObserved) bool {
	return observed != nil && observed.recovered != nil &&
		observed.completeness == usagesource.CompletenessAccounted
}

// recoverThenFinalize ends the active turn on ev, first giving the measurement
// source the one bounded chance it would otherwise never get: a turn that ends
// without a wire result still spent what its completed requests cost. ev is the
// disposition either way, no wire result is waited for, and a turn already
// draining keeps that drain.
func (p *pumpState) recoverThenFinalize(ev agentcore.TurnEvidence) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	if p.reader == nil || !turn.modelReached || turn.draining {
		p.finalizeTurn(ev)
		return
	}
	turn.endSettled = true
	p.armDrain(turn, ev, 0)
}

// beginEndAttempt marks the active turn as winding down toward kind, sends
// session/cancel exactly once, and arms the bounded wait for the prompt
// response.
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

// finalizeActiveTurnOnDeadline ends the active turn once its bounded wait for
// the prompt response has elapsed. The evidence passed here is overridden by
// finalizeTurn's own pendingEnd handling.
func (p *pumpState) finalizeActiveTurnOnDeadline() {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	// A turn whose bounded wait belongs to a drain already has its disposition;
	// the wait elapsing costs it its figure, not its outcome.
	if turn.draining {
		p.finalizeTurn(turn.drainEvidence)
		return
	}
	p.finalizeTurn(agentcore.TurnEvidence{Terminal: agentcore.TerminalFailure, TerminalErrorKind: domain.ErrTurnFailed})
}

// handlePermissionRequest answers a session/request_permission request and, when
// the shared posture ends the attempt, latches or begins that end.
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

	p.reportUncallableToolDelivery()

	if posture.Transmit {
		p.respondSelected(msg.ID, optionID)
	} else {
		p.respondCancelled(msg.ID)
	}

	if posture.EndAttempt {
		p.latchOrBeginEndAttempt("")
	}
}

// reportUncallableToolDelivery reports, once per session, that a session which
// delivered tool servers met a permission request the refusal posture answered,
// so any delivered tool the runtime gates the same way cannot be called.
func (p *pumpState) reportUncallableToolDelivery() {
	if !p.toolServersDelivered {
		return
	}
	if p.toolDeliveryReported {
		return
	}
	p.toolDeliveryReported = true

	agentcore.EmitNotification(p.emitOrQueue, toolDeliveryUncallableNotice)
	p.state.logger.Warn(toolDeliveryUncallableLog, slog.String("reason", "permission_refused"))
}

// answerMethodNotFound answers any request naming a method this client does not
// implement. elicitation/create additionally ends the attempt, per the posture
// for a request only a person could answer; every other unimplemented method
// emits one malformed event and does not end the attempt.
func (p *pumpState) answerMethodNotFound(msg *jsonrpc.Message) {
	if !msg.ID.Present() {
		return
	}
	p.openRequests[msg.ID] = msg.Method
	defer delete(p.openRequests, msg.ID)

	if err := p.state.conn.RespondError(msg.ID, jsonrpc.MethodNotFoundCode, jsonrpc.MethodNotFoundMessage); err != nil {
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

// latchOrBeginEndAttempt ends the active turn's attempt, or, when no turn is in
// flight, latches the human-input-required outcome so the next turn ends with it
// immediately.
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

// respondCancelled answers a permission request with the cancelled outcome.
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

// handleAnswerOpen answers every request the pump received but has not yet
// answered, best-effort, so teardown's first step has a receiver even when a
// reply from before teardown began is still outstanding.
func (p *pumpState) handleAnswerOpen() {
	for id, method := range p.openRequests {
		if method == methodSessionRequestPermission {
			p.respondCancelled(id)
		} else if err := p.state.conn.RespondError(id, jsonrpc.MethodNotFoundCode, jsonrpc.MethodNotFoundMessage); err != nil {
			p.state.logger.Debug("failed to write method-not-found reply", slog.Any("error", err))
		}
		delete(p.openRequests, id)
	}
}
