package procutil

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// CaptureParams configures [StartCapture] and [RunCapture].
//
// Every Write on Stdout and Stderr MUST return: [Capture.Wait] seals
// each writer once the drain has ended, and sealing waits for a write
// already in flight so the caller can read the writer afterwards
// without racing the reader goroutine. A write that never returns
// therefore holds Wait open past every bound it otherwise honours. An
// in-memory sink that discards or trims what exceeds its budget meets
// this; a pipe, socket, or file a slow consumer can block does not.
type CaptureParams struct {
	// Stdout receives standard output. Nil connects the stream to the
	// null device and starts no reader.
	Stdout io.Writer
	// Stderr receives standard error under the same nil rule. When it
	// is the same writer as Stdout, both streams share one pipe and
	// the writer receives them in write order.
	Stderr io.Writer
	// DrainGrace bounds the wait for the captured streams after the
	// reap. Non-positive resolves to [DefaultDrainGrace].
	DrainGrace time.Duration
	// Logger receives the capture's records. Nil resolves to
	// [slog.Default].
	Logger *slog.Logger
}

// CaptureResult reports how a captured launch ended.
type CaptureResult struct {
	// WaitErr is exactly what [exec.Cmd.Wait] returned for the direct
	// child.
	WaitErr error
	// OutputComplete is true when every captured stream reached end of
	// file within DrainGrace after the reap.
	OutputComplete bool
	// TerminatedLeftovers is true when the reap found a live process of
	// the launch's process group or Job Object other than the direct
	// child: on Unix a live member observed before or during the reap's
	// group drain, on Windows a running Job Object member, where a
	// console host is never counted.
	TerminatedLeftovers bool
}

// CaptureAbandonedWarning is the message of the one record Wait logs
// when a captured stream did not reach end of file within the bound.
const CaptureAbandonedWarning = "subprocess output was not fully collected before the launch returned"

// LeftoversTerminatedMessage is the message of the INFO record a launch
// site logs when a command that exited on its own left processes the
// reap terminated.
const LeftoversTerminatedMessage = "leftover processes terminated after the command exited"

// CaptureCleanupWarning is the message of the one record [StartReaper]
// logs when the reap's process-group termination itself failed, leaving
// the launch's process tree unproven.
const CaptureCleanupWarning = "subprocess group termination failed after the launch returned"

// closeWithoutWaiting is [CloseWithoutWaiting], read through a package
// variable that only a test replaces, to exercise a close that never
// completes.
var closeWithoutWaiting = CloseWithoutWaiting

// sinkWriter wraps a caller's writer so a stream's reader goroutine can
// write to it safely for as long as the caller has not sealed it. A
// write error or a short write is ignored: the reader keeps reading to
// end of file, so the subprocess never blocks on a full pipe, and the
// caller's writer, once sealed, is safe to read even when it is not
// itself safe for concurrent use.
type sinkWriter struct {
	mu     sync.Mutex
	w      io.Writer
	sealed bool
}

func (s *sinkWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sealed {
		_, _ = s.w.Write(p)
	}
	return len(p), nil
}

// seal blocks until any write already in progress returns, then marks
// the wrapper so every later chunk is discarded rather than delivered.
// The block is what makes the caller's writer safe to read once Wait
// returns, so it is not bounded: a caller's writer that never returns
// from Write breaks the contract [CaptureParams] states.
func (s *sinkWriter) seal() {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
}

// captureStream is one pipe a Capture reads from: its read end, the
// sink its reader goroutine copies into, and the channel that closes
// once that goroutine has reached end of file, a read error, or a
// close forced by Wait's abandonment.
type captureStream struct {
	file *os.File
	sink *sinkWriter
	done chan struct{}
}

func newCaptureStream(read *os.File, w io.Writer) *captureStream {
	return &captureStream{file: read, sink: &sinkWriter{w: w}, done: make(chan struct{})}
}

func (s *captureStream) drain() {
	_, _ = io.Copy(s.sink, s.file)
	_ = s.file.Close()
	close(s.done)
}

// sameWriter reports whether a and b are the same non-nil writer.
// Comparing interface values with a dynamic type that is not itself
// comparable (a slice- or map-backed writer, which no launch in this
// codebase passes) panics rather than reporting false, so that case is
// treated as "not the same writer" instead.
func sameWriter(a, b io.Writer) (same bool) {
	if a == nil || b == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// Capture is one started subprocess whose output is being captured.
type Capture struct {
	cmd    *exec.Cmd
	logger *slog.Logger

	streams    []*captureStream
	reaper     *Reaper
	drainGrace time.Duration

	// jobHandle is the Windows Job Object handle Wait drains; always
	// zero on Unix.
	jobHandle     uintptr
	startedAt     time.Time
	reapStartedAt time.Time

	waitOnce sync.Once
	result   CaptureResult
}

// StartCapture wires cmd's standard output and standard error to pipes
// procutil owns, starts cmd in its own process group (on Windows
// suspended until its Job Object assignment), closes the parent's
// write ends, starts one reader per pipe, and starts the reap.
//
// cmd.Stdout and cmd.Stderr MUST be nil on entry; StartCapture panics
// otherwise. Every error it returns is a *[StartError]. Before
// returning an error it closes every descriptor it created and resets
// cmd.Stdout and cmd.Stderr to nil; after a [StageProcessResume]
// failure the process has already been terminated and reaped, and its
// teardown logged.
func StartCapture(cmd *exec.Cmd, params CaptureParams) (*Capture, error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		panic("procutil: StartCapture requires nil cmd.Stdout and cmd.Stderr")
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}
	drainGrace := params.DrainGrace
	if drainGrace <= 0 {
		drainGrace = DefaultDrainGrace
	}

	var streams []*captureStream
	var writeEnds []*os.File

	fail := func(stage StartStage, err error) (*Capture, error) {
		for _, s := range streams {
			s.file.Close() //nolint:errcheck,gosec // best-effort cleanup after a failed launch
		}
		for _, w := range writeEnds {
			w.Close() //nolint:errcheck,gosec // best-effort cleanup after a failed launch
		}
		cmd.Stdout = nil
		cmd.Stderr = nil
		return nil, &StartError{Stage: stage, Err: err}
	}

	if sameWriter(params.Stdout, params.Stderr) {
		read, write, err := os.Pipe()
		if err != nil {
			return fail(StageStdoutPipe, err)
		}
		cmd.Stdout, cmd.Stderr = write, write
		writeEnds = append(writeEnds, write)
		streams = append(streams, newCaptureStream(read, params.Stdout))
	} else {
		if params.Stdout != nil {
			read, write, err := os.Pipe()
			if err != nil {
				return fail(StageStdoutPipe, err)
			}
			cmd.Stdout = write
			writeEnds = append(writeEnds, write)
			streams = append(streams, newCaptureStream(read, params.Stdout))
		}
		if params.Stderr != nil {
			read, write, err := os.Pipe()
			if err != nil {
				return fail(StageStderrPipe, err)
			}
			cmd.Stderr = write
			writeEnds = append(writeEnds, write)
			streams = append(streams, newCaptureStream(read, params.Stderr))
		}
	}

	_, jobHandle, startedAt, startErr := startAndAssign(cmd, logger, true)
	if startErr != nil {
		stage := StageProcessResume
		if cmd.Process == nil {
			stage = StageProcessStart
		}
		return fail(stage, startErr)
	}

	// A write end passed to exec.Cmd as an *os.File is never closed by
	// Start or Wait, so the parent's own copy must be closed here: a
	// descendant that inherits the handle and outlives the direct
	// child still holds its own copy, but once every copy but the read
	// end is gone the pipe reaches end of file when that descendant
	// exits too.
	for _, w := range writeEnds {
		w.Close() //nolint:errcheck,gosec // best-effort; only the read end matters from here
	}

	reapStartedAt := time.Now()
	c := &Capture{
		cmd:           cmd,
		logger:        logger,
		streams:       streams,
		reaper:        StartReaper(cmd, logger),
		drainGrace:    drainGrace,
		jobHandle:     jobHandle,
		startedAt:     startedAt,
		reapStartedAt: reapStartedAt,
	}
	for _, s := range c.streams {
		go s.drain()
	}
	return c, nil
}

// Wait waits for the reap, which terminates the process tree, waits on
// Windows for the Job Object to drain, waits up to DrainGrace for every
// stream, and returns the result. A second call returns the first
// call's result.
func (c *Capture) Wait() CaptureResult {
	c.waitOnce.Do(func() {
		<-c.reaper.Done()
		waitErr := c.reaper.Err()
		leftover := c.reaper.Leftover()
		waitMS := time.Since(c.reapStartedAt).Milliseconds()

		drainCaptureJob(c.jobHandle, c.cmd, c.startedAt, waitMS, c.logger)

		complete := c.awaitStreams()

		for _, s := range c.streams {
			s.sink.seal()
		}

		if !complete {
			for _, s := range c.streams {
				select {
				case <-s.done:
				default:
					closeWithoutWaiting(s.file)
				}
			}
			c.logger.Warn(CaptureAbandonedWarning,
				slog.String("command", filepath.Base(c.cmd.Path)),
				slog.Duration("drain_bound", c.drainGrace))
		}

		c.result = CaptureResult{WaitErr: waitErr, OutputComplete: complete, TerminatedLeftovers: leftover}
	})
	return c.result
}

// awaitStreams blocks until every stream has reached end of file or
// c.drainGrace has passed, and reports which happened first.
func (c *Capture) awaitStreams() bool {
	if c.drainGrace <= 0 {
		for _, s := range c.streams {
			<-s.done
		}
		return true
	}

	// One timer spans the streams, so the grace bounds the drain as a
	// whole rather than each stream in turn. Waiting here rather than in
	// a helper goroutine is what keeps an abandoned stream from leaving
	// a goroutine parked forever on a channel that never closes.
	timer := time.NewTimer(c.drainGrace)
	defer timer.Stop()
	for _, s := range c.streams {
		select {
		case <-s.done:
		case <-timer.C:
			return false
		}
	}
	return true
}

// RunCapture calls [SetGroupCancel](cmd, stopGrace), then StartCapture,
// then Wait. cmd MUST be built with [exec.CommandContext]; a command
// built without one fails at [StageProcessStart], because os/exec
// rejects a cancellation function without a context. A non-positive
// stopGrace resolves to [DefaultStopGrace] inside SetGroupCancel.
func RunCapture(cmd *exec.Cmd, stopGrace time.Duration, params CaptureParams) (CaptureResult, error) {
	SetGroupCancel(cmd, stopGrace)
	c, err := StartCapture(cmd, params)
	if err != nil {
		return CaptureResult{}, err
	}
	return c.Wait(), nil
}
