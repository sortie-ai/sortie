// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
)

// WorkflowManager provides the current workflow config and prompt template.
// Satisfied by [workflow.Manager] in production.
type WorkflowManager interface {
	Config() config.ServiceConfig
	PromptTemplate() *prompt.Template
	PromptTemplateByID(id string) *prompt.Template
	Reload() error
	WorkflowAbsPath() string
}

// OrchestratorStore is the persistence interface required by the event
// loop. Satisfied by [persistence.Store].
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
	UpsertBudgetHoldNotice(ctx context.Context, notice persistence.BudgetHoldNotice) error
	DeleteBudgetHoldNotice(ctx context.Context, issueID string) error
	DeleteAllBudgetHoldNotices(ctx context.Context) error
	ListBudgetHoldNotices(ctx context.Context) ([]persistence.BudgetHoldNotice, error)
}

var _ OrchestratorStore = (*persistence.Store)(nil)

// Observer receives notifications when orchestrator state changes.
// Implementations must not block and must not mutate state.
type Observer interface {
	// OnStateChange is called after each event loop iteration that modifies
	// state (tick completion, worker exit, retry fire).
	OnStateChange()
}

// snapshotRequest is a request for a point-in-time snapshot, processed by
// the event loop, which replies on ReplyCh.
type snapshotRequest struct {
	ReplyCh chan<- RuntimeSnapshotResult
}

// agentEventMsg pairs an issue ID with an agent event for delivery through
// agentEventCh.
type agentEventMsg struct {
	IssueID string
	Event   domain.AgentEvent
}

// turnStartedMsg pairs an issue ID with the worker's started-turn count,
// the turn about to run included, for delivery through turnStartedCh.
type turnStartedMsg struct {
	IssueID      string
	TurnsStarted int
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

	// SessionToolRegistryFunc builds the per-session tool registry so the
	// first-turn advertisement matches the MCP sidecar's set. May be nil,
	// in which case the worker advertises from ToolRegistry.
	SessionToolRegistryFunc SessionToolRegistryFunc

	// WorkflowFileFunc returns the base filename of the active workflow file
	// (e.g. "WORKFLOW.md"), recorded on RunningEntry and in run_history. If
	// nil, defaults to empty.
	WorkflowFileFunc func() string

	// DBPath is the absolute SQLite database path, passed to the MCP server.
	DBPath string

	// MCPServerBinary is the absolute path to the sortie binary the runtime
	// spawns as the tool server. Empty resolves to the running executable,
	// correct when the runtime shares the orchestrator's host and
	// deployment. A caller running the orchestrator inside another program
	// must supply the real binary.
	MCPServerBinary string

	// CIProvider is the CI status provider for CI failure detection. Nil
	// when CI feedback is not configured.
	CIProvider domain.CIStatusProvider

	// SCMAdapter is the SCM adapter for review comment routing. Nil when
	// review comment routing is not configured.
	SCMAdapter domain.SCMAdapter

	// ReviewConfig holds validated review reaction configuration. Zero when
	// SCMAdapter is nil.
	ReviewConfig ReviewReactionConfig

	// AutoMergeConfig holds validated auto-merge configuration. Zero when
	// AutoMergeReactionConfigured is false.
	AutoMergeConfig AutoMergeReactionConfig

	AutoMergeReactionConfigured bool

	// BotReviewConfig holds validated bot-review configuration. Zero when
	// BotReviewConfigured is false.
	BotReviewConfig BotReviewReactionConfig

	BotReviewConfigured bool

	// MergeConflictConfig holds validated merge-conflict configuration. Zero
	// when MergeConflictReactionConfigured is false.
	MergeConflictConfig MergeConflictReactionConfig

	MergeConflictReactionConfigured bool

	// LabelReviewConfig holds validated label-review configuration. Zero
	// when LabelReviewReactionConfigured is false.
	LabelReviewConfig LabelReviewReactionConfig

	LabelReviewReactionConfigured bool

	// LabelFixConfig holds validated label-fix configuration. Zero when
	// LabelFixReactionConfigured is false.
	LabelFixConfig LabelFixReactionConfig

	LabelFixReactionConfigured bool

	// MergeCompletionConfig holds validated merge-completion configuration.
	// Zero when MergeCompletionReactionConfigured is false.
	MergeCompletionConfig MergeCompletionReactionConfig

	MergeCompletionReactionConfigured bool

	// AgentAdapterByKind resolves the agent adapter for the given kind,
	// built at startup from the per-kind adapter cache. When nil, the
	// orchestrator falls back to single-adapter behavior for the workflow
	// default kind and rejects every other kind; this fallback exists for
	// legacy callers during migration. The production binary always
	// populates this.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)

	// BlockerResolver completes a candidate's blocker list per its tracker
	// adapter's declared blocker source. Nil means no blocker read; for an
	// adapter that resolves blockers per issue, candidates then stay marked
	// unresolved and the gate holds them, since an unread list is never read
	// as empty. The production binary always populates this.
	BlockerResolver BlockerResolver

	// AbandonCh, once closed, ends every in-flight shutdown wait at once. A
	// nil channel means no abort is wired; a receive on nil blocks forever,
	// which is what an unwired abort needs. Read only during shutdown.
	AbandonCh <-chan struct{}
}

// Orchestrator owns the poll-and-dispatch event loop and all runtime state.
// Construct via [NewOrchestrator] and run with [Orchestrator.Run]. [Run]
// must be called from a single goroutine; external events arrive via
// channels.
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
	abandonCh          <-chan struct{}

	workerExitCh  chan WorkerResult
	retryTimerCh  chan string
	agentEventCh  chan agentEventMsg
	selfReviewCh  chan selfReviewProgressMsg
	turnStartedCh chan turnStartedMsg
	snapshotCh    chan snapshotRequest
	refreshCh     chan struct{}

	preflightParams PreflightParams
	observers       []Observer

	// drainTimeout overrides the worker-drain wait when positive. Zero
	// resolves to the ceiling [Orchestrator.drainRunningWorkers] derives
	// from the current configuration.
	drainTimeout                      time.Duration
	toolRegistry                      *domain.ToolRegistry
	sessionToolRegistryFunc           SessionToolRegistryFunc
	preflightOK                       atomic.Bool
	draining                          atomic.Bool
	hostPool                          *HostPool
	workflowFileFunc                  func() string
	dbPath                            string
	mcpServerBinary                   string
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

	// ciTriage is the frozen ci_failure triage configuration, captured once
	// at construction. Unlike every other CI feedback field, it does not
	// reach the reconcile pass from the reloaded config, so a script or
	// timeout changed mid-run takes effect only on restart.
	ciTriage config.ReactionTriageConfig

	// sshStrictHostKeyChecking is the current effective OpenSSH
	// StrictHostKeyChecking value. Written by applyWorkerConfig, read by
	// makeWorkerFn at dispatch.
	sshStrictHostKeyChecking string

	// sshPassEnv and sshDisallowPassEnv are the current effective
	// worker.ssh_pass_env and worker.ssh_disallow_pass_env lists. Written by
	// applyWorkerConfig, read by makeWorkerFn at dispatch.
	sshPassEnv         []string
	sshDisallowPassEnv []string

	prevWorkerWarnings []WorkerWarning
}

// NewOrchestrator creates an [Orchestrator] with all dependencies wired.
// Does not start the event loop; call [Orchestrator.Run] for that.
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
		// The pool here comes from OrchestratorParams.HostPool, which the
		// running binary leaves unset until Run applies the worker block
		// before its first tick, so a workflow that configures SSH still
		// looks local here; read the block rather than the pool.
		cfg := params.WorkflowManager.Config()
		if worker := cfg.ExtensionSection("worker"); worker != nil && len(ParseWorkerConfig(worker, cfg.ExtensionEnvRefPaths("worker")).SSHHosts) == 0 {
			if _, hasMax := worker["max_concurrent_agents_per_host"]; hasMax {
				logger.Warn("max_concurrent_agents_per_host has no effect without worker.ssh_hosts")
			}
			if _, hasPassEnv := worker["ssh_pass_env"]; hasPassEnv {
				logger.Warn("ssh_pass_env has no effect without worker.ssh_hosts")
			}
			if _, hasDisallowPassEnv := worker["ssh_disallow_pass_env"]; hasDisallowPassEnv {
				logger.Warn("ssh_disallow_pass_env has no effect without worker.ssh_hosts")
			}
		}
	}

	agentAdapterByKind := params.AgentAdapterByKind
	if agentAdapterByKind == nil {
		// Migration fallback for legacy callers (tests, dryrun): resolve the
		// workflow default kind to the single AgentAdapter field; every other
		// kind returns an error so dispatch skips the issue rather than
		// panicking. The production binary always wires AgentAdapterByKind.
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
	var ciTriage config.ReactionTriageConfig
	if params.WorkflowManager != nil {
		handoffParkingLabel = resolveHandoffParkingLabel(params.WorkflowManager.Config().Reactions)
		ciTriage = params.WorkflowManager.Config().CIFeedback.Triage
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
		turnStartedCh:                     make(chan turnStartedMsg, eventBuf),
		snapshotCh:                        make(chan snapshotRequest, 4),
		refreshCh:                         make(chan struct{}, 1),
		preflightParams:                   params.PreflightParams,
		observers:                         observers,
		toolRegistry:                      params.ToolRegistry,
		sessionToolRegistryFunc:           params.SessionToolRegistryFunc,
		hostPool:                          hostPool,
		workflowFileFunc:                  params.WorkflowFileFunc,
		dbPath:                            params.DBPath,
		mcpServerBinary:                   params.MCPServerBinary,
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
		ciTriage:                          ciTriage,
		blockerResolver:                   params.BlockerResolver,
		abandonCh:                         params.AbandonCh,
	}
	// Startup preflight must have passed for construction to reach here.
	o.preflightOK.Store(true)
	return o
}

// applyQueued calls apply for each message ch holds, in order, without
// blocking, stopping at an empty receive or after cap(ch) messages.
func applyQueued[T any](ch <-chan T, apply func(T)) {
	for range cap(ch) {
		select {
		case msg := <-ch:
			apply(msg)
		default:
			return
		}
	}
}

// applyAgentEvent applies one agent event to its issue's running entry and,
// only when enforceCeiling is true, evaluates the in-flight token ceiling.
func (o *Orchestrator) applyAgentEvent(ctx context.Context, msg agentEventMsg, enforceCeiling bool) {
	HandleAgentEvent(o.state, msg.IssueID, msg.Event, o.logger, o.metrics)
	if enforceCeiling {
		evaluateTokenWarning(o.state, msg.IssueID, msg.Event, o.logger)
	}
	o.maybeWriteIncrementalMetadata(ctx, msg.IssueID, msg.Event)
	if enforceCeiling {
		enforceInFlightTokenCeiling(ctx, o.state, msg.IssueID, msg.Event, o.store, o.logger)
	}
}

// applySelfReviewProgress applies one self-review progress message to its
// issue's running entry; a no-op when the issue has none.
func (o *Orchestrator) applySelfReviewProgress(msg selfReviewProgressMsg) {
	entry, ok := o.state.Running[msg.IssueID]
	if !ok {
		return
	}
	if msg.Message == "self_review_done" {
		entry.SelfReviewActive = false
		entry.SelfReviewIteration = 0
	} else {
		entry.SelfReviewActive = true
		entry.SelfReviewIteration = msg.Iteration
	}
}

func (o *Orchestrator) applyTurnStarted(msg turnStartedMsg) {
	entry, ok := o.state.Running[msg.IssueID]
	if !ok {
		return
	}
	entry.TurnCount = msg.TurnsStarted
}

// applyQueuedAheadOfExit applies the messages queued ahead of the
// WorkerResults of exitingIssueIDs, evaluating the token ceiling for every
// applied event except those of an exiting issue: that run has ended, so a
// figure it delivered can no longer be stopped in flight.
func (o *Orchestrator) applyQueuedAheadOfExit(ctx context.Context, exitingIssueIDs map[string]struct{}) {
	applyQueued(o.agentEventCh, func(msg agentEventMsg) {
		_, exiting := exitingIssueIDs[msg.IssueID]
		o.applyAgentEvent(ctx, msg, !exiting)
	})
	applyQueued(o.selfReviewCh, o.applySelfReviewProgress)
	applyQueued(o.turnStartedCh, o.applyTurnStarted)
}

// handleWorkerExit takes workerExit and every WorkerResult already waiting
// behind it, applies the messages queued ahead of all of them so each lands
// on its own run and no finished run is stopped by the ceiling, then hands
// each result to HandleWorkerExit in arrival order.
func (o *Orchestrator) handleWorkerExit(ctx context.Context, workerExit WorkerResult) {
	exits := []WorkerResult{workerExit}
	applyQueued(o.workerExitCh, func(pending WorkerResult) {
		exits = append(exits, pending)
	})
	exitingIssueIDs := make(map[string]struct{}, len(exits))
	for _, result := range exits {
		exitingIssueIDs[result.IssueID] = struct{}{}
	}
	o.applyQueuedAheadOfExit(ctx, exitingIssueIDs)

	cfg := o.workflowManager.Config()
	for _, result := range exits {
		HandleWorkerExit(o.state, result, HandleWorkerExitParams{
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
			NoChangeState:                     cfg.Tracker.NoChangeState,
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
	}
	o.updateGauges(time.Now())
	o.notifyObservers()
}

// Run enters the event loop, blocks until ctx is cancelled, and returns.
// Must be called from a single goroutine. On cancellation it cancels all
// running worker contexts, drains workers (up to the drain timeout, 30s by
// default) processing their results and agent events, stops pending retry
// timers, and returns.
func (o *Orchestrator) Run(ctx context.Context) {
	// A past-due recovered retry can dispatch on the first pass, ahead of
	// the first tick, so the worker settings it launches under must be in
	// force before it is activated.
	o.applyWorkerConfig(o.workflowManager.Config())
	o.activateReconstructedRetries()

	tickTimer := time.NewTimer(0)
	defer tickTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			o.draining.Store(true)
			tickTimer.Stop()
			o.drainRunningWorkers()
			o.drainTriageRuns()
			o.drainTrackerOps()
			o.cancelRetryTimers()
			return

		case <-tickTimer.C:
			o.handleTick(ctx)
			tickTimer.Reset(time.Duration(o.state.PollIntervalMS) * time.Millisecond)

		case workerExit := <-o.workerExitCh:
			o.handleWorkerExit(ctx, workerExit)

		case issueID := <-o.retryTimerCh:
			cfg := o.workflowManager.Config()
			HandleRetryTimer(o.state, issueID, HandleRetryTimerParams{
				Store:                   o.store,
				TrackerAdapter:          o.trackerAdapter,
				ActiveStates:            cfg.Tracker.ActiveStates,
				TerminalStates:          cfg.Tracker.TerminalStates,
				HandoffState:            cfg.Tracker.HandoffState,
				MaxRetryBackoffMS:       cfg.Agent.MaxRetryBackoffMS,
				MakeWorkerFn:            o.makeWorkerFn,
				AgentAdapterByKind:      o.agentAdapterByKind,
				ResolveUsageDisposition: o.resolveUsageDisposition,
				DefaultAgentKind:        cfg.Agent.Kind,
				OnRetryFire:             o.onRetryFire,
				Ctx:                     ctx,
				Logger:                  o.logger,
				MaxSessions:             cfg.Agent.MaxSessions,
				MaxConsecutiveAbsences:  cfg.Agent.MaxConsecutiveAbsences,
				HandoffParkingLabel:     o.handoffParkingLabel,
				HandoffEvidencePolicy:   cfg.Tracker.HandoffEvidence,
				MaxTokens:               cfg.Agent.MaxTokens,
				Metrics:                 o.metrics,
				HostPool:                o.hostPool,
				WorkflowFile:            o.workflowFile(),
			})
			o.updateGauges(time.Now())
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			o.applyAgentEvent(ctx, msg, true)

		case msg := <-o.selfReviewCh:
			o.applySelfReviewProgress(msg)

		case msg := <-o.turnStartedCh:
			o.applyTurnStarted(msg)

		case req := <-o.snapshotCh:
			snap := RuntimeSnapshot(o.state, time.Now())
			req.ReplyCh <- snap

		case <-o.refreshCh:
			o.handleTick(ctx)
		}
	}
}

// updateGauges recomputes point-in-time gauges from current state and
// publishes them. Called from the event loop after tick handling, worker
// exits, and retry timer events.
func (o *Orchestrator) updateGauges(now time.Time) {
	o.metrics.SetRunningSessions(len(o.state.Running))
	o.metrics.SetRetryingSessions(len(o.state.RetryAttempts))
	o.metrics.SetAvailableSlots(GlobalAvailableSlots(o.state.MaxConcurrentAgents, len(o.state.Running)))
	o.metrics.SetActiveSessionsElapsed(ActiveElapsedSeconds(o.state, now))

	// Emit from the full usage map so hosts removed by reload but still
	// holding in-flight workers decrement to zero on exit rather than
	// freezing at their last value.
	for host, count := range o.hostPool.Snapshot() {
		o.metrics.SetSSHHostUsage(host, count)
	}

	// Emit every declared budget reason, so a reason that clears reports
	// zero rather than freezing at its last value.
	budgetCounts := make(map[string]int, len(knownBudgetReasons))
	for _, entry := range o.state.BudgetExhausted {
		budgetCounts[entry.Reason]++
	}
	for _, reason := range knownBudgetReasons {
		o.metrics.SetBudgetExhaustedIssues(reason, budgetCounts[reason])
	}
}

// applyWorkerConfig parses the worker extension section and applies it to
// the host pool and the SSH launch fields makeWorkerFn reads, returning the
// parsing diagnostics. Run applies it once before activating recovered
// retries; handleTick applies it every tick so a reload takes effect.
func (o *Orchestrator) applyWorkerConfig(cfg config.ServiceConfig) []WorkerWarning {
	wc := ParseWorkerConfig(cfg.ExtensionSection("worker"), cfg.ExtensionEnvRefPaths("worker"))
	o.hostPool.Update(wc.SSHHosts, wc.MaxPerHost)
	o.sshStrictHostKeyChecking = wc.SSHStrictHostKeyChecking
	o.sshPassEnv = wc.SSHPassEnv
	o.sshDisallowPassEnv = wc.SSHDisallowPassEnv
	return wc.Warnings
}

// handleTick executes a single poll-and-dispatch cycle: preflight, config
// read, reconcile, fetch, sort, dispatch.
//
// Preflight runs first so a config reload is visible downstream.
// Reconciliation and state-field updates always run, even when preflight
// fails, keeping state aligned with the tracker using last-known-good
// config. Dispatch is the only step gated on preflight success.
func (o *Orchestrator) handleTick(ctx context.Context) {
	tickStart := time.Now()
	pollResult := outcomeSuccess
	defer func() {
		o.metrics.IncPollCycles(pollResult)
		o.metrics.ObservePollDuration(time.Since(tickStart).Seconds())
		o.updateGauges(time.Now())
	}()

	// Preflight triggers a defensive Reload() so the config snapshot below
	// reflects the latest disk state.
	validation := ValidateDispatchConfig(o.preflightParams)
	o.preflightOK.Store(validation.OK())

	// On reload failure the workflow manager retains last-known-good config.
	cfg := o.workflowManager.Config()

	// Applied unconditionally, not gated on preflight success.
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	o.state.MaxTokens = cfg.Agent.MaxTokens
	o.state.TokenWarningThreshold = cfg.Agent.TokenWarningThreshold()
	o.state.MaxConcurrentByState = cfg.Agent.MaxConcurrentByState

	warnings := o.applyWorkerConfig(cfg)

	if !workerWarningsEqual(o.prevWorkerWarnings, warnings) {
		for _, w := range warnings {
			o.logger.LogAttrs(ctx, slog.LevelWarn, w.Message, w.Attrs...) //nolint:sloglint // WorkerWarning.Message comes from one of a fixed set of string constants ParseWorkerConfig produces
		}
		o.prevWorkerWarnings = warnings
	}

	// Reconcile unconditionally so in-flight workers are monitored even when
	// dispatch is skipped.
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
		ReviewPendingTTL:                  time.Duration(o.reviewConfig.WatchWindowMS) * time.Millisecond,
		AutoMergeConfig:                   o.autoMergeConfig,
		AutoMergePendingTTL:               time.Duration(o.autoMergeConfig.WatchWindowMS) * time.Millisecond,
		AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
		BotReviewConfig:                   o.botReviewConfig,
		BotReviewPendingTTL:               time.Duration(o.botReviewConfig.WatchWindowMS) * time.Millisecond,
		BotReviewConfigured:               o.botReviewReactionConfigured,
		MergeConflictConfig:               o.mergeConflictConfig,
		MergeConflictPendingTTL:           time.Duration(o.mergeConflictConfig.WatchWindowMS) * time.Millisecond,
		MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
		LabelReviewConfig:                 o.labelReviewConfig,
		LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
		LabelFixConfig:                    o.labelFixConfig,
		LabelFixReactionConfigured:        o.labelFixReactionConfigured,
		MergeCompletionConfig:             o.mergeCompletionConfig,
		MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
		WorkspaceRoot:                     cfg.Workspace.Root,
		CITriage:                          o.ciTriage,
	})

	// Sweep workspaces periodically to catch issues that transitioned after
	// their worker exited, or aged past the retention window.
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

	// On preflight failure, skip dispatch but still notify observers.
	if !validation.OK() {
		pollResult = outcomeError
		o.logger.Error("dispatch preflight failed",
			slog.Any("error", validation),
		)
		o.notifyObservers()
		return
	}

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

	activeSet := stateSet(cfg.Tracker.ActiveStates)
	terminalSet := stateSet(cfg.Tracker.TerminalStates)

	// Break only on exhausted global capacity; skip individual issues whose
	// per-state limit is full so other states can still dispatch.
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
		arrival, attribution := o.resolveUsageDisposition(resolution.AgentKind, host)
		DispatchIssue(ctx, o.state, issue, nil, host, o.makeWorkerFn("", host, resolution.AgentKind, resolution.TemplateID, "", adapter, arrival))
		if entry := o.state.Running[issue.ID]; entry != nil {
			entry.WorkflowFile = o.workflowFile()
			entry.AgentKind = resolution.AgentKind
			entry.RuleName = resolution.RuleName
			entry.TemplateID = resolution.TemplateID
			entry.UsageArrival, entry.UsageAttribution = arrival, attribution
			freezeIssueTokenBaseline(ctx, o.state, issue.ID, o.store, o.logger)
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

// recordCandidateHold logs the per-issue record for a held candidate and
// increments [IncCandidateHolds] for every reason except [SkipIneligible],
// which produces neither record nor counter.
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

// makeWorkerFn returns a [WorkerFunc] closure running [RunWorkerAttempt]
// with the orchestrator's shared dependencies. agentKind, templateID, and
// adapter carry the rule-resolved selection from the caller; usageArrival
// must be the value the caller freezes onto the running entry; reactionKind
// selects the worker posture. resumeSessionID must be read by the caller on
// the event loop before the goroutine starts, to avoid a Running-map race.
func (o *Orchestrator) makeWorkerFn(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter, usageArrival registry.UsageArrival) WorkerFunc {
	strictHostKeyChecking := o.sshStrictHostKeyChecking
	sshPassEnv := o.sshPassEnv
	sshDisallowPassEnv := o.sshDisallowPassEnv
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
			UsageArrival:           usageArrival,
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
			OnTurnStarted: func(issueID string, turnsStarted int) {
				select {
				case o.turnStartedCh <- turnStartedMsg{IssueID: issueID, TurnsStarted: turnsStarted}:
				case <-ctx.Done():
				}
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
			DispatchID:              dispatchIDFromContext(ctx),
			Logger:                  logger,
			ToolRegistry:            o.toolRegistry,
			SessionToolRegistryFunc: o.sessionToolRegistryFunc,
			AgentToolChannelFunc: func(kind string, remote bool) bool {
				meta, _ := o.preflightParams.AgentRegistry.Meta(kind)
				return meta.MCPInjection.DeliversTools(remote)
			},
			SSHHost:                  sshHost,
			SSHStrictHostKeyChecking: strictHostKeyChecking,
			SSHEnvNamesFunc: func(kind string) []string {
				meta, _ := o.preflightParams.AgentRegistry.Meta(kind)
				return carriedEnvNames(meta.CredentialEnv.Names(), sshPassEnv, sshDisallowPassEnv)
			},
			Metrics:         o.metrics,
			WorkflowPath:    o.workflowManager.WorkflowAbsPath(),
			DBPath:          o.dbPath,
			MCPServerBinary: o.mcpServerBinary,
			Posture:         posture,
		}

		RunWorkerAttempt(ctx, issue, attempt, deps)
	}
}

// workflowFile returns the base filename of the active workflow file, or
// empty when no callback is configured.
func (o *Orchestrator) workflowFile() string {
	if o.workflowFileFunc != nil {
		return o.workflowFileFunc()
	}
	return ""
}

// resolveUsageDisposition resolves the usage-reporting disposition for a
// session of the given kind and SSH host (empty for local), reading the
// registered kind's declaration and the passthrough config in force. An
// unknown kind returns the undeclared pair.
func (o *Orchestrator) resolveUsageDisposition(kind, sshHost string) (registry.UsageArrival, registry.UsageAttribution) {
	meta, registered := o.preflightParams.AgentRegistry.Meta(kind)
	if !registered {
		return registry.UsageArrivalUndeclared, registry.UsageAttributionUndeclared
	}
	settings := config.ResolveAgentSettings(o.workflowManager.Config(), kind, filepath.Dir(o.workflowManager.WorkflowAbsPath()))
	return meta.UsageDisposition(settings.Passthrough, sshHost != "")
}

// onRetryFire delivers a retry timer event to the event loop channel, using
// a non-blocking send to avoid deadlock when the buffer is full.
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

// activateReconstructedRetries starts timers for retry entries populated by
// [PopulateRetries] during startup recovery (TimerHandle == nil).
// scheduledDelayMS > 0 gets a [time.AfterFunc]; past-due entries are written
// directly to retryTimerCh, relying on the channel buffer sizing to tolerate
// immediate-fire entries written before the loop drains it.
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

// drainExitMargin is the budget for the post-stop teardown bookkeeping
// [HandleWorkerExit] performs after StopSession returns (run-history write,
// workspace release, and the rest).
const drainExitMargin = 30 * time.Second

// sessionMetadataWriteInterval bounds how often the event loop writes an
// in-flight session's token totals when no write is owed: at most one
// incremental write per issue per interval, so the advisory cost reading
// trails live spend by at most one interval plus whatever accrued since
// the last token_usage event. A pending write, set when a usage figure
// reaches [State.TokenWarningThreshold], bypasses this bound.
const sessionMetadataWriteInterval = 2 * time.Second

// maybeWriteIncrementalMetadata persists a running session's token totals
// when an event carries non-zero usage, or a token_usage event carries a
// measurement of zero, and either a write is owed or the per-issue
// throttle has elapsed. Widening the gate to the token_usage type makes a
// row exist exactly when the session has reported a measurement,
// including a zero. A no-op for an event with neither signal, an unknown
// issue, an arrival of none, or while throttled with no write owed. Must
// run on the single-writer event loop.
func (o *Orchestrator) maybeWriteIncrementalMetadata(ctx context.Context, issueID string, event domain.AgentEvent) {
	if !hasUsage(event.Usage) && event.Type != domain.EventTokenUsage {
		return
	}
	entry := o.state.Running[issueID]
	if entry == nil {
		return
	}
	if !admitsUsageFigures(entry.UsageArrival) {
		return
	}
	now := time.Now().UTC()
	if !entry.MetadataWritePending && !entry.LastMetadataWrite.IsZero() && now.Sub(entry.LastMetadataWrite) < sessionMetadataWriteInterval {
		return
	}

	// An unmeasured count is stored as zero so a reader cannot find a figure
	// contradicting the qualifier beside it.
	requestsMeasured := apiRequestsMeasured(entry.UsageArrival, entry.TurnCount, entry.APIRequestCount)
	requestCount := 0
	if requestsMeasured {
		requestCount = entry.APIRequestCount
	}
	meta := persistence.SessionMetadata{
		IssueID:             issueID,
		SessionID:           entry.SessionID,
		DispatchID:          entry.DispatchID,
		InputTokens:         entry.AgentInputTokens,
		OutputTokens:        entry.AgentOutputTokens,
		TotalTokens:         entry.AgentTotalTokens,
		CacheReadTokens:     entry.CacheReadTokens,
		ModelName:           entry.ModelName,
		APIRequestCount:     requestCount,
		APIRequestsMeasured: requestsMeasured,
		UpdatedAt:           now.Format(time.RFC3339),
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
	entry.MetadataWritePending = false
}

// refreshParkedIssues evaluates the release rule against this tick's
// candidates, then against every parked issue the candidate slice omits,
// read through one batched, comment-free tracker call. Must run on the
// event loop after the budget rebuild and before the dispatch loop.
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
// handoff-absence count has just reached the ceiling. Skipped under the off
// policy, which records no absence. Must run on the event loop after
// [Orchestrator.refreshParkedIssues] so a release this tick is not
// immediately re-parked.
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

// ceilingSettingByBudgetReason maps a budget-hold reason to the dotted
// config path of its governing setting, for the hold log record. A reason
// absent from this map emits no ceiling_setting attribute.
var ceilingSettingByBudgetReason = map[string]string{
	budgetReasonSession: "agent.max_sessions",
	budgetReasonToken:   "agent.max_tokens",
}

// rebuildBudgetExhausted replaces the BudgetExhausted set once per tick from
// run_history, as the union of the session-count and token-sum gates scoped
// to the candidate set. Token budget takes precedence. On a query error for
// one axis, the prior entries for that axis are folded back in so a
// transient error never drops an issue mid-tick. An issue entering the set
// for the first time under a given reason produces one log record and one
// counter increment; a hold the memory already knows produces neither. Must
// run on the event loop.
func (o *Orchestrator) rebuildBudgetExhausted(ctx context.Context, cfg config.ServiceConfig, sorted []domain.Issue) {
	if cfg.Agent.MaxSessions == 0 && cfg.Agent.MaxTokens == 0 {
		o.state.BudgetExhausted = make(map[string]*BudgetExhaustedEntry)
		o.state.TokenBudgetIncomplete = make(map[string]struct{})
		releaseAllBudgetHoldNotices(ctx, o.state, o.store, o.logger)
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

	// foldedForward tracks IDs carried forward from the prior set by an
	// axis's query-error branch, so the notice pass can skip them: a hold
	// whose evidence was not read this tick may already have cleared, and a
	// comment is not retractable.
	foldedForward := make(map[string]struct{})

	// budgetEvidenceComplete records that every configured axis was read
	// this tick. When an axis query fails, absence from the fresh set is not
	// evidence that a hold cleared, so the notice release below is withheld:
	// releasing it would drop the durable dedup record and let a later read
	// post a second comment for a hold that never ended.
	budgetEvidenceComplete := true

	if cfg.Agent.MaxSessions > 0 {
		counts, qErr := o.store.QueryBudgetExhaustedIssues(ctx, candidateIDs, cfg.Agent.MaxSessions)
		if qErr != nil {
			o.logger.Warn("budget exhaustion query failed, retaining previous set",
				slog.Any("error", qErr),
			)
			budgetEvidenceComplete = false
			for id, entry := range prior {
				if entry.Reason != budgetReasonSession {
					continue
				}
				fresh[id] = entry
				foldedForward[id] = struct{}{}
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
			budgetEvidenceComplete = false
			for id, entry := range prior {
				if entry.Reason != budgetReasonToken {
					continue
				}
				fresh[id] = entry
				foldedForward[id] = struct{}{}
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
						StoppedInFlight:    &usage.StoppedInFlight,
					}
					continue
				}
				if entry, held := fresh[id]; held {
					entry.UsedTokens = &usage.TotalTokens
					entry.UnmeasuredSessions = &usage.UnmeasuredSessions
					entry.StoppedInFlight = &usage.StoppedInFlight
				}
				if !usage.SpendIncomplete() {
					continue
				}
				freshIncomplete[id] = struct{}{}
				if _, wasIncomplete := o.state.TokenBudgetIncomplete[id]; wasIncomplete {
					continue
				}
				issueLog := logging.WithIssue(o.logger, id, identifierByID[id])
				warnTokenBudgetIncomplete(issueLog, usage, cfg.Agent.MaxTokens)
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

	if o.trackerAdapter != nil {
		noticeIDs := make([]string, 0, len(fresh))
		for id := range fresh {
			noticeIDs = append(noticeIDs, id)
		}
		slices.SortFunc(noticeIDs, func(a, b string) int {
			if c := cmp.Compare(fresh[a].Identifier, fresh[b].Identifier); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})

		for _, id := range noticeIDs {
			if _, folded := foldedForward[id]; folded {
				continue
			}
			if o.state.BudgetHoldNoticed[id] == fresh[id].Reason {
				continue
			}
			if !budgetHoldNoticeAllowed(o.state, now) {
				break
			}
			postBudgetHoldNotice(o.state, budgetHoldNoticeParams{
				IssueID:        id,
				Entry:          fresh[id],
				Store:          o.store,
				TrackerAdapter: o.trackerAdapter,
				Metrics:        o.metrics,
				Logger:         logging.WithIssue(o.logger, id, fresh[id].Identifier),
				Ctx:            ctx,
			})
		}
	}

	for _, id := range candidateIDs {
		if _, held := fresh[id]; !held {
			delete(o.state.BudgetAnnounced, id)
			if budgetEvidenceComplete {
				releaseBudgetHoldNotice(ctx, o.state, o.store, id, o.logger)
			}
		}
	}

	o.state.BudgetExhausted = fresh
}

// drainRunningWorkers cancels all running worker contexts and waits for
// them to exit, processing each result and agent event so token usage and
// persistence are captured, and notifying observers after each exit.
// Returns when all workers have exited or the drain timeout expires.
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

	cfg := o.workflowManager.Config()
	ceiling := stopSessionDeadline(cfg) + drainExitMargin
	waitFor := o.drainTimeout
	if waitFor <= 0 {
		waitFor = ceiling
	}
	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()

	// The parent ctx is already cancelled; SQLite writes need a live one.
	drainCtx := context.Background()

	for len(o.state.Running) > 0 {
		select {
		case workerExit := <-o.workerExitCh:
			applyQueued(o.agentEventCh, func(msg agentEventMsg) {
				o.applyAgentEvent(drainCtx, msg, false)
			})
			applyQueued(o.selfReviewCh, o.applySelfReviewProgress)
			applyQueued(o.turnStartedCh, o.applyTurnStarted)
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
				NoChangeState:                     cfg.Tracker.NoChangeState,
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
			o.applyAgentEvent(drainCtx, msg, false)

		case msg := <-o.selfReviewCh:
			o.applySelfReviewProgress(msg)

		case msg := <-o.turnStartedCh:
			o.applyTurnStarted(msg)

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

		case <-o.abandonCh:
			o.logger.Warn("worker drain abandoned at the operator's request",
				slog.Int("remaining", len(o.state.Running)),
			)
			return
		}
	}

}

// trackerOpsDrainTimeout bounds how long shutdown waits for in-flight
// tracker API goroutines, set slightly above their own 30-second context
// timeout.
const trackerOpsDrainTimeout = 35 * time.Second

// drainTrackerOps waits, bounded, for all in-flight fire-and-forget tracker
// API goroutines to complete so a stuck adapter cannot block process exit.
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
	case <-o.abandonCh:
		o.logger.Warn("tracker ops drain abandoned at the operator's request")
	}
}

// drainTriageRuns waits, bounded, for all in-flight triage goroutines.
// Context cancellation already reached the subprocesses through the hook
// runner (which kills each process group), so the wait terminates promptly;
// the bound guards against a script that ignores its kill.
func (o *Orchestrator) drainTriageRuns() {
	done := make(chan struct{})
	go func() {
		o.state.TriageWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(trackerOpsDrainTimeout):
		o.logger.Warn("reaction triage drain timeout exceeded, abandoning in-flight runs")
	case <-o.abandonCh:
		o.logger.Warn("reaction triage drain abandoned at the operator's request")
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

// notifyObservers calls [Observer.OnStateChange] on each observer. Called
// after tick, worker exit, and retry timer handling, not after agent events.
func (o *Orchestrator) notifyObservers() {
	for _, obs := range o.observers {
		obs.OnStateChange()
	}
}

// AddObserver appends an observer. Must be called before [Orchestrator.Run]
// or between event loop iterations, never concurrently with the loop.
func (o *Orchestrator) AddObserver(obs Observer) {
	o.observers = append(o.observers, obs)
}

// PreflightOK reports whether the most recent dispatch preflight passed.
// Safe to call from any goroutine.
func (o *Orchestrator) PreflightOK() bool {
	return o.preflightOK.Load()
}

// SnapshotFunc returns a function that retrieves a point-in-time runtime
// snapshot via the event loop channel. Safe to call from any goroutine; it
// blocks until the snapshot arrives or a 5-second timeout expires.
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

// RefreshFunc returns a function that signals an immediate
// poll+reconciliation cycle. Returns true if accepted, false if coalesced
// (a refresh was already pending) or draining. Safe to call from any
// goroutine.
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
