//go:build unix

package procutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

type captureLogRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]slog.Value
}

type captureLogSpy struct {
	mu      sync.Mutex
	records []captureLogRecord
}

func (s *captureLogSpy) Enabled(context.Context, slog.Level) bool { return true }

func (s *captureLogSpy) Handle(_ context.Context, r slog.Record) error {
	rec := captureLogRecord{Level: r.Level, Msg: r.Message, Attrs: make(map[string]slog.Value, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value
		return true
	})
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.mu.Unlock()
	return nil
}

func (s *captureLogSpy) WithAttrs(_ []slog.Attr) slog.Handler { return s }
func (s *captureLogSpy) WithGroup(_ string) slog.Handler      { return s }

func (s *captureLogSpy) snapshot() []captureLogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]captureLogRecord, len(s.records))
	copy(out, s.records)
	return out
}

func findCaptureLogRecord(spy *captureLogSpy, msg string) (captureLogRecord, bool) {
	for _, r := range spy.snapshot() {
		if r.Msg == msg {
			return r, true
		}
	}
	return captureLogRecord{}, false
}

func init() {
	fakeScenarios["procutil.capture-leader"] = agenttest.Typed(runCaptureLeader)
	fakeScenarios["procutil.capture-periodic-writer"] = agenttest.Typed(runCapturePeriodicWriter)
	fakeScenarios["procutil.capture-signal-child"] = agenttest.Typed(runCaptureSignalChild)
}

type captureLeaderParams struct {
	ChildPath    string
	ChildPIDPath string
	Detach       bool
	Stdout       string
	Stderr       string
	ExitCode     int
}

func runCaptureLeader(_ []string, p captureLeaderParams) int {
	if p.ChildPath != "" {
		cmd := exec.Command(p.ChildPath) //nolint:gosec // fake runtime path this scenario was handed
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if p.Detach {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "capture leader: start child: %v\n", err)
			return 2
		}
		if p.ChildPIDPath != "" {
			if err := os.WriteFile(p.ChildPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "capture leader: write child pid: %v\n", err)
				return 2
			}
		}
	}
	fmt.Print(p.Stdout)
	fmt.Fprint(os.Stderr, p.Stderr)
	return p.ExitCode
}

type capturePeriodicWriterParams struct {
	Line     string
	Interval time.Duration
}

func runCapturePeriodicWriter(_ []string, p capturePeriodicWriterParams) int {
	for {
		if _, err := io.WriteString(os.Stdout, p.Line); err != nil {
			return 2
		}
		time.Sleep(p.Interval)
	}
}

type captureSignalChildParams struct {
	TrapSIGTERM bool
	Marker      string

	IgnoreSIGTERM bool

	ChildPath    string
	ChildPIDPath string

	ReadyPath string
}

func runCaptureSignalChild(_ []string, p captureSignalChildParams) int {
	if p.TrapSIGTERM {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Print(p.Marker)
			os.Exit(0)
		}()
	}
	if p.IgnoreSIGTERM {
		signal.Ignore(syscall.SIGTERM)
	}

	if p.ChildPath != "" {
		cmd := exec.Command(p.ChildPath) //nolint:gosec // fake runtime path this scenario was handed
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "signal child: start descendant: %v\n", err)
			return 2
		}
		if p.ChildPIDPath != "" {
			if err := os.WriteFile(p.ChildPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "signal child: write descendant pid: %v\n", err)
				return 2
			}
		}
	}

	if p.ReadyPath != "" {
		if err := os.WriteFile(p.ReadyPath, nil, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "signal child: write ready marker: %v\n", err)
			return 2
		}
	}

	agenttest.Hang()
	return 0
}

func pollCaptureFileExists(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pollCaptureFileExists(%q): not present after %v", path, timeout)
}

const pollCapturePIDFileTimeout = 5 * time.Second

func pollCapturePIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(pollCapturePIDFileTimeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pollCapturePIDFile(%q): no valid pid after %v", path, pollCapturePIDFileTimeout)
	return 0
}

func assertCaptureProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d still answers signal 0 after %v, want it gone", pid, timeout)
}

func waitCaptureResult(t *testing.T, c *Capture, timeout time.Duration) CaptureResult {
	t.Helper()
	done := make(chan CaptureResult, 1)
	go func() { done <- c.Wait() }()
	select {
	case result := <-done:
		return result
	case <-time.After(timeout):
		t.Fatalf("Capture.Wait() did not return within %v", timeout)
		return CaptureResult{}
	}
}

func TestCapture_EscapedDescendantHoldingBothStreams(t *testing.T) {
	dir := t.TempDir()
	childPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-leader", captureLeaderParams{
		ChildPath:    childPath,
		ChildPIDPath: pidPath,
		Detach:       true,
		Stdout:       "leader output\n",
		ExitCode:     0,
	})

	cmd := exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	var stdout bytes.Buffer
	spy := &captureLogSpy{}
	logger := slog.New(spy)

	const grace = 300 * time.Millisecond
	c, err := StartCapture(context.Background(), cmd, CaptureParams{Stdout: &stdout, DrainGrace: grace, Logger: logger})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	childPID := pollCapturePIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	start := time.Now()
	result := waitCaptureResult(t, c, grace+5*time.Second)
	if elapsed := time.Since(start); elapsed > grace+3*time.Second {
		t.Errorf("Wait() took %v, want close to the %v drain bound", elapsed, grace)
	}

	if result.WaitErr != nil {
		t.Errorf("WaitErr = %v, want nil", result.WaitErr)
	}
	if result.OutputComplete {
		t.Error("OutputComplete = true, want false (the escaped descendant still holds the pipe)")
	}
	if got := stdout.String(); got != "leader output\n" {
		t.Errorf("collected stdout = %q, want %q", got, "leader output\n")
	}

	record, ok := findCaptureLogRecord(spy, CaptureAbandonedWarning)
	if !ok {
		t.Fatalf("no %q record captured", CaptureAbandonedWarning)
	}
	if got := len(record.Attrs); got != 2 {
		t.Errorf("WARN record carries %d attributes, want exactly 2 (command, drain_bound); got %v", got, record.Attrs)
	}
	if got := record.Attrs["command"].String(); got != "leader" {
		t.Errorf("command attribute = %q, want %q (never the argument vector)", got, "leader")
	}
	if got := record.Attrs["drain_bound"].Duration(); got != grace {
		t.Errorf("drain_bound attribute = %v, want %v", got, grace)
	}
}

func TestCapture_HeldDescendantHoldingBothStreams(t *testing.T) {
	dir := t.TempDir()
	childPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-leader", captureLeaderParams{
		ChildPath:    childPath,
		ChildPIDPath: pidPath,
		Detach:       false,
		Stdout:       "leader output\n",
		ExitCode:     0,
	})

	cmd := exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetProcessGroup(cmd)
	var stdout, logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	const grace = 3 * time.Second
	c, err := StartCapture(context.Background(), cmd, CaptureParams{Stdout: &stdout, DrainGrace: grace, Logger: logger})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	childPID := pollCapturePIDFile(t, pidPath)

	start := time.Now()
	result := waitCaptureResult(t, c, grace+5*time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Wait() took %v, want well under the %v drain bound", elapsed, grace)
	}

	if result.WaitErr != nil {
		t.Errorf("WaitErr = %v, want nil", result.WaitErr)
	}
	if !result.OutputComplete {
		t.Error("OutputComplete = false, want true (the held descendant dies with the group)")
	}
	if got := stdout.String(); got != "leader output\n" {
		t.Errorf("collected stdout = %q, want %q", got, "leader output\n")
	}
	if logBuf.Len() != 0 {
		t.Errorf("unexpected log output: %q, want no WARN", logBuf.String())
	}
	assertCaptureProcessGone(t, childPID, 3*time.Second)
}

func TestCapture_SealedSinkDiscardsChunksAfterWaitReturns(t *testing.T) {
	origClose := closeWithoutWaiting
	closeWithoutWaiting = func(io.Closer) {
		go func() { select {} }() //nolint:staticcheck // deliberately never completes, modeling a close that never resolves
	}
	defer func() { closeWithoutWaiting = origClose }()

	dir := t.TempDir()
	writerPath := agenttest.FakeRuntime(t, dir, "writer", "procutil.capture-periodic-writer", capturePeriodicWriterParams{
		Line:     "tick\n",
		Interval: 10 * time.Millisecond,
	})
	pidPath := filepath.Join(dir, "child.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-leader", captureLeaderParams{
		ChildPath:    writerPath,
		ChildPIDPath: pidPath,
		Detach:       true,
		Stdout:       "leader\n",
		ExitCode:     0,
	})

	cmd := exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	var stdout bytes.Buffer

	const grace = 50 * time.Millisecond
	c, err := StartCapture(context.Background(), cmd, CaptureParams{Stdout: &stdout, DrainGrace: grace})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}
	writerPID := pollCapturePIDFile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(writerPID, syscall.SIGKILL) })

	result := waitCaptureResult(t, c, grace+2*time.Second)
	if result.OutputComplete {
		t.Fatal("OutputComplete = true, want false (the periodic writer still holds the pipe)")
	}
	lengthAtReturn := stdout.Len()

	time.Sleep(200 * time.Millisecond)

	if got := stdout.Len(); got != lengthAtReturn {
		t.Errorf("buffer grew from %d to %d bytes during the 200ms after Wait() returned, want no growth: a sealed sink must discard later chunks", lengthAtReturn, got)
	}
}

func TestRunCapture_SIGTERMTrapMarkerCollected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	childPath := agenttest.FakeRuntime(t, dir, "child", "procutil.capture-signal-child", captureSignalChildParams{
		TrapSIGTERM: true,
		Marker:      "graceful-exit\n",
		ReadyPath:   readyPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, childPath) //nolint:gosec // fake runtime path under t.TempDir()
	var stdout bytes.Buffer

	type outcome struct {
		result CaptureResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := RunCapture(ctx, cmd, DefaultStopGrace, CaptureParams{Stdout: &stdout})
		done <- outcome{result, err}
	}()

	pollCaptureFileExists(t, readyPath, 5*time.Second)
	cancel()

	select {
	case oc := <-done:
		if oc.err != nil {
			t.Fatalf("RunCapture() error = %v", oc.err)
		}
		if !strings.Contains(stdout.String(), "graceful-exit") {
			t.Errorf("collected stdout = %q, want it to contain the graceful-exit marker", stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCapture() did not return within 5s of cancellation")
	}
}

func TestStartCapture_CleanupFailureLogsExactlyOneRecord(t *testing.T) {
	shortenDrainBound(t, 100*time.Millisecond)
	stubGroupKill(t, func(int, syscall.Signal) error { return nil })
	stubLiveMember(t, func(int, int) (bool, error) { return true, nil })

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	spy := &captureLogSpy{}
	logger := slog.New(spy)

	c, err := StartCapture(context.Background(), cmd, CaptureParams{Logger: logger})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}

	waitCaptureResult(t, c, groupDrainBound+5*time.Second)

	var count int
	for _, r := range spy.snapshot() {
		if r.Msg == CaptureCleanupWarning {
			count++
		}
	}
	if count != 1 {
		t.Errorf("CaptureCleanupWarning logged %d times for a captured launch, want exactly 1 (Capture.Wait must not log its own duplicate copy)", count)
	}
}

func TestSetGroupKill_CancellationSendsSIGKILLNotGraceful(t *testing.T) {
	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	descendantPIDPath := filepath.Join(dir, "descendant.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-signal-child", captureSignalChildParams{
		IgnoreSIGTERM: true,
		ReadyPath:     readyPath,
		ChildPath:     descendantPath,
		ChildPIDPath:  descendantPIDPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupKill(cmd)

	c, err := StartCapture(ctx, cmd, CaptureParams{})
	if err != nil {
		t.Fatalf("StartCapture() error = %v", err)
	}

	pollCaptureFileExists(t, readyPath, 5*time.Second)
	descendantPID := pollCapturePIDFile(t, descendantPIDPath)
	t.Cleanup(func() { _ = syscall.Kill(descendantPID, syscall.SIGKILL) })

	cancelAt := time.Now()
	cancel()

	result := waitCaptureResult(t, c, time.Second)
	if elapsed := time.Since(cancelAt); elapsed > time.Second {
		t.Errorf("Wait() returned %v after cancellation, want well within 1s", elapsed)
	}

	var exitErr *exec.ExitError
	if !errors.As(result.WaitErr, &exitErr) {
		t.Fatalf("WaitErr = %v (%T), want *exec.ExitError", result.WaitErr, result.WaitErr)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Errorf("WaitErr signal = %v (signaled=%v), want SIGKILL", status.Signal(), ok && status.Signaled())
	}

	assertCaptureProcessGone(t, descendantPID, 3*time.Second)
}
