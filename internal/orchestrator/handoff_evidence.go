package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/workspace"
)

type handoffEvidenceVerdict string

const (
	handoffWorkObserved         handoffEvidenceVerdict = "work observed"
	handoffAbsenceObserved      handoffEvidenceVerdict = "absence of work observed"
	handoffEvidenceUndetermined handoffEvidenceVerdict = "evidence not determinable"
)

type handoffEvidenceResult struct {
	Verdict handoffEvidenceVerdict
	Reason  string
}

func evaluateHandoffEvidence(ctx context.Context, result WorkerResult, log *slog.Logger) handoffEvidenceResult {
	if result.WorkspacePath != "" {
		scm := workspace.ReadSCMMetadata(result.WorkspacePath, log)
		if scm.SHA != "" || scm.PRNumber > 0 {
			return handoffEvidenceResult{
				Verdict: handoffWorkObserved,
				Reason:  "workspace SCM metadata names a pushed commit or pull request",
			}
		}
	}

	if result.HandoffEvidenceBaseline == nil {
		reason := "run has no workspace baseline"
		if result.HandoffEvidenceBaselineError != nil {
			reason = result.HandoffEvidenceBaselineError.Error()
		} else if result.WorkspacePath == "" {
			reason = "run has no recorded workspace"
		}
		return handoffEvidenceResult{Verdict: handoffEvidenceUndetermined, Reason: reason}
	}

	change, err := workspace.CompareHandoffEvidenceBaseline(ctx, result.WorkspacePath, *result.HandoffEvidenceBaseline)
	if err != nil {
		return handoffEvidenceResult{
			Verdict: handoffEvidenceUndetermined,
			Reason:  fmt.Sprintf("workspace comparison failed: %v", err),
		}
	}
	if change.CommitMoved {
		return handoffEvidenceResult{Verdict: handoffWorkObserved, Reason: "workspace commit moved from the run baseline"}
	}
	if change.WorktreeChanged {
		return handoffEvidenceResult{Verdict: handoffWorkObserved, Reason: "working tree changed from the run baseline"}
	}
	return handoffEvidenceResult{
		Verdict: handoffAbsenceObserved,
		Reason:  "workspace commit and working tree match the run baseline",
	}
}

func handoffEvidenceWithholds(policy config.HandoffEvidencePolicy, result handoffEvidenceResult) bool {
	switch result.Verdict {
	case handoffAbsenceObserved:
		return true
	case handoffEvidenceUndetermined:
		return policy == config.HandoffEvidenceStrict
	default:
		return false
	}
}

func handoffEvidenceFailure(policy config.HandoffEvidencePolicy, result handoffEvidenceResult) error {
	return fmt.Errorf("handoff withheld: %s under %s policy", result.Verdict, policy)
}
