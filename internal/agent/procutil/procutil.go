// Package procutil provides subprocess lifecycle utilities shared by
// agent adapters that manage coding agents as local subprocesses.
package procutil

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"os/exec"
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
