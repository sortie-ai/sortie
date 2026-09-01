package opencode

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

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

// TestValidateConfig_TypeFaultNoDrift covers the no-drift property: a
// wrong-typed value for a key the constructor reads fails
// NewOpenCodeAdapter with a plain error carrying the fault message, and
// validateConfig reports the identical fault text under an
// "opencode.<key>.wrong_type" check for the same input, so the two
// surfaces cannot diverge on what counts as a type fault.
func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"model": 123}

	_, constructErr := NewOpenCodeAdapter(config)
	if constructErr == nil {
		t.Fatal("NewOpenCodeAdapter(model=123) error = nil, want non-nil")
	}
	if constructErr.Error() != "model: expected string, got integer" {
		t.Errorf("NewOpenCodeAdapter(model=123) error = %q, want %q", constructErr.Error(), "model: expected string, got integer")
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: config})

	diag := hasCheck(got, "opencode.model.wrong_type")
	if diag == nil {
		t.Fatalf("validateConfig(model=123) missing check %q; got %+v", "opencode.model.wrong_type", got)
	}
	if diag.Message != constructErr.Error() {
		t.Errorf("validateConfig(model=123) check %q Message = %q, want the same text the constructor failed with: %q", diag.Check, diag.Message, constructErr.Error())
	}
}

// TestValidateConfig_TypeFaultAndToolOverlapBothReported covers the combination:
// a passthrough carrying both a mistyped string key and an overlapping
// allowed_tools/denied_tools pair fails NewOpenCodeAdapter with the type
// fault (parsePassthroughConfig runs before checkCrossField), while
// validateConfig, which does not gate the overlap check on the funnel's
// fault, reports both diagnostics offline.
func TestValidateConfig_TypeFaultAndToolOverlapBothReported(t *testing.T) {
	t.Parallel()

	config := map[string]any{
		"model":         123,
		"allowed_tools": []any{"bash"},
		"denied_tools":  []any{"bash"},
	}

	_, constructErr := NewOpenCodeAdapter(config)
	if constructErr == nil {
		t.Fatal("NewOpenCodeAdapter(model=123, overlapping tools) error = nil, want the type fault")
	}
	if constructErr.Error() != "model: expected string, got integer" {
		t.Errorf("NewOpenCodeAdapter(model=123, overlapping tools) error = %q, want the type fault, not the overlap error", constructErr.Error())
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: config})

	if hasCheck(got, "opencode.model.wrong_type") == nil {
		t.Errorf("validateConfig(model=123, overlapping tools) missing check %q; got %+v", "opencode.model.wrong_type", got)
	}
	if hasCheck(got, "opencode.allowed_tools.overlap") == nil {
		t.Errorf("validateConfig(model=123, overlapping tools) missing check %q; got %+v", "opencode.allowed_tools.overlap", got)
	}
}
