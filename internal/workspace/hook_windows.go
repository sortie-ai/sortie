//go:build windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// statusControlCExit is the Windows NTSTATUS code for
// STATUS_CONTROL_C_EXIT (0xC000013A). It is used as the exit code
// when terminating a Job Object so the outcome is distinguishable
// from normal non-zero exits.
const statusControlCExit uint32 = 0xC000013A

// hookAssignDelay is a fault-injection seam that delays job assignment
// relative to process start, for reproducing a start-to-assign race on
// demand. Its zero value is production behavior: RunHook reads it once
// per invocation and skips the sleep entirely when it is zero. It is
// unreachable from configuration, from an environment variable, or
// from any exported identifier, and is only ever written by a
// non-parallel top-level test, which restores it to zero through
// t.Cleanup. It is an atomic.Int64 rather than a plain variable
// because the Windows test job runs without the race detector and so
// cannot enforce single-writer discipline by convention.
var hookAssignDelay atomic.Int64 // nanoseconds

// hookTeardown carries what RunHook observed while tearing the hook
// process tree down, for one invocation. Every field is populated on
// every path that reaches the point where the record is emitted,
// including the resume-failure path.
type hookTeardown struct {
	WaitMS           int64  // cmd.Wait duration
	WaitDelayExpired bool   // errors.Is(waitErr, exec.ErrWaitDelay)
	DrainMS          int64  // waitJobDrained duration
	DrainPolls       int    // QueryInformationJobObject calls made
	ActiveFirst      uint32 // ActiveProcesses at the first poll
	ActiveLast       uint32 // ActiveProcesses at the last poll
	TotalProcesses   uint32 // processes ever associated with the job
	TotalTerminated  uint32 // TotalTerminatedProcesses
	ResumedThreads   int    // threads resumed while starting the hook process
	JobMemberPIDs    []uint32
	RootOpenable     bool  // hook root still openable after the drain
	RootInJobList    bool  // hook root PID still in JobMemberPIDs
	RootProbeErr     error // non-nil when the root probe itself failed
	Survivors        []hookSurvivor
	SurvivorScanErr  error // non-nil when the scan itself failed
	JobListErr       error // non-nil when reading the job's PID list failed
	DrainQueryErr    error // non-nil when a drain accounting query failed
}

// hookSurvivor names one process descended from the hook root that was
// still alive after the drain returned.
type hookSurvivor struct {
	PID       uint32
	ParentPID uint32
	Image     string // ProcessEntry32.ExeFile, base name only
	InJob     bool   // PID present in hookTeardown.JobMemberPIDs
}

// jobObjectBasicProcessIDList mirrors the Win32
// JOBOBJECT_BASIC_PROCESS_ID_LIST layout consumed by
// QueryInformationJobObject with JobObjectBasicProcessIdList;
// x/sys/windows defines the information class constant but not the
// struct. ProcessIDList is a variable-length trailing array: callers
// must over-allocate the buffer passed to QueryInformationJobObject
// and read past the fixed one-element bound to reach every entry.
type jobObjectBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIDList             [1]uintptr
}

// RunHook executes a hook script via cmd.exe on Windows, enforcing a
// timeout and capturing output. The subprocess is created suspended
// and assigned to a Job Object with KILL_ON_JOB_CLOSE before it is
// resumed, so nothing it spawns can start before the job assignment
// takes effect. Before returning, RunHook terminates any process
// remaining in the job and waits for the job to report zero active
// processes, or for the drain deadline to elapse.
//
// On success (exit code 0), returns a [HookResult] with truncated
// output. On failure, returns a [*HookError] with Op indicating the
// failure mode:
//   - "validate": invalid params
//   - "start": subprocess could not be started, or could not be resumed
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
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	procutil.SetProcessGroup(cmd)
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
	startedAt := time.Now()

	// The fault-injection seam: zero in production, a no-op sleep.
	if delay := hookAssignDelay.Load(); delay != 0 {
		time.Sleep(time.Duration(delay))
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

	// The process was created suspended, so it cannot run at all until
	// its threads are resumed. Resume verifies its own success through
	// the returned previous suspend count rather than trusting a nil
	// error, and a failure here terminates the process rather than
	// letting it run unsupervised: a suspended process cannot be made
	// to run without a thread handle, so there is no unsupervised
	// fallback to degrade to.
	var teardown hookTeardown
	resumedThreads, resumeErr := resumeHookProcess(uint32(cmd.Process.Pid)) //nolint:gosec // G115: a Windows PID never exceeds uint32 range
	teardown.ResumedThreads = resumedThreads
	if resumeErr != nil {
		if job != 0 {
			_ = windows.TerminateJobObject(job, statusControlCExit)
		} else {
			_ = cmd.Process.Kill()
		}
		slog.Warn("hook process resume failed",
			slog.String("dir", params.Dir),
			slog.Int("timeout_ms", params.TimeoutMS),
			slog.Any("error", resumeErr))
	}

	waitStart := time.Now()
	err := cmd.Wait()
	teardown.WaitMS = time.Since(waitStart).Milliseconds()
	teardown.WaitDelayExpired = errors.Is(err, exec.ErrWaitDelay)
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
		drainStart := time.Now()
		waitJobDrained(job, &teardown)
		teardown.DrainMS = time.Since(drainStart).Milliseconds()
	}

	var jobPIDs []uint32
	if job != 0 {
		jobPIDs, teardown.JobListErr = jobMemberPIDs(job)
	}
	teardown.JobMemberPIDs = jobPIDs

	pid := uint32(cmd.Process.Pid) //nolint:gosec // G115: a Windows PID never exceeds uint32 range
	teardown.Survivors, teardown.SurvivorScanErr = scanSurvivors(pid, jobPIDs)
	teardown.RootInJobList, teardown.RootOpenable, teardown.RootProbeErr = probeHookRoot(pid, jobPIDs, startedAt)

	logHookTeardown(params, teardown)

	if resumeErr != nil {
		// Consult the context error first, exactly like the ordinary
		// classification below: a deadline or cancellation landing in
		// the resume window killed the suspended process and left no
		// thread to resume, which is a timeout rather than a start
		// failure.
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
		return HookResult{}, &HookError{
			Op:       "start",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("resume hook process: %w", resumeErr),
		}
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

// logHookTeardown emits exactly one record per RunHook invocation
// describing what teardown observed. The level is Warn only on
// conclusive evidence of a leak (a surviving descendant, a non-zero
// active-process count at the last drain poll, or a failed survivor
// scan); otherwise it is Debug, which the default logger suppresses.
// Every populated field of teardown is carried as an attribute on
// both arms so an operator reading either record can tell whether a
// process escaped the job, whether the hook root outlived its removal
// from the job's active count, and how long each phase took.
func logHookTeardown(params HookParams, teardown hookTeardown) {
	args := []any{
		slog.String("dir", params.Dir),
		slog.Int("timeout_ms", params.TimeoutMS),
		slog.Int64("wait_ms", teardown.WaitMS),
		slog.Bool("wait_delay_expired", teardown.WaitDelayExpired),
		slog.Int64("drain_ms", teardown.DrainMS),
		slog.Int("drain_polls", teardown.DrainPolls),
		slog.Int64("active_first", int64(teardown.ActiveFirst)),
		slog.Int64("active_last", int64(teardown.ActiveLast)),
		slog.Int64("total_processes", int64(teardown.TotalProcesses)),
		slog.Int64("total_terminated", int64(teardown.TotalTerminated)),
		slog.Int("resumed_threads", teardown.ResumedThreads),
		slog.Any("job_member_pids", teardown.JobMemberPIDs),
		slog.Bool("root_openable", teardown.RootOpenable),
		slog.Bool("root_in_job_list", teardown.RootInJobList),
		slog.Any("survivors", teardown.Survivors),
	}
	if teardown.RootProbeErr != nil {
		args = append(args, slog.Any("root_probe_err", teardown.RootProbeErr))
	}
	if teardown.SurvivorScanErr != nil {
		args = append(args, slog.Any("survivor_scan_err", teardown.SurvivorScanErr))
	}
	if teardown.JobListErr != nil {
		args = append(args, slog.Any("job_list_err", teardown.JobListErr))
	}
	if teardown.DrainQueryErr != nil {
		args = append(args, slog.Any("drain_query_err", teardown.DrainQueryErr))
	}

	// An observation that failed is not an observation of a clean
	// teardown: it says nothing either way, so it raises the record
	// rather than letting a default zero value read as settled.
	unsettled := len(teardown.Survivors) > 0 ||
		teardown.ActiveLast > 0 ||
		teardown.SurvivorScanErr != nil ||
		teardown.JobListErr != nil ||
		teardown.DrainQueryErr != nil
	if unsettled {
		slog.Warn("hook process tree did not settle", args...)
		return
	}
	slog.Debug("hook process tree settled", args...)
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

// createToolhelp32SnapshotRetry calls CreateToolhelp32Snapshot, retrying
// up to 3 attempts total with a 10 ms pause between attempts.
// CreateToolhelp32Snapshot can fail transiently, so a single failure is
// not conclusive. Callers decide their own policy for exhaustion; this
// helper only retries and reports the last error.
func createToolhelp32SnapshotRetry(flags uint32, processID uint32) (windows.Handle, error) {
	const (
		maxAttempts = 3
		retryDelay  = 10 * time.Millisecond
	)

	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}
		snapshot, err := windows.CreateToolhelp32Snapshot(flags, processID)
		if err == nil {
			return snapshot, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("CreateToolhelp32Snapshot: %w", lastErr)
}

// resumeHookProcess resumes every thread owned by the process
// identified by pid, verifying each resume through the returned
// previous suspend count rather than assuming success from a nil
// error. A returned count greater than 1 means another agent also
// suspended the thread, so resume is retried on that thread until the
// previous count reaches 1, bounded at 8 iterations. PID reuse is not
// a hazard here: os/exec holds an open handle to the child process
// from Start until Wait, so the identifier cannot be recycled while
// this function runs. Any unresolved per-thread failure is reported
// as this function's error; it does not stop enumeration of the
// remaining threads.
func resumeHookProcess(pid uint32) (resumedThreads int, err error) {
	const maxResumeAttempts = 8

	snapshot, snapErr := createToolhelp32SnapshotRetry(windows.TH32CS_SNAPTHREAD, 0)
	if snapErr != nil {
		return 0, fmt.Errorf("thread snapshot: %w", snapErr)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	walkErr := windows.Thread32First(snapshot, &entry)
	if walkErr != nil {
		return 0, fmt.Errorf("Thread32First: %w", walkErr)
	}

	var unresolvedErr error
	for {
		if entry.OwnerProcessID == pid {
			tid := entry.ThreadID
			handle, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, tid)
			if openErr != nil {
				unresolvedErr = fmt.Errorf("OpenThread(%d): %w", tid, openErr)
			} else {
				resolved := false
				for range maxResumeAttempts {
					prev, resumeErr := windows.ResumeThread(handle)
					if resumeErr != nil {
						unresolvedErr = fmt.Errorf("ResumeThread(%d): %w", tid, resumeErr)
						break
					}
					if prev == 1 {
						resolved = true
						break
					}
				}
				_ = windows.CloseHandle(handle)
				switch {
				case resolved:
					resumedThreads++
				case unresolvedErr == nil:
					unresolvedErr = fmt.Errorf("thread %d did not reach a previous suspend count of 1 within %d attempts", tid, maxResumeAttempts)
				}
			}
		}

		entry.Size = uint32(unsafe.Sizeof(entry))
		if nextErr := windows.Thread32Next(snapshot, &entry); nextErr != nil {
			break
		}
	}

	if unresolvedErr == nil && resumedThreads == 0 {
		// The snapshot held no thread owned by the hook process, so
		// nothing was resumed and the process is still suspended.
		// Reporting success here would leave it to block until the
		// context expires and be misreported as a script timeout.
		unresolvedErr = fmt.Errorf("no thread owned by process %d appeared in the thread snapshot", pid)
	}

	return resumedThreads, unresolvedErr
}

// jobMemberPIDs returns the PIDs currently associated with job.
// maxJobMembers bounds the number of entries read from the trailing
// variable-length array; a hook process tree is small, so this is a
// generous ceiling rather than a measured one.
const maxJobMembers = 1024

func jobMemberPIDs(job windows.Handle) ([]uint32, error) {
	bufLen := int(unsafe.Sizeof(jobObjectBasicProcessIDList{})) + (maxJobMembers-1)*int(unsafe.Sizeof(uintptr(0)))
	buf := make([]byte, bufLen)

	err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // G103: QueryInformationJobObject takes the process ID list struct as a uintptr; x/sys/windows exposes no typed alternative
		uint32(bufLen),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("QueryInformationJobObject(JobObjectBasicProcessIdList): %w", err)
	}

	list := (*jobObjectBasicProcessIDList)(unsafe.Pointer(&buf[0])) //nolint:gosec // G103: reading the over-allocated buffer back as the mirrored struct
	count := min(list.NumberOfProcessIdsInList, maxJobMembers)

	first := unsafe.Pointer(&list.ProcessIDList[0]) //nolint:gosec // G103: base address of the over-allocated trailing array
	pids := make([]uint32, 0, count)
	for i := range count {
		entry := (*uintptr)(unsafe.Add(first, uintptr(i)*unsafe.Sizeof(uintptr(0)))) //nolint:gosec // G103: indexing past the fixed one-element array bound into the over-allocated buffer
		pids = append(pids, uint32(*entry))                                          //nolint:gosec // G115: a Windows PID never exceeds uint32 range
	}
	return pids, nil
}

// scanSurvivors reports every process descended from rootPID that is
// still alive after the drain returned. A discovered PID is not
// pinned by any handle of ours, so each candidate is confirmed live
// by opening it before it is reported; a candidate that cannot be
// opened has exited or its identifier was recycled, and is dropped
// rather than reported as a survivor.
func scanSurvivors(rootPID uint32, jobPIDs []uint32) ([]hookSurvivor, error) {
	snapshot, err := createToolhelp32SnapshotRetry(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("process snapshot: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()

	type procInfo struct {
		pid, parent uint32
		image       string
	}
	var entries []procInfo

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	walkErr := windows.Process32First(snapshot, &entry)
	if walkErr != nil {
		return nil, fmt.Errorf("Process32First: %w", walkErr)
	}
	for walkErr == nil {
		entries = append(entries, procInfo{
			pid:    entry.ProcessID,
			parent: entry.ParentProcessID,
			image:  windows.UTF16ToString(entry.ExeFile[:]),
		})
		entry.Size = uint32(unsafe.Sizeof(entry))
		walkErr = windows.Process32Next(snapshot, &entry)
	}

	inJob := make(map[uint32]bool, len(jobPIDs))
	for _, jobPID := range jobPIDs {
		inJob[jobPID] = true
	}

	// Build the transitive closure of PIDs reachable from rootPID by
	// following ParentProcessID edges, breadth-first, excluding
	// rootPID itself.
	closure := make(map[uint32]bool)
	frontier := []uint32{rootPID}
	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]
		for _, e := range entries {
			if e.parent == current && e.pid != rootPID && !closure[e.pid] {
				closure[e.pid] = true
				frontier = append(frontier, e.pid)
			}
		}
	}

	survivors := make([]hookSurvivor, 0, len(closure))
	for _, e := range entries {
		if !closure[e.pid] {
			continue
		}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.pid)
		if openErr != nil {
			// Exited, or the identifier was recycled; not a survivor.
			continue
		}
		_ = windows.CloseHandle(handle)
		survivors = append(survivors, hookSurvivor{
			PID:       e.pid,
			ParentPID: e.parent,
			Image:     e.image,
			InJob:     inJob[e.pid],
		})
	}
	return survivors, nil
}

// probeHookRoot reports whether the hook root process (rootPID) is
// still in the job's PID list and whether it is still openable after
// the drain returned. os/exec releases its own handle to the child
// when cmd.Wait returns, so a successful open after the drain means
// the process object outlived both our handle and its removal from
// the job's active count. PID reuse is guarded by comparing the
// opened process's creation time against startedAt, the moment
// cmd.Start returned: the identifier belonged to our root while our
// root lived, so any later holder of it was created after that
// moment, and a creation time after it means the PID was recycled
// and is not our hook root.
func probeHookRoot(rootPID uint32, jobPIDs []uint32, startedAt time.Time) (inList bool, openable bool, err error) {
	inList = slices.Contains(jobPIDs, rootPID)

	handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, rootPID)
	if openErr != nil {
		// An exited root is the expected outcome on most runs.
		return inList, false, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	if timesErr := windows.GetProcessTimes(handle, &creationTime, &exitTime, &kernelTime, &userTime); timesErr != nil {
		return inList, false, fmt.Errorf("GetProcessTimes: %w", timesErr)
	}

	if time.Unix(0, creationTime.Nanoseconds()).After(startedAt) {
		// Created after our root was started, so the PID was
		// recycled; this is not our hook root.
		return inList, false, nil
	}
	return inList, true, nil
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
// the job remains active or the deadline expires, recording each
// poll's counters into teardown. Termination initiated by
// TerminateJobObject completes asynchronously, so returning before the
// active count reaches zero would let a dying child briefly outlive
// RunHook with its working-directory handle still open.
func waitJobDrained(job windows.Handle, teardown *hookTeardown) {
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
		teardown.DrainPolls++
		if err != nil {
			teardown.DrainQueryErr = err
			slog.Warn("hook job accounting query failed; drain skipped",
				slog.Any("error", err))
			return
		}
		if teardown.DrainPolls == 1 {
			teardown.ActiveFirst = info.ActiveProcesses
		}
		teardown.ActiveLast = info.ActiveProcesses
		teardown.TotalProcesses = info.TotalProcesses
		teardown.TotalTerminated = info.TotalTerminatedProcesses
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
