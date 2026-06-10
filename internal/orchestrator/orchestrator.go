// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
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
	SumTotalTokensByIssue(ctx context.Context, issueID string) (int64, int, error)
	QueryBudgetExhaustedIssues(ctx context.Context, candidateIDs []string, maxSessions int) ([]string, error)
	QueryTokenExhaustedIssues(ctx context.Context, candidateIDs []string, maxTokens int) ([]string, error)
	DeleteReactionFingerprintsByIssue(ctx context.Context, issueID string) error
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
}

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

	// AgentAdapterByKind resolves the agent adapter for the given
	// kind. Constructed once at startup from the eagerly-built
	// per-kind adapter cache. When nil, the orchestrator falls back
	// to its single-adapter behavior using the AgentAdapter field
	// for every kind that matches the workflow default and rejects
	// every other kind. This fallback exists for legacy callers
	// during the migration window; the production binary always
	// populates this field.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)
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

	workerExitCh chan WorkerResult
	retryTimerCh chan string
	agentEventCh chan agentEventMsg
	selfReviewCh chan selfReviewProgressMsg
	snapshotCh   chan snapshotRequest
	refreshCh    chan struct{}

	preflightParams             PreflightParams
	observers                   []Observer
	drainTimeout                time.Duration
	toolRegistry                *domain.ToolRegistry
	preflightOK                 atomic.Bool
	draining                    atomic.Bool
	hostPool                    *HostPool
	workflowFileFunc            func() string
	dbPath                      string
	ciProvider                  domain.CIStatusProvider
	scmAdapter                  domain.SCMAdapter
	reviewConfig                ReviewReactionConfig
	autoMergeConfig             AutoMergeReactionConfig
	autoMergeReactionConfigured bool

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
		if worker, ok := cfg.Extensions["worker"].(map[string]any); ok {
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

	o := &Orchestrator{
		state:                       params.State,
		logger:                      logger,
		trackerAdapter:              params.TrackerAdapter,
		agentAdapter:                params.AgentAdapter,
		agentAdapterByKind:          agentAdapterByKind,
		workflowManager:             params.WorkflowManager,
		store:                       params.Store,
		metrics:                     metrics,
		workerExitCh:                make(chan WorkerResult, exitBuf),
		retryTimerCh:                make(chan string, retryBuf),
		agentEventCh:                make(chan agentEventMsg, eventBuf),
		selfReviewCh:                make(chan selfReviewProgressMsg, eventBuf),
		snapshotCh:                  make(chan snapshotRequest, 4),
		refreshCh:                   make(chan struct{}, 1),
		preflightParams:             params.PreflightParams,
		observers:                   observers,
		drainTimeout:                defaultDrainTimeout,
		toolRegistry:                params.ToolRegistry,
		hostPool:                    hostPool,
		workflowFileFunc:            params.WorkflowFileFunc,
		dbPath:                      params.DBPath,
		ciProvider:                  params.CIProvider,
		scmAdapter:                  params.SCMAdapter,
		reviewConfig:                params.ReviewConfig,
		autoMergeConfig:             params.AutoMergeConfig,
		autoMergeReactionConfigured: params.AutoMergeReactionConfigured,
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
				Store:                       o.store,
				MaxRetryBackoffMS:           cfg.Agent.MaxRetryBackoffMS,
				OnRetryFire:                 o.onRetryFire,
				Ctx:                         ctx,
				Logger:                      o.logger,
				BeforeRemoveHook:            cfg.Hooks.BeforeRemove,
				HookTimeoutMS:               cfg.Hooks.TimeoutMS,
				TrackerAdapter:              o.trackerAdapter,
				HandoffState:                cfg.Tracker.HandoffState,
				ActiveStates:                cfg.Tracker.ActiveStates,
				Metrics:                     o.metrics,
				HostPool:                    o.hostPool,
				CommentsConfig:              cfg.Tracker.Comments,
				CIProvider:                  o.ciProvider,
				SCMAdapter:                  o.scmAdapter,
				AutoMergeReactionConfigured: o.autoMergeReactionConfigured,
			})
			o.updateGauges(time.Now())
			o.notifyObservers()

		case issueID := <-o.retryTimerCh:
			cfg := o.workflowManager.Config()
			HandleRetryTimer(o.state, issueID, HandleRetryTimerParams{
				Store:              o.store,
				TrackerAdapter:     o.trackerAdapter,
				ActiveStates:       cfg.Tracker.ActiveStates,
				TerminalStates:     cfg.Tracker.TerminalStates,
				HandoffState:       cfg.Tracker.HandoffState,
				MaxRetryBackoffMS:  cfg.Agent.MaxRetryBackoffMS,
				MakeWorkerFn:       o.makeWorkerFn,
				AgentAdapterByKind: o.agentAdapterByKind,
				DefaultAgentKind:   cfg.Agent.Kind,
				OnRetryFire:        o.onRetryFire,
				Ctx:                ctx,
				Logger:             o.logger,
				MaxSessions:        cfg.Agent.MaxSessions,
				MaxTokens:          cfg.Agent.MaxTokens,
				Metrics:            o.metrics,
				HostPool:           o.hostPool,
				WorkflowFile:       o.workflowFile(),
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
	wc := ParseWorkerConfig(cfg.Extensions)
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
		TrackerAdapter:              o.trackerAdapter,
		ActiveStates:                cfg.Tracker.ActiveStates,
		TerminalStates:              cfg.Tracker.TerminalStates,
		HandoffState:                cfg.Tracker.HandoffState,
		StallTimeoutMS:              cfg.Agent.StallTimeoutMS,
		MaxRetryBackoffMS:           cfg.Agent.MaxRetryBackoffMS,
		Store:                       o.store,
		OnRetryFire:                 o.onRetryFire,
		Ctx:                         ctx,
		Logger:                      o.logger,
		Metrics:                     o.metrics,
		CIProvider:                  o.ciProvider,
		CIFeedback:                  cfg.CIFeedback,
		CIPendingTTL:                ciPendingDefaultTTL,
		SCMAdapter:                  o.scmAdapter,
		ReviewConfig:                o.reviewConfig,
		ReviewPendingTTL:            reviewPendingDefaultTTL,
		AutoMergeConfig:             o.autoMergeConfig,
		AutoMergePendingTTL:         autoMergePendingDefaultTTL,
		AutoMergeReactionConfigured: o.autoMergeReactionConfigured,
	})

	// Sweep terminal workspaces periodically to catch issues that
	// transitioned after their worker exited.
	o.state.SweepTickCounter++
	if o.state.SweepTickCounter >= sweepEveryNTicks {
		o.state.SweepTickCounter = 0
		SweepTerminalWorkspaces(o.state, SweepTerminalWorkspacesParams{
			WorkspaceRoot:    cfg.Workspace.Root,
			TrackerAdapter:   o.trackerAdapter,
			TerminalStates:   cfg.Tracker.TerminalStates,
			BeforeRemoveHook: cfg.Hooks.BeforeRemove,
			HookTimeoutMS:    cfg.Hooks.TimeoutMS,
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

	// Pre-build state sets once for the dispatch loop.
	activeSet := stateSet(cfg.Tracker.ActiveStates)
	terminalSet := stateSet(cfg.Tracker.TerminalStates)

	// Break only when global capacity is exhausted; skip individual
	// issues whose per-state limit is full so issues in other states
	// can still be dispatched.
	var dispatched, dispatchedByRule, dispatchedByDefault, dispatchedByFallback int
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
		if !ShouldDispatchWithSets(issue, o.state, activeSet, terminalSet) {
			continue
		}

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
		DispatchIssue(ctx, o.state, issue, nil, host, o.makeWorkerFn("", host, resolution.AgentKind, resolution.TemplateID, adapter))
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

	o.logger.Info("tick completed",
		slog.Int("candidates", len(sorted)),
		slog.Int("dispatched", dispatched),
		slog.Int("dispatched_by_rule", dispatchedByRule),
		slog.Int("dispatched_by_default", dispatchedByDefault),
		slog.Int("dispatched_by_fallback", dispatchedByFallback),
		slog.Int("running", len(o.state.Running)),
		slog.Int("retrying", len(o.state.RetryAttempts)),
	)

	o.notifyObservers()
}

// makeWorkerFn returns a [WorkerFunc] closure that runs
// [RunWorkerAttempt] with the orchestrator's shared dependencies.
// The closure captures channel references for OnEvent and OnExit
// delivery. agentKind, templateID, and adapter carry the rule-resolved
// selection from the caller (handleTick for initial dispatches,
// HandleRetryTimer for retries). The resumeSessionID must be read by
// the caller (on the event loop goroutine) before the goroutine
// starts, to avoid a data race on the Running map.
func (o *Orchestrator) makeWorkerFn(resumeSessionID, sshHost, agentKind, templateID string, adapter domain.AgentAdapter) WorkerFunc {
	strictHostKeyChecking := o.sshStrictHostKeyChecking
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
			ResumeSessionID:          resumeSessionID,
			Logger:                   logger,
			ToolRegistry:             o.toolRegistry,
			SSHHost:                  sshHost,
			SSHStrictHostKeyChecking: strictHostKeyChecking,
			Metrics:                  o.metrics,
			WorkflowPath:             o.workflowManager.WorkflowAbsPath(),
			DBPath:                   o.dbPath,
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
// token totals to session_metadata when a token_usage event arrives and
// the per-issue throttle interval has elapsed. It is a no-op for other
// event types, for unknown issues, and while throttled. Must be called
// from the orchestrator's single-writer event loop so it shares the one
// SQLite writer and the running entry it mutates.
func (o *Orchestrator) maybeWriteIncrementalMetadata(ctx context.Context, issueID string, event domain.AgentEvent) {
	if event.Type != domain.EventTokenUsage {
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

// rebuildBudgetExhausted replaces the BudgetExhausted set and its reason
// map once per tick from run_history, as the union of the session-count
// and token-sum budgets scoped to the candidate set. An issue blocked on
// either budget is in the rebuilt set; an issue blocked on both reports
// the token budget. On a query error for one axis, the entire prior set
// is folded in for that axis so a transient error never drops an issue
// mid-tick. Must be called from the event loop goroutine.
func (o *Orchestrator) rebuildBudgetExhausted(ctx context.Context, cfg config.ServiceConfig, sorted []domain.Issue) {
	if cfg.Agent.MaxSessions == 0 && cfg.Agent.MaxTokens == 0 {
		o.state.BudgetExhausted = make(map[string]struct{})
		o.state.BudgetExhaustedReason = make(map[string]string)
		return
	}

	candidateIDs := make([]string, len(sorted))
	for i, issue := range sorted {
		candidateIDs[i] = issue.ID
	}

	prior := o.state.BudgetExhausted
	priorReason := o.state.BudgetExhaustedReason
	fresh := make(map[string]struct{})
	freshReason := make(map[string]string)

	if cfg.Agent.MaxSessions > 0 {
		sessionExhausted, qErr := o.store.QueryBudgetExhaustedIssues(ctx, candidateIDs, cfg.Agent.MaxSessions)
		if qErr != nil {
			o.logger.Warn("budget exhaustion query failed, retaining previous set",
				slog.Any("error", qErr),
			)
			for id := range prior {
				fresh[id] = struct{}{}
				freshReason[id] = priorReason[id]
			}
		} else {
			for _, id := range sessionExhausted {
				fresh[id] = struct{}{}
				freshReason[id] = budgetReasonSession
			}
		}
	}

	if cfg.Agent.MaxTokens > 0 {
		tokenExhausted, qErr := o.store.QueryTokenExhaustedIssues(ctx, candidateIDs, cfg.Agent.MaxTokens)
		if qErr != nil {
			o.logger.Warn("token budget exhaustion query failed, retaining previous set",
				slog.Any("error", qErr),
			)
			for id := range prior {
				fresh[id] = struct{}{}
				if _, ok := freshReason[id]; !ok {
					freshReason[id] = priorReason[id]
				}
			}
		} else {
			for _, id := range tokenExhausted {
				fresh[id] = struct{}{}
				freshReason[id] = budgetReasonToken
			}
		}
	}

	o.state.BudgetExhausted = fresh
	o.state.BudgetExhaustedReason = freshReason
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
				Store:                       o.store,
				MaxRetryBackoffMS:           cfg.Agent.MaxRetryBackoffMS,
				OnRetryFire:                 func(string) {}, // no-op: prevent retry fire events from reaching the event loop during drain
				Ctx:                         drainCtx,
				Logger:                      o.logger,
				BeforeRemoveHook:            cfg.Hooks.BeforeRemove,
				HookTimeoutMS:               cfg.Hooks.TimeoutMS,
				TrackerAdapter:              o.trackerAdapter,
				HandoffState:                cfg.Tracker.HandoffState,
				ActiveStates:                cfg.Tracker.ActiveStates,
				Metrics:                     o.metrics,
				HostPool:                    o.hostPool,
				CommentsConfig:              cfg.Tracker.Comments,
				CIProvider:                  o.ciProvider,
				SCMAdapter:                  o.scmAdapter,
				AutoMergeReactionConfigured: o.autoMergeReactionConfigured,
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
