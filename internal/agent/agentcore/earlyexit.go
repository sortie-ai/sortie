package agentcore

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

// EarlyExitCaptureBytes is the byte budget a one-shot launch retains
// per stream, such as the kiro credential guard's standard output and
// standard error, before [ExitedEarly] decides its outcome.
const EarlyExitCaptureBytes = 64 * 1024

const (
	earlyExitMessage       = "the agent runtime exited before responding"
	earlyExitNoOutput      = "no standard-error output"
	earlyExitOmittedMarker = "[earlier output omitted] "
	earlyExitSeparator     = " | "
	earlyExitOutputBound   = 8192
)

// EarlyExit is the observation of whether a runtime exited on its own
// before its session started, made once per failed start by
// [ObserveEarlyExit] or [ExitedEarly]. The zero value reports no such
// exit, so [EarlyExit.Report] returns nil for it.
type EarlyExit struct {
	observed   bool
	waitErr    error
	incomplete bool
	remote     bool
	grace      time.Duration
}

// EarlyExitError carries the exit status and the masked, bounded end
// of a runtime's own standard error for a [domain.ErrPortExit] built by
// [EarlyExit.Report]. Only this package constructs a populated
// EarlyExitError.
type EarlyExitError struct {
	status  string
	output  string
	waitErr error
}

// ObserveEarlyExit waits for reaper to report the runtime reaped on
// its own, within grace, while ctx stayed live, and records that
// observation for a later call to [EarlyExit.Report]. Call it once per
// failed start, only after a connection loss, before Sortie signals
// the runtime.
//
// ctx is the context the start itself received, never a single
// request's own deadline. A nil reaper returns the zero value. A
// non-positive grace resolves to [procutil.DefaultDrainGrace].
func ObserveEarlyExit(ctx context.Context, target LaunchTarget, reaper *procutil.Reaper, grace time.Duration) EarlyExit {
	if reaper == nil {
		return EarlyExit{}
	}
	if grace <= 0 {
		grace = procutil.DefaultDrainGrace
	}

	select {
	case <-reaper.Done():
	case <-ctx.Done():
	case <-time.After(grace):
	}

	select {
	case <-reaper.Done():
	default:
		return EarlyExit{}
	}
	if ctx.Err() != nil {
		return EarlyExit{}
	}

	return EarlyExit{
		observed: true,
		waitErr:  reaper.Err(),
		remote:   target.RemoteCommand != "",
		grace:    grace,
	}
}

// ExitedEarly records the observation for a launch the caller has
// already waited for, such as the kiro credential guard's canary,
// whose context was still live when result was produced. It is always
// observed.
func ExitedEarly(target LaunchTarget, result procutil.CaptureResult) EarlyExit {
	return EarlyExit{
		observed:   true,
		waitErr:    result.WaitErr,
		incomplete: !result.OutputComplete,
		remote:     target.RemoteCommand != "",
		grace:      procutil.DefaultDrainGrace,
	}
}

// OutputWatch observes, for one turn, whether its runtime has written
// a line of readable text on standard output yet. The zero value has
// observed nothing.
type OutputWatch struct {
	seen bool
}

// Observe records line as the turn's response once
// trimSpace(sanitize(string(line))) is non-empty. It does nothing once
// w has already observed a response.
func (w *OutputWatch) Observe(line []byte) {
	if w.seen {
		return
	}
	if trimSpace(SanitizeLine(string(line))) != "" {
		w.seen = true
	}
}

// ExitedBeforeOutput returns the zero EarlyExit once w has observed a
// response, and an observed one carrying waitErr otherwise. The exit
// status recorded in waitErr plays no part in this decision: a runtime
// that never wrote a readable line has not responded, whatever status
// it exited with.
func (w *OutputWatch) ExitedBeforeOutput(target LaunchTarget, waitErr error) EarlyExit {
	if w.seen {
		return EarlyExit{}
	}
	return EarlyExit{
		observed: true,
		waitErr:  waitErr,
		remote:   target.RemoteCommand != "",
		grace:    procutil.DefaultDrainGrace,
	}
}

// Report builds the port_exit error for e, or nil when e recorded no
// early exit. It waits at most e.grace inside
// [procutil.StderrCollector.FinishAndCollect]; a nil stderr is treated
// as empty output. Calling Report again while the secret registry is
// unchanged returns an equal error.
func (e EarlyExit) Report(stderr *procutil.StderrCollector) *domain.AgentError {
	if !e.observed {
		return nil
	}
	if e.remote && sshutil.ConnectionFailed(procutil.ExtractExitCode(e.waitErr)) {
		return ConnectionFailedError()
	}

	var lines []string
	var omitted bool
	if stderr != nil {
		stderr.FinishAndCollect(e.grace)
		lines, omitted = stderr.LastLines()
	}
	if e.incomplete {
		lines = append(lines, procutil.AbandonedMarker)
	}

	output := render(lines, omitted)
	status := exitStatus(e.waitErr)
	return &domain.AgentError{
		Kind:    domain.ErrPortExit,
		Message: earlyExitMessage + ": " + status,
		Err: &EarlyExitError{
			status:  status,
			output:  output,
			waitErr: e.waitErr,
		},
	}
}

// Error returns the report's output, or a fixed string when the
// runtime's standard error held nothing readable.
func (e *EarlyExitError) Error() string {
	if e.output == "" {
		return earlyExitNoOutput
	}
	return e.output
}

// Unwrap returns the wait error the reap reported, nil for exit
// status 0.
func (e *EarlyExitError) Unwrap() error {
	return e.waitErr
}

// Status returns the runtime's exit status, rendered as
// [*exec.ExitError.Error] would.
func (e *EarlyExitError) Status() string {
	return e.status
}

// Output returns the masked, bounded end of the runtime's own standard
// error, or the empty string when nothing readable remained.
func (e *EarlyExitError) Output() string {
	return e.output
}

// exitStatus renders waitErr the way a report's status line does:
// "exit status 0" for a nil error, and an *exec.ExitError's or any
// other error's own Error() text otherwise.
func exitStatus(waitErr error) string {
	if waitErr == nil {
		return "exit status 0"
	}
	return waitErr.Error()
}

// render joins lines into one line of report text: each sanitized,
// masked as a whole so a registered value split by sanitizing is still
// recognized, then trimmed and joined with earlyExitSeparator. The
// result is bounded to earlyExitOutputBound bytes, kept from the end,
// and prefixed with earlyExitOmittedMarker when the bound cut it or
// omittedEarlier reports lines the collector already discarded.
func render(lines []string, omittedEarlier bool) string {
	sanitized := make([]string, len(lines))
	for i, line := range lines {
		sanitized[i] = SanitizeLine(line)
	}
	masked := redact.Mask(strings.Join(sanitized, "\n"))

	var parts []string
	for part := range strings.SplitSeq(masked, "\n") {
		part = trimSpace(part)
		if part != "" {
			parts = append(parts, part)
		}
	}
	out := strings.Join(parts, earlyExitSeparator)

	if out == "" {
		if omittedEarlier {
			return trimSpace(earlyExitOmittedMarker)
		}
		return ""
	}

	if omittedEarlier || len(out) > earlyExitOutputBound {
		keep := earlyExitOutputBound - len(earlyExitOmittedMarker)
		if len(out) > keep {
			i := len(out) - keep
			for i < len(out) && !utf8.RuneStart(out[i]) {
				i++
			}
			if j := strings.Index(out[i:], earlyExitSeparator); j >= 0 && i+j+len(earlyExitSeparator) < len(out) {
				i += j + len(earlyExitSeparator)
			}
			out = out[i:]
		}
		out = earlyExitOmittedMarker + out
	}
	return out
}

// SanitizeLine strips ANSI/C1 escape sequences and control characters
// from line, replacing invalid UTF-8 with U+FFFD and TAB/CR with a
// space, so a runtime's raw stream never corrupts a stored line or a
// prefix check run against it.
func SanitizeLine(line string) string {
	var b strings.Builder
	b.Grow(len(line))

	data := line
	invalidRun := false
	for len(data) > 0 {
		r, size := utf8.DecodeRuneInString(data)
		if r == utf8.RuneError && size <= 1 {
			if !invalidRun {
				b.WriteRune(utf8.RuneError)
				invalidRun = true
			}
			data = data[1:]
			continue
		}
		invalidRun = false
		if r == 0x1B {
			data = skipEscape(data)
			continue
		}
		switch {
		case r == '\t' || r == '\r':
			b.WriteRune(' ')
		case r <= 0x1F || r == 0x7F || (r >= 0x80 && r <= 0x9F):
			// Deleted: C0, DEL, and C1 control characters carry no
			// readable content and can corrupt a single-line record.
		default:
			b.WriteRune(r)
		}
		data = data[size:]
	}
	return b.String()
}

// skipEscape consumes one escape sequence starting at data[0] (ESC,
// 0x1B) and returns the remainder, per the escape grammar sanitize
// recognizes. A sequence the string ends inside consumes the rest of
// data, dropping it entirely.
func skipEscape(data string) string {
	if len(data) < 2 {
		return ""
	}
	rest := data[1:]
	switch rest[0] {
	case '[':
		for i := 1; i < len(rest); i++ {
			if rest[i] >= 0x40 && rest[i] <= 0x7E {
				return rest[i+1:]
			}
		}
		return ""
	case ']', 'P', 'X', '^', '_':
		for i := 1; i < len(rest); i++ {
			if rest[i] == 0x07 {
				return rest[i+1:]
			}
			if rest[i] == 0x1B && i+1 < len(rest) && rest[i+1] == '\\' {
				return rest[i+2:]
			}
		}
		return ""
	default:
		return rest[1:]
	}
}

// trimSpace trims leading and trailing Unicode white space from p.
func trimSpace(p string) string {
	return strings.TrimFunc(p, unicode.IsSpace)
}
