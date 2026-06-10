package config

import (
	"errors"
	"testing"
)

func TestNotificationsConfig_AbsentSection(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(map[string]any{})
	if err != nil {
		t.Fatalf("NewServiceConfig(empty map): %v", err)
	}
	if cfg.Notifications.Backends != nil {
		t.Errorf("Notifications.Backends = %v, want nil when section absent", cfg.Notifications.Backends)
	}
}

func TestNotificationsConfig_ValidTwoEntries(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind":            "webhook",
				"url":             "https://example.com/hook",
				"max_per_session": 10,
			},
			map[string]any{
				"kind":            "slack",
				"webhook_url":     "https://hooks.slack.com/T/B/SECRET",
				"max_per_session": 0,
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	if len(cfg.Notifications.Backends) != 2 {
		t.Fatalf("Notifications.Backends len = %d, want 2", len(cfg.Notifications.Backends))
	}

	first := cfg.Notifications.Backends[0]
	if first.Kind != "webhook" {
		t.Errorf("Backends[0].Kind = %q, want %q", first.Kind, "webhook")
	}
	if first.MaxPerSession != 10 {
		t.Errorf("Backends[0].MaxPerSession = %d, want 10", first.MaxPerSession)
	}
	if url, ok := first.Config["url"].(string); !ok || url != "https://example.com/hook" {
		t.Errorf("Backends[0].Config[\"url\"] = %v, want %q", first.Config["url"], "https://example.com/hook")
	}

	second := cfg.Notifications.Backends[1]
	if second.Kind != "slack" {
		t.Errorf("Backends[1].Kind = %q, want %q", second.Kind, "slack")
	}
	if second.MaxPerSession != 0 {
		t.Errorf("Backends[1].MaxPerSession = %d, want 0", second.MaxPerSession)
	}
	if wurl, ok := second.Config["webhook_url"].(string); !ok || wurl != "https://hooks.slack.com/T/B/SECRET" {
		t.Errorf("Backends[1].Config[\"webhook_url\"] = %v, want non-empty", second.Config["webhook_url"])
	}
}

func TestNotificationsConfig_MissingKind(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"webhook_url": "https://hooks.slack.com/T/B/SECRET",
			},
		},
	}

	_, err := NewServiceConfig(raw)
	assertConfigErrorField(t, err, "notifications[0].kind")
}

func TestNotificationsConfig_EmptyKind(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind": "",
			},
		},
	}

	_, err := NewServiceConfig(raw)
	assertConfigErrorField(t, err, "notifications[0].kind")
}

func TestNotificationsConfig_NonSequenceValue(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": "not-a-sequence",
	}

	_, err := NewServiceConfig(raw)
	assertConfigErrorField(t, err, "notifications")
}

func TestNotificationsConfig_NonSequenceMapValue(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": map[string]any{
			"kind": "webhook",
		},
	}

	_, err := NewServiceConfig(raw)
	assertConfigErrorField(t, err, "notifications")
}

func TestNotificationsConfig_NegativeMaxPerSession(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind":            "webhook",
				"url":             "https://example.com/hook",
				"max_per_session": -1,
			},
		},
	}

	_, err := NewServiceConfig(raw)
	assertConfigErrorField(t, err, "notifications[0].max_per_session")
}

func TestNotificationsConfig_ZeroMaxPerSession_Valid(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind":            "webhook",
				"url":             "https://example.com/hook",
				"max_per_session": 0,
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig with max_per_session=0: %v", err)
	}
	if cfg.Notifications.Backends[0].MaxPerSession != 0 {
		t.Errorf("MaxPerSession = %d, want 0", cfg.Notifications.Backends[0].MaxPerSession)
	}
}

func TestNotificationsConfig_VarResolution_Set(t *testing.T) {
	// t.Setenv is not compatible with t.Parallel.
	t.Setenv("SORTIE_TEST_WEBHOOK_URL", "https://resolved.example.com/hook")

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind": "webhook",
				"url":  "$SORTIE_TEST_WEBHOOK_URL",
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig with $VAR url: %v", err)
	}

	if len(cfg.Notifications.Backends) != 1 {
		t.Fatalf("Backends len = %d, want 1", len(cfg.Notifications.Backends))
	}

	got, ok := cfg.Notifications.Backends[0].Config["url"].(string)
	if !ok {
		t.Fatalf("Config[\"url\"] type = %T, want string", cfg.Notifications.Backends[0].Config["url"])
	}
	if got != "https://resolved.example.com/hook" {
		t.Errorf("Config[\"url\"] = %q, want %q", got, "https://resolved.example.com/hook")
	}
}

func TestNotificationsConfig_VarResolution_Unset(t *testing.T) {
	// Ensure the variable is not set; t.Setenv with empty string
	// sets it to empty but here we use an unset variable name.
	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind":        "slack",
				"webhook_url": "$SORTIE_NOTIFICATIONS_TEST_UNSET_VARIABLE_XYZ",
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig with unset $VAR: %v", err)
	}

	got, ok := cfg.Notifications.Backends[0].Config["webhook_url"].(string)
	if !ok {
		t.Fatalf("Config[\"webhook_url\"] type = %T, want string", cfg.Notifications.Backends[0].Config["webhook_url"])
	}
	if got != "" {
		t.Errorf("Config[\"webhook_url\"] = %q, want empty string for unset variable", got)
	}
}

func TestNotificationsConfig_BraceVarResolution(t *testing.T) {
	// t.Setenv is not compatible with t.Parallel.
	t.Setenv("SORTIE_TEST_WEBHOOK_BRACE", "https://brace.example.com/hook")

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind": "webhook",
				"url":  "${SORTIE_TEST_WEBHOOK_BRACE}",
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig with ${VAR} url: %v", err)
	}

	got, ok := cfg.Notifications.Backends[0].Config["url"].(string)
	if !ok {
		t.Fatalf("Config[\"url\"] type = %T, want string", cfg.Notifications.Backends[0].Config["url"])
	}
	if got != "https://brace.example.com/hook" {
		t.Errorf("Config[\"url\"] = %q, want %q", got, "https://brace.example.com/hook")
	}
}

func TestNotificationsConfig_FrontMatterNoWarning(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind": "webhook",
				"url":  "https://example.com/hook",
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	warnings := ValidateFrontMatter(raw, cfg)
	for _, w := range warnings {
		if w.Field == "notifications" || (len(w.Field) > 13 && w.Field[:13] == "notifications") {
			t.Errorf("unexpected front-matter warning for notifications: check=%q field=%q msg=%q",
				w.Check, w.Field, w.Message)
		}
	}
}

func TestNotificationsConfig_KindNotPropagatedToConfig(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind":            "webhook",
				"url":             "https://example.com/hook",
				"max_per_session": 5,
			},
		},
	}

	cfg, err := NewServiceConfig(raw)
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	backend := cfg.Notifications.Backends[0]

	if _, present := backend.Config["kind"]; present {
		t.Error("Config map contains \"kind\"; it should be stripped into the Kind field")
	}
	if _, present := backend.Config["max_per_session"]; present {
		t.Error("Config map contains \"max_per_session\"; it should be stripped into MaxPerSession")
	}
}

func TestNotificationsConfig_ErrorIsPtrConfigError(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{"kind": ""},
		},
	}

	_, err := NewServiceConfig(raw)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *ConfigError", err)
	}
}
