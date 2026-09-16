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

	// Use a process that stays alive long enough for AssignProcess to succeed.
	// ping reliably blocks in non-interactive environments.
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

	// Kill the process so the test doesn't wait 5 seconds.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// First CleanupProcess removes the job entry.
	CleanupProcess(pid)

	// Second CleanupProcess must be a no-op (no panic, no double-close).
	CleanupProcess(pid)

	// The job entry must be absent.
	if _, ok := jobs.Load(pid); ok {
		t.Error("job entry still present after CleanupProcess; want removed")
	}
}

func TestKillProcessGroup_KillsChildAndGrandchild(t *testing.T) {
	t.Parallel()

	// Spawn cmd.exe that runs a background child via "start /b".
	// The Job Object with KILL_ON_JOB_CLOSE terminates all descendants.
	// Use ping instead of pause because pause exits immediately when
	// stdin is closed (the Go exec default).
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

	// Allow the child process tree to spawn.
	time.Sleep(300 * time.Millisecond)

	if err := KillProcessGroup(pid); err != nil {
		t.Fatalf("KillProcessGroup() = %v, want nil", err)
	}

	// The process group leader must exit promptly after Job Object termination.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(3 * time.Second):
		t.Fatal("process group leader still alive 3s after KillProcessGroup")
	case <-done:
		// Process exited as expected.
	}
}

// TestKillProcessGroupReportingLeftover_ReapedFailOpenEntry pins that a
// launch registered without a Job Object reports no cleanup error once the
// reap has already waited for its direct child. That is the order the reap
// runs in, so os.Process.Kill answers os.ErrProcessDone for it, and
// reporting that would raise the cleanup warning on every launch that ran
// without a Job Object and exited cleanly.
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

	// A zero handle is what startAndAssign registers when Job Object
	// creation or assignment failed and the launch ran without one.
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

// TestProcessAlreadyGone pins which kill outcomes count as the process
// having been gone. A successful Wait on Windows releases the handle
// rather than marking the process done, so Kill answers with a bare
// EINVAL there and never with ErrProcessDone; a kill that reached a
// live process and failed must still be reported.
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

	// STATUS_CONTROL_C_EXIT is the expected exit code when a console
	// process is terminated by CTRL_BREAK_EVENT.
	const statusControlCExit = uint32(0xC000013A)

	// Launch ping directly (not via cmd.exe) so that ping.exe IS the
	// process group leader and receives CTRL_BREAK_EVENT without
	// cmd.exe intercepting or swallowing the signal.
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

	// Allow the process to initialize its console.
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
		// Process responded to CTRL_BREAK_EVENT; accept any exit code.
		code := uint32(cmd.ProcessState.ExitCode())
		if code != statusControlCExit {
			t.Logf("exit code %#x (expected %#x for STATUS_CONTROL_C_EXIT, but process did exit)", code, statusControlCExit)
		}
	case <-time.After(3 * time.Second):
		_ = KillProcessGroup(pid)
		<-done
		// CTRL_BREAK_EVENT delivery requires an attached console.
		// GitHub Actions Windows runners may run headless, so the
		// event is not delivered. Skip rather than fail.
		t.Skip("CTRL_BREAK_EVENT not delivered; likely a headless CI session without an attached console")
	}
}

func TestWasSignaled_NormalExit1_NotSignaled(t *testing.T) {
	t.Parallel()

	// A normal "exit 1" must NOT be classified as signaled.
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

	// A process killed via KillProcessGroup (Job Object with
	// STATUS_CONTROL_C_EXIT exit code) must be classified as signaled.
	// Use ping as a long-running process that reliably blocks in
	// non-interactive environments.
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

// TestDrainJobObject pins that drainJobObject resends job termination on
// every poll, the same reason TestKillProcessGroupReportingLeftover_ResendsUntilGone
// resends on Unix, and reports a non-nil error once groupDrainBound
// elapses with a member jobHasRunningMember still finds live, rather than
// returning as soon as the first termination call was accepted.
//
// Cannot run on this host; on the CI Windows job this reddens under a
// mutation that makes drainJobObject return right after the first
// terminateJobObjectFunc call succeeds, without checking
// jobHasRunningMember or looping: the "resends" subtest's call count
// would drop to 1 and its nil-error check would fail, since the held
// member newCaptureTestHeldMember started is still running.
//
// groupDrainBound and terminateJobObjectFunc are mutated, so this test
// does not run in parallel with the package's other parallel tests.
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

// TestDrainJobObject_UnreadableMemberList pins that a job whose member
// list cannot be read is reported as unconfirmed rather than drained.
// jobHasRunningMember answers false both for a job it read and found
// empty and for one it could not read at all, and letting the second
// end the drain would publish a launch's outcome with its tree
// unproven, which is the guarantee the resend loop exists to make.
//
// An invalid job handle is what makes QueryInformationJobObject fail
// deterministically; terminateJobObjectFunc is stubbed so the failure
// under test is the member-list read rather than the termination.
//
// Cannot run on this host; on the CI Windows job this reddens under a
// mutation that drops jobHasRunningMember's error, because drainJobObject
// then returns nil on its first poll instead of reporting the bound.
//
// groupDrainBound and terminateJobObjectFunc are mutated, so this test
// does not run in parallel with the package's other parallel tests.
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

// TestGroupEscalationTarget_NoMemberExitLeavesRegistryUntouched pins
// the reap's inputs staying untouched by an escalation that observes
// no member: it reads only its own duplicate handle. jobs.Load(pid)
// still returns the entry, its job field still holds the handle
// registered for the launch, and killProcessGroupReportingLeftover's
// own LoadAndDelete therefore still finds it and drains the job state
// it reads today. A terminate arm races the reap's own member read
// instead, so no leftover-value assertion belongs there and none is
// added here. The escalation's own close of its duplicate is pinned
// separately, by TestArmGroupEscalation_ReleasesDuplicateOnEveryExitPath.
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

	present, memberErr := target.hasMember()
	if memberErr != nil {
		t.Fatalf("hasMember() error = %v, want nil", memberErr)
	}
	if present {
		t.Fatal("hasMember() = true, want false (no process was ever assigned to the job)")
	}
	target.release()

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

// newKillOnCloseJob creates a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, for ownership transfer into the
// jobs registry: the caller closes it only through a production
// closer, never through a cleanup of its own.
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

// newExitedProcess starts and waits for a trivial command, returning
// its *os.Process for use as a jobs registry key that names no
// process of its own still running.
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

// TestArmGroupEscalation_ReleasesDuplicateOnEveryExitPath pins the
// escalation's release guarantee by observing its effect rather than
// calling release itself: a job carries
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so once every handle referencing
// it has closed, Windows kills whatever member the job still holds.
// Each subtest arms the real
// armGroupEscalation, adds a canary member to the job once the
// escalation has had the chance to reach its own exit, and then closes
// the jobs registry's own handle through CleanupProcess: if the
// escalation released its duplicate, that close is the job's last
// handle and the canary dies; if the duplicate leaked, the canary
// survives.
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

		// The job holds no member for the whole grace window, so the
		// escalation's final check finds none and it has already called
		// release before this wait returns. Adding the canary only now,
		// rather than racing the escalation's own poll, is what keeps
		// this subtest pinned to the no-member exit instead of drifting
		// into the terminate exit the sibling subtest already covers:
		// terminate would kill the canary directly through
		// terminateJobObjectFunc, which would pass regardless of
		// whether release ran.
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

		// The original member above is terminated as part of the drain
		// itself, so it proves nothing about release; the canary below
		// is added only once the drain has had time to finish, and it
		// tests release the same way the no-member subtest does.
		time.Sleep(100*time.Millisecond + groupDrainBound + 500*time.Millisecond)

		canary := newCaptureTestHeldMember(t, job)
		CleanupProcess(pid)

		assertCaptureWinProcessGone(t, canary.Process.Pid, 2*time.Second)
	})
}

// TestDuplicateJobHandle_NoDuplicateAfterTeardown pins the guarantee
// against both production closers: a jobs entry whose handle a
// releaser has closed must not yield a duplicate. The check is
// deterministic, resting on each closer having already returned rather
// than on the race detector, which the Windows CI job does not run.
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

		// Read from the entry pointer captured before the closer ran,
		// the way a concurrent reader would have it, rather than from a
		// fresh jobs.Load the closer's own LoadAndDelete has already
		// emptied: a closer that skipped closeJob and closed the handle
		// directly would leave this field non-zero.
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

		// Read from the entry pointer captured before the closer ran,
		// the way a concurrent reader would have it, rather than from a
		// fresh jobs.Load the closer's own LoadAndDelete has already
		// emptied: a closer that skipped closeJob and closed the handle
		// directly would leave this field non-zero.
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

// TestDuplicateJobHandle_RefusesAnEntryCloseJobHasCleared pins the
// same guarantee directly, deterministically and without a concurrent
// closer: an entry still present in the registry, whose
// (*jobEntry).closeJob has already run, must not yield a duplicate. A
// closeJob that left entry.job non-zero after closing is caught by the
// first assertion below; duplicateJobHandle's own answer is then
// checked against that exact state, independent of whichever
// production caller reaches closeJob.
func TestDuplicateJobHandle_RefusesAnEntryCloseJobHasCleared(t *testing.T) {
	t.Parallel()

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("CreateJobObject() error = %v", err)
	}

	const pid = 918273645 // arbitrary; this entry is never a real registered launch
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

// armEscalationForceSendProbe installs a probe on
// terminateJobObjectFunc, escalation_test.go's platform-neutral hook
// into the force-termination seam: every call it counts is a genuine
// termination attempt, since every production caller (drainJobObject,
// runJobDrain) only ever calls it from a drain. Delegates every call
// to the original terminateJobObjectFunc so real termination keeps
// working. Restored on cleanup.
//
// Unlike the Unix probe, this one cannot filter by pid: the seam
// carries only a Job Object handle, so a dangling escalation or reap
// left running by an earlier, unrelated test in this same package
// could still be counted against a job it does not own.
//
// The armed escalation runs on its own goroutine with no completion
// signal, so drainUntilQuiescent gives the caller a happens-before
// edge into its reads instead of a sleep, which orders nothing in the
// Go memory model: it blocks on notify until floor has passed and
// then no call arrives for quiescence, or until budget elapses. Call
// it, and only then read the count, before the test returns and
// cleanup restores terminateJobObjectFunc.
func armEscalationForceSendProbe(t *testing.T, _ int) (calls *atomic.Int32, drainUntilQuiescent func(quiescence time.Duration, floor time.Time, budget time.Duration)) {
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

	drainUntilQuiescent = func(quiescence time.Duration, floor time.Time, budget time.Duration) {
		t.Helper()
		deadline := time.Now().Add(budget)
		for {
			wait := quiescence
			if untilFloor := time.Until(floor); untilFloor > wait {
				wait = untilFloor
			}
			select {
			case <-notify:
				if !time.Now().Before(deadline) {
					return
				}
			case <-time.After(wait):
				if time.Now().Before(floor) {
					continue
				}
				return
			}
		}
	}
	return calls, drainUntilQuiescent
}
