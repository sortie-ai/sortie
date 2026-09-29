package config

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	retiredKind        = "legacy"
	retiredTwinKind    = "legacy-twin"
	retiredDivergent   = "legacy-divergent"
	retiredElsewhere   = "legacy-elsewhere"
	replacementKind    = "modern"
	elsewhereKind      = "elsewhere"
	plainKind          = "plain"
	fixtureCredential  = "LEGACY_KEY"
	fixtureDefaultExec = "legacy-cli"
)

func fixtureConvert(in registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
	settings := in.Settings
	if _, refused := settings["bad"]; refused {
		return registry.AgentConversion{}, &registry.AgentConversionFault{Key: "bad", Message: "bad is refused"}
	}
	if _, refused := settings["remote_bad"]; refused && in.Remote {
		return registry.AgentConversion{}, &registry.AgentConversionFault{Key: "remote_bad", Message: "remote_bad is refused"}
	}

	args := []string{"serve"}
	if in.Remote {
		args = append(args, "--remote")
	}
	model, _ := settings["model"].(string)
	if model != "" {
		args = append(args, "--model", model)
	}

	base := in.Command
	conversion := registry.AgentConversion{Args: args}
	if !base.NamesExecutable() {
		base = domain.AgentCommand{Line: fixtureDefaultExec}
		conversion.DefaultCommand = fixtureDefaultExec
	}
	if len(base.Argv) > 0 || strings.Contains(model, " ") {
		words := base.Argv
		if len(words) == 0 {
			words = strings.Fields(base.Line)
		}
		conversion.Command = domain.AgentCommand{Argv: append(slices.Clone(words), args...)}
	} else {
		conversion.Command = domain.AgentCommand{Line: base.Line + " " + strings.Join(args, " ")}
	}

	mode := "local"
	if in.Remote {
		mode = "remote"
	}
	conversion.Settings = map[string]any{"mode": mode, "literal": "$NOT_RESOLVED_AGAIN"}
	if _, present := settings["model"]; present {
		conversion.Carried = []string{"model"}
	}
	return conversion, nil
}

func fixtureDivergentConvert(registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
	return registry.AgentConversion{
		Command: domain.AgentCommand{Line: "divergent-cli run"},
		Args:    []string{"run"},
	}, nil
}

func fixtureLookup(kind string) (registry.RetiredAgent, bool) {
	credential := registry.DeclareCredentialEnv(fixtureCredential)
	switch kind {
	case retiredKind, retiredTwinKind:
		return registry.RetiredAgent{Replacement: replacementKind, CredentialEnv: credential, Convert: fixtureConvert}, true
	case retiredDivergent:
		return registry.RetiredAgent{Replacement: replacementKind, CredentialEnv: credential, Convert: fixtureDivergentConvert}, true
	case retiredElsewhere:
		return registry.RetiredAgent{Replacement: elsewhereKind, CredentialEnv: credential, Convert: fixtureConvert}, true
	}
	return registry.RetiredAgent{}, false
}

func ruleNaming(kind string) map[string]any {
	return map[string]any{"match": map[string]any{"labels": []any{"backend"}}, "agent": kind}
}

func withSSHHosts(raw map[string]any) map[string]any {
	raw["worker"] = map[string]any{"ssh_hosts": []any{"host-1"}}
	return raw
}

func loadConverted(t *testing.T, raw map[string]any) ServiceConfig {
	t.Helper()
	cfg, err := NewServiceConfig(raw, WithRetiredAgents(fixtureLookup))
	if err != nil {
		t.Fatalf("NewServiceConfig() error = %v", err)
	}
	return cfg
}

func loadConvertedWithDispatch(t *testing.T, raw map[string]any) ServiceConfig {
	t.Helper()
	cfg := loadConverted(t, raw)
	dispatch, err := BuildDispatchConfig(raw, mkDispatchDir(t), alwaysRegistered)
	if err != nil {
		t.Fatalf("BuildDispatchConfig() error = %v", err)
	}
	cfg.SetDispatch(dispatch)
	return cfg
}

func requireConversionError(t *testing.T, err error, wantField, wantMessage string) {
	t.Helper()
	ce := requireConfigError(t, err)
	if ce.Field != wantField {
		t.Errorf("ConfigError.Field = %q, want %q", ce.Field, wantField)
	}
	if ce.Message != wantMessage {
		t.Errorf("ConfigError.Message = %q, want %q", ce.Message, wantMessage)
	}
}

func agentSection(raw map[string]any) map[string]any {
	section, _ := raw["agent"].(map[string]any)
	return section
}

type governanceRow struct {
	name       string
	raw        func() map[string]any
	governs    bool
	wantLocal  domain.AgentCommand
	wantRemote domain.AgentCommand
}

var governanceRows = []governanceRow{
	{
		name: "default kind is the retired kind, string command",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": retiredKind, "command": "legacy-wrapper --x"},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		governs:    true,
		wantLocal:  domain.AgentCommand{Line: "legacy-wrapper --x serve --model m1"},
		wantRemote: domain.AgentCommand{Line: "legacy-wrapper --x serve --remote --model m1"},
	},
	{
		name: "default kind is the retired kind through dispatch.default.agent",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": plainKind, "command": "legacy-wrapper --x"},
				"dispatch":  map[string]any{"default": map[string]any{"agent": retiredKind}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		governs:    true,
		wantLocal:  domain.AgentCommand{Line: "legacy-wrapper --x serve --model m1"},
		wantRemote: domain.AgentCommand{Line: "legacy-wrapper --x serve --remote --model m1"},
	},
	{
		name: "default kind is the retired kind, list command",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": retiredKind, "command": []any{"legacy wrapper", "--x"}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		governs:    true,
		wantLocal:  domain.AgentCommand{Argv: []string{"legacy wrapper", "--x", "serve", "--model", "m1"}},
		wantRemote: domain.AgentCommand{Argv: []string{"legacy wrapper", "--x", "serve", "--remote", "--model", "m1"}},
	},
	{
		name: "default kind is another kind",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": plainKind, "command": "plain-cmd"},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		governs:    true,
		wantLocal:  domain.AgentCommand{Line: "legacy-cli serve --model m1"},
		wantRemote: domain.AgentCommand{Line: "legacy-cli serve --remote --model m1"},
	},
	{
		name: "no agent.kind leaves the built-in default kind",
		raw: func() map[string]any {
			return map[string]any{
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		governs:    true,
		wantLocal:  domain.AgentCommand{Line: "legacy-cli serve --model m1"},
		wantRemote: domain.AgentCommand{Line: "legacy-cli serve --remote --model m1"},
	},
	{
		name: "default kind is the replacement kind",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": replacementKind, "command": "modern-cmd"},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		wantLocal:  domain.AgentCommand{Line: "modern-cmd"},
		wantRemote: domain.AgentCommand{Line: "modern-cmd"},
	},
	{
		name: "default kind is the replacement kind through dispatch.default.agent",
		raw: func() map[string]any {
			return map[string]any{
				"agent":     map[string]any{"kind": plainKind, "command": "modern-cmd"},
				"dispatch":  map[string]any{"default": map[string]any{"agent": replacementKind}, "rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			}
		},
		wantLocal:  domain.AgentCommand{Line: "modern-cmd"},
		wantRemote: domain.AgentCommand{Line: "modern-cmd"},
	},
}

func TestRetiredConversion_CommandGovernanceRecords(t *testing.T) {
	t.Parallel()

	for _, tt := range governanceRows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := loadConverted(t, tt.raw())

			records := cfg.AgentKindConversions()
			if len(records) != 1 {
				t.Fatalf("AgentKindConversions() = %+v, want one record", records)
			}
			record := records[0]
			if tt.governs {
				if !commandsEqual(record.Command, tt.wantLocal) || !commandsEqual(record.RemoteCommand, tt.wantRemote) {
					t.Errorf("record commands = %+v and %+v, want %+v and %+v", record.Command, record.RemoteCommand, tt.wantLocal, tt.wantRemote)
				}
				if !slices.Equal(record.CredentialEnv, []string{fixtureCredential}) {
					t.Errorf("record.CredentialEnv = %v, want %v", record.CredentialEnv, []string{fixtureCredential})
				}
				return
			}
			if !record.Command.IsZero() || !record.RemoteCommand.IsZero() {
				t.Errorf("record commands = %+v and %+v, want both zero for a conversion that does not govern", record.Command, record.RemoteCommand)
			}
			if record.CredentialEnv != nil {
				t.Errorf("record.CredentialEnv = %v, want nil for a conversion that does not govern", record.CredentialEnv)
			}
		})
	}
}

func TestRetiredConversion_RewritesEveryReference(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"agent": map[string]any{"kind": retiredKind},
		"dispatch": map[string]any{
			"default": map[string]any{"agent": retiredKind},
			"rules":   []any{ruleNaming(retiredKind), ruleNaming(plainKind), ruleNaming(retiredKind)},
		},
		retiredKind: map[string]any{"model": "m1"},
	}

	cfg := loadConverted(t, raw)

	records := cfg.AgentKindConversions()
	if len(records) != 1 {
		t.Fatalf("AgentKindConversions() = %+v, want one record", records)
	}
	wantFields := []string{"agent.kind", "dispatch.default.agent", "dispatch.rules[0].agent", "dispatch.rules[2].agent"}
	if records[0].Kind != retiredKind || records[0].Replacement != replacementKind || !slices.Equal(records[0].Fields, wantFields) {
		t.Errorf("record = %+v, want kind %q onto %q with fields %v", records[0], retiredKind, replacementKind, wantFields)
	}
	if cfg.Agent.Kind != replacementKind {
		t.Errorf("Agent.Kind = %q, want %q", cfg.Agent.Kind, replacementKind)
	}

	probe := func(kind string) bool { return kind == replacementKind || kind == plainKind }
	dispatch, err := BuildDispatchConfig(raw, mkDispatchDir(t), probe)
	if err != nil {
		t.Fatalf("BuildDispatchConfig() on the rewritten map error = %v, want the converted kinds to pass the probe", err)
	}
	gotKinds := []string{dispatch.Default.AgentKind}
	for _, rule := range dispatch.Rules {
		gotKinds = append(gotKinds, rule.Selection.AgentKind)
	}
	if want := []string{replacementKind, replacementKind, plainKind, replacementKind}; !slices.Equal(gotKinds, want) {
		t.Errorf("dispatch agent kinds = %v, want %v", gotKinds, want)
	}
}

func TestRetiredConversion_ReplacementBlock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		block     any
		hasBlock  bool
		remote    bool
		wantBlock any
	}{
		{
			name:      "absent block becomes the converted settings",
			wantBlock: map[string]any{"mode": "local", "literal": "$NOT_RESOLVED_AGAIN"},
		},
		{
			name:      "null block becomes the converted settings",
			hasBlock:  true,
			wantBlock: map[string]any{"mode": "local", "literal": "$NOT_RESOLVED_AGAIN"},
		},
		{
			name:      "existing keys win over converted settings",
			block:     map[string]any{"mode": "own", "extra": "kept"},
			hasBlock:  true,
			wantBlock: map[string]any{"mode": "own", "extra": "kept", "literal": "$NOT_RESOLVED_AGAIN"},
		},
		{
			name:      "settings follow the configuration's launch mode",
			remote:    true,
			wantBlock: map[string]any{"mode": "remote", "literal": "$NOT_RESOLVED_AGAIN"},
		},
		{
			name:      "block that is not a mapping stays as written",
			block:     "not a mapping",
			hasBlock:  true,
			wantBlock: "not a mapping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := map[string]any{
				"agent":     map[string]any{"kind": retiredKind},
				retiredKind: map[string]any{"model": "m1"},
			}
			if tt.hasBlock {
				raw[replacementKind] = tt.block
			}
			if tt.remote {
				withSSHHosts(raw)
			}

			cfg := loadConverted(t, raw)

			got, present := cfg.ExtensionValue(replacementKind)
			if !present {
				t.Fatalf("ExtensionValue(%q) reports absent, want the replacement block", replacementKind)
			}
			if !equalBlocks(got, tt.wantBlock) {
				t.Errorf("ExtensionValue(%q) = %v, want %v", replacementKind, got, tt.wantBlock)
			}
			if _, present := cfg.ExtensionValue(retiredKind); present {
				t.Errorf("ExtensionValue(%q) reports present, want the retired block removed", retiredKind)
			}
			if _, present := raw[retiredKind]; present {
				t.Errorf("raw[%q] is still present, want the retired block removed", retiredKind)
			}
		})
	}
}

func equalBlocks(got, want any) bool {
	gotMap, gotIsMap := got.(map[string]any)
	wantMap, wantIsMap := want.(map[string]any)
	if gotIsMap != wantIsMap {
		return false
	}
	if !gotIsMap {
		return got == want
	}
	if len(gotMap) != len(wantMap) {
		return false
	}
	for key, value := range wantMap {
		if gotMap[key] != value {
			return false
		}
	}
	return true
}

func TestRetiredConversion_ConvertSeesResolvedReferences(t *testing.T) {
	t.Setenv("SORTIE_CONFIG_TEST_LEGACY_MODEL", "model-from-env")

	cfg := loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind},
		retiredKind: map[string]any{"model": "$SORTIE_CONFIG_TEST_LEGACY_MODEL"},
	})

	want := domain.AgentCommand{Line: "legacy-cli serve --model model-from-env"}
	if got := cfg.AgentKindConversions()[0].Command; !commandsEqual(got, want) {
		t.Errorf("record.Command = %+v, want %+v", got, want)
	}
}

func TestRetiredConversion_RetiredBlockLeavesStayMasked(t *testing.T) {
	t.Parallel()

	secret := randomConfigSecret(t)

	loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind},
		retiredKind: map[string]any{"api_key": secret},
	})

	requireConfigMasked(t, secret)
}

func TestRetiredConversion_FaultsAreConfigErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		settings    map[string]any
		wantField   string
		wantMessage string
	}{
		{
			name:        "fault in both launch modes",
			settings:    map[string]any{"bad": true},
			wantField:   "legacy.bad",
			wantMessage: `agent kind "legacy" was removed and this configuration cannot be converted to agent kind "modern": bad is refused`,
		},
		{
			name:        "fault only in the remote launch mode",
			settings:    map[string]any{"remote_bad": true},
			wantField:   "legacy.remote_bad",
			wantMessage: `agent kind "legacy" was removed and this configuration cannot be converted to agent kind "modern": remote_bad is refused`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewServiceConfig(map[string]any{
				"agent":     map[string]any{"kind": retiredKind},
				retiredKind: tt.settings,
			}, WithRetiredAgents(fixtureLookup))

			requireConversionError(t, err, tt.wantField, tt.wantMessage)
		})
	}
}

func TestRetiredConversion_MalformedAgentCommandStaysAnAgentCommandError(t *testing.T) {
	t.Parallel()

	_, err := NewServiceConfig(map[string]any{
		"agent":     map[string]any{"kind": retiredKind, "command": []any{}},
		retiredKind: map[string]any{},
	}, WithRetiredAgents(fixtureLookup))

	requireConversionError(t, err, "agent.command", "must not be an empty list")
}

func TestRetiredConversion_SharedReplacementConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       map[string]any
		wantField string
		wantKinds int
	}{
		{
			name: "different commands for one replacement kind",
			raw: map[string]any{
				"agent":    map[string]any{"kind": plainKind},
				"dispatch": map[string]any{"rules": []any{ruleNaming(retiredKind), ruleNaming(retiredDivergent)}},
			},
			wantField: "dispatch.rules[1].agent",
		},
		{
			name: "equal commands for one replacement kind",
			raw: map[string]any{
				"agent":    map[string]any{"kind": plainKind},
				"dispatch": map[string]any{"rules": []any{ruleNaming(retiredKind), ruleNaming(retiredTwinKind)}},
			},
			wantKinds: 2,
		},
		{
			name: "records that govern nothing never conflict",
			raw: map[string]any{
				"agent":    map[string]any{"kind": replacementKind},
				"dispatch": map[string]any{"rules": []any{ruleNaming(retiredKind), ruleNaming(retiredDivergent)}},
			},
			wantKinds: 2,
		},
		{
			name: "different replacement kinds never conflict",
			raw: map[string]any{
				"agent":    map[string]any{"kind": plainKind},
				"dispatch": map[string]any{"rules": []any{ruleNaming(retiredKind), ruleNaming(retiredElsewhere)}},
			},
			wantKinds: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := NewServiceConfig(tt.raw, WithRetiredAgents(fixtureLookup))

			if tt.wantField != "" {
				later := retiredDivergent
				requireConversionError(t, err, tt.wantField,
					`agent kinds "`+retiredKind+`" and "`+later+`" were removed and convert to agent kind "modern" with different commands; name agent kind "modern" in the workflow file with one agent.command`)
				return
			}
			if err != nil {
				t.Fatalf("NewServiceConfig() error = %v", err)
			}
			if got := len(cfg.AgentKindConversions()); got != tt.wantKinds {
				t.Errorf("len(AgentKindConversions()) = %d, want %d", got, tt.wantKinds)
			}
		})
	}
}

func TestRetiredConversion_Advisory(t *testing.T) {
	t.Parallel()

	const tail = ". This conversion is temporary and will be removed in a later release: "

	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "default kind, arguments appended to agent.command",
			raw: map[string]any{
				"agent":     map[string]any{"kind": retiredKind, "command": "legacy-wrapper --x"},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1", "extra": "zz"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (agent.kind, dispatch.rules[0].agent); its sessions launch agent.command with the arguments serve --model m1; not carried: legacy.extra` + tail +
				`name agent kind "modern" where the workflow names "legacy" and give it this invocation in agent.command`,
		},
		{
			name: "string agent.command became a list",
			raw: map[string]any{
				"agent":     map[string]any{"kind": retiredKind, "command": "legacy-wrapper --x"},
				retiredKind: map[string]any{"model": "a b"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (agent.kind); its sessions launch agent.command written as a list, one element per word, followed by the arguments serve --model "a b"` + tail +
				`name agent kind "modern" where the workflow names "legacy" and give it this invocation in agent.command`,
		},
		{
			name: "dropped keys are sorted",
			raw: map[string]any{
				"agent":     map[string]any{"kind": retiredKind, "command": "legacy-wrapper"},
				retiredKind: map[string]any{"zeta": 1, "alpha": 2, "model": "m1"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (agent.kind); its sessions launch agent.command with the arguments serve --model m1; not carried: legacy.alpha, legacy.zeta` + tail +
				`name agent kind "modern" where the workflow names "legacy" and give it this invocation in agent.command`,
		},
		{
			name: "other default kind, string command shown quoted",
			raw: map[string]any{
				"agent":     map[string]any{"kind": plainKind, "command": "plain-cmd"},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (dispatch.rules[0].agent); its sessions launch "legacy-cli serve --model m1"` + tail +
				`agent kind "modern" launches this invocation only as the default agent kind, with it in agent.command`,
		},
		{
			name: "other default kind, list command shown in flow form",
			raw: map[string]any{
				"agent":     map[string]any{"kind": plainKind},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "a b"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (dispatch.rules[0].agent); its sessions launch ["legacy-cli", "serve", "--model", "a b"]` + tail +
				`agent kind "modern" launches this invocation only as the default agent kind, with it in agent.command`,
		},
		{
			name: "remote configuration names the carried credential",
			raw: withSSHHosts(map[string]any{
				"agent":     map[string]any{"kind": plainKind},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			}),
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (dispatch.rules[0].agent); its sessions launch "legacy-cli serve --remote --model m1", and a remote launch carries LEGACY_KEY as agent kind "legacy" did` + tail +
				`agent kind "modern" launches this invocation only as the default agent kind, with it in agent.command and list LEGACY_KEY under worker.ssh_pass_env`,
		},
		{
			name: "replacement already the default kind",
			raw: map[string]any{
				"agent":     map[string]any{"kind": replacementKind, "command": "modern-cmd"},
				"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredKind)}},
				retiredKind: map[string]any{"model": "m1"},
			},
			want: `agent kind "legacy" was removed, so this configuration was converted to agent kind "modern" (dispatch.rules[0].agent); agent kind "modern" is already the default agent kind, so its sessions keep agent.command and carry none of the "legacy" settings` + tail +
				`name agent kind "modern" in the workflow file`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := loadConverted(t, tt.raw)

			advisories := cfg.Advisories()
			if len(advisories) != 1 {
				t.Fatalf("Advisories() = %+v, want exactly one", advisories)
			}
			got := advisories[0]
			if got.Check != "agent.kind.retired" {
				t.Errorf("Advisory.Check = %q, want %q", got.Check, "agent.kind.retired")
			}
			if want := "agent kind was removed and its configuration was converted to the replacement kind; the conversion will be removed in a later release"; got.Message != want {
				t.Errorf("Advisory.Message = %q, want %q", got.Message, want)
			}
			if got.Text != tt.want {
				t.Errorf("Advisory.Text = %q, want %q", got.Text, tt.want)
			}
			wantAttrs := []slog.Attr{slog.String("agent_kind", retiredKind), slog.String("replacement_kind", replacementKind)}
			if len(got.Attrs) != len(wantAttrs) {
				t.Fatalf("Advisory.Attrs = %v, want %v", got.Attrs, wantAttrs)
			}
			for i, attr := range wantAttrs {
				if got.Attrs[i].Key != attr.Key || !got.Attrs[i].Value.Equal(attr.Value) {
					t.Errorf("Advisory.Attrs[%d] = %v, want %v", i, got.Attrs[i], attr)
				}
			}
		})
	}
}

func TestRetiredConversion_AdvisoryNeverPrintsAgentCommand(t *testing.T) {
	t.Parallel()

	const commandText = "sortie-fixture-secret-command"
	cfg := loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind, "command": commandText},
		retiredKind: map[string]any{"model": "m1"},
	})

	for _, advisory := range cfg.Advisories() {
		if strings.Contains(advisory.Text, commandText) {
			t.Errorf("Advisory.Text = %q, want it never to print agent.command as written", advisory.Text)
		}
	}
}

func TestRetiredConversion_AdvisoriesFollowEnvAndPrecedeTheRest(t *testing.T) {
	setDotEnvPathForTest(t, "")
	t.Setenv("SORTIE_ENV_FILE", t.TempDir()+"/absent.env")

	cfg := loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind},
		"dispatch":  map[string]any{"rules": []any{ruleNaming(retiredTwinKind)}},
		retiredKind: map[string]any{},
		"reactions": map[string]any{"label_commands": map[string]any{"provider": "github", "poll_interval_ms": 5000}},
	})

	var got []string
	for _, advisory := range cfg.Advisories() {
		got = append(got, advisory.Check)
	}
	want := []string{"env_file.missing", "agent.kind.retired", "agent.kind.retired", "reactions.label_commands.poll_interval_ms.clamped"}
	if !slices.Equal(got, want) {
		t.Errorf("Advisories() checks = %v, want %v", got, want)
	}
	if records := cfg.AgentKindConversions(); len(records) != 2 || records[0].Kind != retiredKind || records[1].Kind != retiredTwinKind {
		t.Errorf("AgentKindConversions() = %+v, want %q then %q", records, retiredKind, retiredTwinKind)
	}
}

func TestRetiredConversion_ConvertsNothingWithoutARetiredKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  func() map[string]any
		opts []ServiceConfigOption
	}{
		{
			name: "no lookup option",
			raw:  func() map[string]any { return map[string]any{"agent": map[string]any{"kind": retiredKind}} },
		},
		{
			name: "nil lookup",
			raw:  func() map[string]any { return map[string]any{"agent": map[string]any{"kind": retiredKind}} },
			opts: []ServiceConfigOption{WithRetiredAgents(nil)},
		},
		{
			name: "lookup reports no retirement",
			raw:  func() map[string]any { return map[string]any{"agent": map[string]any{"kind": retiredKind}} },
			opts: []ServiceConfigOption{WithRetiredAgents(func(string) (registry.RetiredAgent, bool) { return registry.RetiredAgent{}, false })},
		},
		{
			name: "workflow names the replacement directly",
			raw: func() map[string]any {
				return map[string]any{"agent": map[string]any{"kind": replacementKind}, "dispatch": map[string]any{"rules": []any{ruleNaming(replacementKind)}}}
			},
			opts: []ServiceConfigOption{WithRetiredAgents(fixtureLookup)},
		},
		{
			name: "workflow names the replacement beside a leftover retired block",
			raw: func() map[string]any {
				return map[string]any{"agent": map[string]any{"kind": replacementKind}, retiredKind: map[string]any{"model": "m1"}}
			},
			opts: []ServiceConfigOption{WithRetiredAgents(fixtureLookup)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := tt.raw()
			wantKind := agentSection(raw)["kind"]

			cfg, err := NewServiceConfig(raw, tt.opts...)

			if err != nil {
				t.Fatalf("NewServiceConfig() error = %v", err)
			}
			if cfg.Agent.Kind != wantKind {
				t.Errorf("Agent.Kind = %q, want %q", cfg.Agent.Kind, wantKind)
			}
			if records := cfg.AgentKindConversions(); records == nil || len(records) != 0 {
				t.Errorf("AgentKindConversions() = %#v, want an empty non-nil slice", records)
			}
			if advisories := cfg.Advisories(); len(advisories) != 0 {
				t.Errorf("Advisories() = %+v, want none", advisories)
			}
			if leftover, has := raw[retiredKind]; has && leftover == nil {
				t.Errorf("raw[%q] = nil, want the leftover block untouched", retiredKind)
			}
		})
	}
}

func TestRetiredConversion_BuildingTheConfigurationLogsNothing(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind},
		retiredKind: map[string]any{"model": "m1"},
	})

	if logged.Len() != 0 {
		t.Errorf("NewServiceConfig logged %q, want nothing", logged.String())
	}
}

func TestAgentKindConversions_ReturnsFreshCopies(t *testing.T) {
	t.Parallel()

	cfg := loadConverted(t, map[string]any{
		"agent":     map[string]any{"kind": retiredKind, "command": []any{"legacy-wrapper"}},
		retiredKind: map[string]any{"model": "m1"},
	})

	first := cfg.AgentKindConversions()
	first[0].Fields[0] = "mutated"
	first[0].Command.Argv[0] = "mutated"
	first[0].RemoteCommand.Argv[0] = "mutated"
	first[0].CredentialEnv[0] = "MUTATED"
	first[0].Kind = "mutated"
	second := cfg.AgentKindConversions()

	if second[0].Kind != retiredKind || second[0].Fields[0] != "agent.kind" ||
		second[0].Command.Argv[0] != "legacy-wrapper" || second[0].RemoteCommand.Argv[0] != "legacy-wrapper" ||
		second[0].CredentialEnv[0] != fixtureCredential {
		t.Errorf("AgentKindConversions() after mutating an earlier result = %+v, want the recorded values", second[0])
	}
}

func TestWorkerSSHHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		worker map[string]any
		want   []string
	}{
		{"nil section", nil, nil},
		{"no ssh_hosts key", map[string]any{"other": 1}, nil},
		{"ssh_hosts holds a string", map[string]any{"ssh_hosts": "host-1"}, nil},
		{"ssh_hosts holds a mapping", map[string]any{"ssh_hosts": map[string]any{"a": "b"}}, nil},
		{"empty list", map[string]any{"ssh_hosts": []any{}}, nil},
		{"hosts in order", map[string]any{"ssh_hosts": []any{"b", "a"}}, []string{"b", "a"}},
		{"duplicates kept", map[string]any{"ssh_hosts": []any{"a", "a"}}, []string{"a", "a"}},
		{"empty and non-string elements skipped", map[string]any{"ssh_hosts": []any{"a", "", 7, nil, "b"}}, []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := WorkerSSHHosts(tt.worker)

			if !slices.Equal(got, tt.want) {
				t.Errorf("WorkerSSHHosts(%v) = %v, want %v", tt.worker, got, tt.want)
			}
		})
	}
}
