package clientprotocol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// loadSessionUpdateFixture reads testdata/session_updates/<name>, the
// raw bytes of one session/update notification's "update" payload,
// shaped to decode into the pinned schema's generated wire types
// rather than written as ad hoc JSON.
func loadSessionUpdateFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	data, err := os.ReadFile("testdata/session_updates/" + name)
	if err != nil {
		t.Fatalf("loadSessionUpdateFixture(%q): %v", name, err)
	}
	return json.RawMessage(data)
}

func TestApplySessionUpdate(t *testing.T) {
	t.Parallel()

	t.Run("agent_message_chunk emits a notification", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "agent_message_chunk.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if !got.hasEvent || got.event.Type != domain.EventNotification {
			t.Fatalf("applySessionUpdate() = %+v, want a notification event", got)
		}
		if !got.workPresent {
			t.Error("applySessionUpdate() workPresent = false, want true for an agent message chunk")
		}
		if got.event.Message != "Reading the file to understand its structure." {
			t.Errorf("applySessionUpdate() Message = %q, want the chunk's text", got.event.Message)
		}
	})

	t.Run("agent_thought_chunk emits the reasoning-block constant", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "agent_thought_chunk.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if !got.hasEvent || got.event.Type != domain.EventOtherMessage {
			t.Fatalf("applySessionUpdate() = %+v, want an other_message event", got)
		}
		if got.event.Message != reasoningBlockMessage {
			t.Errorf("applySessionUpdate() Message = %q, want %q", got.event.Message, reasoningBlockMessage)
		}
	})

	t.Run("plan emits the plan-update constant", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "plan.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if !got.hasEvent || got.event.Message != planUpdateMessage {
			t.Fatalf("applySessionUpdate() = %+v, want message %q", got, planUpdateMessage)
		}
	})

	t.Run("tool_call then tool_call_update completed emits a tool_result", func(t *testing.T) {
		t.Parallel()
		tracker := agentcore.NewToolTracker()

		begin, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "tool_call.json"))
		if !ok {
			t.Fatal("parseSessionUpdate(tool_call) found = false, want true")
		}
		if got := applySessionUpdate(tracker, begin); !got.workPresent || got.hasEvent {
			t.Fatalf("applySessionUpdate(tool_call) = %+v, want workPresent=true, hasEvent=false", got)
		}

		end, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "tool_call_update_completed.json"))
		if !ok {
			t.Fatal("parseSessionUpdate(tool_call_update) found = false, want true")
		}
		got := applySessionUpdate(tracker, end)
		if !got.hasEvent || got.event.Type != domain.EventToolResult {
			t.Fatalf("applySessionUpdate(tool_call_update completed) = %+v, want a tool_result event", got)
		}
		if got.event.ToolError {
			t.Error("applySessionUpdate(tool_call_update completed) ToolError = true, want false")
		}
		if got.event.ToolName != "read" {
			t.Errorf("applySessionUpdate(tool_call_update completed) ToolName = %q, want %q", got.event.ToolName, "read")
		}
	})

	t.Run("tool_call_update failed reports ToolError", func(t *testing.T) {
		t.Parallel()
		tracker := agentcore.NewToolTracker()
		tracker.Begin("call_002", "execute")

		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "tool_call_update_failed.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true")
		}
		got := applySessionUpdate(tracker, ev)
		if !got.hasEvent || !got.event.ToolError {
			t.Fatalf("applySessionUpdate(tool_call_update failed) = %+v, want ToolError=true", got)
		}
	})

	t.Run("an unknown session/update variant is recognized as such and left consumed", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "unknown_variant.json"))
		if ok {
			t.Fatalf("parseSessionUpdate() found = true, want false for an unrecognized variant, got %+v", ev)
		}
		if ev.rawVariant != "future_streaming_diff" {
			t.Errorf("parseSessionUpdate() rawVariant = %q, want the discriminator value it could not recognize", ev.rawVariant)
		}
	})

	t.Run("an unknown key on a known variant does not stop normalization", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "unknown_key_agent_message_chunk.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true: an unmodeled key must not fail decoding")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if !got.hasEvent || got.event.Message != "Patching the retry loop." {
			t.Fatalf("applySessionUpdate() = %+v, want the chunk's own text despite the extra key", got)
		}
	})

	t.Run("an unknown field value leaves the message consumed with no event", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "tool_call_update_unknown_status.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true: the variant itself is recognized")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if got.hasEvent {
			t.Errorf("applySessionUpdate() = %+v, want no event for an unrecognized status value", got)
		}
	})

	t.Run("an unrecognized tool kind normalizes to other", func(t *testing.T) {
		t.Parallel()
		tracker := agentcore.NewToolTracker()

		begin, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "tool_call_unknown_kind.json"))
		if !ok {
			t.Fatal("parseSessionUpdate(tool_call) found = false, want true")
		}
		applySessionUpdate(tracker, begin)

		name, _, ok := tracker.End("call_004")
		if !ok {
			t.Fatal("tracker.End(call_004) ok = false, want true")
		}
		if name != string(toolKindOther) {
			t.Errorf("tracker.End(call_004) name = %q, want %q for an unrecognized kind", name, toolKindOther)
		}
	})

	t.Run("usage_update emits no event, per the normalization table", func(t *testing.T) {
		t.Parallel()
		ev, ok := parseSessionUpdate(loadSessionUpdateFixture(t, "usage_update.json"))
		if !ok {
			t.Fatal("parseSessionUpdate() found = false, want true")
		}
		got := applySessionUpdate(agentcore.NewToolTracker(), ev)
		if got.hasEvent {
			t.Errorf("applySessionUpdate(usage_update) = %+v, want no event: this kind reports no token counts", got)
		}
	})
}

func TestStopReasonEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		reason         stopReason
		wantTerminal   agentcore.TerminalReport
		wantErrorKind  domain.AgentErrorKind
		wantMessageSet bool
	}{
		{
			name:          "end_turn maps to terminal success",
			reason:        stopReasonEndTurn,
			wantTerminal:  agentcore.TerminalSuccess,
			wantErrorKind: "",
		},
		{
			name:          "refusal maps to the non-retryable refused kind",
			reason:        stopReasonRefusal,
			wantTerminal:  agentcore.TerminalFailure,
			wantErrorKind: domain.ErrTurnRefused,
		},
		{
			name:          "max_tokens maps to the token limit kind",
			reason:        stopReasonMaxTokens,
			wantTerminal:  agentcore.TerminalFailure,
			wantErrorKind: domain.ErrTurnTokenLimit,
		},
		{
			name:          "max_turn_requests maps to the request limit kind",
			reason:        stopReasonMaxTurnRequests,
			wantTerminal:  agentcore.TerminalFailure,
			wantErrorKind: domain.ErrTurnRequestLimit,
		},
		{
			name:           "cancelled with no cancellation on either side reports failed",
			reason:         stopReasonCancelled,
			wantTerminal:   agentcore.TerminalFailure,
			wantErrorKind:  domain.ErrTurnFailed,
			wantMessageSet: true,
		},
		{
			name:           "a stop reason outside the pinned set settles as a non-retried error carrying the value",
			reason:         stopReason("some_future_reason"),
			wantTerminal:   agentcore.TerminalFailure,
			wantErrorKind:  domain.ErrTurnOutcomeUnknown,
			wantMessageSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stopReasonEvidence(tt.reason, agentcore.WorkPresent)

			if got.Terminal != tt.wantTerminal {
				t.Errorf("stopReasonEvidence(%q).Terminal = %v, want %v", tt.reason, got.Terminal, tt.wantTerminal)
			}
			if got.TerminalErrorKind != tt.wantErrorKind {
				t.Errorf("stopReasonEvidence(%q).TerminalErrorKind = %q, want %q", tt.reason, got.TerminalErrorKind, tt.wantErrorKind)
			}
			if tt.wantMessageSet && got.TerminalMessage == "" {
				t.Errorf("stopReasonEvidence(%q).TerminalMessage is empty, want the received value carried in it", tt.reason)
			}
			if got.Work != agentcore.WorkPresent {
				t.Errorf("stopReasonEvidence(%q).Work = %v, want %v", tt.reason, got.Work, agentcore.WorkPresent)
			}
		})
	}

	t.Run("the unrecognized value itself is carried in the message", func(t *testing.T) {
		t.Parallel()
		got := stopReasonEvidence(stopReason("some_future_reason"), agentcore.WorkAbsent)
		if !strings.Contains(got.TerminalMessage, "some_future_reason") {
			t.Errorf("stopReasonEvidence(...).TerminalMessage = %q, want it to name the value received", got.TerminalMessage)
		}
	})

	t.Run("a token limit terminal carries its kind regardless of work presence", func(t *testing.T) {
		t.Parallel()

		got := stopReasonEvidence(stopReasonMaxTokens, agentcore.WorkAbsent)
		if got.TerminalErrorKind != domain.ErrTurnTokenLimit {
			t.Errorf("stopReasonEvidence(max_tokens, WorkAbsent).TerminalErrorKind = %q, want %q", got.TerminalErrorKind, domain.ErrTurnTokenLimit)
		}
		if got.Terminal != agentcore.TerminalFailure {
			t.Errorf("stopReasonEvidence(max_tokens, WorkAbsent).Terminal = %v, want %v", got.Terminal, agentcore.TerminalFailure)
		}
	})

	t.Run("an unrecognized value is truncated by runes", func(t *testing.T) {
		t.Parallel()

		reason := stopReason(strings.Repeat("界", messageTruncateLimit))

		got := stopReasonEvidence(reason, agentcore.WorkAbsent)

		if gotRunes := len([]rune(got.TerminalMessage)); gotRunes != messageTruncateLimit+1 {
			t.Errorf("len([]rune(stopReasonEvidence(%q).TerminalMessage)) = %d, want %d", reason, gotRunes, messageTruncateLimit+1)
		}
		if !strings.HasSuffix(got.TerminalMessage, "…") {
			t.Errorf("stopReasonEvidence(%q).TerminalMessage = %q, want a truncated message ending in %q", reason, got.TerminalMessage, "…")
		}
	})
}
