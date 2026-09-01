package config

import (
	"fmt"

	"github.com/sortie-ai/sortie/internal/typeutil"
)

// NotificationsConfig holds the parsed notifications list. The zero
// value (nil Backends) means no backend is configured and the
// notify_operator tool is not registered.
type NotificationsConfig struct {
	Backends []NotificationBackend
}

// NotificationBackend is one entry in the notifications list. Kind is
// the registry discriminator; Config carries the entry's remaining keys
// verbatim, with $VAR references resolved, for the backend constructor.
type NotificationBackend struct {
	// Kind is the required registry discriminator, resolved against the
	// notifier registry at sidecar startup.
	Kind string

	// MaxPerSession is the per-session notification cap. 0 selects the
	// default at cap selection time; it never means unlimited.
	MaxPerSession int

	// Config holds the entry's per-backend fields with $VAR references
	// resolved. It is passed through to the backend constructor untyped.
	Config map[string]any
}

// buildNotificationsConfig parses the top-level notifications sequence
// into a [NotificationsConfig]. An absent section yields the zero value
// and a nil error. The value must be a YAML sequence; each entry must
// be a map carrying a non-empty string kind, and an optional
// max_per_session must be a non-negative integer. Every other key is
// copied into the entry's Config for pass-through, with $VAR and ${VAR}
// references resolved on every string leaf. Returns the first
// [*ConfigError] on a malformed section.
func buildNotificationsConfig(raw map[string]any) (NotificationsConfig, error) {
	value, ok := raw["notifications"]
	if !ok || value == nil {
		return NotificationsConfig{}, nil
	}

	seq, ok := value.([]any)
	if !ok {
		return NotificationsConfig{}, &ConfigError{
			Field:   "notifications",
			Message: fmt.Sprintf("expected sequence, got %T", value),
		}
	}

	backends := make([]NotificationBackend, 0, len(seq))
	for i, elem := range seq {
		backend, err := parseNotificationBackend(i, elem)
		if err != nil {
			return NotificationsConfig{}, err
		}
		backends = append(backends, backend)
	}

	if len(backends) == 0 {
		return NotificationsConfig{}, nil
	}
	return NotificationsConfig{Backends: backends}, nil
}

// parseNotificationBackend converts a single notifications entry into a
// [NotificationBackend], resolving $VAR references on its pass-through
// leaves.
func parseNotificationBackend(index int, elem any) (NotificationBackend, error) {
	field := fmt.Sprintf("notifications[%d]", index)

	entry, ok := elem.(map[string]any)
	if !ok {
		return NotificationBackend{}, &ConfigError{
			Field:   field,
			Message: fmt.Sprintf("expected map, got %T", elem),
		}
	}

	kind, fault := typeutil.StringField(entry, "kind")
	if fault != nil {
		return NotificationBackend{}, &ConfigError{
			Field:   field + ".kind",
			Message: fault.Reason(),
		}
	}
	if kind == "" {
		return NotificationBackend{}, &ConfigError{
			Field:   field + ".kind",
			Message: "kind is required and must be a non-empty string",
		}
	}

	maxPerSession := 0
	if rawMax, exists := entry["max_per_session"]; exists && rawMax != nil {
		n, err := coerceInt(rawMax)
		if err != nil {
			return NotificationBackend{}, &ConfigError{
				Field:   field + ".max_per_session",
				Message: fmt.Sprintf("invalid integer value: %v", rawMax),
			}
		}
		if n < 0 {
			return NotificationBackend{}, &ConfigError{
				Field:   field + ".max_per_session",
				Message: "must not be negative",
			}
		}
		maxPerSession = n
	}

	passthrough := make(map[string]any, len(entry))
	for key, val := range entry {
		if key == "kind" || key == "max_per_session" {
			continue
		}
		passthrough[key] = val
	}

	// A typed knownTopLevelKeys section is excluded from extensions, and
	// the only existing $VAR walker runs over extensions, so resolve the
	// entry leaves here. An unset reference resolves to the empty string,
	// which the backend constructor then rejects as a missing secret.
	var snapshot map[string]string
	for key, val := range passthrough {
		passthrough[key] = resolveExtensionEnvValue(key, val, &snapshot)
	}

	return NotificationBackend{
		Kind:          kind,
		MaxPerSession: maxPerSession,
		Config:        passthrough,
	}, nil
}
