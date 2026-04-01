package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// WorkerExitKind classifies how the worker attempt terminated.
type WorkerExitKind string

const (
	// WorkerExitNormal indicates the turn loop completed without error.
	// The issue may still be active (max_turns reached) or may have
	// transitioned to a non-active state.
	WorkerExitNormal WorkerExitKind = "normal"

	// WorkerExitError indicates the worker encountered a fatal error
	// during workspace preparation, prompt rendering, agent session
	// lifecycle, or tracker state refresh.
	WorkerExitError WorkerExitKind = "error"

	// WorkerExitCancelled indicates the worker's context was cancelled
	// (reconciliation kill, stall timeout, or graceful shutdown).
	WorkerExitCancelled WorkerExitKind = "cancelled"
)

// WorkerResult is the terminal outcome of a single worker attempt,
// delivered to the orchestrator via [WorkerDeps.OnExit].
type WorkerResult struct {
	// IssueID is the tracker-internal issue ID.
	IssueID string

	// Identifier is the human-readable ticket key.
	Identifier string

	// ExitKind classifies the exit as normal, error, or cancelled.
	ExitKind WorkerExitKind

	// Error is the error that caused an abnormal exit. Nil for normal
	// exits and context cancellations.
	Error error

	// TurnsCompleted is the number of turns that ran to completion
	// (received a TurnResult) before the worker exited.
	TurnsCompleted int

	// SessionID is the adapter-assigned session identifier. Empty if
	// the worker exited before starting a session. The exit handler
	// uses this to populate RunningEntry.SessionID and to enable
	// session continuity on continuation retries.
	SessionID string

	// WorkspacePath is the workspace directory used for this attempt.
	// Empty if workspace preparation failed.
	WorkspacePath string

	// AgentAdapter is the agent adapter kind string (from config.Agent.Kind).
	AgentAdapter string

	// Attempt is the retry attempt parameter passed to the worker.
	Attempt *int

	// SSHHost is the SSH host the worker executed on. Empty for local
	// execution. Copied from [WorkerDeps] at exit for host pool release.
	SSHHost string

	// StartedAt is copied from the RunningEntry (set by DispatchIssue).
	// The worker does not set this — it is populated by the exit
	// handler from the running map entry.
	StartedAt time.Time
}

// WorkerDeps holds the collaborators injected into the worker attempt
// function. The orchestrator constructs this once and shares it
// across all workers. All fields are required unless documented as
// optional (e.g. ToolRegistry).
type WorkerDeps struct {
	// TrackerAdapter fetches issue states for mid-turn re-checks.
	TrackerAdapter domain.TrackerAdapter

	// AgentAdapter manages agent session lifecycle.
	AgentAdapter domain.AgentAdapter

	// ConfigFunc returns the current effective config. Called at the
	// start of each worker attempt so that dynamically reloaded
	// values take effect for new attempts.
	ConfigFunc func() config.ServiceConfig

	// PromptTemplateFunc returns the current compiled prompt template.
	// Called once per attempt at the start.
	PromptTemplateFunc func() *prompt.Template

	// OnEvent relays agent events to the orchestrator's serialized
	// event loop. Called from the worker goroutine; must be safe for
	// concurrent use.
	OnEvent func(issueID string, event domain.AgentEvent)

	// OnExit reports the worker's terminal outcome to the orchestrator.
	// Called exactly once, as the last action before the goroutine
	// returns. Must be safe for concurrent use.
	OnExit func(issueID string, result WorkerResult)

	// ResumeSessionID is the session ID from a previous worker attempt
	// for the same issue. Non-empty on continuation retries so the
	// agent adapter can resume the conversation. The orchestrator
	// populates this from the previous RunningEntry.SessionID.
	ResumeSessionID string

	// ToolRegistry holds the tools available to agent sessions. May
	// be nil when no tools are registered. Read-only after construction.
	ToolRegistry *domain.ToolRegistry

	// Logger is the structured logger with issue-scoped context fields
	// already attached (issue_id, issue_identifier).
	Logger *slog.Logger

	// SSHHost is the SSH destination for this worker's agent sessions.
	// Empty for local execution. Set by the orchestrator when dispatching
	// to a remote host.
	SSHHost string

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking
	// value for this worker's agent sessions. Empty means "accept-new".
	SSHStrictHostKeyChecking string

	// Metrics records dispatch-time instrumentation counters.
	// Always non-nil: NewOrchestrator falls back to NoopMetrics
	// before wiring WorkerDeps via makeWorkerFn.
	Metrics domain.Metrics

	// WorkflowPath is the absolute path to the active WORKFLOW.md
	// file. Used by MCP config generation to pass --workflow to the
	// mcp-server subcommand. Empty disables MCP config generation.
	WorkflowPath string

	// DBPath is the absolute path to the SQLite database file.
	// Passed to the MCP server via the config env field for future
	// Tier 1 tool access.
	DBPath string
}

// normalizeAttempt converts the nullable attempt to a plain integer.
// nil returns 0; non-nil returns the dereferenced value.
func normalizeAttempt(attempt *int) int {
	if attempt == nil {
		return 0
	}
	return *attempt
}

// isActiveState performs a case-insensitive check of state against the
// active states list. Returns true if state is in the active set.
func isActiveState(state string, activeStates []string) bool {
	lower := strings.ToLower(state)
	for _, s := range activeStates {
		if strings.ToLower(s) == lower {
			return true
		}
	}
	return false
}

// isTurnSuccess returns true when the turn result exit reason indicates
// the turn completed successfully and the worker may continue to the
// next turn.
func isTurnSuccess(reason domain.AgentEventType) bool {
	return reason == domain.EventTurnCompleted
}

// toDomainAgentConfig converts a config-layer AgentConfig to the
// domain-layer AgentConfig expected by agent adapters.
//
// Update this function when adding fields to config.AgentConfig or
// domain.AgentConfig.
func toDomainAgentConfig(c config.AgentConfig) domain.AgentConfig {
	return domain.AgentConfig{
		Kind:           c.Kind,
		Command:        c.Command,
		TurnTimeoutMS:  c.TurnTimeoutMS,
		ReadTimeoutMS:  c.ReadTimeoutMS,
		StallTimeoutMS: c.StallTimeoutMS,
	}
}

// stopSessionBestEffort terminates the agent session using a detached
// context so that teardown proceeds even when the worker's ctx is
// cancelled. The timeout is derived from the agent's ReadTimeoutMS
// config (default: 10 000 ms). Errors are logged and swallowed.
func stopSessionBestEffort(
	ctx context.Context,
	adapter domain.AgentAdapter,
	session domain.Session,
	cfg config.ServiceConfig,
	logger *slog.Logger,
) {
	detachedCtx := context.WithoutCancel(ctx)

	timeoutMS := cfg.Agent.ReadTimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = 10_000
	}

	stopCtx, cancel := context.WithTimeout(detachedCtx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	if err := adapter.StopSession(stopCtx, session); err != nil {
		logger.Warn("stop session failed", slog.Any("error", err))
	}
}

// exitKindForErr returns WorkerExitCancelled if the context is done,
// otherwise WorkerExitError.
func exitKindForErr(ctx context.Context) WorkerExitKind {
	if ctx.Err() != nil {
		return WorkerExitCancelled
	}
	return WorkerExitError
}

// RunWorkerAttempt executes a single worker attempt for the given issue.
// It prepares the workspace, starts an agent session, runs the
// multi-turn loop, and performs teardown. The function calls
// deps.OnExit exactly once before returning, even on panics.
//
// RunWorkerAttempt conforms to [WorkerFunc] when partially applied via
// closure over deps.
func RunWorkerAttempt(ctx context.Context, issue domain.Issue, attempt *int, deps WorkerDeps) {
	cfg := deps.ConfigFunc()
	tmpl := deps.PromptTemplateFunc()
	attemptInt := normalizeAttempt(attempt)
	logger := deps.Logger

	if deps.Metrics == nil {
		deps.Metrics = &domain.NoopMetrics{}
	}

	// Dispatch-time in-progress transition: move the issue to the
	// configured in-progress tracker state before workspace prep.
	// Failure is non-fatal — the worker continues regardless.
	if cfg.Tracker.InProgressState != "" {
		if strings.EqualFold(issue.State, cfg.Tracker.InProgressState) {
			logger.Debug("issue already in in-progress state, skipping transition",
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

	// Dispatch comment: post a tracker comment acknowledging claim.
	// Fires after in-progress transition, before workspace preparation.
	// Failure is non-fatal — the worker continues regardless.
	if cfg.Tracker.Comments.OnDispatch {
		text := buildDispatchComment(cfg.Agent.Kind, attemptInt)
		if err := deps.TrackerAdapter.CommentIssue(ctx, issue.ID, text); err != nil {
			logger.Warn("dispatch comment failed", slog.Any("error", err))
			deps.Metrics.IncTrackerComments("dispatch", "error")
		} else {
			logger.Info("dispatch comment posted")
			deps.Metrics.IncTrackerComments("dispatch", "success")
		}
	}

	// reported tracks whether OnExit has been called. The deferred
	// panic recovery checks this to avoid double-reporting.
	reported := false

	// Pre-declared so the panic recovery defer can access them.
	var workspacePath string
	var sessionID string
	var turnsCompleted int
	var session domain.Session
	var sessionStarted bool
	var mcpConfigPath string

	defer func() {
		if r := recover(); r != nil {
			if sessionStarted {
				stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			}
			if workspacePath != "" {
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
				deps.OnExit(issue.ID, WorkerResult{
					IssueID:        issue.ID,
					Identifier:     issue.Identifier,
					ExitKind:       WorkerExitError,
					Error:          fmt.Errorf("worker panic: %v", r),
					TurnsCompleted: turnsCompleted,
					SessionID:      sessionID,
					WorkspacePath:  workspacePath,
					AgentAdapter:   cfg.Agent.Kind,
					Attempt:        attempt,
					SSHHost:        deps.SSHHost,
				})
			}
		}
	}()

	// Phase 1: Workspace Preparation.
	wsResult, err := workspace.Prepare(ctx, workspace.PrepareParams{
		Root:          cfg.Workspace.Root,
		Identifier:    issue.Identifier,
		IssueID:       issue.ID,
		Attempt:       attemptInt,
		AfterCreate:   cfg.Hooks.AfterCreate,
		BeforeRun:     cfg.Hooks.BeforeRun,
		HookTimeoutMS: cfg.Hooks.TimeoutMS,
		Logger:        logger,
		SSHHost:       deps.SSHHost,
	})
	if err != nil {
		reported = true
		deps.OnExit(issue.ID, WorkerResult{
			IssueID:      issue.ID,
			Identifier:   issue.Identifier,
			ExitKind:     exitKindForErr(ctx),
			Error:        fmt.Errorf("workspace preparation: %w", err),
			AgentAdapter: cfg.Agent.Kind,
			Attempt:      attempt,
			SSHHost:      deps.SSHHost,
		})
		return
	}

	workspacePath = wsResult.Path
	logger.Info("workspace prepared", slog.String("workspace", wsResult.Path))

	// finishWorkspace is a helper that runs the after_run hook
	// best-effort. Called on every exit path after successful
	// workspace preparation.
	finishWorkspace := func() {
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

	// Phase 1.5: MCP Config Generation.
	if deps.WorkflowPath == "" {
		logger.Debug("workflow path empty, skipping MCP config generation")
	} else {
		execPath, execErr := os.Executable()
		if execErr != nil {
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:       issue.ID,
				Identifier:    issue.Identifier,
				ExitKind:      WorkerExitError,
				Error:         fmt.Errorf("MCP config generation: resolve executable: %w", execErr),
				WorkspacePath: wsResult.Path,
				AgentAdapter:  cfg.Agent.Kind,
				Attempt:       attempt,
				SSHHost:       deps.SSHHost,
			})
			return
		}

		execPath, execErr = filepath.EvalSymlinks(execPath)
		if execErr != nil {
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:       issue.ID,
				Identifier:    issue.Identifier,
				ExitKind:      WorkerExitError,
				Error:         fmt.Errorf("MCP config generation: resolve symlinks: %w", execErr),
				WorkspacePath: wsResult.Path,
				AgentAdapter:  cfg.Agent.Kind,
				Attempt:       attempt,
				SSHHost:       deps.SSHHost,
			})
			return
		}

		// Resolve operator MCP config path from extensions.
		var operatorPath string
		if extMap, ok := cfg.Extensions[cfg.Agent.Kind].(map[string]any); ok {
			if v, ok := extMap["mcp_config"].(string); ok {
				operatorPath = v
			}
		}
		if operatorPath != "" && !filepath.IsAbs(operatorPath) {
			operatorPath = filepath.Join(filepath.Dir(deps.WorkflowPath), operatorPath)
		}

		generatedPath, genErr := GenerateMCPConfig(MCPConfigParams{
			BinaryPath:            execPath,
			WorkflowPath:          deps.WorkflowPath,
			WorkspacePath:         wsResult.Path,
			IssueID:               issue.ID,
			Identifier:            issue.Identifier,
			DBPath:                deps.DBPath,
			SessionID:             "",
			OperatorMCPConfigPath: operatorPath,
		})
		if genErr != nil {
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:       issue.ID,
				Identifier:    issue.Identifier,
				ExitKind:      WorkerExitError,
				Error:         fmt.Errorf("MCP config generation: %w", genErr),
				WorkspacePath: wsResult.Path,
				AgentAdapter:  cfg.Agent.Kind,
				Attempt:       attempt,
				SSHHost:       deps.SSHHost,
			})
			return
		}

		mcpConfigPath = generatedPath
		logger.Info("MCP config written", slog.String("mcp_config_path", generatedPath))
	}

	// Check context between workspace preparation and session start.
	if ctx.Err() != nil {
		finishWorkspace()
		reported = true
		deps.OnExit(issue.ID, WorkerResult{
			IssueID:       issue.ID,
			Identifier:    issue.Identifier,
			ExitKind:      WorkerExitCancelled,
			WorkspacePath: wsResult.Path,
			AgentAdapter:  cfg.Agent.Kind,
			Attempt:       attempt,
			SSHHost:       deps.SSHHost,
		})
		return
	}

	// Phase 2: Agent Session Start.
	session, err = deps.AgentAdapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:            wsResult.Path,
		AgentConfig:              toDomainAgentConfig(cfg.Agent),
		ResumeSessionID:          deps.ResumeSessionID,
		SSHHost:                  deps.SSHHost,
		SSHStrictHostKeyChecking: deps.SSHStrictHostKeyChecking,
		MCPConfigPath:            mcpConfigPath,
	})
	if err != nil {
		finishWorkspace()
		reported = true
		deps.OnExit(issue.ID, WorkerResult{
			IssueID:       issue.ID,
			Identifier:    issue.Identifier,
			ExitKind:      exitKindForErr(ctx),
			Error:         fmt.Errorf("agent session start: %w", err),
			WorkspacePath: wsResult.Path,
			AgentAdapter:  cfg.Agent.Kind,
			Attempt:       attempt,
			SSHHost:       deps.SSHHost,
		})
		return
	}

	sessionStarted = true
	sessionID = session.ID
	logger.Info("agent session started", slog.String("session_id", session.ID))

	// Phase 3: Multi-Turn Loop.
	maxTurns := cfg.Agent.MaxTurns
	if maxTurns < 1 {
		logger.Warn("agent max_turns is less than 1; clamping to 1", slog.Int("configured_max_turns", cfg.Agent.MaxTurns))
		maxTurns = 1
	}
	turnNumber := 1
	activeStates := cfg.Tracker.ActiveStates

	for {
		// 3a: Build turn-appropriate prompt.
		issueMap := issue.ToTemplateMap()
		rendered, err := prompt.BuildTurnPrompt(tmpl, issueMap, attemptInt, turnNumber, maxTurns)
		if err != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:        issue.ID,
				Identifier:     issue.Identifier,
				ExitKind:       exitKindForErr(ctx),
				Error:          fmt.Errorf("prompt render (turn %d): %w", turnNumber, err),
				TurnsCompleted: turnsCompleted,
				SessionID:      session.ID,
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
				SSHHost:        deps.SSHHost,
			})
			return
		}

		// Append tool advertisement on the first turn only.
		if turnNumber == 1 && deps.ToolRegistry != nil && deps.ToolRegistry.Len() > 0 {
			rendered += "\n\n" + buildToolAdvertisement(deps.ToolRegistry, cfg.Tracker.Project)
		}

		logger.Info("turn started", slog.Int("turn_number", turnNumber), slog.Int("max_turns", maxTurns))

		// 3b: Execute turn.
		turnResult, err := deps.AgentAdapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt: rendered,
			Issue:  issue,
			OnEvent: func(event domain.AgentEvent) {
				// Defensive copy: RateLimits is a reference type. Copying
				// here, in the worker goroutine, before the event crosses
				// the goroutine boundary ensures the orchestrator never
				// iterates a map that the adapter may still mutate.
				if event.RateLimits != nil {
					event.RateLimits = shallowCopyMap(event.RateLimits)
				}
				deps.OnEvent(issue.ID, event)
			},
		})
		if err != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:        issue.ID,
				Identifier:     issue.Identifier,
				ExitKind:       exitKindForErr(ctx),
				Error:          fmt.Errorf("agent turn %d: %w", turnNumber, err),
				TurnsCompleted: turnsCompleted,
				SessionID:      session.ID,
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
				SSHHost:        deps.SSHHost,
			})
			return
		}

		turnsCompleted++
		logger.Info("turn completed", slog.Int("turn_number", turnNumber), slog.Int("max_turns", maxTurns))

		// 3c: Check for turn-level failure exit reasons.
		if !isTurnSuccess(turnResult.ExitReason) {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			reported = true
			exitKind := exitKindForErr(ctx)
			logger.Warn("turn exit reason indicates failure",
				slog.Int("turn_number", turnNumber),
				slog.Any("exit_reason", turnResult.ExitReason),
			)
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:        issue.ID,
				Identifier:     issue.Identifier,
				ExitKind:       exitKind,
				Error:          fmt.Errorf("agent turn %d ended: %s", turnNumber, turnResult.ExitReason),
				TurnsCompleted: turnsCompleted,
				SessionID:      session.ID,
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
				SSHHost:        deps.SSHHost,
			})
			return
		}

		// 3d: Re-check tracker issue state.
		refreshed, err := deps.TrackerAdapter.FetchIssueStatesByIDs(ctx, []string{issue.ID})
		if err != nil {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			reported = true
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:        issue.ID,
				Identifier:     issue.Identifier,
				ExitKind:       exitKindForErr(ctx),
				Error:          fmt.Errorf("issue state refresh (turn %d): %w", turnNumber, err),
				TurnsCompleted: turnsCompleted,
				SessionID:      session.ID,
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
				SSHHost:        deps.SSHHost,
			})
			return
		}

		if stateStr, ok := refreshed[issue.ID]; ok {
			issue.State = stateStr
		}

		logger.Info("issue state refreshed", slog.String("refreshed_state", issue.State))

		if !isActiveState(issue.State, activeStates) {
			break
		}

		if turnNumber >= maxTurns {
			break
		}

		turnNumber++
	}

	// Phase 4: Clean Exit.
	stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
	finishWorkspace()

	logger.Info("worker exiting",
		slog.Any("exit_kind", WorkerExitNormal),
		slog.Int("turns_completed", turnsCompleted),
	)

	reported = true
	deps.OnExit(issue.ID, WorkerResult{
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		ExitKind:       WorkerExitNormal,
		TurnsCompleted: turnsCompleted,
		SessionID:      session.ID,
		WorkspacePath:  wsResult.Path,
		AgentAdapter:   cfg.Agent.Kind,
		Attempt:        attempt,
		SSHHost:        deps.SSHHost,
	})
}

// buildToolAdvertisement formats a Markdown section documenting the
// tools available in the registry. Appended to the agent prompt on
// the first turn so the agent knows what tools exist.
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

// buildDispatchComment returns the tracker comment text for a session
// dispatch event.
func buildDispatchComment(agentKind string, attempt int) string {
	return fmt.Sprintf("Sortie session started.\nSession: pending\nAgent: %s\nWorkspace: pending\nAttempt: %d", agentKind, attempt)
}
