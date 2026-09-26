package opencode

import (
	"bytes"
	"os"
	"path/filepath"
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
		if !isPermissionWarning(text) {
			t.Errorf("isPermissionWarning(%q) = false, want true", text)
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

	t.Run("a_genuinely_zero_export_is_recovered_not_discarded", func(t *testing.T) {
		t.Parallel()

		// The runtime's assistant message carries `tokens` as a required
		// object, so a turn that cost nothing exports the same shape as
		// any other. Reading the VALUES to decide whether anything was
		// recovered turned that known zero into an unknown spend, and
		// threw away the model the export named along with it.
		data := []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123",` +
			`"providerID":"anthropic","modelID":"claude-sonnet-4-5","finish":"stop",` +
			`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`)
		usage := parseExportOutput(data, "ses_abc123", 0)

		if !usage.Recovered {
			t.Error("Recovered = false, want true for an export that reported zero tokens")
		}
		if !hasUsage(usage) {
			t.Error("hasUsage = false; a zero figure is a figure")
		}
		if usage.Model != "anthropic/claude-sonnet-4-5" {
			t.Errorf("Model = %q, want the model the export named", usage.Model)
		}
		if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 {
			t.Errorf("a zero export must stay zero, got in=%d out=%d total=%d",
				usage.InputTokens, usage.OutputTokens, usage.TotalTokens)
		}
	})

	t.Run("nothing_kept_is_not_recovered", func(t *testing.T) {
		t.Parallel()

		// The accept control for the cell above: "no figure" must stay
		// distinguishable from "a figure that happens to be zero", or the
		// presence test is just a way of always saying yes.
		for name, data := range map[string][]byte{
			"empty_messages": []byte(`{"messages":[]}`),
			"invalid_json":   []byte("not valid json"),
			"user_message":   []byte(`{"messages":[{"info":{"role":"user","sessionID":"ses_abc123","tokens":{"input":100,"output":50}}}]}`),
			"missing_tokens": []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","finish":"stop"}}]}`),
			"other_session":  []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_other","finish":"stop","tokens":{"input":1,"output":1}}}]}`),
			"unparseable_in": []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","finish":"stop","tokens":{"input":"x","output":1}}}]}`),
			"unfinished_step": []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123",` +
				`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`),
		} {
			usage := parseExportOutput(data, "ses_abc123", 0)
			if usage.Recovered {
				t.Errorf("%s: Recovered = true, want false", name)
			}
			if hasUsage(usage) {
				t.Errorf("%s: hasUsage = true, want false", name)
			}
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

		data := []byte(`{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","finish":"stop"}}]}`)
		usage := parseExportOutput(data, "ses_abc123", 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value (no tokens object)", usage)
		}
	})
}

// TestParseSessionExport drives the 2.x export document fixture,
// mirroring parseExportOutput's 1.x semantics: input sums tokens.input
// plus both cache figures, output sums tokens.output plus reasoning,
// and a sinceUnixMS window keeps only messages created at or after it.
func TestParseSessionExport(t *testing.T) {
	t.Parallel()

	const sessionID = "ses_f23828cc8ffeXAmvklUEWnLCuA"
	data := loadFixture(t, "export_usage_v2.json")

	t.Run("sinceUnixMS zero sums every kept message", func(t *testing.T) {
		t.Parallel()

		usage := parseSessionExport(data, sessionID, 0)

		if usage.InputTokens != 13058 {
			t.Errorf("InputTokens = %d, want 13058", usage.InputTokens)
		}
		if usage.OutputTokens != 35 {
			t.Errorf("OutputTokens = %d, want 35", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 7010 {
			t.Errorf("CacheReadTokens = %d, want 7010", usage.CacheReadTokens)
		}
		if usage.TotalTokens != 13093 {
			t.Errorf("TotalTokens = %d, want 13093", usage.TotalTokens)
		}
		if usage.Model != "opencode/big-pickle" {
			t.Errorf("Model = %q, want %q", usage.Model, "opencode/big-pickle")
		}
		if usage.Cost != 0 {
			t.Errorf("Cost = %v, want 0", usage.Cost)
		}
		if !usage.Recovered {
			t.Error("Recovered = false, want true")
		}
	})

	t.Run("a window keeps only messages created at or after it", func(t *testing.T) {
		t.Parallel()

		usage := parseSessionExport(data, sessionID, 1790405606000)

		if usage.InputTokens != 6560 {
			t.Errorf("InputTokens = %d, want 6560", usage.InputTokens)
		}
		if usage.OutputTokens != 8 {
			t.Errorf("OutputTokens = %d, want 8", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 6523 {
			t.Errorf("CacheReadTokens = %d, want 6523", usage.CacheReadTokens)
		}
	})

	t.Run("a session id mismatch returns zero", func(t *testing.T) {
		t.Parallel()

		usage := parseSessionExport(data, "ses_different_session", 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value for a mismatched session", usage)
		}
	})
}

// loadFreeTierRunError decodes the error member of fixture's one line
// into a *rawRunError.
func loadFreeTierRunError(t *testing.T, fixture string) *rawRunError {
	t.Helper()
	ev, err := parseRunEvent(loadFixtureLine(t, fixture, 0))
	if err != nil {
		t.Fatalf("parseRunEvent(%s): %v", fixture, err)
	}
	if ev.Error == nil {
		t.Fatalf("%s: Error is nil", fixture)
	}
	return ev.Error
}

// TestFreeTierRefusalClause drives freeTierRefusalClause directly
// against both envelope shapes, proving the denied-tool clause is
// drawn only when the envelope matches a free-tier refusal and the
// session's own tool policy denies bash, read, or both.
func TestFreeTierRefusalClause(t *testing.T) {
	t.Parallel()

	oneX := loadFreeTierRunError(t, "free_tier_refusal.jsonl")
	twoX := loadFreeTierRunError(t, "free_tier_refusal_v2.jsonl")

	const wantMessage = "Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"
	const clauseBash = "; the opencode.allowed_tools and opencode.denied_tools settings deny bash, a tool the runtime's free tier requires"
	const clauseRead = "; the opencode.allowed_tools and opencode.denied_tools settings deny read, a tool the runtime's free tier requires"
	const clauseBoth = "; the opencode.allowed_tools and opencode.denied_tools settings deny bash and read, tools the runtime's free tier requires"

	tests := []struct {
		name   string
		runErr *rawRunError
		pt     passthroughConfig
		want   string
	}{
		{name: "1.x denies bash", runErr: oneX, pt: passthroughConfig{AllowedTools: []string{"read", "glob"}}, want: clauseBash},
		{name: "1.x denies bash and read", runErr: oneX, pt: passthroughConfig{AllowedTools: []string{"glob"}}, want: clauseBoth},
		{name: "1.x denies read", runErr: oneX, pt: passthroughConfig{AllowedTools: []string{"bash", "glob"}}, want: clauseRead},
		{name: "1.x denied_tools names bash", runErr: oneX, pt: passthroughConfig{DeniedTools: []string{"bash"}}, want: clauseBash},
		{name: "1.x allows both required tools", runErr: oneX, pt: passthroughConfig{AllowedTools: []string{"bash", "read"}}, want: ""},
		{name: "1.x no tool lists configured", runErr: oneX, pt: passthroughConfig{}, want: ""},
		{name: "1.x wildcard deny is not an exact match", runErr: oneX, pt: passthroughConfig{DeniedTools: []string{"*"}}, want: ""},
		{
			name:   "1.x a different gateway error type is not a refusal",
			runErr: &rawRunError{Name: "APIError", Data: map[string]any{"responseBody": `{"type":"error","error":{"type":"RegionError","message":"unavailable in your region"}}`}},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "1.x without a responseBody is not a refusal",
			runErr: &rawRunError{Name: "APIError", Data: map[string]any{"message": wantMessage}},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "1.x with a non-JSON responseBody is not a refusal",
			runErr: &rawRunError{Name: "APIError", Data: map[string]any{"responseBody": "not json"}},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{name: "2.x denies bash", runErr: twoX, pt: passthroughConfig{AllowedTools: []string{"read", "glob"}}, want: clauseBash},
		{name: "2.x denied_tools names read", runErr: twoX, pt: passthroughConfig{DeniedTools: []string{"read"}}, want: clauseRead},
		{name: "2.x denied_tools names an unmapped alias", runErr: twoX, pt: passthroughConfig{DeniedTools: []string{"shell"}}, want: ""},
		{
			name:   "2.x status 401 is not a refusal",
			runErr: &rawRunError{Type: freeTierAuthType, Status: float64(401), Message: wantMessage},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "2.x type provider.quota is not a refusal",
			runErr: &rawRunError{Type: "provider.quota", Status: float64(403), Message: wantMessage},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "2.x message without the marker is not a refusal",
			runErr: &rawRunError{Type: freeTierAuthType, Status: float64(403), Message: "Error from provider (Console): rate limited"},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := freeTierRefusalClause(tt.runErr, tt.pt); got != tt.want {
				t.Errorf("freeTierRefusalClause() = %q, want %q", got, tt.want)
			}
		})
	}
}
