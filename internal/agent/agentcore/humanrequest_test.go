package agentcore

import (
	"testing"
)

// TestDecideHumanRequest_Rows pins all six rows of the posture table
// against a representative (class, replyChannel, answer) combination.
func TestDecideHumanRequest_Rows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		class           HumanRequestClass
		replyChannel    bool
		answer          HumanRequestAnswer
		wantTransmit    bool
		wantContinuable bool
		wantEndAttempt  bool
	}{
		{
			name:         "permission, reply channel, pending: continue and transmit",
			class:        ClassPermission,
			replyChannel: true,
			answer:       AnswerPending,
			wantTransmit: true, wantContinuable: true, wantEndAttempt: false,
		},
		{
			name:         "permission, no reply channel, pending: end the attempt",
			class:        ClassPermission,
			replyChannel: false,
			answer:       AnswerPending,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: true,
		},
		{
			name:         "permission, reply channel, runtime refused: continue, nothing to transmit",
			class:        ClassPermission,
			replyChannel: true,
			answer:       AnswerRuntimeRefused,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: false,
		},
		{
			name:         "permission, no reply channel, runtime refused: continue, nothing to transmit",
			class:        ClassPermission,
			replyChannel: false,
			answer:       AnswerRuntimeRefused,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: false,
		},
		{
			name:         "human input, reply channel, pending: end, not continuable",
			class:        ClassHumanInput,
			replyChannel: true,
			answer:       AnswerPending,
			wantTransmit: true, wantContinuable: false, wantEndAttempt: true,
		},
		{
			name:         "human input, no reply channel, pending: end, nothing to transmit",
			class:        ClassHumanInput,
			replyChannel: false,
			answer:       AnswerPending,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: true,
		},
		{
			name:         "human input, reply channel, runtime refused: end",
			class:        ClassHumanInput,
			replyChannel: true,
			answer:       AnswerRuntimeRefused,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: true,
		},
		{
			name:         "human input, no reply channel, runtime refused: end",
			class:        ClassHumanInput,
			replyChannel: false,
			answer:       AnswerRuntimeRefused,
			wantTransmit: false, wantContinuable: false, wantEndAttempt: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DecideHumanRequest(tt.class, tt.replyChannel, tt.answer)

			if got.Transmit != tt.wantTransmit {
				t.Errorf("DecideHumanRequest(%v, %v, %v).Transmit = %v, want %v",
					tt.class, tt.replyChannel, tt.answer, got.Transmit, tt.wantTransmit)
			}
			if got.Continuable != tt.wantContinuable {
				t.Errorf("DecideHumanRequest(%v, %v, %v).Continuable = %v, want %v",
					tt.class, tt.replyChannel, tt.answer, got.Continuable, tt.wantContinuable)
			}
			if got.EndAttempt != tt.wantEndAttempt {
				t.Errorf("DecideHumanRequest(%v, %v, %v).EndAttempt = %v, want %v",
					tt.class, tt.replyChannel, tt.answer, got.EndAttempt, tt.wantEndAttempt)
			}
			if got.Notice == "" {
				t.Errorf("DecideHumanRequest(%v, %v, %v).Notice = \"\", want non-empty",
					tt.class, tt.replyChannel, tt.answer)
			}
		})
	}
}

// TestDecideHumanRequest_PanicsOnInvalidClass pins that an out-of-range
// class, including its zero value, panics rather than silently returning
// a decision an uninitialized call site never asked for.
func TestDecideHumanRequest_PanicsOnInvalidClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		class HumanRequestClass
	}{
		{name: "zero value", class: HumanRequestClass(0)},
		{name: "out of range", class: HumanRequestClass(99)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Errorf("DecideHumanRequest(class=%d, ...) did not panic, want panic", tt.class)
				}
			}()
			DecideHumanRequest(tt.class, true, AnswerPending)
		})
	}
}

// TestDecideHumanRequest_PanicsOnInvalidAnswer pins that an out-of-range
// answer, including its zero value, panics rather than silently returning
// a decision an uninitialized call site never asked for.
func TestDecideHumanRequest_PanicsOnInvalidAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		answer HumanRequestAnswer
	}{
		{name: "zero value", answer: HumanRequestAnswer(0)},
		{name: "out of range", answer: HumanRequestAnswer(99)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Errorf("DecideHumanRequest(..., answer=%d) did not panic, want panic", tt.answer)
				}
			}()
			DecideHumanRequest(ClassPermission, true, tt.answer)
		})
	}
}

// TestHumanInputEvidence pins that HumanInputEvidence sets Terminal to
// TerminalHumanInputRequired and carries detail on TerminalMessage
// unchanged.
func TestHumanInputEvidence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		detail string
	}{
		{name: "empty detail", detail: ""},
		{name: "non-empty detail", detail: "an answer to a question"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := HumanInputEvidence(tt.detail)

			if got.Terminal != TerminalHumanInputRequired {
				t.Errorf("HumanInputEvidence(%q).Terminal = %v, want %v", tt.detail, got.Terminal, TerminalHumanInputRequired)
			}
			if got.TerminalMessage != tt.detail {
				t.Errorf("HumanInputEvidence(%q).TerminalMessage = %q, want %q", tt.detail, got.TerminalMessage, tt.detail)
			}
		})
	}
}

// TestRefusalPosture_NoticeWithDetail pins that NoticeWithDetail returns
// the bare notice when detail is empty, and notice+": "+detail otherwise.
func TestRefusalPosture_NoticeWithDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		notice string
		detail string
		want   string
	}{
		{name: "empty detail returns the bare notice", notice: "refused", detail: "", want: "refused"},
		{name: "non-empty detail is joined with a colon-space separator", notice: "refused", detail: "a tool call", want: "refused: a tool call"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			posture := RefusalPosture{Notice: tt.notice}
			got := posture.NoticeWithDetail(tt.detail)

			if got != tt.want {
				t.Errorf("RefusalPosture{Notice: %q}.NoticeWithDetail(%q) = %q, want %q", tt.notice, tt.detail, got, tt.want)
			}
		})
	}
}
