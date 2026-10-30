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

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// usageSources: the mock reports the counts its configuration sets as a
// running total since its session state began.
var usageSources = usage.Sources{Live: usage.Source{
	Category: usage.SourceOutput,
	Strategy: usage.StrategyLive,
	Spend:    usage.SpendDeclaration{Shape: usage.SpendProcessCumulative, DuringTurn: true},
}}

func init() {
	registry.Agents.RegisterWithMeta("mock", NewMockAdapter, registry.AgentMeta{
		MCPDialect:            registry.MCPDialectNone,
		MCPDialectNoneReason:  "launches no process",
		UsageLimitRecognition: registry.UsageLimitRecognized,
		UsageSources:          usageSources,
		CredentialEnv:         registry.DeclareCredentialEnv(),
		EffortForwarding:      registry.EffortNotForwarded,
		VerificationContext:   registry.DeclareVerificationContext(),
	})
}

const usageLimitOutcome = "usage_limit"

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*MockAdapter)(nil)

// MockAdapter is a configurable agent adapter for testing. It emits
// canned events and returns pre-configured outcomes without launching
// any subprocess. Each session carries its own settings; only the turn
// cursor is shared by every session.
type MockAdapter struct {
	// mu guards turnIndex for concurrent RunTurn calls.
	mu        sync.Mutex
	turnIndex int
}

// mockSettings is one session's parsed settings block.
type mockSettings struct {
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
	reportTokenUsage       bool
	credentialError        string
	usageLimitResetsAt     time.Time
}

func (s *mockSessionState) accrueTurnUsage() {
	s.usage.InputTokens += int64(s.inputTokensPerTurn)
	s.usage.OutputTokens += int64(s.outputTokensPerTurn)
	s.usage.CacheReadTokens += int64(s.cacheReadTokensPerTurn)
	s.usage.TotalTokens = s.usage.InputTokens + s.usage.OutputTokens
}

type mockSessionState struct {
	mockSettings
	credentialVerification bool
	usage                  domain.TokenUsage
}

// mockToolCall describes a single tool call to emit as a tool_result
// event during each turn.
type mockToolCall struct {
	ToolName   string
	DurationMS int64
	Error      bool
}

// NewMockAdapter creates a [MockAdapter]. Every settings key is optional
// with a safe default; an empty block produces a mock that starts
// successfully, emits 3 notifications plus token_usage and turn_completed
// per turn, and stops cleanly.
//
// Accepted settings keys: session_id, agent_pid, start_error,
// turn_outcomes, events_per_turn, input_tokens_per_turn,
// output_tokens_per_turn, turn_delay_ms, stop_error,
// report_token_usage, usage_limit_resets_at. input_tokens_per_turn counts all input including
// cache reads, so cache_read_tokens_per_turn is a subset of it rather
// than an addition to it. The adapter reports run-cumulative counts by
// construction: each turn's contribution accrues onto the totals of
// every prior turn in the same session.
// report_token_usage defaults to true; when false, the adapter emits no
// token_usage event, leaves [domain.TurnResult] Usage at the zero
// value, and reports UsageMeasured false, simulating a runtime that
// reported nothing: a turn that served output then carries
// SpendUnaccounted, an amount unknown. The error outcome does not carry
// it, because its terminal event bypasses the shared disposition. A
// verification session's turn emits no event, consumes no turn_outcomes
// entry, and fails with credential_error when it is set.
//
// turn_outcomes entries are completed, failed, cancelled, error,
// input_required, incomplete, and usage_limit. A usage_limit turn
// fails with [domain.ErrUsageLimit], accrues no usage because a refused
// request bills nothing, and carries the reset instant from
// usage_limit_resets_at (RFC 3339; absent or unparseable gives a zero
// instant).
func NewMockAdapter() (domain.AgentAdapter, error) {
	return &MockAdapter{}, nil
}

// parseSettings reads a session's settings block.
func parseSettings(config map[string]any) mockSettings {
	m := mockSettings{
		sessionID:           "mock-session-001",
		eventsPerTurn:       3,
		inputTokensPerTurn:  100,
		outputTokensPerTurn: 50,
		reportTokenUsage:    true,
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
	if v, ok := config["credential_error"].(string); ok {
		m.credentialError = v
	}

	if raw, ok := config["turn_outcomes"]; ok {
		switch v := raw.(type) {
		case []any:
			outcomes := make([]string, 0, len(v))
			for _, raw := range v {
				if s, ok := raw.(string); ok {
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

	m.eventsPerTurn = typeutil.IntFrom(config, "events_per_turn", m.eventsPerTurn)
	m.inputTokensPerTurn = typeutil.IntFrom(config, "input_tokens_per_turn", m.inputTokensPerTurn)
	m.outputTokensPerTurn = typeutil.IntFrom(config, "output_tokens_per_turn", m.outputTokensPerTurn)
	m.cacheReadTokensPerTurn = typeutil.IntFrom(config, "cache_read_tokens_per_turn", m.cacheReadTokensPerTurn)
	m.turnDelayMS = typeutil.IntFrom(config, "turn_delay_ms", m.turnDelayMS)
	m.reportTokenUsage = typeutil.BoolFrom(config, "report_token_usage", m.reportTokenUsage)
	if v, ok := config["model_name"].(string); ok {
		m.modelName = v
	}

	if v, ok := config["usage_limit_resets_at"].(string); ok {
		if resetsAt, err := time.Parse(time.RFC3339, v); err == nil {
			m.usageLimitResetsAt = resetsAt.UTC()
		}
	}

	m.apiDurationMS = int64FromConfig(config, "api_duration_ms", 0)
	if raw, ok := config["tool_calls"]; ok {
		if rawCalls, ok := raw.([]any); ok {
			for _, rawCall := range rawCalls {
				if tc, ok := rawCall.(map[string]any); ok {
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

	return m
}

// StartSession returns a canned [domain.Session] or an error when the
// session's settings carry start_error. Validates that
// [domain.StartSessionParams.WorkspacePath] is non-empty.
func (m *MockAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	settings := parseSettings(params.Settings)
	if settings.startError != "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrAgentNotFound,
			Message: settings.startError,
		}
	}

	if params.WorkspacePath == "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "empty workspace path",
		}
	}

	return domain.Session{
		ID:       settings.sessionID,
		AgentPID: settings.agentPID,
		Internal: &mockSessionState{mockSettings: settings, credentialVerification: params.CredentialVerification},
	}, nil
}

// RunTurn emits a configurable sequence of events via params.OnEvent
// and returns a [domain.TurnResult] with the outcome determined by
// the turn_outcomes config. Token usage accumulates across turns.
func (m *MockAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("mock: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*mockSessionState)
	if !ok {
		state = &mockSessionState{mockSettings: parseSettings(nil)}
	}
	verifying := state.credentialVerification

	if verifying {
		result := domain.TurnResult{
			SessionID:     session.ID,
			ExitReason:    domain.EventTurnCompleted,
			UsageMeasured: state.reportTokenUsage,
		}
		if state.credentialError != "" {
			result.ExitReason = domain.EventTurnFailed
			return result, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: state.credentialError,
			}
		}
		return result, nil
	}

	// Critical section: read and advance turn state.
	m.mu.Lock()
	currentIndex := m.turnIndex
	m.turnIndex++
	outcome := state.outcomeAt(currentIndex)
	if state.reportTokenUsage && outcome != usageLimitOutcome {
		state.accrueTurnUsage()
	}
	cumulative := state.usage
	m.mu.Unlock()

	// Artificial delay (outside lock).
	if state.turnDelayMS > 0 {
		timer := time.NewTimer(time.Duration(state.turnDelayMS) * time.Millisecond)
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

	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: time.Now().UTC(),
		AgentPID:  state.agentPID,
		SessionID: state.sessionID,
		Message:   "mock session started",
	})

	for i := range state.eventsPerTurn {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventNotification,
			Timestamp: time.Now().UTC(),
			Message:   fmt.Sprintf("mock notification %d", i+1),
		})
	}

	if state.reportTokenUsage {
		tokenEvt := domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     cumulative,
			Model:     state.modelName,
			Message:   "mock token usage",
		}
		if state.apiDurationMS > 0 {
			tokenEvt.APIDurationMS = state.apiDurationMS
		}
		params.OnEvent(tokenEvt)
	}

	for _, tc := range state.toolCalls {
		params.OnEvent(domain.AgentEvent{
			Type:           domain.EventToolResult,
			Timestamp:      time.Now().UTC(),
			ToolName:       tc.ToolName,
			ToolDurationMS: tc.DurationMS,
			ToolError:      tc.Error,
			Message:        fmt.Sprintf("mock tool %s", tc.ToolName),
		})
	}

	spend := turnSpend(state, outcome)

	switch outcome {
	case "input_required":
		ev := agentcore.TurnEvidence{
			Terminal:        agentcore.TerminalHumanInputRequired,
			TerminalMessage: fmt.Sprintf("mock turn %s", outcome),
		}
		meta := agentcore.TurnMeta{
			SessionID:     session.ID,
			Usage:         cumulative,
			UsageMeasured: state.reportTokenUsage,
		}
		return agentcore.FinalizeTurn(params.OnEvent, nil, ev, meta, spend)
	case "incomplete":
		ev := agentcore.TurnEvidence{
			Terminal:        agentcore.TerminalIncomplete,
			TerminalMessage: "mock turn incomplete",
		}
		meta := agentcore.TurnMeta{
			SessionID:     session.ID,
			Usage:         cumulative,
			UsageMeasured: state.reportTokenUsage,
		}
		return agentcore.FinalizeTurn(params.OnEvent, nil, ev, meta, spend)
	case "error":
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnEndedWithError,
			Timestamp: time.Now().UTC(),
			Message:   fmt.Sprintf("mock turn %s", outcome),
		})
		result := domain.TurnResult{
			SessionID:     session.ID,
			ExitReason:    domain.EventTurnEndedWithError,
			Usage:         cumulative,
			UsageMeasured: state.reportTokenUsage,
		}
		return result, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("mock turn %s", outcome),
		}
	}

	ev := agentcore.TurnEvidence{
		Terminal:        scriptedTerminal(outcome),
		TerminalMessage: fmt.Sprintf("mock turn %s", outcome),
	}
	if outcome == usageLimitOutcome {
		ev.TerminalErrorKind = domain.ErrUsageLimit
		ev.UsageLimit = &domain.UsageLimit{ResetsAt: state.usageLimitResetsAt}
	}
	// The terminal event carries no usage: the token_usage event above is
	// the turn's one figure.
	meta := agentcore.TurnMeta{SessionID: session.ID, UsageMeasured: state.reportTokenUsage}
	result, agentErr := agentcore.FinalizeTurn(params.OnEvent, nil, ev, meta, spend)
	result.Usage = cumulative
	if agentErr == nil {
		return result, nil
	}
	return result, agentErr
}

// turnSpend is the spend evidence of a turn that served output: its amount
// is settled when the mock reports usage and unknown when it does not. A
// usage_limit turn was refused, so it spent nothing.
func turnSpend(state *mockSessionState, outcome string) agentcore.TurnSpend {
	var spend agentcore.TurnSpend
	if outcome == usageLimitOutcome {
		return spend
	}
	spend.MarkModelReached()
	if state.reportTokenUsage {
		spend.MarkSettled()
	}
	return spend
}

// scriptedTerminal maps a turn_outcomes entry that ends through the shared
// disposition to the runtime's terminal report.
func scriptedTerminal(outcome string) agentcore.TerminalReport {
	switch outcome {
	case "failed", usageLimitOutcome:
		return agentcore.TerminalFailure
	case "cancelled":
		return agentcore.TerminalCancelled
	default:
		return agentcore.TerminalSuccess
	}
}

// StopSession returns nil or an error when the session's settings carry
// stop_error.
func (m *MockAdapter) StopSession(_ context.Context, session domain.Session) error {
	state, ok := session.Internal.(*mockSessionState)
	if ok && state.stopError != "" {
		return fmt.Errorf("mock stop: %s", state.stopError)
	}
	return nil
}

// outcomeAt returns the outcome string for the given turn index.
// Falls back to "completed" when the index exceeds the configured
// turn_outcomes slice.
func (s mockSettings) outcomeAt(index int) string {
	if index < len(s.turnOutcomes) {
		return s.turnOutcomes[index]
	}
	return "completed"
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
