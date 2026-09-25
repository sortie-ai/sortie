package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workspace"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// WorkerExitKind classifies how the worker attempt terminated.
type WorkerExitKind string

const (
	// WorkerExitNormal indicates the turn loop completed without error.
	// The issue may still be active (max_turns reached) or transitioned to
	// a non-active state.
	WorkerExitNormal WorkerExitKind = "normal"

	// WorkerExitError indicates a fatal error during workspace prep, prompt
	// rendering, session lifecycle, or tracker refresh.
	WorkerExitError WorkerExitKind = "error"

	// WorkerExitCancelled indicates the worker's context was cancelled
	// (reconciliation kill, stall timeout, or graceful shutdown).
	WorkerExitCancelled WorkerExitKind = "cancelled"
)

// workerState is the .sortie/state.json shape the running agent reads back
// through its status tool. The four token members are nil together,
// exactly when TokensMeasured is false, and a nil member serializes as JSON
// null rather than being omitted, so the agent cannot mistake an unmeasured
// session for one that spent nothing. The session-start write states a
// measured zero: no turn has begun, so nothing has been spent.
type workerState struct {
	TurnNumber      int    `json:"turn_number"`
	MaxTurns        int    `json:"max_turns"`
	Attempt         *int   `json:"attempt"`
	StartedAt       string `json:"started_at"`
	InputTokens     *int64 `json:"input_tokens"`
	OutputTokens    *int64 `json:"output_tokens"`
	TotalTokens     *int64 `json:"total_tokens"`
	CacheReadTokens *int64 `json:"cache_read_tokens"`
	TokensMeasured  bool   `json:"tokens_measured"`
}

// withTokens returns s carrying the measurement mirror and, when measured,
// the four figures folded so far. Every state-file write passes through it.
func (s workerState) withTokens(usage domain.TokenUsage, measured bool) workerState {
	s.TokensMeasured = measured
	if !measured {
		return s
	}
	s.InputTokens = &usage.InputTokens
	s.OutputTokens = &usage.OutputTokens
	s.TotalTokens = &usage.TotalTokens
	s.CacheReadTokens = &usage.CacheReadTokens
	return s
}

// writeWorkerState writes session runtime state to .sortie/state.json.
func writeWorkerState(workspacePath string, state workerState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal worker state: %w", err)
	}
	return workspacekit.WriteSortieFile(workspacePath, "state.json", data)
}

// WorkerResult is the terminal outcome of a single worker attempt,
// delivered to the orchestrator via [WorkerDeps.OnExit].
type WorkerResult struct {
	IssueID string

	Identifier string

	// ExitKind classifies the exit as normal, error, or cancelled.
	ExitKind WorkerExitKind

	// Error is the cause of an abnormal exit. Nil for normal exits and
	// for a cancellation that interrupted no failing operation.
	Error error

	// StoppedByTokenCeiling is true when the worker context's cancellation
	// cause is the in-flight token ceiling's own stop request.
	StoppedByTokenCeiling bool

	// TurnsCompleted is the number of turns that received a TurnResult
	// before exit.
	TurnsCompleted int

	// TurnsStarted counts turns the worker began (self-review included),
	// counted at start, so a turn that errored or was cancelled still
	// counts. Deciding whether the session ever ran needs this rather than
	// TurnsCompleted.
	TurnsStarted int

	// SessionID is the accepted session identifier at the moment report
	// hands this result to [WorkerDeps.OnExit]. It is empty when the
	// working session never started, or started with an empty identifier
	// that no later report replaced. Enables session continuity on
	// continuation retries.
	SessionID string

	// WorkspacePath is the workspace directory for this attempt, empty if
	// preparation failed.
	WorkspacePath string

	// HandoffEvidencePolicy is frozen from the run's initial config
	// snapshot; reloads during the run cannot change it.
	HandoffEvidencePolicy config.HandoffEvidencePolicy

	// HandoffEvidenceBaseline is the Git state captured after workspace prep
	// and pre-run hooks, before StartSession. Nil when capture failed or the
	// frozen policy is off.
	HandoffEvidenceBaseline *workspace.HandoffEvidenceBaseline

	// HandoffEvidenceBaselineError records why baseline capture failed. Nil
	// when capture succeeded or the frozen policy is off.
	HandoffEvidenceBaselineError error

	// AgentAdapter is the agent adapter kind used to dispatch this attempt:
	// the rule-resolved kind, else the workflow-wide default.
	AgentAdapter string

	Attempt *int

	// SSHHost is the SSH host the worker executed on, empty for local.
	SSHHost string

	// SoftStop is true when the worker exited on a recognized A2O status
	// signal, which suppresses continuation retry scheduling.
	SoftStop bool

	// SoftStopReason is the status token that triggered the soft stop, empty
	// when SoftStop is false.
	SoftStopReason string

	// ReviewMetadata summarizes the self-review outcome. Nil when self-review
	// is disabled or the worker exited before the phase.
	ReviewMetadata *domain.ReviewMetadata

	// StartedAt is populated by the exit handler from the running entry, not
	// by the worker.
	StartedAt time.Time

	// ObservedIssueState is the most recent tracker state a worker refresh
	// returned. Empty when no refresh returned a state (a posture that does
	// not drive issue state, or an exit before the first refresh). On a
	// soft-stop exit it is the previous turn's observation, because the
	// status-file check precedes that turn's refresh. Tested for a terminal
	// state ahead of the dispatch-time snapshot; not used for active-state
	// classification.
	ObservedIssueState string

	// Usage is the run-cumulative token usage as of exit, folded from every
	// usage-bearing event and TurnResult.Usage; zero before the first turn
	// returns. Excludes figures the run's arrival rejects.
	Usage domain.TokenUsage

	// UnaccountedTurns counts turns that spent tokens no figure was proven to
	// cover, making Usage a lower bound when non-zero. Orthogonal to
	// UsageMeasured, which reports only whether any figure exists.
	UnaccountedTurns int

	// UsageMeasured is true when the run's spend is known: either no turn was
	// entered (a zero spend is exact) or an admitted measurement arrived
	// since the first turn began. False means a turn ran and no admitted
	// measurement arrived, so spend is unknown rather than zero.
	UsageMeasured bool

	// ModelName is the model from the last admitted token_usage event that
	// carried one. Empty when none did.
	ModelName string

	// APIRequestCount is the number of admitted token_usage events the worker
	// relayed this run. Zero before the first turn began.
	APIRequestCount int
}

// SessionToolRegistryFunc builds the first-turn tool advertisement.
type SessionToolRegistryFunc func(ctx context.Context, issueID, workspacePath string) (*domain.ToolRegistry, error)

// AgentToolChannelFunc reports whether a session of the given kind, in the
// given mode, can execute the registry's tools. remote is true for SSH. A
// nil [WorkerDeps.AgentToolChannelFunc] means the answer is unknown, not
// that no channel exists.
type AgentToolChannelFunc func(kind string, remote bool) bool

// SSHEnvNamesFunc returns the env var names to carry into a remote session
// of the given kind: the kind's declared credential names plus the
// operator-listed names, less the operator-disallowed names. A nil
// [WorkerDeps.SSHEnvNamesFunc] means no name is carried.
type SSHEnvNamesFunc func(kind string) []string

// DispatchPosture selects the worker behavior for a dispatch. Exactly one
// posture applies per dispatch; the type makes the invariant representable.
type DispatchPosture int

const (
	// PostureNormal is the default work dispatch: full clone via operator
	// hooks, and it drives the linked issue's state.
	PostureNormal DispatchPosture = iota

	// PostureReview is the read-only, no-clone review dispatch
	// (label-review): scratch workspace, no hooks, no issue-work side
	// effects, fresh session.
	PostureReview

	// PostureFix is the read-write fix dispatch (label-fix): full clone via
	// operator hooks so the agent can check out the PR head branch and push,
	// a fresh session, and every issue-work side effect suppressed.
	PostureFix
)

// RunsSetupHooks reports whether the posture runs the operator setup and
// teardown hooks. True for PostureNormal and PostureFix.
func (p DispatchPosture) RunsSetupHooks() bool {
	return p == PostureNormal || p == PostureFix
}

// DrivesIssueState reports whether the posture claims and drives the linked
// issue's tracker state. True only for PostureNormal.
func (p DispatchPosture) DrivesIssueState() bool {
	return p == PostureNormal
}

// dispatchPostureForReactionKind maps a dispatch reaction kind to its worker
// posture. It is the single source of truth for posture selection, shared by
// the dispatch builder and the exit handler so the two never disagree.
func dispatchPostureForReactionKind(kind string) DispatchPosture {
	switch kind {
	case ReactionKindLabelReview:
		return PostureReview
	case ReactionKindLabelFix:
		return PostureFix
	default:
		return PostureNormal
	}
}

// WorkerDeps holds the collaborators injected into the worker attempt
// function. Constructed once and shared across all workers. All fields are
// required unless documented as optional.
type WorkerDeps struct {
	// TrackerAdapter fetches issue states for mid-turn re-checks.
	TrackerAdapter domain.TrackerAdapter

	// AgentAdapter manages agent session lifecycle.
	AgentAdapter domain.AgentAdapter

	// ConfigFunc returns the current effective config, called at each
	// attempt's start so reloaded values take effect for new attempts.
	ConfigFunc func() config.ServiceConfig

	// PromptTemplateByIDFunc returns the parsed template for the given ID;
	// the empty-string key selects the WORKFLOW.md body template.
	PromptTemplateByIDFunc func(id string) *prompt.Template

	// TemplateID is the resolved template key frozen at dispatch. Empty
	// selects the body template.
	TemplateID string

	// AgentKind is the rule-resolved kind frozen at dispatch. Empty falls
	// back to the workflow-wide default.
	AgentKind string

	// UsageArrival is the arrival frozen on the run's entry. The zero value
	// admits every figure.
	UsageArrival registry.UsageArrival

	// OnEvent relays agent events to the serialized event loop. Called from
	// the worker goroutine; must be concurrency-safe.
	OnEvent func(issueID string, event domain.AgentEvent)

	// OnExit reports the terminal outcome. Called exactly once, last, before
	// the goroutine returns. Must be concurrency-safe.
	OnExit func(issueID string, result WorkerResult)

	// OnTurnStarted reports turnsStarted before each turn runs. Called from
	// the worker goroutine; must be concurrency-safe. Nil disables it.
	OnTurnStarted func(issueID string, turnsStarted int)

	// ResumeSessionID is the previous attempt's session ID, non-empty on
	// continuation retries so the adapter can resume the conversation.
	ResumeSessionID string

	// DispatchID fences session identity to this worker attempt.
	DispatchID string

	// ToolRegistry holds the tools available to agent sessions. May be nil.
	// Read-only after construction.
	ToolRegistry *domain.ToolRegistry

	// SessionToolRegistryFunc builds the per-session tool registry so the
	// advertised set matches what the MCP sidecar serves. When nil, the
	// worker falls back to ToolRegistry. Optional.
	SessionToolRegistryFunc SessionToolRegistryFunc

	// AgentToolChannelFunc reports whether the session's kind and mode can
	// reach the tools the advertisement would name. Nil means unknown, and
	// the worker withholds the advertisement rather than name uncallable
	// tools. Optional.
	AgentToolChannelFunc AgentToolChannelFunc

	// Logger is the structured logger with issue-scoped context attached.
	Logger *slog.Logger

	// SSHHost is the SSH destination for this worker's sessions. Empty for
	// local execution.
	SSHHost string

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking value.
	// Empty means "accept-new".
	SSHStrictHostKeyChecking string

	// SSHEnvNamesFunc returns the env var names to carry into a remote
	// session. Nil means no name is carried.
	SSHEnvNamesFunc SSHEnvNamesFunc

	// Metrics records dispatch-time counters. Always non-nil:
	// NewOrchestrator falls back to NoopMetrics.
	Metrics domain.Metrics

	// WorkflowPath is the absolute WORKFLOW.md path, passed to MCP config
	// generation. Empty disables MCP config generation.
	WorkflowPath string

	// DBPath is the absolute SQLite database path, passed to the MCP server.
	DBPath string

	// MCPServerBinary is the absolute path to the sortie binary spawned as
	// the tool server. Empty resolves to the running executable.
	MCPServerBinary string

	// ContinuationContext carries reaction continuation data to inject into
	// the first-turn prompt. Non-nil only for reaction continuations.
	ContinuationContext map[string]any

	// Posture selects the worker behavior for this dispatch. The predicate
	// methods [DispatchPosture.RunsSetupHooks] and
	// [DispatchPosture.DrivesIssueState] gate the worker guards. Derived from
	// the dispatch reaction kind via [dispatchPostureForReactionKind].
	Posture DispatchPosture

	// OnProgress relays self-review progress to the event loop. Called from
	// the worker goroutine; must be concurrency-safe. May be nil when
	// self-review is not configured.
	OnProgress func(selfReviewProgressMsg)
}

// normalizeAttempt converts the nullable attempt to a plain integer; nil
// returns 0.
func normalizeAttempt(attempt *int) int {
	if attempt == nil {
		return 0
	}
	return *attempt
}

// isActiveState case-insensitively reports whether state is in
// activeStates.
func isActiveState(state string, activeStates []string) bool {
	return slices.ContainsFunc(activeStates, func(s string) bool {
		return strings.EqualFold(s, state)
	})
}

// isTerminalState case-insensitively reports whether state is in
// terminalStates. False for an empty list.
func isTerminalState(state string, terminalStates []string) bool {
	return slices.ContainsFunc(terminalStates, func(s string) bool {
		return strings.EqualFold(s, state)
	})
}

// isTurnSuccess reports whether the turn's exit reason lets the worker
// continue to the next turn.
func isTurnSuccess(reason domain.AgentEventType) bool {
	return reason == domain.EventTurnCompleted
}

// toDomainAgentConfig converts a config-layer AgentConfig to the
// domain-layer one. kind comes from the caller rather than c.Kind because
// the dispatch-frozen kind and the configuration's default can differ.
func toDomainAgentConfig(c config.AgentConfig, kind string) domain.AgentConfig {
	return domain.AgentConfig{
		Kind:           kind,
		Command:        c.Command,
		TurnTimeoutMS:  c.TurnTimeoutMS,
		ReadTimeoutMS:  c.ReadTimeoutMS,
		StallTimeoutMS: c.StallTimeoutMS,
		StopGraceMS:    c.StopGraceMS,
	}
}

// stopSessionDeadline returns the duration a session stop may spend:
// agent.stop_grace_ms plus three graceful teardown periods, one per bounded
// drain wait the teardown can spend.
func stopSessionDeadline(cfg config.ServiceConfig) time.Duration {
	return procutil.StopGrace(cfg.Agent.StopGraceMS) + 3*procutil.DefaultDrainGrace
}

// stopSessionBestEffort terminates the session using a detached context so
// teardown proceeds even when the worker's ctx is cancelled. Errors are
// logged and swallowed.
func stopSessionBestEffort(
	ctx context.Context,
	adapter domain.AgentAdapter,
	session domain.Session,
	cfg config.ServiceConfig,
	logger *slog.Logger,
) {
	detachedCtx := context.WithoutCancel(ctx)

	stopCtx, cancel := context.WithTimeout(detachedCtx, stopSessionDeadline(cfg))
	defer cancel()

	if err := adapter.StopSession(stopCtx, session); err != nil {
		logger.Warn("stop session failed", slog.Any("error", err))
	}
}

// exitKindAtEnding classifies a worker's exit as observed when the run
// ended. cancelledAtEnding always yields WorkerExitCancelled; otherwise a
// live context yields WorkerExitError, and a done context yields
// WorkerExitError only for the ceiling's own stop cause, so a stop
// landing during teardown never retroactively cancels a finished run.
func exitKindAtEnding(ctx context.Context, cancelledAtEnding bool) WorkerExitKind {
	if cancelledAtEnding {
		return WorkerExitCancelled
	}
	if ctx.Err() == nil {
		return WorkerExitError
	}
	if errors.Is(context.Cause(ctx), errTokenCeilingStop) {
		return WorkerExitError
	}
	return WorkerExitCancelled
}

// defaultTurnTimeoutMS is the fallback bound runBoundedTurn applies for a
// non-positive turnTimeoutMS. Unreachable from a parsed configuration (the
// config layer rejects a non-positive value); it exists so an AgentConfig
// assembled in code cannot leave a turn unbounded.
const defaultTurnTimeoutMS = 3_600_000

func effectiveTurnTimeoutMS(turnTimeoutMS int) int {
	if turnTimeoutMS > 0 {
		return turnTimeoutMS
	}
	return defaultTurnTimeoutMS
}

// runBoundedTurn calls adapter.RunTurn under a deadline derived from
// turnTimeoutMS. A parent ctx already done takes priority over the deadline,
// so stall detection, reconciliation, and shutdown keep reporting their own
// cancellation; only a turn context that expired while ctx stayed live is
// reported as [domain.ErrTurnTimeout]. Every other outcome is returned
// unchanged. ctx is never replaced for the caller; identity carries the
// caller's attributes naming the turn in any log record produced.
func runBoundedTurn(
	ctx context.Context,
	adapter domain.AgentAdapter,
	session domain.Session,
	params domain.RunTurnParams,
	turnTimeoutMS int,
	logger *slog.Logger,
	identity ...slog.Attr,
) (domain.TurnResult, error) {
	effectiveMS := effectiveTurnTimeoutMS(turnTimeoutMS)
	if turnTimeoutMS <= 0 {
		attrs := append([]slog.Attr{slog.Int("configured_turn_timeout_ms", turnTimeoutMS)}, identity...)
		logger.LogAttrs(ctx, slog.LevelWarn, "non-positive turn timeout, applying default", attrs...)
	}

	turnCtx, cancel := context.WithTimeout(ctx, time.Duration(effectiveMS)*time.Millisecond)
	defer cancel()

	result, err := adapter.RunTurn(turnCtx, session, params)

	if ctx.Err() != nil {
		return result, err
	}
	if errors.Is(turnCtx.Err(), context.DeadlineExceeded) {
		attrs := append([]slog.Attr{slog.Int("turn_timeout_ms", effectiveMS)}, identity...)
		logger.LogAttrs(ctx, slog.LevelWarn, "turn timeout exceeded", attrs...)
		cause := err
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		return result, &domain.AgentError{
			Kind:    domain.ErrTurnTimeout,
			Message: fmt.Sprintf("turn exceeded the configured %d ms bound; the adapter's own report follows", effectiveMS),
			Err:     cause,
		}
	}
	return result, err
}

// foldLocalUsage applies the clamped-delta rule to the worker's token
// mirror: each component's delta against lastUsage, clamped to zero, is
// added to cumulative, and lastUsage is raised to the componentwise max.
// Confined to the worker goroutine; never touches orchestrator state.
func foldLocalUsage(usage, cumulative, lastUsage domain.TokenUsage) (newCumulative, newLastUsage domain.TokenUsage) {
	deltaInput := max(usage.InputTokens-lastUsage.InputTokens, 0)
	deltaOutput := max(usage.OutputTokens-lastUsage.OutputTokens, 0)
	deltaTotal := max(usage.TotalTokens-lastUsage.TotalTokens, 0)
	deltaCacheRead := max(usage.CacheReadTokens-lastUsage.CacheReadTokens, 0)

	newCumulative = domain.TokenUsage{
		InputTokens:     cumulative.InputTokens + deltaInput,
		OutputTokens:    cumulative.OutputTokens + deltaOutput,
		TotalTokens:     cumulative.TotalTokens + deltaTotal,
		CacheReadTokens: cumulative.CacheReadTokens + deltaCacheRead,
	}
	newLastUsage = domain.TokenUsage{
		InputTokens:     max(lastUsage.InputTokens, usage.InputTokens),
		OutputTokens:    max(lastUsage.OutputTokens, usage.OutputTokens),
		TotalTokens:     max(lastUsage.TotalTokens, usage.TotalTokens),
		CacheReadTokens: max(lastUsage.CacheReadTokens, usage.CacheReadTokens),
	}
	return newCumulative, newLastUsage
}

// applyUsageOffset adds offset to a measured usage. A working session
// counts from its own start, so the verification spend that preceded it
// must be added explicitly.
func applyUsageOffset(usage, offset domain.TokenUsage) domain.TokenUsage {
	if !hasUsage(usage) {
		return usage
	}
	return domain.TokenUsage{
		InputTokens:     usage.InputTokens + offset.InputTokens,
		OutputTokens:    usage.OutputTokens + offset.OutputTokens,
		TotalTokens:     usage.TotalTokens + offset.TotalTokens,
		CacheReadTokens: usage.CacheReadTokens + offset.CacheReadTokens,
	}
}

// RunWorkerAttempt executes a single worker attempt: it prepares the
// workspace, starts a session, runs the multi-turn loop, and tears down.
// deps.OnExit is called exactly once before returning, even on panic.
// Conforms to [WorkerFunc] when partially applied via closure over deps.
func RunWorkerAttempt(ctx context.Context, issue domain.Issue, attempt *int, deps WorkerDeps) {
	cfg := deps.ConfigFunc()
	handoffEvidencePolicy := cfg.Tracker.HandoffEvidence.Effective()
	tmpl := deps.PromptTemplateByIDFunc(deps.TemplateID)
	attemptInt := normalizeAttempt(attempt)
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// The rule-resolved kind is authoritative for run-history and
	// dispatch-comment text; callers without routing fall back to the
	// workflow-wide default.
	agentKind := deps.AgentKind
	if agentKind == "" {
		agentKind = cfg.Agent.Kind
	}

	if deps.Metrics == nil {
		deps.Metrics = &domain.NoopMetrics{}
	}

	// localMeasured mirrors the run's measurement state on the worker
	// goroutine. It starts true (a run that never enters a turn spent
	// exactly zero), flips false before the first RunTurn, and back true on
	// the first admitted measurement. localUsage/localLastUsage mirror the
	// run-cumulative counters and their watermarks; localModelName and
	// localRequestCount mirror the last relayed token_usage event's model
	// and count. This mirror never touches orchestrator state.
	// localUnaccounted is summed, not latched: each turn carries its own
	// spend-unaccounted verdict.
	localMeasured := true
	localUnaccounted := 0
	discardWarned := false
	var (
		localUsage        domain.TokenUsage
		localLastUsage    domain.TokenUsage
		localModelName    string
		localRequestCount int
	)

	// admitMeasurement reports whether the run's arrival admits a figure,
	// warning on the first rejection only.
	admitMeasurement := func() bool {
		if admitsUsageFigures(deps.UsageArrival) {
			return true
		}
		if !discardWarned {
			logger.Warn("token usage discarded: agent kind declares this session reports none",
				slog.String("agent_kind", agentKind),
			)
			discardWarned = true
		}
		return false
	}

	// foldRelayedEvent is the only code that folds a relayed event into the
	// worker mirror. A rejected measurement reports false so the caller
	// skips the state-file write.
	foldRelayedEvent := func(event domain.AgentEvent) (measurementArrived bool) {
		measurementArrived = event.Type == domain.EventTokenUsage || hasUsage(event.Usage)
		if measurementArrived && !admitMeasurement() {
			return false
		}
		if measurementArrived {
			localMeasured = true
		}
		if hasUsage(event.Usage) {
			localUsage, localLastUsage = foldLocalUsage(event.Usage, localUsage, localLastUsage)
		}
		if event.Type == domain.EventTokenUsage {
			localRequestCount++
		}
		if model := tokenUsageModel(event); model != "" {
			localModelName = model
		}
		return measurementArrived
	}

	foldTurnResult := func(result domain.TurnResult) (carriesMeasurement bool) {
		if result.SpendUnaccounted {
			localUnaccounted++
		}
		carriesMeasurement = hasUsage(result.Usage) || result.UsageMeasured
		if carriesMeasurement && !admitMeasurement() {
			carriesMeasurement = false
		} else if hasUsage(result.Usage) {
			localUsage, localLastUsage = foldLocalUsage(result.Usage, localUsage, localLastUsage)
		}
		if carriesMeasurement {
			localMeasured = true
		}
		return carriesMeasurement
	}

	// reported guards against double-reporting from the panic recovery.
	reported := false

	var acceptedSessionID string

	// cancelledAtEnding is read right after the call that last ended a run attempt.
	var cancelledAtEnding bool

	report := func(result WorkerResult) {
		result.StoppedByTokenCeiling = result.ExitKind == WorkerExitCancelled && errors.Is(context.Cause(ctx), errTokenCeilingStop)
		result.SessionID = acceptedSessionID
		reported = true
		deps.OnExit(issue.ID, result)
	}

	if tmpl == nil {
		logger.Error("prompt template lookup returned nil",
			slog.String("template_id", deps.TemplateID),
		)
		report(WorkerResult{
			IssueID:          issue.ID,
			Identifier:       issue.Identifier,
			ExitKind:         WorkerExitError,
			Error:            fmt.Errorf("prompt template %q is not registered", deps.TemplateID),
			AgentAdapter:     agentKind,
			Attempt:          attempt,
			SSHHost:          deps.SSHHost,
			Usage:            localUsage,
			UsageMeasured:    localMeasured,
			UnaccountedTurns: localUnaccounted,
			ModelName:        localModelName,
			APIRequestCount:  localRequestCount,
		})
		return
	}

	// Move the issue to the in-progress state before workspace prep.
	// Failure is non-fatal. A dispatch that does not drive issue state
	// changes none, so it is suppressed there.
	if cfg.Tracker.InProgressState != "" && deps.Posture.DrivesIssueState() {
		if strings.EqualFold(issue.State, cfg.Tracker.InProgressState) {
			logger.Debug("skipped in-progress transition, issue already in target state",
				slog.String("issue_state", issue.State),
				slog.String("in_progress_state", cfg.Tracker.InProgressState),
			)
			deps.Metrics.IncDispatchTransitions(outcomeSkipped)
		} else {
			transitionErr := deps.TrackerAdapter.TransitionIssue(ctx, issue.ID, cfg.Tracker.InProgressState)
			if transitionErr != nil {
				logger.Warn("dispatch in-progress transition failed",
					slog.String("in_progress_state", cfg.Tracker.InProgressState),
					slog.Any("error", transitionErr),
				)
				deps.Metrics.IncDispatchTransitions(outcomeError)
			} else {
				logger.Info("dispatch in-progress transition succeeded",
					slog.String("in_progress_state", cfg.Tracker.InProgressState),
				)
				deps.Metrics.IncDispatchTransitions(outcomeSuccess)
			}
		}
	}

	// Post a claim-acknowledging comment, after the transition and before
	// workspace prep. Failure is non-fatal. A dispatch that does not drive
	// issue state is not a work claim, so it posts none.
	if cfg.Tracker.Comments.OnDispatch && deps.Posture.DrivesIssueState() {
		if err := deps.TrackerAdapter.CommentIssue(ctx, issue.ID, dispatchComment); err != nil {
			logger.Warn("dispatch comment failed", slog.Any("error", err))
			deps.Metrics.IncTrackerComments("dispatch", "error")
		} else {
			logger.Info("dispatch comment posted")
			deps.Metrics.IncTrackerComments("dispatch", "success")
		}
	}

	// Pre-declared so the panic recovery defer can access them.
	var workspacePath string
	var turnsCompleted int
	var turnsStarted int
	var observedIssueState string
	var session domain.Session
	var sessionStarted bool
	var mcpConfigPath string
	var sessionStartedAt time.Time
	var handoffEvidenceBaseline *workspace.HandoffEvidenceBaseline
	var handoffEvidenceBaselineErr error

	defer func() {
		if r := recover(); r != nil {
			if sessionStarted {
				stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			}
			// A dispatch that runs no operator setup hook has no operator
			// teardown hook to run on its scratch workspace, even on panic.
			if workspacePath != "" && deps.Posture.RunsSetupHooks() {
				workspace.Finish(ctx, workspace.FinishParams{
					Path:          workspacePath,
					Identifier:    issue.Identifier,
					IssueID:       issue.ID,
					Attempt:       attemptInt,
					AfterRun:      cfg.Hooks.AfterRun,
					HookTimeoutMS: cfg.Hooks.TimeoutMS,
					Logger:        logger,
					SSHHost:       deps.SSHHost,
				})
			}
			if !reported {
				report(WorkerResult{
					IssueID:            issue.ID,
					Identifier:         issue.Identifier,
					ExitKind:           WorkerExitError,
					Error:              fmt.Errorf("worker panic: %v", r),
					TurnsCompleted:     turnsCompleted,
					TurnsStarted:       turnsStarted,
					WorkspacePath:      workspacePath,
					AgentAdapter:       agentKind,
					Attempt:            attempt,
					SSHHost:            deps.SSHHost,
					ObservedIssueState: observedIssueState,
					Usage:              localUsage,
					UsageMeasured:      localMeasured,
					UnaccountedTurns:   localUnaccounted,
					ModelName:          localModelName,
					APIRequestCount:    localRequestCount,
				})
			}
		}
	}()

	// A dispatch with no setup hooks gets a scratch directory via
	// workspace.Ensure (no clone, no build); a hook-running dispatch runs
	// the full lifecycle.
	var wsResult workspace.PrepareResult
	var err error
	if !deps.Posture.RunsSetupHooks() {
		// workspace.Ensure does not inspect the context, so honor an
		// already-cancelled dispatch here, matching the normal path's early
		// return.
		var ensureResult workspace.EnsureResult
		prepErr := ctx.Err()
		if prepErr == nil {
			ensureResult, prepErr = workspace.Ensure(cfg.Workspace.Root, issue.Identifier)
		}
		cancelledAtEnding = ctx.Err() != nil
		if prepErr != nil {
			report(WorkerResult{
				IssueID:          issue.ID,
				Identifier:       issue.Identifier,
				ExitKind:         exitKindAtEnding(ctx, cancelledAtEnding),
				Error:            fmt.Errorf("workspace preparation: %w", prepErr),
				AgentAdapter:     agentKind,
				Attempt:          attempt,
				SSHHost:          deps.SSHHost,
				Usage:            localUsage,
				UsageMeasured:    localMeasured,
				UnaccountedTurns: localUnaccounted,
				ModelName:        localModelName,
				APIRequestCount:  localRequestCount,
			})
			return
		}
		// The read-only path reuses the per-issue directory, which may hold
		// a stale .sortie/status; clear it so a stale recognized status does
		// not end the review on turn one.
		workspace.CleanupStatusFile(ensureResult.Path, logger)
		wsResult = workspace.PrepareResult(ensureResult)
	} else {
		wsResult, err = workspace.Prepare(ctx, workspace.PrepareParams{
			Root:          cfg.Workspace.Root,
			Identifier:    issue.Identifier,
			IssueID:       issue.ID,
			Attempt:       attemptInt,
			AfterCreate:   cfg.Hooks.AfterCreate,
			BeforeRun:     cfg.Hooks.BeforeRun,
			HookTimeoutMS: cfg.Hooks.TimeoutMS,
			Logger:        logger,
			SSHHost:       deps.SSHHost,
			PreRunFunc: func(wsPath string) {
				workspace.CleanupStatusFile(wsPath, logger)
			},
		})
		cancelledAtEnding = ctx.Err() != nil
		if err != nil {
			report(WorkerResult{
				IssueID:          issue.ID,
				Identifier:       issue.Identifier,
				ExitKind:         exitKindAtEnding(ctx, cancelledAtEnding),
				Error:            fmt.Errorf("workspace preparation: %w", err),
				AgentAdapter:     agentKind,
				Attempt:          attempt,
				SSHHost:          deps.SSHHost,
				Usage:            localUsage,
				UsageMeasured:    localMeasured,
				UnaccountedTurns: localUnaccounted,
				ModelName:        localModelName,
				APIRequestCount:  localRequestCount,
			})
			return
		}
	}

	workspacePath = wsResult.Path
	logger.Info("workspace prepared", slog.String("workspace", wsResult.Path))

	// finishWorkspace runs the after_run hook best-effort on every exit path
	// after successful preparation.
	finishWorkspace := func() {
		// A dispatch with no setup hook has no teardown hook to run.
		if !deps.Posture.RunsSetupHooks() {
			return
		}
		workspace.Finish(ctx, workspace.FinishParams{
			Path:          wsResult.Path,
			Identifier:    issue.Identifier,
			IssueID:       issue.ID,
			Attempt:       attemptInt,
			AfterRun:      cfg.Hooks.AfterRun,
			HookTimeoutMS: cfg.Hooks.TimeoutMS,
			Logger:        logger,
			SSHHost:       deps.SSHHost,
		})
	}

	if deps.WorkflowPath == "" {
		logger.Debug("skipped mcp config generation, workflow path empty")
	} else {
		execPath, execErr := resolveToolServerBinary(deps.MCPServerBinary)
		if execErr != nil {
			finishWorkspace()
			report(WorkerResult{
				IssueID:          issue.ID,
				Identifier:       issue.Identifier,
				ExitKind:         WorkerExitError,
				Error:            fmt.Errorf("mcp config generation: %w", execErr),
				WorkspacePath:    wsResult.Path,
				AgentAdapter:     agentKind,
				Attempt:          attempt,
				SSHHost:          deps.SSHHost,
				Usage:            localUsage,
				UsageMeasured:    localMeasured,
				UnaccountedTurns: localUnaccounted,
				ModelName:        localModelName,
				APIRequestCount:  localRequestCount,
			})
			return
		}

		settings := config.ResolveAgentSettings(cfg, agentKind, filepath.Dir(deps.WorkflowPath))

		generatedPath, genErr := GenerateMCPConfig(MCPConfigParams{
			BinaryPath:            execPath,
			WorkflowPath:          deps.WorkflowPath,
			WorkspacePath:         wsResult.Path,
			IssueID:               issue.ID,
			Identifier:            issue.Identifier,
			DBPath:                deps.DBPath,
			DispatchID:            deps.DispatchID,
			Attempt:               attempt,
			AgentKind:             agentKind,
			OperatorMCPConfigPath: settings.MCPConfigPath,
			ProcessEnv:            CollectSortieEnv(),
		})
		if genErr != nil {
			finishWorkspace()
			report(WorkerResult{
				IssueID:          issue.ID,
				Identifier:       issue.Identifier,
				ExitKind:         WorkerExitError,
				Error:            fmt.Errorf("mcp config generation: %w", genErr),
				WorkspacePath:    wsResult.Path,
				AgentAdapter:     agentKind,
				Attempt:          attempt,
				SSHHost:          deps.SSHHost,
				Usage:            localUsage,
				UsageMeasured:    localMeasured,
				UnaccountedTurns: localUnaccounted,
				ModelName:        localModelName,
				APIRequestCount:  localRequestCount,
			})
			return
		}

		mcpConfigPath = generatedPath
		logger.Info("mcp config written",
			slog.String("mcp_config_path", generatedPath),
			slog.String("agent_kind", agentKind),
			slog.String("operator_mcp_config_path", settings.MCPConfigPath))
	}

	writeDispatchIdentity := func(sessionID string) {
		if mcpConfigPath == "" || deps.DispatchID == "" {
			return
		}
		if err := workspace.WriteDispatchIdentity(wsResult.Path, workspace.DispatchIdentity{
			DispatchID: deps.DispatchID,
			SessionID:  sessionID,
		}); err != nil {
			logger.Warn("failed to write dispatch identity record", slog.Any("error", err))
		}
	}

	if ctx.Err() != nil {
		finishWorkspace()
		report(WorkerResult{
			IssueID:          issue.ID,
			Identifier:       issue.Identifier,
			ExitKind:         WorkerExitCancelled,
			WorkspacePath:    wsResult.Path,
			AgentAdapter:     agentKind,
			Attempt:          attempt,
			SSHHost:          deps.SSHHost,
			Usage:            localUsage,
			UsageMeasured:    localMeasured,
			UnaccountedTurns: localUnaccounted,
			ModelName:        localModelName,
			APIRequestCount:  localRequestCount,
		})
		return
	}

	var sshEnvNames []string
	if strings.TrimSpace(deps.SSHHost) != "" && deps.SSHEnvNamesFunc != nil {
		sshEnvNames = deps.SSHEnvNamesFunc(agentKind)
	}

	params := domain.StartSessionParams{
		WorkspacePath:            wsResult.Path,
		AgentConfig:              toDomainAgentConfig(cfg.Agent, agentKind),
		ResumeSessionID:          deps.ResumeSessionID,
		SSHHost:                  deps.SSHHost,
		SSHStrictHostKeyChecking: deps.SSHStrictHostKeyChecking,
		SSHEnvNames:              sshEnvNames,
		MCPConfigPath:            mcpConfigPath,
	}

	deps.OnEvent(issue.ID, domain.AgentEvent{
		Type:      domain.EventNotification,
		Timestamp: time.Now().UTC(),
		Message:   "verifying the agent credential",
	})

	relayVerificationEvent := func(event domain.AgentEvent) {
		foldRelayedEvent(event)
		relayType := domain.EventNotification
		var model string
		if event.Type == domain.EventTokenUsage {
			relayType = domain.EventTokenUsage
			model = event.Model
		}
		deps.OnEvent(issue.ID, domain.AgentEvent{
			Type:      relayType,
			Timestamp: event.Timestamp,
			Message:   "verifying the agent credential",
			Usage:     event.Usage,
			Model:     model,
		})
	}

	if handoffEvidencePolicy != config.HandoffEvidenceOff {
		baseline, baselineErr := workspace.CaptureHandoffEvidenceBaseline(ctx, wsResult.Path)
		if baselineErr != nil {
			handoffEvidenceBaselineErr = baselineErr
		} else {
			handoffEvidenceBaseline = &baseline
		}
	}

	redact.AddEnviron(config.DotEnvEntries())

	verificationStarted := time.Now()
	verificationResult, verificationErr := agentcore.VerifyCredential(ctx, deps.AgentAdapter, agentcore.CredentialVerification{
		Session:   params,
		Issue:     issue,
		TurnBound: time.Duration(effectiveTurnTimeoutMS(cfg.Agent.TurnTimeoutMS)) * time.Millisecond,
		StopBound: stopSessionDeadline(cfg),
		OnRequest: func() { localMeasured = false },
		OnEvent:   relayVerificationEvent,
		Logger:    logger,
	})
	cancelledAtEnding = ctx.Err() != nil

	foldTurnResult(verificationResult)

	verificationSpend := localLastUsage

	if verificationErr != nil {
		finishWorkspace()
		report(WorkerResult{
			IssueID:          issue.ID,
			Identifier:       issue.Identifier,
			ExitKind:         exitKindAtEnding(ctx, cancelledAtEnding),
			Error:            fmt.Errorf("agent session start: %w", verificationErr),
			WorkspacePath:    wsResult.Path,
			AgentAdapter:     agentKind,
			Attempt:          attempt,
			SSHHost:          deps.SSHHost,
			Usage:            localUsage,
			UsageMeasured:    localMeasured,
			UnaccountedTurns: localUnaccounted,
			ModelName:        localModelName,
			APIRequestCount:  localRequestCount,
		})
		return
	}

	logger.Info("agent credential verified", slog.Int64("duration_ms", time.Since(verificationStarted).Milliseconds()))

	session, err = deps.AgentAdapter.StartSession(ctx, params)
	cancelledAtEnding = ctx.Err() != nil
	if err != nil {
		finishWorkspace()
		report(WorkerResult{
			IssueID:          issue.ID,
			Identifier:       issue.Identifier,
			ExitKind:         exitKindAtEnding(ctx, cancelledAtEnding),
			Error:            fmt.Errorf("agent session start: %w", err),
			WorkspacePath:    wsResult.Path,
			AgentAdapter:     agentKind,
			Attempt:          attempt,
			SSHHost:          deps.SSHHost,
			Usage:            localUsage,
			UsageMeasured:    localMeasured,
			UnaccountedTurns: localUnaccounted,
			ModelName:        localModelName,
			APIRequestCount:  localRequestCount,
		})
		return
	}

	sessionStarted = true
	logger = logging.WithSession(logger, session.ID)
	logger.Info("agent session started")

	acceptedSessionID = session.ID
	writeDispatchIdentity(acceptedSessionID)

	sessionStartedAt = time.Now().UTC()

	maxTurns := cfg.Agent.MaxTurns
	if maxTurns < 1 {
		logger.Warn("clamped agent max_turns to 1", slog.Int("configured_max_turns", cfg.Agent.MaxTurns))
		maxTurns = 1
	}
	turnNumber := 1
	activeStates := cfg.Tracker.ActiveStates

	publishWorkerState := func(turn int) error {
		if mcpConfigPath == "" {
			return nil
		}
		return writeWorkerState(wsResult.Path, workerState{
			TurnNumber: turn,
			MaxTurns:   maxTurns,
			Attempt:    attempt,
			StartedAt:  sessionStartedAt.Format(time.RFC3339Nano),
		}.withTokens(localUsage, localMeasured))
	}

	acceptSessionID := func(id string) bool {
		if id == "" || id == acceptedSessionID {
			return false
		}
		previous := acceptedSessionID
		logger.Info("agent session id accepted",
			slog.String("previous_session_id", previous),
			slog.String("accepted_session_id", id),
		)
		acceptedSessionID = id
		return true
	}

	relayTurnEvent := func(event domain.AgentEvent) {
		// Defensive copy in the worker goroutine, before the event crosses
		// the goroutine boundary, so the orchestrator never iterates a map
		// the adapter may still mutate.
		if event.RateLimits != nil {
			event.RateLimits = maps.Clone(event.RateLimits)
		}
		event.Usage = applyUsageOffset(event.Usage, verificationSpend)
		if foldRelayedEvent(event) {
			if err := publishWorkerState(turnNumber); err != nil {
				logger.Warn("failed to write status state file on token event", slog.Any("error", err))
			}
		}
		if event.Type == domain.EventSessionStarted && event.SessionID != "" {
			acceptSessionID(event.SessionID)
			writeDispatchIdentity(acceptedSessionID)
		}
		deps.OnEvent(issue.ID, event)
	}

	// An adapter may report a session's only measurement here rather than
	// through an event; on the last turn no later write would carry it, so
	// the file would keep denying a measurement that exists.
	handleTurnResult := func(result domain.TurnResult) {
		result.Usage = applyUsageOffset(result.Usage, verificationSpend)
		if foldTurnResult(result) {
			if err := publishWorkerState(turnNumber); err != nil {
				logger.Warn("failed to write status state file after turn result", slog.Any("error", err))
			}
		}
		if acceptSessionID(result.SessionID) {
			writeDispatchIdentity(acceptedSessionID)
		}
	}

	// pendingSoftStopReason holds the recognized status token once a
	// post-turn read admits one, so the single post-loop teardown can report
	// it after self-review has had a chance to run.
	var pendingSoftStopReason string

	if err := publishWorkerState(0); err != nil {
		logger.Warn("failed to write status state file at session start", slog.Any("error", err))
	}

	for {
		if ctx.Err() != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			report(WorkerResult{
				IssueID:            issue.ID,
				Identifier:         issue.Identifier,
				ExitKind:           WorkerExitCancelled,
				TurnsCompleted:     turnsCompleted,
				TurnsStarted:       turnsStarted,
				WorkspacePath:      wsResult.Path,
				AgentAdapter:       agentKind,
				Attempt:            attempt,
				SSHHost:            deps.SSHHost,
				ObservedIssueState: observedIssueState,
				Usage:              localUsage,
				UsageMeasured:      localMeasured,
				UnaccountedTurns:   localUnaccounted,
				ModelName:          localModelName,
				APIRequestCount:    localRequestCount,
			})
			return
		}

		issueMap := issue.ToTemplateMap()
		var renderOpts []prompt.RenderOption
		if turnNumber == 1 {
			contCtx := deps.ContinuationContext
			if contCtx == nil {
				contCtx = ContinuationFromContext(ctx)
			}
			if contCtx != nil {
				renderOpts = append(renderOpts, prompt.WithContinuationContext(contCtx))
			}
		}
		rendered, err := prompt.BuildTurnPrompt(tmpl, issueMap, attemptInt, turnNumber, maxTurns, renderOpts...)
		cancelledAtEnding = ctx.Err() != nil
		if err != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			report(WorkerResult{
				IssueID:            issue.ID,
				Identifier:         issue.Identifier,
				ExitKind:           exitKindAtEnding(ctx, cancelledAtEnding),
				Error:              fmt.Errorf("prompt render (turn %d): %w", turnNumber, err),
				TurnsCompleted:     turnsCompleted,
				TurnsStarted:       turnsStarted,
				WorkspacePath:      wsResult.Path,
				AgentAdapter:       agentKind,
				Attempt:            attempt,
				SSHHost:            deps.SSHHost,
				ObservedIssueState: observedIssueState,
				Usage:              localUsage,
				UsageMeasured:      localMeasured,
				UnaccountedTurns:   localUnaccounted,
				ModelName:          localModelName,
				APIRequestCount:    localRequestCount,
			})
			return
		}

		if turnNumber == 1 {
			remote := strings.TrimSpace(deps.SSHHost) != ""
			switch {
			case deps.AgentToolChannelFunc == nil:
				logger.Warn("tool channel unknown for agent kind, withholding tool advertisement",
					slog.String("agent_kind", agentKind))
			case !deps.AgentToolChannelFunc(agentKind, remote):
				logger.Info("no tool execution channel for this session, withholding tool advertisement",
					slog.String("agent_kind", agentKind), slog.Bool("remote", remote))
			default:
				if deps.SessionToolRegistryFunc != nil {
					sessionReg, err := deps.SessionToolRegistryFunc(ctx, issue.ID, wsResult.Path)
					if err != nil {
						logger.Warn("failed to build session tool advertisement", slog.Any("error", err))
					} else if sessionReg != nil && sessionReg.Len() > 0 {
						rendered += "\n\n" + buildToolAdvertisement(sessionReg, cfg.Tracker.Project)
					}
				} else if deps.ToolRegistry != nil && deps.ToolRegistry.Len() > 0 {
					rendered += "\n\n" + buildToolAdvertisement(deps.ToolRegistry, cfg.Tracker.Project)
				}
			}
			rendered += "\n\n" + prompt.RuntimeStatusSuffix
		}

		logger.Info("turn started", slog.Int("turn_number", turnNumber), slog.Int("max_turns", maxTurns))
		turnsStarted++
		if deps.OnTurnStarted != nil {
			deps.OnTurnStarted(issue.ID, turnsStarted)
		}

		writeDispatchIdentity(acceptedSessionID)

		if err := publishWorkerState(turnNumber); err != nil {
			logger.Warn("failed to write status state file at turn start",
				slog.Int("turn_number", turnNumber),
				slog.Any("error", err),
			)
		}

		turnResult, err := runBoundedTurn(ctx, deps.AgentAdapter, session, domain.RunTurnParams{
			Prompt:  rendered,
			Issue:   issue,
			OnEvent: relayTurnEvent,
		}, cfg.Agent.TurnTimeoutMS, logger, slog.Int("turn_number", turnNumber))
		cancelledAtEnding = ctx.Err() != nil

		handleTurnResult(turnResult)

		if err != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			report(WorkerResult{
				IssueID:            issue.ID,
				Identifier:         issue.Identifier,
				ExitKind:           exitKindAtEnding(ctx, cancelledAtEnding),
				Error:              fmt.Errorf("agent turn %d: %w", turnNumber, err),
				TurnsCompleted:     turnsCompleted,
				TurnsStarted:       turnsStarted,
				WorkspacePath:      wsResult.Path,
				AgentAdapter:       agentKind,
				Attempt:            attempt,
				SSHHost:            deps.SSHHost,
				ObservedIssueState: observedIssueState,
				Usage:              localUsage,
				UsageMeasured:      localMeasured,
				UnaccountedTurns:   localUnaccounted,
				ModelName:          localModelName,
				APIRequestCount:    localRequestCount,
			})
			return
		}

		turnsCompleted++
		logger.Info("turn completed", slog.Int("turn_number", turnNumber), slog.Int("max_turns", maxTurns))

		// Non-success exit reasons (timeout, max_tokens, etc.) are terminal.
		if !isTurnSuccess(turnResult.ExitReason) {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			logger.Warn("turn exit reason indicates failure",
				slog.Int("turn_number", turnNumber),
				slog.Any("exit_reason", turnResult.ExitReason),
			)
			report(WorkerResult{
				IssueID:            issue.ID,
				Identifier:         issue.Identifier,
				ExitKind:           exitKindAtEnding(ctx, cancelledAtEnding),
				Error:              fmt.Errorf("agent turn %d ended: %s", turnNumber, turnResult.ExitReason),
				TurnsCompleted:     turnsCompleted,
				TurnsStarted:       turnsStarted,
				WorkspacePath:      wsResult.Path,
				AgentAdapter:       agentKind,
				Attempt:            attempt,
				SSHHost:            deps.SSHHost,
				ObservedIssueState: observedIssueState,
				Usage:              localUsage,
				UsageMeasured:      localMeasured,
				UnaccountedTurns:   localUnaccounted,
				ModelName:          localModelName,
				APIRequestCount:    localRequestCount,
			})
			return
		}

		// Read the A2O status file to detect agent-reported blockage before
		// a wasted tracker call. A recognized signal leaves the loop so the
		// single post-loop teardown is the only exit path.
		statusSignal := workspace.ReadStatusFile(wsResult.Path, logger)
		if statusSignal.IsRecognized() {
			pendingSoftStopReason = string(statusSignal)
			break
		}

		// A dispatch that does not drive issue state skips the per-turn
		// refresh and its active-state gate, resting only on max_turns or the
		// agent's own .sortie/status signal.
		if deps.Posture.DrivesIssueState() {
			refreshed, err := deps.TrackerAdapter.FetchIssueStatesByIDs(runContextFrom(ctx), []string{issue.ID})
			cancelledAtEnding = ctx.Err() != nil
			if err != nil {
				stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
				finishWorkspace()
				report(WorkerResult{
					IssueID:            issue.ID,
					Identifier:         issue.Identifier,
					ExitKind:           exitKindAtEnding(ctx, cancelledAtEnding),
					Error:              fmt.Errorf("issue state refresh (turn %d): %w", turnNumber, err),
					TurnsCompleted:     turnsCompleted,
					TurnsStarted:       turnsStarted,
					WorkspacePath:      wsResult.Path,
					AgentAdapter:       agentKind,
					Attempt:            attempt,
					SSHHost:            deps.SSHHost,
					ObservedIssueState: observedIssueState,
					Usage:              localUsage,
					UsageMeasured:      localMeasured,
					UnaccountedTurns:   localUnaccounted,
					ModelName:          localModelName,
					APIRequestCount:    localRequestCount,
				})
				return
			}

			if stateStr, ok := refreshed[issue.ID]; ok {
				issue.State = stateStr
				observedIssueState = stateStr
			}

			logger.Info("issue state refreshed", slog.String("refreshed_state", issue.State))

			if !isActiveState(issue.State, activeStates) {
				break
			}
		}

		if turnNumber >= maxTurns {
			break
		}

		turnNumber++
	}

	// Self-review phase: verify and iterate before final exit, on freshly
	// reloaded config. A pending status reason admits the phase only when it
	// is empty or names the completion signal; a pending blocked reason
	// skips it.
	reviewCfg := deps.ConfigFunc()
	var reviewMeta *domain.ReviewMetadata
	var phaseErr error
	var phaseCut bool

	signalAdmits := pendingSoftStopReason == "" ||
		pendingSoftStopReason == string(workspace.StatusNeedsHumanReview) ||
		pendingSoftStopReason == string(workspace.StatusNoChangeNeeded)
	selfReviewAdmitted := reviewCfg.SelfReview.Enabled && isActiveState(issue.State, activeStates) && deps.Posture.DrivesIssueState() && signalAdmits

	if selfReviewAdmitted && ctx.Err() == nil {
		if pendingSoftStopReason != "" {
			logger.Info("agent signaled a status admitting self-review, entering the phase",
				slog.String("status", pendingSoftStopReason),
				slog.Int("turns_completed", turnsCompleted),
			)
			workspace.CleanupStatusFile(wsResult.Path, logger)
		}
		var phaseSignal workspace.StatusSignal
		reviewMeta, phaseSignal, cancelledAtEnding, phaseErr = runSelfReviewLoop(ctx, RunSelfReviewParams{
			Session:       session,
			Issue:         issue,
			WorkspacePath: wsResult.Path,
			Config:        reviewCfg.SelfReview,
			AgentAdapter:  deps.AgentAdapter,
			OnEvent: func(_ string, event domain.AgentEvent) {
				relayTurnEvent(event)
			},
			OnTurnResult: handleTurnResult,
			OnProgress:   deps.OnProgress,
			OnTurnStarted: func() {
				turnsStarted++
				writeDispatchIdentity(acceptedSessionID)
				if deps.OnTurnStarted != nil {
					deps.OnTurnStarted(issue.ID, turnsStarted)
				}
			},
			Logger:         logger,
			Metrics:        deps.Metrics,
			TurnsCompleted: &turnsCompleted,
			TurnTimeoutMS:  cfg.Agent.TurnTimeoutMS,
		})
		// A blocked signal read inside the phase becomes the run's soft-stop
		// reason unconditionally.
		if phaseSignal == workspace.StatusBlocked {
			pendingSoftStopReason = string(workspace.StatusBlocked)
		}
		pendingSoftStopReason = retractUnconfirmedNoChangeDeclaration(pendingSoftStopReason, reviewMeta, logger)
		phaseCut = phaseErr == nil && cancelledAtEnding
	} else if selfReviewAdmitted {
		phaseCut = true
	} else if pendingSoftStopReason != "" {
		logger.Info("agent signaled status, exiting worker",
			slog.String("status", pendingSoftStopReason),
			slog.Int("turns_completed", turnsCompleted),
		)
	}

	selfReviewStatus := "disabled"
	selfReviewSummaryPath := ""
	if reviewMeta != nil {
		switch {
		case reviewMeta.FinalVerdict == "pass":
			selfReviewStatus = "passed"
		case reviewMeta.CapReached:
			selfReviewStatus = "cap_reached"
		default:
			selfReviewStatus = "error"
		}
		if f, summaryErr := workspacekit.OpenSortieFile(wsResult.Path, "review_summary.md"); summaryErr == nil {
			_ = f.Close() //nolint:errcheck // the handle only confirms the summary is readable; nothing is read from it here
			selfReviewSummaryPath = filepath.Join(wsResult.Path, workspacekit.SortieDir, "review_summary.md")
		}
	}

	// A self-review turn's deadline expiry fails the attempt. Teardown here
	// mirrors the normal exit's, carrying the phase's own status and summary
	// path rather than the empty values finishWorkspace() would pass.
	if phaseErr != nil {
		stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
		if deps.Posture.RunsSetupHooks() {
			workspace.Finish(ctx, workspace.FinishParams{
				Path:                  wsResult.Path,
				Identifier:            issue.Identifier,
				IssueID:               issue.ID,
				Attempt:               attemptInt,
				AfterRun:              cfg.Hooks.AfterRun,
				HookTimeoutMS:         cfg.Hooks.TimeoutMS,
				Logger:                logger,
				SSHHost:               deps.SSHHost,
				SelfReviewStatus:      selfReviewStatus,
				SelfReviewSummaryPath: selfReviewSummaryPath,
			})
		}
		report(WorkerResult{
			IssueID:            issue.ID,
			Identifier:         issue.Identifier,
			ExitKind:           exitKindAtEnding(ctx, cancelledAtEnding),
			Error:              phaseErr,
			ReviewMetadata:     reviewMeta,
			TurnsCompleted:     turnsCompleted,
			TurnsStarted:       turnsStarted,
			WorkspacePath:      wsResult.Path,
			AgentAdapter:       agentKind,
			Attempt:            attempt,
			SSHHost:            deps.SSHHost,
			ObservedIssueState: observedIssueState,
			Usage:              localUsage,
			UsageMeasured:      localMeasured,
			UnaccountedTurns:   localUnaccounted,
			ModelName:          localModelName,
			APIRequestCount:    localRequestCount,
		})
		return
	}

	// A ceiling stop that cut the phase short, or kept it from starting,
	// exits as a ceiling stop; any other cancellation falls through below.
	if phaseCut && errors.Is(context.Cause(ctx), errTokenCeilingStop) {
		stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
		if deps.Posture.RunsSetupHooks() {
			workspace.Finish(ctx, workspace.FinishParams{
				Path:                  wsResult.Path,
				Identifier:            issue.Identifier,
				IssueID:               issue.ID,
				Attempt:               attemptInt,
				AfterRun:              cfg.Hooks.AfterRun,
				HookTimeoutMS:         cfg.Hooks.TimeoutMS,
				Logger:                logger,
				SSHHost:               deps.SSHHost,
				SelfReviewStatus:      selfReviewStatus,
				SelfReviewSummaryPath: selfReviewSummaryPath,
			})
		}
		report(WorkerResult{
			IssueID:            issue.ID,
			Identifier:         issue.Identifier,
			ExitKind:           WorkerExitCancelled,
			ReviewMetadata:     reviewMeta,
			TurnsCompleted:     turnsCompleted,
			TurnsStarted:       turnsStarted,
			WorkspacePath:      wsResult.Path,
			AgentAdapter:       agentKind,
			Attempt:            attempt,
			SSHHost:            deps.SSHHost,
			ObservedIssueState: observedIssueState,
			Usage:              localUsage,
			UsageMeasured:      localMeasured,
			UnaccountedTurns:   localUnaccounted,
			ModelName:          localModelName,
			APIRequestCount:    localRequestCount,
		})
		return
	}

	stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
	if deps.Posture.RunsSetupHooks() {
		workspace.Finish(ctx, workspace.FinishParams{
			Path:                  wsResult.Path,
			Identifier:            issue.Identifier,
			IssueID:               issue.ID,
			Attempt:               attemptInt,
			AfterRun:              cfg.Hooks.AfterRun,
			HookTimeoutMS:         cfg.Hooks.TimeoutMS,
			Logger:                logger,
			SSHHost:               deps.SSHHost,
			SelfReviewStatus:      selfReviewStatus,
			SelfReviewSummaryPath: selfReviewSummaryPath,
		})
	}

	logger.Info("worker exiting",
		slog.Any("exit_kind", WorkerExitNormal),
		slog.Int("turns_completed", turnsCompleted),
	)

	report(WorkerResult{
		IssueID:                      issue.ID,
		Identifier:                   issue.Identifier,
		ExitKind:                     WorkerExitNormal,
		TurnsCompleted:               turnsCompleted,
		TurnsStarted:                 turnsStarted,
		WorkspacePath:                wsResult.Path,
		HandoffEvidencePolicy:        handoffEvidencePolicy,
		HandoffEvidenceBaseline:      handoffEvidenceBaseline,
		HandoffEvidenceBaselineError: handoffEvidenceBaselineErr,
		AgentAdapter:                 agentKind,
		Attempt:                      attempt,
		SSHHost:                      deps.SSHHost,
		SoftStop:                     pendingSoftStopReason != "",
		SoftStopReason:               pendingSoftStopReason,
		ReviewMetadata:               reviewMeta,
		ObservedIssueState:           observedIssueState,
		Usage:                        localUsage,
		ModelName:                    localModelName,
		APIRequestCount:              localRequestCount,
		UsageMeasured:                localMeasured,
		UnaccountedTurns:             localUnaccounted,
	})
}

// retractUnconfirmedNoChangeDeclaration clears pendingSoftStopReason when it
// declares no change was needed and the self-review phase did not confirm
// it. Confirmation requires exactly one iteration ending on "pass" with no
// failed verification; a phase that ran no verification still confirms. A
// nil reviewMeta (the gate did not admit the run) passes through unchanged.
func retractUnconfirmedNoChangeDeclaration(pendingSoftStopReason string, reviewMeta *domain.ReviewMetadata, logger *slog.Logger) string {
	if pendingSoftStopReason != string(workspace.StatusNoChangeNeeded) {
		return pendingSoftStopReason
	}
	if reviewMeta == nil {
		return pendingSoftStopReason
	}
	for _, iteration := range reviewMeta.Iterations {
		for _, result := range iteration.VerificationResults {
			if result.ExitCode != 0 || result.TimedOut {
				logger.Info("no-change declaration retracted",
					slog.String("cause", "verification"),
					slog.String("command", result.Command),
					slog.Int("exit_code", result.ExitCode),
					slog.Bool("timed_out", result.TimedOut),
				)
				return ""
			}
		}
	}
	if reviewMeta.TotalIterations != 1 || reviewMeta.FinalVerdict != "pass" {
		logger.Info("no-change declaration retracted",
			slog.String("cause", "phase_unconfirmed"),
			slog.Int("iterations", reviewMeta.TotalIterations),
			slog.String("final_verdict", reviewMeta.FinalVerdict),
		)
		return ""
	}
	return pendingSoftStopReason
}

// buildToolAdvertisement formats a Markdown section documenting the
// registry's tools, appended to the first-turn prompt.
func buildToolAdvertisement(reg *domain.ToolRegistry, project string) string {
	var sb strings.Builder
	sb.WriteString("## Available Sortie tools\n\n")
	if project != "" {
		sb.WriteString("All operations are scoped to project: ")
		sb.WriteString(project)
		sb.WriteString("\n\n")
	}

	for _, tool := range reg.List() {
		sb.WriteString("### ")
		sb.WriteString(tool.Name())
		sb.WriteString("\n\n")
		sb.WriteString(tool.Description())
		sb.WriteString("\n\n")
		sb.WriteString("Input schema:\n```json\n")
		sb.Write(tool.InputSchema())
		sb.WriteString("\n```\n\n")
	}

	sb.WriteString("All responses are JSON: {\"success\": true, \"data\": ...} or {\"success\": false, \"error\": {\"kind\": \"...\", \"message\": \"...\"}}.\n")

	return sb.String()
}

// dispatchComment is the tracker comment text posted on every session
// dispatch, for every agent kind and every tracker kind alike: a
// business reader of the issue needs only that a session started, not
// an internal integration name or a dispatch counter.
const dispatchComment = "Sortie session started."
