package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
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
			want:    "I'll analyze the codebase and implement the requested changes.",
		},
		{
			name:    "tool blocks without text",
			fixture: "tool_use_message.json",
			want:    "",
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
	if got != "" {
		t.Errorf("summarizeAssistant(empty) = %q, want blank", got)
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
}

// collectToolEvents delegates to [processToolBlocks] and collects the
// emitted [domain.AgentEvent] values. It mirrors the RunTurn call
// sites, using the same now value for both the observed timestamp and
// the event wall-clock timestamp in tests.
func collectToolEvents(t *testing.T, ev rawEvent, tracker *agentcore.ToolTracker, now time.Time) []domain.AgentEvent {
	t.Helper()
	work := agentcore.NewWorkObserver(agentcore.WorkSignals{ToolActivity: true})
	var events []domain.AgentEvent
	processToolBlocks(ev.contentBlocks(), tracker, work, now, func(e domain.AgentEvent) {
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

	if blocks[0].Type != "tool_use" {
		t.Errorf("blocks[0].Type = %q, want %q", blocks[0].Type, "tool_use")
	}
	if blocks[0].ID != "toolu_02B09q90qw90lq917835lhkm" {
		t.Errorf("blocks[0].ID = %q, want %q", blocks[0].ID, "toolu_02B09q90qw90lq917835lhkm")
	}
	if blocks[0].Name != "Bash" {
		t.Errorf("blocks[0].Name = %q, want %q", blocks[0].Name, "Bash")
	}

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

	tracker := agentcore.NewToolTracker()
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, tracker, now)

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
	if got.ToolDurationMS < 0 {
		t.Errorf("event.ToolDurationMS = %d, want >= 0 (same-message pair)", got.ToolDurationMS)
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

	tracker := agentcore.NewToolTracker()
	var toolEvents []domain.AgentEvent

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
		toolEvents = append(toolEvents, collectToolEvents(t, ev, tracker, now)...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if len(toolEvents) != 2 {
		t.Fatalf("total EventToolResult events = %d, want 2", len(toolEvents))
	}

	if toolEvents[0].ToolName != "Read" {
		t.Errorf("toolEvents[0].ToolName = %q, want %q", toolEvents[0].ToolName, "Read")
	}
	if toolEvents[0].ToolError {
		t.Error("toolEvents[0].ToolError = true, want false")
	}
	if toolEvents[0].ToolDurationMS < 0 {
		t.Errorf("toolEvents[0].ToolDurationMS = %d, want >= 0", toolEvents[0].ToolDurationMS)
	}

	if toolEvents[1].ToolName != "Bash" {
		t.Errorf("toolEvents[1].ToolName = %q, want %q", toolEvents[1].ToolName, "Bash")
	}
	if toolEvents[1].ToolError {
		t.Error("toolEvents[1].ToolError = true, want false")
	}
	if toolEvents[1].ToolDurationMS < 0 {
		t.Errorf("toolEvents[1].ToolDurationMS = %d, want >= 0", toolEvents[1].ToolDurationMS)
	}
}

func TestEmitToolResult_ErrorResult(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "tool_use_error_result.json")
	ev, err := parseEvent(data)
	if err != nil {
		t.Fatalf("parseEvent() error = %v", err)
	}

	tracker := agentcore.NewToolTracker()
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, tracker, now)

	if len(events) != 1 {
		t.Fatalf("collectToolEvents() = %d events, want 1", len(events))
	}

	got := events[0]
	if got.ToolName != "" {
		t.Errorf("event.ToolName = %q, want empty for an orphaned result", got.ToolName)
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

	tracker := agentcore.NewToolTracker()
	now := time.Now().UTC()
	events := collectToolEvents(t, ev, tracker, now)

	if len(events) != 0 {
		t.Errorf("collectToolEvents() = %d events, want 0", len(events))
	}

	call1, _, ok1 := tracker.End("toolu_par_01")
	if !ok1 {
		t.Fatal("tracker missing entry \"toolu_par_01\"")
	}
	if call1.Name != "Read" {
		t.Errorf("tracker[\"toolu_par_01\"].Name = %q, want %q", call1.Name, "Read")
	}

	call2, _, ok2 := tracker.End("toolu_par_02")
	if !ok2 {
		t.Fatal("tracker missing entry \"toolu_par_02\"")
	}
	if call2.Name != "Read" {
		t.Errorf("tracker[\"toolu_par_02\"].Name = %q, want %q", call2.Name, "Read")
	}
}

func TestEmitToolResult_UserEventCorrelation(t *testing.T) {
	t.Parallel()

	f, err := os.Open(filepath.Join("testdata", "tool_use_result_user_event.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	tracker := agentcore.NewToolTracker()
	var toolEvents []domain.AgentEvent

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msgIndex := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		ev, parseErr := parseEvent(scanner.Bytes())
		if parseErr != nil {
			t.Fatalf("parseEvent() error = %v", parseErr)
		}
		switch ev.Type {
		case "assistant", "user":
			now := base.Add(time.Duration(msgIndex) * time.Second)
			msgIndex++
			toolEvents = append(toolEvents, collectToolEvents(t, ev, tracker, now)...)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

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
	if got.ToolDurationMS < 0 {
		t.Errorf("toolEvents[0].ToolDurationMS = %d, want >= 0", got.ToolDurationMS)
	}
	if got.Message != "tool_result: Read" {
		t.Errorf("toolEvents[0].Message = %q, want %q", got.Message, "tool_result: Read")
	}

	_, _, stillTracked := tracker.End("toolu_u01")
	if stillTracked {
		t.Error("tracker still has entry \"toolu_u01\" after correlation")
	}
}
