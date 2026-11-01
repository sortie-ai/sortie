package orchestrator

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
)

// identifiesPass reports whether a rule and chain together identify a pass.
func identifiesPass(ruleName, chainID string) bool {
	return ruleName != "" && chainID != ""
}

// recordRunningPass stores the running stage session of entry. A reaction
// session and an entry without a rule or chain identifier store nothing. log
// must already carry issue_id and issue_identifier.
func recordRunningPass(ctx context.Context, store storage.LivePassStore, issueID string, entry *RunningEntry, log *slog.Logger) {
	if entry.ReactionKind != "" || !identifiesPass(entry.RuleName, entry.StageLineage.ChainID) {
		return
	}
	upsertLivePass(ctx, store, storage.LivePass{
		IssueID:       issueID,
		Kind:          storage.LivePassRunning,
		ChainID:       entry.StageLineage.ChainID,
		RuleName:      entry.RuleName,
		StagePrevious: entry.StageLineage.PreviousRule,
	}, log)
}

// recordBudgetPass stores the budget hold of entry. An entry without a rule or
// chain identifier stores nothing. log must already carry issue_id and
// issue_identifier.
func recordBudgetPass(ctx context.Context, store storage.LivePassStore, issueID string, entry *BudgetExhaustedEntry, log *slog.Logger) {
	if !identifiesPass(entry.RuleName, entry.StageLineage.ChainID) {
		return
	}
	upsertLivePass(ctx, store, storage.LivePass{
		IssueID:       issueID,
		Kind:          storage.LivePassBudget,
		ChainID:       entry.StageLineage.ChainID,
		RuleName:      entry.RuleName,
		StagePrevious: entry.StageLineage.PreviousRule,
	}, log)
}

// syncBudgetPasses brings the stored budget holds from prior to fresh: it
// stores a hold that starts or changes its rule, chain, or previous rule and
// deletes the record of a hold that ends or loses its identity.
func syncBudgetPasses(ctx context.Context, store storage.LivePassStore, prior, fresh map[string]*BudgetExhaustedEntry, log *slog.Logger) {
	for id, entry := range fresh {
		issueLog := logging.WithIssue(log, id, entry.Identifier)
		held := prior[id]
		switch {
		case !identifiesPass(entry.RuleName, entry.StageLineage.ChainID):
			if held != nil && identifiesPass(held.RuleName, held.StageLineage.ChainID) {
				deleteLivePass(ctx, store, id, storage.LivePassBudget, issueLog)
			}
		case held == nil ||
			held.RuleName != entry.RuleName ||
			held.StageLineage.ChainID != entry.StageLineage.ChainID ||
			held.StageLineage.PreviousRule != entry.StageLineage.PreviousRule:
			recordBudgetPass(ctx, store, id, entry, issueLog)
		}
	}
	for id, held := range prior {
		if _, stillHeld := fresh[id]; stillHeld {
			continue
		}
		deleteLivePass(ctx, store, id, storage.LivePassBudget, logging.WithIssue(log, id, held.Identifier))
	}
}

func upsertLivePass(ctx context.Context, store storage.LivePassStore, pass storage.LivePass, log *slog.Logger) {
	if err := store.UpsertLivePass(ctx, pass); err != nil {
		log.Error("failed to persist live pass",
			slog.String("kind", pass.Kind),
			slog.Any("error", err),
		)
	}
}

// deleteLivePass removes the live pass row of the issue and kind. log must
// already carry issue_id and issue_identifier.
func deleteLivePass(ctx context.Context, store storage.LivePassStore, issueID, kind string, log *slog.Logger) {
	if err := store.DeleteLivePass(ctx, issueID, kind); err != nil {
		log.Error("failed to delete live pass",
			slog.String("kind", kind),
			slog.Any("error", err),
		)
	}
}
