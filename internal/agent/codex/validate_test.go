package codex

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// TestValidateConfig_TypeFaultNoDrift covers the no-drift property: a
// wrong-typed value for a key the constructor reads fails NewCodexAdapter
// with a plain error carrying the fault message, and validateConfig
// reports the identical fault text under a "codex.<key>.wrong_type" check
// for the same input, so the two surfaces cannot diverge on what counts
// as a type fault.
func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"approval_policy": 123}

	_, constructErr := NewCodexAdapter(config)
	if constructErr == nil {
		t.Fatal("NewCodexAdapter(approval_policy=123) error = nil, want non-nil")
	}
	if constructErr.Error() != "approval_policy: expected string, got integer" {
		t.Errorf("NewCodexAdapter(approval_policy=123) error = %q, want %q", constructErr.Error(), "approval_policy: expected string, got integer")
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "codex", Passthrough: config})

	if len(diags) != 1 {
		t.Fatalf("validateConfig(approval_policy=123) returned %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Check != "codex.approval_policy.wrong_type" {
		t.Errorf("validateConfig(approval_policy=123)[0].Check = %q, want %q", diags[0].Check, "codex.approval_policy.wrong_type")
	}
	if diags[0].Message != constructErr.Error() {
		t.Errorf("validateConfig(approval_policy=123)[0].Message = %q, want the same text the constructor failed with: %q", diags[0].Message, constructErr.Error())
	}
}

// TestValidateConfig covers codex.approval_policy: absent or "never"
// draws no diagnostic, and any other value draws an error-severity
// diagnostic checked "codex.approval_policy.interactive".
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
