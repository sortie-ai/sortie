package opencode

import (
	"context"
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
	version := "1.18.33"
	if major == major2 {
		version = "2.0.18"
	}
	return func(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
		t.Helper()

		config := maps.Clone(settings)
		if major == major2 {
			config["model"] = effortProbeModel
		}
		dir := t.TempDir()
		command := agenttest.FakeRuntime(t, dir, "opencode", agenttest.OutputScenario, agenttest.Output{Version: version})
		adapter, _ := NewOpenCodeAdapter()
		resumeID := ""
		if resumed {
			resumeID = "ses_probe"
		}
		session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
			WorkspacePath:          dir,
			AgentConfig:            domain.AgentConfig{Command: command},
			ResumeSessionID:        resumeID,
			CredentialVerification: verification,
			Settings:               config,
		})
		if err != nil {
			return nil, fmt.Errorf("StartSession: %w", err)
		}
		state := session.Internal.(*sessionState)

		carried := make([]string, 0, turns)
		for turn := 1; turn <= turns; turn++ {
			if turn > 1 {
				state.sessionID = "ses_probe"
			}
			args := buildRunArgs(state, "probe prompt", state.passthrough)
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

			startErr, launched := startRefusedSession(t, tt.passthrough)
			diags := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: tt.passthrough})
			diag := hasCheck(diags, wantCheck)

			if !tt.wantConflict {
				if !launched {
					t.Errorf("StartSession(%v) refused before launch with %v, want the settings accepted", tt.passthrough, startErr)
				}
				if diag != nil {
					t.Errorf("validateConfig(%v) reported %+v, want no %q", tt.passthrough, diag, wantCheck)
				}
				return
			}
			var agentErr *domain.AgentError
			if !errors.As(startErr, &agentErr) {
				t.Fatalf("StartSession(%v) error = %v, want the conflict refusal as a *domain.AgentError", tt.passthrough, startErr)
			}
			if diag == nil {
				t.Fatalf("validateConfig(%v) = %+v, want check %q", tt.passthrough, diags, wantCheck)
			}
			if diag.Severity != "error" {
				t.Errorf("validateConfig(%v) %q Severity = %q, want %q", tt.passthrough, wantCheck, diag.Severity, "error")
			}
			if agentErr.Message != wantMessage {
				t.Errorf("StartSession(%v) Message = %q, want %q", tt.passthrough, agentErr.Message, wantMessage)
			}
			if diag.Message != agentErr.Message {
				t.Errorf("validateConfig(%v) %q Message = %q, want the session-start text %q", tt.passthrough, wantCheck, diag.Message, agentErr.Message)
			}
			if launched {
				t.Errorf("StartSession(%v) launched the runtime, want the refusal before any launch", tt.passthrough)
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

	startErr, _ := startRefusedSession(t, passthrough)
	diags := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: passthrough})

	if _, ok := errors.AsType[*typeutil.TypeFault](startErr); !ok {
		t.Fatalf("StartSession(%v) error = %v, want a wrapped *typeutil.TypeFault", passthrough, startErr)
	}
	wantCheck := "opencode." + registry.EffortKey + ".wrong_type"
	diag := hasCheck(diags, wantCheck)
	if diag == nil {
		t.Fatalf("validateConfig(%v) = %+v, want check %q", passthrough, diags, wantCheck)
	}
	if diag.Severity != "error" {
		t.Errorf("validateConfig(%v) %q Severity = %q, want %q", passthrough, wantCheck, diag.Severity, "error")
	}
	if agentErr, ok := errors.AsType[*domain.AgentError](startErr); !ok || diag.Message != agentErr.Message {
		t.Errorf("validateConfig(%v) %q Message = %q, want the text StartSession refused with: %v", passthrough, wantCheck, diag.Message, startErr)
	}
}
