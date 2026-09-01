package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// wantAllowedToolsDiagMessage is the exact diagnostic message
// validateConfig returns when allowed_tools replaces the blanket grant.
const wantAllowedToolsDiagMessage = "copilot-cli.allowed_tools replaces the --allow-all grant, so only a call the list matches " +
	"is approved; every other permissioned call is denied without a prompt, the turn continues, " +
	"and a turn whose calls were all denied still reports success"

// TestValidateConfig_TypeFaultNoDrift covers the R6.5 no-drift property: a
// wrong-typed value for a key the constructor reads fails
// NewCopilotAdapter with a plain error carrying the fault message, and
// validateConfig reports the identical fault text under a
// "copilot-cli.<key>.wrong_type" check for the same input, so the two
// surfaces cannot diverge on what counts as a type fault.
func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"allowed_tools": 123}

	_, constructErr := NewCopilotAdapter(config)
	if constructErr == nil {
		t.Fatal("NewCopilotAdapter(allowed_tools=123) error = nil, want non-nil")
	}
	if constructErr.Error() != "allowed_tools: expected string, got integer" {
		t.Errorf("NewCopilotAdapter(allowed_tools=123) error = %q, want %q", constructErr.Error(), "allowed_tools: expected string, got integer")
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "copilot-cli", Passthrough: config})

	if len(diags) != 1 {
		t.Fatalf("validateConfig(allowed_tools=123) returned %d diagnostics, want 1: %+v", len(diags), diags)
	}
	if diags[0].Check != "copilot-cli.allowed_tools.wrong_type" {
		t.Errorf("validateConfig(allowed_tools=123)[0].Check = %q, want %q", diags[0].Check, "copilot-cli.allowed_tools.wrong_type")
	}
	if diags[0].Message != constructErr.Error() {
		t.Errorf("validateConfig(allowed_tools=123)[0].Message = %q, want the same text the constructor failed with: %q", diags[0].Message, constructErr.Error())
	}
}

// TestValidateConfig covers copilot-cli's allowed_tools diagnostic: only a
// non-whitespace allowed_tools value draws a warning, because that
// allow-list replaces the blanket grant the runtime otherwise passes.
// denied_tools, available_tools, and excluded_tools compose with the grant
// and draw no diagnostic, alone or together, with or without allowed_tools.
func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		passthrough   map[string]any
		wantDiagCount int
	}{
		{
			name:          "empty passthrough draws no diagnostic",
			passthrough:   map[string]any{},
			wantDiagCount: 0,
		},
		{
			name:          "denied_tools alone draws no diagnostic",
			passthrough:   map[string]any{"denied_tools": "write"},
			wantDiagCount: 0,
		},
		{
			name:          "available_tools alone draws no diagnostic",
			passthrough:   map[string]any{"available_tools": "shell"},
			wantDiagCount: 0,
		},
		{
			name:          "excluded_tools alone draws no diagnostic",
			passthrough:   map[string]any{"excluded_tools": "web_fetch"},
			wantDiagCount: 0,
		},
		{
			name: "denied_tools, available_tools, and excluded_tools together draw no diagnostic",
			passthrough: map[string]any{
				"denied_tools":    "write",
				"available_tools": "shell",
				"excluded_tools":  "web_fetch",
			},
			wantDiagCount: 0,
		},
		{
			name:          "allowed_tools alone draws a warning",
			passthrough:   map[string]any{"allowed_tools": "shell"},
			wantDiagCount: 1,
		},
		{
			name:          "allowed_tools with denied_tools draws a warning",
			passthrough:   map[string]any{"allowed_tools": "shell", "denied_tools": "write"},
			wantDiagCount: 1,
		},
		{
			name:          "allowed_tools with available_tools draws a warning",
			passthrough:   map[string]any{"allowed_tools": "shell", "available_tools": "shell"},
			wantDiagCount: 1,
		},
		{
			name:          "allowed_tools with excluded_tools draws a warning",
			passthrough:   map[string]any{"allowed_tools": "shell", "excluded_tools": "web_fetch"},
			wantDiagCount: 1,
		},
		{
			name:          "allowed_tools whitespace-only draws no diagnostic",
			passthrough:   map[string]any{"allowed_tools": "   "},
			wantDiagCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "copilot-cli", Passthrough: tt.passthrough})

			if len(got) != tt.wantDiagCount {
				t.Fatalf("validateConfig(%v) returned %d diagnostics, want %d: %+v", tt.passthrough, len(got), tt.wantDiagCount, got)
			}
			if tt.wantDiagCount == 0 {
				return
			}
			if got[0].Severity != "warning" {
				t.Errorf("validateConfig(%v)[0].Severity = %q, want %q", tt.passthrough, got[0].Severity, "warning")
			}
			if got[0].Check != "copilot-cli.allowed_tools.auto_deny" {
				t.Errorf("validateConfig(%v)[0].Check = %q, want %q", tt.passthrough, got[0].Check, "copilot-cli.allowed_tools.auto_deny")
			}
			if got[0].Message != wantAllowedToolsDiagMessage {
				t.Errorf("validateConfig(%v)[0].Message = %q, want %q", tt.passthrough, got[0].Message, wantAllowedToolsDiagMessage)
			}
		})
	}
}
