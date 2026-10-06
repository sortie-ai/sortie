//go:build windows

package procutil

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func failJobAssignment(t *testing.T) {
	t.Helper()
	orig := assignToJobObjectFunc
	t.Cleanup(func() { assignToJobObjectFunc = orig })
	assignToJobObjectFunc = func(int, bool) (windows.Handle, windows.Handle, error) {
		return 0, 0, errors.New("injected assignment failure")
	}
}

type heldLaunch struct {
	cmd           *exec.Cmd
	group         *Group
	cancel        context.CancelFunc
	descendantPID int
}

func startHeldLaunch(t *testing.T, grace time.Duration) *heldLaunch {
	t.Helper()
	dir := t.TempDir()
	childPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	leaderPath := agenttest.FakeRuntime(t, dir, "leader", "procutil.capture-win-leader", captureWinLeaderParams{
		ChildPath:    childPath,
		ChildPIDPath: pidPath,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	SetGroupCancel(cmd, grace)
	_, g := startOwned(t, cmd)
	return &heldLaunch{
		cmd:           cmd,
		group:         g,
		cancel:        cancel,
		descendantPID: pollCaptureWinPIDFile(t, pidPath, 5*time.Second),
	}
}

func TestReap_ReleasesRecordAndZeroesJobHandle(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 0")
	_, g := startOwned(t, cmd)

	r := StartReaper(cmd, nil)
	awaitDone(t, r, 5*time.Second)

	if got := lookupGroup(cmd); got != nil {
		t.Errorf("lookupGroup(cmd) after Done = %p, want nil", got)
	}
	g.mu.Lock()
	released, job := g.released, g.job
	g.mu.Unlock()
	if !released || job != 0 {
		t.Errorf("record after Done: released = %t, job = %#x, want true and 0", released, job)
	}
	if _, ok := g.captureEscalation(); ok {
		t.Error("captureEscalation() after Done ok = true, want false")
	}
}

func TestReapWithoutJobObjectReportsNoLeftover(t *testing.T) {
	failJobAssignment(t)
	cmd := fakeRuntimeCmd(t, agenttest.Output{})
	startOwned(t, cmd)

	r := StartReaper(cmd, slog.New(slog.DiscardHandler))
	awaitDone(t, r, 5*time.Second)

	if r.Leftover() {
		t.Error("Leftover() = true, want false (no Job Object, so nothing further is reachable)")
	}
	if err := r.CleanupErr(); err != nil {
		t.Errorf("CleanupErr() = %v, want nil", err)
	}
}

func TestKillWithoutJobObjectReachesDirectChild(t *testing.T) {
	failJobAssignment(t)
	cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
	_, g := startOwned(t, cmd)
	r := StartReaper(cmd, slog.New(slog.DiscardHandler))

	if err := g.Kill(); err != nil {
		t.Fatalf("Kill() without a Job Object = %v, want nil", err)
	}

	awaitDone(t, r, 5*time.Second)
	if err := g.Kill(); err != nil {
		t.Errorf("Kill() after the reap = %v, want nil (os.ErrProcessDone maps to nil)", err)
	}
	if err := killDirectChild(g); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("killDirectChild() after the reap = %v, want %v", err, os.ErrProcessDone)
	}
}

func TestWindowsReleasedRecordMakesNoSend(t *testing.T) {
	tests := []struct {
		name    string
		run     func(g *Group, cmd *exec.Cmd) error
		wantErr error
	}{
		{
			name: "SignalGraceful after os/exec released its handle",
			run:  func(g *Group, _ *exec.Cmd) error { return g.SignalGraceful() },
		},
		{
			name: "Kill",
			run:  func(g *Group, _ *exec.Cmd) error { return g.Kill() },
		},
		{
			name: "SetGroupCancel closure",
			run: func(_ *Group, cmd *exec.Cmd) error {
				SetGroupCancel(cmd, time.Second)
				return cmd.Cancel()
			},
		},
		{
			name: "SetGroupKill closure",
			run: func(_ *Group, cmd *exec.Cmd) error {
				SetGroupKill(cmd)
				return cmd.Cancel()
			},
			wantErr: os.ErrProcessDone,
		},
		{
			name: "armed escalation",
			run: func(g *Group, _ *exec.Cmd) error {
				armGroupEscalation(g, time.Second)
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "cmd.exe", "/C", "exit 0") //nolint:gosec // fixed literal script
			_, g := startOwned(t, cmd)
			r := StartReaper(cmd, nil)
			awaitDone(t, r, 5*time.Second)
			groups.Store(cmd, g)
			t.Cleanup(func() { groups.Delete(cmd) })
			terminations, _ := armEscalationForceSendProbe(t, 0)

			err := tt.run(g, cmd)

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("%s on a released record = %v, want %v", tt.name, err, tt.wantErr)
			}
			if got := terminations.Load(); got != 0 {
				t.Errorf("%s on a released record made %d Job Object terminations, want 0", tt.name, got)
			}
		})
	}
}

func TestWindowsEscalationArmedBeforeReleaseUsesOwnDuplicate(t *testing.T) {
	job := newKillOnCloseJob(t)
	g := newGroup(&exec.Cmd{Process: newExitedProcess(t)})
	g.finishAssignment(job)
	member := newCaptureTestHeldMember(t, job)
	target, ok := g.captureEscalation()
	if !ok {
		t.Fatal("captureEscalation() ok = false, want true")
	}
	t.Cleanup(target.close)
	terminations, _ := armEscalationForceSendProbe(t, 0)

	g.mu.Lock()
	g.released = true
	_ = windows.CloseHandle(g.job)
	g.job = 0
	g.mu.Unlock()

	if err := g.Kill(); err != nil {
		t.Errorf("Kill() after release = %v, want nil", err)
	}
	if got := terminations.Load(); got != 0 {
		t.Errorf("Kill() after release made %d Job Object terminations, want 0", got)
	}
	running, err := target.hasRunningMember()
	if err != nil || !running {
		t.Fatalf("hasRunningMember() through the duplicate = %t, %v, want true, nil (the job outlives the record's handle)", running, err)
	}
	if err := target.terminateAll(); err != nil {
		t.Errorf("terminateAll() through the duplicate = %v, want nil", err)
	}
	assertCaptureWinProcessGone(t, member.Process.Pid, 3*time.Second)
}

func TestTeardownLeavesSameIdentifierRecordUntouched(t *testing.T) {
	tests := []struct {
		name         string
		teardown     func(t *testing.T, l *heldLaunch, r *Reaper)
		wantLeftover bool
	}{
		{
			name:         "reap",
			teardown:     func(*testing.T, *heldLaunch, *Reaper) {},
			wantLeftover: true,
		},
		{
			name: "Kill",
			teardown: func(t *testing.T, l *heldLaunch, _ *Reaper) {
				if err := l.group.Kill(); err != nil {
					t.Errorf("Kill() = %v, want nil", err)
				}
			},
		},
		{
			name:     "Cancel closure",
			teardown: func(_ *testing.T, l *heldLaunch, _ *Reaper) { l.cancel() },
		},
		{
			name: "escalation",
			teardown: func(_ *testing.T, l *heldLaunch, _ *Reaper) {
				armGroupEscalation(l.group, 50*time.Millisecond)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := startHeldLaunch(t, 50*time.Millisecond)

			secondJob := newKillOnCloseJob(t)
			t.Cleanup(func() { _ = windows.CloseHandle(secondJob) })
			secondMember := newCaptureTestHeldMember(t, secondJob)
			second := newGroup(&exec.Cmd{Process: &os.Process{Pid: first.cmd.Process.Pid}})
			second.finishAssignment(secondJob)

			r := StartReaper(first.cmd, slog.New(slog.DiscardHandler))
			tt.teardown(t, first, r)
			awaitDone(t, r, 15*time.Second)

			assertCaptureWinProcessGone(t, first.descendantPID, 5*time.Second)
			if tt.wantLeftover && !r.Leftover() {
				t.Error("Leftover() = false, want true from the first launch's own job")
			}
			if !processIsRunning(uint32(secondMember.Process.Pid)) { //nolint:gosec // G115: a Windows PID fits in uint32
				t.Fatalf("the second record's member %d stopped during the first launch's %s, want it running", secondMember.Process.Pid, tt.name)
			}
			if running, err := jobHasRunningMember(secondJob); err != nil || !running {
				t.Errorf("jobHasRunningMember(second job) = %t, %v, want true, nil (its handle must stay usable)", running, err)
			}

			if err := second.Kill(); err != nil {
				t.Errorf("second record Kill() = %v, want nil", err)
			}
			assertCaptureWinProcessGone(t, secondMember.Process.Pid, 3*time.Second)
		})
	}
}

func awaitChildExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !directChildRunning(cmd) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d did not exit within 5s", cmd.Process.Pid)
}

func TestCancellationAfterOwnExitIsNotAStop(t *testing.T) {
	installs := []struct {
		name    string
		install func(*exec.Cmd)
	}{
		{name: "SetGroupCancel", install: func(cmd *exec.Cmd) { SetGroupCancel(cmd, 100*time.Millisecond) }},
		{name: "SetGroupKill", install: SetGroupKill},
	}

	for _, in := range installs {
		t.Run(in.name, func(t *testing.T) {
			cmd, p := newCancelProbe(t, in.install, "cmd.exe", "/C", "exit 0")
			startOwned(t, cmd)
			awaitChildExit(t, cmd)
			p.fire()

			r := StartReaper(cmd, slog.New(slog.DiscardHandler))
			awaitDone(t, r, 10*time.Second)

			if !p.observed.Load() {
				t.Fatal("the cancellation did not run before the reaper started, want it to have run")
			}
			if err := r.Err(); err != nil {
				t.Errorf("Err() = %v for a child that exited zero before the cancellation, want nil", err)
			}
			if r.Stopped() {
				t.Error("Stopped() = true for a cancellation after the child's own exit, want false")
			}
		})
	}
}
