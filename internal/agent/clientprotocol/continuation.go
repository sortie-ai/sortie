package clientprotocol

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// negativeControlMethod names a request in a vendor namespace no
// protocol release can claim, sent once before the first continuation
// call of a session so an unimplemented continuation method and a
// broken one are distinguishable by their error codes rather than
// assumed apart.
const negativeControlMethod = "_sortie.dev/probe/unimplemented"

// continuationMethod is the route resolveSession chooses from the
// handshake's advertised capabilities, in fixed order: session/load
// when advertised, otherwise session/resume when advertised, otherwise
// no continuation method at all.
type continuationMethod uint8

const (
	continuationNone continuationMethod = iota
	continuationLoad
	continuationResume
)

// chooseContinuationMethod picks the continuation route caps allows.
func chooseContinuationMethod(caps agentCapabilities) continuationMethod {
	if caps.LoadSession != nil && *caps.LoadSession {
		return continuationLoad
	}
	if caps.SessionCapabilities != nil && caps.SessionCapabilities.Resume != nil {
		return continuationResume
	}
	return continuationNone
}

// negativeControlVerdict is what the negative control recorded: an
// error code the agent actually answered with, or none when the agent
// nonconformantly answered success.
type negativeControlVerdict struct {
	code     int
	hadError bool
}

// matches reports whether code is the same error the negative control
// itself produced.
func (v negativeControlVerdict) matches(code int) bool {
	return v.hadError && v.code == code
}

// sendNegativeControl sends the negative control request and records
// the agent's answer. A stream end or a timeout while it is in flight
// surfaces the same normalized failure any other loss of the agent
// connection does.
func sendNegativeControl(ctx context.Context, state *sessionState) (negativeControlVerdict, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()

	resp, err := state.conn.Call(callCtx, negativeControlMethod, struct{}{})
	if agentErr := translateCallError(err, callCtx); agentErr != nil {
		return negativeControlVerdict{}, agentErr
	}
	if resp.Error != nil {
		return negativeControlVerdict{code: resp.Error.Code, hadError: true}, nil
	}
	return negativeControlVerdict{}, nil
}

// resolveSession creates or continues the session: session/new alone
// when resumeID is empty or the handshake advertised no continuation
// method, otherwise the advertised route, confirmed against the
// negative control and, for a load, against observed replay, falling
// back to session/new within this same call whenever continuation is
// not confirmed. It returns the identifier of the session the agent
// actually created, which is the loaded identifier only when the load
// route was confirmed.
func resolveSession(ctx context.Context, state *sessionState, resumeID string, caps agentCapabilities, cwd string, servers []mcpServer) (string, *domain.AgentError) {
	if resumeID == "" {
		return createNewSession(ctx, state, cwd, servers)
	}

	method := chooseContinuationMethod(caps)
	if method == continuationNone {
		return createNewSession(ctx, state, cwd, servers)
	}

	control, agentErr := sendNegativeControl(ctx, state)
	if agentErr != nil {
		if agentErr.Kind != domain.ErrResponseTimeout {
			return "", agentErr
		}
		state.logger.Warn("continuation negative control timed out", slog.String("method", negativeControlMethod))
		return unconfirmedFallback(ctx, state, cwd, servers)
	}

	switch method {
	case continuationLoad:
		return resolveLoad(ctx, state, resumeID, cwd, servers, control)
	case continuationResume:
		return resolveResume(ctx, state, resumeID, cwd, servers, control)
	default:
		return createNewSession(ctx, state, cwd, servers)
	}
}

// resolveLoad attempts session/load for resumeID. A response carrying
// an error lowers sessionContinuation and falls back at once. A
// successful response is not enough on its own: the pump must observe
// at least one chunk replayed for resumeID, before the response
// returns or within the connection's own read timeout after it, or the
// entry is lowered and this falls back the same way. A call that times
// out waiting for any response at all is treated the same as one that
// answered with an error: the capability is not confirmed, not the
// run.
func resolveLoad(ctx context.Context, state *sessionState, resumeID, cwd string, servers []mcpServer, control negativeControlVerdict) (string, *domain.AgentError) {
	if agentErr := sendControl(ctx, state, pumpItem{control: &pumpControl{expectLoad: resumeID}}); agentErr != nil {
		return "", agentErr
	}

	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()
	resp, err := state.conn.Call(callCtx, methodSessionLoad, loadSessionRequest{
		Cwd: cwd, MCPServers: servers, SessionID: sessionId(resumeID),
	})
	if agentErr := translateCallError(err, callCtx); agentErr != nil {
		if agentErr.Kind != domain.ErrResponseTimeout {
			return "", agentErr
		}
		state.logger.Warn("continuation call timed out", slog.String("method", methodSessionLoad))
		return unconfirmedFallback(ctx, state, cwd, servers)
	}
	if resp.Error != nil {
		logContinuationFailure(state, methodSessionLoad, resp.Error.Code, control)
		return unconfirmedFallback(ctx, state, cwd, servers)
	}

	reply := make(chan bool, 1)
	if agentErr := sendControl(ctx, state, pumpItem{control: &pumpControl{query: &replayQuery{reply: reply}}}); agentErr != nil {
		return "", agentErr
	}

	timer := time.NewTimer(readTimeout(state))
	defer timer.Stop()
	select {
	case confirmed := <-reply:
		if confirmed {
			return resumeID, nil
		}
		return createNewSession(ctx, state, cwd, servers)
	case <-timer.C:
		return unconfirmedFallback(ctx, state, cwd, servers)
	}
}

// resolveResume attempts session/resume for resumeID. No replay is
// expected: a successful response alone confirms it. A response
// carrying an error, or a timeout waiting for any response at all,
// lowers sessionContinuation and falls back.
func resolveResume(ctx context.Context, state *sessionState, resumeID, cwd string, servers []mcpServer, control negativeControlVerdict) (string, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()

	var wireServers *[]mcpServer
	if len(servers) > 0 {
		wireServers = &servers
	}
	resp, err := state.conn.Call(callCtx, methodSessionResume, resumeSessionRequest{
		Cwd: cwd, MCPServers: wireServers, SessionID: sessionId(resumeID),
	})
	if agentErr := translateCallError(err, callCtx); agentErr != nil {
		if agentErr.Kind != domain.ErrResponseTimeout {
			return "", agentErr
		}
		state.logger.Warn("continuation call timed out", slog.String("method", methodSessionResume))
		return unconfirmedFallback(ctx, state, cwd, servers)
	}
	if resp.Error != nil {
		logContinuationFailure(state, methodSessionResume, resp.Error.Code, control)
		return unconfirmedFallback(ctx, state, cwd, servers)
	}
	return resumeID, nil
}

// unconfirmedFallback lowers sessionContinuation through the pump's
// own mutation path and creates a fresh session with session/new. It
// is the outcome required whenever a continuation call, or the
// negative control that precedes it, does not confirm the entry: an
// explicit error response or a timeout waiting for one. The fallback
// itself never fails the run on its own account.
func unconfirmedFallback(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (string, *domain.AgentError) {
	if agentErr := sendControl(ctx, state, pumpItem{control: &pumpControl{query: &replayQuery{}}}); agentErr != nil {
		return "", agentErr
	}
	return createNewSession(ctx, state, cwd, servers)
}

// sendControl publishes item to the pump's input channel, bounded by
// ctx and the connection's own read timeout, so a stalled pump can
// never leave a continuation call blocked on an unbounded send the way
// runTurn's own control publish is bounded.
func sendControl(ctx context.Context, state *sessionState, item pumpItem) *domain.AgentError {
	timer := time.NewTimer(readTimeout(state))
	defer timer.Stop()
	select {
	case state.itemCh <- item:
		return nil
	case <-ctx.Done():
		return &domain.AgentError{Kind: domain.ErrPortExit, Message: "context ended before a control message reached the agent connection", Err: ctx.Err()}
	case <-timer.C:
		return &domain.AgentError{Kind: domain.ErrResponseTimeout, Message: "timed out publishing a control message to the agent connection", Err: context.DeadlineExceeded}
	}
}

// createNewSession creates a fresh session with session/new: the route
// taken when no continuation is attempted, none is advertised, or the
// one attempted is not confirmed. A failure here is reported exactly
// as it would have been had continuation never been attempted; the
// fallback itself never fails the run on its own account.
func createNewSession(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (string, *domain.AgentError) {
	resp, agentErr := doNewSession(ctx, state, cwd, servers)
	if agentErr != nil {
		return "", agentErr
	}
	return string(resp.SessionID), nil
}

// logContinuationFailure records that a continuation call did not
// confirm its capability. An error code matching the negative
// control's own establishes the method as genuinely unimplemented; any
// other code is a real failure of that call. Neither distinction
// changes the outcome: both lower the capability entry and fall back
// to a fresh session.
func logContinuationFailure(state *sessionState, method string, code int, control negativeControlVerdict) {
	if control.matches(code) {
		state.logger.Debug("continuation method not implemented", slog.String("method", method))
		return
	}
	state.logger.Warn("continuation call failed", slog.String("method", method), slog.Int("code", code))
}
