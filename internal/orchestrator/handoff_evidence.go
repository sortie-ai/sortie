package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// handoffEvidenceTimeout bounds a single workspace evidence inspection.
//
// The inspection runs Git subprocesses on the orchestrator's event loop, and
// the context it inherits carries no deadline of its own: it is cancelled at
// shutdown, and during drain it is never cancelled at all. An unresponsive
// repository would therefore stall dispatch, agent-event processing, and
// reconciliation for as long as the process lives. The bound is generous
// because exceeding it is reported as a failed inspection, which yields the
// undeterminable verdict and withholds the handoff under the strict policy.
//
// handoffEvidenceTimeout is a package variable, not a constant, so a test can
// shorten it and assert that a stalled inspection still returns.
var handoffEvidenceTimeout = 30 * time.Second

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
	if result.SoftStop && result.SoftStopReason == string(workspace.StatusNoChangeNeeded) {
		return handoffEvidenceResult{
			Verdict: handoffWorkObserved,
			Reason:  "agent declared no change was needed",
		}
	}

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

	compareCtx, cancel := context.WithTimeout(ctx, handoffEvidenceTimeout)
	defer cancel()

	change, err := workspace.CompareHandoffEvidenceBaseline(compareCtx, result.WorkspacePath, *result.HandoffEvidenceBaseline)
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
	return fmt.Errorf("%s%s under %s policy", persistence.HandoffAbsenceErrorPrefix, result.Verdict, policy)
}
