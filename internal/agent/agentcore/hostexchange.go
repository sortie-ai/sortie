package agentcore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// RunHostExchange runs cmd on the session's host as [LaunchTarget.RunHostCommand]
// does and hands the command's standard input and output to exchange.
//
// exchange runs on the caller's goroutine. Standard output is streamed to it,
// and a read past cmd.Limit (zero means 64 KiB) fails. Standard error is
// discarded. When exchange returns, standard input closes, the command's
// process group is stopped within StopGrace, and the call returns after the
// reap; the exit status never turns a nil exchange result into an error.
//
// A done ctx stops the command, which ends its output, and the result wraps
// ctx.Err(). A command that does not start wraps [ErrHostCommandFailed]. An
// exchange error on a remote launch whose ssh client exited by itself with the
// connection-failure status wraps [sshutil.ErrConnectionFailed]; any other
// exchange error comes back labelled by the command.
//
// Both streams may carry a credential: RunHostExchange neither logs them nor
// puts them in an error, and the caller must do the same.
func (t LaunchTarget) RunHostExchange(ctx context.Context, env []string, cmd HostCommand, exchange func(stdin io.Writer, stdout io.Reader) error) error {
	if err := t.checkLaunchWords(cmd); err != nil {
		return err
	}
	limit := cmd.Limit
	if limit <= 0 {
		limit = hostCommandLimit
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	stdinReader, stdin := io.Pipe()
	defer func() { _ = stdinReader.Close() }() //nolint:errcheck // unblocks a pending write once the command is gone

	proc, err := t.exchangeProcess(runCtx, env, cmd, stdinReader)
	if err != nil {
		return err
	}
	procutil.SetGroupCancel(proc, t.StopGrace)
	logger := slog.Default()
	pipes, _, err := procutil.StartWithOwnedPipes(runCtx, proc, logger)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("host command %q: %w", cmd.label(), ctx.Err())
		}
		return fmt.Errorf("start host command %q: %w: %w", cmd.label(), ErrHostCommandFailed, err)
	}
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup; runs after the reap wait below

	go func() { _, _ = io.Copy(io.Discard, pipes.Stderr) }() //nolint:errcheck // ends when pipes close

	reaper := procutil.StartReaper(proc, logger)
	exchangeDone := make(chan struct{})
	procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: exchangeDone,
		Logger:     logger,
	})

	exchangeErr := exchange(stdin, &limitedOutput{
		r:    pipes.Stdout,
		room: limit,
		err:  fmt.Errorf("host command %q printed more than %d bytes", cmd.label(), limit),
	})
	close(exchangeDone)
	_ = stdin.Close() //nolint:errcheck // the exchange is over
	stop()
	<-reaper.Done()

	switch {
	case exchangeErr == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("host command %q: %w", cmd.label(), ctx.Err())
	case t.RemoteCommand != "" && !reaper.Stopped() && sshutil.ConnectionFailed(procutil.ExtractExitCode(reaper.Err())):
		return fmt.Errorf("remote command: %w", sshutil.ErrConnectionFailed)
	}
	return fmt.Errorf("host command %q: %w", cmd.label(), exchangeErr)
}

// exchangeProcess builds the unstarted process for cmd with stdin as its
// standard input: the command itself for a local session, the session's own
// SSH launch for a remote one.
func (t LaunchTarget) exchangeProcess(ctx context.Context, env []string, hc HostCommand, stdin io.Reader) (*exec.Cmd, error) {
	if t.RemoteCommand == "" {
		proc, err := t.localHostCmd(ctx, env, hc)
		if err != nil {
			return nil, err
		}
		proc.Stdin = stdin
		return proc, nil
	}
	runner := t
	var args []string
	runner.RemoteCommand, args = t.remoteHostCommand(hc)
	proc, agentErr := runner.AuxiliaryCommand(ctx, args, stdin, nil, hc.Settings...)
	if agentErr != nil {
		return nil, fmt.Errorf("host command %q: %w", hc.label(), agentErr)
	}
	return proc, nil
}

// limitedOutput fails the read that crosses room instead of delivering bytes
// past it, and keeps failing afterwards.
type limitedOutput struct {
	r    io.Reader
	room int64
	err  error
	over bool
}

func (l *limitedOutput) Read(p []byte) (int, error) {
	if l.over {
		return 0, l.err
	}
	p = p[:min(int64(len(p)), l.room+1)]
	n, err := l.r.Read(p)
	if int64(n) > l.room {
		l.over = true
		return int(l.room), l.err
	}
	l.room -= int64(n)
	return n, err
}
