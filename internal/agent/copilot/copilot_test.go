//go:build unix

package copilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// fakeCopilotBinary creates a minimal shell script at a temp path that
// exits 0 for any invocation (including the --version canary check).
func fakeCopilotBinary(t *testing.T) string {
	t.Helper()
	return agenttest.WriteScript(t, t.TempDir(), "copilot", "exit 0")
}

// requireAgentError asserts err is a *domain.AgentError with the given Kind.
func requireAgentError(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

func TestNewCopilotAdapter(t *testing.T) {
	t.Parallel()

	t.Run("zero config succeeds", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCopilotAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCopilotAdapter(empty) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("nil config succeeds", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCopilotAdapter(nil)
		if err != nil {
			t.Fatalf("NewCopilotAdapter(nil) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("passthrough fields are stored on adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCopilotAdapter(map[string]any{
			"model":                   "gpt-5",
			"max_autopilot_continues": float64(15),
			"agent":                   "custom",
			"disable_builtin_mcps":    true,
			"no_custom_instructions":  true,
			"experimental":            true,
		})
		if err != nil {
			t.Fatalf("NewCopilotAdapter() error = %v", err)
		}
		a := adapter.(*CopilotAdapter)
		if a.passthrough.Model != "gpt-5" {
			t.Errorf("passthrough.Model = %q, want %q", a.passthrough.Model, "gpt-5")
		}
		if a.passthrough.MaxAutopilotContinues != 15 {
			t.Errorf("passthrough.MaxAutopilotContinues = %d, want 15", a.passthrough.MaxAutopilotContinues)
		}
		if a.passthrough.Agent != "custom" {
			t.Errorf("passthrough.Agent = %q, want %q", a.passthrough.Agent, "custom")
		}
		if !a.passthrough.DisableBuiltinMCPs {
			t.Error("passthrough.DisableBuiltinMCPs = false, want true")
		}
		if !a.passthrough.NoCustomInstructions {
			t.Error("passthrough.NoCustomInstructions = false, want true")
		}
		if !a.passthrough.Experimental {
			t.Error("passthrough.Experimental = false, want true")
		}
	})
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	// Verify "copilot-cli" kind is registered.
	factory, err := registry.Agents.Get("copilot-cli")
	if err != nil {
		t.Fatalf("registry.Agents.Get(\"copilot-cli\") error = %v", err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory(empty config) error = %v", err)
	}
	if _, ok := adapter.(*CopilotAdapter); !ok {
		t.Errorf("factory() type = %T, want *CopilotAdapter", adapter)
	}

	// Verify RequiresCommand metadata is set.
	meta, _ := registry.Agents.Meta("copilot-cli")
	if !meta.RequiresCommand {
		t.Error("AgentMeta.RequiresCommand = false, want true")
	}
}

func TestEventStream(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCopilotAdapter(map[string]any{})
	ch := adapter.EventStream()
	if ch != nil {
		t.Errorf("EventStream() = non-nil channel, want nil (synchronous adapter)")
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

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
			name: "non-existent workspace path",
			setup: func(_ *testing.T) domain.StartSessionParams {
				return domain.StartSessionParams{
					WorkspacePath: "/nonexistent/sortie-test-path-12345",
					AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "workspace path is a file not a directory",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				tmpFile := filepath.Join(t.TempDir(), "notadir")
				if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return domain.StartSessionParams{
					WorkspacePath: tmpFile,
					AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "agent command not found on PATH",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				return domain.StartSessionParams{
					WorkspacePath: t.TempDir(),
					AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-copilot-12345"},
				}
			},
			wantErr: domain.ErrAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := NewCopilotAdapter(map[string]any{})
			params := tt.setup(t)
			_, err := adapter.StartSession(context.Background(), params)
			requireAgentError(t, err, tt.wantErr)
		})
	}
}

func TestStartSession_NoAuthSource(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.

	// Unset all GitHub token env vars and ensure gh is not on PATH.
	// If gh is on PATH, this test skips — we cannot override PATH
	// without affecting other tests and the gh check is a best-effort.
	if _, err := exec.LookPath("gh"); err == nil {
		t.Skip("gh is on PATH; checkAuth() will pass via gh fallback, skipping auth-failure test")
	}

	for _, env := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(env, "")
	}

	adapter, _ := NewCopilotAdapter(map[string]any{})
	fakeBin := fakeCopilotBinary(t)
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: fakeBin},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestStartSession_NewSession(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.

	// Provide a GitHub token so checkAuth() passes.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter(map[string]any{})
	fakeBin := fakeCopilotBinary(t)
	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeBin},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	// Copilot CLI does not pre-assign a session ID: the ID is empty
	// until the first turn's result event provides one.
	if session.ID != "" {
		t.Errorf("session.ID = %q, want empty (Copilot ID assigned after first turn)", session.ID)
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	if state.copilotSessionID != "" {
		t.Errorf("state.copilotSessionID = %q, want empty for new session", state.copilotSessionID)
	}
	if state.workspacePath != workspace {
		// t.TempDir() may return a path through a symlink; compare with os.Stat.
		if state.workspacePath != filepath.Clean(workspace) {
			t.Errorf("state.workspacePath = %q, want %q", state.workspacePath, workspace)
		}
	}
	if state.fallbackToContinue {
		t.Error("state.fallbackToContinue = true, want false for new session")
	}
	if state.sshHost != "" {
		t.Errorf("state.sshHost = %q, want empty for local mode", state.sshHost)
	}
}

func TestStartSession_ResumeSessionID(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter(map[string]any{})
	fakeBin := fakeCopilotBinary(t)
	const resumeID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: fakeBin},
		ResumeSessionID: resumeID,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	if session.ID != resumeID {
		t.Errorf("session.ID = %q, want %q", session.ID, resumeID)
	}

	state := session.Internal.(*sessionState)
	if state.copilotSessionID != resumeID {
		t.Errorf("state.copilotSessionID = %q, want %q", state.copilotSessionID, resumeID)
	}
}

func TestStartSession_DefaultCommand(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter(map[string]any{})
	// Empty command falls back to "copilot". In CI, copilot is likely
	// absent, so we expect ErrAgentNotFound. If copilot is installed,
	// the session may succeed — either outcome is acceptable.
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{},
	})
	if err == nil {
		return // copilot is on PATH — that's fine
	}
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestStartSession_SSHMode(t *testing.T) {
	t.Parallel()

	// SSH mode requires ssh on PATH; skip otherwise.
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh not available on PATH")
	}

	adapter, _ := NewCopilotAdapter(map[string]any{})
	workspace := t.TempDir()
	session, lookupErr := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: "copilot"},
		SSHHost:       "dev-host.example.com",
	})
	if lookupErr != nil {
		t.Fatalf("StartSession() (SSH mode) error = %v", lookupErr)
	}

	state := session.Internal.(*sessionState)
	if state.sshHost != "dev-host.example.com" {
		t.Errorf("state.sshHost = %q, want %q", state.sshHost, "dev-host.example.com")
	}
	if state.remoteCommand != "copilot" {
		t.Errorf("state.remoteCommand = %q, want %q", state.remoteCommand, "copilot")
	}
	if state.command != sshPath {
		t.Errorf("state.command = %q, want %q (ssh binary)", state.command, sshPath)
	}
	// Auth check is skipped in SSH mode.
}

// fakeGhBinaryDir creates a fake "gh" binary that exits non-zero (simulating
// an unauthenticated host) and returns the directory containing it, ready for
// use as the sole PATH entry.
func fakeGhBinaryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "gh"),
		[]byte("#!/bin/sh\nexit 1\n"), 0o755,
	); err != nil {
		t.Fatalf("creating fake gh binary: %v", err)
	}
	return dir
}

// TestStartSession_SSHHostWhitespaceOnly verifies that a whitespace-only
// SSHHost value is trimmed to empty and the session falls through to the
// local subprocess path, not SSH mode.
func TestStartSession_SSHHostWhitespaceOnly(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter(map[string]any{})
	fakeBin := fakeCopilotBinary(t)
	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeBin},
		SSHHost:       "   ", // whitespace-only: must be treated as local (no SSH host)
	})
	if err != nil {
		t.Fatalf("StartSession(SSHHost=%q) error = %v", "   ", err)
	}

	state := session.Internal.(*sessionState)
	if state.sshHost != "" {
		t.Errorf("state.sshHost = %q, want empty (whitespace-only SSHHost treated as local mode)", state.sshHost)
	}
	if state.remoteCommand != "" {
		t.Errorf("state.remoteCommand = %q, want empty for local mode", state.remoteCommand)
	}
}

// TestCheckAuth_GhPresentButUnauthenticated verifies that checkAuth returns
// ErrAgentNotFound when the gh binary is present but "gh auth status" exits
// non-zero (i.e., the host has gh installed but not authenticated).
func TestCheckAuth_GhPresentButUnauthenticated(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.

	// Point PATH to a directory containing only a fake gh that exits 1.
	t.Setenv("PATH", fakeGhBinaryDir(t))

	// Unset all GitHub token env vars so the env-var fast-path is skipped.
	for _, env := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(env, "")
	}

	err := checkAuth(context.Background())
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

// TestCheckAuth_WhitespaceOnlyToken verifies that a token env var set to
// whitespace-only does not satisfy the auth preflight. The check must fall
// through to the gh auth probe; when that also fails the function returns
// ErrAgentNotFound.
func TestCheckAuth_WhitespaceOnlyToken(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.

	// COPILOT_GITHUB_TOKEN is whitespace-only; the other vars are absent.
	t.Setenv("COPILOT_GITHUB_TOKEN", "   ")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	// Point PATH to an unauthenticated fake gh so the fallback also fails.
	t.Setenv("PATH", fakeGhBinaryDir(t))

	err := checkAuth(context.Background())
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

// fakeCopilotBinaryWithOutput creates a fake copilot binary that writes content
// to stdout and exits with the given exit code.
func fakeCopilotBinaryWithOutput(t *testing.T, content string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(outFile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing run turn output file: %v", err)
	}
	return agenttest.WriteScript(t, dir, "copilot", fmt.Sprintf("cat '%s'\nexit %d", outFile, exitCode))
}

// newTestSession starts a session backed by fakeCopilotBinary.
// The caller must set GH_TOKEN (or another auth env var) before calling.
func newTestSession(t *testing.T, workspace string) (domain.AgentAdapter, domain.Session) {
	t.Helper()
	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeCopilotBinary(t)},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return adapter, session
}

// loadTestFixture reads a testdata fixture file and returns its content.
func loadTestFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadTestFixture(%q): %v", name, err)
	}
	return string(data)
}

// hasEventType returns true if any event in events matches typ.
func hasEventType(events []domain.AgentEvent, typ domain.AgentEventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// findEventByType returns the first event matching typ and true, or a zero
// value and false when no matching event exists.
func findEventByType(events []domain.AgentEvent, typ domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == typ {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

func TestRunTurn_HappyPath(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "say hello",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	const wantSessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	if result.SessionID != wantSessionID {
		t.Errorf("result.SessionID = %q, want %q", result.SessionID, wantSessionID)
	}
	// Session ID captured from result event for subsequent turns.
	if state.copilotSessionID != wantSessionID {
		t.Errorf("state.copilotSessionID = %q, want %q", state.copilotSessionID, wantSessionID)
	}
	for _, typ := range []domain.AgentEventType{
		domain.EventSessionStarted,
		domain.EventTokenUsage,
		domain.EventTurnCompleted,
	} {
		if !hasEventType(events, typ) {
			t.Errorf("event type %q not delivered", typ)
		}
	}

	// EventTurnCompleted must carry APIDurationMS from result.totalApiDurationMs.
	// simple_session.jsonl has totalApiDurationMs: 6866.
	if e, ok := findEventByType(events, domain.EventTurnCompleted); ok {
		const wantAPIDurationMS int64 = 6866
		if e.APIDurationMS != wantAPIDurationMS {
			t.Errorf("EventTurnCompleted.APIDurationMS = %d, want %d", e.APIDurationMS, wantAPIDurationMS)
		}
	} else {
		t.Error("EventTurnCompleted not found in events")
	}

	// No phantom EventTokenUsage should follow the final turn-completion event.
	for i, e := range events {
		if e.Type == domain.EventTurnCompleted {
			for _, after := range events[i+1:] {
				if after.Type == domain.EventTokenUsage {
					t.Error("phantom EventTokenUsage emitted after EventTurnCompleted")
				}
			}
			break
		}
	}
}

func TestRunTurn_ExitCode127(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, "", 127)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrAgentNotFound)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for exit code 127")
	}
}

func TestRunTurn_NonZeroExitNoResult(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, "", 1)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero exit")
	}
}

func TestRunTurn_NoOutputExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, "", 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for no-output exit 0")
	}
}

func TestRunTurn_PartialOutputNoResultExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello","outputTokens":42}}` + "\n"
	state.command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("expected nil error for partial output exit 0, got %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(events, domain.EventTurnCompleted) {
		t.Error("EventTurnCompleted not delivered for partial-output exit 0")
	}
	const wantTokens int64 = 42
	if result.Usage.OutputTokens != wantTokens {
		t.Errorf("Usage.OutputTokens = %d, want %d", result.Usage.OutputTokens, wantTokens)
	}
}

func TestRunTurn_StderrWarnOnNoOutputExitZero(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithStderrAndExit(t, "Invalid JSON in --additional-mcp-config", 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)

	warnLines := agenttest.RequireWarnLines(t, spy, "agent exited without producing output")
	found := false
	for _, line := range warnLines {
		if strings.Contains(line, "Invalid JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WARN lines %v do not contain \"Invalid JSON\"", warnLines)
	}
}

func TestRunTurn_ContextCancelled(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	// state.command is fakeCopilotBinary: exits 0 immediately, no output.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before RunTurn

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnCancelled)
	if !hasEventType(events, domain.EventTurnCancelled) {
		t.Error("EventTurnCancelled not delivered")
	}
}

func TestRunTurn_MalformedLines(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "malformed_lines.jsonl"), 0)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("RunTurn() unexpected error = %v", err)
	}
	if !hasEventType(events, domain.EventMalformed) {
		t.Error("EventMalformed not delivered for malformed JSONL line")
	}
}

func TestRunTurn_ToolUseEvents(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "tool_use_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var toolEvent *domain.AgentEvent
	for i := range events {
		if events[i].Type == domain.EventToolResult {
			toolEvent = &events[i]
			break
		}
	}
	if toolEvent == nil {
		t.Fatal("EventToolResult not delivered for tool use session")
	}
	if toolEvent.ToolName == "" {
		t.Error("EventToolResult.ToolName is empty")
	}
}

func TestRunTurn_NonZeroResultExitCode(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	// JSONL with a result event reporting a non-zero exit code.
	const failResultJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, failResultJSONL, 0)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero result exit code")
	}
}

// TestRunTurn_TurnFailed_APIDurationMS verifies that EventTurnFailed carries
// APIDurationMS from the result event's totalApiDurationMs field, and that no
// phantom EventTokenUsage follows it.
func TestRunTurn_TurnFailed_APIDurationMS(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	// Result event with non-zero totalApiDurationMs and a non-zero exit code.
	const failJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":2,"totalApiDurationMs":5000,"sessionDurationMs":9000}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithOutput(t, failJSONL, 0)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnFailed)

	// EventTurnFailed must carry APIDurationMS from the result event.
	e, ok := findEventByType(events, domain.EventTurnFailed)
	if !ok {
		t.Fatal("EventTurnFailed not delivered")
	}
	const wantAPIDurationMS int64 = 5000
	if e.APIDurationMS != wantAPIDurationMS {
		t.Errorf("EventTurnFailed.APIDurationMS = %d, want %d", e.APIDurationMS, wantAPIDurationMS)
	}

	// No phantom EventTokenUsage should follow EventTurnFailed.
	for i, ev := range events {
		if ev.Type == domain.EventTurnFailed {
			for _, after := range events[i+1:] {
				if after.Type == domain.EventTokenUsage {
					t.Error("phantom EventTokenUsage emitted after EventTurnFailed")
				}
			}
			break
		}
	}
}

// TestRunTurn_ContextCancelledBeforeStart verifies that RunTurn emits
// EventTurnCancelled and returns ErrTurnCancelled when the context is already
// done before cmd.Start is called.
func TestRunTurn_ContextCancelledBeforeStart(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())

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

func TestStopSession_NilProc(t *testing.T) {
	t.Parallel()

	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	// Bare session with no running subprocess (proc == nil).
	session := domain.Session{
		Internal: &sessionState{
			workspacePath: t.TempDir(),
		},
	}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession(nil proc) error = %v", err)
	}
}

func TestStopSession_TerminatesProcess(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	// Fake binary that blocks until it receives a signal.
	dir := t.TempDir()
	sleepBin := agenttest.WriteScript(t, dir, "copilot", "exec sleep 60")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = sleepBin

	processStarted := make(chan struct{}, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		adapter.RunTurn(context.Background(), session, domain.RunTurnParams{ //nolint:errcheck // error is irrelevant for this test
			OnEvent: func(e domain.AgentEvent) {
				if e.Type == domain.EventSessionStarted {
					select {
					case processStarted <- struct{}{}:
					default:
					}
				}
			},
		})
	}()

	// Wait until RunTurn has started the subprocess.
	select {
	case <-processStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for process to start")
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	// Verify RunTurn unblocked after StopSession terminated the process.
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("RunTurn did not return after StopSession")
	}
}

// fakeCopilotBinaryWithStderrAndExit creates a fake copilot binary that
// writes stderrLine to stderr and exits with exitCode.
func fakeCopilotBinaryWithStderrAndExit(t *testing.T, stderrLine string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	errFile := filepath.Join(dir, "err.txt")
	if err := os.WriteFile(errFile, []byte(stderrLine+"\n"), 0o644); err != nil {
		t.Fatalf("fakeCopilotBinaryWithStderrAndExit: writing stderr file: %v", err)
	}
	return agenttest.WriteScript(t, dir, "copilot",
		fmt.Sprintf("cat '%s' >&2\nexit %d", errFile, exitCode))
}

// TestRunTurn_StderrWarnOnExitCode127 verifies that when the subprocess
// writes to stderr and exits with code 127, the stderr lines are
// re-emitted at WARN level.
func TestRunTurn_StderrWarnOnExitCode127(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithStderrAndExit(t, "license check failed: no valid license", 127)

	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrAgentNotFound {
		t.Errorf("error = %v, want AgentError{Kind: %q}", runErr, domain.ErrAgentNotFound)
	}

	warnLines := agenttest.RequireWarnLines(t, spy, "exit code 127")
	if !strings.Contains(warnLines[0], "license check failed") {
		t.Errorf("WARN line = %q, want it to contain \"license check failed\"", warnLines[0])
	}
}

// TestRunTurn_StderrWarnOnNonZeroExit verifies that when the subprocess
// writes to stderr and exits non-zero (not 127) without a result event,
// the stderr lines are re-emitted at WARN level.
func TestRunTurn_StderrWarnOnNonZeroExit(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.command = fakeCopilotBinaryWithStderrAndExit(t, "internal agent panic", 1)

	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Errorf("error = %v, want AgentError{Kind: %q}", runErr, domain.ErrPortExit)
	}

	warnLines := agenttest.RequireWarnLines(t, spy, "non-zero exit")
	if !strings.Contains(warnLines[0], "internal agent panic") {
		t.Errorf("WARN line = %q, want it to contain \"internal agent panic\"", warnLines[0])
	}
}

// TestRunTurn_StderrNoWarnOnSuccess verifies that when the subprocess
// succeeds, stderr lines are not re-emitted at WARN level.
func TestRunTurn_StderrNoWarnOnSuccess(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	dir := t.TempDir()
	errFile := filepath.Join(dir, "err.txt")
	if err := os.WriteFile(errFile, []byte("minor diagnostic\n"), 0o644); err != nil {
		t.Fatalf("writing stderr file: %v", err)
	}
	outFile := filepath.Join(dir, "out.txt")
	const successJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"no-warn-success-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`
	if err := os.WriteFile(outFile, []byte(successJSONL+"\n"), 0o644); err != nil {
		t.Fatalf("writing stdout file: %v", err)
	}
	state.command = agenttest.WriteScript(t, dir, "copilot",
		fmt.Sprintf("cat '%s' >&2\ncat '%s'", errFile, outFile))

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	if warnLines := spy.WarnLines(); len(warnLines) != 0 {
		t.Errorf("success path produced %d WARN lines for stderr, want 0; got %v", len(warnLines), warnLines)
	}
}
