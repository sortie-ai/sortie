// Package procutil provides subprocess lifecycle utilities shared by
// agent adapters that manage coding agents as local subprocesses.
package procutil

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"syscall"
)

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

// WasSignaled reports whether the process was terminated by a signal.
//
// Returns false when err is nil or is not an [*exec.ExitError].
func WasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}

// StderrCollector drains a reader line by line, logging each at DEBUG
// level while collecting lines for later retrieval. Use
// [NewStderrCollector] to start the drain goroutine and
// [StderrCollector.Lines] to retrieve collected output after the
// subprocess exits.
type StderrCollector struct {
	lines  []string
	done   chan struct{}
	logger *slog.Logger
}

// NewStderrCollector starts a goroutine that drains r line by line,
// logging each line at DEBUG level, and collecting them for later
// retrieval via [StderrCollector.Lines].
func NewStderrCollector(r io.Reader, logger *slog.Logger) *StderrCollector {
	if logger == nil {
		logger = slog.Default()
	}
	c := &StderrCollector{
		done:   make(chan struct{}),
		logger: logger,
	}
	go c.drain(r)
	return c
}

func (c *StderrCollector) drain(r io.Reader) {
	defer close(c.done)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		c.logger.Debug("agent stderr", slog.String("line", line))
		c.lines = append(c.lines, line)
	}
	if err := scanner.Err(); err != nil {
		c.logger.Debug("agent stderr drain failed", slog.Any("error", err))
	}
}

// Lines blocks until the drain goroutine finishes and returns all
// collected stderr lines. Safe to call after the subprocess has
// exited.
func (c *StderrCollector) Lines() []string {
	<-c.done
	return c.lines
}

// WarnLines blocks until the drain goroutine finishes, then re-emits
// each collected line at WARN level using logger. Intended for
// surfacing agent subprocess diagnostics (e.g., startup rejections)
// without requiring DEBUG logging.
func (c *StderrCollector) WarnLines(logger *slog.Logger) {
	for _, line := range c.Lines() {
		logger.Warn("agent stderr", slog.String("line", line))
	}
}
