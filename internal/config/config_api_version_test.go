package config

import "testing"

// TestBuildTrackerConfig_APIVersion verifies the tracker config builder
// carries api_version through and resolves $VAR indirection, mirroring
// the handling of the other optional tracker string fields.
func TestBuildTrackerConfig_APIVersion(t *testing.T) {
	t.Run("string value", func(t *testing.T) {
		t.Parallel()
		tc := buildTrackerConfig(map[string]any{"api_version": "2"}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "2", tc.APIVersion)
	})

	t.Run("absent yields empty", func(t *testing.T) {
		t.Parallel()
		tc := buildTrackerConfig(map[string]any{}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "", tc.APIVersion)
	})

	t.Run("non-string yields empty", func(t *testing.T) {
		t.Parallel()
		// extractString flattens a non-string to ""; the adapter applies
		// the "3" default for an empty value.
		tc := buildTrackerConfig(map[string]any{"api_version": 2}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "", tc.APIVersion)
	})

	t.Run("VAR indirection resolved", func(t *testing.T) {
		t.Setenv("TEST_API_VERSION", "2")
		tc := buildTrackerConfig(map[string]any{"api_version": "$TEST_API_VERSION"}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "2", tc.APIVersion)
	})

	t.Run("VAR indirection skipped when env-override guard set", func(t *testing.T) {
		t.Setenv("TEST_API_VERSION", "2")
		// When the SORTIE_* override path already populated the value, the
		// $VAR guard must leave the literal untouched, exactly as the other
		// tracker string fields behave.
		tc := buildTrackerConfig(
			map[string]any{"api_version": "$TEST_API_VERSION"},
			map[string]bool{"tracker.api_version": true},
		)
		assertStringEqual(t, "TrackerConfig.APIVersion", "$TEST_API_VERSION", tc.APIVersion)
	})
}

// TestNewServiceConfig_APIVersion exercises the same resolution through
// the public entry point so the value reaches TrackerConfig end to end.
func TestNewServiceConfig_APIVersion(t *testing.T) {
	t.Run("string value loads", func(t *testing.T) {
		t.Parallel()
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira", "api_version": "2"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.APIVersion", "2", cfg.Tracker.APIVersion)
	})

	t.Run("env var resolved", func(t *testing.T) {
		t.Setenv("TEST_SVC_API_VERSION", "2")
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira", "api_version": "$TEST_SVC_API_VERSION"},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig: %v", err)
		}
		assertStringEqual(t, "Tracker.APIVersion", "2", cfg.Tracker.APIVersion)
	})

	t.Run("bare YAML integer loads without fatal error", func(t *testing.T) {
		t.Parallel()
		// A bare integer is accepted (flattened to "" here; the adapter
		// applies its own coercion). Loading must not fail.
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira", "api_version": 2},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig with bare integer api_version: %v", err)
		}
		assertStringEqual(t, "Tracker.APIVersion", "", cfg.Tracker.APIVersion)
	})
}
