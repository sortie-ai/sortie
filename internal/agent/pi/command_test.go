package pi

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// assertHasArgPair fails if flag and value do not appear as consecutive
// elements in args.
func assertHasArgPair(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := range len(args) - 1 {
		if args[i] == flag && args[i+1] == value {
			return
		}
	}
	t.Errorf("buildRunArgs() missing %q %q in [%s]", flag, value, strings.Join(args, " "))
}

// assertHasFlag fails if flag does not appear in args.
func assertHasFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		return
	}
	t.Errorf("buildRunArgs() missing flag %q in [%s]", flag, strings.Join(args, " "))
}

// assertNoFlag fails if flag appears anywhere in args.
func assertNoFlag(t *testing.T, args []string, flag string) {
	t.Helper()
	if slices.Contains(args, flag) {
		t.Errorf("buildRunArgs() unexpected flag %q in [%s]", flag, strings.Join(args, " "))
	}
}

// newTestSessionState returns a sessionState suitable for buildRunArgs tests.
func newTestSessionState(workspacePath, sessionID string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			WorkspacePath: workspacePath,
		},
		sessionID: sessionID,
	}
}

func TestNewPiAdapter_ParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    map[string]any
		checkFunc func(t *testing.T, pt passthroughConfig)
	}{
		{
			name:   "defaults",
			config: map[string]any{},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				want := passthroughConfig{ProjectTrust: trustIgnore}
				if !reflect.DeepEqual(pt, want) {
					t.Errorf("passthroughConfig = %+v, want %+v (an unattended turn defaults to --no-approve)", pt, want)
				}
			},
		},
		{
			name:   "effort_is_accepted_and_not_read",
			config: map[string]any{"effort": "high"},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				want := passthroughConfig{ProjectTrust: trustIgnore}
				if !reflect.DeepEqual(pt, want) {
					t.Errorf("passthroughConfig = %+v, want %+v", pt, want)
				}
			},
		},
		{
			name:   "mcp_config_is_the_orchestrators_key",
			config: map[string]any{"mcp_config": "./mcp-servers.json"},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				want := passthroughConfig{ProjectTrust: trustIgnore}
				if !reflect.DeepEqual(pt, want) {
					t.Errorf("passthroughConfig = %+v, want %+v", pt, want)
				}
			},
		},
		{
			name: "model_and_flags",
			config: map[string]any{
				"model":         "anthropic/claude-3-5-sonnet",
				"project_trust": "approve",
			},
			checkFunc: func(t *testing.T, pt passthroughConfig) {
				t.Helper()
				want := passthroughConfig{
					Model:        "anthropic/claude-3-5-sonnet",
					ProjectTrust: trustApprove,
				}
				if !reflect.DeepEqual(pt, want) {
					t.Errorf("passthroughConfig = %+v, want %+v", pt, want)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pt, faults := parsePassthroughConfig(tt.config)
			hasErr := len(faults) > 0

			if hasErr {
				t.Fatalf("parsePassthroughConfig() faults = %v", faults)
			}

			if tt.checkFunc != nil {
				tt.checkFunc(t, pt)
			}
		})
	}
}

func TestBuildRunArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sessionID    string
		verification bool
		pt           passthroughConfig
		wantPairs    [][2]string
		wantAbsent   []string
	}{
		{
			name:       "fresh_session",
			wantAbsent: []string{"--session", "--no-session"},
		},
		{
			name:      "resume_session",
			sessionID: "ses_abc",
			wantPairs: [][2]string{{"--session", "ses_abc"}},
		},
		{
			name:      "noninteractive_json_mode",
			wantPairs: [][2]string{{"-p", ""}, {"--mode", "json"}},
		},
		{
			name:      "model_flag",
			pt:        passthroughConfig{Model: "anthropic/claude-3-5-sonnet"},
			wantPairs: [][2]string{{"--model", "anthropic/claude-3-5-sonnet"}},
		},
		{
			name:      "trust_approve",
			pt:        passthroughConfig{ProjectTrust: trustApprove},
			wantPairs: [][2]string{{"--approve", ""}},
		},
		{
			name:         "credential_verification_is_ephemeral",
			verification: true,
			wantPairs:    [][2]string{{"--no-session", ""}},
			wantAbsent:   []string{"--session"},
		},
		{
			name:         "credential_verification_never_resumes",
			sessionID:    "ses_abc",
			verification: true,
			wantPairs:    [][2]string{{"--no-session", ""}},
			wantAbsent:   []string{"--session"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newTestSessionState("/tmp/workspace", tt.sessionID)
			state.credentialVerification = tt.verification
			args := buildRunArgs(state, tt.pt)

			for _, pair := range tt.wantPairs {
				if pair[1] == "" {
					assertHasFlag(t, args, pair[0])
					continue
				}
				assertHasArgPair(t, args, pair[0], pair[1])
			}
			for _, flag := range tt.wantAbsent {
				assertNoFlag(t, args, flag)
			}

			// An unattended turn is launched with the trust decision
			// stated explicitly, never left to a prompt no one answers.
			if tt.pt.ProjectTrust != trustApprove {
				if !slices.Contains(args, "--no-approve") {
					t.Errorf("args = %v, want --no-approve unless project_trust is approve", args)
				}
				assertNoFlag(t, args, "--approve")
			} else {
				assertNoFlag(t, args, "--no-approve")
			}

			// The prompt travels on standard input, so no argument
			// separator or message follows the flags.
			assertNoFlag(t, args, "--")
		})
	}
}

// TestBuildTurnStdin asserts the prompt reaches pi byte for byte on
// standard input, behind the SSH preamble when there is one. A prompt
// beginning with "@" would be read as a file to include if it were an
// argument.
func TestBuildTurnStdin(t *testing.T) {
	t.Parallel()

	const prompt = "@/etc/passwd\n--help\n" + "'quoted' $(not run)"

	got, err := io.ReadAll(buildTurnStdin(prompt, nil))
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != prompt {
		t.Errorf("stdin without a preamble = %q, want %q", got, prompt)
	}

	got, err = io.ReadAll(buildTurnStdin(prompt, strings.NewReader("PREAMBLE")))
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(got) != "PREAMBLE"+prompt {
		t.Errorf("stdin with a preamble = %q, want %q", got, "PREAMBLE"+prompt)
	}
}

// TestMCPInjectionConformance drives a real turn for a session that
// carries the worker-generated MCP config path and asserts that nothing
// of the configuration reaches the pi process, as the kind's declared
// Unsupported disposition states.
func TestMCPInjectionConformance(t *testing.T) {
	t.Parallel()

	declared, ok := registry.Agents.Meta("pi")
	if !ok {
		t.Fatal(`registry.Agents.Meta("pi") reported not registered`)
	}
	if declared.MCPInjection != registry.MCPInjectionUnsupported {
		t.Fatalf("registered MCPInjection = %q, want %q", declared.MCPInjection, registry.MCPInjectionUnsupported)
	}
	if declared.MCPInjection.DeliversTools(false) {
		t.Error("MCPInjection.DeliversTools(remote=false) = true, want false: a pi session has no Sortie tool channel")
	}

	dir := t.TempDir()
	mcpConfigPath := filepath.Join(dir, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	const generatedConfig = `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server","--workflow","/repo/WORKFLOW.md"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`
	if err := os.WriteFile(mcpConfigPath, []byte(generatedConfig), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	argvFile := filepath.Join(dir, "argv.txt")
	envFile := filepath.Join(dir, "env.txt")
	script := writePiScript(t, dir, `printf '%s\n' "$@" > `+argvFile+`
env > `+envFile+`
exit 0`)

	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig:   domain.AgentConfig{Command: script},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	_, _, _ = collectEvents(t, a, session, "work")

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("ReadFile(argv) error = %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ReadFile(env) error = %v", err)
	}
	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{
		Args: strings.Split(strings.TrimRight(string(argv), "\n"), "\n"),
		Env:  strings.Split(strings.TrimRight(string(env), "\n"), "\n"),
	})
}

// TestValidateConfig_NoFaultsForSupportedConfiguration asserts a pi
// block written with every accepted key passes the offline validator.
func TestValidateConfig_NoFaultsForSupportedConfiguration(t *testing.T) {
	t.Parallel()

	diags := validateConfig(registry.AgentConfigFields{
		Kind: "pi",
		Passthrough: map[string]any{
			"model":         "anthropic/claude-3-5-sonnet",
			"mcp_config":    "./mcp-servers.json",
			"project_trust": "ignore",
		},
	})
	if len(diags) != 0 {
		t.Errorf("validateConfig() = %+v, want no diagnostics", diags)
	}
}
