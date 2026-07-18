package orchestrator

import (
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

// ValidateReactionConfigs checks the reaction configuration that the
// orchestrator would build at construction and returns a diagnostic for
// each active reaction whose configuration would fail. It constructs no
// adapter and makes no network call.
//
// Only the bot_review and auto_merge reactions are inspected, the two
// carrying the review-bot allowlist and the merge strategy. Each is
// validated with the same builder the orchestrator runs at construction,
// so the offline verdict cannot diverge from the construction verdict.
func ValidateReactionConfigs(cfg config.ServiceConfig) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	if rc, ok := cfg.Reactions["bot_review"]; ok && rc.Provider != "" {
		if _, err := BuildBotReviewReactionConfig(rc); err != nil {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    "reactions.bot_review",
				Message:  err.Error(),
			})
		}
	}

	if rc, ok := cfg.Reactions["auto_merge"]; ok && rc.Provider != "" {
		if _, err := BuildAutoMergeReactionConfig(rc); err != nil {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    "reactions.auto_merge",
				Message:  err.Error(),
			})
		}
	}

	return diags
}
