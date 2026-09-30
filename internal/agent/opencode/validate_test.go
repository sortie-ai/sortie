package opencode

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

func startRefusedSession(t *testing.T, settings map[string]any) (err error, launched bool) {
	t.Helper()

	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	command := agenttest.FakeRuntime(t, dir, "opencode", agenttest.RecordedEnvScenario, agenttest.RecordedEnv{Path: marker, Names: []string{"PATH"}})
	a, _ := NewOpenCodeAdapter()

	_, err = a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: command},
		Settings:      settings,
	})
	_, statErr := os.Stat(marker)
	return err, statErr == nil
}

// hasCheck reports whether diags carries a diagnostic with the given
// check name.
func hasCheck(diags []registry.ValidationDiag, check string) *registry.ValidationDiag {
	for i := range diags {
		if diags[i].Check == check {
			return &diags[i]
		}
	}
	return nil
}

// TestValidateConfig_SkipPermissions covers
// opencode.dangerously_skip_permissions: absent or true draws no
// diagnostic, and an explicit false draws a warning-severity diagnostic.
func TestValidateConfig_SkipPermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		wantWarn    bool
	}{
		{name: "absent draws no diagnostic", passthrough: map[string]any{}, wantWarn: false},
		{name: "explicit true draws no diagnostic", passthrough: map[string]any{"dangerously_skip_permissions": true}, wantWarn: false},
		{name: "explicit false draws a warning", passthrough: map[string]any{"dangerously_skip_permissions": false}, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: tt.passthrough})

			diag := hasCheck(got, "opencode.dangerously_skip_permissions.auto_reject")
			if tt.wantWarn && diag == nil {
				t.Fatalf("validateConfig(%v) missing check %q; got %+v", tt.passthrough, "opencode.dangerously_skip_permissions.auto_reject", got)
			}
			if !tt.wantWarn && diag != nil {
				t.Fatalf("validateConfig(%v) has unexpected check %q; got %+v", tt.passthrough, "opencode.dangerously_skip_permissions.auto_reject", got)
			}
			if diag != nil && diag.Severity != "warning" {
				t.Errorf("validateConfig(%v) check %q Severity = %q, want %q", tt.passthrough, diag.Check, diag.Severity, "warning")
			}
		})
	}
}

// TestValidateConfig_ToolOverlap covers allowed_tools/denied_tools
// overlap: an error-severity diagnostic when the two lists name at least
// one common tool, none when they do not.
func TestValidateConfig_ToolOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		wantErr     bool
	}{
		{
			name:        "no overlap draws no diagnostic",
			passthrough: map[string]any{"allowed_tools": []any{"edit"}, "denied_tools": []any{"bash"}},
			wantErr:     false,
		},
		{
			name:        "overlapping tool draws an error",
			passthrough: map[string]any{"allowed_tools": []any{"bash"}, "denied_tools": []any{"bash"}},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: tt.passthrough})

			diag := hasCheck(got, "opencode.allowed_tools.overlap")
			if tt.wantErr && diag == nil {
				t.Fatalf("validateConfig(%v) missing check %q; got %+v", tt.passthrough, "opencode.allowed_tools.overlap", got)
			}
			if !tt.wantErr && diag != nil {
				t.Fatalf("validateConfig(%v) has unexpected check %q; got %+v", tt.passthrough, "opencode.allowed_tools.overlap", got)
			}
			if diag != nil && diag.Severity != "error" {
				t.Errorf("validateConfig(%v) check %q Severity = %q, want %q", tt.passthrough, diag.Check, diag.Severity, "error")
			}
		})
	}
}

func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"model": 123}

	startErr, launched := startRefusedSession(t, config)
	var agentErr *domain.AgentError
	fault, _ := errors.AsType[*typeutil.TypeFault](startErr)
	if !errors.As(startErr, &agentErr) || agentErr.Kind != domain.ErrAgentNotFound || fault == nil || fault.Key != "model" || launched {
		t.Fatalf("StartSession(model=123) error, launched = %v, %v, want an agent_not_found *domain.AgentError wrapping a *typeutil.TypeFault for %q, no launch", startErr, launched, "model")
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: config})

	diag := hasCheck(got, "opencode.model.wrong_type")
	if diag == nil {
		t.Fatalf("validateConfig(model=123) missing check %q; got %+v", "opencode.model.wrong_type", got)
	}
	if diag.Message != agentErr.Message {
		t.Errorf("validateConfig(model=123) check %q Message = %q, want the text StartSession refused with: %q", diag.Check, diag.Message, agentErr.Message)
	}
}

func TestValidateConfig_TypeFaultAndToolOverlapBothReported(t *testing.T) {
	t.Parallel()

	config := map[string]any{
		"model":         123,
		"allowed_tools": []any{"bash"},
		"denied_tools":  []any{"bash"},
	}

	startErr, _ := startRefusedSession(t, config)
	if _, ok := errors.AsType[*typeutil.TypeFault](startErr); !ok {
		t.Errorf("StartSession(model=123, overlapping tools) error = %v, want the type fault, not the overlap error", startErr)
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: config})

	if hasCheck(got, "opencode.model.wrong_type") == nil {
		t.Errorf("validateConfig(model=123, overlapping tools) missing check %q; got %+v", "opencode.model.wrong_type", got)
	}
	if hasCheck(got, "opencode.allowed_tools.overlap") == nil {
		t.Errorf("validateConfig(model=123, overlapping tools) missing check %q; got %+v", "opencode.allowed_tools.overlap", got)
	}
}
