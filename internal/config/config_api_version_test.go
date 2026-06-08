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

	t.Run("bare integer coerced to string", func(t *testing.T) {
		t.Parallel()
		// A bare YAML integer (api_version: 2) is coerced to its decimal
		// string form so a Server/DC config is not silently defaulted to v3.
		tc := buildTrackerConfig(map[string]any{"api_version": 2}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "2", tc.APIVersion)
	})

	t.Run("bare whole float coerced to string", func(t *testing.T) {
		t.Parallel()
		tc := buildTrackerConfig(map[string]any{"api_version": float64(2)}, nil)
		assertStringEqual(t, "TrackerConfig.APIVersion", "2", tc.APIVersion)
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

	t.Run("bare YAML integer coerced and loads", func(t *testing.T) {
		t.Parallel()
		// A bare integer (api_version: 2) is coerced to "2" so a Server/DC
		// config is not silently defaulted to v3. Loading must not fail.
		cfg, err := NewServiceConfig(map[string]any{
			"tracker": map[string]any{"kind": "jira", "api_version": 2},
		})
		if err != nil {
			t.Fatalf("NewServiceConfig with bare integer api_version: %v", err)
		}
		assertStringEqual(t, "Tracker.APIVersion", "2", cfg.Tracker.APIVersion)
	})
}
