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
