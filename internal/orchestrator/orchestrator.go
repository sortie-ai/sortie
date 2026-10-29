// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// WorkflowManager provides the current workflow config and prompt template.
// Satisfied by [workflow.Manager] in production.
type WorkflowManager interface {
	Config() config.ServiceConfig
	Rates() *pricing.Rates
	PromptTemplate() *prompt.Template
	PromptTemplateByID(id string) *prompt.Template
	Reload() error
	WorkflowAbsPath() string
	LoadStatus() workflow.LoadStatus
}

// OrchestratorStore is the persistence interface required by the event
// loop. Satisfied by [persistence.Store].
type OrchestratorStore interface {
	Ping(ctx context.Context) error
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	AppendRunHistoryWithEvents(ctx context.Context, run persistence.RunHistory, events []persistence.RunEvent, limits persistence.RunEventLimits) (persistence.RunHistory, error)
	AppendRunEvents(ctx context.Context, dispatchID, completedAt string, events []persistence.RunEvent, limits persistence.RunEventLimits) error
	UpsertAggregateMetrics(ctx context.Context, metrics persistence.AggregateMetrics) error
	UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	SaveUsageLimitHold(ctx context.Context, hold persistence.UsageLimitHold) error
	DeleteUsageLimitHold(ctx context.Context, agentKind string, modelOnly bool, model string) error
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
	ListReportedModels(ctx context.Context) ([]string, error)
	QueryBudgetExhaustedIssues(ctx context.Context, candidateIDs []string, maxSessions int) (map[string]int, error)
	QueryTokenBudgetUsage(ctx context.Context, candidateIDs []string) (map[string]persistence.IssueTokenUsage, error)
	UsageCompletedBetween(ctx context.Context, since, until time.Time) (persistence.IssueTokenUsage, error)
	LatestAgentRuns(ctx context.Context, issueIDs []string) (map[string]persistence.RunHistory, error)
	LatestResumeUsage(ctx context.Context, issueID, sessionID string) (*persistence.ResumeUsage, error)
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
	AddReactionHandedOffComments(ctx context.Context, issueID, kind string, commentIDs []string) error
	ListReactionHandedOffComments(ctx context.Context, issueID, kind string) ([]string, error)
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
	RecordStageHop(ctx context.Context, hop persistence.StageHop) error
	MarkStageHopObserved(ctx context.Context, issueID string) error
	MarkStageHopDispatched(ctx context.Context, issueID string) error
	DeleteStageHop(ctx context.Context, issueID string) error
	ListStageHops(ctx context.Context) ([]persistence.StageHop, error)
	UpsertLivePass(ctx context.Context, pass persistence.LivePass) error
	DeleteLivePass(ctx context.Context, issueID, kind string) error
	RecordRunStageResult(ctx context.Context, runID int64, targetRule, result string) error
	RecordIssueOutcome(ctx context.Context, outcome persistence.IssueOutcome) error
	DeleteIssueOutcome(ctx context.Context, issueID string) error
	GetReactionEscalation(ctx context.Context, issueID, kind string) (persistence.ReactionEscalation, bool, error)
	SaveReactionEscalation(ctx context.Context, rec persistence.ReactionEscalation) error
	EndReactionEscalation(ctx context.Context, issueID, kind string) error
	RecordReactionActivity(ctx context.Context, records []persistence.ReactionActivity) error
	RecordReactionTriageOutcome(ctx context.Context, outcome persistence.ReactionTriageOutcome) error
	LatestReactionTriageDisposition(ctx context.Context, issueID, kind, fingerprint string) (disposition string, found bool, err error)
	DeleteReactionEscalations(ctx context.Context, issueID string) error
	SaveAttentionAcknowledgement(ctx context.Context, ack persistence.AttentionAcknowledgement) error
	DeleteAttentionAcknowledgements(ctx context.Context, itemIDs []string) error
}

var _ OrchestratorStore = (*persistence.Store)(nil)
var _ RunEventStore = (*persistence.Store)(nil)

// Observer receives notifications when orchestrator state changes.
// Implementations must not block and must not mutate state.
type Observer interface {
	// OnStateChange is called on the event loop goroutine after tick
	// completion, worker exit, retry fire, each agent-lane message (agent
	// event, turn start, self-review progress, unaccounted turns), an
	// operator request the loop applies that changes what the API serves (a
	// retry made due now, an attention item acknowledged), and the start of
	// shutdown. It can run many times a second while sessions are active.
	OnStateChange()
}

// snapshotRequest is a request for a point-in-time snapshot, processed by
// the event loop, which replies on ReplyCh.
type snapshotRequest struct {
	ReplyCh chan<- RuntimeSnapshotResult
}

// loopAction is work a caller asks the event loop to run. state decides who
// owns the action: the loop takes it or the caller withdraws it, never both.
type loopAction struct {
	apply func(ctx context.Context)
	state atomic.Int32
	done  chan struct{}
}

const (
	loopActionPending int32 = iota
	loopActionTaken
	loopActionWithdrawn
)

// loopOutcome is what [Orchestrator.runOnLoop] did with a request.
type loopOutcome int

const (
	loopRan loopOutcome = iota + 1
	loopDraining
	loopWithdrawn
)

// agentEventMsg pairs an issue ID with an agent event for delivery through
// agentEventCh. A message with Workspace set carries the prepared workspace
// path instead of an event; it travels on the same channel so it is applied
// before any event the agent emits afterwards.
type agentEventMsg struct {
	IssueID   string
	Event     domain.AgentEvent
	Workspace string
}

// turnStartedMsg pairs an issue ID with the worker's started-turn count,
// the turn about to run included, for delivery through turnStartedCh.
type turnStartedMsg struct {
	IssueID      string
	TurnsStarted int
}

// issueReadMsg carries a worker's post-transition tracker read for delivery
// through issueReadCh. DispatchID keeps a read of an earlier run from landing
// on a later run of the same issue.
type issueReadMsg struct {
	IssueID    string
	DispatchID string
	Read       domain.IssueStateAndLabels
	StartedAt  time.Time
}

// unaccountedTurnsMsg pairs an issue ID with its worker's cumulative
// unaccounted-turn count for delivery through unaccountedTurnsCh. The count
// is cumulative, so a later message supersedes an earlier one.
type unaccountedTurnsMsg struct {
	IssueID          string
	UnaccountedTurns int
}

// OrchestratorParams holds the construction-time dependencies for
// [NewOrchestrator]. All fields are required unless documented otherwise.
type OrchestratorParams struct {
	State           *State
	Logger          *slog.Logger
	TrackerAdapter  domain.TrackerAdapter
	TrackerKind     string
	TrackerProject  string
	AgentAdapter    domain.AgentAdapter
	WorkflowManager WorkflowManager
	Store           OrchestratorStore
	PreflightParams PreflightParams
	Observers       []Observer
	Metrics         domain.Metrics
	ToolRegistry    *domain.ToolRegistry
	HostPool        *HostPool

	// Rates are the rates in force at startup. Nil prices nothing.
	Rates *pricing.Rates

	// Router selects the destinations of every outbound event. Nil
	// routes nothing, so no event reaches a tracker or a notifier.
	Router *route.Router

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

	// SCMProvider is the provider kind SCMAdapter and CIProvider were built
	// for; empty when both are nil.
	SCMProvider string

	// ReviewConfig holds validated review reaction configuration. Zero when
	// SCMAdapter is nil.
	ReviewConfig ReviewReactionConfig

	// ReviewConfigured is true when reactions.review_comments is active.
	ReviewConfigured bool

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

	// Once makes Run dispatch from one candidate poll and return when that
	// work settles. See [Orchestrator.OnceResult].
	Once bool
}

// Orchestrator owns the poll-and-dispatch event loop and all runtime state.
// Construct via [NewOrchestrator] and run with [Orchestrator.Run]. [Run]
// must be called from a single goroutine; external events arrive via
// channels.
type Orchestrator struct {
	state  *State
	logger *slog.Logger

	trackerAdapter     domain.TrackerAdapter
	trackerKind        string
	trackerProject     string
	router             *route.Router
	agentAdapter       domain.AgentAdapter
	agentAdapterByKind func(kind string) (domain.AgentAdapter, error)
	workflowManager    WorkflowManager
	store              OrchestratorStore
	runEventRetry      *RunEventRetry
	metrics            domain.Metrics
	blockerResolver    BlockerResolver
	abandonCh          <-chan struct{}

	// once is non-nil only for an orchestrator built with Once. The event
	// loop goroutine owns it.
	once *onceState

	workerExitCh       chan WorkerResult
	retryTimerCh       chan string
	agentEventCh       chan agentEventMsg
	selfReviewCh       chan selfReviewProgressMsg
	turnStartedCh      chan turnStartedMsg
	issueReadCh        chan issueReadMsg
	unaccountedTurnsCh chan unaccountedTurnsMsg
	snapshotCh         chan snapshotRequest
	refreshCh          chan struct{}

	// actionCh carries work for the event loop; only Run reads it. stopping
	// closes once, when shutdown begins and before workers drain.
	actionCh chan *loopAction
	stopping chan struct{}

	preflightParams PreflightParams
	observers       []Observer

	// drainTimeout overrides the worker-drain wait when positive. Zero
	// resolves to the ceiling [Orchestrator.drainRunningWorkers] derives
	// from the current configuration.
	drainTimeout                      time.Duration
	toolRegistry                      *domain.ToolRegistry
	sessionToolRegistryFunc           SessionToolRegistryFunc
	preflightOK                       atomic.Bool
	databaseHealth                    *healthRecord
	preflightHealth                   *healthRecord
	pollHealth                        *pollRecord
	scmHealth                         *healthRecord // nil when no reaction uses a source-control provider
	scmProvider                       string
	draining                          atomic.Bool
	hostPool                          *HostPool
	verificationCache                 *verificationCache
	workflowFileFunc                  func() string
	dbPath                            string
	mcpServerBinary                   string
	ciProvider                        domain.CIStatusProvider
	scmAdapter                        domain.SCMAdapter
	reviewConfig                      ReviewReactionConfig
	reviewReactionConfigured          bool
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

	// reportedAdvisories is the set of advisories reported as of the
	// most recent tick, empty at construction. Read and written only by
	// handleTick.
	reportedAdvisories []config.Advisory

	// lastTickSummary holds the attributes of the most recent "tick
	// completed" record, nil before the first. Read and written only by
	// handleTick.
	lastTickSummary []slog.Attr
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

	params.State.Rates = params.Rates

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
		trackerKind:                       params.TrackerKind,
		trackerProject:                    params.TrackerProject,
		router:                            params.Router,
		agentAdapter:                      params.AgentAdapter,
		agentAdapterByKind:                agentAdapterByKind,
		workflowManager:                   params.WorkflowManager,
		store:                             params.Store,
		runEventRetry:                     NewRunEventRetry(params.Store, logger),
		metrics:                           metrics,
		workerExitCh:                      make(chan WorkerResult, exitBuf),
		retryTimerCh:                      make(chan string, retryBuf),
		agentEventCh:                      make(chan agentEventMsg, eventBuf),
		selfReviewCh:                      make(chan selfReviewProgressMsg, eventBuf),
		turnStartedCh:                     make(chan turnStartedMsg, eventBuf),
		issueReadCh:                       make(chan issueReadMsg, eventBuf),
		unaccountedTurnsCh:                make(chan unaccountedTurnsMsg, eventBuf),
		snapshotCh:                        make(chan snapshotRequest, 4),
		refreshCh:                         make(chan struct{}, 1),
		actionCh:                          make(chan *loopAction, 4),
		stopping:                          make(chan struct{}),
		preflightParams:                   params.PreflightParams,
		observers:                         observers,
		toolRegistry:                      params.ToolRegistry,
		sessionToolRegistryFunc:           params.SessionToolRegistryFunc,
		hostPool:                          hostPool,
		verificationCache:                 newVerificationCache(),
		workflowFileFunc:                  params.WorkflowFileFunc,
		dbPath:                            params.DBPath,
		mcpServerBinary:                   params.MCPServerBinary,
		ciProvider:                        params.CIProvider,
		scmAdapter:                        params.SCMAdapter,
		databaseHealth:                    &healthRecord{},
		preflightHealth:                   &healthRecord{},
		pollHealth:                        &pollRecord{},
		reviewConfig:                      params.ReviewConfig,
		reviewReactionConfigured:          params.ReviewConfigured,
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
	if params.Once {
		o.once = newOnceState()
	}
	if params.SCMAdapter != nil || params.CIProvider != nil {
		o.scmHealth = &healthRecord{}
		o.scmProvider = params.SCMProvider
	}
	if params.SCMAdapter != nil {
		o.scmAdapter = observeSCM(params.SCMAdapter, o.scmHealth)
	}
	if params.CIProvider != nil {
		o.ciProvider = observeCI(params.CIProvider, o.scmHealth)
	}
	if params.WorkflowManager != nil {
		o.updateRouter(params.WorkflowManager.Config())
	}
	// Startup preflight must have passed for construction to reach here.
	o.preflightOK.Store(true)
	return o
}

// RunEvents returns a copy of a running run's retained events, oldest first,
// and false when no running run carries dispatchID. It is safe to call from
// any goroutine.
func (o *Orchestrator) RunEvents(dispatchID string) ([]RunEvent, bool) {
	return o.state.RetainedEvents.Events(dispatchID)
}

// RunEventsAfter returns the events of a running run that follow cursor, and
// false when no running run carries dispatchID. It is safe to call from any
// goroutine.
func (o *Orchestrator) RunEventsAfter(dispatchID string, cursor int64) (RunEventPage, bool) {
	return o.state.RetainedEvents.After(dispatchID, cursor)
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
// only when enforceCeiling is true, evaluates the in-flight money and token
// ceilings. The money ceiling is evaluated first, so a money stop wins when
// one event reaches both.
func (o *Orchestrator) applyAgentEvent(ctx context.Context, msg agentEventMsg, enforceCeiling bool) {
	if msg.Workspace != "" {
		if entry, ok := o.state.Running[msg.IssueID]; ok {
			entry.WorkspacePath = msg.Workspace
		}
		return
	}
	HandleAgentEvent(o.state, msg.IssueID, msg.Event, o.logger, o.metrics)
	if enforceCeiling {
		evaluateTokenWarning(o.state, msg.IssueID, msg.Event, o.logger)
	}
	o.maybeWriteIncrementalMetadata(ctx, msg.IssueID, msg.Event)
	if enforceCeiling {
		enforceInFlightMoneyCeiling(ctx, o.state, msg.IssueID, msg.Event, o.store, o.logger)
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

// applyIssueRead records a worker's post-transition read on the matching
// run when the read leaves that run going. It never stops a run or touches a
// retry: only reconcile does that.
func (o *Orchestrator) applyIssueRead(msg issueReadMsg) {
	entry, ok := o.state.Running[msg.IssueID]
	if !ok || entry.DispatchID != msg.DispatchID || entry.PendingCleanup {
		return
	}
	cfg := o.workflowManager.Config()
	if !readKeepsRun(entry, msg.Read.State, cfg.Tracker.ActiveStates, cfg.Tracker.HandoffState) {
		return
	}
	recordIssueRead(entry, msg.Read, msg.StartedAt)
}

// applyUnaccountedTurns records a relayed unaccounted-turn count on the
// issue's running entry and, once the session's in-flight row exists, writes
// it at once so the read-only tool server sees the count without waiting for
// the throttle or the worker exit. Before that row exists the count waits
// for the first write, which keeps a row existing exactly when the session
// reported a measurement. A no-op for an unknown issue.
func (o *Orchestrator) applyUnaccountedTurns(ctx context.Context, msg unaccountedTurnsMsg) {
	entry, ok := o.state.Running[msg.IssueID]
	if !ok {
		return
	}
	entry.UnaccountedTurns = msg.UnaccountedTurns
	if entry.LastMetadataWrite.IsZero() {
		return
	}
	o.writeIncrementalMetadata(ctx, msg.IssueID, entry, time.Now().UTC())
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
	applyQueued(o.issueReadCh, o.applyIssueRead)
	applyQueued(o.unaccountedTurnsCh, func(msg unaccountedTurnsMsg) {
		o.applyUnaccountedTurns(ctx, msg)
	})
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
	rates := o.workflowManager.Rates()
	o.updateRouter(cfg)
	for _, result := range exits {
		outcome := HandleWorkerExit(o.state, result, HandleWorkerExitParams{
			Rates:                             rates,
			Store:                             o.store,
			RunEventRetry:                     o.runEventRetry,
			MaxRetryBackoffMS:                 cfg.Agent.MaxRetryBackoffMS,
			ReleaseHeld:                       o.once != nil,
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
			Dispatch:                          cfg.Dispatch,
			ActiveStates:                      cfg.Tracker.ActiveStates,
			TerminalStates:                    cfg.Tracker.TerminalStates,
			Metrics:                           o.metrics,
			HostPool:                          o.hostPool,
			Router:                            o.router,
			CIProvider:                        o.ciProvider,
			SCMAdapter:                        o.scmAdapter,
			AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
			ReviewReactionConfigured:          o.reviewReactionConfigured,
			BotReviewReactionConfigured:       o.botReviewReactionConfigured,
			MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
			LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
			LabelFixReactionConfigured:        o.labelFixReactionConfigured,
			MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
		})
		o.recordOutcome(result, outcome)
	}
	o.updateGauges(time.Now())
	o.notifyObservers()
}

// recordOutcome keeps the outcome of an attempt for the once-mode result. It
// records nothing outside once mode or for an exit without a running entry.
func (o *Orchestrator) recordOutcome(result WorkerResult, outcome RunOutcome) {
	if o.once == nil || outcome == "" {
		return
	}
	o.once.record(result.IssueID, result.Identifier, outcome)
}

// Run enters the event loop and blocks until ctx is cancelled or, for an
// orchestrator built with [OrchestratorParams.Once], until the work of the
// single poll settles. Must be called from a single goroutine.
//
// Either way Run then cancels all running worker contexts, drains workers
// (up to the stop deadline plus a teardown margin) processing their results
// and agent events, waits for in-flight tracker operations, stops pending
// retry timers, and returns.
func (o *Orchestrator) Run(ctx context.Context) {
	// A past-due recovered retry can dispatch on the first pass, ahead of
	// the first tick, so the worker settings it launches under must be in
	// force before it is activated.
	startCfg := o.workflowManager.Config()
	o.applyWorkerConfig(startCfg)
	o.state.ReactionMaxAttempts = o.reactionMaxAttempts(startCfg)
	o.state.ReactionWatchWindows = o.reactionWatchWindows(startCfg)
	o.state.StageChains = startCfg.Dispatch.Chains()
	o.state.RuleAgents = o.ruleAgents(startCfg)
	o.state.HopCeiling = startCfg.Dispatch.HopCeiling()
	o.state.StallTimeoutMS = startCfg.Agent.StallTimeoutMS
	o.prepareConfiguredModels(startCfg)
	o.prepareReportedModels(ctx)
	o.activateReconstructedRetries()

	tickTimer := time.NewTimer(0)
	defer tickTimer.Stop()
	o.pollHealth.schedule(time.Now())

	for {
		select {
		case <-ctx.Done():
			if o.once != nil {
				o.once.interrupted = true
			}
			o.shutdown(tickTimer)
			return

		case <-tickTimer.C:
			o.handleTick(ctx)
			interval := time.Duration(o.state.PollIntervalMS) * time.Millisecond
			tickTimer.Reset(interval)
			o.pollHealth.schedule(time.Now().Add(interval))

		case workerExit := <-o.workerExitCh:
			o.handleWorkerExit(ctx, workerExit)

		case issueID := <-o.retryTimerCh:
			cfg := o.workflowManager.Config()
			templateHeld := func(id string) bool { return o.workflowManager.PromptTemplateByID(id) != nil }
			resolveSelection := func(frozen DispatchResolution, issue domain.Issue) DispatchResolution {
				selection, routedAfresh := retrySelection(cfg, templateHeld, frozen, issue, hopRouteOf(o.state, issue.ID))
				if routedAfresh {
					warnSeveralStageLabels(o.logger, issue, cfg.Dispatch, selection.RuleName)
				}
				return selection
			}
			resolveAttemptSettings := func(selection DispatchResolution, sshHost string) AttemptSettings {
				return o.resolveAttemptSettings(cfg, selection, sshHost)
			}
			var retryIdentifier string
			if entry, ok := o.state.RetryAttempts[issueID]; ok {
				retryIdentifier = entry.Identifier
			}
			heldReleased := HandleRetryTimer(o.state, issueID, HandleRetryTimerParams{
				Store:                  o.store,
				TrackerAdapter:         o.trackerAdapter,
				Router:                 o.router,
				ActiveStates:           cfg.Tracker.ActiveStates,
				TerminalStates:         cfg.Tracker.TerminalStates,
				Dispatch:               cfg.Dispatch,
				HandoffState:           cfg.Tracker.HandoffState,
				MaxRetryBackoffMS:      cfg.Agent.MaxRetryBackoffMS,
				MakeWorkerFn:           o.makeWorkerFn,
				AgentAdapterByKind:     o.agentAdapterByKind,
				ResolveSelection:       resolveSelection,
				ResolveAttemptSettings: resolveAttemptSettings,
				DefaultAgentKind:       cfg.Agent.Kind,
				OnRetryFire:            o.onRetryFire,
				Ctx:                    ctx,
				Logger:                 o.logger,
				MaxSessions:            cfg.Agent.MaxSessions,
				MaxConsecutiveAbsences: cfg.Agent.MaxConsecutiveAbsences,
				HandoffParkingLabel:    o.handoffParkingLabel,
				HandoffEvidencePolicy:  cfg.Tracker.HandoffEvidence,
				MaxTokens:              cfg.Agent.MaxTokens,
				MaxCostUSD:             cfg.Agent.MaxCostUSD,
				SpendCeilings:          spendCeilingsOf(cfg.Agent),
				PollIntervalMS:         cfg.Polling.IntervalMS,
				Metrics:                o.metrics,
				HostPool:               o.hostPool,
				WorkflowFile:           o.workflowFile(),
				ReleaseHeld:            o.once != nil,
				ConfiguredModelOf: func(selection DispatchResolution) string {
					return o.configuredModelOf(cfg, selection)
				},
			})
			if heldReleased && !o.once.hasOutcome(issueID) {
				o.once.record(issueID, retryIdentifier, RunOutcomeStopped)
			}
			o.updateGauges(time.Now())
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			o.applyAgentEvent(ctx, msg, true)
			o.notifyObservers()

		case msg := <-o.selfReviewCh:
			o.applySelfReviewProgress(msg)
			o.notifyObservers()

		case msg := <-o.turnStartedCh:
			o.applyTurnStarted(msg)
			o.notifyObservers()

		case msg := <-o.issueReadCh:
			o.applyIssueRead(msg)
			o.notifyObservers()

		case msg := <-o.unaccountedTurnsCh:
			o.applyUnaccountedTurns(ctx, msg)
			o.notifyObservers()

		case req := <-o.snapshotCh:
			refreshActiveOrder(o.state)
			snap := RuntimeSnapshot(o.state, time.Now())
			req.ReplyCh <- snap

		case action := <-o.actionCh:
			if action.state.CompareAndSwap(loopActionPending, loopActionTaken) {
				action.apply(ctx)
				close(action.done)
			}

		case <-o.refreshCh:
			o.handleTick(ctx)
		}

		if o.once != nil && ctx.Err() == nil && o.once.settled(o.state) {
			o.shutdown(tickTimer)
			return
		}
	}
}

// shutdown stops the event loop's intake and waits, bounded, for running
// workers, triage runs, and in-flight tracker operations.
func (o *Orchestrator) shutdown(tickTimer *time.Timer) {
	o.draining.Store(true)
	close(o.stopping)
	tickTimer.Stop()
	o.pollHealth.schedule(time.Time{})
	o.notifyObservers()
	o.drainRunningWorkers()
	o.runEventRetry.Stop()
	o.drainTriageRuns()
	o.drainTrackerOps()
	o.cancelRetryTimers()
}

// runOnLoop runs apply on the event loop and waits for it. It reports
// loopDraining when shutdown began first and loopWithdrawn when ctx ended
// first; in both cases apply never ran. The wait has no fixed bound.
func (o *Orchestrator) runOnLoop(ctx context.Context, apply func(loopCtx context.Context)) loopOutcome {
	select {
	case <-o.stopping:
		return loopDraining
	default:
	}

	action := &loopAction{apply: apply, done: make(chan struct{})}
	select {
	case o.actionCh <- action:
	case <-ctx.Done():
		return loopWithdrawn
	case <-o.stopping:
		return loopDraining
	}

	select {
	case <-action.done:
		return loopRan
	case <-ctx.Done():
	case <-o.stopping:
	}

	if action.state.CompareAndSwap(loopActionPending, loopActionWithdrawn) {
		select {
		case <-o.stopping:
			return loopDraining
		default:
			return loopWithdrawn
		}
	}
	<-action.done
	return loopRan
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
// parsing diagnostics as worker advisories. Run applies it once before
// activating recovered retries; handleTick applies it every tick so a
// reload takes effect.
func (o *Orchestrator) applyWorkerConfig(cfg config.ServiceConfig) []config.Advisory {
	wc := ParseWorkerConfig(cfg.ExtensionSection("worker"), cfg.ExtensionEnvRefPaths("worker"))
	o.hostPool.Update(wc.SSHHosts, wc.MaxPerHost)
	o.sshStrictHostKeyChecking = wc.SSHStrictHostKeyChecking
	o.sshPassEnv = wc.SSHPassEnv
	o.sshDisallowPassEnv = wc.SSHDisallowPassEnv
	return wc.Warnings
}

// reactionMaxAttempts returns the ceiling each counting reaction kind's pass
// applies under cfg; a kind with none is absent.
func (o *Orchestrator) reactionMaxAttempts(cfg config.ServiceConfig) map[string]int {
	ceilings := map[string]int{
		ReactionKindCI:              cfg.CIFeedback.MaxRetries,
		ReactionKindReview:          o.reviewConfig.MaxContinuationTurns,
		ReactionKindBotReview:       o.botReviewConfig.MaxContinuationTurns,
		ReactionKindMergeConflict:   o.mergeConflictConfig.MaxRetries,
		ReactionKindMergeCompletion: o.mergeCompletionConfig.MaxRetries,
	}
	if o.autoMergeConfig.MaxRetries > 0 {
		ceilings[ReactionKindAutoMerge] = o.autoMergeConfig.MaxRetries
	}
	return ceilings
}

// reactionWatchWindows returns the watch window each watched reaction kind's
// pass applies under cfg; a kind with none, or with a window of 0, is absent.
func (o *Orchestrator) reactionWatchWindows(cfg config.ServiceConfig) map[string]time.Duration {
	windowsMS := map[string]int{
		ReactionKindCI:            cfg.CIFeedback.WatchWindowMS,
		ReactionKindReview:        o.reviewConfig.WatchWindowMS,
		ReactionKindBotReview:     o.botReviewConfig.WatchWindowMS,
		ReactionKindMergeConflict: o.mergeConflictConfig.WatchWindowMS,
		ReactionKindAutoMerge:     o.autoMergeConfig.WatchWindowMS,
	}
	windows := make(map[string]time.Duration, len(windowsMS))
	for kind, ms := range windowsMS {
		if ms > 0 {
			windows[kind] = time.Duration(ms) * time.Millisecond
		}
	}
	return windows
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
	o.recordPreflight(validation)

	// On reload failure the workflow manager retains last-known-good config.
	cfg := o.workflowManager.Config()
	o.updateRouter(cfg)
	o.state.Rates = o.workflowManager.Rates()

	// Applied unconditionally, not gated on preflight success.
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	o.state.MaxTokens = cfg.Agent.MaxTokens
	o.state.MaxCostUSD = cfg.Agent.MaxCostUSD
	o.state.StallTimeoutMS = cfg.Agent.StallTimeoutMS
	o.state.MaxSessions = cfg.Agent.MaxSessions
	o.state.StageChains = cfg.Dispatch.Chains()
	o.state.RuleAgents = o.ruleAgents(cfg)
	o.prepareConfiguredModels(cfg)
	o.state.HopCeiling = cfg.Dispatch.HopCeiling()
	o.state.ReactionMaxAttempts = o.reactionMaxAttempts(cfg)
	o.state.ReactionWatchWindows = o.reactionWatchWindows(cfg)
	o.state.TokenWarningThreshold = cfg.Agent.TokenWarningThreshold()
	o.state.MaxConcurrentByState = cfg.Agent.MaxConcurrentByState

	current := append(cfg.Advisories(), o.applyWorkerConfig(cfg)...)
	LogAdvisories(ctx, o.logger, newAdvisories(current, o.reportedAdvisories))
	o.reportedAdvisories = current

	if err := o.store.Ping(ctx); !canceled(ctx, err) {
		o.databaseHealth.record(time.Now(), err)
	}

	// Reconcile unconditionally so in-flight workers are monitored even when
	// dispatch is skipped.
	ReconcileRunningIssues(o.state, ReconcileParams{
		TrackerAdapter:                    o.trackerAdapter,
		Router:                            o.router,
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
		SkipReactions:                     o.once != nil,
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

	spendHold := evaluateSpendCeiling(o.state, spendCeilingParams{
		Ctx:      ctx,
		Store:    o.store,
		Ceilings: spendCeilingsOf(cfg.Agent),
		Rates:    o.state.Rates,
		Now:      time.Now(),
		Logger:   o.logger,
	})

	// On preflight failure, skip dispatch but still notify observers.
	if !validation.OK() {
		pollResult = outcomeError
		o.pollHealth.record(time.Now(), pollOutcomePreflightFailed, nil)
		o.logger.Error("dispatch preflight failed",
			slog.Any("error", validation),
		)
		o.once.endPoll(true)
		o.notifyObservers()
		return
	}

	if !o.once.pollNeeded() {
		o.notifyObservers()
		return
	}

	issues, err := o.trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		pollResult = outcomeError
		if !canceled(ctx, err) {
			o.pollHealth.record(time.Now(), pollOutcomeTrackerFailed, err)
			o.once.endPoll(true)
		}
		o.logger.Error("failed to fetch candidate issues",
			slog.Any("error", err),
		)
		o.notifyObservers()
		return
	}

	o.pollHealth.record(time.Now(), pollOutcomeSucceeded, nil)

	sorted := SortForDispatch(issues)
	o.once.setCandidates(sorted)
	input := o.once.dispatchInput(sorted)

	o.rebuildBudgetExhausted(ctx, cfg, sorted)
	o.fillRetryIdentity(sorted)
	o.refreshParkedIssues(ctx, sorted, cfg.Tracker.TerminalStates)
	o.parkExhaustedAbsences(ctx, cfg, sorted)

	activeSet := stateSet(cfg.Tracker.ActiveStates)
	terminalSet := stateSet(cfg.Tracker.TerminalStates)

	// Break only on exhausted global capacity; skip individual issues whose
	// per-state limit is full so other states can still dispatch.
	pass := &TickResolution{offset: o.state.BlockerReadOffset}
	passStart := time.Now().UTC()
	usageGrace := time.Duration(cfg.Polling.IntervalMS) * time.Millisecond
	var usageHold func(domain.Issue) *HeldEntry
	if usageLimitHoldActive(o.state, usageGrace, passStart) {
		usageHold = func(candidate domain.Issue) *HeldEntry {
			resolution := ResolveRule(candidate, cfg.Dispatch, hopRouteOf(o.state, candidate.ID), cfg.Agent.Kind, "")
			return usageLimitHeldEntry(o.state, resolution.AgentKind, o.configuredModelOf(cfg, resolution), usageGrace, passStart)
		}
	}
	held := newHeldPass(o.state.Held, passStart, spendHoldEntry(spendHold, o.state.SpendCeilings), usageHold)

	var dispatched, dispatchedByRule, dispatchedByDefault, dispatchedByFallback int
	var heldByBlockers, blockersUnresolvedHeld, blockersNotReadHeld, blockersIncompleteHeld int
	var readFailures int
	for i, issue := range input {
		if GlobalAvailableSlots(o.state.MaxConcurrentAgents, len(o.state.Running)) == 0 {
			held.holdEligible(input[i:], o.state, activeSet, terminalSet, HeldEntry{
				Reason: SkipAgentSlotsFull,
				Slot:   &SlotHold{Scope: SlotScopeGlobal, Used: len(o.state.Running), Limit: o.state.MaxConcurrentAgents},
			})
			o.once.deferAll(input[i:], deferSlot)
			break
		}
		if o.hostPool.IsSSHEnabled() && !o.hostPool.HasCapacity() {
			held.holdEligible(input[i:], o.state, activeSet, terminalSet, HeldEntry{Reason: SkipSSHHostsFull})
			o.once.deferAll(input[i:], deferSlot)
			break
		}
		if !HasAvailableSlots(o.state, issue.State) {
			held.holdEligible(input[i:i+1], o.state, activeSet, terminalSet, HeldEntry{
				Reason: SkipAgentSlotsFull,
				Slot: &SlotHold{
					Scope: SlotScopeState,
					State: issue.State,
					Used:  RunningCountByState(o.state.Running, issue.State),
					Limit: o.state.MaxConcurrentByState[strings.ToLower(issue.State)],
				},
			})
			o.once.deferFor(issue.ID, deferSlot)
			continue
		}

		decision := EvaluateCandidate(ctx, issue, o.state, activeSet, terminalSet, o.blockerResolver, pass)
		if decision.Dispatch && spendHold != SkipNone {
			decision = CandidateDecision{Reason: spendHold, Issue: decision.Issue}
		}
		if !decision.Dispatch {
			if decision.Err != nil {
				readFailures++
			}
			held.holdDecision(decision, terminalSet)
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
			o.once.declined(issue, decision.Reason)
			continue
		}
		issue, dispatchable := o.holdForStageHop(ctx, decision.Issue, activeSet, terminalSet)
		if !dispatchable {
			if o.state.StageHops[issue.ID] != nil {
				held.hold(issue, HeldEntry{Reason: SkipStageHopUnconfirmed})
			}
			o.once.deferFor(issue.ID, deferTick)
			continue
		}

		resolution := ResolveRule(issue, cfg.Dispatch, hopRouteOf(o.state, issue.ID), cfg.Agent.Kind, "")
		warnSeveralStageLabels(o.logger, issue, cfg.Dispatch, resolution.RuleName)
		if usageHold != nil {
			if entry := usageLimitHeldEntry(o.state, resolution.AgentKind, o.configuredModelOf(cfg, resolution), usageGrace, passStart); entry != nil {
				held.hold(issue, *entry)
				o.once.declined(issue, SkipUsageLimit)
				continue
			}
		}
		adapter, adapterErr := o.agentAdapterByKind(resolution.AgentKind)
		if adapterErr != nil {
			o.logger.Error("agent kind unavailable",
				slog.String("rule_name", resolution.RuleName),
				slog.String("agent_kind", resolution.AgentKind),
				slog.Any("error", adapterErr),
			)
			o.metrics.IncDispatches(outcomeError)
			o.metrics.IncDispatchRuleMatch(resolution.MatchedAt.String(), normalizeDispatchRuleName(resolution.RuleName))
			o.once.dispatchFailed(issue)
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
			o.once.dispatchFailed(issue)
			continue
		}

		host, ok := o.hostPool.AcquireHost(issue.ID, "")
		if !ok {
			held.hold(issue, HeldEntry{Reason: SkipSSHHostsFull})
			held.holdEligible(input[i+1:], o.state, activeSet, terminalSet, HeldEntry{Reason: SkipSSHHostsFull})
			o.once.deferAll(input[i:], deferSlot)
			break
		}
		attemptSettings := o.resolveAttemptSettings(cfg, resolution, host)
		if len(attemptSettings.Refusals) > 0 {
			o.hostPool.ReleaseHost(issue.ID)
			o.logger.Error("agent settings refused",
				slog.Any("error", PreflightResult{Errors: attemptSettings.Refusals}),
				slog.String("rule_name", resolution.RuleName),
				slog.String("agent_kind", resolution.AgentKind),
				slog.String("check", attemptSettings.Refusals[0].Check),
				slog.String("diagnostic", attemptSettings.Refusals[0].Message),
			)
			o.metrics.IncDispatches(outcomeError)
			o.metrics.IncDispatchRuleMatch(resolution.MatchedAt.String(), normalizeDispatchRuleName(resolution.RuleName))
			o.once.dispatchFailed(issue)
			continue
		}
		issueLog := logging.WithIssue(o.logger, issue.ID, issue.Identifier)
		release := o.state.ParkReleases[issue.ID]
		if release != nil {
			comments, err := o.trackerAdapter.FetchIssueComments(ctx, issue.ID)
			if err != nil {
				o.hostPool.ReleaseHost(issue.ID)
				issueLog.Warn("park release comment read failed, holding issue", slog.Any("error", err))
				held.hold(issue, HeldEntry{Reason: SkipParkCommentsUnread})
				o.once.deferFor(issue.ID, deferTick)
				continue
			}
			issue.Comments = comments
		}
		dispatchCtx := ctx
		seed := freshRunSeed(ctx, o.state, freshRunSeedParams{
			SCMAdapter:          o.scmAdapter,
			Store:               o.store,
			WorkspaceRoot:       cfg.Workspace.Root,
			ReviewConfigured:    o.reviewReactionConfigured,
			ReviewConfig:        o.reviewConfig,
			BotReviewConfigured: o.botReviewReactionConfigured,
			BotReviewConfig:     o.botReviewConfig,
		}, issue, o.logger)
		if seed != nil {
			dispatchCtx = WithContinuationContext(ctx, seed)
		}
		lineage := withChainID(freshLineage(o.state, issue.ID, resolution.RuleName))
		dispatchCtx = withStageRender(dispatchCtx, prompt.StageContext{
			Current:         stageCurrent(cfg.Dispatch, resolution.RuleName),
			Previous:        lineage.PreviousRule,
			PreviousOutcome: lineage.PreviousOutcome,
		})
		resumeSessionID := ""
		var parkCtx prompt.ParkContext
		if release != nil {
			parkCtx = parkContextOf(release, issue.Comments)
			dispatchCtx = withParkRender(dispatchCtx, parkCtx)
			resumeSessionID = resumeSessionFor(release.SessionID,
				DispatchResolution{AgentKind: release.AgentKind, TemplateID: release.TemplateID},
				DispatchResolution{AgentKind: resolution.AgentKind, TemplateID: resolution.TemplateID})
		}
		// A park released on this tick reaches the dispatch with no refresh
		// in between, so a person's answer to a standing escalation is
		// recognized here, before the claim makes the issue Sortie's own.
		endOutsideHandoff(ctx, o.state, o.store, o.trackerAdapter, issue.ID, issue.State, cfg.Tracker.HandoffState, cfg.Tracker.TerminalStates, o.logger)
		DispatchIssue(dispatchCtx, o.state, issue, nil, host, o.makeWorkerFn(resumeSessionID, host, resolution.AgentKind, resolution.TemplateID, "", adapter, attemptSettings))
		o.once.drop(issue.ID)
		if entry := o.state.Running[issue.ID]; entry != nil {
			entry.ContinuationContext = seed
			entry.WorkflowFile = o.workflowFile()
			entry.AgentKind = resolution.AgentKind
			entry.RuleName = resolution.RuleName
			entry.StageLineage = lineage
			startPass(entry)
			// Read before the mark below flips RunDispatched.
			hop := o.state.StageHops[issue.ID]
			entry.FirstAfterHop = hop != nil && hop.TargetRule == resolution.RuleName && !hop.RunDispatched
			markStageHopDispatched(ctx, o.state, o.store, issue.ID, issueLog)
			clearIssueOutcome(ctx, o.store, issue.ID, issueLog)
			entry.RuleSettingsApplied = attemptSettings.Settings.RuleName != ""
			entry.ConfiguredModel = attemptSettings.Settings.Model
			entry.ConfiguredEffort = attemptSettings.Settings.Effort
			entry.TemplateID = resolution.TemplateID
			entry.UsageArrival, entry.UsageAttribution = attemptSettings.UsageArrival, attemptSettings.UsageAttribution
			entry.CarriesTokenFigure = attemptSettings.CarriesTokenFigure
			freezeIssueTokenBaseline(ctx, o.state, issue.ID, o.store, o.logger)
			recordRunningPass(ctx, o.store, issue.ID, entry, issueLog)
			if release != nil {
				consumeParkRelease(ctx, o.state, o.store, issue.ID, issueLog)
				issueLog.Info("park release handed to run",
					slog.String("reason", release.Reason),
					slog.String("gesture", release.ReleaseGesture),
					slog.Int("comments", len(parkCtx.Comments)),
					slog.Bool("resumes_session", resumeSessionID != ""),
				)
			}
		}
		logging.WithIssue(o.logger, issue.ID, issue.Identifier).Info("issue dispatched",
			slog.String("agent_kind", resolution.AgentKind),
			slog.String("rule_name", resolution.RuleName),
			slog.String("model", attemptSettings.Settings.Model),
			slog.String("effort", attemptSettings.Settings.Effort),
		)
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

	if len(o.state.Running) == 0 {
		o.once.dropSlotWaiters()
	}
	o.once.endPoll(false)

	o.state.BlockerReadOffset = nextBlockerReadOffset(pass)
	o.state.Held = held.entries()

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

	summary := []slog.Attr{
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
	}
	level := slog.LevelInfo
	if slices.EqualFunc(o.lastTickSummary, summary, slog.Attr.Equal) {
		level = slog.LevelDebug
	}
	o.lastTickSummary = summary
	o.logger.LogAttrs(ctx, level, "tick completed", summary...)

	o.pruneAttentionAcknowledgements(ctx)
	o.notifyObservers()
}

// recordCandidateHold logs the per-issue record of a candidate held by a
// blocker, the only reasons that log, and increments [IncCandidateHolds] for
// every reason except [SkipIneligible], which produces neither record nor
// counter.
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
// adapter carry the rule-resolved selection from the caller;
// attemptSettings carries the settings resolved for this attempt, whose
// usage arrival must be the value the caller freezes onto the running
// entry; reactionKind selects the worker posture. resumeSessionID must be
// read by the caller on the event loop before the goroutine starts, to
// avoid a Running-map race.
func (o *Orchestrator) makeWorkerFn(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter, attemptSettings AttemptSettings) WorkerFunc {
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
	convertedCredentialEnv := o.convertedCredentialEnv(agentKind)
	return func(ctx context.Context, issue domain.Issue, attempt *int) {

		logger := logging.WithIssue(o.logger, issue.ID, issue.Identifier)

		var resumeUsage *domain.TokenUsage
		var resumeUsageByModel map[string]domain.TokenUsage
		if resumeSessionID != "" {
			stored, err := o.store.LatestResumeUsage(ctx, issue.ID, resumeSessionID)
			if err != nil {
				logger.Warn("resume usage lookup failed", slog.Any("error", err))
			}
			resumeUsage = domainResumeUsage(stored)
			resumeUsageByModel = domainResumeUsageByModel(stored)
		}

		deps := WorkerDeps{
			TrackerAdapter:         o.trackerAdapter,
			Router:                 o.router,
			TrackerOpsWg:           &o.state.TrackerOpsWg,
			AgentAdapter:           adapter,
			ConfigFunc:             o.workflowManager.Config,
			PromptTemplateByIDFunc: o.workflowManager.PromptTemplateByID,
			TemplateID:             templateID,
			AgentKind:              agentKind,
			UsageArrival:           attemptSettings.UsageArrival,
			AgentSettings:          attemptSettings.Settings,
			UsageLimitNotified:     usageLimitNotifiedFrom(ctx),
			OnEvent: func(issueID string, event domain.AgentEvent) {
				select {
				case o.agentEventCh <- agentEventMsg{IssueID: issueID, Event: event}:
				default:
					logger.Warn("agent event channel full, dropping event",
						slog.Any("event_type", event.Type),
					)
				}
			},
			OnWorkspaceReady: func(issueID, path string) {
				select {
				case o.agentEventCh <- agentEventMsg{IssueID: issueID, Workspace: path}:
				case <-ctx.Done():
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
			OnIssueRead: func(issueID string, read domain.IssueStateAndLabels, startedAt time.Time) {
				msg := issueReadMsg{IssueID: issueID, DispatchID: dispatchIDFromContext(ctx), Read: read, StartedAt: startedAt}
				select {
				case o.issueReadCh <- msg:
				case <-ctx.Done():
				}
			},
			OnUnaccountedTurns: func(issueID string, unaccountedTurns int) {
				msg := unaccountedTurnsMsg{IssueID: issueID, UnaccountedTurns: unaccountedTurns}
				select {
				case o.unaccountedTurnsCh <- msg:
					return
				default:
				}
				// The run context outlives a ceiling stop, which cancels only
				// the worker context, so a stopped run's final count still
				// lands while the channel is full.
				select {
				case o.unaccountedTurnsCh <- msg:
				case <-runContextFrom(ctx).Done():
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
			ResumeUsage:             resumeUsage,
			ResumeUsageByModel:      resumeUsageByModel,
			DispatchID:              dispatchIDFromContext(ctx),
			Logger:                  logger,
			ToolRegistry:            o.toolRegistry,
			SessionToolRegistryFunc: o.sessionToolRegistryFunc,
			AgentToolChannelFunc: func(kind string) bool {
				meta, _ := o.preflightParams.AgentRegistry.Meta(kind)
				return meta.MCPDialect.DeliversTools()
			},
			SSHHost:                  sshHost,
			SSHStrictHostKeyChecking: strictHostKeyChecking,
			SSHEnvNamesFunc: func(kind string) []string {
				meta, _ := o.preflightParams.AgentRegistry.Meta(kind)
				declared := append(meta.CredentialEnv.Names(), convertedCredentialEnv...)
				return carriedEnvNames(declared, sshPassEnv, sshDisallowPassEnv)
			},
			Metrics:         o.metrics,
			WorkflowPath:    o.workflowManager.WorkflowAbsPath(),
			DBPath:          o.dbPath,
			MCPServerBinary: o.mcpServerBinary,
			ReactionKind:    reactionKind,
			Posture:         posture,

			VerificationCache: o.verificationCache,
		}

		RunWorkerAttempt(ctx, issue, attempt, deps)
	}
}

func domainResumeUsage(stored *persistence.ResumeUsage) *domain.TokenUsage {
	if stored == nil {
		return nil
	}
	return &domain.TokenUsage{
		InputTokens:      stored.InputTokens,
		OutputTokens:     stored.OutputTokens,
		TotalTokens:      stored.InputTokens + stored.OutputTokens,
		CacheReadTokens:  stored.CacheReadTokens,
		CacheWriteTokens: stored.CacheWriteTokens,
	}
}

// domainResumeUsageByModel returns the stored per-model split of the resume
// figure, nil when stored holds none.
func domainResumeUsageByModel(stored *persistence.ResumeUsage) map[string]domain.TokenUsage {
	if stored == nil || stored.ByModel == nil {
		return nil
	}
	byModel := make(map[string]domain.TokenUsage, len(stored.ByModel))
	for model, tokens := range stored.ByModel {
		byModel[model] = domain.TokenUsage{
			InputTokens:      tokens.InputTokens,
			OutputTokens:     tokens.OutputTokens,
			TotalTokens:      tokens.InputTokens + tokens.OutputTokens,
			CacheReadTokens:  tokens.CacheReadTokens,
			CacheWriteTokens: tokens.CacheWriteTokens,
		}
	}
	return byModel
}

// convertedCredentialEnv returns the credential variable names carried by
// the conversions that landed on kind under the configuration in force, in
// conversion order. A session captures them at dispatch so it keeps the
// names it started with across a reload.
func (o *Orchestrator) convertedCredentialEnv(kind string) []string {
	var names []string
	for _, conversion := range o.workflowManager.Config().AgentKindConversions() {
		if conversion.Replacement == kind {
			names = append(names, conversion.CredentialEnv...)
		}
	}
	return names
}

// workflowFile returns the base filename of the active workflow file, or
// empty when no callback is configured.
func (o *Orchestrator) workflowFile() string {
	if o.workflowFileFunc != nil {
		return o.workflowFileFunc()
	}
	return ""
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
// reaches [State.TokenWarningThreshold], and the write a relayed
// unaccounted-turn count triggers bypass this bound.
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
	o.writeIncrementalMetadata(ctx, issueID, entry, now)
}

// writeIncrementalMetadata upserts a running session's token totals,
// unaccounted-turn count, and live cost estimate under its dispatch ID, then
// restarts the throttle and clears any owed write. A failed upsert is logged
// and leaves both untouched. Must run on the single-writer event loop.
func (o *Orchestrator) writeIncrementalMetadata(ctx context.Context, issueID string, entry *RunningEntry, now time.Time) {
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
		CacheWriteTokens:    entry.CacheWriteTokens,
		ModelName:           entry.ModelName,
		APIRequestCount:     requestCount,
		APIRequestsMeasured: requestsMeasured,
		UnaccountedTurns:    entry.UnaccountedTurns,
		UpdatedAt:           now.Format(time.RFC3339),
	}
	if cost, status := pricing.PriceSession(spendOfRunningEntry(entry), o.state.Rates); status != pricing.CostUnmeasured {
		meta.EstimatedCostUSD = cost
		meta.CostStatus = string(status)
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

// fillRetryIdentity gives each queued retry that holds no read of its issue
// the read this tick's candidates carry, and a retry that holds only a title
// or link the candidate's type, labels, and state. A retry that holds a
// complete read, or that no candidate matches, keeps its values. Must run on
// the event loop.
func (o *Orchestrator) fillRetryIdentity(candidates []domain.Issue) {
	if len(o.state.RetryAttempts) == 0 {
		return
	}

	candidateByID := make(map[string]domain.Issue, len(candidates))
	for _, issue := range candidates {
		candidateByID[issue.ID] = issue
	}
	for id, entry := range o.state.RetryAttempts {
		candidate, ok := candidateByID[id]
		if !ok {
			continue
		}
		if entry.IssueIdentity.isZero() {
			entry.DisplayID = candidate.DisplayID
		}
		entry.IssueIdentity = entry.IssueIdentity.filledFrom(issueIdentityOf(candidate))
	}
}

// refreshParkedIssues evaluates the release rule against this tick's
// candidates, then against every parked issue the candidate slice omits,
// read through one batched, comment-free tracker call that returns state and
// labels, so the omitted issue is observed as a candidate is. A park that still
// holds no read of its issue takes the candidate's, or, when the candidates
// omit the issue and the batched read finds it, one single-issue read. A
// park that holds a title or link and no labels takes the candidate's issue
// type and labels only; the batched read never completes it. Must run on the
// event loop after the budget rebuild and before the dispatch loop.
func (o *Orchestrator) refreshParkedIssues(ctx context.Context, candidates []domain.Issue, terminalStates []string) {
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
			o.recordParkReleaseOutcome(ctx, issue.ID, entry.Identifier, issue.State, terminalStates)
			continue
		}
		observeParkedLabels(ctx, o.state, o.store, o.logger, entry, issue.ID, issue.Labels)
		o.backfillParkedIdentity(ctx, issue.ID, entry, issue)
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

	reads, err := o.trackerAdapter.FetchIssueStatesAndLabelsByIDs(ctx, missing)
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
		read, found := reads[id]
		if !found {
			continue
		}
		if !observeParkedState(ctx, o.state, o.store, o.logger, entry, id, read.State) {
			o.recordParkReleaseOutcome(ctx, id, entry.Identifier, read.State, terminalStates)
			continue
		}
		observeParkedLabels(ctx, o.state, o.store, o.logger, entry, id, read.Labels)
		if o.state.Parked[id] != entry {
			continue
		}
		if !entry.IssueIdentity.isZero() {
			continue
		}
		issue, err := o.trackerAdapter.FetchIssueByID(ctx, id)
		if err != nil {
			logging.WithIssue(o.logger, id, entry.Identifier).Warn(
				"parked issue identity read failed, retrying next tick",
				slog.Any("error", err),
			)
			continue
		}
		o.backfillParkedIdentity(ctx, id, entry, issue)
	}
}

// recordParkReleaseOutcome records the terminal outcome of an issue whose
// park a state change just released, when the state that released it is
// terminal.
func (o *Orchestrator) recordParkReleaseOutcome(ctx context.Context, issueID, identifier, observed string, terminalStates []string) {
	if !isTerminalState(observed, terminalStates) {
		return
	}
	recordIssueOutcome(ctx, o.store, terminalOutcome(issueID, observed, time.Now()), logging.WithIssue(o.logger, issueID, identifier))
}

// backfillParkedIdentity completes the identity of a park that is still
// parked and holds no read or a partial one, from issue. A read that names
// neither a title nor a link leaves the park as it was. The stored labels
// leave out the park's own parking label, which a read taken after the label
// write carries.
func (o *Orchestrator) backfillParkedIdentity(ctx context.Context, id string, entry *ParkedEntry, issue domain.Issue) {
	if o.state.Parked[id] != entry {
		return
	}
	if !entry.IssueIdentity.isZero() && !entry.IssueIdentity.holdsPartialRead() {
		return
	}
	read := issueIdentityOf(issue)
	if read.Title == "" && read.URL == "" {
		return
	}
	identity := entry.IssueIdentity.filledFrom(read)
	identity.Labels = labelsWithout(identity.Labels, entry.Label)
	entry.IssueIdentity = identity
	if err := o.store.UpsertParkedIssue(ctx, parkedIssueRow(id, entry)); err != nil {
		logging.WithIssue(o.logger, id, entry.Identifier).Error(
			"failed to persist park identity backfill",
			slog.Any("error", err),
		)
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

	var exhausted []domain.Issue
	for _, issue := range candidates {
		if counts[issue.ID] < ceiling {
			continue
		}
		if _, parked := o.state.Parked[issue.ID]; parked {
			continue
		}
		if slices.ContainsFunc(exhausted, func(held domain.Issue) bool { return held.ID == issue.ID }) {
			continue
		}
		exhausted = append(exhausted, issue)
	}
	if len(exhausted) == 0 {
		return
	}

	runs := o.latestRunsOfExhausted(ctx, exhausted)
	for _, issue := range exhausted {
		var run parkedRun
		if row, ok := runs[issue.ID]; ok {
			run = parkedRunOf(row)
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
			issueIdentityOf(issue),
			run,
			issue.State,
			counts[issue.ID],
			ceiling,
			o.handoffParkingLabel,
			issueLog,
		)
	}
}

// latestRunsOfExhausted reads the newest agent run of each issue in one call.
// A failed read yields no runs: the caller parks every issue regardless, so
// a store fault never lets an issue whose runs produce no work keep
// dispatching.
func (o *Orchestrator) latestRunsOfExhausted(ctx context.Context, issues []domain.Issue) map[string]persistence.RunHistory {
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	slices.Sort(ids)

	runs, err := o.store.LatestAgentRuns(ctx, ids)
	if err != nil {
		o.logger.Warn("park dispatch context read failed", slog.Any("error", err))
		return nil
	}
	return runs
}

// ceilingSettingByBudgetReason maps a budget-hold reason to the dotted
// config path of its governing setting, for the hold log record. A reason
// absent from this map emits no ceiling_setting attribute.
var ceilingSettingByBudgetReason = map[string]string{
	budgetReasonSession: "agent.max_sessions",
	budgetReasonToken:   "agent.max_tokens",
	budgetReasonMoney:   "agent.max_cost_usd",
}

// rebuildBudgetExhausted replaces the BudgetExhausted set once per tick from
// run_history, as the union of the session-count, token-sum, and money gates
// scoped to the candidate set. Money budget takes precedence, then token
// budget. On a query error for one axis, the prior entries for that axis are
// folded back in so a transient error never drops an issue mid-tick. An issue entering the set
// for the first time under a given reason produces one log record and one
// counter increment; a hold the memory already knows produces neither. Must
// run on the event loop.
func (o *Orchestrator) rebuildBudgetExhausted(ctx context.Context, cfg config.ServiceConfig, sorted []domain.Issue) {
	if cfg.Agent.MaxSessions == 0 && cfg.Agent.MaxTokens == 0 && cfg.Agent.MaxCostUSD == 0 {
		prior := o.state.BudgetExhausted
		o.state.BudgetExhausted = make(map[string]*BudgetExhaustedEntry)
		syncBudgetPasses(ctx, o.store, prior, o.state.BudgetExhausted, o.logger)
		o.state.TokenBudgetIncomplete = make(map[string]struct{})
		o.state.MoneyBudgetIncomplete = make(map[string]struct{})
		releaseAllBudgetHoldNotices(ctx, o.state, o.store, o.logger)
		return
	}

	candidateIDs := make([]string, len(sorted))
	identifierByID := make(map[string]string, len(sorted))
	displayIDByID := make(map[string]string, len(sorted))
	identityByID := make(map[string]IssueIdentity, len(sorted))
	for i, issue := range sorted {
		candidateIDs[i] = issue.ID
		identifierByID[issue.ID] = issue.Identifier
		displayIDByID[issue.ID] = issue.DisplayID
		identityByID[issue.ID] = issueIdentityOf(issue)
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
					IssueIdentity:  identityByID[id],
					Reason:         budgetReasonSession,
					UsedSessions:   count,
					BudgetSessions: cfg.Agent.MaxSessions,
					BudgetTokens:   int64(cfg.Agent.MaxTokens),
					BudgetUSD:      cfg.Agent.MaxCostUSD,
				}
			}
		}
	}

	freshIncomplete := make(map[string]struct{})
	freshMoneyIncomplete := make(map[string]struct{})
	if cfg.Agent.MaxTokens > 0 || cfg.Agent.MaxCostUSD > 0 {
		usageByIssue, qErr := o.store.QueryTokenBudgetUsage(ctx, candidateIDs)
		if qErr != nil {
			if cfg.Agent.MaxTokens > 0 {
				o.logger.Warn("token budget exhaustion query failed, retaining previous set",
					slog.Any("error", qErr),
				)
			}
			if cfg.Agent.MaxCostUSD > 0 {
				o.logger.Warn("money budget exhaustion query failed, retaining previous set",
					slog.Any("error", qErr),
				)
			}
			budgetEvidenceComplete = false
			for id, entry := range prior {
				tokenHeld := entry.Reason == budgetReasonToken && cfg.Agent.MaxTokens > 0
				moneyHeld := entry.Reason == budgetReasonMoney && cfg.Agent.MaxCostUSD > 0
				if !tokenHeld && !moneyHeld {
					continue
				}
				fresh[id] = entry
				foldedForward[id] = struct{}{}
			}
		} else {
			if cfg.Agent.MaxTokens > 0 {
				for _, id := range candidateIDs {
					usage := usageByIssue[id]
					if usage.UncachedTokens >= int64(cfg.Agent.MaxTokens) {
						entry := &BudgetExhaustedEntry{
							Identifier:     identifierByID[id],
							DisplayID:      displayIDByID[id],
							IssueIdentity:  identityByID[id],
							Reason:         budgetReasonToken,
							UsedSessions:   usage.Sessions,
							BudgetSessions: cfg.Agent.MaxSessions,
							BudgetTokens:   int64(cfg.Agent.MaxTokens),
							BudgetUSD:      cfg.Agent.MaxCostUSD,
						}
						entry.setTokenUsage(usage)
						fresh[id] = entry
						continue
					}
					if entry, held := fresh[id]; held {
						entry.setTokenUsage(usage)
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
			if cfg.Agent.MaxCostUSD > 0 {
				for _, id := range candidateIDs {
					usage := usageByIssue[id]
					used, status := pricing.IssueCost(recordedOf(usage), pricing.SessionCost{}, !usage.SpendIncomplete())
					if pricing.CeilingReached(used, cfg.Agent.MaxCostUSD) {
						entry := &BudgetExhaustedEntry{
							Identifier:     identifierByID[id],
							DisplayID:      displayIDByID[id],
							IssueIdentity:  identityByID[id],
							Reason:         budgetReasonMoney,
							UsedSessions:   usage.Sessions,
							BudgetSessions: cfg.Agent.MaxSessions,
							BudgetTokens:   int64(cfg.Agent.MaxTokens),
							BudgetUSD:      cfg.Agent.MaxCostUSD,
						}
						entry.setMoneyUsage(usage)
						fresh[id] = entry
						continue
					}
					if entry, held := fresh[id]; held {
						entry.setMoneyUsage(usage)
					}
					if status == pricing.CostEstimated {
						continue
					}
					freshMoneyIncomplete[id] = struct{}{}
					if _, wasIncomplete := o.state.MoneyBudgetIncomplete[id]; wasIncomplete {
						continue
					}
					issueLog := logging.WithIssue(o.logger, id, identifierByID[id])
					warnMoneyBudgetIncomplete(issueLog, usage, cfg.Agent.MaxCostUSD)
				}
			}
		}
	}
	o.state.TokenBudgetIncomplete = freshIncomplete
	o.state.MoneyBudgetIncomplete = freshMoneyIncomplete

	o.describeBudgetHolds(ctx, cfg, prior, fresh)
	syncBudgetPasses(ctx, o.store, prior, fresh, o.logger)

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
		if entry.UsedUSD != nil {
			attrs = append(attrs,
				slog.Float64("used_usd", *entry.UsedUSD),
				slog.Float64("budget_usd", entry.BudgetUSD),
			)
		}
		if setting, known := ceilingSettingByBudgetReason[entry.Reason]; known {
			attrs = append(attrs, slog.String("ceiling_setting", setting))
		}
		issueLog.LogAttrs(ctx, slog.LevelWarn, "candidate held by budget ceiling", attrs...)
		o.metrics.IncBudgetExhaustions(entry.Reason)
	}

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
		delivery := o.router.Route(budgetHeldNotification(id, fresh[id]))
		if delivery.Empty() {
			continue
		}
		if !budgetHoldNoticeAllowed(o.state, now) {
			break
		}
		postBudgetHoldNotice(o.state, budgetHoldNoticeParams{
			IssueID:  id,
			Entry:    fresh[id],
			Store:    o.store,
			Delivery: delivery,
			Metrics:  o.metrics,
			Logger:   logging.WithIssue(o.logger, id, fresh[id].Identifier),
			Ctx:      ctx,
		})
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

// describeBudgetHolds gives each record in fresh the context of the dispatch
// its hold blocks. A record keeps the context of its prior record; one still
// without takes a waiting hop's target, resolved from cfg, and otherwise the
// issue's latest agent run, read for all such issues in one call. A failed
// read leaves those records without a context until the next tick.
func (o *Orchestrator) describeBudgetHolds(ctx context.Context, cfg config.ServiceConfig, prior, fresh map[string]*BudgetExhaustedEntry) {
	var needRun []string
	for id, entry := range fresh {
		if entry.AgentKind != "" {
			continue
		}
		if held := prior[id]; held != nil && held.AgentKind != "" {
			entry.RuleName, entry.StageLineage, entry.AgentKind, entry.ConfiguredModel =
				held.RuleName, held.StageLineage, held.AgentKind, held.ConfiguredModel
			continue
		}
		if o.describeFromWaitingHop(cfg, id, entry) {
			continue
		}
		needRun = append(needRun, id)
	}
	if len(needRun) == 0 {
		return
	}

	slices.Sort(needRun)
	runs, err := o.store.LatestAgentRuns(ctx, needRun)
	if err != nil {
		o.logger.Warn("budget hold dispatch context read failed", slog.Any("error", err))
		return
	}
	for id, run := range runs {
		entry := fresh[id]
		entry.RuleName = run.RuleName
		entry.StageLineage = stageLineageOf(run)
		entry.AgentKind = run.AgentAdapter
		entry.ConfiguredModel = run.ConfiguredModel
	}
}

// describeFromWaitingHop sets entry's dispatch context from the target of the
// issue's hop when no run was dispatched since and cfg still names the target
// rule. It reports whether it did.
func (o *Orchestrator) describeFromWaitingHop(cfg config.ServiceConfig, issueID string, entry *BudgetExhaustedEntry) bool {
	hop := o.state.StageHops[issueID]
	if hop == nil || hop.RunDispatched {
		return false
	}
	rule, ok := cfg.Dispatch.RuleByName(hop.TargetRule)
	if !ok {
		return false
	}
	agent := o.ruleAgentOf(cfg, rule)
	entry.RuleName = rule.Name
	entry.StageLineage = freshLineage(o.state, issueID, rule.Name)
	entry.AgentKind = agent.AgentKind
	entry.ConfiguredModel = agent.ConfiguredModel
	return true
}

// ruleAgentOf returns the agent kind and model a dispatch of rule resolves to
// under cfg.
func (o *Orchestrator) ruleAgentOf(cfg config.ServiceConfig, rule config.DispatchRule) RuleAgent {
	resolution := ruleResolution(rule, cfg.Dispatch, cfg.Agent.Kind, "")
	return RuleAgent{
		AgentKind:       resolution.AgentKind,
		ConfiguredModel: o.configuredModelOf(cfg, resolution),
	}
}

// configuredModelOf returns the model a dispatch of resolution is configured
// with under cfg.
func (o *Orchestrator) configuredModelOf(cfg config.ServiceConfig, resolution DispatchResolution) string {
	return o.resolveAttemptSettings(cfg, resolution, "").Settings.Model
}

// ruleAgents maps each named rule of cfg that carries a stage label, the rules
// a stage hop can target, to its [Orchestrator.ruleAgentOf].
func (o *Orchestrator) ruleAgents(cfg config.ServiceConfig) map[string]RuleAgent {
	agents := make(map[string]RuleAgent, len(cfg.Dispatch.Rules))
	for _, rule := range cfg.Dispatch.Rules {
		if rule.Name == "" || rule.Stage == "" {
			continue
		}
		agents[rule.Name] = o.ruleAgentOf(cfg, rule)
	}
	return agents
}

// ConfiguredModel is the model one settings block of the workflow configures
// for an agent kind.
type ConfiguredModel struct {
	AgentKind string
	// Model is empty for a block that configures none.
	Model string
}

// configuredModels returns the distinct agent kind and model pairs of the
// settings blocks cfg can resolve, with an empty Model where a block
// configures none.
func configuredModels(cfg config.ServiceConfig) []ConfiguredModel {
	var pairs []ConfiguredModel
	add := func(selection config.SettingsSelection) {
		pair := ConfiguredModel{
			AgentKind: selection.Kind,
			Model:     config.ResolveAgentSettings(cfg, selection, "").Model,
		}
		if !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	for _, ref := range orderedUniqueAgentKinds(cfg) {
		add(config.SettingsSelection{Kind: ref.Kind})
	}
	for _, rule := range cfg.Dispatch.Rules {
		if rule.SettingsKind != "" {
			add(config.SettingsSelection{Kind: rule.SettingsKind, RuleName: rule.Name})
		}
	}
	return pairs
}

// ConfiguredModels returns the agent kind and model pairs of the workflow
// configuration in effect that name a model, or nil without a workflow
// manager. It reads no event-loop state, so any goroutine may call it.
func (o *Orchestrator) ConfiguredModels() []ConfiguredModel {
	if o.workflowManager == nil {
		return nil
	}
	var named []ConfiguredModel
	for _, pair := range configuredModels(o.workflowManager.Config()) {
		if pair.Model != "" {
			named = append(named, pair)
		}
	}
	return named
}

// prepareConfiguredModels exports zero series for every model cfg
// configures, so rate() and increase() count each model's first sample.
func (o *Orchestrator) prepareConfiguredModels(cfg config.ServiceConfig) {
	var prepared []string
	for _, pair := range configuredModels(cfg) {
		if slices.Contains(prepared, pair.Model) {
			continue
		}
		prepared = append(prepared, pair.Model)
		o.metrics.PrepareModel(pair.Model)
	}
}

// prepareReportedModels exports zero series for every model earlier runs
// reported. A history read failure is not fatal: the configured models
// are prepared regardless, and a model first reported later is prepared
// when its event arrives.
func (o *Orchestrator) prepareReportedModels(ctx context.Context) {
	models, err := o.store.ListReportedModels(ctx)
	if err != nil {
		o.logger.Warn("list reported models failed", slog.Any("error", err))
		return
	}
	for _, model := range models {
		o.metrics.PrepareModel(model)
	}
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
		entry.cancelRun(errCancelShutdown)
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
			applyQueued(o.issueReadCh, o.applyIssueRead)
			applyQueued(o.unaccountedTurnsCh, func(msg unaccountedTurnsMsg) {
				o.applyUnaccountedTurns(drainCtx, msg)
			})
			cfg := o.workflowManager.Config()
			rates := o.workflowManager.Rates()
			o.updateRouter(cfg)
			outcome := HandleWorkerExit(o.state, workerExit, HandleWorkerExitParams{
				Rates:                             rates,
				Store:                             o.store,
				MaxRetryBackoffMS:                 cfg.Agent.MaxRetryBackoffMS,
				ReleaseHeld:                       o.once != nil,
				MaxConsecutiveAbsences:            cfg.Agent.MaxConsecutiveAbsences,
				OnRetryFire:                       func(string) {}, // no-op: prevent retry fire events from reaching the event loop during drain
				Ctx:                               drainCtx,
				Logger:                            o.logger,
				BeforeRemoveHook:                  cfg.Hooks.BeforeRemove,
				HookTimeoutMS:                     cfg.Hooks.TimeoutMS,
				TrackerAdapter:                    o.trackerAdapter,
				HandoffState:                      cfg.Tracker.HandoffState,
				NoChangeState:                     cfg.Tracker.NoChangeState,
				Dispatch:                          cfg.Dispatch,
				ActiveStates:                      cfg.Tracker.ActiveStates,
				TerminalStates:                    cfg.Tracker.TerminalStates,
				Metrics:                           o.metrics,
				HostPool:                          o.hostPool,
				Router:                            o.router,
				CIProvider:                        o.ciProvider,
				SCMAdapter:                        o.scmAdapter,
				AutoMergeReactionConfigured:       o.autoMergeReactionConfigured,
				ReviewReactionConfigured:          o.reviewReactionConfigured,
				BotReviewReactionConfigured:       o.botReviewReactionConfigured,
				MergeConflictReactionConfigured:   o.mergeConflictReactionConfigured,
				LabelReviewReactionConfigured:     o.labelReviewReactionConfigured,
				LabelFixReactionConfigured:        o.labelFixReactionConfigured,
				MergeCompletionReactionConfigured: o.mergeCompletionReactionConfigured,
			})
			o.recordOutcome(workerExit, outcome)
			o.updateGauges(time.Now())
			o.notifyObservers()

		case msg := <-o.agentEventCh:
			o.applyAgentEvent(drainCtx, msg, false)
			o.notifyObservers()

		case msg := <-o.selfReviewCh:
			o.applySelfReviewProgress(msg)
			o.notifyObservers()

		case msg := <-o.turnStartedCh:
			o.applyTurnStarted(msg)
			o.notifyObservers()

		case msg := <-o.issueReadCh:
			o.applyIssueRead(msg)
			o.notifyObservers()

		case msg := <-o.unaccountedTurnsCh:
			o.applyUnaccountedTurns(drainCtx, msg)
			o.notifyObservers()

		case req := <-o.snapshotCh:
			refreshActiveOrder(o.state)
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
// from the event loop goroutine only, after the change is applied.
func (o *Orchestrator) notifyObservers() {
	refreshActiveOrder(o.state)
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
