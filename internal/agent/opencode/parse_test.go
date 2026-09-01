package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
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

// TestIsPermissionWarning_AbandonedMarkerIsNotAWarning pins that the stderr
// abandonment marker can never be mistaken for a permission-refusal
// warning. It references [procutil.AbandonedMarker] rather than a copy of
// its text, so a future change to the marker cannot leave this guard
// passing on stale text.
func TestIsPermissionWarning_AbandonedMarkerIsNotAWarning(t *testing.T) {
	t.Parallel()

	if isPermissionWarning(procutil.AbandonedMarker) {
		t.Errorf("isPermissionWarning(%q) = true, want false", procutil.AbandonedMarker)
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
		usage := parseExportOutput(data, "ses_abc123", 0)

		// InputTokens is tokens.input plus cache.read plus cache.write
		// (1500 + 200 + 50); OutputTokens is tokens.output plus
		// tokens.reasoning (300 + 0); TotalTokens is InputTokens plus
		// OutputTokens, not the vendor tokens.total field.
		if usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.TotalTokens != 2050 {
			t.Errorf("TotalTokens = %d, want 2050", usage.TotalTokens)
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
		usage := parseExportOutput(data, "ses_abc123", 0)

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
		usage := parseExportOutput(data, "ses_different_session", 0)

		if usage.InputTokens != 0 {
			t.Errorf("InputTokens = %d, want 0 for mismatched session", usage.InputTokens)
		}
	})

	t.Run("parse_invalid_json_returns_zero", func(t *testing.T) {
		t.Parallel()

		usage := parseExportOutput([]byte("not valid json"), "ses_abc123", 0)
		if usage.InputTokens != 0 || usage.OutputTokens != 0 {
			t.Errorf("invalid JSON should return zero usage, got InputTokens=%d OutputTokens=%d",
				usage.InputTokens, usage.OutputTokens)
		}
	})

	t.Run("parse_empty_messages_returns_zero", func(t *testing.T) {
		t.Parallel()

		usage := parseExportOutput([]byte(`{"messages":[]}`), "ses_abc123", 0)
		if usage.InputTokens != 0 {
			t.Errorf("empty messages should return zero usage, got InputTokens=%d", usage.InputTokens)
		}
	})

	t.Run("parse_user_message_skipped", func(t *testing.T) {
		t.Parallel()

		// Only user message in the array; should return zero usage.
		data := []byte(`{"messages":[{"info":{"role":"user","sessionID":"ses_abc123","tokens":{"input":100,"output":50}}}]}`)
		usage := parseExportOutput(data, "ses_abc123", 0)
		if usage.InputTokens != 0 {
			t.Errorf("user message should be skipped, got InputTokens=%d", usage.InputTokens)
		}
	})

	// parse_multi_message_sums_across_the_session drives export_usage_multi.json,
	// captured from opencode 1.17.1 (session ses_18c61ba15ffe1524eHja237B0R,
	// per-message vendor totals 16593, 16609, 16626), asserting the sum
	// across all three assistant messages rather than only the last one.
	t.Run("parse_multi_message_sums_across_the_session", func(t *testing.T) {
		t.Parallel()

		data := loadFixture(t, "export_usage_multi.json")
		usage := parseExportOutput(data, "ses_18c61ba15ffe1524eHja237B0R", 0)

		if usage.InputTokens != 49814 {
			t.Errorf("InputTokens = %d, want 49814", usage.InputTokens)
		}
		if usage.OutputTokens != 14 {
			t.Errorf("OutputTokens = %d, want 14", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 16586 {
			t.Errorf("CacheReadTokens = %d, want 16586", usage.CacheReadTokens)
		}
		if usage.TotalTokens != 49828 {
			t.Errorf("TotalTokens = %d, want 49828", usage.TotalTokens)
		}

		const vendorTotalSum = 16593 + 16609 + 16626
		if usage.TotalTokens != vendorTotalSum {
			t.Errorf("TotalTokens = %d, want %d (sum of the per-message vendor totals)", usage.TotalTokens, vendorTotalSum)
		}
	})

	t.Run("parse_multi_message_window_keeps_only_messages_at_or_after_sinceUnixMS", func(t *testing.T) {
		t.Parallel()

		data := loadFixture(t, "export_usage_multi.json")

		// The second message's own time.created (1780056268100): the
		// first message (created 1780056264932) falls out of the window,
		// leaving only the second and third.
		windowed := parseExportOutput(data, "ses_18c61ba15ffe1524eHja237B0R", 1780056268100)
		if windowed.InputTokens != 33225 {
			t.Errorf("windowed InputTokens = %d, want 33225 (second and third messages' input+cache.read+cache.write)", windowed.InputTokens)
		}
		if windowed.OutputTokens != 10 {
			t.Errorf("windowed OutputTokens = %d, want 10 (second and third messages only)", windowed.OutputTokens)
		}
		if windowed.CacheReadTokens != 11064 {
			t.Errorf("windowed CacheReadTokens = %d, want 11064", windowed.CacheReadTokens)
		}

		// sinceUnixMS zero counts all three messages.
		all := parseExportOutput(data, "ses_18c61ba15ffe1524eHja237B0R", 0)
		if all.OutputTokens != 14 {
			t.Errorf("unwindowed OutputTokens = %d, want 14 (all three messages)", all.OutputTokens)
		}
	})

	t.Run("parse_export_without_tokens_object_returns_zero", func(t *testing.T) {
		t.Parallel()

		data := []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5"}}]}`)
		usage := parseExportOutput(data, "ses_abc123", 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value (no tokens object)", usage)
		}
	})
}
