package agentcore

import (
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

const defaultReadTimeout = 30 * time.Second

// ReadTimeout returns cfg.ReadTimeoutMS, or 30 seconds when it is not positive.
func ReadTimeout(cfg domain.AgentConfig) time.Duration {
	if cfg.ReadTimeoutMS > 0 {
		return time.Duration(cfg.ReadTimeoutMS) * time.Millisecond
	}
	return defaultReadTimeout
}

// FirstResponseTimeout bounds a wait that can end only after the runtime
// has heard back from its model backend: the read timeout, never shorter
// than [CredentialExchangeBound]. Provider latency, not the runtime's own
// startup, decides how long such a wait takes, and the read timeout's
// default is far below what a healthy provider can take.
func FirstResponseTimeout(cfg domain.AgentConfig) time.Duration {
	return max(ReadTimeout(cfg), CredentialExchangeBound)
}
