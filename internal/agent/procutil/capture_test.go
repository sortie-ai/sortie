package procutil

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
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
