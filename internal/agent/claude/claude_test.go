package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestNewClaudeCodeAdapter(t *testing.T) {
	t.Parallel()

	t.Run("zero config", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewClaudeCodeAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("with config", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewClaudeCodeAdapter(map[string]any{
			"model":           "claude-sonnet-4-20250514",
			"max_turns":       float64(10),
			"max_budget_usd":  1.5,
			"permission_mode": "bypassPermissions",
		})
		if err != nil {
			t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
		}
		a := adapter.(*ClaudeCodeAdapter)
		if a.passthrough.Model != "claude-sonnet-4-20250514" {
			t.Errorf("Model = %q, want %q", a.passthrough.Model, "claude-sonnet-4-20250514")
		}
		if a.passthrough.MaxTurns != 10 {
			t.Errorf("MaxTurns = %d, want 10", a.passthrough.MaxTurns)
		}
		if a.passthrough.MaxBudgetUSD != 1.5 {
			t.Errorf("MaxBudgetUSD = %f, want 1.5", a.passthrough.MaxBudgetUSD)
		}
		if a.passthrough.PermissionMode != "bypassPermissions" {
			t.Errorf("PermissionMode = %q, want %q", a.passthrough.PermissionMode, "bypassPermissions")
		}
	})
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("claude-code")
	if err != nil {
		t.Fatalf("registry.Agents.Get() error = %v", err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*ClaudeCodeAdapter); !ok {
		t.Errorf("factory() type = %T, want *ClaudeCodeAdapter", adapter)
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

	// Use /bin/sh as a stand-in for the claude binary (always on PATH).
	existingCmd := "/bin/sh"

	tests := []struct {
		name    string
		setup   func(t *testing.T) domain.StartSessionParams
		wantErr domain.AgentErrorKind
	}{
		{
			name: "empty workspace path",
			setup: func(_ *testing.T) domain.StartSessionParams {
				return domain.StartSessionParams{}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "non-existent workspace",
			setup: func(_ *testing.T) domain.StartSessionParams {
				return domain.StartSessionParams{
					WorkspacePath: "/nonexistent/path/that/does/not/exist",
					AgentConfig:   domain.AgentConfig{Command: existingCmd},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "workspace is a file",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				tmpFile := filepath.Join(t.TempDir(), "afile")
				if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return domain.StartSessionParams{
					WorkspacePath: tmpFile,
					AgentConfig:   domain.AgentConfig{Command: existingCmd},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "command not found",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				return domain.StartSessionParams{
					WorkspacePath: t.TempDir(),
					AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-binary-12345"},
				}
			},
			wantErr: domain.ErrAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := NewClaudeCodeAdapter(map[string]any{})

			params := tt.setup(t)

			_, err := adapter.StartSession(context.Background(), params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != tt.wantErr {
				t.Errorf("Kind = %q, want %q", agentErr.Kind, tt.wantErr)
			}
		})
	}
}

func TestStartSession_NewSession(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	if session.ID == "" {
		t.Error("session ID is empty")
	}
	state := session.Internal.(*sessionState)
	if state.isContinuation {
		t.Error("isContinuation = true, want false")
	}

	// Verify UUID format.
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(session.ID) {
		t.Errorf("session ID %q does not match UUID v4 format", session.ID)
	}
}

func TestStartSession_ResumeSession(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	resumeID := "resume-1234-5678-abcd-ef0123456789"
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: "/bin/sh"},
		ResumeSessionID: resumeID,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	if session.ID != resumeID {
		t.Errorf("session ID = %q, want %q", session.ID, resumeID)
	}
	state := session.Internal.(*sessionState)
	if !state.isContinuation {
		t.Error("isContinuation = false, want true")
	}
}

func TestStartSession_DefaultCommand(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})

	// With empty command, defaults to "claude" which likely isn't on PATH in CI.
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{},
	})
	if err == nil {
		return // claude is on PATH -- that's fine too
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrAgentNotFound {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrAgentNotFound)
	}
}

func TestBuildArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		state      *sessionState
		prompt     string
		pt         passthroughConfig
		wantArgs   []string
		wantAbsent []string
	}{
		{
			name: "first turn new session",
			state: &sessionState{
				claudeSessionID: "sess-001",
				turnCount:       0,
				isContinuation:  false,
			},
			prompt: "fix the bug",
			pt:     passthroughConfig{SessionPersistence: true},
			wantArgs: []string{
				"-p", "fix the bug",
				"--output-format", "stream-json",
				"--verbose",
				"--dangerously-skip-permissions",
				"--session-id", "sess-001",
			},
			wantAbsent: []string{"--resume", "--no-session-persistence"},
		},
		{
			name: "continuation turn same session",
			state: &sessionState{
				claudeSessionID: "sess-001",
				turnCount:       1,
				isContinuation:  false,
			},
			prompt: "continue",
			pt:     passthroughConfig{SessionPersistence: true},
			wantArgs: []string{
				"-p", "continue",
				"--resume", "sess-001",
			},
			wantAbsent: []string{"--session-id"},
		},
		{
			name: "continuation session first turn",
			state: &sessionState{
				claudeSessionID: "resumed-id",
				turnCount:       0,
				isContinuation:  true,
			},
			prompt: "continue work",
			pt:     passthroughConfig{SessionPersistence: true},
			wantArgs: []string{
				"--resume", "resumed-id",
			},
			wantAbsent: []string{"--session-id"},
		},
		{
			name: "permission mode set",
			state: &sessionState{
				claudeSessionID: "sess-002",
			},
			prompt: "test",
			pt: passthroughConfig{
				PermissionMode:     "bypassPermissions",
				SessionPersistence: true,
			},
			wantArgs:   []string{"--permission-mode", "bypassPermissions"},
			wantAbsent: []string{"--dangerously-skip-permissions"},
		},
		{
			name: "all passthrough flags",
			state: &sessionState{
				claudeSessionID: "sess-003",
			},
			prompt: "work",
			pt: passthroughConfig{
				Model:              "claude-sonnet-4-20250514",
				FallbackModel:      "claude-haiku-3",
				MaxTurns:           5,
				MaxBudgetUSD:       2.5,
				Effort:             "high",
				AllowedTools:       "Bash Read Write",
				DisallowedTools:    "TodoRead",
				SystemPrompt:       "be careful",
				MCPConfig:          "/path/to/mcp.json",
				SessionPersistence: true,
			},
			wantArgs: []string{
				"--model", "claude-sonnet-4-20250514",
				"--fallback-model", "claude-haiku-3",
				"--max-turns", "5",
				"--max-budget-usd", "2.5",
				"--effort", "high",
				"--allowedTools", "Bash Read Write",
				"--disallowedTools", "TodoRead",
				"--append-system-prompt", "be careful",
				"--mcp-config", "/path/to/mcp.json",
			},
			wantAbsent: []string{"--no-session-persistence"},
		},
		{
			name: "no session persistence",
			state: &sessionState{
				claudeSessionID: "sess-004",
			},
			prompt: "ephemeral",
			pt: passthroughConfig{
				SessionPersistence: false,
			},
			wantArgs: []string{"--no-session-persistence"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildArgs(tt.state, tt.prompt, tt.pt)
			argStr := strings.Join(got, " ")

			for i := 0; i < len(tt.wantArgs); i++ {
				found := false
				for j := 0; j < len(got); j++ {
					if got[j] == tt.wantArgs[i] {
						// Check value arg too if there's a pair.
						if i+1 < len(tt.wantArgs) && j+1 < len(got) && got[j+1] == tt.wantArgs[i+1] {
							found = true
							i++ // skip value
							break
						}
						// Standalone flag.
						if i+1 >= len(tt.wantArgs) || strings.HasPrefix(tt.wantArgs[i+1], "--") || tt.wantArgs[i+1] == "-p" {
							found = true
							break
						}
					}
				}
				if !found {
					t.Errorf("missing expected arg %q in: %s", tt.wantArgs[i], argStr)
				}
			}

			for _, absent := range tt.wantAbsent {
				for _, a := range got {
					if a == absent {
						t.Errorf("unexpected arg %q in: %s", absent, argStr)
						break
					}
				}
			}
		})
	}
}

func TestBuildArgs_AlwaysPresent(t *testing.T) {
	t.Parallel()

	got := buildArgs(&sessionState{claudeSessionID: "x"}, "p", passthroughConfig{SessionPersistence: true})
	required := []string{"-p", "--output-format", "--verbose"}
	for _, r := range required {
		found := false
		for _, a := range got {
			if a == r {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required flag %q missing in %v", r, got)
		}
	}
}

func TestNewUUID(t *testing.T) {
	t.Parallel()

	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	for i := range 10 {
		t.Run(fmt.Sprintf("iter_%d", i), func(t *testing.T) {
			t.Parallel()
			id := newUUID()
			if !uuidRe.MatchString(id) {
				t.Errorf("newUUID() = %q does not match v4 UUID pattern", id)
			}
		})
	}

	// Uniqueness check.
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := newUUID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate UUID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	t.Run("full config", func(t *testing.T) {
		t.Parallel()
		cfg := parsePassthroughConfig(map[string]any{
			"permission_mode":     "dontAsk",
			"model":               "claude-opus-4-20250514",
			"fallback_model":      "claude-haiku-3",
			"max_turns":           float64(10), // JSON numbers → float64
			"max_budget_usd":      2.5,
			"effort":              "high",
			"allowed_tools":       "Bash Read",
			"disallowed_tools":    "Write",
			"system_prompt":       "be safe",
			"mcp_config":          "/path/mcp.json",
			"session_persistence": false,
		})
		if cfg.PermissionMode != "dontAsk" {
			t.Errorf("PermissionMode = %q", cfg.PermissionMode)
		}
		if cfg.Model != "claude-opus-4-20250514" {
			t.Errorf("Model = %q", cfg.Model)
		}
		if cfg.MaxTurns != 10 {
			t.Errorf("MaxTurns = %d, want 10", cfg.MaxTurns)
		}
		if cfg.MaxBudgetUSD != 2.5 {
			t.Errorf("MaxBudgetUSD = %f, want 2.5", cfg.MaxBudgetUSD)
		}
		if cfg.SessionPersistence {
			t.Error("SessionPersistence = true, want false")
		}
	})

	t.Run("empty config defaults", func(t *testing.T) {
		t.Parallel()
		cfg := parsePassthroughConfig(map[string]any{})
		if cfg.PermissionMode != "" {
			t.Errorf("PermissionMode = %q, want empty", cfg.PermissionMode)
		}
		if cfg.MaxTurns != 0 {
			t.Errorf("MaxTurns = %d, want 0", cfg.MaxTurns)
		}
		if !cfg.SessionPersistence {
			t.Error("SessionPersistence = false, want true (default)")
		}
	})

	t.Run("wrong types ignored", func(t *testing.T) {
		t.Parallel()
		cfg := parsePassthroughConfig(map[string]any{
			"model":          42,     // int instead of string
			"max_turns":      "five", // string instead of int
			"max_budget_usd": "lots", // string instead of float
		})
		if cfg.Model != "" {
			t.Errorf("Model = %q, want empty (wrong type)", cfg.Model)
		}
		if cfg.MaxTurns != 0 {
			t.Errorf("MaxTurns = %d, want 0 (wrong type)", cfg.MaxTurns)
		}
		if cfg.MaxBudgetUSD != 0 {
			t.Errorf("MaxBudgetUSD = %f, want 0 (wrong type)", cfg.MaxBudgetUSD)
		}
	})

	t.Run("int coerced from float64", func(t *testing.T) {
		t.Parallel()
		cfg := parsePassthroughConfig(map[string]any{
			"max_turns": float64(7),
		})
		if cfg.MaxTurns != 7 {
			t.Errorf("MaxTurns = %d, want 7", cfg.MaxTurns)
		}
	})

	t.Run("float coerced from int", func(t *testing.T) {
		t.Parallel()
		cfg := parsePassthroughConfig(map[string]any{
			"max_budget_usd": 3,
		})
		if cfg.MaxBudgetUSD != 3.0 {
			t.Errorf("MaxBudgetUSD = %f, want 3.0", cfg.MaxBudgetUSD)
		}
	})
}

func TestEventStream(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	if ch := adapter.EventStream(); ch != nil {
		t.Errorf("EventStream() = %v, want nil", ch)
	}
}

func TestStopSession_NilProc(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session := domain.Session{
		ID:       "test",
		Internal: &sessionState{},
	}
	err := adapter.StopSession(context.Background(), session)
	if err != nil {
		t.Errorf("StopSession(nil proc) = %v, want nil", err)
	}
}

func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	return agenttest.WriteScript(t, dir, "fake-claude", content)
}

// TestRunTurn_SuccessfulSession verifies a successful session using a fake
// subprocess script.
func TestRunTurn_SuccessfulSession(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"test-session-id","cwd":"/tmp"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]},"session_id":"test-session-id"}
{"type":"result","subtype":"success","result":"All done.","is_error":false,"usage":{"input_tokens":100,"output_tokens":50},"session_id":"test-session-id"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "do the thing",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.Usage.InputTokens != 100 {
		t.Errorf("Usage.InputTokens = %d, want 100", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("Usage.OutputTokens = %d, want 50", result.Usage.OutputTokens)
	}
	if result.Usage.TotalTokens != 150 {
		t.Errorf("Usage.TotalTokens = %d, want 150", result.Usage.TotalTokens)
	}

	// Verify session ID was updated from init event.
	state := session.Internal.(*sessionState)
	if state.claudeSessionID != "test-session-id" {
		t.Errorf("claudeSessionID = %q, want test-session-id", state.claudeSessionID)
	}

	// Verify event sequence.
	eventTypes := make([]domain.AgentEventType, len(events))
	for i, e := range events {
		eventTypes[i] = e.Type
	}
	wantTypes := []domain.AgentEventType{
		domain.EventSessionStarted,
		domain.EventNotification,
		domain.EventTokenUsage,
		domain.EventTurnCompleted,
	}
	if len(eventTypes) != len(wantTypes) {
		t.Fatalf("events = %v, want %v", eventTypes, wantTypes)
	}
	for i, wt := range wantTypes {
		if eventTypes[i] != wt {
			t.Errorf("event[%d] = %q, want %q", i, eventTypes[i], wt)
		}
	}
}

// TestRunTurn_APIDurationMS_Success verifies that the turn-completed event
// carries APIDurationMS from the result's duration_api_ms field.
func TestRunTurn_APIDurationMS_Success(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"api-dur-session","cwd":"/tmp"}
{"type":"result","subtype":"success","result":"Done.","is_error":false,"duration_api_ms":1500,"usage":{"input_tokens":100,"output_tokens":50},"session_id":"api-dur-session"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Find the turn_completed event.
	var found bool
	for _, e := range events {
		if e.Type == domain.EventTurnCompleted {
			found = true
			if e.APIDurationMS != 1500 {
				t.Errorf("turn_completed APIDurationMS = %d, want 1500", e.APIDurationMS)
			}
		}
	}
	if !found {
		t.Error("no turn_completed event found")
	}
}

// TestRunTurn_APIDurationMS_Error verifies that the turn-failed event
// carries APIDurationMS from the result's duration_api_ms field.
func TestRunTurn_APIDurationMS_Error(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"api-dur-err","cwd":"/tmp"}
{"type":"result","subtype":"error_max_turns","result":"","is_error":true,"duration_api_ms":2200,"usage":{"input_tokens":500,"output_tokens":100},"session_id":"api-dur-err"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, _ = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})

	// Find the turn_failed event.
	var found bool
	for _, e := range events {
		if e.Type == domain.EventTurnFailed {
			found = true
			if e.APIDurationMS != 2200 {
				t.Errorf("turn_failed APIDurationMS = %d, want 2200", e.APIDurationMS)
			}
		}
	}
	if !found {
		t.Error("no turn_failed event found")
	}
}

// TestRunTurn_APIDurationMS_ZeroWhenAbsent verifies that APIDurationMS
// defaults to 0 when the result line omits duration_api_ms.
func TestRunTurn_APIDurationMS_ZeroWhenAbsent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"no-dur","cwd":"/tmp"}
{"type":"result","subtype":"success","result":"OK","is_error":false,"usage":{"input_tokens":100,"output_tokens":50},"session_id":"no-dur"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	for _, e := range events {
		if e.Type == domain.EventTurnCompleted {
			if e.APIDurationMS != 0 {
				t.Errorf("turn_completed APIDurationMS = %d, want 0 (absent field)", e.APIDurationMS)
			}
			return
		}
	}
	t.Error("no turn_completed event found")
}

// TestRunTurn_AssistantUsage_SuppressesResultTokenUsage verifies that when
// assistant messages emit per-request usage, the result event does NOT emit
// a duplicate token_usage event. The dedup uses an explicit boolean.
func TestRunTurn_AssistantUsage_SuppressesResultTokenUsage(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Assistant message carries usage → emittedUsage = true.
	// Result event should NOT emit token_usage.
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"dedup-sess","cwd":"/tmp"}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":5},"content":[{"type":"text","text":"Working."}]},"session_id":"dedup-sess"}
{"type":"result","subtype":"success","result":"Done.","is_error":false,"usage":{"input_tokens":80,"output_tokens":20},"session_id":"dedup-sess"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Count token_usage events — should be exactly 1 (from assistant), not 2.
	var tokenUsageCount int
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageCount++
		}
	}
	if tokenUsageCount != 1 {
		eventTypes := make([]domain.AgentEventType, len(events))
		for i, e := range events {
			eventTypes[i] = e.Type
		}
		t.Errorf("token_usage events = %d, want 1 (dedup should suppress result emission); events = %v",
			tokenUsageCount, eventTypes)
	}
}

// TestRunTurn_AssistantZeroOutputTokens_FallsBackToResult verifies that when
// an assistant message carries a usage object with output_tokens=0 (as
// emitted by Claude Code 2.x for tool_use-only turns), the adapter does NOT
// emit a token_usage event for that assistant message. Instead, the result
// event acts as the fallback and emits exactly one token_usage event.
func TestRunTurn_AssistantZeroOutputTokens_FallsBackToResult(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Assistant carries usage with output_tokens=0 (tool_use-only message).
	// The adapter must defer emission and let the result event provide
	// the canonical token_usage.
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"zero-output","cwd":"/tmp"}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":80,"output_tokens":0,"cache_read_input_tokens":0},"content":[{"type":"text","text":"Thinking."}]},"session_id":"zero-output"}
{"type":"result","subtype":"success","result":"OK.","is_error":false,"usage":{"input_tokens":80,"output_tokens":25},"session_id":"zero-output"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Exactly 1 token_usage event — from the result fallback (not from the
	// zero-output-tokens assistant message).
	var tokenUsageCount int
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageCount++
			if e.Usage.OutputTokens <= 0 {
				t.Errorf("EventTokenUsage.OutputTokens = %d, want > 0", e.Usage.OutputTokens)
			}
		}
	}
	if tokenUsageCount != 1 {
		eventTypes := make([]domain.AgentEventType, len(events))
		for i, e := range events {
			eventTypes[i] = e.Type
		}
		t.Errorf("token_usage events = %d, want 1 (result fallback); events = %v",
			tokenUsageCount, eventTypes)
	}

	// TurnResult.Usage reflects result event totals.
	if result.Usage.OutputTokens != 25 {
		t.Errorf("TurnResult.Usage.OutputTokens = %d, want 25", result.Usage.OutputTokens)
	}
}

// TestRunTurn_ToolUseThenText verifies the tool_use → text assistant sequence
// that caused Claude Code 2.x CI failures. The first assistant message
// (tool_use, output_tokens=0) must not emit token_usage; the second
// (text reply, output_tokens>0) emits the cumulative token_usage. The result
// event is then suppressed to avoid inflating APIRequestCount.
func TestRunTurn_ToolUseThenText(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"tool-then-text","cwd":"/tmp"}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":120,"output_tokens":0},"content":[{"type":"tool_use","id":"tool_01","name":"Read","input":{"file_path":"hello.txt"}}]},"session_id":"tool-then-text"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool_01","content":"Hello"}]},"session_id":"tool-then-text"}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","usage":{"input_tokens":140,"output_tokens":3},"content":[{"type":"text","text":"Hello"}]},"session_id":"tool-then-text"}
{"type":"result","subtype":"success","result":"Hello","is_error":false,"usage":{"input_tokens":260,"output_tokens":3},"session_id":"tool-then-text"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "read hello.txt",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Exactly 1 token_usage (from second assistant, not from first or result).
	var tokenUsageCount int
	var firstUsage domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			if tokenUsageCount == 0 {
				firstUsage = e
			}
			tokenUsageCount++
		}
	}
	if tokenUsageCount != 1 {
		eventTypes := make([]domain.AgentEventType, len(events))
		for i, e := range events {
			eventTypes[i] = e.Type
		}
		t.Errorf("token_usage events = %d, want 1; events = %v", tokenUsageCount, eventTypes)
	}
	if firstUsage.Usage.OutputTokens <= 0 {
		t.Errorf("EventTokenUsage.OutputTokens = %d, want > 0", firstUsage.Usage.OutputTokens)
	}
	if firstUsage.Usage.InputTokens <= 0 {
		t.Errorf("EventTokenUsage.InputTokens = %d, want > 0", firstUsage.Usage.InputTokens)
	}
	if firstUsage.Usage.TotalTokens != firstUsage.Usage.InputTokens+firstUsage.Usage.OutputTokens {
		t.Errorf("EventTokenUsage.TotalTokens = %d, want %d",
			firstUsage.Usage.TotalTokens, firstUsage.Usage.InputTokens+firstUsage.Usage.OutputTokens)
	}

	// TurnResult.Usage comes from the result event (authoritative totals).
	if result.Usage.TotalTokens <= 0 {
		t.Errorf("TurnResult.Usage.TotalTokens = %d, want > 0", result.Usage.TotalTokens)
	}
}

// TestRunTurn_ResultOnlyFallback verifies that when no assistant message
// carries usage, the result event emits token_usage as a fallback.
// This is the "result-only" path — the existing TestRunTurn_SuccessfulSession
// implicitly tests this (assistant has no usage field), but this test makes
// the contract explicit.
func TestRunTurn_ResultOnlyFallback(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Assistant message with NO usage field → emittedUsage stays false.
	// Result event must emit token_usage.
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"fallback-sess","cwd":"/tmp"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working."}]},"session_id":"fallback-sess"}
{"type":"result","subtype":"success","result":"All done.","is_error":false,"usage":{"input_tokens":200,"output_tokens":80},"session_id":"fallback-sess"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	// Exactly 1 token_usage event (from result fallback).
	var tokenUsageCount int
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageCount++
		}
	}
	if tokenUsageCount != 1 {
		eventTypes := make([]domain.AgentEventType, len(events))
		for i, e := range events {
			eventTypes[i] = e.Type
		}
		t.Errorf("token_usage events = %d, want 1 (result fallback); events = %v",
			tokenUsageCount, eventTypes)
	}

	// Verify the usage from the result event.
	if result.Usage.InputTokens != 200 {
		t.Errorf("Usage.InputTokens = %d, want 200", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 80 {
		t.Errorf("Usage.OutputTokens = %d, want 80", result.Usage.OutputTokens)
	}
}

func TestRunTurn_ErrorResult(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"err-session","cwd":"/tmp"}
{"type":"result","subtype":"error_max_turns","result":"","is_error":true,"usage":{"input_tokens":500,"output_tokens":100},"session_id":"err-session"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "test",
		OnEvent: func(domain.AgentEvent) {},
	})

	if err == nil {
		t.Fatal("expected error for error result")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrTurnFailed {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
}

func TestRunTurn_NonZeroExit(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `exit 1`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "test",
		OnEvent: func(domain.AgentEvent) {},
	})

	if err == nil {
		t.Fatal("expected error for non-zero exit")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q", result.ExitReason)
	}
}

func TestRunTurn_Exit127(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `exit 127`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "test",
		OnEvent: func(domain.AgentEvent) {},
	})

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrAgentNotFound {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrAgentNotFound)
	}
}

func TestRunTurn_MalformedOutput(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
echo 'this is not json'
echo '{"type":"result","subtype":"success","result":"ok","is_error":false,"usage":{"input_tokens":10,"output_tokens":5}}'
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q", result.ExitReason)
	}

	// First event should be malformed.
	if len(events) < 1 {
		t.Fatal("no events received")
	}
	if events[0].Type != domain.EventMalformed {
		t.Errorf("events[0].Type = %q, want %q", events[0].Type, domain.EventMalformed)
	}
}

func TestRunTurn_ContextCancellation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Script that sleeps long enough for cancellation to fire.
	script := writeScript(t, tmpDir, `
echo '{"type":"system","subtype":"init","session_id":"cancel-test","cwd":"/tmp"}'
exec sleep 60
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var events []domain.AgentEvent
	done := make(chan struct{})
	var result domain.TurnResult
	var runErr error
	go func() {
		result, runErr = adapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt: "test",
			OnEvent: func(e domain.AgentEvent) {
				events = append(events, e)
				// Cancel as soon as we get the init event.
				if e.Type == domain.EventSessionStarted {
					cancel()
				}
			},
		})
		close(done)
	}()
	<-done

	if runErr == nil {
		t.Fatal("expected error on cancellation")
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", runErr)
	}
	if agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrTurnCancelled)
	}
	if result.ExitReason != domain.EventTurnCancelled {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
	}
}

// TestRunTurn_ContextCancelledBeforeStart verifies that RunTurn emits
// EventTurnCancelled and returns ErrTurnCancelled when the context is already
// done before cmd.Start is called.
func TestRunTurn_ContextCancelledBeforeStart(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, "exit 0")

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before RunTurn

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err == nil {
		t.Fatal("expected error on pre-cancelled context, got nil")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnCancelled)
	}
	if result.ExitReason != domain.EventTurnCancelled {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
	}
	var foundCancel bool
	for _, e := range events {
		if e.Type == domain.EventTurnCancelled {
			foundCancel = true
			break
		}
	}
	if !foundCancel {
		t.Error("EventTurnCancelled not delivered on pre-cancelled context")
	}
}

func TestRunTurn_PanicsOnNilOnEvent(t *testing.T) {
	t.Parallel()

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session := domain.Session{
		ID:       "panic-test",
		Internal: &sessionState{command: "/bin/sh", workspacePath: t.TempDir()},
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for nil OnEvent")
		}
	}()
	adapter.RunTurn(context.Background(), session, domain.RunTurnParams{ //nolint:errcheck // testing panic
		Prompt: "test",
	})
}

func TestRunTurn_TurnCountIncrement(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
echo '{"type":"result","subtype":"success","result":"done","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := session.Internal.(*sessionState)
	if state.turnCount != 0 {
		t.Fatalf("initial turnCount = %d", state.turnCount)
	}

	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "first",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.turnCount != 1 {
		t.Errorf("turnCount after turn 1 = %d, want 1", state.turnCount)
	}
}

func TestRunTurn_APIRetryEvent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"retry-session","cwd":"/tmp"}
{"type":"system","subtype":"api_retry","attempt":1,"max_retries":3,"retry_delay_ms":500,"error_status":429,"error":"rate_limit","session_id":"retry-session"}
{"type":"result","subtype":"success","result":"ok","is_error":false,"usage":{"input_tokens":10,"output_tokens":5},"session_id":"retry-session"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Find the notification event for api_retry.
	found := false
	for _, e := range events {
		if e.Type == domain.EventNotification && strings.Contains(e.Message, "API retry attempt") {
			found = true
			if !strings.Contains(e.Message, "429") {
				t.Errorf("api retry message missing status: %q", e.Message)
			}
			break
		}
	}
	if !found {
		types := make([]string, len(events))
		for i, e := range events {
			types[i] = string(e.Type)
		}
		t.Errorf("no api_retry notification found in events: %v", types)
	}
}

func TestRunTurn_StreamEvent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"stream_event","event":{"type":"content_block_delta"}}
{"type":"result","subtype":"success","result":"ok","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if events[0].Type != domain.EventNotification {
		t.Errorf("stream_event mapped to %q, want %q", events[0].Type, domain.EventNotification)
	}
}

func TestRunTurn_UnknownEventType(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"unknown_future_type","data":"something"}
{"type":"result","subtype":"success","result":"ok","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if events[0].Type != domain.EventOtherMessage {
		t.Errorf("unknown type mapped to %q, want %q", events[0].Type, domain.EventOtherMessage)
	}
}

func TestRunTurn_NoResultExitZero(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Script outputs no result event and exits 0.
	script := writeScript(t, tmpDir, `
echo '{"type":"system","subtype":"init","session_id":"noresult","cwd":"/tmp"}'
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "test",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("expected nil error for exit 0 without result, got %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
}

func TestStopSession_RunningProcess(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
echo '{"type":"system","subtype":"init","session_id":"stop-test","cwd":"/tmp"}'
exec sleep 60
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Start RunTurn in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		adapter.RunTurn(ctx, session, domain.RunTurnParams{ //nolint:errcheck // testing StopSession
			Prompt: "test",
			OnEvent: func(e domain.AgentEvent) {
				if e.Type == domain.EventSessionStarted {
					close(started)
				}
			},
		})
		close(done)
	}()

	<-started

	// StopSession should terminate the process.
	err = adapter.StopSession(context.Background(), session)
	if err != nil {
		t.Errorf("StopSession() error = %v", err)
	}

	cancel()
	<-done
}

// TestRunTurn_ToolResultInUserEvent is a regression test verifying that
// tool_result content blocks in user-type JSONL events (not assistant)
// produce correlated EventToolResult events with the correct ToolName.
// Prior to the fix, user-event tool_result blocks were silently dropped.
func TestRunTurn_ToolResultInUserEvent(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "tool_use_result_user_event.jsonl")

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, fmt.Sprintf(`cat <<'JSONL'
%s
JSONL
exit 0
`, string(fixture)))

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "read main.go",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	// Collect all EventToolResult events.
	var toolResults []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	if len(toolResults) != 1 {
		eventTypes := make([]string, len(events))
		for i, e := range events {
			eventTypes[i] = string(e.Type)
		}
		t.Fatalf("EventToolResult count = %d, want 1; all events = %v", len(toolResults), eventTypes)
	}

	got := toolResults[0]
	if got.ToolName != "Read" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "Read")
	}
	if got.ToolError {
		t.Error("ToolError = true, want false")
	}
	if got.ToolDurationMS < 0 {
		t.Errorf("ToolDurationMS = %d, want >= 0", got.ToolDurationMS)
	}
	if got.Message != "tool_result: Read" {
		t.Errorf("Message = %q, want %q", got.Message, "tool_result: Read")
	}
}

// TestRunTurn_ToolResultInAssistantEvent validates that the original
// assistant-only tool_result correlation path still works after the
// user-event fix. Uses the tool_use_result_separate.jsonl fixture.
func TestRunTurn_ToolResultInAssistantEvent(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "tool_use_result_separate.jsonl")

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, fmt.Sprintf(`cat <<'JSONL'
%s
JSONL
exit 0
`, string(fixture)))

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test tools",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	// Collect all EventToolResult events.
	var toolResults []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	// tool_use_result_separate.jsonl has 2 tool uses with results in assistant events.
	if len(toolResults) != 2 {
		eventTypes := make([]string, len(events))
		for i, e := range events {
			eventTypes[i] = string(e.Type)
		}
		t.Fatalf("EventToolResult count = %d, want 2; all events = %v", len(toolResults), eventTypes)
	}

	if toolResults[0].ToolName != "Read" {
		t.Errorf("toolResults[0].ToolName = %q, want %q", toolResults[0].ToolName, "Read")
	}
	if toolResults[1].ToolName != "Bash" {
		t.Errorf("toolResults[1].ToolName = %q, want %q", toolResults[1].ToolName, "Bash")
	}
}

// TestRunTurn_ToolResultErrorText verifies that when a tool call completes
// with is_error: true, the EventToolResult Message contains the actual error
// text from the tool_result content block, not the generic
// "tool_result: <name>" placeholder.
func TestRunTurn_ToolResultErrorText(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "tool_use_error_result.jsonl")

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, fmt.Sprintf(`cat <<'JSONL'
%s
JSONL
exit 0
`, string(fixture)))

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "run foobar",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var toolResults []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	if len(toolResults) != 1 {
		eventTypes := make([]string, len(events))
		for i, e := range events {
			eventTypes[i] = string(e.Type)
		}
		t.Fatalf("EventToolResult count = %d, want 1; all events = %v", len(toolResults), eventTypes)
	}

	got := toolResults[0]
	if got.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "Bash")
	}
	if !got.ToolError {
		t.Error("ToolError = false, want true")
	}
	if got.ToolDurationMS < 0 {
		t.Errorf("ToolDurationMS = %d, want >= 0", got.ToolDurationMS)
	}
	const wantMsg = "bash: command not found: foobar"
	if got.Message != wantMsg {
		t.Errorf("Message = %q, want %q", got.Message, wantMsg)
	}
}

// TestToolResultText verifies the toolResultText helper correctly extracts
// error text from various rawContentBlock shapes.
func TestToolResultText(t *testing.T) {
	t.Parallel()

	longStr := strings.Repeat("a", 600)
	longContent := `"` + longStr + `"`

	tests := []struct {
		name  string
		block rawContentBlock
		want  string
	}{
		{
			name:  "text_field",
			block: rawContentBlock{Text: "error from text"},
			want:  "error from text",
		},
		{
			name:  "json_string",
			block: rawContentBlock{Content: []byte(`"bash: not found"`)},
			want:  "bash: not found",
		},
		{
			name:  "json_array_text",
			block: rawContentBlock{Content: []byte(`[{"type":"text","text":"array error"}]`)},
			want:  "array error",
		},
		{
			name:  "json_array_no_text",
			block: rawContentBlock{Content: []byte(`[{"type":"image"}]`)},
			want:  "",
		},
		{
			name:  "empty",
			block: rawContentBlock{},
			want:  "",
		},
		{
			name:  "long_json_string",
			block: rawContentBlock{Content: []byte(longContent)},
			want:  longStr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := toolResultText(tt.block)

			if got != tt.want {
				t.Errorf("toolResultText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestTruncateToolError verifies the truncateToolError helper enforces the
// byte ceiling while preserving UTF-8 rune boundaries using the
// first-line-plus-tail algorithm.
func TestTruncateToolError(t *testing.T) {
	t.Parallel()

	// 172 CJK runes × 3 bytes = 516 bytes.
	cjk := strings.Repeat("一", 172)

	tests := []struct {
		name     string
		input    string
		maxLen   int
		wantLen  int    // expected len(result); 0 means check wantSame/wantHead/wantTail
		wantSame bool   // true if result must equal input
		wantHead string // if non-empty, result must start with this
		wantTail string // if non-empty, result must end with this
	}{
		{
			name:     "short_string",
			input:    "hello",
			maxLen:   maxToolErrorLen,
			wantSame: true,
		},
		{
			name:     "exactly_512_bytes",
			input:    strings.Repeat("a", 512),
			maxLen:   maxToolErrorLen,
			wantSame: true,
		},
		{
			// Pin to 512 to preserve as a boundary test. No newline in
			// input → tailBytes(s, 512) → last 512 bytes.
			name:    "600_ascii_bytes",
			input:   strings.Repeat("a", 600),
			maxLen:  512,
			wantLen: 512,
		},
		{
			// Pin to 512 to preserve CJK rune-boundary coverage.
			// tailBytes(cjk, 512): 512 mod 3 = 1 (continuation byte),
			// advances to next rune start → 510 bytes (170 full runes).
			name:    "multibyte_mid_rune_boundary",
			input:   cjk,
			maxLen:  512,
			wantLen: 510, // 170 full CJK runes × 3 bytes
		},
		{
			name:     "multiline_tail_preserving",
			input:    "Exit code 2\n" + strings.Repeat("ok  \tpkg/a\t0.04s\n", 60) + "FAIL pkg 0.5s\n",
			maxLen:   512,
			wantHead: "Exit code 2",
			wantTail: "FAIL pkg 0.5s\n",
		},
		{
			name:     "multiline_fits_unchanged",
			input:    "Exit code 2\nFAIL pkg 0.5s\n",
			maxLen:   512,
			wantSame: true,
		},
		{
			// No newline → tailBytes fallback.
			name:    "no_newline_tail_only",
			input:   strings.Repeat("x", 600),
			maxLen:  512,
			wantLen: 512,
		},
		{
			// First line alone > maxLen → tailBudget <= 0 → tailBytes fallback.
			name:    "first_line_exceeds_budget",
			input:   strings.Repeat("a", 600) + "\nFAIL pkg",
			maxLen:  512,
			wantLen: 512,
		},
		{
			// Tail lands mid-rune; tailBytes must advance to valid rune start.
			name:     "multiline_tail_utf8_boundary",
			input:    "Exit code 1\n" + strings.Repeat("一", 200) + "\nFAIL",
			maxLen:   512,
			wantHead: "Exit code 1",
			wantTail: "FAIL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateToolError(tt.input, tt.maxLen)

			if tt.wantSame {
				if got != tt.input {
					t.Errorf("truncateToolError() modified input: got len %d, want len %d", len(got), len(tt.input))
				}
				return
			}
			if tt.wantLen != 0 && len(got) != tt.wantLen {
				t.Errorf("truncateToolError() len = %d, want %d", len(got), tt.wantLen)
			}
			if len(got) > tt.maxLen {
				t.Errorf("truncateToolError() len = %d exceeds maxLen %d", len(got), tt.maxLen)
			}
			if !utf8.ValidString(got) {
				t.Error("truncateToolError() returned invalid UTF-8")
			}
			if tt.wantHead != "" && !strings.HasPrefix(got, tt.wantHead) {
				t.Errorf("truncateToolError() head = %q, want prefix %q", got[:min(len(got), 40)], tt.wantHead)
			}
			if tt.wantTail != "" && !strings.HasSuffix(got, tt.wantTail) {
				suffix := got[max(0, len(got)-40):]
				t.Errorf("truncateToolError() tail = %q, want suffix %q", suffix, tt.wantTail)
			}
		})
	}
}

// TestStripClaudeMarkup verifies that stripClaudeMarkup removes the
// <tool_use_error> XML wrapper and ANSI escape sequences from error text.
func TestStripClaudeMarkup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "xml_wrapped",
			input: "<tool_use_error>Found 3 matches of the string to replace, but replace_all is false.</tool_use_error>",
			want:  "Found 3 matches of the string to replace, but replace_all is false.",
		},
		{
			name:  "xml_wrapped_with_surrounding_whitespace",
			input: "  <tool_use_error>some error</tool_use_error>  ",
			want:  "some error",
		},
		{
			name:  "no_xml",
			input: "bash: command not found: foobar",
			want:  "bash: command not found: foobar",
		},
		{
			name:  "partial_open_tag",
			input: "<tool_use_error>only open tag",
			want:  "<tool_use_error>only open tag",
		},
		{
			name:  "partial_close_tag",
			input: "only close tag</tool_use_error>",
			want:  "only close tag</tool_use_error>",
		},
		{
			name:  "ansi_color_codes",
			input: "\x1b[31mFAIL\x1b[0m pkg/foo \u2014 some assertion",
			want:  "FAIL pkg/foo \u2014 some assertion",
		},
		{
			name:  "xml_with_ansi_inside",
			input: "<tool_use_error>\x1b[1mfile not found\x1b[0m</tool_use_error>",
			want:  "file not found",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := stripClaudeMarkup(tt.input)
			if got != tt.want {
				t.Errorf("stripClaudeMarkup(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestProcessToolBlocks_XMLStripping verifies that processToolBlocks strips
// the <tool_use_error> XML wrapper from tool error messages before emitting
// events.
func TestProcessToolBlocks_XMLStripping(t *testing.T) {
	t.Parallel()

	xmlContent := `"<tool_use_error>Found 3 matches of the string to replace, but replace_all is false. Use str_replace API with replace_all=true to replace all instances.</tool_use_error>"`
	blocks := []rawContentBlock{
		{
			Type:      "tool_result",
			ToolUseID: "toolu_test01",
			IsError:   true,
			Content:   json.RawMessage(xmlContent),
		},
	}
	inFlight := map[string]inFlightTool{
		"toolu_test01": {Name: "Edit", Timestamp: time.Now()},
	}

	var events []domain.AgentEvent
	processToolBlocks(blocks, inFlight, time.Now(), time.Now(), func(e domain.AgentEvent) {
		events = append(events, e)
	})

	if len(events) != 1 {
		t.Fatalf("processToolBlocks() emitted %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != domain.EventToolResult {
		t.Fatalf("event.Type = %q, want %q", e.Type, domain.EventToolResult)
	}
	if !e.ToolError {
		t.Error("event.ToolError = false, want true")
	}
	if strings.Contains(e.Message, "<tool_use_error>") {
		t.Errorf("event.Message contains <tool_use_error>: %q", e.Message)
	}
	if strings.Contains(e.Message, "</tool_use_error>") {
		t.Errorf("event.Message contains </tool_use_error>: %q", e.Message)
	}
	if !strings.Contains(e.Message, "Found 3 matches") {
		t.Errorf("event.Message missing expected text: %q", e.Message)
	}
}

// TestProcessToolBlocks_TailTruncation verifies that processToolBlocks
// preserves the first line and the failure-bearing tail of large CLI output
// within the maxToolErrorLen byte budget.
func TestProcessToolBlocks_TailTruncation(t *testing.T) {
	t.Parallel()

	// Build a content string that exceeds 2048 bytes:
	// "Exit code 2\n" + 80 ok-lines + "FAIL ...\n"
	var b strings.Builder
	b.WriteString("Exit code 2\n")
	for range 80 {
		b.WriteString("ok  \tgithub.com/example/pkg\t0.042s\n")
	}
	b.WriteString("FAIL github.com/example/fail_pkg 0.5s\n")
	largeOutput := b.String()

	// Wrap in JSON string for rawContentBlock.Content.
	contentJSON, err := json.Marshal(largeOutput)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	blocks := []rawContentBlock{
		{
			Type:      "tool_result",
			ToolUseID: "toolu_tail01",
			IsError:   true,
			Content:   json.RawMessage(contentJSON),
		},
	}
	inFlight := map[string]inFlightTool{
		"toolu_tail01": {Name: "Bash", Timestamp: time.Now()},
	}

	var events []domain.AgentEvent
	processToolBlocks(blocks, inFlight, time.Now(), time.Now(), func(e domain.AgentEvent) {
		events = append(events, e)
	})

	if len(events) != 1 {
		t.Fatalf("processToolBlocks() emitted %d events, want 1", len(events))
	}
	msg := events[0].Message
	if !strings.Contains(msg, "Exit code 2") {
		t.Errorf("message missing head line: %q", msg[:min(len(msg), 80)])
	}
	if !strings.Contains(msg, "FAIL github.com/example/fail_pkg 0.5s") {
		suffix := msg[max(0, len(msg)-80):]
		t.Errorf("message missing FAIL tail: %q", suffix)
	}
	if len(msg) > maxToolErrorLen {
		t.Errorf("len(msg) = %d, exceeds maxToolErrorLen %d", len(msg), maxToolErrorLen)
	}
	if !utf8.ValidString(msg) {
		t.Error("message is not valid UTF-8")
	}
}

// TestRunTurn_ToolResultXMLWrappedError verifies end-to-end that an
// XML-wrapped tool error from the Claude Code subprocess has its
// <tool_use_error> envelope stripped before reaching the emitted event.
func TestRunTurn_ToolResultXMLWrappedError(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "tool_use_error_xml_wrapped.jsonl")

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, fmt.Sprintf(`cat <<'JSONL'
%s
JSONL
exit 0
`, string(fixture)))

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "edit file",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var toolResults []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventToolResult {
			toolResults = append(toolResults, e)
		}
	}

	if len(toolResults) != 1 {
		eventTypes := make([]string, len(events))
		for i, e := range events {
			eventTypes[i] = string(e.Type)
		}
		t.Fatalf("EventToolResult count = %d, want 1; all events = %v", len(toolResults), eventTypes)
	}

	got := toolResults[0]
	if got.ToolName != "Edit" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "Edit")
	}
	if !got.ToolError {
		t.Error("ToolError = false, want true")
	}
	if strings.Contains(got.Message, "<tool_use_error>") {
		t.Errorf("Message contains <tool_use_error>: %q", got.Message)
	}
	if strings.Contains(got.Message, "</tool_use_error>") {
		t.Errorf("Message contains </tool_use_error>: %q", got.Message)
	}
	if !strings.Contains(got.Message, "Found 3 matches") {
		t.Errorf("Message missing expected text: %q", got.Message)
	}
}

// TestRunTurn_PerRequestAPIDurationMS verifies that each assistant event
// with per-request usage emits a token_usage event with APIDurationMS > 0,
// measured from the preceding system/init or user event.
func TestRunTurn_PerRequestAPIDurationMS(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"per-req-timing","cwd":"/tmp"}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"toolu_req01","name":"Read","input":{"file_path":"main.go"}}],"usage":{"input_tokens":100,"output_tokens":10}},"session_id":"per-req-timing"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_req01","content":"package main","is_error":false}]}}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":150,"output_tokens":5}},"session_id":"per-req-timing"}
{"type":"result","subtype":"success","result":"Done.","is_error":false,"usage":{"input_tokens":250,"output_tokens":15},"session_id":"per-req-timing"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "read main.go",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var tokenUsageEvents []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageEvents = append(tokenUsageEvents, e)
		}
	}

	if len(tokenUsageEvents) != 2 {
		eventTypes := make([]string, len(events))
		for i, e := range events {
			eventTypes[i] = string(e.Type)
		}
		t.Fatalf("token_usage event count = %d, want 2; all events = %v", len(tokenUsageEvents), eventTypes)
	}
	for i, e := range tokenUsageEvents {
		if e.APIDurationMS <= 0 {
			t.Errorf("token_usage[%d].APIDurationMS = %d, want > 0", i, e.APIDurationMS)
		}
	}
}

// TestRunTurn_PerRequestAPIDurationMS_NoDoubleCount verifies that the
// turn_completed event carries APIDurationMS == 0 when per-request timing
// was already emitted on token_usage events during the turn.
func TestRunTurn_PerRequestAPIDurationMS_NoDoubleCount(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// Same flow as PerRequestAPIDurationMS but result carries duration_api_ms.
	// The turn-finalization guard must suppress it to prevent double-counting.
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"system","subtype":"init","session_id":"no-double-count","cwd":"/tmp"}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"toolu_dc01","name":"Read","input":{"file_path":"main.go"}}],"usage":{"input_tokens":100,"output_tokens":10}},"session_id":"no-double-count"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_dc01","content":"package main","is_error":false}]}}
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":150,"output_tokens":5}},"session_id":"no-double-count"}
{"type":"result","subtype":"success","result":"Done.","is_error":false,"duration_api_ms":5000,"usage":{"input_tokens":250,"output_tokens":15},"session_id":"no-double-count"}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "read main.go",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	for _, e := range events {
		if e.Type == domain.EventTurnCompleted {
			if e.APIDurationMS != 0 {
				t.Errorf("turn_completed APIDurationMS = %d, want 0 (per-request timing suppresses turn-level value)", e.APIDurationMS)
			}
			return
		}
	}
	t.Error("no turn_completed event found")
}

// TestRunTurn_PerRequestAPIDurationMS_NoInitGuard verifies that when the
// first event is an assistant event with no preceding system/init, the
// token_usage event carries APIDurationMS == 0 (apiCallStart is unset),
// and the turn-finalization fallback then uses result.duration_api_ms.
func TestRunTurn_PerRequestAPIDurationMS_NoInitGuard(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// No system/init event — apiCallStart stays zero, so the guard
	// !apiCallStart.IsZero() prevents any timing from being recorded.
	// emittedAPITiming stays false, so the fallback path fires on result.
	script := writeScript(t, tmpDir, `
cat <<'JSONL'
{"type":"assistant","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Working."}],"usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"result","subtype":"success","result":"Done.","is_error":false,"duration_api_ms":1000,"usage":{"input_tokens":100,"output_tokens":10}}
JSONL
exit 0
`)

	adapter, _ := NewClaudeCodeAdapter(map[string]any{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	var events []domain.AgentEvent
	_, err = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var foundTokenUsage, foundTurnCompleted bool
	for _, e := range events {
		switch e.Type {
		case domain.EventTokenUsage:
			foundTokenUsage = true
			if e.APIDurationMS != 0 {
				t.Errorf("token_usage APIDurationMS = %d, want 0 (no preceding init event)", e.APIDurationMS)
			}
		case domain.EventTurnCompleted:
			foundTurnCompleted = true
			if e.APIDurationMS != 1000 {
				t.Errorf("turn_completed APIDurationMS = %d, want 1000 (fallback to result.duration_api_ms)", e.APIDurationMS)
			}
		}
	}
	if !foundTokenUsage {
		t.Error("no token_usage event found")
	}
	if !foundTurnCompleted {
		t.Error("no turn_completed event found")
	}
}
