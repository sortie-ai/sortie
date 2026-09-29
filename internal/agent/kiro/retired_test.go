package kiro

import (
	"fmt"
	"maps"
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

func retiredLookup(kind string) (registry.RetiredAgent, bool) {
	decl, err := registry.RetiredAgents.Get(kind)
	return decl, err == nil
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

func loadW(t *testing.T, raw map[string]any, lookup config.RetiredAgentLookup) config.ServiceConfig {
	t.Helper()
	cfg, err := config.NewServiceConfig(raw, config.WithRetiredAgents(lookup))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	return cfg
}

func TestRetiredKiro_LoadsOntoTheProtocolKind(t *testing.T) {
	t.Parallel()

	raw := fixtureW()

	cfg := loadW(t, raw, retiredLookup)

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

	cfg := loadW(t, fixtureW(), retiredLookup)

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
	cfg := loadW(t, fixtureW(), retiredLookup)
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

	cfg := loadW(t, raw, retiredLookup)

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

	cfg := loadW(t, raw, retiredLookup)

	want := "kiro-cli acp -a --model claude-sonnet-4.6 --agent reviewer"
	if got := cfg.AgentCommand(acpKind, false); got.Line != want {
		t.Errorf("AgentCommand(%q, false) = %+v, want the string %q", acpKind, got, want)
	}
}

func TestRetiredKiro_SpacedAgentNameKeepsOneArgument(t *testing.T) {
	t.Parallel()

	raw := fixtureW()
	raw["kiro"] = map[string]any{"model": "claude-sonnet-4.6", "agent": "space name"}

	cfg := loadW(t, raw, retiredLookup)

	wantLocal := []string{"kiro-cli", "acp", "-a", "--model", "claude-sonnet-4.6", "--agent", "space name"}
	if got := cfg.AgentCommand(acpKind, false); got.Line != "" || !slices.Equal(got.Argv, wantLocal) {
		t.Errorf("AgentCommand(%q, false) = %+v, want the list %q", acpKind, got, wantLocal)
	}
	wantRemote := "kiro-cli acp -a --model claude-sonnet-4.6 --agent 'space name'"
	if got := cfg.AgentCommand(acpKind, true); got.Argv != nil || got.Line != wantRemote {
		t.Errorf("AgentCommand(%q, true) = %+v, want the string %q", acpKind, got, wantRemote)
	}
}

func TestRetiredKiro_LiveKindRunsUnconverted(t *testing.T) {
	t.Parallel()

	if _, converts := registry.RetiredAgentOf("kiro"); converts {
		t.Fatal(`RetiredAgentOf("kiro") = true while the kind is registered live, want false`)
	}

	cfg := loadW(t, fixtureW(), registry.RetiredAgentOf)

	if cfg.Agent.Kind != "kiro" {
		t.Errorf("Agent.Kind = %q, want %q", cfg.Agent.Kind, "kiro")
	}
	if records := cfg.AgentKindConversions(); len(records) != 0 {
		t.Errorf("AgentKindConversions() = %+v, want none", records)
	}
	if advisories := cfg.Advisories(); len(advisories) != 0 {
		t.Errorf("Advisories() = %+v, want none", advisories)
	}
}

var validatorSettings = []map[string]any{
	nil,
	{},
	{"trust_all_tools": true, "trust_tools": []any{"read", "grep"}},
	{"trust_all_tools": true},
	{"trust_tools": []any{"read", "grep"}},
	{"trust_all_tools": true, "trust_tools": []any{}},
	{"trust_all_tools": false},
	{"trust_tools": []any{"read"}},
	{"model": 123},
	{"model": "claude-sonnet-4.6", "agent": "reviewer"},
	{"model": "claude-sonnet-4.6", "agent": 7},
	{"agent": true, "trust_tools": []any{"read"}},
	{"model": 1, "trust_all_tools": true, "trust_tools": []any{"read"}},
	{"model": "m", "trust_all_tools": false, "trust_tools": []any{"read"}},
	{"agent": "space name", "mcp_config": "mcp.json", "unknown": "x"},
}

func firstErrorDiagnostic(settings map[string]any) *registry.ValidationDiag {
	diags := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: settings})
	for i := range diags {
		if diags[i].Severity == "error" {
			return &diags[i]
		}
	}
	return nil
}

func keyOfCheck(check string) string {
	key := strings.TrimPrefix(check, "kiro.")
	if i := strings.LastIndex(key, "."); i >= 0 {
		key = key[:i]
	}
	return key
}

func TestRetiredKiro_ConvertFaultsExactlyWhenTheValidatorDoes(t *testing.T) {
	t.Parallel()

	decl, ok := retiredLookup("kiro")
	if !ok {
		t.Fatal(`registry.RetiredAgents holds no "kiro" declaration`)
	}

	for i, settings := range validatorSettings {
		for _, remote := range []bool{false, true} {
			t.Run(fmt.Sprintf("settings %d remote %v", i, remote), func(t *testing.T) {
				t.Parallel()

				before := maps.Clone(settings)
				want := firstErrorDiagnostic(settings)

				conversion, fault := decl.Convert(registry.AgentConversionInput{Command: domain.AgentCommand{Line: "kiro-cli"}, Settings: settings, Remote: remote})

				if (fault != nil) != (want != nil) {
					t.Fatalf("Convert(%v, remote=%v) fault = %+v, validator error = %+v, want both or neither", settings, remote, fault, want)
				}
				if want != nil {
					if fault.Message != want.Message || fault.Key != keyOfCheck(want.Check) {
						t.Errorf("Convert(%v, remote=%v) fault = {Key: %q, Message: %q}, want {Key: %q, Message: %q}", settings, remote, fault.Key, fault.Message, keyOfCheck(want.Check), want.Message)
					}
					if !conversion.Command.IsZero() {
						t.Errorf("Convert(%v, remote=%v) returned command %+v beside a fault", settings, remote, conversion.Command)
					}
				} else if conversion.Command.IsZero() {
					t.Errorf("Convert(%v, remote=%v) returned a zero command without a fault", settings, remote)
				}
				if !reflect.DeepEqual(before, settings) {
					t.Errorf("Convert changed its settings to %v, want %v", settings, before)
				}
			})
		}
	}
}
