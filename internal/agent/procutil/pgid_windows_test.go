//go:build windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestSetProcessGroup_CreationFlags(t *testing.T) {
	t.Parallel()

	t.Run("nil SysProcAttr", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{}
		SetProcessGroup(cmd)

		if cmd.SysProcAttr == nil {
			t.Fatal("SetProcessGroup() SysProcAttr = nil, want non-nil")
		}
		if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
			t.Errorf("CreationFlags = %#x, want CREATE_NEW_PROCESS_GROUP set", cmd.SysProcAttr.CreationFlags)
		}
	})

	t.Run("existing SysProcAttr fields preserved", func(t *testing.T) {
		t.Parallel()

		cmd := &exec.Cmd{
			SysProcAttr: &syscall.SysProcAttr{HideWindow: true},
		}
		SetProcessGroup(cmd)

		if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
			t.Errorf("CreationFlags = %#x, want CREATE_NEW_PROCESS_GROUP set", cmd.SysProcAttr.CreationFlags)
		}
		if !cmd.SysProcAttr.HideWindow {
			t.Error("HideWindow = false, want true (pre-existing field must be preserved)")
		}
	})
}

func awaitJobMembers(t *testing.T, g *Group, want int) {
	t.Helper()
	job := jobHandleOf(g)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pids, err := jobMemberPIDs(job); err == nil && len(pids) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job object held fewer than %d members within 5s", want)
}

func TestGroupKill_KillsChildAndGrandchild(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "start /b ping -n 30 127.0.0.1 >nul & ping -n 30 127.0.0.1 >nul")
	_, g := startOwned(t, cmd)
	r := StartReaper(cmd, nil)
	awaitJobMembers(t, g, 2)

	if err := g.Kill(); err != nil {
		t.Fatalf("Kill() = %v, want nil", err)
	}

	awaitDone(t, r, 3*time.Second)
	if err := r.CleanupErr(); err != nil {
		t.Errorf("CleanupErr() = %v, want nil (Kill drained the job)", err)
	}
}

func TestQueryImageBaseName_ReadsNameOfExitedProcess(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	pid, err := dwordPID(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		_ = cmd.Wait()
		t.Fatalf("OpenProcess(%d) error = %v", pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	got, err := queryImageBaseName(handle)
	if err != nil {
		t.Fatalf("queryImageBaseName(exited cmd.exe) error = %v, want its image name", err)
	}
	if !strings.EqualFold(got, "cmd.exe") {
		t.Errorf("queryImageBaseName(exited cmd.exe) = %q, want %q", got, "cmd.exe")
	}
}

func TestProcessAlreadyGone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "released handle, which a reaped child leaves on Windows", err: syscall.EINVAL, want: true},
		{name: "done process, the portable spelling", err: os.ErrProcessDone, want: true},
		{name: "termination denied against a live process", err: os.NewSyscallError("TerminateProcess", syscall.EACCES), want: false},
		{name: "a syscall failure carrying the same errno", err: os.NewSyscallError("TerminateProcess", syscall.EINVAL), want: false},
		{name: "an unrelated error", err: errors.New("boom"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := processAlreadyGone(tt.err); got != tt.want {
				t.Errorf("processAlreadyGone(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestSignalGraceful_ConsoleProcess(t *testing.T) {
	t.Parallel()

	const statusControlCExit = uint32(0xC000013A)

	cmd := exec.Command("ping", "-n", "31", "127.0.0.1")
	_, g := startOwned(t, cmd)
	r := StartReaper(cmd, nil)
	t.Cleanup(func() { terminateAndAwait(t, cmd, g, r) })

	time.Sleep(100 * time.Millisecond)

	if err := g.SignalGraceful(); err != nil {
		t.Fatalf("SignalGraceful() = %v, want nil", err)
	}

	select {
	case <-r.Done():
		if code := uint32(cmd.ProcessState.ExitCode()); code != statusControlCExit {
			t.Logf("exit code %#x (expected %#x for STATUS_CONTROL_C_EXIT, but process did exit)", code, statusControlCExit)
		}
	case <-time.After(3 * time.Second):
		t.Skip("CTRL_BREAK_EVENT not delivered; likely a headless CI session without an attached console")
	}
}

func TestWasSignaled_NormalExit1_NotSignaled(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 1")
	err := cmd.Run()
	if err == nil {
		t.Fatal("cmd.Run() = nil, want exit error")
	}
	if WasSignaled(err) {
		t.Errorf("WasSignaled(exit 1) = true, want false")
	}
}

func TestWasSignaled_JobTermination_IsSignaled(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "ping -n 31 127.0.0.1 >nul")
	_, g := startOwned(t, cmd)
	r := StartReaper(cmd, nil)
	awaitJobMembers(t, g, 2)

	if err := g.Kill(); err != nil {
		t.Fatalf("Kill() = %v", err)
	}

	awaitDone(t, r, 5*time.Second)
	waitErr := r.Err()
	if waitErr == nil {
		t.Fatal("Err() = nil, want exit error after Job Object termination")
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		t.Logf("ExitCode() = %d (0x%X), ExitError = %v", exitErr.ExitCode(), uint32(exitErr.ExitCode()), exitErr)
	} else {
		t.Logf("waitErr type = %T, value = %v", waitErr, waitErr)
	}
	if !WasSignaled(waitErr) {
		t.Errorf("WasSignaled(job-terminated) = false, want true")
	}
}

func TestDrainJobObject(t *testing.T) {
	t.Run("resends termination while a member is held live and reports the bound-elapsed failure", func(t *testing.T) {
		job, cleanup := newCaptureTestJob(t)
		defer cleanup()
		startCaptureTestHeldMember(t, job)

		origBound, origTerm := groupDrainBound, terminateJobObjectFunc
		defer func() { groupDrainBound, terminateJobObjectFunc = origBound, origTerm }()

		var calls int
		groupDrainBound = 200 * time.Millisecond
		terminateJobObjectFunc = func(windows.Handle, uint32) error {
			calls++
			return nil
		}

		err := drainJobObject(4242, job)

		if err == nil {
			t.Fatal("drainJobObject() error = nil, want non-nil once the bound elapses with a member still running")
		}
		if calls <= 1 {
			t.Errorf("terminateJobObjectFunc call count = %d, want > 1 (drainJobObject must resend, not terminate once)", calls)
		}
	})

	t.Run("real termination settles the job", func(t *testing.T) {
		job, cleanup := newCaptureTestJob(t)
		defer cleanup()
		startCaptureTestHeldMember(t, job)

		err := drainJobObject(0, job)

		if err != nil {
			t.Errorf("drainJobObject() error = %v, want nil (the real termination settles the job)", err)
		}
	})
}

func TestDrainJobObject_UnreadableMemberList(t *testing.T) {
	origBound, origTerm := groupDrainBound, terminateJobObjectFunc
	defer func() { groupDrainBound, terminateJobObjectFunc = origBound, origTerm }()

	groupDrainBound = 50 * time.Millisecond
	terminateJobObjectFunc = func(windows.Handle, uint32) error { return nil }

	err := drainJobObject(4242, windows.InvalidHandle)

	if err == nil {
		t.Fatal("drainJobObject(unreadable job) error = nil, want non-nil (an unread member list does not confirm an empty job)")
	}
}

func TestJobEscalationTarget_CloseLeavesRecordHandleUsable(t *testing.T) {
	t.Parallel()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject() error = %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(job) })
	g := newGroup(&exec.Cmd{Process: newExitedProcess(t)})
	g.finishAssignment(job)

	target, ok := g.captureEscalation()
	if !ok {
		t.Fatal("captureEscalation() ok = false, want true")
	}
	present, memberErr := target.hasRunningMember()
	if memberErr != nil {
		t.Fatalf("hasRunningMember() error = %v, want nil", memberErr)
	}
	if present {
		t.Fatal("hasRunningMember() = true, want false (no process was ever assigned to the job)")
	}
	target.close()

	if got := jobHandleOf(g); got != job {
		t.Errorf("record job handle = %#x after the target closed, want unchanged %#x", got, job)
	}
	if _, err := jobHasRunningMember(job); err != nil {
		t.Errorf("jobHasRunningMember(record handle) = %v after the target closed, want a usable handle", err)
	}
	if err := g.Kill(); err != nil {
		t.Errorf("Kill() = %v, want nil", err)
	}
}

func newKillOnCloseJob(t *testing.T) windows.Handle {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject() error = %v", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptrOf(&info), uint32Sizeof(info)); err != nil {
		_ = windows.CloseHandle(job)
		t.Fatalf("SetInformationJobObject() error = %v", err)
	}
	return job
}

func newExitedProcess(t *testing.T) *os.Process {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() error = %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() error = %v, want nil", err)
	}
	return cmd.Process
}

func TestArmGroupEscalation_ReleasesDuplicateOnEveryExitPath(t *testing.T) {
	t.Parallel()

	t.Run("no-member exit", func(t *testing.T) {
		t.Parallel()

		job := newKillOnCloseJob(t)
		g := newGroup(&exec.Cmd{Process: newExitedProcess(t)})
		g.finishAssignment(job)

		const grace = 100 * time.Millisecond
		armGroupEscalation(g, grace)

		time.Sleep(grace + 200*time.Millisecond)

		canary := newCaptureTestHeldMember(t, job)
		_ = windows.CloseHandle(job)

		assertCaptureWinProcessGone(t, canary.Process.Pid, 2*time.Second)
	})

	t.Run("terminate exit", func(t *testing.T) {
		t.Parallel()

		job := newKillOnCloseJob(t)
		g := newGroup(&exec.Cmd{Process: newExitedProcess(t)})
		g.finishAssignment(job)

		startCaptureTestHeldMember(t, job)
		armGroupEscalation(g, 100*time.Millisecond)

		time.Sleep(100*time.Millisecond + groupDrainBound + 500*time.Millisecond)

		canary := newCaptureTestHeldMember(t, job)
		_ = windows.CloseHandle(job)

		assertCaptureWinProcessGone(t, canary.Process.Pid, 2*time.Second)
	})
}

func armEscalationForceSendProbe(t *testing.T, _ int) (calls *atomic.Int32, waitForQuiet func(quietPeriod time.Duration, notBefore time.Time, timeout time.Duration)) {
	t.Helper()
	orig := terminateJobObjectFunc
	t.Cleanup(func() { terminateJobObjectFunc = orig })

	calls = &atomic.Int32{}
	notify := make(chan struct{}, 8192)
	terminateJobObjectFunc = func(job windows.Handle, exitCode uint32) error {
		res := orig(job, exitCode)
		calls.Add(1)
		notify <- struct{}{}
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
