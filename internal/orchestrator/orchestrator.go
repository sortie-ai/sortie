package orchestrator

import (
	"context"
	"log/slog"
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
	Reload() error
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
}

// Observer receives notifications when orchestrator state changes.
// Implementations must not block and must not mutate state.
type Observer interface {
	// OnStateChange is called after each event loop iteration that
	// modifies state (tick completion, worker exit, retry fire).
	OnStateChange()
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
	Observers       []Observer // may be nil/empty
}

// Orchestrator owns the poll-and-dispatch event loop and all runtime
// state. Construct via [NewOrchestrator] and run with [Orchestrator.Run].
// Not safe for concurrent use — [Run] must be called from a single
// goroutine. External events are delivered via channels.
type Orchestrator struct {
	state  *State
	logger *slog.Logger

	trackerAdapter  domain.TrackerAdapter
	agentAdapter    domain.AgentAdapter
	workflowManager WorkflowManager
	store           OrchestratorStore

	workerExitCh chan WorkerResult
	retryTimerCh chan string
	agentEventCh chan agentEventMsg

	preflightParams PreflightParams
	observers       []Observer
	drainTimeout    time.Duration
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

	maxConc := params.State.MaxConcurrentAgents
	exitBuf := max(maxConc*2, 64)
	retryBuf := max(maxConc*2, 64, len(params.State.RetryAttempts))
	eventBuf := max(maxConc*16, 256)

	return &Orchestrator{
		state:           params.State,
		logger:          logger,
		trackerAdapter:  params.TrackerAdapter,
		agentAdapter:    params.AgentAdapter,
		workflowManager: params.WorkflowManager,
		store:           params.Store,
		workerExitCh:    make(chan WorkerResult, exitBuf),
		retryTimerCh:    make(chan string, retryBuf),
		agentEventCh:    make(chan agentEventMsg, eventBuf),
		preflightParams: params.PreflightParams,
		observers:       observers,
		drainTimeout:    defaultDrainTimeout,
	}
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
			tickTimer.Stop()
			o.drainRunningWorkers()
			o.cancelRetryTimers()
			return

		case <-tickTimer.C:
			o.handleTick(ctx)
			tickTimer.Reset(time.Duration(o.state.PollIntervalMS) * time.Millisecond)

		case result := <-o.workerExitCh:
			cfg := o.workflowManager.Config()
			HandleWorkerExit(o.state, result, HandleWorkerExitParams{
				Store:             o.store,
				MaxRetryBackoffMS: cfg.Agent.MaxRetryBackoffMS,
				OnRetryFire:       o.onRetryFire,
				Ctx:               ctx,
				Logger:            o.logger,
				BeforeRemoveHook:  cfg.Hooks.BeforeRemove,
				HookTimeoutMS:     cfg.Hooks.TimeoutMS,
				TrackerAdapter:    o.trackerAdapter,
				HandoffState:      cfg.Tracker.HandoffState,
				ActiveStates:      cfg.Tracker.ActiveStates,
			})
			o.notifyObservers()

		case issueID := <-o.retryTimerCh:
			cfg := o.workflowManager.Config()
			HandleRetryTimer(o.state, issueID, HandleRetryTimerParams{
				Store:             o.store,
				TrackerAdapter:    o.trackerAdapter,
				ActiveStates:      cfg.Tracker.ActiveStates,
				TerminalStates:    cfg.Tracker.TerminalStates,
				MaxRetryBackoffMS: cfg.Agent.MaxRetryBackoffMS,
				WorkerFn:          o.makeWorkerFn(""),
				OnRetryFire:       o.onRetryFire,
				Ctx:               ctx,
				Logger:            o.logger,
				MaxSessions:       cfg.Agent.MaxSessions,
			})
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			HandleAgentEvent(o.state, msg.IssueID, msg.Event)
		}
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
	// Step 1: dispatch preflight validation. This triggers a
	// defensive Reload() of the workflow file, ensuring the config
	// snapshot returned by Config() below reflects the latest disk
	// state.
	validation := ValidateDispatchConfig(o.preflightParams)

	// Step 2: read fresh config unconditionally. On reload failure
	// the workflow manager retains last-known-good config, so
	// Config() always returns a usable snapshot.
	cfg := o.workflowManager.Config()

	// Step 3: apply config to state (unconditional — not gated on
	// preflight success).
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	o.state.MaxConcurrentByState = cfg.Agent.MaxConcurrentByState

	// Step 4: reconcile running issues with fresh config. Runs
	// unconditionally so in-flight workers are monitored even when
	// dispatch is skipped.
	ReconcileRunningIssues(o.state, ReconcileParams{
		TrackerAdapter:    o.trackerAdapter,
		ActiveStates:      cfg.Tracker.ActiveStates,
		TerminalStates:    cfg.Tracker.TerminalStates,
		StallTimeoutMS:    cfg.Agent.StallTimeoutMS,
		MaxRetryBackoffMS: cfg.Agent.MaxRetryBackoffMS,
		Store:             o.store,
		OnRetryFire:       o.onRetryFire,
		Ctx:               ctx,
		Logger:            o.logger,
	})

	// Step 5: if preflight failed, skip dispatch but still notify
	// observers so the UI reflects the reconciliation outcome.
	if !validation.OK() {
		o.logger.Error("dispatch preflight failed",
			slog.String("error", validation.Error()),
		)
		o.notifyObservers()
		return
	}

	// Step 6: fetch candidate issues.
	issues, err := o.trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Error("failed to fetch candidate issues",
			slog.Any("error", err),
		)
		o.notifyObservers()
		return
	}

	// Step 7: sort for dispatch.
	sorted := SortForDispatch(issues)

	// Step 8: pre-build state sets once for the dispatch loop.
	activeSet := stateSet(cfg.Tracker.ActiveStates)
	terminalSet := stateSet(cfg.Tracker.TerminalStates)

	// Step 9: dispatch loop. Break only when global capacity is
	// exhausted; skip individual issues whose per-state limit is full
	// so issues in other states can still be dispatched.
	var dispatched int
	for _, issue := range sorted {
		if GlobalAvailableSlots(o.state.MaxConcurrentAgents, len(o.state.Running)) == 0 {
			break
		}
		if !HasAvailableSlots(o.state, issue.State) {
			continue
		}
		if !ShouldDispatchWithSets(issue, o.state, activeSet, terminalSet) {
			continue
		}
		DispatchIssue(ctx, o.state, issue, nil, o.makeWorkerFn(""))
		dispatched++
	}

	o.logger.Info("tick completed",
		slog.Int("candidates", len(sorted)),
		slog.Int("dispatched", dispatched),
		slog.Int("running", len(o.state.Running)),
		slog.Int("retrying", len(o.state.RetryAttempts)),
	)

	o.notifyObservers()
}

// makeWorkerFn returns a [WorkerFunc] closure that runs
// [RunWorkerAttempt] with the orchestrator's shared dependencies.
// The closure captures channel references for OnEvent and OnExit
// delivery. The resumeSessionID must be read by the caller (on the
// event loop goroutine) before the goroutine starts, to avoid a
// data race on the Running map.
func (o *Orchestrator) makeWorkerFn(resumeSessionID string) WorkerFunc {
	return func(ctx context.Context, issue domain.Issue, attempt *int) {

		logger := logging.WithIssue(o.logger, issue.ID, issue.Identifier)

		deps := WorkerDeps{
			TrackerAdapter:     o.trackerAdapter,
			AgentAdapter:       o.agentAdapter,
			ConfigFunc:         o.workflowManager.Config,
			PromptTemplateFunc: o.workflowManager.PromptTemplate,
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
			ResumeSessionID: resumeSessionID,
			Logger:          logger,
		}

		RunWorkerAttempt(ctx, issue, attempt, deps)
	}
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
			id := issueID
			entry.TimerHandle = time.AfterFunc(
				time.Duration(entry.scheduledDelayMS)*time.Millisecond,
				func() { o.onRetryFire(id) },
			)
		} else {
			o.retryTimerCh <- issueID
		}
	}
}

// defaultDrainTimeout is the maximum duration the orchestrator waits for
// running workers to exit during graceful shutdown.
const defaultDrainTimeout = 30 * time.Second

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
		case result := <-o.workerExitCh:
			cfg := o.workflowManager.Config()
			HandleWorkerExit(o.state, result, HandleWorkerExitParams{
				Store:             o.store,
				MaxRetryBackoffMS: cfg.Agent.MaxRetryBackoffMS,
				OnRetryFire:       func(string) {}, // no-op: prevent retry fire events from reaching the event loop during drain
				Ctx:               drainCtx,
				Logger:            o.logger,
				BeforeRemoveHook:  cfg.Hooks.BeforeRemove,
				HookTimeoutMS:     cfg.Hooks.TimeoutMS,
				TrackerAdapter:    o.trackerAdapter,
				HandoffState:      cfg.Tracker.HandoffState,
				ActiveStates:      cfg.Tracker.ActiveStates,
			})
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			HandleAgentEvent(o.state, msg.IssueID, msg.Event)

		case <-deadline.C:
			o.logger.Warn("drain timeout exceeded, abandoning workers",
				slog.Int("remaining", len(o.state.Running)),
			)
			return
		}
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
