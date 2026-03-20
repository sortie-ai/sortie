package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
			t.Errorf("Model = %q", a.passthrough.Model)
		}
		if a.passthrough.MaxTurns != 10 {
			t.Errorf("MaxTurns = %d, want 10", a.passthrough.MaxTurns)
		}
		if a.passthrough.MaxBudgetUSD != 1.5 {
			t.Errorf("MaxBudgetUSD = %f, want 1.5", a.passthrough.MaxBudgetUSD)
		}
		if a.passthrough.PermissionMode != "bypassPermissions" {
			t.Errorf("PermissionMode = %q", a.passthrough.PermissionMode)
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
		t.Error("factory returned wrong type")
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

	// Use /bin/sh as a stand-in for the claude binary (always on PATH).
	existingCmd := "/bin/sh"

	tests := []struct {
		name    string
		params  domain.StartSessionParams
		wantErr domain.AgentErrorKind
	}{
		{
			name:    "empty workspace path",
			params:  domain.StartSessionParams{},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "non-existent workspace",
			params: domain.StartSessionParams{
				WorkspacePath: "/nonexistent/path/that/does/not/exist",
				AgentConfig:   domain.AgentConfig{Command: existingCmd},
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "workspace is a file",
			params: domain.StartSessionParams{
				AgentConfig: domain.AgentConfig{Command: existingCmd},
				// WorkspacePath set below
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "command not found",
			params: domain.StartSessionParams{
				AgentConfig: domain.AgentConfig{Command: "sortie-nonexistent-binary-12345"},
				// WorkspacePath set below
			},
			wantErr: domain.ErrAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := NewClaudeCodeAdapter(map[string]any{})

			params := tt.params
			if tt.name == "workspace is a file" {
				tmpFile := filepath.Join(t.TempDir(), "afile")
				if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				params.WorkspacePath = tmpFile
			}
			if tt.name == "command not found" {
				params.WorkspacePath = t.TempDir()
			}

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

func TestExtractExitCode(t *testing.T) {
	t.Parallel()

	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		if got := extractExitCode(nil); got != 0 {
			t.Errorf("extractExitCode(nil) = %d, want 0", got)
		}
	})

	t.Run("non-ExitError", func(t *testing.T) {
		t.Parallel()
		if got := extractExitCode(fmt.Errorf("something")); got != -1 {
			t.Errorf("extractExitCode(fmt.Errorf) = %d, want -1", got)
		}
	})

	t.Run("real exit code", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command("/bin/sh", "-c", "exit 42")
		err := cmd.Run()
		got := extractExitCode(err)
		if got != 42 {
			t.Errorf("extractExitCode(exit 42) = %d, want 42", got)
		}
	})
}

func TestWasSignaled(t *testing.T) {
	t.Parallel()

	t.Run("non-ExitError", func(t *testing.T) {
		t.Parallel()
		if wasSignaled(fmt.Errorf("not exec")) {
			t.Error("wasSignaled(non-ExitError) = true")
		}
	})

	t.Run("normal exit", func(t *testing.T) {
		t.Parallel()
		cmd := exec.Command("/bin/sh", "-c", "exit 1")
		err := cmd.Run()
		if wasSignaled(err) {
			t.Error("wasSignaled(exit 1) = true, want false")
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

// writeScript writes an executable shell script to the given directory and
// returns the path. Used by RunTurn integration tests.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Section 10.1: RunTurn integration tests using fake subprocess scripts.
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
