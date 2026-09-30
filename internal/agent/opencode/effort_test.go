package opencode

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

const effortProbeModel = "anthropic/claude-sonnet-4-5"

func flagValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func opencodeEffortProbe(major runtimeMajor) agenttest.EffortProbe {
	return func(t *testing.T, passthrough map[string]any, verification bool, turns int) ([]string, error) {
		t.Helper()

		config := maps.Clone(passthrough)
		if major == major2 {
			config["model"] = effortProbeModel
		}
		adapter, err := NewOpenCodeAdapter(config)
		if err != nil {
			return nil, err
		}
		pt := adapter.(*OpenCodeAdapter).passthrough

		carried := make([]string, 0, turns)
		for turn := 1; turn <= turns; turn++ {
			state := &sessionState{major: major, credentialVerification: verification}
			if turn > 1 {
				state.sessionID = "ses_probe"
			}
			args := buildRunArgs(state, "probe prompt", pt)
			if major == major2 {
				_, suffix, _ := strings.Cut(flagValue(args, "--model"), "#")
				carried = append(carried, suffix)
				continue
			}
			carried = append(carried, flagValue(args, "--variant"))
		}
		return carried, nil
	}
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		major runtimeMajor
	}{
		{name: "major 1 reads the variant flag", major: major1},
		{name: "major 2 reads the model suffix", major: major2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agenttest.AssertEffortForwarding(t, opencodeEffortProbe(tt.major))
		})
	}
}

func TestEffortVariantConflict(t *testing.T) {
	t.Parallel()

	wantMessage := fmt.Sprintf("opencode.%s and opencode.variant both set the model variant; set one of them", registry.EffortKey)
	wantCheck := "opencode." + registry.EffortKey + ".conflict"

	tests := []struct {
		name         string
		passthrough  map[string]any
		wantConflict bool
	}{
		{name: "effort and variant both set", passthrough: map[string]any{registry.EffortKey: "high", "variant": "max"}, wantConflict: true},
		{name: "effort alone", passthrough: map[string]any{registry.EffortKey: "high"}},
		{name: "variant alone", passthrough: map[string]any{"variant": "max"}},
		{name: "effort set and variant empty", passthrough: map[string]any{registry.EffortKey: "high", "variant": ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, constructErr := NewOpenCodeAdapter(tt.passthrough)
			diags := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: tt.passthrough})
			diag := hasCheck(diags, wantCheck)

			if !tt.wantConflict {
				if constructErr != nil {
					t.Errorf("NewOpenCodeAdapter(%v) error = %v, want nil", tt.passthrough, constructErr)
				}
				if diag != nil {
					t.Errorf("validateConfig(%v) reported %+v, want no %q", tt.passthrough, diag, wantCheck)
				}
				return
			}
			if constructErr == nil {
				t.Fatalf("NewOpenCodeAdapter(%v) error = nil, want the conflict refusal", tt.passthrough)
			}
			if diag == nil {
				t.Fatalf("validateConfig(%v) = %+v, want check %q", tt.passthrough, diags, wantCheck)
			}
			if diag.Severity != "error" {
				t.Errorf("validateConfig(%v) %q Severity = %q, want %q", tt.passthrough, wantCheck, diag.Severity, "error")
			}
			if constructErr.Error() != wantMessage {
				t.Errorf("NewOpenCodeAdapter(%v) error = %q, want %q", tt.passthrough, constructErr.Error(), wantMessage)
			}
			if diag.Message != constructErr.Error() {
				t.Errorf("validateConfig(%v) %q Message = %q, want the constructor's text %q", tt.passthrough, wantCheck, diag.Message, constructErr.Error())
			}
		})
	}
}

func TestEffortMajor2Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pt      passthroughConfig
		major   runtimeMajor
		wantMsg string
	}{
		{
			name:    "effort without a model names opencode.effort",
			pt:      passthroughConfig{Effort: "high"},
			major:   major2,
			wantMsg: fmt.Sprintf("opencode.%[1]s needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.%[1]s", registry.EffortKey),
		},
		{
			name:    "effort with a model carrying a suffix names opencode.effort",
			pt:      passthroughConfig{Effort: "high", Model: "anthropic/claude-sonnet-4-5#max"},
			major:   major2,
			wantMsg: fmt.Sprintf("opencode.model already names a variant after #; remove that suffix or remove opencode.%s", registry.EffortKey),
		},
		{
			name:    "variant without a model still names opencode.variant",
			pt:      passthroughConfig{Variant: "high"},
			major:   major2,
			wantMsg: "opencode.variant needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.variant",
		},
		{
			name:    "variant with a model carrying a suffix still names opencode.variant",
			pt:      passthroughConfig{Variant: "high", Model: "anthropic/claude-sonnet-4-5#max"},
			major:   major2,
			wantMsg: "opencode.model already names a variant after #; remove that suffix or remove opencode.variant",
		},
		{
			name:  "major 1 never refuses effort without a model",
			pt:    passthroughConfig{Effort: "high"},
			major: major1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := checkMajorSettings(tt.pt, tt.major)

			if tt.wantMsg == "" {
				if got != nil {
					t.Fatalf("checkMajorSettings(%+v, %d) = %v, want nil", tt.pt, tt.major, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("checkMajorSettings(%+v, %d) = nil, want message %q", tt.pt, tt.major, tt.wantMsg)
			}
			if got.Kind != domain.ErrAgentNotFound {
				t.Errorf("checkMajorSettings(%+v, %d).Kind = %q, want %q", tt.pt, tt.major, got.Kind, domain.ErrAgentNotFound)
			}
			if got.Message != tt.wantMsg {
				t.Errorf("checkMajorSettings(%+v, %d).Message = %q, want %q", tt.pt, tt.major, got.Message, tt.wantMsg)
			}
		})
	}
}

func TestEffortWrongType(t *testing.T) {
	t.Parallel()

	passthrough := map[string]any{registry.EffortKey: 7}

	_, constructErr := NewOpenCodeAdapter(passthrough)
	diags := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: passthrough})

	if _, ok := errors.AsType[*typeutil.TypeFault](constructErr); !ok {
		t.Fatalf("NewOpenCodeAdapter(%v) error = %v, want a *typeutil.TypeFault", passthrough, constructErr)
	}
	wantCheck := "opencode." + registry.EffortKey + ".wrong_type"
	diag := hasCheck(diags, wantCheck)
	if diag == nil {
		t.Fatalf("validateConfig(%v) = %+v, want check %q", passthrough, diags, wantCheck)
	}
	if diag.Severity != "error" {
		t.Errorf("validateConfig(%v) %q Severity = %q, want %q", passthrough, wantCheck, diag.Severity, "error")
	}
	if diag.Message != constructErr.Error() {
		t.Errorf("validateConfig(%v) %q Message = %q, want the constructor's text %q", passthrough, wantCheck, diag.Message, constructErr.Error())
	}
}
