package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Compile-time interface satisfaction check.
var _ AgentAdapter = (*mockAgentAdapter)(nil)

type mockAgentAdapter struct{}

func (m *mockAgentAdapter) StartSession(_ context.Context, _ StartSessionParams) (Session, error) {
	return Session{}, nil
}

func (m *mockAgentAdapter) RunTurn(_ context.Context, _ Session, _ RunTurnParams) (TurnResult, error) {
	return TurnResult{}, nil
}

func (m *mockAgentAdapter) StopSession(_ context.Context, _ Session) error {
	return nil
}

func (m *mockAgentAdapter) EventStream() <-chan AgentEvent {
	return nil
}

// Section 10.3: verify all 13 normalized event type string values match the
// architecture specification exactly.
func TestAgentEventType_Values(t *testing.T) {
	t.Parallel()

	tests := []struct {
		constant AgentEventType
		want     string
	}{
		{EventSessionStarted, "session_started"},
		{EventStartupFailed, "startup_failed"},
		{EventTurnCompleted, "turn_completed"},
		{EventTurnFailed, "turn_failed"},
		{EventTurnCancelled, "turn_cancelled"},
		{EventTurnEndedWithError, "turn_ended_with_error"},
		{EventTurnInputRequired, "turn_input_required"},
		{EventApprovalAutoApproved, "approval_auto_approved"},
		{EventUnsupportedToolCall, "unsupported_tool_call"},
		{EventTokenUsage, "token_usage"},
		{EventNotification, "notification"},
		{EventOtherMessage, "other_message"},
		{EventMalformed, "malformed"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if string(tt.constant) != tt.want {
				t.Errorf("AgentEventType constant = %q, want %q", tt.constant, tt.want)
			}
		})
	}
	if len(tests) != 13 {
		t.Errorf("expected 13 event types, got %d", len(tests))
	}
}

// Section 10.5: verify all 9 agent error kind string values match the
// architecture specification exactly.
func TestAgentErrorKind_Values(t *testing.T) {
	t.Parallel()

	tests := []struct {
		constant AgentErrorKind
		want     string
	}{
		{ErrAgentNotFound, "agent_not_found"},
		{ErrInvalidWorkspaceCwd, "invalid_workspace_cwd"},
		{ErrResponseTimeout, "response_timeout"},
		{ErrTurnTimeout, "turn_timeout"},
		{ErrPortExit, "port_exit"},
		{ErrResponseError, "response_error"},
		{ErrTurnFailed, "turn_failed"},
		{ErrTurnCancelled, "turn_cancelled"},
		{ErrTurnInputRequired, "turn_input_required"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if string(tt.constant) != tt.want {
				t.Errorf("AgentErrorKind constant = %q, want %q", tt.constant, tt.want)
			}
		})
	}
	if len(tests) != 9 {
		t.Errorf("expected 9 error kinds, got %d", len(tests))
	}
}

func TestAgentError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  AgentError
		want string
	}{
		{
			name: "without wrapped error",
			err:  AgentError{Kind: ErrTurnFailed, Message: "exit code 1"},
			want: "agent: turn_failed: exit code 1",
		},
		{
			name: "with wrapped error",
			err:  AgentError{Kind: ErrPortExit, Message: "agent subprocess died", Err: fmt.Errorf("status 137")},
			want: "agent: port_exit: agent subprocess died: status 137",
		},
		{
			name: "agent not found",
			err:  AgentError{Kind: ErrAgentNotFound, Message: "command not in PATH"},
			want: "agent: agent_not_found: command not in PATH",
		},
		{
			name: "invalid workspace",
			err:  AgentError{Kind: ErrInvalidWorkspaceCwd, Message: "path does not exist"},
			want: "agent: invalid_workspace_cwd: path does not exist",
		},
		{
			name: "response timeout",
			err:  AgentError{Kind: ErrResponseTimeout, Message: "deadline exceeded"},
			want: "agent: response_timeout: deadline exceeded",
		},
		{
			name: "turn timeout",
			err:  AgentError{Kind: ErrTurnTimeout, Message: "turn exceeded limit"},
			want: "agent: turn_timeout: turn exceeded limit",
		},
		{
			name: "response error",
			err:  AgentError{Kind: ErrResponseError, Message: "bad protocol"},
			want: "agent: response_error: bad protocol",
		},
		{
			name: "turn cancelled",
			err:  AgentError{Kind: ErrTurnCancelled, Message: "context cancelled"},
			want: "agent: turn_cancelled: context cancelled",
		},
		{
			name: "turn input required",
			err:  AgentError{Kind: ErrTurnInputRequired, Message: "agent needs input"},
			want: "agent: turn_input_required: agent needs input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentError_Unwrap(t *testing.T) {
	t.Parallel()

	inner := fmt.Errorf("underlying error")
	agentErr := &AgentError{
		Kind:    ErrTurnTimeout,
		Message: "turn exceeded deadline",
		Err:     inner,
	}

	if agentErr.Unwrap() != inner {
		t.Errorf("Unwrap() = %v, want %v", agentErr.Unwrap(), inner)
	}

	// Verify errors.As works through a wrapping chain.
	wrapped := fmt.Errorf("outer: %w", agentErr)
	var extracted *AgentError
	if !errors.As(wrapped, &extracted) {
		t.Fatal("errors.As failed to extract *AgentError from wrapped chain")
	}
	if extracted.Kind != ErrTurnTimeout {
		t.Errorf("extracted.Kind = %q, want %q", extracted.Kind, ErrTurnTimeout)
	}
}

func TestAgentError_UnwrapNil(t *testing.T) {
	t.Parallel()

	err := &AgentError{
		Kind:    ErrAgentNotFound,
		Message: "command not in PATH",
	}
	if err.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", err.Unwrap())
	}
}

// Section 10.2: Session.Internal carries adapter-specific opaque state.
func TestSession_InternalRoundTrip(t *testing.T) {
	t.Parallel()

	type adapterState struct {
		pid  int
		pipe string
	}
	state := &adapterState{pid: 42, pipe: "/tmp/agent.sock"}

	s := Session{
		ID:       "sess-1",
		AgentPID: "42",
		Internal: state,
	}

	got, ok := s.Internal.(*adapterState)
	if !ok {
		t.Fatalf("Internal type assertion failed, got %T", s.Internal)
	}
	if got.pid != 42 {
		t.Errorf("Internal.pid = %d, want 42", got.pid)
	}
	if got.pipe != "/tmp/agent.sock" {
		t.Errorf("Internal.pipe = %q, want %q", got.pipe, "/tmp/agent.sock")
	}
}

// Section 10.3: AgentEvent can carry all fields including token usage.
func TestAgentEvent_Construction(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	event := AgentEvent{
		Type:      EventTokenUsage,
		Timestamp: ts,
		AgentPID:  "1234",
		Usage: TokenUsage{
			InputTokens:  500,
			OutputTokens: 200,
			TotalTokens:  700,
		},
		Message: "token update",
	}

	if event.Type != EventTokenUsage {
		t.Errorf("Type = %q, want %q", event.Type, EventTokenUsage)
	}
	if !event.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v", event.Timestamp, ts)
	}
	if event.Usage.InputTokens != 500 {
		t.Errorf("Usage.InputTokens = %d, want 500", event.Usage.InputTokens)
	}
	if event.Usage.OutputTokens != 200 {
		t.Errorf("Usage.OutputTokens = %d, want 200", event.Usage.OutputTokens)
	}
	if event.Usage.TotalTokens != 700 {
		t.Errorf("Usage.TotalTokens = %d, want 700", event.Usage.TotalTokens)
	}
}

// TokenUsage zero value is the documented sentinel for non-token events.
func TestTokenUsage_ZeroValue(t *testing.T) {
	t.Parallel()

	var usage TokenUsage
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 {
		t.Errorf("zero TokenUsage = %+v, want all zeros", usage)
	}
}
