//go:build windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// statusControlCExit is the Windows NTSTATUS code for
// STATUS_CONTROL_C_EXIT (0xC000013A). It is used as the exit code
// when terminating a Job Object so the outcome is distinguishable
// from normal non-zero exits.
const statusControlCExit uint32 = 0xC000013A

// RunHook executes a hook script via cmd.exe on Windows, enforcing a
// timeout and capturing output. The subprocess is placed in a Job
// Object with KILL_ON_JOB_CLOSE so that the entire process tree is
// terminated on timeout or context cancellation. Before returning,
// RunHook terminates any process remaining in the job and waits for
// the job to drain, so no descendant still holds a handle into the
// hook directory when the caller proceeds to clean it up.
//
// On success (exit code 0), returns a [HookResult] with truncated
// output. On failure, returns a [*HookError] with Op indicating the
// failure mode:
//   - "validate": invalid params
//   - "start": subprocess could not be started
//   - "run": subprocess exited with non-zero exit code
//   - "timeout": subprocess exceeded TimeoutMS or parent ctx cancelled
func RunHook(ctx context.Context, params HookParams) (HookResult, error) {
	if err := validateParams(params); err != nil {
		return HookResult{}, err
	}

	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "cmd.exe", "/C", params.Script) //nolint:gosec // G204: hook scripts are from trusted workflow configuration
	cmd.Dir = params.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Env = hookEnv(params.Env)

	buf := &limitedBuffer{max: MaxHookOutputBytes}
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Start(); err != nil {
		return HookResult{}, &HookError{
			Op:       "start",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      err,
		}
	}

	// Create a Job Object and assign the hook process so that all
	// descendants are terminated together on timeout.
	job, jobErr := createHookJobObject(cmd.Process.Pid)
	if jobErr != nil {
		slog.Warn("hook job object creation failed; child tree may survive timeout",
			slog.String("dir", params.Dir),
			slog.Int("timeout_ms", params.TimeoutMS),
			slog.Any("error", jobErr))
	}

	if job != 0 {
		cmd.Cancel = func() error {
			// On cancellation/timeout, actively terminate the tree.
			// The handle stays open so the post-Wait drain can observe
			// the job; the deferred close releases it on every path.
			return windows.TerminateJobObject(job, statusControlCExit)
		}
		// A failing CloseHandle means the handle was already invalid;
		// there is nothing the hook caller could do about it.
		defer func() { _ = windows.CloseHandle(job) }()
	}

	cmd.WaitDelay = 3 * time.Second

	err := cmd.Wait()
	output := buf.String()

	// Job process termination is asynchronous: TerminateJobObject and
	// KILL_ON_JOB_CLOSE both return before the dying processes release
	// their handles. A descendant still holding the hook directory as
	// its working directory makes the caller's directory cleanup fail
	// with a sharing violation, so terminate any survivors explicitly
	// and wait until the job reports zero active processes.
	if job != 0 {
		if termErr := windows.TerminateJobObject(job, statusControlCExit); termErr != nil {
			slog.Warn("hook job termination after wait failed; drain may not settle",
				slog.Any("error", termErr))
		}
		waitJobDrained(job)
	}

	if err == nil {
		return HookResult{Output: output}, nil
	}

	// Check context error BEFORE *exec.ExitError. A process killed by
	// Job Object termination also produces an ExitError. Checking
	// context first ensures correct classification.
	if hookCtx.Err() == context.DeadlineExceeded {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook timed out after %dms: %w", params.TimeoutMS, context.DeadlineExceeded),
		}
	}

	if hookCtx.Err() == context.Canceled {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook cancelled: %w", context.Canceled),
		}
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return HookResult{}, &HookError{
			Op:       "run",
			Script:   truncateScript(params.Script),
			ExitCode: exitErr.ExitCode(),
			Output:   output,
			Err:      err,
		}
	}

	return HookResult{}, &HookError{
		Op:       "start",
		Script:   truncateScript(params.Script),
		ExitCode: -1,
		Output:   output,
		Err:      err,
	}
}

// createHookJobObject creates an anonymous Job Object with
// KILL_ON_JOB_CLOSE and assigns the process identified by pid to it.
// Returns the Job Object handle on success; callers are responsible
// for closing it. Returns 0 on failure.
func createHookJobObject(pid int) (windows.Handle, error) {
	// Windows identifies a process by a 32-bit unsigned value while Go
	// reports a pid as an int, so a pid outside that range names no
	// live process. Narrowing one unchecked would wrap it onto an
	// unrelated identifier and put the wrong process in the job.
	if pid < 0 || pid > math.MaxUint32 {
		return 0, fmt.Errorf("process id %d is outside the windows process identifier range", pid)
	}
	processID := uint32(pid)

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: SetInformationJobObject takes the limit struct as a uintptr; x/sys/windows exposes no typed alternative
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		processID,
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("OpenProcess: %w", err)
	}

	err = windows.AssignProcessToJobObject(job, procHandle)
	_ = windows.CloseHandle(procHandle)
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	return job, nil
}

// jobObjectBasicAccountingInformation mirrors the Win32
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION layout consumed by
// QueryInformationJobObject; x/sys/windows does not define the type.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// waitJobDrained polls the job accounting counters until no process in
// the job remains active or the deadline expires. Termination initiated
// by TerminateJobObject completes asynchronously, so returning before
// the active count reaches zero would let a dying child briefly outlive
// RunHook with its working-directory handle still open.
func waitJobDrained(job windows.Handle) {
	const (
		pollInterval = 5 * time.Millisecond
		drainTimeout = 2 * time.Second
	)

	deadline := time.Now().Add(drainTimeout)
	for {
		var info jobObjectBasicAccountingInformation
		err := windows.QueryInformationJobObject(
			job,
			windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: QueryInformationJobObject takes the accounting struct as a uintptr; x/sys/windows exposes no typed alternative
			uint32(unsafe.Sizeof(info)),
			nil,
		)
		if err != nil {
			slog.Warn("hook job accounting query failed; drain skipped",
				slog.Any("error", err))
			return
		}
		if info.ActiveProcesses == 0 {
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("hook job processes still active after drain deadline",
				slog.Int64("active_processes", int64(info.ActiveProcesses)))
			return
		}
		time.Sleep(pollInterval)
	}
}
