package mock

import (
	"context"
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

var _ domain.AgentAdapter = (*MockAdapter)(nil)

func defaultParams() domain.RunTurnParams {
	return domain.RunTurnParams{
		Prompt:  "test prompt",
		OnEvent: func(domain.AgentEvent) {},
	}
}

func collectEvents(params *domain.RunTurnParams) *[]domain.AgentEvent {
	var events []domain.AgentEvent
	params.OnEvent = func(e domain.AgentEvent) {
		events = append(events, e)
	}
	return &events
}

func TestNewMockAdapter_Defaults(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	m := adapter.(*MockAdapter)
	if m.sessionID != "mock-session-001" {
		t.Errorf("sessionID = %q, want %q", m.sessionID, "mock-session-001")
	}
	if m.agentPID != "" {
		t.Errorf("agentPID = %q, want empty", m.agentPID)
	}
	if m.startError != "" {
		t.Errorf("startError = %q, want empty", m.startError)
	}
	if m.turnOutcomes != nil {
		t.Errorf("turnOutcomes = %v, want nil", m.turnOutcomes)
	}
	if m.eventsPerTurn != 3 {
		t.Errorf("eventsPerTurn = %d, want 3", m.eventsPerTurn)
	}
	if m.inputTokensPerTurn != 100 {
		t.Errorf("inputTokensPerTurn = %d, want 100", m.inputTokensPerTurn)
	}
	if m.outputTokensPerTurn != 50 {
		t.Errorf("outputTokensPerTurn = %d, want 50", m.outputTokensPerTurn)
	}
	if m.turnDelayMS != 0 {
		t.Errorf("turnDelayMS = %d, want 0", m.turnDelayMS)
	}
	if m.stopError != "" {
		t.Errorf("stopError = %q, want empty", m.stopError)
	}
}

func TestNewMockAdapter_AllConfigKeys(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{
		"session_id":             "custom-session",
		"agent_pid":              "12345",
		"start_error":            "boom",
		"stop_error":             "stop-boom",
		"turn_outcomes":          []any{"completed", "failed"},
		"events_per_turn":        5,
		"input_tokens_per_turn":  200,
		"output_tokens_per_turn": 75,
		"turn_delay_ms":          10,
	})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	m := adapter.(*MockAdapter)
	if m.sessionID != "custom-session" {
		t.Errorf("sessionID = %q, want %q", m.sessionID, "custom-session")
	}
	if m.agentPID != "12345" {
		t.Errorf("agentPID = %q, want %q", m.agentPID, "12345")
	}
	if m.startError != "boom" {
		t.Errorf("startError = %q, want %q", m.startError, "boom")
	}
	if m.stopError != "stop-boom" {
		t.Errorf("stopError = %q, want %q", m.stopError, "stop-boom")
	}
	if len(m.turnOutcomes) != 2 || m.turnOutcomes[0] != "completed" || m.turnOutcomes[1] != "failed" {
		t.Errorf("turnOutcomes = %v, want [completed failed]", m.turnOutcomes)
	}
	if m.eventsPerTurn != 5 {
		t.Errorf("eventsPerTurn = %d, want 5", m.eventsPerTurn)
	}
	if m.inputTokensPerTurn != 200 {
		t.Errorf("inputTokensPerTurn = %d, want 200", m.inputTokensPerTurn)
	}
	if m.outputTokensPerTurn != 75 {
		t.Errorf("outputTokensPerTurn = %d, want 75", m.outputTokensPerTurn)
	}
	if m.turnDelayMS != 10 {
		t.Errorf("turnDelayMS = %d, want 10", m.turnDelayMS)
	}
}

func TestNewMockAdapter_FloatTokenValues(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{
		"events_per_turn":        float64(7),
		"input_tokens_per_turn":  float64(250),
		"output_tokens_per_turn": float64(125),
		"turn_delay_ms":          float64(42),
	})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	m := adapter.(*MockAdapter)
	if m.eventsPerTurn != 7 {
		t.Errorf("eventsPerTurn = %d, want 7", m.eventsPerTurn)
	}
	if m.inputTokensPerTurn != 250 {
		t.Errorf("inputTokensPerTurn = %d, want 250", m.inputTokensPerTurn)
	}
	if m.outputTokensPerTurn != 125 {
		t.Errorf("outputTokensPerTurn = %d, want 125", m.outputTokensPerTurn)
	}
	if m.turnDelayMS != 42 {
		t.Errorf("turnDelayMS = %d, want 42", m.turnDelayMS)
	}
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	ctor, err := registry.Agents.Get("mock")
	if err != nil {
		t.Fatalf("registry.Agents.Get(\"mock\") error = %v", err)
	}

	adapter, err := ctor(map[string]any{})
	if err != nil {
		t.Fatalf("constructor() error = %v", err)
	}
	if adapter == nil {
		t.Fatal("constructor() returned nil adapter")
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]any
		params   domain.StartSessionParams
		wantID   string
		wantPID  string
		wantErr  bool
		wantKind domain.AgentErrorKind
	}{
		{
			name:   "success",
			config: map[string]any{"session_id": "sess-1", "agent_pid": "99"},
			params: domain.StartSessionParams{WorkspacePath: "/tmp/work"},
			wantID: "sess-1", wantPID: "99",
		},
		{
			name:     "start_error",
			config:   map[string]any{"start_error": "agent missing"},
			params:   domain.StartSessionParams{WorkspacePath: "/tmp/work"},
			wantErr:  true,
			wantKind: domain.ErrAgentNotFound,
		},
		{
			name:     "empty workspace",
			config:   map[string]any{},
			params:   domain.StartSessionParams{WorkspacePath: ""},
			wantErr:  true,
			wantKind: domain.ErrInvalidWorkspaceCwd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, err := NewMockAdapter(tt.config)
			if err != nil {
				t.Fatalf("NewMockAdapter() error = %v", err)
			}

			sess, err := adapter.StartSession(context.Background(), tt.params)
			if tt.wantErr {
				var ae *domain.AgentError
				if !errors.As(err, &ae) {
					t.Fatalf("expected *AgentError, got %T: %v", err, err)
				}
				if ae.Kind != tt.wantKind {
					t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			if sess.ID != tt.wantID {
				t.Errorf("Session.ID = %q, want %q", sess.ID, tt.wantID)
			}
			if sess.AgentPID != tt.wantPID {
				t.Errorf("Session.AgentPID = %q, want %q", sess.AgentPID, tt.wantPID)
			}
		})
	}
}

func TestRunTurn_DefaultSuccess(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	result, err := adapter.RunTurn(context.Background(), sess, params)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Verify result.
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.SessionID != "mock-session-001" {
		t.Errorf("SessionID = %q, want %q", result.SessionID, "mock-session-001")
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 50 || result.Usage.TotalTokens != 150 {
		t.Errorf("Usage = %+v, want {100 50 150}", result.Usage)
	}

	// Verify event sequence: session_started, 3×notification, token_usage, turn_completed.
	wantTypes := []domain.AgentEventType{
		domain.EventSessionStarted,
		domain.EventNotification,
		domain.EventNotification,
		domain.EventNotification,
		domain.EventTokenUsage,
		domain.EventTurnCompleted,
	}
	if len(*events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(*events), len(wantTypes))
	}
	for i, want := range wantTypes {
		if (*events)[i].Type != want {
			t.Errorf("events[%d].Type = %q, want %q", i, (*events)[i].Type, want)
		}
	}

	// Verify token_usage event carries correct usage.
	tokenEvt := (*events)[4]
	if tokenEvt.Usage.InputTokens != 100 || tokenEvt.Usage.OutputTokens != 50 || tokenEvt.Usage.TotalTokens != 150 {
		t.Errorf("token event Usage = %+v, want {100 50 150}", tokenEvt.Usage)
	}
}

// TestRunTurn_SecondTurnEmitsSessionStarted verifies that session_started
// is emitted on every turn, not just the first. Real adapters (Claude Code)
// spawn a fresh subprocess per turn and emit session_started at startup.
func TestRunTurn_SecondTurnEmitsSessionStarted(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	sess := domain.Session{ID: "s"}

	// Turn 1 — consume.
	adapter.RunTurn(context.Background(), sess, defaultParams()) //nolint:errcheck // test setup

	// Turn 2 — collect events.
	params := defaultParams()
	events := collectEvents(&params)

	_, err := adapter.RunTurn(context.Background(), sess, params)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var found bool
	for _, e := range *events {
		if e.Type == domain.EventSessionStarted {
			found = true
			break
		}
	}
	if !found {
		t.Error("session_started not emitted on second turn; expected on every turn")
	}
}

func TestRunTurn_MultiTurnTokenAccumulation(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	sess := domain.Session{ID: "s"}

	// Token usage is cumulative across turns.
	wantUsage := []domain.TokenUsage{
		{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
		{InputTokens: 300, OutputTokens: 150, TotalTokens: 450},
	}

	for i, want := range wantUsage {
		result, err := adapter.RunTurn(context.Background(), sess, defaultParams())
		if err != nil {
			t.Fatalf("turn %d: RunTurn() error = %v", i+1, err)
		}
		if result.Usage != want {
			t.Errorf("turn %d: Usage = %+v, want %+v", i+1, result.Usage, want)
		}
	}
}

func TestRunTurn_ConfiguredOutcomes(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"turn_outcomes": []any{"completed", "failed", "cancelled"},
	})
	sess := domain.Session{ID: "s"}

	tests := []struct {
		turn     int
		wantExit domain.AgentEventType
		wantErr  bool
		wantKind domain.AgentErrorKind
	}{
		{1, domain.EventTurnCompleted, false, ""},
		{2, domain.EventTurnFailed, true, domain.ErrTurnFailed},
		{3, domain.EventTurnCancelled, true, domain.ErrTurnCancelled},
	}

	for _, tt := range tests {
		result, err := adapter.RunTurn(context.Background(), sess, defaultParams())

		if result.ExitReason != tt.wantExit {
			t.Errorf("turn %d: ExitReason = %q, want %q", tt.turn, result.ExitReason, tt.wantExit)
		}
		if tt.wantErr {
			var ae *domain.AgentError
			if !errors.As(err, &ae) {
				t.Fatalf("turn %d: expected *AgentError, got %T: %v", tt.turn, err, err)
			}
			if ae.Kind != tt.wantKind {
				t.Errorf("turn %d: AgentError.Kind = %q, want %q", tt.turn, ae.Kind, tt.wantKind)
			}
		} else if err != nil {
			t.Errorf("turn %d: unexpected error = %v", tt.turn, err)
		}
	}
}

func TestRunTurn_ExhaustedOutcomes(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"turn_outcomes": []any{"failed"},
	})
	sess := domain.Session{ID: "s"}

	// Turn 1: fails per config.
	_, err := adapter.RunTurn(context.Background(), sess, defaultParams())
	if err == nil {
		t.Fatal("turn 1: expected error")
	}

	// Turn 2: outcomes exhausted, defaults to completed.
	result, err := adapter.RunTurn(context.Background(), sess, defaultParams())
	if err != nil {
		t.Fatalf("turn 2: unexpected error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("turn 2: ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
}

func TestRunTurn_InputRequired(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"turn_outcomes": []any{"input_required"},
	})
	sess := domain.Session{ID: "s"}
	params := defaultParams()
	events := collectEvents(&params)

	_, err := adapter.RunTurn(context.Background(), sess, params)

	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AgentError, got %T: %v", err, err)
	}
	if ae.Kind != domain.ErrTurnInputRequired {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, domain.ErrTurnInputRequired)
	}

	// Verify terminal event.
	last := (*events)[len(*events)-1]
	if last.Type != domain.EventTurnInputRequired {
		t.Errorf("last event = %q, want %q", last.Type, domain.EventTurnInputRequired)
	}
}

func TestRunTurn_ErrorOutcome(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"turn_outcomes": []any{"error"},
	})
	sess := domain.Session{ID: "s"}
	params := defaultParams()
	events := collectEvents(&params)

	_, err := adapter.RunTurn(context.Background(), sess, params)

	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AgentError, got %T: %v", err, err)
	}
	if ae.Kind != domain.ErrPortExit {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, domain.ErrPortExit)
	}

	last := (*events)[len(*events)-1]
	if last.Type != domain.EventTurnEndedWithError {
		t.Errorf("last event = %q, want %q", last.Type, domain.EventTurnEndedWithError)
	}
}

func TestRunTurn_ContextCancellation(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"turn_delay_ms": 60000,
	})
	sess := domain.Session{ID: "s"}
	params := defaultParams()
	events := collectEvents(&params)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := adapter.RunTurn(ctx, sess, params)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	if len(*events) == 0 {
		t.Fatal("expected at least one event")
	}
	if (*events)[0].Type != domain.EventTurnCancelled {
		t.Errorf("event type = %q, want %q", (*events)[0].Type, domain.EventTurnCancelled)
	}
}

func TestRunTurn_CustomEventsPerTurn(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"events_per_turn": 0,
	})
	sess := domain.Session{ID: "s"}
	params := defaultParams()
	events := collectEvents(&params)

	_, err := adapter.RunTurn(context.Background(), sess, params)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// With 0 notifications: session_started + token_usage + turn_completed = 3.
	wantTypes := []domain.AgentEventType{
		domain.EventSessionStarted,
		domain.EventTokenUsage,
		domain.EventTurnCompleted,
	}
	if len(*events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d", len(*events), len(wantTypes))
	}
	for i, want := range wantTypes {
		if (*events)[i].Type != want {
			t.Errorf("events[%d].Type = %q, want %q", i, (*events)[i].Type, want)
		}
	}
}

func TestStopSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
		wantMsg string
	}{
		{
			name:   "success",
			config: map[string]any{},
		},
		{
			name:    "configured error",
			config:  map[string]any{"stop_error": "cleanup failed"},
			wantErr: true,
			wantMsg: "cleanup failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, _ := NewMockAdapter(tt.config)
			err := adapter.StopSession(context.Background(), domain.Session{})

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if got := err.Error(); got != "mock stop: "+tt.wantMsg {
					t.Errorf("error = %q, want containing %q", got, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Errorf("StopSession() error = %v", err)
			}
		})
	}
}

func TestEventStream_ReturnsNil(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	if ch := adapter.EventStream(); ch != nil {
		t.Errorf("EventStream() = %v, want nil", ch)
	}
}

// TestNewMockAdapter_ExtendedConfigKeys verifies that the new
// cache_read_tokens_per_turn and model_name config keys are parsed.
func TestNewMockAdapter_ExtendedConfigKeys(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{
		"cache_read_tokens_per_turn": 500,
		"model_name":                 "claude-sonnet-4-20250514",
	})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	m := adapter.(*MockAdapter)
	if m.cacheReadTokensPerTurn != 500 {
		t.Errorf("cacheReadTokensPerTurn = %d, want 500", m.cacheReadTokensPerTurn)
	}
	if m.modelName != "claude-sonnet-4-20250514" {
		t.Errorf("modelName = %q, want %q", m.modelName, "claude-sonnet-4-20250514")
	}
}

// TestRunTurn_ExtendedTokenFields verifies that token_usage events include
// CacheReadTokens and Model, and that cumulative accumulation works.
func TestRunTurn_ExtendedTokenFields(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"input_tokens_per_turn":      100,
		"output_tokens_per_turn":     50,
		"cache_read_tokens_per_turn": 200,
		"model_name":                 "test-model",
		"events_per_turn":            0,
	})
	sess := domain.Session{ID: "s"}

	// Turn 1: cumulative cache_read = 200.
	params1 := defaultParams()
	events1 := collectEvents(&params1)
	if _, err := adapter.RunTurn(context.Background(), sess, params1); err != nil {
		t.Fatalf("RunTurn(1) error = %v", err)
	}

	var tokenEv1 *domain.AgentEvent
	for i := range *events1 {
		if (*events1)[i].Type == domain.EventTokenUsage {
			tokenEv1 = &(*events1)[i]
			break
		}
	}
	if tokenEv1 == nil {
		t.Fatal("no token_usage event in turn 1")
	}
	if tokenEv1.Usage.CacheReadTokens != 200 {
		t.Errorf("turn 1 CacheReadTokens = %d, want 200", tokenEv1.Usage.CacheReadTokens)
	}
	if tokenEv1.Model != "test-model" {
		t.Errorf("turn 1 Model = %q, want %q", tokenEv1.Model, "test-model")
	}

	// Turn 2: cumulative cache_read = 400.
	params2 := defaultParams()
	events2 := collectEvents(&params2)
	if _, err := adapter.RunTurn(context.Background(), sess, params2); err != nil {
		t.Fatalf("RunTurn(2) error = %v", err)
	}

	var tokenEv2 *domain.AgentEvent
	for i := range *events2 {
		if (*events2)[i].Type == domain.EventTokenUsage {
			tokenEv2 = &(*events2)[i]
			break
		}
	}
	if tokenEv2 == nil {
		t.Fatal("no token_usage event in turn 2")
	}
	if tokenEv2.Usage.CacheReadTokens != 400 {
		t.Errorf("turn 2 CacheReadTokens = %d, want 400", tokenEv2.Usage.CacheReadTokens)
	}
}

// --- Per-session timing tests (Spec 8.19) ---

// TestNewMockAdapter_TimingConfig verifies parsing of api_duration_ms
// and tool_calls configuration keys.
func TestNewMockAdapter_TimingConfig(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{
		"api_duration_ms": float64(750),
		"tool_calls": []any{
			map[string]any{"tool_name": "Read", "duration_ms": float64(120)},
			map[string]any{"tool_name": "Write", "duration_ms": float64(350)},
		},
	})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	m := adapter.(*MockAdapter)
	if m.apiDurationMS != 750 {
		t.Errorf("apiDurationMS = %d, want 750", m.apiDurationMS)
	}
	if len(m.toolCalls) != 2 {
		t.Fatalf("len(toolCalls) = %d, want 2", len(m.toolCalls))
	}
	if m.toolCalls[0].ToolName != "Read" || m.toolCalls[0].DurationMS != 120 {
		t.Errorf("toolCalls[0] = %+v, want {Read 120}", m.toolCalls[0])
	}
	if m.toolCalls[1].ToolName != "Write" || m.toolCalls[1].DurationMS != 350 {
		t.Errorf("toolCalls[1] = %+v, want {Write 350}", m.toolCalls[1])
	}
}

// TestRunTurn_APIDurationMS_OnTokenUsage verifies that the token_usage
// event carries APIDurationMS when api_duration_ms is configured.
func TestRunTurn_APIDurationMS_OnTokenUsage(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"api_duration_ms": float64(500),
	})
	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	if _, err := adapter.RunTurn(context.Background(), sess, params); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	for _, e := range *events {
		if e.Type == domain.EventTokenUsage {
			if e.APIDurationMS != 500 {
				t.Errorf("token_usage APIDurationMS = %d, want 500", e.APIDurationMS)
			}
			return
		}
	}
	t.Error("no token_usage event found")
}

// TestRunTurn_APIDurationMS_ZeroByDefault verifies that APIDurationMS
// is 0 on token_usage events when api_duration_ms is not configured.
func TestRunTurn_APIDurationMS_ZeroByDefault(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	if _, err := adapter.RunTurn(context.Background(), sess, params); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	for _, e := range *events {
		if e.Type == domain.EventTokenUsage {
			if e.APIDurationMS != 0 {
				t.Errorf("token_usage APIDurationMS = %d, want 0", e.APIDurationMS)
			}
			return
		}
	}
	t.Error("no token_usage event found")
}

// TestRunTurn_ToolCalls_EmitsToolResultEvents verifies that configured
// tool_calls produce tool_result events with correct ToolName and
// ToolDurationMS.
func TestRunTurn_ToolCalls_EmitsToolResultEvents(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{
		"tool_calls": []any{
			map[string]any{"tool_name": "Read", "duration_ms": float64(100)},
			map[string]any{"tool_name": "Bash", "duration_ms": float64(250)},
		},
	})
	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	if _, err := adapter.RunTurn(context.Background(), sess, params); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var toolResults []domain.AgentEvent
	for _, e := range *events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	if len(toolResults) != 2 {
		t.Fatalf("got %d tool_result events, want 2", len(toolResults))
	}
	if toolResults[0].ToolName != "Read" || toolResults[0].ToolDurationMS != 100 {
		t.Errorf("tool_result[0] = {%q, %d}, want {Read, 100}", toolResults[0].ToolName, toolResults[0].ToolDurationMS)
	}
	if toolResults[1].ToolName != "Bash" || toolResults[1].ToolDurationMS != 250 {
		t.Errorf("tool_result[1] = {%q, %d}, want {Bash, 250}", toolResults[1].ToolName, toolResults[1].ToolDurationMS)
	}
}

// TestRunTurn_NoToolCalls_NoToolResultEvents verifies that no tool_result
// events are emitted when tool_calls is not configured.
func TestRunTurn_NoToolCalls_NoToolResultEvents(t *testing.T) {
	t.Parallel()

	adapter, _ := NewMockAdapter(map[string]any{})
	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	if _, err := adapter.RunTurn(context.Background(), sess, params); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	for _, e := range *events {
		if e.Type == domain.EventToolResult {
			t.Error("unexpected tool_result event when tool_calls not configured")
		}
	}
}

// TestRunTurn_ToolCalls_ErrorField verifies that the error field in
// tool_calls config is parsed and emitted as ToolError on the event.
func TestRunTurn_ToolCalls_ErrorField(t *testing.T) {
	t.Parallel()

	adapter, err := NewMockAdapter(map[string]any{
		"tool_calls": []any{
			map[string]any{"tool_name": "Bash", "duration_ms": float64(100), "error": true},
			map[string]any{"tool_name": "Read", "duration_ms": float64(50)},
		},
	})
	if err != nil {
		t.Fatalf("NewMockAdapter() error = %v", err)
	}

	sess := domain.Session{ID: "mock-session-001"}
	params := defaultParams()
	events := collectEvents(&params)

	if _, err := adapter.RunTurn(context.Background(), sess, params); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var toolResults []domain.AgentEvent
	for _, e := range *events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	if len(toolResults) != 2 {
		t.Fatalf("got %d tool_result events, want 2", len(toolResults))
	}
	if !toolResults[0].ToolError {
		t.Errorf("tool_result[0] ToolError = false, want true (Bash configured with error: true)")
	}
	if toolResults[1].ToolError {
		t.Errorf("tool_result[1] ToolError = true, want false (Read has no error config)")
	}
}
