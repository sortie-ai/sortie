package copilot

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// scanFixtureLines reads non-empty lines from a testdata/ fixture file.
func scanFixtureLines(t *testing.T, name string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("scanFixtureLines(%q): %v", name, err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("closing fixture %q: %v", name, err)
		}
	})

	var lines [][]byte
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		b := scanner.Bytes()
		if len(b) == 0 {
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		lines = append(lines, cp)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning fixture %q: %v", name, err)
	}
	return lines
}

func TestParseEvent(t *testing.T) {
	t.Parallel()

	exitZero := 0
	exitOne := 1

	tests := []struct {
		name     string
		line     string
		wantErr  bool
		wantType string
		check    func(t *testing.T, ev rawEvent)
	}{
		{
			name:     "result event with sessionId and exit code 0",
			line:     `{"type":"result","timestamp":"2026-01-01T00:00:00Z","sessionId":"aaa-bbb-ccc","exitCode":0,"usage":{"premiumRequests":5,"totalApiDurationMs":1000,"sessionDurationMs":2000}}`,
			wantType: "result",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if ev.SessionID != "aaa-bbb-ccc" {
					t.Errorf("SessionID = %q, want %q", ev.SessionID, "aaa-bbb-ccc")
				}
				if ev.ExitCode == nil {
					t.Fatal("ExitCode is nil")
				}
				if *ev.ExitCode != exitZero {
					t.Errorf("ExitCode = %d, want 0", *ev.ExitCode)
				}
				if ev.Usage == nil {
					t.Fatal("Usage is nil")
				}
				if ev.Usage.PremiumRequests != 5 {
					t.Errorf("Usage.PremiumRequests = %d, want 5", ev.Usage.PremiumRequests)
				}
				if ev.Usage.TotalAPIDurMS != 1000 {
					t.Errorf("Usage.TotalAPIDurMS = %d, want 1000", ev.Usage.TotalAPIDurMS)
				}
				if ev.Usage.SessionDurMS != 2000 {
					t.Errorf("Usage.SessionDurMS = %d, want 2000", ev.Usage.SessionDurMS)
				}
			},
		},
		{
			name:     "result event with non-zero exit code",
			line:     `{"type":"result","timestamp":"2026-01-01T00:00:00Z","sessionId":"x","exitCode":1}`,
			wantType: "result",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if ev.ExitCode == nil {
					t.Fatal("ExitCode is nil")
				}
				if *ev.ExitCode != exitOne {
					t.Errorf("ExitCode = %d, want 1", *ev.ExitCode)
				}
			},
		},
		{
			name:     "assistant.message with outputTokens and content",
			line:     `{"type":"assistant.message","id":"e1","timestamp":"2026-01-01T00:00:00Z","data":{"messageId":"m1","content":"hello","toolRequests":[],"outputTokens":42}}`,
			wantType: "assistant.message",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseAssistantMessageData(ev.Data)
				if err != nil {
					t.Fatalf("parseAssistantMessageData: %v", err)
				}
				if data.OutputTokens != 42 {
					t.Errorf("OutputTokens = %d, want 42", data.OutputTokens)
				}
				if data.Content != "hello" {
					t.Errorf("Content = %q, want %q", data.Content, "hello")
				}
				if data.MessageID != "m1" {
					t.Errorf("MessageID = %q, want %q", data.MessageID, "m1")
				}
			},
		},
		{
			name:     "assistant.message with tool requests",
			line:     `{"type":"assistant.message","id":"e9","timestamp":"2026-01-01T00:00:00Z","data":{"messageId":"m2","content":"","toolRequests":[{"toolCallId":"call-x","name":"view","arguments":{},"intentionSummary":"view main.go"}],"outputTokens":10}}`,
			wantType: "assistant.message",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseAssistantMessageData(ev.Data)
				if err != nil {
					t.Fatalf("parseAssistantMessageData: %v", err)
				}
				if len(data.ToolRequests) != 1 {
					t.Fatalf("len(ToolRequests) = %d, want 1", len(data.ToolRequests))
				}
				if data.ToolRequests[0].Name != "view" {
					t.Errorf("ToolRequests[0].Name = %q, want %q", data.ToolRequests[0].Name, "view")
				}
				if data.ToolRequests[0].ToolCallID != "call-x" {
					t.Errorf("ToolRequests[0].ToolCallID = %q, want %q", data.ToolRequests[0].ToolCallID, "call-x")
				}
			},
		},
		{
			name:     "tool.execution_start with toolName and toolCallId",
			line:     `{"type":"tool.execution_start","id":"e2","timestamp":"2026-01-01T00:00:00Z","data":{"toolCallId":"call-1","toolName":"view","arguments":{"path":"/tmp/x"}}}`,
			wantType: "tool.execution_start",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseToolExecutionData(ev.Data)
				if err != nil {
					t.Fatalf("parseToolExecutionData: %v", err)
				}
				if data.ToolName != "view" {
					t.Errorf("ToolName = %q, want %q", data.ToolName, "view")
				}
				if data.ToolCallID != "call-1" {
					t.Errorf("ToolCallID = %q, want %q", data.ToolCallID, "call-1")
				}
			},
		},
		{
			name:     "tool.execution_complete with success true",
			line:     `{"type":"tool.execution_complete","id":"e3","timestamp":"2026-01-01T00:00:00Z","data":{"toolCallId":"call-1","model":"claude-opus-4.6","interactionId":"i1","success":true}}`,
			wantType: "tool.execution_complete",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseToolExecutionData(ev.Data)
				if err != nil {
					t.Fatalf("parseToolExecutionData: %v", err)
				}
				if !data.Success {
					t.Error("Success = false, want true")
				}
				if data.ToolCallID != "call-1" {
					t.Errorf("ToolCallID = %q, want %q", data.ToolCallID, "call-1")
				}
			},
		},
		{
			name:     "tool.execution_complete with success false",
			line:     `{"type":"tool.execution_complete","id":"e10","timestamp":"2026-01-01T00:00:00Z","data":{"toolCallId":"call-2","success":false}}`,
			wantType: "tool.execution_complete",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseToolExecutionData(ev.Data)
				if err != nil {
					t.Fatalf("parseToolExecutionData: %v", err)
				}
				if data.Success {
					t.Error("Success = true, want false")
				}
			},
		},
		{
			name:     "session.warning with message and ephemeral flag",
			line:     `{"type":"session.warning","id":"e4","timestamp":"2026-01-01T00:00:00Z","ephemeral":true,"data":{"warningType":"rate_limit","message":"rate limited"}}`,
			wantType: "session.warning",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if !ev.Ephemeral {
					t.Error("Ephemeral = false, want true")
				}
				data, err := parseSessionWarningData(ev.Data)
				if err != nil {
					t.Fatalf("parseSessionWarningData: %v", err)
				}
				if data.Message != "rate limited" {
					t.Errorf("Message = %q, want %q", data.Message, "rate limited")
				}
				if data.WarningType != "rate_limit" {
					t.Errorf("WarningType = %q, want %q", data.WarningType, "rate_limit")
				}
			},
		},
		{
			name:     "session.info with message",
			line:     `{"type":"session.info","id":"e5","timestamp":"2026-01-01T00:00:00Z","ephemeral":true,"data":{"infoType":"autopilot_continue","message":"continuing step 2"}}`,
			wantType: "session.info",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseSessionInfoData(ev.Data)
				if err != nil {
					t.Fatalf("parseSessionInfoData: %v", err)
				}
				if data.Message != "continuing step 2" {
					t.Errorf("Message = %q, want %q", data.Message, "continuing step 2")
				}
				if data.InfoType != "autopilot_continue" {
					t.Errorf("InfoType = %q, want %q", data.InfoType, "autopilot_continue")
				}
			},
		},
		{
			name:     "session.task_complete with summary and success",
			line:     `{"type":"session.task_complete","id":"e6","timestamp":"2026-01-01T00:00:00Z","data":{"summary":"task done","success":true}}`,
			wantType: "session.task_complete",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				data, err := parseSessionTaskCompleteData(ev.Data)
				if err != nil {
					t.Fatalf("parseSessionTaskCompleteData: %v", err)
				}
				if data.Summary != "task done" {
					t.Errorf("Summary = %q, want %q", data.Summary, "task done")
				}
				if !data.Success {
					t.Error("Success = false, want true")
				}
			},
		},
		{
			name:     "assistant.message_delta is ephemeral",
			line:     `{"type":"assistant.message_delta","id":"e7","timestamp":"2026-01-01T00:00:00Z","ephemeral":true,"data":{"messageId":"m1","deltaContent":"hel"}}`,
			wantType: "assistant.message_delta",
			check: func(t *testing.T, ev rawEvent) {
				t.Helper()
				if !ev.Ephemeral {
					t.Error("Ephemeral = false, want true")
				}
			},
		},
		{
			name:     "unknown future event type is preserved",
			line:     `{"type":"future.event.unknown","id":"e8","timestamp":"2026-01-01T00:00:00Z","data":{"foo":"bar"}}`,
			wantType: "future.event.unknown",
		},
		{
			name:     "empty object parses without error",
			line:     `{}`,
			wantType: "",
		},
		{
			name:    "malformed JSON returns error",
			line:    "this is not json",
			wantErr: true,
		},
		{
			name:    "partial JSON returns error",
			line:    `{"type":"result"`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseEvent([]byte(tt.line))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEvent(%q) = no error, want error", tt.line)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEvent(%q) unexpected error: %v", tt.line, err)
			}
			if got.Type != tt.wantType {
				t.Errorf("parseEvent(%q).Type = %q, want %q", tt.line, got.Type, tt.wantType)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestParseFixture_SimpleSession(t *testing.T) {
	t.Parallel()

	lines := scanFixtureLines(t, "simple_session.jsonl")
	if len(lines) != 8 {
		t.Fatalf("simple_session.jsonl: got %d lines, want 8", len(lines))
	}

	wantTypes := []string{
		"session.mcp_servers_loaded",
		"session.tools_updated",
		"user.message",
		"assistant.turn_start",
		"assistant.message_delta",
		"assistant.message",
		"assistant.turn_end",
		"result",
	}

	events := make([]rawEvent, len(lines))
	for i, line := range lines {
		ev, err := parseEvent(line)
		if err != nil {
			t.Fatalf("parseEvent(line %d): %v", i+1, err)
		}
		events[i] = ev
	}

	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}

	// Verify the assistant.message event carries accumulated output tokens.
	msgData, err := parseAssistantMessageData(events[5].Data)
	if err != nil {
		t.Fatalf("parseAssistantMessageData(events[5]): %v", err)
	}
	if msgData.OutputTokens != 6 {
		t.Errorf("assistant.message.outputTokens = %d, want 6", msgData.OutputTokens)
	}

	// Verify the result event carries the session ID and exit code.
	result := events[7]
	const wantSessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	if result.SessionID != wantSessionID {
		t.Errorf("result.SessionID = %q, want %q", result.SessionID, wantSessionID)
	}
	if result.ExitCode == nil {
		t.Fatal("result.ExitCode is nil")
	}
	if *result.ExitCode != 0 {
		t.Errorf("result.ExitCode = %d, want 0", *result.ExitCode)
	}
	if result.Usage == nil {
		t.Fatal("result.Usage is nil")
	}
	if result.Usage.PremiumRequests != 6 {
		t.Errorf("result.Usage.PremiumRequests = %d, want 6", result.Usage.PremiumRequests)
	}
	if result.Usage.TotalAPIDurMS != 6866 {
		t.Errorf("result.Usage.TotalAPIDurMS = %d, want 6866", result.Usage.TotalAPIDurMS)
	}
}

func TestParseFixture_ToolUseSession(t *testing.T) {
	t.Parallel()

	lines := scanFixtureLines(t, "tool_use_session.jsonl")
	if len(lines) != 9 {
		t.Fatalf("tool_use_session.jsonl: got %d lines, want 9", len(lines))
	}

	wantTypes := []string{
		"user.message",
		"assistant.turn_start",
		"assistant.message", // contains toolRequests
		"tool.execution_start",
		"tool.execution_complete",
		"assistant.message", // contains content
		"session.task_complete",
		"assistant.turn_end",
		"result",
	}

	events := make([]rawEvent, len(lines))
	for i, line := range lines {
		ev, err := parseEvent(line)
		if err != nil {
			t.Fatalf("parseEvent(line %d): %v", i+1, err)
		}
		events[i] = ev
	}

	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}

	// Verify tool execution start carries tool name and call ID.
	startData, err := parseToolExecutionData(events[3].Data)
	if err != nil {
		t.Fatalf("parseToolExecutionData(events[3]): %v", err)
	}
	if startData.ToolName != "view" {
		t.Errorf("tool.execution_start.toolName = %q, want %q", startData.ToolName, "view")
	}
	if startData.ToolCallID != "toolu_vrtx_001" {
		t.Errorf("tool.execution_start.toolCallId = %q, want %q", startData.ToolCallID, "toolu_vrtx_001")
	}

	// Verify tool execution complete carries call ID and success flag.
	completeData, err := parseToolExecutionData(events[4].Data)
	if err != nil {
		t.Fatalf("parseToolExecutionData(events[4]): %v", err)
	}
	if completeData.ToolCallID != "toolu_vrtx_001" {
		t.Errorf("tool.execution_complete.toolCallId = %q, want %q", completeData.ToolCallID, "toolu_vrtx_001")
	}
	if !completeData.Success {
		t.Error("tool.execution_complete.success = false, want true")
	}

	// Verify the first assistant.message has a tool request with no content.
	firstMsgData, err := parseAssistantMessageData(events[2].Data)
	if err != nil {
		t.Fatalf("parseAssistantMessageData(events[2]): %v", err)
	}
	if len(firstMsgData.ToolRequests) != 1 {
		t.Fatalf("first assistant.message: len(ToolRequests) = %d, want 1", len(firstMsgData.ToolRequests))
	}
	if firstMsgData.ToolRequests[0].Name != "view" {
		t.Errorf("ToolRequests[0].Name = %q, want %q", firstMsgData.ToolRequests[0].Name, "view")
	}

	// Verify the session.task_complete carries summary.
	taskData, err := parseSessionTaskCompleteData(events[6].Data)
	if err != nil {
		t.Fatalf("parseSessionTaskCompleteData(events[6]): %v", err)
	}
	if taskData.Summary != "Read and summarized main.go" {
		t.Errorf("session.task_complete.summary = %q, want %q", taskData.Summary, "Read and summarized main.go")
	}
	if !taskData.Success {
		t.Error("session.task_complete.success = false, want true")
	}

	// Verify the result event carries the session ID.
	result := events[8]
	const wantSessionID = "bb889fb1-7fbc-5dea-c98f-22e7e44ebc50"
	if result.SessionID != wantSessionID {
		t.Errorf("result.SessionID = %q, want %q", result.SessionID, wantSessionID)
	}
	if result.ExitCode == nil {
		t.Fatal("result.ExitCode is nil")
	}
	if *result.ExitCode != 0 {
		t.Errorf("result.ExitCode = %d, want 0", *result.ExitCode)
	}
}

func TestParseFixture_AuthFailure(t *testing.T) {
	t.Parallel()

	lines := scanFixtureLines(t, "auth_failure.jsonl")
	if len(lines) != 1 {
		t.Fatalf("auth_failure.jsonl: got %d lines, want 1", len(lines))
	}

	// The single line is plain text (error message from CLI), not JSON.
	_, err := parseEvent(lines[0])
	if err == nil {
		t.Fatalf("parseEvent(auth_failure line) = no error, want parse error for non-JSON content")
	}
}

func TestParseFixture_MalformedLines(t *testing.T) {
	t.Parallel()

	lines := scanFixtureLines(t, "malformed_lines.jsonl")
	if len(lines) != 4 {
		t.Fatalf("malformed_lines.jsonl: got %d lines, want 4", len(lines))
	}

	var parseErrors int
	var validEvents []rawEvent
	for _, line := range lines {
		ev, err := parseEvent(line)
		if err != nil {
			parseErrors++
			continue
		}
		validEvents = append(validEvents, ev)
	}

	// Exactly one line ("this is not json") should fail to parse.
	if parseErrors != 1 {
		t.Errorf("parse errors = %d, want 1", parseErrors)
	}
	// Three lines should parse successfully.
	if len(validEvents) != 3 {
		t.Errorf("valid events = %d, want 3", len(validEvents))
	}

	// One of the valid events should be an unknown future event type.
	var foundUnknown bool
	for _, ev := range validEvents {
		if ev.Type == "future.event.type" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Error("expected event with type \"future.event.type\" among valid events")
	}

	// The assistant.message event should be parseable.
	var foundAssistant bool
	for _, ev := range validEvents {
		if ev.Type == "assistant.message" {
			foundAssistant = true
			data, err := parseAssistantMessageData(ev.Data)
			if err != nil {
				t.Fatalf("parseAssistantMessageData: %v", err)
			}
			if data.Content != "hello after malformed" {
				t.Errorf("assistant.message.content = %q, want %q", data.Content, "hello after malformed")
			}
		}
	}
	if !foundAssistant {
		t.Error("expected assistant.message event among valid events")
	}
}

func TestParseFixture_ResumeSession(t *testing.T) {
	t.Parallel()

	lines := scanFixtureLines(t, "resume_session.jsonl")
	if len(lines) != 5 {
		t.Fatalf("resume_session.jsonl: got %d lines, want 5", len(lines))
	}

	wantTypes := []string{
		"user.message",
		"assistant.turn_start",
		"assistant.message",
		"assistant.turn_end",
		"result",
	}

	events := make([]rawEvent, len(lines))
	for i, line := range lines {
		ev, err := parseEvent(line)
		if err != nil {
			t.Fatalf("parseEvent(line %d): %v", i+1, err)
		}
		events[i] = ev
	}

	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}

	// resume_session.jsonl reuses the session ID from simple_session.jsonl,
	// confirming that --resume preserves the session identity across turns.
	result := events[4]
	const wantSessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	if result.SessionID != wantSessionID {
		t.Errorf("result.SessionID = %q, want %q (should match simple_session)", result.SessionID, wantSessionID)
	}
	if result.ExitCode == nil {
		t.Fatal("result.ExitCode is nil")
	}
	if *result.ExitCode != 0 {
		t.Errorf("result.ExitCode = %d, want 0", *result.ExitCode)
	}
}

func TestSummarizeAssistantMessage(t *testing.T) {
	t.Parallel()

	longContent := strings.Repeat("a", 201)
	exactContent := strings.Repeat("b", 200)

	tests := []struct {
		name string
		data assistantMessageData
		want string
	}{
		{
			name: "short content returned as-is",
			data: assistantMessageData{Content: "hello world"},
			want: "hello world",
		},
		{
			name: "leading and trailing whitespace is trimmed",
			data: assistantMessageData{Content: "\n\nhello world\n"},
			want: "hello world",
		},
		{
			name: "content at exactly 200 runes is not truncated",
			data: assistantMessageData{Content: exactContent},
			want: exactContent,
		},
		{
			name: "content at 201 runes is truncated with ellipsis",
			data: assistantMessageData{Content: longContent},
			want: strings.Repeat("a", 200) + "…",
		},
		{
			name: "empty content with single tool request",
			data: assistantMessageData{
				Content:      "",
				ToolRequests: []rawToolRequest{{Name: "view"}},
			},
			want: "requesting 1 tool(s): view",
		},
		{
			name: "empty content with multiple tool requests joined by comma",
			data: assistantMessageData{
				Content: "",
				ToolRequests: []rawToolRequest{
					{Name: "view"},
					{Name: "edit_file"},
				},
			},
			want: "requesting 2 tool(s): view, edit_file",
		},
		{
			name: "no content and no tool requests",
			data: assistantMessageData{},
			want: "assistant message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := summarizeAssistantMessage(tt.data)
			if got != tt.want {
				t.Errorf("summarizeAssistantMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"empty string", "", 10, ""},
		{"short string within limit", "hello", 10, "hello"},
		{"exact length not truncated", "hello", 5, "hello"},
		{"one over limit gets ellipsis", "hello!", 5, "hello…"},
		{"unicode two-byte runes counted by rune", "héllo", 4, "héll…"},
		{"multibyte CJK runes", "日本語テスト", 3, "日本語…"},
		{"single rune truncated to zero is just ellipsis", "ab", 1, "a…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := typeutil.TruncateRunes(tt.s, tt.maxLen)
			if got != tt.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", tt.s, tt.maxLen, got, tt.want)
			}
		})
	}
}
