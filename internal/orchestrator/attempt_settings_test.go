package orchestrator

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func settingsMeta() registry.AgentMeta {
	return registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionPerModel,
		UsageSessionRules: []registry.UsageSessionRule{{
			When:        func(passthrough map[string]any, remote bool) bool { return remote || passthrough["quiet"] == true },
			Arrival:     registry.UsageArrivalNone,
			Attribution: registry.UsageAttributionNone,
		}},
		SessionResumeBlockedBy: func(passthrough map[string]any) string {
			if persist, ok := passthrough["session_persistence"].(bool); ok && !persist {
				return "session_persistence"
			}
			return ""
		},
		ValidateAgentConfig: func(fields registry.AgentConfigFields) []registry.ValidationDiag {
			var diags []registry.ValidationDiag
			if _, fault := registry.ModelSetting(fields.Passthrough); fault != nil {
				diags = append(diags, registry.ValidationDiag{Severity: "error", Check: fields.Kind + ".model.wrong_type", Message: fault.Error()})
			}
			if mode, _ := fields.Passthrough["permission_mode"].(string); mode != "" && mode != "bypassPermissions" {
				diags = append(diags, registry.ValidationDiag{Severity: "error", Check: fields.Kind + ".permission_mode.interactive", Message: "permission mode asks"})
			}
			if typeutil.BoolFrom(fields.Passthrough, "noisy", false) {
				diags = append(diags, registry.ValidationDiag{Severity: "warning", Check: fields.Kind + ".noisy", Message: "noisy"})
			}
			return diags
		},
	}
}

func attemptSettingsOrchestrator(cfg config.ServiceConfig, metas map[string]registry.AgentMeta) *Orchestrator {
	return &Orchestrator{
		workflowManager: &stubWorkflowManager{config: cfg, absPath: "/wf/WORKFLOW.md"},
		preflightParams: PreflightParams{AgentRegistry: &stubAgentRegistry{
			metaFunc: func(kind string) (registry.AgentMeta, bool) {
				meta, ok := metas[kind]
				return meta, ok
			},
		}},
	}
}

func settingsConfig(topLevel map[string]any, rules ...config.DispatchRule) config.ServiceConfig {
	cfg := config.ServiceConfig{Agent: config.AgentConfig{Kind: "kind-a", Command: "a-cmd"}}
	if topLevel != nil {
		cfg.SetExtensionSection("kind-a", topLevel)
	}
	cfg.SetDispatch(config.DispatchConfig{Rules: rules})
	return cfg
}

func TestResolveAttemptSettings_UsagePair(t *testing.T) {
	t.Parallel()

	quiet := config.DispatchRule{Name: "quiet-rule", SettingsKind: "kind-a", Settings: map[string]any{"quiet": true}}
	tests := []struct {
		name        string
		kind        string
		sshHost     string
		ruleName    string
		wantArrival registry.UsageArrival
		wantAttrib  registry.UsageAttribution
	}{
		{name: "declared pair for a local launch", kind: "kind-a", wantArrival: registry.UsageArrivalIncremental, wantAttrib: registry.UsageAttributionPerModel},
		{name: "session rule applies to a remote launch", kind: "kind-a", sshHost: "host-1", wantArrival: registry.UsageArrivalNone, wantAttrib: registry.UsageAttributionNone},
		{name: "session rule reads the selected rule's block", kind: "kind-a", ruleName: "quiet-rule", wantArrival: registry.UsageArrivalNone, wantAttrib: registry.UsageAttributionNone},
		{name: "unregistered kind resolves the undeclared pair", kind: "ghost", wantArrival: registry.UsageArrivalUndeclared, wantAttrib: registry.UsageAttributionUndeclared},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := settingsConfig(nil, quiet)
			o := attemptSettingsOrchestrator(cfg, map[string]registry.AgentMeta{"kind-a": settingsMeta()})

			got := o.resolveAttemptSettings(cfg, DispatchResolution{AgentKind: tt.kind, RuleName: tt.ruleName}, tt.sshHost)

			if got.UsageArrival != tt.wantArrival || got.UsageAttribution != tt.wantAttrib {
				t.Errorf("resolveAttemptSettings(%q, rule %q, host %q) usage = %q, %q, want %q, %q", tt.kind, tt.ruleName, tt.sshHost, got.UsageArrival, got.UsageAttribution, tt.wantArrival, tt.wantAttrib)
			}
		})
	}
}

func TestResolveAttemptSettings_Refusals(t *testing.T) {
	t.Parallel()

	const interactive = "kind-a.permission_mode.interactive"
	rule := func(block map[string]any) *config.DispatchRule {
		return &config.DispatchRule{Name: "r", SettingsKind: "kind-a", Settings: block}
	}
	tests := []struct {
		name       string
		topLevel   map[string]any
		rule       *config.DispatchRule
		wantChecks []string
	}{
		{name: "clean block", topLevel: map[string]any{"model": "m"}},
		{name: "no block at all"},
		{name: "interactive permission mode", topLevel: map[string]any{"permission_mode": "default"}, wantChecks: []string{interactive}},
		{name: "session resume blocker", topLevel: map[string]any{"session_persistence": false}, wantChecks: []string{"agent.kind.session_resume"}},
		{name: "wrong-typed key", topLevel: map[string]any{"model": 7}, wantChecks: []string{"kind-a.model.wrong_type"}},
		{name: "warnings never enter the refusals", topLevel: map[string]any{"noisy": true}},
		{name: "a rule block that overlays a bad value over a clean top-level block", topLevel: map[string]any{"model": "m"}, rule: rule(map[string]any{"permission_mode": "acceptEdits"}), wantChecks: []string{interactive}},
		{name: "a rule block that clears a bad top-level value", topLevel: map[string]any{"permission_mode": "default"}, rule: rule(map[string]any{"permission_mode": nil})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rules []config.DispatchRule
			selection := DispatchResolution{AgentKind: "kind-a"}
			if tt.rule != nil {
				rules, selection.RuleName = append(rules, *tt.rule), tt.rule.Name
			}
			cfg := settingsConfig(tt.topLevel, rules...)
			o := attemptSettingsOrchestrator(cfg, map[string]registry.AgentMeta{"kind-a": settingsMeta()})

			got := o.resolveAttemptSettings(cfg, selection, "")

			var checks []string
			for _, refusal := range got.Refusals {
				checks = append(checks, refusal.Check)
			}
			if !slices.Equal(checks, tt.wantChecks) {
				t.Errorf("resolveAttemptSettings(%+v) refusal checks = %v, want %v", selection, checks, tt.wantChecks)
			}
		})
	}
}

func TestSettingsDiagnostics_SplitsSeveritiesAndSkipsUnregisteredKinds(t *testing.T) {
	t.Parallel()

	cfg := settingsConfig(map[string]any{"permission_mode": "default", "noisy": true})
	settings := config.ResolveAgentSettings(cfg, config.SettingsSelection{Kind: "kind-a"}, "")

	errs, warns := settingsDiagnostics(cfg, settingsMeta(), true, settings, false)
	skippedErrs, skippedWarns := settingsDiagnostics(cfg, settingsMeta(), false, settings, false)

	if len(errs) != 1 || errs[0].Check != "kind-a.permission_mode.interactive" || len(warns) != 1 || warns[0].Check != "kind-a.noisy" {
		t.Errorf("settingsDiagnostics errors, warnings = %+v, %+v, want the permission_mode check and the noisy check", errs, warns)
	}
	if len(skippedErrs) != 0 || len(skippedWarns) != 0 {
		t.Errorf("settingsDiagnostics for an unregistered kind = %+v, %+v, want nothing", skippedErrs, skippedWarns)
	}
}
