package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
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
func PopulateRetries(state *State, entries []storage.PendingRetry, log *slog.Logger) {
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

		lineage := StageLineage{
			PreviousRule:    e.StagePrevious,
			PreviousOutcome: e.StagePreviousOutcome,
			ChainID:         e.ChainID,
			DispatchedAt:    parseDispatchedAt(e.DispatchedAt),
		}
		state.RetryAttempts[e.IssueID] = &RetryEntry{
			IssueID:    e.IssueID,
			Identifier: e.Identifier,
			DisplayID:  e.DisplayID,
			IssueIdentity: IssueIdentity{
				Title:     e.IssueTitle,
				URL:       e.IssueURL,
				IssueType: e.IssueType,
				Labels:    slices.Clone(e.IssueLabels),
				State:     e.IssueState,
			},
			SessionID:        sessionID,
			Attempt:          e.Attempt,
			DueAtMS:          e.DueAtMs,
			Error:            errStr,
			RuleName:         e.RuleName,
			StageLineage:     lineage,
			TemplateID:       e.TemplateID,
			AgentKind:        e.AgentKind,
			ConfiguredModel:  e.ConfiguredModel,
			Trigger:          e.Trigger,
			OriginTrigger:    e.OriginTrigger,
			scheduledDelayMS: pending.RemainingMs,

			UsageLimitNotified: e.UsageLimitNotified,
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

// PopulateUsageLimitHolds loads persisted usage-limit holds into the state.
// An expired hold loads and stays inert until a refusal replaces it or a
// run started after it clears it.
func PopulateUsageLimitHolds(state *State, holds []storage.UsageLimitHold) {
	for _, hold := range holds {
		key := UsageLimitHoldKey{AgentKind: hold.AgentKind, ModelOnly: hold.ModelOnly, Model: hold.Model}
		state.UsageLimitHolds[key] = UsageLimitHold{
			ResetsAt: time.UnixMilli(hold.ResetsAtMs).UTC(),
			Since:    time.UnixMilli(hold.HeldSinceMs).UTC(),
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

// PendingReactionRecoveryParams holds all inputs for [RecoverPendingReactions].
type PendingReactionRecoveryParams struct {
	WorkspaceRoot                     string
	TrackerAdapter                    domain.TrackerAdapter
	HandoffState                      string
	TerminalStates                    []string
	CIProvider                        domain.CIStatusProvider
	SCMAdapter                        domain.SCMAdapter
	AutoMergeReactionConfigured       bool
	ReviewReactionConfigured          bool
	BotReviewReactionConfigured       bool
	MergeConflictReactionConfigured   bool
	LabelReviewReactionConfigured     bool
	LabelFixReactionConfigured        bool
	MergeCompletionReactionConfigured bool
	RecoveryLookback                  time.Duration
	MaxCandidates                     int
	NowFunc                           func() time.Time
	Logger                            *slog.Logger
}

// PendingReactionRecoveryResult reports outcome counts from a single
// [RecoverPendingReactions] call.
type PendingReactionRecoveryResult struct {
	Candidates               int
	CapSkipped               int
	StateChecked             int
	ReviewRecovered          int
	CIRecovered              int
	AutoMergeRecovered       int
	BotReviewRecovered       int
	MergeConflictRecovered   int
	LabelReviewRecovered     int
	LabelFixRecovered        int
	MergeCompletionRecovered int
	StaleSkipped             int
	Skipped                  int
}

// RecoverPendingReactions reconstructs runtime pending reaction entries from
// persisted run history, tracker state, and workspace SCM metadata. It must be
// called before the orchestrator event loop starts.
//
// Recovery is best-effort: per-candidate invalid data is skipped, while a
// tracker state fetch failure aborts recovery for this startup and returns a
// zero result with the fetch error.
func RecoverPendingReactions(ctx context.Context, state *State, runs []storage.RunHistory, params PendingReactionRecoveryParams) (PendingReactionRecoveryResult, error) {
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

func filterReactionRecoveryRuns(state *State, runs []storage.RunHistory, outcome *PendingReactionRecoveryResult) []storage.RunHistory {
	eligibleRuns := make([]storage.RunHistory, 0, len(runs))
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

func validReactionRecoveryRuns(runs []storage.RunHistory, log *slog.Logger, outcome *PendingReactionRecoveryResult) ([]storage.RunHistory, []string) {
	validRuns := make([]storage.RunHistory, 0, len(runs))
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

// stageLineageOf returns the lineage frozen with the dispatch that wrote
// run.
func stageLineageOf(run storage.RunHistory) StageLineage {
	return StageLineage{
		PreviousRule:    run.StagePrevious,
		PreviousOutcome: run.StagePreviousOutcome,
		ChainID:         run.ChainID,
		DispatchedAt:    parseDispatchedAt(run.DispatchedAt),
	}
}

// issueIdentityOfRun returns the identity run recorded at dispatch: a title
// and link with nil Labels, because run history keeps no other fact.
func issueIdentityOfRun(run storage.RunHistory) IssueIdentity {
	return IssueIdentity{Title: run.IssueTitle, URL: run.IssueURL}
}

func recoverPendingReactionKinds(
	state *State,
	run storage.RunHistory,
	meta domain.SCMMetadata,
	now time.Time,
	params PendingReactionRecoveryParams,
	outcome *PendingReactionRecoveryResult,
) int {
	pr := pullRequestRef{Number: meta.PRNumber, Owner: meta.Owner, Repo: meta.Repo, Branch: meta.Branch, HeadSHA: meta.SHA}
	hasPullRequest := pr.Number > 0 && pr.Owner != "" && pr.Repo != ""
	hasCheckout := hasPullRequest && pr.Branch != ""
	added := 0

	recoverKind := func(kind string, counter *int) {
		key := ReactionKey(run.IssueID, kind)
		if _, exists := state.PendingReactions[key]; exists {
			return
		}
		state.PendingReactions[key] = recoveredPendingReaction(run, kind, pr, now)
		*counter++
		added++
	}

	if reactionKindConfigured(params, ReactionKindReview) && hasCheckout {
		recoverKind(ReactionKindReview, &outcome.ReviewRecovered)
	}
	if reactionKindConfigured(params, ReactionKindBotReview) && hasCheckout {
		recoverKind(ReactionKindBotReview, &outcome.BotReviewRecovered)
	}
	if reactionKindConfigured(params, ReactionKindCI) && hasCheckout {
		recoverKind(ReactionKindCI, &outcome.CIRecovered)
	}
	if reactionKindConfigured(params, ReactionKindAutoMerge) && hasCheckout {
		recoverKind(ReactionKindAutoMerge, &outcome.AutoMergeRecovered)
	}
	if reactionKindConfigured(params, ReactionKindMergeConflict) && hasCheckout {
		recoverKind(ReactionKindMergeConflict, &outcome.MergeConflictRecovered)
	}

	// The label-review reaction has no checkout and requires no branch, so
	// unlike the sibling kinds its guard omits the branch clause.
	if reactionKindConfigured(params, ReactionKindLabelReview) && hasPullRequest {
		recoverKind(ReactionKindLabelReview, &outcome.LabelReviewRecovered)
	}

	// The label-fix reaction checks out the PR head branch, so unlike
	// label-review its guard requires a branch, matching every other
	// checkout-bearing sibling kind.
	if reactionKindConfigured(params, ReactionKindLabelFix) && hasCheckout {
		recoverKind(ReactionKindLabelFix, &outcome.LabelFixRecovered)
	}

	// The merge-completion reaction has no checkout and requires no
	// branch, so unlike the checkout-bearing kinds its guard omits the
	// branch clause, matching label-review.
	if reactionKindConfigured(params, ReactionKindMergeCompletion) && hasPullRequest {
		recoverKind(ReactionKindMergeCompletion, &outcome.MergeCompletionRecovered)
	}

	return added
}

// reactionKindConfigured reports whether params carries what recovery needs
// to watch a pull request under the kind.
func reactionKindConfigured(params PendingReactionRecoveryParams, kind string) bool {
	switch kind {
	case ReactionKindCI:
		return params.CIProvider != nil && params.SCMAdapter != nil
	case ReactionKindReview:
		return params.SCMAdapter != nil && params.ReviewReactionConfigured
	case ReactionKindBotReview:
		return params.BotReviewReactionConfigured
	case ReactionKindAutoMerge:
		return params.SCMAdapter != nil && params.AutoMergeReactionConfigured
	case ReactionKindMergeConflict:
		return params.SCMAdapter != nil && params.MergeConflictReactionConfigured
	case ReactionKindLabelReview:
		return params.SCMAdapter != nil && params.LabelReviewReactionConfigured
	case ReactionKindLabelFix:
		return params.SCMAdapter != nil && params.LabelFixReactionConfigured
	case ReactionKindMergeCompletion:
		return params.SCMAdapter != nil && params.MergeCompletionReactionConfigured
	default:
		return false
	}
}

// recoveredPendingReaction builds the entry startup rebuilds for a pull
// request the run's workspace carried. HeadRecordedAt stays zero: a head
// recorded by a previous process cannot bound this process's attribution
// query, so the first pass after a restart records a baseline and
// classifies it unknown rather than resetting a counter against a boundary
// this process never observed.
func recoveredPendingReaction(run storage.RunHistory, kind string, pr pullRequestRef, now time.Time) *PendingReaction {
	return &PendingReaction{
		IssueID:    run.IssueID,
		Identifier: run.Identifier,
		DisplayID:  run.DisplayID,
		Attempt:    run.Attempt,
		Kind:       kind,
		CreatedAt:  now,
		KindData:   recoveredKindData(kind, pr),

		AgentKind:       run.AgentAdapter,
		RuleName:        run.RuleName,
		StageLineage:    stageLineageOf(run),
		ConfiguredModel: run.ConfiguredModel,
		TemplateID:      run.TemplateID,

		IssueIdentity: issueIdentityOfRun(run),
	}
}

// recoveredKindData returns the kind data of a rebuilt entry, or nil for a
// kind that rebuilds none.
func recoveredKindData(kind string, pr pullRequestRef) any {
	switch kind {
	case ReactionKindCI:
		return &CIReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch, SHA: pr.HeadSHA}
	case ReactionKindReview:
		return &ReviewReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch, SHA: pr.HeadSHA}
	case ReactionKindBotReview:
		return &BotReviewReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch, SHA: pr.HeadSHA}
	case ReactionKindAutoMerge:
		return &AutoMergeReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch, SHA: pr.HeadSHA}
	case ReactionKindMergeConflict:
		return &MergeConflictReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch, SHA: pr.HeadSHA}
	case ReactionKindLabelReview:
		return &LabelReviewReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo}
	case ReactionKindLabelFix:
		return &LabelFixReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo, Branch: pr.Branch}
	case ReactionKindMergeCompletion:
		return &MergeCompletionReactionData{PRNumber: pr.Number, Owner: pr.Owner, Repo: pr.Repo}
	default:
		return nil
	}
}

func freshReactionRecoveryActivity(run storage.RunHistory, meta domain.SCMMetadata, cutoff time.Time) bool {
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

func warnReactionRecoverySkip(log *slog.Logger, run storage.RunHistory, reason string, attrs ...slog.Attr) {
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
