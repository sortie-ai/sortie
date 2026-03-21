package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
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

	// WorkspacePath is the workspace directory used for this attempt.
	// Empty if workspace preparation failed.
	WorkspacePath string

	// AgentAdapter is the agent adapter kind string (from config.Agent.Kind).
	AgentAdapter string

	// Attempt is the retry attempt parameter passed to the worker.
	Attempt *int

	// StartedAt is copied from the RunningEntry (set by DispatchIssue).
	// The worker does not set this — it is populated by the exit
	// handler from the running map entry.
	StartedAt time.Time
}

// WorkerDeps holds the collaborators injected into the worker attempt
// function. All fields are required. The orchestrator constructs this
// once and shares it across all workers.
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

	// Logger is the structured logger with issue-scoped context fields
	// already attached (issue_id, issue_identifier).
	Logger *slog.Logger
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
		logger.Warn("StopSession failed", "error", err)
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

	// reported tracks whether OnExit has been called. The deferred
	// panic recovery checks this to avoid double-reporting.
	reported := false

	defer func() {
		if r := recover(); r != nil {
			if !reported {
				deps.OnExit(issue.ID, WorkerResult{
					IssueID:      issue.ID,
					Identifier:   issue.Identifier,
					ExitKind:     WorkerExitError,
					Error:        fmt.Errorf("worker panic: %v", r),
					AgentAdapter: cfg.Agent.Kind,
					Attempt:      attempt,
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
		})
		return
	}

	logger.Info("workspace prepared", "workspace", wsResult.Path)

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
		})
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
		})
		return
	}

	// Phase 2: Agent Session Start.
	session, err := deps.AgentAdapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   wsResult.Path,
		AgentConfig:     toDomainAgentConfig(cfg.Agent),
		ResumeSessionID: "",
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
		})
		return
	}

	logger.Info("agent session started", "session_id", session.ID)

	// Phase 3: Multi-Turn Loop.
	maxTurns := cfg.Agent.MaxTurns
	turnNumber := 1
	turnsCompleted := 0
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
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
			})
			return
		}

		logger.Info("turn started", "turn_number", turnNumber, "max_turns", maxTurns)

		// 3b: Execute turn.
		turnResult, err := deps.AgentAdapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt: rendered,
			Issue:  issue,
			OnEvent: func(event domain.AgentEvent) {
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
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
			})
			return
		}

		turnsCompleted++
		logger.Info("turn completed", "turn_number", turnNumber, "max_turns", maxTurns)

		// 3c: Check for turn-level failure exit reasons.
		if !isTurnSuccess(turnResult.ExitReason) {
			stopSessionBestEffort(ctx, deps.AgentAdapter, session, cfg, logger)
			finishWorkspace()
			reported = true
			exitKind := exitKindForErr(ctx)
			logger.Warn("turn exit reason indicates failure",
				"turn_number", turnNumber,
				"exit_reason", turnResult.ExitReason,
			)
			deps.OnExit(issue.ID, WorkerResult{
				IssueID:        issue.ID,
				Identifier:     issue.Identifier,
				ExitKind:       exitKind,
				Error:          fmt.Errorf("agent turn %d ended: %s", turnNumber, turnResult.ExitReason),
				TurnsCompleted: turnsCompleted,
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
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
				WorkspacePath:  wsResult.Path,
				AgentAdapter:   cfg.Agent.Kind,
				Attempt:        attempt,
			})
			return
		}

		if stateStr, ok := refreshed[issue.ID]; ok {
			issue.State = stateStr
		}

		logger.Info("issue state refreshed", "refreshed_state", issue.State)

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
		"exit_kind", WorkerExitNormal,
		"turns_completed", turnsCompleted,
	)

	reported = true
	deps.OnExit(issue.ID, WorkerResult{
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		ExitKind:       WorkerExitNormal,
		TurnsCompleted: turnsCompleted,
		WorkspacePath:  wsResult.Path,
		AgentAdapter:   cfg.Agent.Kind,
		Attempt:        attempt,
	})
}
