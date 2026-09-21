package agentcore

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// ConnectionFailedError is the error for an ssh client that exited
// with the status OpenSSH reserves for its own connection failures.
func ConnectionFailedError() *domain.AgentError {
	return &domain.AgentError{
		Kind:    domain.ErrPortExit,
		Message: "ssh connection failed",
		Err:     sshutil.ErrConnectionFailed,
	}
}

// ConnectionFailedForRequest reports whether an ssh connection failure
// should replace the request's own outcome: sshFailed holds and the
// request produced no terminal result.
func ConnectionFailedForRequest(sshFailed, hasTerminalResult bool) bool {
	return sshFailed && !hasTerminalResult
}

// ReaperConnectionFailed reports whether a remote session's ssh subprocess
// exited with OpenSSH's connection-failure status. It waits up to drainGrace
// so a caller that saw the stream end does not race the reap.
func ReaperConnectionFailed(remote bool, reaper *procutil.Reaper, drainGrace time.Duration) bool {
	if !remote || reaper == nil {
		return false
	}
	if drainGrace <= 0 {
		drainGrace = procutil.DefaultDrainGrace
	}
	select {
	case <-reaper.Done():
	case <-time.After(drainGrace):
		return false
	}
	return sshutil.ConnectionFailed(procutil.ExtractExitCode(reaper.Err()))
}
