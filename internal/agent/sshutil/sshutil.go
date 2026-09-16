// Package sshutil provides SSH invocation utilities shared by agent
// adapters that launch coding agents on remote hosts via SSH.
package sshutil

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// shellQuote quotes s for safe inclusion in a POSIX shell command.
// Uses single-quoting with embedded single-quote escaping to prevent
// shell injection when SSH passes the remote command through the
// remote shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
}

// Format implements [fmt.Formatter], rendering StrictHostKeyChecking
// and Env for every verb. Env renders through [EnvVar.Format], so a
// carried value never reaches a formatted log line or error.
func (o SSHOptions) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sshutil.SSHOptions{StrictHostKeyChecking:%q, Env:%v}", o.StrictHostKeyChecking, o.Env) //nolint:errcheck // best-effort formatting
}

// LogValue implements [slog.LogValuer]. Env renders through
// [EnvVar.LogValue] and [EnvVar.MarshalJSON], so a carried value never
// reaches a structured log record.
func (o SSHOptions) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("strict_host_key_checking", o.StrictHostKeyChecking),
		slog.Any("env", o.Env),
	)
}

// MarshalJSON implements [json.Marshaler]. Env marshals through
// [EnvVar.MarshalJSON], so a carried value never reaches a
// JSON-encoded log record.
func (o SSHOptions) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		StrictHostKeyChecking string   `json:"strict_host_key_checking"`
		Env                   []EnvVar `json:"env"`
	}{StrictHostKeyChecking: o.StrictHostKeyChecking, Env: o.Env})
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
}

// buildSSHOpts builds the SSH client options and destination arguments
// shared by every launch, whether or not it carries variables.
func buildSSHOpts(host string, opts SSHOptions) []string {
	strictHostKey := opts.StrictHostKeyChecking
	if strictHostKey == "" {
		strictHostKey = "accept-new"
	}

	return []string{
		"-o", "StrictHostKeyChecking=" + strictHostKey,
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=30",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"--",
		strings.TrimSpace(host),
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
// rejects before the agent runs.
func agentGroup(remoteCommand string, agentArgs []string) string {
	command, terminator := splitCommandTerminator(remoteCommand)
	parts := make([]string, 0, len(agentArgs)+2)
	parts = append(parts, command)
	for _, arg := range agentArgs {
		parts = append(parts, shellQuote(arg))
	}
	if terminator != "" {
		parts = append(parts, terminator)
	}
	return "{ " + strings.Join(parts, " ") + "\n}"
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
// BuildSSHLaunch panics when an opts.Env entry's Name fails
// [IsEnvName] or is one [IsReservedEnvName] reports; the panic message
// names the entry's index and carries neither its Name nor its Value.
func BuildSSHLaunch(host, workspacePath, remoteCommand string, agentArgs []string, opts SSHOptions) SSHLaunch {
	sshOpts := buildSSHOpts(host, opts)

	if len(opts.Env) == 0 {
		remoteCmd := strings.Join([]string{
			"cd", "--", shellQuote(workspacePath), "&&", agentGroup(remoteCommand, agentArgs),
		}, " ")
		return SSHLaunch{Args: append(sshOpts, remoteCmd)}
	}

	for i, entry := range opts.Env {
		switch {
		case !IsEnvName(entry.Name):
			panic(fmt.Sprintf("sshutil: BuildSSHLaunch: invalid environment variable name at Env[%d]", i))
		case IsReservedEnvName(entry.Name):
			panic(fmt.Sprintf("sshutil: BuildSSHLaunch: reserved environment variable name at Env[%d]", i))
		}
	}

	assignments := make([]string, len(opts.Env))
	for i, entry := range opts.Env {
		assignments[i] = entry.Name + "=" + shellQuote(entry.Value)
	}
	preamble := "unset _sortie_env && export " + strings.Join(assignments, " ") + " && " + completionMarkerName + "=1"

	guard := "{ command -v dd >/dev/null 2>&1 || { echo '" + ddMissingMessage + "' >&2; exit 1; }; }"
	importStep := fmt.Sprintf(`unset %[1]s && _sortie_env=$(dd bs=1 count=%[2]d 2>/dev/null) && eval "$_sortie_env" && [ "${%[1]s-}" = 1 ]`, completionMarkerName, len(preamble))

	remoteCmd := strings.Join([]string{
		"cd", "--", shellQuote(workspacePath), "&&", guard, "&&", importStep, "&&", agentGroup(remoteCommand, agentArgs),
	}, " ")

	return SSHLaunch{
		Args:     append(sshOpts, remoteCmd),
		preamble: []byte(preamble),
	}
}

// Format implements [fmt.Formatter], redacting the unexported
// preamble for every verb so a carried value baked into it never
// reaches a formatted log line or error.
func (l SSHLaunch) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "sshutil.SSHLaunch{Args:%q}", l.Args) //nolint:errcheck // best-effort formatting
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
	r *strings.Reader
}

// Read implements [io.Reader].
func (r *preambleReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

// Format implements [fmt.Formatter], rendering no preamble bytes for
// any verb.
func (r *preambleReader) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "sshutil.preambleReader{}") //nolint:errcheck // best-effort formatting
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
func (l SSHLaunch) StdinReader() io.Reader {
	if len(l.preamble) == 0 {
		return nil
	}
	return &preambleReader{r: strings.NewReader(string(l.preamble))}
}

// PrefixStdin returns w unchanged when the launch carries no
// preamble. Otherwise it returns a writer whose first Write sends the
// whole preamble to w followed by the caller's bytes, returning the
// preamble's own write error, or [io.ErrShortWrite] when w accepted
// only part of it, and none of the caller's bytes on failure; every
// later Write passes straight through to w; and Close closes w
// without waiting for a Write in progress.
func (l SSHLaunch) PrefixStdin(w io.WriteCloser) io.WriteCloser {
	if len(l.preamble) == 0 {
		return w
	}
	return &preambleWriter{w: w, preamble: l.preamble}
}

// preambleWriter prepends a preamble to the first Write it receives.
// once guards only that one-time send, so preambleWriter is safe for
// concurrent Write calls to the same extent the wrapped writer w is.
type preambleWriter struct {
	w           io.WriteCloser
	preamble    []byte
	once        sync.Once
	preambleErr error
}

// Write implements [io.Writer]. A preamble only partly accepted fails
// the write with [io.ErrShortWrite] rather than starting the agent
// without the variables the launch carries, so a writer reporting a
// short count and no error cannot pass for a delivered preamble.
func (p *preambleWriter) Write(b []byte) (int, error) {
	p.once.Do(func() {
		n, err := p.w.Write(p.preamble)
		if err == nil && n != len(p.preamble) {
			err = io.ErrShortWrite
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
	return p.w.Close()
}

// Format implements [fmt.Formatter], rendering no preamble bytes for
// any verb.
func (p *preambleWriter) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, "sshutil.preambleWriter{}") //nolint:errcheck // best-effort formatting
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
