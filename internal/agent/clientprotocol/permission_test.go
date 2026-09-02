package clientprotocol

import (
	"fmt"
	"reflect"
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
