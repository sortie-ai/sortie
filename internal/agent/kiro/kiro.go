// Package kiro implements [domain.AgentAdapter] for the Kiro CLI (the
// rebranded Amazon Q Developer CLI). It launches one
// "kiro-cli chat --no-interactive" subprocess per turn, captures the
// human-readable transcript from stdout (ANSI stripped) into notification
// events, and classifies the turn outcome from the process exit status and
// stderr because headless Kiro emits no structured output. Registered under
// kind "kiro" via an init function.
//
// Kiro is a no-token-accounting adapter: the headless path reports only an
// abstract credits figure, never token counts, so it emits no token-usage
// events and leaves [domain.TurnResult] Usage at the zero value. Token-based
// budget enforcement is therefore inert; only the turn timeout applies.
// Every run is reported unmeasured, because the headless runtime reports no
// token counts: [domain.TurnResult] UsageMeasured is left at its zero value
// on every turn.
//
// MCP injection has no effect under KIRO_API_KEY authentication: the backend
// profile gate disables MCP, so [domain.StartSessionParams] MCPConfigPath is
// ignored and no MCP startup flag is passed.
//
// Safe for concurrent use across sessions: callers may invoke
// [KiroAdapter.RunTurn] concurrently for different [domain.Session]
// instances, but turns for a single session must be serialized.
package kiro

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Agents.RegisterWithMeta("kiro", NewKiroAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionUnsupported,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*KiroAdapter)(nil)

// whoamiSuccessMarker is the line "kiro-cli whoami" prints when the API key
// is valid. Its absence from the canary output marks a present-but-unusable
// credential, which the adapter rejects before any turn runs.
const whoamiSuccessMarker = "Authenticated with API key"

// KiroAdapter satisfies [domain.AgentAdapter] by managing Kiro CLI
// subprocesses. One adapter instance serves all concurrent sessions;
// per-session state is held in [sessionState] via the [domain.Session]
// Internal field.
type KiroAdapter struct {
	passthrough passthroughConfig
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It owns the workspace launch target and the fork-per-turn
// subprocess lifecycle. No long-lived process is started in StartSession.
type sessionState struct {
	target      agentcore.LaunchTarget
	agentConfig domain.AgentConfig
	passthrough passthroughConfig
	baseLogger  *slog.Logger

	// sessionID is set from params.ResumeSessionID and is the value the
	// GetSessionID hook closes over. The headless path reports no session
	// ID of its own, so this is the only identity the adapter carries.
	sessionID string

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// resumeRequested becomes true after the first turn runs successfully,
	// so turn 2+ adds --resume for cwd-scoped continuation.
	resumeRequested bool

	// turnStdout accumulates ANSI-stripped stdout for the active turn.
	// Reset at the top of each RunTurn before delegating to forkSession.
	turnStdout *strings.Builder
}

func (s *sessionState) logger() *slog.Logger {
	if s.sessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.sessionID)
}

// NewKiroAdapter creates a [KiroAdapter] from adapter configuration. The
// config parameter is the raw map from the "kiro" sub-object in WORKFLOW.md.
//
// It returns a non-nil error when the passthrough config sets both
// trust_all_tools and a non-empty trust_tools list. Command resolution is
// deferred to [KiroAdapter.StartSession].
func NewKiroAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt, fault := parsePassthroughConfig(config)
	if fault != nil {
		return nil, fault
	}
	if err := checkCrossField(pt); err != nil {
		return nil, err
	}
	return &KiroAdapter{passthrough: pt}, nil
}

// StartSession validates the workspace path, resolves the kiro-cli binary,
// verifies the credential, and initializes per-session state. No subprocess
// is spawned; that happens in [KiroAdapter.RunTurn].
//
// In local mode it confirms KIRO_API_KEY is present and runs a "kiro-cli
// whoami" canary, because a missing credential makes headless chat hang on
// interactive login and an invalid credential exits 0 with empty output. In
// SSH mode the credential preflight is skipped and KIRO_API_KEY is injected
// inline into the remote command.
func (a *KiroAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "kiro-cli")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	if target.RemoteCommand == "" {
		if authErr := checkCredential(ctx, target.Command); authErr != nil {
			return domain.Session{}, authErr
		}
	} else {
		target.RemoteCommand = buildSSHRemoteCmd(target.RemoteCommand, os.Getenv("KIRO_API_KEY"))
	}

	state := &sessionState{
		target:      target,
		agentConfig: params.AgentConfig,
		passthrough: a.passthrough,
		baseLogger:  slog.Default().With(slog.String("component", "kiro-adapter")),
		sessionID:   params.ResumeSessionID,
		turnStdout:  &strings.Builder{},
	}

	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(turn int, prompt string) []string {
			return buildArgs(state, turn, prompt, a.passthrough)
		},
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string) (any, error) {
			text := stripANSI(string(line))
			state.turnStdout.WriteString(text)
			if text != "" {
				emit(domain.AgentEvent{
					Type:      domain.EventNotification,
					Timestamp: time.Now().UTC(),
					Message:   typeutil.TruncateRunes(text, 500),
					AgentPID:  pid,
				})
			}
			return nil, nil
		},
		GetUsage:     func() domain.TokenUsage { return domain.TokenUsage{} },
		GetSessionID: func() string { return state.sessionID },
		OnFinalize: func(emit func(domain.AgentEvent), _ any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			creditsSeen, authFailed := classifyStderr(stderrLines)

			// Headless Kiro reports no per-turn token count, so the work
			// signal this adapter can offer is the currency the runtime
			// actually exposes: the credits trailer, consumed below as the
			// success signal rather than as Work.
			ev := agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				Work:         agentcore.WorkUnobservable,
				WorkDetail:   "no credits trailer on stderr",
			}

			switch {
			case exitCode == 0 && creditsSeen:
				ev.Terminal = agentcore.TerminalSuccess
				state.resumeRequested = true
			case exitCode == 0 && authFailed && state.turnStdout.Len() == 0:
				ev.Terminal = agentcore.TerminalFailure
				ev.TerminalErrorKind = domain.ErrResponseError
				ev.TerminalMessage = "kiro authentication failed"
			}

			meta := agentcore.TurnMeta{SessionID: state.sessionID}

			return agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
		},
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.logger())

	return domain.Session{
		ID:       state.sessionID,
		AgentPID: "",
		Internal: state,
	}, nil
}

// checkCredential verifies that KIRO_API_KEY is present and usable before
// any chat turn. It returns nil on success and a [domain.AgentError] with
// [domain.ErrResponseError] when the key is absent, when the whoami canary
// times out or exits non-zero, or when the canary output shows the key is
// invalid.
//
// The canary binary is already resolved by [agentcore.ResolveLaunchTarget]
// before this runs, so a canary execution failure means the present binary
// could not confirm the credential (a timeout or non-zero exit), not that
// the agent is missing. It is classified as a retryable credential problem
// rather than the non-retryable [domain.ErrAgentNotFound].
func checkCredential(ctx context.Context, command string) *domain.AgentError {
	if strings.TrimSpace(os.Getenv("KIRO_API_KEY")) == "" {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "KIRO_API_KEY is not set",
		}
	}

	canaryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, canaryErr := exec.CommandContext(canaryCtx, command, "whoami").CombinedOutput() //nolint:gosec // command resolved by ResolveLaunchTarget via LookPath
	if canaryErr != nil {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "kiro-cli whoami canary timed out or exited non-zero",
			Err:     canaryErr,
		}
	}

	output := string(out)
	if strings.Contains(output, authFailedMarker) || !strings.Contains(output, whoamiSuccessMarker) {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "KIRO_API_KEY is invalid or expired",
		}
	}

	return nil
}

// RunTurn executes one agent turn by delegating to the session's
// [agentcore.ForkPerTurnSession]. The per-turn stdout accumulator is reset
// before delegation so each turn starts clean.
func (a *KiroAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("kiro: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "unexpected session internal type",
		}
	}

	state.turnStdout = &strings.Builder{}

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// StopSession terminates a running Kiro CLI subprocess gracefully by
// delegating to the session's [agentcore.ForkPerTurnSession]. It is a no-op
// when no subprocess is active and is safe to call after a failed RunTurn.
func (a *KiroAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "unexpected session internal type",
		}
	}
	if state.forkSession == nil {
		return nil
	}
	return state.forkSession.Stop(ctx)
}
