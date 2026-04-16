//go:build unix

package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

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

func TestNewCodexAdapter(t *testing.T) {
	t.Parallel()

	t.Run("nil config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(nil)
		if err != nil {
			t.Fatalf("NewCodexAdapter(nil) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("empty config returns adapter", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{})
		if err != nil {
			t.Fatalf("NewCodexAdapter(empty) error = %v", err)
		}
		if adapter == nil {
			t.Fatal("adapter is nil")
		}
	})

	t.Run("all passthrough fields stored", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{
			"model":           "o4-mini",
			"effort":          "high",
			"approval_policy": "never",
			"thread_sandbox":  "workspaceWrite",
			"personality":     "helpful",
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := adapter.(*CodexAdapter)
		if a.passthrough.Model != "o4-mini" {
			t.Errorf("passthrough.Model = %q, want %q", a.passthrough.Model, "o4-mini")
		}
		if a.passthrough.Effort != "high" {
			t.Errorf("passthrough.Effort = %q, want %q", a.passthrough.Effort, "high")
		}
		if a.passthrough.ApprovalPolicy != "never" {
			t.Errorf("passthrough.ApprovalPolicy = %q, want %q", a.passthrough.ApprovalPolicy, "never")
		}
		if a.passthrough.ThreadSandbox != "workspaceWrite" {
			t.Errorf("passthrough.ThreadSandbox = %q, want %q", a.passthrough.ThreadSandbox, "workspaceWrite")
		}
		if a.passthrough.Personality != "helpful" {
			t.Errorf("passthrough.Personality = %q, want %q", a.passthrough.Personality, "helpful")
		}
	})

	t.Run("tool_registry stored when provided", func(t *testing.T) {
		t.Parallel()
		reg := domain.NewToolRegistry()
		adapter, err := NewCodexAdapter(map[string]any{
			"tool_registry": reg,
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := adapter.(*CodexAdapter)
		if a.toolRegistry != reg {
			t.Error("toolRegistry not stored on adapter")
		}
	})

	t.Run("non-registry tool_registry value is ignored", func(t *testing.T) {
		t.Parallel()
		adapter, err := NewCodexAdapter(map[string]any{
			"tool_registry": "not-a-registry",
		})
		if err != nil {
			t.Fatalf("NewCodexAdapter() error = %v", err)
		}
		a := adapter.(*CodexAdapter)
		if a.toolRegistry != nil {
			t.Error("toolRegistry should be nil for invalid type")
		}
	})
}

func TestEventStream(t *testing.T) {
	t.Parallel()

	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}
	if ch := adapter.EventStream(); ch != nil {
		t.Errorf("EventStream() = %v, want nil", ch)
	}
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("codex")
	if err != nil {
		t.Fatalf(`registry.Agents.Get("codex") error = %v`, err)
	}
	adapter, err := factory(map[string]any{})
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*CodexAdapter); !ok {
		t.Errorf("factory() type = %T, want *CodexAdapter", adapter)
	}
}

func TestStartSession_EmptyWorkspace(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_NonexistentPath(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: "/nonexistent/path/that/does/not/exist/codex-test",
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_WorkspaceIsFile(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpFile,
		AgentConfig:   domain.AgentConfig{Command: "codex app-server"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestStartSession_BinaryNotFound(t *testing.T) {
	t.Parallel()

	adapter, _ := NewCodexAdapter(map[string]any{})
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}
