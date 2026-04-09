//go:build windows

package procutil

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobEntry holds the Windows Job Object handle associated with a
// process.
type jobEntry struct {
	job windows.Handle
}

// jobs maps PIDs to their Job Object entries. Concurrent access is
// safe via sync.Map lock-free reads and safe concurrent writes.
var jobs sync.Map // map[int]*jobEntry

// SetProcessGroup configures cmd to start in a new console process
// group. Must be called before [exec.Cmd.Start]. Any pre-existing
// [syscall.SysProcAttr] fields are preserved.
//
// CREATE_NEW_PROCESS_GROUP gives the child a new console process
// group ID equal to its PID, enabling GenerateConsoleCtrlEvent to
// target the tree precisely without affecting the orchestrator.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// SignalProcessGroup sends a signal to the process group led by pid.
//
// For SIGTERM and SIGINT, sends CTRL_BREAK_EVENT via
// GenerateConsoleCtrlEvent. CTRL_BREAK_EVENT is used instead of
// CTRL_C_EVENT because CTRL_C_EVENT with a non-zero pgid is
// unreliable. For SIGKILL, delegates to [KillProcessGroup]. Returns
// an error for unsupported signals.
func SignalProcessGroup(pid int, sig syscall.Signal) error {
	switch sig {
	case syscall.SIGTERM, syscall.SIGINT:
		return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	case syscall.SIGKILL:
		return KillProcessGroup(pid)
	default:
		return fmt.Errorf("unsupported signal %d on windows", sig)
	}
}

// SignalGraceful sends CTRL_BREAK_EVENT to the process group led by
// pid for graceful shutdown.
func SignalGraceful(pid int) error {
	return SignalProcessGroup(pid, syscall.SIGTERM)
}

// jobTerminateExitCode is the exit code passed to TerminateJobObject.
// STATUS_CONTROL_C_EXIT (0xC000013A) is used so that [WasSignaled]
// can distinguish "killed by us" from normal non-zero exits. Exit
// code 1 cannot be used because it is the most common legitimate
// failure code on Windows.
const jobTerminateExitCode uint32 = 0xC000013A

// KillProcessGroup terminates all processes in the Job Object
// associated with pid. If no Job Object is registered (degraded
// mode), falls back to killing the single process by PID.
//
// LoadAndDelete atomicity ensures exactly one concurrent caller gets
// the handle when RunTurn and StopSession race.
func KillProcessGroup(pid int) error {
	v, ok := jobs.LoadAndDelete(pid)
	if ok {
		entry := v.(*jobEntry)
		err := windows.TerminateJobObject(entry.job, jobTerminateExitCode)
		windows.CloseHandle(entry.job)
		return err
	}

	// Degraded mode: no Job Object registered. The process may have
	// exited before AssignProcess or AssignProcess itself failed. In
	// this mode, os.FindProcess always succeeds on Windows (does not
	// check liveness), so p.Kill() could target a reused PID. The
	// risk is acceptable because this path fires only in a degraded
	// configuration and adapters call KillProcessGroup immediately
	// after cmd.Wait().
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
}

// AssignProcess creates an anonymous Job Object with
// KILL_ON_JOB_CLOSE, opens a handle to the process, and assigns it
// to the Job Object. The Job Object ensures all child processes are
// terminated when the handle is closed, preventing orphans even if
// the orchestrator crashes.
//
// Must be called after [exec.Cmd.Start] and before any signal/kill
// operation. On failure, returns an error; callers should log at WARN
// and continue without Job Object protection.
func AssignProcess(pid int, _ *os.Process) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}

	processHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("OpenProcess: %w", err)
	}

	err = windows.AssignProcessToJobObject(job, processHandle)
	windows.CloseHandle(processHandle)
	if err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	jobs.Store(pid, &jobEntry{job: job})
	return nil
}

// CleanupProcess releases the Job Object handle associated with pid.
// Safe to call multiple times or with an unregistered PID.
func CleanupProcess(pid int) {
	v, ok := jobs.LoadAndDelete(pid)
	if ok {
		entry := v.(*jobEntry)
		windows.CloseHandle(entry.job)
	}
}
