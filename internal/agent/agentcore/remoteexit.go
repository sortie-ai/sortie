package agentcore

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// awaitRemoteExit returns once the agent tree of the remote launch that holds
// barrier has exited, or once the supervisor's own worst case for ending an
// agent has passed. The ssh client of a stopped launch ends before the remote
// tree does, so this wait runs on a second connection; it never fails the
// turn: a host that cannot be reached cannot be waited on.
func (t LaunchTarget) awaitRemoteExit(ctx context.Context, logger *slog.Logger, barrier string) {
	bound := sshutil.TeardownBound(t.StopGrace)
	waitCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	_, result, err := t.captureRemote(waitCtx, sshutil.AwaitExitCommand(barrier), nil, 0, nil)
	switch {
	case waitCtx.Err() != nil:
		logger.Warn("remote agent exit not confirmed inside the teardown bound", slog.Duration("bound", bound))
	case err != nil:
		logger.Debug("remote agent exit wait did not start", slog.Any("error", err))
	case sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)):
		logger.Debug("remote agent exit wait could not connect")
	}
}
