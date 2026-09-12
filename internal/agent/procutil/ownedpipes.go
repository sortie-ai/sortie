package procutil

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// OwnedPipes holds the parent's read ends of a subprocess's standard
// output and standard error. The adapter owns their lifetime: exec.Cmd
// never closes them, so reaping the subprocess cannot cut a reader
// short and the reap may run while both readers are still consuming.
type OwnedPipes struct {
	Stdout *os.File
	Stderr *os.File

	closeStdoutOnce sync.Once
	closeStderrOnce sync.Once
}

// StartStage names the stage of StartWithOwnedPipes an error came from.
type StartStage int

const (
	StageStdoutPipe StartStage = iota + 1
	StageStderrPipe
	StageProcessStart
)

// StartError reports the stage StartWithOwnedPipes failed at. Unwrap
// returns the operating-system error alone, so a caller reproduces the
// per-stage diagnostic it reported before by switching on Stage and
// passing the unwrapped cause through unchanged.
type StartError struct {
	Stage StartStage
	Err   error
}

func (e *StartError) Error() string {
	return fmt.Sprintf("procutil: start with owned pipes failed at stage %d: %v", e.Stage, e.Err)
}

// Unwrap returns the operating-system error that caused the failure.
func (e *StartError) Unwrap() error {
	return e.Err
}

// StartWithOwnedPipes wires cmd's standard output and standard error to
// pipes the caller owns, starts cmd, and closes the parent's copies of
// the two write ends. cmd.Stdout and cmd.Stderr MUST be nil on entry.
// It closes every descriptor it created before returning an error, and
// every error it returns is a *StartError. A caller whose subprocess
// state is guarded by a mutex MUST hold that mutex across this call,
// because the call starts the process.
func StartWithOwnedPipes(cmd *exec.Cmd) (*OwnedPipes, error) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, &StartError{Stage: StageStdoutPipe, Err: err}
	}

	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		closeFiles(stdoutRead, stdoutWrite)
		return nil, &StartError{Stage: StageStderrPipe, Err: err}
	}

	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite

	if startErr := cmd.Start(); startErr != nil {
		closeFiles(stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		cmd.Stdout = nil
		cmd.Stderr = nil
		return nil, &StartError{Stage: StageProcessStart, Err: startErr}
	}

	// A write end passed to exec.Cmd as an *os.File is never closed by
	// Start or Wait, so the parent's own copy must be closed here: a
	// descendant that inherits the handle and outlives the direct child
	// still holds its own copy, but once every copy but the read end is
	// gone the pipe reaches end of file when that descendant exits too.
	stdoutWrite.Close() //nolint:errcheck,gosec // best-effort; only the read end matters from here
	stderrWrite.Close() //nolint:errcheck,gosec // best-effort; only the read end matters from here

	return &OwnedPipes{Stdout: stdoutRead, Stderr: stderrRead}, nil
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		f.Close() //nolint:errcheck,gosec // best-effort cleanup after a failed launch
	}
}

// Close closes both read ends. It is safe to call more than once. The
// caller defers it at launch rather than placing it in the wait
// sequence: exec.Cmd closes neither end, and closing either one ahead
// of its reader's last read truncates that reader with no diagnostic.
func (p *OwnedPipes) Close() error {
	stdoutErr := p.CloseStdout()
	stderrErr := p.closeStderr()
	if stdoutErr != nil {
		return stdoutErr
	}
	return stderrErr
}

// CloseStderr closes the standard-error read end alone, ending a
// collector's scanner that an escaped descendant holding the write end
// would otherwise keep blocked in a read for the process's lifetime.
// Abandoning a drain releases the caller waiting for it; only closing
// the read end releases the drain itself. Safe to call more than once,
// and a later Close still closes the standard-output end.
func (p *OwnedPipes) CloseStderr() error { return p.closeStderr() }

// CloseStdout closes the standard-output read end alone, releasing a
// reader parked on it while the standard-error collector keeps
// draining. It is safe to call more than once, and a later Close still
// closes the standard-error end.
func (p *OwnedPipes) CloseStdout() error {
	var err error
	p.closeStdoutOnce.Do(func() {
		err = p.Stdout.Close()
	})
	return err
}

func (p *OwnedPipes) closeStderr() error {
	var err error
	p.closeStderrOnce.Do(func() {
		err = p.Stderr.Close()
	})
	return err
}
