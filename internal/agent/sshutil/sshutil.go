// Package sshutil provides SSH invocation utilities shared by agent
// adapters that launch coding agents on remote hosts via SSH.
package sshutil

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrConnectionFailed marks an ssh client that failed to connect, as
// opposed to a remote command that failed.
var ErrConnectionFailed = errors.New("the SSH connection to the worker host failed")

const sshConnectionFailedExitStatus = 255

// ConnectionFailed reports whether exitStatus is the one OpenSSH reserves
// for its own connection failures rather than the remote command's.
func ConnectionFailed(exitStatus int) bool {
	return exitStatus == sshConnectionFailedExitStatus
}

// shellQuote quotes s for safe inclusion in a POSIX shell command.
// Uses single-quoting with embedded single-quote escaping to prevent
// shell injection when SSH passes the remote command through the
// remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// QuoteArgv renders argv as one remote shell fragment: every element
// single-quoted, joined by one space, so the remote shell passes each
// element as one unexpanded word and never reads the first as an
// assignment or a reserved word.
func QuoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// AppendArgs returns fragment followed by args, placed where
// [BuildSSHLaunch] places its agentArgs: past the blanks and newlines
// that end the fragment, and in front of a single unescaped ; or & that
// ends it. A non-empty argument made only of A-Za-z0-9_@%+=:,./- is
// written as is; any other argument, the empty one included, is
// single-quoted. A POSIX shell reading the result passes each argument
// to the command as one unexpanded word. With no args it returns
// fragment unchanged.
func AppendArgs(fragment string, args ...string) string {
	if len(args) == 0 {
		return fragment
	}
	command, terminator := splitCommandTerminator(fragment)
	parts := make([]string, 0, len(args)+2)
	parts = append(parts, command)
	for _, arg := range args {
		parts = append(parts, quoteUnlessSafe(arg))
	}
	if terminator != "" {
		parts = append(parts, terminator)
	}
	return strings.Join(parts, " ")
}

// quoteUnlessSafe writes arg as is when it is non-empty and holds only
// characters no POSIX shell reads specially, and single-quotes it
// otherwise.
func quoteUnlessSafe(arg string) string {
	if arg == "" {
		return shellQuote(arg)
	}
	for _, r := range arg {
		if !isShellSafeRune(r) {
			return shellQuote(arg)
		}
	}
	return arg
}

func isShellSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_@%+=:,./-", r)
}

// SSHOptions configures SSH transport behavior for remote agent
// execution. Adapters populate this from orchestrator-provided
// configuration. Zero-value fields select safe defaults.
type SSHOptions struct {
	// StrictHostKeyChecking is the OpenSSH StrictHostKeyChecking
	// value. When empty, defaults to "accept-new" (TOFU).
	StrictHostKeyChecking string

	// Env lists the environment variables to carry into the remote
	// session, in the order they are exported. A carried value
	// reaches the remote shell on the SSH session's standard input,
	// never in a process argument list.
	Env []EnvVar

	// Unset lists variable names the remote shell unsets before it runs
	// the agent command.
	Unset []string

	// Forward, when non-nil, asks the launch for a reverse forward. The agent
	// command runs only after the outcome resolves; see
	// [SSHLaunch.WatchStderr].
	Forward *ReverseForward

	// StopGrace is how long the remote agent has between SIGTERM and SIGKILL
	// once its SSH connection is gone, rounded up to whole seconds.
	StopGrace time.Duration

	// ExitBarrier, when non-empty, is a name from [NewExitBarrier] that the
	// remote agent tree holds until it has exited, so [AwaitExitCommand] can
	// wait for that exit from a second connection after this one is gone.
	// Empty leaves the launch without a barrier.
	ExitBarrier string
}

func (o SSHOptions) forwardHostPort() (int, bool) {
	if o.Forward == nil {
		return 0, false
	}
	return o.Forward.HostPort, true
}

// Format implements [fmt.Formatter], rendering StrictHostKeyChecking, Env
// and the Forward host port for every verb. Env renders through
// [EnvVar.Format], so a carried value never reaches a formatted log line or
// error.
func (o SSHOptions) Format(f fmt.State, _ rune) {
	forward := ""
	if port, ok := o.forwardHostPort(); ok {
		forward = fmt.Sprintf(", Forward:{HostPort:%d}", port)
	}
	_, _ = fmt.Fprintf(f, "sshutil.SSHOptions{StrictHostKeyChecking:%q, Env:%v%s}", o.StrictHostKeyChecking, o.Env, forward) //nolint:errcheck // a fmt.Formatter has no way to report a write error
}

// LogValue implements [slog.LogValuer]. Env renders through
// [EnvVar.LogValue] and [EnvVar.MarshalJSON], so a carried value never
// reaches a structured log record.
func (o SSHOptions) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.String("strict_host_key_checking", o.StrictHostKeyChecking),
		slog.Any("env", o.Env),
	}
	if port, ok := o.forwardHostPort(); ok {
		attrs = append(attrs, slog.Int("forward_host_port", port))
	}
	return slog.GroupValue(attrs...)
}

// MarshalJSON implements [json.Marshaler]. Env marshals through
// [EnvVar.MarshalJSON], so a carried value never reaches a
// JSON-encoded log record.
func (o SSHOptions) MarshalJSON() ([]byte, error) {
	view := struct {
		StrictHostKeyChecking string   `json:"strict_host_key_checking"`
		Env                   []EnvVar `json:"env"`
		ForwardHostPort       *int     `json:"forward_host_port,omitempty"`
	}{StrictHostKeyChecking: o.StrictHostKeyChecking, Env: o.Env}
	if port, ok := o.forwardHostPort(); ok {
		view.ForwardHostPort = &port
	}
	return json.Marshal(view)
}

// ddMissingMessage is the line the remote guard writes to standard
// error, and the only line it writes, when the remote host has no dd
// on PATH.
const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"

// completionMarkerName is the shell variable the preamble assigns last
// and the import step tests, so a preamble that arrives incomplete
// fails the launch. A launch must not carry an entry of this name: the
// marker assignment would overwrite the carried value, and a preamble
// truncated just after that carried assignment would satisfy the test
// the marker exists to fail. [IsReservedEnvName] reports it.
const completionMarkerName = "_sortie_complete"

// SSHLaunch is the SSH invocation arguments and, when the launch
// carries environment variables, the standard input preamble the
// remote shell must receive ahead of the agent's own protocol bytes.
// Obtain one from [BuildSSHLaunch]; the zero value carries no
// preamble and no arguments.
type SSHLaunch struct {
	// Args is the SSH invocation argument vector, suitable for
	// exec.Command's variadic arguments after the ssh binary path.
	Args []string

	// preamble is the standard input bytes the remote shell's import
	// step reads before the agent's own protocol bytes. Unexported so
	// no caller can build Args without also carrying its matching
	// standard input; use [SSHLaunch.StdinReader] or
	// [SSHLaunch.PrefixStdin].
	preamble []byte

	// forward is non-nil on a launch built with [SSHOptions.Forward]; its
	// preamble exists only once the outcome resolves, so preamble stays empty.
	forward *forwardState
}

// buildSSHOpts builds the SSH client options and destination arguments
// shared by every launch, whether or not it carries variables.
func buildSSHOpts(host string, opts SSHOptions) []string {
	strictHostKey := opts.StrictHostKeyChecking
	if strictHostKey == "" {
		strictHostKey = "accept-new"
	}

	args := []string{
		"-o", "StrictHostKeyChecking=" + strictHostKey,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		// A shared connection outlives this client, so the remote supervisor
		// would never see it end, and its master would own a forward's
		// allocation line.
		"-o", "ControlPath=none",
	}
	if opts.Forward != nil {
		args = append(args, forwardOptions(opts.Forward.HostPort)...)
	}
	return append(args, "--", strings.TrimSpace(host))
}

// forwardOptions returns the ssh options a forwarded launch adds. A refused
// forward must not end the launch; LogLevel=INFO keeps the allocation line
// visible whatever the operator's ssh_config sets. Remote port 0 lets sshd
// allocate one, since a fixed port collides across sessions on one host.
//
// It panics when hostPort is outside 1..65535.
func forwardOptions(hostPort int) []string {
	if hostPort < 1 || hostPort > 65535 {
		panic("sshutil: BuildSSHLaunch: forward host port out of range")
	}
	return []string{
		"-o", "ExitOnForwardFailure=no",
		"-o", "LogLevel=INFO",
		"-R", "127.0.0.1:0:127.0.0.1:" + strconv.Itoa(hostPort),
	}
}

// agentGroup renders remoteCommand and its arguments as one compound
// command. && and || share a single precedence level in a POSIX
// shell, so an ungrouped fragment carrying a top-level || or ; binds
// to the launch's own && chain: its right-hand side would then run
// even though the cd or the environment import ahead of it failed. A
// newline closes the group rather than a semicolon: the fragment is
// the operator's own command and may itself end in &, which a
// semicolon after it turns into a syntax error the remote shell
// rejects before the agent runs. With positional set, "$@" follows the
// arguments so the preamble's late arguments reach the command.
func agentGroup(remoteCommand string, agentArgs []string, positional bool) string {
	command, terminator := splitCommandTerminator(remoteCommand)
	parts := make([]string, 0, len(agentArgs)+3)
	parts = append(parts, command)
	for _, arg := range agentArgs {
		parts = append(parts, shellQuote(arg))
	}
	if positional {
		parts = append(parts, `"$@"`)
	}
	if terminator != "" {
		parts = append(parts, terminator)
	}
	return "{ " + strings.Join(parts, " ") + "\n}"
}

//go:embed supervise.sh
var supervisor string

// supervisedAgent runs agent under supervisor, which ends the session's
// process group once the SSH connection is gone, granting the agent grace
// rounded up to whole seconds between SIGTERM and SIGKILL. A non-empty
// barrier names the exit barrier the agent tree holds.
func supervisedAgent(agent string, grace time.Duration, barrier string) string {
	hold := ""
	if barrier != "" {
		hold = "sortie_hold_barrier " + shellQuote(exitBarrierPath(barrier)) + "\n"
	}
	return "{\n" + supervisor +
		"sortie_agent() {\n" + agent + "\n}\n" +
		hold +
		`sortie_start_agent sortie_agent "$@"` + "\n" +
		"sortie_supervise " + strconv.Itoa(wholeSeconds(grace)) + "\n}"
}

// shellBlanks holds the trailing characters a POSIX shell either
// ignores or reads as the end of a command, so dropping them from a
// fragment leaves the operator's own command unchanged. A carriage
// return is not one of them: the shell reads it as an ordinary
// character inside a word, so dropping one would rename the command
// the operator wrote.
const shellBlanks = " \t\n"

// splitCommandTerminator splits a remote command fragment into the
// command and the single top-level ; or & that ends it, returning an
// empty terminator when the fragment ends in neither, and drops the
// blanks and newlines that trail either one. A fragment written as a
// multi-line block in the workflow arrives ending in a newline, which
// ends the command just as a terminator does, so arguments left after
// it would run as a command of their own. A terminator ends the
// command rather than belonging to it, so arguments placed after one
// run as a command of their own instead of reaching the agent, while
// arguments placed in front of it reach the agent and leave the
// operator's own terminator its meaning. A terminator the fragment
// escapes, as find -exec does with \;, is an argument of the
// operator's command, and a doubled one is part of an operator the
// fragment leaves incomplete; both stay where they are.
func splitCommandTerminator(fragment string) (command, terminator string) {
	trimmed := strings.TrimRight(fragment, shellBlanks)
	if trimmed == "" {
		return trimmed, ""
	}
	last := trimmed[len(trimmed)-1]
	if last != ';' && last != '&' {
		return trimmed, ""
	}
	head := trimmed[:len(trimmed)-1]
	if endsInEscape(head) || strings.HasSuffix(head, string(last)) {
		return trimmed, ""
	}
	return strings.TrimRight(head, shellBlanks), string(last)
}

// endsInEscape reports whether s ends in an odd number of backslashes,
// which escapes whatever character follows them.
func endsInEscape(s string) bool {
	return (len(s)-len(strings.TrimRight(s, `\`)))%2 == 1
}

// BuildSSHLaunch constructs the SSH invocation arguments for remote
// agent execution. The workspace path sets the remote cwd via cd.
// remoteCommand is treated as a pre-formed POSIX shell fragment;
// callers are responsible for any quoting within that fragment.
// agentArgs are individually shell-quoted and appended after
// remoteCommand, past any blank or newline that ends it, or in front
// of a single unescaped ; or & that ends it, so they reach the
// fragment's own command rather than forming a command of their own
// and the fragment keeps the meaning its terminator gives it. The
// fragment and its arguments run as one compound command, so a
// top-level || or ; inside the fragment cannot run when the cd or the
// environment import ahead of it failed.
//
// SSH options applied (unless overridden via opts):
//   - StrictHostKeyChecking=accept-new (TOFU), configurable via opts
//   - BatchMode=yes (no interactive prompts)
//   - ConnectTimeout=30
//   - ServerAliveInterval=15
//   - ServerAliveCountMax=3
//
// With opts.Env nil or empty, the launch carries no preamble. With a
// non-empty opts.Env, the final element of Args guards for a POSIX dd
// on the remote host, imports the preamble [SSHLaunch.StdinReader] or
// [SSHLaunch.PrefixStdin] delivers, and exports each name before
// running remoteCommand. A preamble that arrives incomplete fails the
// launch instead of running the agent without its variables: dd
// reports success when the input ends before its count, and a
// truncated preamble can be valid shell that exports nothing, so the
// import step requires the completion marker the preamble sets last.
// BuildSSHLaunch neither reorders nor deduplicates opts.Env.
//
// With a non-nil opts.Forward, the launch adds the forward's ssh options and
// always carries a preamble, whose length is reserved from
// opts.Forward.Render(WidestOutcome); [SSHLaunch.StdinReader] and
// [SSHLaunch.PrefixStdin] pad the real rendering to it.
//
// With a non-empty opts.ExitBarrier, the agent tree holds that barrier from
// its start until it has exited; see [AwaitExitCommand].
//
// With a non-empty opts.Unset, the remote shell also unsets each named
// variable after the import step, when there is one, and before the
// agent command runs, so the agent does not inherit a variable the
// remote host's login environment sets.
//
// BuildSSHLaunch panics when an opts.Env entry's Name, a late entry's Name or
// an opts.Unset name fails [IsEnvName] or is reserved, when
// opts.Forward.HostPort is outside 1..65535, and when opts.ExitBarrier is not
// a name [NewExitBarrier] returns; the message names the entry's index and
// carries neither its Name nor its Value.
func BuildSSHLaunch(host, workspacePath, remoteCommand string, agentArgs []string, opts SSHOptions) SSHLaunch {
	sshOpts := buildSSHOpts(host, opts)

	for i, name := range opts.Unset {
		switch {
		case !IsEnvName(name):
			panic(fmt.Sprintf("sshutil: BuildSSHLaunch: invalid environment variable name at Unset[%d]", i))
		case IsReservedEnvName(name):
			panic(fmt.Sprintf("sshutil: BuildSSHLaunch: reserved environment variable name at Unset[%d]", i))
		}
	}
	agent := agentGroup(remoteCommand, agentArgs, opts.Forward != nil)
	if len(opts.Unset) > 0 {
		agent = "unset " + strings.Join(opts.Unset, " ") + " && " + agent
	}
	if opts.ExitBarrier != "" && !isExitBarrierName(opts.ExitBarrier) {
		panic("sshutil: BuildSSHLaunch: invalid exit barrier name")
	}
	agent = supervisedAgent(agent, opts.StopGrace, opts.ExitBarrier)

	if opts.Forward != nil {
		return buildForwardedLaunch(sshOpts, workspacePath, agent, opts)
	}

	if len(opts.Env) == 0 {
		remoteCmd := strings.Join([]string{
			"cd", "--", shellQuote(workspacePath), "&&", agent,
		}, " ")
		return SSHLaunch{Args: append(sshOpts, remoteCmd)}
	}

	panicOnBadEnvName(opts.Env, "Env")

	preamble := "unset _sortie_env && export " + strings.Join(envAssignments(opts.Env), " ") + " && " + completionMarkerName + "=1"

	remoteCmd := strings.Join([]string{
		"cd", "--", shellQuote(workspacePath), "&&", ddGuard, "&&", importStep(len(preamble)), "&&", agent,
	}, " ")

	return SSHLaunch{
		Args:     append(sshOpts, remoteCmd),
		preamble: []byte(preamble),
	}
}

func buildForwardedLaunch(sshOpts []string, workspacePath, agent string, opts SSHOptions) SSHLaunch {
	panicOnBadEnvName(opts.Env, "Env")
	widest := opts.Forward.Render(WidestOutcome)
	panicOnBadEnvName(widest.Env, "LatePreamble.Env")

	state := newForwardState(opts.Forward)
	state.static = slices.Clone(opts.Env)
	state.reserved = len(forwardPreamble(state.static, widest))

	remoteCmd := strings.Join([]string{
		"cd", "--", shellQuote(workspacePath), "&&", ddGuard, "&&", readyStep, "&&", importStep(state.reserved), "&&", agent,
	}, " ")
	return SSHLaunch{Args: append(sshOpts, remoteCmd), forward: state}
}

// forwardPreamble always runs set -- so the agent command never inherits the
// remote shell's own positional parameters.
func forwardPreamble(static []EnvVar, late LatePreamble) string {
	var b strings.Builder
	b.WriteString("unset _sortie_env")
	if len(static)+len(late.Env) > 0 {
		b.WriteString(" && export ")
		b.WriteString(strings.Join(envAssignments(append(slices.Clone(static), late.Env...)), " "))
	}
	b.WriteString(" && set --")
	for _, arg := range late.Args {
		b.WriteString(" ")
		b.WriteString(shellQuote(arg))
	}
	b.WriteString(" && " + completionMarkerName + "=1")
	return b.String()
}

func envAssignments(entries []EnvVar) []string {
	assignments := make([]string, len(entries))
	for i, entry := range entries {
		assignments[i] = entry.Name + "=" + shellQuote(entry.Value)
	}
	return assignments
}

// badEnvName returns the index of the first entry the preamble cannot carry
// and why, or -1.
func badEnvName(entries []EnvVar) (index int, reason string) {
	for i, entry := range entries {
		switch {
		case !IsEnvName(entry.Name):
			return i, "invalid"
		case IsReservedEnvName(entry.Name):
			return i, "reserved"
		}
	}
	return -1, ""
}

func panicOnBadEnvName(entries []EnvVar, where string) {
	if i, reason := badEnvName(entries); i >= 0 {
		panic(fmt.Sprintf("sshutil: BuildSSHLaunch: %s environment variable name at %s[%d]", reason, where, i))
	}
}

const ddGuard = "{ command -v dd >/dev/null 2>&1 || { echo '" + ddMissingMessage + "' >&2; exit 1; }; }"

// readyMarker tells [SSHLaunch.WatchStderr] that ssh has said everything it
// will about the forward.
const readyMarker = "sortie: remote shell ready"

const readyStep = "printf '%s\\n' '" + readyMarker + "' >&2"

func importStep(count int) string {
	return fmt.Sprintf(`unset %[1]s && _sortie_env=$(dd bs=1 count=%[2]d 2>/dev/null) && eval "$_sortie_env" && [ "${%[1]s-}" = 1 ]`, completionMarkerName, count)
}

// Format implements [fmt.Formatter], redacting the unexported
// preamble for every verb so a carried value baked into it never
// reaches a formatted log line or error.
func (l SSHLaunch) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sshutil.SSHLaunch{Args:%q}", l.Args) //nolint:errcheck // a fmt.Formatter has no way to report a write error
}

// LogValue implements [slog.LogValuer], redacting the unexported
// preamble so a carried value baked into it never reaches a
// structured log record.
func (l SSHLaunch) LogValue() slog.Value {
	return slog.GroupValue(slog.Any("args", l.Args))
}

// MarshalJSON implements [json.Marshaler], redacting the unexported
// preamble so a carried value baked into it never reaches a
// JSON-encoded log record.
func (l SSHLaunch) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Args []string `json:"args"`
	}{Args: l.Args})
}

// preambleReader yields an [SSHLaunch]'s standard input preamble.
// Unexported so no caller can hold an exported reader type whose
// default reflection-based formatting would render the preamble
// verbatim; Format, LogValue, and MarshalJSON render none of it.
type preambleReader struct {
	fetch func(cancel <-chan struct{}) ([]byte, error)
	once  sync.Once
	r     *strings.Reader
	err   error
}

func (r *preambleReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		preamble, err := r.fetch(nil)
		if err != nil {
			r.err = err
			return
		}
		r.r = strings.NewReader(string(preamble))
	})
	if r.err != nil {
		return 0, r.err
	}
	return r.r.Read(p)
}

// Format implements [fmt.Formatter], rendering no preamble bytes for
// any verb.
func (r *preambleReader) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "sshutil.preambleReader{}") //nolint:errcheck // a fmt.Formatter has no way to report a write error
}

// LogValue implements [slog.LogValuer], rendering no preamble bytes.
func (r *preambleReader) LogValue() slog.Value {
	return slog.StringValue("sshutil.preambleReader")
}

// MarshalJSON implements [json.Marshaler], rendering no preamble
// bytes.
func (r *preambleReader) MarshalJSON() ([]byte, error) {
	return json.Marshal("sshutil.preambleReader")
}

// StdinReader returns a reader yielding the launch's standard input
// preamble, or a nil [io.Reader] interface value when the launch
// carries no preamble. Each call returns a fresh reader.
//
// On a forwarded launch the first Read blocks until the outcome resolves,
// which needs a reader of [SSHLaunch.WatchStderr], and fails with
// [ErrPreambleOverflow] when the rendering exceeds the reserved length.
func (l SSHLaunch) StdinReader() io.Reader {
	if !l.hasPreamble() {
		return nil
	}
	return &preambleReader{fetch: l.preambleBytes}
}

func (l SSHLaunch) hasPreamble() bool {
	return l.forward != nil || len(l.preamble) > 0
}

func (l SSHLaunch) preambleBytes(cancel <-chan struct{}) ([]byte, error) {
	if l.forward == nil {
		return l.preamble, nil
	}
	return l.forward.await(cancel)
}

// PrefixStdin returns w unchanged when the launch carries no
// preamble. Otherwise it returns a writer whose first Write sends the
// whole preamble to w followed by the caller's bytes, returning the
// preamble's own write error, or [io.ErrShortWrite] when w accepted
// only part of it, and none of the caller's bytes on failure; every
// later Write passes straight through to w; and Close closes w
// without waiting for a Write in progress.
//
// On a forwarded launch the first Write blocks until the outcome resolves,
// which needs a reader of [SSHLaunch.WatchStderr], and fails with
// [ErrPreambleOverflow] when the rendering exceeds the reserved length or
// with [io.ErrClosedPipe] when Close arrives first.
func (l SSHLaunch) PrefixStdin(w io.WriteCloser) io.WriteCloser {
	if !l.hasPreamble() {
		return w
	}
	return &preambleWriter{w: w, fetch: l.preambleBytes, closed: make(chan struct{})}
}

// preambleWriter prepends a preamble to the first Write it receives.
// once guards only that one-time send, so preambleWriter is safe for
// concurrent Write calls to the same extent the wrapped writer w is.
type preambleWriter struct {
	w           io.WriteCloser
	fetch       func(cancel <-chan struct{}) ([]byte, error)
	closed      chan struct{}
	closeOnce   sync.Once
	once        sync.Once
	preambleErr error
}

// Write implements [io.Writer]. A preamble only partly accepted fails
// the write with [io.ErrShortWrite] rather than starting the agent
// without the variables the launch carries, so a writer reporting a
// short count and no error cannot pass for a delivered preamble.
func (p *preambleWriter) Write(b []byte) (int, error) {
	p.once.Do(func() {
		preamble, err := p.fetch(p.closed)
		if err == nil {
			var n int
			n, err = p.w.Write(preamble)
			if err == nil && n != len(preamble) {
				err = io.ErrShortWrite
			}
		}
		p.preambleErr = err
	})
	if p.preambleErr != nil {
		return 0, p.preambleErr
	}
	return p.w.Write(b)
}

// Close implements [io.Closer]. It closes the wrapped writer directly
// rather than through any synchronization Write uses, so a Close
// racing with a Write blocked on a peer that stopped reading unblocks
// that Write rather than waiting for it.
func (p *preambleWriter) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return p.w.Close()
}

// Format implements [fmt.Formatter], rendering no preamble bytes for
// any verb.
func (p *preambleWriter) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "sshutil.preambleWriter{}") //nolint:errcheck // a fmt.Formatter has no way to report a write error
}

// LogValue implements [slog.LogValuer], rendering no preamble bytes.
func (p *preambleWriter) LogValue() slog.Value {
	return slog.StringValue("sshutil.preambleWriter")
}

// MarshalJSON implements [json.Marshaler], rendering no preamble
// bytes.
func (p *preambleWriter) MarshalJSON() ([]byte, error) {
	return json.Marshal("sshutil.preambleWriter")
}
