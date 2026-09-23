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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
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
		UsageArrival:        registry.UsageArrivalNone,
		UsageAttribution:    registry.UsageAttributionNone,
		CredentialEnv:       registry.DeclareCredentialEnv("KIRO_API_KEY"),
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*KiroAdapter)(nil)

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

	// work is the per-turn work-evidence observer for the shared
	// turn-disposition decision. Reset at the top of each RunTurn before
	// delegating to forkSession.
	work *agentcore.WorkObserver

	credentialVerification bool

	verificationBefore []kiroSessionListing

	// verificationBeforeListed false makes StopSession delete nothing:
	// without a baseline every existing conversation would read as new.
	verificationBeforeListed bool
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

// StartSession resolves the kiro-cli binary and initializes per-session
// state. Only a verification session spawns anything here: the whoami
// guard and a listing of the workspace's conversations.
func (a *KiroAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "kiro-cli")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	baseLogger := slog.Default().With(slog.String("component", "kiro-adapter"))

	var verificationBefore []kiroSessionListing
	var verificationBeforeListed bool
	if params.CredentialVerification {
		if authErr := checkCredential(ctx, target, params.AgentConfig.StopGraceMS); authErr != nil {
			return domain.Session{}, authErr
		}
		listing, listErr := listWorkspaceConversations(ctx, target, agentcore.AuxiliaryTimeout(params.AgentConfig), params.AgentConfig.StopGraceMS)
		if listErr != nil {
			baseLogger.Warn("failed to delete credential verification session", slog.Any("error", listErr))
		} else {
			verificationBefore = listing
			verificationBeforeListed = true
		}
	}

	state := &sessionState{
		target:                   target,
		agentConfig:              params.AgentConfig,
		passthrough:              a.passthrough,
		baseLogger:               baseLogger,
		sessionID:                params.ResumeSessionID,
		credentialVerification:   params.CredentialVerification,
		verificationBefore:       verificationBefore,
		verificationBeforeListed: verificationBeforeListed,
	}

	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(turn int, prompt string) []string {
			return buildArgs(state, turn, prompt, a.passthrough)
		},
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string) (any, error) {
			text := stripANSI(string(line))
			if strings.TrimSpace(text) != "" {
				state.work.ObserveAssistantOutput()
			}
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
		GetUsage:     func() (domain.TokenUsage, bool) { return domain.TokenUsage{}, false },
		GetSessionID: func() string { return state.sessionID },
		OnFinalize: func(emit func(domain.AgentEvent), _ any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			creditsSeen := classifyStderr(stderrLines)

			ev := agentcore.TurnEvidence{ExitObserved: true, ExitCode: exitCode}
			ev.Work, ev.WorkDetail = state.work.Report()

			if exitCode == 0 && creditsSeen {
				ev.Terminal = agentcore.TerminalSuccess
				state.resumeRequested = true
			}

			meta := agentcore.TurnMeta{SessionID: state.sessionID}

			return agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
		},
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.logger(), state.agentConfig.StopGraceMS)

	return domain.Session{
		ID:       state.sessionID,
		AgentPID: "",
		Internal: state,
	}, nil
}

// checkCredential runs "kiro-cli whoami" because a headless chat under a
// missing or invalid credential either hangs on interactive login or exits
// 0 with empty output. The generous bound covers a token refresh.
func checkCredential(ctx context.Context, target agentcore.LaunchTarget, stopGraceMS int) *domain.AgentError {
	canaryCtx, cancel := context.WithTimeout(ctx, agentcore.CredentialExchangeBound)
	defer cancel()

	cmd, agentErr := target.AuxiliaryCommand(canaryCtx, []string{"whoami"}, nil, nil)
	if agentErr != nil {
		return agentErr
	}
	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(stopGraceMS), procutil.CaptureParams{})

	if target.RemoteCommand != "" && startErr == nil && sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)) {
		return agentcore.ConnectionFailedError()
	}

	switch {
	case startErr != nil:
		return agentcore.CredentialAbsentError("whoami could not be started", startErr)
	case errors.Is(canaryCtx.Err(), context.DeadlineExceeded):
		reason := fmt.Sprintf("whoami did not finish within %d ms", agentcore.CredentialExchangeBound.Milliseconds())
		return agentcore.CredentialAbsentError(reason, canaryCtx.Err())
	case result.WaitErr != nil:
		reason := fmt.Sprintf("whoami exited with status %d", procutil.ExtractExitCode(result.WaitErr))
		return agentcore.CredentialAbsentError(reason, result.WaitErr)
	}
	return nil
}

type kiroSessionListing struct {
	SessionID string `json:"sessionId"`
	Source    string `json:"source"`
	Title     string `json:"title"`
}

type kiroSessionListGroup struct {
	Cwd      string               `json:"cwd"`
	Sessions []kiroSessionListing `json:"sessions"`
}

func listWorkspaceConversations(ctx context.Context, target agentcore.LaunchTarget, timeout time.Duration, stopGraceMS int) ([]kiroSessionListing, error) {
	listCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var out bytes.Buffer
	cmd, agentErr := target.AuxiliaryCommand(listCtx, []string{"chat", "--list-sessions", "-f", "json"}, nil, nil)
	if agentErr != nil {
		return nil, agentErr
	}
	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(stopGraceMS), procutil.CaptureParams{Stdout: &out})
	if startErr != nil {
		return nil, startErr
	}
	if result.WaitErr != nil {
		return nil, result.WaitErr
	}

	var groups []kiroSessionListGroup
	if err := json.Unmarshal(out.Bytes(), &groups); err != nil {
		return nil, fmt.Errorf("unmarshal conversation listing: %w", err)
	}
	if len(groups) != 1 {
		return nil, fmt.Errorf("conversation listing carries %d directory groups, want 1", len(groups))
	}
	group := groups[0]
	if filepath.Clean(group.Cwd) != filepath.Clean(target.WorkspacePath) {
		return nil, fmt.Errorf("conversation listing cwd %q does not match workspace %q", group.Cwd, target.WorkspacePath)
	}
	return group.Sessions, nil
}

var kiroSessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// errAmbiguousVerificationListing names the outcome logged when the
// before/after listings do not resolve to exactly one new classic
// conversation: deleting the wrong one would destroy real work.
var errAmbiguousVerificationListing = errors.New("credential verification listing is ambiguous")

type verificationListingOutcome uint8

const (
	verificationNoNewEntry verificationListingOutcome = iota
	verificationConversationFound
	verificationAmbiguous
)

// findVerificationConversation reports anything but exactly one new classic
// conversation as ambiguous, since deleting a wrong one destroys real work.
func findVerificationConversation(before, after []kiroSessionListing) (listing kiroSessionListing, outcome verificationListingOutcome) {
	beforeIDs := make(map[string]bool, len(before))
	for _, s := range before {
		beforeIDs[s.SessionID] = true
	}

	var newEntries []kiroSessionListing
	for _, s := range after {
		if !beforeIDs[s.SessionID] {
			newEntries = append(newEntries, s)
		}
	}
	if len(newEntries) == 0 {
		return kiroSessionListing{}, verificationNoNewEntry
	}
	if len(newEntries) != 1 {
		return kiroSessionListing{}, verificationAmbiguous
	}

	candidate := newEntries[0]
	if candidate.Source != "classic" || !kiroSessionIDPattern.MatchString(candidate.SessionID) {
		return kiroSessionListing{}, verificationAmbiguous
	}
	return candidate, verificationConversationFound
}

func deleteConversation(ctx context.Context, target agentcore.LaunchTarget, id string, timeout time.Duration, stopGraceMS int) error {
	delCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, agentErr := target.AuxiliaryCommand(delCtx, []string{"chat", "--delete-session", id, "--session-source", "v1"}, nil, nil)
	if agentErr != nil {
		return agentErr
	}
	result, startErr := procutil.RunCapture(cmd, procutil.StopGrace(stopGraceMS), procutil.CaptureParams{})
	if startErr != nil {
		return startErr
	}
	return result.WaitErr
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

	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true})

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
	var stopErr error
	if state.forkSession != nil {
		stopErr = state.forkSession.Stop(ctx)
	}
	if state.credentialVerification {
		state.deleteVerificationConversation(ctx)
	}
	return stopErr
}

func (s *sessionState) deleteVerificationConversation(ctx context.Context) {
	if !s.verificationBeforeListed {
		return
	}

	timeout := agentcore.AuxiliaryTimeout(s.agentConfig)
	after, listErr := listWorkspaceConversations(ctx, s.target, timeout, s.agentConfig.StopGraceMS)
	if listErr != nil {
		s.logger().Warn("failed to delete credential verification session", slog.Any("error", listErr))
		return
	}

	candidate, outcome := findVerificationConversation(s.verificationBefore, after)
	switch outcome {
	case verificationNoNewEntry:
		return
	case verificationConversationFound:
		if delErr := deleteConversation(ctx, s.target, candidate.SessionID, timeout, s.agentConfig.StopGraceMS); delErr != nil {
			s.logger().Warn("failed to delete credential verification session", slog.Any("error", delErr))
		}
	default:
		s.logger().Warn("failed to delete credential verification session", slog.Any("error", errAmbiguousVerificationListing))
	}
}
