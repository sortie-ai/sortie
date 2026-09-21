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
	Session domain.StartSessionParams

	Issue domain.Issue

	// TurnBound bounds StartSession and RunTurn together; <= 0 is unbounded.
	TurnBound time.Duration

	// StopBound bounds StopSession, which outlives ctx's cancellation.
	StopBound time.Duration

	// OnRequest runs once before RunTurn, never after a failed StartSession.
	OnRequest func()

	OnEvent func(domain.AgentEvent)

	Logger *slog.Logger
}

// VerifyCredential sends [CredentialVerificationPrompt] through a fresh
// session on adapter and returns the turn's result with a nil error
// when the runtime answered. A started session is always stopped, even
// when RunTurn panics; a stop failure is only logged.
func VerifyCredential(ctx context.Context, adapter domain.AgentAdapter, v CredentialVerification) (domain.TurnResult, error) {
	logger := v.Logger
	if logger == nil {
		logger = slog.Default()
	}

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
		return domain.TurnResult{}, classifyVerificationStartError(ctx, turnCtx, v.TurnBound, startErr)
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

	result, runErr := adapter.RunTurn(turnCtx, session, domain.RunTurnParams{
		Prompt:  CredentialVerificationPrompt,
		Issue:   v.Issue,
		OnEvent: onEvent,
	})
	return result, classifyVerificationRunTurnOutcome(ctx, turnCtx, v.TurnBound, result, runErr)
}

func verificationTurnBoundElapsed(ctx, turnCtx context.Context) bool {
	return turnCtx.Err() != nil && ctx.Err() == nil
}

// isTerminalVerificationError reports whether err must not be reclassified.
func isTerminalVerificationError(err error) bool {
	if errors.Is(err, sshutil.ErrConnectionFailed) {
		return true
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		return false
	}
	if agentErr.Kind == domain.ErrCredentialUnverified {
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

// classifyVerificationRunTurnOutcome checks cancellation and the turn
// bound before runErr, because both make any RunTurn outcome moot.
func classifyVerificationRunTurnOutcome(ctx, turnCtx context.Context, turnBound time.Duration, result domain.TurnResult, runErr error) error {
	if ctx.Err() != nil {
		if runErr != nil {
			return runErr
		}
		return ctx.Err()
	}
	if verificationTurnBoundElapsed(ctx, turnCtx) {
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
		Message: fmt.Sprintf("the agent runtime refused its credential: %s", runtimeText),
		Err:     cause,
	}
}

// CredentialAbsentError builds the error for a presence guard that found
// no credential before starting a runtime that would block waiting for one.
func CredentialAbsentError(reason string, cause error) *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrCredentialUnverified,
		Message: fmt.Sprintf("the agent runtime reports no usable credential: %s", reason),
		Err:     cause,
	}
}
