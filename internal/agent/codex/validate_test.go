package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"approval_policy": 123}
	recordPath := filepath.Join(t.TempDir(), "frames")
	command := agenttest.FakeRuntime(t, t.TempDir(), "codex", scenarioRecordFrames, recordFramesParams{RecordPath: recordPath})

	_, startErr := (&CodexAdapter{}).StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
		Settings:      config,
	})
	var agentErr *domain.AgentError
	var fault *typeutil.TypeFault
	if !errors.As(startErr, &agentErr) || agentErr.Kind != domain.ErrAgentNotFound || !errors.As(startErr, &fault) || fault.Key != "approval_policy" {
		t.Fatalf("StartSession(approval_policy=123) error = %v, want an agent_not_found *domain.AgentError wrapping a *typeutil.TypeFault for %q", startErr, "approval_policy")
	}
	if _, statErr := os.Stat(recordPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("StartSession(approval_policy=123) launched the runtime: stat %q error = %v, want not exist", recordPath, statErr)
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "codex", Passthrough: config})

	if len(diags) != 1 {
		t.Fatalf("validateConfig(approval_policy=123) returned %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Check != "codex.approval_policy.wrong_type" {
		t.Errorf("validateConfig(approval_policy=123)[0].Check = %q, want %q", diags[0].Check, "codex.approval_policy.wrong_type")
	}
	if diags[0].Message != agentErr.Message {
		t.Errorf("validateConfig(approval_policy=123)[0].Message = %q, want the text StartSession refused with: %q", diags[0].Message, agentErr.Message)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		passthrough   map[string]any
		wantDiagCount int
		wantCheck     string
	}{
		{
			name:          "absent approval_policy draws no diagnostic",
			passthrough:   map[string]any{},
			wantDiagCount: 0,
		},
		{
			name:          `approval_policy "never" draws no diagnostic`,
			passthrough:   map[string]any{"approval_policy": "never"},
			wantDiagCount: 0,
		},
		{
			name:          `approval_policy "untrusted" draws an error`,
			passthrough:   map[string]any{"approval_policy": "untrusted"},
			wantDiagCount: 1,
			wantCheck:     "codex.approval_policy.interactive",
		},
		{
			name:          `approval_policy "on-request" draws an error`,
			passthrough:   map[string]any{"approval_policy": "on-request"},
			wantDiagCount: 1,
			wantCheck:     "codex.approval_policy.interactive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "codex", Passthrough: tt.passthrough})

			if len(got) != tt.wantDiagCount {
				t.Fatalf("validateConfig(%v) returned %d diagnostics, want %d: %+v", tt.passthrough, len(got), tt.wantDiagCount, got)
			}
			if tt.wantDiagCount == 0 {
				return
			}
			if got[0].Severity != "error" {
				t.Errorf("validateConfig(%v)[0].Severity = %q, want %q", tt.passthrough, got[0].Severity, "error")
			}
			if got[0].Check != tt.wantCheck {
				t.Errorf("validateConfig(%v)[0].Check = %q, want %q", tt.passthrough, got[0].Check, tt.wantCheck)
			}
			if got[0].Message == "" {
				t.Errorf("validateConfig(%v)[0].Message = \"\", want non-empty", tt.passthrough)
			}
		})
	}
}
