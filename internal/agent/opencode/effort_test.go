package opencode

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

const (
	effortProbeModel = "provider/model"

	scenarioVersionMarker = "opencode.versionmarker"
)

type versionMarker struct {
	Path    string
	Version string
}

func init() {
	fakeScenarios[scenarioVersionMarker] = agenttest.Typed(runVersionMarker)
}

func runVersionMarker(_ []string, params versionMarker) int {
	if err := os.WriteFile(params.Path, nil, 0o600); err != nil {
		return 2
	}
	fmt.Println(params.Version)
	return 0
}

func startOnVersionMarkerRuntime(t *testing.T, settings map[string]any) (session domain.Session, err error, launched bool) {
	t.Helper()

	dir := t.TempDir()
	marker := filepath.Join(dir, "launched")
	command := agenttest.FakeRuntime(t, dir, "opencode", scenarioVersionMarker, versionMarker{Path: marker, Version: "opencode v2.0.18"})
	a, _ := NewOpenCodeAdapter()

	session, err = a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: command},
		Settings:      settings,
	})
	_, statErr := os.Stat(marker)
	return session, err, statErr == nil
}

func flagValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func opencodeEffortProbe() agenttest.EffortProbe {
	return func(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
		t.Helper()

		config := maps.Clone(settings)
		config["model"] = effortProbeModel
		dir := t.TempDir()
		command := agenttest.FakeRuntime(t, dir, "opencode", agenttest.OutputScenario, agenttest.Output{Version: "2.0.18"})
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
			args := buildRunArgs(state, state.passthrough)
			_, suffix, _ := strings.Cut(flagValue(args, "--model"), "#")
			carried = append(carried, suffix)
		}
		return carried, nil
	}
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, opencodeEffortProbe())
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
		{name: "effort alone", passthrough: map[string]any{"model": "provider/model", registry.EffortKey: "high"}},
		{name: "variant alone", passthrough: map[string]any{"model": "provider/model", "variant": "max"}},
		{name: "effort set and variant empty", passthrough: map[string]any{"model": "provider/model", registry.EffortKey: "high", "variant": ""}},
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

func TestStartSession_RefusesPureAndBareSlot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		settings    map[string]any
		wantMessage string
	}{
		{
			name:        "pure true",
			settings:    map[string]any{"pure": true},
			wantMessage: "opencode.pure is not supported by OpenCode 2.x; remove it",
		},
		{
			name:        "effort without a model",
			settings:    map[string]any{registry.EffortKey: "high"},
			wantMessage: fmt.Sprintf("opencode.%[1]s needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.%[1]s", registry.EffortKey),
		},
		{
			name:        "effort with a model carrying a suffix",
			settings:    map[string]any{"model": "provider/model#max", registry.EffortKey: "high"},
			wantMessage: fmt.Sprintf("opencode.model already names a variant after #; remove that suffix or remove opencode.%s", registry.EffortKey),
		},
		{
			name:        "variant without a model",
			settings:    map[string]any{"variant": "high"},
			wantMessage: "opencode.variant needs opencode.model on OpenCode 2.x; set opencode.model or remove opencode.variant",
		},
		{
			name:        "variant with a model carrying a suffix",
			settings:    map[string]any{"model": "provider/model#max", "variant": "high"},
			wantMessage: "opencode.model already names a variant after #; remove that suffix or remove opencode.variant",
		},
		{name: "pure false starts", settings: map[string]any{"pure": false}},
		{name: "pure as the string true starts", settings: map[string]any{"pure": "true"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session, err, launched := startOnVersionMarkerRuntime(t, tt.settings)

			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("StartSession(%v) error = %v, want nil", tt.settings, err)
				}
				if session.Internal == nil {
					t.Errorf("StartSession(%v) session = %+v, want a started session", tt.settings, session)
				}
				return
			}
			agentErr, ok := errors.AsType[*domain.AgentError](err)
			if !ok {
				t.Fatalf("StartSession(%v) error = %v, want a *domain.AgentError", tt.settings, err)
			}
			if agentErr.Kind != domain.ErrAgentNotFound {
				t.Errorf("StartSession(%v) Kind = %q, want %q", tt.settings, agentErr.Kind, domain.ErrAgentNotFound)
			}
			if agentErr.Message != tt.wantMessage {
				t.Errorf("StartSession(%v) Message = %q, want %q", tt.settings, agentErr.Message, tt.wantMessage)
			}
			if launched {
				t.Errorf("StartSession(%v) launched the runtime, want the refusal before any launch", tt.settings)
			}
		})
	}
}
