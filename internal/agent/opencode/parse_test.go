package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadFixture reads testdata/<name> and returns its bytes.
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	return data
}

// loadFixtureLine returns the zero-based line at index from a fixture file.
func loadFixtureLine(t *testing.T, name string, index int) []byte {
	t.Helper()
	data := loadFixture(t, name)
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if index < 0 || index >= len(lines) {
		t.Fatalf("loadFixtureLine(%q, %d): file has %d lines", name, index, len(lines))
	}
	return lines[index]
}

func TestParseRunEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		fixture   string
		lineIdx   int
		wantType  string
		checkFunc func(t *testing.T, ev rawRunEvent)
	}{
		{
			name:     "step_start_line",
			fixture:  "simple_turn.jsonl",
			lineIdx:  0,
			wantType: "step_start",
			checkFunc: func(t *testing.T, ev rawRunEvent) {
				t.Helper()
				if len(ev.Part) == 0 {
					t.Error("Part is empty, want non-empty")
				}
				part, err := parseStepStartPart(ev.Part)
				if err != nil {
					t.Fatalf("parseStepStartPart() error = %v", err)
				}
				if part.ID == "" {
					t.Error("StepStartPart.ID is empty")
				}
			},
		},
		{
			name:     "text_line",
			fixture:  "simple_turn.jsonl",
			lineIdx:  1,
			wantType: "text",
			checkFunc: func(t *testing.T, ev rawRunEvent) {
				t.Helper()
				if len(ev.Part) == 0 {
					t.Error("Part is empty, want non-empty")
				}
				part, err := parseTextPart(ev.Part)
				if err != nil {
					t.Fatalf("parseTextPart() error = %v", err)
				}
				if part.Text == "" {
					t.Error("TextPart.Text is empty")
				}
			},
		},
		{
			name:     "step_finish_line",
			fixture:  "simple_turn.jsonl",
			lineIdx:  2,
			wantType: "step_finish",
			checkFunc: func(t *testing.T, ev rawRunEvent) {
				t.Helper()
				if len(ev.Part) == 0 {
					t.Error("Part is empty, want non-empty")
				}
				part, err := parseStepFinishPart(ev.Part)
				if err != nil {
					t.Fatalf("parseStepFinishPart() error = %v", err)
				}
				if part.Reason != "stop" {
					t.Errorf("StepFinishPart.Reason = %q, want %q", part.Reason, "stop")
				}
			},
		},
		{
			name:     "tool_use_line",
			fixture:  "tool_success.jsonl",
			lineIdx:  1,
			wantType: "tool_use",
			checkFunc: func(t *testing.T, ev rawRunEvent) {
				t.Helper()
				if len(ev.Part) == 0 {
					t.Error("Part is empty, want non-empty")
				}
				part, err := parseToolPart(ev.Part)
				if err != nil {
					t.Fatalf("parseToolPart() error = %v", err)
				}
				if part.Tool != "read" {
					t.Errorf("ToolPart.Tool = %q, want %q", part.Tool, "read")
				}
				if part.State.Status != "completed" {
					t.Errorf("ToolPart.State.Status = %q, want %q", part.State.Status, "completed")
				}
			},
		},
		{
			name:     "error_line",
			fixture:  "logical_failure_exit0.jsonl",
			lineIdx:  1,
			wantType: "error",
			checkFunc: func(t *testing.T, ev rawRunEvent) {
				t.Helper()
				if ev.Error == nil {
					t.Fatal("Error is nil, want non-nil")
				}
				if ev.Error.Name != "ProviderAuthError" {
					t.Errorf("Error.Name = %q, want %q", ev.Error.Name, "ProviderAuthError")
				}
				if ev.Error.Data == nil {
					t.Fatal("Error.Data is nil, want non-nil")
				}
				if msg, _ := ev.Error.Data["message"].(string); msg != "invalid api key" {
					t.Errorf("Error.Data[message] = %q, want %q", msg, "invalid api key")
				}
			},
		},
		{
			name:     "unknown_type_no_error",
			fixture:  "malformed_event.jsonl",
			lineIdx:  1,
			wantType: "unknown_future_type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line := loadFixtureLine(t, tt.fixture, tt.lineIdx)
			ev, err := parseRunEvent(line)
			if err != nil {
				t.Fatalf("parseRunEvent() error = %v", err)
			}
			if ev.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tt.wantType)
			}
			if ev.SessionID == "" && tt.fixture != "malformed_event.jsonl" {
				t.Errorf("SessionID is empty")
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, ev)
			}
		})
	}
}

func TestParseRunEvent_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := parseRunEvent([]byte("not valid json"))
	if err == nil {
		t.Fatal("parseRunEvent(invalid) error = nil, want error")
	}
}

func TestScanLines(t *testing.T) {
	t.Parallel()

	data := loadFixture(t, "permission_warning_then_error.txt")
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("fixture has %d lines, want >= 2", len(lines))
	}

	t.Run("plain_text_line_fails_json_parse", func(t *testing.T) {
		t.Parallel()

		_, err := parseRunEvent(lines[0])
		if err == nil {
			t.Fatal("parseRunEvent(plain text) error = nil, want error")
		}
		text := string(lines[0])
		if !strings.HasPrefix(text, "! permission requested:") {
			t.Errorf("plain text = %q, want prefix %q", text, "! permission requested:")
		}
	})

	t.Run("json_line_parsed_as_tool_use", func(t *testing.T) {
		t.Parallel()

		ev, err := parseRunEvent(lines[1])
		if err != nil {
			t.Fatalf("parseRunEvent(json line) error = %v", err)
		}
		if ev.Type != "tool_use" {
			t.Errorf("Type = %q, want %q", ev.Type, "tool_use")
		}
		part, err := parseToolPart(ev.Part)
		if err != nil {
			t.Fatalf("parseToolPart() error = %v", err)
		}
		if part.State.Status != "error" {
			t.Errorf("State.Status = %q, want %q", part.State.Status, "error")
		}
	})
}

func TestQueryExportUsage(t *testing.T) {
	t.Parallel()

	t.Run("parse_usage_extracted", func(t *testing.T) {
		t.Parallel()

		data := loadFixture(t, "export_usage.json")
		usage := parseExportOutput(data, "ses_abc123")

		if usage.InputTokens != 1500 {
			t.Errorf("InputTokens = %d, want 1500", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.TotalTokens != 1800 {
			t.Errorf("TotalTokens = %d, want 1800", usage.TotalTokens)
		}
		if usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.CacheReadTokens)
		}
		if usage.Model != "anthropic/claude-sonnet-4-5" {
			t.Errorf("Model = %q, want %q", usage.Model, "anthropic/claude-sonnet-4-5")
		}
	})

	t.Run("parse_missing_tokens_returns_zero", func(t *testing.T) {
		t.Parallel()

		data := loadFixture(t, "export_usage_missing_tokens.json")
		usage := parseExportOutput(data, "ses_abc123")

		if usage.InputTokens != 0 {
			t.Errorf("InputTokens = %d, want 0", usage.InputTokens)
		}
		if usage.OutputTokens != 0 {
			t.Errorf("OutputTokens = %d, want 0", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 0 {
			t.Errorf("CacheReadTokens = %d, want 0", usage.CacheReadTokens)
		}
	})

	t.Run("parse_session_id_mismatch_returns_zero", func(t *testing.T) {
		t.Parallel()

		data := loadFixture(t, "export_usage.json")
		usage := parseExportOutput(data, "ses_different_session")

		if usage.InputTokens != 0 {
			t.Errorf("InputTokens = %d, want 0 for mismatched session", usage.InputTokens)
		}
	})

	t.Run("parse_invalid_json_returns_zero", func(t *testing.T) {
		t.Parallel()

		usage := parseExportOutput([]byte("not valid json"), "ses_abc123")
		if usage.InputTokens != 0 || usage.OutputTokens != 0 {
			t.Errorf("invalid JSON should return zero usage, got InputTokens=%d OutputTokens=%d",
				usage.InputTokens, usage.OutputTokens)
		}
	})

	t.Run("parse_empty_messages_returns_zero", func(t *testing.T) {
		t.Parallel()

		usage := parseExportOutput([]byte(`{"messages":[]}`), "ses_abc123")
		if usage.InputTokens != 0 {
			t.Errorf("empty messages should return zero usage, got InputTokens=%d", usage.InputTokens)
		}
	})

	t.Run("parse_user_message_skipped", func(t *testing.T) {
		t.Parallel()

		// Only user message in the array; should return zero usage.
		data := []byte(`{"messages":[{"info":{"role":"user","sessionID":"ses_abc123","tokens":{"input":100,"output":50}}}]}`)
		usage := parseExportOutput(data, "ses_abc123")
		if usage.InputTokens != 0 {
			t.Errorf("user message should be skipped, got InputTokens=%d", usage.InputTokens)
		}
	})
}
