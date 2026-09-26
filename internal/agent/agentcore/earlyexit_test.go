package agentcore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/redact"
)

func earlyExitCmd(t *testing.T, out agenttest.Output) *exec.Cmd {
	t.Helper()
	path := agenttest.FakeRuntime(t, t.TempDir(), "earlyexit-fake", agenttest.OutputScenario, out)
	return exec.Command(path) //nolint:gosec // fake runtime path under t.TempDir()
}

func reapedReaper(t *testing.T, out agenttest.Output) *procutil.Reaper {
	t.Helper()
	cmd := earlyExitCmd(t, out)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	r := procutil.StartReaper(cmd, nil)
	<-r.Done()
	return r
}

func TestObserveEarlyExit_NilReaperReturnsZeroValue(t *testing.T) {
	t.Parallel()

	got := ObserveEarlyExit(context.Background(), LaunchTarget{}, nil, time.Second)
	if got.observed {
		t.Errorf("ObserveEarlyExit(nil reaper) = %+v, want the zero value", got)
	}
	if report := got.Report(nil); report != nil {
		t.Errorf("Report() on a nil-reaper observation = %v, want nil", report)
	}
}

func TestObserveEarlyExit_ObservesAReapedRuntime(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 7})
	got := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	if !got.observed {
		t.Fatalf("ObserveEarlyExit() = %+v, want observed true", got)
	}
	if got.remote {
		t.Error("ObserveEarlyExit() remote = true for a LaunchTarget with no RemoteCommand")
	}
	var exitErr *exec.ExitError
	if !errors.As(got.waitErr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Errorf("ObserveEarlyExit() waitErr = %v, want an *exec.ExitError with code 7", got.waitErr)
	}
}

func TestObserveEarlyExit_MarksRemoteFromTarget(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 0})
	target := LaunchTarget{RemoteCommand: "codex app-server"}
	got := ObserveEarlyExit(context.Background(), target, r, time.Second)

	if !got.observed || !got.remote {
		t.Errorf("ObserveEarlyExit() = %+v, want observed and remote true", got)
	}
}

func TestObserveEarlyExit_NonPositiveGraceResolvesToDefault(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 0})
	got := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, 0)

	if got.grace != procutil.DefaultDrainGrace {
		t.Errorf("ObserveEarlyExit() grace = %v, want %v", got.grace, procutil.DefaultDrainGrace)
	}
}

func TestObserveEarlyExit_ReturnsZeroWhenContextEndedBeforeReap(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 0})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := ObserveEarlyExit(ctx, LaunchTarget{}, r, time.Second)
	if got.observed {
		t.Errorf("ObserveEarlyExit() = %+v, want the zero value when ctx ended before the observation ran", got)
	}
}

func TestObserveEarlyExit_ReturnsZeroWhenRuntimeStaysAlivePastGrace(t *testing.T) {
	t.Parallel()

	cmd := earlyExitCmd(t, agenttest.Output{Hang: true})
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	r := procutil.StartReaper(cmd, nil)

	got := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, 50*time.Millisecond)
	if got.observed {
		t.Errorf("ObserveEarlyExit() = %+v, want the zero value for a runtime still alive past the grace", got)
	}
}

func TestExitedEarly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		target         LaunchTarget
		result         procutil.CaptureResult
		wantIncomplete bool
		wantRemote     bool
	}{
		{
			name:   "complete local capture",
			target: LaunchTarget{},
			result: procutil.CaptureResult{WaitErr: nil, OutputComplete: true},
		},
		{
			name:           "incomplete capture",
			target:         LaunchTarget{},
			result:         procutil.CaptureResult{WaitErr: nil, OutputComplete: false},
			wantIncomplete: true,
		},
		{
			name:       "remote target",
			target:     LaunchTarget{RemoteCommand: "codex app-server"},
			result:     procutil.CaptureResult{WaitErr: nil, OutputComplete: true},
			wantRemote: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ExitedEarly(tt.target, tt.result)
			if !got.observed {
				t.Error("ExitedEarly() observed = false, want true (always observed)")
			}
			if got.incomplete != tt.wantIncomplete {
				t.Errorf("ExitedEarly() incomplete = %v, want %v", got.incomplete, tt.wantIncomplete)
			}
			if got.remote != tt.wantRemote {
				t.Errorf("ExitedEarly() remote = %v, want %v", got.remote, tt.wantRemote)
			}
			if got.grace != procutil.DefaultDrainGrace {
				t.Errorf("ExitedEarly() grace = %v, want %v", got.grace, procutil.DefaultDrainGrace)
			}
		})
	}
}

func TestEarlyExit_Report_ZeroValueReturnsNil(t *testing.T) {
	t.Parallel()

	var e EarlyExit
	if got := e.Report(nil); got != nil {
		t.Errorf("Report() on the zero EarlyExit = %v, want nil", got)
	}
}

func TestEarlyExit_Report_RemoteConnectionFailure(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 255})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{RemoteCommand: "codex app-server"}, r, time.Second)

	got := e.Report(nil)
	if got == nil || got.Kind != domain.ErrPortExit || !errors.Is(got, sshutil.ErrConnectionFailed) {
		t.Fatalf("Report() = %v, want a port_exit *domain.AgentError wrapping sshutil.ErrConnectionFailed", got)
	}
}

func TestEarlyExit_Report_BuildsPortExitWithStatusAndOutput(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 2})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	stderr := procutil.NewStderrCollector(strings.NewReader("boom: bad argument\n"), nil)
	got := e.Report(stderr)
	if got == nil || got.Kind != domain.ErrPortExit {
		t.Fatalf("Report() = %v, want a port_exit *domain.AgentError", got)
	}
	wantMessage := "the agent runtime exited before responding: exit status 2"
	if got.Message != wantMessage {
		t.Errorf("Report() Message = %q, want %q", got.Message, wantMessage)
	}

	var earlyExitErr *EarlyExitError
	if !errors.As(got.Err, &earlyExitErr) {
		t.Fatalf("Report() Err = %v (%T), want *EarlyExitError", got.Err, got.Err)
	}
	if earlyExitErr.Status() != "exit status 2" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 2")
	}
	if earlyExitErr.Output() != "boom: bad argument" {
		t.Errorf("EarlyExitError.Output() = %q, want %q", earlyExitErr.Output(), "boom: bad argument")
	}
	if earlyExitErr.Error() != earlyExitErr.Output() {
		t.Errorf("EarlyExitError.Error() = %q, want it to equal Output()", earlyExitErr.Error())
	}
	if !errors.Is(got, earlyExitErr.waitErr) && earlyExitErr.Unwrap() != earlyExitErr.waitErr {
		t.Errorf("EarlyExitError.Unwrap() = %v, want the wait error", earlyExitErr.Unwrap())
	}
}

func TestEarlyExit_Report_ExitStatusZero(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 0})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	got := e.Report(nil)
	var earlyExitErr *EarlyExitError
	if !errors.As(got.Err, &earlyExitErr) {
		t.Fatalf("Report().Err = %v, want *EarlyExitError", got.Err)
	}
	if earlyExitErr.Status() != "exit status 0" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 0")
	}
	if earlyExitErr.Unwrap() != nil {
		t.Errorf("EarlyExitError.Unwrap() = %v, want nil for exit status 0", earlyExitErr.Unwrap())
	}
}

func TestEarlyExit_Report_NoOutputYieldsFixedError(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 1})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	got := e.Report(nil)
	var earlyExitErr *EarlyExitError
	if !errors.As(got.Err, &earlyExitErr) {
		t.Fatalf("Report().Err = %v, want *EarlyExitError", got.Err)
	}
	if earlyExitErr.Output() != "" {
		t.Errorf("EarlyExitError.Output() = %q, want empty for a nil stderr collector", earlyExitErr.Output())
	}
	if earlyExitErr.Error() != earlyExitNoOutput {
		t.Errorf("EarlyExitError.Error() = %q, want %q", earlyExitErr.Error(), earlyExitNoOutput)
	}
}

func TestEarlyExit_Report_MasksRegisteredValueInOutput(t *testing.T) {
	t.Parallel()

	secret := "earlyexit-report-secret-9f3a7c21"
	redact.Add("test.earlyexit report", secret)

	r := reapedReaper(t, agenttest.Output{ExitCode: 2})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	stderr := procutil.NewStderrCollector(strings.NewReader("failed with credential "+secret+"\n"), nil)
	got := e.Report(stderr)

	var earlyExitErr *EarlyExitError
	if !errors.As(got.Err, &earlyExitErr) {
		t.Fatalf("Report().Err = %v, want *EarlyExitError", got.Err)
	}
	if strings.Contains(earlyExitErr.Output(), secret) {
		t.Fatalf("EarlyExitError.Output() = %q, leaked the registered value", earlyExitErr.Output())
	}
	if !strings.Contains(earlyExitErr.Output(), redact.Marker) {
		t.Errorf("EarlyExitError.Output() = %q, want it to contain %q", earlyExitErr.Output(), redact.Marker)
	}
}

func TestEarlyExit_Report_IncompleteAppendsAbandonedMarker(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 2})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)
	e.incomplete = true

	stderr := procutil.NewStderrCollector(strings.NewReader("a readable line\n"), nil)
	got := e.Report(stderr)

	var earlyExitErr *EarlyExitError
	if !errors.As(got.Err, &earlyExitErr) {
		t.Fatalf("Report().Err = %v, want *EarlyExitError", got.Err)
	}
	if !strings.Contains(earlyExitErr.Output(), procutil.AbandonedMarker) {
		t.Errorf("EarlyExitError.Output() = %q, want it to contain %q for an incomplete capture", earlyExitErr.Output(), procutil.AbandonedMarker)
	}
}

// TestEarlyExit_Report_SecondFinishAndCollectDoesNotWait pins that once
// a collector's own FinishAndCollect call has already resolved, a
// second call inside Report, such as the early-exit computation's own,
// returns immediately rather than paying a further grace, per
// [procutil.StderrCollector.FinishAndCollect]'s sync.Once guard.
func TestEarlyExit_Report_SecondFinishAndCollectDoesNotWait(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	stderr := procutil.NewStderrCollector(r, nil)

	const drainDelay = 150 * time.Millisecond
	time.AfterFunc(drainDelay, func() { _ = w.Close() })

	// The first call actually waits out the drain, so there is a real
	// wait for a bug to double.
	stderr.FinishAndCollect(5 * time.Second)

	e := EarlyExit{observed: true, grace: 5 * time.Second}
	start := time.Now()
	_ = e.Report(stderr)
	elapsed := time.Since(start)

	if elapsed > drainDelay {
		t.Errorf("Report() took %v after FinishAndCollect already resolved, want well under the drain delay %v, want no additional wait toward the 5s grace", elapsed, drainDelay)
	}
}

func TestEarlyExit_Report_RepeatedCallsProduceEqualErrors(t *testing.T) {
	t.Parallel()

	r := reapedReaper(t, agenttest.Output{ExitCode: 3})
	e := ObserveEarlyExit(context.Background(), LaunchTarget{}, r, time.Second)

	first := e.Report(procutil.NewStderrCollector(strings.NewReader("stable line\n"), nil))
	second := e.Report(procutil.NewStderrCollector(strings.NewReader("stable line\n"), nil))

	if first.Message != second.Message {
		t.Errorf("Report() Message differs across calls: %q vs %q", first.Message, second.Message)
	}
	var firstErr, secondErr *EarlyExitError
	if !errors.As(first.Err, &firstErr) || !errors.As(second.Err, &secondErr) {
		t.Fatalf("Report().Err did not chain *EarlyExitError on both calls")
	}
	if firstErr.Status() != secondErr.Status() || firstErr.Output() != secondErr.Output() {
		t.Errorf("Report() produced different EarlyExitError content across calls: %+v vs %+v", firstErr, secondErr)
	}
}

func TestOutputWatch_Observe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lines    [][]byte
		wantSeen bool
	}{
		{name: "readable line sets seen", lines: [][]byte{[]byte("hello")}, wantSeen: true},
		{name: "empty line is not a response", lines: [][]byte{[]byte("")}, wantSeen: false},
		{name: "whitespace-only line is not a response", lines: [][]byte{[]byte("   \t  ")}, wantSeen: false},
		{name: "escape-sequence-only line is not a response", lines: [][]byte{[]byte("\x1b[31m\x1b[0m")}, wantSeen: false},
		{name: "an unreadable line then a readable one sets seen", lines: [][]byte{[]byte(""), []byte("ok")}, wantSeen: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var w OutputWatch
			for _, line := range tt.lines {
				w.Observe(line)
			}
			if w.seen != tt.wantSeen {
				t.Errorf("OutputWatch.Observe(%q) seen = %v, want %v", tt.lines, w.seen, tt.wantSeen)
			}
		})
	}
}

func TestOutputWatch_Observe_DoesNothingOnceSeen(t *testing.T) {
	t.Parallel()

	var w OutputWatch
	w.Observe([]byte("first response"))
	w.Observe([]byte(""))

	if !w.seen {
		t.Fatal("OutputWatch.seen = false after a readable line, want true")
	}
}

func TestOutputWatch_ExitedBeforeOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		seen       bool
		waitErr    error
		target     LaunchTarget
		wantZero   bool
		wantRemote bool
	}{
		{name: "seen returns the zero value regardless of exit status", seen: true, wantZero: true},
		{name: "unseen exit status 0 is observed", seen: false, waitErr: nil},
		{name: "unseen exit status 127 is observed", seen: false, waitErr: exitErrorWithCode(t, 127)},
		{name: "unseen signal death is observed", seen: false, waitErr: exitErrorWithCode(t, -1)},
		{name: "unseen remote target marks remote", seen: false, target: LaunchTarget{RemoteCommand: "codex app-server"}, wantRemote: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w := OutputWatch{seen: tt.seen}
			got := w.ExitedBeforeOutput(tt.target, tt.waitErr)

			if tt.wantZero {
				if got != (EarlyExit{}) {
					t.Errorf("ExitedBeforeOutput() = %+v, want the zero EarlyExit", got)
				}
				return
			}
			if !got.observed {
				t.Error("ExitedBeforeOutput() observed = false, want true")
			}
			if got.remote != tt.wantRemote {
				t.Errorf("ExitedBeforeOutput() remote = %v, want %v", got.remote, tt.wantRemote)
			}
			if got.grace != procutil.DefaultDrainGrace {
				t.Errorf("ExitedBeforeOutput() grace = %v, want %v", got.grace, procutil.DefaultDrainGrace)
			}
			if !errors.Is(got.waitErr, tt.waitErr) && got.waitErr != tt.waitErr {
				t.Errorf("ExitedBeforeOutput() waitErr = %v, want %v", got.waitErr, tt.waitErr)
			}
		})
	}
}

// exitErrorWithCode runs a fake runtime exiting with code and returns
// the resulting *exec.ExitError, so a table case can exercise a real
// wait error rather than a hand-built stand-in.
func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	if code < 0 {
		cmd := earlyExitCmd(t, agenttest.Output{Hang: true})
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("Process.Kill() = %v", err)
		}
		return cmd.Wait()
	}
	cmd := earlyExitCmd(t, agenttest.Output{ExitCode: code})
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	return cmd.Wait()
}

func TestRender(t *testing.T) {
	t.Parallel()

	t.Run("empty input with no earlier omission is empty", func(t *testing.T) {
		t.Parallel()
		if got := render(nil, false); got != "" {
			t.Errorf("render(nil, false) = %q, want empty", got)
		}
	})

	t.Run("empty input with earlier omission is the marker alone", func(t *testing.T) {
		t.Parallel()
		if got := render(nil, true); got != strings.TrimSpace(earlyExitOmittedMarker) {
			t.Errorf("render(nil, true) = %q, want %q", got, strings.TrimSpace(earlyExitOmittedMarker))
		}
	})

	t.Run("lines join with the separator, trimmed and empties dropped", func(t *testing.T) {
		t.Parallel()
		got := render([]string{"  first  ", "", "second", "   "}, false)
		want := "first" + earlyExitSeparator + "second"
		if got != want {
			t.Errorf("render() = %q, want %q", got, want)
		}
	})

	t.Run("bound cut keeps the tail and prefixes the omitted marker", func(t *testing.T) {
		t.Parallel()
		lines := make([]string, 0, 2000)
		for i := range 2000 {
			lines = append(lines, "line-"+string(rune('a'+i%26)))
		}
		got := render(lines, false)
		if len(got) > earlyExitOutputBound {
			t.Fatalf("render() length = %d, want at most %d", len(got), earlyExitOutputBound)
		}
		if !strings.HasPrefix(got, earlyExitOmittedMarker) {
			t.Errorf("render() = %q, want it to start with %q", got[:min(len(got), 60)], earlyExitOmittedMarker)
		}
		if !strings.HasSuffix(got, "line-"+string(rune('a'+1999%26))) {
			t.Errorf("render() does not end with the last line: %q", got[max(0, len(got)-20):])
		}
	})

	t.Run("omitted earlier always prefixes the marker even under the bound", func(t *testing.T) {
		t.Parallel()
		got := render([]string{"short line"}, true)
		if !strings.HasPrefix(got, earlyExitOmittedMarker) {
			t.Errorf("render() = %q, want it to start with %q", got, earlyExitOmittedMarker)
		}
		if !strings.HasSuffix(got, "short line") {
			t.Errorf("render() = %q, want it to end with %q", got, "short line")
		}
	})

	t.Run("cut never splits a multi-byte rune", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("日", 5000)
		got := render([]string{line}, false)
		if !utf8ValidCheck(got) {
			t.Errorf("render() produced invalid UTF-8: %q", got[:min(len(got), 40)])
		}
	})
}

func utf8ValidCheck(s string) bool {
	for _, r := range s {
		if r == '�' {
			// Only acceptable when the source line already carried an
			// invalid rune, which this helper's callers never construct;
			// range over a string always yields valid runes for valid
			// UTF-8 input, so a decoded replacement character here
			// signals a cut mid-rune, which render must never produce.
			return false
		}
	}
	return true
}

func TestSanitize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text unchanged", in: "hello world", want: "hello world"},
		{name: "CSI sequence stripped", in: "before\x1b[31mred\x1b[0mafter", want: "beforeredafter"},
		{name: "OSC sequence terminated by BEL stripped", in: "a\x1b]0;title\x07b", want: "ab"},
		{name: "OSC sequence terminated by ST stripped", in: "a\x1b]0;title\x1b\\b", want: "ab"},
		{name: "lone escape byte pair consumed", in: "a\x1bcb", want: "ab"},
		{name: "unterminated escape drops the rest of the line", in: "keep\x1b[31", want: "keep"},
		{name: "tab and carriage return become a space", in: "a\tb\rc", want: "a b c"},
		{name: "C0 control characters deleted", in: "a\x00\x01\x02b", want: "ab"},
		{name: "DEL deleted", in: "a\x7fb", want: "ab"},
		{name: "C1 control characters deleted", in: "a\u0080\u009Fb", want: "ab"},
		{name: "invalid UTF-8 becomes replacement character", in: "a\xffb", want: "a�b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SanitizeLine(tt.in); got != tt.want {
				t.Errorf("SanitizeLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimSpace(t *testing.T) {
	t.Parallel()

	if got := trimSpace("  padded  "); got != "padded" {
		t.Errorf("trimSpace(%q) = %q, want %q", "  padded  ", got, "padded")
	}
}
