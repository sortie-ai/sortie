package copilot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// fakeCopilotBinary creates a minimal shell script at a temp path that
// exits 0 for any invocation (including the --version canary check).
func fakeCopilotBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "copilot")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("creating fake copilot binary: %v", err)
	}
	return path
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
	meta := registry.Agents.Meta("copilot-cli")
	if !meta.RequiresCommand {
		t.Error("AdapterMeta.RequiresCommand = false, want true")
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
