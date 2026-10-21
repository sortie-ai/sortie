package pi

import (
	"maps"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// TestValidateConfig_AcceptsEveryKey covers each accepted key on its own,
// so a key added to knownConfigKeys without a reader wired to it fails
// here rather than at runtime. Completeness runs in both directions: a
// known key with no value below, and a value below that is not a known
// key.
func TestValidateConfig_AcceptsEveryKey(t *testing.T) {
	t.Parallel()

	accepted := map[string]any{
		"model":         "anthropic/claude-sonnet-4-5",
		"effort":        "high",
		"mcp_config":    "./mcp-servers.json",
		"project_trust": "approve",
	}

	for _, key := range slices.Sorted(maps.Keys(knownConfigKeys)) {
		if _, ok := accepted[key]; !ok {
			t.Errorf("knownConfigKeys has %q, want this table to carry an accepted value for it", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(accepted)) {
		if _, ok := knownConfigKeys[key]; !ok {
			t.Errorf("this table accepts %q, want it to be a knownConfigKeys entry", key)
		}
	}

	extra := map[string]any{
		"trust unset":              map[string]any{"project_trust": ""},
		"trust ignore":             map[string]any{"project_trust": "ignore"},
		"model unset":              map[string]any{"model": ""},
		"nil value reads as unset": map[string]any{"model": nil},
	}

	extra["effort of any type is not read"] = map[string]any{"effort": 7}

	for _, name := range slices.Sorted(maps.Keys(accepted)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertNoDiagnostics(t, map[string]any{name: accepted[name]})
		})
	}
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertNoDiagnostics(t, extra[name].(map[string]any))
		})
	}
}

// assertNoDiagnostics fails t when validateConfig reports anything for
// config.
func assertNoDiagnostics(t *testing.T, config map[string]any) {
	t.Helper()

	diags := validateConfig(registry.AgentConfigFields{Kind: "pi", Passthrough: config})
	if len(diags) != 0 {
		t.Errorf("validateConfig(%v) = %+v, want no diagnostics", config, diags)
	}
}
