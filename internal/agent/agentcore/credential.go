package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// CredentialVerificationPrompt names no tool, so a runtime whose tool
// policy forbids everything still answers it.
const CredentialVerificationPrompt = "Say exactly: SORTIE_CREDENTIAL_OK"

// CredentialExchangeBound is how long a runtime may take to establish
// its credential with its backend before it answers at session start.
const CredentialExchangeBound = 60 * time.Second

// CredentialVerification carries the inputs of [VerifyCredential].
type CredentialVerification struct {
	// Session is the working session's parameters; VerifyCredential
	// overrides CredentialVerification, ResumeSessionID and MCPConfigPath.
	// Its VerificationFullContext is set only by the retry inside
	// VerifyCredential.
	Session domain.StartSessionParams

	Issue domain.Issue

	// TurnBound bounds StartSession and RunTurn together; <= 0 is unbounded.
	TurnBound time.Duration

	// StopBound bounds StopSession, which outlives ctx's cancellation.
	StopBound time.Duration

	// OnRequest runs before each RunTurn, never after a failed StartSession.
	OnRequest func()

	OnEvent func(domain.AgentEvent)

	Logger *slog.Logger
}

// VerificationSystemPrompt replaces a runtime's built-in system prompt
// in a verification session. It is the text the measured Claude Code
// and Codex runs used.
const VerificationSystemPrompt = "You are a helpful assistant."

// VerifyCredential sends [CredentialVerificationPrompt] through a fresh
// session on adapter and returns the turn's result with a nil error
// when the runtime answered. A started session is always stopped, even
// when RunTurn panics; a stop failure is only logged.
//
// A session that never reached the model, because the runtime rejected
// a switch that drops agent context, is repeated once with the full
// context under a fresh TurnBound; that session's outcome is returned.
//
// A turn the provider refused for a usage limit returns its
// [domain.ErrUsageLimit] error unchanged, so the caller's reset wait applies.
func VerifyCredential(ctx context.Context, adapter domain.AgentAdapter, v CredentialVerification) (domain.TurnResult, error) {
	logger := v.Logger
	if logger == nil {
		logger = slog.Default()
	}

	result, startFailed, err := verifyOnce(ctx, adapter, v, logger)
	if !retriesWithFullContext(ctx, v.Session, startFailed, err) {
		return result, err
	}

	logger.Warn("credential verification did not reach the model, retrying with the full agent context", slog.Any("error", err))
	v.Session.VerificationFullContext = true
	result, _, err = verifyOnce(ctx, adapter, v, logger)
	return result, err
}

func retriesWithFullContext(ctx context.Context, session domain.StartSessionParams, startFailed bool, err error) bool {
	if err == nil || session.VerificationFullContext || ctx.Err() != nil || errors.Is(err, sshutil.ErrConnectionFailed) {
		return false
	}
	if _, isEarlyExit := errors.AsType[*EarlyExitError](err); isEarlyExit {
		return true
	}
	if !startFailed {
		return false
	}
	agentErr, isAgentErr := errors.AsType[*domain.AgentError](err)
	return isAgentErr && (agentErr.Kind == domain.ErrPortExit || agentErr.Kind == domain.ErrResponseError)
}

// verifyOnce reports startFailed when StartSession itself returned the error.
func verifyOnce(ctx context.Context, adapter domain.AgentAdapter, v CredentialVerification, logger *slog.Logger) (result domain.TurnResult, startFailed bool, err error) {
	turnCtx := ctx
	if v.TurnBound > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, v.TurnBound)
		defer cancel()
	}

	sessionParams := v.Session
	sessionParams.CredentialVerification = true
	sessionParams.ResumeSessionID = ""
	sessionParams.MCPConfigPath = ""

	session, startErr := adapter.StartSession(turnCtx, sessionParams)
	if startErr != nil {
		return domain.TurnResult{}, true, classifyVerificationStartError(ctx, turnCtx, v.TurnBound, startErr)
	}

	defer func() {
		stopCtx := context.WithoutCancel(ctx)
		stopCancel := func() {}
		if v.StopBound > 0 {
			stopCtx, stopCancel = context.WithTimeout(stopCtx, v.StopBound)
		}
		defer stopCancel()
		if stopErr := adapter.StopSession(stopCtx, session); stopErr != nil {
			logger.Warn("credential verification session stop failed", slog.Any("error", stopErr))
		}
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	if v.OnRequest != nil {
		v.OnRequest()
	}

	onEvent := func(domain.AgentEvent) {}
	if v.OnEvent != nil {
		onEvent = v.OnEvent
	}

	turnResult, runErr := adapter.RunTurn(turnCtx, session, domain.RunTurnParams{
		Prompt:  CredentialVerificationPrompt,
		Issue:   v.Issue,
		OnEvent: onEvent,
	})
	return turnResult, false, classifyVerificationRunTurnOutcome(ctx, turnCtx, v.TurnBound, turnResult, runErr)
}

func verificationTurnBoundElapsed(ctx, turnCtx context.Context) bool {
	return turnCtx.Err() != nil && ctx.Err() == nil
}

// isTerminalVerificationError reports whether err must not be reclassified.
func isTerminalVerificationError(err error) bool {
	if errors.Is(err, sshutil.ErrConnectionFailed) {
		return true
	}
	if _, isEarlyExit := errors.AsType[*EarlyExitError](err); isEarlyExit {
		return true
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		return false
	}
	if agentErr.Kind == domain.ErrCredentialUnverified || agentErr.Kind == domain.ErrUsageLimit {
		return true
	}
	return !agentErr.Kind.RetryClassification().Retryable
}

func classifyVerificationStartError(ctx, turnCtx context.Context, turnBound time.Duration, startErr error) error {
	if ctx.Err() != nil {
		return startErr
	}
	if verificationTurnBoundElapsed(ctx, turnCtx) {
		return turnBoundError(turnBound, turnCtx.Err())
	}
	return startErr
}

// classifyVerificationRunTurnOutcome checks cancellation before runErr,
// and the turn bound ahead of it only for an outcome the adapter reports
// as cancelled: a turn that completed after the bound elapsed stands.
func classifyVerificationRunTurnOutcome(ctx, turnCtx context.Context, turnBound time.Duration, result domain.TurnResult, runErr error) error {
	if ctx.Err() != nil {
		if runErr != nil {
			return runErr
		}
		return ctx.Err()
	}
	if CancelledOutcome(result, runErr) && verificationTurnBoundElapsed(ctx, turnCtx) {
		return turnBoundError(turnBound, turnCtx.Err())
	}
	if runErr != nil && isTerminalVerificationError(runErr) {
		return runErr
	}

	agentErr, isAgentErr := errors.AsType[*domain.AgentError](runErr)
	if isAgentErr && agentErr.Kind == domain.ErrTurnIncomplete {
		return nil
	}
	if runErr == nil {
		if result.ExitReason == domain.EventTurnCompleted {
			return nil
		}
		return CredentialUnverifiedError(fmt.Sprintf("the request ended as %s", result.ExitReason), nil)
	}
	if isAgentErr {
		return CredentialUnverifiedError(agentErr.Message, agentErr.Err)
	}
	return CredentialUnverifiedError(runErr.Error(), runErr)
}

// turnBoundError builds the error for a verification request that did
// not complete within bound.
func turnBoundError(bound time.Duration, cause error) *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrCredentialUnverified,
		Message: fmt.Sprintf("the agent runtime did not complete a credential verification request within %d ms", bound.Milliseconds()),
		Err:     cause,
	}
}

// CredentialUnverifiedError builds the error for a verification request
// that failed for a reason other than the turn bound elapsing.
func CredentialUnverifiedError(reason string, cause error) *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrCredentialUnverified,
		Message: fmt.Sprintf("the agent runtime did not complete a credential verification request: %s", reason),
		Err:     cause,
	}
}

// CredentialRefusedError builds the error for a runtime that refused
// its credential, carrying the runtime's text.
func CredentialRefusedError(runtimeText string, cause error) *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrCredentialUnverified,
		Message: CredentialRefusedMessage(runtimeText),
		Err:     cause,
	}
}

// CredentialRefusedMessage builds the message of a credential refusal
// from the runtime's own text, for a turn that reports through
// [TurnEvidence].
func CredentialRefusedMessage(runtimeText string) string {
	return fmt.Sprintf("the agent runtime refused its credential: %s", runtimeText)
}
