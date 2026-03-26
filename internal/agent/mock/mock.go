// Package mock implements [domain.AgentAdapter] as a configurable test
// double for orchestrator and integration tests. Events are delivered
// synchronously via the [domain.RunTurnParams] OnEvent callback with
// canned outcomes, token usage accumulation, and optional artificial
// delays. Registered under kind "mock" via an init function.
// Safe for concurrent use.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.Register("mock", NewMockAdapter)
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*MockAdapter)(nil)

// MockAdapter is a configurable agent adapter for testing. It emits
// canned events and returns pre-configured outcomes without launching
// any subprocess. All fields except [MockAdapter.turnIndex] are
// read-only after construction.
type MockAdapter struct {
	sessionID              string
	agentPID               string
	startError             string
	turnOutcomes           []string
	eventsPerTurn          int
	inputTokensPerTurn     int
	outputTokensPerTurn    int
	cacheReadTokensPerTurn int
	modelName              string
	turnDelayMS            int
	stopError              string
	apiDurationMS          int64
	toolCalls              []mockToolCall

	// mu guards turnIndex for concurrent RunTurn calls.
	mu        sync.Mutex
	turnIndex int
}

// mockToolCall describes a single tool call to emit as a tool_result
// event during each turn.
type mockToolCall struct {
	ToolName   string
	DurationMS int64
	Error      bool
}

// NewMockAdapter creates a [MockAdapter] from adapter configuration.
// All config keys are optional with safe defaults. A zero-config map
// produces a mock that starts successfully, emits 3 notifications plus
// token_usage and turn_completed per turn, and stops cleanly.
//
// Accepted config keys: session_id, agent_pid, start_error,
// turn_outcomes, events_per_turn, input_tokens_per_turn,
// output_tokens_per_turn, turn_delay_ms, stop_error.
func NewMockAdapter(config map[string]any) (domain.AgentAdapter, error) {
	m := &MockAdapter{
		sessionID:           "mock-session-001",
		agentPID:            "",
		eventsPerTurn:       3,
		inputTokensPerTurn:  100,
		outputTokensPerTurn: 50,
	}

	if v, ok := config["session_id"].(string); ok {
		m.sessionID = v
	}
	if v, ok := config["agent_pid"].(string); ok {
		m.agentPID = v
	}
	if v, ok := config["start_error"].(string); ok {
		m.startError = v
	}
	if v, ok := config["stop_error"].(string); ok {
		m.stopError = v
	}

	if raw, ok := config["turn_outcomes"]; ok {
		switch v := raw.(type) {
		case []any:
			outcomes := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok {
					outcomes = append(outcomes, s)
				}
			}
			m.turnOutcomes = outcomes
		case []string:
			outcomes := make([]string, len(v))
			copy(outcomes, v)
			m.turnOutcomes = outcomes
		}
	}

	m.eventsPerTurn = intFromConfig(config, "events_per_turn", m.eventsPerTurn)
	m.inputTokensPerTurn = intFromConfig(config, "input_tokens_per_turn", m.inputTokensPerTurn)
	m.outputTokensPerTurn = intFromConfig(config, "output_tokens_per_turn", m.outputTokensPerTurn)
	m.cacheReadTokensPerTurn = intFromConfig(config, "cache_read_tokens_per_turn", m.cacheReadTokensPerTurn)
	m.turnDelayMS = intFromConfig(config, "turn_delay_ms", m.turnDelayMS)
	if v, ok := config["model_name"].(string); ok {
		m.modelName = v
	}

	m.apiDurationMS = int64FromConfig(config, "api_duration_ms", 0)
	if raw, ok := config["tool_calls"]; ok {
		if items, ok := raw.([]any); ok {
			for _, item := range items {
				if tc, ok := item.(map[string]any); ok {
					name, _ := tc["tool_name"].(string)
					dur := int64FromConfig(tc, "duration_ms", 0)
					tcError, _ := tc["error"].(bool)
					m.toolCalls = append(m.toolCalls, mockToolCall{
						ToolName:   name,
						DurationMS: dur,
						Error:      tcError,
					})
				}
			}
		}
	}

	return m, nil
}

// StartSession returns a canned [domain.Session] or an error when
// configured with start_error. Validates that
// [domain.StartSessionParams.WorkspacePath] is non-empty.
func (m *MockAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if m.startError != "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: m.startError,
		}
	}

	if params.WorkspacePath == "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "empty workspace path",
		}
	}

	return domain.Session{
		ID:       m.sessionID,
		AgentPID: m.agentPID,
	}, nil
}

// RunTurn emits a configurable sequence of events via params.OnEvent
// and returns a [domain.TurnResult] with the outcome determined by
// the turn_outcomes config. Token usage accumulates across turns.
func (m *MockAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("mock: OnEvent must be non-nil")
	}

	// Critical section: read and advance turn state.
	m.mu.Lock()
	currentIndex := m.turnIndex
	m.turnIndex++
	outcome := m.outcomeAt(currentIndex)
	isFirstTurn := currentIndex == 0
	m.mu.Unlock()

	// Artificial delay (outside lock).
	if m.turnDelayMS > 0 {
		timer := time.NewTimer(time.Duration(m.turnDelayMS) * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventTurnCancelled,
				Timestamp: time.Now().UTC(),
				Message:   "context cancelled",
			})
			return domain.TurnResult{
				SessionID:  session.ID,
				ExitReason: domain.EventTurnCancelled,
			}, ctx.Err()
		}
	}

	// Emit session_started on the very first turn.
	if isFirstTurn {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventSessionStarted,
			Timestamp: time.Now().UTC(),
			AgentPID:  m.agentPID,
			SessionID: m.sessionID,
			Message:   "mock session started",
		})
	}

	// Emit N notification events.
	for i := 0; i < m.eventsPerTurn; i++ {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventNotification,
			Timestamp: time.Now().UTC(),
			Message:   fmt.Sprintf("mock notification %d", i+1),
		})
	}

	// Compute cumulative token usage.
	cumulativeInput := int64(currentIndex+1) * int64(m.inputTokensPerTurn)
	cumulativeOutput := int64(currentIndex+1) * int64(m.outputTokensPerTurn)
	cumulativeCacheRead := int64(currentIndex+1) * int64(m.cacheReadTokensPerTurn)
	usage := domain.TokenUsage{
		InputTokens:     cumulativeInput,
		OutputTokens:    cumulativeOutput,
		TotalTokens:     cumulativeInput + cumulativeOutput,
		CacheReadTokens: cumulativeCacheRead,
	}

	// Emit token_usage event.
	tokenEvt := domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Usage:     usage,
		Model:     m.modelName,
		Message:   "mock token usage",
	}
	if m.apiDurationMS > 0 {
		tokenEvt.APIDurationMS = m.apiDurationMS
	}
	params.OnEvent(tokenEvt)

	// Emit tool_result events for configured tool calls.
	for _, tc := range m.toolCalls {
		params.OnEvent(domain.AgentEvent{
			Type:           domain.EventToolResult,
			Timestamp:      time.Now().UTC(),
			ToolName:       tc.ToolName,
			ToolDurationMS: tc.DurationMS,
			ToolError:      tc.Error,
			Message:        fmt.Sprintf("mock tool %s", tc.ToolName),
		})
	}

	// Map outcome to terminal event and emit it.
	exitReason, errKind, isError := outcomeToEvent(outcome)
	params.OnEvent(domain.AgentEvent{
		Type:      exitReason,
		Timestamp: time.Now().UTC(),
		Message:   fmt.Sprintf("mock turn %s", outcome),
	})

	result := domain.TurnResult{
		SessionID:  session.ID,
		ExitReason: exitReason,
		Usage:      usage,
	}

	if isError {
		return result, &domain.AgentError{
			Kind:    errKind,
			Message: fmt.Sprintf("mock turn %s", outcome),
		}
	}

	return result, nil
}

// StopSession returns nil or an error when configured with stop_error.
func (m *MockAdapter) StopSession(_ context.Context, _ domain.Session) error {
	if m.stopError != "" {
		return fmt.Errorf("mock stop: %s", m.stopError)
	}
	return nil
}

// EventStream returns nil. The mock adapter delivers all events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (m *MockAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}

// outcomeAt returns the outcome string for the given turn index.
// Falls back to "completed" when the index exceeds the configured
// turn_outcomes slice.
func (m *MockAdapter) outcomeAt(index int) string {
	if index < len(m.turnOutcomes) {
		return m.turnOutcomes[index]
	}
	return "completed"
}

// outcomeToEvent maps an outcome config string to the corresponding
// terminal event type, error kind, and whether an error should be
// returned to the caller.
func outcomeToEvent(outcome string) (domain.AgentEventType, domain.AgentErrorKind, bool) {
	switch outcome {
	case "failed":
		return domain.EventTurnFailed, domain.ErrTurnFailed, true
	case "cancelled":
		return domain.EventTurnCancelled, domain.ErrTurnCancelled, true
	case "error":
		return domain.EventTurnEndedWithError, domain.ErrPortExit, true
	case "input_required":
		return domain.EventTurnInputRequired, domain.ErrTurnInputRequired, true
	default:
		return domain.EventTurnCompleted, "", false
	}
}

// intFromConfig extracts an integer value from a config map, accepting
// both int and float64 (JSON unmarshalling yields float64). Fractional
// float64 values are rejected to prevent silent truncation. Returns
// the fallback if the key is missing or has an unexpected type.
func intFromConfig(config map[string]any, key string, fallback int) int {
	v, ok := config[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		if n != float64(int(n)) {
			return fallback
		}
		return int(n)
	default:
		return fallback
	}
}

// int64FromConfig extracts an int64 value from a config map, accepting
// int, int64, and float64 (JSON unmarshalling yields float64).
// Fractional float64 values are rejected. Returns the fallback if the
// key is missing or has an unexpected type.
func int64FromConfig(config map[string]any, key string, fallback int64) int64 {
	v, ok := config[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		if n != float64(int64(n)) {
			return fallback
		}
		return int64(n)
	default:
		return fallback
	}
}
