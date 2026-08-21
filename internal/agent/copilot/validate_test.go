package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// TestValidateConfig covers copilot-cli's tool-scoping diagnostic: absent
// tool scoping draws no diagnostic, and each of the four keys that
// displace --allow-all (allowed_tools, denied_tools, available_tools,
// excluded_tools) draws a warning-severity diagnostic on its own, since
// the CLI's own non-interactive permission policy denies and continues
// past a scoped-out tool call rather than stalling.
func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		passthrough   map[string]any
		wantDiagCount int
	}{
		{
			name:          "no tool scoping draws no diagnostic",
			passthrough:   map[string]any{},
			wantDiagCount: 0,
		},
		{
			name:          "allowed_tools draws a warning",
			passthrough:   map[string]any{"allowed_tools": "bash"},
			wantDiagCount: 1,
		},
		{
			name:          "denied_tools draws a warning",
			passthrough:   map[string]any{"denied_tools": "bash"},
			wantDiagCount: 1,
		},
		{
			name:          "available_tools draws a warning",
			passthrough:   map[string]any{"available_tools": "bash"},
			wantDiagCount: 1,
		},
		{
			name:          "excluded_tools draws a warning",
			passthrough:   map[string]any{"excluded_tools": "bash"},
			wantDiagCount: 1,
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
			if got[0].Check != "copilot-cli.tool_scoping.interactive" {
				t.Errorf("validateConfig(%v)[0].Check = %q, want %q", tt.passthrough, got[0].Check, "copilot-cli.tool_scoping.interactive")
			}
			if got[0].Message == "" {
				t.Errorf("validateConfig(%v)[0].Message = \"\", want non-empty", tt.passthrough)
			}
		})
	}
}
