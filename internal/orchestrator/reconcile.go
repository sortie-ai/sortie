package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// ReconcileStore is the persistence interface required by
// [ReconcileRunningIssues]. Satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type ReconcileStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	DeleteReactionFingerprintsByIssue(ctx context.Context, issueID string) error
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
}

// ReconcileParams holds the dependencies for [ReconcileRunningIssues] that
// are not part of the core [State]. This separates pure state mutation from
// I/O side effects (tracker API, SQLite persistence).
type ReconcileParams struct {
	// TrackerAdapter fetches current issue states for running issues.
	TrackerAdapter domain.TrackerAdapter

	// ActiveStates is the current list of configured active issue states.
	ActiveStates []string

	// TerminalStates is the current list of configured terminal issue states.
	TerminalStates []string

	// StallTimeoutMS is the configured stall detection threshold.
	// If <= 0, stall detection is skipped entirely.
	StallTimeoutMS int

	// MaxRetryBackoffMS is the configured cap for exponential backoff
	// delay (from config.Agent.MaxRetryBackoffMS). Used when scheduling
	// stall-detection retries.
	MaxRetryBackoffMS int

	// Store is the SQLite persistence layer for retry entry operations.
	Store ReconcileStore

	// OnRetryFire is the callback invoked when a stall-detection retry
	// timer expires. Routes back into the event loop.
	OnRetryFire func(issueID string)

	// NowFunc returns the current UTC time. Injected for testability.
	// If nil, time.Now().UTC() is used.
	NowFunc func() time.Time

	// Ctx is the context for tracker API calls and persistence operations.
	Ctx context.Context

	// Logger is the structured logger with orchestrator context.
	Logger *slog.Logger

	// Metrics records instrumentation counters for reconciliation events.
	// If nil, defaults to [domain.NoopMetrics].
	Metrics domain.Metrics

	// CIProvider is the CI status provider. When non-nil, the reconcile
	// loop polls CI status for issues in state.PendingReactions.
	CIProvider domain.CIStatusProvider

	// CIFeedback holds CI feedback tuning (max retries, escalation mode).
	// Only read when CIProvider is non-nil.
	CIFeedback config.CIFeedbackConfig

	// CIPendingTTL is the maximum age of a PendingReaction entry before
	// it is dropped and a warning is logged. Protects against indefinite
	// spinning when the CI provider is unreachable and no new worker exit
	// refreshes the entry. Zero or negative disables TTL enforcement
	// entirely. Production callers should set this to a positive value
	// (e.g. [ciPendingDefaultTTL]); test helpers that do not set NowFunc
	// may leave it zero to preserve legacy behavior.
	CIPendingTTL time.Duration

	// SCMAdapter provides review comment fetching. When non-nil, the
	// reconcile loop polls review comments for issues with PR metadata.
	SCMAdapter domain.SCMAdapter

	// ReviewConfig holds review reaction configuration. Only read when
	// SCMAdapter is non-nil.
	ReviewConfig ReviewReactionConfig

	// ReviewPendingTTL is the maximum age of a review PendingReaction
	// entry before it is dropped. Zero disables TTL enforcement.
	ReviewPendingTTL time.Duration
}

// ReconcileRunningIssues detects stalled workers and refreshes tracker
// state for all running issues. Intended to be called from the poll tick
// before dispatch; wiring into the event loop is done by the caller.
//
// Stall detection cancels workers that have exceeded the configured stall
// timeout and schedules exponential-backoff retries. Tracker state refresh
// queries the tracker for current issue states: terminal issues are marked
// for workspace cleanup, active issues get their in-memory snapshot updated,
// and non-active/non-terminal issues are cancelled without cleanup.
//
// Running entries are never removed by reconciliation. Cancelled workers
// exit asynchronously and are processed by [HandleWorkerExit].
func ReconcileRunningIssues(state *State, params ReconcileParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	// Cancel stalled workers and schedule exponential-backoff retries.
	reconcileStalled(state, params, log, ctx, now, metrics)

	// Refresh issue states from the tracker and stop workers for
	// terminal or non-active issues.
	reconcileTrackerState(state, params, log, ctx, metrics)

	// Poll CI status for issues with pending CI checks.
	reconcileCIStatus(state, params, log, ctx, metrics)

	// Poll review comments for issues with pending review reactions.
	reconcileReviewComments(state, params, log, ctx, metrics)
}

// reconcileStalled cancels running entries whose last activity exceeds the
// stall timeout and schedules an exponential-backoff retry for each.
func reconcileStalled(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, now time.Time, metrics domain.Metrics) {
	if params.StallTimeoutMS <= 0 {
		return
	}

	stallThreshold := int64(params.StallTimeoutMS)

	for issueID, entry := range state.Running {
		entryLog := logging.WithIssue(log, issueID, entry.Identifier)

		refTime := entry.StartedAt
		if !entry.LastAgentTimestamp.IsZero() {
			refTime = entry.LastAgentTimestamp
		}

		elapsedMS := now.Sub(refTime).Milliseconds()
		if elapsedMS <= stallThreshold {
			continue
		}

		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}

		nextAttempt := NextAttempt(entry.RetryAttempt)

		// Skip scheduling when a retry is already present at the same or
		// higher attempt. Without this guard, every reconciliation tick
		// would replace the existing timer, pushing DueAtMS forward and
		// preventing the retry from ever firing.
		if existing, ok := state.RetryAttempts[issueID]; ok && existing.Attempt >= nextAttempt {
			entryLog.Debug("stall retry already scheduled, skipping reschedule",
				slog.Int("current_attempt", existing.Attempt),
				slog.Int("next_attempt", nextAttempt),
			)
			continue
		}

		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: entry.Identifier,
			DisplayID:  entry.Issue.DisplayID,
			Attempt:    nextAttempt,
			DelayMS:    delayMS,
			Error:      "stall timeout exceeded",
		}, params.OnRetryFire)
		metrics.IncRetries(triggerStall)

		if retryEntry, ok := state.RetryAttempts[issueID]; ok {
			pEntry := persistence.RetryEntry{
				IssueID:    retryEntry.IssueID,
				Identifier: retryEntry.Identifier,
				Attempt:    retryEntry.Attempt,
				DueAtMs:    retryEntry.DueAtMS,
				Error:      stringPtr(retryEntry.Error),
			}
			if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
				entryLog.Error("failed to persist stall retry entry",
					slog.Any("error", err),
				)
			}
		}

		entryLog.Warn("stall detected, cancelling worker",
			slog.Int64("elapsed_ms", elapsedMS),
			slog.Int("stall_timeout_ms", params.StallTimeoutMS),
		)
	}
}

// reconcileTrackerState fetches current issue states for all running IDs
// and cancels workers whose issues are terminal or no longer active.
func reconcileTrackerState(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if len(state.Running) == 0 {
		return
	}

	runningIDs := make([]string, 0, len(state.Running))
	for id := range state.Running {
		runningIDs = append(runningIDs, id)
	}

	refreshed, err := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		log.Warn("tracker state refresh failed, keeping workers running",
			slog.Any("error", err),
		)
		return
	}

	activeSet := stateSet(params.ActiveStates)
	terminalSet := stateSet(params.TerminalStates)

	for issueID, stateName := range refreshed {
		entry, ok := state.Running[issueID]
		if !ok {
			continue
		}

		entryLog := logging.WithIssue(log, issueID, entry.Identifier)

		normalized := strings.ToLower(stateName)

		if _, terminal := terminalSet[normalized]; terminal {
			if entry.PendingCleanup {
				continue
			}
			if entry.CancelFunc != nil {
				entry.CancelFunc()
			}
			CancelRetry(state, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				entryLog.Error("failed to delete retry entry for terminal issue",
					slog.Any("error", err),
				)
			}
			entry.PendingCleanup = true
			metrics.IncReconciliationActions(actionCleanup)
			entryLog.Info("stopping worker for terminal issue",
				slog.String("state", stateName),
			)
			continue
		}

		if _, active := activeSet[normalized]; active {
			entry.Issue.State = stateName
			metrics.IncReconciliationActions(actionKeep)
			entryLog.Debug("refreshed issue state",
				slog.String("state", stateName),
			)
			continue
		}

		// Non-active, non-terminal: cancel without workspace cleanup.
		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}
		CancelRetry(state, issueID)
		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			entryLog.Error("failed to delete retry entry for non-active issue",
				slog.Any("error", err),
			)
		}
		metrics.IncReconciliationActions(actionStop)
		entryLog.Info("stopping worker for non-active issue",
			slog.String("state", stateName),
		)
	}
}

// sweepEveryNTicks is the number of poll ticks between terminal workspace
// sweeps. At the default 10s poll interval this fires roughly every 10
// minutes — frequent enough for eventual consistency, infrequent enough to
// avoid unbounded tracker API load from orphaned non-terminal workspaces.
const sweepEveryNTicks = 60

// SweepTerminalWorkspacesParams holds the dependencies for
// [SweepTerminalWorkspaces]. All fields except Logger and Metrics are
// required; nil Logger defaults to [slog.Default] and nil Metrics defaults
// to [domain.NoopMetrics].
type SweepTerminalWorkspacesParams struct {
	WorkspaceRoot    string
	TrackerAdapter   domain.TrackerAdapter
	TerminalStates   []string
	BeforeRemoveHook string
	HookTimeoutMS    int
	Ctx              context.Context
	Logger           *slog.Logger
	Metrics          domain.Metrics
}

// SweepTerminalWorkspaces removes workspace directories for issues that
// transitioned to a terminal state after their worker exited. It lists
// workspace keys on disk, excludes any that belong to in-flight entries,
// queries the tracker for the remaining identifiers, and cleans up those
// whose state is terminal.
func SweepTerminalWorkspaces(state *State, params SweepTerminalWorkspacesParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	keys, err := workspace.ListWorkspaceKeys(params.WorkspaceRoot)
	if err != nil {
		log.Warn("sweep: failed to list workspace keys",
			slog.Any("error", err),
		)
		return
	}
	if len(keys) == 0 {
		return
	}

	inFlightKeys := make(map[string]struct{})
	for _, entry := range state.Running {
		k, sErr := workspace.SanitizeKey(entry.Identifier)
		if sErr != nil {
			log.Warn("sweep: failed to sanitize running identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", sErr),
			)
			continue
		}
		inFlightKeys[k] = struct{}{}
	}
	for _, entry := range state.RetryAttempts {
		k, sErr := workspace.SanitizeKey(entry.Identifier)
		if sErr != nil {
			log.Warn("sweep: failed to sanitize retry identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", sErr),
			)
			continue
		}
		inFlightKeys[k] = struct{}{}
	}
	for _, entry := range state.PendingReactions {
		k, sErr := workspace.SanitizeKey(entry.Identifier)
		if sErr != nil {
			log.Warn("sweep: failed to sanitize pending reaction identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", sErr),
			)
			continue
		}
		inFlightKeys[k] = struct{}{}
	}

	unclaimedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := inFlightKeys[key]; !ok {
			unclaimedKeys = append(unclaimedKeys, key)
		}
	}
	if len(unclaimedKeys) == 0 {
		return
	}

	statesByKey, err := params.TrackerAdapter.FetchIssueStatesByIdentifiers(params.Ctx, unclaimedKeys)
	if err != nil {
		log.Warn("sweep: failed to fetch issue states",
			slog.Any("error", err),
		)
		return
	}

	terminalSet := make(map[string]struct{}, len(params.TerminalStates))
	for _, s := range params.TerminalStates {
		terminalSet[strings.ToLower(s)] = struct{}{}
	}

	var toClean []string
	for _, key := range unclaimedKeys {
		stateName, ok := statesByKey[key]
		if !ok {
			continue
		}
		if _, terminal := terminalSet[strings.ToLower(stateName)]; terminal {
			toClean = append(toClean, key)
		}
	}
	if len(toClean) == 0 {
		return
	}

	log.Info("sweep: cleaning terminal workspaces",
		slog.Int("count", len(toClean)),
	)

	result := workspace.CleanupTerminal(params.Ctx, workspace.CleanupTerminalParams{
		Root:          params.WorkspaceRoot,
		Identifiers:   toClean,
		BeforeRemove:  params.BeforeRemoveHook,
		HookTimeoutMS: params.HookTimeoutMS,
		Logger:        log,
	})

	for range result.Removed {
		metrics.IncReconciliationActions(actionSweepCleanup)
	}
}
