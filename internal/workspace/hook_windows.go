//go:build windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// RunHook executes a hook script via cmd.exe on Windows, enforcing a
// timeout and capturing output. The subprocess is placed in a Job
// Object with KILL_ON_JOB_CLOSE so that the entire process tree is
// terminated on timeout or context cancellation.
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

	cmd := exec.CommandContext(hookCtx, "cmd.exe", "/C", params.Script)
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
		var closeJob sync.Once
		closeHandle := func() {
			closeJob.Do(func() { windows.CloseHandle(job) })
		}
		cmd.Cancel = func() error {
			// On cancellation/timeout, actively terminate the tree
			// before closing the handle.
			windows.TerminateJobObject(job, 0xC000013A)
			closeHandle()
			return nil
		}
		// On the happy path, just close the handle.
		// KILL_ON_JOB_CLOSE ensures any stray background children
		// are still cleaned up when the handle is released.
		defer closeHandle()
	}

	cmd.WaitDelay = 3 * time.Second

	err := cmd.Wait()
	output := buf.String()

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

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
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
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
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
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	procHandle, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("OpenProcess: %w", err)
	}

	err = windows.AssignProcessToJobObject(job, procHandle)
	windows.CloseHandle(procHandle)
	if err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	return job, nil
}
