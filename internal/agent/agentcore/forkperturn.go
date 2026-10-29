package agentcore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// ForkPerTurnHooks provides the adapter-specific behavior points plugged into
// [ForkPerTurnSession]. BuildArgs, ParseLine, GetUsage, GetSessionID, and
// OnFinalize are required. EmitSessionStartID, PromptOnStdin,
// FirstEventTimeout, SettleUsage, PullWindows, RefusalWindowReset, TurnEnv,
// Tools, and ToolPlacement are optional, and each zero value keeps the default turn
// behavior.
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
	// spend is the turn's spend evidence, one per RunTurn call and shared
	// with OnFinalize. ParseLine marks on it what the line proves and
	// MUST NOT retain the pointer past the turn.
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
	ParseLine func(line []byte, emit func(domain.AgentEvent), pid string, spend *TurnSpend) (result any, err error)

	// GetUsage returns the session's run-cumulative token usage snapshot
	// together with the session's measurement verdict. usage is the
	// run-cumulative snapshot, not a per-turn figure. byModel is its
	// per-model split, nil when the session records none. measured reports
	// whether the session has observed at least one usage measurement by
	// the time of the call. Within one session, once a call returns
	// measured true, every later call returns true. A session whose
	// resolved usage arrival is none returns measured false on every
	// call.
	//
	// GetUsage is always called on RunTurn's goroutine.
	GetUsage func() (usage domain.TokenUsage, byModel map[string]domain.TokenUsage, measured bool)

	// GetSessionID returns the adapter's current session identifier.
	// The skeleton calls this to populate TurnResult.SessionID on every
	// return it builds itself, including a start-failure return. The
	// returned value may be empty on the first turn before the agent has
	// assigned a session identifier.
	GetSessionID func() string

	// OnFinalize determines the final TurnResult and error from the
	// subprocess exit state. The skeleton calls OnFinalize after the
	// subprocess has exited following the scan loop, for every ending it
	// has not already classified as a cancellation, an [EndTurn] signal,
	// a first-event timeout, a stdout scan error, an ssh connection
	// failure, or exit code 127, so a non-zero exit
	// code, including one from a signal Sortie did not send, reaches it.
	//
	// emit is the per-turn event callback passed to RunTurn. OnFinalize
	// MUST use this to emit the terminal event (EventTurnCompleted or
	// EventTurnFailed).
	//
	// lastParsed is the last non-nil value returned by ParseLine during
	// the scan loop, or nil if no terminal event was observed.
	// exitCode is the process exit code extracted by
	// [procutil.ExtractExitCode]. waitErr is the wait error the exit code
	// came from, nil for exit 0; the adapter MUST copy it onto
	// [TurnEvidence.WaitErr]. stderrLines contains the lines
	// collected from the stderr pipe by the time the skeleton's bounded
	// drain ended, which runs after the reap rather than before it; a
	// drain that hit its bound reports the abandonment marker instead.
	// earlyExit is non-nil exactly when the turn's process exited on its
	// own and no readable line reached standard output; the adapter MUST copy it onto [TurnEvidence.EarlyExit] in
	// the evidence it finalizes. spend is the turn's spend evidence that
	// ParseLine marked on; the adapter completes it and passes *spend to
	// [FinalizeTurn] or [TurnEndUsage.Finalize].
	//
	// The skeleton calls [procutil.EmitWarnLines] automatically when
	// OnFinalize returns a non-nil *[domain.AgentError]. The adapter MUST
	// NOT call [procutil.EmitWarnLines] inside OnFinalize.
	OnFinalize func(emit func(domain.AgentEvent), lastParsed any, exitCode int, waitErr error, stderrLines []string, earlyExit *domain.AgentError, spend *TurnSpend) (domain.TurnResult, *domain.AgentError)

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

	// PromptOnStdin writes the prompt to standard input, after the SSH
	// preamble on a remote launch.
	PromptOnStdin bool

	// FirstEventTimeout bounds the wait for the turn's first accepted
	// line; a rejected line restarts it. Zero disables it.
	FirstEventTimeout time.Duration

	// SettleUsage runs once per started turn, on RunTurn's goroutine after
	// the process is reaped and its stderr drained, on every ending but an
	// [EndTurn] one. ctx is detached from the turn's cancellation, so the
	// adapter bounds its own work. Events it emits reach the turn's
	// consumer ahead of the terminal event, and GetUsage reflects what it
	// settled. Nil disables it.
	SettleUsage func(ctx context.Context, emit func(domain.AgentEvent), spend *TurnSpend)

	// PullWindows runs once per started turn, on RunTurn's goroutine, after
	// SettleUsage and before the terminal event, with the ending RunTurn
	// observed. The usage component decides from the ending whether a pull
	// runs. ctx is the turn's own, so the adapter bounds its own work. A
	// non-empty result reaches the consumer as usage windows; an error is
	// logged and does not change how the turn ends. Nil disables it.
	PullWindows func(ctx context.Context, ending usage.TurnEnding) ([]domain.UsageWindow, error)

	// RefusalWindowReset reports, once OnFinalize returned for a turn,
	// whether that turn's usage_limit refusal reads its reset from the
	// windows PullWindows returned. A refusal that carries no reset then
	// takes the earliest exhausted window's. Nil means no turn's does.
	RefusalWindowReset func() bool

	// TurnEnv returns the started process's environment and the settings
	// an SSH launch carries. Nil keeps the host environment and no
	// settings.
	TurnEnv func() (environ []string, sshEnv []sshutil.EnvVar)

	// Tools is the session's tool-server delivery; Stop closes it. Nil
	// delivers no tool servers.
	Tools *ToolSession

	// ToolPlacement is required when Tools is non-nil and the target is
	// remote.
	ToolPlacement PlaceFunc
}

// endTurnError is the ParseLine error that ends the turn.
type endTurnError struct {
	kind    domain.AgentErrorKind
	message string
}

func (e *endTurnError) Error() string { return e.message }

// EndTurn returns the error a [ForkPerTurnHooks] ParseLine hook returns to
// end the turn as a failure of the given kind and message.
func EndTurn(kind domain.AgentErrorKind, message string) error {
	return &endTurnError{kind: kind, message: message}
}

// turnTrigger names what made the turn stop its own process.
type turnTrigger int

const (
	triggerNone turnTrigger = iota
	triggerFirstEventTimeout
	triggerEndTurn
	triggerReadFailure
)

// turnEnding reports how a finished turn ended, in the terms the usage
// component's pull policy reads.
func turnEnding(cancelled, endedByHook bool, trigger turnTrigger, scanErr error) usage.TurnEnding {
	switch {
	case cancelled:
		return usage.EndingCancelled
	case endedByHook:
		return usage.EndingProtocolBroken
	case trigger == triggerFirstEventTimeout:
		return usage.EndingTimedOut
	case trigger == triggerReadFailure, scanErr != nil && !errors.Is(scanErr, procutil.ErrStdoutAbandoned):
		return usage.EndingReadFailed
	default:
		return usage.EndingAnswered
	}
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
	logger *slog.Logger // session's base logger; immutable

	// turns is the count of subprocesses successfully started by this
	// session. Incremented only after cmd.Start() returns nil. Accessed
	// only from RunTurn's goroutine; not protected by mu.
	turns int

	mu         sync.Mutex
	group      *procutil.Group
	waitCh     chan struct{}
	turnLogger *slog.Logger // active turn's logger, read by Stop; guarded by mu

	// drainGrace bounds the wait for the stderr drain before each
	// cmd.Wait call. Set by NewForkPerTurnSession; overridden only by
	// tests in this package.
	drainGrace time.Duration

	// stopGrace bounds the graceful phase Stop waits before it force-
	// terminates the process group, and the wait cmd.WaitDelay allows
	// a cancelled turn before RunTurn's SetGroupCancel setup escalates
	// to a force kill. Set by NewForkPerTurnSession from the operator's
	// configured stop grace.
	stopGrace time.Duration
}

// startWithOwnedPipes is the turn's process start. Only a test replaces
// it, to return a start failure no real launch can be made to produce at
// a chosen moment.
var startWithOwnedPipes = procutil.StartWithOwnedPipes

// NewForkPerTurnSession constructs a ForkPerTurnSession. target must be a
// pointer to a [LaunchTarget] obtained from [ResolveLaunchTarget] during
// StartSession. The skeleton holds this pointer so mutations to the target
// between turns are observed by subsequent RunTurn calls. logger must be
// non-nil and is the session's base logger: each turn derives its own from
// it, adding the session identifier once GetSessionID reports one. The
// required hooks are BuildArgs, ParseLine, GetUsage, GetSessionID, and
// OnFinalize; NewForkPerTurnSession panics if any is nil or if logger is
// nil.
//
// stopGraceMS is the operator's configured agent.stop_grace_ms, resolved
// through [procutil.StopGrace]; a non-positive value therefore resolves
// to [procutil.DefaultStopGrace].
func NewForkPerTurnSession(
	target *LaunchTarget,
	hooks ForkPerTurnHooks,
	logger *slog.Logger,
	stopGraceMS int,
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
	if hooks.Tools != nil && target.RemoteCommand != "" && hooks.ToolPlacement == nil {
		panic("agentcore: ForkPerTurnHooks.ToolPlacement must be non-nil when Tools is set on a remote launch")
	}
	return &ForkPerTurnSession{
		target:     target,
		hooks:      hooks,
		logger:     logger,
		drainGrace: procutil.DefaultDrainGrace,
		stopGrace:  procutil.StopGrace(stopGraceMS),
	}
}

// loggerFor derives a turn's logger from the base logger and sessionID.
func (s *ForkPerTurnSession) loggerFor(sessionID string) *slog.Logger {
	if sessionID == "" {
		return s.logger
	}
	return logging.WithSession(s.logger, sessionID)
}

// RunTurn executes one agent turn. It forks a subprocess, scans its
// stdout, and returns the outcome.
//
// ctx controls the turn lifetime. Cancellation triggers a graceful
// shutdown (SIGTERM to the process group) followed by the session's
// configured stop grace before force-kill.
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
	if s.hooks.Tools != nil {
		emit = s.hooks.Tools.Emit(emit, nil)
	}

	var spend TurnSpend

	sessionID := s.hooks.GetSessionID()
	logger := s.loggerFor(sessionID)

	// A result the skeleton builds is never a completed turn, so it
	// carries the flag but is not coverable.
	unfinished := func(result domain.TurnResult) domain.TurnResult {
		spend.applyTo(&result, false)
		return result
	}

	// ctx is read only to pick the message: whether the turn was
	// cancelled is the launch record's verdict, not the context's.
	cancelledTurn := func() (domain.TurnResult, error) {
		usage, byModel, measured := s.hooks.GetUsage()
		agentErr := &domain.AgentError{Kind: domain.ErrTurnCancelled, Message: "turn cancelled"}
		if ctx.Err() != nil {
			EmitTurnCancelled(emit, "context cancelled", usage, byModel)
			agentErr.Err = ctx.Err()
		} else {
			EmitTurnCancelled(emit, "turn cancelled", usage, byModel)
		}
		return unfinished(domain.TurnResult{
			SessionID:     s.hooks.GetSessionID(),
			ExitReason:    domain.EventTurnCancelled,
			Usage:         usage,
			UsageMeasured: measured,
		}), agentErr
	}

	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()

	prospectiveTurn := s.turns + 1
	cmdArgs := s.hooks.BuildArgs(prospectiveTurn, prompt)

	environ := os.Environ()
	var sshEnv []sshutil.EnvVar
	if s.hooks.TurnEnv != nil {
		environ, sshEnv = s.hooks.TurnEnv()
	}
	if s.hooks.Tools != nil && s.target.RemoteCommand == "" {
		environ = append(slices.Clip(environ), s.hooks.Tools.LocalEnv()...)
	}

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if s.target.RemoteCommand != "" {
		opts := s.target.SSHOptions(sshEnv...)
		if s.hooks.Tools != nil {
			opts.Forward = s.hooks.Tools.Forward(s.hooks.ToolPlacement)
		}
		launch = sshutil.BuildSSHLaunch(
			s.target.SSHHost,
			s.target.WorkspacePath,
			s.target.RemoteCommand,
			cmdArgs,
			opts,
		)
		cmd = exec.CommandContext(cmdCtx, s.target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
		cmd.Stdin = s.turnStdin(launch.StdinReader(), prompt)
	} else {
		allArgs := append(slices.Clip(s.target.Args), cmdArgs...)       //nolint:gocritic // intentional: target.Args has cap==len so append always allocates
		cmd = exec.CommandContext(cmdCtx, s.target.Command, allArgs...) //nolint:gosec // args are constructed programmatically
		cmd.Stdin = s.turnStdin(nil, prompt)
	}
	procutil.SetGroupCancel(cmd, s.stopGrace)
	cmd.Env = environ

	if bindErr := s.target.BindWorkspace(cmd); bindErr != nil {
		usage, _, measured := s.hooks.GetUsage()
		return unfinished(domain.TurnResult{
			SessionID:     s.hooks.GetSessionID(),
			Usage:         usage,
			UsageMeasured: measured,
		}), bindErr
	}

	// On Windows os/exec resolves the executable before it checks the
	// context, so a turn already cancelled with a missing binary would
	// otherwise surface as a failed start rather than a cancellation.
	if ctx.Err() != nil {
		return cancelledTurn()
	}

	// Lock before starting the pipes and the process together, so a Stop
	// arriving in a reopened window cannot read s.group == nil and miss
	// signaling a process that was about to be recorded.
	s.mu.Lock()
	pipes, group, err := startWithOwnedPipes(cmdCtx, cmd, logger)
	if err != nil {
		s.mu.Unlock()

		var startErr *procutil.StartError
		isStartErr := errors.As(err, &startErr)
		if isStartErr && startErr.Cancelled {
			return cancelledTurn()
		}

		usage, _, measured := s.hooks.GetUsage()
		startFailed := unfinished(domain.TurnResult{
			SessionID:     s.hooks.GetSessionID(),
			Usage:         usage,
			UsageMeasured: measured,
		})

		if !isStartErr {
			return startFailed, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to start subprocess",
				Err:     err,
			}
		}

		switch startErr.Stage {
		case procutil.StageStdoutPipe:
			return startFailed, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to create stdout pipe",
				Err:     startErr.Err,
			}
		case procutil.StageStderrPipe:
			return startFailed, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to create stderr pipe",
				Err:     startErr.Err,
			}
		default: // procutil.StageProcessStart, procutil.StageProcessResume
			return startFailed, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to start subprocess",
				Err:     startErr.Err,
			}
		}
	}

	// The turn's only close of either pipe end: exec.Cmd closes neither
	// once the pipes are caller-owned, and every return past this point
	// sits behind the stderr collector's own bound below, so a deferred
	// close here cannot cut that bound short.
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup

	s.turns = prospectiveTurn
	s.group = group
	s.turnLogger = logger
	s.waitCh = make(chan struct{})
	localWaitCh := s.waitCh
	pidStr := strconv.Itoa(cmd.Process.Pid)
	s.mu.Unlock()

	// The orchestrator reads the process ID from any event, so every event
	// after the start carries it, whichever hook or arm emitted it.
	forward := emit
	emit = func(event domain.AgentEvent) {
		if event.AgentPID == "" {
			event.AgentPID = pidStr
		}
		forward(event)
	}

	if s.hooks.EmitSessionStartID != nil {
		EmitSessionStarted(emit, pidStr, s.hooks.EmitSessionStartID())
	}

	// A forwarded launch holds its standard input until the collector reads
	// the forward's outcome from standard error.
	stderrCollector := procutil.NewStderrCollector(launch.WatchStderr(pipes.Stderr), logger)
	reader := procutil.NewStdoutReader(pipes.Stdout, logger)
	reaper := procutil.StartReaper(cmd, logger)

	var lastParsed any
	var output OutputWatch

	reaped := false
	trigger := triggerNone
	stoppedBeforeTrigger := false
	var endTurn *endTurnError

	// A positive timeout is armed from the start until the first accepted
	// line or the reap; a nil channel keeps the select arm inert.
	var firstEvent *time.Timer
	var firstEventC <-chan time.Time
	if s.hooks.FirstEventTimeout > 0 {
		firstEvent = time.NewTimer(s.hooks.FirstEventTimeout)
		firstEventC = firstEvent.C
		defer firstEvent.Stop()
	}
	disarm := func() {
		if firstEvent != nil {
			firstEvent.Stop()
		}
		firstEventC = nil
	}

	// The first trigger fixes the ending. A stop that began before it
	// still makes the turn a cancellation, since the turn's own stop
	// ends the process too.
	stopForTrigger := func(t turnTrigger) {
		if trigger != triggerNone {
			return
		}
		trigger = t
		stoppedBeforeTrigger = reaper.Stopped()
		if !reaped {
			cancelCmd()
		}
	}

	parseLine := func(line []byte) {
		if trigger == triggerEndTurn {
			return
		}
		result, parseErr := s.hooks.ParseLine(line, emit, pidStr, &spend)
		if current := s.hooks.GetSessionID(); current != sessionID {
			sessionID = current
			logger = s.loggerFor(current)
			s.mu.Lock()
			s.turnLogger = logger
			s.mu.Unlock()
		}
		if parseErr != nil {
			if ending, ok := errors.AsType[*endTurnError](parseErr); ok {
				disarm()
				endTurn = ending
				stopForTrigger(triggerEndTurn)
				return
			}
			EmitMalformed(emit, line)
			if firstEventC != nil {
				firstEvent.Reset(s.hooks.FirstEventTimeout)
			}
			return
		}
		disarm()
		output.Observe(line)
		if result != nil {
			lastParsed = result
		}
	}

	reaperDone := reaper.Done()
	var deadline <-chan time.Time
	var deadlineAt time.Time

loop:
	for {
		// Checked ahead of the select, not as one of its arms: a
		// descendant that keeps writing holds the line arm ready, and
		// the choice among ready arms is random, so the deadline arm
		// could be passed over for as long as that descendant talks.
		if !deadlineAt.IsZero() && !time.Now().Before(deadlineAt) {
			drainReaderBounded(reader, deadlineAt, s.drainGrace, parseLine)
			break loop
		}

		select {
		case line, ok := <-reader.Stream():
			if !ok {
				if reader.Err() != nil && !reaped {
					stopForTrigger(triggerReadFailure)
				}
				break loop
			}
			parseLine(line)

		case <-reaperDone:
			close(localWaitCh)
			s.mu.Lock()
			s.group = nil
			s.waitCh = nil
			s.mu.Unlock()
			reaped = true
			reaperDone = nil
			disarm()
			deadline = time.After(s.drainGrace)
			deadlineAt = time.Now().Add(s.drainGrace)

		case <-firstEventC:
			disarm()
			// Both arms can be ready at once and the choice between
			// them is random: the reap wins on the merits, because a
			// process already gone did not time out.
			select {
			case <-reaper.Done():
				continue
			default:
			}
			stopForTrigger(triggerFirstEventTimeout)

		case <-deadline:
			drainReaderBounded(reader, deadlineAt, s.drainGrace, parseLine)
			break loop
		}
	}

	// Standard output reached end of file, so every write end is
	// closed and the child has exited or is about to; a child that
	// closed its output and kept running is a turn that is still
	// running, so this wait carries no bound of its own.
	if !reaped {
		<-reaper.Done()
		close(localWaitCh)
		s.mu.Lock()
		s.group = nil
		s.waitCh = nil
		s.mu.Unlock()
	}

	waitErr := reaper.Err()
	scanErr := reader.Err()

	stderrLines := stderrCollector.FinishAndCollect(s.drainGrace)

	cancelled := reaper.Stopped()
	if trigger != triggerNone {
		cancelled = stoppedBeforeTrigger
	}

	endedByHook := !cancelled && trigger == triggerEndTurn
	if s.hooks.SettleUsage != nil && !endedByHook {
		s.hooks.SettleUsage(context.WithoutCancel(ctx), emit, &spend)
	}

	var pulledWindows []domain.UsageWindow
	if s.hooks.PullWindows != nil {
		windows, err := s.hooks.PullWindows(ctx, turnEnding(cancelled, endedByHook, trigger, scanErr))
		if err != nil {
			logger.Debug("usage pull failed", slog.Any("error", err))
		} else {
			pulledWindows = windows
		}
		if len(windows) > 0 {
			EmitUsageWindows(emit, time.Now().UTC(), windows)
		}
	}

	failedTurn := func(message string, agentErr *domain.AgentError) (domain.TurnResult, error) {
		usage, byModel, measured := s.hooks.GetUsage()
		EmitTurnFailed(emit, message, 0, usage, byModel)
		return unfinished(domain.TurnResult{
			SessionID:     s.hooks.GetSessionID(),
			ExitReason:    domain.EventTurnFailed,
			Usage:         usage,
			UsageMeasured: measured,
		}), agentErr
	}

	if cancelled {
		return cancelledTurn()
	}

	if endedByHook {
		procutil.EmitWarnLines(stderrLines, logger)
		return failedTurn(endTurn.message, &domain.AgentError{Kind: endTurn.kind, Message: endTurn.message})
	}

	if trigger == triggerFirstEventTimeout {
		procutil.EmitWarnLines(stderrLines, logger)
		const message = "timed out waiting for first event"
		return failedTurn(message, &domain.AgentError{Kind: domain.ErrResponseTimeout, Message: message})
	}

	// An abandoned scan is not a read failure: the exit code is real and
	// the transcript is complete except for the tail an abandoned reader
	// can lose, so it falls through to the exit-based arms below rather
	// than reporting a scanner error.
	if scanErr != nil && !errors.Is(scanErr, procutil.ErrStdoutAbandoned) {
		procutil.EmitWarnLines(stderrLines, logger)
		return failedTurn("stdout read error: "+scanErr.Error(), &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "stdout scanner error",
			Err:     scanErr,
		})
	}

	exitCode := procutil.ExtractExitCode(waitErr)

	sshFailed := s.target.RemoteCommand != "" && sshutil.ConnectionFailed(exitCode)
	if ConnectionFailedForRequest(sshFailed, lastParsed != nil) {
		return failedTurn("ssh connection failed", ConnectionFailedError())
	}

	earlyExit := output.ExitedBeforeOutput(*s.target, waitErr).Report(stderrCollector)

	if earlyExit == nil {
		if exitCode == 127 {
			procutil.EmitWarnLines(stderrLines, logger)
			return failedTurn("agent binary not found", &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "exit code 127",
			})
		}
	}

	// The explicit nil check prevents a typed-nil *domain.AgentError from
	// becoming a non-nil error interface on the success path. The
	// skeleton calls EmitWarnLines when agentErr is non-nil, so
	// OnFinalize must not call it.
	result, agentErr := s.hooks.OnFinalize(emit, lastParsed, exitCode, waitErr, stderrLines, earlyExit, &spend)
	s.fillRefusalReset(agentErr, pulledWindows)
	if agentErr != nil {
		procutil.EmitWarnLines(stderrLines, logger)
		return result, agentErr
	}
	return result, nil
}

// fillRefusalReset gives a usage_limit error that carries no reset the earliest
// exhausted window of the turn's pull, when the adapter says this turn's
// refusal reads its reset from there. A reset the error already carries is
// kept.
func (s *ForkPerTurnSession) fillRefusalReset(agentErr *domain.AgentError, windows []domain.UsageWindow) {
	if s.hooks.RefusalWindowReset == nil || agentErr == nil || agentErr.Kind != domain.ErrUsageLimit {
		return
	}
	if agentErr.UsageLimit == nil || !agentErr.UsageLimit.ResetsAt.IsZero() || !s.hooks.RefusalWindowReset() {
		return
	}
	agentErr.UsageLimit.ResetsAt = usage.RefusalReset(windows, time.Now().UTC())
}

// turnStdin returns the reader a turn's process reads from: the SSH
// preamble alone, or with the prompt after it when the hooks ask for the
// prompt on standard input.
func (s *ForkPerTurnSession) turnStdin(preamble io.Reader, prompt string) io.Reader {
	if !s.hooks.PromptOnStdin {
		return preamble
	}
	promptReader := strings.NewReader(prompt)
	if preamble == nil {
		return promptReader
	}
	return io.MultiReader(preamble, promptReader)
}

// Stop signals the active subprocess to exit gracefully and waits for it to
// be reaped and its process group cleaned up, which RunTurn's wait sequence
// reports as soon as the reap completes rather than at the end of its own
// stdout drain. If no subprocess is running, Stop returns immediately with
// nil. Every return closes [ForkPerTurnHooks.Tools] after the teardown.
//
// Shutdown sequence:
//  1. SIGTERM to process group ([procutil.Group.SignalGraceful])
//  2. Wait up to the session's configured stop grace for RunTurn to close waitCh
//  3. If the grace elapses: SIGKILL to process group
//  4. If ctx is cancelled before waitCh closes: SIGKILL and return ctx.Err()
func (s *ForkPerTurnSession) Stop(ctx context.Context) error {
	if s.hooks.Tools != nil {
		defer s.hooks.Tools.Close() //nolint:errcheck // Stop reports the runtime's own teardown, and Relay.Close logs a forced termination itself
	}

	s.mu.Lock()
	group := s.group
	waitCh := s.waitCh
	logger := s.turnLogger
	s.group = nil
	s.mu.Unlock()

	if group == nil {
		return nil
	}

	_ = group.SignalGraceful() //nolint:errcheck // best-effort signal; process may already be dead

	started := time.Now()

	select {
	case <-waitCh:
		logger.Debug("agent exited during the graceful phase", slog.String("outcome", "exited"))
		return nil
	case <-time.After(s.stopGrace):
		logger.Warn("agent did not exit inside the graceful period and was force-terminated",
			slog.String("outcome", "grace elapsed"), slog.Duration("grace", s.stopGrace),
			slog.Duration("elapsed", time.Since(started)))
		_ = group.Kill() //nolint:errcheck // best-effort kill
		return nil
	case <-ctx.Done():
		logger.Warn("agent did not exit inside the graceful period and was force-terminated",
			slog.String("outcome", "caller deadline"), slog.Duration("grace", s.stopGrace),
			slog.Duration("elapsed", time.Since(started)))
		_ = group.Kill() //nolint:errcheck // best-effort kill
		return ctx.Err()
	}
}

// drainReaderBounded takes whatever the reader has already produced and
// gives up on it at capAt, the one deadline the post-reap wait carries:
// starting a fresh grace here would let a descendant that keeps writing
// spend two of them. The deadline is checked before each line because a
// descendant that keeps writing holds the stream ready, so an arm that
// merely competes with it can be passed over indefinitely. grace is
// reported in the abandonment record and does not extend the wait.
func drainReaderBounded(reader *procutil.StdoutReader, capAt time.Time, grace time.Duration, parseLine func([]byte)) {
	for time.Now().Before(capAt) {
		select {
		case line, ok := <-reader.Stream():
			if !ok {
				return
			}
			parseLine(line)
		default:
			reader.Abandon(grace)
			return
		}
	}
	reader.Abandon(grace)
}
