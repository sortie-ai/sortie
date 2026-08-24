package claude

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// TestValidateConfig covers claude-code.permission_mode: absent or
// "bypassPermissions" draws no diagnostic, and every other named mode plus
// an arbitrary unrecognized string draws an error-severity diagnostic,
// since the allowlist direction means an unlisted future mode is refused
// rather than silently passed.
func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		passthrough   map[string]any
		wantDiagCount int
	}{
		{
			name:          "absent permission_mode draws no diagnostic",
			passthrough:   map[string]any{},
			wantDiagCount: 0,
		},
		{
			name:          `permission_mode "bypassPermissions" draws no diagnostic`,
			passthrough:   map[string]any{"permission_mode": "bypassPermissions"},
			wantDiagCount: 0,
		},
		{
			name:          `permission_mode "acceptEdits" draws an error`,
			passthrough:   map[string]any{"permission_mode": "acceptEdits"},
			wantDiagCount: 1,
		},
		{
			name:          `permission_mode "auto" draws an error`,
			passthrough:   map[string]any{"permission_mode": "auto"},
			wantDiagCount: 1,
		},
		{
			name:          `permission_mode "manual" draws an error`,
			passthrough:   map[string]any{"permission_mode": "manual"},
			wantDiagCount: 1,
		},
		{
			name:          `permission_mode "dontAsk" draws an error`,
			passthrough:   map[string]any{"permission_mode": "dontAsk"},
			wantDiagCount: 1,
		},
		{
			name:          `permission_mode "plan" draws an error`,
			passthrough:   map[string]any{"permission_mode": "plan"},
			wantDiagCount: 1,
		},
		{
			name:          "arbitrary unrecognized mode draws an error",
			passthrough:   map[string]any{"permission_mode": "somethingFutureRuntimeAdds"},
			wantDiagCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "claude-code", Passthrough: tt.passthrough})

			if len(got) != tt.wantDiagCount {
				t.Fatalf("validateConfig(%v) returned %d diagnostics, want %d: %+v", tt.passthrough, len(got), tt.wantDiagCount, got)
			}
			if tt.wantDiagCount == 0 {
				return
			}
			if got[0].Severity != "error" {
				t.Errorf("validateConfig(%v)[0].Severity = %q, want %q", tt.passthrough, got[0].Severity, "error")
			}
			if got[0].Check != "claude-code.permission_mode.interactive" {
				t.Errorf("validateConfig(%v)[0].Check = %q, want %q", tt.passthrough, got[0].Check, "claude-code.permission_mode.interactive")
			}
			if got[0].Message == "" {
				t.Errorf("validateConfig(%v)[0].Message = \"\", want non-empty", tt.passthrough)
			}
		})
	}
}

// TestSessionResumeBlockedBy covers the claude-code declaration directly:
// disabling session_persistence returns the blocking key, and the key
// being absent, true, wrong-typed, or read from a nil map all return the
// empty string, matching typeutil.BoolFrom's own defaulting behavior.
func TestSessionResumeBlockedBy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		want        string
	}{
		{
			name:        "session_persistence false returns the blocking key",
			passthrough: map[string]any{"session_persistence": false},
			want:        "session_persistence",
		},
		{
			name:        "session_persistence absent returns empty",
			passthrough: map[string]any{},
			want:        "",
		},
		{
			name:        "session_persistence true returns empty",
			passthrough: map[string]any{"session_persistence": true},
			want:        "",
		},
		{
			name:        "session_persistence wrong-typed returns empty",
			passthrough: map[string]any{"session_persistence": "false"},
			want:        "",
		},
		{
			name:        "nil map returns empty",
			passthrough: nil,
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sessionResumeBlockedBy(tt.passthrough); got != tt.want {
				t.Errorf("sessionResumeBlockedBy(%v) = %q, want %q", tt.passthrough, got, tt.want)
			}
		})
	}
}
