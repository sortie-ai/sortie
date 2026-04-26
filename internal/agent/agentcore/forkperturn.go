package agentcore

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

const (
	// stdoutScannerMaxTokenSize is the maximum JSONL token size for the
	// stdout bufio.Scanner. Lines exceeding this limit cause
	// scanner.Err() to return bufio.ErrTooLong, which is handled as a
	// scan-error path (TurnFailed / ErrPortExit, or TurnCancelled if ctx
	// was already done).
	stdoutScannerMaxTokenSize = 10 * 1024 * 1024 // 10 MB

	// stdoutScannerInitialBufSize is the initial buffer capacity
	// allocated for the stdout scanner. Growth up to
	// stdoutScannerMaxTokenSize is handled by bufio.Scanner automatically.
	stdoutScannerInitialBufSize = 64 * 1024 // 64 KB

	// stopGracePeriod is the time between SIGTERM and SIGKILL in Stop
	// and in the cmd.WaitDelay grace period.
	stopGracePeriod = 5 * time.Second
)

// ForkPerTurnHooks provides the adapter-specific behavior points plugged into
// [ForkPerTurnSession]. All function fields are required except
// EmitSessionStartID, which is nil for adapters that emit EventSessionStarted
// from within ParseLine (e.g. Claude Code).
type ForkPerTurnHooks struct {
	// BuildArgs is called once per RunTurn before the subprocess starts.
	// turn is the prospective 1-based turn number for this session
	// instance. The skeleton commits it only after the subprocess starts
	// successfully, so failed starts may pass the same turn value again.
	// prompt is the rendered task prompt passed by the orchestrator.
	//
	// The adapter uses turn to decide session-continuation flags (e.g.
	// --resume vs. --continue) and must not perform I/O here.
	// The returned slice is appended to LaunchTarget.Args; it must not
	// alias LaunchTarget.Args.
	BuildArgs func(turn int, prompt string) []string

	// ParseLine is called for each line read from stdout.
	// The adapter calls emit for any normalized events derived from line.
	// pid is the PID of the active subprocess as a string; adapters that
	// emit EventSessionStarted from within ParseLine (e.g. Claude Code
	// on the "system/init" event) use this to populate AgentPID.
	//
	// When parsing fails, ParseLine returns a non-nil err. The skeleton
	// then calls [EmitMalformed] and continues to the next line. The
	// returned result is ignored on error.
	//
	// When parsing succeeds and line represents a terminal event (the
	// adapter-defined event that signals turn completion), ParseLine
	// returns a non-nil result reference and a nil err. The skeleton
	// stores this as lastParsed, overwriting any previous value.
	//
	// For non-terminal lines (the common case), ParseLine returns
	// (nil, nil). The skeleton does not store nil results.
	//
	// ParseLine is called on a single goroutine and is not required to
	// be concurrency-safe.
	ParseLine func(line []byte, emit func(domain.AgentEvent), pid string) (result any, err error)

	// GetUsage returns the token usage accumulated so far in this turn.
	// The skeleton calls this when constructing the TurnResult for Arms
	// 1–5 of the decision tree (cancellation, scan-error, exit-127, and
	// signal paths). The adapter implements this as a one-line closure
	// over its *[UsageAccumulator]: func() domain.TokenUsage { return
	// acc.Snapshot() }.
	//
	// GetUsage is called after the scan loop completes and is always
	// called on RunTurn's goroutine.
	GetUsage func() domain.TokenUsage

	// GetSessionID returns the adapter's current session identifier.
	// The skeleton calls this when constructing the TurnResult for Arms
	// 1–5 of the decision tree (cancellation, scan-error, exit-127, and
	// signal paths) to populate TurnResult.SessionID. The returned value
	// may be empty on the first turn before the agent has assigned a
	// session identifier.
	GetSessionID func() string

	// OnFinalize determines the final TurnResult and error from the
	// subprocess exit state. The skeleton calls OnFinalize only on the
	// success paths of the post-Wait decision tree (arms 6–10). Arms 1–5
	// are handled entirely by the skeleton.
	//
	// emit is the per-turn event callback passed to RunTurn. OnFinalize
	// MUST use this to emit the terminal event (EventTurnCompleted or
	// EventTurnFailed) for arms 6–10.
	//
	// lastParsed is the last non-nil value returned by ParseLine during
	// the scan loop, or nil if no terminal event was observed.
	// exitCode is the process exit code extracted by
	// [procutil.ExtractExitCode]. stderrLines contains all lines drained
	// from the stderr pipe before cmd.Wait() was called.
	//
	// The skeleton calls [procutil.EmitWarnLines] automatically when
	// OnFinalize returns a non-nil *[domain.AgentError]. The adapter MUST
	// NOT call [procutil.EmitWarnLines] inside OnFinalize.
	OnFinalize func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError)

	// EmitSessionStartID, when non-nil, causes the skeleton to call
	// [EmitSessionStarted] immediately after cmd.Start() succeeds and
	// before the scan loop begins. The function returns the adapter's
	// current session ID, which may be empty on the first turn.
	//
	// Set to nil for adapters that emit EventSessionStarted from within
	// ParseLine (e.g., Claude Code). Set to a non-nil closure for
	// adapters that must emit EventSessionStarted before the scan loop
	// (e.g., Copilot CLI).
	EmitSessionStartID func() string
}

// ForkPerTurnSession owns the entire fork-per-turn subprocess lifecycle
// for a single agent session. For each call to RunTurn, the skeleton
// forks a new subprocess, scans its stdout for JSONL events, and
// determines the TurnResult via the hooks provided at construction.
//
// Safe for concurrent use between RunTurn and Stop: Stop may be called
// from a different goroutine while RunTurn is executing. RunTurn MUST
// NOT be called concurrently with itself for the same session; the
// orchestrator serializes turns per session.
//
// Construct with [NewForkPerTurnSession]. The zero value is invalid.
type ForkPerTurnSession struct {
	target *LaunchTarget
	hooks  ForkPerTurnHooks
	logger *slog.Logger

	// turns is the count of subprocesses successfully started by this
	// session. Incremented only after cmd.Start() returns nil. Accessed
	// only from RunTurn's goroutine; not protected by mu.
	turns int

	mu     sync.Mutex
	proc   *os.Process
	waitCh chan struct{}
}

// NewForkPerTurnSession constructs a ForkPerTurnSession. target must be a
// pointer to a [LaunchTarget] obtained from [ResolveLaunchTarget] during
// StartSession. The skeleton holds this pointer so mutations to the target
// between turns are observed by subsequent RunTurn calls. logger must be
// non-nil. All function fields in hooks except EmitSessionStartID are
// required; NewForkPerTurnSession panics if any required field is nil or if
// logger is nil.
func NewForkPerTurnSession(
	target *LaunchTarget,
	hooks ForkPerTurnHooks,
	logger *slog.Logger,
) *ForkPerTurnSession {
	if hooks.BuildArgs == nil {
		panic("agentcore: ForkPerTurnHooks.BuildArgs must be non-nil")
	}
	if hooks.ParseLine == nil {
		panic("agentcore: ForkPerTurnHooks.ParseLine must be non-nil")
	}
	if hooks.GetUsage == nil {
		panic("agentcore: ForkPerTurnHooks.GetUsage must be non-nil")
	}
	if hooks.GetSessionID == nil {
		panic("agentcore: ForkPerTurnHooks.GetSessionID must be non-nil")
	}
	if hooks.OnFinalize == nil {
		panic("agentcore: ForkPerTurnHooks.OnFinalize must be non-nil")
	}
	if logger == nil {
		panic("agentcore: ForkPerTurnSession logger must be non-nil")
	}
	return &ForkPerTurnSession{
		target: target,
		hooks:  hooks,
		logger: logger,
	}
}

// SetLogger replaces the logger used for future internal lifecycle and stderr
// log records emitted by [RunTurn]. logger must be non-nil.
func (s *ForkPerTurnSession) SetLogger(logger *slog.Logger) {
	if logger == nil {
		panic("agentcore: ForkPerTurnSession logger must be non-nil")
	}
	s.logger = logger
}

// RunTurn executes one agent turn. It forks a subprocess, scans its
// stdout via the ten-armed decision tree, and returns the outcome.
//
// ctx controls the turn lifetime. Cancellation triggers a graceful
// shutdown (SIGTERM to the process group) followed by a 5-second grace
// period before force-kill.
//
// prompt is passed verbatim to hooks.BuildArgs.
//
// emit receives all normalized [domain.AgentEvent] values produced
// during the turn. It must be non-nil; RunTurn panics otherwise.
//
// Returns ([domain.TurnResult], nil) on success. Returns
// ([domain.TurnResult], *[domain.AgentError]) on all failure and
// cancellation paths; the returned TurnResult.Usage is populated
// with whatever token counts were accumulated before the failure.
func (s *ForkPerTurnSession) RunTurn(
	ctx context.Context,
	prompt string,
	emit func(domain.AgentEvent),
) (domain.TurnResult, error) {
	if emit == nil {
		panic("agentcore: ForkPerTurnSession.RunTurn: emit must be non-nil")
	}

	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()

	prospectiveTurn := s.turns + 1
	cmdArgs := s.hooks.BuildArgs(prospectiveTurn, prompt)

	var cmd *exec.Cmd
	if s.target.RemoteCommand != "" {
		sshArgs := sshutil.BuildSSHArgs(
			s.target.SSHHost,
			s.target.WorkspacePath,
			s.target.RemoteCommand,
			cmdArgs,
			sshutil.SSHOptions{StrictHostKeyChecking: s.target.SSHStrictHostKeyChecking},
		)
		cmd = exec.CommandContext(cmdCtx, s.target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		allArgs := append(s.target.Args[:len(s.target.Args):len(s.target.Args)], cmdArgs...) //nolint:gocritic // intentional: target.Args has cap==len so append always allocates
		cmd = exec.CommandContext(cmdCtx, s.target.Command, allArgs...)                      //nolint:gosec // args are constructed programmatically
	}
	procutil.SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		return procutil.SignalGraceful(cmd.Process.Pid)
	}
	cmd.WaitDelay = stopGracePeriod
	cmd.Dir = s.target.WorkspacePath
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdout pipe",
			Err:     err,
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stderr pipe",
			Err:     err,
		}
	}

	// Lock before Start to prevent a race with Stop.
	s.mu.Lock()
	err = cmd.Start()
	if err != nil {
		s.mu.Unlock()
		if ctx.Err() != nil {
			EmitTurnCancelled(emit, "context cancelled")
			return domain.TurnResult{
					SessionID:  s.hooks.GetSessionID(),
					ExitReason: domain.EventTurnCancelled,
				}, &domain.AgentError{
					Kind:    domain.ErrTurnCancelled,
					Message: "turn cancelled",
					Err:     ctx.Err(),
				}
		}
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to start subprocess",
			Err:     err,
		}
	}
	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		s.logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}
	s.turns = prospectiveTurn
	s.proc = cmd.Process
	s.waitCh = make(chan struct{})
	localWaitCh := s.waitCh
	pidStr := strconv.Itoa(cmd.Process.Pid)
	s.mu.Unlock()

	if s.hooks.EmitSessionStartID != nil {
		EmitSessionStarted(emit, pidStr, s.hooks.EmitSessionStartID())
	}

	stderrCollector := procutil.NewStderrCollector(stderrPipe, s.logger)

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, stdoutScannerInitialBufSize), stdoutScannerMaxTokenSize)

	var lastParsed any

	for scanner.Scan() {
		line := scanner.Bytes()
		result, parseErr := s.hooks.ParseLine(line, emit, pidStr)
		if parseErr != nil {
			EmitMalformed(emit, line)
			continue
		}
		if result != nil {
			lastParsed = result
		}
	}

	if scanErr := scanner.Err(); scanErr != nil {
		cancelCmd()
		stderrLines := stderrCollector.Lines()     // drain before cmd.Wait() closes the pipe
		cmd.Wait()                                 //nolint:errcheck,gosec // best-effort reap; exit code is irrelevant on scanner failure
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)
		close(localWaitCh)
		s.mu.Lock()
		s.proc = nil
		s.waitCh = nil
		s.mu.Unlock()

		// Context cancellation propagates through exec.CommandContext
		// and can surface as a pipe read error. Treat as cancellation.
		if ctx.Err() != nil {
			EmitTurnCancelled(emit, "context cancelled")
			return domain.TurnResult{
					SessionID:  s.hooks.GetSessionID(),
					ExitReason: domain.EventTurnCancelled,
					Usage:      s.hooks.GetUsage(),
				}, &domain.AgentError{
					Kind:    domain.ErrTurnCancelled,
					Message: "turn cancelled",
					Err:     ctx.Err(),
				}
		}

		procutil.EmitWarnLines(stderrLines, s.logger)
		EmitTurnFailed(emit, "stdout read error: "+scanErr.Error(), 0)
		return domain.TurnResult{
				SessionID:  s.hooks.GetSessionID(),
				ExitReason: domain.EventTurnFailed,
				Usage:      s.hooks.GetUsage(),
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "stdout scanner error",
				Err:     scanErr,
			}
	}

	// Drain stderr before cmd.Wait() to avoid losing buffered data:
	// cmd.Wait() closes the pipe read end, which can prevent the drain
	// goroutine from reading data that the process already wrote.
	stderrLines := stderrCollector.Lines()
	waitErr := cmd.Wait()
	procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
	procutil.CleanupProcess(cmd.Process.Pid)
	close(localWaitCh)
	s.mu.Lock()
	s.proc = nil
	s.waitCh = nil
	s.mu.Unlock()

	if ctx.Err() != nil {
		EmitTurnCancelled(emit, "context cancelled")
		return domain.TurnResult{
				SessionID:  s.hooks.GetSessionID(),
				ExitReason: domain.EventTurnCancelled,
				Usage:      s.hooks.GetUsage(),
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "turn cancelled",
				Err:     ctx.Err(),
			}
	}

	exitCode := procutil.ExtractExitCode(waitErr)

	if exitCode == 127 {
		procutil.EmitWarnLines(stderrLines, s.logger)
		EmitTurnFailed(emit, "agent binary not found", 0)
		return domain.TurnResult{
				SessionID:  s.hooks.GetSessionID(),
				ExitReason: domain.EventTurnFailed,
				Usage:      s.hooks.GetUsage(),
			}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "exit code 127",
			}
	}

	if procutil.WasSignaled(waitErr) {
		EmitTurnCancelled(emit, "killed by signal")
		return domain.TurnResult{
				SessionID:  s.hooks.GetSessionID(),
				ExitReason: domain.EventTurnCancelled,
				Usage:      s.hooks.GetUsage(),
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "killed by signal",
			}
	}

	// Arms 6–10: delegated to OnFinalize.
	// The explicit nil check prevents a typed-nil *domain.AgentError from
	// becoming a non-nil error interface on the success path.
	// The skeleton calls EmitWarnLines when agentErr is non-nil, so
	// OnFinalize must not call it.
	result, agentErr := s.hooks.OnFinalize(emit, lastParsed, exitCode, stderrLines)
	if agentErr != nil {
		procutil.EmitWarnLines(stderrLines, s.logger)
		return result, agentErr
	}
	return result, nil
}

// Stop signals the active subprocess to exit gracefully and waits for RunTurn
// to complete cleanup. If no subprocess is running, Stop returns immediately
// with nil.
//
// Shutdown sequence:
//  1. SIGTERM to process group ([procutil.SignalGraceful])
//  2. Wait up to 5 seconds for RunTurn to close waitCh
//  3. If timeout elapses: SIGKILL to process group
//  4. If ctx is cancelled before waitCh closes: SIGKILL and return ctx.Err()
func (s *ForkPerTurnSession) Stop(ctx context.Context) error {
	s.mu.Lock()
	proc := s.proc
	waitCh := s.waitCh
	s.proc = nil
	s.mu.Unlock()

	if proc == nil {
		return nil
	}

	_ = procutil.SignalGraceful(proc.Pid) //nolint:errcheck // best-effort signal; process may already be dead

	select {
	case <-waitCh:
		return nil
	case <-time.After(stopGracePeriod):
		_ = procutil.KillProcessGroup(proc.Pid) //nolint:errcheck // best-effort kill
		return nil
	case <-ctx.Done():
		_ = procutil.KillProcessGroup(proc.Pid) //nolint:errcheck // best-effort kill
		return ctx.Err()
	}
}
