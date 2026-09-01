package kiro

import (
	"strings"
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

// TestValidateConfig covers the trust_tools conflict: an error-severity
// diagnostic checked "kiro.trust_tools.conflict" when trust_all_tools is
// true and trust_tools is also non-empty, none in every other case.
func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		wantErr     bool
	}{
		{
			name:        "trust_all_tools with non-empty trust_tools conflicts",
			passthrough: map[string]any{"trust_all_tools": true, "trust_tools": []any{"read", "grep"}},
			wantErr:     true,
		},
		{
			name:        "trust_all_tools alone is valid",
			passthrough: map[string]any{"trust_all_tools": true},
			wantErr:     false,
		},
		{
			name:        "trust_tools alone is valid",
			passthrough: map[string]any{"trust_tools": []any{"read", "grep"}},
			wantErr:     false,
		},
		{
			name:        "trust_all_tools with empty trust_tools is valid",
			passthrough: map[string]any{"trust_all_tools": true, "trust_tools": []any{}},
			wantErr:     false,
		},
		{
			name:        "neither key set is valid",
			passthrough: map[string]any{},
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: tt.passthrough})

			diag := hasCheck(got, "kiro.trust_tools.conflict")
			if tt.wantErr && diag == nil {
				t.Fatalf("validateConfig(%v) missing check %q; got %+v", tt.passthrough, "kiro.trust_tools.conflict", got)
			}
			if !tt.wantErr && diag != nil {
				t.Fatalf("validateConfig(%v) has unexpected check %q; got %+v", tt.passthrough, "kiro.trust_tools.conflict", got)
			}
			if diag != nil && diag.Severity != "error" {
				t.Errorf("validateConfig(%v) check %q Severity = %q, want %q", tt.passthrough, diag.Check, diag.Severity, "error")
			}
		})
	}
}

// TestValidateConfig_TrustToolsUntrusted covers the kiro.trust_tools.untrusted
// check: an error-severity diagnostic fires whenever the resolved trust
// posture ([resolveTrustPosture]) falls short of full trust, and none fires
// once it resolves to full trust, including the empty-config and nil-config
// default. kiro-cli's actual behavior on an untrusted tool could not be
// observed in this environment, so the check assumes the conservative
// wait-for-approval reading documented in docs/kiro-adapter-notes.md's
// "Untrusted-tool behavior under --no-interactive" section.
func TestValidateConfig_TrustToolsUntrusted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		wantErr     bool
	}{
		{
			name:        "non-empty trust_tools without trust_all_tools is untrusted",
			passthrough: map[string]any{"trust_tools": []any{"read", "grep"}},
			wantErr:     true,
		},
		{
			name:        "trust_all_tools explicitly false is untrusted",
			passthrough: map[string]any{"trust_all_tools": false},
			wantErr:     true,
		},
		{
			name:        "trust_all_tools true is fully trusted",
			passthrough: map[string]any{"trust_all_tools": true},
			wantErr:     false,
		},
		{
			name:        "empty config defaults to full trust",
			passthrough: map[string]any{},
			wantErr:     false,
		},
		{
			name:        "nil config defaults to full trust",
			passthrough: nil,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: tt.passthrough})

			diag := hasCheck(got, "kiro.trust_tools.untrusted")
			if tt.wantErr && diag == nil {
				t.Fatalf("validateConfig(%v) missing check %q; got %+v", tt.passthrough, "kiro.trust_tools.untrusted", got)
			}
			if !tt.wantErr && diag != nil {
				t.Fatalf("validateConfig(%v) has unexpected check %q; got %+v", tt.passthrough, "kiro.trust_tools.untrusted", got)
			}
			if diag != nil && diag.Severity != "error" {
				t.Errorf("validateConfig(%v) check %q Severity = %q, want %q", tt.passthrough, diag.Check, diag.Severity, "error")
			}
		})
	}
}

// TestValidateConfig_TypeFaultNoDrift covers the R6.5 no-drift property: a
// wrong-typed value for a key the constructor reads fails NewKiroAdapter
// with a plain error carrying the fault message, and validateConfig
// reports the identical fault text under a "kiro.<key>.wrong_type" check
// for the same input, so the two surfaces cannot diverge on what counts
// as a type fault.
func TestValidateConfig_TypeFaultNoDrift(t *testing.T) {
	t.Parallel()

	config := map[string]any{"model": 123}

	_, constructErr := NewKiroAdapter(config)
	if constructErr == nil {
		t.Fatal("NewKiroAdapter(model=123) error = nil, want non-nil")
	}
	if constructErr.Error() != "model: expected string, got integer" {
		t.Errorf("NewKiroAdapter(model=123) error = %q, want %q", constructErr.Error(), "model: expected string, got integer")
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: config})

	diag := hasCheck(got, "kiro.model.wrong_type")
	if diag == nil {
		t.Fatalf("validateConfig(model=123) missing check %q; got %+v", "kiro.model.wrong_type", got)
	}
	if diag.Message != constructErr.Error() {
		t.Errorf("validateConfig(model=123) check %q Message = %q, want the same text the constructor failed with: %q", diag.Check, diag.Message, constructErr.Error())
	}
}

// TestValidateConfig_TrustConflictNotWrongType covers R6.6/R8.9: moving the
// trust_all_tools/trust_tools conflict out of the funnel and into
// [checkCrossField] must not turn it into a wrong_type diagnostic. A
// config carrying only the conflict (no mistyped string key) reports
// kiro.trust_tools.conflict exactly once and no kiro.*.wrong_type check.
func TestValidateConfig_TrustConflictNotWrongType(t *testing.T) {
	t.Parallel()

	config := map[string]any{"trust_all_tools": true, "trust_tools": []any{"read"}}

	_, constructErr := NewKiroAdapter(config)
	if constructErr == nil {
		t.Fatal("NewKiroAdapter(trust conflict) error = nil, want error")
	}
	if constructErr.Error() != trustToolsConflictMessage {
		t.Errorf("NewKiroAdapter(trust conflict) error = %q, want %q", constructErr.Error(), trustToolsConflictMessage)
	}

	got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: config})

	conflictCount := 0
	for _, d := range got {
		if d.Check == "kiro.trust_tools.conflict" {
			conflictCount++
		}
		if strings.HasSuffix(d.Check, ".wrong_type") {
			t.Errorf("validateConfig(trust conflict) unexpectedly has a wrong_type check: %+v", d)
		}
	}
	if conflictCount != 1 {
		t.Errorf("validateConfig(trust conflict) has %d kiro.trust_tools.conflict diagnostics, want exactly 1: %+v", conflictCount, got)
	}
}

// TestValidateConfig_ConflictAndUntrustedCoexist asserts that
// validateTrustToolsConflict and validateTrustToolsUntrusted are
// independent checks, each still producible after the other was added: a
// conflicting configuration reports kiro.trust_tools.conflict without
// kiro.trust_tools.untrusted (the conflicting posture resolves to full
// trust), and an untrusted configuration reports
// kiro.trust_tools.untrusted without kiro.trust_tools.conflict.
func TestValidateConfig_ConflictAndUntrustedCoexist(t *testing.T) {
	t.Parallel()

	t.Run("conflict fires without untrusted", func(t *testing.T) {
		t.Parallel()

		passthrough := map[string]any{"trust_all_tools": true, "trust_tools": []any{"read"}}
		got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: passthrough})

		if hasCheck(got, "kiro.trust_tools.conflict") == nil {
			t.Errorf("validateConfig(%v) missing check %q; got %+v", passthrough, "kiro.trust_tools.conflict", got)
		}
		if hasCheck(got, "kiro.trust_tools.untrusted") != nil {
			t.Errorf("validateConfig(%v) has unexpected check %q; got %+v", passthrough, "kiro.trust_tools.untrusted", got)
		}
	})

	t.Run("untrusted fires without conflict", func(t *testing.T) {
		t.Parallel()

		passthrough := map[string]any{"trust_tools": []any{"read"}}
		got := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: passthrough})

		if hasCheck(got, "kiro.trust_tools.untrusted") == nil {
			t.Errorf("validateConfig(%v) missing check %q; got %+v", passthrough, "kiro.trust_tools.untrusted", got)
		}
		if hasCheck(got, "kiro.trust_tools.conflict") != nil {
			t.Errorf("validateConfig(%v) has unexpected check %q; got %+v", passthrough, "kiro.trust_tools.conflict", got)
		}
	})
}
