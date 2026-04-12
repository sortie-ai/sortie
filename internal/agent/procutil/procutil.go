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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
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
	}
	go c.drain(r)
	return c
}

func (c *StderrCollector) drain(r io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(r)
	initCap := 64 * 1024
	if c.scannerMax < initCap {
		initCap = c.scannerMax
	}
	scanner.Buffer(make([]byte, 0, initCap), c.scannerMax)

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

// Lines blocks until the drain goroutine finishes and returns all
// collected stderr lines in chronological order. When lines were
// discarded, a synthetic marker line is inserted between the head and
// tail sections. Safe to call after the subprocess has exited.
func (c *StderrCollector) Lines() []string {
	<-c.done

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

// Dropped blocks until the drain goroutine finishes and returns the
// number of stderr lines discarded due to the line cap or byte budget.
func (c *StderrCollector) Dropped() int {
	<-c.done
	return c.dropped
}

// WarnLines blocks until the drain goroutine finishes, then re-emits
// each collected line at WARN level using logger. Intended for
// surfacing agent subprocess diagnostics (e.g., startup rejections)
// without requiring DEBUG logging.
//
// Callers that manage an [*exec.Cmd] MUST call [StderrCollector.Lines]
// before calling [exec.Cmd.Wait]; [exec.Cmd.Wait] closes the pipe read
// end, which can prevent the drain goroutine from reading buffered data.
// Use [EmitWarnLines] with the result of [StderrCollector.Lines] to log
// pre-collected lines after [exec.Cmd.Wait] returns.
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
