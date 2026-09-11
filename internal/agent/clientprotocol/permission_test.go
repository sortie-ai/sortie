package clientprotocol

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestSelectRefusingOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		options   []permissionOption
		wantID    string
		wantFound bool
	}{
		{
			name: "an option list offering reject_once and reject_always selects the once-only kind",
			options: []permissionOption{
				{Kind: permissionOptionKindAllowOnce, OptionID: "allow-1"},
				{Kind: permissionOptionKindRejectAlways, OptionID: "reject-always-1"},
				{Kind: permissionOptionKindRejectOnce, OptionID: "reject-once-1"},
			},
			wantID: "reject-once-1", wantFound: true,
		},
		{
			name: "two options of the same selected kind selects the earliest in the agent's order",
			options: []permissionOption{
				{Kind: permissionOptionKindRejectOnce, OptionID: "first"},
				{Kind: permissionOptionKindRejectOnce, OptionID: "second"},
			},
			wantID: "first", wantFound: true,
		},
		{
			name: "an option list with no refusing kind reports not found",
			options: []permissionOption{
				{Kind: permissionOptionKindAllowOnce, OptionID: "a"},
				{Kind: permissionOptionKindAllowAlways, OptionID: "b"},
			},
			wantFound: false,
		},
		{
			name:      "an empty option list reports not found",
			options:   nil,
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotID, gotFound := selectRefusingOption(tt.options)
			if gotFound != tt.wantFound {
				t.Fatalf("selectRefusingOption(%+v) found = %v, want %v", tt.options, gotFound, tt.wantFound)
			}
			if gotFound && gotID != tt.wantID {
				t.Errorf("selectRefusingOption(%+v) = %q, want %q", tt.options, gotID, tt.wantID)
			}
		})
	}
}

// TestSelectRefusingOptionIgnoresIdentifier confirms the same kinds in
// the same order with different identifiers select the same position,
// proving selection reads Kind and order only.
func TestSelectRefusingOptionIgnoresIdentifier(t *testing.T) {
	t.Parallel()

	kinds := []permissionOptionKind{
		permissionOptionKindAllowOnce,
		permissionOptionKindRejectOnce,
		permissionOptionKindRejectAlways,
	}
	build := func(ids []string) []permissionOption {
		opts := make([]permissionOption, len(kinds))
		for i, k := range kinds {
			opts[i] = permissionOption{Kind: k, OptionID: permissionOptionId(ids[i])}
		}
		return opts
	}

	first := build([]string{"id-a", "id-b", "id-c"})
	second := build([]string{"different-1", "different-2", "different-3"})

	gotFirst, foundFirst := selectRefusingOption(first)
	gotSecond, foundSecond := selectRefusingOption(second)

	if !foundFirst || !foundSecond {
		t.Fatalf("selectRefusingOption found = (%v, %v), want (true, true)", foundFirst, foundSecond)
	}
	if gotFirst != "id-b" || gotSecond != "different-2" {
		t.Errorf("selectRefusingOption selected (%q, %q), want the option at index 1 in both regardless of identifier", gotFirst, gotSecond)
	}
}

// TestPermissionRequestNoRefusingOptionEndsAttempt confirms an option
// list with no refusing kind, and an empty option list, both answer
// cancelled and both end the attempt with the human-input-required
// outcome, and neither ever answers with an allowing selection.
func TestPermissionRequestNoRefusingOptionEndsAttempt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options string
	}{
		{
			name:    "no refusing kind offered: cancelled, never an allowing selection",
			options: `[{"kind":"allow_once","name":"allow","optionId":"allow-id"},{"kind":"allow_always","name":"allow-always","optionId":"allow-always-id"}]`,
		},
		{
			name:    "empty option list: cancelled",
			options: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			var events []domain.AgentEvent
			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})

			promptID := out.awaitMethod(t, methodSessionPrompt)

			sendLine(t, inPw, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":9,"method":"session/request_permission","params":{"sessionId":"sess-test","options":%s,"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`,
				tt.options))

			respLine := out.next(t)
			assertRawID(t, respLine, "9")
			resp := decodeResponse(t, respLine)
			if resp.Result.Outcome.Outcome != outcomeCancelled {
				t.Errorf("permission reply outcome = %q, want %q", resp.Result.Outcome.Outcome, outcomeCancelled)
			}
			if resp.Result.Outcome.OptionID != "" {
				t.Errorf("permission reply optionId = %q, want empty: an allowing option must never be selected", resp.Result.Outcome.OptionID)
			}

			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

			outcome := awaitOutcome(t, outcomeCh)
			dispositiontest.AssertDispositionContract(t, agentcore.HumanInputEvidence(""), outcome.result, outcome.err)
		})
	}
}

// TestPermissionRequestSelectsRefusingOption confirms a permission
// request offering a refusing option is answered with that selection,
// on one reply line, and the turn continues rather than ending.
func TestPermissionRequestSelectsRefusingOption(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})

	promptID := out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"sess-test","options":[{"kind":"reject_once","name":"reject","optionId":"reject-id"}],"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`)

	respLine := out.next(t)
	assertRawID(t, respLine, `"perm-1"`)
	resp := decodeResponse(t, respLine)
	if resp.Result.Outcome.Outcome != outcomeSelected || resp.Result.Outcome.OptionID != "reject-id" {
		t.Errorf("permission reply = %+v, want outcome %q with optionId %q", resp.Result.Outcome, outcomeSelected, "reject-id")
	}

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})

	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil: the attempt must not end on a selected refusal", outcome.err)
	}
	if outcome.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn() ExitReason = %q, want %q", outcome.result.ExitReason, domain.EventTurnCompleted)
	}

	posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
	var wantNotice domain.AgentEvent
	agentcore.EmitNotification(func(event domain.AgentEvent) { wantNotice = event }, posture.NoticeWithDetail(""))

	var gotNotice domain.AgentEvent
	for _, event := range events {
		if event.Type == domain.EventNotification && event.Message == wantNotice.Message {
			gotNotice = event
			break
		}
	}
	wantNotice.Timestamp = gotNotice.Timestamp
	if !reflect.DeepEqual(gotNotice, wantNotice) {
		t.Errorf("permission refusal notification = %+v, want shared emitter shape %+v", gotNotice, wantNotice)
	}
}

// permissionRequestLine builds a session/request_permission wire line
// naming id and options, for a session already marked known under
// "sess-test".
func permissionRequestLine(id, options string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"method":"session/request_permission","params":{"sessionId":"sess-test","options":%s,"toolCall":{"toolCallId":"tc-1","title":"do a thing"}}}`,
		id, options)
}

// countToolDeliveryNotifications reports how many events in events are
// an EventNotification carrying toolDeliveryUncallableNotice.
func countToolDeliveryNotifications(events []domain.AgentEvent) int {
	var n int
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == toolDeliveryUncallableNotice {
			n++
		}
	}
	return n
}

// TestToolDeliveryReportedOnceThenLatched confirms a session that
// delivered tool servers reports the uncallable-tool notice on the
// first permission request a refusal posture answers, and that a
// second such request in the same turn does not repeat it. The
// session's own capability record is untouched by this reporting
// path: toolServers is reported strictly through the once-per-session
// capability gap notice, a distinct mechanism this handshake leaves at
// its stage-one state.
func TestToolDeliveryReportedOnceThenLatched(t *testing.T) {
	t.Parallel()

	state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
	out := newOutboundReader(outPr)
	state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
	markSessionKnown(state)

	var events []domain.AgentEvent
	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, permissionRequestLine(`"perm-1"`, `[{"kind":"reject_once","name":"reject","optionId":"reject-id-1"}]`))
	out.next(t)
	sendLine(t, inPw, permissionRequestLine(`"perm-2"`, `[{"kind":"reject_once","name":"reject","optionId":"reject-id-2"}]`))
	out.next(t)

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	if got := countToolDeliveryNotifications(events); got != 1 {
		t.Errorf("tool delivery uncallable notifications across two permission requests = %d, want 1", got)
	}
	if state.caps.toolServers != capabilityProtocol {
		t.Errorf("caps.toolServers after the report = %q, want %q: this handshake never withholds tool servers", state.caps.toolServers, capabilityProtocol)
	}
}

// TestToolDeliveryReportLogsWarnRecord confirms the uncallable-tool
// report's Warn record carries exactly the message and the one
// reason=permission_refused attribute, with no other attribute
// attached.
func TestToolDeliveryReportLogsWarnRecord(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
	out := newOutboundReader(outPr)
	state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
	markSessionKnown(state)

	outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: func(domain.AgentEvent) {}})
	promptID := out.awaitMethod(t, methodSessionPrompt)

	sendLine(t, inPw, permissionRequestLine(`"perm-1"`, `[{"kind":"reject_once","name":"reject","optionId":"reject-id"}]`))
	out.next(t)

	respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
	outcome := awaitOutcome(t, outcomeCh)
	if outcome.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
	}

	var warnLines []string
	for line := range strings.SplitSeq(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.Contains(line, "level=WARN") {
			warnLines = append(warnLines, line)
		}
	}
	if len(warnLines) != 1 {
		t.Fatalf("Warn records logged = %d, want 1: %v", len(warnLines), warnLines)
	}
	warnLine := warnLines[0]
	if !strings.Contains(warnLine, `msg="`+toolDeliveryUncallableLog+`"`) {
		t.Errorf("Warn record = %q, want msg %q", warnLine, toolDeliveryUncallableLog)
	}
	if !strings.Contains(warnLine, "reason=permission_refused") {
		t.Errorf("Warn record = %q, want attribute reason=permission_refused", warnLine)
	}
	if got := strings.Count(warnLine, "="); got != 4 {
		t.Errorf("Warn record %q carries %d key=value fields, want 4 (time, level, msg, reason): exactly one attribute besides the standard record fields", warnLine, got)
	}
}

// TestToolDeliveryReportCoversBothPostureArms confirms the
// uncallable-tool report fires exactly once whether the permission
// request offers a refusing option or not, covering both the option
// list TestPermissionRequestSelectsRefusingOption exercises and the
// two option-list shapes TestPermissionRequestNoRefusingOptionEndsAttempt
// exercises for the arm with no refusing option.
func TestToolDeliveryReportCoversBothPostureArms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options string
	}{
		{"a refusing kind offered", `[{"kind":"reject_once","name":"reject","optionId":"reject-id"}]`},
		{"no refusing kind offered: only allowing kinds", `[{"kind":"allow_once","name":"allow","optionId":"allow-id"},{"kind":"allow_always","name":"allow-always","optionId":"allow-always-id"}]`},
		{"no refusing kind offered: empty option list", `[]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
			out := newOutboundReader(outPr)
			state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
			markSessionKnown(state)

			var events []domain.AgentEvent
			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})
			promptID := out.awaitMethod(t, methodSessionPrompt)

			sendLine(t, inPw, permissionRequestLine("9", tt.options))
			out.next(t)

			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
			awaitOutcome(t, outcomeCh)

			if got := countToolDeliveryNotifications(events); got != 1 {
				t.Errorf("tool delivery uncallable notifications = %d, want 1", got)
			}
			if got := strings.Count(buf.String(), toolDeliveryUncallableLog); got != 1 {
				t.Errorf("tool delivery uncallable Warn records = %d, want 1: %s", got, buf.String())
			}
		})
	}
}

// TestNoToolDeliveryReportWhenNoToolServersDelivered confirms a
// session that never delivered a tool server produces neither the
// notice nor the Warn record, for either posture arm, because
// reportUncallableToolDelivery returns immediately when
// toolServersDelivered is false.
func TestNoToolDeliveryReportWhenNoToolServersDelivered(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options string
	}{
		{"a refusing kind offered", `[{"kind":"reject_once","name":"reject","optionId":"reject-id"}]`},
		{"no refusing kind offered", `[]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			state, outPr, inPw := newTestSessionWithLogger(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes, logger)
			out := newOutboundReader(outPr)
			markSessionKnown(state)

			var events []domain.AgentEvent
			outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})
			promptID := out.awaitMethod(t, methodSessionPrompt)

			sendLine(t, inPw, permissionRequestLine("9", tt.options))
			out.next(t)

			respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
			awaitOutcome(t, outcomeCh)

			if got := countToolDeliveryNotifications(events); got != 0 {
				t.Errorf("tool delivery uncallable notifications with no tool servers delivered = %d, want 0", got)
			}
			if strings.Contains(buf.String(), toolDeliveryUncallableLog) {
				t.Errorf("unexpected tool delivery uncallable Warn record with no tool servers delivered: %s", buf.String())
			}
		})
	}
}

// assertAdjacentNotifications fails t unless events contains an
// EventNotification carrying ownNotice immediately followed by an
// EventNotification carrying deliveryNotice, with nothing between
// them.
func assertAdjacentNotifications(t *testing.T, events []domain.AgentEvent, ownNotice, deliveryNotice string) {
	t.Helper()
	for i, e := range events {
		if e.Type != domain.EventNotification || e.Message != ownNotice {
			continue
		}
		if i+1 >= len(events) {
			t.Fatalf("the posture's own notice %q is the last event in %+v, want the uncallable-tool notice right after it", ownNotice, events)
		}
		next := events[i+1]
		if next.Type != domain.EventNotification || next.Message != deliveryNotice {
			t.Fatalf("event immediately after the posture's own notice = %+v, want an %q notification carrying %q", next, domain.EventNotification, deliveryNotice)
		}
		return
	}
	t.Fatalf("no event in %+v carries the posture's own notice %q", events, ownNotice)
}

// TestToolDeliveryNoticeOrderAndStemExclusion confirms the
// uncallable-tool notice is published immediately after the posture's
// own notice, with nothing between them, whether the pair is
// published into an active turn or queued out-of-turn and flushed
// into the next turn's sink.
func TestToolDeliveryNoticeOrderAndStemExclusion(t *testing.T) {
	t.Parallel()

	ownNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending).NoticeWithDetail("")

	t.Run("published into an active turn", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{ReadTimeoutMS: 2000}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}
		markSessionKnown(state)

		var events []domain.AgentEvent
		outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "do something", OnEvent: collectEvents(&events)})
		promptID := out.awaitMethod(t, methodSessionPrompt)

		sendLine(t, inPw, permissionRequestLine(`"perm-1"`, `[{"kind":"reject_once","name":"reject","optionId":"reject-id"}]`))
		out.next(t)

		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
		outcome := awaitOutcome(t, outcomeCh)
		if outcome.err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
		}

		assertAdjacentNotifications(t, events, ownNotice, toolDeliveryUncallableNotice)
	})

	// This mirrors TestSessionUpdateBeforeAnyPromptNotLost's
	// queue-then-flush pattern: the permission request is answered,
	// and both notices are queued, before any turn exists, and both
	// must survive the flush in the same relative order.
	t.Run("queued out-of-turn then flushed into the next turn", func(t *testing.T) {
		t.Parallel()

		state, outPr, inPw := newTestSession(t, domain.AgentConfig{}, clientProtocolMaxLineBytes)
		out := newOutboundReader(outPr)
		markSessionKnown(state)
		state.itemCh <- pumpItem{control: &pumpControl{handshake: &handshakeFacts{toolServersDelivered: true}}}

		sendLine(t, inPw, permissionRequestLine(`"perm-1"`, `[{"kind":"reject_once","name":"reject","optionId":"reject-id"}]`))
		out.next(t)

		var events []domain.AgentEvent
		outcomeCh := runTurnAsync(state, domain.RunTurnParams{Prompt: "go", OnEvent: collectEvents(&events)})
		promptID := out.awaitMethod(t, methodSessionPrompt)
		respondLine(t, inPw, promptID, promptResponse{StopReason: stopReasonEndTurn})
		outcome := awaitOutcome(t, outcomeCh)
		if outcome.err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", outcome.err)
		}

		if len(events) == 0 || events[0].Type != domain.EventSessionStarted {
			t.Fatalf("first event = %+v, want %q", events, domain.EventSessionStarted)
		}
		assertAdjacentNotifications(t, events, ownNotice, toolDeliveryUncallableNotice)
	})
}
