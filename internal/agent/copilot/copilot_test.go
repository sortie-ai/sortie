//go:build unix

package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
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
	meta, ok := registry.Agents.Meta("copilot-cli")
	if !ok {
		t.Fatal(`registry.Agents.Meta("copilot-cli") reported not registered`)
	}
	if !meta.RequiresCommand {
		t.Error("AgentMeta.RequiresCommand = false, want true")
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
	if state.target.WorkspacePath != workspace {
		// t.TempDir() may return a path through a symlink; compare with os.Stat.
		if state.target.WorkspacePath != filepath.Clean(workspace) {
			t.Errorf("state.target.WorkspacePath = %q, want %q", state.target.WorkspacePath, workspace)
		}
	}
	if state.fallbackToContinue {
		t.Error("state.fallbackToContinue = true, want false for new session")
	}
	if state.target.SSHHost != "" {
		t.Errorf("state.target.SSHHost = %q, want empty for local mode", state.target.SSHHost)
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
	if state.target.SSHHost != "dev-host.example.com" {
		t.Errorf("state.target.SSHHost = %q, want %q", state.target.SSHHost, "dev-host.example.com")
	}
	if state.target.RemoteCommand != "copilot" {
		t.Errorf("state.target.RemoteCommand = %q, want %q", state.target.RemoteCommand, "copilot")
	}
	if state.target.Command != sshPath {
		t.Errorf("state.target.Command = %q, want %q (ssh binary)", state.target.Command, sshPath)
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
	if state.target.SSHHost != "" {
		t.Errorf("state.target.SSHHost = %q, want empty (whitespace-only SSHHost treated as local mode)", state.target.SSHHost)
	}
	if state.target.RemoteCommand != "" {
		t.Errorf("state.target.RemoteCommand = %q, want empty for local mode", state.target.RemoteCommand)
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
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

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

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)

	agenttest.AssertModelReported(t, events, "")
}

func TestRunTurn_ExitCode127(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 127)

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
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 1)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero exit")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     1,
		Work:         agentcore.WorkAbsent,
	}, result, err)
}

func TestRunTurn_NoOutputExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 0)

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

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
	}, result, err)
}

func TestRunTurn_PartialOutputNoResultExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello","outputTokens":42}}` + "\n"
	state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

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

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

func TestRunTurn_StderrWarnOnNoOutputExitZero(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "Invalid JSON in --additional-mcp-config", 0)

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
	// state.target.Command is fakeCopilotBinary: exits 0 immediately, no output.

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
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "malformed_lines.jsonl"), 0)

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
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "tool_use_session.jsonl"), 0)

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

// TestRunTurn_ToolDeniedContinuesTurn drives a tool.execution_complete
// event whose error.code is "denied", the CLI's own non-interactive
// permission policy refusing a tool call and continuing the session. With
// --no-ask-user closing the human-question path unconditionally, every
// recognized request here is a permission request with no reply channel,
// so the turn must continue rather than end.
func TestRunTurn_ToolDeniedContinuesTurn(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const deniedJSONL = `{"type":"tool.execution_complete","data":{"toolCallId":"tc-1","toolName":"bash","success":false,"error":{"code":"denied","message":"denied by policy"}}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"dd112233-4455-6677-8899-aabbccddeeff","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, deniedJSONL, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q (turn must continue past the runtime's own refusal)", result.ExitReason, domain.EventTurnCompleted)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused).Notice
	var found bool
	for _, ev := range events {
		if ev.Type == domain.EventNotification && ev.Message == wantNotice {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no EventNotification with Message = %q found among events %v", wantNotice, events)
	}
}

func TestRunTurn_NonZeroResultExitCode(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	// JSONL with a result event reporting a non-zero exit code.
	const failResultJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, failResultJSONL, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero result exit code")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:        agentcore.TerminalFailure,
		TerminalMessage: "non-zero exit in result event",
		ExitObserved:    true,
		ExitCode:        0,
		Work:            agentcore.WorkAbsent,
	}, result, err)
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
	state.target.Command = fakeCopilotBinaryWithOutput(t, failJSONL, 0)

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
			target: agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
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
	state.target.Command = sleepBin

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
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "license check failed: no valid license", 127)

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
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "internal agent panic", 1)

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
	const successJSONL = `{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"no-warn-success-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`
	if err := os.WriteFile(outFile, []byte(successJSONL+"\n"), 0o644); err != nil {
		t.Fatalf("writing stdout file: %v", err)
	}
	state.target.Command = agenttest.WriteScript(t, dir, "copilot",
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

// journalPath returns the events.jsonl path readSessionUsage resolves
// for sessionID when COPILOT_HOME is set to copilotHome.
func journalPath(copilotHome, sessionID string) string {
	return filepath.Join(copilotHome, "session-state", sessionID, "events.jsonl")
}

// writeJournal writes content to path, creating parent directories as
// needed.
func writeJournal(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// TestRunTurn_SessionStateRecovery_FirstRecord drives RunTurn with a
// temporary session-state root containing one session.shutdown record
// captured from Copilot CLI 1.0.78, whose modelMetrics reports
// inputTokens 193011, outputTokens 596, cacheReadTokens 154053. It
// asserts the terminal event carries the recovered totals, the
// per-message token_usage event delivered before it carries only the
// stream's output-only provisional count, and exactly one token_usage
// event fires, matching the fixture's single assistant.message.
func TestRunTurn_SessionStateRecovery_FirstRecord(t *testing.T) {
	// No t.Parallel(): t.Setenv is incompatible with it.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	copilotHome := t.TempDir()
	t.Setenv("COPILOT_HOME", copilotHome)

	const sessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	fixture := loadTestFixture(t, "session_shutdown.jsonl")
	lines := strings.Split(strings.TrimRight(fixture, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("session_shutdown.jsonl has %d lines, want at least 2", len(lines))
	}
	writeJournal(t, journalPath(copilotHome, sessionID), lines[0]+"\n")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "say hello",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.SessionID != sessionID {
		t.Fatalf("result.SessionID = %q, want %q", result.SessionID, sessionID)
	}

	wantUsage := domain.TokenUsage{InputTokens: 193011, OutputTokens: 596, TotalTokens: 193607, CacheReadTokens: 154053}
	if result.Usage != wantUsage {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantUsage)
	}

	completed, ok := findEventByType(events, domain.EventTurnCompleted)
	if !ok {
		t.Fatal("EventTurnCompleted not delivered")
	}
	if completed.Usage != wantUsage {
		t.Errorf("EventTurnCompleted.Usage = %+v, want %+v", completed.Usage, wantUsage)
	}

	var tokenUsageEvents []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageEvents = append(tokenUsageEvents, e)
		}
	}
	if len(tokenUsageEvents) != 1 {
		t.Fatalf("token_usage event count = %d, want 1 (one assistant.message in the fixture)", len(tokenUsageEvents))
	}
	if tokenUsageEvents[0].Usage.InputTokens != 0 {
		t.Errorf("token_usage event InputTokens = %d, want 0 (output-only provisional, not the recovered figure)", tokenUsageEvents[0].Usage.InputTokens)
	}
	if tokenUsageEvents[0].Usage.OutputTokens != 6 {
		t.Errorf("token_usage event OutputTokens = %d, want 6", tokenUsageEvents[0].Usage.OutputTokens)
	}

	agenttest.AssertUsageContract(t, events)
}

// TestRunTurn_SessionStateRecovery_BaselineDifference repeats the
// first-record scenario with both session_shutdown.jsonl records
// already present, verifying the reported snapshot is the difference
// between the two records rather than the second record's raw total.
func TestRunTurn_SessionStateRecovery_BaselineDifference(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	copilotHome := t.TempDir()
	t.Setenv("COPILOT_HOME", copilotHome)

	const sessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	writeJournal(t, journalPath(copilotHome, sessionID), loadTestFixture(t, "session_shutdown.jsonl"))

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "say hello",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	wantUsage := domain.TokenUsage{InputTokens: 78913, OutputTokens: 100, TotalTokens: 79013, CacheReadTokens: 78279}
	if result.Usage != wantUsage {
		t.Errorf("TurnResult.Usage = %+v, want %+v (difference between the two records)", result.Usage, wantUsage)
	}
}

// TestRunTurn_UsageMeasured_OutputTokenField covers the outputTokens
// field's role in measurement, independent of the session-state journal
// read, including the boundary case where the journal cannot be read at
// all: SSH mode, one of the runtime conditions the adapter treats the
// same way as an invalid session id or an unreadable file.
func TestRunTurn_UsageMeasured_OutputTokenField(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	t.Run("present zero field measures the run", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello","outputTokens":0}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"zero-output-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

		var events []domain.AgentEvent
		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}

		tokenUsageEvents := 0
		for _, e := range events {
			if e.Type == domain.EventTokenUsage {
				tokenUsageEvents++
			}
		}
		if tokenUsageEvents != 1 {
			t.Errorf("token_usage event count = %d, want 1", tokenUsageEvents)
		}
		if !result.UsageMeasured {
			t.Error("RunTurn().UsageMeasured = false, want true for a present zero outputTokens field")
		}
	})

	t.Run("absent field alone leaves the run unmeasured", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello"}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"no-output-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

		var events []domain.AgentEvent
		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}

		agenttest.AssertMeasurementAbsent(t, events, result)
	})

	t.Run("unreadable journal in SSH mode with the field carried still measures the run, output-only", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)
		state.target.RemoteCommand = "copilot"
		state.target.SSHHost = "dev-host.example.com"

		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(domain.AgentEvent) {},
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}

		if !result.UsageMeasured {
			t.Error("RunTurn().UsageMeasured = false, want true when the stream carried the output-token field despite an unreadable session-state journal")
		}
		wantUsage := domain.TokenUsage{OutputTokens: 6, TotalTokens: 6}
		if result.Usage != wantUsage {
			t.Errorf("TurnResult.Usage = %+v, want %+v (output-only, journal unreadable in SSH mode)", result.Usage, wantUsage)
		}
	})

	t.Run("unreadable journal in SSH mode with no field carried leaves the run unmeasured", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello"}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"ssh-no-output-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)
		state.target.RemoteCommand = "copilot"
		state.target.SSHHost = "dev-host.example.com"

		var events []domain.AgentEvent
		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}

		agenttest.AssertMeasurementAbsent(t, events, result)
	})
}

// TestRunTurn_SessionStateRecovery_Degradation drives
// sessionState.recoverUsage directly (bypassing the subprocess) to
// exercise the three read-skipping conditions: an absent events file,
// a session id that fails the path-segment check, and SSH mode. In
// every case the provisional output-only snapshot must stand, and only
// the path-segment rejection logs a Warn.
func TestRunTurn_SessionStateRecovery_Degradation(t *testing.T) {
	t.Run("events file absent", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		state := &sessionState{
			target:            agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
			copilotSessionID:  "no-file-session",
			runCreatedSession: true,
			acc:               agentcore.NewRunUsage(),
		}
		state.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: 42})

		got, _ := state.recoverUsage(logger)
		want := domain.TokenUsage{OutputTokens: 42, TotalTokens: 42}
		if got != want {
			t.Errorf("recoverUsage() = %+v, want %+v (provisional stands)", got, want)
		}
		if strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("unexpected WARN log for a merely-absent events file: %s", buf.String())
		}
	})

	t.Run("session id contains a path separator", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		state := &sessionState{
			target:            agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
			copilotSessionID:  "abc/../def",
			runCreatedSession: true,
			acc:               agentcore.NewRunUsage(),
		}
		state.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: 7})

		got, _ := state.recoverUsage(logger)
		want := domain.TokenUsage{OutputTokens: 7, TotalTokens: 7}
		if got != want {
			t.Errorf("recoverUsage() = %+v, want %+v (provisional stands)", got, want)
		}
		if warnCount := strings.Count(buf.String(), "level=WARN"); warnCount != 1 {
			t.Errorf("WARN log count = %d, want 1 (path-separator rejection)", warnCount)
		}

		// A second finalize for the same run must not repeat the Warn.
		state.recoverUsage(logger)
		if warnCount := strings.Count(buf.String(), "level=WARN"); warnCount != 1 {
			t.Errorf("WARN log count after second call = %d, want 1 (not repeated)", warnCount)
		}
	})

	t.Run("SSH mode skips the read", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		state := &sessionState{
			target: agentcore.LaunchTarget{
				WorkspacePath: t.TempDir(),
				RemoteCommand: "copilot",
				SSHHost:       "dev-host.example.com",
			},
			copilotSessionID:  "valid-session-id",
			runCreatedSession: true,
			acc:               agentcore.NewRunUsage(),
		}
		state.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: 11})

		got, _ := state.recoverUsage(logger)
		want := domain.TokenUsage{OutputTokens: 11, TotalTokens: 11}
		if got != want {
			t.Errorf("recoverUsage() = %+v, want %+v (provisional stands)", got, want)
		}
		if buf.Len() != 0 {
			t.Errorf("unexpected log output in SSH mode: %s", buf.String())
		}
	})
}

// TestRunTurn_SessionStateRecovery_FirstReadAttempt drives
// sessionState.recoverUsage directly across two simulated finalizes,
// the first of which misses its read (the events file does not exist
// yet), and asserts the two ways R24 resolves the resulting baseline
// ambiguity: a run that created the session takes a zero baseline on
// its later successful read, while a run that resumed a session marks
// recovery unavailable for the rest of the run.
func TestRunTurn_SessionStateRecovery_FirstReadAttempt(t *testing.T) {
	t.Run("run created the session takes a zero baseline", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("COPILOT_HOME", root)
		logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))

		state := &sessionState{
			target:            agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
			copilotSessionID:  "created-session",
			runCreatedSession: true,
			acc:               agentcore.NewRunUsage(),
		}

		// First finalize: the events file does not exist yet, so no read
		// attempt succeeds.
		state.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: 5})
		first, _ := state.recoverUsage(logger)
		if first != (domain.TokenUsage{OutputTokens: 5, TotalTokens: 5}) {
			t.Fatalf("first recoverUsage() = %+v, want provisional (5, 5)", first)
		}

		// Second finalize: the runtime has since written both records.
		// Because this run created the session, the baseline is zero
		// rather than the first record's totals, so the run's own
		// earlier spend is not subtracted a second time.
		writeJournal(t, journalPath(root, "created-session"), loadTestFixture(t, "session_shutdown.jsonl"))
		second, _ := state.recoverUsage(logger)
		if second.InputTokens != 271924 {
			t.Errorf("second recoverUsage().InputTokens = %d, want 271924 (zero baseline)", second.InputTokens)
		}
	})

	t.Run("run resumed a session marks recovery unavailable", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("COPILOT_HOME", root)
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		state := &sessionState{
			target:            agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
			copilotSessionID:  "resumed-session",
			runCreatedSession: false,
			acc:               agentcore.NewRunUsage(),
		}

		state.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: 9})
		first, _ := state.recoverUsage(logger)
		if first != (domain.TokenUsage{OutputTokens: 9, TotalTokens: 9}) {
			t.Fatalf("first recoverUsage() = %+v, want provisional (9, 9)", first)
		}

		writeJournal(t, journalPath(root, "resumed-session"), loadTestFixture(t, "session_shutdown.jsonl"))
		second, _ := state.recoverUsage(logger)
		if !state.recoveryUnavailable {
			t.Error("recoveryUnavailable = false, want true")
		}
		if second != (domain.TokenUsage{OutputTokens: 9, TotalTokens: 9}) {
			t.Errorf("second recoverUsage() = %+v, want unchanged provisional (9, 9)", second)
		}
		if warnCount := strings.Count(buf.String(), "level=WARN"); warnCount != 1 {
			t.Errorf("WARN log count = %d, want 1", warnCount)
		}

		// A third finalize must not attempt another read.
		third, _ := state.recoverUsage(logger)
		if third != second {
			t.Errorf("third recoverUsage() = %+v, want unchanged %+v", third, second)
		}
		if warnCount := strings.Count(buf.String(), "level=WARN"); warnCount != 1 {
			t.Errorf("WARN log count after third call = %d, want 1 (no further read attempted)", warnCount)
		}
	})
}

// TestRunTurn_SuccessfulResultZeroOutputTokens pins that a successful
// result event completes the turn even when this turn's own output
// tokens are zero, because a positive terminal report is authoritative
// and never consults Work.
func TestRunTurn_SuccessfulResultZeroOutputTokens(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const successJSONL = `{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"zero-output-success-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, successJSONL, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
	}, result, err)
}

// TestRunTurn_SuccessMessageStaysEmpty is the regression pin for the
// success-message mapping: copilot's result event carries no completion
// text, so the emitted turn_completed event's message stays empty
// unchanged, even on a turn that produced real assistant output.
func TestRunTurn_SuccessMessageStaysEmpty(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	e, ok := findEventByType(events, domain.EventTurnCompleted)
	if !ok {
		t.Fatal("turn_completed event not delivered")
	}
	if e.Message != "" {
		t.Errorf("turn_completed Message = %q, want empty (copilot's result event carries no completion text)", e.Message)
	}
}

// TestRunTurn_WorkPredicateIsPerTurn pins that the work predicate reads
// this turn's own output tokens, not the run cumulative. The first turn
// produces output and completes; the second turn, on the same session,
// produces none and must fail rather than inherit the first turn's
// evidence.
func TestRunTurn_WorkPredicateIsPerTurn(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "turn-count")
	outFile := filepath.Join(tmpDir, "out.jsonl")
	const outputJSONL = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello","outputTokens":10}}` + "\n"
	if err := os.WriteFile(outFile, []byte(outputJSONL), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state.target.Command = agenttest.WriteScript(t, tmpDir, "copilot", fmt.Sprintf(`
if [ -f '%s' ]; then
  exit 0
fi
touch '%s'
cat '%s'
exit 0
`, counterFile, counterFile, outFile))

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (per-turn work predicate must not carry the first turn's output forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
}

// TestRunTurn_PremiumRequestsLoggedOnce pins the premium_requests side
// effect: the Info log keeps firing on exactly today's condition, a
// result event carrying a non-nil usage object, exactly once per turn.
func TestRunTurn_PremiumRequestsLoggedOnce(t *testing.T) {
	// No t.Parallel(): installs a global slog default; t.Setenv is also
	// incompatible with t.Parallel.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const successJSONL = `{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"premium-session","exitCode":0,"usage":{"premiumRequests":3,"totalApiDurationMs":10}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, successJSONL, 0)

	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	var premiumCount int
	for _, e := range spy.Entries() {
		if e.Msg == "copilot turn completed" {
			premiumCount++
		}
	}
	if premiumCount != 1 {
		t.Errorf("premium_requests Info line count = %d, want 1", premiumCount)
	}
}

// TestRunTurn_CompleteVersusIncompleteDisposition drives the two endings
// the autopilot continuation ceiling makes indistinguishable at the exit
// code and process-exit level: a stream that reports session.task_complete
// before its terminal result, and a stream that reaches the same
// exitCode:0 result without ever reporting it. Both pin their disposition
// against agentcore.DecideTurn, and the test additionally asserts the two
// ExitReason values differ, since a discriminator that both endings
// satisfy would defeat the point of the fix.
func TestRunTurn_CompleteVersusIncompleteDisposition(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	completeAdapter, completeSession := newTestSession(t, t.TempDir())
	completeState := completeSession.Internal.(*sessionState)
	completeState.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	completeResult, completeErr := completeAdapter.RunTurn(context.Background(), completeSession, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if completeErr != nil {
		t.Fatalf("RunTurn(complete) error = %v", completeErr)
	}
	if completeResult.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(complete).ExitReason = %q, want %q", completeResult.ExitReason, domain.EventTurnCompleted)
	}
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, completeResult, completeErr)

	incompleteAdapter, incompleteSession := newTestSession(t, t.TempDir())
	incompleteState := incompleteSession.Internal.(*sessionState)
	incompleteState.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "autopilot_no_report.jsonl"), 0)

	incompleteResult, incompleteErr := incompleteAdapter.RunTurn(context.Background(), incompleteSession, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if incompleteResult.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(incomplete).ExitReason = %q, want %q", incompleteResult.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, incompleteErr, domain.ErrTurnIncomplete)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:        agentcore.TerminalIncomplete,
		TerminalMessage: "raise copilot-cli.max_autopilot_continues if the turn needs more steps",
	}, incompleteResult, incompleteErr)

	if completeResult.ExitReason == incompleteResult.ExitReason {
		t.Errorf("ExitReason for the complete and incomplete endings both = %q, want them to differ", completeResult.ExitReason)
	}
}

// TestRunTurn_TaskCompleteSuccessFalse pins the session.task_complete
// "success: false" ending: it surfaces as a domain.ErrTurnFailed carrying
// completionFailureMessage's rendering of the report's own summary, not
// the generic zero-work or non-zero-exit message.
func TestRunTurn_TaskCompleteSuccessFalse(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const summary = "ran into a blocker"
	stream := fmt.Sprintf(`{"type":"session.task_complete","data":{"summary":%q,"success":false}}
{"type":"result","timestamp":"2026-04-08T00:00:04Z","sessionId":"success-false-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0}}`, summary)

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, stream, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   completionFailureMessage(summary),
	}, result, err)
}

// TestRunTurn_TaskCompleteSuccessKeyAbsent pins the adapter-level ending
// for a session.task_complete report whose payload carries no success key
// at all: the turn completes, since the parsed *bool stays nil and the
// adapter's default success posture applies.
func TestRunTurn_TaskCompleteSuccessKeyAbsent(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const stream = `{"type":"session.task_complete","data":{"summary":"all set"}}
{"type":"result","timestamp":"2026-04-08T00:00:05Z","sessionId":"absent-flag-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, stream, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn().ExitReason = %q, want %q (absent success key defaults to success)", result.ExitReason, domain.EventTurnCompleted)
	}
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess}, result, err)
}

// TestRunTurn_IncompleteEndingLogsContinuationCounts pins the diagnostic
// attributes on the adapter's own "copilot turn ended without a
// task-completion report" warning: autopilot_continuations_observed
// counts the isAutopilotContinuation lines this turn actually saw, and
// max_autopilot_continues reports the session's effective ceiling.
//
// agenttest.LogSpy only extracts the "line" attribute it was built for
// (see TestLogSpy_Handle_OtherAttrsIgnored in that package) and has no
// way to report other attributes' values, so this test installs its own
// buffer-backed slog.TextHandler as the default logger instead, the
// pattern this project's own testing guidance documents for asserting on
// structured log output.
func TestRunTurn_IncompleteEndingLogsContinuationCounts(t *testing.T) {
	// No t.Parallel() at the top level: installs a global slog default;
	// t.Setenv is also incompatible with t.Parallel.
	var logOutput bytes.Buffer
	previousDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousDefault) })

	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "autopilot_no_report.jsonl"), 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Fatalf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnIncomplete)

	got := logOutput.String()
	if !strings.Contains(got, `msg="copilot turn ended without a task-completion report"`) {
		t.Errorf("log output missing the expected warn message; got:\n%s", got)
	}
	if !strings.Contains(got, "autopilot_continuations_observed=1") {
		t.Errorf("log output missing autopilot_continuations_observed=1 (the fixture carries exactly one isAutopilotContinuation line); got:\n%s", got)
	}
	if !strings.Contains(got, "max_autopilot_continues=50") {
		t.Errorf("log output missing max_autopilot_continues=50 (the unconfigured default); got:\n%s", got)
	}
}

// dropLastFixtureLine returns content with its final non-empty line
// removed, preserving a trailing newline on what remains.
func dropLastFixtureLine(t *testing.T, content string) string {
	t.Helper()
	trimmed := strings.TrimRight(content, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) < 2 {
		t.Fatalf("dropLastFixtureLine: content has too few lines (%d) to drop one", len(lines))
	}
	return strings.Join(lines[:len(lines)-1], "\n") + "\n"
}

func TestRunTurn_ModelMessageRelocation(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	t.Run("five model.message measurements sum to the captured total", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "model_message_session.jsonl"), 0)

		var events []domain.AgentEvent
		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}

		tokenUsageEvents := 0
		for _, e := range events {
			if e.Type == domain.EventTokenUsage {
				tokenUsageEvents++
			}
			if e.Type == domain.EventOtherMessage && (e.Message == "model.message" || e.Message == "model.messages_snapshot") {
				t.Errorf("event carries EventOtherMessage for %q, want it never routed to the default arm", e.Message)
			}
		}
		if tokenUsageEvents != 5 {
			t.Errorf("token_usage event count = %d, want 5", tokenUsageEvents)
		}

		const wantOutputTokens int64 = 448
		if result.Usage.OutputTokens != wantOutputTokens {
			t.Errorf("result.Usage.OutputTokens = %d, want %d", result.Usage.OutputTokens, wantOutputTokens)
		}
		if result.Usage.TotalTokens != result.Usage.InputTokens+result.Usage.OutputTokens {
			t.Errorf("result.Usage.TotalTokens = %d, want InputTokens+OutputTokens = %d",
				result.Usage.TotalTokens, result.Usage.InputTokens+result.Usage.OutputTokens)
		}
		if !result.UsageMeasured {
			t.Error("result.UsageMeasured = false, want true")
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
		}

		agenttest.AssertUsageContract(t, events)
		agenttest.AssertModelReported(t, events, "claude-sonnet-5")
	})

	t.Run("work evidence holds with the terminal result line removed", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		content := dropLastFixtureLine(t, loadTestFixture(t, "model_message_session.jsonl"))
		state.target.Command = fakeCopilotBinaryWithOutput(t, content, 0)

		result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt:  "say hello",
			OnEvent: func(domain.AgentEvent) {},
		})
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
		}
	})
}

func TestRunTurn_ModelMessageNoOutputTokens(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	const jsonl = `{"type":"model.message","timestamp":"2026-04-08T00:00:00Z","data":{"kind":"message","turn":0,"message":{"role":"assistant","apiCallId":"no-output-call"}}}
{"type":"model.message","timestamp":"2026-04-08T00:00:00Z","data":{"kind":"message","turn":0,"message":{"role":"tool","tool_call_id":"tool-1"}}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"model-message-no-tokens-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "say hello",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertMeasurementAbsent(t, events, result)
}

// TestRunTurn_DedupeAcrossShapes drives a stream carrying both wire
// shapes for one apiCallId. No measured Copilot CLI version emits both
// shapes for the same call; this stream is synthetic, built to exercise
// the dedupe guard rather than to represent captured evidence.
func TestRunTurn_DedupeAcrossShapes(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	const jsonl = `{"type":"model.message","timestamp":"2026-04-08T00:00:00Z","data":{"kind":"message","turn":0,"message":{"role":"assistant","apiCallId":"dedupe-call-1","outputTokens":50}}}
{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"apiCallId":"dedupe-call-1","content":"hello","outputTokens":99}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"dedupe-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "say hello",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := 0
	var lastUsage domain.TokenUsage
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageEvents++
			lastUsage = e.Usage
		}
	}
	if tokenUsageEvents != 1 {
		t.Fatalf("token_usage event count = %d, want 1", tokenUsageEvents)
	}

	const wantOutputTokens int64 = 50
	if lastUsage.OutputTokens != wantOutputTokens {
		t.Errorf("token_usage event Usage.OutputTokens = %d, want %d (the first-sighted model.message value)", lastUsage.OutputTokens, wantOutputTokens)
	}
	if result.Usage.OutputTokens != wantOutputTokens {
		t.Errorf("result.Usage.OutputTokens = %d, want %d (the first-sighted model.message value)", result.Usage.OutputTokens, wantOutputTokens)
	}
}

// TestRunTurn_ReplaySnapshotExcluded drives a stream carrying a
// model.messages_snapshot whose replayed assistant record names an
// apiCallId no live record of the stream carried. No measured Copilot
// CLI version emits a replay snapshot with an identifier absent from
// its own stream; this stream is synthetic, built to exercise
// exclusion of the replay event rather than to represent captured
// evidence.
func TestRunTurn_ReplaySnapshotExcluded(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	const jsonl = `{"type":"model.message","timestamp":"2026-04-08T00:00:00Z","data":{"kind":"message","turn":0,"message":{"role":"assistant","apiCallId":"live-call-1","outputTokens":30}}}
{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"apiCallId":"live-call-1","content":"hello"}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"model.messages_snapshot","timestamp":"2026-04-08T00:00:01Z","data":{"kind":"messages_snapshot","messages":[{"role":"assistant","apiCallId":"replay-only-call","outputTokens":999}]}}
{"type":"result","timestamp":"2026-04-08T00:00:02Z","sessionId":"replay-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}
`
	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "say hello",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	tokenUsageEvents := 0
	for _, e := range events {
		if e.Type == domain.EventTokenUsage {
			tokenUsageEvents++
		}
	}
	if tokenUsageEvents != 1 {
		t.Errorf("token_usage event count = %d, want 1 (the snapshot must contribute none)", tokenUsageEvents)
	}

	const wantOutputTokens int64 = 30
	if result.Usage.OutputTokens != wantOutputTokens {
		t.Errorf("result.Usage.OutputTokens = %d, want %d (the run-cumulative total excludes the snapshot-only identifier)", result.Usage.OutputTokens, wantOutputTokens)
	}
}
