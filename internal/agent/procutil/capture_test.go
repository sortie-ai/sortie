package procutil

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func init() {
	fakeScenarios["procutil.capture-alternate"] = agenttest.Typed(runCaptureAlternate)
}

// captureAlternateParams parameterizes the procutil.capture-alternate
// scenario.
type captureAlternateParams struct{}

// runCaptureAlternate writes four short chunks alternating between
// standard output and standard error, with a brief pause between each
// so the writes land as separate pipe operations, then exits 0.
func runCaptureAlternate(_ []string, _ captureAlternateParams) int {
	writes := []struct {
		w    *os.File
		text string
	}{
		{os.Stdout, "1"},
		{os.Stderr, "2"},
		{os.Stdout, "3"},
		{os.Stderr, "4"},
	}
	for _, wr := range writes {
		if _, err := io.WriteString(wr.w, wr.text); err != nil {
			return 2
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0
}

// TestStartCapture_SameWriterSharesOnePipeInWriteOrder pins the rule
// that when Stdout and Stderr are the same writer, StartCapture wires
// cmd.Stdout and cmd.Stderr to the same *os.File, and alternating
// writes from the subprocess arrive at the caller's writer in write
// order.
func TestStartCapture_SameWriterSharesOnePipeInWriteOrder(t *testing.T) {
	t.Parallel()

	path := agenttest.FakeRuntime(t, t.TempDir(), "alt", "procutil.capture-alternate", captureAlternateParams{})
	cmd := exec.Command(path) //nolint:gosec // fake runtime path under t.TempDir()

	var combined bytes.Buffer
	c, err := StartCapture(cmd, CaptureParams{Stdout: &combined, Stderr: &combined})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}

	stdoutFile, ok1 := cmd.Stdout.(*os.File)
	stderrFile, ok2 := cmd.Stderr.(*os.File)
	if !ok1 || !ok2 {
		t.Fatalf("cmd.Stdout, cmd.Stderr = %T, %T, want both *os.File", cmd.Stdout, cmd.Stderr)
	}
	if stdoutFile != stderrFile {
		t.Errorf("cmd.Stdout = %v, cmd.Stderr = %v, want the same *os.File (one shared pipe)", stdoutFile, stderrFile)
	}

	result := c.Wait()
	if result.WaitErr != nil {
		t.Fatalf("Wait().WaitErr = %v, want nil", result.WaitErr)
	}
	if !result.OutputComplete {
		t.Fatal("Wait().OutputComplete = false, want true")
	}
	if got, want := combined.String(), "1234"; got != want {
		t.Errorf("combined output = %q, want %q (write order preserved across the shared pipe)", got, want)
	}
}

// TestCapture_NonZeroExitReportsExitErrorWithOutputCollected pins that
// a non-zero exit is reported as an *exec.ExitError carrying the exit
// code, and the subprocess's output is still collected.
func TestCapture_NonZeroExitReportsExitErrorWithOutputCollected(t *testing.T) {
	t.Parallel()

	path := agenttest.FakeRuntime(t, t.TempDir(), "fake", agenttest.OutputScenario, agenttest.Output{
		Stdout:   "partial output\n",
		ExitCode: 5,
	})
	cmd := exec.Command(path) //nolint:gosec // fake runtime path under t.TempDir()

	var stdout bytes.Buffer
	c, err := StartCapture(cmd, CaptureParams{Stdout: &stdout})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	result := c.Wait()

	var exitErr *exec.ExitError
	if !errors.As(result.WaitErr, &exitErr) {
		t.Fatalf("WaitErr = %v (%T), want *exec.ExitError", result.WaitErr, result.WaitErr)
	}
	if got := exitErr.ExitCode(); got != 5 {
		t.Errorf("ExitCode() = %d, want 5", got)
	}
	if got := stdout.String(); got != "partial output\n" {
		t.Errorf("collected stdout = %q, want %q", got, "partial output\n")
	}
}

// TestCapture_AbandonedStreamClosesThroughAsyncSeam pins that Wait
// closes an abandoned stream through the closeWithoutWaiting seam
// (which, per its own contract, returns at once regardless of whether
// the underlying close ever completes) rather than a direct synchronous
// close, so a close that never completes cannot block Wait's return.
//
// This is a white-box test: it builds a Capture directly from a
// manually held-open pipe rather than driving a real capture, so it
// needs no platform-specific descendant machinery and belongs in this
// platform-neutral file. c.cmd still needs a real started process: on
// Windows Wait's drainCaptureJob step dereferences cmd.Process
// unconditionally, and a synthetic *exec.Cmd that was never started
// leaves that field nil.
func TestCapture_AbandonedStreamClosesThroughAsyncSeam(t *testing.T) {
	origClose := closeWithoutWaiting
	var once sync.Once
	invoked := make(chan struct{})
	closeWithoutWaiting = func(io.Closer) {
		// A close that never completes: nothing here ever calls the
		// real Close, modeling a cancellation that never resolves
		// (the asyncclose.go doc comment's "or whether it ever
		// does"). It still returns at once, per that same contract.
		once.Do(func() { close(invoked) })
	}
	defer func() { closeWithoutWaiting = origClose }()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	defer func() { _ = write.Close() }()
	defer func() { _ = read.Close() }()

	path := agenttest.FakeRuntime(t, t.TempDir(), "held", agenttest.OutputScenario, agenttest.Output{Hang: true})
	cmd := exec.Command(path) //nolint:gosec // fake runtime path under t.TempDir()
	if startErr := cmd.Start(); startErr != nil {
		t.Fatalf("cmd.Start() error = %v", startErr)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	reaper := &Reaper{done: make(chan struct{})}
	close(reaper.done)

	var buf bytes.Buffer
	stream := newCaptureStream(read, &buf)
	go stream.drain()

	c := &Capture{
		cmd:        cmd,
		logger:     slog.New(slog.DiscardHandler),
		streams:    []*captureStream{stream},
		reaper:     reaper,
		drainGrace: 50 * time.Millisecond,
	}

	type outcome struct{ result CaptureResult }
	done := make(chan outcome, 1)
	go func() { done <- outcome{c.Wait()} }()

	select {
	case oc := <-done:
		if oc.result.OutputComplete {
			t.Error("OutputComplete = true, want false (the pipe was never written to or closed)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return within 2s despite the close seam returning at once, want it not to block on the close ever completing")
	}

	select {
	case <-invoked:
	default:
		t.Error("closeWithoutWaiting was never invoked for the abandoned stream, want Wait to close it through the seam rather than a direct synchronous close")
	}
}

var startEntries = []struct {
	name  string
	start func(cmd *exec.Cmd) error
}{
	{
		name: "StartCapture",
		start: func(cmd *exec.Cmd) error {
			_, err := StartCapture(cmd, CaptureParams{})
			return err
		},
	},
	{
		name: "StartWithOwnedPipes",
		start: func(cmd *exec.Cmd) error {
			_, _, err := StartWithOwnedPipes(cmd, nil)
			return err
		},
	},
}

type cancelProbe struct {
	cancel   context.CancelFunc
	ran      chan struct{}
	observed atomic.Bool
}

func newCancelProbe(t *testing.T, install func(*exec.Cmd), name string, args ...string) (*exec.Cmd, *cancelProbe) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fake runtime path under t.TempDir() or a fixed literal script
	install(cmd)

	p := &cancelProbe{cancel: cancel, ran: make(chan struct{})}
	inner := cmd.Cancel
	cmd.Cancel = func() error {
		defer close(p.ran)
		return inner()
	}
	return cmd, p
}

func (p *cancelProbe) fire() {
	p.cancel()
	select {
	case <-p.ran:
		p.observed.Store(true)
	case <-time.After(5 * time.Second):
	}
}

func awaitCaptureResult(t *testing.T, c *Capture, within time.Duration) CaptureResult {
	t.Helper()
	done := make(chan CaptureResult, 1)
	go func() { done <- c.Wait() }()
	select {
	case result := <-done:
		return result
	case <-time.After(within):
		t.Fatalf("Wait() did not return within %v", within)
		return CaptureResult{}
	}
}

func TestCapture_StopWhileRunningIsRecorded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		install func(*exec.Cmd)
		stop    func(t *testing.T, cmd *exec.Cmd, p *cancelProbe)
	}{
		{
			name:    "context cancellation through SetGroupCancel",
			install: func(cmd *exec.Cmd) { SetGroupCancel(cmd, 200*time.Millisecond) },
			stop:    func(_ *testing.T, _ *exec.Cmd, p *cancelProbe) { p.fire() },
		},
		{
			name:    "context cancellation through SetGroupKill",
			install: SetGroupKill,
			stop:    func(_ *testing.T, _ *exec.Cmd, p *cancelProbe) { p.fire() },
		},
		{
			name:    "Group.Kill",
			install: func(*exec.Cmd) {},
			stop: func(t *testing.T, cmd *exec.Cmd, _ *cancelProbe) {
				t.Helper()
				if err := lookupGroup(cmd).Kill(); err != nil {
					t.Errorf("Kill() = %v, want nil", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := agenttest.FakeRuntime(t, t.TempDir(), "fake", agenttest.OutputScenario, agenttest.Output{Hang: true})
			cmd, p := newCancelProbe(t, tt.install, path)
			c, err := StartCapture(cmd, CaptureParams{})
			if err != nil {
				t.Fatalf("StartCapture() error = %v", err)
			}

			tt.stop(t, cmd, p)
			result := awaitCaptureResult(t, c, 10*time.Second)

			if !result.Stopped {
				t.Errorf("Wait().Stopped = false after %s while the child ran, want true", tt.name)
			}
			if result.WaitErr == nil {
				t.Error("Wait().WaitErr = nil for a child the stop terminated, want its exit status")
			}
		})
	}
}

func TestStartFailureReportsWhetherCancellationRefusedIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		command func(t *testing.T) *exec.Cmd
		want    bool
	}{
		{
			name: "a context that is already done",
			command: func(t *testing.T) *exec.Cmd {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				path := agenttest.FakeRuntime(t, t.TempDir(), "fake", agenttest.OutputScenario, agenttest.Output{})
				return exec.CommandContext(ctx, path) //nolint:gosec // fake runtime path under t.TempDir()
			},
			want: true,
		},
		{
			name: "a file that is not executable under a live context",
			command: func(t *testing.T) *exec.Cmd {
				t.Helper()
				path := filepath.Join(t.TempDir(), "notexec.exe")
				if err := os.WriteFile(path, []byte("not a program"), 0o600); err != nil {
					t.Fatalf("os.WriteFile(%q) = %v", path, err)
				}
				return exec.CommandContext(t.Context(), path) //nolint:gosec // file written under t.TempDir()
			},
			want: false,
		},
	}

	for _, tt := range tests {
		for _, st := range startEntries {
			t.Run(tt.name+" through "+st.name, func(t *testing.T) {
				t.Parallel()

				err := st.start(tt.command(t))

				var startErr *StartError
				if !errors.As(err, &startErr) {
					t.Fatalf("%s error = %v (%T), want *StartError", st.name, err, err)
				}
				if startErr.Stage != StageProcessStart {
					t.Errorf("StartError.Stage = %d, want %d (StageProcessStart)", startErr.Stage, StageProcessStart)
				}
				if startErr.Cancelled != tt.want {
					t.Errorf("StartError.Cancelled = %t for %s, want %t", startErr.Cancelled, tt.name, tt.want)
				}
			})
		}
	}
}
