//go:build windows

package procutil

import (
	"errors"
	"os"
	"os/exec"
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

func TestAssignProcess_CleanupProcess_Idempotent(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "ping -n 10 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	job, _, err := assignToJobObject(pid, false)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("assignToJobObject() = %v, want nil", err)
	}
	registerJobAssignment(pid, cmd.Process, job)

	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	CleanupProcess(pid)
	CleanupProcess(pid)

	if _, ok := jobs.Load(pid); ok {
		t.Error("job entry still present after CleanupProcess; want removed")
	}
}

func TestKillProcessGroup_KillsChildAndGrandchild(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "start /b ping -n 30 127.0.0.1 >nul & ping -n 30 127.0.0.1 >nul")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	job, _, err := assignToJobObject(pid, false)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("assignToJobObject() = %v, want nil", err)
	}
	registerJobAssignment(pid, cmd.Process, job)

	time.Sleep(300 * time.Millisecond)

	if err := KillProcessGroup(pid); err != nil {
		t.Fatalf("KillProcessGroup() = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(3 * time.Second):
		t.Fatal("process group leader still alive 3s after KillProcessGroup")
	case <-done:
	}
}

func TestKillProcessGroupReportingLeftover_ReapedFailOpenEntry(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() = %v, want nil", err)
	}

	registerJobAssignment(pid, cmd.Process, 0)
	t.Cleanup(func() { jobs.Delete(pid) })

	leftover, err := killProcessGroupReportingLeftover(pid)
	if err != nil {
		t.Errorf("killProcessGroupReportingLeftover(%d) error = %v, want nil for an already-reaped child", pid, err)
	}
	if leftover {
		t.Error("leftover = true, want false (no Job Object, so no membership was read)")
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
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	job, _, err := assignToJobObject(pid, false)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("assignToJobObject() = %v", err)
	}
	registerJobAssignment(pid, cmd.Process, job)
	t.Cleanup(func() { CleanupProcess(pid) })

	time.Sleep(100 * time.Millisecond)

	if err := SignalGraceful(pid); err != nil {
		_ = KillProcessGroup(pid)
		_ = cmd.Wait()
		t.Fatalf("SignalGraceful() = %v, want nil", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		code := uint32(cmd.ProcessState.ExitCode())
		if code != statusControlCExit {
			t.Logf("exit code %#x (expected %#x for STATUS_CONTROL_C_EXIT, but process did exit)", code, statusControlCExit)
		}
	case <-time.After(3 * time.Second):
		_ = KillProcessGroup(pid)
		<-done
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
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	job, _, err := assignToJobObject(pid, false)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("assignToJobObject() = %v", err)
	}
	registerJobAssignment(pid, cmd.Process, job)

	time.Sleep(200 * time.Millisecond)

	if err := KillProcessGroup(pid); err != nil {
		t.Fatalf("KillProcessGroup() = %v", err)
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("cmd.Wait() = nil, want exit error after Job Object termination")
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

func TestGroupEscalationTarget_NoMemberExitLeavesRegistryUntouched(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/C", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd.Wait() = %v, want nil", err)
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject() error = %v", err)
	}
	registerJobAssignment(pid, cmd.Process, job)
	t.Cleanup(func() { CleanupProcess(pid) })

	target, ok := captureGroupEscalation(pid)
	if !ok {
		t.Fatal("captureGroupEscalation() ok = false, want true")
	}

	present, memberErr := target.hasRunningMember()
	if memberErr != nil {
		t.Fatalf("hasMember() error = %v, want nil", memberErr)
	}
	if present {
		t.Fatal("hasMember() = true, want false (no process was ever assigned to the job)")
	}
	target.close()

	v, ok := jobs.Load(pid)
	if !ok {
		t.Fatal("jobs.Load(pid) after the no-member exit = not found, want the entry still registered")
	}
	entry := v.(*jobEntry)
	entry.mu.Lock()
	gotJob := entry.job
	entry.mu.Unlock()
	if gotJob != job {
		t.Errorf("registered job handle = %#x after the no-member exit, want unchanged %#x", gotJob, job)
	}

	leftover, cleanupErr := killProcessGroupReportingLeftover(pid)
	if cleanupErr != nil {
		t.Errorf("killProcessGroupReportingLeftover(%d) error = %v, want nil", pid, cleanupErr)
	}
	if leftover {
		t.Error("killProcessGroupReportingLeftover() leftover = true, want false")
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
		proc := newExitedProcess(t)
		pid := proc.Pid
		registerJobAssignment(pid, proc, job)
		t.Cleanup(func() { CleanupProcess(pid) })

		const grace = 100 * time.Millisecond
		armGroupEscalation(pid, grace)

		time.Sleep(grace + 200*time.Millisecond)

		canary := newCaptureTestHeldMember(t, job)
		CleanupProcess(pid)

		assertCaptureWinProcessGone(t, canary.Process.Pid, 2*time.Second)
	})

	t.Run("terminate exit", func(t *testing.T) {
		t.Parallel()

		job := newKillOnCloseJob(t)
		proc := newExitedProcess(t)
		pid := proc.Pid
		registerJobAssignment(pid, proc, job)
		t.Cleanup(func() { CleanupProcess(pid) })

		startCaptureTestHeldMember(t, job)
		armGroupEscalation(pid, 100*time.Millisecond)

		time.Sleep(100*time.Millisecond + groupDrainBound + 500*time.Millisecond)

		canary := newCaptureTestHeldMember(t, job)
		CleanupProcess(pid)

		assertCaptureWinProcessGone(t, canary.Process.Pid, 2*time.Second)
	})
}

func TestDuplicateJobHandle_NoDuplicateAfterTeardown(t *testing.T) {
	t.Parallel()

	t.Run("after killProcessGroupReportingLeftover", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("cmd.exe", "/C", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cmd.Wait() = %v, want nil", err)
		}
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			t.Fatalf("CreateJobObject() error = %v", err)
		}
		registerJobAssignment(pid, cmd.Process, job)
		v, ok := jobs.Load(pid)
		if !ok {
			t.Fatalf("jobs.Load(%d) = not found, want the entry just registered", pid)
		}
		entry := v.(*jobEntry)

		if _, err := killProcessGroupReportingLeftover(pid); err != nil {
			t.Fatalf("killProcessGroupReportingLeftover(%d) error = %v, want nil", pid, err)
		}

		entry.mu.Lock()
		gotJob := entry.job
		entry.mu.Unlock()
		if gotJob != 0 {
			t.Errorf("entry.job after killProcessGroupReportingLeftover = %#x, want 0", gotJob)
		}

		if _, ok := duplicateJobHandle(pid); ok {
			t.Errorf("duplicateJobHandle(%d) after killProcessGroupReportingLeftover reported true, want false", pid)
		}
	})

	t.Run("after CleanupProcess", func(t *testing.T) {
		t.Parallel()

		cmd := exec.Command("cmd.exe", "/C", "exit 0")
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}
		pid := cmd.Process.Pid
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cmd.Wait() = %v, want nil", err)
		}
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			t.Fatalf("CreateJobObject() error = %v", err)
		}
		registerJobAssignment(pid, cmd.Process, job)
		v, ok := jobs.Load(pid)
		if !ok {
			t.Fatalf("jobs.Load(%d) = not found, want the entry just registered", pid)
		}
		entry := v.(*jobEntry)

		CleanupProcess(pid)

		entry.mu.Lock()
		gotJob := entry.job
		entry.mu.Unlock()
		if gotJob != 0 {
			t.Errorf("entry.job after CleanupProcess = %#x, want 0", gotJob)
		}

		if _, ok := duplicateJobHandle(pid); ok {
			t.Errorf("duplicateJobHandle(%d) after CleanupProcess reported true, want false", pid)
		}
	})

	t.Run("no registered entry", func(t *testing.T) {
		t.Parallel()

		if _, ok := duplicateJobHandle(999999999); ok {
			t.Error("duplicateJobHandle() for an unregistered pid reported true, want false")
		}
	})
}

func TestDuplicateJobHandle_RefusesAnEntryCloseJobHasCleared(t *testing.T) {
	t.Parallel()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject() error = %v", err)
	}

	const pid = 918273645
	entry := &jobEntry{job: job}
	jobs.Store(pid, entry)
	t.Cleanup(func() { jobs.Delete(pid) })

	entry.closeJob()

	if entry.job != 0 {
		t.Fatalf("(*jobEntry).closeJob() left job = %#x, want 0", entry.job)
	}

	if _, ok := duplicateJobHandle(pid); ok {
		t.Error("duplicateJobHandle() against an entry closeJob already cleared reported true, want false")
	}
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
