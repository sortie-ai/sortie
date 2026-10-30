package opencode

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadFixture(%q): %v", name, err)
	}
	return data
}

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
				if ev.Error.Type != "provider.auth" {
					t.Errorf("Error.Type = %q, want %q", ev.Error.Type, "provider.auth")
				}
				if ev.Error.Message != "invalid api key" {
					t.Errorf("Error.Message = %q, want %q", ev.Error.Message, "invalid api key")
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

func TestFreeTierRefusalClause(t *testing.T) {
	t.Parallel()

	refusal := loadFreeTierRunError(t, "free_tier_refusal.jsonl")

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
		{name: "denies bash", runErr: refusal, pt: passthroughConfig{AllowedTools: []string{"read", "glob"}}, want: clauseBash},
		{name: "denies bash and read", runErr: refusal, pt: passthroughConfig{AllowedTools: []string{"glob"}}, want: clauseBoth},
		{name: "denies read", runErr: refusal, pt: passthroughConfig{AllowedTools: []string{"bash", "glob"}}, want: clauseRead},
		{name: "denied_tools names bash", runErr: refusal, pt: passthroughConfig{DeniedTools: []string{"bash"}}, want: clauseBash},
		{name: "allows both required tools", runErr: refusal, pt: passthroughConfig{AllowedTools: []string{"bash", "read"}}, want: ""},
		{name: "no tool lists configured", runErr: refusal, pt: passthroughConfig{}, want: ""},
		{name: "wildcard deny is not an exact match", runErr: refusal, pt: passthroughConfig{DeniedTools: []string{"*"}}, want: ""},
		{name: "denied_tools names read", runErr: refusal, pt: passthroughConfig{DeniedTools: []string{"read"}}, want: clauseRead},
		{name: "denied_tools names an unmapped alias", runErr: refusal, pt: passthroughConfig{DeniedTools: []string{"shell"}}, want: ""},
		{
			name:   "status 401 is not a refusal",
			runErr: &rawRunError{Type: freeTierAuthType, Status: float64(401), Message: wantMessage},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "type provider.quota is not a refusal",
			runErr: &rawRunError{Type: "provider.quota", Status: float64(403), Message: wantMessage},
			pt:     passthroughConfig{AllowedTools: []string{"read", "glob"}},
			want:   "",
		},
		{
			name:   "message without the marker is not a refusal",
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
