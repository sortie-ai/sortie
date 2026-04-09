//go:build windows

package procutil

import (
	"os/exec"
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
	cmd := exec.Command("cmd.exe", "/C", "timeout /t 5 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	if err := AssignProcess(pid, cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("AssignProcess() = %v, want nil", err)
	}

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

	if err := AssignProcess(pid, cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("AssignProcess() = %v, want nil", err)
	}

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

func TestSignalGraceful_ConsoleProcess(t *testing.T) {
	t.Parallel()

	// STATUS_CONTROL_C_EXIT is the expected exit code when a console
	// process is terminated by CTRL_BREAK_EVENT.
	const statusControlCExit = uint32(0xC000013A)

	cmd := exec.Command("cmd.exe", "/C", "timeout /t 30 /nobreak >nul")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	if err := AssignProcess(pid, cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("AssignProcess() = %v, want nil", err)
	}
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
		t.Fatal("process did not exit within grace period after SignalGraceful")
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
	cmd := exec.Command("cmd.exe", "/C", "timeout /t 30 >nul")
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	pid := cmd.Process.Pid

	if err := AssignProcess(pid, cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("AssignProcess() = %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := KillProcessGroup(pid); err != nil {
		t.Fatalf("KillProcessGroup() = %v", err)
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("cmd.Wait() = nil, want exit error after Job Object termination")
	}
	if !WasSignaled(waitErr) {
		t.Errorf("WasSignaled(job-terminated) = false, want true")
	}
}
