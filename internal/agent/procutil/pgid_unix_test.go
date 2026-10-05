//go:build unix

package procutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestGroupSignalGraceful(t *testing.T) {
	tests := []struct {
		name    string
		sendErr error
		wantErr error
	}{
		{name: "delivered"},
		{name: "group already gone", sendErr: syscall.ESRCH},
		{name: "group not signalable", sendErr: syscall.EPERM},
		{name: "other failure", sendErr: syscall.EINVAL, wantErr: syscall.EINVAL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := unreapedRecord(t)
			var gotPIDs []int
			var gotSigs []syscall.Signal
			stubGroupKill(t, func(pid int, sig syscall.Signal) error {
				gotPIDs, gotSigs = append(gotPIDs, pid), append(gotSigs, sig)
				return tt.sendErr
			})

			err := g.SignalGraceful()

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("SignalGraceful() = %v, want %v", err, tt.wantErr)
			}
			if len(gotPIDs) != 1 || gotPIDs[0] != -g.cmd.Process.Pid || gotSigs[0] != syscall.SIGTERM {
				t.Errorf("SignalGraceful() sent %v %v, want one SIGTERM to %d", gotSigs, gotPIDs, -g.cmd.Process.Pid)
			}
		})
	}
}

func TestGroupKill(t *testing.T) {
	tests := []struct {
		name      string
		sends     []error
		wantCalls int
		wantErr   error
	}{
		{name: "group already gone", sends: []error{syscall.ESRCH}, wantCalls: 1},
		{name: "send failure is returned", sends: []error{syscall.EINVAL}, wantCalls: 1, wantErr: syscall.EINVAL},
		{name: "resends until the group is gone", sends: []error{nil, nil, syscall.ESRCH}, wantCalls: 3},
		{name: "EPERM keeps resending", sends: []error{syscall.EPERM, syscall.ESRCH}, wantCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := unreapedRecord(t)
			next := sequence(tt.sends...)
			var gotPIDs []int
			var gotSigs []syscall.Signal
			stubGroupKill(t, func(pid int, sig syscall.Signal) error {
				gotPIDs, gotSigs = append(gotPIDs, pid), append(gotSigs, sig)
				return next()
			})

			err := g.Kill()

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Kill() = %v, want %v", err, tt.wantErr)
			}
			if len(gotPIDs) != tt.wantCalls {
				t.Fatalf("Kill() made %d sends, want %d", len(gotPIDs), tt.wantCalls)
			}
			for i := range gotPIDs {
				if gotPIDs[i] != -g.cmd.Process.Pid || gotSigs[i] != syscall.SIGKILL {
					t.Errorf("Kill() send %d = signal %v to %d, want SIGKILL to %d", i, gotSigs[i], gotPIDs[i], -g.cmd.Process.Pid)
				}
			}
		})
	}
}

func TestKillBoundWhileUnreaped(t *testing.T) {
	g := unreapedRecord(t)
	shortenDrainBound(t, 200*time.Millisecond)
	var calls int
	stubGroupKill(t, func(int, syscall.Signal) error {
		calls++
		return syscall.EPERM
	})

	start := time.Now()
	err := g.Kill()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Kill() = nil, want the bound error while the direct child still holds the group")
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ESRCH) {
		t.Errorf("Kill() = %v, want the bound error, not the send's errno", err)
	}
	if calls <= 1 {
		t.Errorf("Kill() made %d sends, want more than 1 (it must resend, not send once)", calls)
	}
	if overrun := elapsed - groupDrainBound; overrun > 10*groupDrainPollInterval {
		t.Errorf("Kill() took %v, %v over the %v drain bound", elapsed, overrun, groupDrainBound)
	}
}

func TestKillReturnsNilOnceReleased(t *testing.T) {
	g := unreapedRecord(t)
	var calls int
	stubGroupKill(t, func(int, syscall.Signal) error {
		calls++
		g.released = true
		return nil
	})

	if err := g.Kill(); err != nil {
		t.Errorf("Kill() = %v, want nil once the reap has released the record", err)
	}
	if calls != 1 {
		t.Errorf("Kill() made %d sends, want 1 (none after release)", calls)
	}
}

func TestGroupSignalsRealProcess(t *testing.T) {
	t.Parallel()

	t.Run("SignalGraceful terminates a live launch", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
		_, g := startOwned(t, cmd)

		if err := g.SignalGraceful(); err != nil {
			_ = g.Kill()
			_ = cmd.Wait()
			t.Fatalf("SignalGraceful() = %v, want nil", err)
		}

		if err := cmd.Wait(); !WasSignaled(err) {
			t.Errorf("WasSignaled(cmd.Wait()) = false for %v, want true (terminated by SIGTERM)", err)
		}
	})

	t.Run("a group that is already gone is success", func(t *testing.T) {
		t.Parallel()

		cmd := fakeRuntimeCmd(t, agenttest.Output{})
		_, g := startOwned(t, cmd)
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cmd.Wait() = %v, want nil", err)
		}

		if err := g.SignalGraceful(); err != nil {
			t.Errorf("SignalGraceful() on an empty group = %v, want nil", err)
		}
		start := time.Now()
		if err := g.Kill(); err != nil {
			t.Errorf("Kill() on an empty group = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed >= groupDrainBound/2 {
			t.Errorf("Kill() on an empty group took %v, want well under the %v drain bound", elapsed, groupDrainBound)
		}
	})
}

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

type groupLeaderParams struct {
	DescendantPath string
}

func runGroupLeader(_ []string, params groupLeaderParams) int {
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

type groupDescendantParams struct {
	Marker  string
	PIDFile string
}

func runGroupDescendant(_ []string, params groupDescendantParams) int {
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
	_, g := startOwned(t, cmd)
	reaper := StartReaper(cmd, nil)
	t.Cleanup(func() { terminateAndAwait(t, cmd, g, reaper) })

	descendantPID := pollForPID(t, pidFile, 5*time.Second)

	cancel()
	awaitDone(t, reaper, 10*time.Second)

	if !pollForFile(marker, 5*time.Second) {
		t.Errorf("SetGroupCancel(): cancelling left %q absent, want the descendant to have caught a graceful signal", marker)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil {
		t.Errorf("SetGroupCancel(): descendant %d still alive after cancellation, want gone", descendantPID)
	}
}

func TestStartReaper_DoneWaitsForGroupDrain(t *testing.T) {
	unlock := make(chan struct{})
	var calls atomic.Int32
	stubGroupKill(t, func(int, syscall.Signal) error {
		if calls.Add(1) < 3 {
			return nil
		}
		<-unlock
		return syscall.ESRCH
	})
	stubLiveMember(t, func(int, int) (bool, error) { return true, nil })
	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	startOwned(t, cmd)

	r := StartReaper(cmd, nil)

	select {
	case <-r.Done():
		t.Fatal("Done() closed before the process group drain was allowed to finish")
	case <-time.After(200 * time.Millisecond):
	}

	close(unlock)
	awaitDone(t, r, 3*time.Second)

	if got := calls.Load(); got < 3 {
		t.Errorf("group send count = %d, want >= 3 (the drain must resend before Done closes)", got)
	}
}

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

func TestStartReaper_CleanupFailureLogsOneRecordBeforeDoneCloses(t *testing.T) {
	shortenDrainBound(t, 100*time.Millisecond)
	stubGroupKill(t, func(int, syscall.Signal) error { return nil })
	stubLiveMember(t, func(int, int) (bool, error) { return true, nil })

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	startOwned(t, cmd)

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

func TestStartReaper_NilLoggerLogsThroughDefault(t *testing.T) {
	shortenDrainBound(t, 100*time.Millisecond)
	stubGroupKill(t, func(int, syscall.Signal) error { return nil })
	stubLiveMember(t, func(int, int) (bool, error) { return true, nil })

	spy := &captureLogSpy{}
	origDefault := slog.Default()
	slog.SetDefault(slog.New(spy))
	t.Cleanup(func() { slog.SetDefault(origDefault) })

	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	startOwned(t, cmd)

	r := StartReaper(cmd, nil)
	awaitDone(t, r, 3*time.Second)

	record, ok := findCaptureLogRecord(spy, CaptureCleanupWarning)
	if !ok {
		t.Fatal("a nil logger lost the CaptureCleanupWarning record, want it logged through slog.Default()")
	}
	if got := len(record.Attrs); got != 2 {
		t.Errorf("record carries %d attributes, want exactly 2 (command, error); got %v", got, record.Attrs)
	}
}

func armEscalationForceSendProbe(t *testing.T, pid int) (calls *atomic.Int32, waitForQuiet func(quietPeriod time.Duration, notBefore time.Time, timeout time.Duration)) {
	t.Helper()
	orig := groupKillFunc
	t.Cleanup(func() { groupKillFunc = orig })

	want := -pid
	calls = &atomic.Int32{}
	notify := make(chan struct{}, 8192)
	groupKillFunc = func(gotPID int, sig syscall.Signal) error {
		res := orig(gotPID, sig)
		if gotPID == want {
			if sig == syscall.SIGKILL {
				calls.Add(1)
			}
			notify <- struct{}{}
		}
		return res
	}

	waitForQuiet = func(quietPeriod time.Duration, notBefore time.Time, timeout time.Duration) {
		t.Helper()
		timeoutAt := time.Now().Add(timeout)
		for {
			wait := quietPeriod
			if untilNotBefore := time.Until(notBefore); untilNotBefore > wait {
				wait = untilNotBefore
			}
			select {
			case <-notify:
				if !time.Now().Before(timeoutAt) {
					return
				}
			case <-time.After(wait):
				if time.Now().Before(notBefore) {
					continue
				}
				return
			}
		}
	}
	return calls, waitForQuiet
}
