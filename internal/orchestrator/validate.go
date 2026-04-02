package orchestrator

import (
	"fmt"

	"github.com/sortie-ai/sortie/internal/config"
)

// ValidateConfigForPromotion checks whether a parsed config is safe to
// promote as the effective config for ongoing orchestration operations
// (reconciliation, retry scheduling, worker exit handling). Returns an
// error if the config would cause incorrect behavior in these paths.
//
// It is compatible with [workflow.WithValidateFunc] so that
// [workflow.Manager] can gate config promotion on domain-level safety
// rules without coupling to orchestration internals.
func ValidateConfigForPromotion(cfg config.ServiceConfig) error {
	if len(cfg.Tracker.ActiveStates) == 0 && len(cfg.Tracker.TerminalStates) == 0 {
		return fmt.Errorf("tracker.active_states and tracker.terminal_states are both empty; at least one must be configured")
	}
	return nil
}
