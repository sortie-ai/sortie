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

// DrainStderr reads from r line by line and logs each line at debug
// level using logger.
//
// Returns when r reaches EOF or encounters an error. Intended to run
// as a goroutine draining a subprocess stderr pipe.
func DrainStderr(r io.Reader, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logger.Debug("agent stderr", slog.String("line", scanner.Text()))
	}

	if err := scanner.Err(); err != nil {
		logger.Debug("agent stderr drain failed", slog.Any("error", err))
	}
}
