package agentcore

import "github.com/sortie-ai/sortie/internal/domain"

// SettingsError returns the StartSession error for a settings block the
// adapter cannot use: message is the text the kind's validator reports,
// err the wrapped cause.
func SettingsError(message string, err error) *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrAgentNotFound,
		Message: message,
		Err:     err,
	}
}
