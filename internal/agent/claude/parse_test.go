package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loading fixture %s: %v", name, err)
	}
	return data
}

func TestParseEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fixture   string
		wantType  string
		wantSub   string
		checkFunc func(t *testing.T, ev rawEvent)
	}{
		{
			name:     "init system event",
			fixture:  "init_event.json",
			wantType: "system",
			wantSub:  "init",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if ev.SessionID != "abc12345-def6-4789-abcd-ef0123456789" {
					t.Errorf("SessionID = %q, want abc12345-...", ev.SessionID)
				}
				if ev.Cwd != "/home/user/workspace/PROJECT-123" {
					t.Errorf("Cwd = %q", ev.Cwd)
				}
			},
		},
		{
			name:     "assistant message with text and tool_use",
			fixture:  "assistant_message.json",
			wantType: "assistant",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				blocks := ev.contentBlocks()
				if len(blocks) != 2 {
					t.Fatalf("content blocks = %d, want 2", len(blocks))
				}
				if blocks[0].Type != "text" {
					t.Errorf("block[0].Type = %q, want text", blocks[0].Type)
				}
				if blocks[1].Type != "tool_use" {
					t.Errorf("block[1].Type = %q, want tool_use", blocks[1].Type)
				}
				if blocks[1].Name != "Read" {
					t.Errorf("block[1].Name = %q, want Read", blocks[1].Name)
				}
			},
		},
		{
			name:     "tool_use message",
			fixture:  "tool_use_message.json",
			wantType: "assistant",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				blocks := ev.contentBlocks()
				if len(blocks) != 2 {
					t.Fatalf("content blocks = %d, want 2", len(blocks))
				}
				if blocks[0].Type != "tool_use" {
					t.Errorf("block[0].Type = %q, want tool_use", blocks[0].Type)
				}
				if blocks[1].Type != "tool_result" {
					t.Errorf("block[1].Type = %q, want tool_result", blocks[1].Type)
				}
				if blocks[1].IsError {
					t.Error("block[1].IsError = true, want false")
				}
			},
		},
		{
			name:     "result success",
			fixture:  "result_success.json",
			wantType: "result",
			wantSub:  "success",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if ev.IsError {
					t.Error("IsError = true, want false")
				}
				if ev.Usage == nil {
					t.Fatal("Usage is nil")
				}
				if ev.Usage.InputTokens != 15000 {
					t.Errorf("InputTokens = %d, want 15000", ev.Usage.InputTokens)
				}
				if ev.Usage.OutputTokens != 3200 {
					t.Errorf("OutputTokens = %d, want 3200", ev.Usage.OutputTokens)
				}
				if ev.NumTurns != 3 {
					t.Errorf("NumTurns = %d, want 3", ev.NumTurns)
				}
			},
		},
		{
			name:     "result error max_turns",
			fixture:  "result_error_max_turns.json",
			wantType: "result",
			wantSub:  "error_max_turns",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if !ev.IsError {
					t.Error("IsError = false, want true")
				}
				if ev.Usage == nil {
					t.Fatal("Usage is nil")
				}
				if ev.Usage.InputTokens != 50000 {
					t.Errorf("InputTokens = %d, want 50000", ev.Usage.InputTokens)
				}
			},
		},
		{
			name:     "result error during execution",
			fixture:  "result_error_execution.json",
			wantType: "result",
			wantSub:  "error_during_execution",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if !ev.IsError {
					t.Error("IsError = false, want true")
				}
				if ev.Result == "" {
					t.Error("Result is empty, want error message")
				}
			},
		},
		{
			name:     "api retry event",
			fixture:  "api_retry_event.json",
			wantType: "system",
			wantSub:  "api_retry",
			checkFunc: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if ev.Attempt != 1 {
					t.Errorf("Attempt = %d, want 1", ev.Attempt)
				}
				if ev.MaxRetries != 5 {
					t.Errorf("MaxRetries = %d, want 5", ev.MaxRetries)
				}
				if ev.RetryDelayMS != 1000 {
					t.Errorf("RetryDelayMS = %d, want 1000", ev.RetryDelayMS)
				}
				if ev.ErrorStatus != 429 {
					t.Errorf("ErrorStatus = %d, want 429", ev.ErrorStatus)
				}
				if ev.ErrorField != "rate_limit" {
					t.Errorf("ErrorField = %q, want rate_limit", ev.ErrorField)
				}
			},
		},
		{
			name:     "stream event",
			fixture:  "stream_event.json",
			wantType: "stream_event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := loadFixture(t, tt.fixture)

			ev, err := parseEvent(data)
			if err != nil {
				t.Fatalf("parseEvent() error = %v", err)
			}
			if ev.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tt.wantType)
			}
			if tt.wantSub != "" && ev.Subtype != tt.wantSub {
				t.Errorf("Subtype = %q, want %q", ev.Subtype, tt.wantSub)
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, ev)
			}
		})
	}
}

func TestParseEvent_Malformed(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "malformed_line.txt")
	_, err := parseEvent(data)
	if err == nil {
		t.Fatal("parseEvent(malformed) did not return error")
	}
}

func TestNormalizeUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  *rawUsage
		want domain.TokenUsage
	}{
		{
			name: "nil usage",
			raw:  nil,
			want: domain.TokenUsage{},
		},
		{
			name: "valid usage",
			raw: &rawUsage{
				InputTokens:  15000,
				OutputTokens: 3200,
			},
			want: domain.TokenUsage{
				InputTokens:  15000,
				OutputTokens: 3200,
				TotalTokens:  18200,
			},
		},
		{
			name: "zero usage",
			raw:  &rawUsage{},
			want: domain.TokenUsage{},
		},
		{
			name: "cache read input tokens",
			raw: &rawUsage{
				InputTokens:          12000,
				OutputTokens:         3000,
				CacheReadInputTokens: 8000,
			},
			want: domain.TokenUsage{
				InputTokens:     12000,
				OutputTokens:    3000,
				TotalTokens:     15000,
				CacheReadTokens: 8000,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeUsage(tt.raw)
			if got != tt.want {
				t.Errorf("normalizeUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSummarizeAssistant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		want    string
	}{
		{
			name:    "text and tool_use",
			fixture: "assistant_message.json",
			want:    "I'll analyze the codebase and implement the requested changes. [tool: Read]",
		},
		{
			name:    "tool_use and tool_result",
			fixture: "tool_use_message.json",
			want:    "[tool: Bash] [tool_result: ok]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := loadFixture(t, tt.fixture)
			ev, err := parseEvent(data)
			if err != nil {
				t.Fatal(err)
			}
			got := summarizeAssistant(ev)
			if got != tt.want {
				t.Errorf("summarizeAssistant() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSummarizeAssistant_EmptyMessage(t *testing.T) {
	t.Parallel()

	ev := rawEvent{Type: "assistant"}
	got := summarizeAssistant(ev)
	if got != "assistant message" {
		t.Errorf("summarizeAssistant(empty) = %q, want %q", got, "assistant message")
	}
}

func TestFormatAPIRetry(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "api_retry_event.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatal(err)
	}
	got := formatAPIRetry(ev)
	want := "API retry attempt 1/5 (delay 1000ms, status 429: rate_limit)"
	if got != want {
		t.Errorf("formatAPIRetry() = %q, want %q", got, want)
	}
}

func TestRawEventSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   rawEvent
		want string
	}{
		{
			name: "type only",
			ev:   rawEvent{Type: "stream_event"},
			want: "stream_event",
		},
		{
			name: "type and subtype",
			ev:   rawEvent{Type: "system", Subtype: "init"},
			want: "system/init",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.ev.summary()
			if got != tt.want {
				t.Errorf("summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"over limit", "hello world", 5, "hello…"},
		{"unicode safe", "日本語テスト", 3, "日本語…"},
		{"empty string", "", 5, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestParseFullSession(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "full_session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	wantTypes := []string{"system", "assistant", "assistant", "assistant", "result"}
	scanner := bufio.NewScanner(f)
	var i int
	for scanner.Scan() {
		ev, err := parseEvent(scanner.Bytes())
		if err != nil {
			t.Fatalf("line %d: parseEvent() error = %v", i+1, err)
		}
		if i >= len(wantTypes) {
			t.Fatalf("unexpected extra line %d: type=%q", i+1, ev.Type)
		}
		if ev.Type != wantTypes[i] {
			t.Errorf("line %d: Type = %q, want %q", i+1, ev.Type, wantTypes[i])
		}
		i++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(wantTypes) {
		t.Errorf("parsed %d lines, want %d", i, len(wantTypes))
	}
}

func TestContentBlocks_InvalidJSON(t *testing.T) {
	t.Parallel()

	ev := rawEvent{
		Type:    "assistant",
		Message: []byte(`not json`),
	}
	blocks := ev.contentBlocks()
	if blocks != nil {
		t.Errorf("contentBlocks(invalid) = %v, want nil", blocks)
	}
}

// TestRawAssistantMessageMeta_FromFixture verifies that model and
// per-request usage can be extracted from the assistant_message fixture.
func TestRawAssistantMessageMeta_FromFixture(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "assistant_message.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent: %v", err)
	}

	var meta rawAssistantMessageMeta
	if err := json.Unmarshal(ev.Message, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q", meta.Model, "claude-sonnet-4-20250514")
	}
	if meta.Usage == nil {
		t.Fatal("Usage is nil, want non-nil")
	}
	if meta.Usage.InputTokens != 12500 {
		t.Errorf("Usage.InputTokens = %d, want 12500", meta.Usage.InputTokens)
	}
	if meta.Usage.OutputTokens != 350 {
		t.Errorf("Usage.OutputTokens = %d, want 350", meta.Usage.OutputTokens)
	}
	if meta.Usage.CacheReadInputTokens != 8000 {
		t.Errorf("Usage.CacheReadInputTokens = %d, want 8000", meta.Usage.CacheReadInputTokens)
	}

	// normalizeUsage must map CacheReadInputTokens → CacheReadTokens.
	normalized := normalizeUsage(meta.Usage)
	if normalized.CacheReadTokens != 8000 {
		t.Errorf("normalizeUsage().CacheReadTokens = %d, want 8000", normalized.CacheReadTokens)
	}
}

// collectToolEvents delegates to [processToolBlocks] and collects the
// emitted [domain.AgentEvent] values. It mirrors the RunTurn call
// sites, using now for both the monotonic observed timestamp and the
// wall-clock event timestamp.
func collectToolEvents(t *testing.T, ev rawEvent, inFlight map[string]inFlightTool, now time.Time) []domain.AgentEvent {
	t.Helper()
	var events []domain.AgentEvent
	processToolBlocks(ev.contentBlocks(), inFlight, now, now, func(e domain.AgentEvent) {
		events = append(events, e)
	})
	return events
}

func TestContentBlock_ToolUseID(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "tool_use_message.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent() error = %v", err)
	}

	blocks := ev.contentBlocks()
	if len(blocks) != 2 {
		t.Fatalf("contentBlocks() = %d blocks, want 2", len(blocks))
	}

	// blocks[0]: tool_use
	if blocks[0].Type != "tool_use" {
		t.Errorf("blocks[0].Type = %q, want %q", blocks[0].Type, "tool_use")
	}
	if blocks[0].ID != "toolu_02B09q90qw90lq917835lhkm" {
		t.Errorf("blocks[0].ID = %q, want %q", blocks[0].ID, "toolu_02B09q90qw90lq917835lhkm")
	}
	if blocks[0].Name != "Bash" {
		t.Errorf("blocks[0].Name = %q, want %q", blocks[0].Name, "Bash")
	}

	// blocks[1]: tool_result with ToolUseID correlation
	if blocks[1].Type != "tool_result" {
		t.Errorf("blocks[1].Type = %q, want %q", blocks[1].Type, "tool_result")
	}
	if blocks[1].ToolUseID != "toolu_02B09q90qw90lq917835lhkm" {
		t.Errorf("blocks[1].ToolUseID = %q, want %q", blocks[1].ToolUseID, "toolu_02B09q90qw90lq917835lhkm")
	}
	if blocks[1].IsError {
		t.Error("blocks[1].IsError = true, want false")
	}
}

func TestEmitToolResult_SameMessage(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "tool_use_message.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent() error = %v", err)
	}

	inFlight := make(map[string]inFlightTool)
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, inFlight, now)

	if len(events) != 1 {
		t.Fatalf("collectToolEvents() = %d events, want 1", len(events))
	}

	got := events[0]
	if got.Type != domain.EventToolResult {
		t.Errorf("event.Type = %q, want %q", got.Type, domain.EventToolResult)
	}
	if got.ToolName != "Bash" {
		t.Errorf("event.ToolName = %q, want %q", got.ToolName, "Bash")
	}
	if got.ToolDurationMS != 0 {
		t.Errorf("event.ToolDurationMS = %d, want 0 (same-message pair)", got.ToolDurationMS)
	}
	if got.ToolError {
		t.Error("event.ToolError = true, want false")
	}
	if got.Message != "tool_result: Bash" {
		t.Errorf("event.Message = %q, want %q", got.Message, "tool_result: Bash")
	}
}

func TestEmitToolResult_CrossMessage(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "tool_use_result_separate.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	inFlight := make(map[string]inFlightTool)
	var toolEvents []domain.AgentEvent

	// Use deterministic synthetic timestamps so the test does not
	// depend on wall-clock timing or monotonic clock behavior.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgIndex := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, parseErr := parseEvent(scanner.Bytes())
		if parseErr != nil {
			t.Fatalf("parseEvent() error = %v", parseErr)
		}
		if ev.Type != "assistant" {
			continue
		}
		now := base.Add(time.Duration(msgIndex) * time.Second)
		msgIndex++
		toolEvents = append(toolEvents, collectToolEvents(t, ev, inFlight, now)...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if len(toolEvents) != 2 {
		t.Fatalf("total EventToolResult events = %d, want 2", len(toolEvents))
	}

	// First: Read tool result (tool_use at T+0s, tool_result at T+1s → 1000ms)
	if toolEvents[0].ToolName != "Read" {
		t.Errorf("toolEvents[0].ToolName = %q, want %q", toolEvents[0].ToolName, "Read")
	}
	if toolEvents[0].ToolError {
		t.Error("toolEvents[0].ToolError = true, want false")
	}
	if toolEvents[0].ToolDurationMS != 1000 {
		t.Errorf("toolEvents[0].ToolDurationMS = %d, want 1000", toolEvents[0].ToolDurationMS)
	}

	// Second: Bash tool result (tool_use at T+1s, tool_result at T+2s → 1000ms)
	if toolEvents[1].ToolName != "Bash" {
		t.Errorf("toolEvents[1].ToolName = %q, want %q", toolEvents[1].ToolName, "Bash")
	}
	if toolEvents[1].ToolError {
		t.Error("toolEvents[1].ToolError = true, want false")
	}
	if toolEvents[1].ToolDurationMS != 1000 {
		t.Errorf("toolEvents[1].ToolDurationMS = %d, want 1000", toolEvents[1].ToolDurationMS)
	}
}

func TestEmitToolResult_ErrorResult(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "tool_use_error_result.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent() error = %v", err)
	}

	// Empty in-flight map simulates orphaned tool_result.
	inFlight := make(map[string]inFlightTool)
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, inFlight, now)

	if len(events) != 1 {
		t.Fatalf("collectToolEvents() = %d events, want 1", len(events))
	}

	got := events[0]
	if got.ToolName != "unknown" {
		t.Errorf("event.ToolName = %q, want %q", got.ToolName, "unknown")
	}
	if !got.ToolError {
		t.Error("event.ToolError = false, want true")
	}
	if got.ToolDurationMS != 0 {
		t.Errorf("event.ToolDurationMS = %d, want 0 (orphaned result)", got.ToolDurationMS)
	}
}

func TestEmitToolResult_ParallelToolUse(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "tool_use_parallel.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent() error = %v", err)
	}

	inFlight := make(map[string]inFlightTool)
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, inFlight, now)

	// No tool_result blocks → no EventToolResult events.
	if len(events) != 0 {
		t.Errorf("collectToolEvents() = %d events, want 0", len(events))
	}

	// In-flight map should have 2 entries.
	if len(inFlight) != 2 {
		t.Fatalf("len(inFlight) = %d, want 2", len(inFlight))
	}

	entry1, ok := inFlight["toolu_par_01"]
	if !ok {
		t.Fatal("inFlight missing key \"toolu_par_01\"")
	}
	if entry1.Name != "Read" {
		t.Errorf("inFlight[\"toolu_par_01\"].Name = %q, want %q", entry1.Name, "Read")
	}

	entry2, ok := inFlight["toolu_par_02"]
	if !ok {
		t.Fatal("inFlight missing key \"toolu_par_02\"")
	}
	if entry2.Name != "Read" {
		t.Errorf("inFlight[\"toolu_par_02\"].Name = %q, want %q", entry2.Name, "Read")
	}
}

// TestEmitToolResult_UserEventCorrelation verifies that tool_result blocks
// in user-type events correlate correctly with tool_use blocks from
// preceding assistant events. This is a regression test for the bug where
// user-event tool_result blocks were silently dropped.
func TestEmitToolResult_UserEventCorrelation(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "tool_use_result_user_event.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	inFlight := make(map[string]inFlightTool)
	var toolEvents []domain.AgentEvent

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgIndex := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, parseErr := parseEvent(scanner.Bytes())
		if parseErr != nil {
			t.Fatalf("parseEvent() error = %v", parseErr)
		}
		// Process both assistant and user events to mirror RunTurn logic.
		switch ev.Type {
		case "assistant", "user":
			now := base.Add(time.Duration(msgIndex) * time.Second)
			msgIndex++
			toolEvents = append(toolEvents, collectToolEvents(t, ev, inFlight, now)...)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	// Exactly one tool_result expected (Read, from user event).
	if len(toolEvents) != 1 {
		t.Fatalf("total EventToolResult events = %d, want 1", len(toolEvents))
	}

	got := toolEvents[0]
	if got.ToolName != "Read" {
		t.Errorf("toolEvents[0].ToolName = %q, want %q", got.ToolName, "Read")
	}
	if got.ToolError {
		t.Error("toolEvents[0].ToolError = true, want false")
	}
	// tool_use registered at T+0s (assistant msg), tool_result at T+1s (user msg) → 1000ms.
	if got.ToolDurationMS != 1000 {
		t.Errorf("toolEvents[0].ToolDurationMS = %d, want 1000", got.ToolDurationMS)
	}
	if got.Message != "tool_result: Read" {
		t.Errorf("toolEvents[0].Message = %q, want %q", got.Message, "tool_result: Read")
	}

	// In-flight map should be empty after correlation.
	if len(inFlight) != 0 {
		t.Errorf("len(inFlight) = %d, want 0 (all tool_use blocks should be resolved)", len(inFlight))
	}
}
