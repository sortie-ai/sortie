package config

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
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

func TestNotificationsConfig_WrongTypeKind(t *testing.T) {
	t.Parallel()

	raw := map[string]any{
		"notifications": []any{
			map[string]any{
				"kind": 123,
			},
		},
	}

	_, err := NewServiceConfig(raw)

	assertConfigErrorField(t, err, "notifications[0].kind")
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *ConfigError", err)
	}
	assertStringEqual(t, "ConfigError.Message", "expected string, got integer", ce.Message)
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

func TestNotificationsConfig_MaxPerSessionOutOfRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		val  any
	}{
		{"uint64", uint64(9223372036854775808)},
		{"float64", float64(1e20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw := map[string]any{
				"notifications": []any{
					map[string]any{
						"kind":            "webhook",
						"url":             "https://example.com/hook",
						"max_per_session": tt.val,
					},
				},
			}

			_, err := NewServiceConfig(raw)
			assertConfigErrorField(t, err, "notifications[0].max_per_session")
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("error type = %T, want *ConfigError", err)
			}
			assertStringEqual(t, "ConfigError.Message", ErrIntegerOutOfRange.Error(), ce.Message)
		})
	}
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
	// sets it to empty, but this uses an unset variable name instead.
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

	if _, ok := errors.AsType[*ConfigError](err); !ok {
		t.Fatalf("error type = %T, want *ConfigError", err)
	}
}

func trackerCommentRaw(entries ...any) map[string]any {
	return map[string]any{
		"tracker":       map[string]any{"kind": "file"},
		"notifications": entries,
	}
}

func catalogNames(types []domain.EventType) []any {
	names := make([]any, len(types))
	for i, eventType := range types {
		names[i] = string(eventType)
	}
	return names
}

func TestNotificationsConfig_EventsErrors(t *testing.T) {
	t.Parallel()

	catalog := make([]string, 0, len(domain.EventTypes()))
	for _, eventType := range domain.EventTypes() {
		catalog = append(catalog, string(eventType))
	}

	webhook := func(events any) map[string]any {
		return map[string]any{"kind": "webhook", "url": "https://example.com/hook", "events": events}
	}
	trackerComment := func(fields map[string]any) map[string]any {
		entry := map[string]any{"kind": domain.TrackerCommentKind}
		maps.Copy(entry, fields)
		return entry
	}

	tests := []struct {
		name        string
		raw         map[string]any
		wantField   string
		wantMessage string
	}{
		{
			name:        "events not a sequence",
			raw:         trackerCommentRaw(webhook("budget.held")),
			wantField:   "notifications[0].events",
			wantMessage: "expected sequence, got string",
		},
		{
			name:        "event not a string",
			raw:         trackerCommentRaw(webhook([]any{"budget.held", 7})),
			wantField:   "notifications[0].events[1]",
			wantMessage: "expected string, got integer",
		},
		{
			name:        "unknown event type lists the catalog",
			raw:         trackerCommentRaw(webhook([]any{"session.begun"})),
			wantField:   "notifications[0].events[0]",
			wantMessage: `unknown event type "session.begun"; valid: ` + strings.Join(catalog, ", "),
		},
		{
			name:        "repeated event type",
			raw:         trackerCommentRaw(webhook([]any{"budget.held", "session.failed", "budget.held"})),
			wantField:   "notifications[0].events[2]",
			wantMessage: `event type "budget.held" is listed more than once`,
		},
		{
			name:        "agent message on tracker comment",
			raw:         trackerCommentRaw(trackerComment(map[string]any{"events": []any{"session.failed", "agent.message"}})),
			wantField:   "notifications[0].events[1]",
			wantMessage: "agent.message cannot be posted as a tracker comment",
		},
		{
			name:        "tracker comment without events",
			raw:         trackerCommentRaw(trackerComment(nil)),
			wantField:   "notifications[0].events",
			wantMessage: "a tracker_comment entry must list its events; write [] to post only what the deprecated settings enable",
		},
		{
			name: "second tracker comment",
			raw: trackerCommentRaw(
				trackerComment(map[string]any{"events": []any{}}),
				webhook([]any{"budget.held"}),
				trackerComment(map[string]any{"events": []any{"budget.held"}}),
			),
			wantField:   "notifications[2].kind",
			wantMessage: "only one notifications entry may have kind tracker_comment",
		},
		{
			name: "tracker comment without a tracker",
			raw: map[string]any{
				"notifications": []any{trackerComment(map[string]any{"events": []any{}})},
			},
			wantField:   "notifications[0].kind",
			wantMessage: "kind tracker_comment requires a configured tracker",
		},
		{
			name:        "tracker comment with a pass-through key",
			raw:         trackerCommentRaw(trackerComment(map[string]any{"events": []any{}, "url": "https://example.com"})),
			wantField:   "notifications[0].url",
			wantMessage: "kind tracker_comment takes only kind and events",
		},
		{
			name:        "tracker comment with a cap",
			raw:         trackerCommentRaw(trackerComment(map[string]any{"events": []any{}, "max_per_session": 3})),
			wantField:   "notifications[0].max_per_session",
			wantMessage: "kind tracker_comment takes only kind and events",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewServiceConfig(tt.raw)

			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("NewServiceConfig(%s) error = %v, want *ConfigError", tt.name, err)
			}
			if ce.Field != tt.wantField {
				t.Errorf("ConfigError.Field = %q, want %q", ce.Field, tt.wantField)
			}
			if ce.Message != tt.wantMessage {
				t.Errorf("ConfigError.Message = %q, want %q", ce.Message, tt.wantMessage)
			}
		})
	}
}

func TestNotificationsConfig_EventsParsed(t *testing.T) {
	t.Parallel()

	catalog := domain.EventTypes()
	withoutAgentMessage := slices.DeleteFunc(slices.Clone(catalog), func(eventType domain.EventType) bool {
		return eventType == domain.EventAgentMessage
	})

	tests := []struct {
		name         string
		entry        map[string]any
		wantEvents   []domain.EventType
		wantDeclared bool
	}{
		{
			name:         "every catalog type on a registered kind, in written order",
			entry:        map[string]any{"kind": "webhook", "url": "https://example.com/hook", "events": catalogNames(catalog)},
			wantEvents:   catalog,
			wantDeclared: true,
		},
		{
			name:         "reversed order is kept",
			entry:        map[string]any{"kind": "webhook", "url": "https://example.com/hook", "events": []any{"budget.held", "session.started"}},
			wantEvents:   []domain.EventType{domain.EventBudgetHeld, domain.EventTypeSessionStarted},
			wantDeclared: true,
		},
		{
			name:         "every orchestrator type on tracker comment",
			entry:        map[string]any{"kind": domain.TrackerCommentKind, "events": catalogNames(withoutAgentMessage)},
			wantEvents:   withoutAgentMessage,
			wantDeclared: true,
		},
		{
			name:         "empty list on tracker comment",
			entry:        map[string]any{"kind": domain.TrackerCommentKind, "events": []any{}},
			wantEvents:   []domain.EventType{},
			wantDeclared: true,
		},
		{
			name:         "absent events on a registered kind",
			entry:        map[string]any{"kind": "slack", "webhook_url": "https://hooks.slack.com/T/B/SECRET"},
			wantEvents:   []domain.EventType{domain.EventAgentMessage},
			wantDeclared: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := NewServiceConfig(trackerCommentRaw(tt.entry))
			if err != nil {
				t.Fatalf("NewServiceConfig(%s): %v", tt.name, err)
			}
			if len(cfg.Notifications.Backends) != 1 {
				t.Fatalf("Notifications.Backends len = %d, want 1", len(cfg.Notifications.Backends))
			}

			got := cfg.Notifications.Backends[0]
			if !slices.Equal(got.Events, tt.wantEvents) {
				t.Errorf("Backends[0].Events = %v, want %v", got.Events, tt.wantEvents)
			}
			if got.EventsDeclared != tt.wantDeclared {
				t.Errorf("Backends[0].EventsDeclared = %v, want %v", got.EventsDeclared, tt.wantDeclared)
			}
			if _, ok := got.Config["events"]; ok {
				t.Errorf("Backends[0].Config[\"events\"] present, want it excluded from pass-through config")
			}
		})
	}
}

func TestNotificationsConfig_EventsKeepMaxPerSession(t *testing.T) {
	t.Parallel()

	cfg, err := NewServiceConfig(trackerCommentRaw(map[string]any{
		"kind":            "webhook",
		"url":             "https://example.com/hook",
		"max_per_session": 4,
		"events":          []any{"budget.held"},
	}))
	if err != nil {
		t.Fatalf("NewServiceConfig: %v", err)
	}

	got := cfg.Notifications.Backends[0]
	if got.MaxPerSession != 4 {
		t.Errorf("Backends[0].MaxPerSession = %d, want 4", got.MaxPerSession)
	}
	if !slices.Equal(got.Events, []domain.EventType{domain.EventBudgetHeld}) {
		t.Errorf("Backends[0].Events = %v, want [budget.held]", got.Events)
	}
}
