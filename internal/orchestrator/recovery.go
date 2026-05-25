package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// PopulateRetries loads persisted retry entries into state maps without
// starting timers. Each entry is stored in [State.RetryAttempts] with
// TimerHandle == nil and its computed remaining delay preserved in
// scheduledDelayMS for later activation. The issue is marked as Claimed
// to prevent double-dispatch on the first poll tick.
//
// Legacy rows persisted before dispatch rule routing was added carry
// an empty AgentKind. The orchestrator treats the empty value as the
// workflow-wide default during retry dispatch; a one-shot audit log
// records each legacy row so operators can correlate the routing
// decision with run history.
//
// Must be called before [NewOrchestrator] so the retry timer channel
// buffer can be sized to accommodate immediate-fire entries.
func PopulateRetries(state *State, entries []persistence.PendingRetry, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	for _, pending := range entries {
		e := pending.Entry

		var errStr string
		if e.Error != nil {
			errStr = *e.Error
		}

		var sessionID string
		if e.SessionID != nil {
			sessionID = *e.SessionID
		}

		state.RetryAttempts[e.IssueID] = &RetryEntry{
			IssueID:          e.IssueID,
			Identifier:       e.Identifier,
			SessionID:        sessionID,
			Attempt:          e.Attempt,
			DueAtMS:          e.DueAtMs,
			Error:            errStr,
			RuleName:         e.RuleName,
			TemplateID:       e.TemplateID,
			AgentKind:        e.AgentKind,
			scheduledDelayMS: pending.RemainingMs,
		}
		state.Claimed[e.IssueID] = struct{}{}

		if e.AgentKind == "" {
			log.Info("rehydrated legacy retry entry without agent kind",
				slog.String("recovery", "legacy_retry_default_agent"),
				slog.String("issue_id", e.IssueID),
			)
		}
	}
}

// PendingReactionRecoveryLookback is the maximum age of a run_history row or
// SCM activity timestamp considered eligible for pending reaction recovery on
// startup.
const PendingReactionRecoveryLookback = 30 * 24 * time.Hour

// PendingReactionRecoveryMaxCandidates is the maximum number of unique issues
// considered for pending reaction recovery in a single startup pass.
const PendingReactionRecoveryMaxCandidates = 200

// ReactionRecoveryRunStore is the persistence contract consumed by startup
// wiring and orchestrator tests.
type ReactionRecoveryRunStore interface {
	LoadLatestSuccessfulRunsForReactionRecovery(ctx context.Context, completedAfter time.Time, limit int) ([]persistence.RunHistory, error)
}

// PendingReactionRecoveryParams holds all inputs for [RecoverPendingReactions].
type PendingReactionRecoveryParams struct {
	WorkspaceRoot               string
	TrackerAdapter              domain.TrackerAdapter
	HandoffState                string
	TerminalStates              []string
	CIProvider                  domain.CIStatusProvider
	SCMAdapter                  domain.SCMAdapter
	AutoMergeReactionConfigured bool
	RecoveryLookback            time.Duration
	MaxCandidates               int
	NowFunc                     func() time.Time
	Logger                      *slog.Logger
}

// PendingReactionRecoveryResult reports outcome counts from a single
// [RecoverPendingReactions] call.
type PendingReactionRecoveryResult struct {
	Candidates         int
	CapSkipped         int
	StateChecked       int
	ReviewRecovered    int
	CIRecovered        int
	AutoMergeRecovered int
	StaleSkipped       int
	Skipped            int
}

// RecoverPendingReactions reconstructs runtime pending reaction entries from
// persisted run history, tracker state, and workspace SCM metadata. It must be
// called before the orchestrator event loop starts.
//
// Recovery is best-effort: per-candidate invalid data is skipped, while a
// tracker state fetch failure aborts recovery for this startup and returns a
// zero result with the fetch error.
func RecoverPendingReactions(ctx context.Context, state *State, runs []persistence.RunHistory, params PendingReactionRecoveryParams) (PendingReactionRecoveryResult, error) {
	if params.HandoffState == "" || (params.SCMAdapter == nil && params.CIProvider == nil && !params.AutoMergeReactionConfigured) {
		return PendingReactionRecoveryResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return PendingReactionRecoveryResult{}, err
	}

	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}
	lookback := params.RecoveryLookback
	if lookback <= 0 {
		lookback = PendingReactionRecoveryLookback
	}
	if lookback > PendingReactionRecoveryLookback {
		lookback = PendingReactionRecoveryLookback
	}
	cutoff := now.Add(-lookback)

	maxCandidates := params.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = PendingReactionRecoveryMaxCandidates
	}
	if maxCandidates > PendingReactionRecoveryMaxCandidates {
		maxCandidates = PendingReactionRecoveryMaxCandidates
	}

	outcome := PendingReactionRecoveryResult{Candidates: len(runs)}
	eligibleRuns := filterReactionRecoveryRuns(state, runs, &outcome)
	if len(eligibleRuns) > maxCandidates {
		outcome.CapSkipped = len(eligibleRuns) - maxCandidates
		eligibleRuns = eligibleRuns[:maxCandidates]
	}

	validRuns, issueIDs := validReactionRecoveryRuns(eligibleRuns, log, &outcome)
	if len(validRuns) == 0 {
		return outcome, nil
	}

	statesByID, err := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		return PendingReactionRecoveryResult{}, fmt.Errorf("fetch pending reaction recovery issue states: %w", err)
	}
	outcome.StateChecked = len(issueIDs)

	terminalSet := stateSet(params.TerminalStates)
	if state.PendingReactions == nil {
		state.PendingReactions = make(map[string]*PendingReaction)
	}

	for _, run := range validRuns {
		if err := ctx.Err(); err != nil {
			return PendingReactionRecoveryResult{}, err
		}

		stateName, ok := statesByID[run.IssueID]
		if !ok {
			outcome.Skipped++
			continue
		}
		if _, terminal := terminalSet[strings.ToLower(stateName)]; terminal {
			outcome.Skipped++
			continue
		}
		if !strings.EqualFold(stateName, params.HandoffState) {
			outcome.Skipped++
			continue
		}

		pathResult, err := workspace.ComputePath(params.WorkspaceRoot, run.Identifier)
		if err != nil {
			warnReactionRecoverySkip(log, run, "workspace path", slog.Any("error", err))
			outcome.Skipped++
			continue
		}

		meta := workspace.ReadSCMMetadata(pathResult.Path, log)
		if meta == (domain.SCMMetadata{}) {
			outcome.Skipped++
			continue
		}
		if !freshReactionRecoveryActivity(run, meta, cutoff) {
			outcome.StaleSkipped++
			continue
		}

		if recoverPendingReactionKinds(state, run, meta, now, params, &outcome) == 0 {
			outcome.Skipped++
		}
	}

	return outcome, nil
}

func filterReactionRecoveryRuns(state *State, runs []persistence.RunHistory, outcome *PendingReactionRecoveryResult) []persistence.RunHistory {
	eligibleRuns := make([]persistence.RunHistory, 0, len(runs))
	for _, run := range runs {
		if _, exists := state.Running[run.IssueID]; exists {
			outcome.Skipped++
			continue
		}
		if _, exists := state.RetryAttempts[run.IssueID]; exists {
			outcome.Skipped++
			continue
		}
		if _, exists := state.Claimed[run.IssueID]; exists {
			outcome.Skipped++
			continue
		}
		eligibleRuns = append(eligibleRuns, run)
	}
	return eligibleRuns
}

func validReactionRecoveryRuns(runs []persistence.RunHistory, log *slog.Logger, outcome *PendingReactionRecoveryResult) ([]persistence.RunHistory, []string) {
	validRuns := make([]persistence.RunHistory, 0, len(runs))
	issueIDs := make([]string, 0, len(runs))
	seen := make(map[string]struct{}, len(runs))

	for _, run := range runs {
		switch {
		case run.IssueID == "":
			warnReactionRecoverySkip(log, run, "missing issue id")
			outcome.Skipped++
			continue
		case run.Identifier == "":
			warnReactionRecoverySkip(log, run, "missing identifier")
			outcome.Skipped++
			continue
		case run.Workspace == "":
			warnReactionRecoverySkip(log, run, "missing workspace")
			outcome.Skipped++
			continue
		case run.Attempt <= 0:
			warnReactionRecoverySkip(log, run, "invalid attempt", slog.Int("attempt", run.Attempt))
			outcome.Skipped++
			continue
		}

		validRuns = append(validRuns, run)
		if _, exists := seen[run.IssueID]; exists {
			continue
		}
		seen[run.IssueID] = struct{}{}
		issueIDs = append(issueIDs, run.IssueID)
	}

	return validRuns, issueIDs
}

func recoverPendingReactionKinds(
	state *State,
	run persistence.RunHistory,
	meta domain.SCMMetadata,
	now time.Time,
	params PendingReactionRecoveryParams,
	outcome *PendingReactionRecoveryResult,
) int {
	added := 0

	if params.SCMAdapter != nil && meta.PRNumber > 0 && meta.Owner != "" && meta.Repo != "" && meta.Branch != "" {
		key := ReactionKey(run.IssueID, ReactionKindReview)
		if _, exists := state.PendingReactions[key]; !exists {
			state.PendingReactions[key] = &PendingReaction{
				IssueID:    run.IssueID,
				Identifier: run.Identifier,
				DisplayID:  run.DisplayID,
				Attempt:    run.Attempt,
				Kind:       ReactionKindReview,
				CreatedAt:  now,
				KindData: &ReviewReactionData{
					PRNumber: meta.PRNumber,
					Owner:    meta.Owner,
					Repo:     meta.Repo,
					Branch:   meta.Branch,
					SHA:      meta.SHA,
				},
				AgentKind:  run.AgentAdapter,
				RuleName:   run.RuleName,
				TemplateID: run.TemplateID,
			}
			outcome.ReviewRecovered++
			added++
		}
	}

	if params.CIProvider != nil && meta.Branch != "" {
		key := ReactionKey(run.IssueID, ReactionKindCI)
		if _, exists := state.PendingReactions[key]; !exists {
			state.PendingReactions[key] = &PendingReaction{
				IssueID:    run.IssueID,
				Identifier: run.Identifier,
				DisplayID:  run.DisplayID,
				Attempt:    run.Attempt,
				Kind:       ReactionKindCI,
				CreatedAt:  now,
				KindData: &CIReactionData{
					Branch: meta.Branch,
					SHA:    meta.SHA,
				},
				AgentKind:  run.AgentAdapter,
				RuleName:   run.RuleName,
				TemplateID: run.TemplateID,
			}
			outcome.CIRecovered++
			added++
		}
	}

	if params.SCMAdapter != nil && params.AutoMergeReactionConfigured && meta.PRNumber > 0 && meta.Owner != "" && meta.Repo != "" && meta.Branch != "" {
		key := ReactionKey(run.IssueID, ReactionKindAutoMerge)
		if _, exists := state.PendingReactions[key]; !exists {
			state.PendingReactions[key] = &PendingReaction{
				IssueID:    run.IssueID,
				Identifier: run.Identifier,
				DisplayID:  run.DisplayID,
				Attempt:    run.Attempt,
				Kind:       ReactionKindAutoMerge,
				CreatedAt:  now,
				KindData: &AutoMergeReactionData{
					PRNumber: meta.PRNumber,
					Owner:    meta.Owner,
					Repo:     meta.Repo,
					Branch:   meta.Branch,
					SHA:      meta.SHA,
				},
				AgentKind:  run.AgentAdapter,
				RuleName:   run.RuleName,
				TemplateID: run.TemplateID,
			}
			outcome.AutoMergeRecovered++
			added++
		}
	}

	return added
}

func freshReactionRecoveryActivity(run persistence.RunHistory, meta domain.SCMMetadata, cutoff time.Time) bool {
	activity := meta.PushedAt
	if activity == "" {
		activity = run.CompletedAt
	}
	parsed, err := time.Parse(time.RFC3339, activity)
	if err != nil {
		return false
	}
	return !parsed.UTC().Before(cutoff)
}

func warnReactionRecoverySkip(log *slog.Logger, run persistence.RunHistory, reason string, attrs ...slog.Attr) {
	entryLog := logging.WithIssue(log, run.IssueID, run.Identifier)
	logAttrs := []any{
		slog.Int64("run_history_id", run.ID),
		slog.String("reason", reason),
	}
	for _, attr := range attrs {
		logAttrs = append(logAttrs, attr)
	}
	entryLog.Warn("skipped pending reaction recovery candidate", logAttrs...)
}
