package clientprotocol

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// newPumpForCapabilityTests builds a *sessionState with a local
// launch's stage-one capability record and a *pumpState pointing at
// it, with no connection or subprocess involved: everything below
// exercises the record's mutation path directly, never a decision that
// needs a live wire.
func newPumpForCapabilityTests(t *testing.T) (*sessionState, *pumpState) {
	t.Helper()
	state := &sessionState{caps: newCapabilityRecord(false), logger: discardLogger()}
	return state, &pumpState{state: state}
}

// TestNewCapabilityRecordToolServersStageOne confirms the stage-one
// toolServers state is decided by the remote flag alone: a remote
// launch starts at gap because a remote runtime cannot be trusted to
// deliver a locally-generated tool-server configuration, while a local
// launch starts at protocol.
func TestNewCapabilityRecordToolServersStageOne(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		remote    bool
		wantState capabilityState
	}{
		{name: "a local launch starts toolServers at protocol", remote: false, wantState: capabilityProtocol},
		{name: "a remote launch starts toolServers at gap", remote: true, wantState: capabilityGap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			record := newCapabilityRecord(tt.remote)

			if record.toolServers != tt.wantState {
				t.Errorf("newCapabilityRecord(%v).toolServers = %q, want %q", tt.remote, record.toolServers, tt.wantState)
			}
		})
	}
}

// TestNewCapabilityRecordStageOneEntries confirms all four stage-one
// entries independently of any handshake, for both launch arms. The
// sessionContinuation case is the load-bearing one: it must start at
// protocol whichever way the launch resolves, because nothing is known
// yet about whether the agent advertises a continuation method. Only a
// handshake-free assertion can prove it, because the handshake lowers
// that entry when neither method is advertised and would mask a wrong
// default.
func TestNewCapabilityRecordStageOneEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remote bool
	}{
		{name: "local launch", remote: false},
		{name: "remote launch", remote: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			record := newCapabilityRecord(tt.remote)

			wantToolServers := capabilityProtocol
			if tt.remote {
				wantToolServers = capabilityGap
			}
			if record.toolServers != wantToolServers {
				t.Errorf("newCapabilityRecord(%v).toolServers = %q, want %q", tt.remote, record.toolServers, wantToolServers)
			}
			if record.tokenCounts != capabilityGap {
				t.Errorf("newCapabilityRecord(%v).tokenCounts = %q, want %q", tt.remote, record.tokenCounts, capabilityGap)
			}
			if record.sessionContinuation != capabilityProtocol {
				t.Errorf("newCapabilityRecord(%v).sessionContinuation = %q, want %q", tt.remote, record.sessionContinuation, capabilityProtocol)
			}
			if record.agentVersion != capabilityProtocol {
				t.Errorf("newCapabilityRecord(%v).agentVersion = %q, want %q", tt.remote, record.agentVersion, capabilityProtocol)
			}
		})
	}
}

// TestCapabilityLoweringNeverRises confirms an entry the handshake
// lowers to gap stays there for the rest of the session: a later
// application reporting the same capability as present must not move
// the entry back to protocol.
func TestCapabilityLoweringNeverRises(t *testing.T) {
	t.Parallel()

	state, p := newPumpForCapabilityTests(t)

	p.applyHandshakeCapabilityLowering(&handshakeFacts{agentInfoPresent: false, toolServersWithheld: true})
	if state.caps.agentVersion != capabilityGap {
		t.Fatalf("agentVersion after the first lowering = %q, want %q", state.caps.agentVersion, capabilityGap)
	}
	if state.caps.toolServers != capabilityGap {
		t.Fatalf("toolServers after the first lowering = %q, want %q", state.caps.toolServers, capabilityGap)
	}

	// A later application reporting the capability as present must not
	// raise either entry back to protocol; nothing in this record's
	// mutation path offers a rise, and this proves that rather than
	// merely assuming it.
	p.applyHandshakeCapabilityLowering(&handshakeFacts{agentInfoPresent: true, toolServersWithheld: false})
	if state.caps.agentVersion != capabilityGap {
		t.Errorf("agentVersion after a later report of presence = %q, want %q: an entry must never rise once lowered", state.caps.agentVersion, capabilityGap)
	}
	if state.caps.toolServers != capabilityGap {
		t.Errorf("toolServers after a later report of delivery = %q, want %q: an entry must never rise once lowered", state.caps.toolServers, capabilityGap)
	}
}

// TestHandshakeLoweringPersistsAcrossTurns confirms the same invariant
// end to end, through the running pump and two successive turns of the
// same session, rather than through a direct call alone. It reads the
// invariant through the event stream rather than through state.caps: the
// pump goroutine is that record's sole owner, and reading its fields
// from the test goroutine is race-free only by accident of this piece
// having a single lowering trigger. A later piece that schedules
// lowering from the pump mid-turn would turn that direct read into a
// genuine data race.
func TestHandshakeLoweringPersistsAcrossTurns(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{agentInfoPresent: false}}}
	markSessionKnown(state)

	var firstTurnEvents []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&firstTurnEvents)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("first RunTurn() error = %v, want nil", outcome.err)
	}

	notice := firstNotification(t, firstTurnEvents)
	if !strings.Contains(notice.Message, capabilityLabelAgentVersion) {
		t.Fatalf("first turn gap notice = %q, want it to list %q: the handshake lowered agentVersion", notice.Message, capabilityLabelAgentVersion)
	}

	// A second turn must not repeat the notice: the once-per-session gate
	// already proves the first turn's lowering was not undone and
	// reapplied, without this goroutine touching the pump-owned record.
	var secondTurnEvents []domain.AgentEvent
	outcomeCh2 := runTurnAsync(state, domain.RunTurnParams{Prompt: "go again", OnEvent: collectEvents(&secondTurnEvents)})
	promptID2 := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID2, promptResponse{StopReason: stopReasonEndTurn})
	outcome2 := awaitOutcome(t, outcomeCh2)
	if outcome2.err != nil {
		t.Fatalf("second RunTurn() error = %v, want nil", outcome2.err)
	}
	for _, ev := range secondTurnEvents {
		if ev.Type == domain.EventNotification {
			t.Errorf("second turn emitted a notification %q, want none: the lowering already reported by the first turn must not fire again", ev.Message)
		}
	}
}

// firstNotification returns the first domain.EventNotification event in
// events, failing t if none is present.
func firstNotification(t *testing.T, events []domain.AgentEvent) domain.AgentEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Type == domain.EventNotification {
			return ev
		}
	}
	t.Fatal("no notification event observed, want the gap notice")
	return domain.AgentEvent{}
}

// TestTurnSnapshotIsolatesMidTurnLowering confirms a turn reads the
// capability record as it stood when the turn began: capsSnapshot is a
// value copy taken at turn start, so a lowering the pump applies while
// that turn is still in flight does not retroactively change what the
// turn already read. The lowering that takes effect from the next turn
// onward is exercised here directly, because this piece's own
// handshake is the only lowering trigger this transport has landed so
// far; the observation-based trigger of a later piece lands on the
// same lowerCapability path this test drives.
func TestTurnSnapshotIsolatesMidTurnLowering(t *testing.T) {
	t.Parallel()

	state, p := newPumpForCapabilityTests(t)
	turn := &activeTurn{capsSnapshot: *state.caps}

	if turn.capsSnapshot.agentVersion != capabilityProtocol {
		t.Fatalf("turn snapshot agentVersion at turn start = %q, want %q", turn.capsSnapshot.agentVersion, capabilityProtocol)
	}

	p.lowerCapability(&state.caps.agentVersion, capabilityLabelAgentVersion)

	if state.caps.agentVersion != capabilityGap {
		t.Fatalf("live record agentVersion after the lowering = %q, want %q", state.caps.agentVersion, capabilityGap)
	}
	if turn.capsSnapshot.agentVersion != capabilityProtocol {
		t.Errorf("turn snapshot agentVersion after a mid-turn lowering = %q, want %q: a lowering observed mid-turn must not change decisions the turn already made", turn.capsSnapshot.agentVersion, capabilityProtocol)
	}

	// The next turn's own snapshot, taken after the lowering, does see it.
	nextTurn := &activeTurn{capsSnapshot: *state.caps}
	if nextTurn.capsSnapshot.agentVersion != capabilityGap {
		t.Errorf("next turn's snapshot agentVersion = %q, want %q: the lowering takes effect from the following turn", nextTurn.capsSnapshot.agentVersion, capabilityGap)
	}
}

// TestEmitCapabilityGapNoticeOnceReadsTurnSnapshot confirms the notice
// is assembled from the turn's own capsSnapshot, not from the pump's
// live record. The two tables below diverge the snapshot from the live
// record in opposite directions: a snapshot that already carries a gap
// the live record lacks, and a live record that carries a gap the
// snapshot lacks. Reading the live record instead of the snapshot
// would report the wrong outcome in both directions, even though a
// production turn always builds the two equal in the same call.
func TestEmitCapabilityGapNoticeOnceReadsTurnSnapshot(t *testing.T) {
	t.Parallel()

	allProtocol := capabilityRecord{
		toolServers:         capabilityProtocol,
		tokenCounts:         capabilityProtocol,
		sessionContinuation: capabilityProtocol,
		agentVersion:        capabilityProtocol,
	}
	withTokenCountsGap := allProtocol
	withTokenCountsGap.tokenCounts = capabilityGap

	tests := []struct {
		name         string
		liveCaps     capabilityRecord
		capsSnapshot capabilityRecord
		wantNotice   bool
	}{
		{
			name:         "snapshot has a gap the live record does not",
			liveCaps:     allProtocol,
			capsSnapshot: withTokenCountsGap,
			wantNotice:   true,
		},
		{
			name:         "live record has a gap the snapshot does not",
			liveCaps:     withTokenCountsGap,
			capsSnapshot: allProtocol,
			wantNotice:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := &sessionState{caps: &tt.liveCaps, logger: discardLogger()}
			p := &pumpState{state: state}
			turn := &activeTurn{
				sink:         make(chan domain.AgentEvent, 1),
				done:         make(chan struct{}),
				capsSnapshot: tt.capsSnapshot,
			}

			p.emitCapabilityGapNoticeOnce(turn)

			select {
			case ev := <-turn.sink:
				if !tt.wantNotice {
					t.Fatalf("emitCapabilityGapNoticeOnce() published %+v, want no event: the turn's own snapshot carried no gap entry", ev)
				}
				if ev.Type != domain.EventNotification {
					t.Fatalf("emitCapabilityGapNoticeOnce() published event type = %q, want %q", ev.Type, domain.EventNotification)
				}
			default:
				if tt.wantNotice {
					t.Fatal("emitCapabilityGapNoticeOnce() published no event, want the gap notice for the turn's own snapshot")
				}
			}
		})
	}
}

// TestAgentVersionLoweredByPresenceBoolean confirms an absent agentInfo
// lowers the version entry through the agentInfoPresent boolean alone.
// The zero-value case is the load-bearing one: if the lowering were
// driven by comparing the recorded implementation value instead, a
// present-but-zero-value agentInfo would be indistinguishable from an
// absent one and would wrongly lower the entry.
func TestAgentVersionLoweredByPresenceBoolean(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		agentInfoPresent bool
		agentInfo        implementation
		wantGap          bool
	}{
		{
			name:             "absent agentInfo lowers the version entry",
			agentInfoPresent: false,
			agentInfo:        implementation{},
			wantGap:          true,
		},
		{
			name:             "present agentInfo that happens to be the zero value lowers nothing",
			agentInfoPresent: true,
			agentInfo:        implementation{},
			wantGap:          false,
		},
		{
			name:             "present agentInfo with populated fields lowers nothing",
			agentInfoPresent: true,
			agentInfo:        implementation{Name: "some-agent", Version: "9.9.9"},
			wantGap:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, p := newPumpForCapabilityTests(t)
			p.applyHandshakeCapabilityLowering(&handshakeFacts{agentInfoPresent: tt.agentInfoPresent, agentInfo: tt.agentInfo})

			gotGap := state.caps.agentVersion == capabilityGap
			if gotGap != tt.wantGap {
				t.Errorf("applyHandshakeCapabilityLowering(agentInfoPresent=%v) agentVersion gap = %v, want %v", tt.agentInfoPresent, gotGap, tt.wantGap)
			}
		})
	}
}

// TestToolServersLoweredWhenWithheld confirms an HTTP tool server the
// generated configuration declared, but the handshake's advertised MCP
// capabilities do not support, lowers toolServers; a handshake that
// withholds nothing leaves it at its stage-one state.
func TestToolServersLoweredWhenWithheld(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withheld  bool
		wantState capabilityState
	}{
		{name: "an HTTP server withheld by the handshake lowers toolServers", withheld: true, wantState: capabilityGap},
		{name: "nothing withheld leaves toolServers at its stage-one state", withheld: false, wantState: capabilityProtocol},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, p := newPumpForCapabilityTests(t)
			p.applyHandshakeCapabilityLowering(&handshakeFacts{agentInfoPresent: true, toolServersWithheld: tt.withheld})

			if state.caps.toolServers != tt.wantState {
				t.Errorf("toolServers after applyHandshakeCapabilityLowering(toolServersWithheld=%v) = %q, want %q", tt.withheld, state.caps.toolServers, tt.wantState)
			}
		})
	}
}

// TestSessionContinuationLoweredWhenHandshakeAdvertisesNeither confirms
// the handshake-based half of sessionContinuation's lowering: a
// handshake advertising neither continuation method lowers the entry
// from its now-protocol stage-one default, while advertising either
// one leaves it there. This branch was inert before this piece flipped
// newCapabilityRecord's stage-one default to protocol, so no earlier
// test observed it doing anything.
func TestSessionContinuationLoweredWhenHandshakeAdvertisesNeither(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		caps      agentCapabilities
		wantState capabilityState
	}{
		{name: "neither loadSession nor resume advertised lowers the entry", caps: agentCapabilities{}, wantState: capabilityGap},
		{name: "loadSession advertised leaves the entry at its stage-one state", caps: capsAdvertisingLoad(), wantState: capabilityProtocol},
		{name: "sessionCapabilities.resume advertised leaves the entry at its stage-one state", caps: capsAdvertisingResume(), wantState: capabilityProtocol},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, p := newPumpForCapabilityTests(t)
			p.applyHandshakeCapabilityLowering(&handshakeFacts{agentInfoPresent: true, caps: tt.caps})

			if state.caps.sessionContinuation != tt.wantState {
				t.Errorf("sessionContinuation after applyHandshakeCapabilityLowering(caps=%+v) = %q, want %q", tt.caps, state.caps.sessionContinuation, tt.wantState)
			}
		})
	}
}

// TestAdvertisesSessionContinuation confirms the predicate over the
// four shapes that matter. The asymmetry between the two branches is
// schema-driven, not a testing inconsistency: LoadSession is a *bool,
// so only a present and true value advertises continuation, while
// Resume is a struct pointer with no boolean payload, so its mere
// presence is the advertisement.
func TestAdvertisesSessionContinuation(t *testing.T) {
	t.Parallel()

	trueVal := true
	falseVal := false

	tests := []struct {
		name string
		caps agentCapabilities
		want bool
	}{
		{
			name: "loadSession true advertises continuation",
			caps: agentCapabilities{LoadSession: &trueVal},
			want: true,
		},
		{
			name: "loadSession present and false does not advertise continuation",
			caps: agentCapabilities{LoadSession: &falseVal},
			want: false,
		},
		{
			name: "sessionCapabilities.resume present with loadSession absent advertises continuation",
			caps: agentCapabilities{SessionCapabilities: &sessionCapabilities{Resume: &sessionResumeCapabilities{}}},
			want: true,
		},
		{
			name: "neither loadSession nor sessionCapabilities.resume present does not advertise continuation",
			caps: agentCapabilities{},
			want: false,
		},
		{
			name: "sessionCapabilities present with close but no resume does not advertise continuation",
			caps: agentCapabilities{SessionCapabilities: &sessionCapabilities{Close: &sessionCloseCapabilities{}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := advertisesSessionContinuation(tt.caps)

			if got != tt.want {
				t.Errorf("advertisesSessionContinuation(%+v) = %v, want %v", tt.caps, got, tt.want)
			}
		})
	}
}

// TestAdvertisesSessionContinuationAgreesWithChooseContinuationMethod
// confirms the handshake's lowering predicate and resolveSession's own
// routing decision can never disagree about what caps advertises,
// across a matrix wider than either function's own dedicated test: an
// agent that advertised both, or that advertised one falsely, would
// otherwise risk one predicate reporting a capability the other never
// attempts, or an attempt the other reports as a gap.
func TestAdvertisesSessionContinuationAgreesWithChooseContinuationMethod(t *testing.T) {
	t.Parallel()

	trueVal := true
	falseVal := false
	resume := &sessionCapabilities{Resume: &sessionResumeCapabilities{}}

	tests := []struct {
		name string
		caps agentCapabilities
	}{
		{name: "neither advertised", caps: agentCapabilities{}},
		{name: "loadSession true only", caps: agentCapabilities{LoadSession: &trueVal}},
		{name: "loadSession false only", caps: agentCapabilities{LoadSession: &falseVal}},
		{name: "resume only", caps: agentCapabilities{SessionCapabilities: resume}},
		{name: "loadSession true and resume both advertised", caps: agentCapabilities{LoadSession: &trueVal, SessionCapabilities: resume}},
		{name: "loadSession false and resume advertised", caps: agentCapabilities{LoadSession: &falseVal, SessionCapabilities: resume}},
		{name: "sessionCapabilities present with close but no resume", caps: agentCapabilities{SessionCapabilities: &sessionCapabilities{Close: &sessionCloseCapabilities{}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			advertises := advertisesSessionContinuation(tt.caps)
			routes := chooseContinuationMethod(tt.caps) != continuationNone

			if advertises != routes {
				t.Errorf("advertisesSessionContinuation(%+v) = %v, chooseContinuationMethod(%+v) != continuationNone = %v, want agreement", tt.caps, advertises, tt.caps, routes)
			}
		})
	}
}

// TestCapabilityRecordGapNotice confirms the notice text lists only the
// entries in the gap state, in the record's own fixed field order,
// assembled from nothing but the compile-time constant fragments.
func TestCapabilityRecordGapNotice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record capabilityRecord
		want   string
		wantOK bool
	}{
		{
			name: "no gap entries reports nothing to notify",
			record: capabilityRecord{
				toolServers: capabilityProtocol, tokenCounts: capabilityProtocol,
				sessionContinuation: capabilityProtocol, agentVersion: capabilityProtocol,
			},
			wantOK: false,
		},
		{
			name: "one gap entry lists only that label",
			record: capabilityRecord{
				toolServers: capabilityProtocol, tokenCounts: capabilityGap,
				sessionContinuation: capabilityProtocol, agentVersion: capabilityProtocol,
			},
			want:   capabilityGapNoticeStem + capabilityLabelTokenCounts,
			wantOK: true,
		},
		{
			name: "two non-adjacent gap entries are joined without picking up a neighbor",
			record: capabilityRecord{
				toolServers: capabilityGap, tokenCounts: capabilityProtocol,
				sessionContinuation: capabilityProtocol, agentVersion: capabilityGap,
			},
			want: capabilityGapNoticeStem + strings.Join([]string{
				capabilityLabelToolServers, capabilityLabelAgentVersion,
			}, capabilityGapNoticeSeparator),
			wantOK: true,
		},
		{
			name: "every entry gap lists all four labels in the record's own order",
			record: capabilityRecord{
				toolServers: capabilityGap, tokenCounts: capabilityGap,
				sessionContinuation: capabilityGap, agentVersion: capabilityGap,
			},
			want: capabilityGapNoticeStem + strings.Join([]string{
				capabilityLabelToolServers, capabilityLabelTokenCounts,
				capabilityLabelSessionContinuation, capabilityLabelAgentVersion,
			}, capabilityGapNoticeSeparator),
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.record.gapNotice()
			if ok != tt.wantOK {
				t.Fatalf("gapNotice() ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("gapNotice() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCapabilityGapNoticeOncePerSession confirms the once-per-session
// notice is the first notification of the session's first turn, is
// never repeated by a later turn, and lists the entries the handshake
// left in the gap state in the record's fixed order.
func TestCapabilityGapNoticeOncePerSession(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	// A local launch's stage-one record already carries tokenCounts and
	// sessionContinuation at gap; this handshake lowers the two
	// remaining entries so the notice lists all four labels.
	state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{
		agentInfoPresent:    false,
		toolServersWithheld: true,
	}}}
	markSessionKnown(state)

	var firstTurnEvents []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&firstTurnEvents)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("first RunTurn() error = %v, want nil", outcome.err)
	}

	if len(firstTurnEvents) < 2 {
		t.Fatalf("first turn delivered %d events, want at least session_started and the gap notice", len(firstTurnEvents))
	}
	if firstTurnEvents[0].Type != domain.EventSessionStarted {
		t.Fatalf("first event type = %q, want %q", firstTurnEvents[0].Type, domain.EventSessionStarted)
	}
	notice := firstTurnEvents[1]
	if notice.Type != domain.EventNotification {
		t.Fatalf("second event type = %q, want %q (the once-per-session gap notice)", notice.Type, domain.EventNotification)
	}

	wantMessage := capabilityGapNoticeStem + strings.Join([]string{
		capabilityLabelToolServers, capabilityLabelTokenCounts,
		capabilityLabelSessionContinuation, capabilityLabelAgentVersion,
	}, capabilityGapNoticeSeparator)
	if notice.Message != wantMessage {
		t.Errorf("gap notice = %q, want %q", notice.Message, wantMessage)
	}

	for _, ev := range firstTurnEvents[2:] {
		if ev.Type == domain.EventNotification {
			t.Errorf("first turn emitted a second notification %q, want exactly one", ev.Message)
		}
	}

	// A second turn in the same session must not repeat the notice, even
	// though the record still carries the same gap entries.
	var secondTurnEvents []domain.AgentEvent
	outcomeCh2 := runTurnAsync(state, domain.RunTurnParams{Prompt: "go again", OnEvent: collectEvents(&secondTurnEvents)})
	promptID2 := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID2, promptResponse{StopReason: stopReasonEndTurn})
	outcome2 := awaitOutcome(t, outcomeCh2)
	if outcome2.err != nil {
		t.Fatalf("second RunTurn() error = %v, want nil", outcome2.err)
	}
	for _, ev := range secondTurnEvents {
		if ev.Type == domain.EventNotification {
			t.Errorf("second turn emitted a notification %q, want none: the gap notice fires exactly once per session", ev.Message)
		}
	}
}

// runOneTurnAndCaptureNotice starts a session carrying the given
// handshake facts and session identifier, runs one turn to completion,
// and returns the message of the notification event it observed.
func runOneTurnAndCaptureNotice(t *testing.T, sessionID string, facts *handshakeFacts) string {
	t.Helper()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)

	state.itemCh <- pumpItem{control: &pumpControl{handshake: facts}}
	state.itemCh <- pumpItem{control: &pumpControl{sessionID: sessionID}}

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)
	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	for _, ev := range events {
		if ev.Type == domain.EventNotification {
			return ev.Message
		}
	}
	t.Fatal("no notification event observed, want the gap notice")
	return ""
}

// TestCapabilityGapNoticeInterpolatesNoRuntimeValue confirms the notice
// is byte-identical across two sessions that carry different runtime
// details (session identifier, agent name and version) but land on the
// same set of gap entries: it is assembled only from compile-time
// constant fragments and never from a wire-derived value.
func TestCapabilityGapNoticeInterpolatesNoRuntimeValue(t *testing.T) {
	t.Parallel()

	first := runOneTurnAndCaptureNotice(t, "sess-aaaa", &handshakeFacts{
		agentInfo:        implementation{Name: "agent-one", Version: "1.2.3"},
		agentInfoPresent: true,
	})
	second := runOneTurnAndCaptureNotice(t, "sess-bbbb", &handshakeFacts{
		agentInfo:        implementation{Name: "a-completely-different-agent", Version: "9.9.9"},
		agentInfoPresent: true,
	})

	if first != second {
		t.Errorf("gap notice varied with runtime details: %q vs %q, want identical text for the same gap set", first, second)
	}
	if strings.Contains(first, "sess-aaaa") || strings.Contains(first, "agent-one") {
		t.Errorf("gap notice = %q, contains a runtime value, want only constant fragments", first)
	}
}
