// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
)

// WorkflowManager provides access to the current workflow config and
// prompt template. Satisfied by [workflow.Manager] in production.
type WorkflowManager interface {
	Config() config.ServiceConfig
	PromptTemplate() *prompt.Template
	PromptTemplateByID(id string) *prompt.Template
	Reload() error
	WorkflowAbsPath() string
}

// OrchestratorStore is the persistence interface required by the
// orchestrator event loop. Satisfied by [persistence.Store].
type OrchestratorStore interface {
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	UpsertAggregateMetrics(ctx context.Context, metrics persistence.AggregateMetrics) error
	UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
	QueryBudgetExhaustedIssues(ctx context.Context, candidateIDs []string, maxSessions int) (map[string]int, error)
	QueryTokenBudgetUsage(ctx context.Context, candidateIDs []string) (map[string]persistence.IssueTokenUsage, error)
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
	UpsertReactionObservation(
		ctx context.Context,
		issueID, kind, fingerprint string,
		observedAt time.Time,
	) (persistence.ReactionObservation, error)
	MarkReactionObservationDispatched(ctx context.Context, issueID, kind, fingerprint string) error
	CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error)
	LatestRunCompletionByIdentifier(ctx context.Context, identifiers []string) (map[string]string, error)
	UpsertParkedIssue(ctx context.Context, entry persistence.ParkedIssue) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	MarkParkedIssueLabelApplied(ctx context.Context, issueID string) error
	ListParkedIssues(ctx context.Context) ([]persistence.ParkedIssue, error)
}

var _ OrchestratorStore = (*persistence.Store)(nil)

// Observer receives notifications when orchestrator state changes.
// Implementations must not block and must not mutate state.
type Observer interface {
	// OnStateChange is called after each event loop iteration that
	// modifies state (tick completion, worker exit, retry fire).
	OnStateChange()
}

// snapshotRequest is a request for a point-in-time runtime snapshot.
// Created and sent to the event loop by [Orchestrator.SnapshotFunc].
// The orchestrator's event loop processes it and sends the result on
// ReplyCh.
type snapshotRequest struct {
	ReplyCh chan<- RuntimeSnapshotResult
}

// agentEventMsg pairs an issue ID with the agent event for delivery
// through the agentEventCh channel.
type agentEventMsg struct {
	IssueID string
	Event   domain.AgentEvent
}

// OrchestratorParams holds the construction-time dependencies for
// [NewOrchestrator]. All fields are required unless documented otherwise.
type OrchestratorParams struct {
	State           *State
	Logger          *slog.Logger
	TrackerAdapter  domain.TrackerAdapter
	AgentAdapter    domain.AgentAdapter
	WorkflowManager WorkflowManager
	Store           OrchestratorStore
	PreflightParams PreflightParams
	Observers       []Observer           // may be nil/empty
	Metrics         domain.Metrics       // may be nil; defaults to NoopMetrics
	ToolRegistry    *domain.ToolRegistry // may be nil
	HostPool        *HostPool            // may be nil; defaults to local-mode pool

	// SessionToolRegistryFunc builds the per-session tool registry for
	// the first-turn advertisement so it matches the set the MCP sidecar
	// serves. Threaded into WorkerDeps. May be nil, in which case the
	// worker advertises from ToolRegistry instead.
	SessionToolRegistryFunc SessionToolRegistryFunc

	// WorkflowFileFunc returns the base filename of the active workflow
	// file (e.g. "WORKFLOW.md"). Used for observability: recorded on
	// RunningEntry and persisted in run_history. If nil, defaults to
	// empty string.
	WorkflowFileFunc func() string

	// DBPath is the absolute path to the SQLite database file. Passed
	// to the MCP server via the config env field. If empty,
	// SORTIE_DB_PATH is set to the empty string in the MCP config.
	DBPath string

	// CIProvider is the CI status provider for CI failure detection.
	// Nil when CI feedback is not configured.
	CIProvider domain.CIStatusProvider

	// SCMAdapter is the SCM adapter for review comment routing.
	// Nil when review comment routing is not configured.
	SCMAdapter domain.SCMAdapter

	// ReviewConfig holds validated review reaction configuration.
	// Zero value when SCMAdapter is nil.
	ReviewConfig ReviewReactionConfig

	// AutoMergeConfig holds validated auto-merge reaction
	// configuration. Zero value when AutoMergeReactionConfigured is
	// false.
	AutoMergeConfig AutoMergeReactionConfig

	// AutoMergeReactionConfigured marks whether the auto-merge feature
	// is active for this process. Threaded into ReconcileParams,
	// HandleWorkerExitParams, and the recovery params.
	AutoMergeReactionConfigured bool

	// BotReviewConfig holds validated bot-review reaction
	// configuration. Zero value when BotReviewConfigured is false.
	BotReviewConfig BotReviewReactionConfig

	// BotReviewConfigured marks whether the bot-review feature is
	// active for this process. Threaded into ReconcileParams,
	// HandleWorkerExitParams, and the recovery params.
	BotReviewConfigured bool

	// MergeConflictConfig holds validated merge-conflict reaction
	// configuration. Zero value when MergeConflictReactionConfigured is
	// false.
	MergeConflictConfig MergeConflictReactionConfig

	// MergeConflictReactionConfigured marks whether the merge-conflict
	// feature is active for this process. Threaded into ReconcileParams,
	// HandleWorkerExitParams, and the recovery params.
	MergeConflictReactionConfigured bool

	// LabelReviewConfig holds validated label-review reaction
	// configuration. Zero value when LabelReviewReactionConfigured is
	// false.
	LabelReviewConfig LabelReviewReactionConfig

	// LabelReviewReactionConfigured marks whether the label-review feature
	// is active for this process. Threaded into ReconcileParams,
	// HandleWorkerExitParams, and the recovery params.
	LabelReviewReactionConfigured bool

	// LabelFixConfig holds validated label-fix reaction
	// configuration. Zero value when LabelFixReactionConfigured is
	// false.
	LabelFixConfig LabelFixReactionConfig

	// LabelFixReactionConfigured marks whether the label-fix feature
	// is active for this process. Threaded into ReconcileParams,
	// HandleWorkerExitParams, and the recovery params.
	LabelFixReactionConfigured bool

	// MergeCompletionConfig holds validated merge-completion reaction
	// configuration. Zero value when MergeCompletionReactionConfigured
	// is false.
	MergeCompletionConfig MergeCompletionReactionConfig

	// MergeCompletionReactionConfigured marks whether the
	// merge-completion feature is active for this process. Threaded
	// into ReconcileParams and HandleWorkerExitParams.
	MergeCompletionReactionConfigured bool

	// AgentAdapterByKind resolves the agent adapter for the given
	// kind. Constructed once at startup from the eagerly-built
	// per-kind adapter cache. When nil, the orchestrator falls back
	// to its single-adapter behavior using the AgentAdapter field
	// for every kind that matches the workflow default and rejects
	// every other kind. This fallback exists for legacy callers
	// during the migration window; the production binary always
	// populates this field.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)

	// BlockerResolver completes a candidate's blocker list according
	// to its tracker adapter's declared blocker source. Optional, and
	// nil means no blocker read happens. That is equivalent to the
	// behavior before this collaborator existed only for an adapter
	// whose candidates already carry every blocker; for an adapter
	// that resolves blockers per issue, candidates stay marked
	// unresolved and the gate holds every one of them, because an
	// unread list is never read as an empty one. The production
	// binary always populates this field.
	BlockerResolver BlockerResolver
}

// Orchestrator owns the poll-and-dispatch event loop and all runtime
// state. Construct via [NewOrchestrator] and run with [Orchestrator.Run].
// Not safe for concurrent use - [Run] must be called from a single
// goroutine. External events are delivered via channels.
type Orchestrator struct {
	state  *State
	logger *slog.Logger

	trackerAdapter     domain.TrackerAdapter
	agentAdapter       domain.AgentAdapter
	agentAdapterByKind func(kind string) (domain.AgentAdapter, error)
	workflowManager    WorkflowManager
	store              OrchestratorStore
	metrics            domain.Metrics
	blockerResolver    BlockerResolver

	workerExitCh chan WorkerResult
	retryTimerCh chan string
	agentEventCh chan agentEventMsg
	selfReviewCh chan selfReviewProgressMsg
	snapshotCh   chan snapshotRequest
	refreshCh    chan struct{}

	preflightParams                   PreflightParams
	observers                         []Observer
	drainTimeout                      time.Duration
	toolRegistry                      *domain.ToolRegistry
	sessionToolRegistryFunc           SessionToolRegistryFunc
	preflightOK                       atomic.Bool
	draining                          atomic.Bool
	hostPool                          *HostPool
	workflowFileFunc                  func() string
	dbPath                            string
	ciProvider                        domain.CIStatusProvider
	scmAdapter                        domain.SCMAdapter
	reviewConfig                      ReviewReactionConfig
	autoMergeConfig                   AutoMergeReactionConfig
	autoMergeReactionConfigured       bool
	botReviewConfig                   BotReviewReactionConfig
	botReviewReactionConfigured       bool
	mergeConflictConfig               MergeConflictReactionConfig
	mergeConflictReactionConfigured   bool
	labelReviewConfig                 LabelReviewReactionConfig
	labelReviewReactionConfigured     bool
	labelFixConfig                    LabelFixReactionConfig
	labelFixReactionConfigured        bool
	mergeCompletionConfig             MergeCompletionReactionConfig
	mergeCompletionReactionConfigured bool
	handoffParkingLabel               string

	// sshStrictHostKeyChecking is the current effective OpenSSH
	// StrictHostKeyChecking value. Written by handleTick on every
	// tick/reload; read by makeWorkerFn at dispatch time.
	sshStrictHostKeyChecking string

	prevWorkerWarnings []WorkerWarning
}

// NewOrchestrator creates an [Orchestrator] with all dependencies wired.
// Does not start the event loop — call [Orchestrator.Run] for that.
func NewOrchestrator(params OrchestratorParams) *Orchestrator {
	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	observers := params.Observers
	if observers == nil {
		observers = []Observer{}
	}

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	maxConc := params.State.MaxConcurrentAgents
	exitBuf := max(maxConc*2, 64)
	retryBuf := max(maxConc*2, 64, len(params.State.RetryAttempts))
	eventBuf := max(maxConc*16, 256)

	hostPool := params.HostPool
	if hostPool == nil {
		hostPool = NewHostPool(nil, 0)
	}

	if hostPool.IsSSHEnabled() {
		snap := hostPool.Snapshot()
		logger.Info("SSH worker mode enabled",
			slog.Int("host_count", len(snap)),
			slog.Int("max_per_host", hostPool.maxPerHost),
		)
	} else {
		// Warn if max_concurrent_agents_per_host is set without ssh_hosts.
		cfg := params.WorkflowManager.Config()
		if worker := cfg.ExtensionSection("worker"); worker != nil {
			if _, hasMax := worker["max_concurrent_agents_per_host"]; hasMax {
				logger.Warn("max_concurrent_agents_per_host has no effect without worker.ssh_hosts")
			}
		}
	}

	agentAdapterByKind := params.AgentAdapterByKind
	if agentAdapterByKind == nil {
		// Migration fallback for legacy callers (tests, dryrun) that
		// have not wired the closure yet. The production binary
		// always populates AgentAdapterByKind via the per-kind
		// adapter cache constructed in cmd/sortie. Resolves the
		// workflow default kind to the single AgentAdapter field;
		// every other kind returns an error so the dispatch path
		// gracefully skips the issue rather than panicking.
		defaultAdapter := params.AgentAdapter
		defaultKind := ""
		if params.WorkflowManager != nil {
			defaultKind = params.WorkflowManager.Config().Agent.Kind
		}
		agentAdapterByKind = func(kind string) (domain.AgentAdapter, error) {
			if kind == defaultKind && defaultAdapter != nil {
				return defaultAdapter, nil
			}
			return nil, fmt.Errorf("agent kind %q is not available (AgentAdapterByKind not wired)", kind)
		}
		logger.Warn("AgentAdapterByKind not provided; falling back to single-adapter mode for the workflow default kind only")
	}

	handoffParkingLabel := defaultHandoffParkingLabel
	if params.WorkflowManager != nil {
		handoffParkingLabel = resolveHandoffParkingLabel(params.WorkflowManager.Config().Reactions)
	}

	o := &Orchestrator{
		state:                             params.State,
		logger:                            logger,
		trackerAdapter:                    params.TrackerAdapter,
		agentAdapter:                      params.AgentAdapter,
		agentAdapterByKind:                agentAdapterByKind,
		workflowManager:                   params.WorkflowManager,
		store:                             params.Store,
		metrics:                           metrics,
		workerExitCh:                      make(chan WorkerResult, exitBuf),
		retryTimerCh:                      make(chan string, retryBuf),
		agentEventCh:                      make(chan agentEventMsg, eventBuf),
		selfReviewCh:                      make(chan selfReviewProgressMsg, eventBuf),
		snapshotCh:                        make(chan snapshotRequest, 4),
		refreshCh:                         make(chan struct{}, 1),
		preflightParams:                   params.PreflightParams,
		observers:                         observers,
		drainTimeout:                      defaultDrainTimeout,
		toolRegistry:                      params.ToolRegistry,
		sessionToolRegistryFunc:           params.SessionToolRegistryFunc,
		hostPool:                          hostPool,
		workflowFileFunc:                  params.WorkflowFileFunc,
		dbPath:                            params.DBPath,
		ciProvider:                        params.CIProvider,
		scmAdapter:                        params.SCMAdapter,
		reviewConfig:                      params.ReviewConfig,
		autoMergeConfig:                   params.AutoMergeConfig,
		autoMergeReactionConfigured:       params.AutoMergeReactionConfigured,
		botReviewConfig:                   params.BotReviewConfig,
		botReviewReactionConfigured:       params.BotReviewConfigured,
		mergeConflictConfig:               params.MergeConflictConfig,
		mergeConflictReactionConfigured:   params.MergeConflictReactionConfigured,
		labelReviewConfig:                 params.LabelReviewConfig,
		labelReviewReactionConfigured:     params.LabelReviewReactionConfigured,
		labelFixConfig:                    params.LabelFixConfig,
		labelFixReactionConfigured:        params.LabelFixReactionConfigured,
		mergeCompletionConfig:             params.MergeCompletionConfig,
		mergeCompletionReactionConfigured: params.MergeCompletionReactionConfigured,
		handoffParkingLabel:               handoffParkingLabel,
		blockerResolver:                   params.BlockerResolver,
	}
	// Startup preflight must have passed for the orchestrator to be
	// constructed, so the initial value is true.
	o.preflightOK.Store(true)
	return o
}

// Run enters the event loop, blocks until ctx is cancelled, and returns.
// Must be called from a single goroutine. On context cancellation the
// tick timer is stopped and a draining shutdown begins: all running
// worker contexts are cancelled, the loop waits up to the drain
// timeout (30 seconds by default) for workers to exit (processing
// results through [HandleWorkerExit] and agent events through
// [HandleAgentEvent]), pending retry timers are stopped, and the
// function returns.
func (o *Orchestrator) Run(ctx context.Context) {
	o.activateReconstructedRetries()

	tickTimer := time.NewTimer(0)
	defer tickTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			o.draining.Store(true)
			tickTimer.Stop()
			o.drainRunningWorkers()
			o.drainTrackerOps()
			o.cancelRetryTimers()
			return

		case <-tickTimer.C:
			o.handleTick(ctx)
			tickTimer.Reset(time.Duration(o.state.PollIntervalMS) * time.Millisecond)

		case workerExit := <-o.workerExitCh:
			cfg := o.workflowManager.Config()
			HandleWorkerExit(o.state, workerExit, HandleWorkerExitParams{
				Store:                             o.store,
				MaxRetryBackoffMS:                 cfg.Agent.MaxRetryBackoffMS,
				MaxConsecutiveAbsences:            cfg.Agent.MaxConsecutiveAbsences,
				HandoffParkingLabel:               o.handoffParkingLabel,
				OnRetryFire:                       o.onRetryFire,
				Ctx:                               ctx,
				Logger:                            o.logger,
				BeforeRemoveHook:                  cfg.Hooks.BeforeRemove,
				HookTimeoutMS:                     cfg.Hooks.TimeoutMS,
				TrackerAdapter:                    o.trackerAdapter,
				HandoffState:                      cfg.Tracker.HandoffState,
				ActiveStates:                      cfg.Tracker.ActiveStates,
				TerminalStates:                    cfg.Tracker.TerminalStates,
				Metrics:                           o.metrics,
				HostPool:                          o.hostPool,
				CommentsConfig:                    cfg.Tracker.Comments,
				CIProvider:                        o.ciProvider,
				SCMAdapter:                        o.scmAdapter,
				AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
				BotReviewReactionConfigured:       o.botReviewReactionConfigured,
				MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
				LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
				LabelFixReactionConfigured:        o.labelFixReactionConfigured,
				MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
			})
			o.updateGauges(time.Now())
			o.notifyObservers()

		case issueID := <-o.retryTimerCh:
			cfg := o.workflowManager.Config()
			HandleRetryTimer(o.state, issueID, HandleRetryTimerParams{
				Store:                  o.store,
				TrackerAdapter:         o.trackerAdapter,
				ActiveStates:           cfg.Tracker.ActiveStates,
				TerminalStates:         cfg.Tracker.TerminalStates,
				HandoffState:           cfg.Tracker.HandoffState,
				MaxRetryBackoffMS:      cfg.Agent.MaxRetryBackoffMS,
				MakeWorkerFn:           o.makeWorkerFn,
				AgentAdapterByKind:     o.agentAdapterByKind,
				DefaultAgentKind:       cfg.Agent.Kind,
				OnRetryFire:            o.onRetryFire,
				Ctx:                    ctx,
				Logger:                 o.logger,
				MaxSessions:            cfg.Agent.MaxSessions,
				MaxConsecutiveAbsences: cfg.Agent.MaxConsecutiveAbsences,
				HandoffParkingLabel:    o.handoffParkingLabel,
				HandoffEvidencePolicy:  cfg.Tracker.HandoffEvidence,
				MaxTokens:              cfg.Agent.MaxTokens,
				Metrics:                o.metrics,
				HostPool:               o.hostPool,
				WorkflowFile:           o.workflowFile(),
			})
			o.updateGauges(time.Now())
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			HandleAgentEvent(o.state, msg.IssueID, msg.Event, o.logger, o.metrics)
			o.maybeWriteIncrementalMetadata(ctx, msg.IssueID, msg.Event)

		case msg := <-o.selfReviewCh:
			if entry, ok := o.state.Running[msg.IssueID]; ok {
				if msg.Message == "self_review_done" {
					entry.SelfReviewActive = false
					entry.SelfReviewIteration = 0
				} else {
					entry.SelfReviewActive = true
					entry.SelfReviewIteration = msg.Iteration
				}
			}

		case req := <-o.snapshotCh:
			snap := RuntimeSnapshot(o.state, time.Now())
			req.ReplyCh <- snap

		case <-o.refreshCh:
			o.handleTick(ctx)
		}
	}
}

// updateGauges recomputes all point-in-time gauges from current state
// and publishes them via the Metrics interface. It is called from the
// event loop after tick handling, worker exits, and retry timer events.
func (o *Orchestrator) updateGauges(now time.Time) {
	o.metrics.SetRunningSessions(len(o.state.Running))
	o.metrics.SetRetryingSessions(len(o.state.RetryAttempts))
	o.metrics.SetAvailableSlots(GlobalAvailableSlots(o.state.MaxConcurrentAgents, len(o.state.Running)))
	o.metrics.SetActiveSessionsElapsed(ActiveElapsedSeconds(o.state, now))

	// Always emit host-usage gauges from the full usage map. This covers
	// hosts removed by config reload that still have in-flight workers,
	// ensuring their gauges decrement to zero when workers exit rather
	// than freezing at the last published value.
	for host, count := range o.hostPool.Snapshot() {
		o.metrics.SetSSHHostUsage(host, count)
	}

	// Always emit a value for every declared budget reason, not only
	// those currently held, so a reason that clears reports zero rather
	// than freezing at its last published value.
	budgetCounts := make(map[string]int, len(knownBudgetReasons))
	for _, entry := range o.state.BudgetExhausted {
		budgetCounts[entry.Reason]++
	}
	for _, reason := range knownBudgetReasons {
		o.metrics.SetBudgetExhaustedIssues(reason, budgetCounts[reason])
	}
}

// handleTick executes a single poll-and-dispatch cycle: preflight,
// config read, reconcile, fetch, sort, dispatch. Called from the event
// loop on each tick timer fire.
//
// Preflight runs first so the config reload (if any) is visible to all
// subsequent steps. Reconciliation and state-field updates always run —
// even when preflight fails — to keep orchestrator state aligned with
// the tracker using the last-known-good config, which remains valid for
// those purposes. Dispatch is the only step gated on preflight success.
func (o *Orchestrator) handleTick(ctx context.Context) {
	tickStart := time.Now()
	pollResult := outcomeSuccess
	defer func() {
		o.metrics.IncPollCycles(pollResult)
		o.metrics.ObservePollDuration(time.Since(tickStart).Seconds())
		o.updateGauges(time.Now())
	}()

	// Preflight triggers a defensive Reload() so the config snapshot
	// below reflects the latest disk state.
	validation := ValidateDispatchConfig(o.preflightParams)
	o.preflightOK.Store(validation.OK())

	// On reload failure the workflow manager retains last-known-good
	// config, so Config() always returns a usable snapshot.
	cfg := o.workflowManager.Config()

	// Apply config to state unconditionally — not gated on preflight
	// success.
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	o.state.MaxConcurrentByState = cfg.Agent.MaxConcurrentByState

	// Update host pool from config extensions.
	wc := ParseWorkerConfig(cfg.ExtensionSection("worker"))
	o.hostPool.Update(wc.SSHHosts, wc.MaxPerHost)
	o.sshStrictHostKeyChecking = wc.SSHStrictHostKeyChecking

	if !workerWarningsEqual(o.prevWorkerWarnings, wc.Warnings) {
		for _, w := range wc.Warnings {
			o.logger.LogAttrs(ctx, slog.LevelWarn, w.Message, w.Attrs...) //nolint:sloglint // WorkerWarning.Message is one of two fixed string constants from parseSSHStrictHostKeyChecking
		}
		o.prevWorkerWarnings = wc.Warnings
	}

	// Reconcile running issues unconditionally so in-flight workers
	// are monitored even when dispatch is skipped.
	ReconcileRunningIssues(o.state, ReconcileParams{
		TrackerAdapter:                    o.trackerAdapter,
		ActiveStates:                      cfg.Tracker.ActiveStates,
		TerminalStates:                    cfg.Tracker.TerminalStates,
		HandoffState:                      cfg.Tracker.HandoffState,
		StallTimeoutMS:                    cfg.Agent.StallTimeoutMS,
		MaxRetryBackoffMS:                 cfg.Agent.MaxRetryBackoffMS,
		Store:                             o.store,
		OnRetryFire:                       o.onRetryFire,
		Ctx:                               ctx,
		Logger:                            o.logger,
		Metrics:                           o.metrics,
		CIProvider:                        o.ciProvider,
		CIFeedback:                        cfg.CIFeedback,
		CIWatchWindow:                     time.Duration(cfg.CIFeedback.WatchWindowMS) * time.Millisecond,
		SCMAdapter:                        o.scmAdapter,
		ReviewConfig:                      o.reviewConfig,
		ReviewPendingTTL:                  reviewPendingDefaultTTL,
		AutoMergeConfig:                   o.autoMergeConfig,
		AutoMergePendingTTL:               autoMergePendingDefaultTTL,
		AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
		BotReviewConfig:                   o.botReviewConfig,
		BotReviewPendingTTL:               reviewPendingDefaultTTL,
		BotReviewConfigured:               o.botReviewReactionConfigured,
		MergeConflictConfig:               o.mergeConflictConfig,
		MergeConflictPendingTTL:           mergeConflictPendingDefaultTTL,
		MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
		LabelReviewConfig:                 o.labelReviewConfig,
		LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
		LabelFixConfig:                    o.labelFixConfig,
		LabelFixReactionConfigured:        o.labelFixReactionConfigured,
		MergeCompletionConfig:             o.mergeCompletionConfig,
		MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
	})

	// Sweep workspaces periodically to catch issues that transitioned
	// after their worker exited, or whose activity has aged past the
	// configured retention window.
	o.state.SweepTickCounter++
	if o.state.SweepTickCounter >= sweepEveryNTicks {
		o.state.SweepTickCounter = 0
		SweepWorkspaces(o.state, SweepWorkspacesParams{
			WorkspaceRoot:    cfg.Workspace.Root,
			TrackerAdapter:   o.trackerAdapter,
			TerminalStates:   cfg.Tracker.TerminalStates,
			BeforeRemoveHook: cfg.Hooks.BeforeRemove,
			HookTimeoutMS:    cfg.Hooks.TimeoutMS,
			RetentionDays:    cfg.Workspace.RetentionDays,
			Store:            o.store,
			Ctx:              ctx,
			Logger:           o.logger,
			Metrics:          o.metrics,
		})
	}

	// On preflight failure, skip dispatch but still notify observers
	// so the UI reflects the reconciliation outcome.
	if !validation.OK() {
		pollResult = outcomeError
		o.logger.Error("dispatch preflight failed",
			slog.Any("error", validation),
		)
		o.notifyObservers()
		return
	}

	// Fetch candidate issues from the tracker.
	issues, err := o.trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		pollResult = outcomeError
		o.logger.Error("failed to fetch candidate issues",
			slog.Any("error", err),
		)
		o.notifyObservers()
		return
	}

	sorted := SortForDispatch(issues)

	o.rebuildBudgetExhausted(ctx, cfg, sorted)
	o.refreshParkedIssues(ctx, sorted)
	o.parkExhaustedAbsences(ctx, cfg, sorted)

	// Pre-build state sets once for the dispatch loop.
	activeSet := stateSet(cfg.Tracker.ActiveStates)
	terminalSet := stateSet(cfg.Tracker.TerminalStates)

	// Break only when global capacity is exhausted; skip individual
	// issues whose per-state limit is full so issues in other states
	// can still be dispatched.
	pass := &TickResolution{offset: o.state.BlockerReadOffset}

	var dispatched, dispatchedByRule, dispatchedByDefault, dispatchedByFallback int
	var heldByBlockers, blockersUnresolvedHeld, blockersNotReadHeld, blockersIncompleteHeld int
	var readFailures int
	for _, issue := range sorted {
		if GlobalAvailableSlots(o.state.MaxConcurrentAgents, len(o.state.Running)) == 0 {
			break
		}
		if o.hostPool.IsSSHEnabled() && !o.hostPool.HasCapacity() {
			break
		}
		if !HasAvailableSlots(o.state, issue.State) {
			continue
		}

		decision := EvaluateCandidate(ctx, issue, o.state, activeSet, terminalSet, o.blockerResolver, pass)
		if !decision.Dispatch {
			if decision.Err != nil {
				readFailures++
			}
			o.recordCandidateHold(decision, pass, terminalSet)
			switch decision.Reason {
			case SkipBlockedBy:
				heldByBlockers++
			case SkipBlockersUnresolved:
				blockersUnresolvedHeld++
			case SkipBlockersNotRead:
				blockersNotReadHeld++
			case SkipBlockersIncomplete:
				blockersIncompleteHeld++
			}
			continue
		}
		issue = decision.Issue

		resolution := ResolveRule(issue, cfg.Dispatch, cfg.Agent.Kind, "")
		adapter, adapterErr := o.agentAdapterByKind(resolution.AgentKind)
		if adapterErr != nil {
			o.logger.Error("agent kind unavailable",
				slog.String("rule_name", resolution.RuleName),
				slog.String("agent_kind", resolution.AgentKind),
				slog.Any("error", adapterErr),
			)
			o.metrics.IncDispatches(outcomeError)
			o.metrics.IncDispatchRuleMatch(resolution.MatchedAt.String(), normalizeDispatchRuleName(resolution.RuleName))
			continue
		}
		tmpl := o.workflowManager.PromptTemplateByID(resolution.TemplateID)
		if tmpl == nil {
			o.logger.Error("template id unavailable",
				slog.String("rule_name", resolution.RuleName),
				slog.String("template_id", resolution.TemplateID),
			)
			o.metrics.IncDispatches(outcomeError)
			o.metrics.IncDispatchRuleMatch(resolution.MatchedAt.String(), normalizeDispatchRuleName(resolution.RuleName))
			continue
		}

		host, ok := o.hostPool.AcquireHost(issue.ID, "")
		if !ok {
			break
		}
		DispatchIssue(ctx, o.state, issue, nil, host, o.makeWorkerFn("", host, resolution.AgentKind, resolution.TemplateID, "", adapter))
		if entry := o.state.Running[issue.ID]; entry != nil {
			entry.WorkflowFile = o.workflowFile()
			entry.AgentKind = resolution.AgentKind
			entry.RuleName = resolution.RuleName
			entry.TemplateID = resolution.TemplateID
		}
		o.metrics.IncDispatches(outcomeSuccess)
		o.metrics.IncDispatchRuleMatch(resolution.MatchedAt.String(), normalizeDispatchRuleName(resolution.RuleName))
		switch resolution.MatchedAt {
		case ResolvedFromRule:
			dispatchedByRule++
		case ResolvedFromDefault:
			dispatchedByDefault++
		default:
			dispatchedByFallback++
		}
		dispatched++
	}

	o.state.BlockerReadOffset = nextBlockerReadOffset(pass)

	switch {
	case pass.halted:
		o.logger.Error("blocker reads halted for this tick",
			slog.String("error_kind", blockerErrorKind(pass.haltErr)),
			slog.Int("http_status", blockerErrorStatus(pass.haltErr)),
			slog.String("operation", "fetch_blockers"),
			slog.Int("held_unread", pass.heldUnread),
		)
	case dispatched == 0 && pass.reads > 0 && readFailures == pass.reads:
		o.logger.Warn("tick dispatched nothing: every attempted candidate blocker read failed",
			slog.Int("reads_failed", readFailures),
		)
	}

	o.logger.Info("tick completed",
		slog.Int("candidates", len(sorted)),
		slog.Int("dispatched", dispatched),
		slog.Int("dispatched_by_rule", dispatchedByRule),
		slog.Int("dispatched_by_default", dispatchedByDefault),
		slog.Int("dispatched_by_fallback", dispatchedByFallback),
		slog.Int("running", len(o.state.Running)),
		slog.Int("retrying", len(o.state.RetryAttempts)),
		slog.Int("held_by_blockers", heldByBlockers),
		slog.Int("blockers_unresolved", blockersUnresolvedHeld),
		slog.Int("blockers_not_read", blockersNotReadHeld),
		slog.Int("blockers_incomplete", blockersIncompleteHeld),
		slog.Int("budget_exhausted", len(o.state.BudgetExhausted)),
	)

	o.notifyObservers()
}

// recordCandidateHold logs the per-issue observability record for one
// candidate the dispatch gate held, and increments [IncCandidateHolds]
// for every reason except [SkipIneligible], which produces no record
// and no counter increment (it predates the blocker gate and carries
// its own silence forward).
func (o *Orchestrator) recordCandidateHold(decision CandidateDecision, pass *TickResolution, terminalSet map[string]struct{}) {
	if decision.Reason == SkipIneligible {
		return
	}

	issue := decision.Issue
	log := logging.WithIssue(o.logger, issue.ID, issue.Identifier)

	switch decision.Reason {
	case SkipBlockedBy:
		blocker := firstNonTerminalBlocker(issue.BlockedBy, terminalSet)
		log.Debug("candidate held by blocker",
			slog.String("blocker_identifier", blocker.Identifier),
			slog.String("blocker_state", blocker.State),
		)
	case SkipBlockersUnresolved:
		if decision.Err != nil {
			log.Warn("candidate blockers unresolved, holding issue",
				slog.Any("error", decision.Err),
			)
		} else {
			log.Debug("candidate blockers not read this tick, pass halted",
				slog.String("error_kind", blockerErrorKind(pass.haltErr)),
			)
		}
	case SkipBlockersNotRead:
		log.Debug("candidate blockers not read this tick, holding issue",
			slog.Int("reads_spent", pass.reads),
		)
	case SkipBlockersIncomplete:
		log.Debug("candidate blocker list incomplete, holding issue")
	}

	o.metrics.IncCandidateHolds(string(decision.Reason))
}

// makeWorkerFn returns a [WorkerFunc] closure that runs
// [RunWorkerAttempt] with the orchestrator's shared dependencies.
// The closure captures channel references for OnEvent and OnExit
// delivery. agentKind, templateID, and adapter carry the rule-resolved
// selection from the caller (handleTick for initial dispatches,
// HandleRetryTimer for retries). reactionKind selects the worker
// posture via [dispatchPostureForReactionKind]. The
// resumeSessionID must be read by the caller (on the event loop
// goroutine) before the goroutine starts, to avoid a data race on the
// Running map.
func (o *Orchestrator) makeWorkerFn(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter) WorkerFunc {
	strictHostKeyChecking := o.sshStrictHostKeyChecking
	posture := dispatchPostureForReactionKind(reactionKind)
	if adapter == nil {
		adapter = o.agentAdapter
	}
	if agentKind == "" {
		agentKind = o.workflowManager.Config().Agent.Kind
	}
	return func(ctx context.Context, issue domain.Issue, attempt *int) {

		logger := logging.WithIssue(o.logger, issue.ID, issue.Identifier)

		deps := WorkerDeps{
			TrackerAdapter:         o.trackerAdapter,
			AgentAdapter:           adapter,
			ConfigFunc:             o.workflowManager.Config,
			PromptTemplateByIDFunc: o.workflowManager.PromptTemplateByID,
			TemplateID:             templateID,
			AgentKind:              agentKind,
			OnEvent: func(issueID string, event domain.AgentEvent) {
				select {
				case o.agentEventCh <- agentEventMsg{IssueID: issueID, Event: event}:
				default:
					logger.Warn("agent event channel full, dropping event",
						slog.Any("event_type", event.Type),
					)
				}
			},
			OnExit: func(issueID string, result WorkerResult) {
				o.workerExitCh <- result
			},
			OnProgress: func(msg selfReviewProgressMsg) {
				select {
				case o.selfReviewCh <- msg:
				default:
					logger.Warn("self-review progress channel full, dropping",
						slog.String("issue_id", msg.IssueID),
					)
				}
			},
			ResumeSessionID:         resumeSessionID,
			Logger:                  logger,
			ToolRegistry:            o.toolRegistry,
			SessionToolRegistryFunc: o.sessionToolRegistryFunc,
			AgentToolChannelFunc: func(kind string, remote bool) bool {
				meta, _ := o.preflightParams.AgentRegistry.Meta(kind)
				return meta.MCPInjection.DeliversTools(remote)
			},
			SSHHost:                  sshHost,
			SSHStrictHostKeyChecking: strictHostKeyChecking,
			Metrics:                  o.metrics,
			WorkflowPath:             o.workflowManager.WorkflowAbsPath(),
			DBPath:                   o.dbPath,
			Posture:                  posture,
		}

		RunWorkerAttempt(ctx, issue, attempt, deps)
	}
}

// workflowFile returns the base filename of the active workflow file.
// Returns empty string when no callback is configured.
func (o *Orchestrator) workflowFile() string {
	if o.workflowFileFunc != nil {
		return o.workflowFileFunc()
	}
	return ""
}

// onRetryFire delivers a retry timer event to the event loop channel.
// Uses a non-blocking send to prevent deadlock when the buffer is full.
func (o *Orchestrator) onRetryFire(issueID string) {
	select {
	case o.retryTimerCh <- issueID:
	default:
		o.logger.Warn("retry timer channel full, dropping event",
			slog.String("issue_id", issueID),
			slog.Int("retry_timer_channel_len", len(o.retryTimerCh)),
			slog.Int("retry_timer_channel_cap", cap(o.retryTimerCh)),
		)
	}
}

// activateReconstructedRetries starts timers for retry entries that
// were populated by [PopulateRetries] during startup recovery. Entries
// with TimerHandle == nil are pending activation. Entries with
// scheduledDelayMS > 0 get a [time.AfterFunc] timer; entries with
// scheduledDelayMS == 0 (past-due) are written directly to
// retryTimerCh. Called at the top of [Run] before entering the select
// loop, relying on the channel buffer sizing to tolerate immediate-fire
// entries written before the loop begins draining the channel.
func (o *Orchestrator) activateReconstructedRetries() {
	for issueID, entry := range o.state.RetryAttempts {
		if entry.TimerHandle != nil {
			continue
		}
		if entry.scheduledDelayMS > 0 {
			entry.TimerHandle = time.AfterFunc(
				time.Duration(entry.scheduledDelayMS)*time.Millisecond,
				func() { o.onRetryFire(issueID) },
			)
		} else {
			o.retryTimerCh <- issueID
		}
	}
}

// defaultDrainTimeout is the maximum duration the orchestrator waits for
// running workers to exit during graceful shutdown.
const defaultDrainTimeout = 30 * time.Second

// sessionMetadataWriteInterval bounds how often the event loop writes an
// in-flight session's token totals to session_metadata. At most one
// incremental write per issue per interval, so the advisory cost reading
// trails live spend by at most one interval plus whatever accrued since
// the last token_usage event.
const sessionMetadataWriteInterval = 2 * time.Second

// maybeWriteIncrementalMetadata persists the running session's current
// token totals to session_metadata when an event carrying non-zero
// usage arrives, or a token_usage event carrying a measurement of zero
// arrives, and the per-issue throttle interval has elapsed. Widening
// the gate to the token_usage event type, not only a non-zero usage
// component, makes a session_metadata row exist exactly when the
// session has reported a measurement, including one that reports zero.
// It is a no-op for an event carrying neither signal, for unknown
// issues, and while throttled. Must be called from the orchestrator's
// single-writer event loop so it shares the one SQLite writer and the
// running entry it mutates.
func (o *Orchestrator) maybeWriteIncrementalMetadata(ctx context.Context, issueID string, event domain.AgentEvent) {
	if !hasUsage(event.Usage) && event.Type != domain.EventTokenUsage {
		return
	}
	entry := o.state.Running[issueID]
	if entry == nil {
		return
	}
	now := time.Now().UTC()
	if !entry.LastMetadataWrite.IsZero() && now.Sub(entry.LastMetadataWrite) < sessionMetadataWriteInterval {
		return
	}

	meta := persistence.SessionMetadata{
		IssueID:         issueID,
		SessionID:       entry.SessionID,
		InputTokens:     entry.AgentInputTokens,
		OutputTokens:    entry.AgentOutputTokens,
		TotalTokens:     entry.AgentTotalTokens,
		CacheReadTokens: entry.CacheReadTokens,
		ModelName:       entry.ModelName,
		APIRequestCount: entry.APIRequestCount,
		UpdatedAt:       now.Format(time.RFC3339),
	}
	if entry.AgentPID != "" {
		meta.AgentPID = &entry.AgentPID
	}
	if err := o.store.UpsertSessionMetadata(ctx, meta); err != nil {
		o.logger.Error("failed to persist incremental session metadata",
			slog.Any("error", err),
		)
		return
	}
	entry.LastMetadataWrite = now
}

// refreshParkedIssues evaluates the release rule against this tick's
// candidates, then against the state of every parked issue the candidate
// slice does not carry, read through one batched, comment-free tracker
// call. Must be called from the event loop goroutine, after the budget
// rebuild and before the dispatch loop.
func (o *Orchestrator) refreshParkedIssues(ctx context.Context, candidates []domain.Issue) {
	if len(o.state.Parked) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, issue := range candidates {
		entry, ok := o.state.Parked[issue.ID]
		if !ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		if !observeParkedState(ctx, o.state, o.store, o.logger, entry, issue.ID, issue.State) {
			continue
		}
		observeParkedLabels(ctx, o.state, o.store, o.logger, entry, issue.ID, issue.Labels)
	}

	missing := make([]string, 0, len(o.state.Parked))
	for id := range o.state.Parked {
		if _, ok := seen[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 || o.trackerAdapter == nil {
		return
	}
	slices.Sort(missing)

	states, err := o.trackerAdapter.FetchIssueStatesByIDs(ctx, missing)
	if err != nil {
		o.logger.Warn("parked issue state read failed, retaining parks",
			slog.Any("error", err),
		)
		return
	}
	for _, id := range missing {
		entry, ok := o.state.Parked[id]
		if !ok {
			continue
		}
		observed, found := states[id]
		if !found {
			continue
		}
		observeParkedState(ctx, o.state, o.store, o.logger, entry, id, observed)
	}
}

// parkExhaustedAbsences parks each candidate whose consecutive
// handoff-absence count has just reached the ceiling. Skipped entirely
// under the off evidence policy, which records no absence. Must be called
// from the event loop goroutine, after [Orchestrator.refreshParkedIssues]
// so a release on this tick is not immediately re-parked.
func (o *Orchestrator) parkExhaustedAbsences(ctx context.Context, cfg config.ServiceConfig, candidates []domain.Issue) {
	if cfg.Tracker.HandoffEvidence.Effective() == config.HandoffEvidenceOff {
		return
	}

	candidateIDs := make([]string, len(candidates))
	for i, issue := range candidates {
		candidateIDs[i] = issue.ID
	}

	ceiling := handoffAbsenceCeiling(cfg.Agent.MaxConsecutiveAbsences)
	counts, err := o.store.QueryConsecutiveHandoffAbsenceCounts(ctx, candidateIDs)
	if err != nil {
		o.logger.Warn("handoff absence exhaustion query failed, retaining previous set",
			slog.Any("error", err),
		)
		return
	}

	for _, issue := range candidates {
		count := counts[issue.ID]
		if count < ceiling {
			continue
		}
		if _, parked := o.state.Parked[issue.ID]; parked {
			continue
		}
		issueLog := logging.WithIssue(o.logger, issue.ID, issue.Identifier)
		parkHandoffAbsence(
			o.state,
			ctx,
			o.store,
			o.trackerAdapter,
			o.metrics,
			issue.ID,
			issue.Identifier,
			issue.DisplayID,
			issue.State,
			count,
			ceiling,
			o.handoffParkingLabel,
			issueLog,
		)
	}
}

// ceilingSettingByBudgetReason maps a machine-readable budget-hold
// reason to the dotted configuration path of the setting that governs
// it, for the "candidate held by budget ceiling" log record. A reason
// absent from this map emits no ceiling_setting attribute rather than
// an empty or invented one.
var ceilingSettingByBudgetReason = map[string]string{
	budgetReasonSession: "agent.max_sessions",
	budgetReasonToken:   "agent.max_tokens",
}

// rebuildBudgetExhausted replaces the BudgetExhausted set once per tick
// from run_history, as the union of the session-count and token-sum
// gates scoped to the candidate set. Token budget takes precedence over
// the ordinary session budget. On a query error for one axis, the prior
// entries attributed to that axis are folded back in so a transient
// error never drops an issue mid-tick, while the other axis keeps its
// fresh results. An issue entering the set for the first time under a
// given reason, since restart or since its last hold, produces one
// per-issue log record and one counter increment; a hold the memory
// already knows about produces neither. Must be called from the event
// loop goroutine.
func (o *Orchestrator) rebuildBudgetExhausted(ctx context.Context, cfg config.ServiceConfig, sorted []domain.Issue) {
	if cfg.Agent.MaxSessions == 0 && cfg.Agent.MaxTokens == 0 {
		o.state.BudgetExhausted = make(map[string]*BudgetExhaustedEntry)
		o.state.TokenBudgetIncomplete = make(map[string]struct{})
		return
	}

	candidateIDs := make([]string, len(sorted))
	identifierByID := make(map[string]string, len(sorted))
	displayIDByID := make(map[string]string, len(sorted))
	for i, issue := range sorted {
		candidateIDs[i] = issue.ID
		identifierByID[issue.ID] = issue.Identifier
		displayIDByID[issue.ID] = issue.DisplayID
	}

	prior := o.state.BudgetExhausted
	fresh := make(map[string]*BudgetExhaustedEntry)
	now := time.Now().UTC()

	if cfg.Agent.MaxSessions > 0 {
		counts, qErr := o.store.QueryBudgetExhaustedIssues(ctx, candidateIDs, cfg.Agent.MaxSessions)
		if qErr != nil {
			o.logger.Warn("budget exhaustion query failed, retaining previous set",
				slog.Any("error", qErr),
			)
			for id, entry := range prior {
				if entry.Reason != budgetReasonSession {
					continue
				}
				fresh[id] = entry
			}
		} else {
			for id, count := range counts {
				fresh[id] = &BudgetExhaustedEntry{
					Identifier:     identifierByID[id],
					DisplayID:      displayIDByID[id],
					Reason:         budgetReasonSession,
					UsedSessions:   count,
					BudgetSessions: cfg.Agent.MaxSessions,
					BudgetTokens:   int64(cfg.Agent.MaxTokens),
				}
			}
		}
	}

	freshIncomplete := make(map[string]struct{})
	if cfg.Agent.MaxTokens > 0 {
		usageByIssue, qErr := o.store.QueryTokenBudgetUsage(ctx, candidateIDs)
		if qErr != nil {
			o.logger.Warn("token budget exhaustion query failed, retaining previous set",
				slog.Any("error", qErr),
			)
			for id, entry := range prior {
				if entry.Reason != budgetReasonToken {
					continue
				}
				fresh[id] = entry
			}
		} else {
			for _, id := range candidateIDs {
				usage := usageByIssue[id]
				if usage.TotalTokens >= int64(cfg.Agent.MaxTokens) {
					fresh[id] = &BudgetExhaustedEntry{
						Identifier:         identifierByID[id],
						DisplayID:          displayIDByID[id],
						Reason:             budgetReasonToken,
						UsedSessions:       usage.Sessions,
						BudgetSessions:     cfg.Agent.MaxSessions,
						UsedTokens:         &usage.TotalTokens,
						BudgetTokens:       int64(cfg.Agent.MaxTokens),
						UnmeasuredSessions: &usage.UnmeasuredSessions,
					}
					continue
				}
				if entry, held := fresh[id]; held {
					entry.UsedTokens = &usage.TotalTokens
					entry.UnmeasuredSessions = &usage.UnmeasuredSessions
				}
				if usage.UnmeasuredSessions == 0 {
					continue
				}
				freshIncomplete[id] = struct{}{}
				if _, wasIncomplete := o.state.TokenBudgetIncomplete[id]; wasIncomplete {
					continue
				}
				issueLog := logging.WithIssue(o.logger, id, identifierByID[id])
				issueLog.Warn("token budget cannot be fully evaluated, allowing dispatch",
					slog.Int64("used_tokens", usage.TotalTokens),
					slog.Int64("budget_tokens", int64(cfg.Agent.MaxTokens)),
					slog.Int("unmeasured_sessions", usage.UnmeasuredSessions),
				)
			}
		}
	}
	o.state.TokenBudgetIncomplete = freshIncomplete

	for id, entry := range fresh {
		told, wasTold := o.state.BudgetAnnounced[id]
		if wasTold && told.Reason == entry.Reason {
			entry.ExhaustedAt = told.At
			continue
		}
		entry.ExhaustedAt = now
		o.state.BudgetAnnounced[id] = BudgetAnnouncement{Reason: entry.Reason, At: now}
		issueLog := logging.WithIssue(o.logger, id, entry.Identifier)
		attrs := []slog.Attr{
			slog.String("reason", entry.Reason),
			slog.Int("used_sessions", entry.UsedSessions),
			slog.Int("budget_sessions", entry.BudgetSessions),
		}
		if entry.UsedTokens != nil {
			attrs = append(attrs,
				slog.Int64("used_tokens", *entry.UsedTokens),
				slog.Int64("budget_tokens", entry.BudgetTokens),
			)
		}
		if setting, known := ceilingSettingByBudgetReason[entry.Reason]; known {
			attrs = append(attrs, slog.String("ceiling_setting", setting))
		}
		issueLog.LogAttrs(ctx, slog.LevelWarn, "candidate held by budget ceiling", attrs...)
		o.metrics.IncBudgetExhaustions(entry.Reason)
	}

	for _, id := range candidateIDs {
		if _, held := fresh[id]; !held {
			delete(o.state.BudgetAnnounced, id)
		}
	}

	o.state.BudgetExhausted = fresh
}

// drainRunningWorkers cancels all running worker contexts and waits for
// them to exit, processing each [WorkerResult] through [HandleWorkerExit]
// for clean persistence. Agent events are processed through
// [HandleAgentEvent] to capture final token usage. Observer notifications
// fire after each worker exit for dashboard visibility. Returns when all
// workers have exited or the drain timeout expires.
func (o *Orchestrator) drainRunningWorkers() {
	remaining := len(o.state.Running)
	if remaining == 0 {
		return
	}

	o.logger.Info("draining workers",
		slog.Int("count", remaining),
	)

	for _, entry := range o.state.Running {
		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}
	}

	deadline := time.NewTimer(o.drainTimeout)
	defer deadline.Stop()

	// The parent ctx is already cancelled; SQLite writes in
	// HandleWorkerExit need a live context.
	drainCtx := context.Background()

	for len(o.state.Running) > 0 {
		select {
		case workerExit := <-o.workerExitCh:
			cfg := o.workflowManager.Config()
			HandleWorkerExit(o.state, workerExit, HandleWorkerExitParams{
				Store:                             o.store,
				MaxRetryBackoffMS:                 cfg.Agent.MaxRetryBackoffMS,
				MaxConsecutiveAbsences:            cfg.Agent.MaxConsecutiveAbsences,
				OnRetryFire:                       func(string) {}, // no-op: prevent retry fire events from reaching the event loop during drain
				Ctx:                               drainCtx,
				Logger:                            o.logger,
				BeforeRemoveHook:                  cfg.Hooks.BeforeRemove,
				HookTimeoutMS:                     cfg.Hooks.TimeoutMS,
				TrackerAdapter:                    o.trackerAdapter,
				HandoffState:                      cfg.Tracker.HandoffState,
				ActiveStates:                      cfg.Tracker.ActiveStates,
				TerminalStates:                    cfg.Tracker.TerminalStates,
				Metrics:                           o.metrics,
				HostPool:                          o.hostPool,
				CommentsConfig:                    cfg.Tracker.Comments,
				CIProvider:                        o.ciProvider,
				SCMAdapter:                        o.scmAdapter,
				AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
				BotReviewReactionConfigured:       o.botReviewReactionConfigured,
				MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
				LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
				LabelFixReactionConfigured:        o.labelFixReactionConfigured,
				MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
			})
			o.updateGauges(time.Now())
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			HandleAgentEvent(o.state, msg.IssueID, msg.Event, o.logger, o.metrics)
			o.maybeWriteIncrementalMetadata(drainCtx, msg.IssueID, msg.Event)

		case req := <-o.snapshotCh:
			snap := RuntimeSnapshot(o.state, time.Now())
			req.ReplyCh <- snap

		case <-o.refreshCh:
			// Discard refresh signals during drain; the event loop is no
			// longer accepting new work.

		case <-deadline.C:
			o.logger.Warn("drain timeout exceeded, abandoning workers",
				slog.Int("remaining", len(o.state.Running)),
			)
			return
		}
	}

}

// trackerOpsDrainTimeout bounds how long shutdown waits for in-flight
// tracker API goroutines. Set slightly above the 30-second context
// timeout used by the goroutines themselves.
const trackerOpsDrainTimeout = 35 * time.Second

// drainTrackerOps waits for all in-flight fire-and-forget tracker API
// goroutines (comments, labels) to complete. The wait is bounded so a
// stuck adapter cannot block process exit indefinitely. Called from
// Run after drainRunningWorkers regardless of whether workers existed.
func (o *Orchestrator) drainTrackerOps() {
	done := make(chan struct{})
	go func() {
		o.state.TrackerOpsWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(trackerOpsDrainTimeout):
		o.logger.Warn("tracker ops drain timeout exceeded, abandoning in-flight calls")
	}
}

// cancelRetryTimers stops all pending retry timers to prevent late fires
// after the event loop exits.
func (o *Orchestrator) cancelRetryTimers() {
	for _, entry := range o.state.RetryAttempts {
		if entry.TimerHandle != nil {
			entry.TimerHandle.Stop()
		}
	}
}

// notifyObservers calls [Observer.OnStateChange] on each registered
// observer. Called after tick completion, worker exit handling, and
// retry timer handling. Not called after agent events (high frequency).
func (o *Orchestrator) notifyObservers() {
	for _, obs := range o.observers {
		obs.OnStateChange()
	}
}

// AddObserver appends an observer to the notification list. Must be
// called before [Orchestrator.Run] or between event loop iterations
// (i.e., never concurrently with the event loop).
func (o *Orchestrator) AddObserver(obs Observer) {
	o.observers = append(o.observers, obs)
}

// PreflightOK returns whether the most recent dispatch preflight
// validation passed. Safe to call from any goroutine.
func (o *Orchestrator) PreflightOK() bool {
	return o.preflightOK.Load()
}

// SnapshotFunc returns a function that retrieves a point-in-time
// runtime snapshot via the event loop channel. The returned function
// is safe to call from any goroutine. It blocks until the event loop
// produces the snapshot or a 5-second timeout expires.
func (o *Orchestrator) SnapshotFunc() func() (RuntimeSnapshotResult, error) {
	return func() (RuntimeSnapshotResult, error) {
		replyCh := make(chan RuntimeSnapshotResult, 1)
		req := snapshotRequest{ReplyCh: replyCh}

		select {
		case o.snapshotCh <- req:
		case <-time.After(5 * time.Second):
			return RuntimeSnapshotResult{}, fmt.Errorf("timed out sending snapshot request")
		}

		select {
		case snap := <-replyCh:
			return snap, nil
		case <-time.After(5 * time.Second):
			return RuntimeSnapshotResult{}, fmt.Errorf("timed out waiting for snapshot reply")
		}
	}
}

// RefreshFunc returns a function that signals the orchestrator to
// perform an immediate poll+reconciliation cycle. Returns true if the
// signal was accepted, false if it was coalesced (a refresh was
// already pending) or if the orchestrator is draining. The returned
// function is safe to call from any goroutine.
func (o *Orchestrator) RefreshFunc() func() bool {
	return func() bool {
		if o.draining.Load() {
			return false
		}
		select {
		case o.refreshCh <- struct{}{}:
			return true
		default:
			return false
		}
	}
}
