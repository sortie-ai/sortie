package clientprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

// negativeControlMethod names a request in a vendor namespace no
// protocol release can claim, sent once before the first continuation
// call of a session so an unimplemented continuation method and a
// broken one are distinguishable by their error codes rather than
// assumed apart.
const negativeControlMethod = "_sortie.dev/probe/unimplemented"

// continuationMethod is the route continuationProbe takes to continue a session.
type continuationMethod uint8

const (
	continuationNone continuationMethod = iota
	continuationLoad
	continuationResume
)

// chooseContinuationMethod picks the continuation route caps allows, preferring
// session/load over session/resume.
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
	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()

	resp, err := state.conn.Call(callCtx, negativeControlMethod, struct{}{})
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return negativeControlVerdict{}, agentErr
	}
	if resp.Error != nil {
		return negativeControlVerdict{code: resp.Error.Code, hadError: true}, nil
	}
	return negativeControlVerdict{}, nil
}

// continuationSignal is the continuation probe's answer: the raw result of the
// call that continued the session, when it did.
type continuationSignal = agentcore.ResumeSignal[json.RawMessage]

// continuationProbe returns the probe of the continuation route caps
// advertises. Only a confirmed call is [agentcore.ResumeCarried]; a call that
// fails other than by timeout fails the start, as any other handshake call does.
func continuationProbe(state *sessionState, caps agentCapabilities, cwd string, servers []mcpServer) agentcore.ResumeProbe[json.RawMessage] {
	return agentcore.ResumeProbeFunc[json.RawMessage](func(ctx context.Context, resumeID string) (continuationSignal, *domain.AgentError) {
		method := chooseContinuationMethod(caps)
		if method == continuationNone {
			return continuationSignal{Verdict: agentcore.ResumeReplaced}, nil
		}

		control, agentErr := sendNegativeControl(ctx, state)
		if agentErr != nil {
			return failedContinuationCall(state, agentErr, negativeControlMethod)
		}

		if method == continuationLoad {
			return probeLoad(ctx, state, resumeID, cwd, servers, control)
		}
		return probeResume(ctx, state, resumeID, cwd, servers, control)
	})
}

// probeLoad attempts session/load for resumeID. A response carrying an error
// lowers sessionContinuation and replaces the session. A successful response
// is not enough on its own: the pump must observe at least one chunk replayed
// for resumeID, before the response returns or within the connection's own
// read timeout after it, or the session is not confirmed. A call that times
// out waiting for any response at all is treated as unconfirmed, not as a
// failed run.
func probeLoad(ctx context.Context, state *sessionState, resumeID, cwd string, servers []mcpServer, control negativeControlVerdict) (continuationSignal, *domain.AgentError) {
	if err := awaitCreationMinuteEnd(ctx, state, cwd, resumeID); err != nil {
		return continuationSignal{Cause: err}, nil
	}

	state.inbox.Put(pumpItem{control: &pumpControl{expectLoad: resumeID}})

	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()
	resp, err := state.conn.Call(callCtx, methodSessionLoad, loadSessionRequest{
		Cwd: cwd, MCPServers: servers, SessionID: sessionId(resumeID),
	})
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return failedContinuationCall(state, agentErr, methodSessionLoad)
	}
	if resp.Error != nil {
		return rejectedContinuationCall(state, methodSessionLoad, resp.Error.Code, control)
	}

	reply := make(chan bool, 1)
	state.inbox.Put(pumpItem{control: &pumpControl{query: &replayQuery{reply: reply}}})

	timer := time.NewTimer(agentcore.ReadTimeout(state.agentConfig))
	defer timer.Stop()
	select {
	case confirmed := <-reply:
		if confirmed {
			return continuationSignal{Verdict: agentcore.ResumeCarried, Answer: resp.Result}, nil
		}
		return continuationSignal{Cause: fmt.Errorf("%s replayed no history", methodSessionLoad)}, nil
	case <-timer.C:
		lowerSessionContinuation(state)
		return continuationSignal{Cause: fmt.Errorf("%s replayed no history within the read timeout", methodSessionLoad)}, nil
	}
}

// awaitCreationMinuteEnd waits until the UTC minute this adapter created
// resumeID in has ended, and returns ctx's error if ctx ends first.
func awaitCreationMinuteEnd(ctx context.Context, state *sessionState, cwd, resumeID string) error {
	createdAt, known := state.origins.createdAt(cwd, resumeID)
	if !known {
		return nil
	}
	wait := loadDeferral(createdAt, state.origins.clock())
	if wait <= 0 {
		return nil
	}
	state.logger.Debug("session load deferred past the session's creation minute",
		slog.String("session_id", resumeID), slog.Int64("wait_ms", wait.Milliseconds()))
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// probeResume attempts session/resume for resumeID. No replay is expected: a
// successful response alone confirms it. A response carrying an error, or a
// timeout waiting for any response at all, lowers sessionContinuation.
func probeResume(ctx context.Context, state *sessionState, resumeID, cwd string, servers []mcpServer, control negativeControlVerdict) (continuationSignal, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()

	var wireServers *[]mcpServer
	if len(servers) > 0 {
		wireServers = &servers
	}
	resp, err := state.conn.Call(callCtx, methodSessionResume, resumeSessionRequest{
		Cwd: cwd, MCPServers: wireServers, SessionID: sessionId(resumeID),
	})
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return failedContinuationCall(state, agentErr, methodSessionResume)
	}
	if resp.Error != nil {
		return rejectedContinuationCall(state, methodSessionResume, resp.Error.Code, control)
	}
	return continuationSignal{Verdict: agentcore.ResumeCarried, Answer: resp.Result}, nil
}

// failedContinuationCall maps a continuation call that failed to the probe's
// answer. A timeout lowers sessionContinuation and leaves the verdict unknown.
// Any other failure ends the start with the early-exit report the call
// observed, else the call's own error.
func failedContinuationCall(state *sessionState, agentErr *domain.AgentError, method string) (continuationSignal, *domain.AgentError) {
	if agentErr.Kind != domain.ErrResponseTimeout {
		if report := state.earlyExit.Report(state.stderrCollector); report != nil {
			return continuationSignal{}, report
		}
		return continuationSignal{}, agentErr
	}
	state.logger.Debug("continuation call timed out", slog.String("method", method))
	lowerSessionContinuation(state)
	return continuationSignal{Cause: fmt.Errorf("%s timed out", method)}, nil
}

// rejectedContinuationCall maps an error response to a continuation call. An
// error code matching the negative control's own establishes the method as
// genuinely unimplemented; any other code is a real failure of that call.
// Neither distinction changes the outcome: both lower sessionContinuation and
// replace the session.
func rejectedContinuationCall(state *sessionState, method string, code int, control negativeControlVerdict) (continuationSignal, *domain.AgentError) {
	if control.matches(code) {
		state.logger.Debug("continuation method not implemented", slog.String("method", method))
	} else {
		state.logger.Debug("continuation call failed", slog.String("method", method), slog.Int("code", code))
	}
	lowerSessionContinuation(state)
	return continuationSignal{
		Verdict: agentcore.ResumeReplaced,
		Cause:   fmt.Errorf("%s answered with error code %d", method, code),
	}, nil
}

// lowerSessionContinuation lowers sessionContinuation through the pump's own
// mutation path, the outcome required whenever a continuation call, or the
// negative control that precedes it, does not confirm the entry.
func lowerSessionContinuation(state *sessionState) {
	state.inbox.Put(pumpItem{control: &pumpControl{query: &replayQuery{}}})
}

// createNewSession creates a fresh session with session/new: the route taken
// for every session that does not continue another. A failure here is reported
// exactly as it would have been had continuation never been attempted.
func createNewSession(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (string, json.RawMessage, *domain.AgentError) {
	resp, raw, agentErr := doNewSession(ctx, state, cwd, servers)
	if agentErr != nil {
		return "", nil, agentErr
	}
	// A verification session is never resumed.
	if !state.credentialVerification {
		state.origins.record(cwd, string(resp.SessionID))
	}
	return string(resp.SessionID), raw, nil
}
