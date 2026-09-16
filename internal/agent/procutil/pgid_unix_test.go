//go:build unix

package procutil

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func init() {
	fakeScenarios["procutil.group-leader"] = agenttest.Typed(runGroupLeader)
	fakeScenarios["procutil.group-descendant"] = agenttest.Typed(runGroupDescendant)
}

func TestSetProcessGroup(t *testing.T) {
	t.Parallel()

	t.Run("nil SysProcAttr", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{}
		SetProcessGroup(cmd)

		if cmd.SysProcAttr == nil {
			t.Fatal("SetProcessGroup() SysProcAttr = nil, want non-nil")
		}
		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
	})

	t.Run("existing SysProcAttr fields preserved", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{
			SysProcAttr: &syscall.SysProcAttr{Noctty: true},
		}
		SetProcessGroup(cmd)

		if !cmd.SysProcAttr.Setpgid {
			t.Error("SetProcessGroup() Setpgid = false, want true")
		}
		if !cmd.SysProcAttr.Noctty {
			t.Error("SetProcessGroup() Noctty = false, want true (pre-existing field must be preserved)")
		}
	})
}

func TestSignalProcessGroup_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; no such process group can exist.
	err := SignalProcessGroup(math.MaxInt32, syscall.SIGTERM)
	if err != nil {
		t.Errorf("SignalProcessGroup(MaxInt32, SIGTERM) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalGraceful_ESRCH(t *testing.T) {
	t.Parallel()

	// math.MaxInt32 is an implausible PID; ESRCH is silently swallowed.
	err := SignalGraceful(math.MaxInt32)
	if err != nil {
		t.Errorf("SignalGraceful(MaxInt32) = %v, want nil (ESRCH must be suppressed)", err)
	}
}

func TestSignalProcessGroup_LiveProcess(t *testing.T) {
	t.Parallel()

	cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	if err := SignalProcessGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("SignalProcessGroup(pid, SIGTERM) = %v, want nil", err)
	}

	err := cmd.Wait()
	if !WasSignaled(err) {
		t.Errorf("WasSignaled(cmd.Wait()) = false, want true (process should have been terminated by SIGTERM)")
	}
}

// pollForPID polls path until it holds a positive integer, returning it.
func pollForPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollForPID(%q) = no PID after %v, want a PID", path, timeout)
	return 0
}

// pollForFile reports whether path appears before the timeout expires.
func pollForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// groupLeaderParams parameterizes the procutil.group-leader scenario: a
// fake runtime that starts a descendant fake runtime and waits for it,
// remaining a member of the process group SetGroupCancel places its own
// launch command into.
type groupLeaderParams struct {
	DescendantPath string
}

func runGroupLeader(_ []string, params groupLeaderParams) int {
	// Disables the default, uncatchable SIGTERM disposition so the
	// leader survives long enough to wait for (and so reap) the
	// descendant, rather than leaving it a zombie for the test's
	// kill(pid, 0) liveness check to trip over.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)

	cmd := exec.Command(params.DescendantPath) //nolint:gosec // fake runtime path under t.TempDir()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "group leader: start descendant: %v\n", err)
		return 1
	}
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "group leader: wait for descendant: %v\n", err)
		return 1
	}
	return 0
}

// groupDescendantParams parameterizes the procutil.group-descendant
// scenario: a fake runtime that records its own PID, then traps a
// catchable termination signal and records that it caught one.
type groupDescendantParams struct {
	Marker  string
	PIDFile string
}

func runGroupDescendant(_ []string, params groupDescendantParams) int {
	// The handler is installed before the PID file publishes readiness:
	// the test cancels as soon as it reads that file, and the default
	// disposition would end this process before it records the signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)

	if err := os.WriteFile(params.PIDFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "group descendant: write pid: %v\n", err)
		return 1
	}

	<-sig

	if err := os.WriteFile(params.Marker, []byte("terminated"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "group descendant: write marker: %v\n", err)
		return 1
	}
	return 0
}

// TestSetGroupCancel_CancelReachesDescendant verifies that cancelling the
// context of a command prepared by SetGroupCancel delivers a catchable
// termination signal to the whole process group, not just to the direct
// child.
//
// The evidence is a marker a grandchild writes from inside its own
// signal handler. A grandchild is reachable only through the group, and
// it can only run a handler if the signal was catchable, so the marker
// distinguishes a group-wide graceful signal from os/exec's default of
// force-killing the direct child alone. The leader waits for the
// descendant before exiting, so it is reaped rather than left a zombie,
// and cmd.Wait cannot return until the marker is on disk.
func TestSetGroupCancel_CancelReachesDescendant(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "descendant.terminated")
	pidFile := filepath.Join(dir, "descendant.pid")

	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", "procutil.group-descendant", groupDescendantParams{
		Marker:  marker,
		PIDFile: pidFile,
	})
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.group-leader", groupLeaderParams{
		DescendantPath: descendantPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, DefaultStopGrace)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v, want nil", err)
	}
	leaderPID := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-leaderPID, syscall.SIGKILL) })

	descendantPID := pollForPID(t, pidFile, 5*time.Second)

	cancel()
	_ = cmd.Wait() //nolint:errcheck // a cancelled command reports the cancellation, not a fault

	if !pollForFile(marker, 5*time.Second) {
		t.Errorf("SetGroupCancel(): cancelling left %q absent, want the descendant to have caught a graceful signal", marker)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil {
		t.Errorf("SetGroupCancel(): descendant %d still alive after cancellation, want gone", descendantPID)
	}
}

// TestKillProcessGroupReportingLeftover_ReturnsPromptlyOnceGone pins that
// the wait returns as soon as the group reports itself gone rather than
// always paying groupDrainBound in full: an already-exited, already-reaped
// group answers ESRCH on the first send, well inside a shortened bound.
//
// groupDrainBound is mutated, so this test does not run in parallel with
// the package's other parallel tests.
func TestKillProcessGroupReportingLeftover_ReturnsPromptlyOnceGone(t *testing.T) {
	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() = %v, want nil", err)
	}

	origBound := groupDrainBound
	t.Cleanup(func() { groupDrainBound = origBound })
	groupDrainBound = 500 * time.Millisecond

	start := time.Now()
	leftover, err := killProcessGroupReportingLeftover(pid)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("killProcessGroupReportingLeftover(%d) error = %v, want nil (an empty group answers ESRCH)", pid, err)
	}
	if leftover {
		t.Errorf("killProcessGroupReportingLeftover(%d) leftover = %t, want false", pid, leftover)
	}
	if elapsed >= groupDrainBound/2 {
		t.Errorf("killProcessGroupReportingLeftover(%d) took %v, want well under the %v drain bound", pid, elapsed, groupDrainBound)
	}
}

// TestKillProcessGroupReportingLeftover_ResendsUntilGone pins that the
// wait resends the group signal rather than sending it once: a process
// joining the group after the first signal is the reason the loop exists,
// so a leftover member that only stops answering after several sends must
// still be observed gone.
//
// groupKillFunc and groupDrainBound are mutated, so this test does not run
// in parallel with the package's other parallel tests.
func TestKillProcessGroupReportingLeftover_ResendsUntilGone(t *testing.T) {
	origBound, origKill := groupDrainBound, groupKillFunc
	t.Cleanup(func() { groupDrainBound, groupKillFunc = origBound, origKill })

	groupDrainBound = time.Second
	const wantCalls = 4
	var calls int
	groupKillFunc = func(int, syscall.Signal) error {
		calls++
		if calls < wantCalls {
			return nil
		}
		return syscall.ESRCH
	}

	leftover, err := killProcessGroupReportingLeftover(4242)

	if err != nil {
		t.Errorf("killProcessGroupReportingLeftover() error = %v, want nil once the group reports gone", err)
	}
	if !leftover {
		t.Error("leftover = false, want true (a member answered before the group reported gone)")
	}
	if calls != wantCalls {
		t.Errorf("groupKillFunc call count = %d, want %d (the wait must resend a member gained after the first signal, not signal once)", calls, wantCalls)
	}
}

// TestKillProcessGroupReportingLeftover_BoundElapsed pins that a group
// that keeps answering past groupDrainBound is reported as a non-nil
// error, and that the wait does not run away past the bound: it stops
// within about one extra poll interval of it, not several multiples.
//
// groupKillFunc and groupDrainBound are mutated, so this test does not run
// in parallel with the package's other parallel tests.
func TestKillProcessGroupReportingLeftover_BoundElapsed(t *testing.T) {
	origBound, origKill := groupDrainBound, groupKillFunc
	t.Cleanup(func() { groupDrainBound, groupKillFunc = origBound, origKill })

	groupDrainBound = 200 * time.Millisecond
	var calls int
	groupKillFunc = func(int, syscall.Signal) error {
		calls++
		return nil
	}

	start := time.Now()
	leftover, err := killProcessGroupReportingLeftover(4242)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("killProcessGroupReportingLeftover() error = nil, want non-nil once the group keeps answering past the drain bound")
	}
	if !leftover {
		t.Error("leftover = false, want true (a member answered at least once)")
	}
	if calls <= 1 {
		t.Errorf("groupKillFunc call count = %d, want > 1 (the wait must resend, not signal once)", calls)
	}
	if overrun := elapsed - groupDrainBound; overrun > 10*groupDrainPollInterval {
		t.Errorf("killProcessGroupReportingLeftover() took %v, %v over the %v drain bound, want at most about one poll interval (%v) over", elapsed, overrun, groupDrainBound, groupDrainPollInterval)
	}
}

// TestStartReaper_DoneWaitsForGroupDrain pins the user-visible half of the
// defect: StartReaper's Done must not close, and therefore a launch's
// outcome must not be published, while killProcessGroupReportingLeftover
// is still resending because a group member has not yet confirmed gone.
// The direct child here exits almost immediately, so any premature close
// of Done would come from not waiting on the group drain.
//
// groupKillFunc is mutated, so this test does not run in parallel with the
// package's other parallel tests.
func TestStartReaper_DoneWaitsForGroupDrain(t *testing.T) {
	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	origKill := groupKillFunc
	t.Cleanup(func() { groupKillFunc = origKill })
	unlock := make(chan struct{})
	var calls int
	groupKillFunc = func(int, syscall.Signal) error {
		calls++
		if calls < 3 {
			return nil
		}
		<-unlock
		return syscall.ESRCH
	}

	r := StartReaper(cmd, nil)

	select {
	case <-r.Done():
		t.Fatal("Done() closed before the process group drain was allowed to finish")
	case <-time.After(200 * time.Millisecond):
	}

	close(unlock)

	select {
	case <-r.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() did not close after the process group drain was allowed to finish")
	}
	if calls < 3 {
		t.Errorf("groupKillFunc call count = %d, want >= 3 (the drain must resend before Done closes)", calls)
	}
}

// blockingWarnHandler wraps a [captureLogSpy] and blocks inside Handle
// for the one record whose message equals msg, signaling hit once it
// has entered that block. A test uses hit to know the record has been
// handed to the logger, then release to let the call return, so it can
// observe that [Reaper.Done] is still open while StartReaper's log call
// is in flight and only closes once that call has returned.
type blockingWarnHandler struct {
	inner   *captureLogSpy
	msg     string
	hit     chan struct{}
	release chan struct{}
}

func (h *blockingWarnHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockingWarnHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if r.Message == h.msg {
		close(h.hit)
		<-h.release
	}
	return err
}

func (h *blockingWarnHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingWarnHandler) WithGroup(string) slog.Handler      { return h }

// TestStartReaper_CleanupFailureLogsOneRecordBeforeDoneCloses pins the
// fix's core claim: a reap whose group termination cannot prove the
// process tree gone logs exactly one CaptureCleanupWarning record,
// carrying the command and error attributes, and that record is written
// before Done closes rather than after.
//
// groupKillFunc and groupDrainBound are mutated, so this test does not
// run in parallel with the package's other parallel tests.
func TestStartReaper_CleanupFailureLogsOneRecordBeforeDoneCloses(t *testing.T) {
	origBound, origKill := groupDrainBound, groupKillFunc
	t.Cleanup(func() { groupDrainBound, groupKillFunc = origBound, origKill })
	groupDrainBound = 100 * time.Millisecond
	groupKillFunc = func(int, syscall.Signal) error { return nil }

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	spy := &captureLogSpy{}
	handler := &blockingWarnHandler{inner: spy, msg: CaptureCleanupWarning, hit: make(chan struct{}), release: make(chan struct{})}
	logger := slog.New(handler)

	r := StartReaper(cmd, logger)

	select {
	case <-handler.hit:
	case <-time.After(3 * time.Second):
		t.Fatal("CaptureCleanupWarning was not logged within 3s")
	}

	select {
	case <-r.Done():
		t.Fatal("Done() closed before the blocked log call returned, want the record written first")
	default:
	}

	close(handler.release)

	select {
	case <-r.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() did not close after the log call was allowed to return")
	}

	var matches []captureLogRecord
	for _, rec := range spy.snapshot() {
		if rec.Msg == CaptureCleanupWarning {
			matches = append(matches, rec)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("CaptureCleanupWarning logged %d times, want exactly 1", len(matches))
	}
	rec := matches[0]
	if got := len(rec.Attrs); got != 2 {
		t.Errorf("record carries %d attributes, want exactly 2 (command, error); got %v", got, rec.Attrs)
	}
	if _, ok := rec.Attrs["command"]; !ok {
		t.Error("record missing the command attribute")
	}
	if _, ok := rec.Attrs["error"]; !ok {
		t.Error("record missing the error attribute")
	}
}

// TestStartReaper_NilLoggerLogsThroughDefault pins StartReaper's nil
// fallback: a reap started with a nil logger neither panics nor loses
// the CaptureCleanupWarning record, which lands on slog.Default().
//
// groupKillFunc and groupDrainBound are mutated and slog.Default() is
// replaced, so this test does not run in parallel with the package's
// other parallel tests.
func TestStartReaper_NilLoggerLogsThroughDefault(t *testing.T) {
	origBound, origKill := groupDrainBound, groupKillFunc
	t.Cleanup(func() { groupDrainBound, groupKillFunc = origBound, origKill })
	groupDrainBound = 100 * time.Millisecond
	groupKillFunc = func(int, syscall.Signal) error { return nil }

	spy := &captureLogSpy{}
	origDefault := slog.Default()
	slog.SetDefault(slog.New(spy))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	r := StartReaper(cmd, nil)

	select {
	case <-r.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() did not close within 3s")
	}

	record, ok := findCaptureLogRecord(spy, CaptureCleanupWarning)
	if !ok {
		t.Fatal("a nil logger lost the CaptureCleanupWarning record, want it logged through slog.Default()")
	}
	if got := len(record.Attrs); got != 2 {
		t.Errorf("record carries %d attributes, want exactly 2 (command, error); got %v", got, record.Attrs)
	}
}
