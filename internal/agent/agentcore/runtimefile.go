package agentcore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
)

// remoteFileMissingExit is the exit status the remote read command
// reserves for a missing file. The ssh client reserves 255 for itself
// and cat exits 1 on a failed read, so neither can be mistaken for it.
const remoteFileMissingExit = 66

// hostCommandLimit caps what [LaunchTarget.RunHostCommand] accepts on
// standard output.
const hostCommandLimit = 64 * 1024

// ErrHostCommandFailed marks a host command that did not start or did
// not exit zero.
var ErrHostCommandFailed = errors.New("host command failed")

// ReadRuntimeFile reads rel under the runtime's home on the session's
// host and returns its bytes. A local session reads under homeVar's
// value in env, or in [os.Environ] when env is nil, and under the
// user's home directory when that value is empty. A remote session
// reads through its own SSH launch, where the remote shell resolves
// ${homeVar:-$HOME}; homeVar must then be a valid environment variable
// name, and env is not consulted.
//
// A missing file wraps [fs.ErrNotExist]. A file above limit bytes is
// an error, and at most limit+1 bytes are read. A remote SSH
// connection failure wraps [sshutil.ErrConnectionFailed].
//
// The bytes may be a credential: ReadRuntimeFile neither logs them nor
// puts them in an error, and the caller must do the same.
func (t LaunchTarget) ReadRuntimeFile(ctx context.Context, env []string, homeVar, rel string, limit int64) ([]byte, error) {
	if t.RemoteCommand != "" {
		return t.readRemoteFile(ctx, homeVar, rel, limit)
	}
	return t.readLocalFile(ctx, env, homeVar, rel, limit)
}

// ScanRuntimeFile reads rel as [LaunchTarget.ReadRuntimeFile] locates it and
// calls fn with each line in file order, without its terminator; the last
// line needs none. The slice is valid only during the call. A local session
// holds one line at a time, never the file; a remote session scans the bytes
// its SSH read returns.
//
// A missing file wraps [fs.ErrNotExist], and a remote SSH connection failure
// wraps [sshutil.ErrConnectionFailed]. A file above limit bytes, a line above
// lineLimit bytes, an ended context, and an error from fn stop the scan and
// return an error; fn may already have seen the lines before the stop.
//
// The lines may carry a credential: ScanRuntimeFile neither logs them nor puts
// them in an error, and the caller must do the same.
func (t LaunchTarget) ScanRuntimeFile(ctx context.Context, env []string, homeVar, rel string, limit, lineLimit int64, fn func(line []byte) error) error {
	if t.RemoteCommand != "" {
		data, err := t.readRemoteFile(ctx, homeVar, rel, limit)
		if err != nil {
			return err
		}
		return scanLines(ctx, bytes.NewReader(data), rel, limit, lineLimit, fn)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := t.openLocalFile(env, homeVar, rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime file: %w", err)
	}
	if info.Size() > limit {
		return fmt.Errorf("runtime file %q exceeds %d bytes", rel, limit)
	}
	return scanLines(ctx, f, rel, limit, lineLimit, fn)
}

// RunHostCommand runs one command on the session's host and returns its
// standard output, at most 64 KiB. A local session starts name directly;
// a remote session runs the same words, quoted, through its own SSH
// launch, so name and args reach the remote shell unexpanded. The
// command never sees standard input.
//
// A command that does not start or does not exit zero wraps
// [ErrHostCommandFailed], whatever its status; a remote SSH connection
// failure wraps [sshutil.ErrConnectionFailed] instead.
//
// The output may be a credential: RunHostCommand neither logs it nor
// puts it in an error, and the caller must do the same.
func (t LaunchTarget) RunHostCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	stdout, result, err := t.captureHost(ctx, name, args)
	if err != nil {
		return nil, err
	}
	if result.WaitErr != nil {
		if t.RemoteCommand != "" && sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)) {
			return nil, fmt.Errorf("remote command: %w", sshutil.ErrConnectionFailed)
		}
		return nil, fmt.Errorf("host command %q: %w", name, ErrHostCommandFailed)
	}
	if stdout.Len() > hostCommandLimit {
		return nil, fmt.Errorf("host command %q printed more than %d bytes", name, hostCommandLimit)
	}
	return stdout.Bytes(), nil
}

func (t LaunchTarget) captureHost(ctx context.Context, name string, args []string) (*bytes.Buffer, procutil.CaptureResult, error) {
	if t.RemoteCommand != "" {
		return t.captureRemote(ctx, sshutil.QuoteArgv(append([]string{name}, args...)))
	}
	return t.captureLocal(ctx, name, args)
}

func (t LaunchTarget) captureLocal(ctx context.Context, name string, args []string) (*bytes.Buffer, procutil.CaptureResult, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: name and args are the adapter's fixed read-only lookup
	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(ctx, cmd, t.StopGrace, procutil.CaptureParams{Stdout: &stdout})
	switch {
	case ctx.Err() != nil:
		return nil, result, fmt.Errorf("host command %q: %w", name, ctx.Err())
	case startErr != nil:
		return nil, result, fmt.Errorf("start host command %q: %w: %w", name, ErrHostCommandFailed, startErr)
	}
	return &stdout, result, nil
}

func (t LaunchTarget) readLocalFile(ctx context.Context, env []string, homeVar, rel string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := t.openLocalFile(env, homeVar, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return readWithin(f, limit, rel)
}

func (t LaunchTarget) openLocalFile(env []string, homeVar, rel string) (*os.File, error) {
	home, _ := t.RuntimeEnv(env, homeVar)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		home = userHome
	}
	f, err := os.Open(filepath.Join(home, filepath.FromSlash(rel))) //nolint:gosec // G304: rel is the adapter's fixed runtime file and home is the runtime's own home
	if err != nil {
		return nil, fmt.Errorf("open runtime file: %w", err)
	}
	return f, nil
}

func (t LaunchTarget) readRemoteFile(ctx context.Context, homeVar, rel string, limit int64) ([]byte, error) {
	if !sshutil.IsEnvName(homeVar) {
		return nil, fmt.Errorf("home variable %q is not an environment variable name", homeVar)
	}
	stdout, result, err := t.captureRemote(ctx, remoteReadCommand(homeVar, rel, limit))
	if err != nil {
		return nil, err
	}
	if err := remoteReadStatus(result.WaitErr, rel); err != nil {
		return nil, err
	}
	if int64(stdout.Len()) > limit {
		return nil, fmt.Errorf("runtime file %q exceeds %d bytes", rel, limit)
	}
	return stdout.Bytes(), nil
}

// captureRemote runs command, a remote shell fragment, through t's own SSH
// launch and returns what it printed. The error covers a launch that did
// not start or did not finish; the command's own exit status is in the
// result.
func (t LaunchTarget) captureRemote(ctx context.Context, command string) (*bytes.Buffer, procutil.CaptureResult, error) {
	runner := t
	runner.RemoteCommand = command
	cmd, agentErr := runner.AuxiliaryCommand(ctx, nil, nil, nil)
	if agentErr != nil {
		return nil, procutil.CaptureResult{}, agentErr
	}

	var stdout bytes.Buffer
	result, startErr := procutil.RunCapture(ctx, cmd, t.StopGrace, procutil.CaptureParams{Stdout: &stdout})
	switch {
	case startErr != nil:
		return nil, result, fmt.Errorf("start remote command: %w", startErr)
	case ctx.Err() != nil:
		return nil, result, fmt.Errorf("remote command: %w", ctx.Err())
	}
	return &stdout, result, nil
}

// remoteReadCommand renders the one read-only command a remote read
// runs. The remote shell expands only the home variable; rel is quoted
// so it reaches the path as written. The remote end stops after limit+1
// bytes, so a file above limit is seen without being streamed whole.
func remoteReadCommand(homeVar, rel string, limit int64) string {
	path := `"${` + homeVar + `:-$HOME}"/` + sshutil.QuoteArgv([]string{rel})
	return fmt.Sprintf(`f=%s; [ -e "$f" ] || exit %d; head -c %d -- "$f"`, path, remoteFileMissingExit, limit+1)
}

func remoteReadStatus(waitErr error, rel string) error {
	if waitErr == nil {
		return nil
	}
	switch code := procutil.ExtractExitCode(waitErr); {
	case code == remoteFileMissingExit:
		return fmt.Errorf("remote runtime file %q: %w", rel, fs.ErrNotExist)
	case sshutil.ConnectionFailed(code):
		return fmt.Errorf("remote read: %w", sshutil.ErrConnectionFailed)
	default:
		return fmt.Errorf("remote read: %w", waitErr)
	}
}

func readWithin(r io.Reader, limit int64, rel string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read runtime file: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("runtime file %q exceeds %d bytes", rel, limit)
	}
	return data, nil
}

// countingReader counts the bytes read through it, which lets a scan notice a
// file that grew past its cap after the size check.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// scanLines calls fn with each line of r. It reads at most limit+1 bytes, so
// a file that outgrew the cap is seen without being streamed whole.
func scanLines(ctx context.Context, r io.Reader, rel string, limit, lineLimit int64, fn func(line []byte) error) error {
	counted := &countingReader{r: io.LimitReader(r, limit+1)}
	scanner := bufio.NewScanner(counted)
	// The extra two bytes let a line of exactly lineLimit bytes be seen with
	// its "\r\n" terminator; the explicit check below enforces the cap.
	scanner.Buffer(make([]byte, 0, 64*1024), int(lineLimit)+2)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if counted.n > limit {
			return fmt.Errorf("runtime file %q exceeds %d bytes", rel, limit)
		}
		line := scanner.Bytes()
		if int64(len(line)) > lineLimit {
			return fmt.Errorf("runtime file %q has a line above %d bytes", rel, lineLimit)
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("runtime file %q has a line above %d bytes", rel, lineLimit)
		}
		return fmt.Errorf("scan runtime file: %w", err)
	}
	if counted.n > limit {
		return fmt.Errorf("runtime file %q exceeds %d bytes", rel, limit)
	}
	return nil
}
