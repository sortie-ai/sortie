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
// The builder-validated reaction kinds are inspected with the same builders
// the orchestrator runs at construction, so the offline verdict cannot
// diverge from the construction verdict. trackerMeta MUST come from the
// tracker registry's Meta(cfg.Tracker.Kind) lookup.
func ValidateReactionConfigs(cfg config.ServiceConfig, trackerMeta registry.TrackerMeta) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	if rc, ok := cfg.Reactions["review_comments"]; ok && rc.Provider != "" {
		if _, err := BuildReviewReactionConfig(rc); err != nil {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    "reactions.review_comments",
				Message:  err.Error(),
			})
		}
	}

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

	if rc, ok := cfg.Reactions["merge_conflicts"]; ok && rc.Provider != "" {
		if _, err := BuildMergeConflictReactionConfig(rc); err != nil {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    "reactions.merge_conflicts",
				Message:  err.Error(),
			})
		}
	}

	if rc, ok := cfg.Reactions["merge_completion"]; ok && rc.Provider != "" {
		if _, err := BuildMergeCompletionReactionConfig(rc, cfg.Tracker, trackerMeta); err != nil {
			diags = append(diags, registry.ValidationDiag{
				Severity: "error",
				Check:    "reactions.merge_completion",
				Message:  err.Error(),
			})
		}
	}

	return diags
}
