package kiro

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const acpKind = "agent-client-protocol"

func TestMain(m *testing.M) {
	agenttest.Main(m, nil)
}

func fixtureW() map[string]any {
	return map[string]any{
		"agent": map[string]any{"kind": "kiro", "command": "kiro-cli"},
		"kiro":  map[string]any{"model": "claude-sonnet-4.6", "agent": "reviewer", "mcp_config": "mcp.json"},
		"dispatch": map[string]any{"rules": []any{
			map[string]any{"name": "review", "match": map[string]any{"labels": []any{"review"}}, "agent": "kiro"},
		}},
	}
}

func loadW(t *testing.T, raw map[string]any) config.ServiceConfig {
	t.Helper()
	cfg, err := config.NewServiceConfig(raw, config.WithRetiredAgents(registry.RetiredAgentOf))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	return cfg
}

func TestRetiredKiro_LoadsOntoTheProtocolKind(t *testing.T) {
	t.Parallel()

	raw := fixtureW()

	cfg := loadW(t, raw)

	if cfg.Agent.Kind != acpKind {
		t.Errorf("Agent.Kind = %q, want %q", cfg.Agent.Kind, acpKind)
	}
	if got := raw["dispatch"].(map[string]any)["rules"].([]any)[0].(map[string]any)["agent"]; got != acpKind {
		t.Errorf("dispatch.rules[0].agent = %v, want %q", got, acpKind)
	}
	if _, present := cfg.ExtensionValue("kiro"); present {
		t.Error(`ExtensionValue("kiro") reports present, want the kiro section removed`)
	}
	if _, present := cfg.ExtensionValue(acpKind); !present {
		t.Errorf("ExtensionValue(%q) reports absent, want the replacement section", acpKind)
	}
	records := cfg.AgentKindConversions()
	if len(records) != 1 || records[0].Kind != "kiro" || records[0].Replacement != acpKind ||
		!slices.Equal(records[0].Fields, []string{"agent.kind", "dispatch.rules[0].agent"}) {
		t.Fatalf("AgentKindConversions() = %+v, want one record for kiro with fields agent.kind and dispatch.rules[0].agent", records)
	}
	want := domain.AgentCommand{Line: "kiro-cli acp -a --model claude-sonnet-4.6 --agent reviewer"}
	for _, remote := range []bool{false, true} {
		if got := cfg.AgentCommand(acpKind, remote); got.Line != want.Line || got.Argv != nil {
			t.Errorf("AgentCommand(%q, %v) = %+v, want %+v", acpKind, remote, got, want)
		}
	}
}

func TestRetiredKiro_AdvisoryNamesWhatIsNotCarried(t *testing.T) {
	t.Parallel()

	cfg := loadW(t, fixtureW())

	advisories := cfg.Advisories()
	if len(advisories) != 1 {
		t.Fatalf("Advisories() = %+v, want exactly one", advisories)
	}
	want := `agent kind "kiro" was removed, so this configuration was converted to agent kind "agent-client-protocol" (agent.kind, dispatch.rules[0].agent); its sessions launch agent.command with the arguments acp -a --model claude-sonnet-4.6 --agent reviewer; not carried: kiro.mcp_config. This conversion is temporary and will be removed in a later release: name agent kind "agent-client-protocol" where the workflow names "kiro" and give it this invocation in agent.command`
	if advisories[0].Check != "agent.kind.retired" || advisories[0].Text != want {
		t.Errorf("Advisories()[0] = %+v, want check %q with text %q", advisories[0], "agent.kind.retired", want)
	}
}

func TestRetiredKiro_LocalLaunchArgv(t *testing.T) {
	binPath := agenttest.FakeRuntime(t, t.TempDir(), "kiro-cli", agenttest.OutputScenario, agenttest.Output{})
	t.Setenv("PATH", filepath.Dir(binPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := loadW(t, fixtureW())
	command := cfg.AgentCommand(acpKind, false)

	target, agentErr := agentcore.ResolveLaunchTarget(domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Kind: acpKind, Command: command.Line, CommandArgv: command.Argv},
	}, "")

	if agentErr != nil {
		t.Fatalf("ResolveLaunchTarget() error = %v", agentErr)
	}
	if target.Command != binPath {
		t.Errorf("LaunchTarget.Command = %q, want the kiro-cli on PATH %q", target.Command, binPath)
	}
	wantArgs := []string{"acp", "-a", "--model", "claude-sonnet-4.6", "--agent", "reviewer"}
	if !slices.Equal(target.Args, wantArgs) {
		t.Errorf("LaunchTarget.Args = %q, want %q", target.Args, wantArgs)
	}
}

func TestRetiredKiro_RemoteLaunchAndCredential(t *testing.T) {
	t.Parallel()

	raw := fixtureW()
	raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}}

	cfg := loadW(t, raw)

	command := cfg.AgentCommand(acpKind, true)
	launch := sshutil.BuildSSHLaunch("host-1", "/w", command.Line, nil, sshutil.SSHOptions{})
	wantFinal := "cd -- '/w' && { kiro-cli acp -a --model claude-sonnet-4.6 --agent reviewer\n}"
	if got := launch.Args[len(launch.Args)-1]; got != wantFinal {
		t.Errorf("BuildSSHLaunch final element = %q, want %q", got, wantFinal)
	}
	records := cfg.AgentKindConversions()
	if len(records) != 1 || !slices.Equal(records[0].CredentialEnv, []string{"KIRO_API_KEY"}) {
		t.Errorf("AgentKindConversions() = %+v, want one record carrying %q", records, "KIRO_API_KEY")
	}
	advisories := cfg.Advisories()
	if len(advisories) != 1 || !strings.Contains(advisories[0].Text, "and a remote launch carries KIRO_API_KEY as agent kind \"kiro\" did") ||
		!strings.Contains(advisories[0].Text, "and list KIRO_API_KEY under worker.ssh_pass_env") {
		t.Errorf("Advisories() = %+v, want one advisory naming KIRO_API_KEY and worker.ssh_pass_env", advisories)
	}
}

func TestRetiredKiro_NonDefaultKindLaunchesItsOwnExecutable(t *testing.T) {
	t.Parallel()

	raw := fixtureW()
	raw["agent"] = map[string]any{"kind": "plain-kind", "command": "plain-cmd"}

	cfg := loadW(t, raw)

	want := "kiro-cli acp -a --model claude-sonnet-4.6 --agent reviewer"
	if got := cfg.AgentCommand(acpKind, false); got.Line != want {
		t.Errorf("AgentCommand(%q, false) = %+v, want the string %q", acpKind, got, want)
	}
}

func TestRetiredKiro_SpacedAgentNameKeepsOneArgument(t *testing.T) {
	t.Parallel()

	raw := fixtureW()
	raw["kiro"] = map[string]any{"model": "claude-sonnet-4.6", "agent": "space name"}

	cfg := loadW(t, raw)

	wantLocal := []string{"kiro-cli", "acp", "-a", "--model", "claude-sonnet-4.6", "--agent", "space name"}
	if got := cfg.AgentCommand(acpKind, false); got.Line != "" || !slices.Equal(got.Argv, wantLocal) {
		t.Errorf("AgentCommand(%q, false) = %+v, want the list %q", acpKind, got, wantLocal)
	}
	wantRemote := "kiro-cli acp -a --model claude-sonnet-4.6 --agent 'space name'"
	if got := cfg.AgentCommand(acpKind, true); got.Argv != nil || got.Line != wantRemote {
		t.Errorf("AgentCommand(%q, true) = %+v, want the string %q", acpKind, got, wantRemote)
	}
}

func TestRetiredKiro_IsRetiredNotRegistered(t *testing.T) {
	t.Parallel()

	decl, retired := registry.RetiredAgentOf("kiro")

	if registry.Agents.Has("kiro") {
		t.Error(`registry.Agents.Has("kiro") = true, want false`)
	}
	if !retired {
		t.Fatal(`RetiredAgentOf("kiro") = false, want a declaration`)
	}
	if decl.Replacement != acpKind {
		t.Errorf(`RetiredAgentOf("kiro").Replacement = %q, want %q`, decl.Replacement, acpKind)
	}
}

func TestRetiredKiro_ConvertVerdicts(t *testing.T) {
	t.Parallel()

	untrusted := &registry.AgentConversionFault{Key: "trust_tools", Message: trustToolsUntrustedMessage}
	tests := []struct {
		name     string
		settings map[string]any
		want     *registry.AgentConversionFault
	}{
		{name: "nil settings", settings: nil},
		{name: "empty settings", settings: map[string]any{}},
		{
			name:     "full trust with an allowlist",
			settings: map[string]any{"trust_all_tools": true, "trust_tools": []any{"read", "grep"}},
			want:     &registry.AgentConversionFault{Key: "trust_tools", Message: trustToolsConflictMessage},
		},
		{name: "full trust", settings: map[string]any{"trust_all_tools": true}},
		{
			name:     "allowlist only",
			settings: map[string]any{"trust_tools": []any{"read", "grep"}},
			want:     untrusted,
		},
		{
			name:     "full trust with an empty allowlist",
			settings: map[string]any{"trust_all_tools": true, "trust_tools": []any{}},
		},
		{name: "trust explicitly off", settings: map[string]any{"trust_all_tools": false}, want: untrusted},
		{name: "single allowlisted tool", settings: map[string]any{"trust_tools": []any{"read"}}, want: untrusted},
		{
			name:     "model not a string",
			settings: map[string]any{"model": 123},
			want:     &registry.AgentConversionFault{Key: "model", Message: "model: expected string, got integer"},
		},
		{name: "model and agent", settings: map[string]any{"model": "claude-sonnet-4.6", "agent": "reviewer"}},
		{
			name:     "agent not a string beside a model",
			settings: map[string]any{"model": "claude-sonnet-4.6", "agent": 7},
			want:     &registry.AgentConversionFault{Key: "agent", Message: "agent: expected string, got integer"},
		},
		{
			name:     "agent not a string beside an allowlist",
			settings: map[string]any{"agent": true, "trust_tools": []any{"read"}},
			want:     &registry.AgentConversionFault{Key: "agent", Message: "agent: expected string, got boolean"},
		},
		{
			name:     "model type fault outranks the trust conflict",
			settings: map[string]any{"model": 1, "trust_all_tools": true, "trust_tools": []any{"read"}},
			want:     &registry.AgentConversionFault{Key: "model", Message: "model: expected string, got integer"},
		},
		{
			name:     "trust off with an allowlist",
			settings: map[string]any{"model": "m", "trust_all_tools": false, "trust_tools": []any{"read"}},
			want:     untrusted,
		},
		{
			name:     "uncarried keys are not faults",
			settings: map[string]any{"agent": "space name", "mcp_config": "mcp.json", "unknown": "x"},
		},
	}
	decl, retired := registry.RetiredAgentOf("kiro")
	if !retired {
		t.Fatal(`RetiredAgentOf("kiro") = false, want a declaration`)
	}

	for _, tt := range tests {
		for _, remote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s remote %v", tt.name, remote), func(t *testing.T) {
				t.Parallel()

				before := cloneSettings(tt.settings)

				conversion, fault := decl.Convert(registry.AgentConversionInput{
					Command:  domain.AgentCommand{Line: "kiro-cli"},
					Settings: tt.settings,
					Remote:   remote,
				})

				if tt.want == nil {
					if fault != nil {
						t.Fatalf("Convert(%v, remote=%v) fault = %+v, want none", tt.settings, remote, *fault)
					}
					if conversion.Command.IsZero() {
						t.Errorf("Convert(%v, remote=%v) command is zero without a fault, want a launch command", tt.settings, remote)
					}
				} else {
					if fault == nil {
						t.Fatalf("Convert(%v, remote=%v) fault = nil, want %+v", tt.settings, remote, *tt.want)
					}
					if *fault != *tt.want {
						t.Errorf("Convert(%v, remote=%v) fault = %+v, want %+v", tt.settings, remote, *fault, *tt.want)
					}
					if !conversion.Command.IsZero() {
						t.Errorf("Convert(%v, remote=%v) command = %+v beside a fault, want zero", tt.settings, remote, conversion.Command)
					}
				}
				if !reflect.DeepEqual(before, tt.settings) {
					t.Errorf("Convert(%v, remote=%v) changed its settings, want them left as %v", tt.settings, remote, before)
				}
			})
		}
	}
}

func cloneSettings(settings map[string]any) map[string]any {
	if settings == nil {
		return nil
	}
	clone := make(map[string]any, len(settings))
	for key, value := range settings {
		if list, ok := value.([]any); ok {
			value = slices.Clone(list)
		}
		clone[key] = value
	}
	return clone
}
