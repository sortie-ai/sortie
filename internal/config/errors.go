package config

import "fmt"

// ConfigError represents a configuration validation failure for a
// specific field. It satisfies the error interface and can be extracted
// from the error returned by [NewServiceConfig] via [errors.As].
type ConfigError struct {
	// Field is the dotted path to the offending configuration key
	// (e.g. "polling.interval_ms", "agent.max_concurrent_agents").
	Field string

	// Message describes the validation failure in operator-friendly terms.
	Message string
}

// Error returns a human-readable diagnostic including the field path.
func (e *ConfigError) Error() string {
	return fmt.Sprintf("config: %s: %s", e.Field, e.Message)
}
