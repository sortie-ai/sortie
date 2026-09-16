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

// BuildSSHLaunch constructs the SSH invocation arguments for remote
// agent execution. The workspace path sets the remote cwd via cd.
// remoteCommand is treated as a pre-formed POSIX shell fragment and
// appended verbatim; callers are responsible for any quoting within
// that fragment. agentArgs are individually shell-quoted and appended
// after remoteCommand.
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
// [IsEnvName]; the panic message names the entry's index and carries
// neither its Name nor its Value.
func BuildSSHLaunch(host, workspacePath, remoteCommand string, agentArgs []string, opts SSHOptions) SSHLaunch {
	sshOpts := buildSSHOpts(host, opts)

	if len(opts.Env) == 0 {
		var parts []string
		parts = append(parts, "cd", "--", shellQuote(workspacePath), "&&")
		parts = append(parts, remoteCommand)
		for _, arg := range agentArgs {
			parts = append(parts, shellQuote(arg))
		}
		return SSHLaunch{Args: append(sshOpts, strings.Join(parts, " "))}
	}

	for i, entry := range opts.Env {
		if !IsEnvName(entry.Name) {
			panic(fmt.Sprintf("sshutil: BuildSSHLaunch: invalid environment variable name at Env[%d]", i))
		}
	}

	assignments := make([]string, len(opts.Env))
	for i, entry := range opts.Env {
		assignments[i] = entry.Name + "=" + shellQuote(entry.Value)
	}
	preamble := "unset _sortie_env && export " + strings.Join(assignments, " ") + " && _sortie_complete=1"

	guard := "{ command -v dd >/dev/null 2>&1 || { echo '" + ddMissingMessage + "' >&2; exit 1; }; }"
	importStep := fmt.Sprintf(`unset _sortie_complete && _sortie_env=$(dd bs=1 count=%d 2>/dev/null) && eval "$_sortie_env" && [ "${_sortie_complete-}" = 1 ]`, len(preamble))

	var parts []string
	parts = append(parts, "cd", "--", shellQuote(workspacePath), "&&", guard, "&&", importStep, "&&")
	parts = append(parts, remoteCommand)
	for _, arg := range agentArgs {
		parts = append(parts, shellQuote(arg))
	}
	remoteCmd := strings.Join(parts, " ")

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
// preamble's own write error and none of the caller's bytes on
// failure; every later Write passes straight through to w; and Close
// closes w without waiting for a Write in progress.
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

// Write implements [io.Writer].
func (p *preambleWriter) Write(b []byte) (int, error) {
	p.once.Do(func() {
		_, p.preambleErr = p.w.Write(p.preamble)
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
