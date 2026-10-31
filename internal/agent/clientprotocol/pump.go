package clientprotocol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/toolrelay"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// Operator-facing messages; none interpolates a body, a raw line, or a code
// identifier.
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

	pendingEnd    turnEndKind
	pendingDetail string
	cancelSent    bool
	deadlineC     <-chan time.Time

	// spend lets a turn that reached the model with no figure count as spend
	// that occurred unmeasured.
	spend agentcore.TurnSpend

	// draining separates a turn waiting on its records read from one whose
	// bounded wait belongs to an end attempt; drainEvidence is the disposition
	// held meanwhile.
	drainEvidence agentcore.TurnEvidence
	draining      bool

	// endSettled marks a turn the transport has already seen end, kept in flight
	// only long enough to collect its figure.
	endSettled bool

	// pullPending marks a usage pull still running; a turn that would end
	// meanwhile waits in held for the pull's result.
	pullPending bool
	held        *heldEnd

	// pullArmed marks a pull owed while the turn's records read is in flight.
	pullArmed bool

	// spendEnd and spendLive are what the ledger settles the turn on while its
	// records read is in flight, so a turn that ends meanwhile settles on them.
	// spendSettled latches once the ledger has settled the turn, which keeps a
	// held turn from being settled a second time.
	spendEnd       usage.TurnEnd
	spendLive      *usage.SpendReading
	recordsPending bool
	spendSettled   bool

	// children is what the turn's updates showed of child sessions.
	children childSessions

	usageUpdateSeen bool

	// refusal is the usage-limit refusal the turn's notifications or prompt
	// error showed, a copy the turn owns. retrying latches once the runtime
	// announced a retry of a refused request, after which a notified refusal is
	// no longer the turn's.
	refusal  *usagesource.Refusal
	retrying bool

	// pulledWindows are the windows of the turn-end pull, nil when none ran or
	// it failed.
	pulledWindows []domain.UsageWindow
}

// childSessions is the child-session state of one running turn, kept in
// first-seen order and read and written only by the pump goroutine.
type childSessions struct {
	started        []string // calls that started a child session
	startedUnnamed bool     // a start named no call, so no completion can match it
	completedCalls []string // started calls that completed and named their session
	sessions       []string // child sessions the turn named
	settled        []string // sessions a completed call named
}

// note folds one session update's child signal into the state.
func (c *childSessions) note(signal usagesource.ChildSignal) {
	if signal.Started {
		if signal.Call == "" {
			c.startedUnnamed = true
		} else {
			c.started = appendNew(c.started, signal.Call)
		}
	}
	if signal.Session == "" {
		return
	}
	c.sessions = appendNew(c.sessions, signal.Session)
	if signal.Completed && signal.Call != "" {
		c.completedCalls = appendNew(c.completedCalls, signal.Call)
		c.settled = appendNew(c.settled, signal.Session)
	}
}

// end reports whether the turn showed a child session and whether every one it
// showed completed and named its identifier.
func (c *childSessions) end() (children, settled bool) {
	children = c.startedUnnamed || len(c.started) > 0 || len(c.sessions) > 0
	if !children {
		return false, false
	}
	settled = !c.startedUnnamed &&
		isSubset(c.started, c.completedCalls) &&
		isSubset(c.sessions, c.settled)
	return children, settled
}

func appendNew(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

func isSubset(members, of []string) bool {
	return !slices.ContainsFunc(members, func(member string) bool {
		return !slices.Contains(of, member)
	})
}

// spendTurnEnd builds the ledger's view of how the turn ended.
func (t *activeTurn) spendTurnEnd(replied, cancelled bool) usage.TurnEnd {
	children, settled := t.children.end()
	return usage.TurnEnd{
		Replied:         replied,
		Cancelled:       cancelled,
		ModelReached:    t.spend.ModelReached(),
		Children:        children,
		ChildrenSettled: settled,
	}
}

// heldEnd is the disposition and settled figure a turn ends on once its pull
// has returned.
type heldEnd struct {
	evidence agentcore.TurnEvidence
	observed *usageObserved
}

// disposition returns the evidence the turn is finalized on. A forced outcome
// wins unless the transport already saw how the turn ended: an end attempt
// arriving while such a turn waits for its figure stops nothing still running,
// and taking its outcome would misreport the failure.
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

// pumpState is the pump's mutable state. It lives only inside runPump's
// goroutine, which makes the pump the sole mutator of protocol state.
type pumpState struct {
	state *sessionState

	tracker *agentcore.ToolTracker

	declaration usagesource.Declaration

	loopDone <-chan struct{}

	// ledger is the session's spend accounting, set at the handshake and never
	// dropped. figureSeen latches once a turn produced a figure.
	ledger     *usage.Ledger
	figureSeen bool

	// pullInFlight keeps a second pull from starting while an earlier one holds
	// the puller, which is not safe for concurrent use.
	puller       *usage.Puller
	pullInFlight bool

	sessionID      string
	sessionIDKnown bool

	agentInfo        implementation
	agentInfoPresent bool
	caps             agentCapabilities

	queued []domain.AgentEvent

	agentMessageText      strings.Builder
	agentMessageID        *messageId
	agentMessageRunes     int
	agentMessageTruncated bool
	// agentMessageAt is when the message's last text chunk was observed. The
	// completed message carries it, so history a load replays before a turn
	// is not dated into that turn.
	agentMessageAt time.Time

	activeTurn *activeTurn

	latchedHumanInput       bool
	latchedHumanInputDetail string

	malformedVariantLogged bool
	streamEnded            bool

	// capabilityNoticeSent latches once the gap notice is out; later lowerings
	// log at warn instead of producing a second notice.
	capabilityNoticeSent bool

	toolServersDelivered bool
	toolDeliveryReported bool

	// openRequests holds the request being answered while it is answered, so
	// teardown's answerOpen step walks a defined set rather than assuming one.
	openRequests map[jsonrpc.ID]string

	// loadExpected gates observeReplay so a chunk seen before the session/load
	// call cannot confirm a load that replayed nothing.
	loadExpected   bool
	replayObserved bool

	// pendingReplayQuery and replayDeadlineC are set and cleared together.
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
		openRequests: make(map[jsonrpc.ID]string),
	}
	defer p.finalizeDecidedTurn()

	doneCh := state.conn.Done()
	doneFired := false
	writeFailedCh := state.conn.WriteFailed()
	var writeFailDeadlineC <-chan time.Time

	// abandonedCh is nil for a session built without a release.
	abandonedCh := state.release.Abandoned()
	// stopArm stays nil until abandonment: only then may the pump return on stop
	// without waiting for the reader.
	var stopArm <-chan struct{}
	stopCh := state.stopCh
	stopRequested := false

	loopDone := make(chan struct{})
	defer close(loopDone)
	p.loopDone = loopDone

	for {
		if stopRequested && p.settleForStop() {
			p.logDroppedQueue()
			return
		}

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
				stopRequested = true
				stopArm = nil
			}
			continue
		}

		// The reader has exited; control messages keep draining until teardown
		// closes the stop channel, the second condition for exit.
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
		case <-stopCh:
			stopRequested = true
			stopCh = nil
		}
	}
}

// settleForStop reports whether the pump may return. A turn awaiting a records
// read or pull is left to that result or its deadline: a stop never cuts a
// measurement short.
func (p *pumpState) settleForStop() bool {
	turn := p.activeTurn
	if turn == nil {
		return true
	}
	if !turn.awaitsBackgroundResult() {
		p.recoverThenFinalize(streamEndedEvidence(p.state, p.state.release.TurnEndMessage(streamEndedMessage), nil))
	}
	return p.activeTurn == nil
}

func (t *activeTurn) awaitsBackgroundResult() bool {
	return t.recordsPending || t.draining || t.pullPending
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
		p.declaration = ctrl.handshake.declaration
		p.puller = ctrl.handshake.puller
		p.ledger = ctrl.handshake.ledger
		p.applyHandshakeCapabilityLowering(ctrl.handshake)
		if p.ledger != nil && p.declaration.Live.Spend.Shape != usage.SpendUndeclared {
			p.state.caps.tokenCounts = capabilityOurs
		}

	case ctrl.sessionID != "":
		p.sessionID = ctrl.sessionID
		p.sessionIDKnown = true
		// Logged here rather than on the handshake message: the handshake is
		// published first, so only now are both the implementation record and the
		// session identifier known.
		p.state.logger.Info("agent implementation",
			slog.String("session_id", p.sessionID),
			slog.String("name", p.agentInfo.Name),
			slog.String("version", p.agentInfo.Version))

	case ctrl.expectLoad != "":
		// Adopted ahead of the sessionID message that always follows, so replayed
		// updates are not filtered as foreign. An unconfirmed load reverts it in
		// handleReplayQuery and finalizeReplayQueryOnDeadline.
		p.sessionID = ctrl.expectLoad
		p.sessionIDKnown = true
		p.loadExpected = true

	case ctrl.query != nil:
		p.handleReplayQuery(ctrl.query)

	case ctrl.startTurn != nil:
		p.handleStartTurn(ctrl.startTurn)

	case ctrl.pulled != nil:
		p.handlePullResult(ctrl.pulled)

	case ctrl.records != nil:
		p.handleRecordsRead(ctrl.records)

	case ctrl.answerOpen != nil:
		p.handleAnswerOpen()
		close(ctrl.answerOpen)
	}
}

// handleReplayQuery applies q. A nil reply means the call already failed: the
// provisional identifier is cleared and the entry lowered, nothing sent back.
// Otherwise it answers true if replay was already observed, or defers until a
// replayed chunk or the bounded wait resolves it.
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
	p.replayDeadlineC = time.After(agentcore.ReadTimeout(p.state.agentConfig))
}

// finalizeReplayQueryOnDeadline answers a pending replay query false once its
// bounded wait elapsed: the session/load succeeded but replayed nothing.
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

// cancelPendingReplayQuery disarms the deadline so an answered or abandoned
// wait cannot fire later against a replaced session.
func (p *pumpState) cancelPendingReplayQuery() {
	p.pendingReplayQuery = nil
	p.replayDeadlineC = nil
}

// clearProvisionalSessionID reverts the identifier expectLoad adopted. Left in
// place, it would make updates for the session session/new created drop as
// foreign.
func (p *pumpState) clearProvisionalSessionID() {
	p.sessionID = ""
	p.sessionIDKnown = false
	p.loadExpected = false
}

// observeReplay records a chunk replayed for the session a session/load control
// message named and resolves a pending replay query. A chunk seen before that
// message cannot confirm a load not yet attempted.
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

// applyHandshakeCapabilityLowering lowers capability entries from what the
// initialize response advertised. It never raises one, and precedes the
// once-per-session notice. The raise of the token-counts entry for a session
// with a ledger follows it in handleControl.
func (p *pumpState) applyHandshakeCapabilityLowering(facts *handshakeFacts) {
	if !facts.agentInfoPresent {
		p.lowerCapability(&p.state.caps.agentVersion, capabilityLabelAgentVersion)
	}
	if !advertisesSessionContinuation(facts.caps) {
		p.lowerCapability(&p.state.caps.sessionContinuation, capabilityLabelSessionContinuation)
	}
}

// lowerCapability lowers entry, logging at warn when the once-per-session
// notice has already gone out.
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

// handleStreamEnd runs when jsonrpc.Conn.Done() closes. It drains queued items
// first, because the reader delivers into the inbox before closing Done(), so a
// KindStreamEnd message is already there. Only a turn still in flight afterward
// is finalized as a process exit.
func (p *pumpState) handleStreamEnd() {
	p.drainReadyItems()
	p.streamEnded = true
	p.flushAgentMessage()
	if p.activeTurn == nil {
		return
	}
	p.recoverThenFinalize(streamEndedEvidence(p.state, p.state.release.TurnEndMessage(streamEndedMessage), nil))
}

func streamEndedEvidence(state *sessionState, message string, cause error) agentcore.TurnEvidence {
	if agentcore.ConnectionFailedForRequest(state.sshConnectionFailed(), false) {
		connErr := agentcore.ConnectionFailedError()
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: connErr.Kind,
			TerminalMessage:   connErr.Message,
			Cause:             connErr.Err,
		}
	}
	return agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   message,
		Cause:             cause,
	}
}

// handleAbandonment runs once the release gives up on the connection's reader.
// Unlike handleStreamEnd it does not wait for state.conn.Done(): where closing
// the read end does not unpark a parked read, that wait would never end. Queued
// items are handled first, so a response delivered before the reader was given
// up on still decides the turn.
func (p *pumpState) handleAbandonment() {
	p.drainReadyItems()
	p.streamEnded = true
	p.flushAgentMessage()
	if p.activeTurn == nil {
		return
	}
	ev := streamEndedEvidence(p.state, p.state.release.TurnEndMessage(streamEndedMessage), nil)
	if p.ledger != nil {
		p.endUnreplied(ev)
		return
	}
	p.finalizeTurn(ev)
}

// releaseAbandoned reports whether the release gave up on the reader, even
// before runPump has handled that.
func (p *pumpState) releaseAbandoned() bool {
	select {
	case <-p.state.release.Abandoned():
		return true
	default:
		return false
	}
}

// armWriteFailedDeadline arms the bounded wait a write failure starts, or
// returns nil when no turn is active. The reader stays free to end the turn
// through the stream-end path meanwhile.
func (p *pumpState) armWriteFailedDeadline() <-chan time.Time {
	if p.activeTurn == nil {
		return nil
	}
	return time.After(agentcore.ReadTimeout(p.state.agentConfig))
}

// handleWriteFailed ends the active turn with the send-failure outcome once the
// bounded wait elapsed; a reader that ended inside it already reported the turn,
// making this a no-op.
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

func (p *pumpState) logDroppedQueue() {
	if len(p.queued) == 0 {
		return
	}
	p.state.logger.Warn("dropping events queued for a turn that never started", slog.Int("count", len(p.queued)))
	p.queued = nil
}

// handleMessage dispatches one routed message, on method before kind:
// session/update is never answered and session/request_permission always is.
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
		p.handleTurnNotification(msg)
	case jsonrpc.KindMalformed:
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: malformedLineMessage})
	case jsonrpc.KindStreamEnd:
		p.handleStreamEndMessage(msg)
	}
}

// handleTurnNotification lets the declaration read a notification, in a turn or
// outside one, hands on the amounts it reports, and records what it shows of a
// retry or refusal in the active turn. A notification that shows none of these
// is only logged.
func (p *pumpState) handleTurnNotification(msg *jsonrpc.Message) {
	if !p.sessionIDKnown || p.declaration.TurnNotification == nil {
		p.state.logger.Debug("unhandled notification", slog.String("method", msg.Method))
		return
	}
	signal := p.declaration.TurnNotification(p.sessionID, msg.Method, msg.Params)
	applied := p.applyTurnSignal(signal)
	p.observeAmounts(signal.Amounts)
	if !applied && len(signal.Amounts) == 0 {
		p.state.logger.Debug("unhandled notification", slog.String("method", msg.Method))
	}
}

// applyTurnSignal records a retry or refusal in the active turn and reports
// whether the turn took one; outside a turn it takes none.
func (p *pumpState) applyTurnSignal(signal usagesource.TurnSignal) bool {
	turn := p.activeTurn
	if turn == nil || (!signal.Retrying && signal.Refusal == nil) {
		return false
	}
	if signal.Retrying {
		turn.retrying = true
	}
	if signal.Refusal != nil && turn.refusal == nil && !turn.retrying {
		refusal := *signal.Refusal
		turn.refusal = &refusal
	}
	return true
}

// handleStreamEndMessage handles a KindStreamEnd message that reached the pump
// through the message arm; the shared package delivers it before closing Done()
// and the select arms are chosen at random. A line above the connection's bound
// is not the loss of the subprocess and is not retried.
func (p *pumpState) handleStreamEndMessage(msg *jsonrpc.Message) {
	p.streamEnded = true
	p.flushAgentMessage()
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
	p.recoverThenFinalize(streamEndedEvidence(p.state, p.state.release.TurnEndMessage(streamEndedMessage), msg.Err))
}

// handleResponse finalizes the active turn when msg answers its awaited request
// id. A response matching no awaited id is recorded at debug and ignored.
func (p *pumpState) handleResponse(msg *jsonrpc.Message) {
	turn := p.activeTurn
	if turn == nil || !turn.awaitedID.Present() || !msg.ID.Equal(turn.awaitedID) {
		p.state.logger.Debug("unmatched response", slog.String("id", msg.ID.String()))
		return
	}

	p.armPull(turn)
	defer p.launchPull(turn)

	if msg.Error != nil {
		p.recoverThenFinalize(p.promptErrorEvidence(turn, msg.Error))
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
	if turn.refusal != nil && resp.StopReason != stopReasonCancelled {
		evidence = refusalEvidence(*turn.refusal, refusalMessage(*turn.refusal))
	}
	bound, _ := spendLowerBound(resp.Meta)
	if bound > 0 {
		turn.spend.MarkModelReached()
	}

	if p.ledger != nil {
		var live *usage.SpendReading
		if p.declaration.PromptSpend != nil {
			live = p.declaration.PromptSpend(msg.Result)
		}
		p.settleReply(turn, evidence, resp.StopReason == stopReasonCancelled, live)
		return
	}

	p.settleTurn(evidence, nil)
}

// promptErrorEvidence maps a session/prompt error. The declaration's verdict
// outranks the authentication-required code, which a runtime also uses for its
// usage limits; a refusal a notification showed earlier does not.
func (p *pumpState) promptErrorEvidence(turn *activeTurn, e *jsonrpc.Error) agentcore.TurnEvidence {
	message := fmt.Sprintf("session/prompt error %d: %s", e.Code, quoteJSONRPCError(e))

	var verdict usagesource.PromptVerdict
	if p.declaration.PromptError != nil {
		verdict = p.declaration.PromptError(*e)
	}
	switch {
	case verdict.Refusal != nil:
		refusal := *verdict.Refusal
		turn.refusal = &refusal
		return refusalEvidence(refusal, message)
	case verdict.Transient:
		return responseErrorEvidence(message)
	case e.Code == errorCodeAuthRequired:
		return agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrCredentialUnverified,
			TerminalMessage:   agentcore.CredentialRefusedMessage(quoteJSONRPCError(e)),
		}
	case turn.refusal != nil:
		return refusalEvidence(*turn.refusal, message)
	}
	return responseErrorEvidence(message)
}

func responseErrorEvidence(message string) agentcore.TurnEvidence {
	return agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrResponseError,
		TerminalMessage:   message,
	}
}

// refusalEvidence carries a copy of the refusal's limit, so a reset filled in
// later never reaches the refusal the turn keeps.
func refusalEvidence(r usagesource.Refusal, message string) agentcore.TurnEvidence {
	limit := r.Limit
	return agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrUsageLimit,
		UsageLimit:        &limit,
		TerminalMessage:   message,
	}
}

func refusalMessage(r usagesource.Refusal) string {
	if r.Message == "" {
		return "usage-limit refusal"
	}
	return fmt.Sprintf("usage-limit refusal: %q", redact.Truncate(r.Message, messageTruncateLimit))
}

// contextUntilStop returns a context that ends when stop closes or cancel is
// called, so a background wait never outlives the session.
func contextUntilStop(stop <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// settleReply settles a turn whose prompt result arrived through the ledger. The
// live reading comes from the raw result because the generated response type
// carries no usage member, and the ledger alone decides whether the records are
// needed.
func (p *pumpState) settleReply(turn *activeTurn, evidence agentcore.TurnEvidence, cancelled bool, live *usage.SpendReading) {
	end := turn.spendTurnEnd(true, cancelled)
	if p.ledger.NeedsRecords(end, live) {
		p.startRecordsRead(turn, evidence, end, live)
		return
	}
	p.settleTurn(evidence, p.settleSpend(turn, end, live, usage.RecordsRead{}))
}

// startRecordsRead reads the runtime's records for turn on its own goroutine: the
// read touches the host's disk, and a blocked pump would stop answering the
// runtime meanwhile. The result returns through the inbox, so the ledger stays
// on the pump goroutine. The read ends with the main loop, not a stop, so a
// stopped turn settles on the same reading as one that was not.
func (p *pumpState) startRecordsRead(turn *activeTurn, evidence agentcore.TurnEvidence, end usage.TurnEnd, live *usage.SpendReading) {
	turn.drainEvidence = evidence
	turn.draining = true
	turn.recordsPending = true
	turn.spendEnd = end
	turn.spendLive = live
	if turn.deadlineC == nil {
		turn.deadlineC = time.After(agentcore.ReadTimeout(p.state.agentConfig))
	}

	state := p.state
	declaration := p.declaration
	children := slices.Clone(turn.children.sessions)
	inbox := state.inbox
	loopDone := p.loopDone

	go func() {
		ctx, cancel := contextUntilStop(loopDone)
		defer cancel()

		read, refusal := readTurnRecords(ctx, state, declaration, children)
		inbox.Put(pumpItem{control: &pumpControl{records: &recordsResult{turn: turn, read: read, refusal: refusal}}})
	}()
}

// handleRecordsRead settles the turn a records read was started for, discarding
// the result of a turn that already ended or was settled without it.
func (p *pumpState) handleRecordsRead(result *recordsResult) {
	turn := p.activeTurn
	if turn == nil || turn != result.turn || !turn.recordsPending {
		return
	}
	applyRecordedReset(turn, result.refusal)
	p.settleTurn(turn.drainEvidence, p.settleSpend(turn, turn.spendEnd, turn.spendLive, result.read))
}

// applyRecordedReset gives the held refusal the reset the runtime's records
// hold for it. A record naming another refusal, or carrying no reset, leaves
// the evidence as it stood.
func applyRecordedReset(turn *activeTurn, recorded usagesource.RecordedRefusal) {
	limit := turn.drainEvidence.UsageLimit
	if turn.refusal == nil || turn.refusal.Name == "" || recorded.Name != turn.refusal.Name ||
		recorded.ResetsAt.IsZero() || limit == nil || turn.drainEvidence.TerminalErrorKind != domain.ErrUsageLimit {
		return
	}
	limit.ResetsAt = recorded.ResetsAt
}

// settleSpend runs the ledger over turn and applies the settlement to the turn's
// spend evidence.
func (p *pumpState) settleSpend(turn *activeTurn, end usage.TurnEnd, live *usage.SpendReading, read usage.RecordsRead) *usageObserved {
	settlement := p.ledger.Settle(end, live, read)
	turn.recordsPending = false
	turn.spendSettled = true
	p.launchPull(turn)
	p.noteSpendFigure(end, settlement)
	return &usageObserved{turn: turn, recovered: turn.spend.ApplySettlement(settlement)}
}

// noteSpendFigure lowers the token-counts entry when the first turn that reached
// the model ends with no figure and no earlier turn had one.
func (p *pumpState) noteSpendFigure(end usage.TurnEnd, settlement usage.Settlement) {
	if settlement.Figure {
		p.figureSeen = true
		return
	}
	if end.ModelReached && !p.figureSeen {
		p.lowerCapability(&p.state.caps.tokenCounts, capabilityLabelTokenCounts)
	}
}

// endUnreplied settles a turn that ends without a prompt result. A turn whose
// records read is in flight settles at once on the end and reading it stored,
// without waiting for the read.
func (p *pumpState) endUnreplied(ev agentcore.TurnEvidence) {
	turn := p.activeTurn
	switch {
	case turn == nil:
		return
	case turn.spendSettled:
		p.settleTurn(ev, nil)
	case turn.recordsPending:
		p.settleTurn(ev, p.settleSpend(turn, turn.spendEnd, turn.spendLive, usage.RecordsRead{}))
	default:
		end := turn.spendTurnEnd(false, false)
		if p.ledger.NeedsRecords(end, nil) {
			turn.endSettled = true
			p.startRecordsRead(turn, ev, end, nil)
			return
		}
		p.settleTurn(ev, p.settleSpend(turn, end, nil, usage.RecordsRead{}))
	}
}

// pullEnding names a turn's pending end in the terms the usage component's
// pull policy reads.
func pullEnding(end turnEndKind) usage.TurnEnding {
	switch end {
	case turnEndCancelled:
		return usage.EndingCancelled
	case turnEndHumanInput:
		return usage.EndingHumanInput
	default:
		return usage.EndingAnswered
	}
}

// armPull marks a turn whose ending the usage component pulls after as owing a
// pull, and holds its wait open for the puller's bound, which the shorter
// records-read wait would cut. launchPull starts the pull.
func (p *pumpState) armPull(turn *activeTurn) {
	ending := pullEnding(turn.pendingEnd)
	if !p.puller.PullsAfter(ending) || p.pullInFlight {
		return
	}
	select {
	case <-p.state.stopCh:
		return
	default:
	}

	turn.pullPending = true
	turn.pullArmed = true
	p.pullInFlight = true
	if turn.deadlineC == nil {
		cfg := p.state.agentConfig
		turn.deadlineC = time.After(max(agentcore.ReadTimeout(cfg), agentcore.AuxiliaryTimeout(cfg)))
	}
}

// launchPull runs the armed pull on its own goroutine once no records read of
// the turn is in flight; the result returns through the inbox.
//
// The pull and the read each start the runtime on the session's host, and two
// runs in one workspace can contend for its state, failing the read and costing
// the figure its served model. settleSpend launches the pull when the read ends.
func (p *pumpState) launchPull(turn *activeTurn) {
	if !turn.pullArmed || turn.recordsPending {
		return
	}
	turn.pullArmed = false

	ending := pullEnding(turn.pendingEnd)
	puller := p.puller
	inbox := p.state.inbox
	stop := p.state.stopCh

	go func() {
		ctx, cancel := contextUntilStop(stop)
		defer cancel()

		windows, err := puller.Pull(ctx, ending)
		inbox.Put(pumpItem{control: &pumpControl{pulled: &pullResult{turn: turn, windows: windows, err: err}}})
	}()
}

// handlePullResult publishes the pulled windows ahead of the turn's terminal
// event and ends the turn if it was waiting on the pull. A result for a turn
// that already ended is dropped.
func (p *pumpState) handlePullResult(result *pullResult) {
	p.pullInFlight = false
	turn := p.activeTurn
	if turn == nil || turn != result.turn {
		return
	}
	turn.pullPending = false

	if result.err != nil {
		p.state.logger.Debug("usage pull failed", slog.Any("error", result.err))
	} else {
		turn.pulledWindows = result.windows
	}
	if len(result.windows) > 0 {
		agentcore.EmitUsageWindows(p.publishOrQueue, time.Now().UTC(), result.windows)
	}
	if held := turn.held; held != nil {
		p.finalizeTurnWithUsage(held.evidence, held.observed)
	}
}

// settleTurn ends the active turn on ev, or holds that end until the turn's
// pull has returned so its windows precede the terminal event.
func (p *pumpState) settleTurn(ev agentcore.TurnEvidence, observed *usageObserved) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	if !turn.pullPending {
		p.finalizeTurnWithUsage(ev, observed)
		return
	}
	if observed == nil && turn.held != nil {
		observed = turn.held.observed
	}
	turn.held = &heldEnd{evidence: ev, observed: observed}
}

// isForeignSession reports whether id names a conversation this adapter did not
// create. Before the session identifier is known, every update counts as the
// session's own.
func (p *pumpState) isForeignSession(id sessionId) bool {
	return p.sessionIDKnown && string(id) != p.sessionID
}

func (p *pumpState) handleSessionUpdateMessage(msg *jsonrpc.Message) {
	var notif sessionNotification
	if err := json.Unmarshal(msg.Params, &notif); err != nil {
		p.emitOrQueue(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: time.Now().UTC(), Message: malformedLineMessage})
		return
	}

	if p.isForeignSession(notif.SessionID) {
		p.state.logger.Debug("dropping session/update for a foreign session")
		return
	}

	p.observeSessionUpdateAmounts(notif.Update.Remainder)

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
	p.observeChildSession(notif.Update.Remainder)
	p.observeModelReached(sue.kind)
	if sue.kind == updateUsage {
		p.observeUsageUpdate()
	}

	result := applySessionUpdate(p.tracker, sue)
	switch {
	case result.messageChunk:
		p.handleAgentMessageChunk(result.messageID, result.messageText, result.event.Timestamp)
		// emitOrQueue would complete the message this chunk belongs to.
		if result.hasEvent {
			p.publishOrQueue(result.event)
		}
		return
	case sue.kind == updateUserMessageChunk:
		p.flushAgentMessage()
	}
	if result.hasEvent {
		p.emitOrQueue(result.event)
	}
}

// observeChildSession folds what the declared runtime shows of a child session
// in update into the running turn. Updates outside a turn show nothing the
// ledger counts.
func (p *pumpState) observeChildSession(update json.RawMessage) {
	turn := p.activeTurn
	if turn == nil || !p.sessionIDKnown || p.declaration.ChildSession == nil {
		return
	}
	turn.children.note(p.declaration.ChildSession(p.sessionID, update))
}

// observeSessionUpdateAmounts hands the amounts the declaration reads off a
// session update to the ledger, whatever the update's variant.
func (p *pumpState) observeSessionUpdateAmounts(update json.RawMessage) {
	if p.declaration.SessionUpdateAmounts == nil {
		return
	}
	p.observeAmounts(p.declaration.SessionUpdateAmounts(update))
}

// observeAmounts hands readings to the ledger, which counts those read in a
// turn at the session's next settlement and none read outside one.
func (p *pumpState) observeAmounts(readings []usage.AmountReading) {
	if p.ledger == nil || len(readings) == 0 {
		return
	}
	p.ledger.ObserveAmounts(readings, p.activeTurn != nil)
}

func (p *pumpState) observeUsageUpdate() {
	if p.activeTurn != nil {
		p.activeTurn.usageUpdateSeen = true
	}
}

func sameMessageID(a, b *messageId) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (p *pumpState) handleAgentMessageChunk(id *messageId, text string, at time.Time) {
	if p.agentMessageRunes > 0 && !sameMessageID(p.agentMessageID, id) {
		p.flushAgentMessage()
	}
	if text == "" {
		return
	}
	if p.agentMessageRunes == 0 {
		p.agentMessageID = id
	}
	p.agentMessageAt = at
	for _, r := range text {
		if p.agentMessageRunes == messageTruncateLimit {
			p.agentMessageTruncated = true
			break
		}
		p.agentMessageText.WriteRune(r)
		p.agentMessageRunes++
	}
}

func (p *pumpState) flushAgentMessage() {
	if p.agentMessageRunes == 0 {
		return
	}

	text := p.agentMessageText.String()
	if p.agentMessageTruncated {
		text = redact.MaskTruncated(text)
		text = redact.Truncate(text, messageTruncateLimit)
		if !strings.HasSuffix(text, "…") {
			text += "…"
		}
	} else {
		text = redact.Truncate(text, messageTruncateLimit)
	}
	p.publishOrQueue(domain.AgentEvent{Type: domain.EventNotification, Timestamp: p.agentMessageAt, Message: text})

	p.agentMessageText.Reset()
	p.agentMessageID = nil
	p.agentMessageAt = time.Time{}
	p.agentMessageRunes = 0
	p.agentMessageTruncated = false
}

// observeModelReached records that the active turn reached the model. A thought
// chunk counts, since reasoning tokens are billed; a replayed user chunk does
// not, being this session's own prompt echoed back. Under a declaration that
// sends a usage_update before each model request, an agent message chunk ahead
// of the turn's first one is the runtime's own text and does not count.
func (p *pumpState) observeModelReached(kind sessionUpdateKind) {
	if p.activeTurn == nil {
		return
	}
	switch kind {
	case updateAgentMessageChunk:
		if p.declaration.UsageUpdateBeforeRequest && !p.activeTurn.usageUpdateSeen {
			return
		}
		p.activeTurn.spend.MarkModelReached()
	case updateAgentThoughtChunk, updateToolCall:
		p.activeTurn.spend.MarkModelReached()
	}
}

// emitOrQueue completes any agent message still being accumulated before
// publishing ev, so events keep the order the agent produced them in.
func (p *pumpState) emitOrQueue(ev domain.AgentEvent) {
	p.flushAgentMessage()
	p.publishOrQueue(ev)
}

// publishOrQueue publishes ev into the active turn's sink, or queues it for
// flushing after the next turn's session_started event.
func (p *pumpState) publishOrQueue(ev domain.AgentEvent) {
	if p.activeTurn != nil {
		p.publish(p.activeTurn)(ev)
		return
	}
	p.queued = append(p.queued, ev)
}

func (p *pumpState) publish(turn *activeTurn) func(domain.AgentEvent) {
	return p.state.tools.Emit(deliverToTurn(turn), p.openingNotice)
}

// deliverToTurn is guarded by turn's done channel so a delivery never blocks
// once the turn has ended.
func deliverToTurn(turn *activeTurn) func(domain.AgentEvent) {
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
		// Accepting a verdict nobody waits for would prompt the agent for work no
		// one collects and leave a turn that can never end, rejecting every later
		// turn as already in flight.
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

	p.flushAgentMessage()
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

// openingNotice lowers the tool-server gap first so the notice lists it beside
// every other gap.
func (p *pumpState) openingNotice(emit func(domain.AgentEvent), toolServersGap bool) {
	if toolServersGap {
		p.lowerCapability(&p.state.caps.toolServers, toolrelay.ToolServersLabel)
	}
	p.capabilityNoticeSent = true

	if message, hasGap := p.state.caps.gapNotice(); hasGap {
		agentcore.EmitNotification(emit, message)
	}
}

// finalizeTurn ends the active turn on the disposition [activeTurn.disposition]
// settles.
func (p *pumpState) finalizeTurn(ev agentcore.TurnEvidence) {
	p.finalizeTurnWithUsage(ev, nil)
}

// finalizeTurnWithUsage ends the active turn, settling the recovered figure into
// the run-cumulative snapshot. A nil observed, or one with no record, leaves the
// snapshot as it stood.
func (p *pumpState) finalizeTurnWithUsage(ev agentcore.TurnEvidence, observed *usageObserved) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	p.flushAgentMessage()
	p.activeTurn = nil
	ev = turn.disposition(ev)
	p.resetFromPulledWindows(turn, ev)

	var recovered *agentcore.RecoveredUsage
	if observed != nil {
		recovered = observed.recovered
	}

	emit := p.publish(turn)
	result, agentErr := p.state.usage.Finalize(emit, p.state.logger, ev, p.sessionID, 0, recovered, turn.spend)

	select {
	case turn.resultCh <- turnEnd{result: result, err: agentErr}:
	case <-turn.done:
	}
}

// resetFromPulledWindows fills a zero reset of the turn's refusal with the one
// the usage component chooses among the windows of the turn-end pull, under the
// configured model. A reset the records already set is never replaced.
func (p *pumpState) resetFromPulledWindows(turn *activeTurn, ev agentcore.TurnEvidence) {
	if turn.refusal == nil || !turn.refusal.WindowReset || ev.TerminalErrorKind != domain.ErrUsageLimit ||
		ev.UsageLimit == nil || !ev.UsageLimit.ResetsAt.IsZero() {
		return
	}
	ev.UsageLimit.ResetsAt = usage.RefusalReset(turn.pulledWindows, p.state.configuredModel, time.Now().UTC())
}

// recoverThenFinalize ends the active turn on ev: a turn that ends without a
// wire result still spent what its completed requests cost, which the ledger
// settles. ev is the disposition either way.
func (p *pumpState) recoverThenFinalize(ev agentcore.TurnEvidence) {
	turn := p.activeTurn
	if turn == nil {
		return
	}
	if p.ledger != nil {
		p.endUnreplied(ev)
		return
	}
	p.settleTurn(ev, nil)
}

// beginEndAttempt marks the active turn as winding down toward kind, sends
// session/cancel once, and arms the bounded wait for the prompt response.
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
		turn.deadlineC = time.After(agentcore.ReadTimeout(p.state.agentConfig))
	}
}

// finalizeDecidedTurn ends the active turn on the disposition it already has,
// when it has one, and reports whether it did. The wait elapsing, or the pump
// exiting, costs a held turn its windows and a draining turn its figure, never
// its outcome; the pull or read behind either delivers into an inbox no one
// reads once the pump is gone, and RunTurn would wait on the turn for good.
func (p *pumpState) finalizeDecidedTurn() bool {
	turn := p.activeTurn
	switch {
	case turn == nil:
		return false
	case turn.held != nil:
		p.finalizeTurnWithUsage(turn.held.evidence, turn.held.observed)
	case turn.recordsPending:
		p.finalizeTurnWithUsage(turn.drainEvidence, p.settleSpend(turn, turn.spendEnd, turn.spendLive, usage.RecordsRead{}))
	case turn.draining:
		p.finalizeTurn(turn.drainEvidence)
	default:
		return false
	}
	return true
}

// finalizeActiveTurnOnDeadline ends the active turn once its bounded wait for
// the prompt response has elapsed; pendingEnd overrides the evidence given here.
func (p *pumpState) finalizeActiveTurnOnDeadline() {
	turn := p.activeTurn
	if turn == nil || p.finalizeDecidedTurn() {
		return
	}
	ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalFailure, TerminalErrorKind: domain.ErrTurnFailed}
	if p.ledger == nil || turn.spendSettled {
		p.finalizeTurn(ev)
		return
	}
	end := turn.spendTurnEnd(false, false)
	if p.ledger.NeedsRecords(end, nil) {
		// The elapsed wait is spent; the read gets a wait of its own.
		turn.deadlineC = nil
		p.startRecordsRead(turn, ev, end, nil)
		return
	}
	p.finalizeTurnWithUsage(ev, p.settleSpend(turn, end, nil, usage.RecordsRead{}))
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
	if err := json.Unmarshal(dropMalformedToolCallNameFromParams(msg.Params), &params); err != nil {
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

// dropMalformedToolCallNameFromParams drops a wrong-typed "name" from raw's
// "toolCall" member and substitutes the result back in.
func dropMalformedToolCallNameFromParams(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	toolCall, present := fields["toolCall"]
	if !present {
		return raw
	}
	fields["toolCall"] = dropMalformedToolCallName(toolCall)
	adapted, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return adapted
}

// reportUncallableToolDelivery reports once per session that a permission
// request met the refusal posture after tool servers were delivered, so any tool
// the runtime gates the same way cannot be called.
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

// answerMethodNotFound answers a request naming a method this client does not
// implement. elicitation/create also ends the attempt, being a request only a
// person could answer; every other method emits one malformed event.
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

// latchOrBeginEndAttempt ends the active turn's attempt, or with no turn in
// flight latches the human-input outcome for the next turn.
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

// handleAnswerOpen answers every request received but not yet answered,
// best-effort, so teardown's first step has a receiver.
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
