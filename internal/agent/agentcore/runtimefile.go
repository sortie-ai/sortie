package agentcore

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/workspacekit"
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

// ErrInvalidRuntimePath marks a [RuntimePath] that fails validation. The
// error text names the field and element index, never the value.
var ErrInvalidRuntimePath = errors.New("invalid runtime path")

// RuntimePath names one file under a runtime's home on the session's host.
// The home is HomeVar's value as that host resolves it, or the user's home
// directory joined with HomeDefault when HomeVar is unset or empty.
type RuntimePath struct {
	// HomeVar is the environment variable naming the runtime's home. It must
	// satisfy [sshutil.IsEnvName].
	HomeVar string

	// HomeDefault holds the segments under the user's home that the runtime
	// uses when HomeVar is unset or empty. It may be empty.
	HomeDefault []string

	// Segments is the file's path under the runtime's home. It holds at least
	// one element.
	Segments []string
}

// ValidPathSegment reports whether s is one path segment that stays inside the
// directory it is joined to: non-empty, not ".", local in the sense of
// [filepath.IsLocal], and free of the separators of either platform. A "."
// passes IsLocal but would drop the segment from the path, redirecting a
// session identifier to its parent directory's file.
func ValidPathSegment(s string) bool {
	return s != "" && s != "." && filepath.IsLocal(s) && !strings.ContainsAny(s, `/\`)
}

func (p RuntimePath) validate() error {
	if !sshutil.IsEnvName(p.HomeVar) {
		return fmt.Errorf("%w: HomeVar is not an environment variable name", ErrInvalidRuntimePath)
	}
	if len(p.Segments) == 0 {
		return fmt.Errorf("%w: Segments is empty", ErrInvalidRuntimePath)
	}
	if err := validateSegments("HomeDefault", p.HomeDefault); err != nil {
		return err
	}
	return validateSegments("Segments", p.Segments)
}

func validateSegments(field string, segments []string) error {
	for i, segment := range segments {
		if !ValidPathSegment(segment) {
			return fmt.Errorf("%w: %s[%d] is not a valid path segment", ErrInvalidRuntimePath, field, i)
		}
	}
	return nil
}

// label names the file in errors by its segments under the home.
func (p RuntimePath) label() string {
	return strings.Join(p.Segments, "/")
}

// ReadRuntimeFile reads the file path names on the session's host and returns
// its bytes. A local session resolves the home from path.HomeVar in env, or in
// [os.Environ] when env is nil, and from the user's home directory joined with
// path.HomeDefault when that value is empty. A remote session reads through its
// own SSH launch, where the remote shell resolves the home; env is not
// consulted. An invalid path wraps [ErrInvalidRuntimePath] before any file
// opens or process starts.
//
// A missing file wraps [fs.ErrNotExist]. A file above limit bytes is an error,
// and at most limit+1 bytes are read. A remote SSH connection failure wraps
// [sshutil.ErrConnectionFailed].
//
// The bytes may be a credential: ReadRuntimeFile neither logs them nor puts
// them in an error, and the caller must do the same.
func (t LaunchTarget) ReadRuntimeFile(ctx context.Context, env []string, path RuntimePath, limit int64) ([]byte, error) {
	if err := path.validate(); err != nil {
		return nil, err
	}
	if t.RemoteCommand != "" {
		return t.readRemoteFile(ctx, path, limit)
	}
	return t.readLocalFile(ctx, env, path, limit)
}

// ScanRuntimeFile reads the file path names as [LaunchTarget.ReadRuntimeFile]
// locates it and calls fn with each line in file order, without its
// terminator; the last line needs none. The slice is valid only during the
// call. Neither a local nor a remote session holds more than one line and the
// scanner's buffer: a remote session streams the file over its SSH launch.
//
// A missing file wraps [fs.ErrNotExist], and a remote SSH connection failure
// wraps [sshutil.ErrConnectionFailed]. A file above limit bytes, a line above
// lineLimit bytes, an ended context, and an error from fn stop the scan and
// return an error; fn may already have seen the lines before the stop. A remote
// scan returns nil only after the stream ended and the SSH client exited zero,
// so a connection that dropped mid-file is an error even when fn saw lines. It
// never outlives ctx by more than the target's StopGrace.
//
// The lines may carry a credential: ScanRuntimeFile neither logs them nor puts
// them in an error, and the caller must do the same.
func (t LaunchTarget) ScanRuntimeFile(ctx context.Context, env []string, path RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error {
	if err := path.validate(); err != nil {
		return err
	}
	if t.RemoteCommand != "" {
		return t.scanRemote(ctx, path, limit, lineLimit, fn)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := t.openLocalFile(env, path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime file: %w", err)
	}
	if info.Size() > limit {
		return fmt.Errorf("runtime file %q exceeds %d bytes", path.label(), limit)
	}
	return scanLines(ctx, f, path.label(), limit, lineLimit, fn)
}

// HostCommand describes one command [LaunchTarget.RunHostCommand] runs. The
// zero options run Name and Args as given, in Sortie's own environment and
// working directory, with at most 64 KiB of output.
type HostCommand struct {
	// Name is the program. It is empty exactly when LaunchWords is positive.
	Name string

	// Args are the arguments, one each.
	Args []string

	// LaunchWords, when positive, makes the command the configured agent
	// command cut to its first LaunchWords words, followed by Args. A launcher,
	// an env assignment, or a path before the runtime word is kept, so the
	// command runs the binary and environment the session runs.
	LaunchWords int

	// InSession runs a local command in the verified workspace with the launch
	// environment. A remote command always runs in the workspace.
	InSession bool

	// Limit is the largest standard output accepted, in bytes. Zero means 64 KiB.
	Limit int64

	// Settings are the per-launch SSH settings the session's own launch carried.
	// A remote run carries them in its environment preamble; a local run ignores
	// them.
	Settings []sshutil.EnvVar
}

// label names the command in errors without echoing its arguments.
func (c HostCommand) label() string {
	return cmp.Or(c.Name, "agent command")
}

// RunHostCommand runs cmd on the session's host and returns its standard
// output. A local session starts the command directly; a remote session runs
// the same words, quoted, through its own SSH launch, so Name and Args reach
// the remote shell unexpanded. With LaunchWords the remote shell instead
// expands the configured command's own text, as it did for the launch. The
// command never sees standard input and its standard error is discarded.
//
// env is the environment of an InSession local command, [os.Environ] when
// nil; a remote session does not consult it. A command that does not start or
// does not exit zero wraps [ErrHostCommandFailed], whatever its status; a
// remote SSH connection failure wraps [sshutil.ErrConnectionFailed] instead.
// Output above cmd.Limit is an error, and at most Limit+1 bytes are held.
//
// The output may be a credential: RunHostCommand neither logs it nor puts it
// in an error, and the caller must do the same.
func (t LaunchTarget) RunHostCommand(ctx context.Context, env []string, cmd HostCommand) ([]byte, error) {
	limit := cmd.Limit
	if limit <= 0 {
		limit = hostCommandLimit
	}
	stdout, result, err := t.captureHost(ctx, env, cmd, limit+1)
	if err != nil {
		return nil, err
	}
	if result.WaitErr != nil {
		if t.RemoteCommand != "" && sshutil.ConnectionFailed(procutil.ExtractExitCode(result.WaitErr)) {
			return nil, fmt.Errorf("remote command: %w", sshutil.ErrConnectionFailed)
		}
		return nil, fmt.Errorf("host command %q: %w", cmd.label(), ErrHostCommandFailed)
	}
	if int64(stdout.Len()) > limit {
		return nil, fmt.Errorf("host command %q printed more than %d bytes", cmd.label(), limit)
	}
	return stdout.Bytes(), nil
}

// cappedWriter keeps the first max bytes written to it and discards the rest
// while accepting every write, so a chatty command is neither stopped by a
// broken pipe nor held whole in memory.
type cappedWriter struct {
	buf bytes.Buffer
	max int64
}

func (b *cappedWriter) Write(p []byte) (int, error) {
	if room := b.max - int64(b.buf.Len()); room > 0 {
		b.buf.Write(p[:min(int64(len(p)), room)])
	}
	return len(p), nil
}

func (b *cappedWriter) Len() int { return b.buf.Len() }

func (b *cappedWriter) Bytes() []byte { return b.buf.Bytes() }

func (t LaunchTarget) captureHost(ctx context.Context, env []string, cmd HostCommand, maxBytes int64) (*cappedWriter, procutil.CaptureResult, error) {
	if err := t.checkLaunchWords(cmd); err != nil {
		return nil, procutil.CaptureResult{}, err
	}
	switch {
	case t.RemoteCommand == "":
		return t.captureLocal(ctx, env, cmd, maxBytes)
	case cmd.LaunchWords > 0:
		return t.captureRemote(ctx, remotePrefix(t.RemoteCommand, cmd.LaunchWords), cmd.Args, maxBytes, cmd.Settings)
	}
	return t.captureRemote(ctx, sshutil.QuoteArgv(append([]string{cmd.Name}, cmd.Args...)), nil, maxBytes, cmd.Settings)
}

// checkLaunchWords refuses a command that names its program twice or not at
// all, or that cuts more words than the configured command holds.
func (t LaunchTarget) checkLaunchWords(cmd HostCommand) error {
	switch {
	case cmd.LaunchWords < 0, cmd.LaunchWords > 0 && cmd.Name != "", cmd.LaunchWords == 0 && cmd.Name == "":
		return fmt.Errorf("host command %q: %w: Name and LaunchWords must name the program exactly once", cmd.label(), ErrHostCommandFailed)
	case cmd.LaunchWords > len(t.CommandWords()):
		return fmt.Errorf("host command %q: %w: LaunchWords %d is above the configured command's word count", cmd.label(), ErrHostCommandFailed, cmd.LaunchWords)
	}
	return nil
}

func (t LaunchTarget) captureLocal(ctx context.Context, env []string, hc HostCommand, maxBytes int64) (*cappedWriter, procutil.CaptureResult, error) {
	program, args := hc.Name, hc.Args
	if hc.LaunchWords > 0 {
		program = t.Command
		args = append(slices.Clone(t.Args[:hc.LaunchWords-1]), hc.Args...)
	}
	cmd := exec.CommandContext(ctx, program, args...) //nolint:gosec // G204: the program and arguments are the adapter's fixed read-only lookup
	if hc.InSession {
		cmd.Env = env
		if agentErr := t.BindWorkspace(cmd); agentErr != nil {
			return nil, procutil.CaptureResult{}, fmt.Errorf("host command %q: %w", hc.label(), agentErr)
		}
	}
	stdout := &cappedWriter{max: maxBytes}
	result, startErr := procutil.RunCapture(ctx, cmd, t.StopGrace, procutil.CaptureParams{Stdout: stdout})
	switch {
	case ctx.Err() != nil:
		return nil, result, fmt.Errorf("host command %q: %w", hc.label(), ctx.Err())
	case startErr != nil:
		return nil, result, fmt.Errorf("start host command %q: %w: %w", hc.label(), ErrHostCommandFailed, startErr)
	}
	return stdout, result, nil
}

// remotePrefix returns the text of command through the end of its n-th
// whitespace-separated word, as written. The caller guarantees command holds
// at least n words.
func remotePrefix(command string, n int) string {
	inWord := false
	for i, r := range command {
		if !unicode.IsSpace(r) {
			inWord = true
			continue
		}
		if !inWord {
			continue
		}
		if n--; n == 0 {
			return command[:i]
		}
		inWord = false
	}
	return command
}

func (t LaunchTarget) readLocalFile(ctx context.Context, env []string, path RuntimePath, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := t.openLocalFile(env, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	return readWithin(f, limit, path.label())
}

// openLocalFile opens the file through a root at the home, so a symlink under
// the home cannot lead the read outside it.
func (t LaunchTarget) openLocalFile(env []string, path RuntimePath) (*os.File, error) {
	home, _ := t.RuntimeEnv(env, path.HomeVar)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		home = filepath.Join(append([]string{userHome}, path.HomeDefault...)...)
	}
	// A home kept as a symlink (a dotfiles checkout) is the operator's
	// choice; containment applies below it, not to the home itself.
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, fmt.Errorf("open runtime home: %w", err)
	}
	root, err := workspacekit.OpenDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("open runtime home: %w", err)
	}
	defer func() { _ = root.Close() }()

	f, err := root.Open(filepath.Join(path.Segments...))
	if err != nil {
		return nil, fmt.Errorf("open runtime file: %w", err)
	}
	return f, nil
}

func (t LaunchTarget) readRemoteFile(ctx context.Context, path RuntimePath, limit int64) ([]byte, error) {
	stdout, result, err := t.captureRemote(ctx, remoteReadCommand(path, limit), nil, limit+1, nil)
	if err != nil {
		return nil, err
	}
	if err := remoteReadStatus(result.WaitErr, path.label()); err != nil {
		return nil, err
	}
	if int64(stdout.Len()) > limit {
		return nil, fmt.Errorf("runtime file %q exceeds %d bytes", path.label(), limit)
	}
	return stdout.Bytes(), nil
}

// errScanEnded is what a write to a scanRemote pipe reports once the scan has
// returned.
var errScanEnded = errors.New("scan ended")

// pipeSink hands the capture a pipe whose reader is scanRemote's scan. A Write
// waits only for that reader, which scanRemote closes on every return.
type pipeSink struct{ w *io.PipeWriter }

func (s *pipeSink) Write(p []byte) (int, error) { return s.w.Write(p) }

// scanRemote streams the remote file through scanLines, so the file is never
// held whole. An early stop ends the SSH client within StopGrace; otherwise a
// nil return needs end of stream and a zero exit status, which tells a complete
// file from one cut by a dropped connection.
func (t LaunchTarget) scanRemote(ctx context.Context, path RuntimePath, limit, lineLimit int64, fn func(line []byte) error) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := t
	runner.RemoteCommand = remoteReadCommand(path, limit)
	cmd, agentErr := runner.AuxiliaryCommand(runCtx, nil, nil, nil)
	if agentErr != nil {
		return agentErr
	}
	procutil.SetGroupCancel(cmd, t.StopGrace)

	pr, pw := io.Pipe()
	// Every return below leaves the pipe's read end closed, so a Write
	// blocked on it returns at once.
	defer func() { _ = pr.CloseWithError(errScanEnded) }()
	capture, startErr := procutil.StartCapture(runCtx, cmd, procutil.CaptureParams{Stdout: &pipeSink{w: pw}})
	if startErr != nil {
		return fmt.Errorf("start remote command: %w", startErr)
	}

	finished := make(chan procutil.CaptureResult, 1)
	go func() {
		result := capture.Wait()
		_ = pw.Close()
		finished <- result
	}()

	scanErr := scanLines(ctx, pr, path.label(), limit, lineLimit, fn)
	if scanErr != nil {
		// Closing the read end fails the capture's pending write, which
		// would otherwise hold the drain open until the client is gone.
		_ = pr.CloseWithError(scanErr)
		cancel()
	}
	result := <-finished
	switch {
	case scanErr != nil:
		return scanErr
	case ctx.Err() != nil:
		return fmt.Errorf("remote command: %w", ctx.Err())
	case result.WaitErr == nil && !result.OutputComplete:
		return errors.New("remote read: output was not fully collected")
	}
	return remoteReadStatus(result.WaitErr, path.label())
}

// captureRemote runs command, a remote shell fragment, followed by args, each
// quoted, through t's own SSH launch and returns what it printed, at most
// maxBytes of it. carried joins the launch's environment preamble. The error
// covers a launch that did not start or did not finish; the command's own exit
// status is in the result.
func (t LaunchTarget) captureRemote(ctx context.Context, command string, args []string, maxBytes int64, carried []sshutil.EnvVar) (*cappedWriter, procutil.CaptureResult, error) {
	runner := t
	runner.RemoteCommand = command
	cmd, agentErr := runner.AuxiliaryCommand(ctx, args, nil, nil, carried...)
	if agentErr != nil {
		return nil, procutil.CaptureResult{}, agentErr
	}

	stdout := &cappedWriter{max: maxBytes}
	result, startErr := procutil.RunCapture(ctx, cmd, t.StopGrace, procutil.CaptureParams{Stdout: stdout})
	switch {
	case startErr != nil:
		return nil, result, fmt.Errorf("start remote command: %w", startErr)
	case ctx.Err() != nil:
		return nil, result, fmt.Errorf("remote command: %w", ctx.Err())
	}
	return stdout, result, nil
}

// remoteReadCommand renders the one read-only command a remote read runs. The
// remote shell expands only the home variable and $HOME; every HomeDefault and
// Segments element is single-quoted so it reaches the path as written. The
// remote end stops after limit+1 bytes, so a file above limit is seen without
// being streamed whole.
func remoteReadCommand(path RuntimePath, limit int64) string {
	return fmt.Sprintf(`f="${%s:-}"; [ -n "$f" ] || f="$HOME"%s; f="$f"%s; [ -e "$f" ] || exit %d; head -c %d -- "$f"`,
		path.HomeVar, quotedSuffix(path.HomeDefault), quotedSuffix(path.Segments), remoteFileMissingExit, limit+1)
}

// quotedSuffix renders segments as "/'a'/'b'", each quoted for the remote shell.
func quotedSuffix(segments []string) string {
	var suffix strings.Builder
	for _, segment := range segments {
		suffix.WriteString("/" + sshutil.QuoteArgv([]string{segment}))
	}
	return suffix.String()
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
