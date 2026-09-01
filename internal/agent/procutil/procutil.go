// Package procutil provides subprocess lifecycle utilities shared by
// agent adapters that manage coding agents as local subprocesses.
package procutil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

const (
	// DefaultScannerMaxSize is the maximum token size for the stderr
	// bufio.Scanner, matching the stdout scanner in agent adapters.
	DefaultScannerMaxSize = 10 * 1024 * 1024

	// DefaultMaxLines is the maximum number of stderr lines retained
	// in memory. When exceeded, the collector keeps the first half
	// and last half, discarding the middle.
	DefaultMaxLines = 1000

	// DefaultMaxBytes is the total byte budget for retained stderr
	// lines across head and tail combined.
	DefaultMaxBytes = 5 * 1024 * 1024

	// DefaultDrainGrace is the time an adapter waits for a stderr drain
	// to finish before it reaps the subprocess without it, and again
	// before it gives up on the drain and reads the collector anyway.
	DefaultDrainGrace = 5 * time.Second

	// AbandonedMarker replaces the collected lines of a collector whose
	// drain was abandoned. It is exported so an adapter that classifies
	// stderr can pin, in its own tests, that the marker is not evidence.
	AbandonedMarker = "... (agent stderr not collected: the output handle stayed open) ..."

	droppedMarkerFmt = "... (%d lines discarded) ..."
)

type collectorConfig struct {
	maxLines   int
	maxBytes   int
	scannerMax int
}

// CollectorOption configures a [StderrCollector].
type CollectorOption func(*collectorConfig)

// WithMaxLines sets the maximum number of stderr lines retained in
// memory. When the line count exceeds n, the collector retains the
// first n/2 and last n-n/2 lines, discarding the middle. Zero or
// negative values are ignored (default applies).
func WithMaxLines(n int) CollectorOption {
	return func(cfg *collectorConfig) {
		if n > 0 {
			cfg.maxLines = n
		}
	}
}

// WithMaxBytes sets the total byte budget for retained stderr lines.
// The collector continues draining and logging all lines, but skips
// retaining any individual line whose storage would exceed the current
// byte budget. Zero or negative values are ignored (default applies).
func WithMaxBytes(n int) CollectorOption {
	return func(cfg *collectorConfig) {
		if n > 0 {
			cfg.maxBytes = n
		}
	}
}

// WithScannerMax sets the maximum token size for the internal
// bufio.Scanner. Zero or negative values are ignored (default applies).
func WithScannerMax(n int) CollectorOption {
	return func(cfg *collectorConfig) {
		if n > 0 {
			cfg.scannerMax = n
		}
	}
}

// ExtractExitCode returns the process exit code from an
// [*exec.ExitError], or -1 if the error is not an ExitError.
//
// Returns 0 when err is nil.
func ExtractExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// StderrCollector drains a reader line by line, logging each at DEBUG
// level while collecting lines for later retrieval. Use
// [NewStderrCollector] to start the drain goroutine and
// [StderrCollector.Lines] to retrieve collected output after the
// subprocess exits.
//
// When the number of lines exceeds the configured maximum, the
// collector retains the first half (head) and last half (tail ring
// buffer), discarding the middle. A byte budget independently caps
// total retained bytes. In both cases the collector continues draining
// the reader to avoid blocking the subprocess.
type StderrCollector struct {
	head       []string
	tail       []string
	tailPos    int
	tailFull   bool
	dropped    int
	headCap    int
	tailCap    int
	maxBytes   int
	bytesUsed  int
	scannerMax int
	done       chan struct{}
	logger     *slog.Logger

	// abandonOnce guards the single transition into the abandoned state.
	abandonOnce sync.Once

	// abandoned closes when Abandon gives up on the drain goroutine.
	// Readers select on it alongside done.
	abandoned chan struct{}
}

// NewStderrCollector starts a goroutine that drains r line by line,
// logging each line at DEBUG level, and collecting them for later
// retrieval via [StderrCollector.Lines].
//
// Options override the default scanner buffer size, line cap, and byte
// budget. With no options the collector uses [DefaultScannerMaxSize],
// [DefaultMaxLines], and [DefaultMaxBytes], which means default
// behavior includes truncation safeguards that were not present in
// earlier versions.
func NewStderrCollector(r io.Reader, logger *slog.Logger, opts ...CollectorOption) *StderrCollector {
	if logger == nil {
		logger = slog.Default()
	}

	cfg := collectorConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.maxLines == 0 {
		cfg.maxLines = DefaultMaxLines
	}
	if cfg.maxBytes == 0 {
		cfg.maxBytes = DefaultMaxBytes
	}
	if cfg.scannerMax == 0 {
		cfg.scannerMax = DefaultScannerMaxSize
	}

	headCap := cfg.maxLines / 2
	tailCap := cfg.maxLines - headCap

	c := &StderrCollector{
		tail:       make([]string, tailCap),
		headCap:    headCap,
		tailCap:    tailCap,
		maxBytes:   cfg.maxBytes,
		scannerMax: cfg.scannerMax,
		done:       make(chan struct{}),
		logger:     logger,
		abandoned:  make(chan struct{}),
	}
	go c.drain(r)
	return c
}

func (c *StderrCollector) drain(r io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, min(64*1024, c.scannerMax)), c.scannerMax)

	for scanner.Scan() {
		line := scanner.Text()
		c.logger.Debug("agent stderr", slog.String("line", line))

		if len(c.head) < c.headCap {
			if c.bytesUsed+len(line) > c.maxBytes {
				c.dropped++
				continue
			}
			c.head = append(c.head, line)
			c.bytesUsed += len(line)
			continue
		}

		reclaimable := 0
		if c.tailFull {
			reclaimable = len(c.tail[c.tailPos])
		}

		if c.bytesUsed-reclaimable+len(line) > c.maxBytes {
			c.dropped++
			continue
		}

		if c.tailFull {
			c.dropped++
		}
		c.tail[c.tailPos] = line
		c.bytesUsed = c.bytesUsed - reclaimable + len(line)
		c.tailPos = (c.tailPos + 1) % c.tailCap
		if c.tailPos == 0 {
			c.tailFull = true
		}
	}
	if err := scanner.Err(); err != nil {
		c.logger.Debug("agent stderr drain failed", slog.Any("error", err))
	}
}

// Done returns a channel that closes once the drain goroutine has
// finished reading its source to EOF (or a read error), after which all
// collected lines are available.
//
// A caller that owns the underlying [*exec.Cmd] and created the reader
// via [exec.Cmd.StderrPipe] waits for the drain before calling
// [exec.Cmd.Wait]: Wait reaps the process and then closes the pipe's
// read end, which races with a concurrent read in the drain goroutine
// and can make it return early, silently discarding stderr lines that
// were still buffered in the pipe. [StderrCollector.WaitDone] bounds
// that wait; a caller that gives up on it reaps the process anyway and
// falls back to [StderrCollector.Abandon] so a later reader does not
// block.
func (c *StderrCollector) Done() <-chan struct{} {
	return c.done
}

// WaitDone waits up to d for the drain goroutine to finish and reports
// whether it finished. It has no side effect: a false return leaves the
// collector exactly as it was.
//
// A non-positive d polls once rather than waiting unboundedly.
func (c *StderrCollector) WaitDone(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	default:
	}
	if d <= 0 {
		return false
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.done:
		return true
	case <-timer.C:
		select {
		case <-c.done:
			return true
		default:
			return false
		}
	}
}

// Abandon gives up on a drain that a bounded wait did not finish, so
// that Lines, Dropped, and WarnLines stop blocking on it. bound is the
// wait that elapsed, reported with the warning Abandon logs.
//
// Abandon is a one-shot latch: only the first call across the
// collector's lifetime has any effect, whatever the number of
// concurrent or repeated calls. It returns without blocking, and does
// nothing when the drain has already finished, so a drain that ends
// between the caller's bounded wait and its Abandon call produces no
// warning about output that was in fact collected.
func (c *StderrCollector) Abandon(bound time.Duration) {
	c.abandonOnce.Do(func() {
		select {
		case <-c.done:
			return
		default:
		}
		c.logger.Warn("agent stderr was not fully collected before the process was reaped",
			slog.Duration("drain_bound", bound))
		close(c.abandoned)
	})
}

// Lines blocks until the drain goroutine finishes or the collector is
// abandoned, whichever comes first, and returns all collected stderr
// lines in chronological order. When lines were discarded, a synthetic
// marker line is inserted between the head and tail sections. Safe to
// call after the subprocess has exited.
//
// When the collector was abandoned before the drain finished, Lines
// returns a single-element slice containing [AbandonedMarker] instead
// of blocking. A drain that finishes after abandonment still wins: a
// later call, or one already blocked when abandonment happened, reports
// the real lines rather than the marker.
func (c *StderrCollector) Lines() []string {
	select {
	case <-c.done:
	case <-c.abandoned:
	}

	select {
	case <-c.done:
	default:
		return []string{AbandonedMarker}
	}

	hasTail := c.tailFull || c.tailPos > 0
	if len(c.head) == 0 && !hasTail {
		if c.dropped > 0 {
			return []string{fmt.Sprintf(droppedMarkerFmt, c.dropped)}
		}
		return nil
	}

	result := make([]string, len(c.head), len(c.head)+1+c.tailCap)
	copy(result, c.head)

	if c.dropped > 0 {
		result = append(result, fmt.Sprintf(droppedMarkerFmt, c.dropped))
	}

	if c.tailFull {
		result = append(result, c.tail[c.tailPos:]...)
		result = append(result, c.tail[:c.tailPos]...)
	} else {
		result = append(result, c.tail[:c.tailPos]...)
	}

	return result
}

// Dropped blocks until the drain goroutine finishes or the collector is
// abandoned, whichever comes first, and returns the number of stderr
// lines discarded due to the line cap or byte budget.
//
// When the collector was abandoned before the drain finished, Dropped
// returns 0. That zero reports an unknown count rather than an empty
// one: an abandoned collector cannot know what the drain discarded.
func (c *StderrCollector) Dropped() int {
	select {
	case <-c.done:
	case <-c.abandoned:
	}

	select {
	case <-c.done:
		return c.dropped
	default:
		return 0
	}
}

// WarnLines blocks until the drain goroutine finishes or the collector
// is abandoned, then re-emits each collected line at WARN level using
// logger. Intended for surfacing agent subprocess diagnostics (e.g.,
// startup rejections) without requiring DEBUG logging.
//
// A caller that manages an [*exec.Cmd] and cannot use
// [StderrCollector.WaitDone] still has the option of draining with
// [StderrCollector.Lines] before calling [exec.Cmd.Wait]; [exec.Cmd.Wait]
// closes the pipe read end, which can prevent the drain goroutine from
// reading buffered data. A caller that can adopt the bounded wait
// should prefer it instead, and use [EmitWarnLines] with the result of
// [StderrCollector.Lines] to log pre-collected lines after
// [exec.Cmd.Wait] returns.
//
// If logger is nil, WarnLines uses [slog.Default].
func (c *StderrCollector) WarnLines(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	EmitWarnLines(c.Lines(), logger)
}

// EmitWarnLines re-emits each line in lines at WARN level with the
// "agent stderr" message. Pass pre-collected lines obtained from
// [StderrCollector.Lines] when stderr must be drained before
// [exec.Cmd.Wait] is called.
//
// If logger is nil, EmitWarnLines uses [slog.Default].
func EmitWarnLines(lines []string, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for _, line := range lines {
		logger.Warn("agent stderr", slog.String("line", line))
	}
}
